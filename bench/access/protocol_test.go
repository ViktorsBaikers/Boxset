package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
)

// The samples below are synthetic: their values are chosen, never measured. The
// identities they carry are the run's own, and every test gets an image of its
// own — a temporary /src whose run directories are published the way the image
// holds them (files 0444, run and samples directories 0555, the ancestors
// sticky) and whose manifest lists them — so the judge is exercised through the
// same reader the recorded run must pass.
const (
	testRun    = "bench-20261017T101500Z"
	testImage  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testInputs = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// testBase is the run's instant, fixed: nothing here reads the wall clock.
var testBase = time.Date(2026, 10, 17, 10, 15, 0, 0, time.UTC).Unix()

// Chosen values well inside every recorded budget.
const (
	controlStartNS  = 100 * 1_000_000
	controlKeyNS    = 50_000
	controlListNS   = 5 * 1_000_000
	controlGZIPNS   = 500_000
	controlRSSBytes = 40_000_000
	controlPeak     = 2 << 30
	controlGoro     = 400
)

// validRecord is an independent serialized control: four physical same-NUMA
// cores without SMT siblings, no foreign workload, 4 GiB with no container
// swap, no swap activity, a read-only empty ext4 NVMe /config and one UTC date,
// with a data block that passes the data rules.
func validRecord(stage string) benchprofile.Record {
	r := benchprofile.Record{
		Envelope: benchprofile.Envelope{Schema: benchprofile.RecordSchema, Stage: stage, Run: testRun, Image: testImage, Inputs: testInputs},
		Host: benchprofile.Host{
			StartUnix: testBase, EndUnix: testBase, Observations: 1,
			CPUs: 4, Cores: 4, ThreadsPerCoreMax: 2, SiblingsOutsideSet: 4, NUMANodes: 1,
			Load1MilliMax: 20, MemAvailableKiBMin: 16 << 20,
			SwapInStart: 7, SwapInEnd: 7, SwapOutStart: 9, SwapOutEnd: 9,
			MemLimitBytes: benchprofile.RequiredMemory, MemSwapLimitBytes: benchprofile.RequiredMemory, CpusetMatches: true,
			ConfigFS: benchprofile.ConfigFSExt4, ConfigNVMe: true, ConfigEmpty: true, ConfigReadOnly: true,
		},
		Data: benchprofile.Data{FS: benchprofile.ConfigFSExt4, NVMe: true, Writable: true, Bound: true, Mounted: true},
	}
	switch stage {
	case benchprofile.StageDuring:
		r.Host.StartUnix, r.Host.EndUnix, r.Host.Observations = testBase+1, testBase+9, 9
	case benchprofile.StageAfter:
		r.Host.StartUnix, r.Host.EndUnix = testBase+10, testBase+10
	}
	return r
}

func validRecs() [3]benchprofile.Record {
	return [3]benchprofile.Record{validRecord(benchprofile.StageBefore), validRecord(benchprofile.StageDuring), validRecord(benchprofile.StageAfter)}
}

// testEnv is a fresh image: an empty docs/spikes/data under a temporary root.
func testEnv(t *testing.T) *judgeEnv {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "docs", "spikes", "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, filepath.Join(root, "docs"), filepath.Join(root, "docs", "spikes"), data} {
		if err := os.Chmod(d, os.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}
	}
	return &judgeEnv{img: benchprofile.NewImage(root, uint32(os.Getuid()), nil)}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// syncManifest rewrites the image manifest from the files under docs, in
// sha256sum format with the image's own /src paths.
func syncManifest(t *testing.T, env *judgeEnv) {
	t.Helper()
	var b bytes.Buffer
	err := filepath.WalkDir(filepath.Join(env.img.Root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(env.img.Root, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  /src/%s\n", sha256Hex(data), filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	env.img.Manifest = b.Bytes()
}

// publishFiles writes the named files of one run, locks them the way the image
// does and lists them in the manifest. It returns the run directory.
func publishFiles(t *testing.T, env *judgeEnv, run string, files map[string][]byte) string {
	t.Helper()
	dir := env.img.RunPath(run)
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

// syntheticSet is a complete, eligible, on-budget sample set: every timing
// slot of both sides, every cap reading, and one RSS hold with a reading inside
// the verdict window.
func syntheticSet(run string) *sampleSet {
	set := &sampleSet{Header: header{
		Schema: samplesSchema, Kind: samplesKind, Run: run, InputsSHA: testInputs, DataDir: dataMount,
		Purpose: purposeProduction, CGO: "0", Trimpath: true, Go: "go-test", GOOS: "linux", GOARCH: "amd64",
		Goroutines: goroutinesObserved, Executions: executions,
		StartDate: "20261017", EndDate: "20261017",
	}}
	for _, spec := range timingSeries {
		for _, side := range []string{sideA1, sideA2} {
			for exec := 1; exec <= executions; exec++ {
				set.Samples = append(set.Samples, sample{Series: spec.series, Side: side, Exec: exec, Purpose: purposeProduction, Stat: controlStat(spec.series)})
			}
		}
	}
	for _, side := range []string{sideA1, sideA2} {
		for exec := 1; exec <= executions; exec++ {
			set.Samples = append(set.Samples,
				sample{Series: seriesMemoryPeak, Side: side, Exec: exec, Purpose: purposeProduction, Stat: controlPeak},
				sample{Series: seriesOOMKill, Side: side, Exec: exec, Purpose: purposeProduction},
				sample{Series: seriesGoroutines, Side: side, Exec: exec, Purpose: purposeProduction, Stat: controlGoro},
			)
		}
	}
	for at := int64(0); at <= rssWindowEndMS; at += 10_000 {
		set.Samples = append(set.Samples, sample{Series: seriesRSS, Purpose: purposeFill, Stat: controlRSSBytes, Raw: []int64{at}})
	}
	return set
}

func controlStat(series string) int64 {
	switch series {
	case seriesStart:
		return controlStartNS
	case seriesKey:
		return controlKeyNS
	case seriesGZIP:
		return controlGZIPNS
	}
	return controlListNS
}

// setSeries replaces every sample of one series and keeps the header's count.
func setSeries(set *sampleSet, series string, value int64) {
	for i := range set.Samples {
		if set.Samples[i].Series == series {
			set.Samples[i].Stat = value
		}
	}
}

// setSide replaces the statistic of one series on one side only.
func setSide(set *sampleSet, series, side string, value int64) {
	for i := range set.Samples {
		if set.Samples[i].Series == series && set.Samples[i].Side == side {
			set.Samples[i].Stat = value
		}
	}
}

// dropSeries removes every sample of one series.
func dropSeries(set *sampleSet, series string) {
	set.Samples = slices.DeleteFunc(set.Samples, func(s sample) bool { return s.Series == series })
}

// dropOne removes the first sample that matches and reports whether it found one.
func dropOne(set *sampleSet, match func(sample) bool) bool {
	for i, s := range set.Samples {
		if match(s) {
			set.Samples = slices.Delete(set.Samples, i, i+1)
			return true
		}
	}
	return false
}

// publish publishes records and samples as one run of the image.
func publish(t *testing.T, env *judgeEnv, run string, recs [3]benchprofile.Record, set *sampleSet) string {
	t.Helper()
	files := map[string][]byte{"samples/" + samplesName: sampleBytes(t, set)}
	for i, stage := range benchprofile.RecordStages {
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		files[stage+benchprofile.FactFileNameSuffix] = data
	}
	return publishFiles(t, env, run, files)
}

// judged runs the final judge over run of the image.
func judged(t *testing.T, env *judgeEnv, run string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	return judgeWith(env, env.img.RunPath(run), &out), out.String()
}

func wantJudged(t *testing.T, env *judgeEnv, run, reason string) {
	t.Helper()
	code, out := judged(t, env, run)
	if want := "ACCESS-BENCH-INVALID reason=" + reason + "\n"; code != exitInvalid || out != want {
		t.Fatalf("exit %d: %q, want INVALID %s", code, out, reason)
	}
}

// The command's own codes: a malformed invocation is usage, never a verdict, and
// the measuring path refuses it before any directory or binary is touched.
func TestCommandUsageExitCode(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"extra"},
		{"-out", "x", "-judge", "y"},
		{"-control", "fast", "-out", "x"},
		{"-control", "slow", "-judge", "y"},
	} {
		if code := run(args, io.Discard, io.Discard); code != exitUsage {
			t.Fatalf("args %q: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestJudgeControlSamples(t *testing.T) {
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), syntheticSet(testRun))
	code, out := judged(t, env, testRun)
	if code != exitPass || !strings.HasPrefix(out, "ACCESS-BENCH-PASS ") {
		t.Fatalf("exit %d: %q", code, out)
	}
	for _, want := range []string{" start_ns=100000000 ", " key_ns=50000 ", " rss_bytes=40000000 ", " goroutines=400 "} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q lacks %q", out, want)
		}
	}
	// The producer's writer and the judge's reader agree on the wire, and the
	// header's own count is the writer's, not the caller's.
	set := syntheticSet(testRun)
	set.Header.Samples = 1 << 30
	data := sampleBytes(t, set)
	got, err := parseSamples(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Samples != len(got.Samples) {
		t.Fatalf("written count = %d, want %d", got.Header.Samples, len(got.Samples))
	}
	if got.Header != set.Header {
		t.Fatalf("header changed in the writer: %+v", got.Header)
	}
}

// A measured noise at or above the series' own budget makes even a fast side's
// result unusable: the threshold sits below the noise the run itself measured.
func TestJudgeInvalidWhenNoiseExceedsBudget(t *testing.T) {
	set := syntheticSet(testRun)
	setSide(set, seriesStart, sideA2, controlStartNS+startBudgetNS)
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), set)
	wantJudged(t, env, testRun, "noise")

	// One nanosecond less noise is still judged: the sides then straddle the
	// budget and the verdict is within_noise, never noise.
	set = syntheticSet(testRun)
	setSide(set, seriesStart, sideA2, controlStartNS+startBudgetNS-1)
	env = testEnv(t)
	publish(t, env, testRun, validRecs(), set)
	wantJudged(t, env, testRun, "within_noise")
}

// The measured noise is added to the larger side median, and only a smaller
// side median past the budget fails: anything between is within noise.
func TestJudgeInvalidWithinNoise(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a1, a2 int64
		code   int
		reason string
	}{
		{"equal sides inside the budget", 100 * 1_000_000, 100 * 1_000_000, exitPass, ""},
		{"larger median plus noise over the budget", 200 * 1_000_000, 100 * 1_000_000, exitInvalid, "within_noise"},
		{"both medians under the budget but straddling it", 260 * 1_000_000, 200 * 1_000_000, exitInvalid, "within_noise"},
		{"smaller median over the budget", 260 * 1_000_000, 255 * 1_000_000, exitFail, "start_budget"},
		{"exactly at the budget", startBudgetNS, startBudgetNS, exitPass, ""},
		{"one nanosecond over the budget", startBudgetNS + 1, startBudgetNS + 1, exitFail, "start_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			setSide(set, seriesStart, sideA1, tc.a1)
			setSide(set, seriesStart, sideA2, tc.a2)
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			code, out := judged(t, env, testRun)
			if code != tc.code {
				t.Fatalf("exit %d: %q", code, out)
			}
			switch tc.code {
			case exitPass:
				if !strings.HasPrefix(out, "ACCESS-BENCH-PASS ") {
					t.Fatalf("q %q", out)
				}
			case exitFail:
				if want := "ACCESS-BENCH-FAIL reason=" + tc.reason + " "; !strings.HasPrefix(out, want) {
					t.Fatalf("%q, want prefix %q", out, want)
				}
			default:
				if want := "ACCESS-BENCH-INVALID reason=" + tc.reason + "\n"; out != want {
					t.Fatalf("%q, want %q", out, want)
				}
			}
		})
	}
	// KEY is the one strict budget: 100 µs itself misses it.
	for _, tc := range []struct {
		value int64
		code  int
	}{{keyBudgetNS - 1, exitPass}, {keyBudgetNS, exitFail}} {
		set := syntheticSet(testRun)
		setSeries(set, seriesKey, tc.value)
		env := testEnv(t)
		publish(t, env, testRun, validRecs(), set)
		if code, out := judged(t, env, testRun); code != tc.code {
			t.Fatalf("key %d: exit %d: %q", tc.value, code, out)
		}
	}
}

