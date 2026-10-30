//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys drive the Rebase step's merge-only integration through the
// real binary: a real gate, a real daemon, a real base branch that moved, and
// a real remote. The unit tests in internal/pipeline/steps/rebase_merge_test.go
// drive the step in isolation; what only a journey can show is the shape the
// pipeline actually PUBLISHES, and that a remote which refuses to have its
// branch rewritten accepts it.

// harnessRepoConfigWithExtra rebuilds the harness's default-branch
// .no-mistakes.yaml with extra appended. rebase.strategy is trusted-only, so a
// journey has to set it the way a maintainer does: committed on the default
// branch, never on the pushed branch.
func harnessRepoConfigWithExtra(extra string) string {
	return "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\n" + extra
}

// commitTrustedRepoConfig commits a .no-mistakes.yaml on main and pushes it to
// origin, which is where the daemon reads the trusted copy from.
func commitTrustedRepoConfig(t *testing.T, h *Harness, extra string) {
	t.Helper()
	h.CommitChange("main", ".no-mistakes.yaml", harnessRepoConfigWithExtra(extra), "set trusted repo config")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted repo config: %v\n%s", err, out)
	}
}

// initFromOwnWorktree runs `no-mistakes init` off a branch of its own so the
// journey's feature branch is never the one init was standing on.
func initFromOwnWorktree(t *testing.T, h *Harness, name string) {
	t.Helper()
	h.CommitChange(name, name+".txt", "init\n", "seed "+name)
	wt := h.AddWorktree(name)
	if out, err := h.RunInDir(wt, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
}

// advanceMain moves the default branch under the gated branch and publishes it,
// which is the only condition under which the rebase step integrates anything.
func advanceMain(t *testing.T, h *Harness, path, content, message string) string {
	t.Helper()
	sha := h.CommitChange("main", path, content, message)
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("advance origin/main: %v\n%s", err, out)
	}
	return sha
}

