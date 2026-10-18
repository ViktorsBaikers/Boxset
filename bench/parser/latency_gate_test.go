package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// The judge reads the real corpora and sources of the repository: the samples
// below are synthetic (their durations are chosen, never measured), the identities
// they carry are not. Each test gets an image of its own: a temporary /src whose
// run directories are published the way the image holds them (files 0444, run and
// samples directories 0555, the ancestors sticky) and whose manifest lists them.
const testRoot = "../.."

var (
	refOnce sync.Once
	refVal  *reference
	refErr  error
)

func testReference(t *testing.T) *reference {
	t.Helper()
	refOnce.Do(func() { refVal, refErr = loadReference(testRoot) })
	if refErr != nil {
		t.Fatalf("reference: %v", refErr)
	}
	return refVal
}

// testEnv is a fresh image: an empty docs/spikes/data under a temporary root.
func testEnv(t *testing.T) *judgeEnv {
	t.Helper()
	img := t.TempDir()
	data := filepath.Join(img, "docs", "spikes", "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{img, filepath.Join(img, "docs"), filepath.Join(img, "docs", "spikes"), data} {
		if err := os.Chmod(d, os.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}
	}
	return &judgeEnv{root: testRoot, imageRoot: img, uid: uint32(os.Getuid()), ref: testReference(t)}
}

// syncManifest rewrites the image manifest from the files under docs, in
// sha256sum format with the image's own /src paths; skip names files left out.
func syncManifest(t *testing.T, env *judgeEnv, skip ...string) {
	t.Helper()
	var b bytes.Buffer
	err := filepath.WalkDir(filepath.Join(env.imageRoot, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || slices.Contains(skip, p) {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(env.imageRoot, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  /src/%s\n", sha256Hex(data), filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	env.manifest = b.Bytes()
}

// publishFiles writes the named files of one run, locks them the way the image
// does and lists them in the manifest. It returns the run directory.
func publishFiles(t *testing.T, env *judgeEnv, run string, files map[string][]byte) string {
	t.Helper()
	dir := env.runPath(run)
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, data)
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	samples := filepath.Join(dir, "samples")
	for _, d := range []string{samples, dir} {
		if _, err := os.Stat(d); err == nil {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
		}
	}
	syncManifest(t, env)
	return dir
}

func recFiles(t *testing.T, recs [3]Facts) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for i, stage := range factStages {
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		files[stage+".json"] = data
	}
	return files
}

func sampleBytes(t *testing.T, set *sampleSet) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), samplesName)
	if err := writeSamples(p, set); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// publish publishes records and samples as one run of the image.
func publish(t *testing.T, env *judgeEnv, run string, recs [3]Facts, set *sampleSet) string {
	t.Helper()
	files := recFiles(t, recs)
	files["samples/"+samplesName] = sampleBytes(t, set)
	return publishFiles(t, env, run, files)
}

// independent digests of what a measurement is about, derived here from the
// recorded definition (a file's bytes; the lock and source lists framed as
// `name NUL size NUL bytes`) and not from the producer's functions.
func indepSHA(t *testing.T, root string, names []string, framed bool) string {
	t.Helper()
	h := sha256.New()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(n)))
		if err != nil {
			t.Fatal(err)
		}
		if framed {
			_, _ = io.WriteString(h, n+"\x00"+strconv.Itoa(len(data))+"\x00")
		}
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func indepSources(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	for _, dir := range []string{"internal/parse", "internal/parse/corpus"} {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(matches)
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				names = append(names, dir+"/"+filepath.Base(m))
			}
		}
	}
	return names
}

// syntheticSet builds a structurally and provenance-valid sample set whose
// duration at call index k of round r is ns(k, r), taken on 2026-10-17.
func syntheticSet(t *testing.T, run string, rounds int, ns func(k, r int) int64) *sampleSet {
	t.Helper()
	return syntheticSetOn(t, testRoot, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), run, rounds, ns)
}

