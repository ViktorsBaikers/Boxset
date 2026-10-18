package parse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
	"github.com/dlclark/regexp2/v2"
)

// steppingClock is an injected, deterministic clock: every read advances it.
type steppingClock struct {
	mu    sync.Mutex
	now   time.Time
	step  time.Duration
	calls int
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

func (c *steppingClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newTestParser(t *testing.T, now func() time.Time) *Parser {
	t.Helper()
	p, err := NewParser(Options{Now: now})
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	return p
}

func wantReason(t *testing.T, err error, code string, params map[string]int64) {
	t.Helper()
	var re *ReasonError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want *ReasonError %q", err, code)
	}
	if re.Reason.Code != code {
		t.Errorf("reason code = %q, want %q", re.Reason.Code, code)
	}
	for k, want := range params {
		if got, ok := re.Reason.Params[k]; !ok || got != want {
			t.Errorf("reason param %s = %d (present %v), want %d", k, got, ok, want)
		}
	}
	if len(re.Reason.Params) != len(params) {
		t.Errorf("reason params = %v, want exactly %v", re.Reason.Params, params)
	}
}

// padTo returns name left-padded with 'a' so the result is exactly n bytes.
func padTo(name string, n int) string {
	return strings.Repeat("a", n-len(name)) + name
}

func TestQualityBounds(t *testing.T) {
	clk := &steppingClock{now: time.Unix(1000, 0), step: time.Microsecond}
	p := newTestParser(t, clk.Now)
	ctx := context.Background()

	for _, k := range []struct {
		kind Kind
		name string
	}{{KindSeries, "series"}, {KindMovie, "movie"}} {
		t.Run(k.name, func(t *testing.T) {
			// Valid controls: exactly 4096 bytes, ASCII and multi-byte UTF-8.
			ascii := padTo(".S01E01.720p.HDTV.x264-GRP", maxNameBytes)
			multi := strings.Repeat("é", 2040) + ".720p.HDTV.x264" + "a"
			for _, name := range []string{ascii, multi} {
				if len(name) != maxNameBytes {
					t.Fatalf("control is %d bytes", len(name))
				}
				q, err := p.ParseQuality(ctx, name, k.kind)
				if err != nil || q.Code != "hdtv-720p" || q.Resolution != 720 || q.SourceDetection != detectionName {
					t.Errorf("4096-byte control: %+v, %v", q, err)
				}
			}

			// max+1 bytes (counted in bytes, not runes), invalid UTF-8 and an
			// unsupported kind reject with exact structured reasons and no value,
			// and before any matching work (the budget clock is never read).
			before := clk.Calls()
			tooLong := padTo(".720p.HDTV", maxNameBytes+1)
			manyRunes := strings.Repeat("é", 2049) // 2049 runes, 4098 bytes
			for name, observed := range map[string]int64{tooLong: maxNameBytes + 1, manyRunes: 4098} {
				q, err := p.ParseQuality(ctx, name, k.kind)
				wantReason(t, err, "release_name_too_long", map[string]int64{"limit_bytes": maxNameBytes, "observed_bytes": observed})
				if q != (Quality{}) {
					t.Errorf("a rejected name must carry no usable value: %+v", q)
				}
				if strings.Contains(err.Error(), "aaaa") {
					t.Errorf("the error text must not echo the name: %q", err.Error()[:40])
				}
			}
			q, err := p.ParseQuality(ctx, "Show\xff.720p.HDTV", k.kind)
			wantReason(t, err, "release_name_invalid_utf8", map[string]int64{"offset": 4})
			if q != (Quality{}) {
				t.Errorf("invalid UTF-8 must carry no usable value: %+v", q)
			}
			if clk.Calls() != before {
				t.Error("name guards must run before any budget or matching work")
			}
		})
	}

	for _, kind := range []Kind{0, 3, 255} {
		q, err := p.ParseQuality(ctx, "Show.720p.HDTV", kind)
		wantReason(t, err, "kind_unsupported", map[string]int64{"kind": int64(kind)})
		if q != (Quality{}) {
			t.Errorf("unsupported kind must carry no value: %+v", q)
		}
	}
}

func TestQualityBudgetIsSharedByNestedHelpers(t *testing.T) {
	name := "Series.Title.S01E01.REPACK.1080p.BluRay.REMUX.x264-GRP"
	for _, kind := range []Kind{KindSeries, KindMovie} {
		// Slow clock: every read costs 20 ms, so the one shared 250 ms budget runs
		// out inside a single parse. Per-helper fresh budgets would each stay
		// under it, so only a shared budget fails here.
		slow := &steppingClock{now: time.Unix(1000, 0), step: 20 * time.Millisecond}
		q, err := newTestParser(t, slow.Now).ParseQuality(context.Background(), name, kind)
		wantReason(t, err, "release_work_budget_exhausted", map[string]int64{"limit_ms": 250})
		if q != (Quality{}) {
			t.Errorf("kind %d: exhausted budget must carry no value: %+v", kind, q)
		}
		// The same name with a fast clock is a valid control.
		fast := &steppingClock{now: time.Unix(1000, 0), step: time.Microsecond}
		q, err = newTestParser(t, fast.Now).ParseQuality(context.Background(), name, kind)
		if err != nil || q.Code != "remux-1080p" {
			t.Errorf("kind %d: control = %+v, %v", kind, q, err)
		}
		if fast.Calls() < 8 {
			t.Errorf("kind %d: parse read the clock only %d times; the budget is not checked around each match", kind, fast.Calls())
		}
	}
}

func TestQualityLateSuccessIsNotAPass(t *testing.T) {
	// The budget expires while the first match runs: the (successful) match
	// result must be discarded, not returned.
	calls := 0
	base := time.Unix(1000, 0)
	now := func() time.Time {
		calls++
		if calls <= 3 {
			return base
		}
		return base.Add(releaseBudget + time.Millisecond)
	}
	q, err := newTestParser(t, now).ParseQuality(context.Background(), "Series.Title.S01E01.720p.HDTV.x264-GRP", KindSeries)
	wantReason(t, err, "release_work_budget_exhausted", map[string]int64{"limit_ms": 250})
	if q != (Quality{}) {
		t.Errorf("late success must not produce a value: %+v", q)
	}
}

func TestQualityContextCancellation(t *testing.T) {
	p := newTestParser(t, time.Now)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	q, err := p.ParseQuality(cancelled, "Series.Title.S01E01.720p.HDTV.x264-GRP", KindSeries)
	wantReason(t, err, "evaluation_cancelled", map[string]int64{})
	if q != (Quality{}) {
		t.Errorf("cancelled parse must carry no value: %+v", q)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	_, err = p.ParseQuality(expired, "Movie.Title.2018.720p.HDTV.x264-GRP", KindMovie)
	wantReason(t, err, "decision_deadline_exceeded", map[string]int64{})
}

func TestQualitySourceSpecificBehavior(t *testing.T) {
	p := newTestParser(t, time.Now)
	ctx := context.Background()
	parse := func(name string, kind Kind) Quality {
		t.Helper()
		q, err := p.ParseQuality(ctx, name, kind)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		return q
	}

	// Blank names are Unknown with the default revision and no detection.
	for _, kind := range []Kind{KindSeries, KindMovie} {
		q := parse(" \u00a0\t ", kind)
		if q.Code != "unknown" || q.Revision != (Revision{Version: 1}) || q.SourceDetection != detectionUnknown {
			t.Errorf("blank name kind %d: %+v", kind, q)
		}
	}

	// The movie parser keeps the parsed resolution on a TELESYNC quality.
	ts := parse("Movie.Title.3.2017.720p.TSRip.x264.AAC-Ozlem", KindMovie)
	if ts.Code != "telesync" || ts.Resolution != 720 || ts.Source != "telesync" {
		t.Errorf("telesync: %+v", ts)
	}
	if ts := parse("Movie.Title.2017.TSRip.x264", KindMovie); ts.Code != "telesync" || ts.Resolution != 0 {
		t.Errorf("telesync without resolution: %+v", ts)
	}

	// A name with no quality still reports extension detection.
	s := parse("The.Series.S02E15", KindSeries)
	if s.Code != "unknown" || s.SourceDetection != detectionExtension || s.ResolutionDetection != detectionExtension {
		t.Errorf("unknown quality from extension: %+v", s)
	}
	m := parse("Some.Movie.S02E15.avi", KindMovie)
	if m.Code != "sdtv" || m.ModifierDetection != detectionExtension {
		t.Errorf("movie extension quality: %+v", m)
	}
	// The extension rule is skipped for a name with an invalid path char (NUL).
	if q := parse("The.Series.S02E15\x00.mkv", KindSeries); q.SourceDetection != detectionUnknown {
		t.Errorf("NUL name must not use the extension: %+v", q)
	}
	// The movie and series parsers disagree on .mkv: the vocabulary keeps both.
	if q := parse("The.Series.S01E01.mkv", KindSeries); q.Code != "hdtv-720p" {
		t.Errorf("series .mkv: %+v", q)
	}
	if q := parse("The.Series.S01E01.mkv", KindMovie); q.Code != "webdl-720p" {
		t.Errorf("movie .mkv: %+v", q)
	}

	// A non-ASCII digit satisfies \d but does not parse as an integer:
	// there is no value, so the parse is rejected.
	_, err := p.ParseQuality(ctx, "Anime 1v\u0663 (720p)", KindSeries)
	wantReason(t, err, "title_unrecognized", map[string]int64{})
}

func TestQualityFinderFallbacks(t *testing.T) {
	s := newTestParserTables(t)
	if got := s.series.find(ssTelevision, 576); got != sqSDTV {
		t.Errorf("series television 576 = %d, want SDTV", got)
	}
	if got := s.series.find(ssWebRip, 576); got != sqWEBRip480p {
		t.Errorf("series webrip 576 = %d, want WEBRip480p", got)
	}
	if got := s.series.find(ssWeb, 480); got != sqWEBDL480p {
		t.Errorf("series web 480 = %d, want WEBDL480p", got)
	}
	if got := s.series.find(ssBlurayRaw, 720); got != sqUnknown {
		t.Errorf("series bluray-raw 720 = %d, want Unknown", got)
	}
	if got := s.series.find(ssTelevision, 1440); got != sqUnknown {
		t.Errorf("series television 1440 = %d, want Unknown", got)
	}
	if got := s.movie.find(rsBluray, 720, "remux"); got != rqUnknown {
		t.Errorf("movie remux 720 = %d, want Unknown", got)
	}
	if got := s.movie.find(rsBluray, 1080, "remux"); got != rqRemux1080p {
		t.Errorf("movie remux 1080 = %d, want Remux1080p", got)
	}
	if got := s.movie.find(rsDVD, 480, ""); got != rqDVD {
		t.Errorf("movie dvd 480 = %d, want DVD (unknown-resolution quality of that source)", got)
	}
	if got := s.movie.find(rsWebDL, 480, ""); got != rqWEBDL480p {
		t.Errorf("movie webdl 480 = %d, want WEBDL480p", got)
	}
}

func newTestParserTables(t *testing.T) *Parser {
	t.Helper()
	return newTestParser(t, time.Now)
}

func TestVocabularyIsExplicitAndConsistent(t *testing.T) {
	for name, defs := range map[string][]qualityDef{
		"series": func() []qualityDef { d := seriesDefs(); return d[:] }(),
		"movie":  func() []qualityDef { d := movieDefs(); return d[:] }(),
	} {
		codes, members := map[string]bool{}, map[string]bool{}
		for i, d := range defs {
			if d.member == "" || d.code == "" || d.source == "" {
				t.Errorf("%s def %d is incomplete: %+v", name, i, d)
			}
			if codes[d.code] || members[d.member] {
				t.Errorf("%s: duplicate code or member at %d: %+v", name, i, d)
			}
			codes[d.code], members[d.member] = true, true
			var src string
			var ok bool
			if name == "series" {
				src, ok = seriesSourceCode(map[string]string{
					"unknown": "Unknown", "tv": "Television", "tv-raw": "TelevisionRaw", "web-dl": "Web",
					"web-rip": "WebRip", "dvd": "DVD", "bluray": "Bluray", "bluray-raw": "BlurayRaw",
				}[d.source])
			} else {
				src, ok = movieSourceCode(map[string]string{
					"unknown": "UNKNOWN", "cam": "CAM", "telesync": "TELESYNC", "telecine": "TELECINE", "workprint": "WORKPRINT",
					"dvd": "DVD", "tv": "TV", "web-dl": "WEBDL", "web-rip": "WEBRIP", "bluray": "BLURAY",
				}[d.source])
			}
			if !ok || src != d.source {
				t.Errorf("%s def %s: source code %q is not in the explicit source map", name, d.member, d.source)
			}
			if got, ok := map[bool]func(string) (qualityDef, bool){true: seriesQualityDef, false: movieQualityDef}[name == "series"](d.member); !ok || got != d {
				t.Errorf("%s: member lookup of %s", name, d.member)
			}
		}
	}
	for member, want := range map[string]string{"Unknown": "unknown", "Name": "name", "Extension": "extension", "MediaInfo": "mediainfo"} {
		if got, ok := detectionCode(member); !ok || got != want {
			t.Errorf("detection %s = %q (%v)", member, got, ok)
		}
	}
	if _, ok := detectionCode("Other"); ok {
		t.Error("unknown detection members must not map")
	}
	for member, want := range map[string]string{"NONE": "", "REGIONAL": "regional", "SCREENER": "screener", "RAWHD": "raw-hd", "BRDISK": "br-disk", "REMUX": "remux"} {
		if got, ok := movieModifierCode(member); !ok || got != want {
			t.Errorf("modifier %s = %q (%v)", member, got, ok)
		}
	}
	if _, ok := movieModifierCode("Remux"); ok {
		t.Error("modifier members are case-sensitive names")
	}
}

func TestSourceStringHelpers(t *testing.T) {
	if got := trimWhite("\u00a0 \tName\u2003\n"); got != "Name" {
		t.Errorf("trim = %q", got)
	}
	if !isBlank(" \u3000\t") || isBlank(" a ") {
		t.Error("isBlank")
	}
	if !containsFold("The.BLURAY720p", "bluray720p") || containsFold("The.Blu-ray", "bluray") {
		t.Error("containsFold")
	}
	for in, want := range map[string]string{
		"a/b.c/d": "", "x.avi": ".avi", "x.": "", "x": "", "dir.v/file.MKV": ".MKV", "E:\\Downloads\\x.avi": ".avi",
	} {
		if got := pathExtension(in); got != want {
			t.Errorf("pathExtension(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pathExtensionAnywhere("a/b.c/d"); got != ".c/d" {
		t.Errorf("pathExtensionAnywhere = %q", got)
	}
	if !containsInvalidPathChar("a\x00b") || containsInvalidPathChar("a\\b") {
		t.Error("containsInvalidPathChar")
	}
	if seriesExtensionQuality(".MkV") != sqHDTV720p || movieExtensionQuality(".mk3d") != rqWEBDL720p || movieExtensionQuality(".webm") != rqUnknown {
		t.Error("extension tables")
	}
}

// concurrentStride picks the bounded subset of each committed corpus the
// concurrency test runs: every 11th case, spread over every scope.
const concurrentStride = 11

func TestSharedParserIsConcurrencySafe(t *testing.T) {
	p := newTestParser(t, time.Now)
	byID := dispositionsByID(readDispositions(t))
	tc := corpus.TimeContext{Now: time.Now(), Location: time.Local}
	type subset struct {
		app  string
		kind Kind
		c    *corpus.Corpus
	}
	var subsets []subset
	for _, app := range []struct {
		name string
		kind Kind
	}{{"series", KindSeries}, {"movie", KindMovie}} {
		full, _ := readCorpusFile(t, app.name)
		picked := &corpus.Corpus{Header: full.Header}
		for i, cs := range full.Cases {
			if _, disposed := byID[cs.ID]; i%concurrentStride != 0 || cs.Scope != corpus.ScopeQuality || disposed {
				continue
			}
			if _, err := cs.Inputs(tc); err != nil {
				t.Fatalf("%s: %v", cs.ID, err)
			}
			picked.Cases = append(picked.Cases, cs)
		}
		if len(picked.Cases) == 0 {
			t.Fatalf("%s: the bounded subset is empty", app.name)
		}
		subsets = append(subsets, subset{app.name, app.kind, picked})
	}
	titles := titleSubset(t, byID)
	seriesTitles := seriesTitleSubset(t, byID)
	runNow, runLoc := seriesRunClock(t)
	seriesParser := newSeriesTestParser(t, runNow, runLoc)
	seriesClock := corpus.TimeContext{Now: runNow, Location: runLoc}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for range 8 {
		for _, s := range subsets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := runQualityCorpus(p, s.kind, s.app, s.c, nil)
				if len(r.problems) != 0 || r.failed != 0 || r.passed != len(s.c.Cases) {
					errs <- fmt.Sprintf("%s: passed %d of %d, failed %d: %v", s.app, r.passed, len(s.c.Cases), r.failed, r.problems)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := runTitleCorpus(p, titles, nil)
			if len(r.problems) != 0 || r.failed != 0 || r.passed != len(titles.Cases) {
				errs <- fmt.Sprintf("movie titles: passed %d of %d, failed %d: %v", r.passed, len(titles.Cases), r.failed, r.problems)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := runSeriesCorpus(seriesParser, seriesTitles, nil, seriesClock)
			if len(r.problems) != 0 || r.failed != 0 || r.passed != len(seriesTitles.Cases) {
				errs <- fmt.Sprintf("series titles: passed %d of %d, failed %d: %v", r.passed, len(seriesTitles.Cases), r.failed, r.problems)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Errorf("concurrent parse diverged: %s", msg)
	}
}

// titleSubset is the bounded subset of the committed movie title cases the
// concurrency test runs: every 11th case of the whole file, which must reach
// all three title entry points.
func titleSubset(t *testing.T, byID map[string]corpus.Disposition) *corpus.Corpus {
	t.Helper()
	full, _ := readCorpusFile(t, "movie")
	picked := &corpus.Corpus{Header: full.Header}
	owners := map[string]bool{}
	for i, cs := range full.Cases {
		if _, disposed := byID[cs.ID]; i%concurrentStride != 0 || cs.Scope != corpus.ScopeTitle || disposed {
			continue
		}
		picked.Cases = append(picked.Cases, cs)
		owners[cs.ScopeOwner] = true
	}
	if len(picked.Cases) == 0 || len(owners) != 3 {
		t.Fatalf("the bounded title subset has %d cases over entry points %v", len(picked.Cases), owners)
	}
	return picked
}

// ---- committed-corpus consumer shared by the full corpus run --------------------

const corpusDir = "../../testdata/parser"

func readCorpusFile(t *testing.T, app string) (*corpus.Corpus, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, app+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := corpus.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s corpus: %v", app, err)
	}
	sum := sha256.Sum256(data)
	return c, hex.EncodeToString(sum[:])
}

func readDispositions(t *testing.T) *corpus.Dispositions {
	t.Helper()
	f, err := os.Open(filepath.Join(corpusDir, "dispositions.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	d, err := corpus.ReadDispositions(f)
	if err != nil {
		t.Fatalf("dispositions: %v", err)
	}
	return d
}

func dispositionsByID(d *corpus.Dispositions) map[string]corpus.Disposition {
	byID := make(map[string]corpus.Disposition, len(d.Dispositions))
	for _, disp := range d.Dispositions {
		byID[disp.CaseID] = disp
	}
	return byID
}

func str(s string) corpus.Value { return corpus.Value{Kind: corpus.KindString, Str: s} }

// resolve maps one asserted property to the native comparison: the
// expectation expressed as a Boxset value (enum members through the
// explicit code maps) and the observed native value. An unknown property, enum
// type or member is a hard error, never a skipped assertion.
func resolve(app, key string, want corpus.Value, q Quality) (wantNative, observed corpus.Value, err error) {
	enumCode := func(typ string, code func(string) (string, bool)) (corpus.Value, error) {
		if want.Kind != corpus.KindEnum || want.Type != typ {
			return corpus.Value{}, fmt.Errorf("%s: expected a %s enum, got %+v", key, typ, want)
		}
		c, ok := code(want.Str)
		if !ok {
			return corpus.Value{}, fmt.Errorf("%s: %s.%s has no explicit code mapping", key, typ, want.Str)
		}
		return str(c), nil
	}
	wantInt := func(got int) (corpus.Value, corpus.Value, error) {
		if want.Kind != corpus.KindInt {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected an int, got %+v", key, want)
		}
		return want, corpus.Value{Kind: corpus.KindInt, Int: int64(got)}, nil
	}
	sourceCode, qualityDefFor := seriesSourceCode, seriesQualityDef
	if app == "movie" {
		sourceCode, qualityDefFor = movieSourceCode, movieQualityDef
	}
	switch key {
	case "Quality":
		wantNative, err = enumCode("Quality", func(m string) (string, bool) {
			d, ok := qualityDefFor(m)
			return d.code, ok
		})
		return wantNative, str(q.Code), err
	case "Quality.Source":
		wantNative, err = enumCode("QualitySource", sourceCode)
		return wantNative, str(q.Source), err
	case "Quality.Modifier":
		if app != "movie" {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: %s has no modifier", key, app)
		}
		wantNative, err = enumCode("Modifier", movieModifierCode)
		return wantNative, str(q.Modifier), err
	case "Quality.Resolution":
		return wantInt(q.Resolution)
	case "Revision.Version":
		return wantInt(q.Revision.Version)
	case "Revision.Real":
		return wantInt(q.Revision.Real)
	case "Revision.IsRepack":
		if want.Kind != corpus.KindBool {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected a bool, got %+v", key, want)
		}
		return want, corpus.Value{Kind: corpus.KindBool, Bool: q.Revision.IsRepack}, nil
	case "SourceDetectionSource", "ResolutionDetectionSource", "ModifierDetectionSource", "RevisionDetectionSource":
		got := map[string]string{
			"SourceDetectionSource": q.SourceDetection, "ResolutionDetectionSource": q.ResolutionDetection,
			"ModifierDetectionSource": q.ModifierDetection, "RevisionDetectionSource": q.RevisionDetection,
		}[key]
		wantNative, err = enumCode("QualityDetectionSource", detectionCode)
		return wantNative, str(got), err
	}
	return corpus.Value{}, corpus.Value{}, fmt.Errorf("unsupported asserted property %q", key)
}

// qualityRun is the outcome of one corpus through one parser. The selected
// scope is quality: cases counts what ran. other counts the title cases that a
// later consumer owns, so the whole inventory reconciles separately from the
// selected scope.
type qualityRun struct {
	inputs                   int
	passed, disposed, failed int
	cases, other             int
	problems                 []string
}

func (r *qualityRun) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// runQualityCorpus runs every quality case of c through p. A case passes only
// when every assertion holds for every input; a mismatch needs a disposition
// (byID) binding every distinct mismatching observation.
func runQualityCorpus(p *Parser, kind Kind, app string, c *corpus.Corpus, byID map[string]corpus.Disposition) qualityRun {
	var r qualityRun
	// Quality names carry no symbolic date; the context is only the shared contract.
	tc := corpus.TimeContext{Now: time.Now(), Location: time.Local}
	for _, cs := range c.Cases {
		switch {
		case cs.Scope != corpus.ScopeQuality:
			r.other++
		default:
			r.cases++
			r.runCase(p, kind, app, cs, tc, byID)
		}
	}
	return r
}

func (r *qualityRun) runCase(p *Parser, kind Kind, app string, cs corpus.Case, tc corpus.TimeContext, byID map[string]corpus.Disposition) {
	names, err := cs.Inputs(tc)
	if err != nil {
		r.problem("%s: %v", cs.ID, err)
		r.failed++
		return
	}
	d, has := byID[cs.ID]
	mismatch := map[string][]corpus.Value{} // property -> distinct observed values
	bad := false
	for _, name := range names {
		r.inputs++
		q, err := p.ParseQuality(context.Background(), name, kind)
		if err != nil {
			r.problem("%s %q: %v", cs.ID, name, err)
			bad = true
			continue
		}
		for _, key := range corpus.SortedKeys(cs.Expected) {
			if key == corpus.InputKey {
				continue
			}
			wantNative, observed, err := resolve(app, key, cs.Expected[key], q)
			if err != nil {
				r.problem("%s: %v", cs.ID, err)
				bad = true
				continue
			}
			if corpus.Equal(wantNative, observed) {
				continue
			}
			if !slices.ContainsFunc(mismatch[key], func(v corpus.Value) bool { return corpus.Equal(v, observed) }) {
				mismatch[key] = append(mismatch[key], observed)
			}
			if !has {
				r.problem("%s %q: %s = %+v, expects %+v", cs.ID, name, key, observed, wantNative)
			}
		}
	}
	switch {
	case bad:
		r.failed++
	case len(mismatch) == 0 && !has:
		r.passed++
	case len(mismatch) == 0:
		r.problem("%s: disposition is stale, the case passes", cs.ID)
		r.failed++
	case !has:
		r.failed++
	case !dispositionBinds(d, mismatch):
		r.problem("%s: disposition does not bind every observed value %v", cs.ID, mismatch)
		r.failed++
	default:
		r.disposed++
	}
}

// dispositionBinds reports whether d binds exactly the mismatching
// properties, with every distinct observed value of each equal to the bound one.
func dispositionBinds(d corpus.Disposition, mismatch map[string][]corpus.Value) bool {
	if len(mismatch) != len(d.Expected) {
		return false
	}
	for key, observed := range mismatch {
		bound, ok := d.Observed[key]
		if _, expected := d.Expected[key]; !ok || !expected {
			return false
		}
		for _, v := range observed {
			if !corpus.Equal(v, bound) {
				return false
			}
		}
	}
	return true
}

// scopeCounts is the number of cases of c per scope.
func scopeCounts(c *corpus.Corpus) map[string]int {
	counts := map[string]int{}
	for _, cs := range c.Cases {
		counts[cs.Scope]++
	}
	return counts
}

// reconcile compares the run with the per-scope case counts of its corpus: the
// whole inventory (both scopes), and, separately, the selected quality scope.
func (r qualityRun) reconcile(counts map[string]int) []string {
	var out []string
	check := func(name string, got, want int) {
		if got != want {
			out = append(out, fmt.Sprintf("inventory %s = %d, corpus %d", name, got, want))
		}
	}
	check(corpus.ScopeQuality, r.cases, counts[corpus.ScopeQuality])
	check(corpus.ScopeTitle, r.other, counts[corpus.ScopeTitle])
	if r.cases == 0 {
		out = append(out, "selected quality scope has no case")
	}
	if r.passed+r.disposed+r.failed != r.cases {
		out = append(out, fmt.Sprintf("selected scope: passed %d + disposed %d + failed %d != cases %d", r.passed, r.disposed, r.failed, r.cases))
	}
	if r.failed != 0 || r.passed == 0 {
		out = append(out, fmt.Sprintf("selected scope: failed=%d passed=%d", r.failed, r.passed))
	}
	return out
}

// mixedCase is one synthetic quality-corpus case.
func mixedCase(id, scope string, want map[string]corpus.Value, inputs ...string) corpus.Case {
	items := make([]corpus.Value, len(inputs))
	for i, in := range inputs {
		items[i] = str(in)
	}
	expected := map[string]corpus.Value{corpus.InputKey: {Kind: corpus.KindArray, Items: items}}
	maps.Copy(expected, want)
	return corpus.Case{
		ID: id, Scope: scope, ScopeOwner: "QualityParser.ParseQuality",
		Args: []corpus.Argument{str(inputs[0])}, Expected: expected,
	}
}

func qualityEnum(member string) corpus.Value {
	return corpus.Value{Kind: corpus.KindEnum, Type: "Quality", Str: member}
}

func mixedCorpus(cases ...corpus.Case) *corpus.Corpus {
	return &corpus.Corpus{Header: corpus.Header{Schema: corpus.SchemaVersion, App: "series"}, Cases: cases}
}

func TestQualityConsumerReconcilesInventoryAndSelectedScopeSeparately(t *testing.T) {
	const sdtvName = "The.Series.S01E13.x264-CtrlSD"
	cases := []corpus.Case{
		mixedCase("pass", corpus.ScopeQuality, map[string]corpus.Value{"Quality": qualityEnum("SDTV")}, sdtvName),
		mixedCase("mismatch", corpus.ScopeQuality, map[string]corpus.Value{"Quality": qualityEnum("DVD")}, sdtvName),
		mixedCase("title-1", corpus.ScopeTitle, map[string]corpus.Value{"Title": str("x")}, sdtvName),
		mixedCase("title-2", corpus.ScopeTitle, map[string]corpus.Value{"Title": str("x")}, sdtvName),
	}
	c := mixedCorpus(cases...)
	if err := c.Validate(); err != nil {
		t.Fatalf("mixed fixture: %v", err)
	}
	byID := map[string]corpus.Disposition{"mismatch": {
		CaseID: "mismatch", Expected: map[string]corpus.Value{"Quality": qualityEnum("DVD")},
		Observed: map[string]corpus.Value{"Quality": str("sdtv")},
	}}
	r := runQualityCorpus(newTestParser(t, time.Now), KindSeries, "series", c, byID)
	want := qualityRun{inputs: 2, passed: 1, disposed: 1, cases: 2, other: 2}
	if r.inputs != want.inputs || r.passed != want.passed || r.disposed != want.disposed || r.failed != 0 || r.cases != want.cases || r.other != want.other {
		t.Fatalf("run = %+v, want %+v", r, want)
	}
	if problems := append(r.problems, r.reconcile(scopeCounts(c))...); len(problems) != 0 {
		t.Fatalf("a mixed quality/title inventory must reconcile: %v", problems)
	}

	// Every scope count is checked against the whole inventory, not the selected scope.
	for _, scope := range []string{corpus.ScopeQuality, corpus.ScopeTitle} {
		counts := maps.Clone(scopeCounts(c))
		counts[scope]++
		if len(r.reconcile(counts)) == 0 {
			t.Errorf("a %s count off by one must fail reconciliation", scope)
		}
	}

	// A selected scope with no case fails even when the inventory reconciles.
	titleOnly := mixedCorpus(cases[2], cases[3])
	tr := runQualityCorpus(newTestParser(t, time.Now), KindSeries, "series", titleOnly, nil)
	if len(tr.reconcile(scopeCounts(titleOnly))) == 0 {
		t.Error("a corpus with no selected-scope case must fail")
	}
}

func TestQualityDispositionBindsEveryMismatchingInput(t *testing.T) {
	// Both inputs are SDTV for the series parser, and only that observation is bound.
	const sdtvA, sdtvB, hdtv = "The.Series.S01E13.x264-CtrlSD", "The Series S02E01 HDTV XviD 2HD", "My series S01E01 [HDTV-1080p]"
	bind := corpus.Disposition{
		CaseID: "c", Expected: map[string]corpus.Value{"Quality": qualityEnum("DVD")},
		Observed: map[string]corpus.Value{"Quality": str("sdtv")},
	}
	run := func(inputs ...string) qualityRun {
		c := mixedCorpus(mixedCase("c", corpus.ScopeQuality, map[string]corpus.Value{"Quality": qualityEnum("DVD")}, inputs...))
		return runQualityCorpus(newTestParser(t, time.Now), KindSeries, "series", c, map[string]corpus.Disposition{"c": bind})
	}
	if r := run(sdtvA, sdtvB); r.disposed != 1 || r.failed != 0 || len(r.problems) != 0 {
		t.Errorf("a disposition equal for every mismatching input must bind: %+v", r)
	}
	// A second input that mismatches with another observed value is not bound by the first one's.
	if r := run(sdtvA, hdtv); r.disposed != 0 || r.failed != 1 || len(r.problems) == 0 {
		t.Errorf("a differing second observation must not ride on the first: %+v", r)
	}
	if r := run(hdtv, sdtvA); r.disposed != 0 || r.failed != 1 {
		t.Errorf("the order of the inputs must not matter: %+v", r)
	}
}

// privateReads counts the clock reads one parse makes before its final guard: the budget's creation,
// the opening check and the table's own matching, run through the private evaluation and never
// through the public ParseQuality, so the count does not depend on the public final guard existing.
func privateReads(t *testing.T, p *Parser, kind Kind, input string) int {
	t.Helper()
	probe := &steppingClock{now: time.Unix(1000, 0), step: time.Nanosecond}
	b := newBudget(context.Background(), probe.Now)
	if err := b.check(); err != nil {
		t.Fatalf("opening check: %v", err)
	}
	var err error
	if kind == KindMovie {
		_, err = p.movie.parseQuality(b, input)
	} else {
		_, err = p.series.parseQuality(b, input)
	}
	if err != nil {
		t.Fatalf("private evaluation: %v", err)
	}
	return probe.Calls()
}

func TestQualityFinalBudgetCheckCoversEveryTerminalPath(t *testing.T) {
	base := time.Unix(1000, 0)
	for _, tc := range []struct {
		name  string
		kind  Kind
		input string
	}{
		{"series blank", KindSeries, " \u00a0\t "},
		{"movie blank", KindMovie, " \u00a0\t "},
		{"series unmatched", KindSeries, "The.Series.S02E15"},
		{"movie unmatched", KindMovie, "Movie.Title.2018"},
		{"series matched", KindSeries, "My series S01E01 [Bluray-1080p]"},
		{"movie matched", KindMovie, "Movie.Title.2018.720p.HDTV.x264-GRP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := privateReads(t, newTestParser(t, time.Now), tc.kind, tc.input)
			ok := &steppingClock{now: base, step: time.Nanosecond}
			if _, err := newTestParser(t, ok.Now).ParseQuality(context.Background(), tc.input, tc.kind); err != nil {
				t.Fatalf("control: %v", err)
			}

			// Every read the evaluation itself makes is within the budget; only the read after the last
			// of them, the public final guard's, is past it. Without that guard no read follows and the
			// parse would return a value.
			calls := 0
			expireAfterEvaluation := func() time.Time {
				calls++
				if calls <= inner {
					return base
				}
				return base.Add(releaseBudget + time.Millisecond)
			}
			q, err := newTestParser(t, expireAfterEvaluation).ParseQuality(context.Background(), tc.input, tc.kind)
			wantReason(t, err, "release_work_budget_exhausted", map[string]int64{"limit_ms": 250})
			if q != (Quality{}) {
				t.Errorf("a result completed after the budget must carry no value: %+v", q)
			}

			// Only the final guard sees a cancellation raised at the evaluation's last clock read.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls = 0
			cancelAtLastEvaluationRead := func() time.Time {
				calls++
				if calls == inner {
					cancel()
				}
				return base
			}
			q, err = newTestParser(t, cancelAtLastEvaluationRead).ParseQuality(ctx, tc.input, tc.kind)
			wantReason(t, err, "evaluation_cancelled", map[string]int64{})
			if q != (Quality{}) {
				t.Errorf("a result completed after cancellation must carry no value: %+v", q)
			}
		})
	}
}

func TestQualityVocabularyMatchesIndependentMemberTable(t *testing.T) {
	// Fixed by hand from the Quality member names, not computed from vocab.go.
	series := map[string]string{
		"Unknown": "unknown", "SDTV": "sdtv", "DVD": "dvd", "WEBRip480p": "webrip-480p", "WEBDL480p": "webdl-480p",
		"Bluray480p": "bluray-480p", "Bluray576p": "bluray-576p", "HDTV720p": "hdtv-720p", "WEBRip720p": "webrip-720p",
		"WEBDL720p": "webdl-720p", "Bluray720p": "bluray-720p", "Bluray1080p": "bluray-1080p", "HDTV1080p": "hdtv-1080p",
		"WEBRip1080p": "webrip-1080p", "WEBDL1080p": "webdl-1080p", "RAWHD": "raw-hd", "HDTV2160p": "hdtv-2160p",
		"WEBRip2160p": "webrip-2160p", "WEBDL2160p": "webdl-2160p", "Bluray2160p": "bluray-2160p",
		"Bluray1080pRemux": "remux-1080p", "Bluray2160pRemux": "remux-2160p",
	}
	movie := map[string]string{
		"Unknown": "unknown", "WORKPRINT": "workprint", "CAM": "cam", "TELESYNC": "telesync", "TELECINE": "telecine",
		"DVDSCR": "dvdscr", "REGIONAL": "regional", "SDTV": "sdtv", "DVD": "dvd", "DVDR": "dvd-r",
		"HDTV720p": "hdtv-720p", "HDTV1080p": "hdtv-1080p", "HDTV2160p": "hdtv-2160p",
		"WEBDL480p": "webdl-480p", "WEBDL720p": "webdl-720p", "WEBDL1080p": "webdl-1080p", "WEBDL2160p": "webdl-2160p",
		"WEBRip480p": "webrip-480p", "WEBRip720p": "webrip-720p", "WEBRip1080p": "webrip-1080p", "WEBRip2160p": "webrip-2160p",
		"Bluray480p": "bluray-480p", "Bluray576p": "bluray-576p", "Bluray720p": "bluray-720p", "Bluray1080p": "bluray-1080p",
		"Bluray2160p": "bluray-2160p", "Remux1080p": "remux-1080p", "Remux2160p": "remux-2160p", "BRDISK": "br-disk", "RAWHD": "raw-hd",
	}
	sides := []struct {
		name   string
		kind   Kind
		table  map[string]string
		defs   []qualityDef
		lookup func(string) (qualityDef, bool)
		names  map[string]string // one public input per exposed code
	}{
		{"series", KindSeries, series, func() []qualityDef { d := seriesDefs(); return d[:] }(), seriesQualityDef, map[string]string{
			"unknown": "The.Series.S02E15", "sdtv": "S07E23 .avi ", "dvd": "My series S01E01 [DVD]",
			"webrip-480p": "Series.Title.1x04.ITA.WEBMux.x264-NovaRip", "webdl-480p": "My series S01E01 [WEBDL-480p]",
			"bluray-480p": "My series S01E01 [Bluray-480p]", "bluray-576p": "My series S01E01 [Bluray-576p]",
			"hdtv-720p": "S07E23.mkv ", "webrip-720p": "Series.Title.1x04.ITA.720p.WEBMux.x264-NovaRip",
			"webdl-720p": "S07E23 - [WEBDL].mkv ", "bluray-720p": "My series S01E01 [Bluray-720p]",
			"bluray-1080p": "My series S01E01 [Bluray-1080p]", "hdtv-1080p": "My series S01E01 [HDTV-1080p]",
			"webrip-1080p": "Series Title S01 WEBRip 1080p x266-GROUP", "webdl-1080p": "My series S01E01 [WEBDL-1080p]",
			"raw-hd": "Show - S03E01 - Episode Title Raw-HD.ts", "hdtv-2160p": "My series S01E01 [HDTV-2160p]",
			"webrip-2160p": "Series S01E01.2160P AMZN WEBRIP DD2.0 HI10P X264-TROLLUHD", "webdl-2160p": "My series S01E01 [WEBDL-2160p]",
			"bluray-2160p": "My series S01E01 [Bluray-2160p]", "remux-1080p": "My series S01E01 [Bluray-1080p Remux]",
			"remux-2160p": "My series S01E01 [Bluray-2160p Remux]",
		}},
		{"movie", KindMovie, movie, func() []qualityDef { d := movieDefs(); return d[:] }(), movieQualityDef, map[string]string{
			"unknown": "Movie.Title.2018", "workprint": "Movie.Title.2018.WORKPRINT.x264", "cam": "Movie.Title.2018.CAMRIP.x264",
			"telesync": "Movie.Title.2017.TSRip.x264", "telecine": "Movie.Title.2018.TELECINE.x264",
			"dvdscr": "Movie.Title.2018.DVDSCR.x264", "regional": "Movie.Title.2018.R5.x264", "sdtv": "Movie.Title.2018.SDTV",
			"dvd": "Movie.Title.2018.DVD.x264", "dvd-r": "Movie.Title.2018.DVDR",
			"hdtv-720p": "Movie.Title.2018.720p.HDTV.x264-GRP", "hdtv-1080p": "Movie.Title.2018.1080p.HDTV.x264-GRP",
			"hdtv-2160p": "Movie.Title.2018.2160p.HDTV.x265-GRP",
			"webdl-480p": "Movie.Title.2018.WEB-DL.x264-GRP", "webdl-720p": "Movie.Title.2018.720p.WEB-DL.x264-GRP",
			"webdl-1080p": "Movie.Title.2018.1080p.WEB-DL.x264-GRP", "webdl-2160p": "Movie.Title.2018.2160p.WEB-DL.x265-GRP",
			"webrip-480p": "Movie.Title.2018.WEBRip.x264-GRP", "webrip-720p": "Movie.Title.2018.720p.WEBRip.x264-GRP",
			"webrip-1080p": "Movie.Title.2018.1080p.WEBRip.x264-GRP", "webrip-2160p": "Movie.Title.2018.2160p.WEBRip.x265-GRP",
			"bluray-480p": "Movie.Title.2018.480p.BluRay.x264-GRP", "bluray-576p": "Movie.Title.2018.576p.BluRay.x264-GRP",
			"bluray-720p": "Movie.Title.2018.720p.BluRay.x264-GRP", "bluray-1080p": "Movie.Title.2018.1080p.BluRay.x264-GRP",
			"bluray-2160p": "Movie.Title.2018.2160p.BluRay.x265-GRP",
			"remux-1080p":  "Movie.Title.2016.REMUX.1080p.BluRay.AVC.DTS-HD.MA.5.1-iFT",
			"remux-2160p":  "Movie.Title.2016.REMUX.2160p.BluRay.HEVC.DTS-HD.MA.5.1-iFT",
			"br-disk":      "Movie.Title.2018.BR-DISK", "raw-hd": "Movie.Title.2018.RawHD.x264",
		}},
	}
	p := newTestParser(t, time.Now)
	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			if len(side.defs) != len(side.table) {
				t.Errorf("%d members, independent table has %d", len(side.defs), len(side.table))
			}
			for member, code := range side.table {
				d, ok := side.lookup(member)
				if !ok || d.code != code {
					t.Errorf("member %s = %q (found %v), want %q", member, d.code, ok, code)
				}
			}
			exposed := map[string]bool{}
			for _, d := range side.defs {
				exposed[d.code] = true
			}
			for _, code := range side.table {
				if !exposed[code] {
					t.Errorf("independent code %q is not exposed by the vocabulary", code)
				}
				name, ok := side.names[code]
				if !ok {
					t.Errorf("no public input for code %q", code)
					continue
				}
				if q, err := p.ParseQuality(context.Background(), name, side.kind); err != nil || q.Code != code {
					t.Errorf("%q: code %q, %v; want %q", name, q.Code, err, code)
				}
			}
			if len(side.names) != len(side.table) {
				t.Errorf("%d public inputs for %d codes", len(side.names), len(side.table))
			}
		})
	}
}

func TestLanguageVocabularyMatchesIndependentMemberTable(t *testing.T) {
	// Fixed by hand from the Language member names (in declaration order),
	// not computed from vocab.go.
	table := map[string]string{
		"Unknown": "unknown", "English": "english", "French": "french", "Spanish": "spanish", "German": "german",
		"Italian": "italian", "Danish": "danish", "Dutch": "dutch", "Japanese": "japanese", "Icelandic": "icelandic",
		"Chinese": "chinese", "Russian": "russian", "Polish": "polish", "Vietnamese": "vietnamese", "Swedish": "swedish",
		"Norwegian": "norwegian", "Finnish": "finnish", "Turkish": "turkish", "Portuguese": "portuguese", "Flemish": "flemish",
		"Greek": "greek", "Korean": "korean", "Hungarian": "hungarian", "Hebrew": "hebrew", "Lithuanian": "lithuanian",
		"Czech": "czech", "Hindi": "hindi", "Romanian": "romanian", "Thai": "thai", "Bulgarian": "bulgarian",
		"PortugueseBR": "portuguese-br", "Arabic": "arabic", "Ukrainian": "ukrainian", "Persian": "persian", "Bengali": "bengali",
		"Slovak": "slovak", "Latvian": "latvian", "SpanishLatino": "spanish-latino", "Catalan": "catalan", "Croatian": "croatian",
		"Serbian": "serbian", "Bosnian": "bosnian", "Estonian": "estonian", "Tamil": "tamil", "Indonesian": "indonesian",
		"Telugu": "telugu", "Macedonian": "macedonian", "Slovenian": "slovenian", "Malayalam": "malayalam", "Kannada": "kannada",
		"Albanian": "albanian", "Afrikaans": "afrikaans", "Marathi": "marathi", "Tagalog": "tagalog", "Urdu": "urdu",
		"Romansh": "romansh", "Mongolian": "mongolian", "Georgian": "georgian", "Any": "any", "Original": "original",
		"Azerbaijani": "azerbaijani", "Uzbek": "uzbek", "Irish": "irish",
	}
	defs := languageDefs()
	if len(defs) != len(table) {
		t.Errorf("%d vocabulary members, independent table has %d", len(defs), len(table))
	}
	seen := map[string]bool{}
	for _, d := range defs {
		want, ok := table[d.member]
		if !ok {
			t.Errorf("vocabulary member %q is not in the independent table", d.member)
		} else if d.code != want {
			t.Errorf("member %s = %q, want %q", d.member, d.code, want)
		}
		if seen[d.code] {
			t.Errorf("code %q is used by two members", d.code)
		}
		seen[d.code] = true
	}
	for member, want := range table {
		if code, ok := languageCode(member); !ok || code != want {
			t.Errorf("languageCode(%s) = %q, %v; want %q", member, code, ok, want)
		}
	}
	// The series fixtures name the Brazilian Portuguese member PortugueseBrazil.
	if code, ok := languageCode("PortugueseBrazil"); !ok || code != "portuguese-br" {
		t.Errorf("languageCode(PortugueseBrazil) = %q, %v; want portuguese-br", code, ok)
	}
	if code, ok := languageCode("Portuguese (Brazil)"); ok {
		t.Errorf("the display name has no mapping, got %q", code)
	}
}

// ---- title corpus consumer ----------------------------------------------------------

// Reserved Expected keys of title cases beside the input key: the assertion on
// the result of an entry point itself, and the isDir argument of ParseMovieTitle.
const (
	titleResultKey = "$result"
	titleDirKey    = "$isDir"
)

// The title-scope entry points of the committed movie corpus.
const (
	ownerMovieTitle   = "Parser.ParseMovieTitle"
	ownerReleaseGroup = "ReleaseGroupParser.ParseReleaseGroup"
	ownerLanguages    = "LanguageParser.ParseLanguages"
	ownerCleanTitle   = "Parser.CleanMovieTitle"
)

// absentValue is observed where there is nothing to read (a null result
// or an index past the list): no expectation, null included, equals it.
var absentValue = corpus.Value{Kind: corpus.KindEnum, Type: "$absent", Str: "no value"}

// titleResult is what one title entry point produced for one input.
type titleResult struct {
	movie     *MovieInfo   // ParseMovieTitle: nil when the name has no movie title
	episode   *EpisodeInfo // series ParseTitle: nil when the name has no episode
	group     string       // ParseReleaseGroup: empty where there is no group
	languages []string     // ParseLanguages
	clean     string       // CleanMovieTitle, CleanSeriesTitle
	name      string       // ParseSeriesName
}

// observeTitle runs the entry point a title case names on one input. A name
// ParseMovieTitle rejects has no movie (a nil movie, no error); any
// other failure is an error.
func observeTitle(p *Parser, owner string, dir bool, name string) (titleResult, error) {
	ctx := context.Background()
	switch owner {
	case ownerMovieTitle:
		var (
			pt  ParsedTitle
			err error
		)
		if dir {
			pt, err = p.parseTitle(ctx, name, KindMovie, true)
		} else {
			pt, err = p.ParseTitle(ctx, name, KindMovie)
		}
		var re *ReasonError
		switch {
		case errors.As(err, &re) && re.Reason.Code == codeTitleUnrecognized:
			return titleResult{}, nil
		case err != nil:
			return titleResult{}, err
		case pt.Kind != KindMovie || pt.Movie == nil:
			return titleResult{}, fmt.Errorf("ParseTitle succeeded without a movie: %+v", pt)
		}
		return titleResult{movie: pt.Movie}, nil
	case ownerCleanTitle:
		if err := validateName(name); err != nil {
			return titleResult{}, err
		}
		clean, err := cleanMovieTitle(name)
		return titleResult{clean: clean}, err
	case ownerReleaseGroup, ownerLanguages:
		if err := validateName(name); err != nil {
			return titleResult{}, err
		}
		b := newBudget(ctx, p.now)
		if owner == ownerReleaseGroup {
			g, err := p.movieTitle.group.parse(b, p.movieTitle, name)
			return titleResult{group: g}, err
		}
		l, err := p.movieTitle.lang.parse(b, name)
		return titleResult{languages: l}, err
	}
	return titleResult{}, fmt.Errorf("unsupported title entry point %q", owner)
}

// resolveTitle maps one asserted property of a title entry point to the
// native comparison: the expectation as a Boxset value (language members
// through the explicit code map; a null string is the empty text)
// and the observed native value. An unknown property, entry point, enum type or
// member is a hard error, never a skipped assertion.
func resolveTitle(owner, key string, want corpus.Value, res titleResult) (wantNative, observed corpus.Value, err error) {
	text := func(got string) (corpus.Value, corpus.Value, error) {
		switch want.Kind {
		case corpus.KindString:
			return want, str(got), nil
		case corpus.KindNull:
			return str(""), str(got), nil
		}
		return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected a string or null, got %+v", key, want)
	}
	number := func(got int) (corpus.Value, corpus.Value, error) {
		if want.Kind != corpus.KindInt {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected an int, got %+v", key, want)
		}
		return want, corpus.Value{Kind: corpus.KindInt, Int: int64(got)}, nil
	}
	language := func(v corpus.Value) (string, error) {
		if v.Kind != corpus.KindEnum || v.Type != "Language" {
			return "", fmt.Errorf("%s: expected a Language enum, got %+v", key, v)
		}
		code, ok := languageCode(v.Str)
		if !ok {
			return "", fmt.Errorf("%s: Language.%s has no explicit code mapping", key, v.Str)
		}
		return code, nil
	}
	first := func(list []string) string {
		if len(list) == 0 {
			return ""
		}
		return list[0]
	}
	// listProperty resolves the language list assertions of both entry points.
	listProperty := func(list []string, contain, firstKey string) (corpus.Value, corpus.Value, bool, error) {
		switch key {
		case contain:
			if want.Kind != corpus.KindArray {
				return corpus.Value{}, corpus.Value{}, true, fmt.Errorf("%s: expected an array, got %+v", key, want)
			}
			wanted := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
			got := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
			contained := true
			for _, item := range want.Items {
				code, err := language(item)
				if err != nil {
					return corpus.Value{}, corpus.Value{}, true, err
				}
				wanted.Items = append(wanted.Items, str(code))
				contained = contained && slices.Contains(list, code)
			}
			for _, code := range list {
				got.Items = append(got.Items, str(code))
			}
			if contained {
				got = wanted
			}
			return wanted, got, true, nil
		case firstKey:
			code, err := language(want)
			return str(code), str(first(list)), true, err
		}
		return corpus.Value{}, corpus.Value{}, false, nil
	}

	switch owner {
	case ownerReleaseGroup:
		if key == titleResultKey {
			return text(res.group)
		}
	case ownerCleanTitle:
		if key == titleResultKey {
			return text(res.clean)
		}
	case ownerLanguages:
		if w, o, ok, err := listProperty(res.languages, titleResultKey+".Contain", "First().Id"); ok {
			return w, o, err
		}
	case ownerMovieTitle:
		if key == titleResultKey {
			if want.Kind != corpus.KindNull {
				return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: only a null result is asserted, got %+v", key, want)
			}
			observed := str("null")
			if res.movie != nil {
				observed = str("a movie")
			}
			return str("null"), observed, nil
		}
		m := res.movie
		if m == nil {
			// A null result is dereferenced: every property assertion fails,
			// whatever it expects. Only an explicit $result null compares as null.
			return want, absentValue, nil
		}
		switch key {
		case "PrimaryMovieTitle":
			return text(first(m.MovieTitles))
		case "ReleaseGroup":
			return text(m.ReleaseGroup)
		case "Edition":
			return text(m.Edition)
		case "HardcodedSubs":
			return text(m.HardcodedSubs)
		case "ImdbId":
			return text(m.ImdbID)
		case "Year":
			return number(m.Year)
		case "TmdbId":
			return number(m.TmdbID)
		case "MovieTitle.CleanMovieTitle":
			// Both the parsed primary title and the expected title are normalized.
			if want.Kind != corpus.KindString {
				return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected a string, got %+v", key, want)
			}
			w, err := cleanMovieTitle(want.Str)
			if err != nil {
				return corpus.Value{}, corpus.Value{}, err
			}
			o, err := cleanMovieTitle(first(m.MovieTitles))
			if err != nil {
				return corpus.Value{}, corpus.Value{}, err
			}
			return str(w), str(o), nil
		case "MovieTitles.Count":
			return number(len(m.MovieTitles))
		case "Languages.Count":
			return number(len(m.Languages))
		}
		if w, o, ok, err := listProperty(m.Languages, "Languages.Contain", "Languages.First()"); ok {
			return w, o, err
		}
		var i int
		if n, scanErr := fmt.Sscanf(key, "MovieTitles[%d]", &i); n == 1 && scanErr == nil && strings.HasSuffix(key, "]") {
			if i >= len(m.MovieTitles) {
				// An index past the list fails the assertion.
				return want, absentValue, nil
			}
			return text(m.MovieTitles[i])
		}
	}
	return corpus.Value{}, corpus.Value{}, fmt.Errorf("unsupported asserted property %q of %s", key, owner)
}

// cleanMovieTitleRe is the normalization pattern: the common words (a, an,
// the, and, or, of) that are neither first, last nor part of an acronym, and
// every non-word character and underscore.
var cleanMovieTitleRe = func() *regexp2.Regexp {
	re, err := regexp2.Compile(`((?:\b|_)(?<!^|[^a-zA-Z0-9_']\w[^a-zA-Z0-9_'])([aà](?!$|[^a-zA-Z0-9_']\w[^a-zA-Z0-9_'])|an|the|and|or|of)(?!$)(?:\b|_))|\W|_`, ic)
	if err != nil {
		panic(err)
	}
	re.MatchTimeout = matchTimeout
	return re
}()

var germanUmlauts = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss")

// accentFold maps each lower-case letter of Latin-1 Supplement and Latin
// Extended-A that has a canonical decomposition to its base letter, and ð to d
// (it has no decomposition, but a fixture asserts that "Ríkið" cleans to
// "rikid"). The other letters without a decomposition (ø, æ, œ, ł, đ, ħ, ŋ, ŧ,
// ı, ĳ, ĸ, ŀ, ŉ, ſ) are deliberately absent: normalization form D leaves them
// as they are.
var accentFold = func() *strings.Replacer {
	var pairs []string
	for base, accented := range map[string]string{
		"a": "àáâãåāăą", "c": "çćĉċč", "d": "ďð", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥ", "i": "ìíîïĩīĭį",
		"j": "ĵ", "k": "ķ", "l": "ĺļľ", "n": "ñńņň", "o": "òóôõōŏő", "r": "ŕŗř", "s": "śŝşš", "t": "ţť",
		"u": "ùúûũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range accented {
			pairs = append(pairs, string(r), base)
		}
	}
	return strings.NewReplacer(pairs...)
}()

// removeAccent removes accents (decompose, drop the non-spacing
// marks, recompose) for lower-case text, with the standard library only: the
// letters of accentFold lose their accent and a combining mark of already
// decomposed text is dropped. The standard library has no decomposition table
// beyond that, so any other non-ASCII letter is an error: it might carry a
// decomposition, and a silently unfolded title would be wrong.
func removeAccent(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, accentFold.Replace(s))
	for _, r := range s {
		if r > unicode.MaxASCII {
			return "", fmt.Errorf("RemoveAccent(%q): %q is outside the Latin letters whose decomposition is handled", s, r)
		}
	}
	return s, nil
}

// cleanMovieTitle normalizes a title: a blank or all-digit title
// is returned as it is, otherwise the normalized title is lower-cased, its
// German umlauts spelled out and its accents removed.
func cleanMovieTitle(title string) (string, error) {
	if isBlank(title) {
		return title, nil
	}
	if _, err := strconv.ParseInt(strings.Trim(title, " \t\n\v\f\r"), 10, 64); err == nil {
		return title, nil
	}
	out, err := cleanMovieTitleRe.Replace(title, "", -1, -1)
	if err != nil {
		return "", err
	}
	return removeAccent(germanUmlauts.Replace(strings.ToLower(out)))
}

// titleRun is the outcome of one corpus through one parser. The selected scope
// is title: cases counts what ran. other counts the quality cases (owned by the
// quality consumer), so the whole inventory reconciles separately from the
// selected scope.
type titleRun struct {
	inputs                   int
	passed, disposed, failed int
	cases, other             int
	privateDir               int // passed or disposed cases proved on the private isDir adapter, not public ParseTitle
	problems                 []string
	observe                  func(p *Parser, owner string, dir bool, name string) (titleResult, error)
	resolve                  func(owner, key string, want corpus.Value, res titleResult, tc corpus.TimeContext) (corpus.Value, corpus.Value, error)
}

func (r *titleRun) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// runTitleCorpus runs every movie title case of c through p. A case passes only
// when every assertion holds for every input; a mismatch needs a disposition
// (byID) binding every distinct mismatching observation.
func runTitleCorpus(p *Parser, c *corpus.Corpus, byID map[string]corpus.Disposition) titleRun {
	// Movie title names carry no symbolic date; the context is only the shared contract.
	tc := corpus.TimeContext{Now: time.Now(), Location: time.Local}
	return runTitleScope(titleRun{observe: observeTitle, resolve: resolveMovieTitle}, p, c, byID, tc)
}

// runSeriesCorpus is runTitleCorpus for the series corpus. tc is the clock of the
// run: the symbolic dates of the names are materialized with it, so p must read
// the same instant and calendar.
func runSeriesCorpus(p *Parser, c *corpus.Corpus, byID map[string]corpus.Disposition, tc corpus.TimeContext) titleRun {
	return runTitleScope(titleRun{observe: observeSeriesTitle, resolve: resolveSeriesTitle}, p, c, byID, tc)
}

func runTitleScope(r titleRun, p *Parser, c *corpus.Corpus, byID map[string]corpus.Disposition, tc corpus.TimeContext) titleRun {
	for _, cs := range c.Cases {
		switch {
		case cs.Scope != corpus.ScopeTitle:
			r.other++
		default:
			r.cases++
			r.runCase(p, cs, tc, byID)
		}
	}
	return r
}

func (r *titleRun) runCase(p *Parser, cs corpus.Case, tc corpus.TimeContext, byID map[string]corpus.Disposition) {
	names, err := cs.Inputs(tc)
	if err != nil {
		r.problem("%s: %v", cs.ID, err)
		r.failed++
		return
	}
	dir := cs.Expected[titleDirKey].Bool
	d, has := byID[cs.ID]
	mismatch := map[string][]corpus.Value{} // property -> distinct observed values
	bad := false
	for _, name := range names {
		r.inputs++
		res, err := r.observe(p, cs.ScopeOwner, dir, name)
		if err != nil {
			r.problem("%s %q: %v", cs.ID, name, err)
			bad = true
			continue
		}
		for _, key := range corpus.SortedKeys(cs.Expected) {
			if key == corpus.InputKey || key == titleDirKey {
				continue
			}
			wantNative, observed, err := r.resolve(cs.ScopeOwner, key, cs.Expected[key], res, tc)
			if err != nil {
				r.problem("%s: %v", cs.ID, err)
				bad = true
				continue
			}
			if corpus.Equal(wantNative, observed) {
				continue
			}
			if !slices.ContainsFunc(mismatch[key], func(v corpus.Value) bool { return corpus.Equal(v, observed) }) {
				mismatch[key] = append(mismatch[key], observed)
			}
			if !has {
				r.problem("%s %q: %s = %+v, expects %+v", cs.ID, name, key, observed, wantNative)
			}
		}
	}
	switch {
	case bad:
		r.failed++
	case len(mismatch) == 0 && !has:
		r.passed++
		r.privateDir += b2i(dir)
	case len(mismatch) == 0:
		r.problem("%s: disposition is stale, the case passes", cs.ID)
		r.failed++
	case !has:
		r.failed++
	case !dispositionBinds(d, mismatch):
		r.problem("%s: disposition does not bind every observed value %v", cs.ID, mismatch)
		r.failed++
	default:
		r.disposed++
		r.privateDir += b2i(dir)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// reconcile compares the run with the per-scope case counts of its corpus: the
// whole inventory (both scopes), and, separately, the selected title scope.
func (r titleRun) reconcile(counts map[string]int) []string {
	var out []string
	check := func(name string, got, want int) {
		if got != want {
			out = append(out, fmt.Sprintf("inventory %s = %d, corpus %d", name, got, want))
		}
	}
	check(corpus.ScopeTitle, r.cases, counts[corpus.ScopeTitle])
	check(corpus.ScopeQuality, r.other, counts[corpus.ScopeQuality])
	if r.cases == 0 {
		out = append(out, "selected title scope has no case")
	}
	if r.passed+r.disposed+r.failed != r.cases {
		out = append(out, fmt.Sprintf("selected scope: passed %d + disposed %d + failed %d != cases %d", r.passed, r.disposed, r.failed, r.cases))
	}
	if r.failed != 0 || r.passed == 0 {
		out = append(out, fmt.Sprintf("selected scope: failed=%d passed=%d", r.failed, r.passed))
	}
	return out
}

// resolveMovieTitle is resolveTitle as a titleRun resolver: the movie corpus has
// no symbolic date in a title assertion.
func resolveMovieTitle(owner, key string, want corpus.Value, res titleResult, _ corpus.TimeContext) (corpus.Value, corpus.Value, error) {
	return resolveTitle(owner, key, want, res)
}

// ---- series title corpus consumer ---------------------------------------------------

// The title-scope entry points of the committed series corpus. The release group
// and language parsers share ownerReleaseGroup and ownerLanguages with the movie
// corpus.
const (
	ownerSeriesTitle = "Parser.ParseTitle"
	ownerSeriesName  = "Parser.ParseSeriesName"
	ownerCleanSeries = "Parser.CleanSeriesTitle"
)

// newSeriesTestParser is a Parser whose clock reads the fixed instant now and
// whose local calendar is loc: the instant and calendar of a series corpus run,
// so the symbolic dates of its names and the parser's air date limit agree. A
// fixed clock never exhausts the elapsed-work budget; the budget has its own
// tests.
func newSeriesTestParser(t *testing.T, now time.Time, loc *time.Location) *Parser {
	t.Helper()
	p, err := NewParser(Options{Now: func() time.Time { return now }, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func intValue(n int) corpus.Value   { return corpus.Value{Kind: corpus.KindInt, Int: int64(n)} }
func boolValue(b bool) corpus.Value { return corpus.Value{Kind: corpus.KindBool, Bool: b} }

// cleanSeriesRe is the normalization pattern of cleanSeriesTitle: the common
// words (a, an, the, and, or, of) that are neither first nor last, and every
// non-word character and underscore; percentRe is its percent rule.
var cleanSeriesRe, percentRe = func() (*regexp2.Regexp, *regexp2.Regexp) {
	clean, err := regexp2.Compile(`((?:\b|_)(?<!^)([aà](?!$)|an|the|and|or|of)(?!$)(?:\b|_))|\W|_`, ic)
	if err != nil {
		panic(err)
	}
	percent, err := regexp2.Compile(`(?<=\b\d+)%`, regexp2.None)
	if err != nil {
		panic(err)
	}
	clean.MatchTimeout, percent.MatchTimeout = matchTimeout, matchTimeout
	return clean, percent
}()

// cleanSeriesTitle normalizes a title: a blank or all-digit title
// is returned as it is, otherwise "%" after digits reads "percent", the
// normalized title is lower-cased and its accents removed.
func cleanSeriesTitle(title string) (string, error) {
	if isBlank(title) {
		return title, nil
	}
	if _, err := strconv.ParseInt(strings.Trim(title, " \t\n\v\f\r"), 10, 64); err == nil {
		return title, nil
	}
	title, err := percentRe.Replace(title, "percent", -1, -1)
	if err != nil {
		return "", err
	}
	out, err := cleanSeriesRe.Replace(title, "", -1, -1)
	if err != nil {
		return "", err
	}
	return removeAccent(strings.ToLower(out))
}

// observeSeriesTitle runs the entry point a series title case names on one
// input. A name ParseTitle rejects has no episode (a nil episode, no
// error); any other failure is an error.
func observeSeriesTitle(p *Parser, owner string, _ bool, name string) (titleResult, error) {
	ctx := context.Background()
	switch owner {
	case ownerSeriesTitle, ownerSeriesName:
		pt, err := p.ParseTitle(ctx, name, KindSeries)
		var re *ReasonError
		switch {
		case errors.As(err, &re) && re.Reason.Code == codeTitleUnrecognized:
			// No episode: the name has no title.
		case err != nil:
			return titleResult{}, err
		case pt.Kind != KindSeries || pt.Episode == nil || pt.Movie != nil:
			return titleResult{}, fmt.Errorf("ParseTitle succeeded without exactly one episode: %+v", pt)
		}
		if owner == ownerSeriesTitle {
			return titleResult{episode: pt.Episode}, nil
		}
		// ParseSeriesName: the parsed series title, or the cleaned name where the
		// title does not parse.
		if pt.Episode != nil {
			return titleResult{name: pt.Episode.SeriesTitle}, nil
		}
		cleaned, err := cleanSeriesTitle(name)
		return titleResult{name: cleaned}, err
	case ownerCleanSeries:
		if err := validateName(name); err != nil {
			return titleResult{}, err
		}
		clean, err := cleanSeriesTitle(name)
		return titleResult{clean: clean}, err
	case ownerReleaseGroup, ownerLanguages:
		if err := validateName(name); err != nil {
			return titleResult{}, err
		}
		b := newBudget(ctx, p.now)
		if owner == ownerReleaseGroup {
			g, err := p.seriesTitle.group.parse(b, p.seriesTitle, name)
			return titleResult{group: g}, err
		}
		l, err := p.seriesTitle.lang.parse(b, name)
		return titleResult{languages: l}, err
	}
	return titleResult{}, fmt.Errorf("unsupported series title entry point %q", owner)
}

// canonicalDecimal is the exact value of a decimal text: no leading zeros of the
// integer part and no trailing zeros of the fraction, so 5.50 and 5.5 are one
// number as they are to a decimal comparison.
func canonicalDecimal(s string) string {
	whole, frac, _ := strings.Cut(s, ".")
	if whole = strings.TrimLeft(whole, "0"); whole == "" {
		whole = "0"
	}
	if frac = strings.TrimRight(frac, "0"); frac != "" {
		return whole + "." + frac
	}
	return whole
}

// seriesList compares a list property: want must be an array of items of one
// kind, each mapped by native to the observed side's comparable form; an
// equivalent list is compared without order.
func seriesList(key string, want corpus.Value, got []corpus.Value, native func(corpus.Value) (corpus.Value, error), ordered bool) (corpus.Value, corpus.Value, error) {
	if want.Kind != corpus.KindArray {
		return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected an array, got %+v", key, want)
	}
	wanted := make([]corpus.Value, 0, len(want.Items))
	for _, item := range want.Items {
		v, err := native(item)
		if err != nil {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: %w", key, err)
		}
		wanted = append(wanted, v)
	}
	if !ordered {
		less := func(a, b corpus.Value) int {
			return strings.Compare(fmt.Sprint(a.Int, a.Str), fmt.Sprint(b.Int, b.Str))
		}
		slices.SortFunc(wanted, less)
		got = slices.Clone(got)
		slices.SortFunc(got, less)
	}
	return corpus.Value{Kind: corpus.KindArray, Items: wanted}, corpus.Value{Kind: corpus.KindArray, Items: got}, nil
}

// languageCodeOf maps a Language enum value of an assertion to its Boxset code.
func languageCodeOf(v corpus.Value) (corpus.Value, error) {
	if v.Kind != corpus.KindEnum || v.Type != "Language" {
		return corpus.Value{}, fmt.Errorf("expected a Language enum, got %+v", v)
	}
	code, ok := languageCode(v.Str)
	if !ok {
		return corpus.Value{}, fmt.Errorf("Language.%s has no explicit code mapping", v.Str)
	}
	return str(code), nil
}

// languageValues are the codes of a language list as comparable values.
func languageValues(list []string) []corpus.Value {
	out := make([]corpus.Value, len(list))
	for i, code := range list {
		out[i] = str(code)
	}
	return out
}

// languageContain resolves a Contain assertion: every wanted language must be in
// the observed list.
func languageContain(key string, want corpus.Value, list []string) (corpus.Value, corpus.Value, error) {
	wanted, got, err := seriesList(key, want, languageValues(list), languageCodeOf, true)
	if err != nil {
		return corpus.Value{}, corpus.Value{}, err
	}
	contained := true
	for _, w := range wanted.Items {
		contained = contained && slices.ContainsFunc(got.Items, func(g corpus.Value) bool { return corpus.Equal(w, g) })
	}
	if contained {
		got = wanted
	}
	return wanted, got, nil
}

// resolveSeriesTitle maps one asserted property of a series title entry
// point to the native comparison: the expectation as a Boxset value
// (language members through the explicit code map, a symbolic date through tc,
// a null string as the empty text) and the observed native value. An
// unknown property, entry point, enum type or member is a hard error, never a
// skipped assertion.
func resolveSeriesTitle(owner, key string, want corpus.Value, res titleResult, tc corpus.TimeContext) (corpus.Value, corpus.Value, error) {
	switch owner {
	case ownerReleaseGroup, ownerCleanSeries:
		if key == titleResultKey {
			got := res.group
			if owner == ownerCleanSeries {
				got = res.clean
				// The fixture holds the substituted group in a lower-case expected
				// title; the normalized title is lower case.
				if want.Kind == corpus.KindString {
					want.Str = strings.ReplaceAll(want.Str, substitutedGroup, strings.ToLower(substitutedGroup))
				}
			}
			return seriesText(key, want, got)
		}
	case ownerSeriesName:
		if key == "SeriesName.CleanSeriesTitle" {
			// Both the parsed series title and the expected title are normalized.
			if want.Kind != corpus.KindString {
				return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected a string, got %+v", key, want)
			}
			w, err := cleanSeriesTitle(want.Str)
			if err != nil {
				return corpus.Value{}, corpus.Value{}, err
			}
			o, err := cleanSeriesTitle(res.name)
			if err != nil {
				return corpus.Value{}, corpus.Value{}, err
			}
			return str(w), str(o), nil
		}
	case ownerLanguages:
		switch key {
		case titleResultKey + ".Contain":
			return languageContain(key, want, res.languages)
		case titleResultKey + ".EquivalentTo":
			return seriesList(key, want, languageValues(res.languages), languageCodeOf, false)
		}
	case ownerSeriesTitle:
		return resolveEpisode(key, want, res.episode, tc)
	}
	return corpus.Value{}, corpus.Value{}, fmt.Errorf("unsupported asserted property %q of %s", key, owner)
}

// substitutedGroup is the neutral release group the series fixtures hold wherever
// a group name would name an upstream product.
const substitutedGroup = "Neutral"

// seriesText compares a text property: a null string is the empty text.
func seriesText(key string, want corpus.Value, got string) (corpus.Value, corpus.Value, error) {
	switch want.Kind {
	case corpus.KindString:
		return want, str(got), nil
	case corpus.KindNull:
		return str(""), str(got), nil
	}
	return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected a string or null, got %+v", key, want)
}

// resolveEpisode resolves one property assertion on the result of the series
// ParseTitle. A missing episode fails every property
// assertion, whatever it expects: only the explicit null result, the not-null
// assertion and the no-throw requirement are about the result itself.
func resolveEpisode(key string, want corpus.Value, ep *EpisodeInfo, tc corpus.TimeContext) (corpus.Value, corpus.Value, error) {
	bad := func(what string) (corpus.Value, corpus.Value, error) {
		return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: expected %s, got %+v", key, what, want)
	}
	switch key {
	case titleResultKey:
		if want.Kind != corpus.KindNull {
			return bad("a null result")
		}
		observed := str("null")
		if ep != nil {
			observed = str("an episode")
		}
		return str("null"), observed, nil
	case titleResultKey + ".NotNull":
		if want.Kind != corpus.KindBool || !want.Bool {
			return bad("true")
		}
		return want, boolValue(ep != nil), nil
	case titleResultKey + ".NoThrow":
		// The call completed without an error, or the case would have failed already.
		return want, want, nil
	}
	if ep == nil {
		// A null result is dereferenced: the assertion fails, whatever it expects.
		return want, absentValue, nil
	}

	text := func(got string) (corpus.Value, corpus.Value, error) { return seriesText(key, want, got) }
	number := func(got int) (corpus.Value, corpus.Value, error) {
		if want.Kind != corpus.KindInt {
			return bad("an int")
		}
		return want, intValue(got), nil
	}
	boolean := func(got bool) (corpus.Value, corpus.Value, error) {
		if want.Kind != corpus.KindBool {
			return bad("a bool")
		}
		return want, boolValue(got), nil
	}
	// optional is a nullable int: a null equals a null expectation only.
	optional := func(got int, ok bool) (corpus.Value, corpus.Value, error) {
		switch {
		case want.Kind == corpus.KindNull && !ok:
			return want, want, nil
		case want.Kind == corpus.KindNull:
			return want, intValue(got), nil
		case want.Kind != corpus.KindInt:
			return bad("an int or null")
		case !ok:
			return want, absentValue, nil
		}
		return want, intValue(got), nil
	}
	ints := func(list []int, ordered bool) (corpus.Value, corpus.Value, error) {
		got := make([]corpus.Value, len(list))
		for i, n := range list {
			got[i] = intValue(n)
		}
		return seriesList(key, want, got, func(v corpus.Value) (corpus.Value, error) {
			if v.Kind != corpus.KindInt {
				return v, fmt.Errorf("expected an int, got %+v", v)
			}
			return v, nil
		}, ordered)
	}
	strs := func(list []string) (corpus.Value, corpus.Value, error) {
		got := make([]corpus.Value, len(list))
		for i, s := range list {
			got[i] = str(s)
		}
		return seriesList(key, want, got, func(v corpus.Value) (corpus.Value, error) {
			if v.Kind != corpus.KindString {
				return v, fmt.Errorf("expected a string, got %+v", v)
			}
			return v, nil
		}, false)
	}
	decimals := func(list []string) []corpus.Value {
		got := make([]corpus.Value, len(list))
		for i, s := range list {
			got[i] = corpus.Value{Kind: corpus.KindDecimal, Str: canonicalDecimal(s)}
		}
		return got
	}
	decimalOf := func(v corpus.Value) (corpus.Value, error) {
		if v.Kind != corpus.KindDecimal {
			return v, fmt.Errorf("expected a decimal, got %+v", v)
		}
		return corpus.Value{Kind: corpus.KindDecimal, Str: canonicalDecimal(v.Str)}, nil
	}
	// first and single read an element the way First, Single and
	// SingleOrDefault do: where it throws, the assertion fails.
	first := func(list []int) (corpus.Value, corpus.Value, error) {
		if len(list) == 0 {
			return number(0)
		}
		return number(list[0])
	}
	count := func(n int) (corpus.Value, corpus.Value, error) { return number(n) }

	info := ep.SeriesTitleInfo
	switch key {
	case "SeriesTitle":
		return text(ep.SeriesTitle)
	case "ReleaseGroup":
		return text(ep.ReleaseGroup)
	case "ReleaseHash":
		return text(ep.ReleaseHash)
	case "ReleaseTitle":
		return text(ep.ReleaseTitle)
	case "SeriesTitleInfo.Title":
		return text(info.Title)
	case "SeriesTitleInfo.TitleWithoutYear":
		return text(info.TitleWithoutYear)
	case "SeriesTitleInfo.Title==SeriesTitleInfo.TitleWithoutYear":
		return boolean(info.Title == info.TitleWithoutYear)
	case "SeriesTitleInfo.Year":
		return number(info.Year)
	case "SeriesTitleInfo.AllTitles.EquivalentTo":
		return strs(info.AllTitles)
	case "AirDate":
		if want.Kind == corpus.KindDate {
			s, err := tc.Materialize(want)
			if err != nil {
				return corpus.Value{}, corpus.Value{}, err
			}
			return str(s), str(ep.AirDate), nil
		}
		return text(ep.AirDate)
	case "AirDate.NotNull":
		return boolean(ep.AirDate != "")
	case "DailyPart":
		if ep.DailyPart == nil {
			return optional(0, false)
		}
		return optional(*ep.DailyPart, true)
	case "SeasonNumber":
		n, ok := ep.SeasonNumber()
		return optional(n, ok)
	case "SeasonNumbers.Equal":
		return ints(ep.SeasonNumbers, true)
	case "SeasonPart":
		return number(ep.SeasonPart)
	case "EpisodeNumbers.Count":
		return count(len(ep.EpisodeNumbers))
	case "EpisodeNumbers.First()":
		if len(ep.EpisodeNumbers) == 0 {
			return want, absentValue, nil
		}
		return first(ep.EpisodeNumbers)
	case "EpisodeNumbers.SingleOrDefault()":
		if len(ep.EpisodeNumbers) > 1 {
			return want, absentValue, nil
		}
		return first(ep.EpisodeNumbers)
	case "EpisodeNumbers.EquivalentTo":
		return ints(ep.EpisodeNumbers, false)
	case "AbsoluteEpisodeNumbers.Count":
		return count(len(ep.AbsoluteEpisodeNumbers))
	case "AbsoluteEpisodeNumbers.Single()":
		if len(ep.AbsoluteEpisodeNumbers) != 1 {
			return want, absentValue, nil
		}
		return first(ep.AbsoluteEpisodeNumbers)
	case "AbsoluteEpisodeNumbers.EquivalentTo":
		return ints(ep.AbsoluteEpisodeNumbers, false)
	case "AbsoluteEpisodeNumbers.NotEmpty":
		return boolean(len(ep.AbsoluteEpisodeNumbers) > 0)
	case "SpecialAbsoluteEpisodeNumbers.Count":
		return count(len(ep.SpecialAbsoluteEpisodeNumbers))
	case "SpecialAbsoluteEpisodeNumbers.NotEmpty":
		return boolean(len(ep.SpecialAbsoluteEpisodeNumbers) > 0)
	case "SpecialAbsoluteEpisodeNumbers.First()":
		if len(ep.SpecialAbsoluteEpisodeNumbers) == 0 {
			return want, absentValue, nil
		}
		w, err := decimalOf(want)
		if err != nil {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: %w", key, err)
		}
		return w, decimals(ep.SpecialAbsoluteEpisodeNumbers)[0], nil
	case "SpecialAbsoluteEpisodeNumbers.EquivalentTo":
		return seriesList(key, want, decimals(ep.SpecialAbsoluteEpisodeNumbers), decimalOf, false)
	case "Languages.Count":
		return count(len(ep.Languages))
	case "Languages.Contain":
		return languageContain(key, want, ep.Languages)
	case "FullSeason":
		return boolean(ep.FullSeason)
	case "IsMultiSeason":
		return boolean(ep.IsMultiSeason())
	case "IsPartialSeason":
		return boolean(ep.IsPartialSeason)
	case "IsPossibleSpecialEpisode":
		return boolean(ep.IsPossibleSpecialEpisode())
	case "IsSeasonExtra":
		return boolean(ep.IsSeasonExtra)
	case "IsSeasonTitle":
		return boolean(ep.IsSeasonTitle)
	case "IsSplitEpisode":
		return boolean(ep.IsSplitEpisode)
	case "Special":
		return boolean(ep.Special)
	case "Quality.Quality.NotBe":
		if want.Kind != corpus.KindEnum || want.Type != "Quality" {
			return bad("a Quality enum")
		}
		def, ok := seriesQualityDef(want.Str)
		if !ok {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: Quality.%s has no explicit code mapping", key, want.Str)
		}
		return boolValue(true), boolValue(ep.Quality.Code != def.code), nil
	case "Quality.SourceDetectionSource", "Quality.ResolutionDetectionSource":
		if want.Kind != corpus.KindEnum || want.Type != "QualityDetectionSource" {
			return bad("a QualityDetectionSource enum")
		}
		code, ok := detectionCode(want.Str)
		if !ok {
			return corpus.Value{}, corpus.Value{}, fmt.Errorf("%s: QualityDetectionSource.%s has no explicit code mapping", key, want.Str)
		}
		if key == "Quality.SourceDetectionSource" {
			return str(code), str(ep.Quality.SourceDetection), nil
		}
		return str(code), str(ep.Quality.ResolutionDetection), nil
	}
	return corpus.Value{}, corpus.Value{}, fmt.Errorf("unsupported asserted property %q of %s", key, ownerSeriesTitle)
}

// seriesRunClock is the instant and calendar of the committed series corpus run:
// the parser's air date limit and the materialized symbolic dates read the same
// local date, so a +14 name and a +15 name are decided by the parser, not by the
// moment the test runs.
func seriesRunClock(t *testing.T) (time.Time, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	return time.Date(2026, 10, 25, 0, 30, 0, 0, loc), loc
}

// seriesTitleSubset is the bounded subset of the committed series title cases the
// concurrency test runs: every 11th case of the whole file, plus the first case
// of any entry point that stride misses, so all five title entry points are
// reached.
func seriesTitleSubset(t *testing.T, byID map[string]corpus.Disposition) *corpus.Corpus {
	t.Helper()
	full, _ := readCorpusFile(t, "series")
	picked := &corpus.Corpus{Header: full.Header}
	owners := map[string]bool{}
	var missed []corpus.Case
	for i, cs := range full.Cases {
		if _, disposed := byID[cs.ID]; cs.Scope != corpus.ScopeTitle || disposed {
			continue
		}
		switch {
		case i%concurrentStride == 0:
			picked.Cases = append(picked.Cases, cs)
			owners[cs.ScopeOwner] = true
		case !owners[cs.ScopeOwner]:
			missed = append(missed, cs)
		}
	}
	for _, cs := range missed {
		if !owners[cs.ScopeOwner] {
			picked.Cases = append(picked.Cases, cs)
			owners[cs.ScopeOwner] = true
		}
	}
	if len(picked.Cases) == 0 || len(owners) != 5 {
		t.Fatalf("the bounded series title subset has %d cases over entry points %v", len(picked.Cases), owners)
	}
	return picked
}
