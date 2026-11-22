package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
	"nomanqureshi.dev/hpyo-agent/internal/runtime"
)

const maxChatMessages = 100

// pingEvery keeps idle streams open through proxies and is how often a stream
// checks whether its run died without a final event.
const pingEvery = 15 * time.Second

// startRun persists a request for the dispatcher. Older clients can still
// send their own message history instead of selecting a conversation.
func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConversationID string `json:"conversation_id"`
		RequestID      string `json:"request_id"`
		Prompt         string `json:"prompt"`
		DeepResearch   bool   `json:"deep_research"`
		Messages       []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	req := runtime.Request{
		ID: "run-" + rand.Text(), Owner: currentUser(r).ID, RequestID: in.RequestID,
		Input: executor.Input{Workflow: executor.RecurringWorkflowName, Skills: s.skills},
	}
	if in.DeepResearch {
		req.Input.Workflow = executor.DeepResearchWorkflowV2
	}
	if in.Messages != nil {
		n := len(in.Messages)
		if in.Prompt != "" || in.ConversationID != "" || in.RequestID != "" ||
			n == 0 || n > maxChatMessages || in.Messages[n-1].Role != "user" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, m := range in.Messages {
			if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
		for _, m := range in.Messages[:n-1] {
			req.Input.History = append(req.Input.History, agent.Message{Role: agent.Role(m.Role), Text: m.Content})
		}
		req.Input.Prompt = in.Messages[n-1].Content
	} else {
		req.Input.Prompt = strings.TrimSpace(in.Prompt)
		if req.Input.Prompt == "" || len(req.Input.Prompt) > 32<<10 ||
			len(in.ConversationID) > 64 || len(in.RequestID) > 64 || strings.TrimSpace(in.RequestID) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.ConversationID = in.ConversationID
		if req.ConversationID == "" {
			req.NewConversationID = "chat-" + rand.Text()
		}
	}
	receipt, err := s.scheduler.Admit(r.Context(), req)
	switch {
	case errors.Is(err, runtime.ErrQueueFull):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, runtime.ErrMissingConversation):
		http.NotFound(w, r)
	case errors.Is(err, runtime.ErrRequestConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		s.fail(w, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		if in.Messages == nil {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(receipt)
	}
}

func (s *server) startWorkflow(ctx context.Context, id string, input []byte) error {
	var version struct {
		Workflow string `json:"workflow"`
	}
	if err := json.Unmarshal(input, &version); err != nil {
		return err
	}
	workflowName := version.Workflow
	switch workflowName {
	case "":
		workflowName = executor.WorkflowName
	case executor.MemoryWorkflowName, executor.SkillsWorkflowName, executor.SchedulingWorkflowName, executor.ScheduledWorkflowName, executor.RecurringWorkflowName, executor.DeepResearchWorkflowName, executor.DeepResearchWorkflowV2:
	default:
		return fmt.Errorf("unknown workflow type %q", workflowName)
	}
	timeout := 30 * time.Minute
	if executor.IsDeepResearchWorkflow(workflowName) {
		timeout = time.Hour
	}
	_, err := s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: id, TaskQueue: temporalkernel.TaskQueue,
		WorkflowExecutionTimeout:                 timeout,
		WorkflowIDReusePolicy:                    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, workflowName, input)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil // the same app run was accepted on an earlier attempt
	}
	return err
}

func (s *server) inspectWorkflow(ctx context.Context, id string) (bool, error) {
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, id, "")
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return false, runtime.ErrWorkflowMissing
	}
	if err != nil {
		return false, err
	}
	return desc.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

