-- +goose Up
ALTER TABLE agent_runs ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN cancel_requested;
