package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
)

// Supervise releases slots for closed workflows even without a connected
// browser. The next_check lease bounds concurrent checks across replicas.
func (s Scheduler) Supervise(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if err := s.checkDue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("run supervisor: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

const cancellationReason = "You cancelled this run."

// RequestCancellation stops a queued run atomically or records a durable
// request for the supervisor. Active runs retain their slot until confirmed closed.
func (s Scheduler) RequestCancellation(ctx context.Context, id string, owner int64) (bool, error) {
	cancelled, err := transition(Queued, UserCancelled)
	if err != nil {
		return false, err
	}
	var found bool
	err = s.DB.QueryRow(ctx, `WITH changed AS (
		UPDATE agent_runs SET cancel_requested = TRUE,
			status = CASE WHEN status = $3 THEN $4 ELSE status END,
			failure = CASE WHEN status = $3 THEN $5 ELSE failure END,
			next_check = clock_timestamp()
		WHERE id = $1 AND user_id = $2 AND NOT cancel_requested AND status IN ($3, $6, $7)
		RETURNING id
	), notified AS (
		SELECT pg_notify('run_events', id) FROM changed
	)
	SELECT EXISTS(SELECT 1 FROM notified)
		OR EXISTS(SELECT 1 FROM agent_runs WHERE id = $1 AND user_id = $2)`,
		id, owner, Queued.String(), cancelled.String(), cancellationReason, Starting.String(), Running.String()).Scan(&found)
	return found, err
}

type dueRun struct {
	id                  string
	status              Status
	input               []byte
	expired, cancelling bool
}

// checkDue leases every run due for a check and inspects them concurrently,
// so a restart with many stale runs releases their slots within one tick.
func (s Scheduler) checkDue(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `WITH due AS (
		SELECT id FROM agent_runs WHERE status IN ($1, $2) AND next_check <= clock_timestamp()
		ORDER BY next_check, ordinal LIMIT 64 FOR UPDATE SKIP LOCKED
	)
	UPDATE agent_runs r SET next_check = clock_timestamp() + interval '15 seconds'
	FROM due WHERE r.id = due.id RETURNING r.id, r.status, r.workflow_input,
		COALESCE(r.start_deadline <= clock_timestamp(), false), r.cancel_requested`,
		Starting.String(), Running.String())
	if err != nil {
		return err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueRun, error) {
		var run dueRun
		var raw string
		if err := row.Scan(&run.id, &raw, &run.input, &run.expired, &run.cancelling); err != nil {
			return run, err
		}
		return run, run.status.UnmarshalText([]byte(raw))
	})
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, run := range runs {
		wg.Go(func() {
			if err := s.check(ctx, run); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("run %s: %w", run.id, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s Scheduler) check(ctx context.Context, run dueRun) error {
	id, status, input, expired, cancelling := run.id, run.status, run.input, run.expired, run.cancelling
	rpc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	open, err := s.Inspect(rpc, id)
	switch {
	case errors.Is(err, ErrWorkflowMissing) && cancelling:
		if status == Starting && !expired {
			return nil // an earlier start RPC may still be accepted
		}
		return s.advance(ctx, id, status, UserCancelled, cancellationReason)
	case errors.Is(err, ErrWorkflowMissing) && status == Starting && !expired:
		if err := s.Start(rpc, id, input); err != nil {
			return err
		}
		return s.advance(ctx, id, Starting, WorkflowStarted, "")
	case errors.Is(err, ErrWorkflowMissing) && status == Starting:
		return s.advance(ctx, id, Starting, StartFailed, "The run could not start before its deadline.")
	case errors.Is(err, ErrWorkflowMissing):
		return s.advance(ctx, id, status, WorkflowClosed, "The run is no longer available.")
	case err != nil:
		return err
	case !open:
		return s.reconcileClosed(ctx, id, status, cancelling)
	case cancelling:
		err := s.CancelWorkflow(rpc, id)
		if errors.Is(err, ErrWorkflowMissing) {
			return nil // the execution closed after inspection
		}
		return err
	case status == Starting:
		return s.advance(ctx, id, Starting, WorkflowStarted, "")
	default:
		return nil
	}
}

func (s Scheduler) reconcileClosed(ctx context.Context, id string, status Status, cancelling bool) error {
	// During a rolling deployment an older worker may have appended a
	// terminal event using the old sink, leaving the runtime status active.
	var last agent.Event
	err := s.DB.QueryRow(ctx, "SELECT event FROM run_events WHERE run_id = $1 ORDER BY seq DESC LIMIT 1", id).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	switch {
	case last.Kind == agent.Failed:
		return s.advance(ctx, id, status, AgentFailed, last.Err)
	case last.Kind == agent.ModelReply && len(last.ToolCalls) == 0:
		return s.advance(ctx, id, status, AgentSucceeded, "")
	case cancelling:
		return s.advance(ctx, id, status, UserCancelled, cancellationReason)
	default:
		reason := "The run stopped without a final reply."
		if s.ClosedReason != nil {
			lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if detail := s.ClosedReason(lookup, id); detail != "" {
				reason = detail
			}
		}
		return s.advance(ctx, id, status, WorkflowClosed, reason)
	}
}

// advance uses the observed state as a compare-and-swap. A final event can
// win the race against the acknowledgement or reconciliation.
func (s Scheduler) advance(ctx context.Context, id string, from Status, event EventKind, reason string) error {
	to, err := transition(from, event)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `WITH changed AS (
		UPDATE agent_runs SET status = $3, failure = NULLIF($4, '')
		WHERE id = $1 AND status = $2 RETURNING id
	)
	SELECT pg_notify('run_events', id) FROM changed`, id, from.String(), to.String(), reason)
	return err
}

// AppendEvent is the worker's event sink. Persist the final event and release
// the slot atomically. Legacy rows have a null runtime status.
func (s Scheduler) AppendEvent(ctx context.Context, id string, seq int, e agent.Event) error {
	var event EventKind
	switch {
	case e.Kind == agent.Failed:
		event = AgentFailed
	case e.Kind == agent.ModelReply && len(e.ToolCalls) == 0:
		event = AgentSucceeded
	default:
		_, err := s.DB.Exec(ctx, `INSERT INTO run_events (run_id, seq, event) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, seq, e)
		return err
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw *string
	if err := tx.QueryRow(ctx, "SELECT status FROM agent_runs WHERE id = $1 FOR UPDATE", id).Scan(&raw); err != nil {
		return err
	}
	if raw != nil {
		var from Status
		if err := from.UnmarshalText([]byte(*raw)); err != nil {
			return err
		}
		if from == Succeeded || from == Failed {
			return nil
		}
		to, err := transition(from, event)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET status = $2, failure = NULLIF($3, '') WHERE id = $1",
			id, to.String(), e.Err); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, seq, event) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		id, seq, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
