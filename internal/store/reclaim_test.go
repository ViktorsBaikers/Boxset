package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

// checkpoint runs wal_checkpoint(mode) on the writer's connection, outside any transaction.
func checkpoint(t *testing.T, db *DB, mode string) {
	t.Helper()
	conn, err := db.writer.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeIt(t, conn)
	var busy, frames, done int64
	if err := conn.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &frames, &done); err != nil {
		t.Fatalf("wal_checkpoint(%s): %v", mode, err)
	}
	if busy != 0 {
		t.Fatalf("wal_checkpoint(%s) was held back by a reader", mode)
	}
}

func fileSize(t *testing.T, name string) int64 {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// pageStats reads free and total pages through a reader.
func pageStats(t *testing.T, db *DB) (free, pages int64) {
	t.Helper()
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		free, pages, err = pageCounts(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return free, pages
}

// rowSpec is one fixture row: its id and the length of its payload.
type rowSpec struct{ id, size int }

// payloadOf is the payload bytes the fixture stores for a row. Every id and
// every offset gives a different byte pattern, so a payload that moved, shrank
// or was overwritten differs from the expected bytes.
func payloadOf(s rowSpec) []byte {
	b := make([]byte, s.size)
	for i := range b {
		b[i] = byte(s.id*7 + i*13 + i/251)
	}
	return b
}

// specRange is count rows from first, step apart, each of size bytes.
func specRange(first, count, step, size int) []rowSpec {
	out := make([]rowSpec, count)
	for i := range out {
		out[i] = rowSpec{first + i*step, size}
	}
	return out
}

// insertRows stores the rows of table(id, payload) in one Write.
func insertRows(t *testing.T, db *DB, table string, rows []rowSpec) {
	t.Helper()
	insert := fmt.Sprintf("INSERT INTO %s(id, payload) VALUES (?, ?)", table)
	mustWrite(t, db, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(insert, r.id, payloadOf(r)); err != nil {
				return err
			}
		}
		return nil
	})
}

// verifyRows fails unless table holds exactly rows, with exactly their payload bytes.
func verifyRows(t *testing.T, db *DB, table string, rows []rowSpec) {
	t.Helper()
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		res, err := tx.QueryContext(t.Context(), "SELECT id, payload FROM "+table+" ORDER BY id")
		if err != nil {
			return err
		}
		defer closeIt(t, res)
		i := 0
		for res.Next() {
			var id int
			var payload []byte
			if err := res.Scan(&id, &payload); err != nil {
				return err
			}
			if i >= len(rows) {
				t.Errorf("%s holds an unexpected row id %d beyond the %d expected", table, id, len(rows))
				return nil
			}
			if want := rows[i]; id != want.id || !bytes.Equal(payload, payloadOf(want)) {
				t.Errorf("%s row %d: id %d (%d bytes), want id %d with its %d fixture bytes", table, i, id, len(payload), want.id, want.size)
				return nil
			}
			i++
		}
		if i != len(rows) {
			t.Errorf("%s holds %d rows, want %d", table, i, len(rows))
		}
		return res.Err()
	}); err != nil {
		t.Fatal(err)
	}
}

// integrityOK fails unless PRAGMA integrity_check says exactly "ok".
func integrityOK(t *testing.T, db *DB) {
	t.Helper()
	var got []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		res, err := tx.QueryContext(t.Context(), "PRAGMA integrity_check")
		if err != nil {
			return err
		}
		defer closeIt(t, res)
		for res.Next() {
			var line string
			if err := res.Scan(&line); err != nil {
				return err
			}
			got = append(got, line)
		}
		return res.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("integrity_check = %q, want exactly [ok]", got)
	}
}

// churnStore is a store whose churn table held rows rows of size bytes, 1..rows,
// and had every even id deleted; it returns the retained (odd) rows. The WAL
// was truncated before the delete, so the freed pages are not yet in the file.
func churnStore(t *testing.T, rows, size int) (db *DB, path string, retained []rowSpec) {
	t.Helper()
	path = newPath(t)
	db = openStore(t, path, Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE churn(id INTEGER PRIMARY KEY, payload BLOB NOT NULL)")
		return err
	})
	insertRows(t, db, "churn", specRange(1, rows, 1, size))
	checkpoint(t, db, "TRUNCATE")
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM churn WHERE id % 2 = 0")
		return err
	})
	return db, path, specRange(1, rows/2, 2, size)
}

