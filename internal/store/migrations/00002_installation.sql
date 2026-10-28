-- +goose Up
-- The one row that names this installation: install_id scopes cookies and
-- continuity checks, restore_epoch is bumped by a restore. The row is created
-- here, once, so every migrated file carries it.

CREATE TABLE installation (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    install_id    TEXT NOT NULL CHECK (length(install_id) = 12 AND install_id NOT GLOB '*[^0-9a-f]*'),
    restore_epoch INTEGER NOT NULL CHECK (restore_epoch >= 1),
    created_at    INTEGER NOT NULL
) STRICT;

INSERT INTO installation (id, install_id, restore_epoch, created_at)
VALUES (1, lower(hex(randomblob(6))), 1, unixepoch());