func gitIn(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isAncestorIn(t *testing.T, h *Harness, dir, ancestor, descendant string) bool {
	t.Helper()
	_, err := h.runGit(context.Background(), dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// parentsOf returns a commit's parent SHAs in order, so first-parent identity
// can be asserted rather than inferred.
func parentsOf(t *testing.T, h *Harness, dir, rev string) []string {
	t.Helper()
	fields := strings.Fields(gitIn(t, h, dir, "rev-list", "--parents", "-n", "1", rev))
	if len(fields) == 0 {
		t.Fatalf("no commit for %s in %s", rev, dir)
	}
	return fields[1:]
}

// mergeOfOnFirstParentChain finds the merge commit that joined wantFirst and
// wantSecond on rev's first-parent chain. Walking the chain rather than
// inspecting rev itself keeps the assertion exact while still tolerating a
// pipeline commit landing on top of the integration.
func mergeOfOnFirstParentChain(t *testing.T, h *Harness, dir, rev, wantFirst, wantSecond string) string {
	t.Helper()
	for _, commit := range strings.Fields(gitIn(t, h, dir, "rev-list", "--first-parent", rev)) {
		parents := parentsOf(t, h, dir, commit)
		if len(parents) == 2 && parents[0] == wantFirst && parents[1] == wantSecond {
			return commit
		}
	}
	return ""
}

// traceGitCommands makes every git process of this test - the daemon's
// included, which inherits the environment through its login-shell probe -
// append its argv to a trace file, and returns that file. It is how a journey
// sees the exact flags the pipeline pushed with, which the remote cannot.
func traceGitCommands(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", path)
	return path
}

// tracedPushes returns the traced `git push` invocations aimed at target.
func tracedPushes(t *testing.T, tracePath, target string) []string {
	t.Helper()
	f, err := os.Open(tracePath)
	if err != nil {
		t.Fatalf("open git trace: %v", err)
	}
	defer f.Close()
	var pushes []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "trace: built-in: git push ") && strings.Contains(line, target) {
			pushes = append(pushes, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	return pushes
}

// TestRebaseMergesAMovedBaseJourney is the default-config contract end to end.
// The branch is already published (the case an open PR is in) and the remote
// refuses any non-fast-forward update to it; the default branch then moves
// under the branch. The run must bring the base in with a merge commit whose
// FIRST parent is the head the operator submitted, so every commit already on
// the remote keeps its SHA, and the pipeline must publish it without --force or
// --force-with-lease.
func TestRebaseMergesAMovedBaseJourney(t *testing.T) {
	trace := traceGitCommands(t)
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-merge-only")

	const branch = "feature/merge-only"
	first := h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")
	submitted := h.CommitChange(branch, "feature-more.txt", "more feature work\n", "add more feature work")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("pre-publish %s: %v\n%s", branch, err, out)
	}
	gitIn(t, h, h.UpstreamDir, "config", "receive.denyNonFastForwards", "true")

	advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	published := h.UpstreamBranchSHA(branch)
	mainSHA := h.UpstreamBranchSHA("main")
	if published != run.HeadSHA {
		t.Fatalf("published head %s != run head %s", published, run.HeadSHA)
	}
	if published == submitted {
		t.Fatalf("nothing was integrated: published head still the submitted head %s", submitted)
	}
	for _, kept := range []string{first, submitted} {
		if !isAncestorIn(t, h, h.UpstreamDir, kept, published) {
			t.Fatalf("published commit %s lost its SHA: not an ancestor of the published head %s\n%s",
				kept, published, gitIn(t, h, h.UpstreamDir, "log", "--graph", "--format=%h %p %s", "-8", published))
		}
	}
	if mergeOfOnFirstParentChain(t, h, h.UpstreamDir, published, submitted, mainSHA) == "" {
		t.Fatalf("published head %s carries no merge commit with parents [%s, %s]:\n%s",
			published, submitted, mainSHA,
			gitIn(t, h, h.UpstreamDir, "log", "--format=%h %p %s", "-8", published))
	}
	files := gitIn(t, h, h.UpstreamDir, "ls-tree", "--name-only", "-r", published)
	for _, want := range []string{"feature.txt", "feature-more.txt", "main-advance.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("published tree is missing %s:\n%s", want, files)
		}
	}

	pushes := tracedPushes(t, trace, h.UpstreamDir)
	if len(pushes) == 0 {
		t.Fatalf("git trace recorded no push to %s; the no-force check would prove nothing", h.UpstreamDir)
	}
	for _, push := range pushes {
		if strings.Contains(push, "--force") || strings.Contains(push, " -f ") || strings.Contains(push, " +") {
			t.Errorf("a push rewrote the remote branch: %s", push)
		}
	}
}

// conflictingBase puts shared.txt on a collision course between the branch and
// the moved base, publishes the branch, and returns its submitted head.
func conflictingBase(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	advanceMain(t, h, "shared.txt", "base\n", "add the shared file")
	submitted := h.CommitChange(branch, "shared.txt", "base\nfeature change\n", "change the shared file on the branch")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("pre-publish %s: %v\n%s", branch, err, out)
	}
	advanceMain(t, h, "shared.txt", "base\nmain change\n", "change the shared file on main")
	h.Checkout(branch)
	return submitted
}

// requireConflictPark asserts the Rebase gate parked for a human with one
// ask-user finding naming the conflicted file, the target, and the remedy.
func requireConflictPark(t *testing.T, run *ipc.RunInfo) {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepRebase)
	if !ok || step.FindingsJSON == nil {
		t.Fatalf("rebase conflict gate recorded no findings: %+v", run.Steps)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse rebase conflict findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("conflict findings = %+v, want exactly one for shared.txt", findings.Items)
	}
	finding := findings.Items[0]
	if finding.File != "shared.txt" || finding.Action != types.ActionAskUser {
		t.Fatalf("conflict finding = %+v, want an ask-user finding on shared.txt", finding)
	}
	for _, want := range []string{"origin/main", "merge", "resolve", "push", "does not resolve merge conflicts"} {
		if !strings.Contains(finding.Description, want) {
			t.Errorf("conflict finding description is missing %q: %q", want, finding.Description)
		}
	}
}

