package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

const ScheduleToolName = "schedule"

var errPastSchedule = errors.New("the scheduled time has passed; pick a future time")

type Schedule struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	Prompt         string         `json:"prompt"`
	FireAt         time.Time      `json:"fire_at"`
	Timezone       string         `json:"timezone"`
	Status         ScheduleStatus `json:"status"`
	RunID          string         `json:"run_id,omitempty"`
	Repeat         Recurrence     `json:"repeat"`
	StartsAt       time.Time      `json:"starts_at"`
	LastOccurrence *Occurrence    `json:"last_occurrence,omitempty"`
}

type Occurrence struct {
	ScheduledFor time.Time      `json:"scheduled_for"`
	Status       ScheduleStatus `json:"status"`
	Reason       string         `json:"reason,omitempty"`
}

type scheduleArgs struct {
	When     string     `json:"when"`
	Timezone string     `json:"timezone"`
	Task     string     `json:"task"`
	Repeat   Recurrence `json:"repeat"`
}

func (s Scheduler) ScheduleTool(recurring bool) tool.Tool {
	description := "Schedule one reminder or read-only research task in this conversation. The user approves the exact task and time. " +
		"Use an IANA timezone and an RFC3339 local timestamp with its UTC offset at the requested time. " +
		"If the time or timezone is ambiguous, ask the user. Make the task self-contained."
	schema := `{"type":"object","properties":{"when":{"type":"string","format":"date-time","description":"Local date/time with a UTC offset, e.g. 2027-01-15T09:00:00+02:00"},"timezone":{"type":"string","description":"IANA timezone, e.g. Europe/Helsinki"},"task":{"type":"string","maxLength":8000}},"required":["when","timezone","task"]}`
	if recurring {
		description = "Schedule a reminder or read-only research task in this conversation. " +
			"Choose once, daily or weekly as requested. The user approves the task, first time and frequency. " +
			"Use an IANA timezone and an RFC3339 local timestamp with its UTC offset. Daily and weekly tasks keep the local time. " +
			"If the time or timezone is ambiguous, ask the user. Make the task self-contained."
		schema = strings.Replace(schema, `"task":{"type":"string","maxLength":8000}`, `"task":{"type":"string","maxLength":8000},"repeat":{"type":"string","enum":["once","daily","weekly"],"default":"once"}`, 1)
	}
	t := tool.Typed(ScheduleToolName, description, schema,
		func(ctx context.Context, args scheduleArgs) (string, error) {
			entry, err := s.createSchedule(ctx, tool.RunID(ctx), tool.CallKey(ctx), args)
			if err != nil {
				return "", err
			}
			zone, err := time.LoadLocation(entry.Timezone)
			if err != nil {
				return "", err
			}
			if entry.Repeat != Once {
				return fmt.Sprintf("Scheduled %s %s starting %s (%s). Results will appear in this chat.",
					entry.ID, entry.Repeat, entry.StartsAt.In(zone).Format(time.RFC3339), entry.Timezone), nil
			}
			return fmt.Sprintf("Scheduled %s for %s (%s). The result will appear in this chat.",
				entry.ID, entry.StartsAt.In(zone).Format(time.RFC3339), entry.Timezone), nil
		})
	t.Effect = tool.Write
	return t
}

// A numeric offset disambiguates overlaps and lets us reject nonexistent
// local times by checking the proposed wall time against the zone's rules.
func scheduleTime(when, timezone string) (time.Time, error) {
	if timezone == "" || timezone == "Local" || len(timezone) > 100 {
		return time.Time{}, errors.New("an IANA timezone is required")
	}
	zone, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("unknown timezone %q", timezone)
	}
	at, err := time.Parse(time.RFC3339, when)
	if err != nil {
		return time.Time{}, errors.New("use an RFC3339 date and time with a UTC offset")
	}
	_, supplied := at.Zone()
	_, actual := at.In(zone).Zone()
	if supplied != actual {
		return time.Time{}, fmt.Errorf("the local time and offset do not match %s on that date", timezone)
	}
	return at.UTC(), nil
}

