-- +goose Up
CREATE TABLE api_key (
    id INTEGER PRIMARY KEY,
    token_sha256 TEXT NOT NULL UNIQUE CHECK (length(token_sha256) = 64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    scope TEXT NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
    epoch INTEGER NOT NULL CHECK (epoch >= 1),
    created_at INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0
) STRICT;
