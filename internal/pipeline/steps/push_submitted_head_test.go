package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// refuseEveryPush installs a pre-receive hook on a bare repository that records
// any push attempt and rejects it, so a test can prove publication never even
// tried to push (a refused push leaves the remote unchanged too, which alone
// cannot tell "refused before pushing" from "pushed and was rejected").
func refuseEveryPush(t *testing.T, bare string) (attempted func() bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "push-attempted")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
}

// attestedPRFixture gives a Push step context an existing GitHub PR whose body
// carries a pipeline attestation for priorHead, and returns the body file and
// the fake gh call log, so a test can prove whether an attestation was written.
func attestedPRFixture(t *testing.T, sctx *pipeline.StepContext, priorHead string) (bodyFile, logFile string) {
	t.Helper()
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PRURL = &prURL
	bodyFile = filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, priorHead)), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile = filepath.Join(t.TempDir(), "gh.log")
	sctx.Env = append(fakeCIGH(t, "OPEN", `[]`),
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: existing pr",
		"FAKE_CLI_LOG="+logFile,
	)
	return bodyFile, logFile
}

func assertNoAttestationWrite(t *testing.T, bodyFile, logFile, priorHead string) {
	t.Helper()
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != compliantPipelineBody(t, priorHead) {
		t.Fatal("the PR body changed: an attestation was written for a head that was never published")
	}
	if logData, err := os.ReadFile(logFile); err == nil && strings.Contains(string(logData), " --body ") {
		t.Fatalf("an attestation write reached the forge:\n%s", logData)
	}
}

// TestPushStep_RefusesAHeadThatDropsTheSubmittedHead is the publication
// backstop: a head that does not contain the run's submitted head is a
// history the pipeline rewrote (a rebase, an agent's reset, a dropped commit),
// and it never reaches the remote, the PR attestation, or the gate mirror -
// even when Review approved it and the lease anchor would let it through.
func TestPushStep_RefusesAHeadThatDropsTheSubmittedHead(t *testing.T) {
	for _, variant := range []string{"rebased_onto_a_moved_base", "reset_behind_the_submission"} {
		t.Run(variant, func(t *testing.T) {
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			dir, baseSHA, submitted := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "origin", "main")
			gitCmd(t, dir, "push", "origin", "feature")

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
			gateDir := setupGateMirror(t, sctx)
			gitCmd(t, gateDir, "fetch", dir, "refs/heads/feature:refs/heads/feature")
			bodyFile, logFile := attestedPRFixture(t, sctx, submitted)

			head := baseSHA
			if variant == "rebased_onto_a_moved_base" {
				// The rewrite carries the submission's change on a moved base, so
				// only ancestry, not content, tells it apart from the submission.
				gitCmd(t, dir, "reset", "--hard", baseSHA)
				if err := os.WriteFile(filepath.Join(dir, "advanced.txt"), []byte("base advanced\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "base advanced")
				gitCmd(t, dir, "cherry-pick", submitted)
				head = gitCmd(t, dir, "rev-parse", "HEAD")
			} else {
				gitCmd(t, dir, "reset", "--hard", baseSHA)
			}
			sctx.Run.HeadSHA = head
			recordReviewApproval(t, sctx, head)
			pushAttempted := refuseEveryPush(t, upstream)

			_, err := (&PushStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "does not contain the run's submitted head "+shortObjectID(submitted)) {
				t.Fatalf("Execute() error = %v, want the submitted-head refusal", err)
			}
			if pushAttempted() {
				t.Fatal("a push was attempted for a head that drops the submitted head")
			}
			assertNoAttestationWrite(t, bodyFile, logFile, submitted)
			if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != submitted {
				t.Fatalf("remote = %s, want the submission %s", got, submitted)
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != submitted {
				t.Fatalf("gate mirror = %s, want the submission %s", got, submitted)
			}
			if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
				t.Fatalf("refused publication archived the gate branch: %s", tags)
			}
			if run, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || run.LastPushedSHA != nil {
				t.Fatalf("refused publication was recorded: %+v err=%v", run, err)
			}
		})
	}
}

// TestPushStep_MissingSubmittedHeadFailsClosed: without a submitted head the
// backstop cannot prove the head keeps the operator's submission, so the push
// is refused rather than published unchecked.
func TestPushStep_MissingSubmittedHeadFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		submitted *string
		want      string
	}{
		{name: "absent", submitted: nil, want: "run has no recorded submitted head"},
		{name: "empty", submitted: ptrTo("  "), want: "run has no recorded submitted head"},
		{name: "malformed", submitted: ptrTo("abc123"), want: "is malformed"},
		{name: "unresolvable", submitted: ptrTo(strings.Repeat("e", 40)), want: "is unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			dir, baseSHA, submitted := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "origin", "main")
			gitCmd(t, dir, "push", "origin", "feature")

			// An ordinary pipeline commit on top of the submission, which would
			// publish as a fast-forward if the submission could be verified.
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "pipeline fix")
			head := gitCmd(t, dir, "rev-parse", "HEAD")

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
			setupGateMirror(t, sctx)
			bodyFile, logFile := attestedPRFixture(t, sctx, submitted)
			sctx.Run.HeadSHA = head
			sctx.Run.SubmittedHeadSHA = tc.submitted
			recordReviewApproval(t, sctx, head)
			pushAttempted := refuseEveryPush(t, upstream)

			_, err := (&PushStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "refusing to push") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want a fail-closed refusal containing %q", err, tc.want)
			}
			if pushAttempted() {
				t.Fatal("a push was attempted without a verifiable submitted head")
			}
			assertNoAttestationWrite(t, bodyFile, logFile, submitted)
		})
	}
}

