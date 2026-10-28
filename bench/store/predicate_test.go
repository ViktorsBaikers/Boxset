package storebench

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

func fullPolicy(queryOnly int64) store.ConnPolicy {
	return store.ConnPolicy{ForeignKeys: 1, BusyTimeoutMS: 5000, CacheSize: -8192, MmapSize: 64 << 20,
		JournalSizeLimit: 64 << 20, JournalMode: "wal", Synchronous: synchronousFull, QueryOnly: queryOnly, AutoVacuum: 2}
}

var testHost = Host{OS: "linux", Arch: "amd64", GoVersion: "go1.27", CPUs: 8, GOMAXPROCS: 8}

// skew is what an execution adds to the base value at a metric position. Its
// third smallest is 0, its mean is not, and a run of five rotates it by the
// position, so each metric's median comes from another execution and no
// execution, minimum, mean or pooled answer reproduces them all. The spread
// stays below the 100 ns between a series' percentiles.
var skew = [ExecutionsPerRun]int64{-45, -30, 0, 20, 49}

// tweak adds a value to the metric j of workload wi in every execution.
type tweak func(wi, j int) int64

func shifted(n int64) tweak { return func(int, int) int64 { return n } }

// rankOf is the nearest-rank p-th percentile of sorted samples, taken from
// the definition (the smallest value with at least p percent of the samples at
// or below it) and not from the production helper.
func rankOf(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Ceil(float64(len(sorted))*float64(p)/100))-1]
}

func seriesOf(a []int64) Series {
	s := slices.Clone(a)
	slices.Sort(s)
	return Series{rankOf(s, 50), rankOf(s, 95), rankOf(s, 99)}
}

func pooled(arrays [][]int64) Series { return seriesOf(slices.Concat(arrays...)) }

// samples is a sorted array of 100 whose p50, p95 and p99 are s; low spreads
// the values below p50, so two windows can differ and pool to the same series.
func samples(s Series, low int64) []int64 {
	a := make([]int64, 100)
	for i := range a {
		switch {
		case i < 49:
			a[i] = s.P50 - low*int64(49-i)
		case i < 94:
			a[i] = s.P50
		case i < 98:
			a[i] = s.P95
		default:
			a[i] = s.P99
		}
	}
	return a
}

// testArrays are the raw windows of execution i: per workload two windows of
// 100 samples per array, whose series at metric position j are the base
// value 1000+100j (+7 for the second workload), the skew of that execution
// and adj.
func testArrays(i int, adj tweak) [][]RawWindow {
	out := make([][]RawWindow, 2)
	for wi := range out {
		val := func(j int) int64 {
			return int64(1000+100*j+7*wi) + skew[(i+9*wi+j)%ExecutionsPerRun] + adj(wi, j)
		}
		series := func(c int) Series { return Series{val(3 * c), val(3*c + 1), val(3*c + 2)} }
		for _, low := range []int64{1, 2} {
			out[wi] = append(out[wi], RawWindow{Commit: samples(series(0), low), Read: samples(series(1), low), Wait: samples(series(2), low)})
		}
	}
	return out
}

func testArraysSet(adj tweak) [][][]RawWindow {
	out := make([][][]RawWindow, ExecutionsPerRun)
	for i := range out {
		out[i] = testArrays(i, adj)
	}
	return out
}

// buildExecution is execution i of a run: every series and window is derived
// from the given raw windows (arrays[workload][window]), the raw samples are
// their encoding, and every other recorded value is distinct and non-zero.
func buildExecution(i int, arrays [][]RawWindow) Run {
	c := int64(1000 * (i + 1))
	next := func() int64 { c++; return c }
	run := Run{Kind: KindExecution, Host: testHost}
	for wi, name := range []string{WorkloadChurn, WorkloadSkew} {
		w := Workload{Name: name, Shape: name + "-shape",
			Settings: Settings{Writer: fullPolicy(0), Reader: fullPolicy(1), ReaderCount: 4, WriterMaxOpen: 1, ReaderMaxOpen: 4},
			Connects: Connects{Writer: next(), Reader: next()},
			Allocs:   Allocs{Mallocs: next(), Bytes: next(), Ops: next()},
			WriteAmp: WriteAmp{WrittenBytes: next(), LogicalBytes: next(), Milli: next()},
			WAL: WAL{FileBytes: next(), DBBytes: next(), PageSize: next(), CheckpointBusy: next(),
				LogFrames: next(), Checkpointed: next()},
			Memory: Memory{VmHWMKiB: next(), VmRSSKiB: next(), GoSysBytes: next(), GoHeapInuseBytes: next()},
			Plans: map[string][]string{
				"rollup_row":   {name + " SEARCH title_rollup USING PRIMARY KEY"},
				"title_search": {name + " SCAN title_search VIRTUAL TABLE INDEX 0:M1", name + " SEARCH title_alias USING INTEGER PRIMARY KEY"},
			},
		}
		var commits, reads, waits [][]int64
		for _, a := range arrays[wi] {
			w.Windows = append(w.Windows, Window{Probes: int64(len(a.Commit)), Reads: int64(len(a.Read)),
				Commit: seriesOf(a.Commit), Read: seriesOf(a.Read), Wait: seriesOf(a.Wait)})
			commits, reads, waits = append(commits, a.Commit), append(reads, a.Read), append(waits, a.Wait)
		}
		w.Commit, w.Read, w.Wait = pooled(commits), pooled(reads), pooled(waits)
		raw, err := encodeRaw(arrays[wi], RawLimit, FileLimit)
		if err != nil {
			panic(fmt.Sprintf("fixture raw samples: %v", err))
		}
		w.Raw = raw
		run.Workloads = append(run.Workloads, w)
	}
	return run
}

func executionsOf(arrays [][][]RawWindow) []Run {
	out := make([]Run, len(arrays))
	for i, a := range arrays {
		out[i] = buildExecution(i, a)
	}
	return out
}

// testRun is a median run of the given kind over five valid executions.
func testRun(kind string) Run { return Median(kind, executionsOf(testArraysSet(shifted(0)))) }

// editAll applies f to every execution of r and re-derives the median, so a
// run stays consistent whatever is edited. The raw samples are not touched.
func editAll(r Run, f func(*Run)) Run {
	execs := slices.Clone(r.Executions)
	for i := range execs {
		execs[i].Workloads = slices.Clone(execs[i].Workloads)
		f(&execs[i])
	}
	return Median(r.Kind, execs)
}

// withMetric returns r with one compared metric set to v in all five
// executions, the median re-derived.
func withMetric(r Run, wi, j int, v int64) Run {
	return editAll(r, func(e *Run) { *e.Workloads[wi].metric(j) = v })
}

// withExec returns r with only execution i edited. The median is not
// re-derived, so an edit that changes a measured value makes the run
// inconsistent with it.
func withExec(r Run, i int, f func(*Run)) Run {
	execs := slices.Clone(r.Executions)
	e := execs[i]
	e.Workloads = slices.Clone(e.Workloads)
	for k := range e.Workloads {
		e.Workloads[k].Plans = maps.Clone(e.Workloads[k].Plans)
		e.Workloads[k].Windows = slices.Clone(e.Workloads[k].Windows)
	}
	f(&e)
	execs[i] = e
	r.Executions = execs
	return r
}

// withRaw returns r with execution i's workload wi retaining the samples of
// mutate applied to the arrays it was measured from; its windows and series
// stay as measured.
func withRaw(r Run, i, wi int, mutate func([]RawWindow) []RawWindow) Run {
	raw, err := encodeRaw(mutate(testArrays(i, shifted(0))[wi]), RawLimit, FileLimit)
	if err != nil {
		panic(fmt.Sprintf("fixture raw samples: %v", err))
	}
	return withExec(r, i, func(e *Run) { e.Workloads[wi].Raw = raw })
}

// textOf is the JSON text of windows, one value each, as the codec writes it.
func textOf(ws []RawWindow) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, w := range ws {
		if err := enc.Encode(w); err != nil {
			panic(err)
		}
	}
	return b.String()
}

func gzRaw(text string) Raw {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := io.WriteString(zw, text); err != nil || zw.Close() != nil {
		panic("fixture gzip")
	}
	return Raw{Codec: RawCodec, Data: b.Bytes()}
}

// gunzipWindows decodes retained samples with the standard library alone.
func gunzipWindows(raw Raw) ([]RawWindow, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw.Data))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(zr)
	var out []RawWindow
	for {
		var w RawWindow
		if err := dec.Decode(&w); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
}

