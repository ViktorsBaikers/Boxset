package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// Durable accepted work. A job row is the intent the domain wrote in its own
// transaction; this file moves it, one Write per transition, and never decides
// policy: which kind, class, lane, key and attempt budget a piece of work gets
// belongs to the owner that writes the intent (internal/library, internal/jobs).

// The class of a job: required work is never expired or pruned and exhaustion
// leaves it in review; optional work may expire and fails terminally.
const (
	JobRequired = "required"
	JobOptional = "optional"
)

// The lane a job runs in: interactive work keeps its own slots.
const (
	JobInteractive = "interactive"
	JobBackground  = "background"
)

// The states a job moves through. queued, running, reconciling and review are
// open: an open row owns its dedupe key and, from running on, its concurrency key.
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobReconciling = "reconciling"
	JobSucceeded   = "succeeded"
	JobFailed      = "failed"
	JobReview      = "review"
	JobExpired     = "expired"
)

// JobMaxAttempts and JobMaxParams are the bounds the schema enforces, named
// here so a caller refuses before the statement does.
const (
	JobMaxAttempts = 10
	JobMaxParams   = 4096
)

// ErrJobIntent: the intent handed to EnqueueJob cannot be one job row.
var ErrJobIntent = errors.New("store: job intent refused")

// JobIntentError is a refused intent, with the reason word and its parameters.
type JobIntentError struct {
	Reason string
	Params map[string]string
}

func (e *JobIntentError) Error() string {
	return fmt.Sprintf("%v reason=%s params=%v", ErrJobIntent, e.Reason, e.Params)
}

// Is lets errors.Is(err, ErrJobIntent) match any refused intent.
func (e *JobIntentError) Is(target error) bool { return target == ErrJobIntent }

// JobIntent is one piece of durable work, written in the transaction that needs
// it. Params are non-secret values the handler reads back from the row.
type JobIntent struct {
	Kind           string
	Class          string
	Lane           string
	DedupeKey      string
	ConcurrencyKey string
	Params         map[string]string
	MaxAttempts    int
	DueAt          int64
	ExpiresAt      *int64
	CreatedBy      *int64
}

// validate refuses an intent the schema or the runtime could not honour, before
// any statement runs.
func (in JobIntent) validate() error {
	refuse := func(reason string, params map[string]string) error {
		return &JobIntentError{Reason: reason, Params: params}
	}
	if !legibleToken(in.Kind, 64) {
		return refuse("kind_invalid", map[string]string{"kind": in.Kind})
	}
	if in.Class != JobRequired && in.Class != JobOptional {
		return refuse("class_invalid", map[string]string{"class": in.Class})
	}
	if in.Lane != JobInteractive && in.Lane != JobBackground {
		return refuse("lane_invalid", map[string]string{"lane": in.Lane})
	}
	if in.DedupeKey == "" || len(in.DedupeKey) > 128 {
		return refuse("dedupe_key_invalid", map[string]string{})
	}
	if in.ConcurrencyKey == "" || len(in.ConcurrencyKey) > 128 {
		return refuse("concurrency_key_invalid", map[string]string{})
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > JobMaxAttempts {
		return refuse("max_attempts_invalid", map[string]string{"max_attempts": fmt.Sprint(in.MaxAttempts)})
	}
	body, err := encodeJobParams(in.Params)
	if err != nil {
		return err
	}
	if len(body) > JobMaxParams {
		return refuse("params_too_large", map[string]string{"bytes": fmt.Sprint(len(body)), "max": fmt.Sprint(JobMaxParams)})
	}
	if in.DueAt <= 0 {
		return refuse("due_at_invalid", map[string]string{})
	}
	if in.ExpiresAt != nil && *in.ExpiresAt <= in.DueAt {
		return refuse("expires_at_invalid", map[string]string{})
	}
	return nil
}

// legibleToken reports whether s is 1..max bytes of the kind alphabet.
func legibleToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// encodeJobParams is the one JSON form of a params map: encoding/json sorts map
// keys, so equal maps always produce equal bytes and one stored value.
func encodeJobParams(params map[string]string) (string, error) {
	if len(params) == 0 {
		return "{}", nil
	}
	body, err := json.Marshal(params)
	if err != nil {
		return "", &JobIntentError{Reason: "params_invalid", Params: map[string]string{}}
	}
	return string(body), nil
}

// decodeJobParams reads a stored params or reason_params value. A value that is
// not an object of strings is reported, never guessed at.
func decodeJobParams(body string) (map[string]string, error) {
	out := map[string]string{}
	if body == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("store: job params: %w", err)
	}
	return out, nil
}

