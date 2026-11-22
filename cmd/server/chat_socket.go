package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/runtime"
)

type publicRunEvent struct {
	agent.Event
	AssistantMessage string `json:"assistant_message,omitempty"`
}

// The socket carries admissions and subscriptions. A request ID correlates
// admissions and makes them safe to resend after a lost acknowledgement.
type chatCommand struct {
	runRequest
	Type  string `json:"type"`
	RunID string `json:"run_id"`
	After *int   `json:"after"`
}

type runLifecycle struct {
	Status             string `json:"status"`
	BackgroundResearch bool   `json:"background_research"`
	CancelRequested    bool   `json:"cancel_requested"`
	WaitingFor         string `json:"waiting_for,omitempty"`
}

type runFrame struct {
	Type           string          `json:"type"`
	RunID          string          `json:"run_id,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Seq            int             `json:"seq"`
	Event          *publicRunEvent `json:"event,omitempty"`
	Lifecycle      *runLifecycle   `json:"lifecycle,omitempty"`
	Error          string          `json:"error,omitempty"`
}

func (s *server) chatSocket(w http.ResponseWriter, r *http.Request) {
	owner := currentUser(r)
	if owner == nil {
		http.Error(w, "sign in to chat", http.StatusUnauthorized)
		return
	}
	// An upgrade is a GET, so it needs its own origin check before the
	// authenticated socket can accept write commands.
	origin := r.Header.Get("Origin")
	if origin != "http://"+r.Host && origin != "https://"+r.Host {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}
	authenticated := websocket.Server{
		Handler: func(conn *websocket.Conn) { s.serveChat(conn, r.Context(), owner.ID) },
	}
	authenticated.ServeHTTP(w, r)
}

func (s *server) serveChat(conn *websocket.Conn, parent context.Context, owner int64) {
	conn.MaxPayloadBytes = 1 << 20
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var writeMu, watchMu sync.Mutex
	watching := map[string]bool{}
	send := func(frame runFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := websocket.JSON.Send(conn, frame)
		if err != nil {
			_ = conn.Close()
		}
		return err
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		var cmd chatCommand
		if err := websocket.JSON.Receive(conn, &cmd); err != nil {
			return
		}
		switch cmd.Type {
		case "ping":
			if send(runFrame{Type: "pong"}) != nil {
				return
			}
		case "submit":
			var receipt runtime.Receipt
			err := errInvalidRun
			if cmd.Messages == nil {
				receipt, err = s.admitRun(ctx, owner, cmd.runRequest)
			}
			frame := runFrame{Type: "accepted", RequestID: cmd.RequestID, RunID: receipt.ID, ConversationID: receipt.ConversationID}
			if err != nil {
				frame.Type, frame.Error = "rejected", chatAdmissionError(err)
			}
			if send(frame) != nil {
				return
			}
		case "watch":
			watchMu.Lock()
			duplicate, full := watching[cmd.RunID], len(watching) >= 16
			watchMu.Unlock()
			if duplicate {
				continue
			}
			var owned bool
			if !full && len(cmd.RunID) <= 64 {
				err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = $1 AND user_id = $2)`, cmd.RunID, owner).Scan(&owned)
				if err != nil {
					log.Printf("chat socket run ownership: %v", err)
					return
				}
			}
			if !owned {
				if send(runFrame{Type: "error", RunID: cmd.RunID, Error: "This run is unavailable."}) != nil {
					return
				}
				continue
			}
			watchMu.Lock()
			watching[cmd.RunID] = true
			watchMu.Unlock()
			after := -1
			if cmd.After != nil {
				after = max(-1, *cmd.After)
			}
			go func(id string, after int) {
				defer func() {
					watchMu.Lock()
					delete(watching, id)
					watchMu.Unlock()
				}()
				terminal := false
				s.followRun(ctx, id, after, func(frame runFrame) error {
					terminal = frame.Type == "end" || frame.Type == "error"
					return send(frame)
				})
				if !terminal && ctx.Err() == nil {
					_ = conn.Close() // reconnect and resume on a transient journal read failure
				}
			}(cmd.RunID, after)
		default:
			return
		}
	}
}

func chatAdmissionError(err error) string {
	switch {
	case errors.Is(err, runtime.ErrQueueFull):
		return "Your run queue is full. Try again when a run finishes."
	case errors.Is(err, runtime.ErrMissingConversation):
		return "This conversation is unavailable."
	case errors.Is(err, runtime.ErrRequestConflict):
		return "This request was already used for a different prompt."
	case errors.Is(err, errInvalidRun):
		return "The prompt could not be queued. Check its length and try again."
	default:
		log.Printf("chat admission: %v", err)
		return "The agent could not start. Please try again."
	}
}