// syntheticSetOn is syntheticSet for the UTC calendar day of day, measured from
// the corpora, lock and sources under root: the names are materialized for that day
// and the header carries it as the start and end date. The corpus, lock and source
// digests are derived independently of the producer.
func syntheticSetOn(t *testing.T, root string, day time.Time, run string, rounds int, ns func(k, r int) int64) *sampleSet {
	t.Helper()
	items, cases, err := testReference(t).items(corpus.TimeContext{Now: day.Add(12 * time.Hour), Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	set := &sampleSet{Items: items}
	for i := range set.Items {
		set.Items[i].Out = outcomeOK
	}
	set.Header = header{
		Schema: samplesSchema, Kind: samplesKind, Run: run, InputsSHA: testInputs,
		CorpusSeries: indepSHA(t, root, []string{"testdata/parser/series.jsonl"}, false),
		CorpusMovie:  indepSHA(t, root, []string{"testdata/parser/movie.jsonl"}, false),
		Lock:         indepSHA(t, root, []string{"go.mod", "go.sum"}, true),
		Source:       indepSHA(t, root, indepSources(t, root), true),
		Names:        namesHash(items), Go: "go-test", GOOS: "linux", GOARCH: "amd64", CGO: "0", Race: "false",
		Cases: cases, Items: len(items), Rounds: rounds, Seed: 1, OK: len(items), CompileNS: 1,
		StartDate: day.Format(factsDateLayout), EndDate: day.Format(factsDateLayout),
	}
	for r := 1; r <= rounds; r++ {
		order := rand.New(rand.NewPCG(1, uint64(r))).Perm(len(items))
		vals := make([]int64, len(order))
		for k := range order {
			vals[k] = ns(k, r)
		}
		set.Rounds = append(set.Rounds, round{R: r, Order: order, NS: vals})
	}
	return set
}

// recsFor is validRecs for another run and another instant (seconds since the epoch).
func recsFor(run string, t0 int64) [3]Facts {
	recs := validRecs()
	for i := range recs {
		recs[i].Run = run
		recs[i].StartUnix += t0 - testBase
		recs[i].EndUnix += t0 - testBase
	}
	return recs
}

// spread is a deterministic duration pattern: 40-60 microseconds.
func spread(k, r int) int64 { return 40_000 + int64((k*7919+r*104729)%20_000) }

func constant(v int64) func(k, r int) int64 { return func(int, int) int64 { return v } }

// judged runs the final judge over run (and base, when given) of the image.
func judged(t *testing.T, env *judgeEnv, run, base string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	baseDir := ""
	if base != "" {
		baseDir = env.runPath(base)
	}
	return judgeWith(env, env.runPath(run), baseDir, &out), out.String()
}

func wantJudged(t *testing.T, env *judgeEnv, run, reason string) {
	t.Helper()
	code, out := judged(t, env, run, "")
	if want := "PARSER-LATENCY-INVALID reason=" + reason + "\n"; code != exitInvalid || out != want {
		t.Fatalf("exit %d: %q, want INVALID %s", code, out, reason)
	}
}

func TestLatencyGateNearestRankPercentiles(t *testing.T) {
	seq := func(n int) []int64 {
		s := make([]int64, n)
		for i := range s {
			s[i] = int64(i + 1)
		}
		return s
	}
	for _, tc := range []struct {
		n        int
		p50, p99 int64
	}{{1, 1, 1}, {2, 1, 2}, {100, 50, 99}, {101, 51, 100}, {200, 100, 198}, {1000, 500, 990}} {
		s := seq(tc.n)
		if got := nearestRank(s, 50); got != tc.p50 {
			t.Errorf("n=%d p50 = %d, want %d", tc.n, got, tc.p50)
		}
		if got := nearestRank(s, 99); got != tc.p99 {
			t.Errorf("n=%d p99 = %d, want %d", tc.n, got, tc.p99)
		}
	}
}

func TestLatencyGateBudgetBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		head   stats
		base   *stats
		code   int
		reason string
	}{
		{"both at the budget", stats{p50: 100_000, p99: 1_000_000}, nil, exitPass, ""},
		{"p50 one over", stats{p50: 100_001, p99: 1_000_000}, nil, exitFail, "p50_budget"},
		{"p99 one over", stats{p50: 50_000, p99: 1_000_001}, nil, exitFail, "p99_budget"},
		{"base exactly 10% slower", stats{p50: 55_000, p99: 110_000}, &stats{p50: 50_000, p99: 100_000}, exitPass, ""},
		{"p50 over 10%", stats{p50: 55_001, p99: 100_000}, &stats{p50: 50_000, p99: 100_000}, exitFail, "p50_regression"},
		{"p99 over 10%", stats{p50: 50_000, p99: 110_001}, &stats{p50: 50_000, p99: 100_000}, exitFail, "p99_regression"},
		{"faster than base", stats{p50: 10_000, p99: 20_000}, &stats{p50: 50_000, p99: 100_000}, exitPass, ""},
	} {
		code, reason := verdictOf(tc.head, tc.base)
		if code != tc.code || reason != tc.reason {
			t.Errorf("%s: verdict = %d %q, want %d %q", tc.name, code, reason, tc.code, tc.reason)
		}
	}
}