// The slow control must fail START, KEY, HASH and ABUSE; failing fewer is a
// finding, not a pass, and no control run ever prints one.
func TestJudgeFailsSlowControl(t *testing.T) {
	control := func(set *sampleSet) *sampleSet {
		set.Header.Control = controlSlow
		set.Header.Purpose = purposeSlow
		set.Header.Tags = tagSlow
		for i := range set.Samples {
			if set.Samples[i].Purpose == purposeProduction {
				set.Samples[i].Purpose = purposeSlow
			}
		}
		setSeries(set, seriesStart, startBudgetNS+1)
		setSeries(set, seriesKey, keyBudgetNS)
		setSeries(set, seriesHash, listBudgetNS+1)
		setSeries(set, seriesAbuse, listBudgetNS+1)
		return set
	}
	env := testEnv(t)
	publish(t, env, testRun, validRecs(), control(syntheticSet(testRun)))
	code, out := judged(t, env, testRun)
	if code != exitFail || !strings.HasPrefix(out, "ACCESS-BENCH-FAIL reason=start_budget ") {
		t.Fatalf("exit %d: %q", code, out)
	}
	if strings.Contains(out, "ACCESS-BENCH-PASS") {
		t.Fatalf("a control run printed a pass: %q", out)
	}

	// Three of the four failing is the recorded finding.
	set := control(syntheticSet(testRun))
	setSeries(set, seriesAbuse, controlListNS)
	env = testEnv(t)
	publish(t, env, testRun, validRecs(), set)
	code, out = judged(t, env, testRun)
	if code != exitFail || !strings.HasPrefix(out, "ACCESS-BENCH-FAIL reason=control ") {
		t.Fatalf("exit %d: %q", code, out)
	}

	// A control run that breaches nothing still never passes.
	set = control(syntheticSet(testRun))
	for _, series := range []string{seriesStart, seriesKey, seriesHash, seriesAbuse} {
		setSeries(set, series, controlStat(series))
	}
	env = testEnv(t)
	publish(t, env, testRun, validRecs(), set)
	if code, out := judged(t, env, testRun); code != exitFail || !strings.HasPrefix(out, "ACCESS-BENCH-FAIL reason=control ") {
		t.Fatalf("exit %d: %q", code, out)
	}
}

