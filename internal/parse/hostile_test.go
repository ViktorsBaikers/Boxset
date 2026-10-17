package parse

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// evilPattern backtracks exponentially on evilName (a run of 'a' that fails
// at the end), far beyond the 50 ms match timeout.
const evilPattern = `^(a+)+$`

var evilName = strings.Repeat("a", 64) + "!"

// stackPattern exhausts regexp2's default backtracking stack on any name.
const stackPattern = `(?:^){60000}`

func TestHostileTimeoutIsNeverAMatch(t *testing.T) {
	timeout := map[string]int64{"limit_ms": 50}
	for _, tc := range []struct {
		name string
		cond ConditionSpec
	}{
		{"required", ConditionSpec{Pattern: evilPattern, Required: true}},
		{"optional", ConditionSpec{Pattern: evilPattern}},
		{"negated required", ConditionSpec{Pattern: evilPattern, Negate: true, Required: true}},
		{"negated optional", ConditionSpec{Pattern: evilPattern, Negate: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestParser(t, nil)
			compileFormats(t, p, 1, single("evil", tc.cond))
			res, err := p.Evaluate(context.Background(), evilName)
			wantReason(t, err, codeMatchTimeout, timeout)
			if res != nil {
				t.Errorf("a timeout must carry no results: %+v", res)
			}
			if retired, readers := leaseState(p); retired != 0 || readers != 0 {
				t.Errorf("lease state = %d retired, %d readers", retired, readers)
			}
		})
	}
}

func TestHostilePriorSuccessCannotBecomeAMatch(t *testing.T) {
	name := evilName + ".HDTV"
	p := newTestParser(t, nil)
	compileFormats(t, p, 1,
		// Fully matched before the hostile format is reached.
		single("done", required("hdtv")),
		// An optional condition already holds when the next one times out.
		single("mixed", ConditionSpec{Pattern: "hdtv"}, ConditionSpec{Pattern: evilPattern}),
	)
	res, err := p.Evaluate(context.Background(), name)
	wantReason(t, err, codeMatchTimeout, map[string]int64{"limit_ms": 50})
	if res != nil {
		t.Errorf("an earlier match must not survive a later error: %+v", res)
	}

	// The same early optional match inside one release of a batch: that
	// release is Rejected without formats, never a partial Completed row.
	rows, err := p.EvaluateBatch(context.Background(), []string{name})
	if err != nil || len(rows) != 1 || rows[0].State != ReleaseRejected || rows[0].Formats != nil ||
		rows[0].Reason == nil || rows[0].Reason.Code != codeMatchTimeout {
		t.Errorf("batch row = %+v, %v", rows, err)
	}
}

func TestHostileStackExhaustionIsNeverAMatch(t *testing.T) {
	for _, negate := range []bool{false, true} {
		p := newTestParser(t, nil)
		compileFormats(t, p, 1, single("stack", ConditionSpec{Pattern: stackPattern, Negate: negate, Required: true}))
		res, err := p.Evaluate(context.Background(), hdtvName)
		wantReason(t, err, codeRegexStackLimit, nil)
		if res != nil {
			t.Errorf("negate=%v: stack exhaustion must carry no results: %+v", negate, res)
		}
	}
}

func TestHostileValidControlsStillMatch(t *testing.T) {
	p := newTestParser(t, nil)
	compileFormats(t, p, 1, single("evil-shape", required(evilPattern)), single("hd", required("hdtv")))
	// The hostile-shaped pattern is fine on an input that does not trigger it.
	res, err := p.Evaluate(context.Background(), "aaaa")
	want := []FormatResult{{ID: "evil-shape", Outcome: OutcomeMatched}, {ID: "hd", Outcome: OutcomeNotMatched}}
	if err != nil || !slices.Equal(res, want) {
		t.Fatalf("control = %+v, %v; want %+v", res, err, want)
	}
}

func TestBatchHostileReleaseIsRejectedAndTheNextCompletes(t *testing.T) {
	p := newTestParser(t, nil)
	compileFormats(t, p, 1, single("evil-shape", required(evilPattern)))
	rows, err := p.EvaluateBatch(context.Background(), []string{evilName, "aaa!", "aa"})
	if err != nil {
		t.Fatalf("a release error must not fail the batch: %v", err)
	}
	if want := []ReleaseState{ReleaseRejected, ReleaseCompleted, ReleaseCompleted}; !slices.Equal(batchStates(rows), want) {
		t.Fatalf("states = %v, want %v", batchStates(rows), want)
	}
	if rows[0].Reason == nil || rows[0].Reason.Code != codeMatchTimeout || rows[0].Reason.Params["limit_ms"] != 50 || rows[0].Formats != nil {
		t.Errorf("rejected row = %+v", rows[0])
	}
	if !slices.Equal(rows[1].Formats, []FormatResult{{ID: "evil-shape", Outcome: OutcomeNotMatched}}) ||
		!slices.Equal(rows[2].Formats, []FormatResult{{ID: "evil-shape", Outcome: OutcomeMatched}}) {
		t.Errorf("completed rows = %+v, %+v", rows[1], rows[2])
	}
}

