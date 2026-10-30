package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// No image or video of the work leaves the machine: the PR step uploads
// nothing, and no PR body it writes embeds or links an image or a video, on any
// provider or body flavor. Image and video evidence is described by its caption
// in words.

const noMediaAttachmentURL = "https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa"

// noMediaInjection is pipeline-generated text that itself tries every media
// shape: image markdown, image and video HTML, a user-attachments URL, and
// plain links to an image and a video.
const noMediaInjection = `see ![shot](y.png) and <img src="https://example.com/z.png"> and <IMG SRC=q.gif> and ` +
	`<video src="clip.mp4" controls></video> and <picture><source srcset="a.webp"></picture> and ` +
	noMediaAttachmentURL + ` and [raw shot](https://example.com/raw/shot.png?raw=true) and [clip](clips/run.webm) and <a href="https://example.com/b.jpg">b</a>`

// noMediaForbidden are the substrings no PR body may carry, compared
// case-insensitively.
var noMediaForbidden = []string{"![", "<img", "<video", "<picture", "<source", "user-attachments"}

var (
	markdownLinkTarget = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)`)
	htmlLinkTarget     = regexp.MustCompile(`(?i)\b(?:href|src|srcset|poster)\s*=\s*["']?([^"'\s>]+)`)
	bareURLTarget      = regexp.MustCompile(`(?i)https?://[^\s<>"'\x60)\]]+`)
	mediaCaptionLine   = regexp.MustCompile(`(?m)^- (?:Screenshot|Recording): `)
)

// assertNoMediaInPRBody fails when body embeds, uploads or links an image or a
// video in any shape a forge renders.
func assertNoMediaInPRBody(t *testing.T, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, needle := range noMediaForbidden {
		if strings.Contains(lower, needle) {
			t.Errorf("PR body contains %q:\n%s", needle, body)
		}
	}
	for _, pattern := range []*regexp.Regexp{markdownLinkTarget, htmlLinkTarget, bareURLTarget} {
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			target := match[len(match)-1]
			if evidence.IsMedia("", target) {
				t.Errorf("PR body links to the media target %q (%q):\n%s", target, match[0], body)
			}
		}
	}
	// A forge's autolinker reads text after entities are decoded, so a URL
	// inside HTML-escaped text must not classify as media either.
	for _, target := range bareURLTarget.FindAllString(html.UnescapeString(body), -1) {
		if evidence.IsMedia("", target) {
			t.Errorf("PR body autolinks the media URL %q once entities are decoded:\n%s", target, body)
		}
	}
}

type noMediaFixture struct {
	sctx          *pipeline.StepContext
	baseSHA       string
	screenshot    string
	uncaptioned   string
	recording     string
	cliLog        string
	templateDraft bool
}

