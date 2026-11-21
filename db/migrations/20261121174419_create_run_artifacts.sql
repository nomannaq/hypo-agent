-- +goose Up
-- Files tools produce for the user (browser screenshots), served to the run's
-- owner by GET /api/runs/{run}/artifacts/{id}. key is the producing call's
-- idempotency key, so a retried call overwrites instead of duplicating.
-- run_id is the top-level run: a subagent's artifacts belong to its parent.
CREATE TABLE run_artifacts (
    id         TEXT PRIMARY KEY,
    key        TEXT        NOT NULL UNIQUE,
    run_id     TEXT        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    mime       TEXT        NOT NULL,
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE run_artifacts;
