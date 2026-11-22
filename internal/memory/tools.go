package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

const (
	RememberTool      = "remember"
	RecallTool        = "recall"
	SessionSearchTool = "session_search"
)

func (s Store) Tools() tool.Registry {
	remember := tool.Typed(RememberTool,
		"Save a user preference or note for future chats. Ask for approval using this tool when the user requests remembering something. Save only the requested information.",
		`{"type":"object","properties":{"kind":{"type":"string","enum":["preference","note"]},"content":{"type":"string","maxLength":2000}},"required":["kind","content"]}`,
		func(ctx context.Context, in struct {
			Kind    Kind   `json:"kind"`
			Content string `json:"content"`
		}) (string, error) {
			id, err := s.remember(ctx, tool.RunID(ctx), tool.CallKey(ctx), in.Kind, in.Content)
			if err != nil {
				return "", err
			}
			return "Saved memory " + id, nil
		})
	remember.Effect = tool.Write
	type queryArgs struct {
		Query string `json:"query"`
	}
	const schema = `{"type":"object","properties":{"query":{"type":"string","maxLength":1000}},"required":["query"]}`
	recall := tool.Typed(RecallTool, "Search the user's saved preferences, notes, and run summaries. Results include the source run and conversation.", schema,
		func(ctx context.Context, in queryArgs) (string, error) {
			if strings.TrimSpace(in.Query) == "" {
				return "", errors.New("recall needs a query")
			}
			out, err := s.recall(ctx, tool.RunID(ctx), in.Query, false)
			if out == "" && err == nil {
				return "No matching memories.", nil
			}
			return out, err
		})
	sessionSearch := tool.Typed(SessionSearchTool, "Search the user's past messages and final assistant replies. Returns excerpts and relative chat links.", schema,
		func(ctx context.Context, in queryArgs) (string, error) {
			return s.sessionSearch(ctx, tool.RunID(ctx), in.Query)
		})
	return tool.Registry{remember, recall, sessionSearch}
}

func (s Store) sessionSearch(ctx context.Context, run, query string) (string, error) {
	query = bounded(query, 1000)
	if query == "" {
		return "", errors.New("session_search needs a query")
	}
	rows, err := s.DB.Query(ctx, `WITH query AS (SELECT websearch_to_tsquery('simple', $2) AS q)
		SELECT r.id, r.conversation_id, transcript.role,
			ts_headline('simple', transcript.content, query.q, 'StartSel=[, StopSel=], MaxFragments=1, MaxWords=40, MinWords=15')
		FROM agent_runs caller JOIN agent_runs r ON r.user_id = caller.user_id
		LEFT JOIN LATERAL (
			SELECT event->>'text' AS reply FROM run_events
			WHERE run_id = r.id AND event->>'kind' = 'model_reply'
				AND COALESCE(jsonb_array_length(event->'tool_calls'), 0) = 0
			ORDER BY seq DESC LIMIT 1
		) final ON r.status = 'succeeded'
		CROSS JOIN LATERAL (VALUES ('user', r.prompt), ('assistant', final.reply)) transcript(role, content)
		CROSS JOIN query
		WHERE caller.id = $1 AND r.id <> caller.id AND r.conversation_id IS NOT NULL
			AND to_tsvector('simple', transcript.content) @@ query.q
		ORDER BY ts_rank(to_tsvector('simple', transcript.content), query.q) DESC, r.ordinal DESC, transcript.role
		LIMIT $3`, rootRun(run), query, resultLimit)
	if err != nil {
		return "", err
	}
	type match struct {
		Run     string `json:"run_id"`
		Role    string `json:"role"`
		Excerpt string `json:"excerpt"`
		URL     string `json:"url"`
	}
	matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (match, error) {
		var m match
		var chat string
		if err := row.Scan(&m.Run, &chat, &m.Role, &m.Excerpt); err != nil {
			return match{}, err
		}
		m.Excerpt = bounded(m.Excerpt, 450)
		m.URL = (&url.URL{Path: "/agent", RawQuery: url.Values{"chat": {chat}}.Encode(), Fragment: m.Run}).String()
		return m, nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "No matching conversation messages.", nil
	}
	data, err := json.Marshal(matches)
	return string(data), err
}
