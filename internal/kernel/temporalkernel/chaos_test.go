package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

type fakeModel func(context.Context, model.Request) (model.Response, error)

func (f fakeModel) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return f(ctx, req)
}

type fakeTools func(context.Context, string, json.RawMessage) (string, error)

func (fakeTools) Specs() []model.Tool {
	return []model.Tool{{Type: "function", Function: model.ToolFunction{Name: "search"}}}
}

func (fakeTools) Effect(string) tool.Effect { return tool.Read }

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

type writeTools struct{ fakeTools }

func (writeTools) Effect(string) tool.Effect { return tool.Write }

// A Write tool call waits on Temporal for the approval signal, sent the way
// the server sends it (a JSON executor.Approval as raw bytes, named after
// the approval_requested event's seq), and runs once approved.
func TestApprovalSignalOnTemporal(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	var toolCalls atomic.Int32
	requested := make(chan int, 1)
	r := &executor.Runner{
		Model: fakeModel(func(_ context.Context, req model.Request) (model.Response, error) {
			for _, m := range req.Messages {
				if m.Role == "tool" {
					return model.Response{Content: "sent"}, nil
				}
			}
			return model.Response{ToolCalls: []model.ToolCall{{
				ID: "c1", Type: "function", Function: model.FunctionCall{Name: "search", Arguments: `{}`},
			}}}, nil
		}),
		Tools: writeTools{fakeTools(func(context.Context, string, json.RawMessage) (string, error) {
			toolCalls.Add(1)
			return "ok", nil
		})},
		Events: func(_ context.Context, _ string, seq int, e agent.Event) error {
			if e.Kind == agent.ApprovalRequested {
				select {
				case requested <- seq:
				default:
				}
			}
			return nil
		},
	}
	queue := "approval-" + rand.Text()[:8]
	w := worker.New(dev.Client(), queue, worker.Options{})
	temporalkernel.RegisterWorkflow(w, executor.WorkflowName, r.Workflow, temporalkernel.Options{})
	for name, fn := range r.Activities() {
		temporalkernel.RegisterActivity(w, name, fn)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	input, _ := json.Marshal(executor.Input{Prompt: "send it"})
	run, err := dev.Client().ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, executor.WorkflowName, input)
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	select {
	case seq = <-requested:
	case <-time.After(30 * time.Second):
		t.Fatal("no approval was requested")
	}
	if toolCalls.Load() != 0 {
		t.Fatal("the tool ran before it was approved")
	}
	payload, _ := json.Marshal(executor.Approval{Approved: true})
	if err := dev.Client().SignalWorkflow(t.Context(), queue, "", executor.ApprovalSignal(seq), payload); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var out []byte
	if err := temporalkernel.Result(run.Get(ctx, &out)); err != nil {
		t.Fatal(err)
	}
	var s agent.State
	_ = json.Unmarshal(out, &s)
	if s.Status != agent.Done || toolCalls.Load() != 1 {
		t.Fatalf("status %s, tool calls %d", s.Status, toolCalls.Load())
	}
}

// research starts its subagents as Temporal child workflows (the same agent
// workflow, run as subagents) and collects their answers.
func TestResearchSubagentsOnTemporal(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	r := &executor.Runner{
		Model: fakeModel(func(_ context.Context, req model.Request) (model.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			switch {
			case strings.Contains(last.Content, "research subagent"):
				topic, _, _ := strings.Cut(last.Content, "\n")
				return model.Response{Content: "found " + topic}, nil
			case last.Role == "tool":
				return model.Response{Content: last.Content}, nil
			}
			return model.Response{ToolCalls: []model.ToolCall{{ID: "r1", Type: "function", Function: model.FunctionCall{
				Name: tool.ResearchName, Arguments: `{"tasks":[{"title":"A","prompt":"x"},{"title":"B","prompt":"y"}]}`,
			}}}}, nil
		}),
		Tools: tool.Registry{tool.Research()},
	}
	queue := "research-" + rand.Text()[:8]
	w := worker.New(dev.Client(), queue, worker.Options{})
	temporalkernel.RegisterWorkflow(w, executor.WorkflowName, r.Workflow, temporalkernel.Options{})
	for name, fn := range r.Activities() {
		temporalkernel.RegisterActivity(w, name, fn)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	input, _ := json.Marshal(executor.Input{Prompt: "compare"})
	run, err := dev.Client().ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, executor.WorkflowName, input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var out []byte
	if err := temporalkernel.Result(run.Get(ctx, &out)); err != nil {
		t.Fatal(err)
	}
	var s agent.State
	_ = json.Unmarshal(out, &s)
	if got, want := s.Messages[len(s.Messages)-1].Text, "## A\nfound x\n\n## B\nfound y"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
