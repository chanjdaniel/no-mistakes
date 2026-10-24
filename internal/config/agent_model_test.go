package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// danielsAgentModelBlock is the agent_model block of the gate's own
// ~/.no-mistakes/config.yaml, copied verbatim, so an update that would refuse
// the operator's real config fails here rather than in the daemon.
const danielsAgentModelBlock = `agent: opencode

agent_model:
  opencode:
    review: deepseek/deepseek-v4-pro
    test: deepseek/deepseek-flash
    document: deepseek/deepseek-flash
    lint: deepseek/deepseek-flash
`

func TestLoadGlobal_AgentModelLoadsTheGatesRealBlock(t *testing.T) {
	cfg := writeGlobalConfig(t, danielsAgentModelBlock)
	want := map[string]string{
		"review":   "deepseek/deepseek-v4-pro",
		"test":     "deepseek/deepseek-flash",
		"document": "deepseek/deepseek-flash",
		"lint":     "deepseek/deepseek-flash",
	}
	if len(cfg.AgentModel) != len(want) {
		t.Fatalf("AgentModel = %#v, want %#v", cfg.AgentModel, want)
	}
	for key, model := range want {
		if got := cfg.AgentModel[key]; got != model {
			t.Errorf("AgentModel[%q] = %q, want %q", key, got, model)
		}
	}
}

func TestLoadGlobal_AgentModelAcceptsEveryStepKey(t *testing.T) {
	for _, key := range AgentModelKeys() {
		t.Run(key, func(t *testing.T) {
			cfg := writeGlobalConfig(t, "agent_model:\n  opencode:\n    "+key+": \" prov/model-"+key+" \"\n")
			if got := cfg.AgentModel[key]; got != "prov/model-"+key {
				t.Fatalf("AgentModel[%q] = %q, want the trimmed value", key, got)
			}
		})
	}
}