// matchArrays reports the first way got differs from what the generated
// arrays (arrays[execution][workload][window]) imply: the retained samples,
// every window and pooled series, and each of the 18 medians, all derived
// again from the arrays without the codec.
func matchArrays(got Run, arrays [][][]RawWindow) error {
	if len(got.Executions) != len(arrays) {
		return fmt.Errorf("%d executions, want %d", len(got.Executions), len(arrays))
	}
	derived := make([][]Workload, len(arrays))
	for i, e := range got.Executions {
		for wi, w := range e.Workloads {
			ws := arrays[i][wi]
			dec, err := gunzipWindows(w.Raw)
			if err != nil {
				return fmt.Errorf("execution %d %s raw: %w", i, w.Name, err)
			}
			if !reflect.DeepEqual(dec, ws) {
				return fmt.Errorf("execution %d %s: the retained samples are not the generated ones", i, w.Name)
			}
			var d Workload
			var commits, reads, waits [][]int64
			for k, a := range ws {
				want := Window{Probes: int64(len(a.Commit)), Reads: int64(len(a.Read)), Commit: seriesOf(a.Commit), Read: seriesOf(a.Read), Wait: seriesOf(a.Wait)}
				if k >= len(w.Windows) || w.Windows[k] != want {
					return fmt.Errorf("execution %d %s window %d is not derived from its samples", i, w.Name, k)
				}
				commits, reads, waits = append(commits, a.Commit), append(reads, a.Read), append(waits, a.Wait)
			}
			d.Commit, d.Read, d.Wait = pooled(commits), pooled(reads), pooled(waits)
			if d.Commit != w.Commit || d.Read != w.Read || d.Wait != w.Wait {
				return fmt.Errorf("execution %d %s pooled series are not derived from its samples", i, w.Name)
			}
			derived[i] = append(derived[i], d)
		}
	}
	for wi, w := range got.Workloads {
		for j := range 9 {
			v := make([]int64, len(derived))
			for i := range derived {
				v[i] = *derived[i][wi].metric(j)
			}
			slices.Sort(v)
			if got := *w.metric(j); got != v[len(v)/2] {
				return fmt.Errorf("%s %s median %d, want the third smallest %d of %v", w.Name, w.Metrics()[j].Name, got, v[len(v)/2], v)
			}
		}
	}
	return nil
}

func wantVerdict(t *testing.T, got Result, want Verdict, reason string) {
	t.Helper()
	if got.Verdict != want || got.Reason != reason {
		t.Fatalf("verdict %s %q, want %s %q (checks %+v)", got.Verdict, got.Reason, want, reason, got.Checks)
	}
}

// errOutput is the one fixed failure of a failWriter: a test finds it again
// with errors.Is wherever the code under test reports its output failure.
var errOutput = errors.New("storebench test: output failed")

// failWriter counts its writes and, from write number failAt on (1-based, 0
// never), returns errOutput and writes nothing. It is the failing sink of the
// output-fault tests; with failAt 0 it is their counting writer.
type failWriter struct {
	out    io.Writer
	failAt int
	writes int
}

func (f *failWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.failAt > 0 && f.writes >= f.failAt {
		return 0, errOutput
	}
	return f.out.Write(p)
}

// checkRunFile reports why the file at path is not exactly the run want: its
// decoded executions, medians and every raw Data byte.
func checkRunFile(path string, want Run) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the run file: %w", err)
	}
	got, err := DecodeRun(data)
	if err != nil {
		return fmt.Errorf("the run file does not decode: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("the run file is not the completed run")
	}
	return nil
}

