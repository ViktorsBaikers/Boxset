package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Test fixtures: one store on a temp file, one fake clock the test advances,
// and a runtime of one background slot so a claim, a park and a Slot are all
// observable one at a time.

// fakeClock is the runtime's clock: Now only moves when the test advances it, and
// After fires one buffered tick per waiter.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0).UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, ch)
	return ch
}

// waiting is how many timers are armed on the clock: one per parked lane and one
// for the sweep. A test reads it to know the waits exist that a missed wake never
// reaches.
func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// advance moves the clock and fires every waiter that was registered before it.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	waiters := c.waiters
	c.waiters = nil
	now := c.now
	c.mu.Unlock()
	for _, ch := range waiters {
		ch <- now
	}
}

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// enqueue writes one intent through the store, as a domain write would.
func enqueue(t *testing.T, db *store.DB, in store.JobIntent) int64 {
	t.Helper()
	var id int64
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		id, _, err = store.EnqueueJob(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", in.Kind, in.DedupeKey, err)
	}
	return id
}

// dueIntent is one background intent that is due now.
func dueIntent(clock *fakeClock, kind, key string) store.JobIntent {
	return store.JobIntent{
		Kind:           kind,
		Class:          store.JobRequired,
		Lane:           store.JobBackground,
		DedupeKey:      key,
		ConcurrencyKey: key,
		MaxAttempts:    3,
		DueAt:          clock.Now().Unix(),
	}
}

func jobOf(t *testing.T, db *store.DB, id int64) store.Job {
	t.Helper()
	job, err := db.ReadJob(t.Context(), id)
	if err != nil {
		t.Fatalf("ReadJob(%d): %v", id, err)
	}
	return job
}

