package jobs

// Interactive admission and the jobs read model. Interactive work is admitted
// before anything runs: one Write decides from the open rows alone, so a refusal
// creates nothing and the counts are the rows rather than a history that a
// restart would forget. Accept grants nothing; the operation that calls it has
// already decided its own right, and the caller only names whose share is spent.
// List is the same rows read back under those existing rights.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// The recorded admission bounds (architecture § Admission service).
const (
	// interactiveCap is the global ceiling of open interactive rows and
	// interactivePerCaller one caller's share of it.
	interactiveCap       = 16
	interactivePerCaller = 4
	// runNowDedupeKey is the open-row identity of a kind admitted on demand, so a
	// second request for the same kind is the job already there.
	runNowDedupeKey = "run-now"
	// optionalExpiry is how long an optional row may wait before the sweep
	// expires it (architecture § Data model).
	optionalExpiry = time.Hour
	// The waits the two refusals ask for.
	interactiveRetryAfter = 2 * time.Second
	queueRetryAfter       = 30 * time.Second
)

// ErrKindNotRunnable: the kind is not one a caller may ask for on demand.
var ErrKindNotRunnable = errors.New("jobs: kind is not runnable on demand")

// Caller is the authenticated principal behind one admission. It carries only
// the identity the per-caller count is kept by: a caller never names a right, a
// kind or a lane, and nothing here widens what the request already had.
type Caller struct{ UserID int64 }

// AdmissionError is one refusal: the recorded reason word and how long the
// caller should wait before trying again. Rows are the counters, so a caller
// that waits and retries is admitted once capacity exists.
type AdmissionError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("jobs: admission refused: %s", e.Reason)
}

// Job is one row of the jobs read model: what an authorized reader is shown of
// it. The identity keys, params, lease and creator stay in the store.
type Job struct {
	ID           int64
	Kind         string
	Class        string
	Lane         string
	State        string
	ReasonCode   string
	ReasonParams map[string]string
	Attempts     int
	MaxAttempts  int
	DueAt        int64
	CreatedAt    int64
	UpdatedAt    int64
	Revision     int64
}

// jobFromRow reads one stored row into the read model.
func jobFromRow(row storedb.Job) (Job, error) {
	job := Job{
		ID:          row.ID,
		Kind:        row.Kind,
		Class:       row.Class,
		Lane:        row.Lane,
		State:       row.State,
		ReasonCode:  row.ReasonCode.String,
		Attempts:    int(row.Attempts),
		MaxAttempts: int(row.MaxAttempts),
		DueAt:       row.DueAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		Revision:    row.Revision,
	}
	if row.ReasonParams.Valid {
		if err := json.Unmarshal([]byte(row.ReasonParams.String), &job.ReasonParams); err != nil {
			return Job{}, fmt.Errorf("jobs: reason params of job %d: %w", row.ID, err)
		}
	}
	return job, nil
}

// Accept admits one on-demand run of kind for c. The decision and the row it
// admits are one Write, so a refusal writes nothing and no effect can follow a
// row that never committed; the wake that makes the row claimable comes after
// that commit. An open run-now of the kind is the same work, so it is returned
// instead of taking a second row, and a kind the runtime cannot run on demand is
// refused before any read.
func (r *Runtime) Accept(ctx context.Context, c Caller, kind string) (Job, error) {
	if c.UserID < 1 {
		return Job{}, fmt.Errorf("%w: a caller without a user", ErrConfig)
	}
	h, ok := r.handlers[kind]
	if !ok || !h.RunNow {
		return Job{}, fmt.Errorf("%w: %s", ErrKindNotRunnable, kind)
	}
	now := r.cfg.Now()
	var job Job
	created := false
	err := r.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		// This request's own work may already be open: it is answered with that
		// row, exactly as a retry of an accepted request should be, so a cap this
		// row already helps fill cannot refuse the request that owns it.
		id, err := q.OpenJobID(ctx, storedb.OpenJobIDParams{Kind: kind, DedupeKey: runNowDedupeKey})
		switch {
		case err == nil:
			row, err := q.GetJob(ctx, id)
			if err != nil {
				return err
			}
			job, err = jobFromRow(row)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		open, err := q.CountOpenInteractive(ctx)
		if err != nil {
			return err
		}
		mine, err := q.CountOpenInteractiveFor(ctx, sql.NullInt64{Int64: c.UserID, Valid: true})
		if err != nil {
			return err
		}
		if mine >= interactivePerCaller || open >= interactiveCap {
			return &AdmissionError{Reason: "interactive_cap", RetryAfter: interactiveRetryAfter}
		}
		queued, err := q.CountOptionalQueued(ctx)
		if err != nil {
			return err
		}
		if queued >= maxQueuedOptional {
			return &AdmissionError{Reason: "queue_full", RetryAfter: queueRetryAfter}
		}
		expires := now.Add(optionalExpiry).Unix()
		createdID, inserted, err := store.EnqueueJob(ctx, tx, store.JobIntent{
			Kind:           kind,
			Class:          string(h.Class),
			Lane:           string(LaneInteractive),
			DedupeKey:      runNowDedupeKey,
			ConcurrencyKey: kind,
			MaxAttempts:    h.MaxAttempts,
			DueAt:          now.Unix(),
			ExpiresAt:      &expires,
			CreatedBy:      &c.UserID,
		})
		if err != nil {
			return err
		}
		// createdID is this request's row: the one just inserted, or the open row
		// that appeared between the read and the insert.
		created = inserted
		row, err := q.GetJob(ctx, createdID)
		if err != nil {
			return err
		}
		job, err = jobFromRow(row)
		return err
	})
	if err != nil {
		return Job{}, err
	}
	if created {
		r.Wake()
	}
	return job, nil
}

// Query is one page request of the jobs read model: the page bound, an optional
// state filter and the id of the row the previous page ended at.
type Query struct {
	Limit   int
	State   string
	AfterID int64
}

// Page is one page of jobs with the identity it was read under, so a cursor is
// only honoured inside the installation and restore epoch that produced it.
type Page struct {
	Items        []Job
	InstallID    string
	RestoreEpoch int64
	More         bool
}

// List reads one page of rows newest first, in one read that also reads the
// identity the page was taken under. It decides nothing and writes nothing.
func (r *Runtime) List(ctx context.Context, q Query) (Page, error) {
	var page Page
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		gen := storedb.New(tx)
		installation, err := gen.GetInstallation(ctx)
		if err != nil {
			return err
		}
		// A page is bounded by the id it starts below, and one row past the page
		// bound is what tells whether another page exists.
		after := q.AfterID
		if after == 0 {
			after = math.MaxInt64
		}
		maxRows := int64(q.Limit) + 1
		var rows []storedb.Job
		if q.State == "" {
			rows, err = gen.PageJobs(ctx, storedb.PageJobsParams{ID: after, Limit: maxRows})
		} else {
			rows, err = gen.PageJobsInState(ctx, storedb.PageJobsInStateParams{State: q.State, ID: after, Limit: maxRows})
		}
		if err != nil {
			return err
		}
		page = Page{
			Items:        make([]Job, 0, min(q.Limit, len(rows))),
			InstallID:    installation.InstallID,
			RestoreEpoch: installation.RestoreEpoch,
			More:         len(rows) > q.Limit,
		}
		for _, row := range rows[:min(q.Limit, len(rows))] {
			job, err := jobFromRow(row)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, job)
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}
