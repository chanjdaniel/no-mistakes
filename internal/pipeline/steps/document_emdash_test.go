package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// TestDocumentStep_EmDashRewrittenOnlyOnLinesTheStepWrote: an em dash on a
// Markdown line the agent added or changed is committed as a plain dash; one
// on a line it left alone (in the same file), and any in a non-Markdown file,
// is committed as it was.
func TestDocumentStep_EmDashRewrittenOnlyOnLinesTheStepWrote(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			start := "# Rules\n\nNever use the em dash \"—\".\nKeep this — as quoted.\nOld line.\n"
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/a.md":   start,
				"docs/b.mdx":  "Intro.\n",
				"docs/c.txt":  "Plain.\n",
				"docs/d.MD":   "Same — here.\n",
				"docs/e/f.md": "---\ntitle: F\n---\n\nBody.\n",
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/a.md", "# Rules\n\nNever use the em dash \"—\".\nKeep this — as quoted.\nOld line — changed.\nAdded — spaced.\nAdded a—b.\n")
					writeDoc(t, dir, "docs/b.mdx", "Intro.\nNew — line\r\nEnd.\n")
					writeDoc(t, dir, "docs/c.txt", "Plain.\nNew — line.\n")
					writeDoc(t, dir, "docs/d.MD", "Same — here.\n\nNew—tail")
					writeDoc(t, dir, "docs/new.md", "Brand — new.\nAlso—new.\n")
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"refresh docs"}`)}, nil
				},
			}
			sctx, logs := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)

			if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{
				"docs/a.md":  "# Rules\n\nNever use the em dash \"—\".\nKeep this — as quoted.\nOld line - changed.\nAdded - spaced.\nAdded a-b.\n",
				"docs/b.mdx": "Intro.\nNew - line\r\nEnd.\n",
				// Not Markdown: never rewritten.
				"docs/c.txt": "Plain.\nNew — line.\n",
				// The new last line has no newline (committedDoc adds one).
				"docs/d.MD":   "Same — here.\n\nNew-tail\n",
				"docs/new.md": "Brand - new.\nAlso-new.\n",
				// Unchanged: not rewritten, not committed.
				"docs/e/f.md": "---\ntitle: F\n---\n\nBody.\n",
			} {
				if got := committedDoc(t, dir, name); got != want {
					t.Errorf("%s committed as\n%q\nwant\n%q", name, got, want)
				}
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree not clean after the rewritten commit: %q", status)
			}
			joined := strings.Join(*logs, "\n")
			for _, want := range []string{
				"rewrote 3 em dash(es) in docs/a.md",
				"rewrote 1 em dash(es) in docs/b.mdx",
				"rewrote 1 em dash(es) in docs/d.MD",
				"rewrote 2 em dash(es) in docs/new.md",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("log missing %q:\n%s", want, joined)
				}
			}
			if strings.Contains(joined, "docs/c.txt") {
				t.Errorf("a non-Markdown file was rewritten:\n%s", joined)
			}
		})
	}
}

// TestDocumentStep_EmDashRewriteRunsWithTheVerifiedGuard: a doc whose body
// line and verified date the step both changed keeps the guard's verdict and
// loses only the em dash it wrote.
func TestDocumentStep_EmDashRewriteRunsWithTheVerifiedGuard(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := seedDocs(t, map[string]string{
		"docs/a.md": verifiedDoc("2020-01-01", "Alpha — kept."),
	})
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			writeDoc(t, dir, "docs/a.md", verifiedDoc("2999-01-01", "Alpha — kept.\nNew — line."))
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"refresh docs"}`)}, nil
		},
	}
	sctx, _ := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, documentModes[0].cmds, false)
	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got, want := committedDoc(t, dir, "docs/a.md"), verifiedDoc("2020-01-01", "Alpha — kept.\nNew - line."); got != want {
		t.Errorf("docs/a.md committed as\n%q\nwant\n%q", got, want)
	}
}