func TestLatencyGateValidControlPasses(t *testing.T) {
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, spread))
	code, out := judged(t, env, testRun, "")
	if code != exitPass || !strings.HasPrefix(out, "PARSER-LATENCY-PASS eligible=VALID p50_ns=") || !strings.HasSuffix(out, " base=none\n") {
		t.Fatalf("exit %d: %q", code, out)
	}
	if !strings.Contains(out, " rounds=30 ") {
		t.Fatalf("the rounds are not reported: %q", out)
	}
}

func TestLatencyGateSlowedValidSamplesFail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ns     func(k, r int) int64
		reason string
	}{
		{"every call three times slower", func(k, r int) int64 { return 3 * spread(k, r) }, "p50_budget"},
		{"one more nanosecond than the p50 budget", constant(100_001), "p50_budget"},
		// A p99 breach with a mean far inside the budget: no mean stands in for p99.
		{"two percent slow tail", func(k, r int) int64 {
			if k%50 < 2 {
				return 2_000_000
			}
			return 20_000
		}, "p99_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, tc.ns))
			code, out := judged(t, env, testRun, "")
			if code != exitFail || !strings.HasPrefix(out, "PARSER-LATENCY-FAIL reason="+tc.reason+" ") || strings.Contains(out, "PASS") {
				t.Fatalf("exit %d: %q", code, out)
			}
		})
	}
	// The exact budgets pass.
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, constant(100_000)))
	if code, out := judged(t, env, testRun, ""); code != exitPass {
		t.Fatalf("exact budget: exit %d: %q", code, out)
	}
}

const testBaseRun = "bench-20261016T101500Z"

