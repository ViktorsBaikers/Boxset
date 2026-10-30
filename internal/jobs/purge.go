package jobs

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/library"
)

// KindAliasPurge deletes the aliases of an alias generation whose activation
// retired it. It is required work: the generation row is retired in the same
// Write that enqueued this job, so the aliases are already invisible and the
// intent to delete them must survive a crash.
const KindAliasPurge = "alias.purge"

const (
	// purgeBatch is the most aliases one transaction deletes.
	purgeBatch = 500
	// purgeEvery is how long one attempt keeps deleting before it reports
	// progress; the runtime requeues it at once with its attempt count reset.
	purgeEvery = 10 * time.Second
	// purgeMaxAttempts and purgeDeadline are this kind's recorded budget.
	purgeMaxAttempts = 5
	purgeDeadline    = 30 * time.Second
)

// PurgeHandler is the alias.purge load over the library owner.
func PurgeHandler(lib *library.Library) Handler {
	return Handler{
		Kind:        KindAliasPurge,
		Class:       ClassRequired,
		Lane:        LaneBackground,
		MaxAttempts: purgeMaxAttempts,
		Deadline:    purgeDeadline,
		Run: func(ctx context.Context, a *Attempt) Outcome {
			raw, ok := a.Params["generation"]
			if !ok {
				return Outcome{Result: Failed, Reason: "params_invalid", Params: map[string]string{"missing": "generation"}}
			}
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id < 1 {
				return Outcome{Result: Failed, Reason: "params_invalid", Params: map[string]string{"generation": raw}}
			}
			// The generation is a retired row: only staging may activate, so it
			// stays inactive and its aliases stay invisible while they are deleted.
			started := time.Now()
			for {
				remaining, err := lib.PurgeGeneration(ctx, library.GenerationID(id), purgeBatch)
				switch {
				case errors.Is(err, library.ErrNotFound):
					// The generation row is gone, so its aliases went with it.
					return Outcome{Result: Succeeded, Reason: "generation_absent", Params: map[string]string{"generation": raw}}
				case errors.Is(err, library.ErrInvalidInput):
					// An active generation is refused by the library: never retried.
					return Outcome{Result: Failed, Reason: "purge_refused", Params: map[string]string{"generation": raw}}
				case err != nil:
					return Outcome{Result: Retry, Reason: "purge_failed", Params: map[string]string{"generation": raw}}
				case remaining == 0:
					return Outcome{Result: Succeeded, Reason: "aliases_purged", Params: map[string]string{"generation": raw}}
				}
				if time.Since(started) >= purgeEvery {
					return Outcome{Result: Progress, Params: map[string]string{"generation": raw}}
				}
			}
		},
	}
}
