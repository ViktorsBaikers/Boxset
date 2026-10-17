package parse

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func numbered(n int) []FormatSpec {
	specs := make([]FormatSpec, n)
	for i := range specs {
		specs[i] = single(fmt.Sprintf("f%d", i), required("hdtv"))
	}
	return specs
}

func manyConditions(n int) FormatSpec {
	conds := make([]ConditionSpec, n)
	for i := range conds {
		conds[i] = required("x")
	}
	return FormatSpec{ID: "many", Conditions: conds}
}

// patternOfLen is a valid regex of exactly n bytes that matches "x".
func patternOfLen(n int) string {
	return "x(?#" + strings.Repeat("c", n-5) + ")"
}

// multibytePattern is a valid regex of exactly 8192 bytes (plus one for each
// extra ASCII byte) made mostly of two-byte runes; it matches "é".
func multibytePattern(extra int) string {
	return "é(?#" + strings.Repeat("é", 4093) + strings.Repeat("c", extra) + ")"
}

// decisiveLast has n required conditions: the first n-1 match "x" and the last
// needs "y", so it matches "xy" but not "x".
func decisiveLast(n int) FormatSpec {
	conds := make([]ConditionSpec, n)
	for i := range conds {
		conds[i] = required("x")
	}
	conds[n-1] = required("y")
	return FormatSpec{ID: "last", Conditions: conds}
}

func compileCount(p *Parser) int {
	p.formats.publish.Lock()
	defer p.formats.publish.Unlock()
	return p.formats.compiles
}

