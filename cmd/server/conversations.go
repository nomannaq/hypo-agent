package main

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	inertia "github.com/romsar/gonertia/v2"

	"nomanqureshi.dev/hpyo-agent/internal/runtime"
)

type conversationLink struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type chatRun struct {
	ID        string         `json:"id"`
	Prompt    string         `json:"prompt"`
	Reply     string         `json:"reply"`
	Status    runtime.Status `json:"status"`
	Scheduled bool           `json:"scheduled,omitempty"`
}

func (s *server) agentPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := currentUser(r).ID
	rows, err := s.db.Query(ctx, `SELECT id, title FROM conversations WHERE user_id = $1
		ORDER BY created_at DESC, id`, owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	chats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[conversationLink])
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.URL.Query().Get("chat")
	runs := []chatRun{}
	if id != "" {
		found := false
		for _, chat := range chats {
			if chat.ID == id {
				found = true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		rows, err := s.db.Query(ctx, `SELECT r.id, r.prompt, r.status, CASE r.status
			WHEN 'succeeded' THEN COALESCE(final.event->>'text', '')
			WHEN 'failed' THEN COALESCE(r.failure, 'The run stopped without a reply.')
			ELSE '' END, EXISTS (SELECT 1 FROM schedules WHERE run_id = r.id)
			OR EXISTS (SELECT 1 FROM schedule_occurrences WHERE run_id = r.id)
			FROM agent_runs r LEFT JOIN LATERAL (
				SELECT event FROM run_events WHERE run_id = r.id ORDER BY seq DESC LIMIT 1
			) final ON r.status = 'succeeded'
			WHERE r.conversation_id = $1 AND r.user_id = $2 ORDER BY r.ordinal`, id, owner)
		if err != nil {
			s.fail(w, err)
			return
		}
		runs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (chatRun, error) {
			var run chatRun
			var raw string
			if err := row.Scan(&run.ID, &run.Prompt, &raw, &run.Reply, &run.Scheduled); err != nil {
				return run, err
			}
			return run, run.Status.UnmarshalText([]byte(raw))
		})
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	props := inertia.Props{"conversations": chats, "conversationID": id, "runs": runs, "newChat": r.URL.Query().Has("new")}
	s.render(w, r, "agent", props)
}
