-- name: GetInstallationKey :one
SELECT key_id FROM installation_key WHERE id = 1;

-- name: SetInstallationKey :exec
INSERT INTO installation_key (id, key_id) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET key_id = excluded.key_id;

-- name: ListRecordedKeyIDs :many
SELECT DISTINCT key_id FROM secret
UNION
SELECT key_id FROM installation_key
ORDER BY 1;

-- name: ListCurrentSecretKeys :many
SELECT id, key_id FROM secret WHERE is_current = 1 ORDER BY id;

-- name: GetCurrentSecret :one
SELECT id, version, purpose, key_id, nonce, ciphertext
FROM secret WHERE id = ? AND is_current = 1;

-- name: RetireSecretVersion :exec
UPDATE secret SET is_current = 0
WHERE id = ? AND version = ? AND is_current = 1;

-- name: InsertSecretVersion :exec
INSERT INTO secret (id, version, purpose, key_id, nonce, ciphertext, is_current, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
