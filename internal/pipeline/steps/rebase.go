package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RebaseStep syncs the pushed branch with the configured push target and the
// latest default branch from upstream. Despite its historical name it never
// rebases: a moved target is merged with mergeIntoWorktree, so every commit
// already on the branch keeps its SHA, and a target that conflicts parks the
// step for the operator to merge by hand. No agent ever resolves a conflict.
type RebaseStep struct{}

func (s *RebaseStep) Name() types.StepName { return types.StepRebase }

const forkBranchRefPrefix = "refs/remotes/no-mistakes-push/"

func (s *RebaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	run := func(args ...string) (string, error) { return git.Run(ctx, sctx.WorkDir, args...) }
	// Recovery has no integration path of its own: a resumed or restarted run
	// re-enters this step, so whatever an interrupted attempt left behind is
	// undone here before anything reads the worktree's head.
	if err := abortInterruptedIntegration(sctx, run); err != nil {
		return nil, err
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	defaultBranch := effectivePRBaseBranch(sctx)
	branchTarget := ""
	pushRemote := resolveUpstreamURL(sctx)
	if branch != "" {
		branchTarget = "origin/" + branch
		if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
			pushRemote = sctx.Repo.PushURL()
			branchTarget = forkBranchTrackingRef(branch)
		}
	}

	// Detect force push before fetching so we can skip pushed-branch sync.
	// A force push means the user explicitly rewrote the branch - the pushed
	// commit is authoritative and must not be overwritten by prior pipeline
	// state on the remote.
	forcePush := isForcePushAgainstRemote(ctx, sctx.WorkDir, pushRemote, branch, branchTarget, sctx.Run.BaseSHA)

	sctx.Log("fetching latest upstream state...")
	if err := fetchRunUpstreamBranch(ctx, sctx, defaultBranch); err != nil {
		return nil, fmt.Errorf("fetch base branch %q before merging it: %w", defaultBranch, err)
	}
	// Sync the push branch's remote-tracking ref only when we are about to merge
	// it (a normal push). On a force push we deliberately skip both the fetch
	// and the merge: the pushed commit is authoritative, and the remote-tracking
	// ref must keep pointing at the head we last *observed* rather than the live
	// tip. The push step uses that tracking ref as its force-with-lease anchor;
	// if we refreshed it here, the anchor would equal the live remote head and the
	// lease's "remote unchanged since we last saw it" fast path would pass even
	// when the remote carries an out-of-band commit - silently clobbering it
	// (the original #281/#305 hazard, in the force-push path). Leaving it stale is
	// what lets the push step's content check catch that case.
	if !forcePush && branch != "" && branch != defaultBranch {
		if strings.TrimSpace(sctx.Repo.ForkURL) == "" {
			if err := fetchRunUpstreamBranch(ctx, sctx, branch); err != nil {
				sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
			}
		} else if err := git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, pushRemote, branch, branchTarget); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch %s: %v", branchTarget, err))
		}
	}

	// Stop before merging when the gated branch carries commits that live on
	// the contributor's local default branch but were never pushed to
	// origin/<default>. Merging the fresh remote default keeps those
	// commits in the branch's history, so the PR may bundle another
	// workstream's unpushed work. Surface the ambiguity for a human decision.
	if outcome := detectBundledLocalDefaultCommits(ctx, sctx, branch, defaultBranch); outcome != nil {
		return outcome, nil
	}
	if forcePush && branch == defaultBranch && remoteDefaultBranchAdvanced(ctx, sctx.WorkDir, defaultBranch, sctx.Run.BaseSHA) {
		findingsJSON, _ := json.Marshal(Findings{
			Items: []Finding{{
				Severity:    "warning",
				File:        filepath.Join("internal", "pipeline", "steps", "rebase.go"),
				Description: fmt.Sprintf("origin/%s advanced after the force push; manual review required before updating the default branch", defaultBranch),
			}},
			Summary: fmt.Sprintf("remote %s advanced during force push", defaultBranch),
		})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	targets := integrationTargets(branch, defaultBranch, branchTarget)
	if forcePush {
		sctx.Log("force push detected, skipping " + branchTarget + " sync")
		targets = forcePushIntegrationTargets(branch, defaultBranch)
	}

	startHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}

	// A fix round at a conflict gate runs exactly this too: the same
	// deterministic merge, which either lands now that the target no longer
	// conflicts or parks again on the same files.
	var conflictTargets []string
	var conflictFindings []Finding
	for _, target := range targets {
		conflictFiles, err := mergeTarget(sctx, run, target)
		if err != nil {
			// Undo what an earlier target already merged, so a failed step
			// leaves the branch as it found it. A head that never moved is
			// left alone: the failure may be the dirty worktree itself, which
			// a reset would destroy.
			if head, headErr := git.HeadSHA(ctx, sctx.WorkDir); headErr != nil || head != startHead {
				if restoreErr := restoreWorktreeHead(sctx, run, startHead); restoreErr != nil {
					return nil, fmt.Errorf("%w; %v", err, restoreErr)
				}
			}
			return nil, err
		}
		if len(conflictFiles) > 0 {
			label := mergeTargetLabel(target)
			conflictTargets = append(conflictTargets, label)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, mergeConflictFinding(label, file))
			}
		}
	}

	if len(conflictTargets) > 0 {
		// All or nothing: a target that merged cleanly before or after the
		// conflict is undone too, so nothing half-integrated is reviewed,
		// tested, or published, and the operator's own merge starts from the
		// branch exactly as they pushed it.
		if err := restoreWorktreeHead(sctx, run, startHead); err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("conflict merging %s", strings.Join(conflictTargets, ", "))
		findingsJSON, _ := json.Marshal(Findings{Items: dedupeConflictFindings(conflictFindings), Summary: summary})
		outcome := &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   false,
			Findings:      string(findingsJSON),
		}
		if sctx.Fixing {
			outcome.FixSummary = NoChangesAppliedSummary
			sctx.Log("no changes applied: the base still conflicts; merge it into your branch locally, resolve, and push")
		}
		return outcome, nil
	}

	outcome, err := updateHeadSHA(ctx, sctx)
	if err == nil && sctx.Fixing {
		if sctx.Run.HeadSHA == startHead {
			outcome.FixSummary = NoChangesAppliedSummary
			sctx.Log("no changes applied: branch already up to date")
		} else {
			outcome.FixSummary = changesAppliedSummary
			sctx.Log("merged upstream into branch")
		}
	}
	return outcome, err
}

