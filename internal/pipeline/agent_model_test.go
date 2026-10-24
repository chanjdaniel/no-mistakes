package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// modelProbeAgent records the per-invocation model each call arrived with.
type modelProbeAgent struct {
	name   string
	mu     sync.Mutex
	models map[string]string // purpose -> model
}

func (a *modelProbeAgent) Name() string { return a.name }
func (a *modelProbeAgent) Close() error { return nil }
func (a *modelProbeAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.models == nil {
		a.models = map[string]string{}
	}
	a.models[opts.Purpose] = opts.Model
	return &agent.Result{Output: json.RawMessage(`{}`)}, nil
}

func (a *modelProbeAgent) model(purpose string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.models[purpose]
}

// invokingStep runs one agent invocation per RunOpts, in order.
func invokingStep(name types.StepName, calls ...agent.RunOpts) Step {
	return &adaptiveCallStep{
		name: name,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			for _, opts := range calls {
				if _, err := sctx.Agent.Run(sctx.Ctx, opts); err != nil {
					return nil, err
				}
			}
			return &StepOutcome{}, nil
		},
	}
}

func routedConfig(agentName types.AgentName, routes map[string]string) *config.Config {
	return &config.Config{Agent: agentName, AgentModel: routes}
}

func readStepLog(t *testing.T, logDir string, step types.StepName) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, string(step)+".log"))
	if err != nil {
		t.Fatalf("read %s log: %v", step, err)
	}
	return string(data)
}

func TestExecutorRoutesEachInvocationToItsStepsModel(t *testing.T) {
	database, p, run, repo := setupTest(t)
	probe := &modelProbeAgent{name: "opencode"}
	steps := []Step{
		invokingStep(types.StepIntent, agent.RunOpts{Prompt: "summarize", Purpose: "intent"}),
		invokingStep(types.StepReview,
			agent.RunOpts{Prompt: "review", Purpose: "review"},
			agent.RunOpts{Prompt: "fix", Purpose: "review-fix"},
			agent.RunOpts{Prompt: "pinned", Purpose: "pinned", Model: "caller/model"}),
		invokingStep(types.StepTest, agent.RunOpts{Prompt: "test"}, agent.RunOpts{Prompt: "fix", Purpose: "test-fix"}),
	}
	cfg := routedConfig(types.AgentOpenCode, map[string]string{"review": "p/review", "test": "p/test"})
	exec := NewExecutor(database, p, cfg, probe, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	for purpose, want := range map[string]string{
		"intent":     "",
		"review":     "p/review",
		"review-fix": "p/review",
		"pinned":     "caller/model",
		"":           "p/test",
		"test-fix":   "p/test",
	} {
		if got := probe.model(purpose); got != want {
			t.Errorf("invocation %q carried model %q, want %q", purpose, got, want)
		}
	}

	logDir := p.RunLogDir(run.ID)
	reviewLog := readStepLog(t, logDir, types.StepReview)
	const routed = "using agent_model override for step review: p/review (key review)"
	if got := strings.Count(reviewLog, routed); got != 1 {
		t.Errorf("review log has %d %q lines, want one per step execution:\n%s", got, routed, reviewLog)
	}
	if testLog := readStepLog(t, logDir, types.StepTest); !strings.Contains(testLog, "using agent_model override for step test: p/test (key test)") {
		t.Errorf("test log does not name its route:\n%s", testLog)
	}
	intentLog := readStepLog(t, logDir, types.StepIntent)
	if !strings.Contains(intentLog, "no agent_model override configured for step intent; running on agent default") {
		t.Errorf("intent log does not say it runs on the agent default:\n%s", intentLog)
	}
}

func TestExecutorWithoutAgentModelRoutesAndLogsNothing(t *testing.T) {
	database, p, run, repo := setupTest(t)
	probe := &modelProbeAgent{name: "opencode"}
	steps := []Step{invokingStep(types.StepReview, agent.RunOpts{Prompt: "review", Purpose: "review"})}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentOpenCode}, probe, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := probe.model("review"); got != "" {
		t.Fatalf("review carried model %q with no agent_model configured", got)
	}
	if log := readStepLog(t, p.RunLogDir(run.ID), types.StepReview); strings.Contains(log, "agent_model") {
		t.Fatalf("review log mentions agent_model with none configured:\n%s", log)
	}
}

// A harness other than opencode fixes its model at launch, so a route it
// receives is ignored; the step log says so instead of claiming the route.
func TestExecutorWarnsWhenTheServingHarnessIgnoresTheRoute(t *testing.T) {
	database, p, run, repo := setupTest(t)
	probe := &modelProbeAgent{name: "claude"}
	steps := []Step{invokingStep(types.StepReview, agent.RunOpts{Prompt: "review", Purpose: "review"})}
	cfg := routedConfig(types.AgentClaude, map[string]string{"review": "p/review"})
	exec := NewExecutor(database, p, cfg, probe, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	log := readStepLog(t, p.RunLogDir(run.ID), types.StepReview)
	if !strings.Contains(log, `agent_model override "p/review" configured for step review (key review) but agent "claude" ignores per-step model overrides; running on agent default`) {
		t.Fatalf("review log has no ignored-route warning:\n%s", log)
	}
	if strings.Contains(log, "using agent_model override") {
		t.Fatalf("review log claims a route the harness ignores:\n%s", log)
	}
}
