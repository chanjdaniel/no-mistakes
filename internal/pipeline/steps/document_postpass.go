package steps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// verifiedDateLayout is the one date shape the guard reads; any other value
// is not a date it judges.
const verifiedDateLayout = "2006-01-02"

// emDash is U+2014, which the step never leaves on a Markdown line it wrote.
const emDash = "\u2014"

// markdownEdit is one Markdown file the document step changed: its content at
// the step's starting head (absent when the file did not exist there) and in
// the worktree now.
type markdownEdit struct {
	path   string
	before []byte
	absent bool
	after  []byte
}

// documentPostPass is the deterministic pass the document step runs on its
// own edits after the agent returns and before they are committed, so every
// pass that commits document agent edits (the document pass, the combined
// housekeeping pass, and a manual fix round) goes through it. output is the
// agent's structured output, trusted only when it validates as the step's
// findings.
func documentPostPass(sctx *pipeline.StepContext, startHead string, output []byte) error {
	edits, err := changedMarkdownFiles(sctx, startHead)
	if err != nil {
		return fmt.Errorf("document post-pass: %w", err)
	}
	if len(edits) == 0 {
		return nil
	}
	readSet := documentReadSet(output, sctx.WorkDir)
	today := time.Now().Format(verifiedDateLayout)
	for _, edit := range edits {
		written, err := linesWrittenSince(sctx, startHead, edit)
		if err != nil {
			return fmt.Errorf("document post-pass: %w", err)
		}
		original := edit.after
		edit.after = rewriteEmDashOnWrittenLines(sctx, edit, written)
		content := guardVerifiedDate(sctx, edit, readSet, today)
		if bytes.Equal(content, original) {
			continue
		}
		if err := os.WriteFile(filepath.Join(sctx.WorkDir, filepath.FromSlash(edit.path)), content, 0o644); err != nil {
			return fmt.Errorf("document post-pass: write %s: %w", edit.path, err)
		}
	}
	return nil
}

// changedMarkdownFiles lists the .md and .mdx files whose worktree content
// differs from startHead, tracked or new, with both versions. A file the step
// deleted has nothing left to guard.
func changedMarkdownFiles(sctx *pipeline.StepContext, startHead string) ([]markdownEdit, error) {
	tracked, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "--no-renames", "-z", startHead)
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	untracked, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list new files: %w", err)
	}
	var edits []markdownEdit
	seen := map[string]bool{}
	for _, name := range strings.Split(string(tracked)+string(untracked), "\x00") {
		if name == "" || seen[name] || !isMarkdownPath(name) {
			continue
		}
		seen[name] = true
		after, err := os.ReadFile(filepath.Join(sctx.WorkDir, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		edit := markdownEdit{path: name, after: after}
		listed, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "ls-tree", "-z", "--name-only", startHead, "--", name)
		if err != nil {
			return nil, fmt.Errorf("look up %s at %s: %w", name, startHead, err)
		}
		if len(listed) == 0 {
			edit.absent = true
		} else if edit.before, err = git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", startHead+":"+name); err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, startHead, err)
		}
		edits = append(edits, edit)
	}
	return edits, nil
}

func isMarkdownPath(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".md" || ext == ".mdx"
}

// documentReadSet is the set of docs the agent reported reading in full
// against the code in this pass. An output that does not validate as the
// step's findings, or whose read_against_code is not a list of strings, gives
// an empty set, so the guard fails toward restoring.
func documentReadSet(output []byte, workDir string) map[string]bool {
	readSet := map[string]bool{}
	var findings Findings
	if output == nil || unmarshalRequiredFindings(output, &findings, true) != nil {
		return readSet
	}
	var payload struct {
		ReadAgainstCode []string `json:"read_against_code"`
	}
	if json.Unmarshal(output, &payload) != nil {
		return readSet
	}
	for _, reported := range payload.ReadAgainstCode {
		if name := repoRelativePath(reported, workDir); name != "" {
			readSet[name] = true
		}
	}
	return readSet
}

// repoRelativePath normalizes an agent-reported path to the slash-separated,
// repository-relative form git lists, or "" when it is outside the worktree.
func repoRelativePath(reported, workDir string) string {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return ""
	}
	if filepath.IsAbs(reported) {
		rel, err := filepath.Rel(workDir, reported)
		if err != nil {
			return ""
		}
		reported = rel
	}
	cleaned := path.Clean(filepath.ToSlash(reported))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

// linesWrittenSince marks each line of the edit's worktree content that the
// step added or changed relative to startHead: every line of a new file, and
// otherwise the added lines of git's diff, read from the hunk bodies so a
// configured inter-hunk context cannot widen them.
func linesWrittenSince(sctx *pipeline.StepContext, startHead string, edit markdownEdit) ([]bool, error) {
	written := make([]bool, len(splitLinesKeepEnds(string(edit.after))))
	if edit.absent {
		for i := range written {
			written[i] = true
		}
		return written, nil
	}
	diff, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "diff", "--no-color", "--no-ext-diff",
		"--no-textconv", "--text", "--no-renames", "--unified=0", "--inter-hunk-context=0", startHead, "--", edit.path)
	if err != nil {
		return nil, fmt.Errorf("diff %s against %s: %w", edit.path, startHead, err)
	}
	next, inHunk := 0, false
	for _, line := range strings.Split(string(diff), "\n") {
		if strings.HasPrefix(line, "@@ ") {
			start, ok := hunkNewStart(line)
			if !ok {
				return nil, fmt.Errorf("diff %s against %s: unreadable hunk header %q", edit.path, startHead, line)
			}
			next, inHunk = start, true
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			if next >= 1 && next <= len(written) {
				written[next-1] = true
			}
			next++
		case ' ':
			next++
		}
	}
	return written, nil
}

