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

// Older HTTP clients can still send their own message history.
type runRequest struct {
	ConversationID string               `json:"conversation_id"`
	RequestID      string               `json:"request_id"`
	Prompt         string               `json:"prompt"`
	DeepResearch   bool                 `json:"deep_research"`
	Messages       []chatHistoryMessage `json:"messages"`
	InitialHistory []chatHistoryMessage `json:"initial_history"`
}

type chatHistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var errInvalidRun = errors.New("invalid run request")

func (s *server) admitRun(ctx context.Context, owner int64, in runRequest) (runtime.Receipt, error) {
	req := runtime.Request{
		ID: "run-" + rand.Text(), Owner: owner, RequestID: in.RequestID,
		Input: executor.Input{Workflow: executor.ResponsiveWorkflowName, Skills: s.skills},
	}
	if in.DeepResearch {
		req.Input.Workflow = executor.DeepResearchWorkflowV4
	}
	if in.Messages != nil {
		n := len(in.Messages)
		if in.Prompt != "" || in.ConversationID != "" || in.RequestID != "" || len(in.InitialHistory) != 0 ||
			n == 0 || n > maxChatMessages || in.Messages[n-1].Role != "user" {
			return runtime.Receipt{}, errInvalidRun
		}
		for _, m := range in.Messages {
			if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
				return runtime.Receipt{}, errInvalidRun
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
			return runtime.Receipt{}, errInvalidRun
		}
		req.ConversationID = in.ConversationID
		if len(in.InitialHistory) > 0 {
			if req.ConversationID != "" || len(in.InitialHistory) >= maxChatMessages {
				return runtime.Receipt{}, errInvalidRun
			}
			var size int
			for _, m := range in.InitialHistory {
				size += len(m.Content)
				if size > 128<<10 || (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
					return runtime.Receipt{}, errInvalidRun
				}
				req.Input.History = append(req.Input.History, agent.Message{Role: agent.Role(m.Role), Text: m.Content})
			}
		}
		if req.ConversationID == "" {
			req.NewConversationID = "chat-" + rand.Text()
		}
	}
	return s.scheduler.Admit(ctx, req)
}

