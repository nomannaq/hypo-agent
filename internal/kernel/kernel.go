// Package kernel defines the durability contract between the runtime and
// whichever engine executes it — memkernel (tests), pgkernel (our own,
// Postgres-backed), or temporalkernel (production). Nothing outside this
// package and its implementations should import an engine directly.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"time"
)

var (
	// ErrTimeout is returned by Await when no signal arrives in time.
	ErrTimeout = errors.New("kernel: await timed out")
	// ErrNondeterminism means replay reached a journal entry recorded by a
	// different call (another step name, or a Sleep where a Step was). The
	// workflow code changed under a live run; continuing would corrupt it.
	ErrNondeterminism = errors.New("kernel: nondeterministic replay")
)

// Ctx is the only contract between the runtime and the durability engine.
// Implementations: memkernel (tests), pgkernel, temporalkernel.
type Ctx interface {
	context.Context

	// RunID identifies this run (one agent session).
	RunID() string

	// Step runs fn at most once per (RunID, name) and journals its result.
	// On replay, the journaled result is returned and fn is not called
	// again. A name reused with a different input is a nondeterminism bug;
	// implementations should fail loudly rather than silently diverge.
	Step(name string, fn func(context.Context) ([]byte, error)) ([]byte, error)

	// Sleep is a durable timer: it survives worker restarts and crashes.
	Sleep(d time.Duration) error

	// Await blocks for an external signal (an approval, a webhook) or until
	// timeout elapses, whichever comes first.
	Await(signal string, timeout time.Duration) ([]byte, error)

	// Spawn starts a child run (a sub-agent) and returns a handle to it.
	Spawn(workflow string, input []byte) (Future, error)

	// Now returns the journaled, replay-safe clock. Calling time.Now()
	// directly inside a Step body breaks replay — use this instead.
	Now() time.Time

	// Rand returns a journaled, replay-safe random source. An unseeded
	// math/rand.Rand used directly inside a Step body breaks replay.
	Rand() *rand.Rand
}

// Future is a handle to a child run started with Spawn.
type Future interface {
	RunID() string
	Get(ctx Ctx) ([]byte, error)
}

// ContinueAsNewer is implemented by kernels whose execution history has a
// size limit (temporalkernel). The executor calls it periodically on long
// sessions via a type assertion; kernels without the constraint (memkernel,
// pgkernel) simply don't implement it, so the assertion is skipped.
type ContinueAsNewer interface {
	ContinueAsNew(input []byte) error
}

// StepT is a typed wrapper around Ctx.Step for callers that don't want to
// hand-marshal []byte.
func StepT[T any](c Ctx, name string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	out, err := c.Step(name, func(sc context.Context) ([]byte, error) {
		v, err := fn(sc)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	})
	if err != nil {
		return zero, err
	}
	var v T
	if err := json.Unmarshal(out, &v); err != nil {
		return zero, err
	}
	return v, nil
}
