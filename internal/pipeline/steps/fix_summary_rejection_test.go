package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

const fixSummaryPreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","action":"auto-fix","description":"nil dereference in helper","file":"a.txt","line":1}],"summary":"1 issue"}`

// TestReviewStep_RejectedFixSummaryCommitsUnderTheFallback: a fix turn whose
// final summary the adapter rejects did its work all the same, so the round
// commits it under the step's fallback summary and the rereview judges it. The
// fix turn is never retried.
func TestReviewStep_RejectedFixSummaryCommitsUnderTheFallback(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	const rejection = `codex output parse: JSON output summary must be string`
	ag := &mockAgent{
		name: "codex",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if opts.Purpose == "review-fix" {
				if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return nil, rejectedStructuredOutputError{message: rejection}
			}
			return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = fixSummaryPreviousFindings
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a rejected fix summary must not fail the round: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the rereview to complete the round", outcome)
	}
	var purposes []string
	for _, call := range ag.calls {
		purposes = append(purposes, call.Purpose)
	}
	if !slices.Equal(purposes, []string{"review-fix", "review"}) {
		t.Fatalf("agent calls = %q, want one fix turn and its rereview", purposes)
	}
	if subject := strings.TrimSpace(gitCmd(t, dir, "log", "-1", "--format=%s")); subject != "no-mistakes(review): address review findings" {
		t.Fatalf("fix commit subject = %q, want the review step's fallback summary", subject)
	}
	if files := strings.TrimSpace(gitCmd(t, dir, "show", "--name-only", "--format=", "HEAD")); files != "feature.txt" {
		t.Fatalf("fix commit files = %q, want the fixer's edit", files)
	}
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, "fallback summary") && strings.Contains(line, rejection)
	}) {
		t.Fatalf("logs = %q, want one line naming the rejection and the fallback", logs)
	}
}

// TestReviewStep_FixTurnFailuresThatAreNotARejectionStillFail pins what the
// fallback does not cover: Claude's error_max_structured_output_retries is
// Claude's own exhausted retry layer and not a rejection, and a rejection from
// a fix turn its deadline cut short is a timeout, not a formatting slip. Both
// still fail the step with nothing committed, and neither is retried.
func TestReviewStep_FixTurnFailuresThatAreNotARejectionStillFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		timeout   time.Duration
		run       func(ctx context.Context) error
		wantError string
	}{
		{
			name: "claude error_max_structured_output_retries",
			run: func(context.Context) error {
				return errors.New("claude error: subtype=error_max_structured_output_retries")
			},
			wantError: "subtype=error_max_structured_output_retries",
		},
		{
			name:    "a rejection from a turn its deadline cut short",
			timeout: 20 * time.Millisecond,
			run: func(ctx context.Context) error {
				<-ctx.Done()
				return rejectedStructuredOutputError{message: `codex output parse: JSON output summary must be string`}
			},
			wantError: "reached its absolute wall-clock limit after 20ms",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			ag := &mockAgent{
				name: "claude",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if opts.Purpose != "review-fix" {
						t.Errorf("unexpected %s turn after a failed fix turn", opts.Purpose)
						return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
					}
					if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					return nil, tc.run(ctx)
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Fixing = true
			sctx.PreviousFindings = fixSummaryPreviousFindings
			if tc.timeout > 0 {
				sctx.Config.ReviewAgentTimeout = tc.timeout
			}

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err == nil || outcome != nil {
				t.Fatalf("Execute() = %+v, %v; want the fix turn's failure returned", outcome, err)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %q, want %q", err, tc.wantError)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want the one fix turn, never retried", len(ag.calls))
			}
			if head := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD")); head != headSHA {
				t.Fatalf("HEAD = %s, want %s: a failed fix turn commits nothing", head, headSHA)
			}
		})
	}
}
