-- name: CreateAPIKey :one
INSERT INTO api_key (token_sha256, user_id, name, scope, epoch, created_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ReadAPIKey :one
SELECT k.id, k.token_sha256, k.scope, k.epoch, k.last_used_at,
       u.id AS user_id, u.username, u.role, u.disabled, u.revision, u.created_at,
       i.restore_epoch
FROM api_key k JOIN user u ON u.id = k.user_id JOIN installation i ON i.id = 1
WHERE k.token_sha256 = ?;

-- name: RevokeAPIKey :one
DELETE FROM api_key WHERE id = ? RETURNING token_sha256;

-- name: TouchAPIKey :execrows
UPDATE api_key SET last_used_at = sqlc.arg(last_used_at)
WHERE token_sha256 = sqlc.arg(token_sha256)
  AND epoch = sqlc.arg(epoch)
  AND epoch = (SELECT restore_epoch FROM installation WHERE id = 1)
  AND last_used_at < sqlc.arg(last_used_at);
