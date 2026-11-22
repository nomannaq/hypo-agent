package main

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	inertia "github.com/romsar/gonertia/v2"

	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/runtime"
)

type conversationLink struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type chatRun struct {
	ID                 string          `json:"id"`
	Prompt             string          `json:"prompt"`
	Events             json.RawMessage `json:"events"`
	Failure            string          `json:"failure"`
	Status             runtime.Status  `json:"status"`
	Scheduled          bool            `json:"scheduled,omitempty"`
	DeepResearch       bool            `json:"deep_research,omitempty"`
	BackgroundResearch bool            `json:"background_research,omitempty"`
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
			WHEN 'failed' THEN COALESCE(r.failure, 'The run stopped without a reply.')
			ELSE '' END, CASE WHEN r.status IN ('succeeded', 'failed') THEN COALESCE((
				SELECT jsonb_agg(
					CASE WHEN e.event->>'kind' = 'tool_result' THEN
						(e.event - 'text') || jsonb_strip_nulls(jsonb_build_object(
							'text', substring(e.event->>'text' FROM '\[artifact:a-[A-Z2-7]+\]$')))
					ELSE e.event END ||
					jsonb_strip_nulls(jsonb_build_object('seq', e.seq, 'assistant_message', m.body))
					ORDER BY e.seq)
				FROM run_events e LEFT JOIN run_assistant_messages(r.id) m USING (seq)
				WHERE e.run_id = r.id AND e.event->>'kind' <> 'user_message'
			), '[]'::jsonb) ELSE '[]'::jsonb END, EXISTS (SELECT 1 FROM schedules WHERE run_id = r.id)
			OR EXISTS (SELECT 1 FROM schedule_occurrences WHERE run_id = r.id), r.background_research,
			COALESCE(convert_from(r.workflow_input, 'UTF8')::jsonb->>'workflow', '')
			FROM agent_runs r
			WHERE r.conversation_id = $1 AND r.user_id = $2 ORDER BY r.ordinal`, id, owner)
		if err != nil {
			s.fail(w, err)
			return
		}
		runs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (chatRun, error) {
			var run chatRun
			var raw, workflow string
			if err := row.Scan(&run.ID, &run.Prompt, &raw, &run.Failure, &run.Events, &run.Scheduled, &run.BackgroundResearch, &workflow); err != nil {
				return run, err
			}
			run.DeepResearch = executor.IsDeepResearchWorkflow(workflow)
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