func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	var in runRequest
	if !s.decode(w, r, &in) {
		return
	}
	receipt, err := s.admitRun(r.Context(), currentUser(r).ID, in)

	switch {
	case errors.Is(err, errInvalidRun):
		http.Error(w, "bad request", http.StatusBadRequest)
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
	case executor.MemoryWorkflowName, executor.SkillsWorkflowName, executor.SchedulingWorkflowName, executor.ScheduledWorkflowName, executor.RecurringWorkflowName, executor.MessagingWorkflowName, executor.DiscoveryWorkflowName, executor.DeepResearchWorkflowName, executor.DeepResearchWorkflowV2, executor.DeepResearchWorkflowV3, executor.DeepResearchWorkflowV4, executor.OrchestratorWorkflowName, executor.ConversationalWorkflowName, executor.ResponsiveWorkflowName, executor.ScheduledWorkflowV2:
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

func (s *server) workflowCloseReason(ctx context.Context, id string) string {
	history := s.temporal.GetWorkflowHistory(ctx, id, "", false, enums.HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT)
	if !history.HasNext() {
		return ""
	}
	event, err := history.Next()
	if err != nil {
		return ""
	}
	switch event.GetEventType() {
	case enums.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
		return "The run was terminated before it could finish."
	case enums.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
		return "The run was cancelled before it could finish."
	case enums.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
		return "The run reached its time limit before it could finish."
	case enums.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
		return "The workflow failed before it could publish a final reply."
	default:
		return ""
	}
}

func (s *server) requestRunCancellation(w http.ResponseWriter, r *http.Request) {
	found, err := s.scheduler.RequestCancellation(r.Context(), r.PathValue("id"), currentUser(r).ID)
	switch {
	case err != nil:
		s.fail(w, err)
	case !found:
		http.NotFound(w, r)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *server) cancelWorkflow(ctx context.Context, id string) error {
	err := s.temporal.CancelWorkflow(ctx, id, "")
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return runtime.ErrWorkflowMissing
	}
	var alreadyRequested *serviceerror.CancellationAlreadyRequested
	if errors.As(err, &alreadyRequested) {
		return nil
	}
	return err
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	s.followRun(ctx, id, after, func(frame runFrame) error {
		switch frame.Type {
		case "event":
			data, _ := json.Marshal(frame.Event)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", frame.Seq, data)
		case "end":
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
		case "error":
			data, _ := json.Marshal(frame.Error)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		case "ping":
			fmt.Fprint(w, ": ping\n\n")
		}
		return rc.Flush()
	})
}

// followRun replays the durable journal and then tails it. Transport callbacks
// are synchronous to preserve event order within a run.
func (s *server) followRun(ctx context.Context, id string, after int, send func(runFrame) error) {
	// Subscribe before the first read, so an event landing in between still
	// wakes this stream.
	wake, unsubscribe := s.hub.subscribe(id)
	defer unsubscribe()

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	st := agent.Start(nil)
	next := 0
	checkRun := false
	var previous runLifecycle
	for {
		// Read the lifecycle before the events so a concurrent terminal
		// commit cannot be mistaken for a missing final event.
		var status *string
		var failure string
		var lifecycle runLifecycle
		if err := s.db.QueryRow(ctx, `SELECT r.status, COALESCE(r.failure, ''), r.background_research, r.cancel_requested,
			CASE WHEN r.status <> 'queued' OR r.status IS NULL THEN ''
			WHEN (SELECT count(*) FROM agent_runs active WHERE active.user_id = r.user_id
				AND active.background_research = r.background_research AND active.status IN ('starting', 'running'))
				>= CASE WHEN r.background_research THEN $3::int ELSE $2::int END THEN 'capacity'
			ELSE '' END
			FROM agent_runs r WHERE r.id = $1`, id, runtime.MaxForegroundRuns, runtime.MaxBackgroundRuns).
			Scan(&status, &failure, &lifecycle.BackgroundResearch, &lifecycle.CancelRequested, &lifecycle.WaitingFor); err != nil {
			log.Printf("run %s: reading status: %v", id, err)
			return
		}
		if status != nil {
			lifecycle.Status = *status
			if lifecycle != previous {
				if err := send(runFrame{Type: "lifecycle", RunID: id, Lifecycle: &lifecycle}); err != nil {
					return
				}
				previous = lifecycle
			}
		}

		stopped := status != nil && *status == runtime.Failed.String()
		if status == nil && checkRun {
			open, err := s.inspectWorkflow(ctx, id)
			stopped = err == nil && !open
		}
		checkRun = false

		rows, err := s.db.Query(ctx, `SELECT e.seq, e.event, COALESCE(m.body, '')
			FROM run_events e LEFT JOIN run_assistant_messages($1, $2) m USING (seq)
			WHERE e.run_id = $1 AND e.seq >= $2 ORDER BY e.seq`, id, next)
		if err != nil {
			log.Printf("run %s: reading events: %v", id, err)
			return
		}
		for rows.Next() {
			var seq int
			var e agent.Event
			var message string
			if err := rows.Scan(&seq, &e, &message); err != nil || seq != next {
				break // a bad row or a gap: stop here and read again on the next wake
			}
			if st, err = agent.Reduce(st, e); err != nil {
				rows.Close()
				log.Printf("run %s: event %d: %v", id, seq, err)
				return
			}
			next++
			if seq > after {
				if err := send(runFrame{Type: "event", RunID: id, Seq: seq, Event: &publicRunEvent{e, message}}); err != nil {
					rows.Close()
					return
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("run %s: scanning events: %v", id, err)
			return
		}

		switch {
		case st.Status == agent.Done || st.Status == agent.Stopped:
			_ = send(runFrame{Type: "end", RunID: id})
			return
		case stopped:
			_ = send(runFrame{Type: "error", RunID: id, Error: failure})
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ping.C:
			if err := send(runFrame{Type: "ping", RunID: id}); err != nil {
				return
			}
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
