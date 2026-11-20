package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"nomanqureshi.dev/hpyo-agent/internal/kernel"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/conformance"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
)

var (
	dev    *testsuite.DevServer
	devErr error
	quiet  = tlog.NewStructuredLogger(slog.New(slog.DiscardHandler))
)

// One dev server per test binary. StartDevServer downloads the Temporal CLI
// on first use and caches it.
func TestMain(m *testing.M) {
	dev, devErr = testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{
		ClientOptions: &client.Options{Logger: quiet},
		LogLevel:      "error",
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	})
	code := m.Run()
	if dev != nil {
		_ = dev.Stop()
	}
	os.Exit(code)
}

func TestConformance(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	conformance.Run(t, func(t *testing.T) conformance.Harness { return newHarness(t) })
}

// harness runs the conformance suite on a real Temporal dev server.
type harness struct {
	c      client.Client
	queue  string
	prefix string // the suite reuses run IDs like "r1"; workflow IDs are global
	opts   temporalkernel.Options

	mu         sync.Mutex
	workflows  map[string]temporalkernel.Workflow
	activities map[string]kernel.ActivityFunc
	w          worker.Worker
	started    map[string]bool
	pending    map[string][][2][]byte // signals sent before the run started: name, payload
}

func newHarness(t *testing.T) *harness {
	id := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + rand.Text()[:6]
	h := &harness{
		c:      dev.Client(),
		queue:  id,
		prefix: id + "-",
		// Short timeouts so a killed worker's activity is retried quickly.
		opts:       temporalkernel.Options{ActivityTimeout: 3 * time.Second, MaxAttempts: 3, RetryInterval: 100 * time.Millisecond},
		workflows:  map[string]temporalkernel.Workflow{},
		activities: map[string]kernel.ActivityFunc{},
		started:    map[string]bool{},
		pending:    map[string][][2][]byte{},
	}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.w != nil {
			h.w.Stop()
		}
	})
	return h
}

func (h *harness) Register(name string, wf conformance.Workflow) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workflows[name] = wf
}

func (h *harness) RegisterActivity(name string, fn kernel.ActivityFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activities[name] = fn
}

// ensureWorker starts a worker with everything registered so far. Callers hold mu.
func (h *harness) ensureWorker() error {
	if h.w != nil {
		return nil
	}
	w := worker.New(h.c, h.queue, worker.Options{})
	for name, wf := range h.workflows {
		temporalkernel.RegisterWorkflow(w, name, wf, h.opts)
	}
	for name, fn := range h.activities {
		temporalkernel.RegisterActivity(w, name, fn)
	}
	if err := w.Start(); err != nil {
		return err
	}
	h.w = w
	return nil
}

// Restart stops the worker (cancelling activities in flight) and starts a
// fresh one: the closest thing to a worker process dying and coming back.
func (h *harness) Restart() {
	h.mu.Lock()
	old := h.w
	h.w = nil
	h.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.ensureWorker(); err != nil {
		panic(err)
	}
}

func (h *harness) Run(ctx context.Context, runID, workflowName string, input []byte) ([]byte, error) {
	id := h.prefix + runID
	h.mu.Lock()
	if err := h.ensureWorker(); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	h.mu.Unlock()

	desc, err := h.c.DescribeWorkflowExecution(ctx, id, "")
	var notFound *serviceerror.NotFound
	switch {
	case errors.As(err, &notFound):
		return h.start(ctx, id, workflowName, input)
	case err != nil:
		return nil, err
	}

	info := desc.GetWorkflowExecutionInfo()
	if info.GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return result(ctx, h.c.GetWorkflow(ctx, id, ""))
	}
	return h.replay(ctx, id, info.GetExecution().GetRunId(), info.GetType().GetName(), workflowName)
}

// replay re-runs a finished run: the current code for workflowName executes
// against the recorded history (that is what detects a changed step), and its
// own return value is the result, exactly like memkernel's re-run. Returning
// the recorded result instead would hide a non-journaled Now or Rand, because
// Temporal's replay compares commands, not workflow results.
func (h *harness) replay(ctx context.Context, id, runID, recordedType, workflowName string) ([]byte, error) {
	h.mu.Lock()
	wf, ok := h.workflows[workflowName]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("workflow %q not registered", workflowName)
	}
	var out []byte
	var wfErr error
	finished := false
	capture := func(c kernel.Ctx, in []byte) ([]byte, error) {
		out, wfErr = wf(c, in)
		finished = true
		return out, wfErr
	}
	replayer := worker.NewWorkflowReplayer()
	temporalkernel.RegisterWorkflow(replayer, recordedType, capture, h.opts)
	if err := replayer.ReplayWorkflowExecution(ctx, h.c.WorkflowService(), quiet, "default",
		workflow.Execution{ID: id, RunID: runID}); err != nil {
		return nil, fmt.Errorf("%w: %v", kernel.ErrNondeterminism, err)
	}
	if !finished {
		return nil, fmt.Errorf("replay of %s did not reach the end of the workflow", id)
	}
	return out, wfErr
}

func (h *harness) start(ctx context.Context, id, workflowName string, input []byte) ([]byte, error) {
	// Held while starting, so a concurrent Signal either queues before the
	// start (and is delivered with it) or is sent after it exists.
	h.mu.Lock()
	opts := client.StartWorkflowOptions{ID: id, TaskQueue: h.queue}
	var run client.WorkflowRun
	var err error
	if sigs := h.pending[id]; len(sigs) > 0 {
		run, err = h.c.SignalWithStartWorkflow(ctx, id, string(sigs[0][0]), sigs[0][1], opts, workflowName, input)
		for _, s := range sigs[1:] {
			if err == nil {
				err = h.c.SignalWorkflow(ctx, id, "", string(s[0]), s[1])
			}
		}
		delete(h.pending, id)
	} else {
		run, err = h.c.ExecuteWorkflow(ctx, opts, workflowName, input)
	}
	if err == nil {
		h.started[id] = true
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return result(ctx, run)
}

func (h *harness) Signal(ctx context.Context, runID, name string, payload []byte) error {
	id := h.prefix + runID
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started[id] {
		h.pending[id] = append(h.pending[id], [2][]byte{[]byte(name), payload})
		return nil
	}
	return h.c.SignalWorkflow(ctx, id, "", name, payload)
}

func result(ctx context.Context, run client.WorkflowRun) ([]byte, error) {
	var out []byte
	err := run.Get(ctx, &out)
	return out, temporalkernel.Result(err)
}
