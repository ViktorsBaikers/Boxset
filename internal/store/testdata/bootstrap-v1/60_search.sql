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
CREATE TRIGGER title_search_title_ai AFTER INSERT ON title
BEGIN
    INSERT INTO title_search (rowid, term) VALUES (-NEW.id, NEW.name);
END;

CREATE TRIGGER title_search_title_ad AFTER DELETE ON title
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', -OLD.id, OLD.name);
END;

CREATE TRIGGER title_search_title_au AFTER UPDATE OF id, name ON title
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', -OLD.id, OLD.name);
    INSERT INTO title_search (rowid, term) VALUES (-NEW.id, NEW.name);
END;

CREATE TRIGGER title_search_alias_ai AFTER INSERT ON title_alias
BEGIN
    INSERT INTO title_search (rowid, term) VALUES (NEW.id, NEW.alias);
END;

CREATE TRIGGER title_search_alias_ad AFTER DELETE ON title_alias
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', OLD.id, OLD.alias);
END;

CREATE TRIGGER title_search_alias_au AFTER UPDATE OF id, alias ON title_alias
BEGIN
    INSERT INTO title_search (title_search, rowid, term) VALUES ('delete', OLD.id, OLD.alias);
    INSERT INTO title_search (rowid, term) VALUES (NEW.id, NEW.alias);
END;

CREATE TRIGGER alias_generation_count_ai AFTER INSERT ON title_alias WHEN NEW.generation_id IS NOT NULL
BEGIN
    UPDATE alias_generation SET alias_count = alias_count + 1 WHERE id = NEW.generation_id;
END;

CREATE TRIGGER alias_generation_count_ad AFTER DELETE ON title_alias WHEN OLD.generation_id IS NOT NULL
BEGIN
    UPDATE alias_generation SET alias_count = alias_count - 1 WHERE id = OLD.generation_id;
END;

CREATE TRIGGER alias_generation_count_au AFTER UPDATE OF generation_id ON title_alias
BEGIN
    UPDATE alias_generation SET alias_count = alias_count - 1 WHERE id = OLD.generation_id;
    UPDATE alias_generation SET alias_count = alias_count + 1 WHERE id = NEW.generation_id;
END;
