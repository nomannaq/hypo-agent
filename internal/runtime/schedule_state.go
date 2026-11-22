package runtime

import (
	"fmt"
	"slices"
)

type ScheduleStatus uint8

const (
	invalidScheduleStatus ScheduleStatus = iota
	SchedulePending
	ScheduleFired
	ScheduleCancelled
	ScheduleMissed
	numScheduleStatuses
)

var scheduleStatusNames = [...]string{
	SchedulePending: "pending", ScheduleFired: "fired",
	ScheduleCancelled: "cancelled", ScheduleMissed: "missed",
}

var _ [numScheduleStatuses]string = scheduleStatusNames

func (s ScheduleStatus) Valid() bool { return s > invalidScheduleStatus && s < numScheduleStatuses }

func (s ScheduleStatus) String() string {
	if s.Valid() {
		return scheduleStatusNames[s]
	}
	return fmt.Sprintf("ScheduleStatus(%d)", s)
}

func (s ScheduleStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("invalid %s", s)
	}
	return []byte(s.String()), nil
}

func (s *ScheduleStatus) UnmarshalText(b []byte) error {
	parsed := ScheduleStatus(slices.Index(scheduleStatusNames[:], string(b)))
	if !parsed.Valid() {
		return fmt.Errorf("unknown schedule status %q", b)
	}
	*s = parsed
	return nil
}

type ScheduleEvent uint8

const (
	invalidScheduleEvent ScheduleEvent = iota
	ScheduleEnqueued
	ScheduleCancel
	ScheduleExpired
	ScheduleAdvanced
	numScheduleEvents
)

var scheduleEventNames = [...]string{
	ScheduleEnqueued: "occurrence_enqueued", ScheduleCancel: "cancelled", ScheduleExpired: "delivery_window_elapsed", ScheduleAdvanced: "occurrence_recorded",
}

var _ [numScheduleEvents]string = scheduleEventNames

func (e ScheduleEvent) Valid() bool { return e > invalidScheduleEvent && e < numScheduleEvents }

func (e ScheduleEvent) String() string {
	if e.Valid() {
		return scheduleEventNames[e]
	}
	return fmt.Sprintf("ScheduleEvent(%d)", e)
}

func (e ScheduleEvent) MarshalText() ([]byte, error) {
	if !e.Valid() {
		return nil, fmt.Errorf("invalid %s", e)
	}
	return []byte(e.String()), nil
}

func (e *ScheduleEvent) UnmarshalText(b []byte) error {
	parsed := ScheduleEvent(slices.Index(scheduleEventNames[:], string(b)))
	if !parsed.Valid() {
		return fmt.Errorf("unknown schedule event %q", b)
	}
	*e = parsed
	return nil
}

// Pending --enqueue--> Fired, --cancel--> Cancelled, --expire--> Missed.
// Recording a recurring occurrence keeps the series pending. Fired,
// cancelled and missed are terminal for the series or one-shot schedule.
var scheduleTransitions = [numScheduleStatuses][numScheduleEvents]ScheduleStatus{
	SchedulePending: {ScheduleEnqueued: ScheduleFired, ScheduleCancel: ScheduleCancelled, ScheduleExpired: ScheduleMissed, ScheduleAdvanced: SchedulePending},
}

func scheduleTransition(status ScheduleStatus, event ScheduleEvent) (ScheduleStatus, error) {
	if status.Valid() && event.Valid() {
		if next := scheduleTransitions[status][event]; next.Valid() {
			return next, nil
		}
	}
	return invalidScheduleStatus, fmt.Errorf("cannot apply %s to %s schedule", event, status)
}