// Every execution and every reading the protocol records is required: a missing
// timing slot, cap reading or verdict-window RSS sample is INVALID, never a
// pass on the executions that remain.
func TestJudgeInvalidMissingSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(set *sampleSet)
	}{
		{"a start execution", func(set *sampleSet) {
			if !dropOne(set, func(s sample) bool { return s.Series == seriesStart && s.Side == sideA2 && s.Exec == 3 }) {
				t.Fatal("control lacks the start slot")
			}
		}},
		{"every key execution", func(set *sampleSet) { dropSeries(set, seriesKey) }},
		{"an abuse execution", func(set *sampleSet) {
			if !dropOne(set, func(s sample) bool { return s.Series == seriesAbuse && s.Side == sideA1 && s.Exec == 5 }) {
				t.Fatal("control lacks the abuse slot")
			}
		}},
		{"the goroutine readings", func(set *sampleSet) { dropSeries(set, seriesGoroutines) }},
		{"the memory peak readings", func(set *sampleSet) { dropSeries(set, seriesMemoryPeak) }},
		{"the oom_kill readings", func(set *sampleSet) { dropSeries(set, seriesOOMKill) }},
		{"the verdict-window RSS reading", func(set *sampleSet) {
			if !dropOne(set, func(s sample) bool { return s.Series == seriesRSS && s.Raw[0] == 600_000 }) {
				t.Fatal("control lacks the verdict-window reading")
			}
		}},
		{"every RSS reading", func(set *sampleSet) { dropSeries(set, seriesRSS) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			tc.mutate(set)
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			wantJudged(t, env, testRun, "samples_missing")
		})
	}

	// A header whose count is not the file's own is refused structurally.
	set := syntheticSet(testRun)
	data := sampleBytes(t, set)
	lines := bytes.SplitAfter(data, []byte("\n"))
	var h map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(lines[0]), &h); err != nil {
		t.Fatal(err)
	}
	h["samples"] = h["samples"].(float64) + 1
	line, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = append(line, '\n')
	env := testEnv(t)
	files := map[string][]byte{"samples/" + samplesName: bytes.Join(lines, nil)}
	for i, stage := range benchprofile.RecordStages {
		recs := validRecs()
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		files[stage+benchprofile.FactFileNameSuffix] = data
	}
	publishFiles(t, env, testRun, files)
	wantJudged(t, env, testRun, "samples_incomplete")
}

