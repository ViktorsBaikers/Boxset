package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// The durable intent row. These proofs hold the store to the one rule the
// runtime relies on: an intent is written in the transaction that needs it, one
// open row per dedupe key, one live attempt per concurrency key.

// intent is one due background intent of the tests.
func intent(kind, key string) JobIntent {
	return JobIntent{
		Kind:           kind,
		Class:          JobRequired,
		Lane:           JobBackground,
		DedupeKey:      key,
		ConcurrencyKey: key,
		Params:         map[string]string{"generation": "7"},
		MaxAttempts:    3,
		DueAt:          1_800_000_000,
	}
}

func writeIntent(t *testing.T, db *DB, in JobIntent) (int64, bool) {
	t.Helper()
	var id int64
	var created bool
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		id, created, err = EnqueueJob(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	return id, created
}

func TestJobIntentCommit(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		mut  func(*JobIntent)
		want string
	}{
		{"kind", func(in *JobIntent) { in.Kind = "Alias Purge" }, "kind_invalid"},
		{"class", func(in *JobIntent) { in.Class = "normal" }, "class_invalid"},
		{"lane", func(in *JobIntent) { in.Lane = "fast" }, "lane_invalid"},
		{"dedupe key", func(in *JobIntent) { in.DedupeKey = "" }, "dedupe_key_invalid"},
		{"concurrency key", func(in *JobIntent) { in.DedupeKey = "k"; in.ConcurrencyKey = "" }, "concurrency_key_invalid"},
		{"max attempts", func(in *JobIntent) { in.MaxAttempts = 0 }, "max_attempts_invalid"},
		{"params", func(in *JobIntent) { in.Params = map[string]string{"blob": strings.Repeat("v", JobMaxParams)} }, "params_too_large"},
		{"due", func(in *JobIntent) { in.DueAt = 0 }, "due_at_invalid"},
		{"expiry", func(in *JobIntent) { e := in.DueAt; in.ExpiresAt = &e }, "expires_at_invalid"},
	} {
		t.Run("refuses a bad "+tc.name, func(t *testing.T) {
			in := intent("alias.purge", "alias-generation:7")
			tc.mut(&in)
			err := db.Write(ctx, func(tx *sql.Tx) error {
				_, _, err := EnqueueJob(ctx, tx, in)
				return err
			})
			if !errors.Is(err, ErrJobIntent) {
				t.Fatalf("EnqueueJob = %v, want ErrJobIntent", err)
			}
			var refusal *JobIntentError
			if !errors.As(err, &refusal) || refusal.Reason != tc.want {
				t.Fatalf("refusal = %+v, want reason %s", refusal, tc.want)
			}
			if n := count(t, db, `SELECT count(*) FROM job`); n != 0 {
				t.Fatalf("a refused intent wrote %d rows", n)
			}
		})
	}

	id, created := writeIntent(t, db, intent("alias.purge", "alias-generation:7"))
	if !created || id == 0 {
		t.Fatalf("first enqueue = id %d created %v", id, created)
	}
	// The same open work is the same row: no second effect.
	again, created := writeIntent(t, db, intent("alias.purge", "alias-generation:7"))
	if created || again != id {
		t.Fatalf("second enqueue = id %d created %v, want the open row %d", again, created, id)
	}
	if n := count(t, db, `SELECT count(*) FROM job`); n != 1 {
		t.Fatalf("rows after two enqueues of one intent = %d, want one", n)
	}
	job, err := db.ReadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobQueued || job.Attempts != 0 || job.MaxAttempts != 3 || job.Revision != 1 ||
		job.Params["generation"] != "7" || job.Class != JobRequired || job.Lane != JobBackground {
		t.Fatalf("the stored intent = %+v", job)
	}
}

