package runtime

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func TestNextOccurrenceKeepsLocalCalendar(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		t.Fatal(err)
	}
	parse := func(value string) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for _, tc := range []struct {
		name, last, cutoff, want string
		repeat                   Recurrence
	}{
		{"spring gap", "2027-03-27T03:30:00+02:00", "2027-03-27T03:30:00+02:00", "2027-03-29T03:30:00+03:00", Daily},
		{"fall overlap", "2027-10-30T03:30:00+03:00", "2027-10-30T03:30:00+03:00", "2027-10-31T03:30:00+03:00", Daily},
		{"weekly gap", "2027-03-21T03:30:00+02:00", "2027-03-21T03:30:00+02:00", "2027-04-04T03:30:00+03:00", Weekly},
		{"weekly after downtime", "2027-01-03T09:00:00+02:00", "2027-06-06T08:30:00+03:00", "2027-06-06T09:00:00+03:00", Weekly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextOccurrence(parse(tc.last), parse(tc.cutoff), zone, tc.repeat)
			if got.In(zone).Format(time.RFC3339) != tc.want {
				t.Fatalf("next occurrence = %s; want %s", got.In(zone), tc.want)
			}
		})
	}
	for repeat := Once; repeat < numRecurrences; repeat++ {
		wire, err := json.Marshal(repeat)
		var decoded Recurrence
		if err != nil || json.Unmarshal(wire, &decoded) != nil || repeat != decoded {
			t.Fatalf("recurrence round trip = %q, %s, %v", wire, decoded, err)
		}
	}
	var invalid Recurrence
	if json.Unmarshal([]byte(`"monthly"`), &invalid) == nil {
		t.Fatal("unsupported cadence accepted")
	}
}

func TestRecurringSchedulesPostgres(t *testing.T) {
	pool := runtimeTestDB(t, "20261122005314_add_memory.sql", "20261122012539_one_shot_schedules.sql", "20261122013529_recurring_schedules.sql")
	s := Scheduler{DB: pool}
	ctx := t.Context()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users (id,name,email,password_hash) VALUES (1,'One','one@test','-')")
	exec("INSERT INTO conversations (id,user_id,title) VALUES ('chat',1,'Testing')")
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(executor.Input{Workflow: executor.DiscoveryWorkflowName, Skills: catalog, Prompt: "source"})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO agent_runs (id,user_id,conversation_id,prompt,status,workflow_input)
		VALUES ('source',1,'chat','source','succeeded',$1)`, raw)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	args := scheduleArgs{When: at.Format(time.RFC3339), Timezone: "UTC", Task: "Daily fog review", Repeat: Daily}
	series, err := s.createSchedule(ctx, "source", "source/tool/1/daily", args)
	if err != nil {
		t.Fatal(err)
	}
	read := func() Schedule {
		t.Helper()
		entries, err := s.ListSchedules(ctx, 1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("scheduled entries = %+v, %v", entries, err)
		}
		return entries[0]
	}
	due := func(at time.Time) {
		t.Helper()
		exec("UPDATE schedules SET fire_at=$2,next_check=$2 WHERE id=$1", series.ID, at)
	}
	now := time.Now().UTC().Truncate(time.Second)
	countOccurrences := func(want int) {
		t.Helper()
		var got int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM schedule_occurrences WHERE schedule_id=$1", series.ID).Scan(&got); err != nil || got != want {
			t.Fatalf("occurrence count = %d; want %d, %v", got, want, err)
		}
	}
	due(now.Add(-48 * time.Hour))
	if err := s.fireOne(ctx); err != nil {
		t.Fatal(err)
	}
	expired := read()
	if expired.RunID != "" || expired.LastOccurrence == nil || expired.LastOccurrence.Reason != "expired" ||
		expired.FireAt.Before(now.Add(-time.Hour)) {
		t.Fatalf("downtime catch-up: %+v", expired)
	}
	countOccurrences(1)
	firstAt := now.Add(-10 * time.Second)
	due(firstAt)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := s.fireOne(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	countOccurrences(2)
	first := read()
	if first.Status != SchedulePending || first.RunID == "" || first.LastOccurrence == nil ||
		first.LastOccurrence.Status != ScheduleFired || !first.FireAt.Equal(firstAt.AddDate(0, 0, 1)) {
		t.Fatalf("first occurrence: %+v", first)
	}
	if got, err := s.createSchedule(ctx, "source", "source/tool/1/daily", args); err != nil ||
		got.ID != series.ID || !got.StartsAt.Equal(at) {
		t.Fatalf("retry changed original schedule receipt: %+v, %v", got, err)
	}

	// A queued prior occurrence counts as active even before Temporal starts.
	due(now.Add(-8 * time.Second))
	if err := s.fireOne(ctx); err != nil {
		t.Fatal(err)
	}
	skipped := read()
	if skipped.RunID != first.RunID || skipped.LastOccurrence == nil || skipped.LastOccurrence.Reason != "overlap" {
		t.Fatalf("overlap: %+v", skipped)
	}

	exec("UPDATE agent_runs SET status='succeeded' WHERE id=$1", first.RunID)

	due(now.Add(-5 * time.Second))
	if err := s.fireOne(ctx); err != nil {
		t.Fatal(err)
	}
	later := read()
	if later.RunID == "" || later.RunID == first.RunID || later.LastOccurrence == nil || later.LastOccurrence.Status != ScheduleFired {
		t.Fatalf("series did not resume: %+v", later)
	}
	cancelled, err := s.CancelSchedule(ctx, 1, series.ID)
	if err != nil || cancelled.Status != ScheduleCancelled {
		t.Fatalf("cancel series: %+v, %v", cancelled, err)
	}
	due(now.Add(-3 * time.Second))
	if err := s.fireOne(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled series was dispatched: %v", err)
	}
	countOccurrences(4)
	exec("DELETE FROM users WHERE id=1")
	countOccurrences(0)
}
