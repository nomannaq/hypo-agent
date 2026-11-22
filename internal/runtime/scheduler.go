// Package runtime admits and supervises top-level agent workflows.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

const (
	maxQueued         = 10
	MaxForegroundRuns = 4
	MaxBackgroundRuns = 3
)

var (
	ErrQueueFull           = errors.New("run queue is full")
	ErrMissingConversation = errors.New("conversation not found")
	ErrRequestConflict     = errors.New("request ID is already in use")
	ErrWorkflowMissing     = errors.New("workflow does not exist")
)

const dispatchLock = 60261122

type Scheduler struct {
	DB             *pgxpool.Pool
	Start          func(ctx context.Context, id string, input []byte) error
	Inspect        func(ctx context.Context, id string) (open bool, err error)
	ClosedReason   func(ctx context.Context, id string) string
	CancelWorkflow func(ctx context.Context, id string) error
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
	var statusReply string
	if executor.IsOrchestratorWorkflow(req.Input.Workflow) && isResearchStatusQuestion(req.Input.Prompt) {
		var queuedResearch, activeResearch, queuedChat, activeChat int
		err := tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE background_research AND status = 'queued'),
			count(*) FILTER (WHERE background_research AND status IN ('starting', 'running')),
			count(*) FILTER (WHERE NOT background_research AND status = 'queued'),
			count(*) FILTER (WHERE NOT background_research AND status IN ('starting', 'running'))
			FROM agent_runs WHERE user_id = $1 AND conversation_id = $2`, owner, chat).
			Scan(&queuedResearch, &activeResearch, &queuedChat, &activeChat)
		if err != nil {
			return Receipt{}, err
		}
		if activeResearch+queuedResearch > 0 || strings.EqualFold(req.Input.Prompt, "/status") {
			statusReply = fmt.Sprintf("**This conversation**\n\n- Chat turns: %d active, %d queued.\n- Background research: %d active, %d queued.", activeChat, queuedChat, activeResearch, queuedResearch)
		}
	}
	status := Queued
	if statusReply != "" {
		status = Succeeded
	} else {

		var queued int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE user_id = $1 AND status = $2",
			owner, Queued.String()).Scan(&queued); err != nil {
			return Receipt{}, err
		}
		if queued >= maxQueued {
			return Receipt{}, ErrQueueFull
		}

	}

	input, err := json.Marshal(req.Input)
	if err != nil {
		return Receipt{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_runs (id, user_id, conversation_id, request_id, status, prompt, workflow_input, background_research)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8)`,
		req.ID, owner, chat, req.RequestID, status.String(), req.Input.Prompt, input, executor.IsDeepResearchWorkflow(req.Input.Workflow) || executor.IsScheduledWorkflow(req.Input.Workflow))
	if err != nil {
		return Receipt{}, err
	}
	if statusReply != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, seq, event) VALUES ($1, 0, $2), ($1, 1, $3)`,
			req.ID, agent.Event{Kind: agent.UserMessage, Text: req.Input.Prompt},
			agent.Event{Kind: agent.ModelReply, Text: statusReply}); err != nil {
			return Receipt{}, err
		}
	}

	return Receipt{ID: req.ID, ConversationID: chat}, nil
}

func (s Scheduler) Dispatch(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := s.dispatchReady(ctx); err != nil && ctx.Err() == nil {
			log.Printf("run dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// dispatchReady claims every run that has capacity. Claims serialize on the
// dispatch lock; each start RPC runs in its own goroutine so a slow Temporal
// call never holds back the next prompt.
func (s Scheduler) dispatchReady(ctx context.Context) error {
	for {
		id, input, err := s.claim(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		go func() {
			if err := s.start(ctx, id, input); err != nil && ctx.Err() == nil {
				log.Printf("run %s start: %v", id, err)
			}
		}()
	}
}

func (s Scheduler) start(ctx context.Context, id string, input []byte) error {
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
		AND (SELECT count(*) FROM agent_runs active WHERE active.user_id = r.user_id
			AND active.status IN ($2, $3) AND active.background_research = r.background_research)
			< CASE WHEN r.background_research THEN $5::int ELSE $4::int END
		ORDER BY r.ordinal LIMIT 1 FOR UPDATE OF r`, Queued.String(), Starting.String(), Running.String(), MaxForegroundRuns, MaxBackgroundRuns,
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
		if !executor.IsScheduledWorkflow(request.Workflow) {
			history, active, queued, err := chatHistory(ctx, tx, chat, ordinal, executor.IsOrchestratorWorkflow(request.Workflow))
			if err != nil {
				return "", nil, err
			}
			// A first saved turn can carry context from the legacy tab chat.
			if len(history) > 0 {
				request.History = history
			}
			request.ActiveResearch = active
			request.QueuedResearch = queued
			if executor.IsOrchestratorWorkflow(request.Workflow) {
				if request.Progress, err = chatProgress(ctx, tx, chat, ordinal); err != nil {
					return "", nil, err
				}
			}
			if executor.IsOrchestratorWorkflow(request.Workflow) && isResearchStatusQuestion(request.Prompt) {
				switch {
				case len(active) > 0:
					request.ResearchStatusReply = "Earlier work in this chat is still running. Its final answer hasn’t been published yet."
					if len(queued) > 0 {
						request.ResearchStatusReply += " More is queued behind it."
					}
				case len(queued) > 0:
					request.ResearchStatusReply = "Earlier work in this chat is queued. It hasn’t started yet."
				default:
					var failure string
					err := tx.QueryRow(ctx, `SELECT CASE WHEN status = $3 THEN COALESCE(failure, 'The research ended without a report.') ELSE '' END FROM agent_runs
						WHERE conversation_id = $1 AND ordinal < $2 AND background_research AND status IN ($3, $4)
						ORDER BY ordinal DESC LIMIT 1`, chat, ordinal, Failed.String(), Succeeded.String()).Scan(&failure)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return "", nil, err
					}
					if failure != "" {
						request.ResearchStatusReply = failure
					} else if err == nil {
						request.ResearchStatusReply = "Your latest background research finished. Its report is in this conversation."
					}
				}
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

func isResearchStatusQuestion(prompt string) bool {
	prompt = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(prompt)), "’", "'")
	switch strings.TrimRight(prompt, "?.!") {
	case "/status", "did it stop", "did the research stop", "is it running", "is the research running",
		"what's going on", "whats going on", "what is happening now", "research status", "what is the research status":
		return true
	default:
		return false
	}
}

