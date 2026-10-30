package config

import (
	"strings"
	"testing"
)

// resolveRebaseStrategy runs one global YAML document and one repository pair
// (trusted default-branch copy + pushed-branch copy) through the same path the
// daemon uses, and returns the strategy the rebase step would see.
func resolveRebaseStrategy(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) string {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("load global config: %v", err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("load trusted repo config: %v", err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).Rebase.Strategy
}

// unset is an empty YAML document: the loaders reject a zero-byte one.
const unsetRebaseYAML = "{}\n"

const (
	mergeStrategyYAML  = "rebase:\n  strategy: merge\n"
	rebaseStrategyYAML = "rebase:\n  strategy: rebase\n"
)

func TestRebaseStrategy_DefaultsToMerge(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	if got := resolveRebaseStrategy(t, unset, unset, unset); got != RebaseStrategyMerge {
		t.Fatalf("Rebase.Strategy = %q, want %q for an unset config", got, RebaseStrategyMerge)
	}
}

// merge is the only shape, so writing it down anywhere resolves to it.
func TestRebaseStrategy_ExplicitMergeIsAccepted(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	for _, tc := range []struct {
		name                    string
		global, trusted, pushed string
	}{
		{"global", mergeStrategyYAML, unset, unset},
		{"trusted repo", unset, mergeStrategyYAML, unset},
		{"pushed branch", unset, unset, mergeStrategyYAML},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRebaseStrategy(t, tc.global, tc.trusted, tc.pushed); got != RebaseStrategyMerge {
				t.Fatalf("Rebase.Strategy = %q, want %q", got, RebaseStrategyMerge)
			}
		})
	}
}

// The rewriting shape is retired, not ignored: a config that still asks for it
// fails closed with a message saying why, so no repository silently runs on a
// behaviour it did not ask for.
func TestRebaseStrategy_RebaseFailsClosed(t *testing.T) {
	t.Parallel()
	wantRetired := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected rebase.strategy: rebase to fail the config closed")
		}
		for _, want := range []string{"rebase.strategy", `"rebase"`, "only by merging"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q is missing %q", err, want)
			}
		}
	}
	_, err := LoadRepoFromBytes([]byte(rebaseStrategyYAML))
	wantRetired(t, err)
	_, err = LoadGlobalFromBytes([]byte(rebaseStrategyYAML))
	wantRetired(t, err)
}

// rebase.strategy stays trusted-only: a pushed branch's copy never reaches the
// resolved config, even with the commands opt-in, and an empty trusted copy
// resolves to the default rather than to anything the branch said.
func TestRebaseStrategy_TrustedOnly(t *testing.T) {
	t.Parallel()
	trusted, err := LoadRepoFromBytes([]byte("allow_repo_commands: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := LoadRepoFromBytes([]byte(mergeStrategyYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, allowRepoCommands := range []bool{false, true} {
		if got := EffectiveRepoConfig(pushed, trusted, allowRepoCommands).Rebase.Strategy; got != "" {
			t.Fatalf("allow_repo_commands=%v: effective repo rebase.strategy = %q, want the pushed copy dropped", allowRepoCommands, got)
		}
	}
}

func TestRebaseStrategy_RejectsUnknownValue(t *testing.T) {
	t.Parallel()
	if _, err := LoadRepoFromBytes([]byte("rebase:\n  strategy: merges\n")); err == nil {
		t.Fatal("expected an unknown repo rebase.strategy to fail the config closed")
	} else if !strings.Contains(err.Error(), "rebase.strategy") {
		t.Fatalf("error does not name the setting: %v", err)
	}
	if _, err := LoadGlobalFromBytes([]byte("rebase:\n  strategy: squash\n")); err == nil {
		t.Fatal("expected an unknown global rebase.strategy to fail the config closed")
	}
}

// An empty value means "not set" rather than a third strategy, so commenting
// the value out cannot silently invent behavior.
func TestRebaseStrategy_EmptyValueKeepsTheDefault(t *testing.T) {
	t.Parallel()
	if got := resolveRebaseStrategy(t, "rebase:\n  strategy: \"\"\n", unsetRebaseYAML, unsetRebaseYAML); got != RebaseStrategyMerge {
		t.Fatalf("Rebase.Strategy = %q, want %q for an empty value", got, RebaseStrategyMerge)
	}
}
