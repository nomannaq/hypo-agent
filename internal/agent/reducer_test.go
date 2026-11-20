package agent

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

func call(id, name, args string) ToolCall {
	return ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
}

// Each scenario folds events from a starting state; the final state must match
// testdata/<name>.golden.json byte for byte. Regenerate with:
//
//	go test ./internal/agent -update
func TestReduceGolden(t *testing.T) {
	scenarios := map[string]struct {
		start  State
		events []Event
	}{
		"plain_answer": {
			start: Start(nil),
			events: []Event{
				{Kind: UserMessage, Text: "Is coffee a diuretic?"},
				{Kind: ModelReply, Text: "Mildly, at high doses.", Usage: Usage{PromptTokens: 12, CompletionTokens: 6}},
			},
		},
		"tool_round_trip": {
			start: Start(nil),
			events: []Event{
				{Kind: UserMessage, Text: "Find studies on caffeine and memory."},
				{Kind: ModelReply, ToolCalls: []ToolCall{
					call("c1", "search", `{"q":"caffeine memory"}`),
					call("c2", "search", `{"q":"caffeine recall RCT"}`),
				}, Usage: Usage{PromptTokens: 20, CompletionTokens: 9}},
				// Results may arrive out of model order.
				{Kind: ToolResult, CallID: "c2", Text: "2 RCTs found"},
				{Kind: ToolResult, CallID: "c1", Text: "timeout", IsError: true},
				{Kind: ModelReply, Text: "Two RCTs suggest a small effect.", Usage: Usage{PromptTokens: 40, CompletionTokens: 8}},
			},
		},
		"second_turn_keeps_history": {
			start: Start([]Message{
				{Role: RoleUser, Text: "Hi"},
				{Role: RoleAssistant, Text: "Hello!"},
			}),
			events: []Event{
				{Kind: UserMessage, Text: "Summarize our chat."},
				{Kind: ModelReply, Text: "You said hi."},
			},
		},
		"approval_declined": {
			start: Start(nil),
			events: []Event{
				{Kind: UserMessage, Text: "Say hi through the contact form."},
				{Kind: ModelReply, ToolCalls: []ToolCall{call("c1", "submit_form", `{"url":"https://example.com/contact"}`)}},
				{Kind: ApprovalRequested, CallID: "c1"},
				{Kind: ApprovalDecided, CallID: "c1", Text: "declined"},
				{Kind: ToolResult, CallID: "c1", Text: "The user declined this action.", IsError: true},
				{Kind: ModelReply, Text: "Okay, I won't send it."},
			},
		},
		"waiting_for_approval": {
			start: Start(nil),
			events: []Event{
				{Kind: UserMessage, Text: "Say hi through the contact form."},
				{Kind: ModelReply, ToolCalls: []ToolCall{call("c1", "submit_form", `{"url":"https://example.com/contact"}`)}},
				{Kind: ApprovalRequested, CallID: "c1"},
			},
		},
		"failed_mid_tools": {
			start: Start(nil),
			events: []Event{
				{Kind: UserMessage, Text: "Go"},
				{Kind: ModelReply, ToolCalls: []ToolCall{call("c1", "fetch", `{}`)}},
				{Kind: Failed, Err: "turn limit reached"},
			},
		},
	}

	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			s := sc.start
			for i, e := range sc.events {
				var err error
				if s, err = Reduce(s, e); err != nil {
					t.Fatalf("event %d (%s): %v", i, e.Kind, err)
				}
			}
			got, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("state differs from %s:\ngot:\n%s\nwant:\n%s", path, got, want)
			}
		})
	}
}

func TestReduceRejectsInvalidTransitions(t *testing.T) {
	thinking, _ := Reduce(Start(nil), Event{Kind: UserMessage, Text: "x"})
	acting, _ := Reduce(thinking, Event{Kind: ModelReply, ToolCalls: []ToolCall{call("c1", "t", `{}`)}})
	waiting, _ := Reduce(acting, Event{Kind: ApprovalRequested, CallID: "c1"})

	cases := map[string]struct {
		s State
		e Event
	}{
		"reply before prompt":        {Start(nil), Event{Kind: ModelReply, Text: "?"}},
		"tool result while thinking": {thinking, Event{Kind: ToolResult, CallID: "c1"}},
		"unknown call id":            {acting, Event{Kind: ToolResult, CallID: "nope"}},
		"prompt while acting":        {acting, Event{Kind: UserMessage, Text: "y"}},
		"unknown kind":               {thinking, Event{Kind: EventKind(99)}},
		"approval for unknown call":  {acting, Event{Kind: ApprovalRequested, CallID: "nope"}},
		"tool result while waiting":  {waiting, Event{Kind: ToolResult, CallID: "c1"}},
		"decision for another call":  {waiting, Event{Kind: ApprovalDecided, CallID: "c2"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Reduce(c.s, c.e)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got.Status != c.s.Status || len(got.Messages) != len(c.s.Messages) {
				t.Error("state changed on a rejected event")
			}
		})
	}

	// Reduce must not mutate its input: the executor keeps old states around.
	before := len(acting.Pending)
	if _, err := Reduce(acting, Event{Kind: ToolResult, CallID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if len(acting.Pending) != before {
		t.Error("Reduce mutated the input state")
	}
}
