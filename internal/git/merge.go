package git

import (
	"context"
	"strings"
)

// ContentFreeMergeInto reports whether commit is a two-parent merge that
// carries no content of its own relative to head: its tree is exactly the
// conflict-free `git merge-tree --write-tree` result of its parents, and each
// parent is reachable from head.
//
// Such a merge is what the gate's merge-only integration writes for a moved
// base that merged cleanly. When an operator merges the same base by hand, the
// two merge commits differ only in their SHA, message and dates, so a proof
// that asks "is any content of this private commit missing from head" must
// not count it. A merge whose tree differs from the clean merge (a conflict
// resolution or an edit made inside the merge), a merge whose parents do not
// merge cleanly, an octopus merge, or a merge with a parent head lacks still
// carries content of its own and is reported false, as is anything Git cannot
// answer.
func ContentFreeMergeInto(ctx context.Context, dir, commit, head string) bool {
	parentLine, err := Run(ctx, dir, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return false
	}
	fields := strings.Fields(parentLine)
	if len(fields) != 3 {
		return false
	}
	parents := fields[1:]
	for _, parent := range parents {
		if _, err := Run(ctx, dir, "merge-base", "--is-ancestor", parent, head); err != nil {
			return false
		}
	}
	mergedTree, err := Run(ctx, dir, "merge-tree", "--write-tree", parents[0], parents[1])
	if err != nil {
		return false
	}
	commitTree, err := Run(ctx, dir, "rev-parse", "--verify", commit+"^{tree}")
	return err == nil && commitTree == mergedTree
}
