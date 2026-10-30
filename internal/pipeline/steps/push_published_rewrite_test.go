package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestPushStep_RefusesAReviewedRebaseOfTheRunsOwnPublishedHead keeps the
// reproduction that once justified Decision 41-A's last-pushed-head exception:
// a run published a head carrying a pipeline commit, the base took the same
// migration number, and the old CI merge-conflict repair rebased onto it and
// resolved the conflict, rewriting every patch of the published range. Review
// approving that rewrite no longer lets it out: it drops both the submitted
// head and the run's own published head, so the publication backstop refuses
// it before the mirror, the remote, or the recorded push can change. The same
// in-memory run is reused across both publications, as the executor does.
func TestPushStep_RefusesAReviewedRebaseOfTheRunsOwnPublishedHead(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, _ := setupGitRepo(t)
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-q", "-m", message)
		return gitCmd(t, dir, "rev-parse", "HEAD")
	}
	const migration = "CREATE TABLE upload_intents (id text primary key);\n"

	gitCmd(t, dir, "checkout", "-q", "-B", "feature", baseSHA)
	write("migrations/0279_upload_intents.sql", migration)
	write("migrations/journal.txt", "0279 upload_intents\n")
	submitted := commit("feat: upload intents")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "-q", "origin", "main")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	gateDir := setupGateMirror(t, sctx)
	gitCmd(t, gateDir, "fetch", dir, submitted+":refs/heads/feature")

	// A pipeline commit lands before the first publication, so the run's
	// published head differs from its submitted head.
	write("src/finalize.ts", "export const finalize = () => 1;\n")
	published := commit("no-mistakes(lint): extract helpers")
	sctx.Run.HeadSHA = published
	recordReviewApproval(t, sctx, published)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("first publication: %v", err)
	}

	// The base branch takes the same migration number meanwhile.
	gitCmd(t, dir, "checkout", "-q", "main")
	write("migrations/0279_fiscal_profile.sql", "CREATE TABLE fiscal (id int);\n")
	write("migrations/journal.txt", "0279 fiscal_profile\n")
	commit("feat: fiscal profile")
	gitCmd(t, dir, "push", "-q", "origin", "main")

	// The rewrite the old repair produced: the published range replayed onto
	// the moved base with the migration renumbered.
	gitCmd(t, dir, "checkout", "-q", "-B", "feature", "main")
	write("migrations/0280_upload_intents.sql", migration)
	write("migrations/journal.txt", "0279 fiscal_profile\n0280 upload_intents\n")
	commit("feat: upload intents")
	write("src/finalize.ts", "export const finalize = () => 1;\n")
	rebased := commit("no-mistakes(lint): extract helpers")
	if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, rebased); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = rebased
	recordReviewApproval(t, sctx, rebased)

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "does not contain the run's submitted head "+shortObjectID(submitted)) {
		t.Fatalf("a reviewed rebase of the run's published head was not refused by the backstop: %v", err)
	}
	for _, repo := range []string{upstream, gateDir} {
		if got := gitCmd(t, repo, "rev-parse", "refs/heads/feature"); got != published {
			t.Fatalf("%s = %s, want the published head %s", repo, got, published)
		}
	}
	if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refused publication archived a head: %s", tags)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != published {
		t.Fatalf("refused publication changed the recorded push: %+v err=%v", run, err)
	}
}
