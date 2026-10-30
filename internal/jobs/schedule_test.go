package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// The enumerated oracle every expectation below is read from: the instants in
// testdata/occurrences.json come from the IANA zone rules, never from Next, so a
// wrong resolver cannot bake its own answer in. Each instant carries the local
// reading it must have, which on a date whose named wall clock the zone skipped is
// the transition instead.

type occurrenceOracle struct {
	Daily []struct {
		Name          string   `json:"name"`
		Zone          string   `json:"zone"`
		LocalTime     string   `json:"local_time"`
		After         string   `json:"after"`
		Occurrences   []string `json:"occurrences"`
		LocalReadings []string `json:"local_readings"`
	} `json:"daily"`
	Interval []struct {
		Name        string   `json:"name"`
		Seconds     int64    `json:"seconds"`
		Anchor      string   `json:"anchor"`
		Occurrences []string `json:"occurrences"`
		SweptAt     string   `json:"swept_at"`
		Coalesced   string   `json:"coalesced"`
	} `json:"interval"`
}

func loadOracle(t *testing.T) occurrenceOracle {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "occurrences.json"))
	if err != nil {
		t.Fatalf("read the oracle: %v", err)
	}
	var oracle occurrenceOracle
	if err := json.Unmarshal(body, &oracle); err != nil {
		t.Fatalf("decode the oracle: %v", err)
	}
	if len(oracle.Daily) == 0 || len(oracle.Interval) == 0 {
		t.Fatal("the oracle holds no case, so it would prove nothing")
	}
	return oracle
}

func instant(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("oracle instant %q: %v", raw, err)
	}
	return at.UTC()
}

