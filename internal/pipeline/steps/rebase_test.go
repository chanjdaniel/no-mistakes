package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestRebaseStep_ConflictTriesAllTargets(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch, push it to origin
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature-origin\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature origin change")
	gitCmd(t, dir, "push", "origin", "feature")

	// Diverge local feature from origin/feature (conflicting change to shared.txt)
	gitCmd(t, dir, "reset", "--soft", "HEAD~1")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature-local\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature local change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main with a non-conflicting change, push
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("main update\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main non-conflicting update")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertConflictPark(t, outcome, "origin/feature", "shared.txt")

	// origin/main merged cleanly after the origin/feature conflict, but the
	// step is all or nothing: a conflict on any target leaves the branch where
	// the step found it, so nothing half-integrated is reviewed or published.
	assertBranchUnchanged(t, sctx, dir, headSHA)
	if strings.Contains(gitCmd(t, dir, "log", "--oneline"), "main non-conflicting update") {
		t.Fatal("the clean origin/main merge survived a conflict on another target")
	}
}

func TestRebaseStep_UsesConfiguredPRBaseBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	gitCmd(t, dir, "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(dir, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main integration")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "develop", "HEAD~1")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("develop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "develop integration")
	developSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "develop")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, developSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.PR.BaseBranch = "develop"

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("unexpected approval: %s", outcome.Findings)
	}
	if got := gitCmd(t, dir, "merge-base", "HEAD", "origin/develop"); got != developSHA {
		t.Fatalf("merge-base with configured base = %s, want develop %s", got, developSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.txt")); !os.IsNotExist(err) {
		t.Fatal("rebase used forge default main instead of configured develop")
	}
}

func TestRebaseStep_ForkSyncsPushBranchBeforeDefaultBranch(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	fork := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", parent)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", fork, "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "push", fork, "feature")

	if err := os.WriteFile(filepath.Join(dir, "fork.txt"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fork update")
	forkOnlySHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", fork, "feature")

	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "local update")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("unexpected approval after clean fork rebase: %s", outcome.Findings)
	}
	if _, err := os.Stat(filepath.Join(dir, "fork.txt")); err != nil {
		t.Fatalf("expected fork-only commit to be included after rebase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.txt")); err != nil {
		t.Fatalf("expected local commit to remain after rebase: %v", err)
	}
	if mergeBase := gitCmd(t, dir, "merge-base", "HEAD", forkOnlySHA); mergeBase != forkOnlySHA {
		t.Fatalf("merge-base = %s, want fork tip %s", mergeBase, forkOnlySHA)
	}
}

// A branch strictly behind its base is fast-forwarded, and that move must
// refuse a dirty worktree too: a reset to the base would silently drop the
// uncommitted change to a file the base also changed.
func TestRebaseStep_DirtyWorktreeRefusesTheFastForward(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "merge.autoStash", "false")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// main moves on a.txt; feature stays on the base, so it is strictly behind.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("main\n"), 0o644)
	gitCmd(t, dir, "commit", "-am", "main advance")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature", baseSHA)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, baseSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("Execute error = %v, want it to refuse the dirty worktree", err)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected 0 agent calls, got %d", len(ag.calls))
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("head = %s, want %s untouched", got, baseSHA)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(content) != "dirty\n" {
		t.Fatalf("dirty a.txt = %q (err %v), want the uncommitted change kept", content, err)
	}
}

