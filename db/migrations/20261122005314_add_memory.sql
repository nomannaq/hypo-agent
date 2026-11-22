-- +goose Up
ALTER TABLE users ADD COLUMN memory_summaries_since TIMESTAMPTZ;
ALTER TABLE agent_runs ADD COLUMN memory_summary_next TIMESTAMPTZ;
ALTER TABLE agent_runs ADD UNIQUE (id, user_id);

CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source_run TEXT NOT NULL,
    source_key TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('preference', 'note', 'run_summary')),
    content TEXT CHECK (length(content) BETWEEN 1 AND 2000),
    search_text TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', coalesce(content, ''))) STORED,
    embedding VECTOR(1024),
    embedding_model TEXT,
    embedding_next TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    FOREIGN KEY (source_run, user_id) REFERENCES agent_runs (id, user_id) ON DELETE CASCADE,
    CHECK ((deleted_at IS NULL) = (content IS NOT NULL)),
    CHECK ((embedding IS NULL) = (embedding_model IS NULL))
);

CREATE INDEX memories_live_owner ON memories (user_id, created_at DESC, id) WHERE deleted_at IS NULL;
CREATE INDEX memories_live_text ON memories USING GIN (search_text) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE memories;
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_id_user_id_key, DROP COLUMN memory_summary_next;
ALTER TABLE users DROP COLUMN memory_summaries_since;