func (s Scheduler) createSchedule(ctx context.Context, runID, key string, args scheduleArgs) (Schedule, error) {
	if runID == "" || key == "" || strings.Contains(runID, "/") {
		return Schedule{}, errors.New("a schedule needs a top-level run")
	}
	task := strings.TrimSpace(args.Task)
	if task == "" || len(task) > 8000 {
		return Schedule{}, errors.New("schedule needs a task of 1–8000 bytes")
	}
	at, err := scheduleTime(args.When, args.Timezone)
	if err != nil {
		return Schedule{}, err
	}
	var owner int64
	var chat string
	var raw []byte
	err = s.DB.QueryRow(ctx, `SELECT user_id, COALESCE(conversation_id,''), workflow_input FROM agent_runs WHERE id = $1`,
		runID).Scan(&owner, &chat, &raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && chat == "") {
		return Schedule{}, errors.New("schedule requires a saved conversation")
	}
	if err != nil {
		return Schedule{}, err
	}
	var source executor.Input
	if err := json.Unmarshal(raw, &source); err != nil {
		return Schedule{}, err
	}
	supportsRecurrence := source.Workflow == executor.RecurringWorkflowName ||
		source.Workflow == executor.MessagingWorkflowName || source.Workflow == executor.DiscoveryWorkflowName ||
		executor.IsOrchestratorWorkflow(source.Workflow)
	if !args.Repeat.Valid() || (args.Repeat != Once && !supportsRecurrence) {
		return Schedule{}, errors.New("this run cannot create the requested recurrence")
	}
	if err := source.Skills.Validate(); err != nil {
		return Schedule{}, err
	}
	input, err := json.Marshal(executor.Input{Workflow: executor.ScheduledWorkflowV2, Prompt: task, Skills: source.Skills})
	if err != nil {
		return Schedule{}, err
	}
	// The schedule row is also the durable receipt. Lookup runs even when
	// the proposed time has passed, so retries still return the original row.
	row := s.DB.QueryRow(ctx, `WITH inserted AS (
		INSERT INTO schedules (id,user_id,conversation_id,source_key,prompt,fire_at,timezone,next_check,status,workflow_input, repeat, starts_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$6,$8,$9,$10,$6 WHERE $6::timestamptz > clock_timestamp()
		ON CONFLICT (source_key) DO UPDATE SET source_key = schedules.source_key
		WHERE schedules.user_id = EXCLUDED.user_id AND schedules.conversation_id = EXCLUDED.conversation_id
		RETURNING id,conversation_id,prompt,fire_at,timezone,status,run_id,repeat,starts_at
	)
	SELECT id,conversation_id,prompt,fire_at,timezone,status,COALESCE(run_id,''),repeat,starts_at,NULL::jsonb FROM inserted
	UNION ALL
	SELECT id,conversation_id,prompt,fire_at,timezone,status,COALESCE(run_id,''),repeat,starts_at,NULL::jsonb FROM schedules
	WHERE source_key=$4 AND user_id=$2 AND conversation_id=$3 AND NOT EXISTS (SELECT 1 FROM inserted)`,
		"sched-"+rand.Text(), owner, chat, key, task, at, args.Timezone, SchedulePending.String(), input, args.Repeat.String())
	entry, err := scanScheduleData(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, errPastSchedule
	}
	return entry, err
}

func scanSchedule(row pgx.CollectableRow) (Schedule, error) {
	return scanScheduleData(row)
}

func scanScheduleData(row pgx.Row) (Schedule, error) {
	var entry Schedule
	var status, repeat string
	if err := row.Scan(&entry.ID, &entry.ConversationID, &entry.Prompt, &entry.FireAt, &entry.Timezone, &status,
		&entry.RunID, &repeat, &entry.StartsAt, &entry.LastOccurrence); err != nil {
		return Schedule{}, err
	}
	if err := entry.Status.UnmarshalText([]byte(status)); err != nil {
		return Schedule{}, err
	}
	return entry, entry.Repeat.UnmarshalText([]byte(repeat))
}

