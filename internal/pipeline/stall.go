package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// ErrAgentStall is the cause a StallWatch cancels its invocation with when the
// liveness window passes without activity. classifyAgentRun diagnoses it like a
// deadline, so the returned error carries the measured activity and the
// adapter's own report, and IsAgentDeadlineError holds for it.
var ErrAgentStall = errors.New("agent stalled")

// IsAgentStall reports whether err is an invocation a StallWatch cut.
func IsAgentStall(err error) bool { return errors.Is(err, ErrAgentStall) }

// StallWatch is a liveness window over one agent invocation. A wall-clock
// budget alone lets a provider that went silent burn the whole budget; the
// window ends such an invocation once it has shown no activity for that long.
// Activity is what agentActivity also counts as output: streamed text
// (OnChunk) and agent.LifecyclePhaseActivity. Launch, retry, fallback, and exit
// events are control metadata and never extend the window.
type StallWatch struct {
	window time.Duration
	cancel context.CancelCauseFunc
	timer  *time.Timer

	mu      sync.Mutex
	active  bool
	stalled bool
	stopped bool
}

// WatchAgentStall derives the invocation context from parent and wraps
// opts.OnChunk and opts.OnLifecycle so activity restarts the window; the
// wrappers always forward to the caller's callbacks. Pass the returned context
// and the wrapped opts to the agent run, and Stop the watch once it returns.
// A non-positive window watches nothing: the context is parent and opts is
// left as it was.
func WatchAgentStall(parent context.Context, window time.Duration, opts *agent.RunOpts) (context.Context, *StallWatch) {
	w := &StallWatch{window: window}
	if window <= 0 {
		return parent, w
	}
	ctx, cancel := context.WithCancelCause(parent)
	w.cancel = cancel
	w.timer = time.AfterFunc(window, w.fire)

	onChunk := opts.OnChunk
	opts.OnChunk = func(text string) {
		w.observe()
		if onChunk != nil {
			onChunk(text)
		}
	}
	onLifecycle := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		if event.Phase == agent.LifecyclePhaseActivity {
			w.observe()
		}
		if onLifecycle != nil {
			onLifecycle(event)
		}
	}
	return ctx, w
}

func (w *StallWatch) fire() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stalled = true
	w.mu.Unlock()
	w.cancel(ErrAgentStall)
}

func (w *StallWatch) observe() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = true
	if !w.stopped && !w.stalled {
		w.timer.Reset(w.window)
	}
}

// Stop releases the window's timer and context. Callbacks arriving after it
// still forward but no longer re-arm anything.
func (w *StallWatch) Stop() {
	if w == nil || w.timer == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.timer.Stop()
	w.cancel(context.Canceled)
}

// Stalled reports whether the window cut the invocation.
func (w *StallWatch) Stalled() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

// Active reports whether the invocation showed any activity at all.
func (w *StallWatch) Active() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}