// YieldResearch moves an active turn into the background lane if its account
// has capacity. It reserves capacity under the
// same lock as claim. Retrying a successful yield returns true.
func (s Scheduler) YieldResearch(ctx context.Context, id string) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", dispatchLock); err != nil {
		return false, err
	}
	var yielded bool
	err = tx.QueryRow(ctx, `WITH moved AS (
		UPDATE agent_runs r SET background_research = TRUE
		WHERE r.id = $1 AND r.status IN ($2, $3) AND NOT r.cancel_requested
		AND (r.background_research OR
			(SELECT count(*) FROM agent_runs active WHERE active.user_id = r.user_id
				AND active.status IN ($2, $3) AND active.background_research) < $4
		) RETURNING r.id
	), notified AS (
		SELECT pg_notify('run_events', id) FROM moved
	)
	SELECT EXISTS(SELECT 1 FROM notified)`, id, Starting.String(), Running.String(), MaxBackgroundRuns).Scan(&yielded)
	if err != nil {
		return false, err
	}
	return yielded, tx.Commit(ctx)
}

func chatHistory(ctx context.Context, tx pgx.Tx, chat string, before int64, includeActive bool) ([]agent.Message, []string, []string, error) {
	// Read completed and in-flight turns in one snapshot so a turn finishing
	// during dispatch appears in exactly one category. Turns in a chat run
	// concurrently, so a new turn sees earlier unfinished prompts as pending.
	rows, err := tx.Query(ctx, `(SELECT r.prompt, r.status, r.ordinal,
		ARRAY(SELECT body FROM run_assistant_messages(r.id) ORDER BY seq DESC) AS messages FROM agent_runs r
		WHERE r.conversation_id = $1 AND r.ordinal < $2 AND r.status = $3
		ORDER BY r.ordinal DESC LIMIT 49)
		UNION ALL
		(SELECT left(prompt, 1000), status, ordinal, ARRAY[]::text[] FROM agent_runs
		WHERE $4 AND conversation_id = $1 AND ordinal < $2 AND status IN ($5, $6, $7))
		ORDER BY ordinal DESC`, chat, before, Succeeded.String(), includeActive, Starting.String(), Running.String(), Queued.String())
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var history []agent.Message
	var active, queued []string
	var size int
	for rows.Next() {
		var prompt string
		var status string
		var ordinal int64
		var messages []string
		if err := rows.Scan(&prompt, &status, &ordinal, &messages); err != nil {
			return nil, nil, nil, err
		}
		if status == Queued.String() {
			queued = append(queued, prompt)
			continue
		}
		if status != Succeeded.String() {
			active = append(active, prompt)
			continue
		}
		size += len(prompt)
		for _, message := range messages {
			size += len(message)
		}
		if size > 128<<10 {
			continue
		}
		for _, message := range messages {
			history = append(history, agent.Message{Role: agent.RoleAssistant, Text: message})
		}
		history = append(history, agent.Message{Role: agent.RoleUser, Text: prompt})
	}
	slices.Reverse(history)
	slices.Reverse(active)
	slices.Reverse(queued)
	return history, active, queued, rows.Err()
}

