package steps

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// testStallWindow is short enough for a unit test and far below the budget
// these tests set, so a park inside the budget can only be the stall window.
const (
	testStallWindow = 40 * time.Millisecond
	testStallBudget = time.Minute
)

// stallingTestContext is a Test step context whose agent invocations run under
// testStallWindow, with every log line recorded.
func stallingTestContext(t *testing.T, ag agent.Agent, commands config.Commands) (*pipeline.StepContext, *testLogs) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, commands)
	sctx.Config.TestAgentTimeout = testStallBudget
	sctx.Config.TestAgentStallTimeout = testStallWindow
	logs := &testLogs{}
	sctx.Log = logs.add
	return sctx, logs
}

type testLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLogs) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *testLogs) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// silentUntilCut never emits and never returns before its context ends.
func silentUntilCut(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// emitsThenSilent streams one chunk, then goes silent until its context ends.
func emitsThenSilent(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.OnChunk("starting the product\n")
	<-ctx.Done()
	return nil, ctx.Err()
}

// assertStallPark checks the outcome is the Test budget-cut park carrying the
// stall finding and not the budget finding.
func assertStallPark(t *testing.T, outcome *pipeline.StepOutcome, start time.Time) types.Finding {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the Test step parked for a decision", outcome)
	}
	if elapsed := time.Since(start); elapsed >= testStallBudget {
		t.Fatalf("park took %s, want it within the stall windows, not the %s budget", elapsed, testStallBudget)
	}
	stall := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentStall)
	if stall.Action != types.ActionAskUser || stall.Severity != types.FindingSeverityWarning {
		t.Fatalf("stall finding = %+v, want an ask-user warning", stall)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestAgentTimeout {
			t.Fatalf("findings = %s, a stall must not be reported as an expired budget", outcome.Findings)
		}
	}
	if findings.Verdict == types.TestVerdictGo {
		t.Fatal("a stalled turn must not complete Test as a pass")
	}
	return stall
}

func TestTestStep_SilentEvidenceAgentIsRetriedOnceThenParksAsAStall(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "silent", runFn: silentUntilCut}
	sctx, logs := stallingTestContext(t, ag, config.Commands{})

	start := time.Now()
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked stall", err)
	}
	stall := assertStallPark(t, outcome, start)
	if len(ag.calls) != 2 {
		t.Fatalf("evidence invocations = %d, want the silent turn retried exactly once", len(ag.calls))
	}
	if ag.calls[0].Prompt != ag.calls[1].Prompt || ag.calls[1].Session != nil {
		t.Fatal("the retry must send the same prompt with no session reuse")
	}
	if !strings.Contains(stall.Description, "produced no output at all") || !strings.Contains(stall.Description, "test_agent_stall_timeout") {
		t.Fatalf("stall finding = %q, want the measured silence and the setting named", stall.Description)
	}
	if !strings.Contains(stall.Description, "stalled again on its automatic retry") {
		t.Fatalf("stall finding = %q, want it to say the retry stalled too", stall.Description)
	}
	if logs.count("retrying it once") != 1 {
		t.Fatalf("logs = %q, want the automatic retry logged once", logs.lines)
	}
}

func TestTestStep_SilentEvidenceAgentRetryThatFinishesCompletesTheStep(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "silent-once"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if len(ag.calls) == 1 {
			return silentUntilCut(ctx, opts)
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}
	sctx, _ := stallingTestContext(t, ag, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || len(ag.calls) != 2 {
		t.Fatalf("outcome = %+v after %d invocations, want the retry to complete the step", outcome, len(ag.calls))
	}
}

func TestTestStep_EvidenceAgentThatWentSilentAfterActivityParksWithoutARetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(context.Context, agent.RunOpts) (*agent.Result, error)
	}{
		{"streamed text", emitsThenSilent},
		{"lifecycle activity", func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "native", Phase: agent.LifecyclePhaseActivity})
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		// rovodev's tool phase streams no text; its tool events reach the
		// watch only as activity, and a retry would replay the tools.
		{"tool activity with no text", func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "rovodev", Phase: agent.LifecyclePhaseActivity, Message: "rovodev producing output"})
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "goes-quiet", runFn: tc.run}
			sctx, logs := stallingTestContext(t, ag, config.Commands{})

			start := time.Now()
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v, want a parked stall", err)
			}
			stall := assertStallPark(t, outcome, start)
			if len(ag.calls) != 1 {
				t.Fatalf("evidence invocations = %d, want no retry of a turn that already acted", len(ag.calls))
			}
			if !strings.Contains(stall.Description, "after it had already been active") {
				t.Fatalf("stall finding = %q, want it to say why there was no retry", stall.Description)
			}
			if logs.count("retrying it once") != 0 {
				t.Fatalf("logs = %q, want no retry", logs.lines)
			}
		})
	}
}

