// Package memkernel is the in-memory kernel.Ctx implementation: journals live
// in a map, so they survive re-running a workflow inside one process (that is
// what replay and "crash" tests exercise) but not a process restart. It exists
// to develop and test internal/executor before a durable backend is wired, and
// as the reference behaviour kernel/conformance holds the others to.
package memkernel

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/kernel"
)

// Workflow is a registered workflow body.
type Workflow = func(c kernel.Ctx, input []byte) ([]byte, error)

type entry struct {
	kind   string // "step/<name>", "sleep", "await/<signal>", "spawn/<workflow>", "join/<child>", "now", "rand"
	output []byte
	err    error
}

type child struct {
	done chan struct{}
	out  []byte
	err  error
}

type Kernel struct {
	mu        sync.Mutex
	workflows map[string]Workflow
	journals  map[string][]entry
	signals   map[string]chan []byte // key: runID + "\x00" + signal name
	children  map[string]*child
}

func New() *Kernel {
	return &Kernel{
		workflows: map[string]Workflow{},
		journals:  map[string][]entry{},
		signals:   map[string]chan []byte{},
		children:  map[string]*child{},
	}
}

func (k *Kernel) Register(name string, wf Workflow) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.workflows[name] = wf
}

// Run executes workflow as runID. Running the same runID again replays its
// journal: recorded steps return their results without re-running.
func (k *Kernel) Run(ctx context.Context, runID, workflow string, input []byte) ([]byte, error) {
	k.mu.Lock()
	wf, ok := k.workflows[workflow]
	k.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("memkernel: workflow %q not registered", workflow)
	}
	return wf(&runCtx{Context: ctx, k: k, runID: runID}, input)
}

// Signal delivers payload to runID's Await(name). Signals sent before the
// Await are buffered.
func (k *Kernel) Signal(_ context.Context, runID, name string, payload []byte) error {
	select {
	case k.signal(runID, name) <- payload:
		return nil
	default:
		return fmt.Errorf("memkernel: signal buffer full for %s/%s", runID, name)
	}
}

func (k *Kernel) signal(runID, name string) chan []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := runID + "\x00" + name
	ch, ok := k.signals[key]
	if !ok {
		ch = make(chan []byte, 16)
		k.signals[key] = ch
	}
	return ch
}

// startChild runs a spawned child once; later calls (replay) reuse it.
func (k *Kernel) startChild(ctx context.Context, id, workflow string, input []byte) *child {
	k.mu.Lock()
	ch, ok := k.children[id]
	if !ok {
		ch = &child{done: make(chan struct{})}
		k.children[id] = ch
	}
	k.mu.Unlock()
	if !ok {
		go func() {
			defer close(ch.done)
			// The child outlives the parent's call stack, not its cancellation.
			ch.out, ch.err = k.Run(context.WithoutCancel(ctx), id, workflow, input)
		}()
	}
	return ch
}

type runCtx struct {
	context.Context
	k     *Kernel
	runID string
	seq   int
	rng   *rand.Rand
}

func (c *runCtx) RunID() string { return c.runID }

// record is the whole durability model: the n-th journaled call of a run gets
// journal[n]. If it exists, its result is returned (replay), after checking it
// was recorded by the same kind of call. Otherwise fn runs and its result is
// appended. A run whose context ended mid-call is treated as a crash: nothing
// is recorded, so the next Run executes that call again.
func (c *runCtx) record(kind string, fn func() ([]byte, error)) ([]byte, error) {
	seq := c.seq
	c.seq++

	c.k.mu.Lock()
	journal := c.k.journals[c.runID]
	if seq < len(journal) {
		e := journal[seq]
		c.k.mu.Unlock()
		if e.kind != kind {
			return nil, fmt.Errorf("%w: run %s call %d was %q, now %q", kernel.ErrNondeterminism, c.runID, seq, e.kind, kind)
		}
		return e.output, e.err
	}
	c.k.mu.Unlock()

	out, err := fn()
	if c.Err() != nil {
		return nil, c.Err()
	}
	c.k.mu.Lock()
	c.k.journals[c.runID] = append(c.k.journals[c.runID], entry{kind: kind, output: out, err: err})
	c.k.mu.Unlock()
	return out, err
}

func (c *runCtx) Step(name string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	return c.record("step/"+name, func() ([]byte, error) { return fn(c) })
}

func (c *runCtx) Sleep(d time.Duration) error {
	_, err := c.record("sleep", func() ([]byte, error) {
		select {
		case <-time.After(d):
			return nil, nil
		case <-c.Done():
			return nil, c.Err()
		}
	})
	return err
}

func (c *runCtx) Await(signal string, timeout time.Duration) ([]byte, error) {
	return c.record("await/"+signal, func() ([]byte, error) {
		select {
		case p := <-c.k.signal(c.runID, signal):
			return p, nil
		case <-time.After(timeout):
			return nil, kernel.ErrTimeout
		case <-c.Done():
			return nil, c.Err()
		}
	})
}

func (c *runCtx) Spawn(workflow string, input []byte) (kernel.Future, error) {
	id := fmt.Sprintf("%s/%d", c.runID, c.seq)
	if _, err := c.record("spawn/"+workflow, func() ([]byte, error) { return []byte(id), nil }); err != nil {
		return nil, err
	}
	return &future{id: id, ch: c.k.startChild(c, id, workflow, input)}, nil
}

// Now and Rand can't return an error, so a nondeterministic replay panics:
// it is a bug in the workflow code, never a condition to recover from.
func (c *runCtx) Now() time.Time {
	out, err := c.record("now", func() ([]byte, error) { return time.Now().MarshalBinary() })
	if err != nil {
		panic(err)
	}
	var t time.Time
	if err := t.UnmarshalBinary(out); err != nil {
		panic(err)
	}
	return t
}

// Rand journals one seed per run, at the first call; later calls reuse the
// same source, so they consume no journal entries.
func (c *runCtx) Rand() *rand.Rand {
	if c.rng == nil {
		out, err := c.record("rand", func() ([]byte, error) {
			return fmt.Appendf(nil, "%d", time.Now().UnixNano()), nil
		})
		if err != nil {
			panic(err)
		}
		var seed int64
		if _, err := fmt.Sscan(string(out), &seed); err != nil {
			panic(err)
		}
		c.rng = rand.New(rand.NewSource(seed))
	}
	return c.rng
}

type future struct {
	id string
	ch *child
}

func (f *future) RunID() string { return f.id }

func (f *future) Get(ctx kernel.Ctx) ([]byte, error) {
	c, ok := ctx.(*runCtx)
	if !ok {
		return nil, fmt.Errorf("memkernel: Future.Get needs a memkernel Ctx, got %T", ctx)
	}
	return c.record("join/"+f.id, func() ([]byte, error) {
		select {
		case <-f.ch.done:
			return f.ch.out, f.ch.err
		case <-c.Done():
			return nil, c.Err()
		}
	})
}
