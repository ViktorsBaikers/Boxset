package jobs

// The scheduled maintenance the installation runs about itself: the terminal
// rows of optional work past their retention leave the table in bounded batches,
// so the table stays usable without a writer that blocks the work an
// installation is actually asked to do. Required work is never selected: an
// accepted effect keeps its row until an owner decides otherwise.

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// KindJobsPrune is the maintenance kind, fired daily by the built-in schedule
// and runnable on demand.
const KindJobsPrune = "jobs.prune"

const (
	// pruneLocalTime is the built-in schedule's wall clock: 04:00 in the
	// installation's zone.
	pruneLocalTime = "04:00"
	// pruneBatch is the most rows one Write deletes.
	pruneBatch = 500
	// pruneEvery is how long one attempt keeps deleting before it reports
	// progress; the runtime requeues it at once with its attempt count reset.
	pruneEvery = 10 * time.Second
	// pruneMaxAttempts and pruneDeadline are this kind's recorded budget.
	pruneMaxAttempts = 3
	pruneDeadline    = 60 * time.Second
	// pruneRetention is how long a terminal row is kept.
	pruneRetention = 7 * 24 * time.Hour
)

// PruneHandler is the jobs.prune load over the store: one bounded batch per
// Write until the retention set is empty.
func PruneHandler(db *store.DB) Handler {
	return Handler{
		Kind:        KindJobsPrune,
		Class:       ClassOptional,
		Lane:        LaneBackground,
		RunNow:      true,
		MaxAttempts: pruneMaxAttempts,
		Deadline:    pruneDeadline,
		Run: func(ctx context.Context, a *Attempt) Outcome {
			started := time.Now()
			pruned := 0
			for {
				cutoff := time.Now().Add(-pruneRetention).Unix()
				n, err := pruneTerminal(ctx, db, cutoff)
				if err != nil {
					return Outcome{Result: Retry, Reason: "prune_failed", Params: map[string]string{"pruned": strconv.Itoa(pruned)}}
				}
				pruned += int(n)
				if n < pruneBatch {
					return Outcome{Result: Succeeded, Reason: "jobs_pruned", Params: map[string]string{"pruned": strconv.Itoa(pruned)}}
				}
				if time.Since(started) >= pruneEvery {
					return Outcome{Result: Progress, Params: map[string]string{"pruned": strconv.Itoa(pruned)}}
				}
			}
		},
	}
}

// pruneTerminal deletes one batch of terminal optional rows older than cutoff.
func pruneTerminal(ctx context.Context, db *store.DB, cutoff int64) (int64, error) {
	var n int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).PruneTerminalJobs(ctx, cutoff)
		return err
	})
	return n, err
}