// integrationTargets returns the ordered list of refs to integrate: the
// pushed-branch target, then the PR base.
func integrationTargets(branch, defaultBranch, branchTarget string) []string {
	var targets []string
	if branch != "" && branch != defaultBranch {
		targets = append(targets, branchTarget)
	}
	if branch != defaultBranch {
		targets = append(targets, "origin/"+defaultBranch)
	}
	return targets
}

// forcePushIntegrationTargets returns the targets to integrate after a force push. The pushed
// branch target is skipped because it may contain autofix commits from prior
// pipeline runs that the force push intended to discard.
func forcePushIntegrationTargets(branch, defaultBranch string) []string {
	if branch == defaultBranch {
		return nil
	}
	return []string{"origin/" + defaultBranch}
}

// effectivePRBaseBranch resolves the branch used as the integration base.
// Per-run overrides win over repo config; the repository default remains the
// fallback when neither selects a separate PR target branch.
func effectivePRBaseBranch(sctx *pipeline.StepContext) string {
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if runBase := runPRBaseBranch(sctx); runBase != "" {
		defaultBranch = runBase
	} else if sctx.Config != nil && strings.TrimSpace(sctx.Config.PR.BaseBranch) != "" {
		defaultBranch = strings.TrimSpace(sctx.Config.PR.BaseBranch)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return defaultBranch
}

// detectBundledLocalDefaultCommits returns a blocking finding when the gated
// branch carries commits that exist on the contributor's local default branch
// but were never pushed to origin/<default>. In multi-session / monorepo setups
// the local default branch routinely carries another workstream's unpushed
// work; branching a fix off that local tip silently drags it into the PR when
// the branch is integrated with the remote default. Returns nil when no such
// divergence is detected so the run proceeds normally.
//
// It only flags commits the branch actually carries: it reads the local default
// tip from the working repo, confirms that tip is ahead of origin/<default> and
// is a strict ancestor of the branch HEAD, then enumerates the unpushed commits.
// Equal tips are the common commit-on-main-then-name-a-branch workflow, not
// evidence of an additional bundled workstream.
// Detection is best-effort - if the local default tip advanced past the branch
// point, or the working repo cannot be read, it returns nil rather than guess.
func detectBundledLocalDefaultCommits(ctx context.Context, sctx *pipeline.StepContext, branch, defaultBranch string) *pipeline.StepOutcome {
	if branch == "" || branch == defaultBranch {
		return nil
	}
	workingPath := strings.TrimSpace(sctx.Repo.WorkingPath)
	if workingPath == "" {
		return nil
	}
	localTip, err := git.Run(ctx, workingPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch+"^{commit}")
	if err != nil {
		return nil
	}
	localTip = strings.TrimSpace(localTip)
	if localTip == "" {
		return nil
	}
	remoteRef := "origin/" + defaultBranch
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err != nil {
		return nil
	}
	// The local default tip must be present in the gate's object store (it is
	// when the branch carries it as an ancestor) for the reachability checks.
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", localTip+"^{commit}"); err != nil {
		return nil
	}
	// Already pushed (local default not ahead of remote) -> nothing bundled.
	if isAncestor(ctx, sctx.WorkDir, localTip, remoteRef) {
		return nil
	}
	// The branch must actually carry the local default tip's commits.
	if !isAncestor(ctx, sctx.WorkDir, localTip, "HEAD") {
		return nil
	}

	// A delivery branch created at local main's tip carries only that intended
	// work, not an additional workstream beneath its own commits (#998).
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err == nil && head == localTip {
		return nil
	}

	subjects, err := git.Run(ctx, sctx.WorkDir, "log", "--oneline", "--no-decorate", remoteRef+".."+localTip)
	if err != nil || strings.TrimSpace(subjects) == "" {
		return nil
	}
	commits := strings.Split(strings.TrimSpace(subjects), "\n")
	// Report the proposed PR, not a two-dot comparison that can count
	// upstream-only changes as removals from an outdated local default tip.
	base, baseErr := git.Run(ctx, sctx.WorkDir, "merge-base", remoteRef, "HEAD")
	var files []string
	var filesErr error
	if baseErr == nil {
		files, filesErr = git.DiffNameOnly(ctx, sctx.WorkDir, base, "HEAD")
	}
	fileEvidence := "PR file count unavailable"
	if baseErr == nil && filesErr == nil {
		fileEvidence = fmt.Sprintf("proposed PR changes %d file(s)", len(files))
	}
	firstFile := ""
	if len(files) > 0 {
		firstFile = files[0]
	}

	description := fmt.Sprintf(
		"branch carries %d commit(s) that exist on your local %s branch but were never pushed to origin/%s; these may be unintended bundled work (%s):\n- %s\n\nConfirm these commits belong in this PR before approving, or manually separate the intended work onto origin/%s before gating.",
		len(commits), defaultBranch, defaultBranch, fileEvidence, strings.Join(commits, "\n- "), defaultBranch,
	)
	fixSummary := ""
	if sctx.Fixing {
		fixSummary = NoChangesAppliedSummary
		const explanation = "no changes applied: bundled local-default commits require manual separation or explicit approval"
		description += "\n\n" + explanation + "; a fix round only retries the merge and cannot select commits to discard."
		sctx.Log(explanation)
	}
	findingsJSON, _ := json.Marshal(Findings{
		Items: []Finding{{
			Severity:    "warning",
			File:        firstFile,
			Description: description,
			// Bundling another workstream's unpushed commits is a workflow call
			// the contributor must make (push <default>, separate the work, or
			// proceed); the pipeline cannot safely auto-resolve it. Mark it
			// ask-user so the gate classifies it correctly and the driving
			// agent escalates.
			Action: types.ActionAskUser,
		}},
		Summary: fmt.Sprintf("branch bundles %d unpushed %s commit(s)", len(commits), defaultBranch),
	})
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}
}