func TestReclaimShrinksChurnFixture(t *testing.T) {
	const rows, size = 6000, 3000 // one row fills most of a 4 KiB page, so deleting a row frees a page
	path := newPath(t)
	db := openStore(t, path, Options{})

	// Nothing is free in a fresh file: Reclaim leaves it alone.
	if res, err := db.Reclaim(t.Context()); err != nil || res.Batches != 0 || res.Truncated {
		t.Fatalf("Reclaim on a file with no free pages = %+v, %v; want no batch and no checkpoint", res, err)
	}

	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE churn(id INTEGER PRIMARY KEY, payload BLOB NOT NULL)")
		return err
	})
	insertRows(t, db, "churn", specRange(1, rows, 1, size))
	checkpoint(t, db, "TRUNCATE")
	full := fileSize(t, path)

	// Delete every second row: 50% of the rows, so about half the pages become free.
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM churn WHERE id % 2 = 0")
		return err
	})
	retained := specRange(1, rows/2, 2, size)
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)
	free, pages := pageStats(t, db)
	if free*10 <= pages {
		t.Fatalf("fixture: %d of %d pages free, want more than 10%%", free, pages)
	}

	res, err := db.Reclaim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Reclaim: %+v; main file %d -> %d bytes", res, full, fileSize(t, path))

	if res.FreeBefore != free {
		t.Errorf("FreeBefore = %d, want %d", res.FreeBefore, free)
	}
	if res.FreeAfter*10 > res.Pages {
		t.Errorf("after Reclaim %d of %d pages are free, want at most 10%%", res.FreeAfter, res.Pages)
	}
	if res.Batches < 2 {
		t.Errorf("Batches = %d, want at least 2 for %d pages in batches of at most 1024", res.Batches, free)
	}
	if res.Freed > int64(res.Batches)*1024 || res.Freed <= int64(res.Batches-1)*1024 {
		t.Errorf("Freed = %d over %d batches: each batch is at most 1024 pages and only the last may be short", res.Freed, res.Batches)
	}
	if res.Freed != res.FreeBefore-res.FreeAfter {
		t.Errorf("Freed = %d, want FreeBefore-FreeAfter = %d", res.Freed, res.FreeBefore-res.FreeAfter)
	}
	if !res.Truncated {
		t.Error("Truncated = false with no reader running, want the closing checkpoint to complete")
	}
	if got := fileSize(t, path); got >= full*65/100 {
		t.Errorf("main file is %d bytes after Reclaim, was %d: want it below 65%%", got, full)
	}
	if got := fileSize(t, path+"-wal"); got != 0 {
		t.Errorf("-wal is %d bytes after the truncating checkpoint, want 0", got)
	}

	// Every retained row keeps its exact payload, the file passes its integrity
	// check, and both still hold after the file is closed and reopened.
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)
	closeIt(t, db)
	reopened := openStore(t, path, Options{})
	verifyRows(t, reopened, "churn", retained)
	integrityOK(t, reopened)

	// A second pass finds nothing above the threshold.
	if again, err := reopened.Reclaim(t.Context()); err != nil || again.Batches != 0 {
		t.Errorf("second Reclaim = %+v, %v; want no batch", again, err)
	}
}

// thresholdFixture is an open store whose free pages F and page count P satisfy
// 10F - P == d, with F > 0 for d <= 0. Free pages come from a deleted scratch
// table and are tuned with incremental_vacuum; the number of kept rows moves P
// until the residue fits, and the fixture is measured, never assumed.
func thresholdFixture(t *testing.T, d int64) (db *DB, free, pages int64) {
	t.Helper()
	for keep := range 18 {
		cand, err := Open(t.Context(), newPath(t), Options{})
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, cand, func(tx *sql.Tx) error {
			_, err := tx.Exec("CREATE TABLE keep(id INTEGER PRIMARY KEY, payload BLOB NOT NULL); CREATE TABLE scratch(id INTEGER PRIMARY KEY, payload BLOB NOT NULL)")
			return err
		})
		insertRows(t, cand, "keep", specRange(1, keep, 1, 3000))
		insertRows(t, cand, "scratch", specRange(1, 120, 1, 3000))
		mustWrite(t, cand, func(tx *sql.Tx) error {
			_, err := tx.Exec("DELETE FROM scratch")
			return err
		})
		f, p := pageStats(t, cand)
		span := 10*f - p - d
		if span < 0 || span%9 != 0 || span/9 >= f {
			closeIt(t, cand)
			continue
		}
		if k := span / 9; k > 0 {
			mustWrite(t, cand, func(tx *sql.Tx) error {
				_, err := tx.Exec(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", k))
				return err
			})
		}
		if f, p = pageStats(t, cand); 10*f-p != d {
			closeIt(t, cand)
			continue
		}
		t.Cleanup(func() { _ = cand.Close() })
		return cand, f, p
	}
	t.Fatalf("no fixture with 10*free - pages == %d", d)
	return nil, 0, 0
}