func TestFormatsCompileLimits(t *testing.T) {
	ctx := context.Background()
	newParser := func() *Parser { return newTestParser(t, nil) }

	t.Run("exact limits are admitted and still match", func(t *testing.T) {
		p := newParser()
		compileFormats(t, p, 1, numbered(maxFormats)...)
		res, err := p.Evaluate(ctx, hdtvName)
		if err != nil || len(res) != maxFormats {
			t.Fatalf("256 formats: %d results, %v", len(res), err)
		}
		for i, r := range res {
			if r.ID != fmt.Sprintf("f%d", i) || r.Outcome != OutcomeMatched {
				t.Fatalf("result %d = %+v", i, r)
			}
		}

		p = newParser()
		compileFormats(t, p, 1, manyConditions(maxConditions))
		if res, err := p.Evaluate(ctx, "x"); err != nil || len(res) != 1 || res[0].Outcome != OutcomeMatched {
			t.Fatalf("32 conditions: %+v, %v", res, err)
		}

		p = newParser()
		long := patternOfLen(maxPatternBytes)
		if len(long) != maxPatternBytes {
			t.Fatalf("control pattern is %d bytes", len(long))
		}
		compileFormats(t, p, 1, single("long", required(long)))
		if res, err := p.Evaluate(ctx, "x"); err != nil || len(res) != 1 || res[0].Outcome != OutcomeMatched {
			t.Fatalf("8192-byte pattern: %+v, %v", res, err)
		}

		mb := multibytePattern(0)
		if len(mb) != maxPatternBytes || utf8.RuneCountInString(mb) >= maxPatternBytes {
			t.Fatalf("multi-byte control is %d bytes, %d runes", len(mb), utf8.RuneCountInString(mb))
		}
		p = newParser()
		compileFormats(t, p, 1, single("mb", required(mb)))
		if res, err := p.Evaluate(ctx, "é"); err != nil || !slices.Equal(res, []FormatResult{{ID: "mb", Outcome: OutcomeMatched}}) {
			t.Fatalf("8192-byte multi-byte pattern: %+v, %v", res, err)
		}

		// Condition 32 alone decides the result: without it "x" matches.
		last := decisiveLast(maxConditions)
		p = newParser()
		compileFormats(t, p, 1, last)
		for input, want := range map[string]Outcome{"x": OutcomeNotMatched, "xy": OutcomeMatched} {
			if res, err := p.Evaluate(ctx, input); err != nil || !slices.Equal(res, []FormatResult{{ID: "last", Outcome: want}}) {
				t.Fatalf("32 conditions on %q: %+v, %v; want %v", input, res, err, want)
			}
		}
		p = newParser()
		compileFormats(t, p, 1, FormatSpec{ID: "last", Conditions: last.Conditions[:maxConditions-1]})
		if res, err := p.Evaluate(ctx, "x"); err != nil || !slices.Equal(res, []FormatResult{{ID: "last", Outcome: OutcomeMatched}}) {
			t.Fatalf("dropping condition 32 must change the result: %+v, %v", res, err)
		}

		// Every compiled condition carries the fixed 50 ms match timeout.
		p = newParser()
		compileFormats(t, p, 1, numbered(8)...)
		compileFormats(t, p, 2, decisiveLast(maxConditions))
		for _, f := range p.formats.current.formats {
			for j, c := range f.conditions {
				if c.re.MatchTimeout != 50*time.Millisecond {
					t.Fatalf("condition %d MatchTimeout = %v, want 50ms", j, c.re.MatchTimeout)
				}
			}
		}
		for _, id := range []string{strings.Repeat("a", maxFormatIDBytes), strings.Repeat("é", maxFormatIDBytes/2)} {
			p = newParser()
			compileFormats(t, p, 1, single(id, required("x")))
			if res, err := p.Evaluate(ctx, "x"); err != nil || len(res) != 1 || res[0].ID != id {
				t.Fatalf("64-byte ID %q: %+v, %v", id, res, err)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		specs  []FormatSpec
		code   string
		params map[string]int64
	}{
		{"257 formats", numbered(maxFormats + 1), codeTooManyFormats, map[string]int64{"limit": 256, "observed": 257}},
		{"33 conditions", []FormatSpec{numbered(1)[0], manyConditions(maxConditions + 1)}, codeTooManyConditions,
			map[string]int64{"format_index": 1, "limit": 32, "observed": 33}},
		{"8193-byte pattern", []FormatSpec{single("a", required("x"), required(patternOfLen(maxPatternBytes+1)))}, codeRegexSourceTooLong,
			map[string]int64{"format_index": 0, "condition_index": 1, "limit_bytes": 8192, "observed_bytes": 8193}},
		// 4097 runes but 8193 bytes: the limit counts bytes, not runes.
		{"8193-byte multi-byte pattern", []FormatSpec{single("a", required(multibytePattern(1)))}, codeRegexSourceTooLong,
			map[string]int64{"format_index": 0, "condition_index": 0, "limit_bytes": 8192, "observed_bytes": 8193}},
		{"65-byte ID", []FormatSpec{single(strings.Repeat("a", maxFormatIDBytes+1), required("x"))}, codeFormatIDTooLong,
			map[string]int64{"format_index": 0, "limit_bytes": 64, "observed_bytes": 65}},
		{"65-byte multi-byte ID", []FormatSpec{single(strings.Repeat("é", 32)+"a", required("x"))}, codeFormatIDTooLong,
			map[string]int64{"format_index": 0, "limit_bytes": 64, "observed_bytes": 65}},
		{"invalid UTF-8 ID", []FormatSpec{numbered(1)[0], single("id\xff", required("x"))}, codeFormatIDInvalidUTF8,
			map[string]int64{"format_index": 1, "offset": 2}},
		{"invalid UTF-8 pattern", []FormatSpec{single("a", required("x"), required("ab\xffc"))}, codeRegexCompileFailed,
			map[string]int64{"format_index": 0, "condition_index": 1}},
		// Format 0 holds a pattern that cannot compile; the limit breach in
		// format 1 is reported instead because validation precedes compilation.
		{"limits are checked before any pattern compiles", []FormatSpec{single("bad", required("(")), manyConditions(maxConditions + 1)},
			codeTooManyConditions, map[string]int64{"format_index": 1, "limit": 32, "observed": 33}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParser()
			err := p.CompileFormats(ctx, 1, tc.specs)
			wantReason(t, err, tc.code, tc.params)
			if _, err := p.Evaluate(ctx, "x"); err == nil {
				t.Error("a rejected profile must not become current")
			} else {
				wantReason(t, err, codeFormatNotConfigured, nil)
			}
		})
	}
}

func TestFormatsMatchSemantics(t *testing.T) {
	ctx := context.Background()
	opt := func(p string) ConditionSpec { return ConditionSpec{Pattern: p} }
	neg := func(c ConditionSpec) ConditionSpec { c.Negate = true; return c }

	for _, tc := range []struct {
		name  string
		conds []ConditionSpec
		input string
		want  Outcome
	}{
		{"all required hold", []ConditionSpec{required("hdtv"), required("720p")}, hdtvName, OutcomeMatched},
		{"one required fails", []ConditionSpec{required("hdtv"), required("1080p")}, hdtvName, OutcomeNotMatched},
		{"one optional of two holds", []ConditionSpec{opt("bluray"), opt("720p")}, hdtvName, OutcomeMatched},
		{"no optional holds", []ConditionSpec{opt("bluray"), opt("1080p")}, hdtvName, OutcomeNotMatched},
		{"required holds but no optional does", []ConditionSpec{required("hdtv"), opt("bluray")}, hdtvName, OutcomeNotMatched},
		{"required and an optional hold", []ConditionSpec{required("hdtv"), opt("bluray"), opt("720p")}, hdtvName, OutcomeMatched},
		{"negated required absent", []ConditionSpec{neg(required("bluray"))}, hdtvName, OutcomeMatched},
		{"negated required present", []ConditionSpec{neg(required("hdtv"))}, hdtvName, OutcomeNotMatched},
		{"negated optional present", []ConditionSpec{neg(opt("hdtv"))}, hdtvName, OutcomeNotMatched},
		{"no conditions match nothing", nil, hdtvName, OutcomeNotMatched},
		{"patterns ignore case", []ConditionSpec{required("hdtv")}, "SHOW.HDTV", OutcomeMatched},
		{"inline option turns case folding off", []ConditionSpec{required("(?-i)hdtv")}, "SHOW.HDTV", OutcomeNotMatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestParser(t, nil)
			compileFormats(t, p, 1, FormatSpec{ID: "fmt", Conditions: tc.conds})
			res, err := p.Evaluate(ctx, tc.input)
			if err != nil || !slices.Equal(res, []FormatResult{{ID: "fmt", Outcome: tc.want}}) {
				t.Fatalf("Evaluate = %+v, %v; want %v", res, err, tc.want)
			}
		})
	}

	p := newTestParser(t, nil)
	compileFormats(t, p, 1, single("b", required("720p")), single("a", required("bluray")), single("c", required("hdtv")))
	res, err := p.Evaluate(ctx, hdtvName)
	want := []FormatResult{{ID: "b", Outcome: OutcomeMatched}, {ID: "a", Outcome: OutcomeNotMatched}, {ID: "c", Outcome: OutcomeMatched}}
	if err != nil || !slices.Equal(res, want) {
		t.Fatalf("profile order: %+v, %v; want %+v", res, err, want)
	}
}

func TestFormatsNameBounds(t *testing.T) {
	ctx := context.Background()
	p := newTestParser(t, nil)
	compileFormats(t, p, 1, single("a", required("x")))
	exact := padTo("x", maxNameBytes)
	if len(exact) != maxNameBytes {
		t.Fatalf("control is %d bytes", len(exact))
	}
	res, err := p.Evaluate(ctx, exact)
	if err != nil || !slices.Equal(res, []FormatResult{{ID: "a", Outcome: OutcomeMatched}}) {
		t.Fatalf("4096-byte name: %+v, %v; want the exact match of format a with no reason", res, err)
	}
	if res[0].Reason != nil {
		t.Errorf("a matched format carries no reason: %+v", res[0].Reason)
	}
	multi := strings.Repeat("é", maxNameBytes/2-1) + "xa" // 4096 bytes
	if len(multi) != maxNameBytes {
		t.Fatalf("multi-byte control is %d bytes", len(multi))
	}
	if res, err := p.Evaluate(ctx, multi); err != nil || !slices.Equal(res, []FormatResult{{ID: "a", Outcome: OutcomeMatched}}) {
		t.Fatalf("4096-byte multi-byte name: %+v, %v", res, err)
	}
	res, err = p.Evaluate(ctx, padTo("x", maxNameBytes+1))
	wantReason(t, err, codeReleaseNameTooLong, map[string]int64{"limit_bytes": 4096, "observed_bytes": 4097})
	if res != nil {
		t.Errorf("results = %+v", res)
	}
	_, err = p.Evaluate(ctx, "a\xffb")
	wantReason(t, err, codeReleaseNameInvalidUTF8, map[string]int64{"offset": 1})
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("lease state = %d retired, %d readers", retired, readers)
	}
}