// TestForegroundPredicate: later*100 <= base*110 on integers, for every metric
// of both workloads (writer-wait included) of a median run; synchronous other
// than FULL fails even when faster; an ineligible or incomparable run is
// INVALID, never a pass; and the run's five executions with their raw samples
// are the evidence of every compared number.
func TestForegroundPredicate(t *testing.T) {
	base := testRun(KindBaseline)
	later := testRun(KindCompare)

	t.Run("the fixture is honest", func(t *testing.T) {
		for _, r := range []Run{base, later} {
			if got := Consistent(r); got != "" {
				t.Fatalf("fixture consistency: %q", got)
			}
			if got := Recorded(r); got != "" {
				t.Fatalf("fixture records: %q", got)
			}
		}
		for i, e := range base.Executions {
			for wi, w := range e.Workloads {
				for k, a := range testArrays(i, shifted(0))[wi] {
					if got := Summarize(slices.Clone(a.Commit)); w.Windows[k].Commit != got {
						t.Fatalf("execution %d %s window %d: stored %+v, production %+v", i, w.Name, k, w.Windows[k].Commit, got)
					}
				}
			}
		}
	})

	t.Run("identical run passes", func(t *testing.T) {
		res := Compare(base, later)
		wantVerdict(t, res, Pass, "")
		if len(res.Checks) != 18 {
			t.Fatalf("%d checks, want 18 (9 metrics of 2 workloads)", len(res.Checks))
		}
		if res.Line() != "STORE-FOREGROUND-PASS" {
			t.Fatalf("line %q", res.Line())
		}
	})

	t.Run("exactly plus 10 percent passes and one above fails, every metric", func(t *testing.T) {
		for wi := range base.Workloads {
			for j := range 9 {
				name := base.Workloads[wi].Name + "/" + base.Workloads[wi].Metrics()[j].Name
				for _, b := range []int64{1000, 999, 7, 1} { // 999*110/100 and 7*110/100 and 1*110/100 are not whole: the floor is the limit
					limit := b * 110 / 100
					r := withMetric(base, wi, j, b)
					wantVerdict(t, Compare(r, withMetric(r, wi, j, limit)), Pass, "")
					res := Compare(r, withMetric(r, wi, j, limit+1))
					wantVerdict(t, res, Fail, "regression")
					if res.Line() == "STORE-FOREGROUND-PASS" || !strings.HasPrefix(res.Line(), "STORE-FOREGROUND-FAIL") {
						t.Fatalf("%s base %d: line %q", name, b, res.Line())
					}
					var failed []string
					for _, c := range res.Checks {
						if !c.OK {
							failed = append(failed, c.Workload+"/"+c.Metric)
						}
					}
					if len(failed) != 1 || failed[0] != name {
						t.Fatalf("base %d failed %v, want only %s", b, failed, name)
					}
				}
			}
		}
	})

	t.Run("faster passes", func(t *testing.T) {
		wantVerdict(t, Compare(base, withMetric(base, 0, 6, 1)), Pass, "")
	})

	t.Run("synchronous other than FULL fails even when faster", func(t *testing.T) {
		for _, sync := range []int64{0, 1, 3} {
			for wi := range base.Workloads {
				for _, reader := range []bool{false, true} {
					r := editAll(later, func(e *Run) {
						*e.Workloads[wi].metric(0) = 1 // faster
						if reader {
							e.Workloads[wi].Settings.Reader.Synchronous = sync
						} else {
							e.Workloads[wi].Settings.Writer.Synchronous = sync
						}
					})
					wantVerdict(t, Compare(base, r), Fail, "synchronous_not_full")
				}
			}
		}
	})

	t.Run("invalid evidence", func(t *testing.T) {
		mut := func(f func(*Run)) Run { return editAll(testRun(KindCompare), f) }
		mutBase := func(f func(*Run)) Run { return editAll(testRun(KindBaseline), f) }
		for _, c := range []struct {
			name   string
			base   Run
			later  Run
			reason string
		}{
			{"race later", base, mut(func(r *Run) { r.Facts.Race = true }), "later_race_build"},
			{"race base", mutBase(func(r *Run) { r.Facts.Race = true }), later, "base_race_build"},
			{"swap in", base, mut(func(r *Run) { r.Facts.SwapIn = 1 }), "later_swap_activity"},
			{"swap out", base, mut(func(r *Run) { r.Facts.SwapOut = 1 }), "later_swap_activity"},
			{"swapped memory", base, mut(func(r *Run) { r.Facts.SwapKiB = 4 }), "later_swap_activity"},
			{"base swap", mutBase(func(r *Run) { r.Facts.SwapOut = 9 }), later, "base_swap_activity"},
			{"non-linux later", base, mut(func(r *Run) { r.Host.OS = "darwin" }), "later_non_linux"},
			{"non-linux base", mutBase(func(r *Run) { r.Host.OS = "windows" }), later, "base_non_linux"},
			{"host arch", base, mut(func(r *Run) { r.Host.Arch = "arm64" }), "host_mismatch"},
			{"host go version", base, mut(func(r *Run) { r.Host.GoVersion = "go1.28" }), "host_mismatch"},
			{"host cpus", base, mut(func(r *Run) { r.Host.CPUs++ }), "host_mismatch"},
			{"host gomaxprocs", base, mut(func(r *Run) { r.Host.GOMAXPROCS++ }), "host_mismatch"},
			{"workload shape", base, mut(func(r *Run) { r.Workloads[1].Shape = "other" }), "workload_mismatch"},
			{"workload name", base, mut(func(r *Run) { r.Workloads[0].Name = "other" }), "workload_mismatch"},
			{"workload missing", base, mut(func(r *Run) { r.Workloads = r.Workloads[:1] }), "workload_mismatch"},
			{"settings", base, mut(func(r *Run) { r.Workloads[0].Settings.Writer.CacheSize = -1 }), "settings_mismatch"},
			{"reader pool", base, mut(func(r *Run) { r.Workloads[0].Settings.ReaderMaxOpen = 1 }), "settings_mismatch"},
			{"zero later metric", base, withMetric(later, 0, 4, 0), "later_zero_metric"},
			{"negative later metric", base, withMetric(later, 0, 4, -1), "later_zero_metric"},
			{"base not a baseline", mut(func(r *Run) {}), later, "base_not_baseline"},
			{"base one workload", mutBase(func(r *Run) { r.Workloads = r.Workloads[:1] }), later, "base_workload_mismatch"},
			{"base not FULL", mutBase(func(r *Run) { r.Workloads[0].Settings.Writer.Synchronous = 1 }), later, "base_synchronous_not_full"},
		} {
			t.Run(c.name, func(t *testing.T) {
				res := Compare(c.base, c.later)
				wantVerdict(t, res, Invalid, c.reason)
				if res.Line() != "STORE-FOREGROUND-INVALID reason="+c.reason {
					t.Fatalf("line %q", res.Line())
				}
			})
		}
		t.Run("zero baseline metric, every metric", func(t *testing.T) {
			for wi := range base.Workloads {
				for j := range 9 {
					wantVerdict(t, Compare(withMetric(base, wi, j, 0), later), Invalid, "base_zero_metric")
				}
			}
		})
	})

	t.Run("only a baseline-eligible run is recorded", func(t *testing.T) {
		if got := Baseline(base); got != "" {
			t.Fatalf("baseline of a clean run: %q", got)
		}
		if got := Baseline(testRun(KindCompare)); got != "not_baseline" {
			t.Fatalf("a compare run recorded as baseline: %q", got)
		}
		fast := editAll(base, func(e *Run) { e.Workloads[0].Settings.Writer.Synchronous = 0 })
		if got := Baseline(fast); got != "synchronous_not_full" {
			t.Fatalf("baseline with synchronous OFF: %q", got)
		}
	})

	t.Run("a run that is not a pass never prints a pass", func(t *testing.T) {
		for _, r := range []Result{{Verdict: Fail, Reason: "regression"}, {Verdict: Invalid, Reason: "x"}} {
			if strings.Contains(r.Line(), "PASS") {
				t.Fatalf("line %q", r.Line())
			}
		}
		if strings.Contains(BaselineLine, "PASS") || strings.Contains(CaptureLine, "PASS") {
			t.Fatal("a recorded or captured outcome line says PASS")
		}
	})

	t.Run("windows are pooled, not minimised", func(t *testing.T) {
		window := func(ns int64) []int64 {
			s := make([]int64, 1000)
			for i := range s {
				s[i] = ns
			}
			return s
		}
		withSeries := func(r Run, s Series) Run {
			return editAll(r, func(e *Run) {
				for i := range e.Workloads {
					e.Workloads[i].Commit, e.Workloads[i].Read, e.Workloads[i].Wait = s, s, s
				}
			})
		}
		flat := Pool([][]int64{window(1000000), window(1000000), window(1000000), window(1000000), window(1000000)})
		if flat != (Series{1000000, 1000000, 1000000}) {
			t.Fatalf("five equal windows: %+v", flat)
		}
		b := withSeries(testRun(KindBaseline), flat)
		// four windows doubled, one unchanged: a window minimum would report 1000000.
		fourSlow := Pool([][]int64{window(1000000), window(2000000), window(2000000), window(2000000), window(2000000)})
		if fourSlow != (Series{2000000, 2000000, 2000000}) {
			t.Fatalf("four slow windows: %+v", fourSlow)
		}
		res := Compare(b, withSeries(testRun(KindCompare), fourSlow))
		wantVerdict(t, res, Fail, "regression")
		if strings.Contains(res.Line(), "PASS") {
			t.Fatalf("line %q", res.Line())
		}
		// one slow window of five: p95 and p99 sit in it, p50 does not.
		oneSlow := Pool([][]int64{window(1000000), window(1000000), window(1000000), window(1000000), window(2000000)})
		if oneSlow != (Series{1000000, 2000000, 2000000}) {
			t.Fatalf("one slow window: %+v", oneSlow)
		}
		res = Compare(b, withSeries(testRun(KindCompare), oneSlow))
		wantVerdict(t, res, Fail, "regression")
	})

	t.Run("pool of no sample and of one sample", func(t *testing.T) {
		if got := Pool(nil); got != (Series{}) {
			t.Fatalf("no window: %+v", got)
		}
		if got := Pool([][]int64{nil, {}}); got != (Series{}) {
			t.Fatalf("empty windows: %+v", got)
		}
		if got := Pool([][]int64{nil, {7}}); got != (Series{7, 7, 7}) {
			t.Fatalf("one sample: %+v", got)
		}
		if got := Pool([][]int64{{9, 3}, {7}}); got != (Series{7, 9, 9}) {
			t.Fatalf("samples of two windows together: %+v", got)
		}
	})

	t.Run("a baseline built by another estimator is another workload", func(t *testing.T) {
		if AggregateShape != "aggregate=full-sample-nearest-rank" {
			t.Fatalf("aggregate shape %q", AggregateShape)
		}
		old := editAll(base, func(e *Run) { e.Workloads[0].Shape = "titles=16 rounds=5" })
		now := editAll(testRun(KindCompare), func(e *Run) { e.Workloads[0].Shape = "titles=16 rounds=5 " + AggregateShape })
		wantVerdict(t, Compare(old, now), Invalid, "workload_mismatch")
	})

	t.Run("a zero or empty later series is invalid, never an improvement", func(t *testing.T) {
		for wi := range base.Workloads {
			for j := range 9 {
				for _, v := range []int64{0, -1} {
					res := Compare(base, withMetric(later, wi, j, v))
					wantVerdict(t, res, Invalid, "later_zero_metric")
					if strings.Contains(res.Line(), "PASS") {
						t.Fatalf("line %q", res.Line())
					}
				}
			}
		}
		empty := editAll(later, func(e *Run) { e.Workloads[1].Read = Pool(nil) }) // no read was measured
		res := Compare(base, empty)
		wantVerdict(t, res, Invalid, "later_zero_metric")
		if strings.Contains(res.Line(), "PASS") {
			t.Fatalf("line %q", res.Line())
		}
		wantVerdict(t, Compare(withMetric(base, 1, 4, -1), later), Invalid, "base_zero_metric")
	})

	t.Run("a read counts when it started while recording", func(t *testing.T) {
		var recording atomic.Bool
		recording.Store(true)
		_, counted, err := Sample(&recording, func() error {
			recording.Store(false) // recording turns off while the read is in flight
			return nil
		})
		if err != nil || !counted || recording.Load() {
			t.Fatalf("in-flight read: counted=%t recording=%t err=%v", counted, recording.Load(), err)
		}
		ran := false
		_, counted, err = Sample(&recording, func() error { ran = true; return nil })
		if err != nil || counted || !ran {
			t.Fatalf("read started after recording stopped: counted=%t ran=%t err=%v", counted, ran, err)
		}
		boom := errors.New("boom")
		if _, _, err = Sample(&recording, func() error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("op error lost: %v", err)
		}

		// The duration is the second clock reading minus the first, taken around op.
		t0 := time.Unix(1_700_000_000, 0)
		const want = 1234567 * time.Nanosecond
		for _, c := range []struct {
			name string
			on   bool
			op   func(*atomic.Bool) error
			want bool
			err  error
		}{
			{"turns recording off while in flight", true, func(r *atomic.Bool) error { r.Store(false); return nil }, true, nil},
			{"starts after recording stopped", false, func(*atomic.Bool) error { return nil }, false, nil},
			{"fails", true, func(*atomic.Bool) error { return boom }, true, boom},
		} {
			var calls []string
			reading := 0
			now := func() time.Time {
				calls = append(calls, "now")
				reading++
				return t0.Add(time.Duration(reading-1) * want)
			}
			var rec atomic.Bool
			rec.Store(c.on)
			d, counted, err := sample(now, &rec, func() error { calls = append(calls, "op"); return c.op(&rec) })
			if d != want || counted != c.want || !errors.Is(err, c.err) || !slices.Equal(calls, []string{"now", "op", "now"}) {
				t.Errorf("%s: d=%v counted=%t err=%v calls=%v", c.name, d, counted, err, calls)
			}
		}
	})

	t.Run("every recorded field is required of each execution", func(t *testing.T) {
		if got := Recorded(base); got != "" {
			t.Fatalf("fixture: %q", got)
		}
		if got := Recorded(Run{}); got != "executions_incomplete" {
			t.Fatalf("no execution: %q", got)
		}
		if got := Recorded(withExec(base, 3, func(e *Run) { e.Workloads = nil })); got != "execution3_no_workload" {
			t.Fatalf("an execution without workload: %q", got)
		}
		for _, c := range []struct {
			name   string
			mutate func(*Workload)
			want   string
		}{
			{"no plans", func(w *Workload) { w.Plans = nil }, "_plans_missing"},
			{"empty plan", func(w *Workload) { w.Plans["rollup_row"] = nil }, "_plans_missing"},
			{"mallocs", func(w *Workload) { w.Allocs.Mallocs = 0 }, "_allocs_missing"},
			{"alloc bytes", func(w *Workload) { w.Allocs.Bytes = 0 }, "_allocs_missing"},
			{"ops", func(w *Workload) { w.Allocs.Ops = 0 }, "_allocs_missing"},
			{"written", func(w *Workload) { w.WriteAmp.WrittenBytes = 0 }, "_write_amp_missing"},
			{"logical", func(w *Workload) { w.WriteAmp.LogicalBytes = 0 }, "_write_amp_missing"},
			{"milli", func(w *Workload) { w.WriteAmp.Milli = 0 }, "_write_amp_missing"},
			{"wal file negative", func(w *Workload) { w.WAL.FileBytes = -1 }, "_wal_missing"},
			{"db file", func(w *Workload) { w.WAL.DBBytes = 0 }, "_wal_missing"},
			{"page size", func(w *Workload) { w.WAL.PageSize = 0 }, "_wal_missing"},
			{"log frames negative", func(w *Workload) { w.WAL.LogFrames = -1 }, "_wal_missing"},
			{"checkpointed negative", func(w *Workload) { w.WAL.Checkpointed = -1 }, "_wal_missing"},
			{"hwm", func(w *Workload) { w.Memory.VmHWMKiB = 0 }, "_memory_missing"},
			{"rss", func(w *Workload) { w.Memory.VmRSSKiB = 0 }, "_memory_missing"},
			{"go sys", func(w *Workload) { w.Memory.GoSysBytes = 0 }, "_memory_missing"},
			{"go heap", func(w *Workload) { w.Memory.GoHeapInuseBytes = 0 }, "_memory_missing"},
		} {
			for _, i := range []int{0, ExecutionsPerRun - 1} {
				for wi, name := range []string{WorkloadChurn, WorkloadSkew} {
					r := withExec(base, i, func(e *Run) { c.mutate(&e.Workloads[wi]) })
					want := fmt.Sprintf("execution%d_%s%s", i, name, c.want)
					if got := Recorded(r); got != want {
						t.Errorf("%s of %s in execution %d: %q, want %q", c.name, name, i, got, want)
					}
				}
			}
		}
		for _, i := range []int{0, ExecutionsPerRun - 1} {
			for wi, name := range []string{WorkloadChurn, WorkloadSkew} {
				r := withExec(base, i, func(e *Run) {
					w := &e.Workloads[wi]
					w.WAL.FileBytes, w.WAL.LogFrames, w.WAL.Checkpointed = 0, 0, 0
				})
				if got := Recorded(r); got != "" {
					t.Errorf("an empty WAL of %s in execution %d: %q, want none", name, i, got)
				}
			}
		}
	})

	t.Run("the record lines carry every recorded field of every execution", func(t *testing.T) {
		var out bytes.Buffer
		for i, e := range base.Executions {
			if err := PrintExecution(&out, i, e); err != nil {
				t.Fatal(err)
			}
		}
		lines := out.String()
		for i, e := range base.Executions {
			if want := fmt.Sprintf("STORE-RECORD execution=%d\n", i); !strings.Contains(lines, want) {
				t.Errorf("no execution marker %q", want)
			}
			for _, w := range e.Workloads {
				for _, want := range []string{
					fmt.Sprintf("workload=%s shape=%q", w.Name, w.Shape),
					fmt.Sprintf("workload=%s connects writer=%d reader=%d", w.Name, w.Connects.Writer, w.Connects.Reader),
					fmt.Sprintf("workload=%s allocs mallocs=%d bytes=%d ops=%d", w.Name, w.Allocs.Mallocs, w.Allocs.Bytes, w.Allocs.Ops),
					fmt.Sprintf("workload=%s write_amplification written_bytes=%d logical_bytes=%d milli=%d", w.Name, w.WriteAmp.WrittenBytes, w.WriteAmp.LogicalBytes, w.WriteAmp.Milli),
					fmt.Sprintf("workload=%s wal file_bytes=%d db_bytes=%d page_size=%d checkpoint_busy=%d log_frames=%d checkpointed_frames=%d",
						w.Name, w.WAL.FileBytes, w.WAL.DBBytes, w.WAL.PageSize, w.WAL.CheckpointBusy, w.WAL.LogFrames, w.WAL.Checkpointed),
					fmt.Sprintf("workload=%s memory vm_hwm_kib=%d vm_rss_kib=%d go_sys_bytes=%d go_heap_inuse_bytes=%d",
						w.Name, w.Memory.VmHWMKiB, w.Memory.VmRSSKiB, w.Memory.GoSysBytes, w.Memory.GoHeapInuseBytes),
					fmt.Sprintf("workload=%s settings writer=%+v reader=%+v reader_count=%d writer_max_open=%d reader_max_open=%d",
						w.Name, w.Settings.Writer, w.Settings.Reader, w.Settings.ReaderCount, w.Settings.WriterMaxOpen, w.Settings.ReaderMaxOpen),
				} {
					if !strings.Contains(lines, "STORE-RECORD "+want+"\n") {
						t.Errorf("execution %d: no record line %q", i, want)
					}
				}
				for name, plan := range w.Plans {
					for _, d := range plan {
						if want := fmt.Sprintf("STORE-RECORD workload=%s plan=%s detail=%q\n", w.Name, name, d); !strings.Contains(lines, want) {
							t.Errorf("execution %d: no record line %q", i, want)
						}
					}
				}
				for k, win := range w.Windows {
					if want := fmt.Sprintf("STORE-RECORD workload=%s window=%d probes=%d reads=%d", w.Name, k, win.Probes, win.Reads); !strings.Contains(lines, want) {
						t.Errorf("execution %d: no record line %q", i, want)
					}
				}
				for _, m := range w.Metrics() {
					if want := fmt.Sprintf("STORE-RECORD workload=%s metric=%s value=%d\n", w.Name, m.Name, m.Value); !strings.Contains(lines, want) {
						t.Errorf("execution %d: no record line %q", i, want)
					}
				}
			}
		}
	})

	t.Run("record and compare through the baseline file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "baseline.json")
		noPass := func(t *testing.T, out string) {
			t.Helper()
			if strings.Contains(out, "PASS") {
				t.Fatalf("output says PASS:\n%s", out)
			}
		}

		var out bytes.Buffer
		if err := Record(&out, path, testRun(KindBaseline)); err != nil {
			t.Fatalf("eligible record: %v", err)
		}
		for _, want := range []string{"STORE-FOREGROUND-BASELINE\n", "STORE-RECORD baseline_written=true\n"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("record output lacks %q:\n%s", want, out.String())
			}
		}
		noPass(t, out.String())
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the eligible record wrote no file: %v", err)
		}
		if got, err := DecodeRun(written); err != nil || !reflect.DeepEqual(got, testRun(KindBaseline)) {
			t.Fatalf("written baseline: %v\n%+v", err, got)
		}

		out.Reset()
		if err := Record(&out, path, withMetric(testRun(KindBaseline), 0, 0, 5)); err != nil {
			t.Fatalf("second record: %v", err)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(after, written) {
			t.Fatal("a second record changed the baseline bytes")
		}
		if !strings.Contains(out.String(), "baseline_written=false") || strings.Contains(out.String(), "baseline_written=true") {
			t.Fatalf("second record output:\n%s", out.String())
		}
		noPass(t, out.String())

		single := Run{Kind: KindBaseline, Host: testHost, Workloads: base.Executions[0].Workloads}
		for name, r := range map[string]Run{
			"race":         func() Run { r := testRun(KindBaseline); r.Facts.Race = true; return r }(),
			"swap":         func() Run { r := testRun(KindBaseline); r.Facts.SwapOut = 1; return r }(),
			"zero metric":  withMetric(testRun(KindBaseline), 1, 8, 0),
			"compare kind": testRun(KindCompare),
			"single run":   single,
		} {
			fresh := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			out.Reset()
			if err := Record(&out, fresh, r); err == nil {
				t.Errorf("%s: ineligible record returned no error", name)
			}
			if _, err := os.Stat(fresh); !os.IsNotExist(err) {
				t.Errorf("%s: an ineligible record created a file (%v)", name, err)
			}
			if !strings.HasPrefix(out.String(), "STORE-FOREGROUND-INVALID reason=") {
				t.Errorf("%s: output %q", name, out.String())
			}
			noPass(t, out.String())
		}

		measure := func(r Run) func() Run { return func() Run { return r } }
		compare := func(path string, measure func() Run) Result {
			t.Helper()
			res, err := CompareFile(&out, path, measure)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		out.Reset()
		if res := compare(path, measure(testRun(KindCompare))); res.Verdict != Pass || !strings.Contains(out.String(), "STORE-FOREGROUND-PASS\n") {
			t.Fatalf("a later run inside the predicate: %+v\n%s", res, out.String())
		}
		limit := testRun(KindBaseline).Workloads[1].Wait.P99 * 110 / 100
		out.Reset()
		if res := compare(path, measure(withMetric(testRun(KindCompare), 1, 8, limit))); res.Verdict != Pass {
			t.Fatalf("a later run at exactly plus 10 percent: %+v\n%s", res, out.String())
		}
		out.Reset()
		if res := compare(path, measure(withMetric(testRun(KindCompare), 1, 8, limit+1))); res.Verdict != Fail {
			t.Fatalf("a later run over the predicate: %+v", res)
		}
		noPass(t, out.String())
		if !strings.Contains(out.String(), "STORE-FOREGROUND-FAIL reason=regression\n") || !strings.Contains(out.String(), "metric=writer_wait_p99_ns") {
			t.Fatalf("over-predicate output:\n%s", out.String())
		}

		measured := false
		spy := func() Run { measured = true; return testRun(KindCompare) }
		out.Reset()
		if res := compare(filepath.Join(dir, "absent.json"), spy); res.Verdict != Invalid || out.String() != "STORE-FOREGROUND-INVALID reason=base_missing\n" {
			t.Fatalf("missing baseline: %+v %q", res, out.String())
		}
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if res := compare(bad, spy); res.Verdict != Invalid || out.String() != "STORE-FOREGROUND-INVALID reason=base_undecodable\n" {
			t.Fatalf("undecodable baseline: %+v %q", res, out.String())
		}
		if measured {
			t.Fatal("a run was measured against a baseline that did not load")
		}
	})

	t.Run("a failed output write keeps the record and capture outcomes", func(t *testing.T) {
		dir := t.TempDir()
		failingAt := func(at int, out io.Writer) *failWriter { return &failWriter{out: out, failAt: at} }
		missing := func(path string) bool { _, err := os.Stat(path); return os.IsNotExist(err) }

		// The outcome lines come after the file decision: a failed line is
		// reported and the decided file stands, complete and decodable.
		for at := 1; at <= 2; at++ {
			path := filepath.Join(dir, fmt.Sprintf("record-%d.json", at))
			if err := Record(failingAt(at, io.Discard), path, testRun(KindBaseline)); !errors.Is(err, errOutput) {
				t.Errorf("record with line %d failing: error %v", at, err)
			}
			if err := checkRunFile(path, testRun(KindBaseline)); err != nil {
				t.Errorf("record with line %d failing: %v", at, err)
			}
		}
		capturePath := filepath.Join(dir, "capture.json")
		if err := Capture(failingAt(1, io.Discard), capturePath, testRun(KindCompare)); !errors.Is(err, errOutput) {
			t.Errorf("capture with its line failing: error %v", err)
		}
		if err := checkRunFile(capturePath, testRun(KindCompare)); err != nil {
			t.Errorf("capture with its line failing: %v", err)
		}

		// An existing baseline stays byte for byte and is never reported as
		// written, whichever of its two lines fails.
		kept := filepath.Join(dir, "kept.json")
		if err := Record(io.Discard, kept, testRun(KindBaseline)); err != nil {
			t.Fatalf("record the baseline to keep: %v", err)
		}
		before, err := os.ReadFile(kept)
		if err != nil {
			t.Fatal(err)
		}
		for at := 1; at <= 2; at++ {
			var out bytes.Buffer
			if err := Record(failingAt(at, &out), kept, withMetric(testRun(KindBaseline), 0, 0, 5)); !errors.Is(err, errOutput) {
				t.Errorf("existing baseline with line %d failing: error %v", at, err)
			}
			if after, _ := os.ReadFile(kept); !bytes.Equal(after, before) {
				t.Errorf("existing baseline with line %d failing: the baseline bytes changed", at)
			}
			if strings.Contains(out.String(), "baseline_written=true") {
				t.Errorf("existing baseline with line %d failing: output says it was written:\n%s", at, out.String())
			}
		}

		// A refusal keeps its own error and no file; a failed INVALID line is
		// joined to it, never in its place.
		raced := testRun(KindBaseline)
		raced.Facts.Race = true
		for name, r := range map[string]Run{"race": raced, "compare kind": testRun(KindCompare)} {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			reason := Baseline(r)
			err := Record(failingAt(1, io.Discard), path, r)
			if reason == "" || err == nil || !errors.Is(err, errOutput) || !strings.Contains(err.Error(), "the run is not an eligible baseline: "+reason) {
				t.Errorf("%s record with its INVALID line failing: reason %q, error %v", name, reason, err)
			}
			if !missing(path) {
				t.Errorf("%s record with its INVALID line failing created a file", name)
			}
		}

		// Capture refuses another kind with its own error and prints nothing
		// new for it: no output line is required here.
		refused := filepath.Join(dir, "refused.json")
		if err := Capture(failingAt(1, io.Discard), refused, testRun(KindBaseline)); err == nil || !strings.Contains(err.Error(), "capture needs a compare run") {
			t.Errorf("capture of a baseline run: error %v", err)
		}
		if !missing(refused) {
			t.Error("capture of a baseline run created a file")
		}
	})

	t.Run("median of five executions", func(t *testing.T) {
		execs := executionsOf(testArraysSet(shifted(0)))
		run := Median(KindBaseline, execs)
		if run.Kind != KindBaseline || len(run.Executions) != ExecutionsPerRun {
			t.Fatalf("kind %q with %d executions", run.Kind, len(run.Executions))
		}
		for i := range execs {
			if !reflect.DeepEqual(run.Executions[i], execs[i]) {
				t.Fatalf("execution %d is not kept in its place", i)
			}
		}
		arrays := testArraysSet(shifted(0))
		supplier := map[int]int{}
		for wi := range run.Workloads {
			w := run.Workloads[wi]
			if len(w.Windows) != 0 || w.Raw.Data != nil || w.Plans != nil || w.Allocs != (Allocs{}) {
				t.Fatalf("a median workload carries more than the compared series: %+v", w)
			}
			for j := range 9 {
				vals := make([]int64, ExecutionsPerRun)
				for i, e := range execs {
					vals[i] = *e.Workloads[wi].metric(j)
				}
				sorted := slices.Sorted(slices.Values(vals))
				got := *w.metric(j)
				var sum int64
				for _, v := range vals {
					sum += v
				}
				if got != sorted[2] || got == sorted[0] || float64(got) == float64(sum)/ExecutionsPerRun {
					t.Fatalf("%s metric %d: median %d of %v", w.Name, j, got, vals)
				}
				supplier[slices.Index(vals, got)]++
				if j%3 != 2 { // the pooled p50 and p95 differ from the median: a pooling estimator would be caught
					var all [][]int64
					for i := range arrays {
						for _, win := range arrays[i][wi] {
							all = append(all, [][]int64{win.Commit, win.Read, win.Wait}[j/3])
						}
					}
					pool := seriesOf(slices.Concat(all...))
					if got == [...]int64{pool.P50, pool.P95}[j%3] {
						t.Fatalf("%s metric %d: the pooled percentile equals the median %d, the fixture cannot tell them apart", w.Name, j, got)
					}
				}
			}
		}
		if len(supplier) != ExecutionsPerRun {
			t.Fatalf("medians come from executions %v: every execution should supply some and none all", supplier)
		}

		facts := slices.Clone(execs)
		facts[0].Facts.SwapIn, facts[1].Facts.SwapIn = 2, 3
		facts[2].Facts.SwapOut, facts[2].Facts.Race = 4, true
		facts[3].Facts.SwapKiB, facts[4].Facts.SwapKiB = 9, 5
		if got := Median(KindCompare, facts).Facts; got != (Facts{Race: true, SwapIn: 5, SwapOut: 4, SwapKiB: 9}) {
			t.Fatalf("facts of the median: %+v", got)
		}
	})

	t.Run("a series is valid only whole, alike and eligible", func(t *testing.T) {
		wantBoth := func(t *testing.T, name string, r Run, reason string) {
			t.Helper()
			l := r
			l.Kind = KindCompare
			wantVerdict(t, Compare(base, l), Invalid, "later_"+reason)
			wantVerdict(t, Compare(l, later), Invalid, "base_"+reason) // the same run as a baseline
			if r.Kind == KindBaseline {
				if got := Baseline(r); got != reason {
					t.Errorf("%s: baseline %q, want %q", name, got, reason)
				}
			}
		}
		for _, n := range []int{0, 1, 4, 6} {
			r := base
			r.Executions = slices.Clone(base.Executions[:min(n, ExecutionsPerRun)])
			for len(r.Executions) < n {
				r.Executions = append(r.Executions, base.Executions[0])
			}
			wantBoth(t, fmt.Sprintf("%d executions", n), r, "executions_incomplete")
		}
		single := Run{Kind: KindBaseline, Host: testHost, Workloads: base.Executions[0].Workloads}
		wantVerdict(t, Compare(single, later), Invalid, "base_executions_incomplete")

		for _, c := range []struct {
			name   string
			edit   func(*Run)
			reason string
		}{
			{"race", func(e *Run) { e.Facts.Race = true }, "execution_race_build"},
			{"swap in", func(e *Run) { e.Facts.SwapIn = 1 }, "execution_swap_activity"},
			{"swap out", func(e *Run) { e.Facts.SwapOut = 1 }, "execution_swap_activity"},
			{"swapped memory", func(e *Run) { e.Facts.SwapKiB = 1 }, "execution_swap_activity"},
			{"non-linux", func(e *Run) { e.Host.OS = "darwin" }, "execution_non_linux"},
			{"another kind", func(e *Run) { e.Kind = KindBaseline }, "execution_kind"},
			{"a nested series", func(e *Run) { e.Executions = []Run{{}} }, "execution_kind"},
			{"host", func(e *Run) { e.Host.Arch = "arm64" }, "execution_mismatch"},
			{"workload name", func(e *Run) { e.Workloads[0].Name = "other" }, "execution_mismatch"},
			{"workload shape", func(e *Run) { e.Workloads[1].Shape = "other" }, "execution_mismatch"},
			{"workload missing", func(e *Run) { e.Workloads = e.Workloads[:1] }, "execution_mismatch"},
			{"settings", func(e *Run) { e.Workloads[0].Settings.Reader.CacheSize = -1 }, "execution_mismatch"},
			{"synchronous", func(e *Run) { e.Workloads[1].Settings.Writer.Synchronous = 1 }, "execution_mismatch"},
		} {
			for i := range ExecutionsPerRun {
				wantBoth(t, fmt.Sprintf("%s in execution %d", c.name, i), withExec(base, i, c.edit), c.reason)
			}
		}
		for wi := range base.Workloads {
			for j := range 9 {
				r := base
				r.Workloads = slices.Clone(base.Workloads)
				*r.Workloads[wi].metric(j)++
				wantBoth(t, fmt.Sprintf("median %d/%d edited away", wi, j), r, "median_mismatch")
			}
		}
		r := base
		r.Workloads = slices.Clone(base.Workloads)
		r.Workloads[0].Allocs.Mallocs = 5 // a median workload carries nothing beside its series
		wantBoth(t, "extra median field", r, "median_mismatch")

		var out bytes.Buffer
		path := filepath.Join(t.TempDir(), "baseline.json")
		if err := Record(&out, path, single); err == nil || !strings.HasPrefix(out.String(), "STORE-FOREGROUND-INVALID reason=executions_incomplete") {
			t.Fatalf("a single-run record: %v %q", err, out.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a single-run record wrote a file: %v", err)
		}
	})

	t.Run("raw samples of every execution are required and must match", func(t *testing.T) {
		allowance := int64(21*600 + 64*2) // 600 values and two windows are declared by every workload
		for _, i := range []int{0, ExecutionsPerRun - 1} {
			for wi, name := range []string{WorkloadChurn, WorkloadSkew} {
				honest := testArrays(i, shifted(0))[wi]
				for _, c := range []struct {
					name string
					run  Run
					want string
				}{
					{"empty data", withExec(base, i, func(e *Run) { e.Workloads[wi].Raw.Data = nil }), "raw_missing"},
					{"another codec", withExec(base, i, func(e *Run) { e.Workloads[wi].Raw.Codec = "json+zstd" }), "raw_missing"},
					{"garbage", withExec(base, i, func(e *Run) { e.Workloads[wi].Raw.Data = []byte("garbage") }), "raw_undecodable"},
					{"truncated gzip", withExec(base, i, func(e *Run) { d := e.Workloads[wi].Raw.Data; e.Workloads[wi].Raw.Data = d[:len(d)/2] }), "raw_undecodable"},
					{"corrupt json", withExec(base, i, func(e *Run) { e.Workloads[wi].Raw = gzRaw(`{"Commit":[1,2`) }), "raw_undecodable"},
					{"extra field", withExec(base, i, func(e *Run) {
						e.Workloads[wi].Raw = gzRaw(strings.ReplaceAll(textOf(honest), `"Wait":`, `"Extra":1,"Wait":`))
					}), "raw_undecodable"},
					{"text one byte above the declared counts", withExec(base, i, func(e *Run) {
						text := textOf(honest)
						e.Workloads[wi].Raw = gzRaw(text + strings.Repeat(" ", int(allowance)+1-len(text)))
					}), "raw_undecodable"},
					{"text exactly at the declared counts", withExec(base, i, func(e *Run) {
						text := textOf(honest)
						e.Workloads[wi].Raw = gzRaw(text + strings.Repeat(" ", int(allowance)-len(text)))
					}), ""},
					{"fewer windows than declared", withRaw(base, i, wi, func(ws []RawWindow) []RawWindow { return ws[:1] }), "raw_undecodable"},
					{"more windows than declared", withRaw(base, i, wi, func(ws []RawWindow) []RawWindow { return append(ws, ws[0]) }), "raw_undecodable"},
					{"a value changed", withRaw(base, i, wi, func(ws []RawWindow) []RawWindow { ws[0].Commit[49] = 1; return ws }), "raw_mismatch"},
					{"a value dropped", withRaw(base, i, wi, func(ws []RawWindow) []RawWindow { ws[1].Read = ws[1].Read[1:]; return ws }), "raw_mismatch"},
					{"a value added", withRaw(base, i, wi, func(ws []RawWindow) []RawWindow { ws[0].Wait = append(ws[0].Wait, 1); return ws }), "raw_mismatch"},
					{"probes unequal to the commit array", withExec(base, i, func(e *Run) { e.Workloads[wi].Windows[0].Probes-- }), "raw_mismatch"},
					{"reads unequal to the read array", withExec(base, i, func(e *Run) { e.Workloads[wi].Windows[1].Reads++ }), "raw_mismatch"},
					{"a negative declared count", withExec(base, i, func(e *Run) { e.Workloads[wi].Windows[0].Reads = -1 }), "raw_undecodable"},
				} {
					want := ""
					if c.want != "" {
						want = fmt.Sprintf("execution%d_%s_%s", i, name, c.want)
					}
					if got := Recorded(c.run); got != want {
						t.Errorf("%s in execution %d %s: %q, want %q", c.name, i, name, got, want)
					}
				}
			}
		}
		if err := matchArrays(base, testArraysSet(shifted(0))); err != nil {
			t.Fatalf("the fixture raw samples: %v", err)
		}
		if matchArrays(base, testArraysSet(shifted(1))) == nil {
			t.Fatal("matchArrays accepted another run's arrays")
		}
	})

	t.Run("the series output marks every execution and holds no raw data", func(t *testing.T) {
		var out bytes.Buffer
		for i, e := range base.Executions {
			if err := PrintExecution(&out, i, e); err != nil {
				t.Fatal(err)
			}
		}
		head := out.Len()
		if err := PrintMedian(&out, base); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		last := -1
		for i := range ExecutionsPerRun {
			at := strings.Index(text, fmt.Sprintf("STORE-RECORD execution=%d\n", i))
			if at <= last {
				t.Fatalf("execution %d marker at %d, after %d", i, at, last)
			}
			last = at
		}
		tail := text[head:]
		if !strings.HasPrefix(tail, "STORE-RECORD kind=baseline ") || strings.Count(tail, " metric=") != 18 || strings.Contains(tail, " window=") {
			t.Fatalf("median block:\n%s", tail)
		}
		for _, e := range base.Executions {
			for _, w := range e.Workloads {
				if strings.Contains(text, base64.StdEncoding.EncodeToString(w.Raw.Data)[:32]) {
					t.Fatal("the output holds raw data")
				}
			}
		}
	})

	t.Run("full-volume raw retention stays inside the transport bound", func(t *testing.T) {
		// Twice the p99 of an earlier single-run measurement: writer-commit,
		// indexed-read and writer-wait, per workload, in nanoseconds.
		p99 := [2][3]int64{{283403, 670570, 7744781}, {319857, 1086941, 7707615}}
		reads := [2]int{200000, 120000}
		arrays := make([][][]RawWindow, ExecutionsPerRun)
		var values int64
		for i := range arrays {
			arrays[i] = make([][]RawWindow, 2)
			for wi := range 2 {
				for k := range 5 {
					r := rand.New(rand.NewPCG(20261019, uint64(100*i+10*wi+k)))
					fill := func(n int, p99 int64) []int64 {
						a := make([]int64, n)
						for x := range a {
							a[x] = 1 + r.Int64N(2*p99)
						}
						slices.Sort(a)
						values += int64(n)
						return a
					}
					arrays[i][wi] = append(arrays[i][wi], RawWindow{Commit: fill(4000, p99[wi][0]), Read: fill(reads[wi], p99[wi][1]), Wait: fill(4000, p99[wi][2])})
				}
			}
		}
		run := Median(KindBaseline, executionsOf(arrays))
		path := filepath.Join(t.TempDir(), "baseline.json")
		var out bytes.Buffer
		if err := Record(&out, path, run); err != nil {
			t.Fatalf("record: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("the median file holds %d raw values in %d bytes (limit %d, headroom %d)", values, fi.Size(), FileLimit, FileLimit-fi.Size())
		got, reason := loadRun(path, FileLimit)
		if reason != "" || !reflect.DeepEqual(got, run) {
			t.Fatalf("the file does not read back equal: %q", reason)
		}
		if rec := Recorded(got); rec != "" {
			t.Fatalf("recorded: %q", rec)
		}
		if err := matchArrays(got, arrays); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		padded := append(bytes.Clone(data), bytes.Repeat([]byte(" "), FileLimit-len(data))...)
		if r, err := DecodeRun(padded); err != nil || !reflect.DeepEqual(r, run) {
			t.Fatalf("input of exactly the cap: %v", err)
		}
		if _, err := DecodeRun(append(padded, ' ')); !errors.Is(err, errOverLimit) {
			t.Fatalf("input one byte above the cap: %v", err)
		}

		// Incompressible raw data above the cap is refused with no file, by both writers.
		noise := make([]byte, 25<<20)
		r := rand.New(rand.NewPCG(1, 2))
		for i := 0; i < len(noise); i += 8 {
			v := r.Uint64()
			for k := 0; k < 8 && i+k < len(noise); k++ {
				noise[i+k] = byte(v >> (8 * k))
			}
		}
		for _, c := range []struct {
			name string
			do   func(path string) error
		}{
			{"record", func(p string) error {
				return Record(io.Discard, p, withExec(base, 0, func(e *Run) { e.Workloads[0].Raw.Data = noise }))
			}},
			{"capture", func(p string) error {
				return Capture(io.Discard, p, withExec(later, 0, func(e *Run) { e.Workloads[0].Raw.Data = noise }))
			}},
		} {
			p := filepath.Join(t.TempDir(), c.name+".json")
			if err := c.do(p); !errors.Is(err, errOverLimit) {
				t.Errorf("%s of oversize raw data: %v", c.name, err)
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s of oversize raw data wrote a file (%v)", c.name, err)
			}
		}
	})

	t.Run("a compare capture keeps the measured run and the judge measures nothing", func(t *testing.T) {
		dir := t.TempDir()
		basePath := filepath.Join(dir, "baseline.json")
		if err := Record(io.Discard, basePath, base); err != nil {
			t.Fatal(err)
		}
		arraysL := testArraysSet(shifted(11))
		L := Median(KindCompare, executionsOf(arraysL))
		limit := *base.Workloads[1].metric(8) * 110 / 100
		red := Median(KindCompare, executionsOf(testArraysSet(func(wi, j int) int64 {
			if wi == 1 && j == 8 {
				return limit + 1 - *base.Workloads[1].metric(8)
			}
			return 0
		})))
		invalid := withExec(L, 2, func(e *Run) { e.Facts.SwapIn = 1 })

		capture := func(t *testing.T, name string, r Run) (string, []byte) {
			t.Helper()
			path := filepath.Join(dir, name+".json")
			var out bytes.Buffer
			if err := Capture(&out, path, r); err != nil {
				t.Fatalf("capture %s: %v", name, err)
			}
			if out.String() != CaptureLine+"\n" {
				t.Fatalf("capture %s output %q", name, out.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return path, data
		}
		lPath, lData := capture(t, "later", L)
		got, err := DecodeRun(lData)
		if err != nil || !reflect.DeepEqual(got, L) {
			t.Fatalf("the captured file does not read back as the measured run: %v", err)
		}
		if err := matchArrays(got, arraysL); err != nil {
			t.Fatalf("captured run: %v", err)
		}
		relabeled := Median(KindCompare, base.Executions)
		if reflect.DeepEqual(got, relabeled) || matchArrays(relabeled, arraysL) == nil {
			t.Fatal("a relabeled baseline passes as the captured later run")
		}

		for name, bad := range map[string]Run{"baseline": base, "execution": L.Executions[0]} {
			p := filepath.Join(dir, "refused-"+name+".json")
			if err := Capture(io.Discard, p, bad); err == nil {
				t.Errorf("a %s run was captured", name)
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("a refused %s capture left a file (%v)", name, err)
			}
		}
		if err := Capture(io.Discard, lPath, red); err == nil {
			t.Error("a capture replaced an existing file")
		}
		if after, _ := os.ReadFile(lPath); !bytes.Equal(after, lData) {
			t.Fatal("a refused capture changed the existing file")
		}

		judge := func(t *testing.T, path string) (Result, string) {
			t.Helper()
			before, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var out bytes.Buffer
			res, err := JudgeFile(&out, basePath, path)
			if err != nil {
				t.Fatal(err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatalf("the judge changed %s", path)
			}
			return res, out.String()
		}
		res, text := judge(t, lPath)
		if res.Verdict != Pass || !strings.HasSuffix(text, "STORE-FOREGROUND-PASS\n") || strings.Count(text, " ok=true\n") != 18 {
			t.Fatalf("a later run inside the predicate: %+v\n%s", res, text)
		}

		redPath, _ := capture(t, "red", red)
		res, text = judge(t, redPath)
		wantVerdict(t, res, Fail, "regression")
		if !strings.HasSuffix(text, "STORE-FOREGROUND-FAIL reason=regression\n") || strings.Count(text, " ok=") != 18 || strings.Count(text, " ok=false\n") != 1 ||
			!strings.Contains(text, "workload=skew metric=writer_wait_p99_ns") {
			t.Fatalf("a budget-red capture:\n%s", text)
		}

		invalidPath, _ := capture(t, "invalid", invalid)
		res, text = judge(t, invalidPath)
		wantVerdict(t, res, Invalid, "later_execution_swap_activity")
		if text != "STORE-FOREGROUND-INVALID reason=later_execution_swap_activity\n" {
			t.Fatalf("an invalid capture:\n%s", text)
		}

		res, text = judge(t, filepath.Join(dir, "absent.json"))
		wantVerdict(t, res, Invalid, "later_missing")
		if text != "STORE-FOREGROUND-INVALID reason=later_missing\n" {
			t.Fatalf("absent: %q", text)
		}
		wantVerdict(t, func() Result { r, _ := judge(t, basePath); return r }(), Invalid, "later_kind")

		start := strings.Index(string(lData), `"Data": "`) + len(`"Data": "`)
		redacted := filepath.Join(dir, "redacted.json")
		if err := os.WriteFile(redacted, []byte(string(lData[:start+20])+"[REDACTED]"+string(lData[start+30:])), 0o644); err != nil {
			t.Fatal(err)
		}
		res, _ = judge(t, redacted)
		wantVerdict(t, res, Invalid, "later_undecodable")

		corrupt, _ := capture(t, "corrupt", withExec(L, 0, func(e *Run) { e.Workloads[0].Raw.Data = []byte("valid base64 once encoded, not gzip") }))
		res, _ = judge(t, corrupt)
		wantVerdict(t, res, Invalid, "later_execution0_churn_raw_undecodable")

		// A failed write is an error beside the very result a working writer
		// gives, whichever line fails, and the captured file is not touched.
		for name, path := range map[string]string{"pass": lPath, "fail": redPath, "invalid": invalidPath} {
			want, full := judge(t, path)
			counted := &failWriter{out: io.Discard}
			if _, err := JudgeFile(counted, basePath, path); err != nil {
				t.Fatalf("%s judge with a working writer: %v", name, err)
			}
			for k := 1; k <= counted.writes; k++ {
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				res, err := JudgeFile(&failWriter{out: &out, failAt: k}, basePath, path)
				if !errors.Is(err, errOutput) || !reflect.DeepEqual(res, want) {
					t.Fatalf("%s judge with write %d failing: %+v %v, want %+v and the output failure", name, k, res, err, want)
				}
				if !strings.HasPrefix(full, out.String()) {
					t.Fatalf("%s judge with write %d failing wrote %q, not a prefix of %q", name, k, out.String(), full)
				}
				if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
					t.Fatalf("%s judge with write %d failing changed %s", name, k, path)
				}
			}
		}

		// The two early INVALID returns keep their verdict, reason and I/O
		// cause together. A rejected baseline measures nothing and leaves no
		// file; a rejected later file (the baseline itself, which is not a
		// compare run) is left byte for byte.
		measured := false
		spy := func() Run { measured = true; return L }
		absent := filepath.Join(dir, "no-baseline.json")
		var rejected bytes.Buffer
		res, err = CompareFile(&failWriter{out: &rejected, failAt: 1}, absent, spy)
		wantVerdict(t, res, Invalid, "base_missing")
		if !errors.Is(err, errOutput) || measured || rejected.Len() != 0 {
			t.Fatalf("a rejected baseline with a failing output: %v, measured %t, wrote %q", err, measured, rejected.String())
		}
		if _, statErr := os.Stat(absent); !os.IsNotExist(statErr) {
			t.Fatalf("a rejected baseline with a failing output left a file (%v)", statErr)
		}
		baseBytes, err := os.ReadFile(basePath)
		if err != nil {
			t.Fatal(err)
		}
		rejected.Reset()
		res, err = JudgeFile(&failWriter{out: &rejected, failAt: 1}, basePath, basePath)
		wantVerdict(t, res, Invalid, "later_kind")
		if !errors.Is(err, errOutput) || rejected.Len() != 0 {
			t.Fatalf("a rejected later file with a failing output: %v, wrote %q", err, rejected.String())
		}
		if after, _ := os.ReadFile(basePath); !bytes.Equal(baseBytes, after) {
			t.Fatal("a rejected later file with a failing output changed the file")
		}
	})

	t.Run("finite byte bounds", func(t *testing.T) {
		if FileLimit != 33554432 || RawLimit != 134217728 {
			t.Fatalf("FileLimit %d, RawLimit %d", FileLimit, RawLimit)
		}
		const lim = 21*30 + 64*3
		for w := int64(-1); w <= lim/64+2; w++ {
			for v := int64(-1); v <= lim/21+2; v++ {
				if want := v >= 0 && w >= 0 && 21*v+64*w <= lim; rawFits(v, w, lim) != want {
					t.Fatalf("rawFits(%d, %d, %d) = %t", v, w, lim, !want)
				}
			}
		}
		for _, c := range []struct{ v, w int64 }{{lim/21 + 1, 0}, {0, lim/64 + 1}, {math.MaxInt64, 0}, {0, math.MaxInt64}, {math.MaxInt64, math.MaxInt64}} {
			if rawFits(c.v, c.w, lim) {
				t.Errorf("rawFits(%d, %d, %d) accepted an out-of-range count", c.v, c.w, lim)
			}
		}
		if !rawFits(30, 3, lim) || rawFits(31, 3, lim) {
			t.Fatal("the bound is not exactly 21 bytes per value and 64 per window")
		}

		seq := func(n int, from int64) []int64 {
			a := make([]int64, n)
			for i := range a {
				a[i] = from + int64(i)
			}
			return a
		}
		ws := []RawWindow{{Commit: seq(10, 1)}, {Read: seq(10, 1)}, {Wait: seq(10, 1)}}
		if raw, err := encodeRaw(ws, lim, FileLimit); err != nil || raw.Codec != RawCodec || len(raw.Data) == 0 {
			t.Fatalf("encode at the bound: %+v %v", raw, err)
		}
		ws[2].Wait = seq(11, 1)
		if raw, err := encodeRaw(ws, lim, FileLimit); !errors.Is(err, errRawOverflow) || raw.Data != nil {
			t.Fatalf("encode one value above the bound: %+v %v", raw, err)
		}
		if raw, err := encodeRaw(ws[:1], FileLimit, 10); !errors.Is(err, errOverLimit) || raw.Data != nil {
			t.Fatalf("encode into a sink of 10 bytes: %+v %v", raw, err)
		}

		// A workload of 3 windows of 4 probes and 5 reads declares 3*(4+5+4) values.
		var arrays []RawWindow
		var win []Window
		for k := range 3 {
			a := RawWindow{Commit: seq(4, int64(10*k+1)), Read: seq(5, int64(10*k+1)), Wait: seq(4, int64(10*k+1))}
			arrays = append(arrays, a)
			win = append(win, Window{Probes: 4, Reads: 5, Commit: seriesOf(a.Commit), Read: seriesOf(a.Read), Wait: seriesOf(a.Wait)})
		}
		raw, err := encodeRaw(arrays, RawLimit, FileLimit)
		if err != nil {
			t.Fatal(err)
		}
		w := Workload{Name: "tiny", Windows: win, Raw: raw,
			Commit: pooled([][]int64{arrays[0].Commit, arrays[1].Commit, arrays[2].Commit}),
			Read:   pooled([][]int64{arrays[0].Read, arrays[1].Read, arrays[2].Read}),
			Wait:   pooled([][]int64{arrays[0].Wait, arrays[1].Wait, arrays[2].Wait})}
		const declared = 3 * (4 + 5 + 4)
		bound := int64(21*declared + 64*3)
		if got := rawToken(w, bound); got != "" {
			t.Fatalf("declared counts at the bound: %q", got)
		}
		if got := rawToken(w, bound-1); got != "raw_undecodable" {
			t.Fatalf("declared counts one above the bound: %q", got)
		}
		bad := w
		bad.Windows = slices.Clone(win)
		bad.Windows[0].Probes = bound/21 + 1
		if got := rawToken(bad, bound); got != "raw_undecodable" {
			t.Fatalf("a count above limit/21: %q", got)
		}
		text := textOf(arrays)
		padded := func(n int) Workload {
			x := w
			x.Raw = gzRaw(text + strings.Repeat(" ", n-len(text)))
			return x
		}
		if got := rawToken(padded(int(bound)), RawLimit); got != "" {
			t.Fatalf("text at its counts' allowance: %q", got)
		}
		if got := rawToken(padded(int(bound)+1), RawLimit); got != "raw_undecodable" {
			t.Fatalf("text one byte above its counts' allowance: %q", got)
		}

		dir := t.TempDir()
		r := testRun(KindBaseline)
		encoded, err := marshalRun(r, FileLimit)
		if err != nil {
			t.Fatal(err)
		}
		n := int64(len(encoded))
		at := filepath.Join(dir, "at.json")
		if err := writeRun(at, r, n); err != nil {
			t.Fatalf("write of exactly the limit: %v", err)
		}
		if got, reason := loadRun(at, n); reason != "" || !reflect.DeepEqual(got, r) {
			t.Fatalf("read of exactly the limit: %q", reason)
		}
		if _, reason := loadRun(at, n-1); reason != "undecodable" {
			t.Fatalf("read of a file one byte above the limit: %q", reason)
		}
		below := filepath.Join(dir, "below.json")
		if err := writeRun(below, r, n-1); !errors.Is(err, errOverLimit) {
			t.Fatalf("write one byte above the limit: %v", err)
		}
		if _, err := os.Stat(below); !os.IsNotExist(err) {
			t.Fatalf("a refused write left a file (%v)", err)
		}
		var b64 int64
		for _, e := range r.Executions {
			for _, w := range e.Workloads {
				b64 += int64(base64.StdEncoding.EncodedLen(len(w.Raw.Data)))
			}
		}
		if _, err := marshalRun(r, b64-1); !errors.Is(err, errOverLimit) {
			t.Fatalf("raw data of %d bytes in base64 alone passes a limit of %d: %v", b64, b64-1, err)
		}
	})
}

func TestPercentile(t *testing.T) {
	s := make([]int64, 100)
	for i := range s {
		s[i] = int64(i + 1)
	}
	if got := Summarize(s); got != (Series{50, 95, 99}) {
		t.Fatalf("1..100: %+v", got)
	}
	if got := Summarize([]int64{9, 3, 7}); got != (Series{7, 9, 9}) {
		t.Fatalf("3 samples: %+v", got)
	}
	if got := Summarize([]int64{5}); got != (Series{5, 5, 5}) {
		t.Fatalf("1 sample: %+v", got)
	}
	if got := Summarize(nil); got != (Series{}) {
		t.Fatalf("no sample: %+v", got)
	}
}

func TestDecodeRun(t *testing.T) {
	base := testRun(KindBaseline)
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRun(data)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("round trip changed the run:\n got %+v\nwant %+v", got, base)
	}
	if Compare(base, got).Verdict != Pass || got.Kind != KindBaseline {
		t.Fatalf("round trip verdict: %+v", got)
	}
	for name, bad := range map[string]string{
		"unknown field": strings.Replace(string(data), `"Kind"`, `"Extra":1,"Kind"`, 1),
		"trailing data": string(data) + " {}",
		"not an object": "[]",
	} {
		if _, err := DecodeRun([]byte(bad)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A recorded baseline, once it exists, is a baseline-eligible median of five
// executions with every record and every raw sample, and is not labeled a pass.
func TestRecordedBaseline(t *testing.T) {
	if _, err := os.Stat("baseline.json"); os.IsNotExist(err) {
		t.Log("no baseline recorded yet")
		return
	}
	r, reason := loadRun("baseline.json", FileLimit)
	if reason != "" {
		t.Fatalf("baseline.json: %s", reason)
	}
	if r.Kind != KindBaseline {
		t.Fatalf("baseline.json is kind %q", r.Kind)
	}
	if reason := Baseline(r); reason != "" {
		t.Fatalf("baseline.json is not an eligible baseline: %s", reason)
	}
	if reason := Recorded(r); reason != "" {
		t.Fatalf("baseline.json misses a recorded field: %s", reason)
	}
	for i, e := range append([]Run{r}, r.Executions...) {
		for _, w := range e.Workloads {
			if !strings.Contains(w.Shape, AggregateShape) {
				t.Fatalf("baseline.json run %d workload %s was not built by the full-sample aggregate: %q", i, w.Name, w.Shape)
			}
			if i > 0 && len(w.Windows) == 0 {
				t.Fatalf("baseline.json execution %d workload %s has no window spread", i-1, w.Name)
			}
		}
	}
}
