package gate

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mergeMirrorFixture builds the shape a merge-only gate leaves behind: the
// operator's branch head, a base that moved under it, and the gate's own merge
// of that base, published to the private mirror. It returns the work repo, the
// branch head, the base tip and the gate's merge commit.
func mergeMirrorFixture(t *testing.T) (work, branchHead, baseTip, gateMerge string) {
	t.Helper()
	work = initReconcileRepo(t)
	reconcileGit(t, work, "branch", "-M", "main")
	reconcileGit(t, work, "checkout", "-b", "feature")
	writeReconcileFile(t, work, "feature.txt", "feature work\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "feature work")
	branchHead = reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "main")
	writeReconcileFile(t, work, "main.txt", "main advanced\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "advance main")
	baseTip = reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-edit", "-m", "Merge origin/main", baseTip)
	gateMerge = reconcileGit(t, work, "rev-parse", "HEAD")
	return work, branchHead, baseTip, gateMerge
}

func mirrorAt(t *testing.T, work, head string) string {
	t.Helper()
	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, head+":refs/heads/feature")
	return gateDir
}

// The operator merges the same base by hand: the gate's clean merge differs
// from theirs only in SHA and message, so it is archived and the operator's
// head enters with an ordinary push.
func TestReconcileStaleBranchArchivesTheGatesCleanMergeOfABaseTheLiveHeadMerged(t *testing.T) {
	t.Parallel()
	work, branchHead, baseTip, gateMerge := mergeMirrorFixture(t)
	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", baseTip)
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
	if liveHead == gateMerge {
		t.Fatal("fixture reproduced the gate's merge commit")
	}
	gateDir := mirrorAt(t, work, gateMerge)

	result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
	if err != nil || !result.Reconciled {
		t.Fatalf("the gate's clean merge was held at risk: result=%+v err=%v", result, err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != gateMerge {
		t.Fatalf("archive = %s, want the gate's merge %s", got, gateMerge)
	}
	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != liveHead {
		t.Fatalf("ordinary push reached %s, want %s", got, liveHead)
	}
}

// A merge whose tree is not the clean merge of its parents carries content of
// its own. The live head carries the same extra line through an ordinary
// commit, so only the merge-content rule can hold the merge at risk.
func TestReconcileStaleBranchKeepsAMergeWithItsOwnContentAtRisk(t *testing.T) {
	t.Parallel()
	work, branchHead, baseTip, _ := mergeMirrorFixture(t)
	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-commit", baseTip)
	writeReconcileFile(t, work, "extra.txt", "written inside the merge\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "--no-edit", "-m", "Merge origin/main with an edit")
	evilMerge := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", baseTip)
	writeReconcileFile(t, work, "extra.txt", "written inside the merge\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "the same line as an ordinary commit")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
	gateDir := mirrorAt(t, work, evilMerge)

	result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
	if err == nil || result.Reconciled || !strings.Contains(err.Error(), evilMerge) {
		t.Fatalf("a merge with its own content was not held at risk by name: result=%+v err=%v", result, err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != evilMerge {
		t.Fatalf("refusal moved the private ref to %s", got)
	}
}

// A conflict resolution is content of the merge's own: the parents do not
// merge cleanly, so the merge is held at risk even when the live head
// resolved the same conflict the same way.
func TestReconcileStaleBranchKeepsAConflictResolutionMergeAtRisk(t *testing.T) {
	t.Parallel()
	work := initReconcileRepo(t)
	reconcileGit(t, work, "branch", "-M", "main")
	reconcileGit(t, work, "checkout", "-b", "feature")
	writeReconcileFile(t, work, "base.txt", "feature side\n")
	reconcileGit(t, work, "commit", "-am", "feature edits base")
	branchHead := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, work, "checkout", "main")
	writeReconcileFile(t, work, "base.txt", "main side\n")
	reconcileGit(t, work, "commit", "-am", "main edits base")
	baseTip := reconcileGit(t, work, "rev-parse", "HEAD")

	resolve := func(message string) string {
		t.Helper()
		reconcileGit(t, work, "checkout", "--detach", branchHead)
		if out, err := exec.Command("git", "-C", work, "merge", "--no-ff", "--no-edit", baseTip).CombinedOutput(); err == nil {
			t.Fatalf("fixture must conflict: %s", out)
		}
		writeReconcileFile(t, work, "base.txt", "resolved\n")
		reconcileGit(t, work, "add", "-A")
		reconcileGit(t, work, "commit", "--no-edit", "-m", message)
		return reconcileGit(t, work, "rev-parse", "HEAD")
	}
	gateMerge := resolve("Merge origin/main")
	liveHead := resolve("merge main by hand")
	gateDir := mirrorAt(t, work, gateMerge)

	result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
	if err == nil || result.Reconciled || !strings.Contains(err.Error(), gateMerge) {
		t.Fatalf("a conflict-resolution merge was not held at risk by name: result=%+v err=%v", result, err)
	}
}

// A merge with a parent the live head lacks is held at risk even when the
// live head carries that parent's change as a cherry-pick, so only the
// parent-reachability rule can refuse it.
func TestReconcileStaleBranchKeepsAMergeWithAParentTheLiveHeadLacksAtRisk(t *testing.T) {
	t.Parallel()
	work, branchHead, baseTip, _ := mergeMirrorFixture(t)
	reconcileGit(t, work, "checkout", "main")
	writeReconcileFile(t, work, "main-later.txt", "main advanced again\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "advance main again")
	laterBase := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-edit", "-m", "Merge origin/main", laterBase)
	gateMerge := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "--detach", branchHead)
	reconcileGit(t, work, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", baseTip)
	reconcileGit(t, work, "cherry-pick", laterBase)
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
	gateDir := mirrorAt(t, work, gateMerge)

	result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
	if err == nil || result.Reconciled {
		t.Fatalf("a merge with a parent the live head lacks was reconciled: result=%+v", result)
	}
	if !strings.Contains(err.Error(), gateMerge) || strings.Contains(err.Error(), laterBase) {
		t.Fatalf("refusal must name exactly the merge, not the cherry-picked base commit: %v", err)
	}
}
