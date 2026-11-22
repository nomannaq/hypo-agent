package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestScheduleStateMachine(t *testing.T) {
	allowed := map[ScheduleEvent]ScheduleStatus{
		ScheduleEnqueued: ScheduleFired, ScheduleCancel: ScheduleCancelled, ScheduleExpired: ScheduleMissed, ScheduleAdvanced: SchedulePending,
	}
	for status := range numScheduleStatuses + 1 {
		for event := range numScheduleEvents + 1 {
			want := invalidScheduleStatus
			if status == SchedulePending {
				want = allowed[event]
			}
			got, err := scheduleTransition(status, event)
			if got != want || (err == nil) != want.Valid() {
				t.Errorf("%s + %s = %s, %v; want %s", status, event, got, err, want)
			}
		}
	}
	for status := SchedulePending; status < numScheduleStatuses; status++ {
		b, err := json.Marshal(status)
		var decoded ScheduleStatus
		if err != nil || json.Unmarshal(b, &decoded) != nil || decoded != status {
			t.Fatalf("status encoding: %q, %v", b, err)
		}
	}
	var bogus ScheduleStatus
	if json.Unmarshal([]byte(`"bogus"`), &bogus) == nil {
		t.Fatal("unknown schedule state decoded")
	}
	var event ScheduleEvent
	if json.Unmarshal([]byte(`""`), &event) == nil {
		t.Fatal("unknown schedule event decoded")
	}
}

func TestScheduleTimeValidatesZoneAndOffset(t *testing.T) {
	const zone = "Europe/Helsinki"
	a, err := scheduleTime("2027-10-31T03:30:00+03:00", zone)
	if err != nil || a.Format(time.RFC3339) != "2027-10-31T00:30:00Z" {
		t.Fatalf("summer offset: %v, %v", a, err)
	}
	b, err := scheduleTime("2027-10-31T03:30:00+02:00", zone)
	if err != nil || b.Sub(a) != time.Hour {
		t.Fatalf("winter offset: %v, %v", b, err)
	}
	for _, bad := range []string{"2027-03-28T03:30:00+02:00", "2027-03-28T03:30:00+03:00", "2027-06-01T09:00:00+02:00", "2027-10-31T03:30:00"} {
		if _, err := scheduleTime(bad, zone); err == nil {
			t.Errorf("accepted nonexistent or mismatched local time %q", bad)
		}
	}
	if _, err := scheduleTime("2027-06-01T09:00:00Z", "Not/AZone"); err == nil {
		t.Fatal("accepted invalid timezone")
	}
}