// The goroutine cap is the ABUSE window's own observation: the run's maximum is
// judged against the recorded ≤1,000, one over it fails naming the cap, and a run
// that recorded no reading at all is refused rather than passed on absent
// evidence. The samples are synthetic and their instants fixed; only the reading
// under test differs.
func TestJudgeGoroutineCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(set *sampleSet)
		code   int
		prefix string
		value  int64
	}{
		{"observed inside the cap", func(set *sampleSet) { setSeries(set, seriesGoroutines, goroutineCap-1) }, exitPass, "ACCESS-BENCH-PASS ", goroutineCap - 1},
		{"observed at the cap", func(set *sampleSet) { setSeries(set, seriesGoroutines, goroutineCap) }, exitPass, "ACCESS-BENCH-PASS ", goroutineCap},
		{"observed one over the cap", func(set *sampleSet) { setSeries(set, seriesGoroutines, goroutineCap+1) }, exitFail, "ACCESS-BENCH-FAIL reason=goroutines ", goroutineCap + 1},
		{"one window's maximum over the cap", func(set *sampleSet) {
			for i := range set.Samples {
				if set.Samples[i].Series == seriesGoroutines && set.Samples[i].Side == sideA2 && set.Samples[i].Exec == 2 {
					set.Samples[i].Stat = goroutineCap + 1
					set.Samples[i].Raw = []int64{controlGoro, goroutineCap + 1, controlGoro}
				}
			}
		}, exitFail, "ACCESS-BENCH-FAIL reason=goroutines ", goroutineCap + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			tc.mutate(set)
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			code, out := judged(t, env, testRun)
			if code != tc.code || !strings.HasPrefix(out, tc.prefix) {
				t.Fatalf("exit %d: %q, want %d %q", code, out, tc.code, tc.prefix)
			}
			if want := fmt.Sprintf(" goroutines=%d ", tc.value); !strings.Contains(out, want) {
				t.Fatalf("%q lacks %q", out, want)
			}
		})
	}

	// No reading is never a pass. The header's own account of why it has none
	// travels on the line that refuses the run, so the absence is a reported
	// reason and not a bare missing sample.
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"an observed run without a reading", "", "ACCESS-BENCH-INVALID reason=samples_missing\n"},
		{"a build that cannot read the route", goroutinesNoSource, "ACCESS-BENCH-INVALID reason=samples_missing observation=" + goroutinesNoSource + "\n"},
		{"a window whose poll failed", goroutinesPollFailed, "ACCESS-BENCH-INVALID reason=samples_missing observation=" + goroutinesPollFailed + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			dropSeries(set, seriesGoroutines)
			if tc.reason != "" {
				set.Header.Goroutines, set.Header.Reason = goroutinesUnavailable, tc.reason
			}
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			code, out := judged(t, env, testRun)
			if code != exitInvalid || out != tc.want {
				t.Fatalf("exit %d: %q, want %d %q", code, out, exitInvalid, tc.want)
			}
		})
	}
}

