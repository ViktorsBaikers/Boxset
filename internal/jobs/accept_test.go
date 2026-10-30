package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// admitRuntime is a runtime that is not started, so an admission proof counts
// exactly the rows Accept wrote instead of what a lane did with them.
func admitRuntime(t *testing.T, db *store.DB, clock *fakeClock, handlers ...Handler) *Runtime {
	t.Helper()
	rt, err := New(db, Config{
		Now:             clock.Now,
		After:           clock.After,
		Boot:            "boot-admit",
		BackgroundSlots: 1,
		Log:             func(string, ...any) {},
	}, handlers)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return rt
}

// callerRows creates n users, so an admitted row's creator is a row the store can
// reference: a real admission is always attributed to an authenticated user.
func callerRows(t *testing.T, db *store.DB, n int) {
	t.Helper()
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for id := range n {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO user (id, username, username_key, password_phc, role, created_at) VALUES (?, ?, ?, 'synthetic-phc', 'admin', 0)`,
				id+1, fmt.Sprintf("caller-%d", id+1), fmt.Sprintf("caller-%d", id+1)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// runNowHandler is one optional kind a caller may ask for on demand.
func runNowHandler(kind string, ran chan<- struct{}) Handler {
	h := handler(kind, func(context.Context, *Attempt) Outcome {
		if ran != nil {
			ran <- struct{}{}
		}
		return Outcome{Result: Succeeded, Reason: "done"}
	})
	h.Class = ClassOptional
	h.RunNow = true
	return h
}

// runNowKinds is a set of kinds, so each admission takes its own open row: a
// second request of one kind is the row the first one opened.
func runNowKinds(n int) ([]string, []Handler) {
	kinds := make([]string, 0, n)
	handlers := make([]Handler, 0, n)
	for i := range n {
		kind := fmt.Sprintf("run.k%d", i)
		kinds = append(kinds, kind)
		handlers = append(handlers, runNowHandler(kind, nil))
	}
	return kinds, handlers
}

// The admission bounds as AC-024 states them: sixteen open interactive rows
// globally, four of them one caller's, and one thousand and twenty-four queued
// optional rows. They are literals here on purpose: a proof that seeded from the
// package's own constants would move with them and stay green at any bound.
const (
	specOpenInteractive = 16
	specPerCaller       = 4
	specQueuedOptional  = 1024
)

// fillOptionalQueue writes n queued optional rows, the work the admission
// refuses against; the caller seeds the count the spec states.
func fillOptionalQueue(t *testing.T, db *store.DB, clock *fakeClock, n int) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			key := fmt.Sprintf("bulk-%d", i)
			if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
				Kind:           "bulk.queue",
				Class:          store.JobOptional,
				Lane:           store.JobBackground,
				DedupeKey:      key,
				ConcurrencyKey: key,
				MaxAttempts:    1,
				DueAt:          clock.Now().Unix(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fill optional queue: %v", err)
	}
}

// refusal is the AdmissionError in err, failed when err is a different outcome.
func refusal(t *testing.T, err error, reason string, wait time.Duration) {
	t.Helper()
	var refused *AdmissionError
	if !errors.As(err, &refused) {
		t.Fatalf("admission = %v, want %s after %s", err, reason, wait)
	}
	if refused.Reason != reason || refused.RetryAfter != wait {
		t.Fatalf("admission = %s after %s, want %s after %s", refused.Reason, refused.RetryAfter, reason, wait)
	}
}

// TestWorkAdmission proves AC-024 at the admission service: interactive work is
// admitted or refused before any row exists or any effect runs, the open rows are
// the counters, and the same bounds hold after a restart.
func TestWorkAdmission(t *testing.T) {
	t.Run("the fifth request of one caller is refused", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 1)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(interactivePerCaller + 1)
		rt := admitRuntime(t, db, clock, handlers...)
		for i, kind := range kinds[:interactivePerCaller] {
			job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kind)
			if err != nil || job.ID < 1 || job.State != store.JobQueued {
				t.Fatalf("admission %d = %+v, %v", i+1, job, err)
			}
		}
		job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kinds[interactivePerCaller])
		refusal(t, err, "interactive_cap", 2*time.Second)
		if job.ID != 0 {
			t.Errorf("refused admission returned job %d", job.ID)
		}
		if rows := jobRows(t, db); rows != interactivePerCaller {
			t.Fatalf("rows after the refusal = %d, want %d: a refusal creates nothing", rows, interactivePerCaller)
		}
	})

	t.Run("the sixteenth open request is admitted and the seventeenth refused", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(specOpenInteractive + 1)
		rt := admitRuntime(t, db, clock, handlers...)
		// Fifteen of the sixteen open rows the spec allows: three callers at
		// their own share, and the one that takes the sixteenth a row short. A
		// kind's open run-now is that kind's work whoever asked for it, so the
		// rows are distinct kinds.
		callers := specOpenInteractive/specPerCaller + 1
		callerRows(t, db, callers)
		open := 0
		for caller := range callers - 1 {
			share := specPerCaller
			if caller == callers-2 {
				share = specPerCaller - 1
			}
			for range share {
				if _, err := rt.Accept(t.Context(), Caller{UserID: int64(caller) + 1}, kinds[open]); err != nil {
					t.Fatalf("open row %d for caller %d: %v", open+1, caller+1, err)
				}
				open++
			}
		}
		if open != specOpenInteractive-1 {
			t.Fatalf("seeded %d open rows, want %d: the ceiling is never reached", open, specOpenInteractive-1)
		}
		// The sixteenth row is inside both bounds: the global ceiling and the
		// fourth caller's share. It joins the interactive lane, so the count that
		// refuses the next request is the row this one wrote.
		job, err := rt.Accept(t.Context(), Caller{UserID: int64(callers) - 1}, kinds[open])
		if err != nil || job.ID < 1 || job.State != store.JobQueued || job.Lane != store.JobInteractive {
			t.Fatalf("the %dth open request = %+v, %v, want queued interactive work admitted", specOpenInteractive, job, err)
		}
		if rows := jobRows(t, db); rows != specOpenInteractive {
			t.Fatalf("rows after the admission = %d, want the global cap %d", rows, specOpenInteractive)
		}
		// A caller holding none of the open rows, so the refusal is the global
		// ceiling and not that caller's share.
		_, err = rt.Accept(t.Context(), Caller{UserID: int64(callers)}, kinds[open+1])
		refusal(t, err, "interactive_cap", 2*time.Second)
		if rows := jobRows(t, db); rows != specOpenInteractive {
			t.Fatalf("rows after the refusal = %d, want the global cap %d", rows, specOpenInteractive)
		}
	})

	t.Run("a full optional queue is refused before the insert", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		_, handlers := runNowKinds(1)
		rt := admitRuntime(t, db, clock, handlers...)
		fillOptionalQueue(t, db, clock, specQueuedOptional)
		_, err := rt.Accept(t.Context(), Caller{UserID: 1}, "run.k0")
		refusal(t, err, "queue_full", 30*time.Second)
		if rows := jobRows(t, db); rows != specQueuedOptional {
			t.Fatalf("rows after the refusal = %d, want the %d queued optional rows", rows, specQueuedOptional)
		}
	})

	t.Run("the 1024th queued optional request is admitted and the 1025th refused", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 2)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(2)
		rt := admitRuntime(t, db, clock, handlers...)
		fillOptionalQueue(t, db, clock, specQueuedOptional-1)
		if rows := jobRows(t, db); rows != specQueuedOptional-1 {
			t.Fatalf("queued rows = %d, want %d", rows, specQueuedOptional-1)
		}
		// The admitted run-now is queued optional work, so it fills the queue
		// itself; another caller and kind prove the next refusal is the queue and
		// not a share or an open run-now of the same work.
		job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kinds[0])
		if err != nil || job.ID < 1 || job.Class != store.JobOptional || job.Lane != store.JobInteractive {
			t.Fatalf("the %dth queued request = %+v, %v, want queued optional work admitted", specQueuedOptional, job, err)
		}
		if rows := jobRows(t, db); rows != specQueuedOptional {
			t.Fatalf("rows after the admission = %d, want %d", rows, specQueuedOptional)
		}
		_, err = rt.Accept(t.Context(), Caller{UserID: 2}, kinds[1])
		refusal(t, err, "queue_full", 30*time.Second)
		if rows := jobRows(t, db); rows != specQueuedOptional {
			t.Fatalf("rows after the refusal = %d, want %d", rows, specQueuedOptional)
		}
	})

	t.Run("open rows are the counters, finished ones are history", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(interactivePerCaller + 1)
		rt := admitRuntime(t, db, clock, handlers...)
		var first int64
		for i, kind := range kinds[:interactivePerCaller] {
			job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kind)
			if err != nil {
				t.Fatalf("admission %d: %v", i+1, err)
			}
			if i == 0 {
				first = job.ID
			}
		}
		if _, err := rt.Accept(t.Context(), Caller{UserID: 1}, kinds[interactivePerCaller]); err == nil {
			t.Fatal("a fifth open request was admitted")
		}
		// The attempt finished: its row leaves the open set and its share returns.
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE job SET state = 'succeeded', revision = revision + 1 WHERE id = ?`, first)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kinds[interactivePerCaller])
		if err != nil || job.ID < 1 {
			t.Fatalf("admission after a finished row = %+v, %v", job, err)
		}
		if rows := jobRows(t, db); rows != interactivePerCaller+1 {
			t.Fatalf("rows = %d, want %d", rows, interactivePerCaller+1)
		}
	})

	t.Run("the open rows survive a restart", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(interactivePerCaller + 1)
		rt := admitRuntime(t, db, clock, handlers...)
		for i, kind := range kinds[:interactivePerCaller] {
			if _, err := rt.Accept(t.Context(), Caller{UserID: 1}, kind); err != nil {
				t.Fatalf("admission %d: %v", i+1, err)
			}
		}
		restarted := admitRuntime(t, db, clock, handlers...)
		_, err := restarted.Accept(t.Context(), Caller{UserID: 1}, kinds[interactivePerCaller])
		refusal(t, err, "interactive_cap", 2*time.Second)
		if rows := jobRows(t, db); rows != interactivePerCaller {
			t.Fatalf("rows after the restart refusal = %d, want %d: a refusal creates nothing", rows, interactivePerCaller)
		}
	})

	t.Run("a run-now of the kind already open is that job", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		_, handlers := runNowKinds(1)
		rt := admitRuntime(t, db, clock, handlers...)
		first, err := rt.Accept(t.Context(), Caller{UserID: 1}, "run.k0")
		if err != nil {
			t.Fatal(err)
		}
		second, err := rt.Accept(t.Context(), Caller{UserID: 1}, "run.k0")
		if err != nil {
			t.Fatalf("coalesced admission: %v", err)
		}
		if first.ID != second.ID {
			t.Fatalf("coalesced rows %d and %d, want the one open row", first.ID, second.ID)
		}
		if rows := jobRows(t, db); rows != 1 {
			t.Fatalf("rows = %d, want 1: a second request takes no row", rows)
		}
	})

	t.Run("a retry of an open run-now is answered at the caller's cap", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 1)
		clock := newFakeClock()
		kinds, handlers := runNowKinds(interactivePerCaller + 1)
		rt := admitRuntime(t, db, clock, handlers...)
		var first int64
		for i, kind := range kinds[:interactivePerCaller] {
			job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kind)
			if err != nil {
				t.Fatalf("admission %d: %v", i+1, err)
			}
			if i == 0 {
				first = job.ID
			}
		}
		// The caller is at its whole share, and the retry owns one of those rows:
		// it is answered with that row instead of a cap the row itself fills.
		job, err := rt.Accept(t.Context(), Caller{UserID: 1}, kinds[0])
		if err != nil || job.ID != first {
			t.Fatalf("the retry of an open run-now = %+v, %v, want row %d", job, err, first)
		}
		if rows := jobRows(t, db); rows != interactivePerCaller {
			t.Fatalf("rows after the retry = %d, want %d", rows, interactivePerCaller)
		}
	})

	t.Run("a kind not runnable on demand is refused", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		scheduled := handler("run.scheduled", func(context.Context, *Attempt) Outcome {
			return Outcome{Result: Succeeded, Reason: "done"}
		})
		rt := admitRuntime(t, db, clock, scheduled)
		if _, err := rt.Accept(t.Context(), Caller{UserID: 1}, "run.scheduled"); !errors.Is(err, ErrKindNotRunnable) {
			t.Errorf("scheduled kind = %v, want ErrKindNotRunnable", err)
		}
		if _, err := rt.Accept(t.Context(), Caller{UserID: 1}, "run.absent"); !errors.Is(err, ErrKindNotRunnable) {
			t.Errorf("unknown kind = %v, want ErrKindNotRunnable", err)
		}
		if rows := jobRows(t, db); rows != 0 {
			t.Fatalf("rows = %d, want 0", rows)
		}
	})

	t.Run("a caller without a user is refused", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		_, handlers := runNowKinds(1)
		rt := admitRuntime(t, db, clock, handlers...)
		if _, err := rt.Accept(t.Context(), Caller{}, "run.k0"); !errors.Is(err, ErrConfig) {
			t.Fatalf("caller without a user = %v, want ErrConfig", err)
		}
		if rows := jobRows(t, db); rows != 0 {
			t.Fatalf("rows = %d, want 0", rows)
		}
	})

	t.Run("the admitted row carries its lane, creator and expiry", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		_, handlers := runNowKinds(1)
		rt := admitRuntime(t, db, clock, handlers...)
		job, err := rt.Accept(t.Context(), Caller{UserID: 7}, "run.k0")
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.ReadJob(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Lane != store.JobInteractive || row.Class != store.JobOptional || row.State != store.JobQueued {
			t.Errorf("admitted row lane=%s class=%s state=%s, want interactive optional queued", row.Lane, row.Class, row.State)
		}
		if row.CreatedBy == nil || *row.CreatedBy != 7 {
			t.Errorf("admitted row creator = %v, want 7: the per-caller count reads it", row.CreatedBy)
		}
		if row.ExpiresAt == nil || *row.ExpiresAt != row.DueAt+int64(optionalExpiry/time.Second) {
			t.Errorf("admitted row expiry = %v, want its due instant plus %s", row.ExpiresAt, optionalExpiry)
		}
	})

	t.Run("an admitted run-now is claimed after its commit", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		ran := make(chan struct{}, 1)
		rt := newRuntime(t, db, clock, "boot-wake", runNowHandler(KindJobsPrune, ran))
		// Nothing is due and the clock never moves, so only the wake a committed
		// admission sends can make this row claimable before the next sweep.
		job, err := rt.Accept(t.Context(), Caller{UserID: 1}, KindJobsPrune)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatal("admitted work was never claimed: no wake followed the commit")
		}
		waitFor(t, "the admitted row to finish", func() bool {
			row, err := db.ReadJob(t.Context(), job.ID)
			return err == nil && row.State == store.JobSucceeded
		})
	})

	t.Run("an interactive run-now is claimed while every background slot is taken", func(t *testing.T) {
		db := openTestStore(t)
		callerRows(t, db, 8)
		clock := newFakeClock()
		holding, gate := make(chan struct{}, 1), make(chan struct{})
		background := handler("background.work", func(context.Context, *Attempt) Outcome {
			holding <- struct{}{}
			<-gate
			return Outcome{Result: Succeeded, Reason: "done"}
		})
		enqueue(t, db, dueIntent(clock, "background.work", "background-1"))
		ran := make(chan struct{}, 1)
		rt := newRuntime(t, db, clock, "boot-lanes", background, runNowHandler(KindJobsPrune, ran))
		// The runtime's one background slot is held by an attempt that has not
		// returned, so any work that waited for it would never run.
		select {
		case <-holding:
		case <-time.After(5 * time.Second):
			t.Fatal("the background attempt never started")
		}
		if _, err := rt.Accept(t.Context(), Caller{UserID: 1}, KindJobsPrune); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatal("the interactive run waited for a background slot")
		}
		close(gate)
	})
}