// hunkNewStart reads the first new-side line number of a unified diff hunk
// header ("@@ -a,b +c,d @@").
func hunkNewStart(header string) (int, bool) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
		return 0, false
	}
	start, _, _ := strings.Cut(fields[2][1:], ",")
	n, err := strconv.Atoi(start)
	if err != nil {
		return 0, false
	}
	// A hunk that adds nothing names the line before it; it has no '+' or
	// context lines, so its start is never used.
	return n, true
}

// rewriteEmDashOnWrittenLines returns the edit's content with every em dash
// on a line the step wrote replaced by a plain dash; lines it did not write,
// such as a quoted rule, are left as they were.
func rewriteEmDashOnWrittenLines(sctx *pipeline.StepContext, edit markdownEdit, written []bool) []byte {
	lines := splitLinesKeepEnds(string(edit.after))
	total := 0
	for i, line := range lines {
		if i >= len(written) || !written[i] {
			continue
		}
		rewritten, n := rewriteEmDash(line)
		lines[i] = rewritten
		total += n
	}
	if total == 0 {
		return edit.after
	}
	sctx.Log(fmt.Sprintf("rewrote %d em dash(es) in %s", total, edit.path))
	return []byte(strings.Join(lines, ""))
}

// rewriteEmDash replaces each em dash in line with "-" and counts them. That
// one replacement turns " — " into " - " and any other em dash into "-".
func rewriteEmDash(line string) (string, int) {
	n := strings.Count(line, emDash)
	if n == 0 {
		return line, 0
	}
	return strings.ReplaceAll(line, emDash, "-"), n
}

// guardVerifiedDate returns the edit's content with its frontmatter
// `verified` date restored to the starting value (or removed when it was
// absent) when the step moved or introduced a date it may not set: on a doc
// outside the read set, earlier than the starting date, or later than today.
// The read set is only the docs the agent reported reading in full: editing a
// doc for another reason does not earn its date. A file without a dated
// top-level key is untouched.
func guardVerifiedDate(sctx *pipeline.StepContext, edit markdownEdit, readSet map[string]bool, today string) []byte {
	afterLines := splitLinesKeepEnds(string(edit.after))
	afterIdx, afterValue := frontmatterVerified(afterLines)
	if afterIdx < 0 || !isVerifiedDate(afterValue) {
		return edit.after
	}
	beforeLines := splitLinesKeepEnds(string(edit.before))
	beforeIdx, beforeValue := frontmatterVerified(beforeLines)
	if beforeIdx >= 0 && beforeValue == afterValue {
		return edit.after
	}

	reason := ""
	switch {
	case !readSet[edit.path]:
		reason = "not reported in read_against_code"
	case beforeIdx >= 0 && isVerifiedDate(beforeValue) && afterValue < beforeValue:
		reason = "earlier than the date it replaced"
	case afterValue > today:
		reason = "later than the run's day " + today
	default:
		return edit.after
	}

	restored := append([]string(nil), afterLines...)
	oldValue := "(absent)"
	if beforeIdx >= 0 {
		oldValue = beforeValue
		restored[afterIdx] = strings.TrimRight(beforeLines[beforeIdx], "\r\n") + lineEnding(afterLines[afterIdx])
	} else {
		restored = append(restored[:afterIdx], restored[afterIdx+1:]...)
	}
	sctx.Log(fmt.Sprintf("restored verified date in %s: %s -> %s (%s)", edit.path, afterValue, oldValue, reason))
	return []byte(strings.Join(restored, ""))
}

// frontmatterVerified finds the top-level `verified:` key in a leading YAML
// frontmatter block (a first line of "---" up to the next "---" or "...")
// and returns its line index and unquoted value, or -1 when there is none.
func frontmatterVerified(lines []string) (int, string) {
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\r\n") != "---" {
		return -1, ""
	}
	idx, value := -1, ""
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		if trimmed := strings.TrimRight(line, " \t"); trimmed == "---" || trimmed == "..." {
			return idx, value
		}
		rest, ok := strings.CutPrefix(line, "verified:")
		if !ok || idx >= 0 {
			continue
		}
		if comment := strings.Index(rest, " #"); comment >= 0 {
			rest = rest[:comment]
		}
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') && rest[len(rest)-1] == rest[0] {
			rest = rest[1 : len(rest)-1]
		}
		idx, value = i, rest
	}
	// An unclosed block is not frontmatter.
	return -1, ""
}

func isVerifiedDate(value string) bool {
	_, err := time.Parse(verifiedDateLayout, value)
	return err == nil
}

func splitLinesKeepEnds(content string) []string {
	if content == "" {
		return nil
	}
	return strings.SplitAfter(content, "\n")
}

func lineEnding(line string) string {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		return "\r\n"
	case strings.HasSuffix(line, "\n"):
		return "\n"
	}
	return ""
}