// A ready_cap names what it saw, after the word every expectation already
// matches: the state /ping last reported, the elapsed milliseconds against the
// cap and the transport's class. The state is the /ping enum, so a word nobody
// recognises folds into other and the absence of one into none.
func TestReadyCapNamesWhatItSaw(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		elapsed   time.Duration
		transport error
		want      string
	}{
		{"a lifecycle that never left recovering", "recovering", 10012 * time.Millisecond, nil,
			"ready_cap state=recovering elapsed_ms=10012 ping=status"},
		{"an unexpected state word", "\nready_cap state=ready elapsed_ms=1", 10 * time.Second, nil,
			"ready_cap state=other elapsed_ms=10000 ping=status"},
		{"no response at all", "", 9 * time.Second, context.DeadlineExceeded,
			"ready_cap state=none elapsed_ms=9000 ping=timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readyCap(tc.state, tc.elapsed, tc.transport).Error(); got != tc.want {
				t.Fatalf("ready_cap %q, want %q", got, tc.want)
			}
		})
	}
}

// A series step's failure names what it saw, after the word every expectation
// already matches: the HTTP status, the API's own reason word clamped to the
// codes this repository writes, and the elapsed milliseconds into the step. No
// response at all is none, and a word nobody recognises folds into other.
func TestListFailedNamesWhatItSaw(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		reason  string
		elapsed time.Duration
		want    string
	}{
		{"a refused list", http.StatusForbidden, "forbidden", 1001 * time.Millisecond,
			"list_failed status=403 reason=forbidden elapsed_ms=1001"},
		{"a refusal from the reader cap", http.StatusTooManyRequests, "read_cap", 42 * time.Millisecond,
			"list_failed status=429 reason=read_cap elapsed_ms=42"},
		{"an unrecognised reason word", http.StatusServiceUnavailable, "upstream_teapot", 7 * time.Millisecond,
			"list_failed status=503 reason=other elapsed_ms=7"},
		{"no response at all", 0, "", 2500 * time.Millisecond,
			"list_failed status=none reason=other elapsed_ms=2500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := seriesFailed("list_failed", tc.status, tc.reason, tc.elapsed).Error(); got != tc.want {
				t.Fatalf("list_failed %q, want %q", got, tc.want)
			}
		})
	}
}

