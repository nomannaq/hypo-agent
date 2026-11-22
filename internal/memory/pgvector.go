// Package memory stores approved notes and opt-in summaries and searches the
// owning user's transcript. Workflow code reaches it through activities.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const resultLimit = 5

type Store struct {
	DB       *pgxpool.Pool
	Embedder *Embedder
}

type Entry struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	Content string `json:"content"`
	Source  string `json:"source_run"`
	ChatID  string `json:"conversation_id,omitempty"`
}

func rootRun(id string) string {
	root, _, _ := strings.Cut(id, "/")
	return root
}

func (s Store) remember(ctx context.Context, run, key string, kind Kind, content string) (string, error) {
	run = rootRun(run)
	content = strings.TrimSpace(content)
	if run == "" || key == "" || (kind != Preference && kind != Note) || content == "" || len(content) > 2000 {
		return "", fmt.Errorf("remember requires a preference or note of 1–2000 bytes")
	}
	return s.save(ctx, run, key, kind, content)
}

func (s Store) save(ctx context.Context, run, key string, kind Kind, content string) (string, error) {
	// A retried activity can arrive after the note was deleted. Keep the
	// original key and return its receipt without restoring its contents.
	var id string
	err := s.DB.QueryRow(ctx, `INSERT INTO memories (id, user_id, source_run, source_key, kind, content)
		SELECT $1, r.user_id, r.id, $3, $4, $5 FROM agent_runs r WHERE r.id = $2
		ON CONFLICT (source_key) DO UPDATE SET source_key = memories.source_key
		WHERE memories.user_id = EXCLUDED.user_id AND memories.source_run = EXCLUDED.source_run
		RETURNING id`, "mem-"+rand.Text(), run, key, kind.String(), content).Scan(&id)
	return id, err
}

func (s Store) List(ctx context.Context, owner int64) ([]Entry, error) {
	rows, err := s.DB.Query(ctx, `SELECT m.id, m.kind, m.content, m.source_run, COALESCE(r.conversation_id, '')
		FROM memories m JOIN agent_runs r ON r.id = m.source_run WHERE m.user_id = $1 AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC, m.id`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEntry)
}

func scanEntry(row pgx.CollectableRow) (Entry, error) {
	var entry Entry
	var kind string
	if err := row.Scan(&entry.ID, &kind, &entry.Content, &entry.Source, &entry.ChatID); err != nil {
		return Entry{}, err
	}
	return entry, entry.Kind.UnmarshalText([]byte(kind))
}

func (s Store) Delete(ctx context.Context, owner int64, id string) (bool, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE memories SET deleted_at = COALESCE(deleted_at, now()),
		content = NULL, embedding = NULL, embedding_model = NULL, embedding_next = NULL WHERE id = $1 AND user_id = $2`, id, owner)
	return tag.RowsAffected() == 1, err
}

func (s Store) SummariesEnabled(ctx context.Context, owner int64) (bool, error) {
	var enabled bool
	err := s.DB.QueryRow(ctx, "SELECT memory_summaries_since IS NOT NULL FROM users WHERE id = $1", owner).Scan(&enabled)
	return enabled, err
}

func (s Store) SetSummaries(ctx context.Context, owner int64, enabled bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET memory_summaries_since =
		CASE WHEN $2 THEN COALESCE(memory_summaries_since, clock_timestamp()) ELSE NULL END WHERE id = $1`, owner, enabled)
	return err
}

func (s Store) Recall(ctx context.Context, run, query string) (string, error) {
	return s.recall(ctx, run, query, true)
}

func (s Store) recall(ctx context.Context, run, query string, automatic bool) (string, error) {
	query = bounded(query, 1000)
	// Explicit queries honor search operators. Automatic queries can be
	// whole prompts, so match any of the prompt's terms.
	rows, err := s.DB.Query(ctx, `WITH terms AS (
			SELECT CASE WHEN $3 THEN COALESCE((SELECT string_agg(quote_literal(word), ' | ')::tsquery
				FROM unnest(tsvector_to_array(to_tsvector('simple', $2))) AS word), websearch_to_tsquery('simple', ''))
			ELSE websearch_to_tsquery('simple', $2) END AS q
		)
		SELECT m.id, m.kind, m.content, m.source_run, COALESCE(source.conversation_id, '')
		FROM agent_runs caller JOIN memories m ON m.user_id = caller.user_id
		JOIN agent_runs source ON source.id = m.source_run CROSS JOIN terms
		WHERE caller.id = $1 AND m.deleted_at IS NULL AND (m.search_text @@ terms.q OR ($3 AND m.kind = 'preference'))
		ORDER BY (m.search_text @@ terms.q) DESC, ts_rank(m.search_text, terms.q) DESC, m.created_at DESC, m.id
		LIMIT $4`, rootRun(run), query, automatic, resultLimit)
	if err != nil {
		return "", err
	}
	entries, err := pgx.CollectRows(rows, scanEntry)
	if err != nil {
		return "", err
	}
	if s.Embedder != nil && len(entries) < resultLimit && query != "" {
		entries = s.addSemanticMatches(ctx, rootRun(run), query, entries)
	}
	if len(entries) == 0 {
		return "", nil
	}
	for i := range entries {
		entries[i].Content = bounded(entries[i].Content, 500)
	}
	data, err := json.Marshal(entries)
	return string(data), err
}

func (s Store) addSemanticMatches(ctx context.Context, run, query string, entries []Entry) []Entry {
	vector, err := s.Embedder.vector(ctx, query)
	if err != nil {
		return entries
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	// ponytail: rank the owner's small memory set exactly. A partitioned
	// approximate index can help once individual accounts store many notes.
	rows, err := s.DB.Query(ctx, `WITH owned AS MATERIALIZED (
		SELECT m.id, m.kind, m.content, m.source_run, m.embedding, COALESCE(source.conversation_id, '') AS chat
		FROM agent_runs caller JOIN memories m ON m.user_id = caller.user_id
		JOIN agent_runs source ON source.id = m.source_run
		WHERE caller.id = $1 AND m.deleted_at IS NULL AND m.embedding_model = $2 AND NOT (m.id = ANY($4::text[]))
	)
	SELECT id, kind, content, source_run, chat FROM owned WHERE (embedding <=> $3::text::vector) <= 0.55
	ORDER BY embedding <=> $3::text::vector, id LIMIT $5`, run, s.Embedder.Model, vector, ids, resultLimit-len(entries))
	if err != nil {
		return entries
	}
	matches, err := pgx.CollectRows(rows, scanEntry)
	if err != nil {
		return entries
	}
	return append(entries, matches...)
}

func bounded(text string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(text))
	return string(runes[:min(len(runes), maxRunes)])
}
