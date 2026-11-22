// Package runtime admits and supervises top-level agent workflows.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
)

const (
	maxQueued = 10
	maxActive = 2
)

var (
	ErrQueueFull           = errors.New("run queue is full")
	ErrMissingConversation = errors.New("conversation not found")
	ErrRequestConflict     = errors.New("request ID is already in use")
	ErrWorkflowMissing     = errors.New("workflow does not exist")
)

const dispatchLock = 60261122

type Scheduler struct {
	DB      *pgxpool.Pool
	Start   func(ctx context.Context, id string, input []byte) error
	Inspect func(ctx context.Context, id string) (open bool, err error)
}

type Request struct {
	ID                string
	Owner             int64
	ConversationID    string
	NewConversationID string
	RequestID         string
	Input             executor.Input
}

type Receipt struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// Admit records the request before any call to Temporal. The owner row
// serializes queue admission and makes retries safe across server replicas.
func (s Scheduler) Admit(ctx context.Context, req Request) (Receipt, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(ctx)

	receipt, err := admitRun(ctx, tx, req)
	if err != nil {
		return Receipt{}, err
	}
	return receipt, tx.Commit(ctx)
}

func admitRun(ctx context.Context, tx pgx.Tx, req Request) (Receipt, error) {
	var owner int64
	if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id = $1 FOR UPDATE", req.Owner).Scan(&owner); err != nil {
		return Receipt{}, err
	}
	if req.RequestID != "" {
		var existing Receipt
		var prompt string
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT id, COALESCE(conversation_id, ''), prompt, workflow_input FROM agent_runs
			WHERE user_id = $1 AND request_id = $2`, owner, req.RequestID).Scan(&existing.ID, &existing.ConversationID, &prompt, &raw)
		switch {
		case err == nil:
			var accepted executor.Input
			if raw != nil {
				if err := json.Unmarshal(raw, &accepted); err != nil {
					return Receipt{}, err
				}
			}
			modeChanged := executor.IsDeepResearchWorkflow(accepted.Workflow) != executor.IsDeepResearchWorkflow(req.Input.Workflow)
			if modeChanged || prompt != req.Input.Prompt || (req.ConversationID != "" && req.ConversationID != existing.ConversationID) {
				return Receipt{}, ErrRequestConflict
			}
			return existing, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Receipt{}, err
		}
	}

	var queued int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE user_id = $1 AND status = $2",
		owner, Queued.String()).Scan(&queued); err != nil {
		return Receipt{}, err
	}
	if queued >= maxQueued {
		return Receipt{}, ErrQueueFull
	}

	chat := req.ConversationID
	switch {
	case chat != "":
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id = $1 AND user_id = $2)`, chat, owner).Scan(&found); err != nil {
			return Receipt{}, err
		}
		if !found {
			return Receipt{}, ErrMissingConversation
		}
	case req.NewConversationID != "":
		chat = req.NewConversationID
		title := []rune(req.Input.Prompt)
		if _, err := tx.Exec(ctx, "INSERT INTO conversations (id, user_id, title) VALUES ($1, $2, $3)",
			chat, owner, string(title[:min(len(title), 80)])); err != nil {
			return Receipt{}, err
		}
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		return Receipt{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_runs (id, user_id, conversation_id, request_id, status, prompt, workflow_input, background_research)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8)`,
		req.ID, owner, chat, req.RequestID, Queued.String(), req.Input.Prompt, input, executor.IsDeepResearchWorkflow(req.Input.Workflow))
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: req.ID, ConversationID: chat}, nil
}

func (s Scheduler) Dispatch(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := s.dispatchOne(ctx); err != nil && ctx.Err() == nil {
			log.Printf("run dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s Scheduler) dispatchOne(ctx context.Context) error {
	id, input, err := s.claim(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Start(startCtx, id, input); err != nil {
		// An RPC error can arrive after Temporal has accepted the workflow.
		// The supervisor reconciles by workflow ID before retrying.
		return err
	}
	return s.advance(ctx, id, Starting, WorkflowStarted, "")
}

func (s Scheduler) claim(ctx context.Context) (string, []byte, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx)

	// ponytail: serialize the short capacity reservation across dispatchers.
	// Per-user dispatch locks become worthwhile if reservation is a bottleneck.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", dispatchLock); err != nil {
		return "", nil, err
	}
	var id, prompt, chat string
	var ordinal int64
	var input []byte
	err = tx.QueryRow(ctx, `SELECT r.id, r.prompt, COALESCE(r.conversation_id, ''), r.ordinal, r.workflow_input
		FROM agent_runs r
		WHERE r.status = $1
		AND NOT EXISTS (SELECT 1 FROM agent_runs active WHERE active.conversation_id = r.conversation_id AND active.status IN ($2, $3)
			AND active.background_research = r.background_research)
		AND (NOT r.background_research OR NOT EXISTS (
			SELECT 1 FROM agent_runs active WHERE active.user_id = r.user_id
			AND active.status IN ($2, $3) AND active.background_research))
		AND (SELECT count(*) FROM agent_runs active WHERE active.user_id = r.user_id AND active.status IN ($2, $3)) < $4
		ORDER BY r.ordinal LIMIT 1 FOR UPDATE OF r`, Queued.String(), Starting.String(), Running.String(), maxActive,
	).Scan(&id, &prompt, &chat, &ordinal, &input)
	if err != nil {
		return "", nil, err
	}
	if chat != "" {
		request := executor.Input{Prompt: prompt}
		if input != nil {
			if err := json.Unmarshal(input, &request); err != nil {
				return "", nil, err
			}
		}
		if request.Workflow != executor.ScheduledWorkflowName {
			history, err := chatHistory(ctx, tx, chat, ordinal)
			if err != nil {
				return "", nil, err
			}
			// A first saved turn can carry context from the legacy tab chat.
			if len(history) > 0 {
				request.History = history
			}
		}
		if request.Workflow == executor.OrchestratorWorkflowName {
			if err := tx.QueryRow(ctx, `SELECT ARRAY(
				SELECT left(prompt, 1000) FROM agent_runs
				WHERE conversation_id = $1 AND ordinal < $2 AND background_research AND status IN ($3, $4)
				ORDER BY ordinal
			)`, chat, ordinal, Starting.String(), Running.String()).Scan(&request.ActiveResearch); err != nil {
				return "", nil, err
			}
		}

		input, err = json.Marshal(request)
		if err != nil {
			return "", nil, err
		}
	}
	next, err := transition(Queued, DispatchClaimed)
	if err != nil {
		return "", nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE agent_runs SET status = $2, workflow_input = $3,
		next_check = clock_timestamp() + interval '15 seconds',
		start_deadline = clock_timestamp() + interval '10 minutes' WHERE id = $1`, id, next.String(), input)
	if err != nil {
		return "", nil, err
	}
	return id, input, tx.Commit(ctx)
}

