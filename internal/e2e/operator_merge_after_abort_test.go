//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunStartsAfterAnOperatorMergesTheBaseByHandJourney is the T117 sequence
// with the merge-only Rebase step: a run merges a moved base, the operator
// aborts it before it publishes anything, merges the same base into their own
// branch by hand, and pushes. The next `axi run` must start and get through
// Rebase, with nothing edited under NM_HOME.
func TestRunStartsAfterAnOperatorMergesTheBaseByHandJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: baseMergeCustodyScenario(t)})
	initFromOwnWorktree(t, h, "init-operator-merge")

	const branch = "feature/operator-merge"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("pre-publish %s: %v\n%s", branch, err, out)
	}
	base := advanceMain(t, h, "upstream-advance.txt", "advance\n", "advance main under the branch")

	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature on top of the moved base")
	if err != nil || !strings.Contains(gateOut, "merge-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	first := h.ActiveRun(branch)
	if first == nil {
		t.Fatal("no active run at the review gate")
	}
	if first.HeadSHA == submitted || !isAncestorIn(t, h, gateDir, submitted, first.HeadSHA) {
		t.Fatalf("first run head %s is not a merge on top of the submitted head %s", first.HeadSHA, submitted)
	}
	if abortOut, abortErr := h.RunInDir(operator, "axi", "abort"); abortErr != nil {
		t.Fatalf("axi abort: %v\n%s", abortErr, abortOut)
	}
	if done := h.WaitForRun(branch, 30*time.Second); done.Status != types.RunCancelled {
		t.Fatalf("first run status after abort = %s, want cancelled", done.Status)
	}
	if got := h.UpstreamBranchSHA(branch); got != submitted {
		t.Fatalf("the aborted run published %s; want the remote still at the submitted head %s", got, submitted)
	}

	// The operator's own merge of the same base: same parents, same tree, a
	// different commit from the one the aborted run made.
	gitIn(t, h, operator, "fetch", "origin", "main")
	gitIn(t, h, operator, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", "origin/main")
	merged := gitIn(t, h, operator, "rev-parse", "HEAD")
	if merged == first.HeadSHA {
		t.Fatal("the operator's merge reproduced the run's merge commit; the journey proves nothing")
	}
	if got := parentsOf(t, h, operator, merged); len(got) != 2 || got[0] != submitted || got[1] != base {
		t.Fatalf("operator merge parents = %v, want [%s %s]", got, submitted, base)
	}
	gitIn(t, h, operator, "push", "origin", branch)

	nextOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the hand-merged branch")
	if err != nil || !strings.Contains(nextOut, "gate:") {
		t.Fatalf("the next run did not start after the operator's merge and push: %v\n%s", err, nextOut)
	}
	next := h.ActiveRun(branch)
	if next == nil || next.ID == first.ID {
		t.Fatalf("no new run for the hand-merged head: %+v", next)
	}
	step, ok := findStep(next.Steps, types.StepRebase)
	if !ok || step.Status != types.StepStatusCompleted {
		t.Fatalf("the next run's Rebase step = %+v, want completed", step)
	}
	if next.HeadSHA != merged {
		t.Fatalf("the next run's head = %s, want the operator's merge %s untouched", next.HeadSHA, merged)
	}
	if got := gitIn(t, h, gateDir, "rev-parse", "refs/heads/"+branch); got != merged {
		t.Fatalf("gate branch = %s, want the operator's merge %s", got, merged)
	}
}

// TestRunAfterAnAbortedFixRoundNamesTheRecoveryThenStartsJourney is the same
// sequence when the aborted run also holds an unpublished review fix commit
// that the operator's hand merge does not contain. That commit is at risk, so
// `axi run` refuses, and the refusal names `no-mistakes axi status` and the
// exact command that settles it. Following that command, then running again,
// starts a run that carries both the operator's merged base and the fix.
func TestRunAfterAnAbortedFixRoundNamesTheRecoveryThenStartsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: baseMergeCustodyScenario(t)})
	initFromOwnWorktree(t, h, "init-operator-merge-fix")

	const branch = "feature/operator-merge-fix"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("pre-publish %s: %v\n%s", branch, err, out)
	}
	advanceMain(t, h, "upstream-advance.txt", "advance\n", "advance main under the branch")

	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature on top of the moved base")
	if err != nil || !strings.Contains(gateOut, "merge-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "merge-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	if abortOut, abortErr := h.RunInDir(operator, "axi", "abort"); abortErr != nil {
		t.Fatalf("axi abort: %v\n%s", abortErr, abortOut)
	}
	first := h.WaitForRun(branch, 30*time.Second)
	if first.Status != types.RunCancelled {
		t.Fatalf("first run status after abort = %s, want cancelled", first.Status)
	}
	if got := h.UpstreamBranchSHA(branch); got != submitted {
		t.Fatalf("the aborted run published %s; want the remote still at the submitted head %s", got, submitted)
	}

	gitIn(t, h, operator, "fetch", "origin", "main")
	gitIn(t, h, operator, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", "origin/main")
	gitIn(t, h, operator, "push", "origin", branch)

	refusal, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the hand-merged branch")
	if err == nil {
		t.Fatalf("a run started over the aborted run's unpublished fix commit:\n%s", refusal)
	}
	const recoverCommand = "no-mistakes axi sync --recover"
	for _, want := range []string{"no-mistakes axi status", "command: " + recoverCommand + "\n", "code: recover_custody"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, refusal)
		}
	}
	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil || !strings.Contains(statusOut, "command: "+recoverCommand+"\n") {
		t.Fatalf("axi status does not name the recovery command: %v\n%s", err, statusOut)
	}

	recoverOut, err := h.RunInDir(operator, strings.Fields(recoverCommand)[1:]...)
	if err != nil || !strings.Contains(recoverOut, "recovered: true") {
		t.Fatalf("following the named command did not return custody: %v\n%s", err, recoverOut)
	}
	if got := gitIn(t, h, operator, "rev-parse", "HEAD"); got != first.HeadSHA {
		t.Fatalf("operator HEAD after recovery = %s, want the preserved head %s", got, first.HeadSHA)
	}
	for _, name := range []string{"feature.txt", "upstream-advance.txt", "guard.txt"} {
		if gitIn(t, h, operator, "ls-files", name) == "" {
			t.Fatalf("the recovered head lacks %s", name)
		}
	}

	nextOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the recovered branch")
	if err != nil || !strings.Contains(nextOut, "gate:") {
		t.Fatalf("no run started after following the named command: %v\n%s", err, nextOut)
	}
	next := h.ActiveRun(branch)
	if next == nil || next.ID == first.ID || next.SubmittedHeadSHA == nil || *next.SubmittedHeadSHA != first.HeadSHA {
		t.Fatalf("the next run = %+v, want a new run submitted at the recovered head %s", next, first.HeadSHA)
	}
	// The operator's own merge is on the remote branch, so the new run's
	// Rebase step merges it in; its content is already there, so it merges
	// cleanly.
	if step, ok := findStep(next.Steps, types.StepRebase); !ok || step.Status != types.StepStatusCompleted {
		t.Fatalf("the next run's Rebase step = %+v, want completed", step)
	}
}
