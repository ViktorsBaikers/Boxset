-- name: ListSchemaHashes :many
SELECT version, sha256 FROM boxset_schema_hash ORDER BY version;

-- name: InsertSchemaHash :exec
INSERT INTO boxset_schema_hash (version, sha256) VALUES (?, ?);
