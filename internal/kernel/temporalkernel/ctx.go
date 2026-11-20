// Package temporalkernel adapts kernel.Ctx onto the Temporal Go SDK.
// A workflow is a kernel.Ctx workflow wrapped with Wrap; kernel.Ctx.Call is a
// registered Temporal activity called by name with explicit input, so the
// workflow's event history is the journal and Temporal's replay is the
// kernel's replay. See .junie/plans/durability.md.
package temporalkernel

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"nomanqureshi.dev/hpyo-agent/internal/kernel"
)

// TaskQueue is the queue agent runs are started on and workers poll.
const TaskQueue = "hypo-agent"

// Workflow is a registered workflow body.
type Workflow = func(c kernel.Ctx, input []byte) ([]byte, error)

// Options are the activity settings every Call in a workflow uses.
type Options struct {
	// ActivityTimeout bounds one attempt of an activity (StartToCloseTimeout).
	// It is also how long Temporal waits before retrying an attempt whose
	// worker died without reporting back.
	ActivityTimeout time.Duration
	// MaxAttempts is the activity retry budget. Model and tool clients do
	// their own retries for transient HTTP errors; these attempts cover a lost
	// worker or a crashed activity.
	MaxAttempts int32
	// RetryInterval is the first backoff between attempts.
	RetryInterval time.Duration
	// HeartbeatTimeout is how fast a worker that died mid-activity is
	// noticed. Registered activities heartbeat automatically; without this,
	// a lost worker stalls the run for the whole ActivityTimeout.
	HeartbeatTimeout time.Duration
}

var DefaultOptions = Options{
	ActivityTimeout:  3 * time.Minute,
	MaxAttempts:      3,
	RetryInterval:    time.Second,
	HeartbeatTimeout: 20 * time.Second,
}

// RegisterWorkflow registers wf under name on a worker (or a replayer).
func RegisterWorkflow(r worker.WorkflowRegistry, name string, wf Workflow, opts Options) {
	r.RegisterWorkflowWithOptions(Wrap(wf, opts), workflow.RegisterOptions{Name: name})
}

// RegisterActivity registers fn under name on a worker. The registered
// activity heartbeats while fn runs (see Options.HeartbeatTimeout).
func RegisterActivity(r worker.ActivityRegistry, name string, fn kernel.ActivityFunc) {
	r.RegisterActivityWithOptions(heartbeating(fn), activity.RegisterOptions{Name: name})
}

// heartbeating reports liveness while fn runs, so Temporal notices a dead
// worker within the heartbeat timeout. Activity bodies like a model call are
// one blocking request and can't heartbeat themselves.
func heartbeating(fn kernel.ActivityFunc) kernel.ActivityFunc {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if every := activity.GetInfo(ctx).HeartbeatTimeout / 3; every > 0 {
			done := make(chan struct{})
			defer close(done)
			go func() {
				t := time.NewTicker(every)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						activity.RecordHeartbeat(ctx)
					case <-done:
						return
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		return fn(ctx, input)
	}
}

// Wrap turns a kernel workflow into a Temporal workflow function.
func Wrap(wf Workflow, opts Options) func(workflow.Context, []byte) ([]byte, error) {
	if opts == (Options{}) {
		opts = DefaultOptions
	}
	return func(ctx workflow.Context, input []byte) ([]byte, error) {
		out, err := wf(&workflowCtx{ctx: ctx, opts: opts}, input)
		return out, toWorkflowError(err)
	}
}

type workflowCtx struct {
	ctx      workflow.Context
	opts     Options
	spawnSeq int
	rng      *rand.Rand
}

func (c *workflowCtx) RunID() string { return workflow.GetInfo(c.ctx).WorkflowExecution.ID }

// Call schedules the activity with the step name as its activity ID: Temporal's
// replay compares both the ID and the activity type, so renaming a step under
// a live run is detected as nondeterminism, exactly as in memkernel.
func (c *workflowCtx) Call(step, activityName string, input []byte) ([]byte, error) {
	ctx := workflow.WithActivityOptions(c.ctx, workflow.ActivityOptions{
		ActivityID:          step,
		StartToCloseTimeout: c.opts.ActivityTimeout,
		HeartbeatTimeout:    c.opts.HeartbeatTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: c.opts.RetryInterval,
			MaximumAttempts: c.opts.MaxAttempts,
		},
	})
	var out []byte
	err := workflow.ExecuteActivity(ctx, activityName, input).Get(ctx, &out)
	return out, fromTemporal(err)
}