// decideApproval answers the run's pending approval request: the run is
// paused on an approval_requested event (its latest), waiting for the signal
// named after that event's seq. Anything else is a stale or forged answer.
func (s *server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CallID   string `json:"call_id"`
		Approved bool   `json:"approved"`
		Answer   string `json:"answer"` // for an ask_user question
	}
	if !s.decode(w, r, &in) {
		return
	}
	if len(in.Answer) > 4000 {
		http.Error(w, "the answer is too long", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var seq int
	var last agent.Event
	err := s.db.QueryRow(ctx, `SELECT e.seq, e.event FROM run_events e JOIN agent_runs r ON r.id = e.run_id
		WHERE e.run_id = $1 AND r.user_id = $2 ORDER BY e.seq DESC LIMIT 1`, id, currentUser(r).ID).Scan(&seq, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if last.Kind != agent.ApprovalRequested || last.CallID != in.CallID {
		http.Error(w, "nothing is waiting for that approval", http.StatusConflict)
		return
	}
	payload, _ := json.Marshal(executor.Approval{Approved: in.Approved, Answer: strings.TrimSpace(in.Answer)})
	if err := s.temporal.SignalWorkflow(ctx, id, "", executor.ApprovalSignal(seq), payload); err != nil {
		log.Printf("run %s: approval signal: %v", id, err)
		http.Error(w, "the agent runtime is unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// artifact serves a file a tool produced for the run (a screenshot), to the
// run's owner only.
func (s *server) artifact(w http.ResponseWriter, r *http.Request) {
	var mime string
	var data []byte
	err := s.db.QueryRow(r.Context(), `SELECT a.mime, a.data FROM run_artifacts a JOIN agent_runs r ON r.id = a.run_id
		WHERE a.id = $1 AND a.run_id = $2 AND r.user_id = $3`,
		r.PathValue("artifact"), r.PathValue("id"), currentUser(r).ID).Scan(&mime, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400") // an artifact never changes once its run is done
	_, _ = w.Write(data)
}

// runEvents streams a run's events as server-sent events: `id` is the event's
// seq and `data` the agent.Event. A reconnecting EventSource sends
// Last-Event-ID and gets only what came after it. The stream folds events
// through agent.Reduce and ends with `event: end` once the run is done or
// failed, or `event: error` if the run stopped without a final event.
func (s *server) runEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var owned bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = $1 AND user_id = $2)`,
		id, currentUser(r).ID).Scan(&owned); err != nil {
		s.fail(w, err)
		return
	}
	if !owned {
		http.NotFound(w, r)
		return
	}
	after := -1
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			after = n
		}
	}

	// Subscribe before the first read, so an event landing in between still
	// wakes this stream.
	wake, unsubscribe := s.hub.subscribe(id)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	st := agent.Start(nil)
	next := 0
	checkRun := false
	for {
		// Read the lifecycle before the events so a concurrent terminal
		// commit cannot be mistaken for a missing final event.
		var status *string
		var failure string
		if err := s.db.QueryRow(ctx, `SELECT status, COALESCE(failure, '') FROM agent_runs WHERE id = $1`, id).
			Scan(&status, &failure); err != nil {
			log.Printf("run %s: reading status: %v", id, err)
			return
		}
		stopped := status != nil && *status == runtime.Failed.String()
		if status == nil && checkRun {
			open, err := s.inspectWorkflow(ctx, id)
			stopped = err == nil && !open
		}
		checkRun = false

		rows, err := s.db.Query(ctx, `SELECT seq, event FROM run_events WHERE run_id = $1 AND seq >= $2 ORDER BY seq`, id, next)
		if err != nil {
			log.Printf("run %s: reading events: %v", id, err)
			return
		}
		for rows.Next() {
			var seq int
			var e agent.Event
			if err := rows.Scan(&seq, &e); err != nil || seq != next {
				break // a bad row or a gap: stop here and read again on the next wake
			}
			if st, err = agent.Reduce(st, e); err != nil {
				rows.Close()
				log.Printf("run %s: event %d: %v", id, seq, err)
				return
			}
			next++
			if seq > after {
				data, _ := json.Marshal(e)
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, data)
			}
		}
		rows.Close()

		switch {
		case st.Status == agent.Done || st.Status == agent.Stopped:
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			_ = rc.Flush()
			return
		case stopped:
			data, _ := json.Marshal(failure)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
			_ = rc.Flush()
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			checkRun = true
		}
	}
}

// hub fans Postgres notifications from run_events out to the open streams.
// The notification is only a wake-up: streams re-read the table, which is the
// source of truth.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool // run ID -> streams
}

func newHub() *hub { return &hub{subs: map[string]map[chan struct{}]bool{}} }

func (h *hub) subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan struct{}]bool{}
	}
	h.subs[runID][ch] = true
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs[runID], ch)
		if len(h.subs[runID]) == 0 {
			delete(h.subs, runID)
		}
	}
}

// wake nudges the streams following runID, or every stream if runID is "".
// A stream that already has a wake-up pending doesn't need a second one.
func (h *hub) wake(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, chans := range h.subs {
		if runID != "" && id != runID {
			continue
		}
		for ch := range chans {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// listen holds one dedicated connection on LISTEN run_events, reconnecting
// when it drops. Notifications sent while it was down are lost, so every
// stream is woken after a reconnect to re-read.
func (h *hub) listen(cfg *pgx.ConnConfig) {
	for {
		err := h.listenOnce(cfg)
		log.Printf("run events listener: %v; reconnecting", err)
		time.Sleep(time.Second)
	}
}

func (h *hub) listenOnce(cfg *pgx.ConnConfig) error {
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN run_events"); err != nil {
		return err
	}
	h.wake("")
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.wake(n.Payload)
	}
}