// TestScheduleOccurrences is AC-020's independent oracle: every literal instant
// is matched by the rule, including the spring gap and the autumn repeat, and the
// coalesced sweep lands on the next future occurrence instead of catching up.
func TestScheduleOccurrences(t *testing.T) {
	oracle := loadOracle(t)
	for _, c := range oracle.Daily {
		t.Run(c.Name, func(t *testing.T) {
			zone, err := time.LoadLocation(c.Zone)
			if err != nil {
				t.Fatalf("load %s: %v", c.Zone, err)
			}
			if _, _, ok := parseLocalTime(c.LocalTime); !ok {
				t.Fatalf("oracle local time %q", c.LocalTime)
			}
			if len(c.LocalReadings) != len(c.Occurrences) {
				t.Fatalf("the oracle names %d readings for %d occurrences", len(c.LocalReadings), len(c.Occurrences))
			}
			rule := Rule{Kind: KindJobsPrune, LocalTime: c.LocalTime, Zone: zone}
			at := instant(t, c.After)
			for i, want := range c.Occurrences {
				next := Next(at, rule)
				wantAt := instant(t, want)
				if !next.Equal(wantAt) {
					t.Fatalf("occurrence %d after %s = %s, want %s", i+1, at.Format(time.RFC3339), next.Format(time.RFC3339), want)
				}
				local := next.In(zone)
				if got := fmt.Sprintf("%02d:%02d", local.Hour(), local.Minute()); got != c.LocalReadings[i] {
					t.Fatalf("occurrence %d reads %s in %s, want the oracle's %s", i+1, got, c.Zone, c.LocalReadings[i])
				}
				if !next.After(at) {
					t.Fatalf("occurrence %d at %s does not advance past %s", i+1, want, at.Format(time.RFC3339))
				}
				at = next
			}
		})
	}
	for _, c := range oracle.Interval {
		t.Run(c.Name, func(t *testing.T) {
			rule := Rule{Kind: KindJobsPrune, Interval: time.Duration(c.Seconds) * time.Second}
			at := instant(t, c.Anchor)
			for i, want := range c.Occurrences {
				next := Next(at, rule)
				if wantAt := instant(t, want); !next.Equal(wantAt) {
					t.Fatalf("occurrence %d after %s = %s, want %s", i+1, at.Format(time.RFC3339), next.Format(time.RFC3339), want)
				}
				at = next
			}
			swept := instant(t, c.SweptAt)
			next, ok := nextDue(swept, at, rule)
			if !ok {
				t.Fatalf("no occurrence after the sweep at %s", c.SweptAt)
			}
			if wantAt := instant(t, c.Coalesced); !next.Equal(wantAt) {
				t.Fatalf("the sweep at %s advanced to %s, want the coalesced %s", c.SweptAt, next.Format(time.RFC3339), c.Coalesced)
			}
		})
	}

	// The sweep is the only thing that fires a persisted occurrence, so this case
	// starts a runtime and drives it one sweep period instead of calling the
	// firing helper: deleting that call from sweepOnce leaves the row due and this
	// case red, which is what makes the gate certify the wiring and not just the
	// arithmetic.
	t.Run("a sweep period fires the due occurrence once", func(t *testing.T) {
		const kind = "test.work"
		db := openTestStore(t)
		clock := newFakeClock()
		// The fake clock starts at 2027-01-15T08:00Z, so this instant is 07:59Z the
		// same morning and one advance past it is the next day's 04:00.
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, kind, kind, "daily", "04:00", "UTC", due.Unix())
		var runs atomic.Int64
		rt := newRuntime(t, db, clock, "boot", handler(kind, func(context.Context, *Attempt) Outcome {
			runs.Add(1)
			return Outcome{Result: Succeeded, Reason: "ran"}
		}))

		stir(t, clock, "the sweep period to fire the due occurrence", func() bool {
			return runs.Load() == 1
		}, rt)

		if got := occurrenceRows(t, db, kind); got != 1 {
			t.Fatalf("%d occurrence rows after a sweep period, want the one due occurrence", got)
		}
		row := readSchedule(t, db, kind)
		want := time.Date(2027, 1, 16, 4, 0, 0, 0, time.UTC)
		if got := time.Unix(row.NextDueAt, 0).UTC(); !got.Equal(want) {
			t.Fatalf("next_due_at = %s, want the one advance to %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if row.Revision != 2 {
			t.Fatalf("revision = %d after one sweep, want the single advance", row.Revision)
		}
	})

	// A row left due by an earlier day fires on the day it comes back while that
	// day's wall clock is still ahead, and it lands on today's instant instead of
	// skipping to tomorrow's.
	t.Run("a row due from an earlier day fires before the day's wall clock", func(t *testing.T) {
		const kind = "test.work"
		db := openTestStore(t)
		clock := newFakeClock()
		rt := newRuntime(t, db, clock, "boot", handler(kind, func(context.Context, *Attempt) Outcome {
			return Outcome{Result: Succeeded, Reason: "ran"}
		}))
		// The clock starts at 2027-01-15T08:00Z and the row is still due from the
		// day before, so the sweep comes back at 00:30 with 04:00 still ahead.
		due := time.Date(2027, 1, 14, 4, 0, 0, 0, time.UTC)
		seedSchedule(t, db, kind, kind, "daily", "04:00", "UTC", due.Unix())
		clock.advance(-7*time.Hour - 30*time.Minute)

		if err := rt.sweepOnce(t.Context()); err != nil {
			t.Fatalf("sweepOnce: %v", err)
		}
		if got := occurrenceRows(t, db, kind); got != 1 {
			t.Fatalf("%d occurrence rows for the one due occurrence, want one", got)
		}
		row := readSchedule(t, db, kind)
		want := time.Date(2027, 1, 15, 4, 0, 0, 0, time.UTC)
		if got := time.Unix(row.NextDueAt, 0).UTC(); !got.Equal(want) {
			t.Fatalf("next_due_at = %s, want that day's own 04:00 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})
}

// seedSchedule writes one persisted schedule row.
func seedSchedule(t *testing.T, db *store.DB, name, kind, rule, localTime, zone string, nextDue int64) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := storedb.New(tx).InsertScheduleIfAbsent(t.Context(), storedb.InsertScheduleIfAbsentParams{
			Name:      name,
			Kind:      kind,
			Rule:      rule,
			LocalTime: sql.NullString{String: localTime, Valid: rule == "daily"},
			Zone:      sql.NullString{String: zone, Valid: rule == "daily"},
			NextDueAt: nextDue,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed schedule %s: %v", name, err)
	}
}

// readSchedule returns one schedule row.
func readSchedule(t *testing.T, db *store.DB, name string) storedb.Schedule {
	t.Helper()
	var row storedb.Schedule
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		row, err = storedb.New(tx).GetScheduleByName(t.Context(), name)
		return err
	})
	if err != nil {
		t.Fatalf("read schedule %s: %v", name, err)
	}
	return row
}

// countRows counts the rows matching one statement.
func countRows(t *testing.T, db *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// occurrenceRows is how many occurrence jobs one schedule has.
func occurrenceRows(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM job WHERE dedupe_key = ?`, scheduleDedupeKey(name))
}

// TestScheduleSweep is the occurrence identity: one persisted instant, one Write
// per sweep, one job per occurrence.
func TestScheduleSweep(t *testing.T) {
	const kind = "test.work"

	// sweepRuntime builds a runtime whose kind the tests can watch, over their
	// own store and clock.
	sweepRuntime := func(t *testing.T, db *store.DB, clock *fakeClock, runs *atomic.Int64) *Runtime {
		t.Helper()
		return newRuntime(t, db, clock, "boot", handler(kind, func(context.Context, *Attempt) Outcome {
			if runs != nil {
				runs.Add(1)
			}
			return Outcome{Result: Succeeded, Reason: "ran"}
		}))
	}

	t.Run("racing sweeps insert one occurrence", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		rt := sweepRuntime(t, db, clock, nil)
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
					t.Errorf("fireSchedules: %v", err)
				}
			}()
		}
		wg.Wait()

		if got := occurrenceRows(t, db, "test.work"); got != 1 {
			t.Fatalf("%d occurrence rows after the racing sweeps, want exactly one", got)
		}
		row := readSchedule(t, db, "test.work")
		want := time.Date(due.Year(), due.Month(), due.Day()+1, 4, 0, 0, 0, time.UTC)
		if got := time.Unix(row.NextDueAt, 0).UTC(); !got.Equal(want) {
			t.Fatalf("next_due_at = %s, want the next 04:00 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if row.Revision != 2 {
			t.Fatalf("revision = %d after one advance, want 2", row.Revision)
		}
	})

	t.Run("downtime coalesces into one run", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		rt := sweepRuntime(t, db, clock, nil)
		due := time.Date(2027, 1, 1, 4, 0, 0, 0, time.UTC)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())

		// Ten days of downtime: nine occurrences were missed.
		now := due.AddDate(0, 0, 10)
		if err := rt.fireSchedules(t.Context(), now); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 1 {
			t.Fatalf("%d occurrence rows after ten missed days, want one coalesced run", got)
		}
		row := readSchedule(t, db, "test.work")
		want := time.Date(2027, 1, 12, 4, 0, 0, 0, time.UTC)
		if got := time.Unix(row.NextDueAt, 0).UTC(); !got.Equal(want) {
			t.Fatalf("next_due_at = %s, want the first occurrence after the downtime %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("a clock rollback runs nothing early", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := sweepRuntime(t, db, clock, &runs)
		due := time.Date(2027, 1, 16, 4, 0, 0, 0, time.UTC)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())

		// The process clock jumps an hour back: the row is further from due than it
		// was, so the sweep produces no occurrence and moves nothing.
		clock.advance(-time.Hour)
		if err := rt.sweepOnce(t.Context()); err != nil {
			t.Fatalf("sweepOnce: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 0 {
			t.Fatalf("%d occurrence rows after the rollback, want none", got)
		}
		if row := readSchedule(t, db, "test.work"); row.NextDueAt != due.Unix() || row.Revision != 1 {
			t.Fatalf("the schedule = due %d revision %d after the rollback, want it still due at %d with revision 1", row.NextDueAt, row.Revision, due.Unix())
		}
		if got := runs.Load(); got != 0 {
			t.Fatalf("the kind ran %d times early, want nothing before the instant it was due", got)
		}
	})

	t.Run("an open occurrence coalesces instead of a second run", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		// The open occurrence is held running for the whole case, so the sweep meets
		// a row the lane already owns instead of racing it for the claim.
		release := make(chan struct{})
		rt := newRuntime(t, db, clock, "boot", handler(kind, func(ctx context.Context, _ *Attempt) Outcome {
			select {
			case <-release:
				return Outcome{Result: Succeeded, Reason: "ran"}
			case <-ctx.Done():
				return Outcome{Result: Failed, Reason: "cancelled"}
			}
		}))
		t.Cleanup(func() { close(release) })
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())
		open := enqueue(t, db, store.JobIntent{
			Kind: kind, Class: store.JobRequired, Lane: store.JobBackground,
			DedupeKey: scheduleDedupeKey("test.work"), ConcurrencyKey: kind,
			MaxAttempts: 3, DueAt: clock.Now().Unix(),
		})
		// A written intent is claimed after the commit's wake or the next sweep,
		// so the case stirs the runtime rather than assuming a lane was listening.
		stir(t, clock, "the lane to claim the open occurrence", func() bool {
			return jobOf(t, db, open).State == store.JobRunning
		}, rt)

		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 1 {
			t.Fatalf("%d occurrence rows with one open, want the open one alone", got)
		}
		if row := jobOf(t, db, open); row.State != store.JobRunning || row.Attempts != 1 {
			t.Fatalf("the open occurrence = %s after %d attempts, want the one held attempt", row.State, row.Attempts)
		}
		row := readSchedule(t, db, "test.work")
		if row.Revision != 2 {
			t.Fatalf("revision = %d, want the coalesced occurrence to advance once", row.Revision)
		}
	})

	t.Run("a full optional queue defers without advancing", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		rt := sweepRuntime(t, db, clock, nil)
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())
		err := db.Write(t.Context(), func(tx *sql.Tx) error {
			for i := range maxQueuedOptional {
				if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
					Kind: "test.filler", Class: store.JobOptional, Lane: store.JobBackground,
					DedupeKey: fmt.Sprintf("filler-%d", i), ConcurrencyKey: fmt.Sprintf("filler-%d", i),
					MaxAttempts: 1, DueAt: clock.Now().Unix(),
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("fill the optional queue: %v", err)
		}

		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 0 {
			t.Fatalf("%d occurrence rows with a full queue, want none", got)
		}
		row := readSchedule(t, db, "test.work")
		if row.NextDueAt != due.Unix() || row.Revision != 1 {
			t.Fatalf("the deferred schedule = due %d revision %d, want it still due at %d with revision 1", row.NextDueAt, row.Revision, due.Unix())
		}

		// Capacity returns: the occurrence that was deferred fires.
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `DELETE FROM job WHERE kind = 'test.filler'`)
			return err
		}); err != nil {
			t.Fatalf("free the optional queue: %v", err)
		}
		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules after capacity returned: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 1 {
			t.Fatalf("%d occurrence rows after capacity returned, want one", got)
		}
	})

	// A fired optional occurrence joins the queue an accepted run-now joins, so it
	// names the same expiry: only a row that names one is ever expired, and a slot
	// nothing releases is a full optional queue for ever. A required occurrence
	// names none, because required work is never expired.
	t.Run("an optional occurrence carries the queue expiry and a required one does not", func(t *testing.T) {
		const optionalKind = "test.optional"
		db := openTestStore(t)
		clock := newFakeClock()
		ran := func(context.Context, *Attempt) Outcome { return Outcome{Result: Succeeded, Reason: "ran"} }
		optional := handler(optionalKind, ran)
		optional.Class = ClassOptional
		rt := newRuntime(t, db, clock, "boot", handler(kind, ran), optional)
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, kind, kind, "daily", "04:00", "UTC", due.Unix())
		seedSchedule(t, db, optionalKind, optionalKind, "daily", "04:00", "UTC", due.Unix())

		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}

		expiryOf := func(fired string) *int64 {
			t.Helper()
			var at sql.NullInt64
			err := db.Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(),
					`SELECT expires_at FROM job WHERE dedupe_key = ?`, scheduleDedupeKey(fired)).Scan(&at)
			})
			if err != nil {
				t.Fatalf("read the %s occurrence's expiry: %v", fired, err)
			}
			if !at.Valid {
				return nil
			}
			return &at.Int64
		}
		want := clock.Now().Add(optionalExpiry).Unix()
		// The recorded expiry is one hour, asserted beside the constant so a moved
		// constant cannot quietly redefine what this proof accepts.
		if hour := clock.Now().Add(time.Hour).Unix(); want != hour {
			t.Fatalf("optionalExpiry = %s, want the recorded hour", optionalExpiry)
		}
		switch got := expiryOf(optionalKind); {
		case got == nil:
			t.Fatalf("the optional occurrence names no expiry, want %d", want)
		case *got != want:
			t.Fatalf("the optional occurrence expires at %d, want %d", *got, want)
		}
		if got := expiryOf(kind); got != nil {
			t.Fatalf("the required occurrence expires at %d, want no expiry", *got)
		}
	})

	t.Run("an attempt owning the key defers the occurrence", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())
		// A run-now attempt of the same kind holds the key under another dedupe.
		enqueue(t, db, store.JobIntent{
			Kind: kind, Class: store.JobRequired, Lane: store.JobBackground,
			DedupeKey: "run-now", ConcurrencyKey: kind,
			MaxAttempts: 3, DueAt: clock.Now().Unix(),
		})
		if _, claimed, err := db.ClaimJob(t.Context(), store.JobBackground, "boot", clock.Now().Unix()); err != nil || !claimed {
			t.Fatalf("claim the run-now attempt = %v (err %v)", claimed, err)
		}
		// The runtime starts after the claim: a lane started first could take the
		// key itself and finish it, leaving this case nothing to defer.
		rt := sweepRuntime(t, db, clock, nil)

		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 0 {
			t.Fatalf("%d occurrence rows while the key was owned, want none", got)
		}
		if row := readSchedule(t, db, "test.work"); row.NextDueAt != due.Unix() {
			t.Fatalf("the schedule advanced to %d while the key was owned, want it still due at %d", row.NextDueAt, due.Unix())
		}
	})

	t.Run("the occurrence runs the kind once", func(t *testing.T) {
		db := openTestStore(t)
		clock := newFakeClock()
		var runs atomic.Int64
		rt := sweepRuntime(t, db, clock, &runs)
		due := clock.Now().Truncate(time.Minute).Add(-time.Minute)
		seedSchedule(t, db, "test.work", kind, "daily", "04:00", "UTC", due.Unix())
		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("fireSchedules: %v", err)
		}
		stir(t, clock, "the occurrence to run", func() bool { return runs.Load() == 1 }, rt)
		// A second sweep past the same schedule has nothing due: the occurrence that
		// already ran is not produced again.
		if err := rt.fireSchedules(t.Context(), clock.Now()); err != nil {
			t.Fatalf("second fireSchedules: %v", err)
		}
		if got := occurrenceRows(t, db, "test.work"); got != 1 {
			t.Fatalf("%d occurrence rows after the second sweep, want the one that ran", got)
		}
		if row := readSchedule(t, db, "test.work"); row.Revision != 2 {
			t.Fatalf("revision after the second sweep = %d, want the one advance", row.Revision)
		}
		if got := runs.Load(); got != 1 {
			t.Fatalf("the kind ran %d times for one occurrence, want one", got)
		}
	})
}

// TestScheduleSeeding is the built-in maintenance schedule: written once,
// rewritten only when the install zone changed.
func TestScheduleSeeding(t *testing.T) {
	db := openTestStore(t)
	clock := newFakeClock()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Europe/Berlin: %v", err)
	}

	start := func(zone *time.Location) *Runtime {
		t.Helper()
		rt, err := New(db, Config{
			Now: clock.Now, After: clock.After, Boot: "boot", BackgroundSlots: 1,
			Log: func(string, ...any) {}, Zone: zone,
		}, []Handler{PruneHandler(db)})
		if err != nil {
			t.Fatalf("jobs.New: %v", err)
		}
		if err := rt.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return rt
	}
	stop := func(rt *Runtime) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.Stop(ctx); err != nil && !errors.Is(err, ErrAttemptsLive) {
			t.Fatalf("Stop: %v", err)
		}
	}

	// 08:00 UTC is 09:00 in Berlin, so the day's 04:00 already passed.
	first := start(nil)
	seeded := readSchedule(t, db, KindJobsPrune)
	if seeded.Zone.String != "UTC" || seeded.LocalTime.String != pruneLocalTime {
		t.Fatalf("the seeded schedule = zone %q at %q, want UTC at %q", seeded.Zone.String, seeded.LocalTime.String, pruneLocalTime)
	}
	if want := time.Date(2027, 1, 16, 4, 0, 0, 0, time.UTC); !time.Unix(seeded.NextDueAt, 0).UTC().Equal(want) {
		t.Fatalf("the seeded instant = %s, want %s", time.Unix(seeded.NextDueAt, 0).UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
	stop(first)

	// A restart in the same zone keeps the stored instant: downtime is measured
	// from where the schedule was, not from the restart.
	second := start(nil)
	kept := readSchedule(t, db, KindJobsPrune)
	if kept.NextDueAt != seeded.NextDueAt || kept.Revision != seeded.Revision {
		t.Fatalf("the restart rewrote the schedule to %d revision %d, want %d revision %d", kept.NextDueAt, kept.Revision, seeded.NextDueAt, seeded.Revision)
	}
	stop(second)

	// A zone change rewrites it once.
	third := start(berlin)
	changed := readSchedule(t, db, KindJobsPrune)
	if changed.Zone.String != "Europe/Berlin" {
		t.Fatalf("the zone after the change = %q, want Europe/Berlin", changed.Zone.String)
	}
	if want := time.Date(2027, 1, 16, 4, 0, 0, 0, berlin); !time.Unix(changed.NextDueAt, 0).UTC().Equal(want.UTC()) {
		t.Fatalf("the reseeded instant = %s, want 04:00 Europe/Berlin %s", time.Unix(changed.NextDueAt, 0).UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
	}
	if changed.Revision != seeded.Revision+1 {
		t.Fatalf("revision after the zone change = %d, want %d", changed.Revision, seeded.Revision+1)
	}
	stop(third)

	fourth := start(berlin)
	if again := readSchedule(t, db, KindJobsPrune); again.NextDueAt != changed.NextDueAt || again.Revision != changed.Revision {
		t.Fatalf("a same-zone restart rewrote the schedule to %d revision %d, want %d revision %d", again.NextDueAt, again.Revision, changed.NextDueAt, changed.Revision)
	}
	stop(fourth)
}

// insertJobRow writes one row in the state and at the instant the retention
// rules act on, so a prune test seeds the table directly.
func insertJobRow(t *testing.T, db *store.DB, seq int, kind, class, state string, updatedAt int64) {
	t.Helper()
	key := fmt.Sprintf("%s-%d", kind, seq)
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job (
            kind, class, lane, dedupe_key, concurrency_key, params, state,
            attempts, max_attempts, due_at, created_at, updated_at, revision
        ) VALUES (?, ?, 'background', ?, ?, '{}', ?, 0, 3, ?, ?, ?, 1)`,
			kind, class, key, key, state, updatedAt, updatedAt, updatedAt)
		return err
	})
	if err != nil {
		t.Fatalf("insert the %s/%s row: %v", class, state, err)
	}
}