func TestLatencyGateComparableBase(t *testing.T) {
	for _, tc := range []struct {
		name string
		head int64
		code int
		note string
	}{
		{"exactly 10% slower", 55_000, exitPass, " base=compared\n"},
		{"over 10% slower", 55_001, exitFail, " base=compared\n"},
		{"faster", 30_000, exitPass, " base=compared\n"},
	} {
		env := testEnv(t)
		publish(t, env, testBaseRun, recsFor(testBaseRun, testBase), syntheticSet(t, testBaseRun, defaultRounds, constant(50_000)))
		publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, constant(tc.head)))
		code, out := judged(t, env, testRun, testBaseRun)
		if code != tc.code || !strings.HasSuffix(out, tc.note) {
			t.Errorf("%s: exit %d: %q", tc.name, code, out)
		}
		if tc.code == exitFail && !strings.Contains(out, "reason=p50_regression") {
			t.Errorf("%s: %q", tc.name, out)
		}
	}

	// No base: absolute baseline only; a run slower than a base but inside the budget passes.
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, constant(90_000)))
	if code, out := judged(t, env, testRun, ""); code != exitPass || !strings.HasSuffix(out, " base=none\n") {
		t.Errorf("no base: exit %d: %q", code, out)
	}
	if code, out := judged(t, env, testRun, testBaseRun); code != exitPass || !strings.HasSuffix(out, " base=incomparable\n") {
		t.Errorf("absent base: exit %d: %q", code, out)
	}

	// A base the judge cannot trust as much as a head is no base: every one of these would
	// fail the head's 10% bound if it were compared (it is 20 times faster), and none is.
	for _, tc := range []struct {
		name   string
		mutate func(s *sampleSet, recs *[3]Facts)
	}{
		{"race build", func(s *sampleSet, _ *[3]Facts) { s.Header.Race = "true" }},
		{"cgo build", func(s *sampleSet, _ *[3]Facts) { s.Header.CGO = "1" }},
		{"unknown cgo", func(s *sampleSet, _ *[3]Facts) { s.Header.CGO = "unknown" }},
		{"other os", func(s *sampleSet, _ *[3]Facts) { s.Header.GOOS = "darwin" }},
		{"other arch", func(s *sampleSet, _ *[3]Facts) { s.Header.GOARCH = "arm64" }},
		{"other sample run", func(s *sampleSet, _ *[3]Facts) { s.Header.Run = "bench-20261015T101500Z" }},
		{"other sample inputs", func(s *sampleSet, _ *[3]Facts) { s.Header.InputsSHA = strings.Repeat("c", 64) }},
		{"fewer rounds", func(s *sampleSet, _ *[3]Facts) {
			s.Header.Rounds--
			s.Rounds = s.Rounds[:len(s.Rounds)-1]
		}},
		{"other local offset", func(s *sampleSet, _ *[3]Facts) { s.Header.UTCOffset = 3600 }},
		{"names not those of its date", func(s *sampleSet, _ *[3]Facts) { s.Header.Names = strings.Repeat("e", 64) }},
		{"case identity swapped", func(s *sampleSet, _ *[3]Facts) { s.Items[0].ID, s.Items[1].ID = s.Items[1].ID, s.Items[0].ID }},
		{"other corpus", func(s *sampleSet, _ *[3]Facts) { s.Header.CorpusSeries = strings.Repeat("e", 64) }},
		{"ineligible host facts", func(_ *sampleSet, recs *[3]Facts) { recs[1].ForeignAllowedMax = 1 }},
	} {
		t.Run("not a base: "+tc.name, func(t *testing.T) {
			env := testEnv(t)
			recs := recsFor(testBaseRun, testBase)
			set := syntheticSet(t, testBaseRun, defaultRounds, constant(2_500))
			tc.mutate(set, &recs)
			publish(t, env, testBaseRun, recs, set)
			publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, constant(90_000)))
			if code, out := judged(t, env, testRun, testBaseRun); code != exitPass || !strings.HasSuffix(out, " base=incomparable\n") {
				t.Fatalf("exit %d: %q", code, out)
			}
		})
	}

	// Legitimate differences stay comparable: another lock and parser source, another
	// compile time, an earlier UTC day with the names of that day.
	t.Run("legitimate differences", func(t *testing.T) {
		env := testEnv(t)
		yesterday := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
		set := syntheticSetOn(t, testRoot, yesterday, testBaseRun, defaultRounds, constant(50_000))
		set.Header.Lock, set.Header.Source = strings.Repeat("a", 64), strings.Repeat("b", 64)
		set.Header.CompileNS, set.Header.Go = 12345, "go-older"
		publish(t, env, testBaseRun, recsFor(testBaseRun, yesterday.Unix()+10*3600), set)
		publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, constant(55_001)))
		code, out := judged(t, env, testRun, testBaseRun)
		if code != exitFail || !strings.HasSuffix(out, " base=compared\n") || !strings.Contains(out, "reason=p50_regression") {
			t.Fatalf("exit %d: %q", code, out)
		}
	})
}

// Provenance the judge cannot tie to this run, image, build and source is INVALID
// (exit 3), even when the samples are also over budget: INVALID beats FAIL.
func TestLatencyGateInvalidProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(s *sampleSet)
		reason string
	}{
		{"other run", func(s *sampleSet) { s.Header.Run = "bench-20261017T101501Z" }, "run_mismatch"},
		{"other input manifest", func(s *sampleSet) { s.Header.InputsSHA = strings.Repeat("c", 64) }, "inputs_mismatch"},
		{"fewer rounds", func(s *sampleSet) {
			s.Header.Rounds--
			s.Rounds = s.Rounds[:len(s.Rounds)-1]
		}, "rounds"},
		{"arm", func(s *sampleSet) { s.Header.GOARCH = "arm64" }, "platform"},
		{"other os", func(s *sampleSet) { s.Header.GOOS = "darwin" }, "platform"},
		{"cgo build", func(s *sampleSet) { s.Header.CGO = "1" }, "build"},
		{"unknown cgo", func(s *sampleSet) { s.Header.CGO = "unknown" }, "build"},
		{"race build", func(s *sampleSet) { s.Header.Race = "true" }, "build"},
		{"date rolled over", func(s *sampleSet) { s.Header.EndDate = "20261018" }, "date_rollover"},
		{"other local offset", func(s *sampleSet) { s.Header.UTCOffset = 3600 }, "date_rollover"},
		{"date outside the facts", func(s *sampleSet) { s.Header.StartDate, s.Header.EndDate = "20261016", "20261016" }, "date_rollover"},
		{"corpus changed", func(s *sampleSet) { s.Header.CorpusMovie = strings.Repeat("e", 64) }, "corpus_changed"},
		{"lock changed", func(s *sampleSet) { s.Header.Lock = strings.Repeat("e", 64) }, "lock_changed"},
		{"source changed", func(s *sampleSet) { s.Header.Source = strings.Repeat("e", 64) }, "source_changed"},
		{"names changed", func(s *sampleSet) { s.Header.Names = strings.Repeat("e", 64) }, "names_changed"},
		{"case identity swapped", func(s *sampleSet) { s.Items[0].ID, s.Items[1].ID = s.Items[1].ID, s.Items[0].ID }, "names_changed"},
		{"case count changed", func(s *sampleSet) { s.Header.Cases++ }, "names_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			set := syntheticSet(t, testRun, defaultRounds, constant(5_000_000)) // over budget as well
			tc.mutate(set)
			publish(t, env, testRun, validRecs(), set)
			wantJudged(t, env, testRun, tc.reason)
		})
	}
}