func TestScheduleDispatchPostgres(t *testing.T) {
	ctx := t.Context()
	pool := runtimeTestDB(t, "20261122005314_add_memory.sql", "20261122012539_one_shot_schedules.sql", "20261122013529_recurring_schedules.sql")
	s := Scheduler{DB: pool}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,name,email,password_hash) VALUES (1,'One','one@test','-'), (2,'Two','two@test','-')`)
	exec(`INSERT INTO conversations (id,user_id,title) VALUES ('chat',1,'Testing')`)
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(executor.Input{Skills: catalog, Prompt: "private previous conversation"})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO agent_runs (id,user_id,conversation_id,prompt,status,workflow_input)
		VALUES ('source',1,'chat','private previous conversation','succeeded',$1)`, raw)
	if err := s.AppendEvent(ctx, "source", 0, agent.Event{Kind: agent.UserMessage, Text: "private previous conversation"}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO run_events (run_id,seq,event) VALUES ('source',1,'{"kind":"model_reply","text":"private answer"}')`)
	timeFuture := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	args := scheduleArgs{Task: "Research fog hydration", Timezone: "UTC", When: timeFuture}
	create := func(key string) Schedule {
		t.Helper()
		entry, err := s.createSchedule(ctx, "source", key, args)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	read := func(id string) Schedule {
		t.Helper()
		entries, err := s.ListSchedules(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.ID == id {
				return e
			}
		}
		t.Fatalf("schedule %s missing", id)
		return Schedule{}
	}
	first := create("source/tool/1/a")
	if duplicate := create("source/tool/1/a"); duplicate.ID != first.ID {
		t.Fatalf("retry duplicated schedule: %s, %s", first.ID, duplicate.ID)
	}
	if entries, err := s.ListSchedules(ctx, 2); len(entries) != 0 || err != nil {
		t.Fatalf("foreign list leaked schedules: %+v, %v", entries, err)
	}
	if _, err := s.CancelSchedule(ctx, 2, first.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign cancellation: %v", err)
	}
	due := func(id string, agoSeconds int) {
		t.Helper()
		exec(`UPDATE schedules SET fire_at=now()-$2*interval '1 second',next_check=now()-$2*interval '1 second' WHERE id=$1`, id, agoSeconds)
	}
	due(first.ID, 1)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := s.fireOne(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("concurrent dispatch: %v", err)
			}
		})
	}
	wg.Wait()
	fired := read(first.ID)
	if fired.Status != ScheduleFired || fired.RunID == "" {
		t.Fatalf("schedule not dispatched: %+v", fired)
	}
	if id, input, err := s.claim(ctx); err != nil || id != fired.RunID {
		t.Fatalf("claim scheduled run: %q, %v", id, err)
	} else {
		var payload executor.Input
		if err := json.Unmarshal(input, &payload); err != nil || payload.Workflow != executor.ScheduledWorkflowV2 ||
			payload.Skills == nil || payload.Skills.Digest != catalog.Digest || payload.Prompt != first.Prompt || len(payload.History) != 0 {
			t.Fatalf("unattended payload: %+v, %v", payload, err)
		}
	}
	past := args
	past.When = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if got, err := s.createSchedule(ctx, "source", "source/tool/1/a", past); err != nil || got.ID != fired.ID {
		t.Fatalf("retry after due time changed the receipt: %+v, %v", got, err)
	}
	if _, err := s.createSchedule(ctx, "source", "source/tool/1/past", past); !errors.Is(err, errPastSchedule) {
		t.Fatalf("new schedule in the past: %v", err)
	}
	if got, err := s.CancelSchedule(ctx, 1, fired.ID); err != nil || got.Status != ScheduleFired || got.RunID != fired.RunID {
		t.Fatalf("cancel after dispatch lost the run link: %+v, %v", got, err)
	}
	race := create("source/tool/1/b")
	due(race.ID, 1)
	wg.Go(func() {
		if err := s.fireOne(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Error(err)
		}
	})
	wg.Go(func() {
		if _, err := s.CancelSchedule(ctx, 1, race.ID); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	got := read(race.ID)
	if got.Status != ScheduleFired && got.Status != ScheduleCancelled {
		t.Fatalf("cancel/dispatch race: %+v", got)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE id='run-'||$1", race.ID).Scan(&count); err != nil ||
		count > 1 || (got.Status == ScheduleFired) != (count == 1) {
		t.Fatalf("cancel/dispatch race created %d runs, %v", count, err)
	}

	full := create("source/tool/1/c")
	due(full.ID, 1)
	for i := range maxQueued {
		exec(`INSERT INTO agent_runs (id,user_id,prompt,status) VALUES ($1,1,'backlog','queued')`, fmt.Sprintf("backlog-%d", i))
	}
	if err := s.fireOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(full.ID); got.Status != SchedulePending {
		t.Fatalf("queue full: %+v", got)
	}
	due(full.ID, 3601)
	if err := s.fireOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(full.ID); got.Status != ScheduleMissed || got.RunID != "" {
		t.Fatalf("expiry: %+v", got)
	}
	exec("DELETE FROM users WHERE id=1")
	if entries, err := s.ListSchedules(ctx, 1); err != nil || len(entries) != 0 {
		t.Fatalf("account deletion left schedules: %+v, %v", entries, err)
	}
}