// TestPruneRetention is the jobs.prune boundary: only terminal optional rows past
// seven days go, and every other row is left where it is.
func TestPruneRetention(t *testing.T) {
	db := openTestStore(t)
	now := time.Now().Unix()
	old := now - int64((8 * 24 * time.Hour).Seconds())           // older than seven days
	recent := now - int64((6 * 24 * time.Hour).Seconds())        // inside the retention
	boundary := now - int64((7 * 24 * time.Hour).Seconds()) + 60 // seven days, one minute to spare

	insertJobRow(t, db, 1, "test.old.succeeded", store.JobOptional, store.JobSucceeded, old)
	insertJobRow(t, db, 2, "test.old.failed", store.JobOptional, store.JobFailed, old)
	insertJobRow(t, db, 3, "test.old.expired", store.JobOptional, store.JobExpired, old)
	insertJobRow(t, db, 4, "test.recent", store.JobOptional, store.JobSucceeded, recent)
	insertJobRow(t, db, 5, "test.boundary", store.JobOptional, store.JobSucceeded, boundary)
	insertJobRow(t, db, 6, "test.required", store.JobRequired, store.JobSucceeded, old)
	insertJobRow(t, db, 7, "test.queued", store.JobOptional, store.JobQueued, old)
	insertJobRow(t, db, 8, "test.review", store.JobOptional, store.JobReview, old)

	out := PruneHandler(db).Run(t.Context(), &Attempt{})
	if out.Result != Succeeded {
		t.Fatalf("prune outcome = %+v, want it to succeed", out)
	}
	if got := out.Params["pruned"]; got != "3" {
		t.Fatalf("prune reported %q rows, want the three held rows", got)
	}
	if left := countRows(t, db, `SELECT count(*) FROM job`); left != 5 {
		t.Fatalf("%d rows left, want the five the retention keeps", left)
	}
	for _, kind := range []string{"test.old.succeeded", "test.old.failed", "test.old.expired"} {
		if kept := countRows(t, db, `SELECT count(*) FROM job WHERE kind = ?`, kind); kept != 0 {
			t.Fatalf("the terminal row %s survived the prune", kind)
		}
	}
	for _, kind := range []string{"test.recent", "test.boundary", "test.required", "test.queued", "test.review"} {
		if kept := countRows(t, db, `SELECT count(*) FROM job WHERE kind = ?`, kind); kept != 1 {
			t.Fatalf("the row %s was pruned, want it kept", kind)
		}
	}
}

