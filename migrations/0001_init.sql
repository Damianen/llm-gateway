-- Initial schema: projects, API keys, request log, response cache.
-- Timestamps are fixed-width UTC ("YYYY-MM-DDTHH:MM:SS.mmmZ") so
-- lexicographic comparison equals chronological order.

CREATE TABLE projects (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE api_keys (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash      TEXT    NOT NULL UNIQUE,
    name          TEXT    NOT NULL DEFAULT '',
    project_id    INTEGER NOT NULL REFERENCES projects(id),
    rpm           INTEGER NOT NULL DEFAULT 0,
    tpm           INTEGER NOT NULL DEFAULT 0,
    cache_default INTEGER NOT NULL DEFAULT 0,
    revoked       INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_api_keys_project ON api_keys(project_id);

CREATE TABLE requests (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             TEXT    NOT NULL,
    project_id     INTEGER NOT NULL REFERENCES projects(id),
    key_id         INTEGER NOT NULL REFERENCES api_keys(id),
    endpoint       TEXT    NOT NULL,
    model          TEXT    NOT NULL,
    provider       TEXT    NOT NULL,
    upstream_model TEXT    NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_usd       REAL    NOT NULL DEFAULT 0,
    latency_ms     INTEGER NOT NULL DEFAULT 0,
    status         INTEGER NOT NULL,
    cache_hit      INTEGER NOT NULL DEFAULT 0,
    fallback_used  INTEGER NOT NULL DEFAULT 0,
    stream         INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_requests_ts ON requests(ts);
CREATE INDEX idx_requests_project_ts ON requests(project_id, ts);

CREATE TABLE cache (
    key        TEXT    PRIMARY KEY,
    model      TEXT    NOT NULL,
    response   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX idx_cache_expires ON cache(expires_at);