// Reclaim acts only when more than 10% of the pages are free: exactly 10% and
// less leave the file alone, one page over does not.
func TestReclaimThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       int64
		reclaim bool
	}{
		{"free pages below 10%", -1, false},
		{"exactly 10%", 0, false},
		{"one page over 10%", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, free, pages := thresholdFixture(t, tc.d)
			if free <= 0 {
				t.Fatalf("fixture: %d free pages, want some", free)
			}
			res, err := db.Reclaim(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			afterFree, afterPages := pageStats(t, db)
			if !tc.reclaim {
				if res.Batches != 0 || res.Freed != 0 || res.Truncated {
					t.Errorf("Reclaim at %d free of %d pages = %+v, want no batch, nothing freed, no checkpoint", free, pages, res)
				}
				if res.FreeBefore != free || res.FreeAfter != free || res.Pages != pages {
					t.Errorf("result %+v does not describe the untouched %d free of %d pages", res, free, pages)
				}
				if afterFree != free || afterPages != pages {
					t.Errorf("pages changed to %d free of %d, want the untouched %d of %d", afterFree, afterPages, free, pages)
				}
				return
			}
			if res.Batches != 1 || res.Freed != free || res.FreeAfter != 0 || !res.Truncated {
				t.Errorf("Reclaim at %d free of %d pages = %+v, want one batch freeing all %d and a completed checkpoint", free, pages, res, free)
			}
			if afterFree != 0 {
				t.Errorf("%d pages still free after Reclaim, want 0", afterFree)
			}
		})
	}
}

// A reader pinned to an older snapshot holds the closing checkpoint back. Reclaim
// still frees the pages, reports Truncated=false with a nil error, keeps every row
// and leaves the store usable; once the reader is gone the checkpoint completes.
func TestReclaimPinnedReader(t *testing.T) {
	db, path, retained := churnStore(t, 600, 3000)

	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unpin := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unpin)
	go func() {
		done <- db.Read(t.Context(), func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM churn").Scan(&n); err != nil { // the snapshot is taken here
				return err
			}
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the pinning reader ended before it was pinned: %v", err)
	}

	res, err := db.Reclaim(t.Context())
	if err != nil {
		t.Fatalf("Reclaim with a pinned reader = %v, want nil: a held-back checkpoint is not an error", err)
	}
	if res.Batches < 1 || res.FreeAfter*10 > res.Pages {
		t.Errorf("Reclaim = %+v, want the free pages released below 10%%", res)
	}
	if res.Truncated {
		t.Errorf("Truncated = true with a reader pinning the WAL: %+v", res)
	}
	if got := fileSize(t, path+"-wal"); got == 0 {
		t.Error("-wal is empty although a pinned reader holds it")
	}
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE usable(x INTEGER)")
		return err
	})

	unpin()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	checkpoint(t, db, "TRUNCATE") // fatal if still busy
	if got := fileSize(t, path+"-wal"); got != 0 {
		t.Errorf("-wal is %d bytes after the reader released and the checkpoint ran, want 0", got)
	}
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)
}

// A canceled Reclaim reports the batches that committed, no more, and a retry
// finishes the job.
func TestReclaimCancelAndRetry(t *testing.T) {
	db, path, retained := churnStore(t, 6000, 3000)

	// Canceled before it starts: nothing runs and nothing is reported.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	before := fileSize(t, path)
	if res, err := db.Reclaim(canceled); !errors.Is(err, context.Canceled) || res != (ReclaimResult{}) {
		t.Fatalf("Reclaim on a canceled context = %+v, %v; want context.Canceled and an empty result", res, err)
	}
	if got := fileSize(t, path); got != before {
		t.Errorf("file size changed from %d to %d under a canceled Reclaim", before, got)
	}

	// Canceled after the first committed batch.
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	db.afterReclaimBatch = func(batch int) {
		if batch == 1 {
			stop()
		}
	}
	res, err := db.Reclaim(ctx)
	db.afterReclaimBatch = nil
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reclaim canceled after batch 1 = %+v, %v; want context.Canceled", res, err)
	}
	if res.Batches != 1 || res.Freed != 1024 || res.FreeAfter != res.FreeBefore-1024 || res.Truncated {
		t.Errorf("canceled Reclaim reported %+v, want exactly the one committed batch of 1024 pages and no checkpoint", res)
	}
	if free, _ := pageStats(t, db); free != res.FreeAfter {
		t.Errorf("%d pages free in the file, the result says %d", free, res.FreeAfter)
	}
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)

	// The store is usable and a retry completes the reclaim.
	retry, err := db.Reclaim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if retry.FreeBefore != res.FreeAfter || retry.Freed != retry.FreeBefore-retry.FreeAfter || retry.FreeAfter*10 > retry.Pages || !retry.Truncated {
		t.Errorf("retry = %+v after %+v, want it to start where the canceled call stopped and finish below 10%% with a checkpoint", retry, res)
	}
	verifyRows(t, db, "churn", retained)
	integrityOK(t, db)
}

