-- name: InsertOpenJob :execrows
INSERT INTO job (
    kind, class, lane, dedupe_key, concurrency_key, params, state,
    attempts, max_attempts, due_at, expires_at, created_by, created_at, updated_at, revision
)
SELECT ?1, ?2, ?3, ?4, ?5, ?6, 'queued', 0, ?7, ?8, ?9, ?10, ?11, ?12, 1
WHERE NOT EXISTS (
    SELECT 1 FROM job AS open_row
    WHERE open_row.kind = ?1 AND open_row.dedupe_key = ?4
      AND open_row.state IN ('queued', 'running', 'reconciling', 'review')
);

-- name: OpenJobID :one
SELECT id FROM job
WHERE kind = ? AND dedupe_key = ?
  AND state IN ('queued', 'running', 'reconciling', 'review')
ORDER BY id LIMIT 1;

-- name: ClaimCandidate :one
SELECT j.id FROM job AS j
WHERE j.state = 'queued' AND j.lane = ? AND j.due_at <= ?
  AND (j.expires_at IS NULL OR j.expires_at > ?)
  AND NOT EXISTS (
    SELECT 1 FROM job AS k
    WHERE k.concurrency_key = j.concurrency_key
      AND k.state IN ('running', 'reconciling', 'review')
)
ORDER BY j.due_at, j.id LIMIT 1;

-- name: ClaimJob :execrows
UPDATE job
SET state = 'running', lease_owner = ?, attempts = attempts + 1,
    attempt_started_at = ?, updated_at = ?, revision = revision + 1
WHERE job.id = ? AND job.state = 'queued'
  AND NOT EXISTS (
    SELECT 1 FROM job AS k
    WHERE k.concurrency_key = job.concurrency_key
      AND k.state IN ('running', 'reconciling', 'review')
);

-- name: GetJob :one
SELECT id, kind, class, lane, dedupe_key, concurrency_key, params, state,
       reason_code, reason_params, attempts, max_attempts, due_at, expires_at,
       lease_owner, attempt_started_at, created_by, created_at, updated_at, revision
FROM job WHERE id = ?;

-- name: ListJobsInState :many
SELECT id, kind, class, lane, dedupe_key, concurrency_key, params, state,
       reason_code, reason_params, attempts, max_attempts, due_at, expires_at,
       lease_owner, attempt_started_at, created_by, created_at, updated_at, revision
FROM job WHERE state = ? ORDER BY id;

-- name: RecoverForeignRunning :execrows
UPDATE job
SET state = 'reconciling', reason_code = 'attempt_interrupted', reason_params = NULL,
    lease_owner = NULL, updated_at = ?, revision = revision + 1
WHERE job.state = 'running' AND (job.lease_owner IS NULL OR job.lease_owner <> ?);

-- name: MarkJobOverdue :execrows
UPDATE job
SET reason_code = 'attempt_overdue', reason_params = ?,
    updated_at = ?, revision = revision + 1
WHERE id = ? AND state = 'running'
  AND (reason_code IS NULL OR reason_code <> 'attempt_overdue');

-- name: ExpireOptionalJobs :execrows
UPDATE job
SET state = 'expired', reason_code = 'expired', reason_params = NULL,
    lease_owner = NULL, attempt_started_at = NULL, updated_at = ?, revision = revision + 1
WHERE state = 'queued' AND class = 'optional' AND expires_at IS NOT NULL AND expires_at <= ?;

-- name: FinishJob :execrows
UPDATE job
SET state = ?, reason_code = ?, reason_params = ?,
    lease_owner = NULL, attempt_started_at = NULL, updated_at = ?, revision = revision + 1
WHERE id = ? AND state IN ('running', 'reconciling', 'review');

-- name: RequeueJob :execrows
UPDATE job
SET state = 'queued', reason_code = ?, reason_params = ?, attempts = ?,
    due_at = ?, lease_owner = NULL, attempt_started_at = NULL,
    updated_at = ?, revision = revision + 1
WHERE id = ? AND state IN ('running', 'reconciling');

-- name: ReconcileJob :execrows
UPDATE job
SET state = 'reconciling', reason_code = ?, reason_params = ?,
    lease_owner = NULL, updated_at = ?, revision = revision + 1
WHERE id = ? AND state IN ('running', 'reconciling');

-- name: CountOpenInteractive :one
-- The admission counters: every open interactive row, and one caller's own share
-- of them. queued, running, reconciling and review are the open states, so the
-- counts are the rows an accepted request would join rather than a history.
SELECT count(*) FROM job
WHERE lane = 'interactive'
  AND state IN ('queued', 'running', 'reconciling', 'review');

-- name: CountOpenInteractiveFor :one
SELECT count(*) FROM job
WHERE lane = 'interactive' AND created_by = ?
  AND state IN ('queued', 'running', 'reconciling', 'review');

-- name: PageJobs :many
-- One page of the jobs read model, newest first, bounded by the id the previous
-- page ended at. The caller reads one row past the bound to learn whether
-- another page exists.
SELECT id, kind, class, lane, dedupe_key, concurrency_key, params, state,
       reason_code, reason_params, attempts, max_attempts, due_at, expires_at,
       lease_owner, attempt_started_at, created_by, created_at, updated_at, revision
FROM job
WHERE id < ?
ORDER BY id DESC
LIMIT ?;

-- name: PageJobsInState :many
SELECT id, kind, class, lane, dedupe_key, concurrency_key, params, state,
       reason_code, reason_params, attempts, max_attempts, due_at, expires_at,
       lease_owner, attempt_started_at, created_by, created_at, updated_at, revision
FROM job
WHERE state = ? AND id < ?
ORDER BY id DESC
LIMIT ?;

-- name: SweepWork :one
SELECT count(*) FROM job
WHERE state IN ('running', 'reconciling')
   OR (state = 'queued'
       AND (due_at <= ? OR (class = 'optional' AND expires_at IS NOT NULL AND expires_at <= ?)));
