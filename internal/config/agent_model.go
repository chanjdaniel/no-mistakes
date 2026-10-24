package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// agentModelHarness is the one harness agent_model may route. Only the
// opencode adapter takes a model per message; every other native harness fixes
// its model in argv when it starts, which a per-step value cannot change.
const agentModelHarness = types.AgentOpenCode

// agentModelKeys is every agent_model key, and the one place to extend
// routing. A step key routes every agent invocation of that step. A fix-round
// key (<step>-fix) routes only that step's fix-round invocations; a fix round
// without its own key falls back to its step's key (see AgentModelFor).
//
// The step keys are every built-in step that invokes an agent (push never
// does; custom gate steps are out of scope). The fix-round keys are the fix
// rounds that are their own agent invocation, with a purpose of <step>-fix:
// review, test and lint (executeFixMode) and the CI repair. Rebase's conflict
// resolution runs only in its fix round but is that step's only invocation, so
// its step key already routes it; document has no fix round of its own.
var agentModelKeys = []string{
	string(types.StepIntent),
	string(types.StepRebase),
	string(types.StepReview),
	string(types.StepTest),
	string(types.StepDocument),
	string(types.StepLint),
	string(types.StepPR),
	string(types.StepCI),
	string(types.StepReview) + "-fix",
	string(types.StepTest) + "-fix",
	string(types.StepLint) + "-fix",
	string(types.StepCI) + "-fix",
}

// AgentModelKeys returns every valid agent_model key, sorted.
func AgentModelKeys() []string {
	keys := slices.Clone(agentModelKeys)
	slices.Sort(keys)
	return keys
}

// parseAgentModel validates the on-disk agent_model block and returns the
// opencode routes, key -> provider/model. Every refusal names the key, so a
// typo or a malformed model fails at load instead of silently running a step
// on the wrong model or reaching opencode as a 400.
func parseAgentModel(raw map[string]map[string]string) (map[string]string, error) {
	var routes map[string]string
	for harness, entries := range raw {
		if harness != string(agentModelHarness) {
			return nil, fmt.Errorf("agent_model.%s is not supported: only opencode takes a model per step; choose %s's model with agent_config.%s.model, or the review loop's with review_agents", harness, harness, harness)
		}
		for key, value := range entries {
			if !slices.Contains(agentModelKeys, key) {
				return nil, fmt.Errorf("invalid agent_model.opencode key %q (valid: %s)", key, strings.Join(AgentModelKeys(), ", "))
			}
			model := strings.TrimSpace(value)
			if model == "" {
				return nil, fmt.Errorf("agent_model.opencode.%s is empty; delete the key to run that step on the agent default", key)
			}
			if err := agentcfg.Validate(agentModelHarness, agentcfg.Profile{Model: model}); err != nil {
				return nil, fmt.Errorf("invalid agent_model.opencode.%s: %w", key, err)
			}
			if routes == nil {
				routes = make(map[string]string, len(entries))
			}
			routes[key] = model
		}
	}
	return routes, nil
}

// agentModelReviewKeys are the agent_model keys that route the review loop,
// the reviewer's and the fixer's turns that review_agents also selects.
var agentModelReviewKeys = []string{string(types.StepReview), string(types.StepReview) + "-fix"}

// validateAgentModelReviewAgents refuses a review loop configured in both
// agent_model and review_agents. Ranking them instead would let a per-step
// route silently override a role's model, and with the *_after_round roles
// silently cancel upstream's escalation, so the loop is configured in one place
// or the other. Routing any other step alongside review_agents is fine.
func validateAgentModelReviewAgents(routes map[string]string, roles map[string]ReviewAgent) error {
	for _, key := range agentModelReviewKeys {
		if _, ok := routes[key]; !ok {
			continue
		}
		for _, role := range ReviewAgentRoles {
			if _, ok := roles[role]; ok {
				return fmt.Errorf("agent_model.opencode.%s conflicts with review_agents.%s: configure the review loop in one of them, not both", key, role)
			}
		}
	}
	return nil
}

// AgentModelConfigured reports whether agent_model routes any step.
func (c *Config) AgentModelConfigured() bool { return c != nil && len(c.AgentModel) > 0 }

// AgentModelFor resolves the per-invocation model for one agent invocation of
// step, and the agent_model key it came from. A fix-round invocation (purpose
// <step>-fix) uses its fix-round key when set, else its step's key; any other
// invocation of the step uses the step key. With no key it returns nothing, and
// upstream's own resolution applies unchanged: a review_agents role, then
// agent_config.opencode.model, then opencode's own default.
func (c *Config) AgentModelFor(step types.StepName, purpose string) (model, key string) {
	if !c.AgentModelConfigured() {
		return "", ""
	}
	if purpose == string(step)+"-fix" {
		if model, ok := c.AgentModel[purpose]; ok {
			return model, purpose
		}
	}
	if model, ok := c.AgentModel[string(step)]; ok {
		return model, string(step)
	}
	return "", ""
}
