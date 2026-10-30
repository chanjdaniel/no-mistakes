package branchsync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newBaseMergeRecoverFixture builds what an aborted merge-only run leaves
// behind: the operator's feature/recover at the submitted head, a default
// branch that moved after submission, and a cancelled run whose Rebase step
// merged that base (plus whatever pipelineWork commits on top). The preserved
// head exists only in the gate, pinned at the run's recovery ref; the gate
// branch itself stays at the submitted head because nothing was published.
func newBaseMergeRecoverFixture(t *testing.T, pipelineWork func(t *testing.T, pipelineDir string)) *recoverFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "feature.txt"), "feature\n")
	mustRun(t, local, "add", "feature.txt")
	mustRun(t, local, "commit", "-m", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "merge", "--no-ff", "--no-edit", "-m", "Merge origin/main", "origin/main")
	if pipelineWork != nil {
		pipelineWork(t, pipeline)
	}
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, pipeline, "push", "origin", "HEAD:"+custody.RecoveryRef(run.ID))
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
}

// mergeMainByHand is the operator's own merge of the moved base: the same
// parents and tree as the run's merge, a different commit.
func (f *recoverFixture) mergeMainByHand(t *testing.T) string {
	t.Helper()
	mustRun(t, f.local, "merge", "--no-ff", "--no-edit", "-m", "merge main by hand", "main")
	merged := mustRun(t, f.local, "rev-parse", "HEAD")
	if merged == f.preserved {
		t.Fatal("the operator's merge reproduced the run's commit; the fixture proves nothing")
	}
	return merged
}

func addGuardCommit(t *testing.T, pipelineDir string) {
	t.Helper()
	mustWrite(t, filepath.Join(pipelineDir, "guard.txt"), "guard\n")
	mustRun(t, pipelineDir, "add", "guard.txt")
	mustRun(t, pipelineDir, "commit", "-m", "no-mistakes(review): add a guard")
}

func TestRunWhoseOnlyCommitIsTheBaseMergeTheOperatorAlsoMadeReleasesTheBranch(t *testing.T) {
	t.Parallel()
	f := newBaseMergeRecoverFixture(t, nil)
	if state := f.service.InspectCached(f.ctx); state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("before the operator merges, the run's merge must stay recoverable: %s/%s", state.State, state.Safety)
	}
	merged := f.mergeMainByHand(t)
	gateRefs := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)")

	state := f.service.InspectCached(f.ctx)
	if state.State != StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil {
		t.Fatalf("a run whose only commit is the operator's own merge still holds the branch: %#v", state)
	}
	if state.Local.Head != merged || state.Pipeline.RunID != f.run.ID {
		t.Fatalf("released state reports local %s run %s, want %s and %s", state.Local.Head, state.Pipeline.RunID, merged, f.run.ID)
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)"); got != gateRefs {
		t.Fatalf("inspection changed gate refs:\n%s\nwant\n%s", got, gateRefs)
	}
	if recovered := f.service.Recover(f.ctx, false); !recovered.Recovered || recovered.Changed {
		t.Fatalf("recovery on the released branch is not an idempotent no-op: %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != merged {
		t.Fatalf("recovery moved HEAD to %s", got)
	}
	// The gate branch is still the submitted head, so the operator's head
	// enters with an ordinary fast-forward push.
	mustRun(t, f.local, "push", f.gate, "HEAD:refs/heads/feature/recover")
}

func TestRunWithAFixCommitBeyondTheMergeStaysRecoverableAndAdopts(t *testing.T) {
	t.Parallel()
	f := newBaseMergeRecoverFixture(t, addGuardCommit)
	merged := f.mergeMainByHand(t)

	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" ||
		state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("an unpublished fix commit beside the operator's merge must offer plain recovery: %#v", state)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed || recovered.State != StateCustodyReturned {
		t.Fatalf("following the offered recovery did not return custody: %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want the preserved head %s", got, f.preserved)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != merged {
		t.Fatalf("the operator's own merge was not anchored before the move: %s", got)
	}
	mustRun(t, f.local, "push", f.gate, "HEAD:refs/heads/feature/recover")
}

func TestRunMergeOfABaseTheOperatorDidNotMergeStaysRecoverable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		operate func(t *testing.T, f *recoverFixture)
	}{
		{name: "operator never merged", operate: func(*testing.T, *recoverFixture) {}},
		{name: "operator merged an older base", operate: func(t *testing.T, f *recoverFixture) {
			older := mustRun(t, f.local, "rev-parse", "main~1")
			mustRun(t, f.local, "merge", "--no-ff", "--no-edit", "-m", "merge an older base", older)
			mustWrite(t, filepath.Join(f.local, "local.txt"), "local work\n")
			mustRun(t, f.local, "add", "local.txt")
			mustRun(t, f.local, "commit", "-m", "local work")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBaseMergeRecoverFixture(t, nil)
			tc.operate(t, f)
			state := f.service.InspectCached(f.ctx)
			if state.State != StatePipelineOwned || state.Safety == "user_owned" {
				t.Fatalf("the run's merge of a base the local head lacks released the branch: %#v", state)
			}
		})
	}
}

// TestLegacyRebasedMirrorHeadIsResolvedByTheNamedCommand is the T117 state an
// older binary left: an aborted run whose Rebase step REBASED the branch, so
// the gate holds rebased copies of the operator's commits. The operator then
// merges the moved base into their original history by hand, which the gate
// has never seen. Inspection must name a command, and following it must leave
// a head the gate takes with an ordinary push.
func TestLegacyRebasedMirrorHeadIsResolvedByTheNamedCommand(t *testing.T) {
	t.Parallel()
	for _, keepLocal := range []bool{false, true} {
		name := "recover"
		if keepLocal {
			name = "recover --keep-local"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRebasedRecoverFixture(t, types.RunCancelled)
			merged := f.mergeMainByHand(t)
			if objectExists(f.ctx, f.gate, merged) {
				t.Fatal("fixture leaked the operator's merge into the gate")
			}

			state := f.service.InspectCached(f.ctx)
			if state.NextAction == nil || state.NextAction.Code != "recover_custody" || state.NextAction.Command != "no-mistakes axi sync --recover" {
				t.Fatalf("the legacy rebased head names no recovery command: %#v", state)
			}
			recovered := f.service.Recover(f.ctx, keepLocal)
			if !recovered.Recovered {
				t.Fatalf("following the named command did not return custody: %#v", recovered)
			}
			want := f.preserved
			if keepLocal {
				want = merged
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != want {
				t.Fatalf("HEAD after recovery = %s, want %s", got, want)
			}
			if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
				t.Fatalf("the rebased head is no longer preserved: %s", got)
			}
			mustRun(t, f.local, "push", f.gate, "HEAD:refs/heads/feature/recover")
			if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != want {
				t.Fatalf("gate branch after an ordinary push = %s, want %s", got, want)
			}
		})
	}
}
