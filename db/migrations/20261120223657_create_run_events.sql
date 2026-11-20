-- +goose Up
-- One row per agent run (a Temporal workflow ID), owned by the user who
-- started it. Not pgkernel's `runs` table, which is that engine's journal.
CREATE TABLE agent_runs (
    id         TEXT PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A run's agent events, in order. seq is the event's index in the run, so a
-- retried append writes the same key and is ignored (ON CONFLICT DO NOTHING).
CREATE TABLE run_events (
    run_id     TEXT        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq        INT         NOT NULL,
    event      JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

-- Wakes the server's event streams (LISTEN run_events); the payload is the
-- run ID, and listeners re-read the table. Skipped duplicates don't notify.
-- +goose StatementBegin
CREATE FUNCTION notify_run_event() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('run_events', NEW.run_id);
    RETURN NULL;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER run_events_notify AFTER INSERT ON run_events
    FOR EACH ROW EXECUTE FUNCTION notify_run_event();

-- +goose Down
DROP TABLE run_events;
DROP FUNCTION notify_run_event();
DROP TABLE agent_runs;