func TestFormatsErrorsAreStructuredAndSafe(t *testing.T) {
	p := newTestParser(t, nil)
	specs := []FormatSpec{
		single("a", required("x")),
		single("b", required("y")),
		single("secret-id", required("fine"), required("(secret-pattern")),
	}
	err := p.CompileFormats(context.Background(), 1, specs)
	wrapped := fmt.Errorf("configure profile: %w", err)
	var re *ReasonError
	if !errors.As(wrapped, &re) {
		t.Fatalf("errors.As found no *ReasonError in %v", wrapped)
	}
	if re.Reason.Code != codeRegexCompileFailed ||
		re.Reason.Params["format_index"] != 2 || re.Reason.Params["condition_index"] != 1 || len(re.Reason.Params) != 2 {
		t.Errorf("reason = %+v", re.Reason)
	}
	for _, leak := range []string{"secret", "fine", "regexp2", "("} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error text %q leaks %q", err.Error(), leak)
		}
	}
}

func TestRevisionPublication(t *testing.T) {
	ctx := context.Background()
	p := newTestParser(t, nil)

	if _, err := p.Evaluate(ctx, hdtvName); err == nil {
		t.Fatal("no profile configured must fail")
	} else {
		wantReason(t, err, codeFormatNotConfigured, nil)
	}
	if rows, err := p.EvaluateBatch(ctx, []string{hdtvName}); rows != nil {
		t.Fatalf("batch without a profile returned rows %+v", rows)
	} else {
		wantReason(t, err, codeFormatNotConfigured, nil)
	}

	compileFormats(t, p, 5, single("old", required("hdtv")))
	first := p.formats.current

	// Same revision, same content: reused. The set is not replaced and, counted
	// at the compiler, no pattern is compiled again.
	compiled := compileCount(p)
	if compiled != 1 {
		t.Fatalf("first revision compiled %d patterns, want 1", compiled)
	}
	firstRe := first.formats[0].conditions[0].re
	compileFormats(t, p, 5, single("old", required("hdtv")))
	if p.formats.current != first || p.formats.current.formats[0].conditions[0].re != firstRe {
		t.Error("same revision and content must reuse the compiled set")
	}
	if got := compileCount(p); got != compiled {
		t.Errorf("same revision and content compiled %d more patterns", got-compiled)
	}

	// Same revision, different content / lower revision: rejected, old stays.
	wantReason(t, p.CompileFormats(ctx, 5, []FormatSpec{single("old", required("1080p"))}), codeFormatRevisionConflict, nil)
	wantReason(t, p.CompileFormats(ctx, 4, []FormatSpec{single("old", required("hdtv"))}), codeFormatRevisionStale, nil)
	// A higher revision that fails to compile leaves the accepted one current.
	wantReason(t, p.CompileFormats(ctx, 6, []FormatSpec{single("new", required("("))}), codeRegexCompileFailed,
		map[string]int64{"format_index": 0, "condition_index": 0})
	// A cancelled compile publishes nothing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	wantReason(t, p.CompileFormats(cancelled, 7, []FormatSpec{single("new", required("x"))}), codeCancelled, nil)

	if p.formats.current != first {
		t.Fatal("a failed configuration must preserve the previous accepted set")
	}
	res, err := p.Evaluate(ctx, hdtvName)
	if err != nil || !slices.Equal(res, []FormatResult{{ID: "old", Outcome: OutcomeMatched}}) {
		t.Fatalf("after failed configurations: %+v, %v", res, err)
	}

	// The caller's slices are not retained: mutating them changes nothing.
	specs := []FormatSpec{{ID: "mut", Conditions: []ConditionSpec{required("hdtv")}}}
	compileFormats(t, p, 8, specs...)
	specs[0].ID = "changed"
	specs[0].Conditions[0].Pattern = "bluray"
	res, err = p.Evaluate(ctx, hdtvName)
	if err != nil || !slices.Equal(res, []FormatResult{{ID: "mut", Outcome: OutcomeMatched}}) {
		t.Fatalf("after caller mutation: %+v, %v", res, err)
	}
	compiled = compileCount(p)
	compileFormats(t, p, 8, single("mut", required("hdtv")))
	if p.formats.current.revision != 8 {
		t.Error("content comparison must use the retained copy, not the caller's slice")
	}
	if got := compileCount(p); got != compiled {
		t.Errorf("recompiling the retained content compiled %d more patterns", got-compiled)
	}
}

