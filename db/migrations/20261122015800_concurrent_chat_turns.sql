-- +goose Up
-- Turns in one conversation may run at the same time. Account caps still apply.
DROP INDEX agent_runs_one_active_per_chat_lane;

-- +goose Down
CREATE UNIQUE INDEX agent_runs_one_active_per_chat_lane
    ON agent_runs (conversation_id, background_research)
    WHERE status IN ('starting', 'running');