// TestPruneBatch is the write bound: one Write deletes at most 500 rows, and the
// handler loops until the retention set is empty.
func TestPruneBatch(t *testing.T) {
	db := openTestStore(t)
	updatedAt := time.Now().Add(-8 * 24 * time.Hour).Unix()
	const rows = 700
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for i := range rows {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job (
                kind, class, lane, dedupe_key, concurrency_key, params, state,
                attempts, max_attempts, due_at, created_at, updated_at, revision
            ) VALUES ('test.bulk', 'optional', 'background', ?, ?, '{}', 'succeeded', 0, 3, ?, ?, ?, 1)`,
				fmt.Sprintf("bulk-%d", i), fmt.Sprintf("bulk-%d", i), updatedAt, updatedAt, updatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the retention set: %v", err)
	}

	n, err := pruneTerminal(t.Context(), db, time.Now().Add(-pruneRetention).Unix())
	if err != nil {
		t.Fatalf("pruneTerminal: %v", err)
	}
	if n != pruneBatch {
		t.Fatalf("one Write deleted %d rows, want the bounded %d", n, pruneBatch)
	}
	if left := countRows(t, db, `SELECT count(*) FROM job WHERE kind = 'test.bulk'`); left != rows-pruneBatch {
		t.Fatalf("%d rows left after one batch, want %d", left, rows-pruneBatch)
	}

	out := PruneHandler(db).Run(t.Context(), &Attempt{})
	if out.Result != Succeeded {
		t.Fatalf("prune outcome = %+v, want it to succeed", out)
	}
	if got := out.Params["pruned"]; got != fmt.Sprint(rows-pruneBatch) {
		t.Fatalf("prune reported %q rows, want the remaining %d", got, rows-pruneBatch)
	}
	if left := countRows(t, db, `SELECT count(*) FROM job WHERE kind = 'test.bulk'`); left != 0 {
		t.Fatalf("%d retention rows survived the loop, want none", left)
	}
}