func TestRebaseStep_DirtyWorktreeFailsWithoutTouchingIt(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	// An ambient global merge.autoStash=true would stash the dirty file around
	// the merge, so pin it false in this repo regardless of the machine's git
	// config.
	gitCmd(t, dir, "config", "merge.autoStash", "false")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main so a merge is needed. It touches a different file, so git
	// itself would merge over the dirty one; the step must refuse instead,
	// because its conflict restore resets the worktree.
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("main\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main advance")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	// Dirty the working tree.
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("dirty\n"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	_, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("Execute error = %v, want it to refuse the dirty worktree", err)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected 0 agent calls, got %d", len(ag.calls))
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("head = %s, want %s untouched", got, headSHA)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(content) != "dirty\n" {
		t.Fatalf("dirty b.txt = %q (err %v), want the uncommitted change kept", content, err)
	}
}

func TestRebaseStep_LogFileNotVisibleToUser(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "init")
	sha := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch with no upstream ref (will trigger fetch warning)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, sha, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	var userLogs []string
	var fileLogs []string
	sctx.Log = func(s string) { userLogs = append(userLogs, s) }
	sctx.LogFile = func(s string) { fileLogs = append(fileLogs, s) }

	step := &RebaseStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch warnings should go to file only, not user
	for _, log := range userLogs {
		if strings.Contains(log, "could not fetch") {
			t.Errorf("fetch warning leaked to user logs: %s", log)
		}
	}
	hasFileWarning := false
	for _, log := range fileLogs {
		if strings.Contains(log, "could not fetch") {
			hasFileWarning = true
		}
	}
	if !hasFileWarning {
		t.Error("expected fetch warning in file logs")
	}
}

// A merge keeps the uncertified fixer range's commits on the branch under their
// own SHAs, so the recorded range stays exactly as it was and still binds to
// the merged head; nothing has to be remapped.
func TestRebaseStep_MergeKeepsTheUncertifiedRangeBindable(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "author.txt"), []byte("author\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "author")
	fromSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "fixer.txt"), []byte("fixer\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fixer")
	toSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("main update\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main non-conflicting update")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, toSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	if err := sctx.DB.UpsertUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch, fromSHA, toSHA, sctx.Run.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if newHead == toSHA {
		t.Fatal("the moved base was not merged")
	}

	got, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != fromSHA || got.ToSHA != toSHA {
		t.Fatalf("uncertified range = %#v, want it unchanged at %s..%s", got, fromSHA, toSHA)
	}

	bind := &pipeline.StepContext{
		Ctx:     sctx.Ctx,
		DB:      sctx.DB,
		Repo:    sctx.Repo,
		Run:     sctx.Run,
		WorkDir: dir,
	}
	pipeline.BindUncertifiedPipelineRange(bind)
	if bind.UncertifiedFromSHA != fromSHA || bind.UncertifiedToSHA != toSHA {
		t.Fatalf("bind after the merge from=%q to=%q, want from=%q to=%q", bind.UncertifiedFromSHA, bind.UncertifiedToSHA, fromSHA, toSHA)
	}
}

func TestRebaseStep_BaseFetchFailureFailsBeforeRewritingHead(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "-b", "main")

	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(dir, "cached.txt"), []byte("cached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "unverified cached base")
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = filepath.Join(t.TempDir(), "does-not-exist")
	sctx.Repo.URLsVerified = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("rebase step succeeded, want the base-branch fetch failure to fail the step")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s untouched: rebased onto an unverified cached base", got, headSHA)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("persisted head = %s, want %s untouched", run.HeadSHA, headSHA)
	}
}

// Execute fetches the base before integrating, so once the merge has moved
// HEAD nothing may fetch again: a failure then could not undo the merge that
// was already persisted.
func TestRebaseStep_BaseUnreachableAfterMergeStillCompletesTheStep(t *testing.T) {
	t.Parallel()
	upstreamParent := t.TempDir()
	upstream := filepath.Join(upstreamParent, "upstream.git")
	gitCmd(t, upstreamParent, "init", "--bare", upstream)

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "shared.txt", "base content\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFixtureFile(t, dir, "feature.txt", "feature change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	writeFixtureFile(t, dir, "main.txt", "main change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main change")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	sctx := newTestContextWithDBRecords(t, noAgent(t), dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	// The upstream becomes unreachable the moment the merge starts: any
	// base-branch fetch after it would now fail.
	log := sctx.Log
	sctx.Log = func(line string) {
		if strings.HasPrefix(line, "merging ") {
			if err := os.Rename(upstream, upstream+".gone"); err != nil {
				t.Error(err)
			}
		}
		log(line)
	}

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("rebase step failed after HEAD was merged and persisted: %v", err)
	}
	merged := gitCmd(t, dir, "rev-parse", "HEAD")
	if merged == headSHA {
		t.Fatal("HEAD was not merged")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != merged {
		t.Fatalf("persisted head = %s, want merged head %s", run.HeadSHA, merged)
	}
}
