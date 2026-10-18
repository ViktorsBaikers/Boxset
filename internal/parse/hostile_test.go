package parse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlclark/regexp2/v2"
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

// ---- movie title boundaries -------------------------------------------------------------

// movieName is a legitimate release name that parses to a known movie.
const movieName = "Movie.Name.2015.German.DL.1080p.BluRay.x264-GRP"

func TestMovieBoundsFailClosed(t *testing.T) {
	p := newTestParser(t, nil)
	ctx := context.Background()

	// The 4096-byte control that must still parse is TestMovieBoundsLegitimate4096ByteNameParses; it
	// runs on the real clock and so is excluded under -race with the complete corpus run.
	for _, tc := range []struct {
		name   string
		input  string
		kind   Kind
		code   string
		params map[string]int64
	}{
		{"4097 bytes", padTo(movieName, 4097), KindMovie, codeReleaseNameTooLong, map[string]int64{"limit_bytes": 4096, "observed_bytes": 4097}},
		{"invalid UTF-8", "Movie.2015.720p\xff-GRP", KindMovie, codeReleaseNameInvalidUTF8, map[string]int64{"offset": 15}},
		{"unknown kind", movieName, Kind(9), codeKindUnsupported, map[string]int64{"kind": 9}},
		{"nothing to parse", "----", KindMovie, codeTitleUnrecognized, map[string]int64{}},
		{"hashed release", "0e895c37245186812cb08aab1529cf8ee389dd05.mkv", KindMovie, codeTitleUnrecognized, map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseTitle(ctx, tc.input, tc.kind)
			wantReason(t, err, tc.code, tc.params)
			if got.Movie != nil || got.Kind != 0 {
				t.Errorf("a refused name must carry no usable value: %+v", got)
			}
		})
	}
}

// A hostile match in any parser ParseTitle runs (the title patterns, the quality,
// language and release group parsers) is a match_timeout reason, never a result.
func TestHostileMovieTitleMatchInAnySubparserFailsClosed(t *testing.T) {
	evil := func(t *testing.T) *regexp2.Regexp {
		t.Helper()
		re, err := regexp2.Compile(`(a+)+$`, ic)
		if err != nil {
			t.Fatal(err)
		}
		re.MatchTimeout = matchTimeout
		return re
	}
	name := "Movie.Name.2015.720p." + strings.Repeat("a", 64) + "!.x264-GRP"
	for _, tc := range []struct {
		stage   string
		inject  func(p *Parser, re *regexp2.Regexp)
		wantErr string
	}{
		{"title simple release", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.simpleRelease = re }, codeMatchTimeout},
		{"title hardcoded subs", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.hardcodedSubs = re }, codeMatchTimeout},
		{"edition", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.reportEdition = re }, codeMatchTimeout},
		{"quality", func(p *Parser, re *regexp2.Regexp) { p.movie.source = re }, codeMatchTimeout},
		{"language", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.lang.insensitive = re }, codeMatchTimeout},
		{"language case-sensitive", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.lang.caseSensitive = re }, codeMatchTimeout},
		{"release group", func(p *Parser, re *regexp2.Regexp) { p.movieTitle.group.group = re }, codeMatchTimeout},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			p := newTestParser(t, nil)
			// The control: the same name parses with the real tables.
			if _, err := p.ParseTitle(context.Background(), name, KindMovie); err != nil {
				t.Fatalf("control: %v", err)
			}
			tc.inject(p, evil(t))
			got, err := p.ParseTitle(context.Background(), name, KindMovie)
			wantReason(t, err, tc.wantErr, map[string]int64{"limit_ms": 50})
			if got.Movie != nil || got.Kind != 0 {
				t.Errorf("a timeout must carry no usable value: %+v", got)
			}
		})
	}
}

// blipClock reads a constant time, except that its read number at returns a time
// past the release budget (and only that read does). Exactly that read is then the
// one that finds the budget exhausted, whichever budget it belongs to.
type blipClock struct {
	mu    sync.Mutex
	base  time.Time
	at    int
	calls int
}

func (c *blipClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == c.at {
		return c.base.Add(releaseBudget)
	}
	return c.base
}

