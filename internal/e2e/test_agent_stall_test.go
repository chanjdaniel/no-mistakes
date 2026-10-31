//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// testEvidencePromptMarker is the opening line of the Test step's evidence
// prompt, which only that turn (and its stall retry) receives.
const testEvidencePromptMarker = "You are validating a code change by driving the product itself."

// silentTestAgentScenario makes only the Test evidence turn go completely
// silent - no stdout, no stderr, no answer - for far longer than the budget the
// journey configures, so the pipeline, not the fake, ends each attempt.
func silentTestAgentScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent-test-agent-scenario.yaml")
	content := `actions:
  - match: "` + testEvidencePromptMarker + `"
    delay_ms: 120000
    text: "never reached"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write silent test agent scenario: %v", err)
	}
	return path
}

// TestSilentTestAgentParksAtTheStallWindow reproduces T113's failure through
// the stock `no-mistakes axi` surface: a Test agent that starts and then emits
// nothing at all. It used to hold the run for the whole test_agent_timeout;
// now the stall window cuts it, retries the silent turn once, and parks with a
// test-agent-stall finding well inside the budget.
//
// The windows here are seconds rather than the production 10 and 30 minutes;
// the path is identical, and the test must never be "fixed" by raising them.
func TestSilentTestAgentParksAtTheStallWindow(t *testing.T) {
	const (
		stallWindow = 2 * time.Second
		budget      = 90 * time.Second
	)
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: silentTestAgentScenario(t),
		GlobalConfigExtra: strings.Join([]string{
			`test_agent_timeout: "` + budget.String() + `"`,
			`test_agent_stall_timeout: "` + stallWindow.String() + `"`,
		}, "\n"),
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/silent-test-agent"
	h.CommitChange(branch, ".no-mistakes.yaml", "commands:\n  lint: 'exit 0'\n", "configure lint")
	h.CommitChange(branch, "feature.txt", "value\n", "add feature")

	start := time.Now()
	runOut, err := h.Run("axi", "run", "--intent", "validate the feature while the test agent is wedged", "--skip", "pr,ci")
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, runOut)
	}
	run := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, budget)
	elapsed := time.Since(start)
	statusOut, _ := h.Run("axi", "status")
	t.Logf("stock axi run surface after %s:\n%s", elapsed, runOut)
	t.Logf("stock axi status surface:\n%s", statusOut)

	if elapsed >= budget {
		t.Fatalf("the Test step parked after %s, want it inside the stall windows, not the %s budget", elapsed, budget)
	}
	step, ok := findStep(run.Steps, types.StepTest)
	if !ok || step.FindingsJSON == nil {
		t.Fatalf("test step = %+v, want parked findings", step)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var stall *types.Finding
	for i := range findings.Items {
		switch findings.Items[i].ID {
		case types.FindingIDTestAgentStall:
			stall = &findings.Items[i]
		case types.FindingIDTestAgentTimeout:
			t.Fatalf("findings = %s, a stall must not be reported as an expired budget", *step.FindingsJSON)
		}
	}
	if stall == nil || stall.Action != types.ActionAskUser {
		t.Fatalf("findings = %s, want an ask-user test-agent-stall finding", *step.FindingsJSON)
	}
	for _, want := range []string{"stalled again on its automatic retry", "produced no output at all", "test_agent_stall_timeout"} {
		if !strings.Contains(stall.Description, want) {
			t.Fatalf("stall finding = %q, want it to contain %q", stall.Description, want)
		}
	}
	surfaces := runOut + "\n" + statusOut
	if !strings.Contains(surfaces, types.FindingIDTestAgentStall) {
		t.Fatalf("axi surfaces did not show the stall finding:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}

	evidenceTurns := 0
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, testEvidencePromptMarker) {
			evidenceTurns++
		}
	}
	if evidenceTurns != 2 {
		t.Fatalf("Test evidence invocations = %d, want the silent turn plus exactly one retry", evidenceTurns)
	}
}