// reasonParamsOf is the stored form of a reason's parameters, NULL when empty.
func reasonParamsOf(params map[string]string) (sql.NullString, error) {
	if len(params) == 0 {
		return sql.NullString{}, nil
	}
	body, err := json.Marshal(params)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("store: job reason params: %w", err)
	}
	if len(body) > 1024 {
		return sql.NullString{}, &JobIntentError{Reason: "reason_params_too_large", Params: map[string]string{"bytes": fmt.Sprint(len(body))}}
	}
	return sql.NullString{String: string(body), Valid: true}, nil
}

// reasonCodeOf is the stored form of a reason word, NULL when there is none.
func reasonCodeOf(reason string) sql.NullString {
	return sql.NullString{String: reason, Valid: reason != ""}
}

// EnqueueJob writes one job intent in the caller's transaction, so required work
// and the domain change that needs it commit together or not at all. An open row
// with the same (kind, dedupe_key) is the answer: its id is returned with
// created=false and nothing is written. It runs only SQL, so a caller inside a
// Write can compose it with its own statements.
func EnqueueJob(ctx context.Context, tx *sql.Tx, in JobIntent) (int64, bool, error) {
	if err := in.validate(); err != nil {
		return 0, false, err
	}
	params, err := encodeJobParams(in.Params)
	if err != nil {
		return 0, false, err
	}
	// created_at and updated_at are the write instant, not the due instant: an
	// intent written for later is still created now.
	now := time.Now().Unix()
	q := storedb.New(tx)
	rows, err := q.InsertOpenJob(ctx, storedb.InsertOpenJobParams{
		Kind:           in.Kind,
		Class:          in.Class,
		Lane:           in.Lane,
		DedupeKey:      in.DedupeKey,
		ConcurrencyKey: in.ConcurrencyKey,
		Params:         params,
		MaxAttempts:    int64(in.MaxAttempts),
		DueAt:          in.DueAt,
		ExpiresAt:      nullable(in.ExpiresAt),
		CreatedBy:      nullable(in.CreatedBy),
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		return 0, false, err
	}
	id, err := q.OpenJobID(ctx, storedb.OpenJobIDParams{Kind: in.Kind, DedupeKey: in.DedupeKey})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("%w: no open row for %s/%s", ErrJobIntent, in.Kind, in.DedupeKey)
	}
	if err != nil {
		return 0, false, err
	}
	return id, rows == 1, nil
}

