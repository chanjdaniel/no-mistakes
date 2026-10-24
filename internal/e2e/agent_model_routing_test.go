//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Distinct per key, so a route that lands on the wrong step is visible.
const (
	routedReviewModel   = "deepseek/review-pro-e2e"
	routedTestModel     = "deepseek/test-pro-e2e"
	routedDocumentModel = "deepseek/document-flash-e2e"
	routedLintModel     = "deepseek/lint-flash-e2e"
)

// agentModelRoutingConfig is the gate's real routing block (agent: opencode,
// the four step keys) with one model per key.
const agentModelRoutingConfig = "agent_model:\n" +
	"  opencode:\n" +
	"    review: " + routedReviewModel + "\n" +
	"    test: " + routedTestModel + "\n" +
	"    document: " + routedDocumentModel + "\n" +
	"    lint: " + routedLintModel + "\n"

// The gate's real auto_fix budget: review parks for a human, test fixes on its
// own, and lint parks because agent-driven lint findings are never
// auto-fixable.
var gateAutoFix = map[types.StepName]int{
	types.StepRebase:   3,
	types.StepLint:     3,
	types.StepTest:     3,
	types.StepReview:   0,
	types.StepDocument: 3,
	types.StepCI:       3,
}

// Prompt markers for the turns the routing scenario scripts.
const (
	routingIntentMarker       = "transcript of a developer's recent conversation with a coding agent"
	routingReviewMarker       = "Review the code changes and return structured findings"
	routingReviewFixMarker    = "Previous review findings to address"
	routingRoundHistoryMarker = "Previous rounds for this step"
	routingTestMarker         = "You are validating a code change by driving the product itself"
	routingTestFixMarker      = "Fix the failing tests in this repository"
	routingHousekeepingMarker = "Combined lint duty"
	routingLintFixMarker      = "Previous lint findings to address"
)

// cleanRoutingResponse satisfies every schema the pipeline hands an agent.
const cleanRoutingResponse = `    text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: agent_model routing"
      body: "## Summary\nagent_model routing e2e"
`

// agentModelRoutingActions scripts one run through every routed step and its
// fix round: the intent summarizer matches, the review reports one finding a
// human sends to the fixer, the first test pass fails and the test fix round
// repairs it, and the combined housekeeping pass reports one lint finding a
// human sends to the lint fix round. Every round after a fix carries the
// step's round history and comes back clean. First match wins, so the fix
// turns lead.
const agentModelRoutingActions = `  - match: "` + routingTestFixMarker + `"
    text: "fixed the failing test"
    edits:
      - path: "feature.txt"
        old: "seed for agent_model routing\n"
        new: "fixed for agent_model routing\n"
    stage: ["feature.txt"]
    structured:
      summary: "fix failing test"
  - match: "` + routingLintFixMarker + `"
    text: "fixed the lint issue"
    edits:
      - path: "lint.txt"
        new: "lint clean\n"
    stage: ["lint.txt"]
    structured:
      findings: []
      summary: "fix lint issue"
  - match: "` + routingReviewFixMarker + `"
    text: "addressed the review finding"
    edits:
      - path: "review.txt"
        new: "review fixed\n"
    stage: ["review.txt"]
    structured:
      summary: "resolve review finding"
  - match: "` + routingRoundHistoryMarker + `"
` + cleanRoutingResponse + `  - match: "` + routingReviewMarker + `"
    text: "found one blocking issue"
    structured:
      findings:
        - id: "routing-review"
          severity: warning
          description: "mechanical issue for the review fixer"
          file: "feature.txt"
          action: auto-fix
          review_scope: source
      summary: "one blocking issue"
      risk_level: low
      risk_rationale: "single mechanical issue"
      risk_scope: source-or-external
  - match: "` + routingTestMarker + `"
    text: "the feature test fails"
    structured:
      findings:
        - id: "routing-test"
          severity: error
          description: "feature test fails"
          file: "feature.txt"
          action: auto-fix
      summary: "feature test fails"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated feature test failed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: fail
          live: true
          evidence: "fakeagent: simulated test run failed"
          reason: ""
      verdict: no-go
  - match: "` + routingHousekeepingMarker + `"
    text: "one lint issue left"
    structured:
      findings:
        - id: "routing-lint"
          severity: warning
          description: "formatting issue for the lint fixer"
          file: "feature.txt"
          action: auto-fix
          category: lint
      summary: "one lint issue"
  - match: "` + routingIntentMarker + `"
    text: "summarized"
    structured:
      summary: "user wanted agent_model routing exercised"
`

