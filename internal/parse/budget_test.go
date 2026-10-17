package parse

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// hookClock is an injected clock that calls on(n) at its n-th read, before the
// read returns; on may advance the clock (its return value) and run side
// effects such as cancelling a context or publishing a revision. A clock with
// step 0 never moves except through on, so budget edges are exact.
type hookClock struct {
	mu    sync.Mutex
	t     time.Time
	step  time.Duration
	calls int
	on    func(call int) time.Duration
}

func newHookClock(step time.Duration, on func(call int) time.Duration) *hookClock {
	return &hookClock{t: time.Unix(1000, 0), step: step, on: on}
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	var extra time.Duration
	if c.on != nil {
		extra = c.on(n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(extra)
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *hookClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// jumpAt advances the clock by d at read n only.
func jumpAt(n int, d time.Duration) func(int) time.Duration {
	return func(call int) time.Duration {
		if call == n {
			return d
		}
		return 0
	}
}

const hdtvName = "Show.720p.HDTV"

func compileFormats(t *testing.T, p *Parser, revision uint64, specs ...FormatSpec) {
	t.Helper()
	if err := p.CompileFormats(context.Background(), revision, specs); err != nil {
		t.Fatalf("CompileFormats(%d): %v", revision, err)
	}
}

func required(pattern string) ConditionSpec { return ConditionSpec{Pattern: pattern, Required: true} }

func single(id string, conds ...ConditionSpec) FormatSpec {
	return FormatSpec{ID: id, Conditions: conds}
}

// With one format of one condition an Evaluate reads the clock five times: the
// budget's creation, the opening check, before and after the match, and the
// final check. The expiry table below pins each of those reads.
const singleEvaluateReads = 5

func TestBudgetEvaluateExpiryAtEveryClockRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		jumpAt  int
		jump    time.Duration
		wantErr bool
	}{
		{"no jump", 0, 0, false},
		{"jump while the budget is created restarts it", 1, releaseBudget, false},
		{"opening check", 2, releaseBudget, true},
		{"before the match", 3, releaseBudget, true},
		{"after a successful match", 4, releaseBudget, true},
		{"final check", 5, releaseBudget, true},
		{"one nanosecond inside the limit", 4, releaseBudget - 1, false},
		{"exactly the limit", 4, releaseBudget, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newHookClock(0, jumpAt(tc.jumpAt, tc.jump))
			p := newTestParser(t, clk.Now)
			compileFormats(t, p, 1, single("hd", required("hdtv")))
			res, err := p.Evaluate(ctx, hdtvName)
			if !tc.wantErr {
				if err != nil || !slices.Equal(res, []FormatResult{{ID: "hd", Outcome: OutcomeMatched}}) {
					t.Fatalf("control = %+v, %v; want the match", res, err)
				}
				if clk.Calls() != singleEvaluateReads {
					t.Errorf("clock reads = %d, want %d", clk.Calls(), singleEvaluateReads)
				}
				return
			}
			wantReason(t, err, codeWorkBudgetExhausted, map[string]int64{"limit_ms": 250})
			if res != nil {
				t.Errorf("an exhausted budget must carry no results: %+v", res)
			}
			if clk.Calls() != tc.jumpAt {
				t.Errorf("clock reads = %d, want %d: no work may follow the failing check", clk.Calls(), tc.jumpAt)
			}
		})
	}
}

func TestBudgetLateNegatedSuccessIsAnError(t *testing.T) {
	// The condition does not match and is negated, so on time it is satisfied;
	// completing after expiry must still be an error, never that satisfied match.
	for _, late := range []bool{false, true} {
		var on func(int) time.Duration
		if late {
			on = jumpAt(4, releaseBudget)
		}
		clk := newHookClock(0, on)
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("not-bluray", ConditionSpec{Pattern: "bluray", Negate: true, Required: true}))
		res, err := p.Evaluate(context.Background(), hdtvName)
		if !late {
			if err != nil || len(res) != 1 || res[0].Outcome != OutcomeMatched {
				t.Fatalf("on-time negation = %+v, %v; want the match", res, err)
			}
			continue
		}
		wantReason(t, err, codeWorkBudgetExhausted, map[string]int64{"limit_ms": 250})
		if res != nil {
			t.Errorf("late negated success must carry no results: %+v", res)
		}
	}
}

