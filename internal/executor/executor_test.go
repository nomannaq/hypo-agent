package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
)

// scripted returns its responses in order and counts calls.
type scripted struct {
	replies []model.Response
	calls   atomic.Int32
}

func (m *scripted) Complete(_ context.Context, req model.Request) (model.Response, error) {
	i := int(m.calls.Add(1)) - 1
	if i >= len(m.replies) {
		return m.replies[len(m.replies)-1], nil
	}
	return m.replies[i], nil
}

type fakeTools struct {
	calls atomic.Int32
	call  func(ctx context.Context, name string, args json.RawMessage) (string, error)
}

func (f *fakeTools) Specs() []model.Tool {
	return []model.Tool{{Type: "function", Function: model.ToolFunction{Name: "search"}}}
}

func (f *fakeTools) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	f.calls.Add(1)
	return f.call(ctx, name, args)
}

func toolCall(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
}

func run(t *testing.T, k *memkernel.Kernel, r *Runner, runID, prompt string) (agent.State, error) {
	t.Helper()
	k.Register("agent", r.Workflow)
	in, _ := json.Marshal(Input{Prompt: prompt})
	out, err := k.Run(t.Context(), runID, "agent", in)
	if err != nil {
		return agent.State{}, err
	}
	var s agent.State
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	return s, nil
}

func TestRunToolRoundTrip(t *testing.T) {
	m := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "search", `{"q":"caffeine"}`)}, Usage: model.Usage{PromptTokens: 10}},
		{Content: "Caffeine helps a little.", Usage: model.Usage{PromptTokens: 30}},
	}}
	tools := &fakeTools{call: func(_ context.Context, name string, args json.RawMessage) (string, error) {
		return "found 2 studies for " + string(args), nil
	}}
	s, err := run(t, memkernel.New(), &Runner{Model: m, Tools: tools}, "r1", "Does caffeine help memory?")
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != agent.Done || s.Turns != 2 || s.Usage.PromptTokens != 40 {
		t.Fatalf("status %s, turns %d, usage %+v", s.Status, s.Turns, s.Usage)
	}
	roles := ""
	for _, msg := range s.Messages {
		roles += string(msg.Role) + " "
	}
	if roles != "user assistant tool assistant " {
		t.Fatalf("transcript roles: %s", roles)
	}
	if got := s.Messages[2].Text; got != `found 2 studies for {"q":"caffeine"}` {
		t.Errorf("tool result %q", got)
	}
}

// The worker dies during the tool call. Restarting the run must not call the
// model again for the turn that already completed.
func TestRunResumesAfterCrashWithoutRepeatingSteps(t *testing.T) {
	m := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "search", `{}`)}},
		{Content: "done"},
	}}
	crashed := false
	ctx, cancel := context.WithCancel(t.Context())
	tools := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) {
		if !crashed {
			crashed = true
			cancel()
			return "", context.Canceled
		}
		return "ok", nil
	}}
	k := memkernel.New()
	r := &Runner{Model: m, Tools: tools}
	k.Register("agent", r.Workflow)
	in, _ := json.Marshal(Input{Prompt: "go"})

	if _, err := k.Run(ctx, "r1", "agent", in); !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed run: err = %v, want context.Canceled", err)
	}
	out, err := k.Run(t.Context(), "r1", "agent", in)
	if err != nil {
		t.Fatal(err)
	}
	var s agent.State
	_ = json.Unmarshal(out, &s)
	if s.Status != agent.Done {
		t.Fatalf("resumed run status %s (%s)", s.Status, s.Err)
	}
	if m.calls.Load() != 2 {
		t.Errorf("model called %d times across crash + resume, want 2 (no repeat of turn 0)", m.calls.Load())
	}
	if tools.calls.Load() != 2 {
		t.Errorf("tool called %d times, want 2 (crashed attempt + retry)", tools.calls.Load())
	}
}

func TestRunGuards(t *testing.T) {
	loop := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "search", `{}`)}, Usage: model.Usage{PromptTokens: 100}},
	}}
	ok := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil }}

	t.Run("turn limit", func(t *testing.T) {
		s, err := run(t, memkernel.New(), &Runner{Model: loop, Tools: ok, Limits: Limits{MaxTurns: 3, MaxToolCalls: 8, MaxToolOutput: 100, StepTimeout: DefaultLimits.StepTimeout}}, "r1", "loop")
		if err != nil {
			t.Fatal(err)
		}
		if s.Status != agent.Stopped || s.Turns != 3 || !strings.Contains(s.Err, "turn limit") {
			t.Fatalf("status %s, turns %d, err %q", s.Status, s.Turns, s.Err)
		}
	})

	t.Run("token budget", func(t *testing.T) {
		lim := DefaultLimits
		lim.MaxTokens = 250
		s, _ := run(t, memkernel.New(), &Runner{Model: loop, Tools: ok, Limits: lim}, "r1", "loop")
		if s.Status != agent.Stopped || !strings.Contains(s.Err, "token budget") || s.Turns != 3 {
			t.Fatalf("status %s, turns %d, err %q", s.Status, s.Turns, s.Err)
		}
	})

	t.Run("too many tool calls", func(t *testing.T) {
		var calls []model.ToolCall
		for i := range 3 {
			calls = append(calls, toolCall(fmt.Sprint(i), "search", `{}`))
		}
		lim := DefaultLimits
		lim.MaxToolCalls = 2
		s, _ := run(t, memkernel.New(), &Runner{Model: &scripted{replies: []model.Response{{ToolCalls: calls}}}, Tools: ok, Limits: lim}, "r1", "fan out")
		if s.Status != agent.Stopped || !strings.Contains(s.Err, "limit is 2") {
			t.Fatalf("status %s, err %q", s.Status, s.Err)
		}
	})

	t.Run("unknown tool and oversized output go back to the model", func(t *testing.T) {
		m := &scripted{replies: []model.Response{
			{ToolCalls: []model.ToolCall{toolCall("a", "nope", `{}`), toolCall("b", "search", `not json`)}},
			{Content: "recovered"},
		}}
		big := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) {
			return strings.Repeat("x", 50), nil
		}}
		lim := DefaultLimits
		lim.MaxToolOutput = 10
		s, err := run(t, memkernel.New(), &Runner{Model: m, Tools: big, Limits: lim}, "r1", "x")
		if err != nil || s.Status != agent.Done {
			t.Fatalf("status %s, err %v / %q", s.Status, err, s.Err)
		}
		if !s.Messages[2].IsError || !strings.Contains(s.Messages[2].Text, "unknown tool") {
			t.Errorf("unknown tool result: %+v", s.Messages[2])
		}
		if got := s.Messages[3].Text; got != "xxxxxxxxxx\n…[truncated]" {
			t.Errorf("truncated output %q", got)
		}
	})
}