// The measurement's recorded corpus, lock and source digests are checked against the
// bytes the judge sees. The digests below come from this file's own derivation, so the
// producer's hashing is checked, not echoed; changing one hashed byte of a disposable
// fixture then makes the retained measurement INVALID.
func TestLatencyGateRetainedMeasurementTracksHashedInputs(t *testing.T) {
	bump := func(b []byte) []byte {
		i := bytes.Index(b, []byte(`"line":`))
		j := i + len(`"line":`)
		k := j
		for b[k] >= '0' && b[k] <= '9' {
			k++
		}
		return slices.Concat(b[:j], []byte("99999"), b[k:])
	}
	appendLine := func(b []byte) []byte { return append(slices.Clone(b), '\n') }
	for _, tc := range []struct {
		file   string
		mutate func([]byte) []byte
		reason string
	}{
		{"go.mod", appendLine, "lock_changed"},
		{"go.sum", appendLine, "lock_changed"},
		{"internal/parse/parser.go", appendLine, "source_changed"},
		{"internal/parse/corpus/corpus.go", appendLine, "source_changed"},
		{"testdata/parser/series.jsonl", bump, "corpus_changed"},
		{"testdata/parser/movie.jsonl", bump, "corpus_changed"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			fix := t.TempDir()
			rels := slices.Concat([]string{"go.mod", "go.sum", "testdata/parser/series.jsonl", "testdata/parser/movie.jsonl"}, indepSources(t, testRoot))
			for _, rel := range rels {
				data, err := os.ReadFile(filepath.Join(testRoot, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				dst := filepath.Join(fix, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, dst, data)
			}
			env := testEnv(t)
			env.root, env.ref = fix, nil
			publish(t, env, testRun, validRecs(), syntheticSetOn(t, fix, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), testRun, defaultRounds, spread))
			if code, out := judged(t, env, testRun, ""); code != exitPass {
				t.Fatalf("the unchanged fixture must pass: exit %d: %q", code, out)
			}
			p := filepath.Join(fix, filepath.FromSlash(tc.file))
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, p, tc.mutate(data))
			env.ref = nil
			wantJudged(t, env, testRun, tc.reason)
		})
	}
}

// editLines applies fn to the JSON object on line i (0-based) of samples bytes.
func editLine(t *testing.T, data []byte, i int, fn func(m map[string]any)) []byte {
	t.Helper()
	lines := bytes.SplitAfter(data, []byte("\n"))
	dec := json.NewDecoder(bytes.NewReader(lines[i]))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	lines[i] = append(line, '\n')
	return bytes.Join(lines, nil)
}

