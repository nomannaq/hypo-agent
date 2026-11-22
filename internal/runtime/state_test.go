package runtime

import (
	"encoding/json"
	"testing"
)

func TestLifecycleTransitions(t *testing.T) {
	want := map[[2]uint8]Status{
		{uint8(Queued), uint8(UserCancelled)}:     Failed,
		{uint8(Starting), uint8(UserCancelled)}:   Failed,
		{uint8(Running), uint8(UserCancelled)}:    Failed,
		{uint8(Queued), uint8(DispatchClaimed)}:   Starting,
		{uint8(Starting), uint8(WorkflowStarted)}: Running,
		{uint8(Starting), uint8(AgentSucceeded)}:  Succeeded,
		{uint8(Starting), uint8(AgentFailed)}:     Failed,
		{uint8(Starting), uint8(StartFailed)}:     Failed,
		{uint8(Starting), uint8(WorkflowClosed)}:  Failed,
		{uint8(Running), uint8(AgentSucceeded)}:   Succeeded,
		{uint8(Running), uint8(AgentFailed)}:      Failed,
		{uint8(Running), uint8(WorkflowClosed)}:   Failed,
	}
	for status := range numStatuses + 1 {
		for event := range numEvents + 1 {
			expected, allowed := want[[2]uint8{uint8(status), uint8(event)}]
			got, err := transition(Status(status), EventKind(event))
			if (err == nil) != allowed || got != expected {
				t.Errorf("%s + %s = %s, %v; want %s (allowed=%t)", status, event, got, err, expected, allowed)
			}
		}
	}
}

func TestLifecycleNames(t *testing.T) {
	seen := map[string]bool{}
	for status := Queued; status < numStatuses; status++ {
		raw, err := json.Marshal(status)
		var decoded Status
		if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded != status || seen[string(raw)] {
			t.Fatalf("invalid or duplicate status encoding for %s: %s, %v", status, raw, err)
		}
		seen[string(raw)] = true
	}
	clear(seen)
	for event := DispatchClaimed; event < numEvents; event++ {
		raw, err := json.Marshal(event)
		var decoded EventKind
		if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded != event || seen[string(raw)] {
			t.Fatalf("invalid or duplicate event encoding for %s: %s, %v", event, raw, err)
		}
		seen[string(raw)] = true
	}
	var status Status
	var event EventKind
	if json.Unmarshal([]byte(`"bogus"`), &status) == nil || json.Unmarshal([]byte(`""`), &event) == nil {
		t.Fatal("unknown lifecycle name accepted")
	}
	if _, err := json.Marshal(invalidStatus); err == nil {
		t.Fatal("zero status encoded")
	}
}
