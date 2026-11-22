-- +goose Up
CREATE TABLE schedules (
    id TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    conversation_id TEXT NOT NULL,
    source_key TEXT NOT NULL UNIQUE,
    prompt TEXT NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    timezone TEXT NOT NULL,
    next_check TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'fired', 'cancelled', 'missed')),
    workflow_input BYTEA NOT NULL,
    run_id TEXT UNIQUE,
    FOREIGN KEY (conversation_id, user_id) REFERENCES conversations (id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (run_id, user_id) REFERENCES agent_runs (id, user_id) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((status = 'fired') = (run_id IS NOT NULL))
);

CREATE INDEX schedules_due ON schedules (next_check, fire_at, id) WHERE status = 'pending';
CREATE INDEX schedules_by_owner ON schedules (user_id, fire_at DESC, id);

-- +goose Down
DROP TABLE schedules;
