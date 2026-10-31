//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fixSummaryOutputScenario is a review that finds one issue and a fixer whose
// final text is fixOutput verbatim, delivered through the codex adapter's text
// parser exactly as a real codex agent_message would be.
func fixSummaryOutputScenario(t *testing.T, fixOutput string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fix-summary-output-scenario.yaml")
	content := `actions:
  - match: "Investigate previous review findings"
    edits:
      - path: "feature.txt"
        old: "unsafe"
        new: "safe"
    structured_raw: ` + jsonQuoted(t, fixOutput) + `
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: "fix-summary-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "unsafe value needs validation"
          action: auto-fix
          review_scope: source
      summary: "found one issue"
      risk_level: medium
      risk_rationale: "the unsafe value needs a guard"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: guard unsafe value"
      body: "fix summary output journey"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fix summary output scenario: %v", err)
	}
	return path
}

// jsonQuoted renders s as a JSON string, which is also a valid YAML
// double-quoted scalar, so newlines in the fixer's output survive the scenario.
func jsonQuoted(t *testing.T, s string) string {
	t.Helper()
	quoted, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("quote fix output: %v", err)
	}
	return string(quoted)
}

// TestReviewFixTurnSummaryOutputJourney: a review fix round whose fixer
// printed its summary JSON twice used to fail the whole run at Review with
// "multiple bare JSON objects". The fixer's work is what the rereview judges,
// so a duplicated summary must read as one, and a summary that still cannot
// be read must commit under the step's fallback summary rather than fail.
func TestReviewFixTurnSummaryOutputJourney(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fixOutput   string
		wantSubject string
	}{
		{
			name:        "summary printed twice",
			fixOutput:   `{"summary":"guard unsafe value"}` + "\n\n" + `{"summary":"guard unsafe value"}`,
			wantSubject: "no-mistakes(review): guard unsafe value",
		},
		{
			name:        "two different summaries",
			fixOutput:   `{"summary":"draft wording"}` + "\n\n" + `{"summary":"guard unsafe value"}`,
			wantSubject: "no-mistakes(review): guard unsafe value",
		},
		{
			name:        "summary rejected by the schema",
			fixOutput:   `{"summary":42}`,
			wantSubject: "no-mistakes(review): address review findings",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: fixSummaryOutputScenario(t, tc.fixOutput)})

			globalConfig := filepath.Join(h.NMHome, "config.yaml")
			globalData, err := os.ReadFile(globalConfig)
			if err != nil {
				t.Fatalf("read global config: %v", err)
			}
			globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
			if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
				t.Fatalf("write global config: %v", err)
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}

			const branch = "feature/fix-summary-output"
			h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
			h.PushToGate(branch)

			// The rereview still reports the finding, so the round parks at
			// fix_review: reaching it proves the fix turn committed and the
			// rereview ran.
			gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
			h.Respond(gated.ID, types.StepReview, types.ActionApprove)
			run := h.WaitForRun(branch, 60*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
			}

			log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
			if err != nil {
				t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
			}
			subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
			if len(subjects) == 0 || subjects[0] != tc.wantSubject {
				t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], tc.wantSubject, subjects)
			}
			content, err := h.runGit(context.Background(), h.UpstreamDir, "show", "refs/heads/"+branch+":feature.txt")
			if err != nil {
				t.Fatalf("read upstream feature.txt: %v\n%s", err, content)
			}
			if strings.TrimSpace(string(content)) != "safe" {
				t.Fatalf("upstream feature.txt = %q, want the fixer's edit", content)
			}
		})
	}
}