func TestBudgetExpiryStopsFurtherScheduling(t *testing.T) {
	// Two formats; the budget expires after the first format's match. The
	// second format must not be scheduled and the first match must not leak.
	clk := newHookClock(0, jumpAt(4, releaseBudget))
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("a", required("hdtv")), single("b", required("720p")))
	res, err := p.Evaluate(context.Background(), hdtvName)
	wantReason(t, err, codeWorkBudgetExhausted, map[string]int64{"limit_ms": 250})
	if res != nil {
		t.Errorf("no successful subset may escape: %+v", res)
	}
	if clk.Calls() != 4 {
		t.Errorf("clock reads = %d, want 4 (no read, so no match, for the second format)", clk.Calls())
	}
}

func TestBudgetEmptyProfileIsStillGuardedAfterItsLastStep(t *testing.T) {
	for _, jump := range []time.Duration{0, releaseBudget} {
		clk := newHookClock(0, jumpAt(3, jump))
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1)
		res, err := p.Evaluate(context.Background(), hdtvName)
		if jump == 0 {
			if err != nil || res == nil || len(res) != 0 {
				t.Fatalf("empty profile = %+v, %v; want an empty list", res, err)
			}
			continue
		}
		wantReason(t, err, codeWorkBudgetExhausted, map[string]int64{"limit_ms": 250})
		if res != nil {
			t.Errorf("expired empty profile must carry no results: %+v", res)
		}
	}
}

func TestBudgetEvaluateContext(t *testing.T) {
	p := newTestParser(t, newHookClock(time.Microsecond, nil).Now)
	compileFormats(t, p, 1, single("hd", required("hdtv")))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := p.Evaluate(cancelled, hdtvName)
	wantReason(t, err, codeCancelled, nil)
	if res != nil {
		t.Errorf("cancelled evaluation must carry no results: %+v", res)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	res, err = p.Evaluate(expired, hdtvName)
	wantReason(t, err, codeDeadlineExceeded, nil)
	if res != nil {
		t.Errorf("expired evaluation must carry no results: %+v", res)
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("lease state after failures = %d retired, %d readers, want none", retired, readers)
	}
}

// leaseState reports the retired sets and admitted operations of p.
func leaseState(p *Parser) (retired, readers int) {
	p.formats.mu.Lock()
	defer p.formats.mu.Unlock()
	return len(p.formats.retired), p.formats.readers
}

func batchStates(rows []ReleaseResult) []ReleaseState {
	out := make([]ReleaseState, len(rows))
	for i, r := range rows {
		out[i] = r.State
	}
	return out
}

// A batch of single-format single-condition names reads the clock once for the
// decision deadline and then five times per release (see singleEvaluateReads).
const batchHeadReads = 1

func TestBatchReleaseBudgetRejectsOnlyThatRelease(t *testing.T) {
	// The first release expires after its match; the second gets a fresh budget.
	clk := newHookClock(0, jumpAt(batchHeadReads+4, releaseBudget))
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("hd", required("hdtv")))
	rows, err := p.EvaluateBatch(context.Background(), []string{hdtvName, "Other.1080p.HDTV"})
	if err != nil {
		t.Fatalf("a release error must not fail the batch: %v", err)
	}
	if want := []ReleaseState{ReleaseRejected, ReleaseCompleted}; !slices.Equal(batchStates(rows), want) {
		t.Fatalf("states = %v, want %v", batchStates(rows), want)
	}
	if rows[0].Index != 0 || rows[0].Formats != nil || rows[0].Reason == nil ||
		rows[0].Reason.Code != codeWorkBudgetExhausted || rows[0].Reason.Params["limit_ms"] != 250 {
		t.Errorf("rejected row = %+v", rows[0])
	}
	if rows[1].Index != 1 || rows[1].Reason != nil ||
		!slices.Equal(rows[1].Formats, []FormatResult{{ID: "hd", Outcome: OutcomeMatched}}) {
		t.Errorf("completed row = %+v", rows[1])
	}
}