// TestPushStep_RefusesAHeadThatDropsThisRunsPublishedHead: once the run has
// published L (the submission plus a pipeline fix), an agent's reset back to
// the submission still contains the submitted head but drops L's fix, and the
// lease anchored on L would let it force-push. The backstop refuses it too.
func TestPushStep_RefusesAHeadThatDropsThisRunsPublishedHead(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, submitted := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
	gateDir := setupGateMirror(t, sctx)
	gitCmd(t, gateDir, "fetch", dir, "refs/heads/feature:refs/heads/feature")
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "pipeline fix")
	published := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx.Run.HeadSHA = published
	recordReviewApproval(t, sctx, published)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("first publication: %v", err)
	}

	gitCmd(t, dir, "reset", "--hard", submitted)
	sctx.Run.HeadSHA = submitted
	recordReviewApproval(t, sctx, submitted)
	bodyFile, logFile := attestedPRFixture(t, sctx, published)
	pushAttempted := refuseEveryPush(t, upstream)

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "does not contain the run's last pushed head "+shortObjectID(published)) {
		t.Fatalf("Execute() error = %v, want the last-pushed-head refusal", err)
	}
	if pushAttempted() {
		t.Fatal("a push was attempted for a head that drops this run's published fix")
	}
	assertNoAttestationWrite(t, bodyFile, logFile, published)
	for _, repo := range []string{upstream, gateDir} {
		if got := gitCmd(t, repo, "rev-parse", "refs/heads/feature"); got != published {
			t.Fatalf("%s = %s, want the published head %s", repo, got, published)
		}
	}
	if run, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != published {
		t.Fatalf("refused publication changed the recorded push: %+v err=%v", run, err)
	}
}

// TestCIStep_RepairPublicationRefusesAHeadThatDropsTheSubmittedHead covers the
// CI step's half of publishRunHead. The run carries a legacy state an older
// binary left: its Rebase step rewrote the submission and Review approved the
// rewrite, so a CI repair on top of it passes ciRepairContinuityGap. The
// backstop is the only thing left between that history and the remote.
func TestCIStep_RepairPublicationRefusesAHeadThatDropsTheSubmittedHead(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	submitted := f.headSHA

	gitCmd(t, f.dir, "checkout", "-q", "-B", "feature", "main")
	if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("feature, rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-q", "-m", "feature (rewritten)")
	rewritten := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "-q", "--force", "origin", "feature")
	if err := f.sctx.DB.UpdateRunReviewApprovedHeadSHA(f.sctx.Run.ID, rewritten); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.UpdateRunPushBinding(f.sctx.Run.ID, db.PushBinding{
		HeadSHA: rewritten, TargetKind: "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(f.upstream), Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.HeadSHA = rewritten
	f.sctx.Run.ReviewApprovedHeadSHA = &rewritten

	bodyFile, logFile := attestedPRFixture(t, f.sctx, rewritten)
	f.sctx.Ctx = context.Background()
	pushAttempted := refuseEveryPush(t, f.upstream)
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err == nil || !strings.Contains(err.Error(), "does not contain the run's submitted head "+shortObjectID(submitted)) {
		t.Fatalf("commitRepair error = %v, want the submitted-head refusal\nlog:\n%s", err, f.log())
	}
	if repair.HeadAdvanced {
		t.Fatalf("repair = %+v, a refused publication must not advance the run", repair)
	}
	if pushAttempted() {
		t.Fatal("the CI repair pushed a head that drops the submitted head")
	}
	assertNoAttestationWrite(t, bodyFile, logFile, rewritten)
	if got := f.remoteHead(t); got != rewritten {
		t.Fatalf("remote = %s, want it untouched at %s", got, rewritten)
	}
}

func ptrTo(s string) *string { return &s }
