package steps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// mergeFixture builds a repository whose feature branch and origin/main have
// both moved since they diverged, which is the only situation the rebase step
// actually integrates anything in. conflicting decides whether they collide:
// main and feature then write different content to the same file, otherwise
// they touch separate files for a clean integration.
type mergeFixture struct {
	dir      string
	upstream string
	baseSHA  string
	headSHA  string // the feature head the pipeline reviewed, pre-integration
	mainSHA  string // origin/main after it moved
}

func newMergeFixture(t *testing.T, conflicting bool) mergeFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "shared.txt", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if conflicting {
		writeFixtureFile(t, dir, "shared.txt", "feature line\n")
	} else {
		writeFixtureFile(t, dir, "feature.txt", "feature line\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	if conflicting {
		writeFixtureFile(t, dir, "shared.txt", "main line\n")
	} else {
		writeFixtureFile(t, dir, "main.txt", "main line\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main advance")
	mainSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	return mergeFixture{dir: dir, upstream: upstream, baseSHA: baseSHA, headSHA: headSHA, mainSHA: mainSHA}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f mergeFixture) context(t *testing.T, ag agent.Agent) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, f.dir, f.baseSHA, f.headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = f.upstream
	return sctx
}

// noAgent is the only agent a rebase step test hands the step: the step must
// never invoke one, on a clean merge, a conflict, or a fix round.
func noAgent(t *testing.T) *mockAgent {
	t.Helper()
	return &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			t.Error("the rebase step invoked an agent")
			return nil, fmt.Errorf("the rebase step must not invoke an agent")
		},
	}
}

// parents returns a commit's parent SHAs in order. The FIRST one is what the
// merge shape turns on: it must stay the head the pipeline reviewed.
func parents(t *testing.T, dir, rev string) []string {
	t.Helper()
	out := gitCmd(t, dir, "rev-list", "--parents", "-n", "1", rev)
	fields := strings.Fields(out)
	if len(fields) < 1 {
		t.Fatalf("rev-list --parents returned %q", out)
	}
	return fields[1:]
}

// assertBranchUnchanged is what a conflict owes the run: the worktree is left
// on the head the step started from, with nothing of the merge attempt staged,
// modified, untracked, or still in progress on top of it, and the run's
// recorded head has not moved either.
func assertBranchUnchanged(t *testing.T, sctx *pipeline.StepContext, dir, head string) {
	t.Helper()
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head = %s, want the branch left at %s", got, head)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree is not clean:\n%s", status)
	}
	if mergeInProgress(context.Background(), dir) || rebaseInProgress(context.Background(), dir) {
		t.Fatal("a merge or rebase was left in progress")
	}
	if sctx.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, head)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != head {
		t.Fatalf("persisted run head = %s, want %s", run.HeadSHA, head)
	}
}

// assertConflictPark checks the operator-facing park: one ask-user finding per
// conflicted file, naming the target and the remedy, that no automatic
// resolver may act on.
func assertConflictPark(t *testing.T, outcome *pipeline.StepOutcome, target string, files ...string) {
	t.Helper()
	if !outcome.NeedsApproval {
		t.Fatal("expected the conflict to park the step")
	}
	if outcome.AutoFixable {
		t.Fatal("a merge conflict must not be auto-fixable: nothing in the pipeline resolves it")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v\n%s", err, outcome.Findings)
	}
	if len(findings.Items) != len(files) {
		t.Fatalf("findings = %+v, want one per conflicted file %v", findings.Items, files)
	}
	for i, file := range files {
		item := findings.Items[i]
		if item.File != file || item.Severity != "warning" || item.Action != types.ActionAskUser {
			t.Fatalf("finding %d = %+v, want an ask-user warning on %s", i, item, file)
		}
		for _, want := range []string{target, "into your branch locally, resolve, and push", "the gate does not resolve merge conflicts"} {
			if !strings.Contains(item.Description, want) {
				t.Fatalf("finding description %q is missing %q", item.Description, want)
			}
		}
	}
}

func TestRebaseStep_IntegratesMovedBaseAsAMergeCommit(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, noAgent(t))

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean merge, got approval gate: %s", outcome.Findings)
	}

	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	got := parents(t, f.dir, head)
	if len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainSHA {
		t.Fatalf("head %s parents = %v, want a merge commit [%s %s]", head, got, f.headSHA, f.mainSHA)
	}
	for _, name := range []string{"feature.txt", "main.txt"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("expected %s after the merge: %v", name, err)
		}
	}
	if status := gitStatusPorcelain(t, f.dir); status != "" {
		t.Fatalf("expected a clean worktree, got: %s", status)
	}
	if sctx.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want the merge commit %s", sctx.Run.HeadSHA, head)
	}
}

// A conflicting base is never resolved by the pipeline: the step parks with
// the file named for the operator, and the branch stays exactly where it was.
func TestRebaseStep_ConflictParksForTheOperatorWithTheBranchUnchanged(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)
	sctx := f.context(t, noAgent(t))

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertConflictPark(t, outcome, "origin/main", "shared.txt")
	assertBranchUnchanged(t, sctx, f.dir, f.headSHA)
}