func writeAgentModelRoutingScenario(t *testing.T) string {
	t.Helper()
	content := "actions:\n" + agentModelRoutingActions + "  -" + strings.TrimPrefix(cleanRoutingResponse, "   ")
	return writeRoutingScenario(t, "agent-model-routing-scenario.yaml", content)
}

// stepPhase reads the gate-step phase boundary every step invocation's prompt
// opens with, naming the step the executor ran it for.
var stepPhase = regexp.MustCompile(`You are the (\S+) phase inside an already active no-mistakes run`)

func invocationStep(inv Invocation) string {
	if m := stepPhase.FindStringSubmatch(inv.Prompt); m != nil {
		return m[1]
	}
	return ""
}

func readRunStepLog(t *testing.T, h *Harness, runID string, step types.StepName) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, string(step)+".log"))
	if err != nil {
		t.Fatalf("read %s log: %v", step, err)
	}
	return string(data)
}

// TestAgentModelRoutesEachStepOnRealBinary drives the real no-mistakes binary
// (init -> git push -> daemon pipeline) with the gate's own routing and
// auto_fix budget, through a run with a review fix, a test fix and a lint fix,
// and proves from the fake opencode server's record of each message body that
// every invocation of a routed step, fix rounds included, asked for its step's
// model while every unrouted step asked for none.
func TestAgentModelRoutesEachStepOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "opencode",
		Scenario:          writeAgentModelRoutingScenario(t),
		GlobalConfigExtra: agentModelRoutingConfig,
		AutoFix:           gateAutoFix,
	})
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "feature.txt")
	if out, err := h.RunInDir(h.WorkDir, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/agent-model-routing"
	h.CommitChange(branch, "feature.txt", "seed for agent_model routing\n", "add feature for agent_model routing")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("run did not park at the review gate")
	}
	h.RespondWithFindings(gated.ID, types.StepReview, types.ActionFix, []string{"routing-review"})

	gated = waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 120*time.Second)
	if gated == nil {
		t.Fatal("run did not park at the lint gate")
	}
	h.RespondWithFindings(gated.ID, types.StepLint, types.ActionFix, []string{"routing-lint"})

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want completed (error: %s)\n%s", run.Status, runErr, dumpPrompts(h.AgentInvocations()))
	}

	wantByStep := map[string]string{
		"intent":   "",
		"rebase":   "",
		"review":   routedReviewModel,
		"test":     routedTestModel,
		"document": routedDocumentModel,
		"lint":     routedLintModel,
		"pr":       "",
		"ci":       "",
	}
	turns := map[string]int{}
	for _, inv := range h.AgentInvocations() {
		step := invocationStep(inv)
		want, known := wantByStep[step]
		if !known {
			t.Fatalf("invocation for unexpected step %q:\n%s", step, truncate(inv.Prompt, 400))
		}
		if inv.Model != want {
			t.Errorf("%s invocation asked for model %q, want %q:\n%s", step, inv.Model, want, truncate(inv.Prompt, 400))
		}
		if inv.Variant != "" {
			t.Errorf("%s invocation asked for variant %q with no effort configured", step, inv.Variant)
		}
		for _, marker := range []struct{ turn, needle string }{
			{"intent", routingIntentMarker},
			{"review-fix", routingReviewFixMarker},
			{"test-fix", routingTestFixMarker},
			{"housekeeping", routingHousekeepingMarker},
			{"lint-fix", routingLintFixMarker},
		} {
			if strings.Contains(inv.Prompt, marker.needle) {
				turns[marker.turn]++
			}
		}
		turns[step]++
		t.Logf("%-8s asked for model %q", step, inv.Model)
	}
	// Every scripted turn ran: the review, its fix and the rereview; two test
	// passes around the test fix; the housekeeping pass; the lint fix.
	for turn, min := range map[string]int{
		"intent": 1, "review": 3, "review-fix": 1, "test": 3, "test-fix": 1,
		"document": 1, "housekeeping": 1, "lint": 1, "lint-fix": 1,
	} {
		if turns[turn] < min {
			t.Errorf("%s turns = %d, want at least %d (turns: %v)", turn, turns[turn], min, turns)
		}
	}

	for step, model := range map[types.StepName]string{
		types.StepReview:   routedReviewModel,
		types.StepTest:     routedTestModel,
		types.StepDocument: routedDocumentModel,
		types.StepLint:     routedLintModel,
	} {
		want := "using agent_model override for step " + string(step) + ": " + model + " (key " + string(step) + ")"
		if log := readRunStepLog(t, h, run.ID, step); !strings.Contains(log, want) {
			t.Errorf("%s log is missing %q:\n%s", step, want, log)
		}
	}
	const unrouted = "no agent_model override configured for step intent; running on agent default"
	if log := readRunStepLog(t, h, run.ID, types.StepIntent); !strings.Contains(log, unrouted) {
		t.Errorf("intent log is missing %q:\n%s", unrouted, log)
	}
	assertServedModels(t, h, run.ID, func(row db.AgentInvocation) string { return wantByStep[row.StepName] })
}