// A cut adapter may answer its cancellation with a structured-output rejection.
// That is the stall's consequence, not a formatting slip, so it parks rather
// than spending a correction turn on a turn that never finished.
func TestTestStep_StalledTurnThatReportsARejectionStillParksAsAStall(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "rejects-when-cut", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnChunk("working\n")
		<-ctx.Done()
		return nil, rejectedStructuredOutputError{message: "no structured output"}
	}}
	sctx, _ := stallingTestContext(t, ag, config.Commands{})

	start := time.Now()
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked stall", err)
	}
	assertStallPark(t, outcome, start)
	if len(ag.calls) != 1 {
		t.Fatalf("invocations = %d, want no correction turn after a stall", len(ag.calls))
	}
}

func TestTestStep_BusyEvidenceAgentIsNotCutByTheStallWindow(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "busy", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		// Emit for several windows before answering.
		until := time.Now().Add(5 * testStallWindow)
		for time.Now().Before(until) {
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(testStallWindow / 4):
				opts.OnChunk(".")
			}
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx, _ := stallingTestContext(t, ag, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || len(ag.calls) != 1 {
		t.Fatalf("outcome = %+v after %d invocations, want a busy agent to finish normally", outcome, len(ag.calls))
	}
}

func TestTestStep_StalledRepairTurnParksAsAStall(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "repair-goes-quiet", runFn: emitsThenSilent}
	sctx, _ := stallingTestContext(t, ag, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","action":"auto-fix","description":"TestFoo fails"}]}`

	start := time.Now()
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked stall", err)
	}
	stall := assertStallPark(t, outcome, start)
	if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, "Fix the failing tests") {
		t.Fatalf("invocations = %d, want only the repair turn, cut once", len(ag.calls))
	}
	if !strings.Contains(stall.Description, "agent fix tests stalled") {
		t.Fatalf("stall finding = %q, want the repair turn named", stall.Description)
	}
	if !strings.Contains(outcome.Findings, "TestFoo fails") {
		t.Fatalf("findings = %s, want the findings the cut fix round was answering kept", outcome.Findings)
	}
}

func TestTestStep_StalledCorrectionTurnParksAsAStall(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "correction-goes-silent"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if len(ag.calls) == 1 {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":""}`)}, nil
		}
		return silentUntilCut(ctx, opts)
	}
	sctx, _ := stallingTestContext(t, ag, config.Commands{})

	start := time.Now()
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked stall", err)
	}
	assertStallPark(t, outcome, start)
	if len(ag.calls) != 3 {
		t.Fatalf("invocations = %d, want the evidence turn plus a silent correction turn retried once", len(ag.calls))
	}
	if ag.calls[1].Prompt != ag.calls[2].Prompt || !strings.Contains(ag.calls[2].Prompt, "correction-only turn") {
		t.Fatal("the retry must repeat the correction turn's own prompt")
	}
}

func TestTestStep_StallWindowNotShorterThanTheBudgetHasNoEffectAndSaysSoOncePerRun(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "silent", runFn: silentUntilCut}
	sctx, logs := stallingTestContext(t, ag, config.Commands{})
	sctx.Config.TestAgentTimeout = 30 * time.Millisecond
	sctx.Config.TestAgentStallTimeout = 30 * time.Millisecond

	step := &TestStep{}
	for range 2 {
		outcome, err := step.Execute(sctx)
		if err != nil {
			t.Fatal(err)
		}
		testFindingByID(t, outcome.Findings, types.FindingIDTestAgentTimeout)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("invocations = %d, want each budget cut once and no stall retry", len(ag.calls))
	}
	if got := logs.count("test_agent_stall_timeout is not shorter than test_agent_timeout (30ms)"); got != 1 {
		t.Fatalf("no-effect notice logged %d times, want once per run: %q", got, logs.lines)
	}
}

func TestTestStep_StallWindowOffLeavesOnlyTheBudget(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "silent", runFn: silentUntilCut}
	sctx, logs := stallingTestContext(t, ag, config.Commands{})
	sctx.Config.TestAgentTimeout = 60 * time.Millisecond
	sctx.Config.TestAgentStallTimeout = config.TestAgentStallTimeoutOff

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	testFindingByID(t, outcome.Findings, types.FindingIDTestAgentTimeout)
	if len(ag.calls) != 1 || logs.count("test_agent_stall_timeout") != 0 {
		t.Fatalf("invocations = %d, logs = %q; want one budget cut and no stall handling", len(ag.calls), logs.lines)
	}
}

func TestTestStep_FixOfAStallAloneRerunsOnlyValidation(t *testing.T) {
	t.Parallel()
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx, _ := stallingTestContext(t, ag, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-agent-stall","severity":"warning","action":"ask-user","description":"stalled","user_instructions":"use the local fixture server"}]}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || len(prompts) != 1 || strings.Contains(prompts[0], "Fix the failing tests") {
		t.Fatalf("outcome = %+v, prompts = %d; want one passing evidence turn and no repair turn", outcome, len(prompts))
	}
	if !strings.Contains(prompts[0], "use the local fixture server") {
		t.Fatal("operator guidance on the stall finding did not reach the re-run validation")
	}
}