// The measured execution starts on a copy of the fixture, so the copy keeps each
// entry's own permission bits: the installation key is owner-only in the fixture,
// and a copy that widened it would put every start in recovering. An entry that
// is neither a directory nor a regular file is refused, so the caller records
// fixture_copy rather than following a link out of the fixture.
func TestFixtureCopyKeepsEntryPermissions(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keys", "abc.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := copyFixture(src, dst); err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	for _, tc := range []struct {
		name string
		want fs.FileMode
	}{
		{"keys", 0o700},
		{filepath.Join("keys", "abc.key"), 0o600},
	} {
		info, err := os.Stat(filepath.Join(dst, tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := info.Mode().Perm(); got != tc.want {
			t.Errorf("%s copied %v, want %v", tc.name, got, tc.want)
		}
	}
	if err := os.Symlink(filepath.Join(src, "keys", "abc.key"), filepath.Join(src, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := copyFixture(src, t.TempDir()); err == nil {
		t.Fatal("copyFixture accepted a symlink")
	}
}

// A fixture copy carries the fixture's data, never another process's state: the
// shared-memory index and the write-ahead log a database keeps beside itself
// belong to the processes writing it, and a copy that carried them would make the
// first connection on the copy do recovery against a stale index. Only those two
// entries beside a database file are dropped — an ordinary file that merely ends
// the same way, and every other entry, is copied.
func TestFixtureCopySkipsSQLiteEphemeralEntries(t *testing.T) {
	src := t.TempDir()
	names := []string{"x.db", "x.db-wal", "x.db-shm", "log-wal", "notes.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	if err := copyFixture(src, dst); err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"log-wal", "notes.txt", "x.db"}; !slices.Equal(got, want) {
		t.Errorf("copy holds %v, want %v", got, want)
	}
}

// The observation is read from the authorized status route and nowhere else: a
// refusal, a malformed body, an absent field and a count below one are the same
// recorded failure, and a positive count is the reading the window keeps.
func TestObserveGoroutines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int64
		reason string
	}{
		{"a count", http.StatusOK, `{"state":"ready","goroutines":37}`, 37, ""},
		{"an absent field", http.StatusOK, `{"state":"ready"}`, 0, goroutinesPollFailed},
		{"a zero count", http.StatusOK, `{"goroutines":0}`, 0, goroutinesPollFailed},
		{"a refused route", http.StatusUnauthorized, `{"goroutines":37}`, 0, goroutinesPollFailed},
		{"a malformed body", http.StatusOK, `goroutines`, 0, goroutinesPollFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != statusPath || r.Header.Get(keyHeader) != "observer-key" {
					t.Errorf("poll read %s with %q", r.URL.Path, r.Header.Get(keyHeader))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := &client{hc: srv.Client(), base: srv.URL, keyObs: "observer-key"}
			got := c.observeGoroutines(t.Context(), 50*time.Millisecond)
			if got.reason != tc.reason {
				t.Fatalf("reason %q, want %q", got.reason, tc.reason)
			}
			if tc.want == 0 {
				if len(got.counts) != 0 {
					t.Fatalf("counts %v, want none", got.counts)
				}
				return
			}
			if len(got.counts) == 0 {
				t.Fatalf("no reading, want %d", tc.want)
			}
			for _, n := range got.counts {
				if n != tc.want {
					t.Fatalf("counts %v, want every reading %d", got.counts, tc.want)
				}
			}
		})
	}
}

// A timing sample never counts when the binary that produced it was not the
// ordinary production build: production identity is the absence of a build
// -tags setting, so a bench tag and the tree's own tags are both refused.
func TestJudgeRejectsFixtureTimingSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(set *sampleSet)
		reason string
	}{
		{"a fill-fixture sample in a timing series", func(set *sampleSet) {
			for i := range set.Samples {
				if set.Samples[i].Series == seriesKey && set.Samples[i].Side == sideA1 && set.Samples[i].Exec == 1 {
					set.Samples[i].Purpose = purposeFill
				}
			}
		}, "binary_purpose"},
		{"a slow-control sample in a production run", func(set *sampleSet) {
			for i := range set.Samples {
				if set.Samples[i].Series == seriesAbuse && set.Samples[i].Exec == 2 {
					set.Samples[i].Purpose = purposeSlow
				}
			}
		}, "binary_purpose"},
		{"a fill-fixture run", func(set *sampleSet) {
			set.Header.Purpose = purposeFill
			set.Header.Tags = tagFill
			for i := range set.Samples {
				if set.Samples[i].Series != seriesRSS {
					set.Samples[i].Purpose = purposeFill
				}
			}
		}, "binary_purpose"},
		{"the integration build", func(set *sampleSet) { set.Header.Tags = "integration" }, "build"},
		{"the e2e build", func(set *sampleSet) { set.Header.Tags = "e2e" }, "build"},
		{"the fill fixture", func(set *sampleSet) { set.Header.Tags = tagFill }, "build"},
		{"the slow control", func(set *sampleSet) { set.Header.Tags = tagSlow }, "build"},
		{"a cgo build", func(set *sampleSet) { set.Header.CGO = "1" }, "build"},
		{"an unknown cgo setting", func(set *sampleSet) { set.Header.CGO = "unknown" }, "build"},
		{"a build without -trimpath", func(set *sampleSet) { set.Header.Trimpath = false }, "build"},
		{"another platform", func(set *sampleSet) { set.Header.GOARCH = "arm64" }, "platform"},
		{"another os", func(set *sampleSet) { set.Header.GOOS = "darwin" }, "platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			tc.mutate(set)
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			wantJudged(t, env, testRun, tc.reason)
		})
	}
}