// newNoMediaFixture records a run whose every PR-body source carries media:
// the agent's title and body, the intent, a review finding, its risk
// rationale, a fix round's summary, the Test step's summary, tested commands,
// scenario cells, artifact captions, embedded evidence text, and image, video
// and text artifacts reported by local path, repository path and remote URL.
func newNoMediaFixture(t *testing.T, template bool) *noMediaFixture {
	t.Helper()
	f := &noMediaFixture{templateDraft: template}
	var sctx *pipeline.StepContext
	if template {
		sctx, _, _ = templateTestContext(t)
		f.baseSHA = sctx.Run.BaseSHA
		sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			data, _ := json.Marshal(prContent{Title: "feat(pipeline): fill template", Body: filledPRTemplate + "\nNarrative: " + noMediaInjection + "\n"})
			return &agent.Result{Output: data}, nil
		}}
	} else {
		dir, baseSHA, headSHA := setupGitRepo(t)
		f.baseSHA = baseSHA
		sctx = newTestContextWithDBRecords(t, &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			data, _ := json.Marshal(prContent{Title: "feat: checkout total", Body: "## What Changed\n\n- " + noMediaInjection})
			return &agent.Result{Output: data}, nil
		}}, dir, baseSHA, headSHA, config.Commands{})
		review := insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
			Items:         []types.Finding{{ID: "review-1", Severity: types.FindingSeverityWarning, File: "web/checkout.go", Line: 3, Description: "finding " + noMediaInjection, Action: types.ActionAutoFix}},
			RiskLevel:     "low",
			RiskRationale: "risk " + noMediaInjection,
		}), "")
		initial := findingsJSON(t, types.Findings{Items: []types.Finding{{ID: "review-1", Severity: types.FindingSeverityWarning, Description: "finding " + noMediaInjection, Action: types.ActionAutoFix}}})
		fixed := findingsJSON(t, types.Findings{})
		fixSummary := "fix summary " + noMediaInjection
		if _, err := sctx.DB.InsertStepRound(review.ID, 1, "initial", &initial, nil, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.DB.InsertStepRound(review.ID, 2, "auto_fix", &fixed, &fixSummary, 10); err != nil {
			t.Fatal(err)
		}
	}
	sctx.Config.Test = config.Merge(&config.GlobalConfig{}, &config.RepoConfig{}).Test
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.UserIntent = "intent " + noMediaInjection
	f.sctx = sctx

	f.screenshot = writeNoMediaEvidence(t, sctx.EvidenceDir, "checkout.png", "\x89PNG bytes")
	f.uncaptioned = writeNoMediaEvidence(t, sctx.EvidenceDir, "uncaptioned.png", "\x89PNG bytes")
	f.recording = writeNoMediaEvidence(t, sctx.EvidenceDir, "checkout-flow.mp4", "ftyp")
	f.cliLog = writeNoMediaEvidence(t, sctx.EvidenceDir, "cli-run.txt", "checkout total printed: 42.00\n")
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "summary " + noMediaInjection,
		Tested:         []string{"go test ./... " + noMediaInjection},
		Scenarios:      []types.TestScenario{{Name: "user sees the total", Result: types.ScenarioResultPass, Live: true, Evidence: "cell " + noMediaInjection}},
		Artifacts: []types.TestArtifact{
			{Kind: "screenshot", Label: "checkout.png", Path: f.screenshot, Content: "The checkout page shows the order total 42.00 and an enabled Pay button."},
			{Kind: "screenshot", Label: "Uncaptioned tile", Path: f.uncaptioned},
			{Kind: "video", Label: "checkout-flow.mp4", Path: f.recording, Content: "The checkout flow completes and lands on the receipt page."},
			{Kind: "image", Label: "Remote shot", URL: "https://example.com/remote/shot.png", Content: "The remote dashboard shows one green check."},
			{Kind: "log", Label: "Linked webp", URL: "https://example.com/build/screen.webp?x=1", Content: "The build page shows the deploy as healthy."},
			{Kind: "screenshot", Label: "Uploaded", URL: noMediaAttachmentURL, Content: "The settings page shows the saved theme."},
			{Kind: "log", Label: "Repo shot", Path: "docs/shot.png", Content: "The docs page shows the new diagram."},
			{Kind: "screenshot", Label: "Injected caption", Path: f.screenshot, Content: "caption " + noMediaInjection},
			{Kind: "log", Label: "CLI run", Path: f.cliLog},
			{Kind: "command-output", Label: "Agent output", Content: "output " + noMediaInjection},
		},
	}), "")
	return f
}

func writeNoMediaEvidence(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *noMediaFixture) build(t *testing.T, provider scm.Provider) prContent {
	t.Helper()
	// An owned template fails closed rather than drop recorded evidence, and
	// this fixture's evidence is larger than Azure DevOps' cap, so template
	// bodies are built without the host cap; the flavor still follows the
	// provider.
	limit := scm.MaxPRBodyChars(provider)
	if f.templateDraft {
		limit = 0
	}
	content, err := (&PRStep{}).buildPRContent(f.sctx, "feature", "main", f.baseSHA, provider, limit)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// TestPRStep_NoProviderOrFlavorEmbedsOrLinksMedia builds a PR body for every
// provider (which picks the HTML or the Markdown flavor), both as an ordinary
// draft and as an owned template composition, from a run whose every source
// carries media.
func TestPRStep_NoProviderOrFlavorEmbedsOrLinksMedia(t *testing.T) {
	t.Parallel()
	providers := []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderAzureDevOps, scm.ProviderBitbucket}
	for _, template := range []bool{false, true} {
		for _, provider := range providers {
			name := fmt.Sprintf("%s/ordinary", provider)
			if template {
				name = fmt.Sprintf("%s/template", provider)
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newNoMediaFixture(t, template)
				content := f.build(t, provider)
				assertNoMediaInPRBody(t, content.Body)
				assertNoMediaInPRBody(t, content.Title)
				if template {
					if _, err := parsePROwnedBody(content.Body); err != nil {
						t.Fatalf("neutralized owned body must keep its integrity guard: %v", err)
					}
				}
				// Only Azure DevOps' cap may shed the Testing section of an
				// ordinary draft; everywhere else it must describe what was seen.
				if !strings.Contains(content.Body, "## Testing") {
					if provider == scm.ProviderAzureDevOps && !template {
						return
					}
					t.Fatalf("PR body lost its Testing section:\n%s", content.Body)
				}
				for _, want := range []string{
					"- Screenshot: The checkout page shows the order total 42.00 and an enabled Pay button.",
					"- Recording: The checkout flow completes and lands on the receipt page.",
					"- Screenshot: The remote dashboard shows one green check.",
					"- Screenshot: The build page shows the deploy as healthy.",
					"- Screenshot: The settings page shows the saved theme.",
					"- Screenshot: The docs page shows the new diagram.",
					"checkout total printed: 42.00",
				} {
					if !strings.Contains(content.Body, want) {
						t.Errorf("PR body is missing %q:\n%s", want, content.Body)
					}
				}
				for _, unwanted := range []string{"checkout.png", "uncaptioned.png", "Uncaptioned tile", "checkout-flow.mp4", "docs/shot.png", "remote/shot.png", "screen.webp", filepath.Base(f.sctx.EvidenceDir)} {
					if strings.Contains(content.Body, unwanted) {
						t.Errorf("PR body names the media file or tile %q:\n%s", unwanted, content.Body)
					}
				}
			})
		}
	}
}

