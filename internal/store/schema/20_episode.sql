-- Episodes, per-slot episode state, source mappings and the template/persona
-- slot-key mapping. A slot_episode row and its slot and episode always belong
-- to one title (composite foreign keys). air_utc is an instant (UTC unix
-- seconds) only when air_precision is 'instant'; date-only and unknown values
-- never become instants.

CREATE TABLE episode (
    id            INTEGER PRIMARY KEY,
    title_id      INTEGER NOT NULL REFERENCES title (id) ON DELETE CASCADE,
    ord           INTEGER NOT NULL,
    season        INTEGER NOT NULL CHECK (season >= 0),
    number        INTEGER NOT NULL CHECK (number >= 0),
    air_utc       INTEGER,
    air_precision TEXT NOT NULL CHECK (air_precision IN ('instant', 'date', 'unknown')),
    CHECK ((air_precision = 'instant') = (air_utc IS NOT NULL)),
    UNIQUE (title_id, season, number),
    UNIQUE (id, title_id)
) STRICT;

CREATE TABLE slot_episode (
    slot_id    INTEGER NOT NULL,
    episode_id INTEGER NOT NULL,
    title_id   INTEGER NOT NULL,
    ord        INTEGER NOT NULL,
    monitored  INTEGER NOT NULL CHECK (monitored IN (0, 1)),
    state      TEXT NOT NULL CHECK (state IN ('missing', 'on_disk', 'upgrade', 'live', 'unaired')),
    size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    air_utc    INTEGER,
    PRIMARY KEY (slot_id, episode_id),
    FOREIGN KEY (slot_id, title_id) REFERENCES slot (id, title_id) ON DELETE CASCADE,
    FOREIGN KEY (episode_id, title_id) REFERENCES episode (id, title_id) ON DELETE CASCADE
) STRICT;

-- One source title of a connected instance represents at most one slot, and
-- one slot has at most one source title per instance.
CREATE TABLE sync_map (
    instance_id TEXT NOT NULL CHECK (length(instance_id) > 0),
    source_kind TEXT NOT NULL CHECK (source_kind IN ('movie', 'series')),
    source_id   TEXT NOT NULL CHECK (length(source_id) > 0),
    title_id    INTEGER NOT NULL,
    slot_id     INTEGER NOT NULL,
    UNIQUE (instance_id, source_kind, source_id),
    UNIQUE (instance_id, slot_id),
    FOREIGN KEY (slot_id, title_id) REFERENCES slot (id, title_id) ON DELETE CASCADE
) STRICT;

-- The slot keys a library selects for each title added to it. Rows keep
-- insertion order; at most one is the primary key.
CREATE TABLE library_template (
    library_id INTEGER NOT NULL REFERENCES library (id) ON DELETE CASCADE,
    slot_key   TEXT NOT NULL CHECK (length(slot_key) BETWEEN 1 AND 32 AND slot_key NOT GLOB '*[^a-z0-9-]*'),
    is_primary INTEGER NOT NULL CHECK (is_primary IN (0, 1)),
    PRIMARY KEY (library_id, slot_key)
) STRICT;

CREATE UNIQUE INDEX library_template_one_primary ON library_template (library_id) WHERE is_primary = 1;

-- A compat persona names the slot key it reads and writes. Labels and
-- profiles never select a slot.
CREATE TABLE persona (
    key      TEXT PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 32 AND key NOT GLOB '*[^a-z0-9-]*'),
    slot_key TEXT NOT NULL CHECK (length(slot_key) BETWEEN 1 AND 32 AND slot_key NOT GLOB '*[^a-z0-9-]*')
) STRICT;