// TestJobList proves the jobs read model: newest first, one page bound that the
// caller continues from, an optional state filter and the identity the page was
// read under.
func TestJobList(t *testing.T) {
	const rows = 55
	seed := func(t *testing.T) (*store.DB, *Runtime) {
		t.Helper()
		db := openTestStore(t)
		clock := newFakeClock()
		rt := admitRuntime(t, db, clock)
		err := db.Write(t.Context(), func(tx *sql.Tx) error {
			for i := range rows {
				key := fmt.Sprintf("list-%d", i)
				if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
					Kind:           "list.work",
					Class:          store.JobOptional,
					Lane:           store.JobBackground,
					DedupeKey:      key,
					ConcurrencyKey: key,
					MaxAttempts:    1,
					DueAt:          clock.Now().Unix(),
				}); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(t.Context(), `UPDATE job SET state = 'succeeded', reason_code = 'done' WHERE id = 1`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return db, rt
	}

	t.Run("pages newest first and continues from its cursor", func(t *testing.T) {
		_, rt := seed(t)
		first, err := rt.List(t.Context(), Query{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Items) != 50 || !first.More {
			t.Fatalf("first page items=%d more=%v, want 50 and another page", len(first.Items), first.More)
		}
		if first.Items[0].ID != rows {
			t.Errorf("newest row = %d, want %d", first.Items[0].ID, rows)
		}
		for i := 1; i < len(first.Items); i++ {
			if first.Items[i-1].ID <= first.Items[i].ID {
				t.Fatalf("page %d: ids %d then %d are not newest first", i, first.Items[i-1].ID, first.Items[i].ID)
			}
		}
		second, err := rt.List(t.Context(), Query{Limit: 50, AfterID: first.Items[len(first.Items)-1].ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Items) != rows-50 || second.More {
			t.Fatalf("second page items=%d more=%v, want the remaining %d", len(second.Items), second.More, rows-50)
		}
		seen := map[int64]bool{}
		for _, job := range append(first.Items, second.Items...) {
			if seen[job.ID] {
				t.Fatalf("job %d was served twice", job.ID)
			}
			seen[job.ID] = true
		}
		if len(seen) != rows {
			t.Fatalf("pages served %d rows, want %d", len(seen), rows)
		}
	})

	t.Run("shows the states and the reason of each row", func(t *testing.T) {
		_, rt := seed(t)
		page, err := rt.List(t.Context(), Query{Limit: 50, State: store.JobSucceeded})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.More {
			t.Fatalf("succeeded page items=%d more=%v, want the one finished row", len(page.Items), page.More)
		}
		if job := page.Items[0]; job.ID != 1 || job.State != store.JobSucceeded || job.ReasonCode != "done" {
			t.Errorf("succeeded row = %+v", job)
		}
		if page, err = rt.List(t.Context(), Query{Limit: 500, State: store.JobQueued}); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != rows-1 {
			t.Fatalf("queued page items=%d, want %d", len(page.Items), rows-1)
		}
	})

	t.Run("carries the identity its cursor is bound to", func(t *testing.T) {
		db, rt := seed(t)
		var installID string
		var epoch int64
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT install_id, restore_epoch FROM installation WHERE id = 1`).Scan(&installID, &epoch)
		}); err != nil {
			t.Fatal(err)
		}
		page, err := rt.List(t.Context(), Query{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if page.InstallID != installID || page.RestoreEpoch != epoch {
			t.Fatalf("page identity = %q/%d, want %q/%d", page.InstallID, page.RestoreEpoch, installID, epoch)
		}
	})
}
