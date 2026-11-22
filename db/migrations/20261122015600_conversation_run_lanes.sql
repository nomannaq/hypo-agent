-- +goose Up
DROP INDEX agent_runs_one_active_per_chat;
CREATE UNIQUE INDEX agent_runs_one_active_per_chat_lane
    ON agent_runs (conversation_id, background_research)
    WHERE status IN ('starting', 'running');

UPDATE agent_runs
SET background_research = TRUE
WHERE status IN ('queued', 'starting', 'running')
  AND convert_from(workflow_input, 'UTF8')::jsonb->>'workflow' = 'agent-scheduled-v1';

-- +goose Down
DROP INDEX agent_runs_one_active_per_chat_lane;
CREATE UNIQUE INDEX agent_runs_one_active_per_chat ON agent_runs (conversation_id)
    WHERE status IN ('starting', 'running');
