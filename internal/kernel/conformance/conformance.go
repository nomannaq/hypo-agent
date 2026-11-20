// Package conformance is one test suite every kernel implementation must pass
// (memkernel now; pgkernel and temporalkernel later). If the same suite passes
// against all of them, they are interchangeable in fact, not just by the shape
// of kernel.Ctx. Each implementation calls Run from its own _test.go.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/kernel"
)

// Workflow is a registered workflow body.
type Workflow = func(c kernel.Ctx, input []byte) ([]byte, error)

// Harness is what a kernel exposes to the suite. Workflows are registered by
// name, not passed as closures, because engines like Temporal can only run
// registered functions.
type Harness interface {
	Register(name string, wf Workflow)
	// Run executes workflow as runID. Calling it again with the same runID
	// must replay: recorded calls return their results without re-running.
	Run(ctx context.Context, runID, workflow string, input []byte) ([]byte, error)
	Signal(ctx context.Context, runID, name string, payload []byte) error
}

// Run executes the suite. newHarness must return a fresh, empty kernel.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	tests := map[string]func(*testing.T, Harness){
		"StepReplaysWithoutRerunning":   stepReplays,
		"CrashMidStepResumes":           crashResumes,
		"ChangedStepIsNondeterministic": nondeterminism,
		"NowAndRandAreStableOnReplay":   nowAndRand,
		"AwaitReceivesSignal":           awaitSignal,
		"AwaitTimesOutAndReplays":       awaitTimeout,
		"SleepIsSkippedOnReplay":        sleepReplay,
		"SpawnedChildReturnsResult":     spawn,
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) { test(t, newHarness(t)) })
	}
}

func mustRun(t *testing.T, h Harness, runID, wf string) []byte {
	t.Helper()
	out, err := h.Run(t.Context(), runID, wf, nil)
	if err != nil {
		t.Fatalf("run %s: %v", runID, err)
	}
	return out
}

func stepReplays(t *testing.T, h Harness) {
	var calls atomic.Int32
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		return c.Step("a", func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte("result"), nil
		})
	})
	first := mustRun(t, h, "r1", "wf")
	second := mustRun(t, h, "r1", "wf")
	if !bytes.Equal(first, second) || string(first) != "result" {
		t.Fatalf("replay returned %q, first run %q", second, first)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("step body ran %d times, want 1", n)
	}
}

// The kill-worker scenario: the run dies inside step b. Rerunning it must skip
// a (already recorded) and execute b.
func crashResumes(t *testing.T, h Harness) {
	var aCalls, bCalls atomic.Int32
	crash := true
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		if _, err := c.Step("a", func(context.Context) ([]byte, error) {
			aCalls.Add(1)
			return nil, nil
		}); err != nil {
			return nil, err
		}
		return c.Step("b", func(context.Context) ([]byte, error) {
			bCalls.Add(1)
			return []byte("ok"), nil
		})
	})

	ctx, cancel := context.WithCancel(t.Context())
	h.Register("crashing", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		if _, err := c.Step("a", func(context.Context) ([]byte, error) {
			aCalls.Add(1)
			return nil, nil
		}); err != nil {
			return nil, err
		}
		return c.Step("b", func(context.Context) ([]byte, error) {
			if crash {
				cancel() // the worker dies mid-step
				return nil, context.Canceled
			}
			return nil, nil
		})
	})
	if _, err := h.Run(ctx, "r1", "crashing", nil); err == nil {
		t.Fatal("crashed run reported success")
	}
	// Same run, same code path, restarted after the crash.
	if out := mustRun(t, h, "r1", "wf"); string(out) != "ok" {
		t.Fatalf("resumed run returned %q", out)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("a ran %d times, b ran %d times; want 1 and 1", aCalls.Load(), bCalls.Load())
	}
}

func nondeterminism(t *testing.T, h Harness) {
	step := func(name string) Workflow {
		return func(c kernel.Ctx, _ []byte) ([]byte, error) {
			return c.Step(name, func(context.Context) ([]byte, error) { return nil, nil })
		}
	}
	h.Register("v1", step("a"))
	h.Register("v2", step("renamed"))
	mustRun(t, h, "r1", "v1")
	if _, err := h.Run(t.Context(), "r1", "v2", nil); !errors.Is(err, kernel.ErrNondeterminism) {
		t.Fatalf("replay with a renamed step: err = %v, want ErrNondeterminism", err)
	}
}

func nowAndRand(t *testing.T, h Harness) {
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		now := c.Now().UnixNano()
		r := c.Rand().Int63()
		return []byte(strconv.FormatInt(now, 10) + "/" + strconv.FormatInt(r, 10)), nil
	})
	first := mustRun(t, h, "r1", "wf")
	time.Sleep(2 * time.Millisecond)
	if second := mustRun(t, h, "r1", "wf"); !bytes.Equal(first, second) {
		t.Fatalf("Now/Rand changed on replay: %s then %s", first, second)
	}
}

func awaitSignal(t *testing.T, h Harness) {
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		return c.Await("approval", 5*time.Second)
	})
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = h.Signal(context.Background(), "r1", "approval", []byte("approved"))
	}()
	if out := mustRun(t, h, "r1", "wf"); string(out) != "approved" {
		t.Fatalf("await returned %q", out)
	}
	// A signal sent before the Await must not be lost.
	if err := h.Signal(t.Context(), "r2", "approval", []byte("early")); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, h, "r2", "wf"); string(out) != "early" {
		t.Fatalf("early signal: await returned %q", out)
	}
}

func awaitTimeout(t *testing.T, h Harness) {
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		return c.Await("never", 20*time.Millisecond)
	})
	for range 2 { // second pass is the replay
		if _, err := h.Run(t.Context(), "r1", "wf", nil); !errors.Is(err, kernel.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	}
}

func sleepReplay(t *testing.T, h Harness) {
	h.Register("wf", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		return nil, c.Sleep(100 * time.Millisecond)
	})
	mustRun(t, h, "r1", "wf")
	start := time.Now()
	mustRun(t, h, "r1", "wf")
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("replayed sleep took %s; it should return at once", d)
	}
}

func spawn(t *testing.T, h Harness) {
	h.Register("child", func(c kernel.Ctx, in []byte) ([]byte, error) {
		return c.Step("double", func(context.Context) ([]byte, error) {
			return append(in, in...), nil
		})
	})
	h.Register("parent", func(c kernel.Ctx, _ []byte) ([]byte, error) {
		f, err := c.Spawn("child", []byte("ab"))
		if err != nil {
			return nil, err
		}
		return f.Get(c)
	})
	if out := mustRun(t, h, "r1", "parent"); string(out) != "abab" {
		t.Fatalf("child result %q, want abab", out)
	}
	if out := mustRun(t, h, "r1", "parent"); string(out) != "abab" {
		t.Fatalf("replayed child result %q, want abab", out)
	}
}
