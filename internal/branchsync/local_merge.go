package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// gateWithLocalObjects returns the local gate directory and a context whose Git
// commands there can also read the invoking worktree's objects, as read-only
// alternates. A preserved pipeline head lives only in the gate and an
// operator's own local commit (their hand merge of a moved base, say) only in
// the worktree, so no ref-free comparison of the two is possible in either
// repository alone; borrowing the worktree's object store answers it without
// fetching, pushing, or writing a ref in either repository. ok is false when
// the gate or the worktree's object directory is unavailable.
func (s *Service) gateWithLocalObjects(ctx context.Context) (gateCtx context.Context, gateDir string, ok bool) {
	gateDir = strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return ctx, "", false
	}
	if _, err := os.Stat(gateDir); err != nil {
		return ctx, "", false
	}
	commonDir, err := git.Run(ctx, s.workDir(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || commonDir == "" {
		return ctx, "", false
	}
	objects := filepath.Join(commonDir, "objects")
	if info, err := os.Stat(objects); err != nil || !info.IsDir() {
		return ctx, "", false
	}
	overlay := runenv.Overlay{Set: map[string]string{"GIT_ALTERNATE_OBJECT_DIRECTORIES": objects}}
	return git.WithEnvironment(ctx, overlay), gateDir, true
}

// releasedByLocalMerges reports a terminal, never-published run whose moved
// head adds nothing the local head lacks except merges that carry no content
// of their own: every commit reachable from the run's verified head but not
// from local is a clean merge (git.ContentFreeMergeInto) whose parents local
// already has. That is what an operator leaves behind when they abort a run
// whose Rebase step merged a moved base, then merge the same base by hand: the
// run's only commit is its own copy of the operator's merge, so there is no
// pipeline content to recover and the run's outcome released the branch, like
// a run that never moved the head.
//
// A run that also holds any other commit the local head lacks (an unpublished
// review fix, say, or a legacy rebased copy) is not released, and stays
// recoverable custody. An equal or ahead local head is not released either:
// it already reaches the preserved head, and its custody return stays the
// existing anchor-and-stamp recovery. Anything Git cannot answer is false.
func (s *Service) releasedByLocalMerges(ctx context.Context, run *db.Run, local string) bool {
	if run == nil || !terminalRunStatus(run.Status) || run.CustodyReturnedAt != nil || run.LastPushedSHA != nil ||
		run.TerminalHeadVerifiedAt == nil || run.SubmittedHeadSHA == nil || local == "" {
		return false
	}
	preserved := strings.TrimSpace(run.HeadSHA)
	if preserved == "" || preserved == strings.TrimSpace(*run.SubmittedHeadSHA) {
		return false
	}
	gateCtx, gateDir, ok := s.gateWithLocalObjects(ctx)
	if !ok || !objectExists(gateCtx, gateDir, preserved) || !objectExists(gateCtx, gateDir, local) {
		return false
	}
	pipelineOnly, err := revList(gateCtx, gateDir, "rev-list", preserved, "^"+local)
	if err != nil || len(pipelineOnly) == 0 {
		return false
	}
	for _, commit := range pipelineOnly {
		if !git.ContentFreeMergeInto(gateCtx, gateDir, commit, local) {
			return false
		}
	}
	return true
}
