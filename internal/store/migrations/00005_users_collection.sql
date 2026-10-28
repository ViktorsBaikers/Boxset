-- +goose Up
CREATE TABLE collection_revision (
    name TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK (revision >= 1)
) STRICT;
INSERT INTO collection_revision (name, revision) VALUES ('users', 1);

CREATE INDEX user_username_page ON user(username, id);
CREATE INDEX user_created_page ON user(created_at, id);

-- +goose StatementBegin
CREATE TRIGGER user_collection_insert AFTER INSERT ON user BEGIN
    UPDATE collection_revision SET revision = revision + 1 WHERE name = 'users';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER user_collection_update AFTER UPDATE ON user BEGIN
    UPDATE collection_revision SET revision = revision + 1 WHERE name = 'users';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER user_collection_delete AFTER DELETE ON user BEGIN
    UPDATE collection_revision SET revision = revision + 1 WHERE name = 'users';
END;
-- +goose StatementEnd