func TestJobClaimOwnership(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	ctx := t.Context()
	now := int64(1_800_000_000)

	// An empty store has nothing to claim, and a claim writes nothing.
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot", now); err != nil || ok {
		t.Fatalf("claim on an empty store = ok %v err %v", ok, err)
	}

	later := intent("alias.purge", "later")
	later.DueAt = now + 60
	later.ConcurrencyKey = "later"
	laterID, _ := writeIntent(t, db, later)
	first := intent("alias.purge", "first")
	first.DueAt = now - 10
	firstID, _ := writeIntent(t, db, first)

	claimed, ok, err := db.ClaimJob(ctx, JobBackground, "boot-a", now)
	if err != nil || !ok {
		t.Fatalf("claim = ok %v err %v", ok, err)
	}
	if claimed.ID != firstID {
		t.Fatalf("claimed job %d, want the oldest due row %d", claimed.ID, firstID)
	}
	if claimed.State != JobRunning || claimed.Attempts != 1 || claimed.Revision != 2 || claimed.LeaseOwner != "boot-a" || claimed.StartedAt == nil || *claimed.StartedAt != now {
		t.Fatalf("the claimed row = %+v, want one running attempt under this boot", claimed)
	}
	// The row that is not due yet is not claimable, and the running row holds its key.
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot-a", now); err != nil || ok {
		t.Fatalf("claim while the only due row runs = ok %v err %v, want no work", ok, err)
	}
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot-a", now+60); err != nil || !ok {
		t.Fatalf("claim at the later due time = ok %v err %v, want the later row", ok, err)
	}
	// A second open row with the same concurrency key cannot be claimed either.
	blocked := intent("alias.purge", "blocked")
	blocked.ConcurrencyKey = "later"
	writeIntent(t, db, blocked)
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot-b", now+60); err != nil || ok {
		t.Fatalf("claim behind an owned key = ok %v err %v, want no work", ok, err)
	}

	// The schema itself refuses the two states the runtime depends on.
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE job SET state = 'paused' WHERE id = ?`, laterID)
		return err
	}); err == nil {
		t.Fatal("the schema accepted a state the runtime does not know")
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO job (kind, class, lane, dedupe_key, concurrency_key, state, max_attempts, due_at, created_at, updated_at)
			VALUES ('alias.purge', 'required', 'background', 'first', 'first', 'queued', 1, 1, 1, 1)`)
		return err
	}); err == nil {
		t.Fatal("a second open row with the same dedupe key was accepted")
	}
}

func TestJobTransitions(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	ctx := t.Context()
	now := int64(1_800_000_000)
	id, _ := writeIntent(t, db, intent("alias.purge", "alias-generation:7"))
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot", now); err != nil || !ok {
		t.Fatalf("claim = ok %v err %v", ok, err)
	}

	// A reconciling row keeps its key and records its reason parameters.
	if ok, err := db.SetJobReconciling(ctx, id, "outcome_unknown", map[string]string{"page": "2"}, now+1); err != nil || !ok {
		t.Fatalf("SetJobReconciling = ok %v err %v", ok, err)
	}
	job, err := db.ReadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobReconciling || job.ReasonCode != "outcome_unknown" || job.ReasonParams["page"] != "2" || job.Revision != 3 {
		t.Fatalf("the reconciling row = %+v", job)
	}
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot-b", now+1); err != nil || ok {
		t.Fatalf("claim behind a reconciling row = ok %v err %v, want no work", ok, err)
	}
	// Reconciliation requeues it, and the reason it carried stays as evidence.
	if ok, err := db.RequeueJob(ctx, id, 1, now+2, job.ReasonCode, job.ReasonParams, now+2); err != nil || !ok {
		t.Fatalf("RequeueJob = ok %v err %v", ok, err)
	}
	job, err = db.ReadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobQueued || job.Attempts != 1 || job.DueAt != now+2 || job.ReasonCode != "outcome_unknown" || job.Revision != 4 {
		t.Fatalf("the requeued row = %+v", job)
	}
	// The key is claimable again, and the terminal state clears the lease.
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "boot-b", now+2); err != nil || !ok {
		t.Fatalf("claim after the requeue = ok %v err %v", ok, err)
	}
	if ok, err := db.FinishJob(ctx, id, JobSucceeded, "aliases_purged", map[string]string{"generation": "7"}, now+3); err != nil || !ok {
		t.Fatalf("FinishJob = ok %v err %v", ok, err)
	}
	job, err = db.ReadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobSucceeded || job.ReasonCode != "aliases_purged" || job.LeaseOwner != "" || job.StartedAt != nil {
		t.Fatalf("the finished row = %+v", job)
	}
	// A finished row is never moved again.
	if ok, err := db.FinishJob(ctx, id, JobFailed, "handler_failed", nil, now+4); err != nil || ok {
		t.Fatalf("FinishJob of a terminal row = ok %v err %v, want no change", ok, err)
	}
}

