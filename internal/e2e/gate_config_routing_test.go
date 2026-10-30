//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The live gate's models (testdata/gate-config.yaml): review on pro, test,
// document and lint on flash, every other step on opencode's default.
const (
	gateProModel   = "deepseek/deepseek-v4-pro"
	gateFlashModel = "deepseek/deepseek-flash"
)

// gateRoute is the model the live gate's config routes a step's invocations
// to, fix rounds included (it sets no fix-round key, so each falls back to
// its step), or "" for opencode's default.
func gateRoute(step string) string {
	switch step {
	case "review":
		return gateProModel
	case "test", "document", "lint":
		return gateFlashModel
	}
	return ""
}

// writeGateConfigScenario scripts a run through every agent invocation the
// live gate makes: agentModelRoutingActions' review, test and lint fixes,
// then a CI build that fails once and a CI fix round repairs. The Rebase
// step merges the moved base itself and never invokes an agent.
func writeGateConfigScenario(t *testing.T) string {
	t.Helper()
	content := "actions:\n" + ciFixTurn + agentModelRoutingActions +
		"  -" + strings.TrimPrefix(cleanRoutingResponse, "   ")
	return writeRoutingScenario(t, "gate-config-scenario.yaml", content)
}

// setupGateConfigMovedBase changes feature.txt on the branch and moves the
// base under it with another file, so the Rebase step has a base to merge.
// The branch's diff stays that one file, so the seeded transcript that touches
// it is intent's decisive match.
func setupGateConfigMovedBase(t *testing.T, h *Harness, branch string) {
	t.Helper()
	h.CommitChange("main", "feature.txt", "base\n", "add the feature file")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push feature base: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "seed for agent_model routing\n", "change the feature for gate config routing")
	advanceMain(t, h, "main-advance.txt", "main change\n", "move main under the branch")
	h.Checkout(branch)
}

// gateConfigTurn names an invocation by its step and, within the step, by
// the scripted turn its prompt carries.
func gateConfigTurn(inv Invocation) string {
	step := invocationStep(inv)
	for _, fix := range []struct{ step, needle, turn string }{
		{"review", routingReviewFixMarker, "review-fix"},
		{"test", routingTestFixMarker, "test-fix"},
		{"document", routingHousekeepingMarker, "housekeeping"},
		{"lint", routingLintFixMarker, "lint-fix"},
		{"ci", routingCIFixMarker, "ci-fix"},
	} {
		if step == fix.step && strings.Contains(inv.Prompt, fix.needle) {
			return fix.turn
		}
	}
	return step
}

// TestAgentModelRoutesTheLiveGateConfigOnRealBinary loads the live gate's
// routing keys (testdata/gate-config.yaml) through the real binary and drives
// a Gitea-hosted run through every agent invocation the gate makes - the
// intent summarizer, the review and its fix, two test
// passes around the test fix, the housekeeping pass and the lint fix, the PR
// draft, and the CI repair - proving each invocation asked for, and was
// served, the model the spec's routing table gives it.
func TestAgentModelRoutesTheLiveGateConfigOnRealBinary(t *testing.T) {
	gateConfig, err := os.ReadFile(filepath.Join("testdata", "gate-config.yaml"))
	if err != nil {
		t.Fatalf("read gate config fixture: %v", err)
	}
	h := NewHarness(t, SetupOpts{
		Agent:        "opencode",
		Scenario:     writeGateConfigScenario(t),
		GlobalConfig: string(gateConfig),
	})
	setupGiteaCIRepair(t, h)
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "feature.txt")
	if out, err := h.RunInDir(h.WorkDir, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/gate-config-routing"
	setupGateConfigMovedBase(t, h, branch)
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)
	if gated == nil {
		t.Fatal("run did not park at the review gate")
	}
	h.RespondWithFindings(gated.ID, types.StepReview, types.ActionFix, []string{"routing-review"})
	gated = waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 120*time.Second)
	if gated == nil {
		t.Fatal("run did not park at the lint gate")
	}
	h.RespondWithFindings(gated.ID, types.StepLint, types.ActionFix, []string{"routing-lint"})
	run := waitForCompletedRun(t, h, branch)

	turns := map[string]int{}
	for _, inv := range h.AgentInvocations() {
		step := invocationStep(inv)
		turn := gateConfigTurn(inv)
		if step == "" {
			t.Fatalf("invocation names no step:\n%s", truncate(inv.Prompt, 400))
		}
		if want := gateRoute(step); inv.Model != want {
			t.Errorf("%s invocation asked for model %q, want %q:\n%s", turn, inv.Model, want, truncate(inv.Prompt, 400))
		}
		if inv.Variant != "" {
			t.Errorf("%s invocation asked for variant %q with no effort configured", turn, inv.Variant)
		}
		turns[turn]++
		t.Logf("%-15s asked for model %q", turn, inv.Model)
	}
	for turn, min := range map[string]int{
		"intent": 1, "review": 2, "review-fix": 1, "test": 2, "test-fix": 1,
		"housekeeping": 1, "lint-fix": 1, "pr": 1, "ci-fix": 1,
	} {
		if turns[turn] < min {
			t.Errorf("%s turns = %d, want at least %d (turns: %v)", turn, turns[turn], min, turns)
		}
	}
	if turns["rebase"] != 0 {
		t.Errorf("the Rebase step invoked an agent %d time(s); it only ever merges", turns["rebase"])
	}
	assertOneCIFixRow(t, h, run.ID)
	assertServedModels(t, h, run.ID, func(row db.AgentInvocation) string { return gateRoute(row.StepName) })

	for _, step := range []types.StepName{
		types.StepIntent, types.StepReview, types.StepTest,
		types.StepDocument, types.StepLint, types.StepPR, types.StepCI,
	} {
		want := "no agent_model override configured for step " + string(step) + "; running on agent default"
		if model := gateRoute(string(step)); model != "" {
			want = "using agent_model override for step " + string(step) + ": " + model + " (key " + string(step) + ")"
		}
		if log := readRunStepLog(t, h, run.ID, step); !strings.Contains(log, want) {
			t.Errorf("%s log is missing %q:\n%s", step, want, log)
		}
	}
}