func isAncestor(ctx context.Context, workDir, ancestor, descendant string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func remoteDefaultBranchAdvanced(ctx context.Context, workDir, defaultBranch, baseSHA string) bool {
	if baseSHA == "" || git.IsZeroSHA(baseSHA) {
		return false
	}
	remoteSHA, err := git.Run(ctx, workDir, "rev-parse", "--verify", "origin/"+defaultBranch)
	if err != nil {
		return false
	}
	return strings.TrimSpace(remoteSHA) != baseSHA
}

// isForcePush returns true when the current push is non-fast-forward relative
// to the previous push (baseSHA). This indicates the user explicitly rewrote
// history and the pipeline should treat the new HEAD as authoritative.
func isForcePush(ctx context.Context, workDir, branch, baseSHA string) bool {
	localRef := ""
	if branch != "" {
		localRef = "origin/" + branch
	}
	return isForcePushAgainstRemote(ctx, workDir, "origin", branch, localRef, baseSHA)
}

func isForcePushAgainstRemote(ctx context.Context, workDir, remote, branch, localRef, baseSHA string) bool {
	if git.IsZeroSHA(baseSHA) || baseSHA == "" {
		return false
	}
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", baseSHA, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	if branch != "" {
		remoteSHA, err := git.LsRemote(ctx, workDir, remote, "refs/heads/"+branch)
		if err == nil && remoteSHA != "" {
			_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD")
			if err == nil {
				return false
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return true
			}
		}
		if localRef != "" {
			if _, err := git.Run(ctx, workDir, "rev-parse", "--verify", localRef); err == nil {
				return isRemoteBranchRewritten(ctx, workDir, localRef)
			}
		}
	}
	return false
}