// nullable maps an optional instant to the SQL value of its column.
func nullable(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// optional maps a nullable column back to an optional instant.
func optional(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

// Job is one job row as the runtime reads it.
type Job struct {
	ID             int64
	Kind           string
	Class          string
	Lane           string
	DedupeKey      string
	ConcurrencyKey string
	Params         map[string]string
	State          string
	ReasonCode     string
	ReasonParams   map[string]string
	Attempts       int
	MaxAttempts    int
	DueAt          int64
	ExpiresAt      *int64
	LeaseOwner     string
	StartedAt      *int64
	CreatedBy      *int64
	CreatedAt      int64
	UpdatedAt      int64
	Revision       int64
}

// jobOf turns a generated row into a Job, decoding the two JSON columns.
func jobOf(row storedb.Job) (Job, error) {
	params, err := decodeJobParams(row.Params)
	if err != nil {
		return Job{}, err
	}
	reason := map[string]string{}
	if row.ReasonParams.Valid {
		if reason, err = decodeJobParams(row.ReasonParams.String); err != nil {
			return Job{}, err
		}
	}
	return Job{
		ID:             row.ID,
		Kind:           row.Kind,
		Class:          row.Class,
		Lane:           row.Lane,
		DedupeKey:      row.DedupeKey,
		ConcurrencyKey: row.ConcurrencyKey,
		Params:         params,
		State:          row.State,
		ReasonCode:     row.ReasonCode.String,
		ReasonParams:   reason,
		Attempts:       int(row.Attempts),
		MaxAttempts:    int(row.MaxAttempts),
		DueAt:          row.DueAt,
		ExpiresAt:      optional(row.ExpiresAt),
		LeaseOwner:     row.LeaseOwner.String,
		StartedAt:      optional(row.AttemptStartedAt),
		CreatedBy:      optional(row.CreatedBy),
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		Revision:       row.Revision,
	}, nil
}

// ClaimJob takes the oldest due, unexpired, key-unowned queued row of lane and
// makes it running with boot as its lease owner. A candidate whose key is not
// free is not claimable; the answer is a zero Job and false, and nothing is
// written, so an idle claim commits nothing.
func (d *DB) ClaimJob(ctx context.Context, lane, boot string, now int64) (Job, bool, error) {
	var id int64
	err := d.Read(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = storedb.New(tx).ClaimCandidate(ctx, storedb.ClaimCandidateParams{
			Lane:      lane,
			DueAt:     now,
			ExpiresAt: sql.NullInt64{Int64: now, Valid: true},
		})
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	var job Job
	taken := false
	err = d.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		n, err := q.ClaimJob(ctx, storedb.ClaimJobParams{
			LeaseOwner:       sql.NullString{String: boot, Valid: true},
			AttemptStartedAt: sql.NullInt64{Int64: now, Valid: true},
			UpdatedAt:        now,
			ID:               id,
		})
		if err != nil || n == 0 {
			return err
		}
		row, err := q.GetJob(ctx, id)
		if err != nil {
			return err
		}
		if job, err = jobOf(row); err != nil {
			return err
		}
		taken = true
		return nil
	})
	if err != nil {
		return Job{}, false, err
	}
	if !taken {
		return Job{}, false, nil
	}
	return job, true, nil
}

// ReadJob returns one job row.
func (d *DB) ReadJob(ctx context.Context, id int64) (Job, error) {
	var job Job
	err := d.Read(ctx, func(tx *sql.Tx) error {
		row, err := storedb.New(tx).GetJob(ctx, id)
		if err != nil {
			return err
		}
		job, err = jobOf(row)
		return err
	})
	return job, err
}

// JobsInState returns the rows in state, oldest id first.
func (d *DB) JobsInState(ctx context.Context, state string) ([]Job, error) {
	var out []Job
	err := d.Read(ctx, func(tx *sql.Tx) error {
		rows, err := storedb.New(tx).ListJobsInState(ctx, state)
		if err != nil {
			return err
		}
		out = make([]Job, 0, len(rows))
		for _, row := range rows {
			job, err := jobOf(row)
			if err != nil {
				return err
			}
			out = append(out, job)
		}
		return nil
	})
	return out, err
}

// RunningJobs is every row an attempt is running, whatever the lease owner.
func (d *DB) RunningJobs(ctx context.Context) ([]Job, error) {
	return d.JobsInState(ctx, JobRunning)
}

// ReconcilingJobs is every row whose outcome is not decided yet.
func (d *DB) ReconcilingJobs(ctx context.Context) ([]Job, error) {
	return d.JobsInState(ctx, JobReconciling)
}

// RecoverForeignRunning moves the running rows of another start to reconciling
// with attempt_interrupted, so no key they hold is claimable before their
// reconciliation ran. It returns how many rows it moved.
func (d *DB) RecoverForeignRunning(ctx context.Context, boot string, now int64) (int64, error) {
	var moved int64
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		moved, err = storedb.New(tx).RecoverForeignRunning(ctx, storedb.RecoverForeignRunningParams{
			UpdatedAt:  now,
			LeaseOwner: sql.NullString{String: boot, Valid: true},
		})
		return err
	})
	return moved, err
}

