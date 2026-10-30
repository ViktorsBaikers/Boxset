package jobs

// Schedules: a persisted UTC instant is the only scheduler. The sweep re-reads
// next_due_at, advances it with one Write whose CAS on the old instant makes the
// occurrence exactly once, and enqueues the job in that same transaction. A lost
// wake therefore costs at most one sweep period, a competing sweep advances
// nothing, and downtime collapses every missed occurrence into the one that is
// still due. Nothing here arms a timer of its own.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// The bounds a schedule shares with the queue it fires into.
const (
	// maxQueuedOptional is the optional queue ceiling, the same count the
	// admission service reads: a full queue defers a due occurrence instead of
	// advancing past it.
	maxQueuedOptional = 1024
	// intervalMin and intervalMax are the schema's interval bounds, in seconds.
	intervalMin = 60
	intervalMax = 25 * 24 * 60 * 60
)

// Rule is one schedule's recurrence. A daily rule keeps LocalTime on the wall
// clock of Zone; an interval rule repeats every Interval from its anchor, which
// is the instant the schedule was seeded at.
type Rule struct {
	Kind      string
	Interval  time.Duration
	LocalTime string
	Zone      *time.Location
}

// ZoneName is the stored spelling of the rule's zone.
func (r Rule) ZoneName() string {
	if r.Zone == nil {
		return time.UTC.String()
	}
	return r.Zone.String()
}

// Next is the first occurrence after after. A daily rule names its wall clock on
// the next local date of its zone: a wall clock the zone skipped continues at
// the first instant after the gap, and one the zone repeated runs at the earlier
// instant. An interval rule adds one interval, so the chain anchor + k x
// interval is the caller's previous occurrence.
func Next(after time.Time, r Rule) time.Time {
	if r.Interval > 0 {
		return after.Add(r.Interval).UTC().Truncate(time.Second)
	}
	hour, minute, ok := parseLocalTime(r.LocalTime)
	if !ok {
		return after
	}
	zone := r.Zone
	if zone == nil {
		zone = time.UTC
	}
	year, month, day := after.In(zone).Date()
	return wallInstant(zone, time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC), hour, minute)
}

// nextDue is the first occurrence after now for a schedule whose stored instant
// is old. Every instant between old and now is a missed occurrence that collapses
// into this one, so a stopped installation runs once when it comes back. A rule
// that cannot produce an instant after now is reported instead of guessed at.
func nextDue(now, old time.Time, r Rule) (time.Time, bool) {
	if r.Interval > 0 {
		anchor := old
		if anchor.IsZero() {
			// A new interval schedule is anchored where it was seeded.
			anchor = now
		}
		steps := int64(now.Sub(anchor)/r.Interval) + 1
		if steps < 1 {
			steps = 1
		}
		next := anchor.Add(time.Duration(steps) * r.Interval)
		if !next.After(now) {
			// Sub saturates past the duration range, so the estimate can still be
			// short of now after an absurd jump.
			next = next.Add(r.Interval)
		}
		return next, next.After(now) && next.After(anchor)
	}
	hour, minute, ok := parseLocalTime(r.LocalTime)
	if !ok {
		return time.Time{}, false
	}
	zone := r.Zone
	if zone == nil {
		zone = time.UTC
	}
	// The first occurrence after now is today's, when its wall clock has not
	// passed yet, and the next local date's otherwise.
	year, month, day := now.In(zone).Date()
	today := wallInstant(zone, time.Date(year, month, day, 0, 0, 0, 0, time.UTC), hour, minute)
	if today.After(now) && today.After(old) {
		return today, true
	}
	next := Next(now, r)
	if next.After(now) && next.After(old) {
		return next, true
	}
	return time.Time{}, false
}

