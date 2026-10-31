package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestWatchAgentStall_SilentInvocationIsCutWithTheStallCause(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "mute",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{Ctx: context.Background(), Agent: ag, Config: &config.Config{}}
	opts := agent.RunOpts{Prompt: "work"}
	ctx, watch := WatchAgentStall(sctx.Ctx, 30*time.Millisecond, &opts)
	defer watch.Stop()

	start := time.Now()
	_, err := sctx.RunAgentContext(ctx, opts)
	if !IsAgentStall(err) {
		t.Fatalf("error = %v, want the stall cause", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stall took %s, want the window to end the invocation", elapsed)
	}
	if !watch.Stalled() || watch.Active() {
		t.Fatalf("stalled=%v active=%v, want a stall with no activity", watch.Stalled(), watch.Active())
	}
	// The stall is diagnosed like a deadline: the measurement rides along.
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want the measured silence", err)
	}
	if !IsAgentDeadlineError(err) {
		t.Fatalf("error = %v, want a stall to read as a cut invocation, never a formatting slip", err)
	}
}

func TestWatchAgentStall_ActivityThenSilenceIsCutAndRemembersTheActivity(t *testing.T) {
	t.Parallel()
	for _, emit := range []struct {
		name string
		fn   func(agent.RunOpts)
	}{
		{"chunk", func(opts agent.RunOpts) { opts.OnChunk("working") }},
		{"lifecycle activity", func(opts agent.RunOpts) {
			opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseActivity})
		}},
	} {
		t.Run(emit.name, func(t *testing.T) {
			t.Parallel()
			ag := &hangingAgent{
				name: "goes-quiet",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					emit.fn(opts)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}
			sctx := &StepContext{Ctx: context.Background(), Agent: ag, Config: &config.Config{}}
			opts := agent.RunOpts{Prompt: "work"}
			ctx, watch := WatchAgentStall(sctx.Ctx, 30*time.Millisecond, &opts)
			defer watch.Stop()

			_, err := sctx.RunAgentContext(ctx, opts)
			if !IsAgentStall(err) {
				t.Fatalf("error = %v, want the stall cause", err)
			}
			if !watch.Active() {
				t.Fatal("activity before the stall was not recorded")
			}
		})
	}
}

func TestWatchAgentStall_ControlLifecycleEventsAreNotActivity(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "control-only",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			for _, phase := range []string{agent.LifecyclePhaseStart, agent.LifecyclePhaseRetry, agent.LifecyclePhaseFallback, agent.LifecyclePhaseExit} {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: phase})
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{Ctx: context.Background(), Agent: ag, Config: &config.Config{}}
	opts := agent.RunOpts{Prompt: "work"}
	ctx, watch := WatchAgentStall(sctx.Ctx, 30*time.Millisecond, &opts)
	defer watch.Stop()

	if _, err := sctx.RunAgentContext(ctx, opts); !IsAgentStall(err) {
		t.Fatalf("error = %v, want the stall cause", err)
	}
	if watch.Active() {
		t.Fatal("a launch, retry, fallback, or exit event was counted as activity")
	}
}

func TestWatchAgentStall_BusyInvocationIsNeverCut(t *testing.T) {
	t.Parallel()
	var forwarded int
	ag := &hangingAgent{
		name: "busy",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Keep emitting for several windows, alternating the two signals.
			deadline := time.Now().Add(200 * time.Millisecond)
			for i := 0; time.Now().Before(deadline); i++ {
				select {
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				case <-time.After(5 * time.Millisecond):
				}
				if i%2 == 0 {
					opts.OnChunk("x")
				} else {
					opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseActivity})
				}
			}
			return &agent.Result{Text: "done"}, nil
		},
	}
	sctx := &StepContext{Ctx: context.Background(), Agent: ag, Config: &config.Config{}}
	opts := agent.RunOpts{Prompt: "work", OnChunk: func(string) { forwarded++ }}
	ctx, watch := WatchAgentStall(sctx.Ctx, 40*time.Millisecond, &opts)
	defer watch.Stop()

	result, err := sctx.RunAgentContext(ctx, opts)
	if err != nil || result == nil || result.Text != "done" {
		t.Fatalf("result=%+v err=%v, want the busy agent to finish normally", result, err)
	}
	if watch.Stalled() {
		t.Fatal("a busy invocation was reported stalled")
	}
	if forwarded == 0 {
		t.Fatal("the wrapped OnChunk stopped forwarding to the caller's callback")
	}
}

func TestWatchAgentStall_NonPositiveWindowWatchesNothing(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	called := false
	opts := agent.RunOpts{OnChunk: func(string) { called = true }}
	ctx, watch := WatchAgentStall(parent, 0, &opts)
	defer watch.Stop()
	if ctx != parent {
		t.Fatal("a disabled window derived a new context")
	}
	opts.OnChunk("x")
	if !called {
		t.Fatal("a disabled window dropped the caller's callback")
	}
	if watch.Stalled() || watch.Active() {
		t.Fatal("a disabled window reported state")
	}
}

func TestWatchAgentStall_ParentDeadlineKeepsItsOwnCause(t *testing.T) {
	t.Parallel()
	budget := errors.New("budget")
	parent, cancel := context.WithTimeoutCause(context.Background(), 20*time.Millisecond, budget)
	defer cancel()
	opts := agent.RunOpts{}
	ctx, watch := WatchAgentStall(parent, time.Hour, &opts)
	defer watch.Stop()
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), budget) || watch.Stalled() {
		t.Fatalf("cause=%v stalled=%v, want the parent's budget and no stall", context.Cause(ctx), watch.Stalled())
	}
}