type evalOutcome struct {
	res []FormatResult
	err error
}

func goEvaluate(p *Parser, ctx context.Context, name string) <-chan evalOutcome {
	ch := make(chan evalOutcome, 1)
	go func() {
		res, err := p.Evaluate(ctx, name)
		ch <- evalOutcome{res, err}
	}()
	return ch
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

// gatedClock blocks every clock read until release, so an evaluation that has
// been admitted (it holds its lease and pinned set) stays in flight.
func gatedClock(t *testing.T) (clock *hookClock, entered <-chan struct{}, release func()) {
	t.Helper()
	gate := make(chan struct{})
	in := make(chan struct{}, 64)
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	clock = newHookClock(0, func(int) time.Duration {
		select {
		case in <- struct{}{}:
		default:
		}
		<-gate
		return 0
	})
	return clock, in, release
}

func TestRevisionHeldEvaluationsBoundRetiredPayloads(t *testing.T) {
	ctx := context.Background()
	clk, entered, release := gatedClock(t)
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("r1", required("hdtv")))

	a := goEvaluate(p, ctx, hdtvName)
	receive(t, entered)
	compileFormats(t, p, 2, single("r2", required("hdtv")))
	b := goEvaluate(p, ctx, hdtvName) // admitted after publication: pins r2
	receive(t, entered)
	compileFormats(t, p, 3, single("r3", required("hdtv")))

	// Two readers hold r1 and r2; publishing many more idle revisions cannot
	// grow what is retained, and no third operation is admitted.
	for rev := uint64(4); rev <= 40; rev++ {
		compileFormats(t, p, rev, single(fmt.Sprintf("r%d", rev), required("hdtv")))
	}
	if retired, readers := leaseState(p); retired != 2 || readers != 2 {
		t.Fatalf("held lease state = %d retired, %d readers, want 2 and 2", retired, readers)
	}
	_, err := p.Evaluate(ctx, hdtvName)
	wantReason(t, err, codeEvaluatorBusy, map[string]int64{"limit": 2})
	rows, err := p.EvaluateBatch(ctx, []string{hdtvName})
	wantReason(t, err, codeEvaluatorBusy, map[string]int64{"limit": 2})
	if rows != nil {
		t.Errorf("a busy batch must return no rows: %+v", rows)
	}

	release()
	if got := receive(t, a); got.err != nil || len(got.res) != 1 || got.res[0].ID != "r1" {
		t.Errorf("first evaluation = %+v, want the r1 it pinned", got)
	}
	if got := receive(t, b); got.err != nil || len(got.res) != 1 || got.res[0].ID != "r2" {
		t.Errorf("second evaluation = %+v, want the r2 it pinned", got)
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("after the last reader = %d retired, %d readers, want none", retired, readers)
	}
	if res, err := p.Evaluate(ctx, hdtvName); err != nil || res[0].ID != "r40" {
		t.Errorf("current revision after publication = %+v, %v", res, err)
	}
}

