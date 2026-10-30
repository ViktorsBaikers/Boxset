-- +goose Up
-- Durable accepted work. A job row is the intent: it is written in the same
-- transaction as the domain change that needs it, so a crash leaves both or
-- neither, and a worker changes it only through one Write per transition.
-- Instants are UTC unix seconds. The two partial unique indexes are the
-- ownership rules: one open row per (kind, dedupe_key) and one live attempt per
-- concurrency_key.

CREATE TABLE job (
    id                 INTEGER PRIMARY KEY,
    kind               TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 64 AND kind NOT GLOB '*[^a-z0-9._-]*'),
    class              TEXT NOT NULL CHECK (class IN ('required', 'optional')),
    lane               TEXT NOT NULL CHECK (lane IN ('interactive', 'background')),
    dedupe_key         TEXT NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 128),
    concurrency_key    TEXT NOT NULL CHECK (length(concurrency_key) BETWEEN 1 AND 128),
    params             TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(params) AND length(params) <= 4096),
    state              TEXT NOT NULL CHECK (state IN ('queued', 'running', 'reconciling', 'succeeded', 'failed', 'review', 'expired')),
    reason_code        TEXT CHECK (reason_code IS NULL OR (length(reason_code) BETWEEN 1 AND 64 AND reason_code NOT GLOB '*[^a-z_]*')),
    reason_params      TEXT CHECK (reason_params IS NULL OR (json_valid(reason_params) AND length(reason_params) <= 1024)),
    attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts       INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 10),
    due_at             INTEGER NOT NULL,
    expires_at         INTEGER,
    lease_owner        TEXT,
    attempt_started_at INTEGER,
    created_by         INTEGER REFERENCES user (id) ON DELETE SET NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    revision           INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1)
) STRICT;

-- An open row and its dedupe key: a second enqueue of the same work returns the
-- id that is already open instead of creating a second effect.
CREATE UNIQUE INDEX job_open_dedupe ON job (kind, dedupe_key)
    WHERE state IN ('queued', 'running', 'reconciling', 'review');

-- One live attempt per key, held until the attempt provably stopped: running,
-- reconciling and review all keep it, so a late attempt can never mutate beside
-- a new one.
CREATE UNIQUE INDEX job_key_owner ON job (concurrency_key)
    WHERE state IN ('running', 'reconciling', 'review');

CREATE INDEX job_due ON job (lane, due_at) WHERE state = 'queued';

CREATE INDEX job_terminal ON job (updated_at) WHERE state IN ('succeeded', 'failed', 'expired');

CREATE INDEX job_interactive_creator ON job (created_by)
    WHERE lane = 'interactive' AND created_by IS NOT NULL;
