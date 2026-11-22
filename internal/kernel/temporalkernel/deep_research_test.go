package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
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
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestDeepResearchResumesAndReviewsBeforeReporting(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	var plans, browses, drafts atomic.Int32
	inDraft := make(chan struct{}, 1)
	const source = "https://example.test/study"

	modelClient := fakeModel(func(ctx context.Context, req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		switch {
		case strings.HasPrefix(last.Content, "Decompose the current research request"):
			plans.Add(1)
			return model.Response{Content: `{"question":"Is fog capture useful?","lanes":[{"title":"Primary evidence","prompt":"Find measurements."},{"title":"Contrary evidence","prompt":"Find conflicting measurements."}]}`}, nil
		case strings.HasPrefix(last.Content, "Draft a sourced answer"):
			if strings.Count(last.Content, "Pages opened by this worker") != 4 {
				return model.Response{}, fmt.Errorf("missing initial or follow-up evidence")
			}
			if drafts.Add(1) == 1 {
				inDraft <- struct{}{}
				<-ctx.Done()
				return model.Response{}, ctx.Err()
			}
			return model.Response{Content: "Draft with citation " + source}, nil
		case strings.HasPrefix(last.Content, "Write the final answer"):
			if !strings.Contains(last.Content, "## Citation review") || !strings.Contains(last.Content, "## Independent critique") {
				return model.Response{}, fmt.Errorf("report attempted before both reviews")
			}
			return model.Response{Content: "Reviewed report " + source}, nil
		case last.Role == "tool":
			return model.Response{Content: "Evidence and limitations from " + source}, nil
		default:
			if len(req.Tools) != 1 || req.Tools[0].Function.Name != "browse" {
				return model.Response{}, fmt.Errorf("unexpected deep worker tools: %v", req.Tools)
			}
			return model.Response{ToolCalls: []model.ToolCall{{ID: "open", Type: "function", Function: model.FunctionCall{
				Name: "browse", Arguments: `{"url":"https://example.test/study"}`,
			}}}}, nil
		}
	})
	browser := tool.Typed("browse", "read test source", `{"type":"object"}`, func(_ context.Context, _ struct{}) (string, error) {
		browses.Add(1)
		return "# Study\n" + source + "\n\nWater collection was measured.", nil
	})
	runner := &executor.Runner{Model: modelClient, Tools: tool.Registry{browser}}
	queue := "deep-" + rand.Text()[:8]
	opts := temporalkernel.Options{ActivityTimeout: 10 * time.Second, MaxAttempts: 3, RetryInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second}
	startWorker := func() worker.Worker {
		w := worker.New(dev.Client(), queue, worker.Options{})
		temporalkernel.RegisterWorkflow(w, executor.DeepResearchWorkflowName, runner.DeepResearchWorkflow, opts)
		temporalkernel.RegisterWorkflow(w, executor.DeepResearchWorkerName, runner.Workflow, opts)
		for name, fn := range runner.Activities() {
			temporalkernel.RegisterActivity(w, name, fn)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}

	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(executor.Input{Prompt: "Research fog capture", Skills: catalog})
	if err != nil {
		t.Fatal(err)
	}
	first := startWorker()
	run, err := dev.Client().ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, executor.DeepResearchWorkflowName, input)
	if err != nil {
		first.Stop()
		t.Fatal(err)
	}
	select {
	case <-inDraft:
	case <-time.After(20 * time.Second):
		first.Stop()
		t.Fatal("draft phase did not start")
	}
	first.Stop()
	second := startWorker()
	defer second.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	var raw []byte
	if err := run.Get(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	var state agent.State
	if err := json.Unmarshal(raw, &state); err != nil || state.Status != agent.Done ||
		state.Messages[len(state.Messages)-1].Text != "Reviewed report "+source {
		t.Fatalf("unexpected final state: %s, %v", raw, err)
	}
	if plans.Load() != 1 || drafts.Load() != 2 || browses.Load() != 6 {
		t.Fatalf("unexpected completed work across restart: plan=%d draft=%d browse=%d", plans.Load(), drafts.Load(), browses.Load())
	}
}
