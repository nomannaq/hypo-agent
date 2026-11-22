package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/executor"
)

type Recurrence uint8

const (
	Once Recurrence = iota
	Daily
	Weekly
	numRecurrences
)

var recurrenceNames = [...]string{Once: "once", Daily: "daily", Weekly: "weekly"}

var _ [numRecurrences]string = recurrenceNames

func (r Recurrence) Valid() bool { return r < numRecurrences }

func (r Recurrence) String() string {
	if r.Valid() {
		return recurrenceNames[r]
	}
	return fmt.Sprintf("Recurrence(%d)", r)
}

func (r Recurrence) MarshalText() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("invalid %s", r)
	}
	return []byte(r.String()), nil
}

func (r *Recurrence) UnmarshalText(b []byte) error {
	next := Recurrence(slices.Index(recurrenceNames[:], string(b)))
	if !next.Valid() {
		return fmt.Errorf("unknown recurrence %q", b)
	}
	*r = next
	return nil
}

// nextOccurrence advances along the local calendar. The UTC-valued wall time
// is only a representation of calendar fields, so AddDate can't move it
// across a DST transition. Dates whose wall time doesn't exist are skipped.
func nextOccurrence(at, cutoff time.Time, zone *time.Location, repeat Recurrence) time.Time {
	days := 1
	if repeat == Weekly {
		days = 7
	}
	local := at.In(zone)
	wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)

	// Keep the original weekday for weekly schedules. Leave a period of
	// slack when jumping across downtime so UTC offset changes can't skip
	// an eligible date.
	skip := int(cutoff.Sub(wall)/(24*time.Hour))/days - 1
	for later := wall.AddDate(0, 0, max(1, skip)*days); ; later = later.AddDate(0, 0, days) {
		when := resolveWallTime(later, zone)
		if when.After(cutoff) && when.After(at) {
			return when
		}
	}
}

// resolveWallTime tries each timezone interval that could contain the wall
// time. A fall-back fold yields two matching instants. Use the earlier one.
func resolveWallTime(wall time.Time, zone *time.Location) time.Time {
	var earliest time.Time
	limit := wall.Add(26 * time.Hour)
	for instant := wall.Add(-26 * time.Hour); !instant.After(limit); {
		local := instant.In(zone)
		_, offset := local.Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		_, actual := candidate.In(zone).Zone()
		if offset == actual && (earliest.IsZero() || candidate.Before(earliest)) {
			earliest = candidate
		}
		_, end := local.ZoneBounds()
		if end.IsZero() || !end.After(instant) {
			break
		}
		instant = end
	}
	return earliest
}

func advanceRecurring(ctx context.Context, tx pgx.Tx, id, chat string, owner int64, input executor.Input,
	fireAt, now time.Time, zone *time.Location, repeat Recurrence, expired bool) error {
	outcome := ScheduleFired
	var reason, runID string
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM schedule_occurrences o JOIN agent_runs r ON r.id=o.run_id
		WHERE o.schedule_id=$1 AND r.status IN ('queued','starting','running'))`, id).Scan(&active); err != nil {
		return err
	}
	switch {
	case expired:
		outcome, reason = ScheduleMissed, "expired"
	case active:
		outcome, reason = ScheduleMissed, "overlap"
	default:
		runID = fmt.Sprintf("run-%s-%d", id, fireAt.UnixMicro())
		_, err := admitRun(ctx, tx, Request{ID: runID, Owner: owner, ConversationID: chat, Input: input})
		if errors.Is(err, ErrQueueFull) {
			return deferSchedule(ctx, tx, id)
		}
		if err != nil {
			return err
		}
	}

	next, err := scheduleTransition(SchedulePending, ScheduleAdvanced)
	if err != nil {
		return err
	}
	nextAt := nextOccurrence(fireAt, now.Add(-time.Hour), zone, repeat)
	// Keep the occurrence and next due time in the same transaction as the
	// queued run, including when a server dies before acknowledging dispatch.
	if _, err := tx.Exec(ctx, `INSERT INTO schedule_occurrences (schedule_id, scheduled_for, status, reason, run_id)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''))`, id, fireAt, outcome.String(), reason, runID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE schedules SET status=$2, fire_at=$3, next_check=$3 WHERE id=$1`, id, next.String(), nextAt)
	return err
}