// TestDocumentStep_EmDashInCodeIsNeverRewritten: an em dash the step writes
// inside a fenced code block or an inline code span is code, so it is
// committed as written, while a bare one beside it on the same line is
// rewritten. A fence opened before the step's lines still makes them code.
func TestDocumentStep_EmDashInCodeIsNeverRewritten(t *testing.T) {
	t.Parallel()
	for _, mode := range documentModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			start := "# Sample\n\n```go\nconst dash = \"x\"\n```\n\nIntro.\n"
			dir, baseSHA, headSHA := seedDocs(t, map[string]string{
				"docs/a.md": start,
			})
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					writeDoc(t, dir, "docs/a.md", "# Sample\n\n```go\nconst dash = \"x\"\nconst em = \"—\" // a — b\n```\n\nIntro.\nThe rule quotes `\"—\"` and ``a — `b` — c`` — then prose.\n~~~~\nTilde — fenced\n~~~\nstill — fenced\n~~~~\nAfter — the fence, \\`not — code\\`.\n> ```\n> quoted — code\n> ```\nA span `opened —\nacross — lines` — ends.\n")
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"refresh docs"}`)}, nil
				},
			}
			sctx, logs := newDocumentGuardContext(t, ag, dir, baseSHA, headSHA, mode.cmds, mode.fixing)

			if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			want := "# Sample\n\n```go\nconst dash = \"x\"\nconst em = \"—\" // a — b\n```\n\nIntro.\nThe rule quotes `\"—\"` and ``a — `b` — c`` - then prose.\n~~~~\nTilde — fenced\n~~~\nstill — fenced\n~~~~\nAfter - the fence, \\`not - code\\`.\n> ```\n> quoted — code\n> ```\nA span `opened —\nacross — lines` - ends.\n"
			if got := committedDoc(t, dir, "docs/a.md"); got != want {
				t.Errorf("docs/a.md committed as\n%q\nwant\n%q", got, want)
			}
			if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "rewrote 4 em dash(es) in docs/a.md") {
				t.Errorf("log missing the count of bare em dashes:\n%s", joined)
			}
		})
	}
}

func TestDocumentStep_PromptForbidsTheEmDash(t *testing.T) {
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
				"- Never write the em dash (\"—\"); use \"-\".",
				"rewrites any em dash outside code on a Markdown line you added or changed",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestEmDashRewriteLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, line, want string
	}{
		{name: "spaced", line: "a — b\n", want: "a - b\n"},
		{name: "joined", line: "a—b", want: "a-b"},
		{name: "several", line: "— x — y—z —\r\n", want: "- x - y-z -\r\n"},
		{name: "none", line: "a - b – c\n", want: "a - b – c\n"},
		{name: "inline code span", line: "a — `b — c` — ``d`—`e``\n", want: "a - `b — c` - ``d`—`e``\n"},
		{name: "unmatched backtick is literal", line: "a ` b — c\n", want: "a ` b - c\n"},
		{name: "escaped backtick opens no span", line: "\\`a — b`\n", want: "\\`a - b`\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := rewriteEmDash(tc.line, markdownCode(tc.line)); got != tc.want {
				t.Errorf("rewriteEmDash(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestMarkdownCodeFences: which lines markdownCode treats as a fenced code
// block, fences included.
func TestMarkdownCodeFences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		code    []bool // per line
	}{
		{name: "backtick fence", content: "a\n```sh\nb\n```\nc\n", code: []bool{false, true, true, true, false}},
		{name: "a shorter run does not close", content: "````\n```\nb\n````\nc\n", code: []bool{true, true, true, true, false}},
		{name: "the other character does not close", content: "~~~\n```\n~~~\nc\n", code: []bool{true, true, true, false}},
		{name: "unclosed fence runs to the end", content: "a\n```\nb\nc", code: []bool{false, true, true, true}},
		{name: "backtick in the info string is not a fence", content: "``` a`b\nc\n", code: []bool{false, false}},
		{name: "two backticks are not a fence", content: "``\nc\n", code: []bool{false, false}},
		{name: "fence in a list item", content: "1. step\n   ```\n   b\n   ```\nc\n", code: []bool{false, true, true, true, false}},
		{name: "fence after a list marker", content: "- ```\n  b\n  ```\nc\n", code: []bool{true, true, true, false}},
		{name: "fence in a blockquote", content: "> ```\n> b\n> ```\nc\n", code: []bool{true, true, true, false}},
		{name: "a closing fence with text after it does not close", content: "```\n``` x\nb\n```\nc\n", code: []bool{true, true, true, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mask := markdownCode(tc.content)
			offset := 0
			for i, line := range splitLinesKeepEnds(tc.content) {
				if line == "" {
					continue
				}
				if got := mask[offset]; got != tc.code[i] {
					t.Errorf("line %d %q: code = %v, want %v", i+1, line, got, tc.code[i])
				}
				offset += len(line)
			}
		})
	}
}