// YieldResearch moves an active turn into the account's background research
// slot. It uses the same lock as claim so no dispatcher can fill a competing
// slot during the move. Retrying a successful yield returns true.
func (s Scheduler) YieldResearch(ctx context.Context, id string) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", dispatchLock); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs r SET background_research = TRUE
		WHERE r.id = $1 AND r.status IN ($2, $3)
		AND (r.background_research OR NOT EXISTS (
			SELECT 1 FROM agent_runs active WHERE active.user_id = r.user_id
			AND active.status IN ($2, $3) AND active.background_research))`,
		id, Starting.String(), Running.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

func chatHistory(ctx context.Context, tx pgx.Tx, chat string, before int64) ([]agent.Message, error) {
	// The final event and succeeded status commit together. Read recent
	// complete turns in reverse and then restore chronological order.
	rows, err := tx.Query(ctx, `SELECT r.prompt,
		ARRAY(SELECT body FROM run_assistant_messages(r.id) ORDER BY seq DESC) FROM agent_runs r
		WHERE r.conversation_id = $1 AND r.ordinal < $2 AND r.status = $3
		ORDER BY r.ordinal DESC LIMIT 49`, chat, before, Succeeded.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []agent.Message
	var size int
	for rows.Next() {
		var prompt string
		var messages []string
		if err := rows.Scan(&prompt, &messages); err != nil {
			return nil, err
		}
		size += len(prompt)
		for _, message := range messages {
			size += len(message)
		}
		if size > 128<<10 {
			break
		}
		for _, message := range messages {
			history = append(history, agent.Message{Role: agent.RoleAssistant, Text: message})
		}
		history = append(history, agent.Message{Role: agent.RoleUser, Text: prompt})
	}
	slices.Reverse(history)
	return history, rows.Err()
}
