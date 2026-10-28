-- +goose Up
-- The SHA-256 of every applied migration file, written by the migration runner
-- in the transaction that records the version.

CREATE TABLE boxset_schema_hash (
    version INTEGER PRIMARY KEY CHECK (version >= 1),
    sha256  TEXT NOT NULL CHECK (length(sha256) = 64)
) STRICT;

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

-- Review holds and indexer restrictions. A hold records why an acquisition
-- paused; (reason_code, scope_key) is the one-hold-per-cause guard, so a replay
-- adds no row, and scope_key names the affected target so each distinct target
-- keeps its own hold. title_id and slot_id name what the hold affects: a slot hold
-- pauses that slot, a title-only hold pauses every slot of the title, and both
-- are NULL only for a hold that affects no canonical row. An import conflict
-- carries the title or slot it conflicts with. A refusal of a remote-owned
-- slot is not a hold and has no reason_code here.

CREATE TABLE review_hold (
    id          INTEGER PRIMARY KEY,
    reason_code TEXT NOT NULL CHECK (reason_code IN ('identity_conflict', 'restriction_empty_intersection', 'ownership_unresolved')),
    scope_key   TEXT NOT NULL CHECK (length(scope_key) > 0),
    title_id    INTEGER REFERENCES title (id) ON DELETE CASCADE,
    slot_id     INTEGER REFERENCES slot (id) ON DELETE CASCADE,
    params      TEXT NOT NULL CHECK (json_valid(params)),
    UNIQUE (reason_code, scope_key)
) STRICT;

CREATE INDEX review_hold_by_title ON review_hold (title_id);
CREATE INDEX review_hold_by_slot ON review_hold (slot_id);

-- A library or slot with no rows here does not restrict indexers.
CREATE TABLE library_indexer (
    library_id  INTEGER NOT NULL REFERENCES library (id) ON DELETE CASCADE,
    indexer_key TEXT NOT NULL CHECK (length(indexer_key) > 0),
    PRIMARY KEY (library_id, indexer_key)
) STRICT;

CREATE TABLE slot_indexer (
    slot_id     INTEGER NOT NULL REFERENCES slot (id) ON DELETE CASCADE,
    indexer_key TEXT NOT NULL CHECK (length(indexer_key) > 0),
    PRIMARY KEY (slot_id, indexer_key)
) STRICT;

-- Per-(title, slot) rollup of slot state: counts, bytes, next airing and the
-- spine. One row per slot, created with it and kept in step with slot_episode
-- and the movie-slot state by the slot-state writers in the same transaction.
-- episodes and spine_full count marks: one per slot_episode row of a series
-- slot, one for a movie slot. size_bytes is that slot's own bytes only. A
-- spine is at most 24 marks (F on disk, U upgrade, M missing, L live, S
-- unaired, O unmonitored) and spine_omitted = spine_full - length(spine).
-- next_air_utc is an instant (UTC unix seconds) or NULL. revision moves only
-- when a stored value changes.

CREATE TABLE title_rollup (
    title_id      INTEGER NOT NULL,
    slot_id       INTEGER NOT NULL,
    episodes      INTEGER NOT NULL CHECK (episodes >= 0),
    monitored     INTEGER NOT NULL CHECK (monitored >= 0),
    on_disk       INTEGER NOT NULL CHECK (on_disk >= 0),
    missing       INTEGER NOT NULL CHECK (missing >= 0),
    size_bytes    INTEGER NOT NULL CHECK (size_bytes >= 0),
    next_air_utc  INTEGER,
    spine         TEXT NOT NULL CHECK (length(spine) <= 24),
    spine_full    INTEGER NOT NULL CHECK (spine_full >= 0),
    spine_omitted INTEGER NOT NULL CHECK (spine_omitted >= 0),
    revision      INTEGER NOT NULL CHECK (revision >= 0),
    PRIMARY KEY (title_id, slot_id),
    FOREIGN KEY (slot_id, title_id) REFERENCES slot (id, title_id) ON DELETE CASCADE
) STRICT;

-- +goose StatementBegin
CREATE TRIGGER slot_rollup_row AFTER INSERT ON slot
BEGIN
    INSERT INTO title_rollup (title_id, slot_id, episodes, monitored, on_disk, missing, size_bytes, next_air_utc, spine, spine_full, spine_omitted, revision)
    VALUES (NEW.title_id, NEW.id, 0, 0, 0, 0, 0, NULL, '', 0, 0, 0);
END;
-- +goose StatementEnd

