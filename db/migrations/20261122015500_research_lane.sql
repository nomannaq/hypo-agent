-- +goose Up
ALTER TABLE agent_runs ADD COLUMN background_research BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE agent_runs
SET background_research = TRUE
WHERE status IN ('queued', 'starting', 'running')
  AND convert_from(workflow_input, 'UTF8')::jsonb->>'workflow'
      IN ('agent-deep-research-v1', 'agent-deep-research-v2');

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN background_research;