// The run's data evidence is judged before its timing: an ineligible host, a
// V1 record set, a data block that fails a data rule or a samples header that
// describes another mount is INVALID with the refusing predicate's own token.
func TestJudgeInvalidDataEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(recs *[3]benchprofile.Record)
		reason string
	}{
		{"a wrong data mount", func(r *[3]benchprofile.Record) { r[1].Data.Mounted = false }, "data_mount"},
		{"a non-ext4 data backing", func(r *[3]benchprofile.Record) { r[2].Data.FS = benchprofile.ConfigFSOther }, "data_filesystem"},
		{"a non-NVMe data backing", func(r *[3]benchprofile.Record) { r[0].Data.NVMe = false }, "data_filesystem"},
		{"an unbound data block", func(r *[3]benchprofile.Record) { r[1].Data.Bound = false }, "data_binding"},
		{"a read-only data mount", func(r *[3]benchprofile.Record) { r[2].Data.Writable = false }, "data_binding"},
		{"an unbound run identity", func(r *[3]benchprofile.Record) { r[2].Run = "bench-20261017T101501Z" }, "identity_mismatch"},
		{"a second core", func(r *[3]benchprofile.Record) { r[1].Host.CPUs, r[1].Host.Cores = 5, 5 }, "topology"},
		{"an end on the next UTC day", func(r *[3]benchprofile.Record) { r[2].Host.EndUnix += 24 * 3600 }, "date_rollover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			recs := validRecs()
			tc.mutate(&recs)
			publish(t, env, testRun, recs, syntheticSet(testRun))
			wantJudged(t, env, testRun, tc.reason)
		})
	}

	t.Run("an eligible /config with no data evidence", func(t *testing.T) {
		env := testEnv(t)
		recs := validRecs()
		for i := range recs {
			recs[i].Data = benchprofile.Data{FS: benchprofile.ConfigFSOther}
		}
		publish(t, env, testRun, recs, syntheticSet(testRun))
		wantJudged(t, env, testRun, "data_filesystem")
	})

	t.Run("a V1 record set", func(t *testing.T) {
		env := testEnv(t)
		var files = map[string][]byte{"samples/" + samplesName: sampleBytes(t, syntheticSet(testRun))}
		for i, stage := range benchprofile.FactStages {
			data, err := json.Marshal(v1Record(stage, i))
			if err != nil {
				t.Fatal(err)
			}
			files[stage+benchprofile.FactFileNameSuffix] = data
		}
		publishFiles(t, env, testRun, files)
		wantJudged(t, env, testRun, "record_incomplete")
	})

	t.Run("a missing stage", func(t *testing.T) {
		env := testEnv(t)
		files := map[string][]byte{"samples/" + samplesName: sampleBytes(t, syntheticSet(testRun))}
		recs := validRecs()
		for i, stage := range benchprofile.RecordStages[:2] {
			data, err := json.Marshal(recs[i])
			if err != nil {
				t.Fatal(err)
			}
			files[stage+benchprofile.FactFileNameSuffix] = data
		}
		publishFiles(t, env, testRun, files)
		wantJudged(t, env, testRun, "record_missing")
	})

	t.Run("ineligible data before an over-budget timing", func(t *testing.T) {
		env := testEnv(t)
		recs := validRecs()
		recs[0].Data.Mounted = false
		set := syntheticSet(testRun)
		setSeries(set, seriesStart, 10*startBudgetNS)
		publish(t, env, testRun, recs, set)
		wantJudged(t, env, testRun, "data_mount")
	})

	t.Run("another data mount in the samples", func(t *testing.T) {
		set := syntheticSet(testRun)
		set.Header.DataDir = "/tmp"
		env := testEnv(t)
		publish(t, env, testRun, validRecs(), set)
		wantJudged(t, env, testRun, "data_dir")
	})
}

