package citest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// conflictRepairFixture is an open PR whose base moved after it was published:
// the forge reports the PR conflicting with its base, and the CI step's fix
// round must bring that base in by merging it, never by rewriting the branch.
type conflictRepairFixture struct {
	sctx      *pipeline.StepContext
	agent     *stepstest.MockAgent
	dir       string
	upstream  string
	headSHA   string
	mainTip   string
	gitTrace  string
	markCalls int
}

// newConflictRepairFixture publishes main and feature to a bare upstream,
// then moves main. When conflicting, main and feature both rewrite shared.txt
// so the base cannot merge cleanly; otherwise main touches an unrelated file.
func newConflictRepairFixture(t *testing.T, conflicting bool, checksJSON string, runAgent func(opts agent.RunOpts) (*agent.Result, error)) *conflictRepairFixture {
	t.Helper()
	upstream := t.TempDir()
	stepstest.GitCmd(t, upstream, "init", "--bare")
	// A receive into a bare repository may otherwise launch detached automatic
	// maintenance that races t.TempDir cleanup after the synchronous push exits.
	stepstest.GitCmd(t, upstream, "config", "gc.auto", "0")

	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	stepstest.GitCmd(t, dir, "config", "user.name", "test")
	stepstest.GitCmd(t, dir, "config", "user.email", "test@test.com")
	stepstest.GitCmd(t, dir, "checkout", "-b", "main")
	writeFile(t, dir, "shared.txt", "base\n")
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, dir, "remote", "add", "origin", upstream)
	stepstest.GitCmd(t, dir, "push", "origin", "main")

	stepstest.GitCmd(t, dir, "checkout", "-b", "feature")
	writeFile(t, dir, "feature.txt", "feature\n")
	if conflicting {
		writeFile(t, dir, "shared.txt", "feature edit\n")
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "feature")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, dir, "push", "origin", "feature")

	stepstest.GitCmd(t, dir, "checkout", "main")
	if conflicting {
		writeFile(t, dir, "shared.txt", "main edit\n")
	} else {
		writeFile(t, dir, "main.txt", "main moved\n")
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "main moves")
	mainTip := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, dir, "push", "origin", "main")
	stepstest.GitCmd(t, dir, "checkout", "feature")

	f := &conflictRepairFixture{dir: dir, upstream: upstream, headSHA: headSHA, mainTip: mainTip}
	f.agent = &stepstest.MockAgent{AgentName: "test", RunFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if runAgent == nil {
			t.Errorf("the CI repair invoked an agent for a merge conflict:\n%s", opts.Prompt)
			return &agent.Result{}, nil
		}
		return runAgent(opts)
	}}

	sctx := stepstest.NewTestContextWithDBRecords(t, f.agent, dir, baseSHA, headSHA, config.Commands{})
	f.gitTrace = filepath.Join(t.TempDir(), "git-trace.log")
	sctx.Env = append(stepstest.FakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING"),
		"FAKE_CLI_PR_HEAD_SHA="+headSHA,
		// attestHeadBeforePush discovers the PR before every publication, so
		// the fake forge must resolve the PR the run's PRURL names.
		`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`,
		// Every git child of the step logs its argv here, so the test can see
		// how the repair was pushed rather than only where the remote ended up.
		"GIT_TRACE="+f.gitTrace,
	)
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	// The worktree's real origin (the bare upstream) carries the git traffic;
	// this GitHub-shaped value only drives provider and repo-slug resolution.
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	sctx.MarkRunning = func() error { f.markCalls++; return nil }

	// The CI step only runs after Push succeeded, so the run reaches it with a
	// durable review approval and a recorded push binding.
	stepstest.RecordReviewApproval(t, sctx, headSHA)
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: headSHA, TargetKind: "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(upstream), Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	f.sctx = sctx
	return f
}

// drive runs the step the way the executor does: an observation, then the
// auto-fix round over what it selected. The monitor is stopped at its first
// wait, so a repair that resumed monitoring returns context.Canceled.
func (f *conflictRepairFixture) drive(t *testing.T) (*pipeline.StepOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.sctx.Ctx = ctx
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	return stepstest.ExecuteWithAutoFix(t, step, f.sctx, 0)
}

func (f *conflictRepairFixture) remoteHead(t *testing.T) string {
	return stepstest.GitCmd(t, f.upstream, "rev-parse", "refs/heads/feature")
}

// parents returns the commit's parents in order.
func parents(t *testing.T, dir, commit string) []string {
	t.Helper()
	fields := strings.Fields(stepstest.GitCmd(t, dir, "rev-list", "--parents", "-n", "1", commit))
	return fields[1:]
}

// pushes returns the argv of every git push the step ran.
func (f *conflictRepairFixture) pushes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.gitTrace)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "trace: built-in: git push") || strings.Contains(line, "trace: exec: git push") {
			out = append(out, line)
		}
	}
	return out
}

