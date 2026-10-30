package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Test evidence is worded as what was seen. Screenshots and recordings stay on
// local disk with the run and a PR body shows only an image or video's caption
// (see pr_no_media_test.go), so a scenario whose evidence is "checkout.png" or
// a screenshot captioned with its tile name tells a PR reader nothing they can
// check. The evidence turn's prompt and the schema it answers ask for the
// screen, its state, and the check that passed instead.

// TestTestStep_EvidencePromptAsksForWhatWasSeen pins the prompt half: the
// evidence turn asks for scenario evidence and captions describing what was
// observed, refuses a file name as the evidence, and says screenshots stay
// local, including under store_in_repo, which never publishes them
// (the exact store_in_repo line is pinned by
// TestTestStep_PublishedEvidenceGuidanceNamesTheEvidenceBranch).
func TestTestStep_EvidencePromptAsksForWhatWasSeen(t *testing.T) {
	t.Parallel()
	for _, storeInRepo := range []bool{false, true} {
		dir, baseSHA, headSHA := setupGitRepo(t)
		ag := &mockAgent{
			name: "test",
			runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.UserIntent = "Show users a success screen after checkout"
		if storeInRepo {
			sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: ".no-mistakes/evidence", Branch: "team/ci/evidence"}
		}

		if _, err := (&TestStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		prompt := ag.calls[0].Prompt
		for _, want := range []string{
			`Word each scenario's "evidence" as what you observed: the screen or output you saw, the state it was in, and the check that passed`,
			"never cite a file, screenshot, or tile name as the evidence",
			"Screenshots and recordings stay on local disk with this run and are never attached to or linked from the pull request",
			"the pull request shows only an image or video artifact's caption, its \"content\"",
			"Caption every screenshot or recording with what it shows, in the same terms as scenario evidence",
			"an image or video artifact with no caption is left out of the pull request",
			"or, for a screenshot or recording, its caption",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("store_in_repo=%v: expected evidence prompt to contain %q\nprompt:\n%s", storeInRepo, want, prompt)
			}
		}
		for _, forbidden := range []string{
			// The old line let a test file stand in for an observation.
			"run that test as the scenario and cite it as the evidence.",
			// Under store_in_repo the evidence branch carries no media.
			"they are published to the repository's",
		} {
			if strings.Contains(prompt, forbidden) {
				t.Errorf("store_in_repo=%v: evidence prompt still carries %q", storeInRepo, forbidden)
			}
		}
	}
}

// TestTestFindingsSchema_EvidenceAndCaptionSayWhatWasSeen pins the schema
// half, which every adapter delivers alongside the prompt: the scenario
// evidence and the artifact caption carry the same rule, and the label says it
// is not published for an image or video. The artifact path description is
// owned by TestTestFindingsSchema_DoesNotSolicitAbsolutePaths and
// TestTestFindingsSchema_KeepsEvidenceDirectoryPathsReportable.
func TestTestFindingsSchema_EvidenceAndCaptionSayWhatWasSeen(t *testing.T) {
	t.Parallel()
	var schema struct {
		Properties struct {
			Artifacts struct {
				Items struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"artifacts"`
			Scenarios struct {
				Items struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"scenarios"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(testFindingsSchema, &schema); err != nil {
		t.Fatalf("test findings schema is not valid JSON: %v", err)
	}
	evidence := schema.Properties.Scenarios.Items.Properties["evidence"].Description
	caption := schema.Properties.Artifacts.Items.Properties["content"].Description
	label := schema.Properties.Artifacts.Items.Properties["label"].Description

	for name, check := range map[string]struct {
		description string
		want        []string
	}{
		"scenario evidence": {evidence, []string{
			"what was observed",
			"the screen or output seen, its state, and the check that passed",
			"never a file, screenshot, or tile name as the evidence",
		}},
		"artifact caption": {caption, []string{
			"for a screenshot, image, or video, the caption the pull request shows in its place",
			"what it shows: the screen, its state, the check that passed",
			"never its file name",
			"screenshots and videos stay local with the run",
		}},
		"artifact label": {label, []string{
			"not published for a screenshot, image, or video",
		}},
	} {
		for _, want := range check.want {
			if !strings.Contains(check.description, want) {
				t.Errorf("%s description = %q, want it to contain %q", name, check.description, want)
			}
		}
	}
	if strings.Contains(evidence, "evidence file") {
		t.Errorf("scenario evidence description still solicits an evidence file as the evidence: %q", evidence)
	}
}

// TestPRStep_TestingTableWordedAsSeenNamesNoFiles is the reader's side: a run
// whose scenarios and captions describe what was seen renders a Testing table
// and caption lines a reviewer can check, with no evidence file, tile or label
// name anywhere in the body.
func TestPRStep_TestingTableWordedAsSeenNamesNoFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: checkout success screen","body":"## What Changed\n\n- adds the success screen"}`)}, nil
	}}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test = config.Merge(&config.GlobalConfig{}, &config.RepoConfig{}).Test
	success := writeNoMediaEvidence(t, sctx.EvidenceDir, "tile-03-success.png", "\x89PNG bytes")
	declined := writeNoMediaEvidence(t, sctx.EvidenceDir, "tile-04-declined.webm", "webm bytes")
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "Drove checkout against a local instance of the app.",
		Tested:         []string{"manual checkout in a local browser"},
		Scenarios: []types.TestScenario{
			{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "After paying, the success screen shows the order number and a Continue shopping button"},
			{Name: "declined card shows the retry copy", Result: types.ScenarioResultPass, Live: true, Evidence: "With a declined test card the form stays filled in and shows the Try another card message"},
			{Name: "refund email is sent", Result: types.ScenarioResultUntested, Reason: "no mail sandbox credential on this machine; set SMTP_SANDBOX_URL to drive it"},
		},
		Verdict: types.TestVerdictGo,
		Artifacts: []types.TestArtifact{
			{Kind: "screenshot", Label: "tile-03-success", Path: success, Content: "The success screen shows the order number and a Continue shopping button."},
			{Kind: "video", Label: "tile-04-declined", Path: declined, Content: "A declined card keeps the form filled in and shows the Try another card message."},
		},
		TestedHeadSHA: headSHA,
	}), "")

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| user reaches the success screen | ✅ pass | live | After paying, the success screen shows the order number and a Continue shopping button |",
		"| declined card shows the retry copy | ✅ pass | live | With a declined test card the form stays filled in and shows the Try another card message |",
		"| refund email is sent | ⏸️ untested | no | no mail sandbox credential on this machine; set SMTP_SANDBOX_URL to drive it |",
		"- Screenshot: The success screen shows the order number and a Continue shopping button.",
		"- Recording: A declined card keeps the form filled in and shows the Try another card message.",
	} {
		if !strings.Contains(content.Body, want) {
			t.Errorf("expected the Testing section to read %q:\n%s", want, content.Body)
		}
	}
	for _, unwanted := range []string{"tile-03", "tile-04", ".png", ".webm", sctx.EvidenceDir} {
		if strings.Contains(content.Body, unwanted) {
			t.Errorf("PR body names a local evidence file %q:\n%s", unwanted, content.Body)
		}
	}
	assertNoMediaInPRBody(t, content.Body)
}
