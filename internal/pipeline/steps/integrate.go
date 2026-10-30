package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// mergeIntoWorktree is the pipeline's one way to bring another line of history
// into a branch: a `git merge --no-ff --no-edit` of targetSHA into the run
// worktree's HEAD. The merge commit's FIRST parent is the head the step
// started from, so every commit already on the branch keeps its SHA and the
// reviewed head stays an ancestor, which is what lets publication be a plain
// fast-forward and the CI continuity rule hold by ancestry. --no-ff is
// deliberate: a merge that silently fast-forwarded would leave no commit whose
// two parents show what the integration joined.
//
// The target is a SHA, resolved by the caller before this runs, so the commit
// merged is exactly the one the caller decided to merge. label names it for
// the merge commit's subject and for errors ("origin/main").
//
// Nothing here resolves a conflict. A merge that stops on one is aborted, the
// worktree is verified back on the pre-merge head (restoreWorktreeHead), and
// the conflicted files are returned for the caller to park on; resolving them
// is the operator's own merge. A merge that fails without conflicts, or that
// does not produce a commit with exactly [pre-merge head, targetSHA] as its
// parents, is restored the same way and returned as an error. A target the
// head already contains is a no-op.
//
// Tracked files must be unmodified (requireCleanTrackedTree).
func mergeIntoWorktree(sctx *pipeline.StepContext, run gitRunner, targetSHA, label string) ([]string, error) {
	preMerge, err := run("rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve head before merging %s: %w", label, err)
	}
	if err := requireCleanTrackedTree(run, "merge", label); err != nil {
		return nil, err
	}
	if _, err := run("merge-base", "--is-ancestor", targetSHA, preMerge); err == nil {
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("merging %s...", label))
	if _, mergeErr := run("merge", "--no-ff", "--no-edit", "-m", "Merge "+label, targetSHA); mergeErr != nil {
		conflicts := conflictedFiles(run)
		_, _ = run("merge", "--abort")
		if err := restoreWorktreeHead(sctx, run, preMerge); err != nil {
			return nil, fmt.Errorf("merge %s: %v; %w", label, mergeErr, err)
		}
		if len(conflicts) == 0 {
			return nil, fmt.Errorf("merge %s: %w", label, mergeErr)
		}
		return conflicts, nil
	}

	// The merge is git's own and deterministic, but the run only ever carries a
	// head it has proven: anything other than a two-parent commit of exactly
	// these two sides is put back and refused.
	out, err := run("rev-list", "--parents", "-n", "1", "HEAD")
	fields := strings.Fields(out)
	if err != nil || len(fields) != 3 || fields[1] != preMerge || fields[2] != targetSHA {
		cause := fmt.Errorf("merge %s did not produce a merge commit of %s and %s (HEAD %q)", label, preMerge, targetSHA, out)
		if err := restoreWorktreeHead(sctx, run, preMerge); err != nil {
			return nil, fmt.Errorf("%v; %w", cause, err)
		}
		return nil, cause
	}
	return nil, nil
}

// fastForwardWorktree moves the run worktree's HEAD forward to targetSHA, which
// must descend from it. It is the one other way the pipeline moves a head to
// another line of history, and it shares mergeIntoWorktree's guarantees: no
// commit already on the branch is dropped (the old head stays an ancestor),
// tracked files must be unmodified, and the result is verified, restoring the
// old head when it is not exactly targetSHA. `merge --ff-only` rather than a
// reset, so git itself refuses anything that is not a fast-forward.
func fastForwardWorktree(sctx *pipeline.StepContext, run gitRunner, targetSHA, label string) error {
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve head before fast-forwarding to %s: %w", label, err)
	}
	if err := requireCleanTrackedTree(run, "fast-forward to", label); err != nil {
		return err
	}
	if _, err := run("merge-base", "--is-ancestor", head, targetSHA); err != nil {
		return fmt.Errorf("refusing to fast-forward to %s: it does not contain the run worktree's head %s", label, head)
	}
	sctx.Log(fmt.Sprintf("fast-forwarding to %s", label))
	if _, err := run("merge", "--ff-only", targetSHA); err != nil {
		if restoreErr := restoreWorktreeHead(sctx, run, head); restoreErr != nil {
			return fmt.Errorf("fast-forward to %s: %v; %w", label, err, restoreErr)
		}
		return fmt.Errorf("fast-forward to %s: %w", label, err)
	}
	if got, err := run("rev-parse", "HEAD"); err != nil || got != targetSHA {
		cause := fmt.Errorf("fast-forward to %s left HEAD at %q instead of %s", label, got, targetSHA)
		if restoreErr := restoreWorktreeHead(sctx, run, head); restoreErr != nil {
			return fmt.Errorf("%v; %w", cause, restoreErr)
		}
		return cause
	}
	return nil
}

