package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"reflect"
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
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestDiscoveryReplaysOfferedSchemasAfterWorkerLoss(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	var firstCalls, followups, lookupEvents atomic.Int32
	firstFollowup := make(chan []model.Tool, 1)
	retriedFollowup := make(chan []model.Tool, 1)
	resumedRequest := make(chan []model.Tool, 1)
	snapshot := tool.Typed("browser_snapshot", "Inspect the page", `{}`, func(context.Context, struct{}) (string, error) {
		return "Recovered snapshot", nil
	})
	r := &executor.Runner{
		WorkflowType: executor.DiscoveryWorkflowName,
		Tools:        tool.Registry{tool.SearchTools(), tool.BrowserOpen("", ""), snapshot, tool.WebSearch("")},
		Model: fakeModel(func(ctx context.Context, req model.Request) (model.Response, error) {
			switch last := req.Messages[len(req.Messages)-1]; last.ToolCallID {
			case "snapshot":
				resumedRequest <- req.Tools
				return model.Response{Content: last.Content}, nil
			case "lookup":
				if followups.Add(1) == 1 {
					firstFollowup <- req.Tools
					<-ctx.Done()
					return model.Response{}, ctx.Err()
				}
				retriedFollowup <- req.Tools
				return model.Response{ToolCalls: []model.ToolCall{{
					ID: "snapshot", Type: "function",
					Function: model.FunctionCall{Name: "browser_snapshot", Arguments: `{}`},
				}}}, nil
			default:
				firstCalls.Add(1)
				return model.Response{ToolCalls: []model.ToolCall{{
					ID: "lookup", Type: "function",
					Function: model.FunctionCall{Name: tool.SearchToolsName, Arguments: `{"query":"fill a form"}`},
				}}}, nil
			}
		}),
		Events: func(_ context.Context, _ string, _ int, e agent.Event) error {
			if e.Kind == agent.ToolResult && e.CallID == "lookup" {
				lookupEvents.Add(1)
			}
			return nil
		},
	}
	queue := "discovery-" + rand.Text()[:8]
	opts := temporalkernel.Options{ActivityTimeout: 10 * time.Second, MaxAttempts: 3, RetryInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second}
	startWorker := func() worker.Worker {
		w := worker.New(dev.Client(), queue, worker.Options{})
		temporalkernel.RegisterWorkflow(w, executor.DiscoveryWorkflowName, r.Workflow, opts)
		for name, fn := range r.Activities() {
			temporalkernel.RegisterActivity(w, name, fn)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := startWorker()
	defer func() { w.Stop() }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	input, err := json.Marshal(executor.Input{Prompt: "Fill a form", Skills: catalog})
	if err != nil {
		t.Fatal(err)
	}
	run, err := dev.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, executor.DiscoveryWorkflowName, input)
	if err != nil {
		t.Fatal(err)
	}
	var first []model.Tool
	select {
	case first = <-firstFollowup:
	case <-ctx.Done():
		t.Fatal("first follow-up did not start")
	}
	w.Stop()
	w = startWorker()
	var raw []byte
	if err := run.Get(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	var state agent.State
	if err := json.Unmarshal(raw, &state); err != nil || state.Status != agent.Done ||
		state.Messages[len(state.Messages)-1].Text != "Recovered snapshot" {
		t.Fatalf("resumed workflow result = %+v, %v", state, err)
	}
	select {
	case retry := <-retriedFollowup:
		if want := r.Tools.Specs(); !reflect.DeepEqual(first, want) || !reflect.DeepEqual(retry, want) {
			t.Fatalf("ordered schemas changed: initial %v, retry %v, want %v", first, retry, want)
		}
	default:
		t.Fatal("follow-up did not retry")
	}
	select {
	case offered := <-resumedRequest:
		if !reflect.DeepEqual(offered, first) {
			t.Fatalf("new request after replay offered %v, want %v", offered, first)
		}
	default:
		t.Fatal("resumed worker did not send a new model request")
	}
	if firstCalls.Load() != 1 || lookupEvents.Load() != 1 {
		t.Fatalf("first model calls = %d, lookup events = %d", firstCalls.Load(), lookupEvents.Load())
	}
}