func TestWALBoundedAfterImportCheckpoint(t *testing.T) {
	const limit = 64 << 20
	path := newPath(t)
	db := openStore(t, path, Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE big(id INTEGER PRIMARY KEY, payload BLOB NOT NULL)")
		return err
	})

	// One import transaction larger than the limit leaves a WAL above it.
	imported := specRange(1, 72, 1, 1<<20)
	insertRows(t, db, "big", imported)
	if got := fileSize(t, path+"-wal"); got <= limit {
		t.Fatalf("fixture: -wal is %d bytes after the import, want more than the %d limit", got, limit)
	}

	// After the checkpoint the next write restarts the log and the limit truncates it.
	checkpoint(t, db, "RESTART")
	last := rowSpec{1000, 1}
	insertRows(t, db, "big", []rowSpec{last})
	if got := fileSize(t, path+"-wal"); got > limit {
		t.Errorf("-wal is %d bytes after import plus checkpoint, want at most %d", got, limit)
	}

	// Every imported payload survived, in the live store and after a reopen.
	all := append(imported, last)
	verifyRows(t, db, "big", all)
	integrityOK(t, db)
	closeIt(t, db)
	reopened := openStore(t, path, Options{})
	verifyRows(t, reopened, "big", all)
	integrityOK(t, reopened)
}

// A batch that vacuumed, read its final page counts and then failed before its
// commit is rolled back by the database and is not in the result: the result
// and the file both describe the committed prefix, every retained payload and
// the integrity check are intact, and a retry finishes the reclaim.
func TestReclaimRolledBackBatch(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("batch %d fails before its commit", failAt), func(t *testing.T) {
			db, path, retained := churnStore(t, 6000, 3000)
			wantFree, wantPages := pageStats(t, db) // the committed state before any batch
			committed := failAt - 1
			db.afterReclaimBatch = func(int) { wantFree, wantPages = pageStats(t, db) } // each committed batch moves it
			db.beforeReclaimCommit = func(batch int) error {
				if batch == failAt {
					return errInjected
				}
				return nil
			}
			startFree := wantFree
			res, err := db.Reclaim(t.Context())
			db.afterReclaimBatch, db.beforeReclaimCommit = nil, nil
			if !errors.Is(err, errInjected) {
				t.Fatalf("Reclaim = %+v, %v; want the injected failure", res, err)
			}

			// The result is the committed prefix and nothing of the failed batch.
			if committed == 0 {
				if res != (ReclaimResult{}) {
					t.Errorf("result after the first batch failed = %+v, want it empty", res)
				}
			} else if res.Batches != committed || res.Freed != int64(committed)*1024 || res.FreeBefore != startFree || res.FreeAfter != wantFree || res.Pages != wantPages || res.Truncated {
				t.Errorf("result = %+v, want %d committed batch(es) of 1024 pages from %d free, ending at %d free of %d pages, no checkpoint", res, committed, startFree, wantFree, wantPages)
			}

			// The file holds exactly the committed prefix: the failed vacuum was rolled back.
			if free, pages := pageStats(t, db); free != wantFree || pages != wantPages {
				t.Errorf("file has %d free of %d pages after the failed batch, want the committed %d of %d", free, pages, wantFree, wantPages)
			}
			verifyRows(t, db, "churn", retained)
			integrityOK(t, db)

			// A retry starts where the committed prefix stopped and completes.
			retry, err := db.Reclaim(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if retry.FreeBefore != wantFree || retry.Freed != retry.FreeBefore-retry.FreeAfter || retry.FreeAfter*10 > retry.Pages || !retry.Truncated {
				t.Errorf("retry = %+v, want it to start at %d free and finish below 10%% with a checkpoint", retry, wantFree)
			}
			verifyRows(t, db, "churn", retained)
			integrityOK(t, db)
			closeIt(t, db)
			reopened := openStore(t, path, Options{})
			verifyRows(t, reopened, "churn", retained)
			integrityOK(t, reopened)
		})
	}
}
