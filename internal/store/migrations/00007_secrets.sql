-- +goose Up
-- Recoverable credentials. Each record is one AES-256-GCM version under a key
-- file kept outside the database; installation_key names the key new writes
-- use. No query deletes a version or rewrites its key_id, nonce or ciphertext:
-- re-entry retires the current version and appends the next one, so a lost key
-- never destroys the rows it protects.

CREATE TABLE installation_key (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    key_id TEXT NOT NULL CHECK (length(key_id) = 64 AND key_id NOT GLOB '*[^0-9a-f]*')
) STRICT;

CREATE TABLE secret (
    id         INTEGER NOT NULL CHECK (id >= 1),
    version    INTEGER NOT NULL CHECK (version >= 1),
    purpose    TEXT NOT NULL CHECK (length(purpose) BETWEEN 1 AND 64),
    key_id     TEXT NOT NULL CHECK (length(key_id) = 64 AND key_id NOT GLOB '*[^0-9a-f]*'),
    nonce      BLOB NOT NULL CHECK (length(nonce) = 12),
    ciphertext BLOB NOT NULL CHECK (length(ciphertext) >= 16),
    is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (id, version)
) STRICT;

CREATE UNIQUE INDEX secret_current ON secret (id) WHERE is_current = 1;