// A required key that is absent or null is INVALID through the final judge: the
// decoder would otherwise record a missing go, utc_offset_sec or compile_ns, or a
// null count or duration, as zero.
func TestLatencyGateRequiredFieldsThroughTheJudge(t *testing.T) {
	good := syntheticSet(t, testRun, defaultRounds, spread)
	data := sampleBytes(t, good)
	firstRound := good.Header.Items + 1
	publishData := func(t *testing.T, data []byte) *judgeEnv {
		env := testEnv(t)
		files := recFiles(t, validRecs())
		files["samples/"+samplesName] = data
		publishFiles(t, env, testRun, files)
		return env
	}
	if code, out := judged(t, publishData(t, data), testRun, ""); code != exitPass {
		t.Fatalf("control: exit %d: %q", code, out)
	}
	for _, key := range jsonKeys(reflect.TypeFor[header]()) {
		t.Run("header null "+key, func(t *testing.T) {
			wantJudged(t, publishData(t, editLine(t, data, 0, func(m map[string]any) { m[key] = nil })), testRun, "samples_malformed")
		})
		t.Run("header missing "+key, func(t *testing.T) {
			wantJudged(t, publishData(t, editLine(t, data, 0, func(m map[string]any) { delete(m, key) })), testRun, "samples_incomplete")
		})
	}
	for _, key := range []string{"app", "id", "n", "o"} {
		t.Run("item null "+key, func(t *testing.T) {
			wantJudged(t, publishData(t, editLine(t, data, 1, func(m map[string]any) { m[key] = nil })), testRun, "samples_malformed")
		})
	}
	for _, key := range []string{"r", "order", "ns"} {
		t.Run("round null "+key, func(t *testing.T) {
			wantJudged(t, publishData(t, editLine(t, data, firstRound, func(m map[string]any) { m[key] = nil })), testRun, "samples_malformed")
		})
	}
	t.Run("round null element of order", func(t *testing.T) {
		wantJudged(t, publishData(t, editLine(t, data, firstRound, func(m map[string]any) { m["order"].([]any)[0] = nil })), testRun, "samples_malformed")
	})
	t.Run("round null element of ns", func(t *testing.T) {
		wantJudged(t, publishData(t, editLine(t, data, firstRound, func(m map[string]any) { m["ns"].([]any)[0] = nil })), testRun, "samples_malformed")
	})
}

func TestLatencyGateSamplesAreStructurallyChecked(t *testing.T) {
	good := syntheticSet(t, testRun, defaultRounds, spread)
	data := sampleBytes(t, good)
	path := filepath.Join(t.TempDir(), samplesName)
	writeFile(t, path, data)
	if _, err := readSamples(path); err != nil {
		t.Fatalf("the written control must read back: %v", err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	lines = lines[:len(lines)-1] // the empty tail
	first := len(good.Items) + 1 // index of the first round line
	join := func(parts [][]byte) []byte { return bytes.Join(parts, nil) }
	// withRound replaces the first round line by the mutated copy of that round.
	withRound := func(mutate func(r *round)) []byte {
		r := good.Rounds[0]
		r.Order, r.NS = slices.Clone(r.Order), slices.Clone(r.NS)
		mutate(&r)
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		parts := slices.Clone(lines)
		parts[first] = append(line, '\n')
		return join(parts)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"no final newline", bytes.TrimSuffix(data, []byte("\n"))},
		{"a round missing", join(lines[:len(lines)-1])},
		{"a line after the last round", join(append(slices.Clone(lines), lines[len(lines)-1]))},
		{"unknown field", bytes.Replace(data, []byte(`"schema":1`), []byte(`"schema":1,"host":"x"`), 1)},
		{"order repeats an item", withRound(func(r *round) { r.Order[1] = r.Order[0] })},
		{"order leaves the items", withRound(func(r *round) { r.Order[0] = len(good.Items) })},
		{"zero duration", withRound(func(r *round) { r.NS[0] = 0 })},
		{"negative duration", withRound(func(r *round) { r.NS[0] = -5 })},
		{"huge duration", withRound(func(r *round) { r.NS[0] = maxSampleNS + 1 })},
		{"short duration list", withRound(func(r *round) { r.NS = r.NS[1:] })},
		{"round out of sequence", withRound(func(r *round) { r.R = 2 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), samplesName)
			writeFile(t, p, tc.data)
			_, err := readSamples(p)
			if _, ok := invalidReason(err); !ok {
				t.Fatalf("readSamples = %v, want INVALID", err)
			}
		})
	}
	if _, err := readSamples(filepath.Join(t.TempDir(), samplesName)); err == nil {
		t.Fatal("a missing file must be INVALID")
	}
	link := filepath.Join(t.TempDir(), samplesName)
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSamples(link); err == nil {
		t.Fatal("a link must be INVALID")
	}
}