// TestAgentModelKeepsAgentConfigEffortOnRealBinary proves the precedence on
// the real binary: a routed step asks for its route with agent_config's
// effort, and an unrouted step keeps agent_config's model and effort.
func TestAgentModelKeepsAgentConfigEffortOnRealBinary(t *testing.T) {
	const defaultModel = "deepseek/default-e2e"
	h := NewHarness(t, SetupOpts{
		Agent: "opencode",
		GlobalConfigExtra: "agent_config:\n  opencode: {model: " + defaultModel + ", effort: high}\n" +
			"agent_model:\n  opencode:\n    review: " + routedReviewModel + "\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	const branch = "feature/agent-model-effort"
	h.CommitChange(branch, "feature.txt", "agent_model with agent_config\n", "add feature")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed", run.Status)
	}

	steps := map[string]bool{}
	for _, inv := range h.AgentInvocations() {
		step := invocationStep(inv)
		steps[step] = true
		want := defaultModel
		if step == "review" {
			want = routedReviewModel
		}
		if inv.Model != want || inv.Variant != "high" {
			t.Errorf("%s invocation asked for model %q variant %q, want %q variant high", step, inv.Model, inv.Variant, want)
		}
	}
	if !steps["review"] || !steps["test"] {
		t.Fatalf("run invoked steps %v, want review and an unrouted test", steps)
	}
}

// Models for the new step and fix-round keys: intent, pr and ci share a
// cheap model, test runs on a second and its fix round escalates to a third.
const (
	routedCheapModel    = "deepseek/cheap-flash-e2e"
	routedTestOnlyModel = "deepseek/test-flash-e2e"
	routedTestFixModel  = "deepseek/test-fix-pro-e2e"
	routedCIFixModel    = "deepseek/ci-fix-pro-e2e"
)

const routingCIFixMarker = "The following CI checks have failed on this PR"

// ciFixTurn is the scripted CI repair: it changes a file so the repair
// publishes a new head, which the tea stub's CI cycle then reads as merged.
const ciFixTurn = `  - match: "` + routingCIFixMarker + `"
    text: "repaired the build"
    edits:
      - path: "ci.txt"
        new: "build repaired\n"
    stage: ["ci.txt"]
    structured:
      summary: "repair the build"
      code_change_needed: true
`

// writeStepAndFixRoundScenario scripts a run whose first test pass fails and
// is repaired by the test fix round, and whose CI build fails once and is
// repaired by a CI fix round; every other turn is clean.
func writeStepAndFixRoundScenario(t *testing.T) string {
	t.Helper()
	content := `actions:
  - match: "` + routingTestFixMarker + `"
    text: "fixed the failing test"
    edits:
      - path: "feature.txt"
        old: "seed for fix-round routing\n"
        new: "fixed for fix-round routing\n"
    stage: ["feature.txt"]
    structured:
      summary: "fix failing test"
` + ciFixTurn + `  - match: "` + routingRoundHistoryMarker + `"
` + cleanRoutingResponse + `  - match: "` + routingTestMarker + `"
    text: "the feature test fails"
    structured:
      findings:
        - id: "routing-test"
          severity: error
          description: "feature test fails"
          file: "feature.txt"
          action: auto-fix
      summary: "feature test fails"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated feature test failed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: fail
          live: true
          evidence: "fakeagent: simulated test run failed"
          reason: ""
      verdict: no-go
  - match: "` + routingIntentMarker + `"
    text: "summarized"
    structured:
      summary: "user wanted fix-round routing exercised"
  -` + strings.TrimPrefix(cleanRoutingResponse, "   ")
	return writeRoutingScenario(t, "step-and-fix-round-scenario.yaml", content)
}

// writeCIFixScenario scripts a run that is clean until its CI build fails
// once and a CI fix round repairs it.
func writeCIFixScenario(t *testing.T) string {
	t.Helper()
	content := "actions:\n" + ciFixTurn + "  -" + strings.TrimPrefix(cleanRoutingResponse, "   ")
	return writeRoutingScenario(t, "ci-fix-scenario.yaml", content)
}

func writeRoutingScenario(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}

// setupGiteaCIRepair routes the harness to a Gitea host whose tea stub models
// one CI cycle: the head the PR is created at fails its build, and the CI
// repair's new head merges the PR.
func setupGiteaCIRepair(t *testing.T, h *Harness) {
	t.Helper()
	setupGiteaRemote(t, h)
	t.Setenv("FAKEAGENT_TEA_CI_REMOTE", h.UpstreamDir)
	t.Setenv("FAKEAGENT_TEA_CI_STATE", filepath.Join(t.TempDir(), "tea-ci-state.json"))
}

func runAgentInvocations(t *testing.T, h *Harness, runID string) []db.AgentInvocation {
	t.Helper()
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	rows, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		t.Fatalf("read agent invocations: %v", err)
	}
	return rows
}

