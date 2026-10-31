package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// verifiedDoc renders a Markdown doc whose leading frontmatter carries a
// top-level verified date ("" leaves the key out).
func verifiedDoc(verified, body string) string {
	frontmatter := "---\ntitle: Doc\n"
	if verified != "" {
		frontmatter += "verified: " + verified + "\n"
	}
	return frontmatter + "---\n\n" + body + "\n"
}

// seedDocs commits files on top of the template's feature head and returns
// the repo, its base, and the new head the document step starts from.
func seedDocs(t *testing.T, files map[string]string) (string, string, string) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	for name, content := range files {
		writeDoc(t, dir, name, content)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "seed docs")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	return dir, baseSHA, headSHA
}

func writeDoc(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func committedDoc(t *testing.T, dir, name string) string {
	t.Helper()
	return gitCmd(t, dir, "show", "HEAD:"+name) + "\n"
}

// documentModes runs a scenario through each pass that commits document
// agent edits: the document-only pass (a configured lint command), the
// combined housekeeping pass (no lint command), and a manual fix round.
var documentModes = []struct {
	name   string
	cmds   config.Commands
	fixing bool
}{
	{name: "document pass", cmds: config.Commands{Lint: "true"}},
	{name: "housekeeping pass", cmds: config.Commands{}},
	{name: "manual fix round", cmds: config.Commands{Lint: "true"}, fixing: true},
}

func newDocumentGuardContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string, cmds config.Commands, fixing bool) (*pipeline.StepContext, *[]string) {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Shared = &pipeline.RunShared{}
	var logs []string
	sctx.Log = func(s string) {
		logs = append(logs, s)
		t.Log(s)
	}
	if fixing {
		sctx.Fixing = true
		sctx.PreviousFindings = `{"findings":[{"id":"doc-1","severity":"warning","file":"docs/a.md","description":"a is stale","action":"ask-user"}],"summary":"a stale"}`
	}
	return sctx, &logs
}

