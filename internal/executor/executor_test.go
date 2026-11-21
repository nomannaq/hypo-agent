package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
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

// The fake offers search (Read) and submit (Write: needs approval).
func (f *fakeTools) Specs() []model.Tool {
	return []model.Tool{
		{Type: "function", Function: model.ToolFunction{Name: "search"}},
		{Type: "function", Function: model.ToolFunction{Name: "submit"}},
	}
}

func (f *fakeTools) Effect(name string) tool.Effect {
	if name == "submit" {
		return tool.Write
	}
	return tool.Read
}

func (f *fakeTools) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	f.calls.Add(1)
	return f.call(ctx, name, args)
}

func toolCall(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
}

// register wires a runner into a kernel the way a worker does.
func register(k *memkernel.Kernel, r *Runner) {
	k.Register("agent", r.Workflow)
	for name, fn := range r.Activities() {
		k.RegisterActivity(name, fn)
	}
}

func run(t *testing.T, k *memkernel.Kernel, r *Runner, runID, prompt string) (agent.State, error) {
	t.Helper()
	register(k, r)
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
	var appended []string // "seq:kind", as run_events would receive them
	r := &Runner{Model: m, Tools: tools, Events: func(_ context.Context, runID string, seq int, e agent.Event) error {
		appended = append(appended, fmt.Sprintf("%d:%s", seq, e.Kind))
		return nil
	}}
	register(k, r)
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
	// Each event is appended once, in order: replay doesn't re-append the
	// events recorded before the crash.
	want := "0:user_message 1:model_reply 2:tool_result 3:model_reply"
	if got := strings.Join(appended, " "); got != want {
		t.Errorf("events appended %q, want %q", got, want)
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

// approvalRun asks the model for one submit (a Write tool) and then answers.
// The approval is requested as event 2 (user_message, model_reply, then
// approval_requested), so the run awaits ApprovalSignal(2).
func approvalRun(t *testing.T, decide func(k *memkernel.Kernel)) (agent.State, []string, *fakeTools) {
	t.Helper()
	m := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "submit", `{"msg":"hi"}`)}},
		{Content: "finished"},
	}}
	tools := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) { return "sent", nil }}
	var kinds []string
	r := &Runner{Model: m, Tools: tools, Limits: Limits{
		MaxTurns: 8, MaxToolCalls: 8, MaxToolOutput: 1 << 10, StepTimeout: time.Minute,
		ApprovalTimeout: 50 * time.Millisecond,
	}, Events: func(_ context.Context, _ string, _ int, e agent.Event) error {
		kinds = append(kinds, e.Kind.String())
		return nil
	}}
	k := memkernel.New()
	decide(k)
	s, err := run(t, k, r, "r1", "say hi")
	if err != nil {
		t.Fatal(err)
	}
	return s, kinds, tools
}

func toolMessage(s agent.State) agent.Message {
	for _, m := range s.Messages {
		if m.Role == agent.RoleTool {
			return m
		}
	}
	return agent.Message{}
}

func TestWriteToolRunsOnlyWhenApproved(t *testing.T) {
	answer := func(approved bool) func(k *memkernel.Kernel) {
		return func(k *memkernel.Kernel) {
			payload, _ := json.Marshal(Approval{Approved: approved})
			if err := k.Signal(t.Context(), "r1", ApprovalSignal(2), payload); err != nil {
				t.Fatal(err)
			}
		}
	}

	s, kinds, tools := approvalRun(t, answer(true))
	if want := "user_message model_reply approval_requested approval_decided tool_result model_reply"; strings.Join(kinds, " ") != want {
		t.Errorf("approved: events %v, want %s", kinds, want)
	}
	if tools.calls.Load() != 1 || toolMessage(s).Text != "sent" || s.Status != agent.Done {
		t.Errorf("approved: tool calls %d, result %+v, status %s", tools.calls.Load(), toolMessage(s), s.Status)
	}

	for name, decide := range map[string]func(*memkernel.Kernel){
		"declined":  answer(false),
		"timed out": func(*memkernel.Kernel) {}, // nobody answers within ApprovalTimeout
	} {
		s, kinds, tools := approvalRun(t, decide)
		if tools.calls.Load() != 0 {
			t.Errorf("%s: the tool ran %d times", name, tools.calls.Load())
		}
		if msg := toolMessage(s); !msg.IsError || !strings.HasPrefix(msg.Text, "Not performed") {
			t.Errorf("%s: the model was told %+v", name, msg)
		}
		if !slices.Contains(kinds, "approval_decided") || s.Status != agent.Done {
			t.Errorf("%s: events %v, status %s", name, kinds, s.Status)
		}
	}
}

// Read tools never wait for approval.
func TestReadToolNeedsNoApproval(t *testing.T) {
	m := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "search", `{}`)}},
		{Content: "done"},
	}}
	tools := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil }}
	s, err := run(t, memkernel.New(), &Runner{Model: m, Tools: tools}, "r1", "go")
	if err != nil || s.Status != agent.Done || tools.calls.Load() != 1 {
		t.Fatalf("status %s, err %v, tool calls %d", s.Status, err, tools.calls.Load())
	}
}

// The side effect lands, then the worker dies before the result is journaled,
// so the resumed run calls the tool activity again. The ledger must answer it
// from the recorded output instead of repeating the effect.
func TestLedgerStopsARetriedWriteFromRepeating(t *testing.T) {
	m := &scripted{replies: []model.Response{
		{ToolCalls: []model.ToolCall{toolCall("c1", "submit", `{}`)}},
		{Content: "done"},
	}}
	ctx, crash := context.WithCancel(t.Context())
	var effects atomic.Int32
	tools := &fakeTools{call: func(context.Context, string, json.RawMessage) (string, error) {
		effects.Add(1)
		crash() // the form was submitted; now the worker goes away
		return "sent", nil
	}}
	var mu sync.Mutex
	ledger := map[string]string{}
	var keys []string
	r := &Runner{Model: m, Tools: tools, Ledger: func(_ context.Context, key string, run func() (string, error)) (string, error) {
		mu.Lock()
		out, ok := ledger[key]
		keys = append(keys, key)
		mu.Unlock()
		if ok {
			return out, nil
		}
		out, err := run()
		if err == nil {
			mu.Lock()
			ledger[key] = out
			mu.Unlock()
		}
		return out, err
	}}
	k := memkernel.New()
	register(k, r)
	payload, _ := json.Marshal(Approval{Approved: true})
	_ = k.Signal(t.Context(), "r1", ApprovalSignal(2), payload)
	in, _ := json.Marshal(Input{Prompt: "go"})

	if _, err := k.Run(ctx, "r1", "agent", in); !errors.Is(err, kernel.ErrCanceled) {
		t.Fatalf("crashed run: err = %v, want ErrCanceled", err)
	}
	out, err := k.Run(t.Context(), "r1", "agent", in)
	if err != nil {
		t.Fatal(err)
	}
	var s agent.State
	_ = json.Unmarshal(out, &s)
	if s.Status != agent.Done || toolMessage(s).Text != "sent" {
		t.Fatalf("resumed run: %s, tool result %+v", s.Status, toolMessage(s))
	}
	if effects.Load() != 1 {
		t.Errorf("the write ran %d times, want 1", effects.Load())
	}
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] != "r1/tool/1/c1" {
		t.Errorf("ledger keys %v, want the same run/step key twice", keys)
	}
}
