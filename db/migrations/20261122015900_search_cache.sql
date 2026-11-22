-- +goose Up
-- web_search results by normalized query, shared across runs for a day so
-- repeated research queries don't hit the search backends again.
CREATE TABLE search_cache (
    key        TEXT PRIMARY KEY,
    output     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX search_cache_created_at ON search_cache (created_at);

-- +goose Down
DROP TABLE search_cache;
