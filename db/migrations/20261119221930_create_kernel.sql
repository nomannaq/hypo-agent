-- +goose Up
CREATE TABLE runs (
    id          TEXT PRIMARY KEY,
    workflow    TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'pending', -- pending|running|waiting|done|failed
    input       JSONB       NOT NULL DEFAULT '{}',
    output      JSONB,
    error       TEXT,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    epoch       BIGINT      NOT NULL DEFAULT 0,
    parent_id   TEXT        REFERENCES runs (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Claim query: SELECT ... WHERE status IN ('pending','running') AND
-- (lease_until IS NULL OR lease_until < now()) ... FOR UPDATE SKIP LOCKED.
CREATE INDEX runs_claimable_idx ON runs (status, lease_until)
    WHERE status IN ('pending', 'running');

CREATE TABLE journal (
    run_id     TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq        INT         NOT NULL,
    name       TEXT        NOT NULL,
    input_hash TEXT        NOT NULL, -- mismatch on replay = nondeterminism bug
    output     BYTEA,
    status     TEXT        NOT NULL DEFAULT 'done', -- done|failed
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE timers (
    run_id  TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq     INT         NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    fired   BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, seq)
);

-- Poller: SELECT ... WHERE NOT fired AND fire_at <= now().
CREATE INDEX timers_fire_at_idx ON timers (fire_at) WHERE NOT fired;

CREATE TABLE signals (
    run_id     TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    payload    BYTEA,
    consumed   BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX signals_unconsumed_idx ON signals (run_id, name) WHERE NOT consumed;

-- +goose Down
DROP TABLE signals;
DROP TABLE timers;
DROP TABLE journal;
DROP TABLE runs;