// Fix at the conflict gate re-runs the same deterministic merge and nothing
// else: while the conflict stands it re-parks with the same file, and no agent
// is ever asked to resolve it.
func TestRebaseStep_FixRoundReRunsOnlyTheMerge(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)
	ag := noAgent(t)
	sctx := f.context(t, ag)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","file":"shared.txt","description":"merge conflict merging origin/main"}]}`

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertConflictPark(t, outcome, "origin/main", "shared.txt")
	if outcome.FixSummary != NoChangesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q while the conflict stands", outcome.FixSummary, NoChangesAppliedSummary)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("the fix round invoked an agent %d time(s)", len(ag.calls))
	}
	assertBranchUnchanged(t, sctx, f.dir, f.headSHA)
}

// Once the base stops conflicting (here main backed its change out), the same
// fix round merges it like any clean integration.
func TestRebaseStep_FixRoundMergesOnceTheConflictIsGone(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "shared.txt", "base\n")
	gitCmd(t, f.dir, "commit", "-am", "back the conflicting change out")
	mainSHA := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")

	sctx := f.context(t, noAgent(t))
	sctx.Fixing = true

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected the fix round to merge cleanly, got: %s", outcome.Findings)
	}
	if outcome.FixSummary != changesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q", outcome.FixSummary, changesAppliedSummary)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if got := parents(t, f.dir, head); len(got) != 2 || got[0] != f.headSHA || got[1] != mainSHA {
		t.Fatalf("head %s parents = %v, want [%s %s]", head, got, f.headSHA, mainSHA)
	}
}

// A run worktree left mid-merge (a crashed or interrupted earlier attempt) is
// aborted and restored before the step integrates; otherwise git refuses the
// new merge and the half-merged index would be what the run carries forward.
func TestRebaseStep_AbortsAnInterruptedMergeBeforeIntegrating(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	gitCmd(t, f.dir, "merge", "--no-ff", "--no-commit", "origin/main")
	if !mergeInProgress(context.Background(), f.dir) {
		t.Fatal("fixture did not leave a merge in progress")
	}
	sctx := f.context(t, noAgent(t))

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean merge after the abort, got: %s", outcome.Findings)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if got := parents(t, f.dir, head); len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainSHA {
		t.Fatalf("head %s parents = %v, want [%s %s]", head, got, f.headSHA, f.mainSHA)
	}
	if mergeInProgress(context.Background(), f.dir) {
		t.Fatal("a merge is still in progress")
	}
}

// A conflicted rebase is the other way a worktree can be left mid-operation,
// and git sets no MERGE_HEAD for it: HEAD sits on the rebase target with the
// reviewed commits half replayed. The step must abort it back to the reviewed
// head before integrating, never merge on top of the partial replay.
func TestRebaseStep_AbortsAnInterruptedRebaseBeforeIntegrating(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)
	if err := runFixtureGit(f.dir, "rebase", "origin/main"); err == nil {
		t.Fatal("fixture rebase did not stop on its conflict")
	}
	if !rebaseInProgress(context.Background(), f.dir) {
		t.Fatal("fixture did not leave a rebase in progress")
	}
	sctx := f.context(t, noAgent(t))

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertConflictPark(t, outcome, "origin/main", "shared.txt")
	assertBranchUnchanged(t, sctx, f.dir, f.headSHA)
}

// The reviewed head staying an ancestor is the point of the merge shape: the CI
// step's continuity rule then holds by ancestry, with no patch-id or
// content-based guess, so a later repair can be published rather than paying a
// full revalidation cycle.
func TestRebaseStep_MergeKeepsReviewedHeadAncestorForCIRepairContinuity(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, noAgent(t))

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")

	// Review approved the pre-integration head.
	recordReviewApproval(t, sctx, f.headSHA)
	if gap := ciRepairContinuityGap(sctx, head); gap != "" {
		t.Fatalf("merged head does not continue the reviewed head: %s", gap)
	}
}

// Publication after a merge must append, never rewrite: an open PR's head and a
// review attestation bound to an exact SHA both depend on it.
func TestRebaseStep_MergePublishesAsAFastForwardWithoutForce(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	// The reviewed head is already published, as it is by the time anything
	// integrates a moved base mid-run.
	gitCmd(t, f.dir, "push", "origin", "feature")

	sctx := f.context(t, noAgent(t))
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")

	ctx := context.Background()
	gitRun := func(args ...string) (string, error) { return git.Run(ctx, f.dir, args...) }
	decision, err := resolveForcePushDecision(gitRun, f.upstream, "refs/heads/feature", head, f.headSHA, f.baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.fastForward {
		t.Fatalf("expected a fast-forward publication, got %#v", decision)
	}
	if decision.remoteSHA != f.headSHA {
		t.Fatalf("remote head = %s, want the reviewed head %s", decision.remoteSHA, f.headSHA)
	}
}

// runFixtureGit owns the fixture git environment and returns the failure
// rather than deciding what it means.
func runFixtureGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_EDITOR=true",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %s: %w", args, out, err)
	}
	return nil
}