// assertOneCIFixRow checks the run recorded its CI repair as exactly one
// agent invocation with the purpose ci-fix.
func assertOneCIFixRow(t *testing.T, h *Harness, runID string) {
	t.Helper()
	var purposes []string
	for _, row := range runAgentInvocations(t, h, runID) {
		if row.StepName == string(types.StepCI) {
			purposes = append(purposes, row.Purpose)
		}
	}
	if len(purposes) != 1 || purposes[0] != "ci-fix" {
		t.Errorf("ci agent_invocations purposes = %q, want exactly one ci-fix", purposes)
	}
}

func waitForCompletedRun(t *testing.T, h *Harness, branch string) *ipc.RunInfo {
	t.Helper()
	run := h.WaitForRun(branch, 180*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed (error: %s)\n%s", run.Status, deref(run.Error), dumpPrompts(h.AgentInvocations()))
	}
	return run
}

// TestAgentModelRoutesNewStepAndFixRoundKeysOnRealBinary drives the real
// binary through a Gitea-hosted run with intent, pr and ci on one model, test
// on a second and test-fix on a third: the test fix round takes its own key,
// the CI repair (no ci-fix key) falls back to ci, and every other step asks
// for no model.
func TestAgentModelRoutesNewStepAndFixRoundKeysOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "opencode",
		Scenario: writeStepAndFixRoundScenario(t),
		GlobalConfigExtra: "agent_model:\n  opencode:\n" +
			"    intent: " + routedCheapModel + "\n" +
			"    pr: " + routedCheapModel + "\n" +
			"    ci: " + routedCheapModel + "\n" +
			"    test: " + routedTestOnlyModel + "\n" +
			"    test-fix: " + routedTestFixModel + "\n",
		AutoFix: gateAutoFix,
	})
	setupGiteaCIRepair(t, h)
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "feature.txt")
	if out, err := h.RunInDir(h.WorkDir, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	const branch = "feature/fix-round-routing"
	h.CommitChange(branch, "feature.txt", "seed for fix-round routing\n", "add feature for fix-round routing")
	h.PushToGate(branch)
	run := waitForCompletedRun(t, h, branch)

	turns := map[string]int{}
	for _, inv := range h.AgentInvocations() {
		turn := invocationStep(inv)
		want := ""
		switch {
		case turn == "intent", turn == "pr":
			want = routedCheapModel
		case turn == "test" && strings.Contains(inv.Prompt, routingTestFixMarker):
			turn, want = "test-fix", routedTestFixModel
		case turn == "test":
			want = routedTestOnlyModel
		case turn == "ci":
			if !strings.Contains(inv.Prompt, routingCIFixMarker) {
				t.Errorf("ci invocation is not the CI repair:\n%s", truncate(inv.Prompt, 400))
			}
			want = routedCheapModel
		case turn == "":
			t.Fatalf("invocation names no step:\n%s", truncate(inv.Prompt, 400))
		}
		if inv.Model != want {
			t.Errorf("%s invocation asked for model %q, want %q:\n%s", turn, inv.Model, want, truncate(inv.Prompt, 400))
		}
		turns[turn]++
		t.Logf("%-8s asked for model %q", turn, inv.Model)
	}
	for turn, min := range map[string]int{"intent": 1, "test": 2, "test-fix": 1, "pr": 1, "ci": 1, "review": 1} {
		if turns[turn] < min {
			t.Errorf("%s turns = %d, want at least %d (turns: %v)", turn, turns[turn], min, turns)
		}
	}
	assertOneCIFixRow(t, h, run.ID)
	assertServedModels(t, h, run.ID, func(row db.AgentInvocation) string {
		switch {
		case row.StepName == "intent", row.StepName == "pr", row.StepName == "ci":
			return routedCheapModel
		case row.StepName == "test" && row.Purpose == "test-fix":
			return routedTestFixModel
		case row.StepName == "test":
			return routedTestOnlyModel
		}
		return ""
	})

	for step, lines := range map[types.StepName][]string{
		types.StepIntent: {"using agent_model override for step intent: " + routedCheapModel + " (key intent)"},
		types.StepPR:     {"using agent_model override for step pr: " + routedCheapModel + " (key pr)"},
		types.StepCI:     {"using agent_model override for step ci: " + routedCheapModel + " (key ci)"},
		types.StepTest: {
			"using agent_model override for step test: " + routedTestOnlyModel + " (key test)",
			"using agent_model override for step test: " + routedTestFixModel + " (key test-fix)",
		},
		types.StepReview: {"no agent_model override configured for step review; running on agent default"},
	} {
		log := readRunStepLog(t, h, run.ID, step)
		for _, want := range lines {
			if !strings.Contains(log, want) {
				t.Errorf("%s log is missing %q:\n%s", step, want, log)
			}
		}
	}
}

