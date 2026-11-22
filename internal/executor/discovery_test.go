package executor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestDiscoveryOffersToolsOnNextTurn(t *testing.T) {
	for _, approved := range []bool{true, false} {
		name := "declined"
		if approved {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			var requests [][]string
			m := modelFunc(func(req model.Request) model.Response {
				var names []string
				for _, spec := range req.Tools {
					names = append(names, spec.Function.Name)
				}
				requests = append(requests, names)
				switch len(requests) {
				case 1:
					return model.Response{ToolCalls: []model.ToolCall{
						toolCall("search", tool.SearchToolsName, `{"query":"schedule a daily reminder"}`),
						toolCall("premature", "schedule", `{}`),
					}}
				case 2:
					return model.Response{ToolCalls: []model.ToolCall{toolCall("allowed", "schedule", `{}`)}}
				default:
					return model.Response{Content: "Finished"}
				}
			})
			var writes int
			schedule := tool.Typed("schedule", "Schedule a task", `{}`, func(context.Context, struct{}) (string, error) {
				writes++
				return "scheduled", nil
			})
			schedule.Effect = tool.Write
			var events []agent.Event
			r := &Runner{WorkflowType: DiscoveryWorkflowName, Model: m, Tools: tool.Registry{
				schedule, tool.WebSearch(""), tool.SearchTools(),
			}, Events: func(_ context.Context, _ string, _ int, e agent.Event) error {
				events = append(events, e)
				return nil
			}}
			r.Limits = DefaultLimits
			r.Limits.ApprovalTimeout = 100 * time.Millisecond
			k := memkernel.New()
			if err := k.Signal(t.Context(), "discovery", ApprovalSignal(5), mustJSON(t, Approval{Approved: approved})); err != nil {
				t.Fatal(err)
			}
			state := runDiscovery(t, k, r, Input{})
			if state.Status != agent.Done || len(requests) != 3 {
				t.Fatalf("run = %s, requests = %v", state.Status, requests)
			}
			if !slices.Equal(requests[0], []string{"web_search", tool.SearchToolsName}) ||
				!slices.Equal(requests[1], []string{"schedule", "web_search", tool.SearchToolsName}) ||
				!slices.Equal(requests[2], requests[1]) {
				t.Fatalf("offered schemas = %v", requests)
			}
			if events[2].Kind != agent.ToolResult || !strings.Contains(events[2].Text, "schedule") ||
				!events[3].IsError || !strings.Contains(events[3].Text, "not offered") ||
				events[5].Kind != agent.ApprovalRequested || events[5].CallID != "allowed" {
				t.Fatalf("unexpected lookup or approval events: %+v", events)
			}
			if writes > 1 || (writes == 1) != approved {
				t.Fatalf("writes = %d, approved = %v", writes, approved)
			}
		})
	}
}

func TestDiscoveryBudgetAndChildProfile(t *testing.T) {
	schedule := tool.Tool{Name: "schedule", Effect: tool.Write}
	r := &Runner{
		WorkflowType: DiscoveryWorkflowName,
		Tools:        tool.Registry{tool.SearchTools(), tool.WebSearch(""), tool.BrowserOpen("", ""), schedule},
		PrivateTools: []string{tool.SearchToolsName},
	}
	r.Model = modelFunc(func(req model.Request) model.Response {
		if req.Messages[len(req.Messages)-1].Role == "tool" {
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "budget used up") {
				t.Error("over-budget lookup was not rejected")
			}
			var count int
			for _, spec := range req.Tools {
				if spec.Function.Name == "browser_open" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("browser_open appears %d times, want 1", count)
			}
			return model.Response{Content: "done"}
		}
		return model.Response{ToolCalls: []model.ToolCall{
			toolCall("a", tool.SearchToolsName, `{"query":""}`),
			toolCall("b", tool.SearchToolsName, `{"query":"unrecognized"}`),
			toolCall("c", tool.SearchToolsName, `{"query":"browser_open"}`),
			toolCall("d", tool.SearchToolsName, `{"query":"browser forms"}`),
			toolCall("e", tool.SearchToolsName, `{"query":"schedule"}`),
		}}
	})
	s := runDiscovery(t, memkernel.New(), r, Input{})
	if s.Status != agent.Done || !s.Messages[2].IsError || !strings.Contains(s.Messages[3].Text, "No matching tools") {
		t.Fatalf("lookup results: %+v", s.Messages)
	}
	r.Model = modelFunc(func(req model.Request) model.Response {
		if len(req.Tools) != 2 || !offered(req.Tools, "browser_open") || !offered(req.Tools, "web_search") {
			t.Errorf("child schemas = %v", req.Tools)
		}
		return model.Response{Content: "child result"}
	})
	if child := runDiscovery(t, memkernel.New(), r, Input{Subagent: true}); child.Status != agent.Done {
		t.Fatalf("child = %+v", child)
	}
}

func runDiscovery(t *testing.T, k *memkernel.Kernel, r *Runner, in Input) agent.State {
	t.Helper()
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	in.Prompt, in.Skills = "test discovery", catalog
	register(k, r)
	out, err := k.Run(t.Context(), "discovery", "agent", mustJSON(t, in))
	if err != nil {
		t.Fatal(err)
	}
	var state agent.State
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