// MarkJobOverdue records attempt_overdue on a running row. The row keeps its
// key: only its reconciliation may release it.
func (d *DB) MarkJobOverdue(ctx context.Context, id int64, params map[string]string, now int64) (bool, error) {
	reason, err := reasonParamsOf(params)
	if err != nil {
		return false, err
	}
	var n int64
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).MarkJobOverdue(ctx, storedb.MarkJobOverdueParams{
			ReasonParams: reason,
			UpdatedAt:    now,
			ID:           id,
		})
		return err
	})
	return n > 0, err
}

// ExpireOptionalJobs expires the optional queued rows whose expiry passed.
func (d *DB) ExpireOptionalJobs(ctx context.Context, now int64) (int64, error) {
	var n int64
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).ExpireOptionalJobs(ctx, storedb.ExpireOptionalJobsParams{
			UpdatedAt: now,
			ExpiresAt: sql.NullInt64{Int64: now, Valid: true},
		})
		return err
	})
	return n, err
}

// FinishJob records a terminal state with its reason. It reports whether the row
// was still open.
func (d *DB) FinishJob(ctx context.Context, id int64, state, reason string, params map[string]string, now int64) (bool, error) {
	reasonParams, err := reasonParamsOf(params)
	if err != nil {
		return false, err
	}
	var n int64
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).FinishJob(ctx, storedb.FinishJobParams{
			State:        state,
			ReasonCode:   reasonCodeOf(reason),
			ReasonParams: reasonParams,
			UpdatedAt:    now,
			ID:           id,
		})
		return err
	})
	return n > 0, err
}

// RequeueJob makes an open row queued again at dueAt with attempts as the new
// count, which is 0 for work that reported progress.
func (d *DB) RequeueJob(ctx context.Context, id int64, attempts int, dueAt int64, reason string, params map[string]string, now int64) (bool, error) {
	reasonParams, err := reasonParamsOf(params)
	if err != nil {
		return false, err
	}
	var n int64
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).RequeueJob(ctx, storedb.RequeueJobParams{
			ReasonCode:   reasonCodeOf(reason),
			ReasonParams: reasonParams,
			Attempts:     int64(attempts),
			DueAt:        dueAt,
			UpdatedAt:    now,
			ID:           id,
		})
		return err
	})
	return n > 0, err
}

// SetJobReconciling parks an open row until its reconciliation decides what its
// attempt did. The row keeps its key.
func (d *DB) SetJobReconciling(ctx context.Context, id int64, reason string, params map[string]string, now int64) (bool, error) {
	reasonParams, err := reasonParamsOf(params)
	if err != nil {
		return false, err
	}
	var n int64
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).ReconcileJob(ctx, storedb.ReconcileJobParams{
			ReasonCode:   reasonCodeOf(reason),
			ReasonParams: reasonParams,
			UpdatedAt:    now,
			ID:           id,
		})
		return err
	})
	return n > 0, err
}

// SweepPending reports whether a sweep has anything to look at: a running or
// reconciling row, or queued work that is due or past its expiry. An idle sweep
// reads this and writes nothing.
func (d *DB) SweepPending(ctx context.Context, now int64) (bool, error) {
	var n int64
	err := d.Read(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = storedb.New(tx).SweepWork(ctx, storedb.SweepWorkParams{
			DueAt:     now,
			ExpiresAt: sql.NullInt64{Int64: now, Valid: true},
		})
		return err
	})
	return n > 0, err
}
