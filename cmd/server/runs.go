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
)

const maxChatMessages = 100

// pingEvery keeps idle streams open through proxies and is how often a stream
// checks whether its run died without a final event.
const pingEvery = 15 * time.Second

// startRun starts one agent turn as a durable run on Temporal and returns its
// ID without waiting. The run lives on the worker; the UI follows it through
// runEvents and can reattach after a reload.
func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	n := len(in.Messages)
	if n == 0 || n > maxChatMessages || in.Messages[n-1].Role != "user" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	history := make([]agent.Message, 0, n)
	for _, m := range in.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		history = append(history, agent.Message{Role: agent.Role(m.Role), Text: m.Content})
	}

	id := "run-" + rand.Text()
	// The row comes first: the worker's first event references it.
	if _, err := s.db.Exec(r.Context(), `INSERT INTO agent_runs (id, user_id) VALUES ($1, $2)`, id, currentUser(r).ID); err != nil {
		s.fail(w, err)
		return
	}
	input, _ := json.Marshal(executor.Input{History: history[:n-1], Prompt: history[n-1].Text})
	_, err := s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:                       id,
		TaskQueue:                temporalkernel.TaskQueue,
		WorkflowExecutionTimeout: 30 * time.Minute, // backstop; the executor's guards end runs long before
	}, executor.WorkflowName, input)
	if err != nil {
		log.Printf("starting run: %v", err)
		_, _ = s.db.Exec(context.WithoutCancel(r.Context()), `DELETE FROM agent_runs WHERE id = $1`, id)
		var unavailable *serviceerror.Unavailable
		if errors.As(err, &unavailable) {
			http.Error(w, "the agent runtime is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
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
		// Checked before reading, so a run that finished between the two
		// still has its last events read before being called dead.
		stopped := checkRun && !s.running(ctx, id)
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
			fmt.Fprint(w, "event: error\ndata: the run stopped without a reply\n\n")
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

// running reports whether the run's workflow is still going. If Temporal
// can't be asked, the run is assumed alive and checked again on the next ping.
func (s *server) running(ctx context.Context, id string) bool {
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		return true
	}
	return desc.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_RUNNING
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