func TestJobRecoveryAndSweep(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	ctx := t.Context()
	now := int64(1_800_000_000)
	id, _ := writeIntent(t, db, intent("alias.purge", "alias-generation:7"))
	if _, ok, err := db.ClaimJob(ctx, JobBackground, "earlier", now); err != nil || !ok {
		t.Fatalf("seed claim = ok %v err %v", ok, err)
	}
	// A foreign lease is interrupted; this process's own lease is left alone.
	moved, err := db.RecoverForeignRunning(ctx, "boot", now+1)
	if err != nil || moved != 1 {
		t.Fatalf("RecoverForeignRunning = %d (err %v), want the foreign row", moved, err)
	}
	job, err := db.ReadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobReconciling || job.ReasonCode != "attempt_interrupted" || job.LeaseOwner != "" {
		t.Fatalf("the interrupted row = %+v", job)
	}
	if reconciling, err := db.ReconcilingJobs(ctx); err != nil || len(reconciling) != 1 || reconciling[0].ID != id {
		t.Fatalf("ReconcilingJobs = %+v (err %v)", reconciling, err)
	}
	if moved, err := db.RecoverForeignRunning(ctx, "boot", now+2); err != nil || moved != 0 {
		t.Fatalf("RecoverForeignRunning with nothing foreign = %d (err %v)", moved, err)
	}

	// Reconciliation requeues the interrupted row, and only then is it claimable.
	if ok, err := db.RequeueJob(ctx, id, 1, now+2, job.ReasonCode, job.ReasonParams, now+2); err != nil || !ok {
		t.Fatalf("RequeueJob = ok %v err %v", ok, err)
	}
	// An attempt past its deadline records attempt_overdue once and keeps its key.
	claimed, ok, err := db.ClaimJob(ctx, JobBackground, "boot", now+2)
	if err != nil || !ok {
		t.Fatalf("claim after the requeue = ok %v err %v", ok, err)
	}
	if ok, err := db.MarkJobOverdue(ctx, claimed.ID, map[string]string{"deadline_s": "30"}, now+40); err != nil || !ok {
		t.Fatalf("MarkJobOverdue = ok %v err %v", ok, err)
	}
	if ok, err := db.MarkJobOverdue(ctx, claimed.ID, map[string]string{"deadline_s": "30"}, now+41); err != nil || ok {
		t.Fatalf("second MarkJobOverdue = ok %v err %v, want the reason recorded once", ok, err)
	}
	job, err = db.ReadJob(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobRunning || job.ReasonCode != "attempt_overdue" || job.ReasonParams["deadline_s"] != "30" {
		t.Fatalf("the overdue row = %+v, want it running with its key held", job)
	}

	// The sweep sees the running row, and nothing once it is terminal.
	if pending, err := db.SweepPending(ctx, now+40); err != nil || !pending {
		t.Fatalf("SweepPending with a running row = %v (err %v)", pending, err)
	}
	if ok, err := db.FinishJob(ctx, claimed.ID, JobSucceeded, "aliases_purged", nil, now+42); err != nil || !ok {
		t.Fatalf("FinishJob = ok %v err %v", ok, err)
	}
	if pending, err := db.SweepPending(ctx, now+43); err != nil || pending {
		t.Fatalf("SweepPending on an idle store = %v (err %v), want nothing", pending, err)
	}
}

// count runs one scalar read statement.
func count(t *testing.T, db *DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query).Scan(&n)
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
