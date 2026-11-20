package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
)

type fakeModel func(context.Context, model.Request) (model.Response, error)

func (f fakeModel) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return f(ctx, req)
}

type fakeTools func(context.Context, string, json.RawMessage) (string, error)

func (fakeTools) Specs() []model.Tool {
	return []model.Tool{{Type: "function", Function: model.ToolFunction{Name: "search"}}}
}

func (f fakeTools) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return f(ctx, name, args)
}

// The agent loop on Temporal loses its worker while a tool call is in flight.
// A new worker must finish the run without calling the model again for the
// turn that already completed.
func TestChaosRunSurvivesWorkerLoss(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	var modelCalls, toolCalls atomic.Int32
	inFlight := make(chan struct{}, 1)

	r := &executor.Runner{
		Model: fakeModel(func(_ context.Context, req model.Request) (model.Response, error) {
			modelCalls.Add(1)
			for _, m := range req.Messages {
				if m.Role == "tool" {
					return model.Response{Content: "two studies found"}, nil
				}
			}
			return model.Response{ToolCalls: []model.ToolCall{{
				ID: "c1", Type: "function",
				Function: model.FunctionCall{Name: "search", Arguments: `{"q":"caffeine"}`},
			}}}, nil
		}),
		Tools: fakeTools(func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			if toolCalls.Add(1) == 1 {
				inFlight <- struct{}{}
				<-ctx.Done() // the worker goes away mid-call
				return "", ctx.Err()
			}
			return "2 RCTs", nil
		}),
	}

	queue := "chaos-" + rand.Text()[:8]
	opts := temporalkernel.Options{ActivityTimeout: 10 * time.Second, MaxAttempts: 3, RetryInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second}
	startWorker := func() worker.Worker {
		w := worker.New(dev.Client(), queue, worker.Options{})
		temporalkernel.RegisterWorkflow(w, executor.WorkflowName, r.Workflow, opts)
		for name, fn := range r.Activities() {
			temporalkernel.RegisterActivity(w, name, fn)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}

	a := startWorker()
	input, _ := json.Marshal(executor.Input{Prompt: "Does caffeine help memory?"})
	run, err := dev.Client().ExecuteWorkflow(t.Context(),
		client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, executor.WorkflowName, input)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-inFlight:
	case <-time.After(30 * time.Second):
		t.Fatal("tool call never started")
	}
	a.Stop()
	b := startWorker()
	defer b.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var out []byte
	if err := temporalkernel.Result(run.Get(ctx, &out)); err != nil {
		t.Fatal(err)
	}
	var s agent.State
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status != agent.Done || s.Messages[len(s.Messages)-1].Text != "two studies found" {
		t.Fatalf("run ended %s (%s): %+v", s.Status, s.Err, s.Messages)
	}
	if modelCalls.Load() != 2 {
		t.Errorf("model called %d times, want 2: turn 0 must not repeat after the worker loss", modelCalls.Load())
	}
	if toolCalls.Load() != 2 {
		t.Errorf("tool called %d times, want 2 (interrupted attempt + retry)", toolCalls.Load())
	}
}
