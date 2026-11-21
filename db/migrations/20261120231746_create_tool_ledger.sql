-- +goose Up
-- Outputs of tool calls with side effects (tool.Write), keyed by run ID +
-- step. A retried activity finds its key here and returns the recorded
-- output instead of repeating the effect (executor.Runner.Ledger).
CREATE TABLE tool_ledger (
    key        TEXT PRIMARY KEY,
    run_id     TEXT        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    output     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE tool_ledger;