// v1Record is the V1 wire's flat record, written by this test rather than by any
// producer: the access judge refuses it on its key set.
func v1Record(stage string, i int) benchprofile.Facts {
	r := validRecord(stage)
	return benchprofile.Facts{Envelope: r.Envelope, Host: r.Host}
}

// A sample set that belongs to another command is refused by the decoder, with
// the same answer the parser judge gives a V2 record set.
func TestJudgeRejectsNonBenchDataSamples(t *testing.T) {
	env := testEnv(t)
	files := map[string][]byte{}
	recs := validRecs()
	for i, stage := range benchprofile.RecordStages {
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		files[stage+benchprofile.FactFileNameSuffix] = data
	}
	// The parser judge's own wire: a different command's samples.
	parserWire := []byte(`{"schema":1,"kind":"parser-latency-samples","run":"` + testRun + `"}` + "\n")
	files["samples/"+samplesName] = parserWire
	publishFiles(t, env, testRun, files)
	wantJudged(t, env, testRun, "samples_incomplete")

	// This command's header with a field that is not the wire's.
	set := syntheticSet(testRun)
	data := sampleBytes(t, set)
	data = bytes.Replace(data, []byte(`"executions":5`), []byte(`"executions":5,"rounds":30`), 1)
	env = testEnv(t)
	files["samples/"+samplesName] = data
	publishFiles(t, env, testRun, files)
	wantJudged(t, env, testRun, "samples_incomplete")

	// This command's header with a value of the wrong type.
	data = bytes.Replace(sampleBytes(t, syntheticSet(testRun)), []byte(`"trimpath":true`), []byte(`"trimpath":"yes"`), 1)
	env = testEnv(t)
	files["samples/"+samplesName] = data
	publishFiles(t, env, testRun, files)
	wantJudged(t, env, testRun, "samples_malformed")
}

// Samples carry the run's identity and its one UTC date; another run, other
// inputs or another date are not this run's evidence.
func TestJudgeInvalidProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(set *sampleSet)
		reason string
	}{
		{"another sample run", func(set *sampleSet) { set.Header.Run = "bench-20261017T101501Z" }, "run_mismatch"},
		{"another input manifest", func(set *sampleSet) { set.Header.InputsSHA = strings.Repeat("c", 64) }, "inputs_mismatch"},
		{"a local offset", func(set *sampleSet) { set.Header.UTCOffset = 3600 }, "date_rollover"},
		{"a rolled-over date", func(set *sampleSet) { set.Header.EndDate = "20261018" }, "date_rollover"},
		{"a date outside the records", func(set *sampleSet) { set.Header.StartDate, set.Header.EndDate = "20261016", "20261016" }, "date_rollover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := syntheticSet(testRun)
			tc.mutate(set)
			env := testEnv(t)
			publish(t, env, testRun, validRecs(), set)
			wantJudged(t, env, testRun, tc.reason)
		})
	}
}

// controlOut makes TestJudgeControlSet publish the synthetic control samples
// where the benchmark evidence smoke exports them from; without it the test
// checks the writer and the reader against each other in a temporary directory.
var controlOut = flag.String("control-out", "", "directory that receives the synthetic control samples.jsonl")

// TestJudgeControlSet writes the independent synthetic control with the
// production writer and requires the production reader and the judge to accept
// it unchanged: the smoke fixture's samples are this command's own wire, not a
// second hand-written copy of it.
func TestJudgeControlSet(t *testing.T) {
	run := testRun
	dir := t.TempDir()
	if *controlOut != "" {
		abs, err := filepath.Abs(*controlOut)
		if err != nil {
			t.Fatal(err)
		}
		dir = abs
		run = filepath.Base(filepath.Dir(abs))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	set := syntheticSet(run)
	set.Header.InputsSHA = testInputs
	path := filepath.Join(dir, samplesName)
	if err := writeSamples(path, set); err != nil {
		t.Fatal(err)
	}
	got, err := parseSamples(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != set.Header || len(got.Samples) != len(set.Samples) {
		t.Fatalf("header or counts changed: %+v", got.Header)
	}
	for i, want := range set.Samples {
		g := got.Samples[i]
		if g.Series != want.Series || g.Side != want.Side || g.Exec != want.Exec || g.Purpose != want.Purpose ||
			g.Stat != want.Stat || !slices.Equal(g.Raw, want.Raw) {
			t.Fatalf("sample %d changed: %+v", i, g)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