func forkBranchTrackingRef(branch string) string {
	return forkBranchRefPrefix + branch
}

func isRemoteBranchRewritten(ctx context.Context, workDir, remoteRef string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteRef, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// mergeTarget brings one target into the branch: nothing when it is missing
// or already contained, a fast-forward (fastForwardWorktree) when the branch
// is strictly behind it, and otherwise a merge (mergeIntoWorktree). Its tip is
// resolved to a SHA once, here, so the commit integrated is the one the step
// observed. It returns the conflicted files of a merge that stopped on
// conflicts, with the worktree restored.
func mergeTarget(sctx *pipeline.StepContext, run gitRunner, target string) ([]string, error) {
	if _, err := run("rev-parse", "--verify", target); err != nil {
		return nil, nil
	}
	targetSHA, err := run("rev-parse", "--verify", target+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", target, err)
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("get local head: %w", err)
	}
	label := mergeTargetLabel(target)
	switch {
	case head == targetSHA:
		sctx.Log(fmt.Sprintf("already up-to-date with %s", target))
		return nil, nil
	case isAncestor(sctx.Ctx, sctx.WorkDir, targetSHA, head):
		sctx.Log(fmt.Sprintf("already ahead of %s", target))
		return nil, nil
	case isAncestor(sctx.Ctx, sctx.WorkDir, head, targetSHA):
		return nil, fastForwardWorktree(sctx, run, targetSHA, label)
	}
	return mergeIntoWorktree(sctx, run, targetSHA, label)
}

// mergeTargetLabel names a target the way the operator knows it. The push
// remote's tracking ref is private to the gate's worktree, so it is named by
// branch instead.
func mergeTargetLabel(target string) string {
	if branch, ok := strings.CutPrefix(target, forkBranchRefPrefix); ok {
		return branch + " from the push remote"
	}
	return target
}

// rebaseInProgress returns true if a git rebase is currently in progress.
func rebaseInProgress(ctx context.Context, workDir string) bool {
	return gitPathExists(ctx, workDir, "rebase-merge", "rebase-apply")
}

// mergeInProgress returns true if a git merge is currently in progress -
// started and not yet concluded by a commit or an abort.
func mergeInProgress(ctx context.Context, workDir string) bool {
	return gitPathExists(ctx, workDir, "MERGE_HEAD")
}

// gitPathExists reports whether any of the named paths exists inside the git
// directory. It resolves them with git rev-parse --git-path, which works for
// both regular repos and worktrees (where the git dir is not .git).
func gitPathExists(ctx context.Context, workDir string, names ...string) bool {
	for _, name := range names {
		p, err := git.Run(ctx, workDir, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func dedupeConflictFindings(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	seen := make(map[string]bool, len(findings))
	filtered := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := finding.File + "\x00" + finding.Description
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, finding)
	}
	return filtered
}

// updateHeadSHA syncs the run's head SHA after the merge and checks for an empty diff.
// When the branch diff against the default branch is empty, SkipRemaining is set.
func updateHeadSHA(ctx context.Context, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head after merging the base: %w", err)
	}
	if headSHA != "" && headSHA != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("updated head SHA to %s", shortSHA(headSHA)))
	}

	// Check if the branch has any diff against the default branch.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	// Execute already fetched the base branch (fail-closed) before integrating,
	// so reuse that ref instead of fetching again after HEAD was merged and
	// persisted: a failure here could not undo either.
	defaultBranch := effectivePRBaseBranch(sctx)
	baseSHA := mergeBaseWithDefaultBranch(ctx, sctx.WorkDir, defaultBranch)
	if baseSHA == "" {
		baseSHA = resolveBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, defaultBranch)
	}
	diff, err := git.Diff(ctx, sctx.WorkDir, baseSHA, "HEAD")
	if err == nil && strings.TrimSpace(diff) == "" {
		sctx.Log("empty diff after integrating the base, skipping remaining steps")
		return &pipeline.StepOutcome{SkipRemaining: true}, nil
	}

	return &pipeline.StepOutcome{}, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