-- The spine window reads marks in ord order around an anchor; the anchor is
-- the first live row, else the first monitored unaired row. The next airing is
-- the earliest unaired instant. Each is one indexed read, no sort.
CREATE INDEX slot_episode_by_ord ON slot_episode (slot_id, ord);
CREATE INDEX slot_episode_live ON slot_episode (slot_id, ord) WHERE state = 'live';
CREATE INDEX slot_episode_next_unaired ON slot_episode (slot_id, ord) WHERE state = 'unaired' AND monitored = 1;
CREATE INDEX slot_episode_airing ON slot_episode (slot_id, air_utc) WHERE state = 'unaired' AND air_utc IS NOT NULL;

-- Title and alias search. A title name and every alias is one FTS5 trigram row,
-- kept in step with its source row by triggers in the same transaction.
--
-- alias_generation groups the aliases of one dataset. generation_id NULL marks
-- a manual alias. Only manual aliases and those of the active generation are
-- visible to a search; the FTS index holds every alias row, so activation and
-- retirement flip a state and rewrite no index entry. At most one generation
-- per dataset is active. alias_count is the number of aliases the generation
-- holds, kept by triggers in the transaction that inserts, deletes or moves an
-- alias, so a purge reads what remains without scanning it.

CREATE TABLE alias_generation (
    id          INTEGER PRIMARY KEY,
    dataset     TEXT NOT NULL CHECK (length(dataset) > 0),
    state       TEXT NOT NULL CHECK (state IN ('staging', 'active', 'retired')),
    alias_count INTEGER NOT NULL DEFAULT 0 CHECK (alias_count >= 0)
) STRICT;

CREATE UNIQUE INDEX alias_generation_one_active ON alias_generation (dataset) WHERE state = 'active';

CREATE TABLE title_alias (
    id            INTEGER PRIMARY KEY,
    title_id      INTEGER NOT NULL REFERENCES title (id) ON DELETE CASCADE,
    generation_id INTEGER REFERENCES alias_generation (id) ON DELETE RESTRICT,
    alias         TEXT NOT NULL CHECK (length(alias) > 0)
) STRICT;

CREATE INDEX title_alias_by_title ON title_alias (title_id, generation_id);
CREATE INDEX title_alias_by_generation ON title_alias (generation_id);

-- The FTS content: a title is the row -title.id, an alias the row title_alias.id.
CREATE VIEW search_term AS
    SELECT -id AS id, name AS term FROM title
    UNION ALL
    SELECT id, alias FROM title_alias;

CREATE VIRTUAL TABLE title_search USING fts5 (term, content = 'search_term', content_rowid = 'id', tokenize = 'trigram');

-- External content: an update or delete removes the old row by the 'delete'
-- command with the old values, and an update then inserts the new one.
-- +goose StatementBegin
CREATE TRIGGER title_search_title_ai AFTER INSERT ON title
BEGIN
    INSERT INTO title_search (rowid, term) VALUES (-NEW.id, NEW.name);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER title_search_title_ad AFTER DELETE ON title
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', -OLD.id, OLD.name);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER title_search_title_au AFTER UPDATE OF id, name ON title
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', -OLD.id, OLD.name);
    INSERT INTO title_search (rowid, term) VALUES (-NEW.id, NEW.name);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER title_search_alias_ai AFTER INSERT ON title_alias
BEGIN
    INSERT INTO title_search (rowid, term) VALUES (NEW.id, NEW.alias);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER title_search_alias_ad AFTER DELETE ON title_alias
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', OLD.id, OLD.alias);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER title_search_alias_au AFTER UPDATE OF id, alias ON title_alias
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', OLD.id, OLD.alias);
    INSERT INTO title_search (rowid, term) VALUES (NEW.id, NEW.alias);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER alias_generation_count_ai AFTER INSERT ON title_alias WHEN NEW.generation_id IS NOT NULL
BEGIN
    UPDATE alias_generation SET alias_count = alias_count + 1 WHERE id = NEW.generation_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER alias_generation_count_ad AFTER DELETE ON title_alias WHEN OLD.generation_id IS NOT NULL
BEGIN
    UPDATE alias_generation SET alias_count = alias_count - 1 WHERE id = OLD.generation_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER alias_generation_count_au AFTER UPDATE OF generation_id ON title_alias
BEGIN
    UPDATE alias_generation SET alias_count = alias_count - 1 WHERE id = OLD.generation_id;
    UPDATE alias_generation SET alias_count = alias_count + 1 WHERE id = NEW.generation_id;
END;
-- +goose StatementEnd

-- The managed file format is stamp 2 and stays 2 for every later migration.
-- The binary that bootstrapped this schema without migration records admits
-- only stamp 1, so it refuses every file that carries them.
PRAGMA user_version = 2;
