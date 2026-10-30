package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_MergeConflictDetected_ReturnsNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// All CI checks pass, but PR has merge conflicts
	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	// The timeout is an idle production budget, not the mechanism under test.
	// Leave enough room for race-enabled, process-saturated CI to start the fake
	// gh subprocesses before the mergeability response is observed.
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when merge conflict detected")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	foundConflict := false
	for _, f := range findings.Items {
		if strings.Contains(f.Description, "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict finding, got: %+v", findings.Items)
	}
}

// The conflict repair merges the base the forge reports for the open PR, not
// the one the configuration names now.
func TestCIStep_AutoFixUsesExistingPRBaseAfterConfigChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "develop", "main")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("develop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "develop")
	developTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "develop")
	gitCmd(t, dir, "checkout", "feature")

	sctx := newTestContext(t, noAgent(t), dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.PR.BaseBranch = "main"
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42", BaseBranch: "develop"}

	sctx.Env = fakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "CONFLICTING")
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(skip)
	}
	if _, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor(nil, true)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD")); len(got) != 3 || got[1] != headSHA || got[2] != developTip {
		t.Fatalf("HEAD = %v, want a merge of the PR's forge base: [%s %s]", got, headSHA, developTip)
	}
}
