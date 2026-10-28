-- name: AnyUser :one
SELECT EXISTS (SELECT 1 FROM user) AS any_user;

-- name: GetSetup :one
SELECT token_sha256, consumed_at FROM setup WHERE id = 1;

-- name: ArmSetup :exec
INSERT INTO setup (id, token_sha256, consumed_at) VALUES (1, ?, NULL)
ON CONFLICT (id) DO UPDATE SET token_sha256 = excluded.token_sha256 WHERE setup.consumed_at IS NULL;

-- name: ConsumeSetup :execrows
UPDATE setup SET consumed_at = ?
WHERE id = 1 AND consumed_at IS NULL AND token_sha256 = ? AND NOT EXISTS (SELECT 1 FROM user);

-- name: CreateUser :one
INSERT INTO user (username, username_key, password_phc, role, disabled, revision, created_at)
VALUES (?, ?, ?, ?, 0, 1, ?)
RETURNING id, username, role, disabled, revision, created_at;

-- name: GetInstallation :one
SELECT install_id, restore_epoch FROM installation WHERE id = 1;

-- name: GetUser :one
SELECT id, username, role, disabled, revision, created_at FROM user WHERE id = ?;

-- name: CountActiveAdmins :one
SELECT count(*) FROM user WHERE role = 'admin' AND disabled = 0;

-- name: UpdateUser :one
UPDATE user SET role = ?, disabled = ?, revision = revision + 1
WHERE id = ? AND revision = ?
RETURNING id, username, role, disabled, revision, created_at;

-- name: UsersCollectionRevision :one
SELECT revision FROM collection_revision WHERE name = 'users';

-- name: ListUsers :many
WITH ordering AS (
  SELECT CAST(sqlc.arg(sort_by) AS TEXT) AS sort_by,
         CAST(sqlc.arg(sort_order) AS TEXT) AS sort_order
)
SELECT id, username, role, disabled, revision, created_at FROM user CROSS JOIN ordering
WHERE (sqlc.arg(role_filter) = '' OR role = sqlc.arg(role_filter))
  AND (sqlc.arg(search) = '' OR instr(username_key, sqlc.arg(search)) > 0)
  AND (sqlc.arg(after_id) = 0 OR
    (ordering.sort_by = 'username' AND (
      (ordering.sort_order = 'asc' AND (username, id) > (sqlc.arg(after_name), sqlc.arg(after_id))) OR
      (ordering.sort_order = 'desc' AND (username, id) < (sqlc.arg(after_name), sqlc.arg(after_id))))) OR
    (ordering.sort_by = 'createdAt' AND (
      (ordering.sort_order = 'asc' AND (created_at, id) > (sqlc.arg(after_created), sqlc.arg(after_id))) OR
      (ordering.sort_order = 'desc' AND (created_at, id) < (sqlc.arg(after_created), sqlc.arg(after_id))))))
ORDER BY
  CASE WHEN ordering.sort_by = 'username' AND ordering.sort_order = 'asc' THEN username END ASC,
  CASE WHEN ordering.sort_by = 'username' AND ordering.sort_order = 'desc' THEN username END DESC,
  CASE WHEN ordering.sort_by = 'createdAt' AND ordering.sort_order = 'asc' THEN created_at END ASC,
  CASE WHEN ordering.sort_by = 'createdAt' AND ordering.sort_order = 'desc' THEN created_at END DESC,
  CASE WHEN ordering.sort_order = 'asc' THEN id END ASC,
  CASE WHEN ordering.sort_order = 'desc' THEN id END DESC
LIMIT sqlc.arg(page_limit);

-- name: ListUserAuthority :many
SELECT id, username, username_key, password_phc, role, disabled, revision, created_at
FROM user ORDER BY id;

-- name: UpsertRestoredUser :exec
INSERT INTO user (id, username, username_key, password_phc, role, disabled, revision, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  username = excluded.username,
  username_key = excluded.username_key,
  password_phc = excluded.password_phc,
  role = excluded.role,
  disabled = excluded.disabled,
  revision = excluded.revision;

-- name: DisableRestoredUser :exec
UPDATE user SET disabled = 1, revision = ? WHERE id = ?;

-- name: UpsertSetup :exec
INSERT INTO setup (id, token_sha256, consumed_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET token_sha256 = excluded.token_sha256, consumed_at = excluded.consumed_at;

-- name: SetRestoreEpoch :exec
UPDATE installation SET restore_epoch = ? WHERE id = 1;

-- name: SetCollectionRevision :exec
INSERT INTO collection_revision (name, revision) VALUES (?, ?)
ON CONFLICT (name) DO UPDATE SET revision = excluded.revision;
