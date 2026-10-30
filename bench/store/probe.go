package storebench

// The store foreground probe, exported: the foreground series measures it
// behind its own writers, and the effects harness measures the same transaction
// through the composition's store, so both series speak about one write instead
// of two look-alikes.

import (
	"context"
	"database/sql"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// CreateProbe makes the one-row table the probe writes on. It is part of a
// seed, never of a measured region.
func CreateProbe(ctx context.Context, db *store.DB) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE bench_probe (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bench_probe (id, n) VALUES (1, 0)`)
		return err
	})
}

// Probe is one tiny write transaction queued behind the workload's writers. Its
// wait is the time until the writer connection runs it (a retired connection's
// reconnect is part of that), its commit the time Commit takes. The clock (now,
// since) and the transaction call write are private function values: a measured
// run passes time.Now, time.Since and a store's Write, and a proof scripts the
// same four readings — the first before the call, the second and third inside it
// around the UPDATE, and the elapsed one after the call returned.
func Probe(ctx context.Context, now func() time.Time, since func(time.Time) time.Duration, write func(context.Context, func(*sql.Tx) error) error) (wait, commit time.Duration, err error) {
	var begun, written time.Time
	start := now()
	err = write(ctx, func(tx *sql.Tx) error {
		begun = now()
		_, err := tx.ExecContext(ctx, `UPDATE bench_probe SET n = n + 1 WHERE id = 1`)
		written = now()
		return err
	})
	return begun.Sub(start), since(written), err
}
