-- +goose Up
CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, user_id)
);

CREATE INDEX conversations_by_owner ON conversations (user_id, created_at DESC, id);

ALTER TABLE agent_runs
    ADD COLUMN conversation_id TEXT,
    ADD COLUMN request_id TEXT,
    ADD COLUMN ordinal BIGINT GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN status TEXT CHECK (status IN ('queued', 'starting', 'running', 'succeeded', 'failed')),
    ADD COLUMN prompt TEXT,
    ADD COLUMN workflow_input BYTEA,
    ADD COLUMN next_check TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN start_deadline TIMESTAMPTZ,
    ADD COLUMN failure TEXT,
    ADD FOREIGN KEY (conversation_id, user_id) REFERENCES conversations (id, user_id) ON DELETE CASCADE,
    ADD UNIQUE (user_id, request_id),
    ADD CHECK (status IS NULL OR prompt IS NOT NULL);

CREATE UNIQUE INDEX agent_runs_one_active_per_chat ON agent_runs (conversation_id)
    WHERE status IN ('starting', 'running');
CREATE INDEX agent_runs_chat_order ON agent_runs (conversation_id, ordinal);
CREATE INDEX agent_runs_by_owner_state ON agent_runs (user_id, status);
CREATE INDEX agent_runs_pending_dispatch ON agent_runs (ordinal) WHERE status = 'queued';
CREATE INDEX agent_runs_pending_check ON agent_runs (next_check) WHERE status IN ('starting', 'running');

-- +goose Down
DROP INDEX agent_runs_pending_check, agent_runs_pending_dispatch, agent_runs_by_owner_state,
    agent_runs_chat_order, agent_runs_one_active_per_chat;
ALTER TABLE agent_runs
    DROP COLUMN failure,
    DROP COLUMN start_deadline,
    DROP COLUMN next_check,
    DROP COLUMN workflow_input,
    DROP COLUMN prompt,
    DROP COLUMN status,
    DROP COLUMN ordinal,
    DROP COLUMN request_id,
    DROP COLUMN conversation_id;
DROP TABLE conversations;
