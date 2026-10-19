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

CREATE TRIGGER slot_rollup_row AFTER INSERT ON slot
BEGIN
    INSERT INTO title_rollup (title_id, slot_id, episodes, monitored, on_disk, missing, size_bytes, next_air_utc, spine, spine_full, spine_omitted, revision)
    VALUES (NEW.title_id, NEW.id, 0, 0, 0, 0, 0, NULL, '', 0, 0, 0);
END;

-- The spine window reads marks in ord order around an anchor; the anchor is
-- the first live row, else the first monitored unaired row. The next airing is
-- the earliest unaired instant. Each is one indexed read, no sort.
CREATE INDEX slot_episode_by_ord ON slot_episode (slot_id, ord);
CREATE INDEX slot_episode_live ON slot_episode (slot_id, ord) WHERE state = 'live';
CREATE INDEX slot_episode_next_unaired ON slot_episode (slot_id, ord) WHERE state = 'unaired' AND monitored = 1;
CREATE INDEX slot_episode_airing ON slot_episode (slot_id, air_utc) WHERE state = 'unaired' AND air_utc IS NOT NULL;
