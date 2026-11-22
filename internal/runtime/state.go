package runtime

import (
	"fmt"
	"slices"
)

// Status is the persisted run lifecycle. The agent reducer independently
// tracks the work happening inside a running workflow.
type Status uint8

const (
	invalidStatus Status = iota
	Queued
	Starting
	Running
	Succeeded
	Failed
	numStatuses
)

var statusNames = [...]string{
	Queued: "queued", Starting: "starting", Running: "running",
	Succeeded: "succeeded", Failed: "failed",
}

var _ [numStatuses]string = statusNames

func (s Status) Valid() bool { return s > invalidStatus && s < numStatuses }

func (s Status) String() string {
	if s.Valid() {
		return statusNames[s]
	}
	return fmt.Sprintf("Status(%d)", s)
}

func (s Status) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("runtime: invalid %s", s)
	}
	return []byte(s.String()), nil
}

func (s *Status) UnmarshalText(b []byte) error {
	parsed := Status(slices.Index(statusNames[:], string(b)))
	if !parsed.Valid() {
		return fmt.Errorf("runtime: unknown status %q", b)
	}
	*s = parsed
	return nil
}

type EventKind uint8

const (
	invalidEvent EventKind = iota
	DispatchClaimed
	WorkflowStarted
	AgentSucceeded
	AgentFailed
	StartFailed
	WorkflowClosed
	UserCancelled
	numEvents
)

var eventNames = [...]string{
	DispatchClaimed: "dispatch_claimed", WorkflowStarted: "workflow_started",
	AgentSucceeded: "agent_succeeded", AgentFailed: "agent_failed",
	StartFailed: "start_failed", WorkflowClosed: "workflow_closed_without_final", UserCancelled: "user_cancelled",
}

var _ [numEvents]string = eventNames

func (e EventKind) Valid() bool { return e > invalidEvent && e < numEvents }

func (e EventKind) String() string {
	if e.Valid() {
		return eventNames[e]
	}
	return fmt.Sprintf("EventKind(%d)", e)
}

func (e EventKind) MarshalText() ([]byte, error) {
	if !e.Valid() {
		return nil, fmt.Errorf("runtime: invalid %s", e)
	}
	return []byte(e.String()), nil
}

func (e *EventKind) UnmarshalText(b []byte) error {
	parsed := EventKind(slices.Index(eventNames[:], string(b)))
	if !parsed.Valid() {
		return fmt.Errorf("runtime: unknown event %q", b)
	}
	*e = parsed
	return nil
}

var transitions = [numStatuses][numEvents]Status{
	Queued:   {DispatchClaimed: Starting, UserCancelled: Failed},
	Starting: {WorkflowStarted: Running, AgentSucceeded: Succeeded, AgentFailed: Failed, StartFailed: Failed, WorkflowClosed: Failed, UserCancelled: Failed},
	Running:  {AgentSucceeded: Succeeded, AgentFailed: Failed, WorkflowClosed: Failed, UserCancelled: Failed},
}

func transition(from Status, event EventKind) (Status, error) {
	if from.Valid() && event.Valid() {
		if to := transitions[from][event]; to.Valid() {
			return to, nil
		}
	}
	return invalidStatus, fmt.Errorf("runtime: cannot apply %s to %s", event, from)
}