func TestBatchCancellationIsTerminalAndFillsTheRemainder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Release k reads the clock at batchHeadReads+5k+1 .. +5; cancel as the
	// third release creates its budget.
	third := batchHeadReads + 2*singleEvaluateReads + 1
	clk := newHookClock(0, func(call int) time.Duration {
		if call == third {
			cancel()
		}
		return 0
	})
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("hd", required("hdtv")))
	names := []string{hdtvName, hdtvName, hdtvName, hdtvName, hdtvName}
	rows, err := p.EvaluateBatch(ctx, names)
	wantReason(t, err, codeCancelled, nil)
	if len(rows) != len(names) {
		t.Fatalf("rows = %d, want one per admitted name (%d)", len(rows), len(names))
	}
	want := []ReleaseState{ReleaseCompleted, ReleaseCompleted, ReleaseUnscheduled, ReleaseUnscheduled, ReleaseUnscheduled}
	if !slices.Equal(batchStates(rows), want) {
		t.Fatalf("states = %v, want %v", batchStates(rows), want)
	}
	for i, r := range rows {
		if r.Index != i {
			t.Errorf("row %d has index %d", i, r.Index)
		}
		if r.State == ReleaseUnscheduled && (r.Formats != nil || r.Reason == nil || r.Reason.Code != codeCancelled) {
			t.Errorf("unscheduled row %d = %+v", i, r)
		}
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("lease state after a terminal batch = %d retired, %d readers", retired, readers)
	}
}

func TestDecisionDeadlineIsTerminalAndAheadOfTheReleaseBudget(t *testing.T) {
	names := []string{hdtvName, hdtvName, hdtvName, hdtvName}
	decision := map[string]int64{"limit_ms": batchDeadline.Milliseconds()}

	t.Run("deadline reached mid-batch", func(t *testing.T) {
		// The third release (reads 12..16) reaches the 30 s deadline exactly
		// before its match; the deadline outranks that release's own budget.
		clk := newHookClock(0, jumpAt(batchHeadReads+2*singleEvaluateReads+3, batchDeadline))
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("hd", required("hdtv")))
		rows, err := p.EvaluateBatch(context.Background(), names)
		wantReason(t, err, codeDeadlineExceeded, decision)
		want := []ReleaseState{ReleaseCompleted, ReleaseCompleted, ReleaseUnscheduled, ReleaseUnscheduled}
		if !slices.Equal(batchStates(rows), want) {
			t.Fatalf("states = %v, want %v", batchStates(rows), want)
		}
		for _, r := range rows[2:] {
			if r.Reason == nil || r.Reason.Code != codeDeadlineExceeded || r.Reason.Params["limit_ms"] != 30000 {
				t.Errorf("unscheduled reason = %+v", r.Reason)
			}
		}
		if rows[2].Reason == rows[3].Reason {
			t.Error("unscheduled rows must not share one Reason")
		}
	})

	t.Run("one nanosecond inside the deadline is only a release rejection", func(t *testing.T) {
		clk := newHookClock(0, jumpAt(batchHeadReads+4, batchDeadline-1))
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("hd", required("hdtv")))
		rows, err := p.EvaluateBatch(context.Background(), names)
		if err != nil {
			t.Fatalf("inside the deadline the batch continues: %v", err)
		}
		want := []ReleaseState{ReleaseRejected, ReleaseCompleted, ReleaseCompleted, ReleaseCompleted}
		if !slices.Equal(batchStates(rows), want) {
			t.Fatalf("states = %v, want %v", batchStates(rows), want)
		}
		if rows[0].Reason == nil || rows[0].Reason.Code != codeWorkBudgetExhausted {
			t.Errorf("rejected reason = %+v", rows[0].Reason)
		}
	})

	t.Run("an expired caller context stops before any release", func(t *testing.T) {
		expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer stop()
		p := newTestParser(t, newHookClock(time.Microsecond, nil).Now)
		compileFormats(t, p, 1, single("hd", required("hdtv")))
		rows, err := p.EvaluateBatch(expired, names)
		wantReason(t, err, codeDeadlineExceeded, nil)
		for i, r := range rows {
			if r.State != ReleaseUnscheduled || r.Index != i || r.Reason == nil || r.Reason.Code != codeDeadlineExceeded {
				t.Errorf("row %d = %+v", i, r)
			}
		}
		if len(rows) != len(names) {
			t.Errorf("rows = %d, want %d", len(rows), len(names))
		}
	})
}

