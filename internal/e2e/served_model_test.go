//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// fakeOpencodeDefault is what the fake opencode server serves a message that
// names no model, as opencode serves its own default (cmd/fakeagent's
// fakeOpencodeDefaultModel).
const fakeOpencodeDefault = "openai/gpt-5.4"

// assertServedModels checks that every agent_invocations row of the run is an
// opencode row recording the model the fake opencode served it - routeOf's
// provider/model, or the fake's default when routeOf returns "" - with its
// provider and its reasoning tokens; that `no-mistakes stats --run` shows each
// served model in place of unknown; and that `stats --agents` shows reasoning
// tokens for every purpose.
func assertServedModels(t *testing.T, h *Harness, runID string, routeOf func(db.AgentInvocation) string) {
	t.Helper()
	rows := runAgentInvocations(t, h, runID)
	if len(rows) == 0 {
		t.Fatal("run recorded no agent invocations")
	}
	served := map[string]bool{}
	for _, row := range rows {
		want := routeOf(row)
		if want == "" {
			want = fakeOpencodeDefault
		}
		provider, model, ok := agentcfg.SplitProviderModel(want)
		if !ok {
			t.Fatalf("route %q is not provider/model", want)
		}
		label := row.StepName + "/" + row.Purpose
		if row.Agent != "opencode" {
			t.Errorf("%s row agent = %q, want opencode", label, row.Agent)
		}
		if row.Model != model {
			t.Errorf("%s row served model = %q, want %q", label, row.Model, model)
		}
		if row.ModelProvider == nil || *row.ModelProvider != provider {
			t.Errorf("%s row model provider = %v, want %q", label, deref(row.ModelProvider), provider)
		}
		if row.ReasoningTokens == nil {
			t.Errorf("%s row recorded no reasoning tokens", label)
		}
		served[model] = true
		t.Logf("%-20s served %s/%s, reasoning %v", label, deref(row.ModelProvider), row.Model, derefInt(row.ReasoningTokens))
	}

	out, err := h.Run("stats", "--run", runID)
	if err != nil {
		t.Fatalf("stats --run %s: %v\n%s", runID, err, out)
	}
	table := strings.SplitN(out, "\n\n", 3)
	if len(table) < 2 {
		t.Fatalf("stats --run printed no invocation table:\n%s", out)
	}
	// The table is tab-aligned, so a row's MODEL cell starts at the header's
	// first MODEL column (the second MODEL is model time).
	lines := strings.Split(strings.TrimSpace(table[1]), "\n")
	col := strings.Index(lines[0], "MODEL")
	if col < 0 {
		t.Fatalf("stats --run table has no MODEL column:\n%s", table[1])
	}
	if len(lines)-1 != len(rows) {
		t.Errorf("stats --run shows %d invocations, want %d:\n%s", len(lines)-1, len(rows), table[1])
	}
	for _, line := range lines[1:] {
		cell := ""
		if len(line) > col {
			cell, _, _ = strings.Cut(line[col:], " ")
		}
		if !served[cell] {
			t.Errorf("stats --run MODEL cell = %q, want a served model: %q", cell, line)
		}
	}

	// stats --agents' first table ends in REASON TOK, one row per purpose.
	out, err = h.Run("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, out)
	}
	aggregates, _, _ := strings.Cut(strings.TrimSpace(out), "\n\n")
	lines = strings.Split(aggregates, "\n")
	if !strings.HasSuffix(lines[0], "REASON TOK") {
		t.Fatalf("stats --agents first table does not end in REASON TOK:\n%s", out)
	}
	for _, line := range lines[1:] {
		if fields := strings.Fields(line); fields[len(fields)-1] == "-" {
			t.Errorf("stats --agents shows no reasoning tokens: %q", line)
		}
	}
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
