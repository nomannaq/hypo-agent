package executor

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestOrchestratorYieldsAndRunsInvestigationsInOrder(t *testing.T) {
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	var yielded atomic.Bool
	r := &Runner{
		WorkflowType: OrchestratorWorkflowName,
		Tools:        tool.Registry{tool.SearchTools(), tool.Research()},
		PrivateTools: []string{tool.SearchToolsName},
		YieldResearch: func(_ context.Context, id string) (bool, error) {
			if id != "root" {
				t.Errorf("yield run = %q", id)
			}
			yielded.Store(true)
			return true, nil
		},
		Model: modelFunc(func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			switch {
			case strings.HasPrefix(last.Content, "alpha"):
				if !yielded.Load() {
					t.Error("child started before the chat slot was released")
				}
				started <- "alpha"
				<-unblock
				return model.Response{Content: "Alpha finding"}
			case strings.HasPrefix(last.Content, "beta"):
				started <- "beta"
				return model.Response{Content: "Beta finding"}
			case last.ToolCallID == "research":
				if len(req.Tools) != 0 {
					t.Errorf("tools offered to a yielded run: %v", req.Tools)
				}
				return model.Response{}
			case last.ToolCallID == "lookup":
				return model.Response{ToolCalls: []model.ToolCall{toolCall("research", tool.ResearchName,
					`{"tasks":[{"title":"A","prompt":"alpha"},{"title":"B","prompt":"beta"}]}`)}}
			default:
				return model.Response{ToolCalls: []model.ToolCall{toolCall("lookup", tool.SearchToolsName, `{"query":"research"}`)}}
			}
		}),
	}
	k := memkernel.New()
	k.Register(OrchestratorWorkflowName, r.Workflow)
	for name, fn := range r.Activities() {
		k.RegisterActivity(name, fn)
	}
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input := mustJSON(t, Input{Prompt: "Investigate", Skills: catalog})
	go func() {
		body, err := k.Run(ctx, "root", OrchestratorWorkflowName, input)
		done <- result{body, err}
	}()
	awaitChild := func(want string) {
		t.Helper()
		select {
		case name := <-started:
			if name != want {
				t.Fatalf("started %s, want %s", name, want)
			}
		case <-ctx.Done():
			t.Fatalf("child %s did not start", want)
		}
	}
	awaitChild("alpha")
	select {
	case child := <-started:
		t.Fatalf("child %s started before alpha completed", child)
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	awaitChild("beta")
	select {
	case result := <-done:
		var state agent.State
		if result.err != nil || json.Unmarshal(result.body, &state) != nil || state.Status != agent.Done ||
			state.Messages[len(state.Messages)-1].Text != "## A\nAlpha finding\n\n## B\nBeta finding" {
			t.Fatalf("research result = %+v, %v", state, result.err)
		}
	case <-ctx.Done():
		t.Fatal("orchestrator did not finish")
	}
}

func TestOrchestratorAnswersResearchStatusWithoutModel(t *testing.T) {
	status := "Your background research is still running."
	r := &Runner{
		WorkflowType: ConversationalWorkflowName,
		Model: modelFunc(func(model.Request) model.Response {
			t.Error("a scheduler status reply reached the model")
			return model.Response{}
		}),
	}
	s := runDiscovery(t, memkernel.New(), r, Input{ResearchStatusReply: status})
	if s.Status != agent.Done || s.Messages[len(s.Messages)-1].Text != status {
		t.Fatalf("status reply = %+v", s)
	}
}

func TestResponsiveWorkflowBoundsForegroundWebReads(t *testing.T) {
	for _, backgroundAvailable := range []bool{true, false} {
		t.Run(map[bool]string{true: "background available", false: "background busy"}[backgroundAvailable], func(t *testing.T) {
			reads, handoffs, modelCalls := 0, 0, 0
			web := tool.Tool{
				Name: "browse", Description: "Read a page", Params: []byte(`{"type":"object"}`),
				Run: func(context.Context, json.RawMessage) (string, error) {
					reads++
					return "Source evidence", nil
				},
			}
			r := &Runner{
				WorkflowType: ResponsiveWorkflowName,
				Tools:        tool.Registry{web},
				YieldResearch: func(context.Context, string) (bool, error) {
					handoffs++
					if reads != 3 {
						t.Errorf("handoff after %d reads; want 3", reads)
					}
					return backgroundAvailable, nil
				},
				Model: modelFunc(func(req model.Request) model.Response {
					modelCalls++
					if modelCalls == 2 {
						if (len(req.Tools) > 0) != backgroundAvailable {
							t.Errorf("second model request offered %d tools, background available=%t", len(req.Tools), backgroundAvailable)
						}
						return model.Response{Content: "Evidence summary."}
					}
					return model.Response{ToolCalls: []model.ToolCall{
						toolCall("1", "browse", `{}`), toolCall("2", "browse", `{}`),
						toolCall("3", "browse", `{}`), toolCall("4", "browse", `{}`),
					}}
				}),
			}
			state := runDiscovery(t, memkernel.New(), r, Input{})
			wantReads := 3
			if backgroundAvailable {
				wantReads++
			}
			if state.Status != agent.Done || handoffs != 1 || reads != wantReads ||
				state.Messages[len(state.Messages)-1].Text != "Evidence summary." {
				t.Fatalf("status %s, handoffs %d, reads %d (want %d)", state.Status, handoffs, reads, wantReads)
			}
		})
	}
}

func TestOrchestratorSeesSupervisorProgress(t *testing.T) {
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	const progress = `- "Collect evidence": running in the background for 4m0s. Deep research stage: investigate.`
	var system string
	r := &Runner{
		WorkflowType: ResponsiveWorkflowName,
		Tools:        tool.Registry{tool.SearchTools()},
		PrivateTools: []string{tool.SearchToolsName},
		Model: modelFunc(func(req model.Request) model.Response {
			system = req.Messages[0].Content
			return model.Response{Content: "It's in the investigate stage."}
		}),
	}
	k := memkernel.New()
	k.Register(ResponsiveWorkflowName, r.Workflow)
	for name, fn := range r.Activities() {
		k.RegisterActivity(name, fn)
	}
	input, _ := json.Marshal(Input{Workflow: ResponsiveWorkflowName, Prompt: "what is the progress?", Skills: catalog, Progress: progress})
	if _, err := k.Run(t.Context(), "root", ResponsiveWorkflowName, input); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "You supervise the other work in this chat") || !strings.Contains(system, progress) {
		t.Fatalf("system prompt lacks the progress view:\n%s", system)
	}
}