// TestPRStep_ScreenshotRendersAsItsCaptionOnly is the reproduction: the
// default config on GitHub used to upload the PNG to user-attachments and embed
// it as ![...](...user-attachments...).
func TestPRStep_ScreenshotRendersAsItsCaptionOnly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: demo change","body":"## What Changed\n\n- demo"}`)}, nil
	}}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test = config.Merge(&config.GlobalConfig{}, &config.RepoConfig{}).Test
	png := writeNoMediaEvidence(t, sctx.EvidenceDir, "checkout.png", "\x89PNG bytes")
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "Checked the checkout page.",
		Artifacts: []types.TestArtifact{
			{Kind: "screenshot", Label: "Checkout screenshot", Path: png, Content: "The checkout page shows the order total."},
			{Kind: "screenshot", Label: "Second screenshot", Path: png},
		},
	}), "")

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertNoMediaInPRBody(t, content.Body)
	if got := mediaCaptionLine.FindAllString(content.Body, -1); len(got) != 1 {
		t.Fatalf("want exactly one caption line, got %d:\n%s", len(got), content.Body)
	}
	if !strings.Contains(content.Body, "- Screenshot: The checkout page shows the order total.\n") {
		t.Fatalf("captioned screenshot must render as its caption:\n%s", content.Body)
	}
	for _, unwanted := range []string{"checkout.png", "Checkout screenshot", "Second screenshot", "local file:"} {
		if strings.Contains(content.Body, unwanted) {
			t.Fatalf("screenshot rendering must carry no file name, label or path, found %q:\n%s", unwanted, content.Body)
		}
	}
}

// TestPRStep_StoreInRepoPublishesTheLogAndNotTheScreenshot covers the opt-in
// evidence branch: a run with a PNG and a log publishes only the log, links
// only the log, and describes the screenshot by its caption.
func TestPRStep_StoreInRepoPublishesTheLogAndNotTheScreenshot(t *testing.T) {
	t.Parallel()
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: demo change","body":"## What Changed\n\n- demo"}`)}, nil
	}}
	png := writeNoMediaEvidence(t, testEvidenceDir(sctx), "checkout.png", "\x89PNG binary")
	logPath := writeNoMediaEvidence(t, testEvidenceDir(sctx), "server.bin", "\x00\x01 binary log")
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "Evidence was collected.",
		Artifacts: []types.TestArtifact{
			{Kind: "screenshot", Label: "Checkout screenshot", Path: png, Content: "The checkout page shows the order total."},
			{Kind: "log", Label: "Server log", Path: logPath},
		},
	}), "")

	content, err := (&PRStep{}).buildPRContent(sctx, "feature/add-login", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	tree := gitCmd(t, remote, "ls-tree", "-r", "--name-only", tip)
	if strings.Contains(tree, "checkout.png") || !strings.Contains(tree, ".no-mistakes/evidence/feature/add-login/server.bin") {
		t.Fatalf("evidence branch must carry the log and not the screenshot:\n%s", tree)
	}
	assertNoMediaInPRBody(t, content.Body)
	wantLink := "- Evidence: [Server log](https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/server.bin)"
	if !strings.Contains(content.Body, wantLink) {
		t.Fatalf("expected the commit-pinned log link %q:\n%s", wantLink, content.Body)
	}
	if !strings.Contains(content.Body, "- Screenshot: The checkout page shows the order total.") {
		t.Fatalf("expected the screenshot's caption:\n%s", content.Body)
	}
}