func (c *blipClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// One release budget covers every suboperation. A full ParseTitle is run once to
// count its clock reads, then once more for every read k from the first check
// through completion, with the budget found exhausted at read k and no other.
// Every such parse must stop with the budget reason, no value, and exactly k reads
// (no work after the exhaustion). A stage that started a budget of its own reads
// the clock to create it: at that read the blip makes the new deadline a whole
// budget later, so nothing expires and the parse finishes instead of failing.
// Read 1 creates the one legitimate budget (its time defines the deadline).
func TestBudgetMovieTitleIsSharedByEverySubparser(t *testing.T) {
	ctx := context.Background()
	run := func(at int) (*blipClock, ParsedTitle, error) {
		clk := &blipClock{base: time.Unix(1000, 0), at: at}
		pt, err := newTestParser(t, clk.Now).ParseTitle(ctx, movieName, KindMovie)
		return clk, pt, err
	}
	probe, _, err := run(0)
	if err != nil {
		t.Fatalf("ParseTitle: %v", err)
	}
	reads := probe.Calls()
	quality := &blipClock{base: time.Unix(1000, 0)}
	if _, err := newTestParser(t, quality.Now).ParseQuality(ctx, movieName, KindMovie); err != nil {
		t.Fatalf("ParseQuality: %v", err)
	}
	if reads <= 2*quality.Calls() {
		t.Fatalf("ParseTitle read the clock %d times, ParseQuality %d: the title, language and group work must add to the quality work", reads, quality.Calls())
	}

	failures := 0
	for k := 2; k <= reads; k++ {
		clk, got, err := run(k)
		var re *ReasonError
		switch {
		case !errors.As(err, &re) || re.Reason.Code != codeWorkBudgetExhausted:
			t.Errorf("budget exhausted at read %d of %d: err = %v, want %s (a stage with a budget of its own?)", k, reads, err, codeWorkBudgetExhausted)
		case got.Movie != nil || got.Kind != 0:
			t.Errorf("budget exhausted at read %d: no usable value expected, got %+v", k, got)
		case clk.Calls() != k:
			t.Errorf("budget exhausted at read %d: the clock was read %d times (work after the exhaustion)", k, clk.Calls())
		default:
			continue
		}
		if failures++; failures == 5 {
			t.Fatal("more than 5 reads fail; stopping")
		}
	}
}

func TestBudgetMovieTitleContextCancellationAndDeadlineFailClosed(t *testing.T) {
	p := newTestParser(t, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.ParseTitle(cancelled, movieName, KindMovie)
	wantReason(t, err, codeCancelled, map[string]int64{})
	if got.Movie != nil {
		t.Errorf("a cancelled parse must carry no usable value: %+v", got)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	got, err = p.ParseTitle(expired, movieName, KindMovie)
	wantReason(t, err, codeDeadlineExceeded, map[string]int64{})
	if got.Movie != nil {
		t.Errorf("an expired parse must carry no usable value: %+v", got)
	}
}

// A name that only the public folder entry reads: the year-first folder pattern.
func TestMovieTitleDirectoryFormIsTheOnlyReaderOfAYearFirstName(t *testing.T) {
	p := newTestParser(t, nil)
	ctx := context.Background()
	pt, err := p.ParseDirTitle(ctx, "(1995) Movie Name", KindMovie)
	if err != nil || pt.Movie == nil || pt.Movie.MovieTitles[0] != "Movie Name" || pt.Movie.Year != 1995 {
		t.Errorf("directory form = %+v, %v", pt, err)
	}
	_, err = p.ParseTitle(ctx, "(1995) Movie Name", KindMovie)
	wantReason(t, err, codeTitleUnrecognized, map[string]int64{})
}

// The folder entry runs the same single release budget: a clock that expires on
// the last read of a year-first folder parse leaves no usable title.
func TestMovieTitleDirectoryFormExpiryFailsClosed(t *testing.T) {
	const dir = "(1995) Movie Name"
	ctx := context.Background()
	run := func(at int) (*blipClock, ParsedTitle, error) {
		clk := &blipClock{base: time.Unix(1000, 0), at: at}
		pt, err := newTestParser(t, clk.Now).ParseDirTitle(ctx, dir, KindMovie)
		return clk, pt, err
	}
	probe, _, err := run(0)
	if err != nil {
		t.Fatalf("ParseDirTitle: %v", err)
	}
	reads := probe.Calls()
	clk, got, err := run(reads)
	var re *ReasonError
	if !errors.As(err, &re) || (re.Reason.Code != codeWorkBudgetExhausted && re.Reason.Code != codeDeadlineExceeded) {
		t.Fatalf("expired at read %d of %d: err = %v, want %s or %s", reads, reads, err, codeWorkBudgetExhausted, codeDeadlineExceeded)
	}
	if got != (ParsedTitle{}) {
		t.Errorf("an expired folder parse must carry no usable value: %+v", got)
	}
	if clk.Calls() != reads {
		t.Errorf("clock reads = %d, want %d", clk.Calls(), reads)
	}
}

// The public clean methods are bounded like every other entry: an over-long or
// invalid UTF-8 name is rejected before the blank fast path.
func TestCleanTitleBoundsFailClosed(t *testing.T) {
	p := newTestParser(t, nil)
	for name, clean := range map[string]func(string) (string, error){
		"movie":  p.CleanMovieTitle,
		"series": p.CleanSeriesTitle,
	} {
		t.Run(name, func(t *testing.T) {
			atLimit := strings.Repeat(" ", maxNameBytes)
			if got, err := clean(atLimit); err != nil || got != atLimit {
				t.Errorf("%d spaces = %d bytes, %v; want unchanged, nil", maxNameBytes, len(got), err)
			}
			got, err := clean(strings.Repeat(" ", maxNameBytes+1))
			wantReason(t, err, codeReleaseNameTooLong, map[string]int64{"limit_bytes": maxNameBytes, "observed_bytes": maxNameBytes + 1})
			if got != "" {
				t.Errorf("over-long name returned %d bytes", len(got))
			}
			got, err = clean("bad\xffname")
			wantReason(t, err, codeReleaseNameInvalidUTF8, map[string]int64{"offset": 3})
			if got != "" {
				t.Errorf("invalid UTF-8 name returned %q", got)
			}
		})
	}
}

// An unhandled letter is a structured reason. The error text does not contain the name.
func TestCleanUnhandledAccentDoesNotEchoTheName(t *testing.T) {
	p := newTestParser(t, nil)
	for name, clean := range map[string]func(string) (string, error){
		"movie":  p.CleanMovieTitle,
		"series": p.CleanSeriesTitle,
	} {
		t.Run(name, func(t *testing.T) {
			const in = "Bjørn"
			got, err := clean(in)
			wantReason(t, err, codeAccentUnhandled, map[string]int64{"codepoint": 248})
			if got != "" {
				t.Errorf("returned %q", got)
			}
			if strings.Contains(err.Error(), in) || strings.Contains(err.Error(), "ø") {
				t.Errorf("error echoes the name: %s", err)
			}
		})
	}
}

// Generated names (a SHA-256 chain and random alphanumeric hashes) have
// no fixture input, so they are run here: no such name is a title.
func TestHostileMovieTitleRejectsGeneratedHashNames(t *testing.T) {
	p := newTestParser(t, nil)
	ctx := context.Background()
	check := func(name string) {
		t.Helper()
		if got, err := p.ParseTitle(ctx, name, KindMovie); err == nil || got.Movie != nil {
			t.Fatalf("%q parsed as %+v", name, got)
		}
	}
	hash := "CRAPPY TEST SEED"
	for range 100 {
		sum := sha256.Sum256([]byte(hash))
		hash = strings.ToUpper(hex.EncodeToString(sum[:]))
		check(hash)
	}
	const charset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	rng := rand.New(rand.NewPCG(1, 2))
	for _, length := range []int{32, 40} {
		for range 500 {
			var sb strings.Builder
			for range length {
				sb.WriteByte(charset[rng.IntN(len(charset))])
			}
			check(sb.String())
		}
	}
}

// ---- series title boundaries ------------------------------------------------------------

// seriesName is a legitimate release name that parses to a known episode, with a
// language, a quality and a release group for every subparser to run on.
const seriesName = "Series.Title.S03E06.German.DL.1080p.BluRay.x264-GRP"

func TestSeriesBoundsFailClosed(t *testing.T) {
	p := newTestParser(t, nil)
	ctx := context.Background()

	// The 4096-byte control that must still parse is TestEpisodeBoundsLegitimate4096ByteNameParses; it
	// runs on the real clock and so is excluded under -race with the complete corpus run.
	for _, tc := range []struct {
		name   string
		input  string
		kind   Kind
		code   string
		params map[string]int64
	}{
		{"4097 bytes", padTo(seriesName, 4097), KindSeries, codeReleaseNameTooLong, map[string]int64{"limit_bytes": 4096, "observed_bytes": 4097}},
		{"invalid UTF-8", "Series.S01E01.720p\xff-GRP", KindSeries, codeReleaseNameInvalidUTF8, map[string]int64{"offset": 18}},
		{"unknown kind", seriesName, Kind(9), codeKindUnsupported, map[string]int64{"kind": 9}},
		{"nothing to parse", "----", KindSeries, codeTitleUnrecognized, map[string]int64{}},
		{"hashed release", "0e895c37245186812cb08aab1529cf8ee389dd05.mkv", KindSeries, codeTitleUnrecognized, map[string]int64{}},
		{"season folder", "Season 3", KindSeries, codeTitleUnrecognized, map[string]int64{}},
		{"password and yenc", `password - "bdc435cb-93c4-4902-97ea-ca00568c3887.337" yEnc`, KindSeries, codeTitleUnrecognized, map[string]int64{}},
		{"ambiguous air date", "Series.Title.12.02.2025.1080i.HDTV.MPA2.0.H.264", KindSeries, codeTitleUnrecognized, map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseTitle(ctx, tc.input, tc.kind)
			wantReason(t, err, tc.code, tc.params)
			if got.Episode != nil || got.Movie != nil || got.Kind != 0 {
				t.Errorf("a refused name must carry no usable value: %+v", got)
			}
		})
	}
	pt, err := p.ParseTitle(ctx, seriesName, KindSeries)
	if err != nil || pt.Kind != KindSeries || pt.Episode == nil || pt.Movie != nil {
		t.Errorf("the control must parse to exactly one episode: %+v, %v", pt, err)
	}
}

// A hostile match in any parser the series ParseTitle runs (the title patterns,
// the substitutions and cleanups, the quality, language and release group
// parsers, the title info patterns) is a match_timeout reason, never a result.
func TestHostileSeriesTitleMatchInAnySubparserFailsClosed(t *testing.T) {
	evil := func(t *testing.T) *regexp2.Regexp {
		t.Helper()
		re, err := regexp2.Compile(`(a+)+$`, ic)
		if err != nil {
			t.Fatal(err)
		}
		re.MatchTimeout = matchTimeout
		return re
	}
	// The hostile run is in the name, or in the series title for the patterns that read it.
	inName := "Series.Title.S03E06.720p." + strings.Repeat("a", 64) + "!.x264-GRP"
	inTitle := "Show." + strings.Repeat("a", 64) + "!.S03E06.720p.x264-GRP"
	for _, tc := range []struct {
		stage  string
		input  string
		inject func(p *Parser, re *regexp2.Regexp)
	}{
		{"title pattern", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.titles[0] = re }},
		{"pre-substitution", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.preSubstitutions[0].re = re }},
		{"hashed release rejection", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.rejectHashed[0] = re }},
		{"season folder", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.seasonFolder = re }},
		{"reversed title", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.reversed = re }},
		{"file extension", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.fileExtension = re }},
		{"simple title", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.simpleTitle = re }},
		{"website prefix", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.common.websitePrefix = re }},
		{"quality brackets", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.cleanBrackets = re }},
		{"six digit air date", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.sixDigitAirDate = re }},
		{"request info", inTitle, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.requestInfo = re }},
		{"year in title", inTitle, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.yearInTitle = re }},
		{"title components", inTitle, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.titleComponents = re }},
		{"quality", inName, func(p *Parser, re *regexp2.Regexp) { p.series.source = re }},
		{"language clean", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.lang.clean = re }},
		{"language", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.lang.insensitive = re }},
		{"language case-sensitive", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.lang.caseSensitive = re }},
		{"release group", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.group.group = re }},
		{"release group anime", inName, func(p *Parser, re *regexp2.Regexp) { p.seriesTitle.group.anime = re }},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			p := newTestParser(t, nil)
			// The control: the same name parses with the real tables.
			if _, err := p.ParseTitle(context.Background(), tc.input, KindSeries); err != nil {
				t.Fatalf("control: %v", err)
			}
			tc.inject(p, evil(t))
			got, err := p.ParseTitle(context.Background(), tc.input, KindSeries)
			wantReason(t, err, codeMatchTimeout, map[string]int64{"limit_ms": 50})
			if got.Episode != nil || got.Kind != 0 {
				t.Errorf("a timeout must carry no usable value: %+v", got)
			}
		})
	}
}

