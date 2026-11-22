package temporalkernel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
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

func TestSkillsPinCatalogAcrossWorkerRestart(t *testing.T) {
	if devErr != nil {
		t.Skipf("Temporal dev server unavailable: %v", devErr)
	}
	old, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	const newBody = "Updated research procedure."
	newCatalog, err := skills.Load(fstest.MapFS{
		"hypothesis-research/SKILL.md": {Data: []byte("---\nname: hypothesis-research\ndescription: Updated research skill.\n---\n" + newBody)},
	})
	if err != nil || old.Digest == newCatalog.Digest {
		t.Fatalf("catalog revision: %v", err)
	}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "fog" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"results":[{"title":"Fog study","url":"https://example.test/study","content":"Measured water capture."}]}`)
	}))
	defer search.Close()
	call := func(id, name, args string) (model.Response, error) {
		return model.Response{ToolCalls: []model.ToolCall{{ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}}}, nil
	}
	fake := fakeModel(func(_ context.Context, req model.Request) (model.Response, error) {
		msgs := req.Messages
		system, prompt, last := msgs[0].Content, msgs[1].Content, msgs[len(msgs)-1]
		marker := "# Hypothesis research"
		description := old.Entries[0].Description
		if strings.Contains(prompt, "new catalog") {
			marker = newBody
			description = newCatalog.Entries[0].Description
		}
		if !strings.Contains(system, description) || strings.Contains(system, marker) {
			t.Errorf("expected only the pinned skill metadata in %q", system)
		}
		switch last.ToolCallID {
		case "load":
			if !strings.Contains(last.Content, marker) {
				t.Errorf("skill read did not use pinned content %q: %q", marker, last.Content)
			}
			if strings.Contains(prompt, "research subagent") {
				return call("search", "web_search", `{"query":"fog"}`)
			}
			task, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"title": "Evidence", "prompt": prompt}}})
			return call("research", "research", string(task))
		case "search", "research":
			return model.Response{Content: last.Content}, nil
		default:
			return call("load", skills.ReadToolName, `{"name":"hypothesis-research"}`)
		}
	})

	queue := "skills-" + rand.Text()[:8]
	inFlight := make(chan struct{}, 1)
	startWorker := func(read tool.Tool) worker.Worker {
		r := &executor.Runner{
			WorkflowType: executor.SkillsWorkflowName,
			Model:        fake,
			Tools:        tool.Registry{read, tool.Research(), tool.WebSearch(tool.Searcher{SearXNG: search.URL})},
		}
		w := worker.New(dev.Client(), queue, worker.Options{})
		opts := temporalkernel.Options{ActivityTimeout: 10 * time.Second, MaxAttempts: 3, RetryInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second}
		temporalkernel.RegisterWorkflow(w, executor.SkillsWorkflowName, r.Workflow, opts)
		for name, fn := range r.Activities() {
			temporalkernel.RegisterActivity(w, name, fn)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	begin := func(suffix string, catalog *skills.Catalog) client.WorkflowRun {
		input, err := json.Marshal(executor.Input{Skills: catalog, Prompt: suffix + " catalog: does fog improve water capture?"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := dev.Client().ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: queue + "-" + suffix, TaskQueue: queue}, executor.SkillsWorkflowName, input)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	read := skills.ReadTool()
	firstRead := read
	firstRead.Run = func(ctx context.Context, _ json.RawMessage) (string, error) {
		inFlight <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	first := startWorker(firstRead)
	oldRun := begin("old", old)
	select {
	case <-inFlight:
	case <-time.After(15 * time.Second):
		first.Stop()
		t.Fatal("read_skill did not start")
	}
	first.Stop()

	second := startWorker(read)
	defer second.Stop()
	newRun := begin("new", newCatalog)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, run := range []client.WorkflowRun{oldRun, newRun} {
		var out []byte
		if err := run.Get(ctx, &out); err != nil {
			t.Fatal(err)
		}
		var state agent.State
		if err := json.Unmarshal(out, &state); err != nil || state.Status != agent.Done || !strings.Contains(state.Messages[len(state.Messages)-1].Text, "https://example.test/study") {
			t.Fatalf("research did not finish: %s, %v", out, err)
		}
	}
}