// TestAgentModelRoutesCIFixRoundOnRealBinary proves the ci-fix key on the
// real binary: with both ci and ci-fix set, the CI repair asks for ci-fix's
// model, says so in ci.log, and records its row with the purpose ci-fix.
func TestAgentModelRoutesCIFixRoundOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "opencode",
		Scenario: writeCIFixScenario(t),
		GlobalConfigExtra: "agent_model:\n  opencode:\n" +
			"    ci: " + routedCheapModel + "\n" +
			"    ci-fix: " + routedCIFixModel + "\n",
		AutoFix: gateAutoFix,
	})
	setupGiteaCIRepair(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	const branch = "feature/ci-fix-routing"
	h.CommitChange(branch, "feature.txt", "seed for ci-fix routing\n", "add feature for ci-fix routing")
	h.PushToGate(branch)
	run := waitForCompletedRun(t, h, branch)

	repairs := 0
	for _, inv := range h.AgentInvocations() {
		if invocationStep(inv) != "ci" {
			continue
		}
		repairs++
		if inv.Model != routedCIFixModel {
			t.Errorf("CI repair asked for model %q, want ci-fix's %q", inv.Model, routedCIFixModel)
		}
	}
	if repairs != 1 {
		t.Fatalf("CI repairs = %d, want 1\n%s", repairs, dumpPrompts(h.AgentInvocations()))
	}
	assertOneCIFixRow(t, h, run.ID)
	assertServedModels(t, h, run.ID, func(row db.AgentInvocation) string {
		if row.StepName == "ci" {
			return routedCIFixModel
		}
		return ""
	})
	const want = "using agent_model override for step ci: " + routedCIFixModel + " (key ci-fix)"
	if log := readRunStepLog(t, h, run.ID, types.StepCI); !strings.Contains(log, want) {
		t.Errorf("ci log is missing %q:\n%s", want, log)
	}
}
