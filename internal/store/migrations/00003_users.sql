-- +goose Up
-- Accounts and the one-time first-admin setup. A password is stored only as
-- its argon2id PHC string; the setup credential only as the SHA-256 of the
-- random token. Setup is open while no user row exists and consumed_at is
-- NULL; consuming it and inserting the first admin commit together, and the
-- consumed verifier stays only so a replay can be told apart from a wrong
-- token. Times are unix seconds.

CREATE TABLE user (
    id           INTEGER PRIMARY KEY,
    username     TEXT NOT NULL CHECK (length(username) BETWEEN 1 AND 64),
    username_key TEXT NOT NULL COLLATE BINARY UNIQUE CHECK (length(username_key) > 0),
    password_phc TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    disabled     INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    revision     INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE setup (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    token_sha256 TEXT NOT NULL CHECK (length(token_sha256) = 64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
    consumed_at  INTEGER
) STRICT;