// One release budget covers every suboperation of the series ParseTitle, exactly
// as for the movie parser: a full ParseTitle is run once to count its clock
// reads, then once more for every read k from the first check through
// completion, with the budget found exhausted at read k and no other. Every
// such parse must stop with the budget reason, no value, and exactly k reads (no
// work after the exhaustion, no late success returned). A stage that started a
// budget of its own reads the clock to create it: at that read the blip makes
// the new deadline a whole budget later, so nothing expires and the parse
// finishes instead of failing. Read 1 creates the one legitimate budget.
func TestBudgetSeriesTitleIsSharedByEverySubparser(t *testing.T) {
	ctx := context.Background()
	run := func(at int) (*blipClock, ParsedTitle, error) {
		clk := &blipClock{base: time.Unix(1000, 0), at: at}
		pt, err := newTestParser(t, clk.Now).ParseTitle(ctx, seriesName, KindSeries)
		return clk, pt, err
	}
	probe, _, err := run(0)
	if err != nil {
		t.Fatalf("ParseTitle: %v", err)
	}
	reads := probe.Calls()
	quality := &blipClock{base: time.Unix(1000, 0)}
	if _, err := newTestParser(t, quality.Now).ParseQuality(ctx, seriesName, KindSeries); err != nil {
		t.Fatalf("ParseQuality: %v", err)
	}
	if reads <= 2*quality.Calls() {
		t.Fatalf("ParseTitle read the clock %d times, ParseQuality %d: the title, language and group work must add to the quality work", reads, quality.Calls())
	}

	failures := 0
	for k := 2; k <= reads; k++ {
		clk, got, err := run(k)
		var re *ReasonError
		switch {
		case !errors.As(err, &re) || re.Reason.Code != codeWorkBudgetExhausted:
			t.Errorf("budget exhausted at read %d of %d: err = %v, want %s (a stage with a budget of its own?)", k, reads, err, codeWorkBudgetExhausted)
		case got.Episode != nil || got.Kind != 0:
			t.Errorf("budget exhausted at read %d: no usable value expected, got %+v", k, got)
		case clk.Calls() != k:
			t.Errorf("budget exhausted at read %d: the clock was read %d times (work after the exhaustion)", k, clk.Calls())
		default:
			continue
		}
		if failures++; failures == 5 {
			t.Fatal("more than 5 reads fail; stopping")
		}
	}
}

