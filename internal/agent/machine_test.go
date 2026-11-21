package agent

import (
	"encoding/json"
	"slices"
	"testing"
)

// Every status × every event kind: Reduce must accept the pair exactly when the
// transitions table lists it, and land only on a status the table allows.
func TestStateMachineMatchesTable(t *testing.T) {
	one := []ToolCall{call("c1", "t", `{}`)}
	two := []ToolCall{call("c1", "t", `{}`), call("c2", "t", `{}`)}
	must := func(s State, e Event) State {
		t.Helper()
		next, err := Reduce(s, e)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	idle := Start(nil)
	thinking := must(idle, Event{Kind: UserMessage, Text: "q"})

	// A representative state for every status (two for acting, so both of
	// its tool_result targets are exercised).
	states := map[string]State{
		"idle":           idle,
		"thinking":       thinking,
		"acting(1 left)": must(thinking, Event{Kind: ModelReply, ToolCalls: one}),
		"acting(2 left)": must(thinking, Event{Kind: ModelReply, ToolCalls: two}),
		"waiting":        must(must(thinking, Event{Kind: ModelReply, ToolCalls: one}), Event{Kind: ApprovalRequested, CallID: "c1"}),
		"done":           must(thinking, Event{Kind: ModelReply, Text: "a"}),
		"failed":         must(idle, Event{Kind: Failed, Err: "x"}),
	}
	// Events with data that fits the states above, covering both outcomes of
	// model_reply.
	events := []Event{
		{Kind: UserMessage, Text: "q"},
		{Kind: ModelReply, Text: "a"},
		{Kind: ModelReply, ToolCalls: one},
		{Kind: ToolResult, CallID: "c1", Text: "out"},
		{Kind: ApprovalRequested, CallID: "c1"},
		{Kind: ApprovalDecided, CallID: "c1", Approved: true},
		{Kind: Failed, Err: "x"},
	}

	covered := map[Status]bool{}
	reached := map[[3]Status]bool{}
	for name, s := range states {
		covered[s.Status] = true
		for _, e := range events {
			allowed, inTable := transitions[s.Status][e.Kind]
			next, err := Reduce(s, e)
			switch {
			case inTable && err != nil:
				t.Errorf("%s + %s: table allows it, Reduce said %v", name, e.Kind, err)
			case !inTable && err == nil:
				t.Errorf("%s + %s: not in the table, Reduce accepted it (-> %s)", name, e.Kind, next.Status)
			case inTable && !slices.Contains(allowed, next.Status):
				t.Errorf("%s + %s -> %s, table allows only %v", name, e.Kind, next.Status, allowed)
			case inTable:
				reached[[3]Status{s.Status, Status(e.Kind), next.Status}] = true
			}
		}
	}

	for st := range numStatuses {
		if !covered[st] {
			t.Errorf("status %s has no representative state in this test", st)
		}
		// Every non-terminal status must be able to fail; the terminal one
		// must accept nothing.
		_, canFail := transitions[st][Failed]
		if st == Stopped && len(transitions[st]) != 0 {
			t.Errorf("%s is terminal but accepts %v", st, transitions[st])
		}
		if st != Stopped && !canFail {
			t.Errorf("%s cannot fail", st)
		}
		// Every target the table lists must actually be reachable.
		for kind, targets := range transitions[st] {
			for _, to := range targets {
				if !reached[[3]Status{st, Status(kind), to}] {
					t.Errorf("table lists %s --%s--> %s but no input reached it", st, kind, to)
				}
			}
		}
	}
}

// The enums are closed sets: one non-empty, unique name per value, JSON round
// trips, and values outside the set fail in both directions.
func TestEnumsAreClosed(t *testing.T) {
	for _, names := range [][]string{statusNames[:], eventKindNames[:]} {
		seen := map[string]bool{}
		for i, n := range names {
			if n == "" || seen[n] {
				t.Errorf("name %d is empty or duplicated: %q", i, n)
			}
			seen[n] = true
		}
	}

	b, err := json.Marshal(State{Status: Acting})
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || back.Status != Acting {
		t.Fatalf("round trip: %s -> %v, %v", b, back.Status, err)
	}

	if _, err := json.Marshal(State{Status: numStatuses}); err == nil {
		t.Error("marshalled an out-of-range status")
	}
	if _, err := json.Marshal(Event{Kind: numEventKinds}); err == nil {
		t.Error("marshalled an out-of-range event kind")
	}
	if err := json.Unmarshal([]byte(`{"status":"sleeping"}`), &back); err == nil {
		t.Error("unmarshalled an unknown status")
	}
	var e Event
	if err := json.Unmarshal([]byte(`{"kind":"teleport"}`), &e); err == nil {
		t.Error("unmarshalled an unknown event kind")
	}
	if _, err := Reduce(State{Status: numStatuses}, Event{Kind: UserMessage}); err == nil {
		t.Error("Reduce accepted a state with an invalid status")
	}
}