func TestRevisionCancellationReleasesTheLease(t *testing.T) {
	clk, entered, release := gatedClock(t)
	p := newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("r1", required("hdtv")))

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	a := goEvaluate(p, ctxA, hdtvName)
	b := goEvaluate(p, ctxB, hdtvName)
	receive(t, entered)
	receive(t, entered)
	compileFormats(t, p, 2, single("r2", required("hdtv")))
	if retired, readers := leaseState(p); retired != 1 || readers != 2 {
		t.Fatalf("held lease state = %d retired, %d readers, want 1 and 2", retired, readers)
	}

	cancelA()
	cancelB()
	release()
	for _, ch := range []<-chan evalOutcome{a, b} {
		got := receive(t, ch)
		wantReason(t, got.err, codeCancelled, nil)
		if got.res != nil {
			t.Errorf("cancelled evaluation returned %+v", got.res)
		}
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("after cancellation = %d retired, %d readers, want none", retired, readers)
	}
	if res, err := p.Evaluate(context.Background(), hdtvName); err != nil || res[0].ID != "r2" {
		t.Errorf("evaluation after cancellation = %+v, %v", res, err)
	}
}

func TestRevisionConcurrentPublicationIsConsistent(t *testing.T) {
	ctx := context.Background()
	p := newTestParser(t, nil)
	profile := func(rev uint64) []FormatSpec {
		return []FormatSpec{
			single(fmt.Sprintf("r%d-a", rev), required("hdtv")),
			single(fmt.Sprintf("r%d-b", rev), required("720p")),
		}
	}
	compileFormats(t, p, 1, profile(1)...)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		problems  []string
	)
	wg.Go(func() {
		for rev := uint64(2); rev <= 60; rev++ {
			if err := p.CompileFormats(ctx, rev, profile(rev)); err != nil {
				mu.Lock()
				problems = append(problems, "compile: "+err.Error())
				mu.Unlock()
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 150 {
				res, err := p.Evaluate(ctx, hdtvName)
				mu.Lock()
				switch {
				case err != nil:
					var re *ReasonError
					if !errors.As(err, &re) || re.Reason.Code != codeEvaluatorBusy {
						problems = append(problems, "evaluate: "+err.Error())
					}
				case len(res) != 2 || res[0].ID[:strings.IndexByte(res[0].ID, '-')] != res[1].ID[:strings.IndexByte(res[1].ID, '-')]:
					problems = append(problems, fmt.Sprintf("mixed revisions: %+v", res))
				default:
					successes++
				}
				mu.Unlock()
				runtime.Gosched()
			}
		})
	}
	wg.Wait()
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems[:min(len(problems), 5)])
	}
	if successes == 0 {
		t.Error("no evaluation was admitted")
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("lease state after the race = %d retired, %d readers", retired, readers)
	}
	if p.formats.current.revision != 60 {
		t.Errorf("final revision = %d, want 60", p.formats.current.revision)
	}
}

