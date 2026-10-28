-- name: LoginUser :one
SELECT * FROM user WHERE username_key = ?;

-- name: CreateSession :one
INSERT INTO session (token_sha256, user_id, epoch, created_at, expires_at)
VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: ReadSession :one
SELECT s.id, s.token_sha256, s.epoch, s.expires_at,
       u.id AS user_id, u.username, u.role, u.disabled, u.revision, u.created_at,
       i.restore_epoch
FROM session s JOIN user u ON u.id = s.user_id JOIN installation i ON i.id = 1
WHERE s.token_sha256 = ?;

-- name: ExtendSession :exec
UPDATE session SET expires_at = sqlc.arg(expires_at) WHERE token_sha256 = sqlc.arg(token_sha256) AND expires_at < sqlc.arg(expires_at);

-- name: DeleteSession :exec
DELETE FROM session WHERE token_sha256 = ?;