// parseLocalTime reads a stored HH:MM wall clock.
func parseLocalTime(raw string) (hour, minute int, ok bool) {
	before, after, found := strings.Cut(raw, ":")
	if !found || len(before) != 2 || len(after) != 2 {
		return 0, 0, false
	}
	hour, err := strconv.Atoi(before)
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	minute, err = strconv.Atoi(after)
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// wallInstant is the instant whose wall clock in zone reads hh:mm on date. A
// wall clock the zone repeated has two instants and the earlier is the
// occurrence; one a gap skipped has none, and the first instant after the gap is.
func wallInstant(zone *time.Location, date time.Time, hour, minute int) time.Time {
	year, month, day := date.Date()
	want := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	midnight := time.Date(year, month, day, 0, 0, 0, 0, zone)
	var pick time.Time
	found := false
	// The offsets that can apply within one local date: its start, its middle and
	// its end. Reading the wanted wall clock with each names every candidate.
	for _, probe := range []time.Time{midnight, midnight.Add(12 * time.Hour), midnight.Add(24 * time.Hour)} {
		_, offset := probe.Zone()
		at := want.Add(-time.Duration(offset) * time.Second)
		if !wallOf(at, zone).Equal(want) {
			continue
		}
		if !found || at.Before(pick) {
			pick, found = at, true
		}
	}
	if found {
		return pick
	}
	// The zone skipped the wall clock: the answer is the first instant whose local
	// reading is at or past it, which is the transition itself. The lower bound is
	// one second before midnight, because a date whose gap begins at 00:00 has no
	// midnight of its own: time.Date answers inside or past that gap, and a search
	// started there closes one second past the answer.
	low, high := midnight.Add(-time.Second), midnight.Add(24*time.Hour)
	for high.Sub(low) > time.Second {
		// Whole seconds only: an occurrence is stored as a unix second, so the
		// answer lands on the transition instead of a fraction of a second past it.
		middle := low.Add(high.Sub(low) / 2).Truncate(time.Second)
		if wallOf(middle, zone).Before(want) {
			low = middle
		} else {
			high = middle
		}
	}
	return high
}

// wallOf is t's wall clock in zone as a UTC time value, so two local readings
// compare without an offset.
func wallOf(t time.Time, zone *time.Location) time.Time {
	local := t.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
}

// scheduleDedupeKey is one schedule's occurrence identity: a second occurrence
// enqueued while one is open coalesces into that row instead of taking a second.
func scheduleDedupeKey(name string) string { return "sched:" + name }

// builtinSchedules are the rules every installation runs, filtered to the kinds
// this runtime can actually execute.
func (r *Runtime) builtinSchedules() []Rule {
	if _, ok := r.handlers[KindJobsPrune]; !ok {
		return nil
	}
	return []Rule{{Kind: KindJobsPrune, LocalTime: pruneLocalTime, Zone: r.cfg.Zone}}
}

// seedSchedules writes the built-in rules once, and rewrites one only when the
// install zone changed: a restart keeps the stored instant, so downtime is
// measured from where the schedule actually was instead of from the restart.
func (r *Runtime) seedSchedules(ctx context.Context, now time.Time) error {
	rules := r.builtinSchedules()
	if len(rules) == 0 {
		return nil
	}
	return r.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		for _, rule := range rules {
			next, ok := nextDue(now, time.Time{}, rule)
			if !ok {
				return fmt.Errorf("%w: rule %s %q", ErrConfig, rule.Kind, rule.LocalTime)
			}
			// The built-in schedule is named after its kind, so its occurrence
			// dedupe key is the recorded sched:<kind>.
			name := rule.Kind
			inserted, err := q.InsertScheduleIfAbsent(ctx, storedb.InsertScheduleIfAbsentParams{
				Name:      name,
				Kind:      rule.Kind,
				Rule:      "daily",
				LocalTime: sql.NullString{String: rule.LocalTime, Valid: true},
				Zone:      sql.NullString{String: rule.ZoneName(), Valid: true},
				NextDueAt: next.Unix(),
			})
			if err != nil {
				return err
			}
			if inserted == 1 {
				continue
			}
			if _, err := q.ReseedScheduleZone(ctx, storedb.ReseedScheduleZoneParams{
				Zone:      sql.NullString{String: rule.ZoneName(), Valid: true},
				NextDueAt: next.Unix(),
				Name:      name,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// fireSchedules advances every due schedule and enqueues its occurrence.
func (r *Runtime) fireSchedules(ctx context.Context, now time.Time) error {
	var due []storedb.Schedule
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		due, err = storedb.New(tx).DueSchedules(ctx, now.Unix())
		return err
	})
	if err != nil {
		return err
	}
	for _, row := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.fireSchedule(ctx, row, now); err != nil {
			return err
		}
	}
	return nil
}

// fireSchedule is one schedule's occurrence: one Write that advances the row with
// a CAS on the instant the sweep read, and inserts the job the advance is for.
// A deferred row writes nothing and stays due, so the next sweep after capacity
// returns fires it; a coalesced one advances without a second row.
func (r *Runtime) fireSchedule(ctx context.Context, row storedb.Schedule, now time.Time) error {
	rule, ok := ruleOf(row)
	if !ok {
		r.failed("schedule_rule_invalid", "schedule", row.Name, "kind", row.Kind)
		return nil
	}
	next, ok := nextDue(now, time.Unix(row.NextDueAt, 0).UTC(), rule)
	if !ok {
		r.failed("schedule_rule_invalid", "schedule", row.Name, "kind", row.Kind, "next_due_at", row.NextDueAt)
		return nil
	}
	h, ok := r.handlers[row.Kind]
	if !ok {
		r.failed("schedule_kind_unknown", "schedule", row.Name, "kind", row.Kind)
		return nil
	}
	dedupe := scheduleDedupeKey(row.Name)
	deferred := ""
	fired := false
	err := r.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		// The same work is already open: this occurrence coalesces into it.
		if _, err := q.OpenJobID(ctx, storedb.OpenJobIDParams{Kind: row.Kind, DedupeKey: dedupe}); err == nil {
			fired, err = advanceSchedule(ctx, q, row, next)
			return err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A full optional queue defers the occurrence without advancing past it.
		queued, err := q.CountOptionalQueued(ctx)
		if err != nil {
			return err
		}
		if queued >= maxQueuedOptional {
			deferred = "queue_full"
			return nil
		}
		// Another open job of this kind owns its key: the occurrence waits for it
		// rather than taking a second owner of the same work.
		if _, err := q.OpenJobForKey(ctx, row.Kind); err == nil {
			deferred = "key_owned"
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if fired, err = advanceSchedule(ctx, q, row, next); err != nil || !fired {
			return err
		}
		// An optional occurrence waits in the queue an accepted run-now does, so it
		// carries the same expiry and releases its optional-queue slot when nothing
		// claims it in time.
		var expires *int64
		if h.Class == ClassOptional {
			at := now.Add(optionalExpiry).Unix()
			expires = &at
		}
		_, _, err = store.EnqueueJob(ctx, tx, store.JobIntent{
			Kind:           row.Kind,
			Class:          string(h.Class),
			Lane:           string(h.Lane),
			DedupeKey:      dedupe,
			ConcurrencyKey: row.Kind,
			Params:         map[string]string{"schedule": row.Name},
			MaxAttempts:    h.MaxAttempts,
			DueAt:          now.Unix(),
			ExpiresAt:      expires,
		})
		return err
	})
	if err != nil {
		r.failed("schedule_failed", "schedule", row.Name, "kind", row.Kind, "error", err)
		return err
	}
	if deferred != "" {
		r.failed("schedule_deferred", "schedule", row.Name, "kind", row.Kind, "reason", deferred)
		return nil
	}
	if fired {
		r.Wake()
	}
	return nil
}

// advanceSchedule is the schedule's one advance: the old instant is the CAS, so
// a competing sweep that already moved it advances nothing.
func advanceSchedule(ctx context.Context, q *storedb.Queries, row storedb.Schedule, next time.Time) (bool, error) {
	moved, err := q.AdvanceSchedule(ctx, storedb.AdvanceScheduleParams{
		NextDueAt:   next.Unix(),
		ID:          row.ID,
		NextDueAt_2: row.NextDueAt,
	})
	return moved == 1, err
}

// ruleOf reads a persisted row back into the rule the sweep evaluates.
func ruleOf(row storedb.Schedule) (Rule, bool) {
	switch row.Rule {
	case "interval":
		if !row.IntervalS.Valid {
			return Rule{}, false
		}
		if row.IntervalS.Int64 < intervalMin || row.IntervalS.Int64 > intervalMax {
			return Rule{}, false
		}
		return Rule{Kind: row.Kind, Interval: time.Duration(row.IntervalS.Int64) * time.Second}, true
	case "daily":
		if !row.LocalTime.Valid || !row.Zone.Valid {
			return Rule{}, false
		}
		if _, _, ok := parseLocalTime(row.LocalTime.String); !ok {
			return Rule{}, false
		}
		zone, err := time.LoadLocation(row.Zone.String)
		if err != nil {
			return Rule{}, false
		}
		return Rule{Kind: row.Kind, LocalTime: row.LocalTime.String, Zone: zone}, true
	}
	return Rule{}, false
}