// A release whose last match fails while the batch is already over (the context
// was cancelled or the 30 s deadline passed during that match) is not a
// release-local rejection: the batch is terminal.
//
// The terminal state is injected deterministically through the clock seam: the
// before-match check reads the clock after its context check, so cancelling
// there lands after that check has passed and before the match; and the read
// right after a failed match belongs only to the post-error guard, so moving
// the clock there expires the deadline on the failing match alone. Row 0 has
// completed by then. The hostile match then really times out.
func TestBatchTerminalDuringFailingMatchWins(t *testing.T) {
	names := []string{"aaaa", evilName, "aaaa"}
	want := []ReleaseState{ReleaseCompleted, ReleaseUnscheduled, ReleaseUnscheduled}
	// Reads: 1 decision deadline, 2-6 row 0, then row 1: 7 creation, 8 opening
	// check, 9 before-match check.
	const beforeFailingMatch = batchHeadReads + singleEvaluateReads + 3

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clk := newHookClock(0, func(call int) time.Duration {
			if call == beforeFailingMatch {
				cancel()
			}
			return 0
		})
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("evil-shape", required(evilPattern)))
		rows, err := p.EvaluateBatch(ctx, names)
		wantReason(t, err, codeCancelled, nil)
		if !slices.Equal(batchStates(rows), want) {
			t.Fatalf("states = %v, want %v", batchStates(rows), want)
		}
		if !slices.Equal(rows[0].Formats, []FormatResult{{ID: "evil-shape", Outcome: OutcomeMatched}}) {
			t.Errorf("earlier row = %+v", rows[0])
		}
		for _, r := range rows[1:] {
			if r.Reason == nil || r.Reason.Code != codeCancelled || r.Formats != nil {
				t.Errorf("row %d = %+v", r.Index, r)
			}
		}
		if clk.Calls() != beforeFailingMatch {
			t.Errorf("clock reads = %d, want %d: the failing release must be the last one scheduled", clk.Calls(), beforeFailingMatch)
		}
	})

	t.Run("decision deadline", func(t *testing.T) {
		// The read after the failed match (10) is the post-error guard's.
		clk := newHookClock(0, jumpAt(beforeFailingMatch+1, batchDeadline))
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("evil-shape", required(evilPattern)))
		rows, err := p.EvaluateBatch(context.Background(), names)
		wantReason(t, err, codeDeadlineExceeded, map[string]int64{"limit_ms": batchDeadline.Milliseconds()})
		if !slices.Equal(batchStates(rows), want) {
			t.Fatalf("states = %v, want %v", batchStates(rows), want)
		}
		if clk.Calls() != beforeFailingMatch+1 {
			t.Errorf("clock reads = %d, want %d: the deadline must be found by the post-error guard", clk.Calls(), beforeFailingMatch+1)
		}
	})

	t.Run("single evaluation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Reads: 1 creation, 2 opening check, 3 before-match check.
		clk := newHookClock(0, func(call int) time.Duration {
			if call == 3 {
				cancel()
			}
			return 0
		})
		p := newTestParser(t, clk.Now)
		compileFormats(t, p, 1, single("evil-shape", required(evilPattern)))
		res, err := p.Evaluate(ctx, evilName)
		wantReason(t, err, codeCancelled, nil)
		if res != nil {
			t.Errorf("results = %+v", res)
		}
	})

	t.Run("empty batch with a dead context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := newTestParser(t, nil)
		compileFormats(t, p, 1, single("evil-shape", required(evilPattern)))
		rows, err := p.EvaluateBatch(ctx, nil)
		wantReason(t, err, codeCancelled, nil)
		if len(rows) != 0 {
			t.Errorf("rows = %+v", rows)
		}
	})
}

// A required condition that does not match must not hide a later condition's
// error: every condition is evaluated, so the evaluation fails closed.
func TestHostileFailedRequiredConditionDoesNotSkipALaterError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		input   string
		code    string
		params  map[string]int64
	}{
		{"stack limit", stackPattern, hdtvName, codeRegexStackLimit, nil},
		{"match timeout", evilPattern, evilName, codeMatchTimeout, map[string]int64{"limit_ms": 50}},
	} {
		for _, later := range []ConditionSpec{
			{Pattern: tc.pattern, Required: true},
			{Pattern: tc.pattern},
			{Pattern: tc.pattern, Negate: true},
		} {
			p := newTestParser(t, nil)
			// "bluray" is absent, so the format is already decided NotMatched.
			compileFormats(t, p, 1, single("f", required("bluray"), later))
			res, err := p.Evaluate(context.Background(), tc.input)
			wantReason(t, err, tc.code, tc.params)
			if res != nil {
				t.Errorf("%s %+v: results = %+v, want none", tc.name, later, res)
			}
		}
	}
}
