-- Acquisitions, the slots each one serves, and the files recorded for a slot.
-- UNIQUE (client_id, release_key) is the one-acquisition-per-release guard.
-- download_slot and file reference slot with RESTRICT, so a title with either
-- cannot be deleted by cascade.

CREATE TABLE acquisition (
    id          INTEGER PRIMARY KEY,
    client_id   TEXT NOT NULL CHECK (length(client_id) > 0),
    release_key TEXT NOT NULL CHECK (length(release_key) > 0),
    UNIQUE (client_id, release_key)
) STRICT;

CREATE TABLE download_slot (
    acquisition_id INTEGER NOT NULL REFERENCES acquisition (id) ON DELETE CASCADE,
    slot_id        INTEGER NOT NULL REFERENCES slot (id) ON DELETE RESTRICT,
    PRIMARY KEY (acquisition_id, slot_id)
) STRICT;

CREATE INDEX download_slot_by_slot ON download_slot (slot_id);

CREATE TABLE file (
    id            INTEGER PRIMARY KEY,
    slot_id       INTEGER NOT NULL REFERENCES slot (id) ON DELETE RESTRICT,
    root_key      TEXT NOT NULL CHECK (length(root_key) > 0),
    relative_path TEXT NOT NULL CHECK (length(relative_path) > 0),
    size_bytes    INTEGER NOT NULL CHECK (size_bytes >= 0),
    UNIQUE (root_key, relative_path)
) STRICT;

CREATE INDEX file_by_slot ON file (slot_id);
