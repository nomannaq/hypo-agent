package executor

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestDeepResearchRecoversBlankWriting(t *testing.T) {
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	const source = "https://example.test/study"
	for _, tc := range []struct {
		name       string
		retryWorks bool
		want       string
	}{
		{"retry produces report", true, "Final report from " + source},
		{"repeated blanks preserve evidence", false, "# Research notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var browses, writes, retries atomic.Int32
			var draftSize, reportSize int
			var notices []string
			m := modelFunc(func(req model.Request) model.Response {
				last := req.Messages[len(req.Messages)-1]
				filled := func(text string) model.Response { return model.Response{Content: text} }
				blank := model.Response{Usage: model.Usage{CompletionTokens: 8192}}
				switch {
				case strings.HasPrefix(last.Content, "Decompose the current research request"):
					return filled(`{"lanes":[{"title":"Evidence","prompt":"Find measurements."},{"title":"Counterevidence","prompt":"Find conflicting data."}]}`)
				case strings.HasPrefix(last.Content, "Draft a sourced answer"):
					writes.Add(1)
					draftSize = len(last.Content)
					if req.MaxTokens != 16384 {
						t.Errorf("draft token limit = %d", req.MaxTokens)
					}
					return blank
				case strings.HasPrefix(last.Content, "Write the final answer"):
					writes.Add(1)
					reportSize = len(last.Content)
					if req.MaxTokens != 16384 || !strings.Contains(last.Content, "## Citation review") {
						t.Errorf("final report did not receive its token limit and review")
					}
					return blank
				case strings.Contains(last.Content, "Write a concise sourced draft"):
					retries.Add(1)
					if len(last.Content) >= draftSize || len(req.Messages) != 2 || !strings.Contains(last.Content, source) {
						t.Errorf("draft retry did not compact the prompt while keeping sources")
					}
					if tc.retryWorks {
						return filled("Draft from " + source)
					}
					return blank
				case strings.Contains(last.Content, "Write a concise final report"):
					retries.Add(1)
					if len(last.Content) >= reportSize || !strings.Contains(last.Content, "## Citation review") {
						t.Errorf("report retry did not compact the prompt while retaining reviews")
					}
					if tc.retryWorks {
						return filled("Final report from " + source)
					}
					return blank
				case last.Role == "tool":
					return filled(strings.Repeat("Measurements require careful interpretation. ", 70) + source)
				default:
					return model.Response{ToolCalls: []model.ToolCall{toolCall("open", "browse", `{"url":"https://example.test/study"}`)}}
				}
			})
			browser := tool.Typed("browse", "read test source", `{"type":"object"}`, func(context.Context, struct{}) (string, error) {
				browses.Add(1)
				return "# Measurement study\n" + source + "\n\nData collected.", nil
			})
			r := &Runner{Model: m, Tools: tool.Registry{browser}, Events: func(_ context.Context, _ string, _ int, e agent.Event) error {
				if e.Kind == agent.ToolResult {
					notices = append(notices, e.Text)
				}
				return nil
			}}
			k := memkernel.New()
			k.Register(DeepResearchWorkflowV2, r.DeepResearchWorkflow)
			k.Register(DeepResearchWorkerName, r.Workflow)
			for name, fn := range r.Activities() {
				k.RegisterActivity(name, fn)
			}
			input, err := json.Marshal(Input{
				Workflow: DeepResearchWorkflowV2, Prompt: "Assess the claim.", Skills: catalog,
				History: []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("Prior unrelated context. ", 300)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				output, err := k.Run(t.Context(), "deep", DeepResearchWorkflowV2, input)
				if err != nil {
					t.Fatal(err)
				}
				var state agent.State
				if err := json.Unmarshal(output, &state); err != nil || state.Status != agent.Done {
					t.Fatalf("deep run did not finish: %s, %v", output, err)
				}
				final := state.Messages[len(state.Messages)-1].Text
				if !strings.HasPrefix(final, tc.want) || !strings.Contains(final, source) {
					t.Fatalf("unexpected final response: %q", final)
				}
				if !tc.retryWorks && !strings.Contains(final, "## Citation review") {
					t.Fatal("fallback lost the reviews")
				}
			}
			if browses.Load() != 6 || writes.Load() != 2 || retries.Load() != 2 {
				t.Fatalf("completed work repeated: browse=%d write=%d retry=%d", browses.Load(), writes.Load(), retries.Load())
			}
			if !tc.retryWorks && !strings.Contains(strings.Join(notices, "\n"), "Continued to independent review") {
				t.Fatal("the fallback was not reported in the activity trace")
			}
		})
	}
}