// jumpsAt advances the clock by the listed duration at each listed read.
func jumpsAt(m map[int]time.Duration) func(int) time.Duration {
	return func(call int) time.Duration { return m[call] }
}

// The 250 ms budget is cumulative: each step below stays well inside it, but
// together they pass it. Reads: 1 creation, 2 opening check, then before and
// after each match, then the final check.
func TestBudgetIsCumulativeAcrossConditionsAndFormats(t *testing.T) {
	ctx := context.Background()
	conditions := []FormatSpec{single("both", required("hdtv"), required("720p"))}
	formats := []FormatSpec{single("a", required("hdtv")), single("b", required("720p"))}
	step := func(d time.Duration) time.Duration { return d } // each segment is under releaseBudget
	for _, tc := range []struct {
		name      string
		specs     []FormatSpec
		jumps     map[int]time.Duration
		wantReads int
		wantErr   bool
	}{
		// 120 ms after the first match, 120 ms after the second: 240 ms in all.
		{"two conditions below the limit", conditions, map[int]time.Duration{4: step(120 * time.Millisecond), 6: step(120 * time.Millisecond)}, 7, false},
		// 120 ms, then 140 ms: 260 ms in all, seen after the second match.
		{"two conditions over the limit", conditions, map[int]time.Duration{4: step(120 * time.Millisecond), 6: step(140 * time.Millisecond)}, 6, true},
		{"two formats below the limit", formats, map[int]time.Duration{4: step(120 * time.Millisecond), 5: step(120 * time.Millisecond)}, 7, false},
		// The second format is not scheduled once the total has passed 250 ms.
		{"two formats over the limit", formats, map[int]time.Duration{4: step(120 * time.Millisecond), 5: step(140 * time.Millisecond)}, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newHookClock(0, jumpsAt(tc.jumps))
			p := newTestParser(t, clk.Now)
			compileFormats(t, p, 1, tc.specs...)
			res, err := p.Evaluate(ctx, hdtvName)
			if clk.Calls() != tc.wantReads {
				t.Errorf("clock reads = %d, want %d", clk.Calls(), tc.wantReads)
			}
			if !tc.wantErr {
				if err != nil || len(res) != len(tc.specs) {
					t.Fatalf("below the limit: %+v, %v; want %d matches", res, err, len(tc.specs))
				}
				for _, r := range res {
					if r.Outcome != OutcomeMatched || r.Reason != nil {
						t.Errorf("result = %+v", r)
					}
				}
				return
			}
			wantReason(t, err, codeWorkBudgetExhausted, map[string]int64{"limit_ms": 250})
			if res != nil {
				t.Errorf("an exhausted cumulative budget must carry no results: %+v", res)
			}
		})
	}
}

// The 30 s deadline belongs to the batch: each release below stays inside its
// own 250 ms, but their sum reaches the shared deadline, which a deadline
// renewed per release would never do.
func TestDecisionDeadlineIsNotRenewedPerRelease(t *testing.T) {
	const perRelease = 200 * time.Millisecond
	reached := int(batchDeadline / perRelease) // release 149 is the first to end at 30 s
	names := make([]string, reached+3)
	for i := range names {
		names[i] = hdtvName
	}
	// Each release advances 200 ms at its after-match read.
	clk := newHookClock(0, func(call int) time.Duration {
		if call > batchHeadReads && (call-batchHeadReads)%singleEvaluateReads == 4 {
			return perRelease
		}
		return 0
	})
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("hd", required("hdtv")))
	rows, err := p.EvaluateBatch(context.Background(), names)
	wantReason(t, err, codeDeadlineExceeded, map[string]int64{"limit_ms": batchDeadline.Milliseconds()})
	if len(rows) != len(names) {
		t.Fatalf("rows = %d, want %d", len(rows), len(names))
	}
	for i, r := range rows {
		want := ReleaseCompleted
		if i >= reached-1 {
			want = ReleaseUnscheduled
		}
		if r.State != want || r.Index != i {
			t.Fatalf("row %d = %+v, want state %d", i, r, want)
		}
		if want == ReleaseUnscheduled && (r.Reason == nil || r.Reason.Code != codeDeadlineExceeded || r.Formats != nil) {
			t.Fatalf("unscheduled row %d = %+v", i, r)
		}
	}
}