// TestPRStep_TextEvidenceStillRenders keeps logs and command output exactly as
// they rendered before the media rule.
func TestPRStep_TextEvidenceStillRenders(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test = config.Merge(&config.GlobalConfig{}, &config.RepoConfig{}).Test
	logPath := writeNoMediaEvidence(t, sctx.EvidenceDir, "cli-run.txt", "it works\n")
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "Evidence was collected.",
		Artifacts: []types.TestArtifact{
			{Kind: "log", Label: "CLI run", Path: logPath},
			{Kind: "command-output", Label: "Server log", Content: "POST /checkout 200"},
			{Kind: "log", Label: "Remote log", URL: "https://example.com/build.log"},
		},
	}), "")

	_, _, testing := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
	for _, want := range []string{
		"<details>\n<summary>Evidence: CLI run</summary>\n\n```text\nit works\n```\n</details>",
		"<details>\n<summary>Evidence: Server log</summary>\n\n```text\nPOST /checkout 200\n```\n</details>",
		"- Evidence: [Remote log](https://example.com/build.log)",
	} {
		if !strings.Contains(testing, want) {
			t.Errorf("text evidence lost %q:\n%s", want, testing)
		}
	}
}

// TestPRStep_PostsNoPRCommentsAndUploadsNothing drives the whole step against
// the fake gh, for a new PR and for an update, and checks every forge call it
// made: the body is written through create/edit only, and nothing comments or
// uploads.
func TestPRStep_PostsNoPRCommentsAndUploadsNothing(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"", "https://github.com/example/widgets/pull/7"} {
		name := "create"
		if existing != "" {
			name = "update"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newNoMediaFixture(t, false)
			env, logFile := fakeGH(t, existing)
			f.sctx.Env = env
			if _, err := (&PRStep{}).Execute(f.sctx); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			log := string(data)
			t.Logf("gh calls:\n%s", log)
			assertNoMediaInPRBody(t, log)
			for _, line := range strings.Split(log, "\n") {
				lower := strings.ToLower(line)
				// "auth token" is how the deleted uploader fetched its credential.
				if strings.Contains(lower, "comment") || strings.Contains(lower, "upload") || strings.HasPrefix(lower, "auth token") {
					t.Errorf("PR step made a forge call beyond the PR itself: %q", line)
				}
			}
			if !strings.Contains(log, "pr create") && !strings.Contains(log, "pr edit") {
				t.Fatalf("expected the PR body to be written, gh log:\n%s", log)
			}
		})
	}
}

func TestNeutralizePRMedia_BreaksEveryMediaShapeAndKeepsOrdinaryLinks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`![shot](y.png)`, `!\[shot]\(y.png)`},
		{`<img src="a.png">`, `<\img src="(image link removed)">`},
		{`<Video src=clip.mp4>`, `<\Video src=(video link removed)>`},
		{`<picture><source srcset="a.webp"></picture>`, `<\picture><\source srcset="(image link removed)"></picture>`},
		{`see https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa now`, `see (attachment link removed) now`},
		{`[shot](https://example.com/a.png?raw=true)`, `[shot]\((image link removed))`},
		{`(see https://example.com/a.png).`, `(see (image link removed)).`},
		{`&lt;img src=&#34;https://example.com/z.png&#34;&gt;`, `&lt;img src=&#34;(image link removed)&#34;&gt;`},
		{`<a href="https://example.com/b.jpg">b</a>`, `<\a href="(image link removed)">b</a>`},
		{`Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)`, `Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)`},
		{`- Evidence: [Server log](https://github.com/o/r/blob/abc/logs/server.log)`, `- Evidence: [Server log](https://github.com/o/r/blob/abc/logs/server.log)`},
		{`<a href="https://example.com/docs">docs</a> and <!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"abc"} -->`, `<a href="https://example.com/docs">docs</a> and <!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"abc"} -->`},
	} {
		got := neutralizePRMedia(tc.in)
		if got != tc.want {
			t.Errorf("neutralizePRMedia(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
		if again := neutralizePRMedia(got); again != got {
			t.Errorf("neutralizePRMedia is not idempotent on %q: %q", got, again)
		}
	}
}

// TestComposeOwnedPRContent_NeverRewritesAuthorText keeps the owned-template
// contract: the generated appendix is neutralized before its integrity guard
// is stamped, and the author's live text around it is published as written.
func TestComposeOwnedPRContent_NeverRewritesAuthorText(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	author := "# Overview\n\nThe author's own ![diagram](docs/flow.png).\n\n"
	after := "\nAuthor footer <img src=\"logo.png\">"
	content, err := composeOwnedPRContent(prOwnedBody{before: author, after: after, managed: true}, "", appendix+"\n\nGenerated "+noMediaInjection, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatalf("composed body must keep its integrity guard: %v", err)
	}
	if parts.before != author || parts.after != after {
		t.Fatalf("author text was rewritten:\nbefore %q\nafter  %q", parts.before, parts.after)
	}
	assertNoMediaInPRBody(t, parts.appendix)
}