// TestRebaseConflictParksWithTheBranchUnchangedJourney is the conflict path:
// the moved base does not merge cleanly. The run parks at the Rebase step with
// the conflicted file named for the operator to merge by hand, no agent is
// asked to resolve anything, the branch is left exactly where it was in the
// run worktree, the gate and the remote, and selecting fix only re-runs the
// same deterministic merge.
func TestRebaseConflictParksWithTheBranchUnchangedJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-merge-conflict")

	const branch = "feature/merge-conflict"
	submitted := conflictingBase(t, h, branch)
	h.PushToGate(branch)

	run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
	requireConflictPark(t, run)
	invocationsAtPark := h.AgentInvocations()
	if sawPromptContaining(invocationsAtPark, "conflict") {
		t.Fatal("an agent was asked to resolve the merge conflict")
	}

	assertBranchUnchanged := func(when string) {
		t.Helper()
		worktree := filepath.Join(h.NMHome, "worktrees", h.repoID(), run.ID)
		if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != submitted {
			t.Fatalf("%s: run worktree HEAD = %s, want the submitted head %s", when, got, submitted)
		}
		if status := gitIn(t, h, worktree, "status", "--porcelain"); status != "" {
			t.Fatalf("%s: run worktree is not clean:\n%s", when, status)
		}
		gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
		if got := gitIn(t, h, gateDir, "rev-parse", "refs/heads/"+branch); got != submitted {
			t.Fatalf("%s: gate branch = %s, want the submitted head %s", when, got, submitted)
		}
		if got := h.UpstreamBranchSHA(branch); got != submitted {
			t.Fatalf("%s: remote branch = %s, want the submitted head %s", when, got, submitted)
		}
		if info := h.RunInfo(run.ID); info.HeadSHA != submitted {
			t.Fatalf("%s: run head = %s, want the submitted head %s", when, info.HeadSHA, submitted)
		}
	}
	assertBranchUnchanged("at the conflict park")

	// Fix re-runs the deterministic merge, which conflicts again: the gate
	// re-parks with the same file and still no agent runs.
	h.Respond(run.ID, types.StepRebase, types.ActionFix)
	run = waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusFixReview, 60*time.Second)
	requireConflictPark(t, run)
	if got := len(h.AgentInvocations()); got != len(invocationsAtPark) {
		t.Fatalf("the fix round invoked an agent: %d invocations, want %d", got, len(invocationsAtPark))
	}
	assertBranchUnchanged("after the fix round")

	h.Respond(run.ID, types.StepRebase, types.ActionAbort)
	if done := h.WaitForRun(branch, 60*time.Second); done.Status != types.RunFailed {
		t.Fatalf("run status after abort = %s, want failed", done.Status)
	}
}

// TestRebaseStrategyRebaseFailsTheRunClosedJourney is the retirement of the
// rewriting shape: a maintainer's default-branch config that still says
// `rebase.strategy: rebase` must refuse the run with a message saying the gate
// only merges, never degrade to an absent trusted config and quietly rebase.
func TestRebaseStrategyRebaseFailsTheRunClosedJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: rebase\n")
	initFromOwnWorktree(t, h, "init-retired-strategy")

	const branch = "feature/retired-strategy"
	// The pushed branch itself carries a VALID config, so the only refused copy
	// in play is the trusted one.
	h.CommitChange(branch, ".no-mistakes.yaml", harnessRepoConfigWithExtra(""), "restore a valid pushed config")
	submitted := h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")

	advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status == types.RunCompleted {
		t.Fatalf("run completed under a retired trusted rebase.strategy (head %s)", run.HeadSHA)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "unparseable") || !strings.Contains(*run.Error, "only by merging") {
		t.Fatalf("run error = %v, want it to name the refused trusted config and say the gate only merges", deref(run.Error))
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("a run that failed closed still published %s", branch)
	}
	if got := h.WorktreeRefSHA(branch); got != submitted {
		t.Fatalf("local branch moved after a fail-closed run: %s, want %s", got, submitted)
	}
}