func TestBatchBounds(t *testing.T) {
	ctx := context.Background()
	p := newTestParser(t, nil)
	compileFormats(t, p, 1, single("z", required("zzz")))

	t.Run("1024 empty names complete, one row each in order", func(t *testing.T) {
		rows, err := p.EvaluateBatch(ctx, make([]string, maxBatchReleases))
		if err != nil || len(rows) != maxBatchReleases {
			t.Fatalf("rows = %d, err = %v", len(rows), err)
		}
		for i, r := range rows {
			if r.Index != i || r.State != ReleaseCompleted || r.Reason != nil ||
				!slices.Equal(r.Formats, []FormatResult{{ID: "z", Outcome: OutcomeNotMatched}}) {
				t.Fatalf("row %d = %+v", i, r)
			}
		}
	})

	t.Run("1025 empty names are rejected before the output is allocated", func(t *testing.T) {
		names := make([]string, maxBatchReleases+1)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		rows, err := p.EvaluateBatch(ctx, names)
		runtime.ReadMemStats(&after)
		wantReason(t, err, codeTooManyReleases, map[string]int64{"limit": 1024, "observed": 1025})
		if rows != nil {
			t.Errorf("rows = %d", len(rows))
		}
		// One admitted row set alone is 1024 x sizeof(ReleaseResult) = 48 KiB.
		if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<10 {
			t.Errorf("rejected batch allocated %d bytes", grown)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		exact := make([]string, maxBatchReleases)
		for i := range exact {
			exact[i] = strings.Repeat("a", maxNameBytes)
		}
		rows, err := p.EvaluateBatch(ctx, exact)
		if err != nil || len(rows) != maxBatchReleases {
			t.Fatalf("exactly 4 MiB: rows = %d, err = %v", len(rows), err)
		}
		over := slices.Clone(exact)
		for i := range over {
			over[i] += "a"
		}
		rows, err = p.EvaluateBatch(ctx, over)
		wantReason(t, err, codeBatchNameBytes, map[string]int64{"limit_bytes": 4 << 20, "observed_bytes": 1024 * 4097})
		if rows != nil {
			t.Errorf("rows = %d", len(rows))
		}
	})

	t.Run("each name carries its index", func(t *testing.T) {
		rows, err := p.EvaluateBatch(ctx, []string{"a", "b", padTo("x", maxNameBytes+1)})
		wantReason(t, err, codeReleaseNameTooLong, map[string]int64{"limit_bytes": 4096, "observed_bytes": 4097, "index": 2})
		if rows != nil {
			t.Errorf("rows = %+v", rows)
		}
		_, err = p.EvaluateBatch(ctx, []string{"a", "b\xff"})
		wantReason(t, err, codeReleaseNameInvalidUTF8, map[string]int64{"offset": 1, "index": 1})
	})

	t.Run("validation precedes configuration and no names is an empty success", func(t *testing.T) {
		fresh := newTestParser(t, nil)
		_, err := fresh.EvaluateBatch(ctx, make([]string, maxBatchReleases+1))
		wantReason(t, err, codeTooManyReleases, map[string]int64{"limit": 1024, "observed": 1025})
		_, err = fresh.EvaluateBatch(ctx, []string{"a"})
		wantReason(t, err, codeFormatNotConfigured, nil)
		rows, err := p.EvaluateBatch(ctx, nil)
		if err != nil || len(rows) != 0 {
			t.Errorf("no names: %+v, %v", rows, err)
		}
	})
}

func TestBatchPinsOneRevisionAcrossPublication(t *testing.T) {
	var p *Parser
	// Publish a new revision while the second release is in flight.
	clk := newHookClock(0, func(call int) time.Duration {
		if call == batchHeadReads+singleEvaluateReads+2 {
			if err := p.CompileFormats(context.Background(), 2, []FormatSpec{single("r2", required("hdtv"))}); err != nil {
				t.Errorf("publish: %v", err)
			}
		}
		return 0
	})
	p = newTestParser(t, clk.Now)
	compileFormats(t, p, 1, single("r1", required("hdtv")))

	rows, err := p.EvaluateBatch(context.Background(), []string{hdtvName, hdtvName, hdtvName})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %d, err = %v", len(rows), err)
	}
	for i, r := range rows {
		if r.State != ReleaseCompleted || len(r.Formats) != 1 || r.Formats[0].ID != "r1" {
			t.Errorf("row %d = %+v, want the r1 pinned at entry", i, r)
		}
	}
	if p.formats.current.revision != 2 {
		t.Fatal("the revision was not published during the batch")
	}
	if res, err := p.Evaluate(context.Background(), hdtvName); err != nil || res[0].ID != "r2" {
		t.Errorf("evaluation after the batch = %+v, %v", res, err)
	}
	if retired, readers := leaseState(p); retired != 0 || readers != 0 {
		t.Errorf("lease state = %d retired, %d readers", retired, readers)
	}
}

func TestBatchEmptyProfileCompletesWithNoFormats(t *testing.T) {
	p := newTestParser(t, nil)
	compileFormats(t, p, 1)
	rows, err := p.EvaluateBatch(context.Background(), []string{"a", "b"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d, err = %v", len(rows), err)
	}
	for i, r := range rows {
		if r.Index != i || r.State != ReleaseCompleted || len(r.Formats) != 0 || r.Reason != nil {
			t.Errorf("row %d = %+v", i, r)
		}
	}
}
