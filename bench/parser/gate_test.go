//go:build !race

package main

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse"
	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// The full-corpus measurement runs real parser calls on the wall clock, so it is
// excluded under -race with the other timing tests; the deterministic eligibility
// and verdict checks stay race-eligible in their own files.

func testManifest(t *testing.T) (path string, content []byte) {
	t.Helper()
	content = []byte("synthetic input manifest\n")
	path = filepath.Join(t.TempDir(), "inputs.sha256")
	writeFile(t, path, content)
	return path, content
}

// wantItems derives the measured inputs from the corpus files themselves, apart from
// the producer: every title case of both corpora, series first, in file order,
// materialized for the local day startDate names.
func wantItems(t *testing.T, startDate string) ([]item, int) {
	t.Helper()
	day, err := time.ParseInLocation(factsDateLayout, startDate, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	tc := corpus.TimeContext{Now: day.Add(12 * time.Hour), Location: time.Local}
	var want []item
	cases := 0
	for _, app := range []string{"series", "movie"} {
		kind := parse.KindSeries
		if app == "movie" {
			kind = parse.KindMovie
		}
		c, err := corpus.ReadFile(filepath.Join(testRoot, "testdata", "parser", app+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, cs := range c.Cases {
			if cs.Scope != corpus.ScopeTitle {
				continue
			}
			names, err := cs.Inputs(tc)
			if err != nil {
				t.Fatal(err)
			}
			cases++
			dir := cs.Expected["$isDir"].Bool
			for n, name := range names {
				want = append(want, item{App: app, ID: cs.ID, N: n, name: name, kind: kind, dir: dir})
			}
		}
	}
	if len(want) == 0 || cases == 0 {
		t.Fatal("the corpora hold no title case")
	}
	return want, cases
}

// TestBenchmarkCorpus: the production benchmark over the real corpora, through the
// same reader and materializer the corpus tests use, reaches every title
// case's materialized names in 30 deterministically shuffled rounds after a
// discarded warmup, writes exactly samples.jsonl under -out, and a write error
// there is exit 1 with the fixed message and no success.
func TestBenchmarkCorpus(t *testing.T) {
	manifestPath, manifest := testManifest(t)

	t.Run("full corpus", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), testRun, "samples")
		var stdout, stderr bytes.Buffer
		args := []string{"-rounds", "30", "-seed", "1", "-root", testRoot, "-inputs", manifestPath, "-out", out}
		if code := run(args, &stdout, &stderr); code != exitPass {
			t.Fatalf("exit %d: %s%s", code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "PARSER-LATENCY-PASS") || !strings.HasPrefix(stdout.String(), "parserbench: samples=") {
			t.Fatalf("measuring must report a diagnostic only: %q", stdout.String())
		}
		ents, err := os.ReadDir(out)
		if err != nil || len(ents) != 1 || ents[0].Name() != samplesName || !ents[0].Type().IsRegular() {
			t.Fatalf("out holds %v (%v), want exactly %s", ents, err, samplesName)
		}
		set, err := readSamples(filepath.Join(out, samplesName))
		if err != nil {
			t.Fatal(err)
		}
		h := set.Header

		want, wantCases := wantItems(t, h.StartDate)
		if h.CorpusSeries != indepSHA(t, testRoot, []string{"testdata/parser/series.jsonl"}, false) ||
			h.CorpusMovie != indepSHA(t, testRoot, []string{"testdata/parser/movie.jsonl"}, false) {
			t.Error("a corpus hash is not the file's")
		}
		if h.Lock != indepSHA(t, testRoot, []string{"go.mod", "go.sum"}, true) ||
			h.Source != indepSHA(t, testRoot, indepSources(t, testRoot), true) {
			t.Error("the lock or source hash is not the independently derived one")
		}
		if h.Items != len(want) || len(set.Items) != len(want) || h.Cases != wantCases {
			t.Fatalf("items %d/%d cases %d, want %d/%d", h.Items, len(set.Items), h.Cases, len(want), wantCases)
		}
		okByApp := map[string]int{}
		for i, w := range want {
			it := set.Items[i]
			if it.App != w.App || it.ID != w.ID || it.N != w.N {
				t.Fatalf("item %d = %s %s %d, want %s %s %d", i, it.App, it.ID, it.N, w.App, w.ID, w.N)
			}
			if it.Out == outcomeOK {
				okByApp[it.App]++
			}
		}
		if okByApp["series"] == 0 || okByApp["movie"] == 0 {
			t.Errorf("the real parser accepted no name of one application: %v", okByApp)
		}
		if got := namesHash(want); h.Names != got {
			t.Errorf("names hash %s, want %s", h.Names, got)
		}
		if h.OK+h.Unrecognized+h.Rejected != len(want) {
			t.Errorf("outcome classes %d+%d+%d do not cover %d items", h.OK, h.Unrecognized, h.Rejected, len(want))
		}

		// 30 rounds, each every item once, in the seeded shuffle, no warmup among them.
		if h.Rounds != 30 || h.Seed != 1 || len(set.Rounds) != 30 {
			t.Fatalf("rounds %d/%d seed %d", h.Rounds, len(set.Rounds), h.Seed)
		}
		identity := make([]int, len(want))
		for i := range identity {
			identity[i] = i
		}
		for r, rd := range set.Rounds {
			wantOrder := rand.New(rand.NewPCG(1, uint64(r+1))).Perm(len(want))
			if len(rd.Order) != len(want) || len(rd.NS) != len(want) {
				t.Fatalf("round %d holds %d/%d samples, want %d", r+1, len(rd.Order), len(rd.NS), len(want))
			}
			for k := range wantOrder {
				if rd.Order[k] != wantOrder[k] {
					t.Fatalf("round %d is not the seeded shuffle at call %d", r+1, k)
				}
			}
		}
		if slices.Equal(set.Rounds[0].Order, identity) || slices.Equal(set.Rounds[0].Order, set.Rounds[1].Order) {
			t.Error("the rounds are not shuffled")
		}
		st := pooledStats(set)
		if st.n != 30*len(want) || st.p50 < 1 || st.p99 < st.p50 {
			t.Errorf("pooled stats = %+v", st)
		}
		if h.CompileNS < 1 || h.InputsSHA != sha256Hex(manifest) || h.Run != testRun {
			t.Errorf("provenance: compile %d inputs %s run %s", h.CompileNS, h.InputsSHA, h.Run)
		}
		if h.GOARCH == "" || h.Race != "false" {
			t.Errorf("platform %q race %q", h.GOARCH, h.Race)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		if _, err := os.Stat("/dev/full"); err != nil {
			t.Fatalf("the write-failure control needs /dev/full: %v", err)
		}
		out := filepath.Join(t.TempDir(), testRun, "samples")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/dev/full", filepath.Join(out, samplesName)); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"-rounds", "1", "-seed", "1", "-root", testRoot, "-inputs", manifestPath, "-out", out}
		if code := run(args, &stdout, &stderr); code != exitFail {
			t.Fatalf("exit %d, want %d: %s%s", code, exitFail, stdout.String(), stderr.String())
		}
		if got, want := stderr.String(), "parserbench: write_failed file=samples.jsonl\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
		if stdout.Len() != 0 {
			t.Fatalf("a failed write must report no samples: %q", stdout.String())
		}
	})
}

// tracedCall is one call the measurement made of the parser.
type tracedCall struct {
	name string
	kind parse.Kind
	dir  bool
	ok   bool // a full production parse result (a title of the asked kind)
	err  string
}

// fakeClock is the measurement's controllable clock: it moves only when the test moves it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// tracer wraps the real production parser: it records every call in order with its
// result and moves the fake clock by a known amount around each call, so the interval a
// sample brackets is exactly what the test put there. A call's advance is
// callBase+(call number mod 97) microseconds for the warmup and 400 ms plus the same for
// a timed call (a pause after the parser returned must be measured, not rejected);
// construction advances the clock by buildAdvance, far outside any sample.
type tracer struct {
	real     parseCall
	clock    *fakeClock
	calls    []tracedCall
	built    int             // calls made when construction finished
	advances []time.Duration // the clock movement of each call, by call number
	nWarm    int             // calls that are warmup
}

const buildAdvance = 7 * time.Second

func newTracer(t *testing.T, nWarm int, clock *fakeClock) (*tracer, func() (parseCall, error)) {
	t.Helper()
	real, err := newProductionParser()
	if err != nil {
		t.Fatal(err)
	}
	tr := &tracer{real: real, clock: clock, nWarm: nWarm}
	return tr, func() (parseCall, error) {
		clock.advance(buildAdvance)
		tr.built = len(tr.calls)
		return tr.call, nil
	}
}

func (tr *tracer) call(ctx context.Context, name string, kind parse.Kind, dir bool) (parse.ParsedTitle, error) {
	n := len(tr.calls)
	d := time.Duration(n%97+1) * time.Microsecond
	if n >= tr.nWarm {
		d += 400 * time.Millisecond
	}
	pt, err := tr.real(ctx, name, kind, dir)
	tr.clock.advance(d) // after the parser returned: the sample must still hold it
	tr.advances = append(tr.advances, d)
	c := tracedCall{name: name, kind: kind, dir: dir}
	switch {
	case err != nil:
		c.err = err.Error()
	case pt.Kind == kind && (kind == parse.KindMovie && pt.Movie != nil && pt.Episode == nil || kind == parse.KindSeries && pt.Episode != nil && pt.Movie == nil):
		c.ok = true
	}
	tr.calls = append(tr.calls, c)
	return pt, err
}

// TestBenchmarkMeasuresEveryTimedCall: the discarded warmup, the compilation and the
// timed calls are three separate things. Over the real corpora and the real parser, each
// timed call is made with its own materialized name and kind, in the seeded shuffle,
// after exactly one warmup call per input in corpus order; every one of them runs the
// full parse (the same result class as its warmup, the asked kind of title when it
// parsed). On a controllable clock each sample is exactly the clock movement of the timed
// call it brackets (whatever its length), the warmup movements are in no sample and the
// construction movement is compile_ns and nothing else. A measurement that skipped the
// timed call, or timed the warmup or the construction, would fail the count, the order
// or the interval; no wall-clock bound is involved.
func TestBenchmarkMeasuresEveryTimedCall(t *testing.T) {
	const rounds = 2
	manifestPath, _ := testManifest(t)
	clock := &fakeClock{t: time.Now()}
	// The corpora's input count is the warmup length; derive it as the producer does not.
	want, _ := wantItems(t, clock.t.Format(factsDateLayout))
	n := len(want)
	tr, build := newTracer(t, n, clock)
	set, err := measure(testRoot, manifestPath, testRun, rounds, 1, build, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if tr.built != 0 {
		t.Fatalf("%d calls were made before the construction returned", tr.built)
	}
	if len(tr.calls) != n*(1+rounds) {
		t.Fatalf("%d calls, want %d (one warmup call and %d timed calls per input)", len(tr.calls), n*(1+rounds), rounds)
	}
	for i, w := range want {
		if c := tr.calls[i]; c.name != w.name || c.kind != w.kind || c.dir != w.dir {
			t.Fatalf("warmup call %d is %q kind %d dir %v, want %q kind %d dir %v", i, c.name, c.kind, c.dir, w.name, w.kind, w.dir)
		}
	}
	for r := 1; r <= rounds; r++ {
		perm := rand.New(rand.NewPCG(1, uint64(r))).Perm(n)
		for k, idx := range perm {
			c := tr.calls[n*r+k]
			warm := tr.calls[idx]
			if c.name != want[idx].name || c.kind != want[idx].kind || c.dir != want[idx].dir {
				t.Fatalf("round %d call %d is %q kind %d dir %v, want %q kind %d dir %v", r, k, c.name, c.kind, c.dir, want[idx].name, want[idx].kind, want[idx].dir)
			}
			if c.ok != warm.ok || (c.err == "") != (warm.err == "") {
				t.Fatalf("round %d call %d did not run the same full parse as its warmup: %+v against %+v", r, k, c, warm)
			}
			if got, wantNS := set.Rounds[r-1].NS[k], tr.advances[n*r+k].Nanoseconds(); got != wantNS {
				t.Fatalf("sample %d of round %d is %d ns, want the %d ns the clock moved during that call", k, r, got, wantNS)
			}
		}
	}
	ok := 0
	for _, c := range tr.calls[:n] {
		if c.ok {
			ok++
		}
	}
	if ok == 0 || ok != set.Header.OK {
		t.Fatalf("warmup parsed %d names, the header says %d", ok, set.Header.OK)
	}
	if set.Header.CompileNS != buildAdvance.Nanoseconds() {
		t.Fatalf("compile_ns %d, want exactly the construction's %d", set.Header.CompileNS, buildAdvance.Nanoseconds())
	}
}

// A timed call that does not reach its warmup's outcome class, or fails for a reason
// that is not the parser's own, stops the measurement: no samples are written from it.
func TestBenchmarkRejectsTimedCallsThatDoNotRunTheWarmupsParse(t *testing.T) {
	manifestPath, _ := testManifest(t)
	reason := func(code string) error {
		return &parse.ReasonError{Reason: parse.Reason{Code: code}}
	}
	for _, tc := range []struct {
		name string
		call func(n int) error // the call's result by its 0-based call number
		want string
	}{
		{"timed call rejected after a parsed warmup", func(n int) error {
			if n >= 1 {
				return reason("title_unrecognized")
			}
			return nil
		}, "outcome_changed"},
		{"timed call parses what the warmup rejected", func(n int) error {
			if n == 0 {
				return reason("title_unrecognized")
			}
			return nil
		}, "outcome_changed"},
		{"timed call stopped by the budget", func(n int) error {
			if n >= 1 {
				return reason("budget_exceeded")
			}
			return reason("title_unrecognized")
		}, "outcome_changed"},
		{"foreign error in the warmup", func(n int) error { return context.Canceled }, "parse_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := loadReference(testRoot)
			if err != nil {
				t.Fatal(err)
			}
			items, _, err := ref.items(corpus.TimeContext{Now: time.Now(), Location: time.Local})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			build := func() (parseCall, error) {
				return func(context.Context, string, parse.Kind, bool) (parse.ParsedTitle, error) {
					n := calls
					calls++
					// Calls 0..len(items)-1 are the warmup (phase 0), the rest the timed calls (phase 1).
					if n < len(items) {
						return parse.ParsedTitle{}, tc.call(0)
					}
					return parse.ParsedTitle{}, tc.call(1)
				}, nil
			}
			set, err := measure(testRoot, manifestPath, testRun, 1, 1, build, time.Now)
			if err == nil || err.Error() != tc.want || set != nil {
				t.Fatalf("measure = %v, %v, want error %s", set, err, tc.want)
			}
		})
	}
}

// TestParseTitleClockReadsOfAGuardedParse freezes the clock-read count of one public
// ParseTitle call as a literal, so the count is not the candidate's own. The budget
// checks before and after each pattern, and a pattern the guard rules out is skipped
// with its checks kept. wantReads was counted once from a measured parse of this
// title and is not derived here: a guard that dropped a check on its skip path lowers
// the count, which a sweep bounded by the candidate's own count cannot see.
func TestParseTitleClockReadsOfAGuardedParse(t *testing.T) {
	const (
		name      = "Movie.Name.2015.German.DL.1080p.BluRay.x264-GRP"
		wantReads = 125
	)
	reads := 0
	p, err := parse.NewParser(parse.Options{Now: func() time.Time { reads++; return time.Unix(1000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	reads = 0
	pt, err := p.ParseTitle(context.Background(), name, parse.KindMovie)
	if err != nil {
		t.Fatalf("ParseTitle: %v", err)
	}
	if reads != wantReads {
		t.Errorf("clock reads = %d, want %d", reads, wantReads)
	}
	m := pt.Movie
	if pt.Kind != parse.KindMovie || pt.Episode != nil || m == nil {
		t.Fatalf("result = %+v, want a movie title", pt)
	}
	if !slices.Equal(m.MovieTitles, []string{"Movie Name"}) || m.Year != 2015 || m.ReleaseGroup != "GRP" ||
		!slices.Equal(m.Languages, []string{"german", "original"}) || m.ReleaseTitle != name ||
		m.Quality.Code != "bluray-1080p" || m.Quality.Source != "bluray" || m.Quality.Resolution != 1080 {
		t.Errorf("movie = %+v", m)
	}
}

// Folder-name fixtures, including movie:0403, are measured through ParseDirTitle.
// The same name through ParseTitle stays unrecognized, so dropping the directory
// mark fails this test.
func TestBenchmarkFolderNameUsesParseDirTitle(t *testing.T) {
	ref, err := loadReference(testRoot)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := ref.items(corpus.TimeContext{Now: time.Now(), Location: time.Local})
	if err != nil {
		t.Fatal(err)
	}
	var found item
	var n int
	for _, it := range items {
		if it.dir {
			n++
		}
		if it.ID == "movie:0403" {
			found = it
		}
	}
	if n == 0 || found.ID != "movie:0403" || !found.dir || found.name != "(1995) Movie Name" {
		t.Fatalf("movie:0403 dir entry = %+v, marked folder names = %d", found, n)
	}
	call, err := newProductionParser()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pt, err := call(ctx, found.name, found.kind, true)
	if err != nil || pt.Movie == nil || len(pt.Movie.MovieTitles) == 0 || pt.Movie.MovieTitles[0] != "Movie Name" || pt.Movie.Year != 1995 {
		t.Fatalf("ParseDirTitle(%q) = %+v, %v", found.name, pt, err)
	}
	pt, err = call(ctx, found.name, found.kind, false)
	var re *parse.ReasonError
	if err == nil || !errors.As(err, &re) || re.Reason.Code != "title_unrecognized" || pt.Movie != nil {
		t.Fatalf("ParseTitle(%q) = %+v, %v, want title_unrecognized", found.name, pt, err)
	}
}
