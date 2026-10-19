-- Canonical title identity, libraries and per-title slots.
-- Instants elsewhere in the schema are UTC unix seconds; INTEGER is int64.

CREATE TABLE title (
    id          INTEGER PRIMARY KEY,
    metadata_id TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK (kind IN ('movie', 'series')),
    name        TEXT NOT NULL
) STRICT;

CREATE TABLE library (
    id                      INTEGER PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    default_monitored       INTEGER NOT NULL CHECK (default_monitored IN (0, 1)),
    default_naming_language TEXT NOT NULL,
    default_search_language TEXT NOT NULL,
    default_profile         TEXT NOT NULL
) STRICT;

CREATE TABLE library_title (
    library_id INTEGER NOT NULL REFERENCES library (id) ON DELETE RESTRICT,
    title_id   INTEGER NOT NULL REFERENCES title (id) ON DELETE CASCADE,
    PRIMARY KEY (library_id, title_id)
) STRICT;

-- owner is 'local', 'unresolved' or a connected instance id. media_state and
-- size_bytes describe a movie slot and stay NULL on a series slot.
CREATE TABLE slot (
    id              INTEGER PRIMARY KEY,
    title_id        INTEGER NOT NULL REFERENCES title (id) ON DELETE CASCADE,
    slot_key        TEXT NOT NULL CHECK (length(slot_key) BETWEEN 1 AND 32 AND slot_key NOT GLOB '*[^a-z0-9-]*'),
    label           TEXT NOT NULL,
    profile         TEXT NOT NULL,
    is_primary      INTEGER NOT NULL CHECK (is_primary IN (0, 1)),
    owner           TEXT NOT NULL CHECK (length(owner) > 0),
    monitored       INTEGER NOT NULL CHECK (monitored IN (0, 1)),
    naming_language TEXT NOT NULL,
    search_language TEXT NOT NULL,
    media_state     TEXT CHECK (media_state IN ('missing', 'on_disk', 'upgrade', 'live', 'unaired')),
    size_bytes      INTEGER CHECK (size_bytes >= 0),
    UNIQUE (title_id, slot_key),
    UNIQUE (id, title_id)
) STRICT;

CREATE UNIQUE INDEX slot_one_primary ON slot (title_id) WHERE is_primary = 1;