func (f *conflictRepairFixture) assertBranchUnchanged(t *testing.T) {
	t.Helper()
	if head := stepstest.GitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA {
		t.Fatalf("worktree head = %s, want it left on the pushed head %s", head, f.headSHA)
	}
	if status := stepstest.GitStatusPorcelain(t, f.dir); status != "" {
		t.Fatalf("worktree is not clean after the conflict:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".git", "MERGE_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a merge is still in progress in the worktree (MERGE_HEAD stat err = %v)", err)
	}
	if remote := f.remoteHead(t); remote != f.headSHA {
		t.Fatalf("remote feature = %s, want it untouched at %s", remote, f.headSHA)
	}
	if pushes := f.pushes(t); len(pushes) != 0 {
		t.Fatalf("a conflict must publish nothing, pushes:\n%s", strings.Join(pushes, "\n"))
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertPublishedWithoutForce(t *testing.T, f *conflictRepairFixture) {
	t.Helper()
	pushes := f.pushes(t)
	if len(pushes) == 0 {
		t.Fatal("no git push was traced; the repair was not published")
	}
	for _, push := range pushes {
		if strings.Contains(push, "--force") {
			t.Fatalf("the repair was force-pushed, want a fast-forward:\n%s", push)
		}
	}
}

// A forge-reported conflict whose base merges cleanly heals itself: the fix
// round merges the PR's base with a merge commit whose first parent is the
// published head, publishes it as a plain fast-forward, and goes back to
// monitoring - with no agent involved.
func TestCIStep_ConflictRepairMergesACleanBaseAsAFastForward(t *testing.T) {
	t.Parallel()
	f := newConflictRepairFixture(t, false, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, nil)

	outcome, err := f.drive(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome = %#v err = %v, want the monitor resumed after the published merge", outcome, err)
	}
	remote := f.remoteHead(t)
	if remote == f.headSHA {
		t.Fatal("the merged base was not published")
	}
	if got := parents(t, f.dir, remote); len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainTip {
		t.Fatalf("published head parents = %v, want a merge commit of [%s %s]", got, f.headSHA, f.mainTip)
	}
	if local := stepstest.GitCmd(t, f.dir, "rev-parse", "HEAD"); local != remote {
		t.Fatalf("worktree head = %s, want the published merge %s", local, remote)
	}
	if f.sctx.Run.HeadSHA != remote {
		t.Fatalf("run head = %s, want the published merge %s", f.sctx.Run.HeadSHA, remote)
	}
	assertPublishedWithoutForce(t, f)
	if f.markCalls != 1 {
		t.Fatalf("MarkRunning calls = %d, want one after the published repair", f.markCalls)
	}
	if len(f.agent.Calls) != 0 {
		t.Fatalf("agent invocations = %d, want none", len(f.agent.Calls))
	}
}

// A base that conflicts is never resolved by the gate: the round parks with
// one ask-user finding per conflicted file, leaves the branch exactly where it
// was, and invokes no agent.
func TestCIStep_ConflictRepairParksOnAConflictingBase(t *testing.T) {
	t.Parallel()
	f := newConflictRepairFixture(t, true, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, nil)

	outcome, err := f.drive(t)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	assertConflictPark(t, outcome, "build")
	f.assertBranchUnchanged(t)
	if len(f.agent.Calls) != 0 {
		t.Fatalf("agent invocations = %d, want none", len(f.agent.Calls))
	}
}

// assertConflictPark checks a park the auto-fix loop cannot re-enter, naming
// shared.txt in an ask-user finding with the operator's remedy, and every
// selected finding kept as ask-user.
func assertConflictPark(t *testing.T, outcome *pipeline.StepOutcome, wantNoCheck string) {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want an ask-user park", outcome)
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	var fileFinding *types.Finding
	sawSelectedConflict := false
	for i, item := range findings.Items {
		if item.Action != types.ActionAskUser {
			t.Errorf("finding %+v is not ask-user", item)
		}
		if item.File == "shared.txt" {
			fileFinding = &findings.Items[i]
		}
		if item.File == "" && item.Category == types.FindingCategoryCIMergeConflict {
			sawSelectedConflict = true
		}
		if wantNoCheck != "" && item.Check == wantNoCheck {
			t.Errorf("finding %+v names a check that was green", item)
		}
	}
	if fileFinding == nil {
		t.Fatalf("findings = %+v, want shared.txt named", findings.Items)
	}
	for _, want := range []string{"origin/main", "merge origin/main into your branch locally, resolve, and push", "does not resolve merge conflicts"} {
		if !strings.Contains(fileFinding.Description, want) {
			t.Errorf("conflict finding description %q lacks %q", fileFinding.Description, want)
		}
	}
	if !sawSelectedConflict {
		t.Errorf("findings = %+v, want the selected merge-conflict finding kept", findings.Items)
	}
}

// A round that selected a failing check and the conflict merges first. When
// the base conflicts it parks before the agent runs, keeping the check finding
// for the human alongside the conflicted file.
func TestCIStep_CheckAndConflictRepairParksBeforeTheAgentOnAConflictingBase(t *testing.T) {
	t.Parallel()
	f := newConflictRepairFixture(t, true, `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, nil)

	outcome, err := f.drive(t)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	assertConflictPark(t, outcome, "")
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	sawCheck := false
	for _, item := range findings.Items {
		if item.Check == "test" {
			sawCheck = true
		}
	}
	if !sawCheck {
		t.Fatalf("findings = %+v, want the selected failing check kept on the park", findings.Items)
	}
	f.assertBranchUnchanged(t)
	if len(f.agent.Calls) != 0 {
		t.Fatalf("agent invocations = %d, want none before a conflict parks", len(f.agent.Calls))
	}
}

// When the base merges cleanly, the agent repairs the failing check on the
// merged head, and the merge plus its repair publish as one fast-forward.
func TestCIStep_CheckAndConflictRepairRunsTheAgentOnTheMergedHead(t *testing.T) {
	t.Parallel()
	var f *conflictRepairFixture
	var agentHead []string
	f = newConflictRepairFixture(t, false, `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, func(opts agent.RunOpts) (*agent.Result, error) {
		agentHead = parents(t, opts.CWD, "HEAD")
		if strings.Contains(strings.ToLower(opts.Prompt), "rebase") {
			t.Errorf("CI repair prompt mentions rebase:\n%s", opts.Prompt)
		}
		if strings.Contains(opts.Prompt, "merge conflict") {
			t.Errorf("CI repair prompt still hands the agent the merge conflict:\n%s", opts.Prompt)
		}
		writeFile(t, opts.CWD, "fix.txt", "fixed\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair the failing test","code_change_needed":true}`)}, nil
	})

	outcome, err := f.drive(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome = %#v err = %v, want the monitor resumed after the published repair", outcome, err)
	}
	if len(f.agent.Calls) != 1 {
		t.Fatalf("agent invocations = %d, want one check repair", len(f.agent.Calls))
	}
	if len(agentHead) != 2 || agentHead[0] != f.headSHA || agentHead[1] != f.mainTip {
		t.Fatalf("agent ran on a head with parents %v, want the merge of [%s %s]", agentHead, f.headSHA, f.mainTip)
	}
	remote := f.remoteHead(t)
	repairParents := parents(t, f.dir, remote)
	if len(repairParents) != 1 {
		t.Fatalf("published head parents = %v, want the repair commit on top of the merge", repairParents)
	}
	if got := parents(t, f.dir, repairParents[0]); len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainTip {
		t.Fatalf("repair sits on %v, want the merge of [%s %s]", got, f.headSHA, f.mainTip)
	}
	assertPublishedWithoutForce(t, f)
	if f.markCalls != 1 {
		t.Fatalf("MarkRunning calls = %d, want one after the published repair", f.markCalls)
	}
}

// No CI repair prompt says "rebase" (the test name avoids the word because the
// prompt carries the test's temp path): the merge is the step's own
// deterministic act, and the fixer only ever repairs failing checks or the
// findings a human selected.
func TestCIStep_RepairPromptsNeverAskTheFixerToIntegrate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		findings string
	}{
		{name: "failing_check", findings: stepstest.CIGateFindingsJSON("test")},
		{name: "selected_finding_without_check", findings: `{"findings":[{"id":"ci-1","severity":"warning","action":"auto-fix","category":"ci-review-bot","description":"tighten the error message"}]}`},
		{name: "failing_check_and_conflict", findings: `{"findings":[{"id":"ci-1","severity":"error","action":"auto-fix","category":"ci-check","check":"test","description":"CI check failing: test"},{"id":"ci-2","severity":"error","action":"auto-fix","category":"ci-merge-conflict","description":"PR has merge conflicts with the base branch"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var prompt string
			f := newConflictRepairFixture(t, false, `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, func(opts agent.RunOpts) (*agent.Result, error) {
				prompt = opts.Prompt
				return &agent.Result{Output: json.RawMessage(`{"summary":"nothing to change","code_change_needed":false}`)}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.sctx.Ctx = ctx
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = tc.findings
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
				cancel()
				return ctx.Err()
			})
			_, _ = step.Execute(f.sctx)
			if prompt == "" {
				t.Fatal("the CI repair ran no agent turn")
			}
			if strings.Contains(strings.ToLower(prompt), "rebase") {
				t.Fatalf("CI repair prompt mentions rebase:\n%s", prompt)
			}
		})
	}
}

// A worktree an interrupted attempt left mid-merge is put back on its head when
// the step starts, so the conflict repair merges from a clean worktree instead
// of failing on the leftover.
func TestCIStep_InterruptedMergeIsAbortedBeforeTheConflictRepair(t *testing.T) {
	t.Parallel()
	f := newConflictRepairFixture(t, true, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, nil)
	stepstest.GitCmd(t, f.dir, "fetch", "origin", "main")
	if _, err := stepstest.RunGitDirect(f.dir, "merge", "--no-ff", "--no-edit", "FETCH_HEAD"); err == nil {
		t.Fatal("fixture merge must stop on its conflict")
	}

	outcome, err := f.drive(t)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	assertConflictPark(t, outcome, "build")
	f.assertBranchUnchanged(t)
}