func TestLoadGlobal_AgentModelRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "unknown key lists the valid ones",
			yaml: "agent_model:\n  opencode:\n    reveiw: deepseek/deepseek-v4-pro\n",
			want: []string{`agent_model.opencode key "reveiw"`, "valid: document, lint, review, test"},
		},
		{
			name: "push has no agent",
			yaml: "agent_model:\n  opencode:\n    push: deepseek/deepseek-flash\n",
			want: []string{`agent_model.opencode key "push"`},
		},
		{
			name: "custom gate step",
			yaml: "agent_model:\n  opencode:\n    gate.security: deepseek/deepseek-flash\n",
			want: []string{`agent_model.opencode key "gate.security"`},
		},
		{
			name: "empty value",
			yaml: "agent_model:\n  opencode:\n    review: \"\"\n",
			want: []string{"agent_model.opencode.review", "empty"},
		},
		{
			name: "null value",
			yaml: "agent_model:\n  opencode:\n    test:\n",
			want: []string{"agent_model.opencode.test", "empty"},
		},
		{
			name: "value without provider",
			yaml: "agent_model:\n  opencode:\n    document: deepseek-flash\n",
			want: []string{"agent_model.opencode.document", "provider/model"},
		},
		{
			name: "value without model",
			yaml: "agent_model:\n  opencode:\n    lint: deepseek/\n",
			want: []string{"agent_model.opencode.lint", "provider/model"},
		},
		{
			name: "native harness that fixes its model at launch",
			yaml: "agent_model:\n  claude:\n    review: claude-opus\n",
			want: []string{"agent_model.claude", "only opencode", "agent_config", "review_agents"},
		},
		{
			name: "unknown harness",
			yaml: "agent_model:\n  gemini:\n    review: g/x\n",
			want: []string{"agent_model.gemini", "only opencode"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadGlobalConfigError(t, tt.yaml)
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// The review loop is configured in agent_model or in review_agents, never in
// both: a per-step route would silently override a role's model, and with the
// *_after_round roles it would silently cancel upstream's escalation.
func TestLoadGlobal_AgentModelReviewRefusedBesideAnyReviewAgentsRole(t *testing.T) {
	for _, role := range ReviewAgentRoles {
		t.Run(role, func(t *testing.T) {
			yaml := "agent: opencode\n" +
				"agent_model:\n  opencode:\n    review: deepseek/deepseek-v4-pro\n" +
				"review_agents:\n  " + role + ": {agent: opencode, model: deepseek/deepseek-flash}\n"
			err := loadGlobalConfigError(t, yaml)
			for _, want := range []string{"agent_model.opencode.review", "review_agents." + role} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestLoadGlobal_AgentModelOtherStepsCoexistWithReviewAgents(t *testing.T) {
	cfg := writeGlobalConfig(t, "agent: opencode\n"+
		"agent_model:\n  opencode:\n    test: deepseek/deepseek-flash\n    document: deepseek/deepseek-flash\n"+
		"review_agents:\n  reviewer: {agent: opencode, model: deepseek/deepseek-v4-pro}\n")
	if cfg.AgentModel["test"] != "deepseek/deepseek-flash" || cfg.ReviewAgents["reviewer"].Model != "deepseek/deepseek-v4-pro" {
		t.Fatalf("AgentModel = %#v, ReviewAgents = %#v", cfg.AgentModel, cfg.ReviewAgents)
	}
}

// agent_model is global-only, like agent_config and review_agents: a pushed
// branch must never choose which model runs with the operator's credentials.
// The repository loader ignores the key exactly as it ignores every other key
// it does not know, so it can never reach the merged configuration.
func TestRepositoryCannotRouteAgentModel(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("agent_model:\n  opencode:\n    review: attacker/model\n    intent: attacker/model\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(writeGlobalConfig(t, danielsAgentModelBlock), repo)
	if model, key := cfg.AgentModelFor(types.StepReview, "review"); model != "deepseek/deepseek-v4-pro" || key != "review" {
		t.Fatalf("review routed to %q (key %q); the repository changed the operator's route", model, key)
	}
	if model, _ := Merge(DefaultGlobalConfig(), repo).AgentModelFor(types.StepReview, "review"); model != "" {
		t.Fatalf("repository selected a route: %q", model)
	}
}

func TestAgentModelForResolvesTheRoutingTable(t *testing.T) {
	cfg := Merge(writeGlobalConfig(t, `agent: opencode
agent_model:
  opencode:
    review: p/review
    test: p/test
    document: p/document
    lint: p/lint
`), &RepoConfig{})
	tests := []struct {
		step      types.StepName
		purpose   string
		wantModel string
		wantKey   string
	}{
		{types.StepIntent, "", "", ""},
		{types.StepRebase, "", "", ""},
		{types.StepReview, "review", "p/review", "review"},
		{types.StepReview, "review-fix", "p/review", "review"},
		{types.StepTest, "", "p/test", "test"},
		{types.StepTest, "test-fix", "p/test", "test"},
		{types.StepDocument, "document", "p/document", "document"},
		{types.StepDocument, "housekeeping", "p/document", "document"},
		{types.StepLint, "lint", "p/lint", "lint"},
		{types.StepLint, "lint-fix", "p/lint", "lint"},
		{types.StepPush, "", "", ""},
		{types.StepPR, "", "", ""},
		{types.StepCI, "", "", ""},
		{types.StepCI, "ci-fix", "", ""},
		{types.StepName("gate.security"), "", "", ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.step)+"/"+tt.purpose, func(t *testing.T) {
			model, key := cfg.AgentModelFor(tt.step, tt.purpose)
			if model != tt.wantModel || key != tt.wantKey {
				t.Fatalf("AgentModelFor(%q, %q) = (%q, %q), want (%q, %q)", tt.step, tt.purpose, model, key, tt.wantModel, tt.wantKey)
			}
		})
	}
}

func TestAgentModelForWithoutRoutingReturnsNothing(t *testing.T) {
	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if cfg.AgentModelConfigured() {
		t.Fatal("AgentModelConfigured() = true with no agent_model block")
	}
	if model, key := cfg.AgentModelFor(types.StepReview, "review"); model != "" || key != "" {
		t.Fatalf("AgentModelFor = (%q, %q), want nothing", model, key)
	}
	if !Merge(writeGlobalConfig(t, danielsAgentModelBlock), &RepoConfig{}).AgentModelConfigured() {
		t.Fatal("AgentModelConfigured() = false with routes configured")
	}
}