func TestBudgetSeriesTitleContextCancellationAndDeadlineFailClosed(t *testing.T) {
	p := newTestParser(t, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.ParseTitle(cancelled, seriesName, KindSeries)
	wantReason(t, err, codeCancelled, map[string]int64{})
	if got.Episode != nil {
		t.Errorf("a cancelled parse must carry no usable value: %+v", got)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	got, err = p.ParseTitle(expired, seriesName, KindSeries)
	wantReason(t, err, codeDeadlineExceeded, map[string]int64{})
	if got.Episode != nil {
		t.Errorf("an expired parse must carry no usable value: %+v", got)
	}
}

// Generated names (a SHA-256 chain and random alphanumeric hashes)
// have no fixture input, so they are run here: no such name is a
// series title.
func TestHostileSeriesTitleRejectsGeneratedHashNames(t *testing.T) {
	p := newTestParser(t, nil)
	ctx := context.Background()
	check := func(name string) {
		t.Helper()
		if got, err := p.ParseTitle(ctx, name, KindSeries); err == nil || got.Episode != nil {
			t.Fatalf("%q parsed as %+v", name, got)
		}
	}
	hash := "CRAPPY TEST SEED"
	for range 100 {
		sum := sha256.Sum256([]byte(hash))
		hash = strings.ToUpper(hex.EncodeToString(sum[:]))
		check(hash)
	}
	const charset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	rng := rand.New(rand.NewPCG(1, 2))
	for _, length := range []int{32, 40} {
		for range 500 {
			var sb strings.Builder
			for range length {
				sb.WriteByte(charset[rng.IntN(len(charset))])
			}
			check(sb.String())
		}
	}
}
