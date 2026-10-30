-- name: InsertScheduleIfAbsent :execrows
INSERT INTO schedule (name, kind, rule, interval_s, local_time, zone, next_due_at, revision)
SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7, 1
WHERE NOT EXISTS (
    SELECT 1 FROM schedule WHERE schedule.name = ?1
);

-- name: ReseedScheduleZone :execrows
UPDATE schedule
SET zone = ?1, next_due_at = ?2, revision = revision + 1
WHERE name = ?3 AND (zone IS NULL OR zone <> ?1);

-- name: GetScheduleByName :one
SELECT id, name, kind, rule, interval_s, local_time, zone, next_due_at, revision
FROM schedule WHERE name = ?;

-- name: DueSchedules :many
SELECT id, name, kind, rule, interval_s, local_time, zone, next_due_at, revision
FROM schedule WHERE next_due_at <= ? ORDER BY next_due_at, id;

-- name: AdvanceSchedule :execrows
UPDATE schedule
SET next_due_at = ?, revision = revision + 1
WHERE id = ? AND next_due_at = ?;

-- name: CountOptionalQueued :one
SELECT count(*) FROM job WHERE state = 'queued' AND class = 'optional';

-- name: OpenJobForKey :one
SELECT id FROM job
WHERE concurrency_key = ? AND state IN ('running', 'reconciling', 'review')
ORDER BY id LIMIT 1;

-- name: PruneTerminalJobs :execrows
-- The scheduled maintenance's own batch: terminal optional rows past their
-- retention, oldest first, never more than one bounded batch per Write. A
-- required row and an open one (queued, running, reconciling, review) are never
-- selected.
DELETE FROM job
WHERE job.id IN (
    SELECT terminal.id FROM job AS terminal
    WHERE terminal.state IN ('succeeded', 'failed', 'expired')
      AND terminal.class = 'optional'
      AND terminal.updated_at < ?
    ORDER BY terminal.updated_at, terminal.id
    LIMIT 500
);