func (s Scheduler) ListSchedules(ctx context.Context, owner int64) ([]Schedule, error) {
	rows, err := s.DB.Query(ctx, `SELECT s.id,s.conversation_id,s.prompt,s.fire_at,s.timezone,s.status,
		COALESCE(s.run_id, (SELECT o.run_id FROM schedule_occurrences o WHERE o.schedule_id=s.id AND o.run_id IS NOT NULL
			ORDER BY o.scheduled_for DESC LIMIT 1), ''),s.repeat,s.starts_at,
		(SELECT jsonb_build_object('scheduled_for',o.scheduled_for,'status',o.status,'reason',COALESCE(o.reason,''))
			FROM schedule_occurrences o WHERE o.schedule_id=s.id ORDER BY o.scheduled_for DESC LIMIT 1)
		FROM schedules s WHERE s.user_id=$1 ORDER BY (s.status='pending') DESC, s.fire_at DESC, s.id`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSchedule)
}

// CancelSchedule reports the existing terminal state if dispatch already won.
func (s Scheduler) CancelSchedule(ctx context.Context, owner int64, id string) (Schedule, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback(ctx)
	entry, err := scanScheduleData(tx.QueryRow(ctx, `SELECT id,conversation_id,prompt,fire_at,timezone,status,COALESCE(run_id,''),repeat,starts_at,NULL::jsonb
		FROM schedules WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, owner))
	if err != nil {
		return Schedule{}, err
	}
	if entry.Status == SchedulePending {
		entry.Status, err = setScheduleState(ctx, tx, id, entry.Status, ScheduleCancel, "")
		if err != nil {
			return Schedule{}, err
		}
	}
	return entry, tx.Commit(ctx)
}

func (s Scheduler) DispatchSchedules(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if err := s.fireOne(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			log.Printf("schedule dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s Scheduler) fireOne(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Lock the owner before the schedule, matching account removal and
	// admission. Other dispatchers can process a different owner.
	var id string
	var owner int64
	err = tx.QueryRow(ctx, `SELECT s.id, s.user_id FROM schedules s JOIN users u ON u.id=s.user_id
		WHERE s.status='pending' AND s.next_check <= now()
		ORDER BY s.next_check, s.fire_at, s.id LIMIT 1 FOR UPDATE OF u SKIP LOCKED`).Scan(&id, &owner)
	if err != nil {
		return err
	}
	var chat, rawRepeat, timezone string
	var raw []byte
	var fireAt, now time.Time
	err = tx.QueryRow(ctx, `SELECT conversation_id,workflow_input,fire_at,clock_timestamp(),repeat,timezone
		FROM schedules WHERE id=$1 AND status='pending' AND next_check <= now() FOR UPDATE`, id).
		Scan(&chat, &raw, &fireAt, &now, &rawRepeat, &timezone)
	if err != nil {
		return err
	}

	var repeat Recurrence
	if err := repeat.UnmarshalText([]byte(rawRepeat)); err != nil {
		return err
	}
	expired := !fireAt.Add(time.Hour).After(now)
	if expired && repeat == Once {
		_, err := setScheduleState(ctx, tx, id, SchedulePending, ScheduleExpired, "")
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var input executor.Input
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if input.Workflow == executor.ScheduledWorkflowName {
		input.Workflow = executor.ScheduledWorkflowV2
	}

	if repeat != Once {
		zone, err := time.LoadLocation(timezone)
		if err != nil {
			return err
		}
		if err := advanceRecurring(ctx, tx, id, chat, owner, input, fireAt, now, zone, repeat, expired); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	runID := "run-" + id
	_, err = admitRun(ctx, tx, Request{ID: runID, Owner: owner, ConversationID: chat, Input: input})
	if errors.Is(err, ErrQueueFull) {
		err = deferSchedule(ctx, tx, id)
	} else if err == nil {
		// The schedule has exactly one occurrence. Its stable ID and the run
		// insert commit together, so a restart cannot enqueue a second run.
		_, err = setScheduleState(ctx, tx, id, SchedulePending, ScheduleEnqueued, runID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func deferSchedule(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE schedules SET next_check=LEAST(clock_timestamp() + interval '15 seconds', fire_at + interval '1 hour')
		WHERE id=$1`, id)
	return err
}

func setScheduleState(ctx context.Context, tx pgx.Tx, id string, current ScheduleStatus, event ScheduleEvent, runID string) (ScheduleStatus, error) {
	next, err := scheduleTransition(current, event)
	if err != nil {
		return current, err
	}
	_, err = tx.Exec(ctx, `UPDATE schedules SET status=$2, run_id=NULLIF($3,'') WHERE id=$1`, id, next.String(), runID)
	return next, err
}