func TestDocumentStep_VerifiedDateMovesOnlyOnAReportedDoc(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/a.md":   verifiedDoc("2020-01-01", "Alpha."),
				"docs/b.md":   verifiedDoc("2020-01-01", "Bravo."),
				"docs/c.mdx":  verifiedDoc("2020-01-01", "Charlie."),
				"docs/d.md":   verifiedDoc("", "Delta."),
				"docs/e.txt":  verifiedDoc("2020-01-01", "Echo."),
				"docs/f.md":   "# No frontmatter\n\nverified: 2020-01-01\n",
				"docs/g/h.md": "---\nmeta:\n  verified: 2020-01-01\n---\n\nNested.\n",
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/a.md", verifiedDoc("2021-06-15", "Alpha."))
					writeDoc(t, dir, "docs/b.md", verifiedDoc("2021-06-15", "Bravo."))
					writeDoc(t, dir, "docs/c.mdx", verifiedDoc("2021-06-15", "Charlie."))
					writeDoc(t, dir, "docs/d.md", verifiedDoc("2021-06-15", "Delta."))
					writeDoc(t, dir, "docs/e.txt", verifiedDoc("2021-06-15", "Echo."))
					writeDoc(t, dir, "docs/f.md", "# No frontmatter\n\nverified: 2021-06-15\n")
					writeDoc(t, dir, "docs/g/h.md", "---\nmeta:\n  verified: 2021-06-15\n---\n\nNested.\n")
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"refresh docs","read_against_code":["./docs/a.md"]}`)}, nil
				},
			}
			sctx, logs := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)

			outcome, err := (&DocumentStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval {
				t.Fatal("no findings must not park the step")
			}
			for name, want := range map[string]string{
				"docs/a.md":  verifiedDoc("2021-06-15", "Alpha."),
				"docs/b.md":  verifiedDoc("2020-01-01", "Bravo."),
				"docs/c.mdx": verifiedDoc("2020-01-01", "Charlie."),
				// A date the step introduced on an unread doc is removed.
				"docs/d.md": verifiedDoc("", "Delta."),
				// Not Markdown, no frontmatter, and a nested key: untouched.
				"docs/e.txt":  verifiedDoc("2021-06-15", "Echo."),
				"docs/f.md":   "# No frontmatter\n\nverified: 2021-06-15\n",
				"docs/g/h.md": "---\nmeta:\n  verified: 2021-06-15\n---\n\nNested.\n",
			} {
				if got := committedDoc(t, dir, name); got != want {
					t.Errorf("%s committed as\n%s\nwant\n%s", name, got, want)
				}
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree not clean after the guarded commit: %q", status)
			}
			joined := strings.Join(*logs, "\n")
			for _, want := range []string{
				"docs/b.md: 2021-06-15 -> 2020-01-01",
				"docs/c.mdx: 2021-06-15 -> 2020-01-01",
				"docs/d.md: 2021-06-15 -> (absent)",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("logs missing restoration %q:\n%s", want, joined)
				}
			}
			if strings.Contains(joined, "docs/a.md:") {
				t.Errorf("the reported doc must not be restored:\n%s", joined)
			}
			schema := string(ag.calls[0].JSONSchema)
			if !strings.Contains(schema, `"read_against_code"`) {
				t.Errorf("schema does not offer read_against_code:\n%s", schema)
			}
		})
	}
}

// TestDocumentStep_EditingADocDoesNotEarnItsVerifiedDate: a doc the step
// edited for another reason (a corrected sentence, a new page) keeps its body
// edit, but its date moves only when the step reports reading it in full.
func TestDocumentStep_EditingADocDoesNotEarnItsVerifiedDate(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/a.md": verifiedDoc("2020-01-01", "Alpha."),
				"docs/b.md": verifiedDoc("2020-01-01", "Bravo."),
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/a.md", verifiedDoc("2021-06-15", "Alpha, corrected."))
					writeDoc(t, dir, "docs/b.md", verifiedDoc("2021-06-15", "Bravo, corrected."))
					writeDoc(t, dir, "docs/new.md", verifiedDoc("2021-06-15", "New page."))
					writeDoc(t, dir, "docs/new-read.md", verifiedDoc("2021-06-15", "New page, read."))
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"correct docs","read_against_code":["docs/b.md","docs/new-read.md"]}`)}, nil
				},
			}
			sctx, logs := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)

			if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{
				"docs/a.md":        verifiedDoc("2020-01-01", "Alpha, corrected."),
				"docs/b.md":        verifiedDoc("2021-06-15", "Bravo, corrected."),
				"docs/new.md":      verifiedDoc("", "New page."),
				"docs/new-read.md": verifiedDoc("2021-06-15", "New page, read."),
			} {
				if got := committedDoc(t, dir, name); got != want {
					t.Errorf("%s committed as\n%s\nwant\n%s", name, got, want)
				}
			}
			joined := strings.Join(*logs, "\n")
			for _, want := range []string{
				"docs/a.md: 2021-06-15 -> 2020-01-01 (not reported in read_against_code)",
				"docs/new.md: 2021-06-15 -> (absent) (not reported in read_against_code)",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("logs missing restoration %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestDocumentStep_VerifiedDateNeverLoweredNorLaterThanTheRunsDay(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/lowered.md": verifiedDoc("2020-01-01", "Lowered."),
				"docs/future.md":  verifiedDoc("2020-01-01", "Future."),
				"docs/edited.md":  verifiedDoc("2020-01-01", "Edited."),
				"docs/quoted.md":  verifiedDoc(`"2020-01-01"`, "Quoted."),
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/lowered.md", verifiedDoc("2019-12-31", "Lowered."))
					writeDoc(t, dir, "docs/future.md", verifiedDoc("2999-01-01", "Future."))
					writeDoc(t, dir, "docs/edited.md", verifiedDoc("2999-01-01", "Edited body."))
					writeDoc(t, dir, "docs/quoted.md", verifiedDoc(`'2019-06-01'`, "Quoted."))
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"refresh docs","read_against_code":["docs/lowered.md","docs/future.md","docs/edited.md","docs/quoted.md"]}`)}, nil
				},
			}
			sctx, logs := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)

			if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{
				"docs/lowered.md": verifiedDoc("2020-01-01", "Lowered."),
				"docs/future.md":  verifiedDoc("2020-01-01", "Future."),
				// The body edit stays; only the date is restored.
				"docs/edited.md": verifiedDoc("2020-01-01", "Edited body."),
				"docs/quoted.md": verifiedDoc(`"2020-01-01"`, "Quoted."),
			} {
				if got := committedDoc(t, dir, name); got != want {
					t.Errorf("%s committed as\n%s\nwant\n%s", name, got, want)
				}
			}
			joined := strings.Join(*logs, "\n")
			for _, want := range []string{
				"docs/lowered.md: 2019-12-31 -> 2020-01-01",
				"docs/future.md: 2999-01-01 -> 2020-01-01",
				"docs/edited.md: 2999-01-01 -> 2020-01-01",
				"docs/quoted.md: 2019-06-01 -> 2020-01-01",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("logs missing restoration %q:\n%s", want, joined)
				}
			}
		})
	}
}

// TestDocumentStep_UnreadableOutputRestoresEveryMovedDate: an output the
// step cannot trust gives an empty reported list, so every moved date is
// restored, and the step still fails or parks exactly as it does without the
// guard.
func TestDocumentStep_UnreadableOutputRestoresEveryMovedDate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		result  *agent.Result
		wantErr string
		park    bool
	}{
		{name: "no structured output", result: &agent.Result{Text: "updated the docs"}, wantErr: "document analyzer returned no structured findings"},
		{name: "invalid json", result: &agent.Result{Output: json.RawMessage(`{"read_against_code":["docs/a.md"]`)}, wantErr: "validate document analyzer findings"},
		{name: "schema-invalid output", result: &agent.Result{Output: json.RawMessage(`{"findings":[],"read_against_code":["docs/a.md"]}`)}, wantErr: "validate document analyzer findings"},
		{name: "read_against_code of the wrong type", result: &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"docs","read_against_code":"docs/a.md"}`)}, park: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/a.md": verifiedDoc("2020-01-01", "Alpha."),
				"docs/b.md": verifiedDoc("2020-01-01", "Bravo."),
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/a.md", verifiedDoc("2021-06-15", "Alpha."))
					writeDoc(t, dir, "docs/b.md", verifiedDoc("2021-06-15", "Bravo, corrected."))
					return tc.result, nil
				},
			}
			sctx, _ := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"}, false)

			outcome, err := (&DocumentStep{}).Execute(sctx)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Execute() error = %v, want %q", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if outcome.NeedsApproval != tc.park {
					t.Fatalf("NeedsApproval = %v, want %v", outcome.NeedsApproval, tc.park)
				}
			}
			if got, want := committedDoc(t, dir, "docs/a.md"), verifiedDoc("2020-01-01", "Alpha."); got != want {
				t.Errorf("docs/a.md committed as\n%s\nwant\n%s", got, want)
			}
			// A body edit is kept, but it does not earn the date.
			if got, want := committedDoc(t, dir, "docs/b.md"), verifiedDoc("2020-01-01", "Bravo, corrected."); got != want {
				t.Errorf("docs/b.md committed as\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestDocumentStep_PromptMovesVerifiedDatesOnlyOnARead(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
				},
			}
			sctx, _ := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)
			if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			prompt := ag.calls[0].Prompt
			for _, want := range []string{
				"Move a doc's `verified` frontmatter date only when you read that doc in full against the code it describes in this pass",
				`list each such doc's path in "read_against_code"`,
				"never lower a date",
				"never set one later than today",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q:\n%s", want, prompt)
				}
			}
			// Editing a doc is not reading it, so neither the rule nor the
			// schema offers an edit as a reason to move its date.
			for _, text := range []string{prompt, string(ag.calls[0].JSONSchema)} {
				for _, banned := range []string{"nor edited", "content you changed"} {
					if strings.Contains(text, banned) {
						t.Errorf("prompt or schema still exempts edited docs (%q)", banned)
					}
				}
			}
		})
	}
}

// TestDocumentSchemasOfferAnOptionalReadAgainstCode: both schemas the step
// uses offer read_against_code as an optional list of paths.
func TestDocumentSchemasOfferAnOptionalReadAgainstCode(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]json.RawMessage{
		"document":     documentFindingsSchema,
		"housekeeping": housekeepingFindingsSchema,
	} {
		var schema struct {
			Properties map[string]struct {
				Type  string `json:"type"`
				Items struct {
					Type string `json:"type"`
				} `json:"items"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", name, err)
		}
		prop, ok := schema.Properties["read_against_code"]
		if !ok || prop.Type != "array" || prop.Items.Type != "string" {
			t.Errorf("%s schema read_against_code = %+v, want an array of strings", name, prop)
		}
		for _, required := range schema.Required {
			if required == "read_against_code" {
				t.Errorf("%s schema requires read_against_code; it must stay optional", name)
			}
		}
	}
}
