-- +goose Up
CREATE TABLE session (
    id INTEGER PRIMARY KEY,
    token_sha256 TEXT NOT NULL UNIQUE CHECK (length(token_sha256) = 64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    epoch INTEGER NOT NULL CHECK (epoch >= 1),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;