func jobRows(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM job`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// TestClaimFrameCarriesTheRowsOutcome compares the running frame a claim
// publishes to the row the read model returns. A requeued row keeps the reason
// and params its last transition recorded, and the claim does not clear them, so
// a frame that published empty ones would disagree with the store.
func TestClaimFrameCarriesTheRowsOutcome(t *testing.T) {
	db := openTestStore(t)
	clock := newFakeClock()
	now := clock.Now().Unix()
	id := enqueue(t, db, dueIntent(clock, "test.claim", "key"))
	if _, ok, err := db.ClaimJob(t.Context(), store.JobBackground, "boot", now); err != nil || !ok {
		t.Fatalf("first ClaimJob = %v, %v", ok, err)
	}
	if ok, err := db.RequeueJob(t.Context(), id, 1, now, "attempt_overdue", map[string]string{"deadline": "1"}, now); err != nil || !ok {
		t.Fatalf("RequeueJob = %v, %v", ok, err)
	}
	claimed, ok, err := db.ClaimJob(t.Context(), store.JobBackground, "boot", now)
	if err != nil || !ok {
		t.Fatalf("second ClaimJob = %v, %v", ok, err)
	}

	var frames []Event
	rt, err := New(db, Config{
		Now: clock.Now, After: clock.After, Boot: "boot", BackgroundSlots: 1,
		Log:     func(string, ...any) {},
		Publish: func(ev Event) { frames = append(frames, ev) },
	}, []Handler{handler("test.claim", func(context.Context, *Attempt) Outcome {
		return Outcome{Result: Succeeded}
	})})
	if err != nil {
		t.Fatal(err)
	}
	rt.publishClaim(claimed)

	row := jobOf(t, db, id)
	if row.ReasonCode != "attempt_overdue" {
		t.Fatalf("the claimed row's reason = %q, want the recorded attempt_overdue", row.ReasonCode)
	}
	if len(frames) != 1 {
		t.Fatalf("claim frames = %d, want exactly one", len(frames))
	}
	frame := frames[0]
	if frame.Revision != row.Revision {
		t.Fatalf("frame revision = %d, want the read model's %d", frame.Revision, row.Revision)
	}
	if frame.Reason != row.ReasonCode || len(frame.Params) != len(row.ReasonParams) {
		t.Fatalf("frame reason/params = %q/%v, want the read model's %q/%v", frame.Reason, frame.Params, row.ReasonCode, row.ReasonParams)
	}
	for key, want := range row.ReasonParams {
		if got := frame.Params[key]; got != want {
			t.Fatalf("frame param %q = %q, want the read model's %q", key, got, want)
		}
	}
}

// generationStates is every alias generation row, for a before/after comparison.
func generationStates(t *testing.T, db *store.DB) []string {
	t.Helper()
	var out []string
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT id, dataset, state FROM alias_generation ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var dataset, state string
			if err := rows.Scan(&id, &dataset, &state); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%d/%s/%s", id, dataset, state))
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("generation states: %v", err)
	}
	return out
}

// newRuntime builds the runtime of one test: one background slot, a fake clock
// and a log that records nothing unless the test asks for it.
func newRuntime(t *testing.T, db *store.DB, clock *fakeClock, boot string, handlers ...Handler) *Runtime {
	t.Helper()
	return newRuntimeWithLog(t, db, clock, boot, func(string, ...any) {}, handlers...)
}

// newRuntimeWithLog is newRuntime with a runtime that records its failures.
func newRuntimeWithLog(t *testing.T, db *store.DB, clock *fakeClock, boot string, log func(reason string, kv ...any), handlers ...Handler) *Runtime {
	t.Helper()
	rt, err := New(db, Config{
		Now:             clock.Now,
		After:           clock.After,
		Boot:            boot,
		BackgroundSlots: 1,
		Log:             log,
	}, handlers)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.Stop(ctx); err != nil && !errors.Is(err, ErrAttemptsLive) {
			t.Errorf("Stop: %v", err)
		}
	})
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return rt
}

// advanceUntil advances the clock until cond holds. A runtime re-arms its timers
// after each one fires, so a single advance can land before the next timer exists;
// advancing again is what reaches it.
func advanceUntil(t *testing.T, clock *fakeClock, step time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		clock.advance(step)
		time.Sleep(time.Millisecond)
	}
}

// stir wakes the runtimes and advances the clock until cond holds. A wake that
// arrives before a lane re-armed its wait, or a clock advance that landed before
// a timer existed, would otherwise leave a proof waiting forever; repeating both
// is what reaches the work without weakening the assertion.
func stir(t *testing.T, clock *fakeClock, what string, cond func() bool, runtimes ...*Runtime) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		for _, rt := range runtimes {
			if rt != nil {
				rt.Wake()
			}
		}
		clock.advance(sweepEvery)
		time.Sleep(time.Millisecond)
	}
}

// waitFor polls cond until it holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// handler builds a handler with the recorded bounds of the tests.
func handler(kind string, run func(context.Context, *Attempt) Outcome) Handler {
	return Handler{
		Kind:        kind,
		Class:       ClassRequired,
		Lane:        LaneBackground,
		MaxAttempts: 3,
		Deadline:    30 * time.Second,
		Run:         run,
	}
}

// TestDurableIntent proves AC-018: a required intent commits with its domain
// transition before any effect, atomic claims and keys allow exactly one
// mutation-capable attempt, and a wake that never arrived costs at most one
// sweep period rather than losing the work.
func TestDurableIntent(t *testing.T) {
	t.Run("the intent commits with its domain change", func(t *testing.T) {
		db := openTestStore(t)
		lib := library.New(db)
		ctx := t.Context()
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO title (id, metadata_id, kind, name) VALUES (1, 'tmdb:603', 'movie', 'Synthetic Movie')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		first, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &first, []string{"Alpha Alias"}); err != nil {
			t.Fatal(err)
		}
		// No active generation is replaced, so no purge is needed.
		if err := lib.ActivateGeneration(ctx, first); err != nil {
			t.Fatalf("first activation: %v", err)
		}
		if got := jobRows(t, db); got != 0 {
			t.Fatalf("the first activation wrote %d jobs, want none: it retires nothing", got)
		}
		second, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &second, []string{"Bravo Alias"}); err != nil {
			t.Fatal(err)
		}
		if err := lib.ActivateGeneration(ctx, second); err != nil {
			t.Fatalf("second activation: %v", err)
		}
		if got := jobRows(t, db); got != 1 {
			t.Fatalf("jobs after the activation = %d, want the one required purge", got)
		}
		var id int64
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT id FROM job`).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		job := jobOf(t, db, id)
		generation := fmt.Sprint(int64(first))
		if job.Kind != KindAliasPurge || job.Class != store.JobRequired || job.Lane != store.JobBackground ||
			job.State != store.JobQueued || job.Params["generation"] != generation ||
			job.DedupeKey != "alias-generation:"+generation || job.ConcurrencyKey != job.DedupeKey ||
			job.MaxAttempts != 5 || job.Attempts != 0 {
			t.Fatalf("the purge intent is %+v, want the retired generation %s as required background work", job, generation)
		}
		// The domain change is visible in the same commit: only the new aliases search.
		ids, err := lib.Search(ctx, "Bravo", 10)
		if err != nil || len(ids) != 1 || ids[0] != 1 {
			t.Fatalf("search for the new alias = %v (err %v), want the title", ids, err)
		}
		if ids, err := lib.Search(ctx, "Alpha", 10); err != nil || len(ids) != 0 {
			t.Fatalf("search for the retired alias = %v (err %v), want none", ids, err)
		}
		// A refused activation changes neither the domain nor the intent rows.
		if err := lib.ActivateGeneration(ctx, first); !errors.Is(err, library.ErrInvalidInput) {
			t.Fatalf("activating a retired generation = %v, want ErrInvalidInput", err)
		}
		if got := jobRows(t, db); got != 1 {
			t.Fatalf("jobs after the refused activation = %d, want the one purge", got)
		}
	})

	t.Run("a failed second statement leaves neither", func(t *testing.T) {
		db := openTestStore(t)
		lib := library.New(db)
		ctx := t.Context()
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO title (id, metadata_id, kind, name) VALUES (1, 'tmdb:603', 'movie', 'Synthetic Movie')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		active, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &active, []string{"Alpha Alias"}); err != nil {
			t.Fatal(err)
		}
		if err := lib.ActivateGeneration(ctx, active); err != nil {
			t.Fatalf("first activation: %v", err)
		}
		staging, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &staging, []string{"Bravo Alias"}); err != nil {
			t.Fatal(err)
		}
		// The intent insert is the transaction's last statement; a trigger that
		// refuses it makes that statement fail after the retire and the activation
		// ran, while the whole transaction is still open.
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TRIGGER intent_probe BEFORE INSERT ON job BEGIN SELECT RAISE(ABORT, 'intent_probe'); END`)
			return err
		}); err != nil {
			t.Fatalf("intent probe: %v", err)
		}
		beforeGenerations := generationStates(t, db)
		beforeJobs := jobRows(t, db)

		err = lib.ActivateGeneration(ctx, staging)
		if err == nil || !strings.Contains(err.Error(), "intent_probe") {
			t.Fatalf("the activation with a refused intent insert = %v, want the probe's abort", err)
		}
		if after := generationStates(t, db); !slices.Equal(beforeGenerations, after) {
			t.Fatalf("the refused activation moved generation state:\n before %v\n after  %v", beforeGenerations, after)
		}
		if after := jobRows(t, db); after != beforeJobs {
			t.Fatalf("the refused activation wrote %d job rows, want the %d it started with", after, beforeJobs)
		}
	})

	t.Run("an uncommitted intent is invisible and starts no effect", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Succeeded}
		}))
		ctx := t.Context()
		var visibleDuringWrite int64
		err := db.Write(ctx, func(tx *sql.Tx) error {
			if _, _, err := store.EnqueueJob(ctx, tx, dueIntent(clock, "test.work", "k")); err != nil {
				return err
			}
			// A separate reader sees the committed state only: nothing yet.
			return db.Read(ctx, func(rtx *sql.Tx) error {
				return rtx.QueryRowContext(ctx, `SELECT count(*) FROM job`).Scan(&visibleDuringWrite)
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if visibleDuringWrite != 0 {
			t.Fatalf("an uncommitted intent was visible: %d rows", visibleDuringWrite)
		}
		// Woken after the commit, which is what the writer does.
		stir(t, clock, "the committed intent to run", func() bool { return runs.Load() == 1 }, rt)
	})

	t.Run("32 claimers and two runtimes leave one owner", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		enqueue(t, db, dueIntent(clock, "test.work", "key"))
		now := clock.Now().Unix()
		var wins atomic.Int64
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok, err := db.ClaimJob(ctx, store.JobBackground, "boot-a", now); err == nil && ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("32 claimers won %d times, want exactly one", got)
		}
		// The key is owned, so a later claim of any boot finds nothing claimable.
		if _, ok, err := db.ClaimJob(ctx, store.JobBackground, "boot-b", now); err != nil || ok {
			t.Fatalf("a claim behind the owned key = ok %v err %v, want no work", ok, err)
		}

		// Two live runtimes, one due row: one mutation-capable attempt.
		db2 := openTestStore(t)
		clock2 := newFakeClock()
		var runs, active, peak atomic.Int64
		var release = make(chan struct{})
		work := handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			<-release
			active.Add(-1)
			return Outcome{Result: Succeeded}
		})
		firstRuntime := newRuntime(t, db2, clock2, "boot-a", work)
		secondRuntime := newRuntime(t, db2, clock2, "boot-b", work)
		enqueue(t, db2, dueIntent(clock2, "test.work", "key"))
		stir(t, clock2, "the one attempt to start", func() bool { return runs.Load() == 1 }, firstRuntime, secondRuntime)
		time.Sleep(20 * time.Millisecond)
		close(release)
		waitFor(t, "the attempt to finish", func() bool {
			job, err := db2.ReadJob(t.Context(), 1)
			return err == nil && job.State == store.JobSucceeded
		})
		if got := runs.Load(); got != 1 || peak.Load() != 1 {
			t.Fatalf("attempts = %d (peak %d concurrent), want exactly one mutation-capable attempt", got, peak.Load())
		}
	})

	t.Run("a sweep acts while every lane is parked", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		parked, resume := make(chan struct{}, 1), make(chan struct{})
		work := handler("test.work", func(ctx context.Context, a *Attempt) Outcome {
			if err := a.Wait(ctx, func(context.Context) error {
				parked <- struct{}{}
				<-resume
				return nil
			}); err != nil {
				t.Errorf("Wait: %v", err)
			}
			return Outcome{Result: Succeeded, Reason: "done"}
		})
		// One background slot, and the only lane sits inside its handler, so no lane
		// can perform the transition below: only the sweep can.
		rt := newRuntime(t, db, clock, "boot", work)
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		stir(t, clock, "the attempt to park", func() bool {
			row, err := db.ReadJob(ctx, id)
			return err == nil && row.State == store.JobRunning
		}, rt)
		<-parked
		// A row that needs the key the parked attempt owns: it stays queued.
		queuedIntent := dueIntent(clock, "test.work", "queued-dedupe")
		queuedIntent.ConcurrencyKey = "key"
		queuedID := enqueue(t, db, queuedIntent)
		rt.Wake()
		if row := jobOf(t, db, queuedID); row.State != store.JobQueued {
			t.Fatalf("the row written while the lane was parked = %s, want it queued", row.State)
		}
		// Past the attempt's deadline and the grace: the sweep records the overdue
		// attempt, which no lane can do because the lane is inside that attempt.
		advanceUntil(t, clock, work.Deadline+overdueGrace+time.Second, "the sweep's attempt_overdue record", func() bool {
			row, err := db.ReadJob(ctx, id)
			return err == nil && row.ReasonCode == "attempt_overdue"
		})
		if row := jobOf(t, db, id); row.State != store.JobRunning {
			t.Fatalf("the overdue row = %s, want it running with its key held", row.State)
		}
		// The key is still owned, so the queued row cannot start yet.
		if _, ok, err := db.ClaimJob(ctx, store.JobBackground, "probe", clock.Now().Unix()); err != nil || ok {
			t.Fatalf("a claim behind the overdue attempt = ok %v err %v, want no work", ok, err)
		}
		// Once the attempt returns, its key goes with it and the queued row runs.
		close(resume)
		waitFor(t, "the parked attempt and then the queued row", func() bool {
			first, err := db.ReadJob(ctx, id)
			if err != nil || first.State != store.JobSucceeded {
				return false
			}
			second, err := db.ReadJob(ctx, queuedID)
			return err == nil && second.State == store.JobSucceeded
		})
	})

	t.Run("a due row whose wake never arrived still runs", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Succeeded, Reason: "done"}
		}))
		// The wake this row never gets is one that would have reached a waiting
		// lane, so every lane and the sweep are parked before the row is written.
		waitFor(t, "every lane and the sweep to park", func() bool {
			return clock.waiting() == interactiveSlots+2
		})
		enqueue(t, db, dueIntent(clock, "test.work", "key"))

		// One sweep period, and nothing calls Wake: the row's own wait is what
		// finds it.
		clock.advance(sweepEvery)

		waitFor(t, "the missed wake's due row", func() bool { return runs.Load() == 1 })
	})

	t.Run("no work is created by a committed transition", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var events atomic.Int64
		rt, err := New(db, Config{Now: clock.Now, After: clock.After, Boot: "boot", BackgroundSlots: 1, Log: func(string, ...any) {}, Publish: func(Event) { events.Add(1) }}, []Handler{
			handler("test.work", func(context.Context, *Attempt) Outcome { return Outcome{Result: Succeeded, Reason: "done"} }),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rt.Stop(context.Background()) })
		if err := rt.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		enqueue(t, db, dueIntent(clock, "test.work", "alone"))
		// The claim and the finish of one attempt are the two frames it owes; both
		// are published before this row's own question is asked.
		stir(t, clock, "the events of the committed transitions", func() bool { return events.Load() >= 2 }, rt)
		time.Sleep(20 * time.Millisecond)
		if got := events.Load(); got != 2 {
			t.Fatalf("events after one transition = %d, want the claim and the finish", got)
		}
		if got := jobRows(t, db); got != 1 {
			t.Fatalf("rows after the transitions = %d, want only the intent that was written", got)
		}
	})
}

// TestJobRecovery proves AC-019: an attempt that was cancelled, hung, killed or
// drained keeps its row and its key, reconciliation runs before anything is
// claimed again, and exhaustion leaves required work visible with its key held.
func TestJobRecovery(t *testing.T) {
	t.Run("a foreign running row reconciles before any claim", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		// An earlier process claimed the row and died: its lease is foreign.
		if _, ok, err := db.ClaimJob(ctx, store.JobBackground, "earlier-boot", clock.Now().Unix()); err != nil || !ok {
			t.Fatalf("seed claim: ok %v err %v", ok, err)
		}
		var runs atomic.Int64
		var sawReason atomic.Value
		newRuntime(t, db, clock, "boot", handler("test.work", func(ctx context.Context, a *Attempt) Outcome {
			runs.Add(1)
			row, err := db.ReadJob(ctx, a.JobID)
			if err != nil {
				t.Errorf("ReadJob in the handler: %v", err)
			}
			sawReason.Store(row.ReasonCode)
			return Outcome{Result: Succeeded, Reason: "resumed"}
		}))
		waitFor(t, "the reconciliation of the interrupted row", func() bool {
			job, err := db.ReadJob(ctx, id)
			return err == nil && job.State == store.JobSucceeded
		})
		job := jobOf(t, db, id)
		if got := runs.Load(); got != 1 {
			t.Fatalf("attempts = %d, want the one reconciliation", got)
		}
		if reason, _ := sawReason.Load().(string); reason != "attempt_interrupted" {
			t.Fatalf("the claimed row's reason = %q, want the recorded attempt_interrupted", reason)
		}
		if job.Attempts != 2 || job.ReasonCode != "resumed" {
			t.Fatalf("the resumed row = %+v, want attempts 2 and the handler's reason", job)
		}
	})

	t.Run("a stop with a live attempt keeps the row and its key", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		blocked, finished := make(chan struct{}), make(chan struct{})
		var runs atomic.Int64
		newWork := func(boot string) *Runtime {
			rt, err := New(db, Config{Now: clock.Now, After: clock.After, Boot: boot, BackgroundSlots: 1, Log: func(string, ...any) {}}, []Handler{
				handler("test.work", func(context.Context, *Attempt) Outcome {
					if runs.Add(1) == 1 {
						<-blocked
						close(finished)
					}
					return Outcome{Result: Succeeded}
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			return rt
		}
		rt := newWork("boot")
		if err := rt.Start(ctx); err != nil {
			t.Fatal(err)
		}
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		stir(t, clock, "the attempt to start", func() bool { return runs.Load() == 1 }, rt)

		stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := rt.Stop(stopCtx)
		cancel()
		if !errors.Is(err, ErrAttemptsLive) {
			t.Fatalf("Stop with a live attempt = %v, want ErrAttemptsLive", err)
		}
		job := jobOf(t, db, id)
		if job.State != store.JobRunning || job.LeaseOwner != "boot" {
			t.Fatalf("the row after the stop = %+v, want it running under this boot", job)
		}
		// The key is still owned: an intent with the same key is not claimable.
		other := dueIntent(clock, "test.work", "other")
		other.ConcurrencyKey = "key"
		otherID := enqueue(t, db, other)
		if _, ok, err := db.ClaimJob(ctx, store.JobBackground, "boot2", clock.Now().Unix()); err != nil || ok {
			t.Fatalf("a claim behind the live attempt = ok %v err %v, want no work", ok, err)
		}
		close(blocked)
		<-finished
		time.Sleep(20 * time.Millisecond)

		// The next start reconciles the row before it claims anything: the intent
		// is retained and runs once more, then its key is released.
		resumed := newWork("boot2")
		t.Cleanup(func() {
			stop, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := resumed.Stop(stop); err != nil && !errors.Is(err, ErrAttemptsLive) {
				t.Errorf("resumed Stop: %v", err)
			}
		})
		if err := resumed.Start(ctx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the retained intent to finish on the next start", func() bool {
			row, err := db.ReadJob(ctx, id)
			return err == nil && row.State == store.JobSucceeded
		})
		waitFor(t, "the key the live attempt held to be released", func() bool {
			row, err := db.ReadJob(ctx, otherID)
			return err == nil && row.State == store.JobSucceeded
		})
	})

	t.Run("exhaustion leaves required work in review with its key held", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Retry, Reason: "flaky"}
		}))
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		for i := int64(1); i <= 3; i++ {
			stir(t, clock, fmt.Sprintf("attempt %d", i), func() bool { return runs.Load() == i }, rt)
			if i < 3 {
				// The retry is scheduled in the future: the row waits for its due time.
				waitFor(t, fmt.Sprintf("attempt %d to be rescheduled", i), func() bool {
					row, err := db.ReadJob(t.Context(), id)
					return err == nil && row.State == store.JobQueued
				})
			}
		}
		waitFor(t, "the exhausted required row", func() bool {
			row, err := db.ReadJob(t.Context(), id)
			return err == nil && row.State == store.JobReview
		})
		job := jobOf(t, db, id)
		if job.State != store.JobReview || job.ReasonCode != "attempts_exhausted" || job.Attempts != 3 {
			t.Fatalf("the exhausted required row = %+v, want review with attempts_exhausted after 3 attempts", job)
		}
		// Required work keeps its key: another intent with that key never runs.
		other := dueIntent(clock, "test.work", "other")
		other.ConcurrencyKey = "key"
		enqueue(t, db, other)
		if _, ok, err := db.ClaimJob(t.Context(), store.JobBackground, "probe", clock.Now().Unix()); err != nil || ok {
			t.Fatalf("a claim behind the review row = ok %v err %v, want no work", ok, err)
		}
	})

	t.Run("optional work fails and expires without losing its row", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Retry, Reason: "flaky"}
		}))
		intent := dueIntent(clock, "test.work", "optional")
		intent.Class = store.JobOptional
		id := enqueue(t, db, intent)
		for i := int64(1); i <= 3; i++ {
			stir(t, clock, fmt.Sprintf("attempt %d", i), func() bool { return runs.Load() == i }, rt)
		}
		waitFor(t, "the exhausted optional row", func() bool {
			row, err := db.ReadJob(t.Context(), id)
			return err == nil && row.State == store.JobFailed
		})
		job := jobOf(t, db, id)
		if job.State != store.JobFailed || job.ReasonCode != "attempts_exhausted" {
			t.Fatalf("the exhausted optional row = %+v, want failed with attempts_exhausted", job)
		}
	})

	t.Run("an optional row past its expiry ends expired", func(t *testing.T) {
		// The sweep's own call, on a store no lane is claiming from, so the expiry
		// is the only thing that can move the row.
		db := openTestStore(t)
		clock := newFakeClock()
		expiring := dueIntent(clock, "test.work", "expiring")
		expiring.Class = store.JobOptional
		expires := clock.Now().Unix() + 60
		expiring.ExpiresAt = &expires
		id := enqueue(t, db, expiring)
		now := clock.Now().Add(time.Minute).Unix()
		moved, err := db.ExpireOptionalJobs(t.Context(), now)
		if err != nil || moved != 1 {
			t.Fatalf("ExpireOptionalJobs = %d (err %v), want the one optional row", moved, err)
		}
		row := jobOf(t, db, id)
		if row.State != store.JobExpired || row.ReasonCode != "expired" {
			t.Fatalf("the expired row = %+v, want expired with its reason", row)
		}
	})

	t.Run("an idle sweep commits nothing", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Succeeded, Reason: "done"}
		}))
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		stir(t, clock, "the one attempt", func() bool { return runs.Load() == 1 }, rt)
		waitFor(t, "the row to succeed", func() bool {
			row, err := db.ReadJob(t.Context(), id)
			return err == nil && row.State == store.JobSucceeded
		})
		before := jobOf(t, db, id)
		for range 3 {
			clock.advance(sweepEvery)
			time.Sleep(5 * time.Millisecond)
		}
		if after := jobOf(t, db, id); after.Revision != before.Revision || after.UpdatedAt != before.UpdatedAt {
			t.Fatalf("an idle sweep rewrote the terminal row: %+v then %+v", before, after)
		}
		if pending, err := db.SweepPending(t.Context(), clock.Now().Unix()); err != nil || pending {
			t.Fatalf("SweepPending on an idle store = %v (err %v), want nothing to do", pending, err)
		}
	})

	t.Run("Wait parks without its slot and Slot draws from the same ceiling", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		parked, resume, waitErr := make(chan struct{}, 1), make(chan struct{}), make(chan error, 1)
		rt, err := New(db, Config{Now: clock.Now, After: clock.After, Boot: "boot", BackgroundSlots: 1, Log: func(string, ...any) {}}, []Handler{
			handler("test.work", func(ctx context.Context, a *Attempt) Outcome {
				waitErr <- a.Wait(ctx, func(context.Context) error {
					parked <- struct{}{}
					<-resume
					return nil
				})
				return Outcome{Result: Succeeded, Reason: "parked"}
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := rt.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := enqueue(t, db, dueIntent(clock, "test.work", "first"))
		stir(t, clock, "the attempt to park", func() bool {
			row, err := db.ReadJob(t.Context(), first)
			return err == nil && row.State == store.JobRunning
		}, rt)
		<-parked
		// The attempt gave its slot back for the park call, so a Slot call gets it.
		slotCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		release, err := rt.Slot(slotCtx)
		cancel()
		if err != nil {
			t.Fatalf("Slot while the attempt parks: %v", err)
		}
		// While that slot is held, a new claim cannot take it.
		second := enqueue(t, db, dueIntent(clock, "test.work", "second"))
		clock.advance(sweepEvery)
		time.Sleep(20 * time.Millisecond)
		if job := jobOf(t, db, second); job.State != store.JobQueued {
			t.Fatalf("the second row = %s while a Slot held the only slot, want it queued", job.State)
		}
		release()
		close(resume)
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait = %v, want the park call's nil error", err)
		}
		waitFor(t, "the parked attempt to finish", func() bool {
			job, err := db.ReadJob(t.Context(), first)
			return err == nil && job.State == store.JobSucceeded
		})
		waitFor(t, "the second row to be claimed", func() bool {
			job, err := db.ReadJob(t.Context(), second)
			return err == nil && job.State != store.JobQueued
		})

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := rt.Stop(stopCtx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if _, err := rt.Slot(t.Context()); !errors.Is(err, ErrStopped) {
			t.Fatalf("Slot after Stop = %v, want ErrStopped", err)
		}
	})

	t.Run("a freed slot serves a waiting Slot before a new claim", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Succeeded, Reason: "done"}
		}))
		// The one background slot is held outside the lanes.
		first, err := rt.Slot(ctx)
		if err != nil {
			t.Fatalf("first Slot: %v", err)
		}
		// A second Slot call waits for a slot, which is what a claim must yield to.
		waiting := make(chan func(), 1)
		go func() {
			slotCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release, err := rt.Slot(slotCtx)
			if err != nil {
				t.Errorf("waiting Slot: %v", err)
				waiting <- nil
				return
			}
			waiting <- release
		}()
		// The waiter counts itself before it blocks; a lane must then refuse a claim.
		waiters := func() int {
			pool := rt.pools[LaneBackground]
			pool.mu.Lock()
			defer pool.mu.Unlock()
			return pool.waits
		}
		waitFor(t, "the second Slot to wait", func() bool { return waiters() == 1 })
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		rt.Wake()
		clock.advance(sweepEvery)
		// Releasing the held slot hands it to the waiter, not to a new claim.
		first()
		release := <-waiting
		if release == nil {
			t.Fatal("the waiting Slot never got a slot")
		}
		if row := jobOf(t, db, id); row.State != store.JobQueued {
			t.Fatalf("the row = %s while a waiting Slot held the slot, want it queued", row.State)
		}
		release()
		stir(t, clock, "the deferred row to run", func() bool { return runs.Load() == 1 }, rt)
	})

	t.Run("a claim yields to a taker that is already waiting", func(t *testing.T) {
		// The rule is the pool's own: while a taker is counted, a free slot is not
		// claimable. The window it closes is the one between a release and that
		// taker's take, which no black-box sequence can hold open deterministically,
		// so the predicate is pinned here directly.
		pool := newPool(1)
		pool.mu.Lock()
		pool.waits = 1
		pool.mu.Unlock()
		if _, ok := pool.claim(); ok {
			t.Fatal("a claim took a free slot while a taker was waiting for one")
		}
		pool.mu.Lock()
		pool.waits = 0
		pool.mu.Unlock()
		release, ok := pool.claim()
		if !ok {
			t.Fatal("the free slot was not claimable once nothing waited for it")
		}
		release()
	})

	t.Run("an outcome a handler never filled in fails closed", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		rt := newRuntime(t, db, clock, "boot", handler("test.work", func(context.Context, *Attempt) Outcome {
			return Outcome{}
		}))
		id := enqueue(t, db, dueIntent(clock, "test.work", "key"))
		stir(t, clock, "the unset outcome to fail closed", func() bool {
			row, err := db.ReadJob(t.Context(), id)
			return err == nil && row.ReasonCode == "outcome_invalid"
		}, rt)
		row := jobOf(t, db, id)
		if row.State != store.JobReview {
			t.Fatalf("the unset outcome left the row %s, want review: an empty Outcome is never success", row.State)
		}
	})

	t.Run("a refused transition and a refused claim are recorded", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		ctx := t.Context()
		recorder := &reasonLog{}
		rt := newRuntimeWithLog(t, db, clock, "boot", recorder.record,
			// A reason word the schema cannot hold: the terminal write is refused.
			handler("test.work", func(context.Context, *Attempt) Outcome {
				return Outcome{Result: Succeeded, Reason: strings.Repeat("x", 65)}
			}),
			handler("test.good", func(context.Context, *Attempt) Outcome {
				return Outcome{Result: Succeeded, Reason: "done"}
			}),
		)
		bad := enqueue(t, db, dueIntent(clock, "test.work", "bad-reason"))
		stir(t, clock, "the refused transition record", func() bool { return recorder.has("transition_failed") }, rt)
		if row := jobOf(t, db, bad); row.State != store.JobRunning {
			t.Fatalf("the row whose write was refused = %s, want it left running", row.State)
		}
		// The lanes keep working: the next row of another kind still completes.
		good := enqueue(t, db, dueIntent(clock, "test.good", "good"))
		stir(t, clock, "the next row to succeed", func() bool {
			row, err := db.ReadJob(ctx, good)
			return err == nil && row.State == store.JobSucceeded
		}, rt)
		// A store that is gone is recorded too, instead of looking like an idle lane.
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		stir(t, clock, "the claim and sweep failures", func() bool {
			return recorder.has("claim_failed") && recorder.has("sweep_failed")
		}, rt)
	})
}

// reasonLog records the reason words a runtime reports.
type reasonLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *reasonLog) record(reason string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reasons = append(l.reasons, reason)
}

func (l *reasonLog) has(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.reasons, reason)
}
