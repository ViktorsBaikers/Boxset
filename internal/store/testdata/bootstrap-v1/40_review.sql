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
