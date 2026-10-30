-- +goose Up
-- The persisted occurrence instants. A schedule row is the only source of a
-- scheduled run: the sweep re-reads next_due_at, advances it with one Write and
-- enqueues the occurrence in that same transaction, so a lost wake costs a sweep
-- period and never a second run. Instants are UTC unix seconds; a daily rule
-- keeps its IANA zone by name so the wall clock stays what the owner recorded
-- across DST and restarts.

CREATE TABLE schedule (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 64),
    kind        TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 64 AND kind NOT GLOB '*[^a-z0-9._-]*'),
    rule        TEXT NOT NULL CHECK (rule IN ('interval', 'daily')),
    interval_s  INTEGER CHECK (interval_s IS NULL OR interval_s BETWEEN 60 AND 2592000),
    local_time  TEXT CHECK (
        local_time IS NULL
        OR local_time GLOB '[01][0-9]:[0-5][0-9]'
        OR local_time GLOB '2[0-3]:[0-5][0-9]'
    ),
    zone        TEXT CHECK (zone IS NULL OR length(zone) BETWEEN 1 AND 64),
    next_due_at INTEGER NOT NULL,
    revision    INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    CHECK (
        (rule = 'interval' AND interval_s IS NOT NULL AND local_time IS NULL AND zone IS NULL)
        OR (rule = 'daily' AND interval_s IS NULL AND local_time IS NOT NULL AND zone IS NOT NULL)
    )
) STRICT;

CREATE INDEX schedule_due ON schedule (next_due_at);
