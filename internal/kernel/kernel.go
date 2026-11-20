// Package kernel defines the durability contract between the runtime and
// whichever engine executes it — memkernel (tests), pgkernel (our own,
// Postgres-backed), or temporalkernel (production). Nothing outside this
// package and its implementations should import an engine directly.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

var (
	// ErrTimeout is returned by Await when no signal arrives in time.
	ErrTimeout = errors.New("kernel: await timed out")
	// ErrNondeterminism means replay reached a journal entry recorded by a
	// different call (another step or activity name, or a Sleep where a Call
	// was). The workflow code changed under a live run; continuing would
	// corrupt it.
	ErrNondeterminism = errors.New("kernel: nondeterministic replay")
	// ErrCanceled means the run stopped mid-call (worker crash, shutdown,
	// cancellation). Nothing was journaled for that call, so resuming the run
	// executes it again. It is never an agent-level failure.
	ErrCanceled = errors.New("kernel: run canceled")
)

// ActivityFunc is a registered activity body: the only place a workflow may
// do I/O. It runs at most once per (run, step) as far as the workflow can
// tell, and receives a real context.Context.
type ActivityFunc = func(ctx context.Context, input []byte) ([]byte, error)

// Ctx is what workflow code sees. Workflow code must be deterministic: it
// does I/O only through Call, and gets time and randomness only from Now and
// Rand. Ctx is deliberately not a context.Context: Temporal's workflow
// context isn't one, and Go context semantics (Done channels, goroutines)
// don't replay.
//
// Implementations: memkernel (tests), pgkernel, temporalkernel.
type Ctx interface {
	// RunID identifies this run (one agent session).
	RunID() string

	// Call runs the activity registered as activity with input, at most once
	// per (RunID, step), and journals its result. On replay the journaled
	// result is returned and the activity is not called again. A step name
	// reused for a different activity is a nondeterminism bug; kernels fail
	// with ErrNondeterminism rather than silently diverge. If the run stops
	// mid-call, Call returns an error wrapping ErrCanceled.
	Call(step, activity string, input []byte) ([]byte, error)

	// Sleep is a durable timer: it survives worker restarts and crashes.
	Sleep(d time.Duration) error

	// Await blocks for an external signal (an approval, a webhook) or until
	// timeout elapses, whichever comes first.
	Await(signal string, timeout time.Duration) ([]byte, error)

	// Spawn starts a child run (a sub-agent) and returns a handle to it.
	Spawn(workflow string, input []byte) (Future, error)

	// Now returns the journaled, replay-safe clock. Never call time.Now() in
	// workflow code.
	Now() time.Time

	// Rand returns a journaled, replay-safe random source. Never use an
	// unseeded math/rand.Rand in workflow code.
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

// Activity names an activity and fixes its input and output types, so the
// call site (Call) and the registration (Func) can't disagree on them.
type Activity[In, Out any] struct {
	Name string
}

// Func wraps a typed body as the ActivityFunc kernels register, with JSON as
// the wire format.
func (a Activity[In, Out]) Func(fn func(context.Context, In) (Out, error)) ActivityFunc {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var in In
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, fmt.Errorf("activity %s: bad input: %w", a.Name, err)
		}
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}
}

// Call is the typed form of Ctx.Call.
func Call[In, Out any](c Ctx, step string, a Activity[In, Out], in In) (Out, error) {
	var out Out
	input, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("activity %s: encoding input: %w", a.Name, err)
	}
	raw, err := c.Call(step, a.Name, input)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("activity %s: decoding output: %w", a.Name, err)
	}
	return out, nil
}
