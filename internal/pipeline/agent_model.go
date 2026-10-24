package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// agentModelRoutingAgent is the fork's per-step model routing seam. Every
// agent invocation of a step passes through the executor's step-agent stack,
// so routing here covers every agent-backed step and fix round without a
// routing line in any step file. It sets RunOpts.Model from
// config.AgentModelFor when the caller left it empty, and writes one step-log
// line per step execution for each distinct key it resolves, before that
// invocation runs. With no agent_model configured it changes nothing and logs
// nothing, so upstream's own model resolution applies untouched.
type agentModelRoutingAgent struct {
	inner    agent.Agent
	config   *config.Config
	stepName types.StepName
	log      func(string)

	mu     sync.Mutex
	logged map[string]bool // keys already logged; "" is the no-route line
}

func (a *agentModelRoutingAgent) Name() string { return a.inner.Name() }

func (a *agentModelRoutingAgent) Close() error { return a.inner.Close() }

func (a *agentModelRoutingAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *agentModelRoutingAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *agentModelRoutingAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *agentModelRoutingAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

func (a *agentModelRoutingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.Model == "" && a.config.AgentModelConfigured() {
		model, key := a.config.AgentModelFor(a.stepName, opts.Purpose)
		opts.Model = model
		a.logOnce(model, key)
	}
	return a.inner.Run(ctx, opts)
}

func (a *agentModelRoutingAgent) logOnce(model, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logged[key] {
		return
	}
	if a.logged == nil {
		a.logged = map[string]bool{}
	}
	a.logged[key] = true
	switch harness := a.inner.Name(); {
	case key == "":
		a.log(fmt.Sprintf("no agent_model override configured for step %s; running on agent default", a.stepName))
	case harness != string(types.AgentOpenCode):
		// Only the opencode adapter takes a model per message; any other
		// harness fixed its model at launch and silently ignores this one.
		a.log(fmt.Sprintf("agent_model override %q configured for step %s (key %s) but agent %q ignores per-step model overrides; running on agent default", model, a.stepName, key, harness))
	default:
		a.log(fmt.Sprintf("using agent_model override for step %s: %s (key %s)", a.stepName, model, key))
	}
}