func (c *workflowCtx) Sleep(d time.Duration) error {
	return fromTemporal(workflow.Sleep(c.ctx, d))
}

func (c *workflowCtx) Await(signal string, timeout time.Duration) ([]byte, error) {
	timerCtx, cancelTimer := workflow.WithCancel(c.ctx)
	defer cancelTimer()

	var payload []byte
	received := false
	sel := workflow.NewSelector(c.ctx)
	sel.AddReceive(workflow.GetSignalChannel(c.ctx, signal), func(ch workflow.ReceiveChannel, _ bool) {
		ch.Receive(c.ctx, &payload)
		received = true
	})
	timer := workflow.NewTimer(timerCtx, timeout)
	var timerErr error
	sel.AddFuture(timer, func(f workflow.Future) { timerErr = f.Get(c.ctx, nil) })
	sel.Select(c.ctx)

	switch {
	case received:
		return payload, nil
	case timerErr != nil:
		return nil, fromTemporal(timerErr) // the workflow was cancelled
	default:
		return nil, kernel.ErrTimeout
	}
}

func (c *workflowCtx) Spawn(workflowName string, input []byte) (kernel.Future, error) {
	id := fmt.Sprintf("%s/%d", c.RunID(), c.spawnSeq)
	c.spawnSeq++
	ctx := workflow.WithChildOptions(c.ctx, workflow.ChildWorkflowOptions{WorkflowID: id})
	return &future{id: id, f: workflow.ExecuteChildWorkflow(ctx, workflowName, input)}, nil
}

func (c *workflowCtx) Now() time.Time { return workflow.Now(c.ctx) }

// Rand journals one seed per run (a SideEffect, recorded in history), at the
// first call; later calls reuse the source.
func (c *workflowCtx) Rand() *rand.Rand {
	if c.rng == nil {
		var seed int64
		if err := workflow.SideEffect(c.ctx, func(workflow.Context) any {
			return time.Now().UnixNano()
		}).Get(&seed); err != nil {
			panic(err)
		}
		c.rng = rand.New(rand.NewSource(seed))
	}
	return c.rng
}

type future struct {
	id string
	f  workflow.ChildWorkflowFuture
}

func (f *future) RunID() string { return f.id }

func (f *future) Get(ctx kernel.Ctx) ([]byte, error) {
	c, ok := ctx.(*workflowCtx)
	if !ok {
		return nil, fmt.Errorf("temporalkernel: Future.Get needs a temporalkernel Ctx, got %T", ctx)
	}
	var out []byte
	err := f.f.Get(c.ctx, &out)
	return out, Result(fromTemporal(err))
}

// Kernel sentinel errors cross Temporal as application errors of these types,
// so callers can still use errors.Is after Result.
var sentinels = []struct {
	typ string
	err error
}{
	{"kernel.ErrTimeout", kernel.ErrTimeout},
	{"kernel.ErrNondeterminism", kernel.ErrNondeterminism},
	{"kernel.ErrCanceled", kernel.ErrCanceled},
}

// fromTemporal maps errors seen inside a workflow: cancellation becomes
// kernel.ErrCanceled; anything else is returned as is.
func fromTemporal(err error) error {
	var canceled *temporal.CanceledError
	if errors.As(err, &canceled) {
		return fmt.Errorf("%w: %w", kernel.ErrCanceled, err)
	}
	return err
}

func toWorkflowError(err error) error {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return temporal.NewNonRetryableApplicationError(err.Error(), s.typ, err)
		}
	}
	return err
}

// Result maps a workflow error seen by a client (or by a parent run) back to
// the kernel's sentinel errors.
func Result(err error) error {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		for _, s := range sentinels {
			if app.Type() == s.typ {
				return fmt.Errorf("%w: %w", s.err, err)
			}
		}
	}
	return err
}
