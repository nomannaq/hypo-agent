package agent

import (
	"encoding/json"
	"fmt"
	"slices"
)

// EventKind names what happened in a run. Events are the source of truth: a
// run's State is always Reduce folded over its events, which is what makes
// them safe to persist and replay (run_events) and to stream to the UI.
//
// Go has no closed enums, so the set is closed by hand: the kinds are the
// constants below, eventKindNames must have one name per kind (checked at
// compile time for length, and by TestEnumNames for gaps), and JSON encoding
// rejects anything else.
type EventKind uint8

const (
	// UserMessage starts a turn: Text is the prompt.
	UserMessage EventKind = iota
	// ModelReply is one model call's result: Text and/or ToolCalls, plus Usage.
	ModelReply
	// ToolResult answers one pending tool call: CallID, Text (output), IsError.
	ToolResult
	// Failed ends the run: Err says why (guard tripped, model error, ...).
	Failed

	numEventKinds // not a kind: the count, for the arrays below
)

var eventKindNames = [...]string{
	UserMessage: "user_message",
	ModelReply:  "model_reply",
	ToolResult:  "tool_result",
	Failed:      "failed",
}

// Compile-time check: adding a kind without a name changes the array length.
var _ [numEventKinds]string = eventKindNames

func (k EventKind) Valid() bool { return k < numEventKinds }

func (k EventKind) String() string {
	if !k.Valid() {
		return fmt.Sprintf("EventKind(%d)", k)
	}
	return eventKindNames[k]
}

func (k EventKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("agent: invalid event kind %d", k)
	}
	return []byte(eventKindNames[k]), nil
}

func (k *EventKind) UnmarshalText(b []byte) error {
	i := slices.Index(eventKindNames[:], string(b))
	if i < 0 {
		return fmt.Errorf("agent: unknown event kind %q", b)
	}
	*k = EventKind(i)
	return nil
}

type Event struct {
	Kind      EventKind  `json:"kind"`
	Text      string     `json:"text,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	CallID    string     `json:"call_id,omitempty"`
	IsError   bool       `json:"is_error,omitempty"`
	Usage     Usage      `json:"usage,omitzero"`
	Err       string     `json:"err,omitempty"`
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }
