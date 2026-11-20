// Package agent is the pure domain core: State × Event -> State. It has no
// dependency on internal/kernel, internal/tool, or internal/model, which is
// what makes it fast and deterministic to test (golden tests, no I/O).
// internal/executor is the only package that wires this reducer to a
// kernel.Ctx and a tool runtime.
package agent

import (
	"fmt"
	"slices"
)

// Status says what the run needs next; the executor dispatches on it. Like
// EventKind it is a closed set: the constants below, one name each in
// statusNames, and JSON encoding that rejects anything else.
type Status uint8

const (
	Idle     Status = iota // waiting for a user message
	Thinking               // needs a model call
	Acting                 // has tool calls waiting for results
	Waiting                // a tool call with side effects needs the user's approval
	Done                   // the model answered without tool calls
	Stopped                // ended by a Failed event; terminal

	numStatuses // not a status: the count, for the arrays below
)

var statusNames = [...]string{
	Idle:     "idle",
	Thinking: "thinking",
	Acting:   "acting",
	Waiting:  "waiting",
	Done:     "done",
	Stopped:  "failed",
}

// Compile-time check: adding a status without a name changes the array length.
var _ [numStatuses]string = statusNames

func (s Status) Valid() bool { return s < numStatuses }

func (s Status) String() string {
	if !s.Valid() {
		return fmt.Sprintf("Status(%d)", s)
	}
	return statusNames[s]
}

func (s Status) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("agent: invalid status %d", s)
	}
	return []byte(statusNames[s]), nil
}

func (s *Status) UnmarshalText(b []byte) error {
	i := slices.Index(statusNames[:], string(b))
	if i < 0 {
		return fmt.Errorf("agent: unknown status %q", b)
	}
	*s = Status(i)
	return nil
}

// transitions is the whole state machine: for each status, the events it
// accepts and the statuses each event may lead to. Reduce refuses any pair not
// listed here and checks its own result against it, so a change to the
// machine is a change to this table.
//
//	idle     --user_message-->  thinking
//	thinking --model_reply--->  acting   (the reply has tool calls)
//	                            done     (it doesn't)
//	acting   --tool_result--->  acting   (calls still pending)
//	                            thinking (that was the last one)
//	acting   --approval_requested--> waiting (a call with side effects)
//	waiting  --approval_decided--->  acting  (approved or not; then its tool_result)
//	done     --user_message-->  thinking (the next turn)
//	any but failed --failed-->  failed   (terminal)
var transitions = [numStatuses]map[EventKind][]Status{
	Idle:     {UserMessage: {Thinking}, Failed: {Stopped}},
	Thinking: {ModelReply: {Acting, Done}, Failed: {Stopped}},
	Acting:   {ToolResult: {Acting, Thinking}, ApprovalRequested: {Waiting}, Failed: {Stopped}},
	Waiting:  {ApprovalDecided: {Acting}, Failed: {Stopped}},
	Done:     {UserMessage: {Thinking}, Failed: {Stopped}},
	Stopped:  {},
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one transcript entry. Assistant messages may carry ToolCalls;
// tool messages answer one call via CallID.
type Message struct {
	Role      Role       `json:"role"`
	Text      string     `json:"text,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	CallID    string     `json:"call_id,omitempty"`
	IsError   bool       `json:"is_error,omitempty"`
}

type State struct {
	Status   Status     `json:"status"`
	Messages []Message  `json:"messages"`
	Pending  []ToolCall `json:"pending,omitempty"`  // calls still awaiting a ToolResult, in model order
	Awaiting string     `json:"awaiting,omitempty"` // while Waiting: the call the user is asked to approve
	Turns    int        `json:"turns"`              // model calls so far
	Usage    Usage      `json:"usage"`
	Err      string     `json:"err,omitempty"`
}

// Start returns a run that already knows the prior conversation.
func Start(history []Message) State {
	return State{Status: Idle, Messages: slices.Clone(history)}
}

// Reduce applies e to s. It never mutates s. Events the transitions table
// doesn't allow from s.Status are rejected: an invalid transition is a bug in
// the caller, not something to paper over.
func Reduce(s State, e Event) (State, error) {
	if !s.Status.Valid() {
		return s, fmt.Errorf("agent: state has invalid status %s", s.Status)
	}
	if !e.Kind.Valid() {
		return s, fmt.Errorf("agent: unknown event kind %s", e.Kind)
	}
	allowed, ok := transitions[s.Status][e.Kind]
	if !ok {
		return s, fmt.Errorf("agent: %s event not allowed while %s", e.Kind, s.Status)
	}

	next := s
	next.Messages = slices.Clone(s.Messages)
	next.Pending = slices.Clone(s.Pending)

	// The table decided whether the event is allowed; each case only decides
	// which of the allowed statuses it lands in, and updates the data.
	switch e.Kind {
	case UserMessage:
		next.Messages = append(next.Messages, Message{Role: RoleUser, Text: e.Text})
		next.Status = Thinking

	case ModelReply:
		next.Messages = append(next.Messages, Message{Role: RoleAssistant, Text: e.Text, ToolCalls: e.ToolCalls})
		next.Turns++
		next.Usage.PromptTokens += e.Usage.PromptTokens
		next.Usage.CompletionTokens += e.Usage.CompletionTokens
		if len(e.ToolCalls) > 0 {
			next.Pending = slices.Clone(e.ToolCalls)
			next.Status = Acting
		} else {
			next.Status = Done
		}

	case ToolResult:
		i := slices.IndexFunc(s.Pending, func(c ToolCall) bool { return c.ID == e.CallID })
		if i < 0 {
			return s, fmt.Errorf("agent: tool result for unknown call %q", e.CallID)
		}
		next.Messages = append(next.Messages, Message{Role: RoleTool, Text: e.Text, CallID: e.CallID, IsError: e.IsError})
		next.Pending = slices.Delete(next.Pending, i, i+1)
		if len(next.Pending) == 0 {
			next.Pending = nil
			next.Status = Thinking
		}

	case ApprovalRequested:
		if !slices.ContainsFunc(s.Pending, func(c ToolCall) bool { return c.ID == e.CallID }) {
			return s, fmt.Errorf("agent: approval requested for unknown call %q", e.CallID)
		}
		next.Awaiting = e.CallID
		next.Status = Waiting

	case ApprovalDecided:
		if e.CallID != s.Awaiting {
			return s, fmt.Errorf("agent: approval decided for call %q, but %q is awaiting", e.CallID, s.Awaiting)
		}
		next.Awaiting = ""
		next.Status = Acting

	case Failed:
		next.Status = Stopped
		next.Err = e.Err
		next.Pending = nil
		next.Awaiting = ""
	}

	if !slices.Contains(allowed, next.Status) {
		return s, fmt.Errorf("agent: bug: %s --%s--> %s is not in the transitions table", s.Status, e.Kind, next.Status)
	}
	return next, nil
}
