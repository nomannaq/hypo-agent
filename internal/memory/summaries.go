package memory

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

type Summarize func(ctx context.Context, prompt, reply string) (string, error)

func (s Store) Maintain(ctx context.Context, summarize Summarize) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if err := s.summarizeOne(ctx, summarize); err != nil && ctx.Err() == nil {
			log.Printf("memory summary: %v", err)
		}
		if s.Embedder != nil {
			if err := s.embedOne(ctx); err != nil && ctx.Err() == nil {
				log.Printf("memory embedding: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s Store) summarizeOne(ctx context.Context, summarize Summarize) error {
	var run, prompt, reply string
	var epoch time.Time
	err := s.DB.QueryRow(ctx, `WITH due AS (
		SELECT r.id, u.memory_summaries_since AS epoch
		FROM agent_runs r JOIN users u ON r.user_id = u.id
		WHERE r.status = 'succeeded' AND r.conversation_id IS NOT NULL AND r.created_at >= u.memory_summaries_since
			AND (r.memory_summary_next IS NULL OR r.memory_summary_next <= now())
			AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.source_key = 'summary/' || r.id)
		ORDER BY r.ordinal LIMIT 1 FOR UPDATE OF r SKIP LOCKED
	), claimed AS (
		UPDATE agent_runs r SET memory_summary_next = now() + interval '5 minutes'
		FROM due WHERE r.id = due.id RETURNING r.id, r.prompt, due.epoch
	)
	SELECT c.id, c.prompt, COALESCE(final.event->>'text', ''), c.epoch FROM claimed c
	JOIN LATERAL (SELECT event FROM run_events WHERE run_id = c.id ORDER BY seq DESC LIMIT 1) final ON true`,
	).Scan(&run, &prompt, &reply, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	task, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	summary, err := summarize(task, bounded(prompt, 2000), bounded(reply, 4000))
	if err != nil {
		return err
	}
	summary = bounded(summary, 1200)
	if summary == "" {
		return errors.New("summary model returned no text")
	}
	// Recheck consent after the network call. A deleted summary retains
	// this key, so duplicate jobs cannot bring it back.
	_, err = s.DB.Exec(ctx, `INSERT INTO memories (id, user_id, source_run, source_key, kind, content)
		SELECT 'summary-' || r.id, r.user_id, r.id, 'summary/' || r.id, $3, $4
		FROM agent_runs r JOIN users u ON u.id = r.user_id
		WHERE r.id = $1 AND u.memory_summaries_since = $2 FOR SHARE OF u
		ON CONFLICT (source_key) DO NOTHING`, run, epoch, RunSummary.String(), summary)
	return err
}
