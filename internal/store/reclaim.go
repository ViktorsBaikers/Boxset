package store

import (
	"context"
	"database/sql"
	"errors"
)

// reclaimBatch moves at most 1024 pages, so one writer transaction stays short
// and other writers run between batches.
const reclaimBatch = "PRAGMA incremental_vacuum(1024)"

// ReclaimResult reports what one Reclaim did. Every figure describes batches
// that committed: a batch whose transaction did not commit is not counted.
type ReclaimResult struct {
	Batches          int   // incremental-vacuum batches committed
	Freed            int64 // free pages those batches removed from the freelist; a logical count, not file bytes
	FreeBefore       int64 // free pages at the first committed observation
	FreeAfter, Pages int64 // free pages and total pages after the last committed batch (or observation)
	Truncated        bool  // the closing wal_checkpoint(TRUNCATE) completed; false when a reader held it back
}

// reclaimStep is what one Reclaim transaction saw and did.
type reclaimStep struct {
	free, pages           int64 // before the batch
	vacuumed              bool  // a batch ran
	freeAfter, pagesAfter int64 // after the batch; equal to free and pages when none ran
}

// Reclaim returns free pages to the freelist's end, and so to the file system,
// while more than a tenth of the pages are free. Each batch moves at most 1024
// pages inside its own Write, so no write transaction stays long and others run
// between batches. The batches are bounded; the whole call is not given a
// duration. After at least one batch it truncates the WAL with a checkpoint,
// because under WAL the database file only shrinks once the moved pages are
// checkpointed. A checkpoint a pinned reader holds back is Truncated=false with
// a nil error; the next checkpoint finishes it. A file at or below the threshold
// is left alone. On a canceled context or any error the result still reports the
// batches that committed before it.
func (d *DB) Reclaim(ctx context.Context) (ReclaimResult, error) {
	var res ReclaimResult
	for first := true; ; first = false {
		var step reclaimStep
		err := d.Write(ctx, func(tx *sql.Tx) error {
			step = reclaimStep{}
			var err error
			if step.free, step.pages, err = pageCounts(ctx, tx); err != nil {
				return err
			}
			step.freeAfter, step.pagesAfter = step.free, step.pages
			if step.free*10 <= step.pages {
				return nil
			}
			if _, err := tx.ExecContext(ctx, reclaimBatch); err != nil {
				return err
			}
			step.vacuumed = true
			if step.freeAfter, step.pagesAfter, err = pageCounts(ctx, tx); err != nil {
				return err
			}
			if d.beforeReclaimCommit != nil { // test seam: fail after the final counts, before the commit
				return d.beforeReclaimCommit(res.Batches + 1)
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		// The transaction committed: only now does the step count.
		if first {
			res.FreeBefore = step.free
		}
		res.FreeAfter, res.Pages = step.freeAfter, step.pagesAfter
		if !step.vacuumed {
			break
		}
		res.Batches++
		res.Freed += step.free - step.freeAfter
		if d.afterReclaimBatch != nil {
			d.afterReclaimBatch(res.Batches)
		}
		if step.freeAfter*10 <= step.pagesAfter || step.freeAfter >= step.free {
			break // below the threshold, or no page moved so nothing more can be reclaimed
		}
	}
	if res.Batches == 0 {
		return res, nil
	}
	return res, d.truncateWAL(ctx, &res)
}

// truncateWAL runs the checkpoint outside any transaction, on the writer's one
// connection, under the writer admission lease.
func (d *DB) truncateWAL(ctx context.Context, res *ReclaimResult) (err error) {
	if err := d.admit(ctx, nil); err != nil {
		return err
	}
	defer d.release()
	conn, err := d.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	var busy, walFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &walFrames, &checkpointed); err != nil {
		return err
	}
	res.Truncated = busy == 0
	return nil
}

func pageCounts(ctx context.Context, tx *sql.Tx) (free, pages int64, err error) {
	if err := tx.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, 0, err
	}
	return free, pages, nil
}