// chatProgress describes the conversation's unfinished earlier runs for the
// orchestrator: stage, elapsed time, tool use, pages read and the last public
// message. Deep research children don't journal events, so their runs report
// stages only.
func chatProgress(ctx context.Context, tx pgx.Tx, chat string, before int64) (string, error) {
	rows, err := tx.Query(ctx, `SELECT left(r.prompt, 300), r.status, r.background_research,
		extract(epoch FROM clock_timestamp() - r.created_at)::int,
		ARRAY(SELECT c->>'name' FROM run_events e, jsonb_array_elements(e.event->'tool_calls') c
			WHERE e.run_id = r.id AND e.event->>'kind' = 'model_reply' ORDER BY e.seq),
		ARRAY(SELECT DISTINCT ON (u) u FROM (SELECT c->'args'->>'url' AS u, e.seq FROM run_events e, jsonb_array_elements(e.event->'tool_calls') c
			WHERE e.run_id = r.id AND e.event->>'kind' = 'model_reply' AND c->>'name' IN ('browse', 'browser_open')
			AND jsonb_typeof(c->'args') = 'object') pages WHERE u IS NOT NULL ORDER BY u, seq DESC),
		COALESCE((SELECT left(body, 400) FROM run_assistant_messages(r.id) ORDER BY seq DESC LIMIT 1), '')
		FROM agent_runs r WHERE r.conversation_id = $1 AND r.ordinal < $2 AND r.status IN ($3, $4, $5)
		ORDER BY r.ordinal`, chat, before, Queued.String(), Starting.String(), Running.String())
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var prompt, status, last string
		var background bool
		var seconds int
		var calls, pages []string
		if err := rows.Scan(&prompt, &status, &background, &seconds, &calls, &pages, &last); err != nil {
			return "", err
		}
		lane := "in the foreground"
		if background {
			lane = "in the background"
		}
		fmt.Fprintf(&b, "- %q: %s %s for %s.", prompt, status, lane, (time.Duration(seconds) * time.Second).String())
		counts := map[string]int{}
		var stage string
		for _, name := range calls {
			counts[name]++
			if s, ok := strings.CutPrefix(name, "deep_research_"); ok {
				stage = s
			}
		}
		if stage != "" {
			fmt.Fprintf(&b, " Deep research stage: %s.", stage)
		}
		if n := counts["web_search"]; n > 0 {
			fmt.Fprintf(&b, " Web searches: %d.", n)
		}
		if n := counts[tool.ResearchName]; n > 0 {
			b.WriteString(" Delegated investigations are running.")
		}
		if len(pages) > 0 {
			fmt.Fprintf(&b, " Pages read (%d): %s.", len(pages), strings.Join(pages[:min(len(pages), 6)], ", "))
		}
		if last != "" {
			fmt.Fprintf(&b, " Last message to the user: %q.", last)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), rows.Err()
}
