-- +goose Up
ALTER TABLE schedules
    ADD COLUMN repeat TEXT NOT NULL DEFAULT 'once' CHECK (repeat IN ('once', 'daily', 'weekly')),
    ADD COLUMN starts_at TIMESTAMPTZ;
UPDATE schedules SET starts_at = fire_at;
ALTER TABLE schedules ALTER COLUMN starts_at SET NOT NULL;

CREATE TABLE schedule_occurrences (
    schedule_id TEXT NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    scheduled_for TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('fired', 'missed')),
    reason TEXT CHECK (reason IN ('expired', 'overlap')),
    run_id TEXT UNIQUE REFERENCES agent_runs (id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (schedule_id, scheduled_for),
    CHECK ((status = 'fired' AND run_id IS NOT NULL AND reason IS NULL)
        OR (status = 'missed' AND run_id IS NULL AND reason IS NOT NULL))
);

-- +goose Down
DROP TABLE schedule_occurrences;
-- Stop pending series rather than convert them into surprise one-shot tasks.
UPDATE schedules SET status = 'cancelled' WHERE repeat <> 'once' AND status = 'pending';
ALTER TABLE schedules DROP COLUMN starts_at, DROP COLUMN repeat;