// requireCleanTrackedTree refuses to move the head while tracked files carry
// uncommitted changes: a restore resets them, so they would be lost. They
// never belong in the run worktree, so their presence is an error rather than
// something to stash around.
func requireCleanTrackedTree(run gitRunner, verb, label string) error {
	dirty, err := run("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("read worktree status before the %s %s: %w", verb, label, err)
	}
	if dirty != "" {
		return fmt.Errorf("refusing to %s %s: the run worktree has uncommitted changes:\n%s", verb, label, dirty)
	}
	return nil
}

// mergeConflictFinding is the finding a step parks on for one file a merge of
// label conflicted in. Resolving a conflict is a human's deliberate, auditable
// act, so it is ask-user and nothing automatic may select it; the description
// tells the operator the one way forward.
func mergeConflictFinding(label, file string) Finding {
	return Finding{
		Severity: "warning",
		File:     file,
		Description: fmt.Sprintf("merge conflict merging %s: merge %s into your branch locally, resolve, and push; the gate does not resolve merge conflicts",
			label, label),
		Action: types.ActionAskUser,
	}
}

// abortInterruptedIntegration puts a run worktree left mid-merge or mid-rebase
// (an attempt interrupted by a crash, a cancellation, or an older binary) back
// on the head that operation started from, before a step integrates anything.
// Integrating on top of it would either be refused by git or, for a stopped
// rebase, carry a half-replayed branch forward as if it were the real one.
func abortInterruptedIntegration(sctx *pipeline.StepContext, run gitRunner) error {
	switch {
	case rebaseInProgress(sctx.Ctx, sctx.WorkDir):
		origHead := rebaseOrigHead(sctx, run)
		if origHead == "" {
			return fmt.Errorf("the run worktree has an interrupted rebase whose original head cannot be read; refusing to integrate")
		}
		sctx.Log("aborting an interrupted rebase in the run worktree...")
		if _, err := run("rebase", "--abort"); err != nil {
			return fmt.Errorf("abort the interrupted rebase in the run worktree: %w", err)
		}
		return restoreWorktreeHead(sctx, run, origHead)
	case mergeInProgress(sctx.Ctx, sctx.WorkDir):
		preMerge, err := run("rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("resolve head of the interrupted merge: %w", err)
		}
		sctx.Log("aborting an interrupted merge in the run worktree...")
		if _, err := run("merge", "--abort"); err != nil {
			return fmt.Errorf("abort the interrupted merge in the run worktree: %w", err)
		}
		return restoreWorktreeHead(sctx, run, preMerge)
	}
	return nil
}

// rebaseOrigHead reads the head an interrupted rebase started from, which is
// where `git rebase --abort` returns HEAD to.
func rebaseOrigHead(sctx *pipeline.StepContext, run gitRunner) string {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path, err := run("rev-parse", "--git-path", dir+"/orig-head")
		if err != nil {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(sctx.WorkDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(string(data)); sha != "" {
			return sha
		}
	}
	return ""
}

// restoreWorktreeHead puts the run worktree back exactly on head with a clean
// tracked tree and no merge or rebase in progress. It is fail-closed: a
// restore that does not land exactly there is returned as an error, never
// swallowed, so nothing is described as restored that was not.
//
// HEAD ATTACHMENT is deliberately not verified. The pipeline's run worktree is
// created detached (`git worktree add --detach`) and no step ever checks a
// branch out in it, so requiring an attached HEAD would report every correct
// restore as a failed one; the commit comparison is what proves the restore.
func restoreWorktreeHead(sctx *pipeline.StepContext, run gitRunner, head string) error {
	if _, err := run("reset", "--hard", head); err != nil {
		return fmt.Errorf("restoring the run worktree to %s failed, it is left at the rejected state: %w", head, err)
	}
	got, err := run("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("restoring the run worktree to %s could not be verified: %w", head, err)
	}
	if got != head {
		return fmt.Errorf("restoring the run worktree to %s left it at %s instead", head, got)
	}
	// HEAD reading as head is not the same as the worktree being back on it: a
	// reset performed while a rebase is interrupted moves HEAD and leaves the
	// rebase underneath it.
	if rebaseInProgress(sctx.Ctx, sctx.WorkDir) || mergeInProgress(sctx.Ctx, sctx.WorkDir) {
		return fmt.Errorf("restoring the run worktree to %s left a rebase or merge in progress", head)
	}
	return nil
}

// conflictedFiles lists the unmerged paths of a merge that stopped on
// conflicts.
func conflictedFiles(run gitRunner) []string {
	out, err := run("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}