// The judge accepts a run only from the immutable image that holds it: at its
// place in docs/spikes/data, owned by root and unwritable (the run and samples
// directories included, the ancestors at most sticky), with every one of the four
// files byte for byte what the image's manifest lists for it. A complete look-alike
// made anywhere else, or by the benchmarked child, is INVALID.
func TestLatencyGateJudgesOnlyTheImage(t *testing.T) {
	good := func(t *testing.T) (*judgeEnv, string) {
		env := testEnv(t)
		dir := publish(t, env, testRun, validRecs(), syntheticSet(t, testRun, defaultRounds, spread))
		if code, out := judged(t, env, testRun, ""); code != exitPass {
			t.Fatalf("control: exit %d: %q", code, out)
		}
		return env, dir
	}
	chmod := func(t *testing.T, path string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	}
	for _, name := range []string{"before.json", "during.json", "after.json", "samples/samples.jsonl"} {
		t.Run("writable "+name, func(t *testing.T) {
			env, dir := good(t)
			chmod(t, filepath.Join(dir, filepath.FromSlash(name)), 0o644)
			wantJudged(t, env, testRun, "record_mutable")
		})
		t.Run("group writable "+name, func(t *testing.T) {
			env, dir := good(t)
			chmod(t, filepath.Join(dir, filepath.FromSlash(name)), 0o464)
			wantJudged(t, env, testRun, "record_mutable")
		})
	}
	for _, tc := range []struct {
		name string
		path func(env *judgeEnv, dir string) string
		mode os.FileMode
	}{
		{"run directory", func(_ *judgeEnv, dir string) string { return dir }, 0o755},
		{"samples directory", func(_ *judgeEnv, dir string) string { return filepath.Join(dir, "samples") }, 0o755},
		{"data directory without the sticky bit", func(env *judgeEnv, _ string) string { return filepath.Dir(env.runPath(testRun)) }, 0o777},
		{"spikes directory without the sticky bit", func(env *judgeEnv, _ string) string { return filepath.Dir(filepath.Dir(env.runPath(testRun))) }, 0o777},
		{"image root without the sticky bit", func(env *judgeEnv, _ string) string { return env.imageRoot }, 0o777},
		{"run directory, sticky", func(_ *judgeEnv, dir string) string { return dir }, os.ModeSticky | 0o777},
	} {
		t.Run("writable parent: "+tc.name, func(t *testing.T) {
			env, dir := good(t)
			chmod(t, tc.path(env, dir), tc.mode)
			wantJudged(t, env, testRun, "record_mutable")
		})
	}
	t.Run("owned by another user", func(t *testing.T) {
		env, _ := good(t)
		env.uid++
		wantJudged(t, env, testRun, "record_mutable")
	})
	t.Run("not in the manifest", func(t *testing.T) {
		env, dir := good(t)
		for _, name := range []string{"before.json", "during.json", "after.json", "samples/samples.jsonl"} {
			syncManifest(t, env, filepath.Join(dir, filepath.FromSlash(name)))
			wantJudged(t, env, testRun, "image_mismatch")
		}
	})
	t.Run("another digest", func(t *testing.T) {
		env, _ := good(t)
		env.manifest = bytes.Replace(env.manifest, []byte("/src/docs/spikes/data/"+testRun+"/during.json"), []byte("/src/docs/spikes/data/"+testRun+"/other.json"), 1)
		wantJudged(t, env, testRun, "image_mismatch")
	})
	t.Run("changed after the manifest", func(t *testing.T) {
		env, dir := good(t)
		p := filepath.Join(dir, "after.json")
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(validFacts(stageAfter))
		writeFile(t, p, append(data, ' '))
		chmod(t, p, 0o444)
		chmod(t, dir, 0o555)
		wantJudged(t, env, testRun, "image_mismatch")
	})
	t.Run("listed twice", func(t *testing.T) {
		env, _ := good(t)
		env.manifest = append(slices.Clone(env.manifest), env.manifest...)
		wantJudged(t, env, testRun, "image_mismatch")
	})
	t.Run("run outside the image", func(t *testing.T) {
		env, dir := good(t)
		other := filepath.Join(t.TempDir(), testRun)
		if err := os.CopyFS(other, os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := judgeWith(env, other, "", &out); code != exitInvalid || out.String() != "PARSER-LATENCY-INVALID reason=run_location\n" {
			t.Fatalf("exit %d: %q", code, out.String())
		}
		for _, bad := range []string{".", "..", filepath.Join(filepath.Dir(dir), "x"), dir + "/samples", "bench-1"} {
			out.Reset()
			if code := judgeWith(env, bad, "", &out); code != exitInvalid || strings.Contains(out.String(), "PASS") {
				t.Errorf("%q: exit %d: %q", bad, code, out.String())
			}
		}
	})
	// A complete child-made look-alike: valid, eligible and byte-identical content of
	// the run under another run name inside the image's data directory, owned and
	// writable by the child and in no manifest.
	t.Run("child-made look-alike", func(t *testing.T) {
		env, dir := good(t)
		const alike = "bench-20261017T101501Z"
		files := map[string][]byte{}
		for _, name := range []string{"before.json", "during.json", "after.json", "samples/samples.jsonl"} {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			files[name] = bytes.ReplaceAll(data, []byte(testRun), []byte(alike))
		}
		for name, data := range files {
			p := filepath.Join(env.runPath(alike), filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, p, data)
		}
		wantJudged(t, env, alike, "record_mutable")
		for _, d := range []string{filepath.Join(env.runPath(alike), "samples"), env.runPath(alike)} {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
		}
		for name := range files {
			if err := os.Chmod(filepath.Join(env.runPath(alike), filepath.FromSlash(name)), 0o444); err != nil {
				t.Fatal(err)
			}
		}
		wantJudged(t, env, alike, "image_mismatch")
	})
	t.Run("manifest missing", func(t *testing.T) {
		var out bytes.Buffer
		if code := judge(testRoot, "bench-x", "", filepath.Join(t.TempDir(), "none"), &out); code != exitInvalid || out.String() != "PARSER-LATENCY-INVALID reason=image_manifest\n" {
			t.Fatalf("exit %d: %q", code, out.String())
		}
	})
}

// controlOut makes TestLatencyGateControlSamples publish its synthetic samples where
// the benchmark evidence smoke exports them from (inside a runner child); without it
// the test checks the samples writer and reader against each other in a temporary
// directory.
var controlOut = flag.String("control-out", "", "directory that receives the synthetic control samples.jsonl")

// TestLatencyGateControlSamples writes the independent synthetic control (chosen
// durations of 40-60 microseconds, real corpus, lock and source identities, derived
// independently of the producer) with the production samples writer and requires the
// production reader to return it unchanged.
func TestLatencyGateControlSamples(t *testing.T) {
	dir, run, inputs, day := t.TempDir(), testRun, testInputs, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if *controlOut != "" {
		abs, err := filepath.Abs(*controlOut)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := os.ReadFile(inputsManifest)
		if err != nil {
			t.Fatal(err)
		}
		dir, run, inputs, day = abs, filepath.Base(filepath.Dir(abs)), sha256Hex(manifest), time.Now().UTC().Truncate(24*time.Hour)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	set := syntheticSetOn(t, testRoot, day, run, defaultRounds, spread)
	set.Header.InputsSHA = inputs
	path := filepath.Join(dir, samplesName)
	if err := writeSamples(path, set); err != nil {
		t.Fatal(err)
	}
	got, err := readSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != set.Header || len(got.Items) != len(set.Items) || len(got.Rounds) != len(set.Rounds) {
		t.Fatalf("header or counts changed: %+v", got.Header)
	}
	for i, it := range set.Items {
		if g := got.Items[i]; g.App != it.App || g.ID != it.ID || g.N != it.N || g.Out != it.Out {
			t.Fatalf("item %d changed", i)
		}
	}
	for r, rd := range set.Rounds {
		if !slices.Equal(got.Rounds[r].Order, rd.Order) || !slices.Equal(got.Rounds[r].NS, rd.NS) || got.Rounds[r].R != rd.R {
			t.Fatalf("round %d changed", r+1)
		}
	}
	if st := pooledStats(got); st.p50 < 40_000 || st.p99 > 60_000 || st.n != defaultRounds*len(set.Items) {
		t.Fatalf("control stats = %+v", st)
	}
	// The producer's own identities of the same tree are the independently derived ones.
	ref := testReference(t)
	if ref.seriesSHA != set.Header.CorpusSeries || ref.movieSHA != set.Header.CorpusMovie || ref.lockSHA != set.Header.Lock || ref.sourceSHA != set.Header.Source {
		t.Fatal("the producer's corpus, lock or source digest differs from the independent derivation")
	}
}
