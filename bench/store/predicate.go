// Package storebench holds the foreground series of the store: this file is
// the pure regression predicate and the shape of one recorded run; the
// measurement is foreground_test.go. A run is five consecutive executions and
// every compared number is its own median of the five. The first eligible run
// is the baseline and never a pass; a later run passes only inside the
// predicate with synchronous FULL.
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
	"os"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The two workloads of the series: AC-013 connection churn and AC-014 skewed
// long-series mutations.
const (
	WorkloadChurn = "churn"
	WorkloadSkew  = "skew"
)

// The kinds of a run: the recorded first run, the measured later run, and the
// one execution a run holds five of. None of them is a pass.
const (
	KindBaseline  = "baseline"
	KindCompare   = "compare"
	KindExecution = "execution"
)

// ExecutionsPerRun is how many consecutive same-shape executions make a run.
const ExecutionsPerRun = 5

// BaselineLine is the only line a record run prints as its outcome.
const BaselineLine = "STORE-FOREGROUND-BASELINE"

// CaptureLine is the only line a compare run prints as its outcome: the
// measured run was kept, and nothing about it passed.
const CaptureLine = "STORE-FOREGROUND-CAPTURE"

// synchronousFull is the synchronous read-back of FULL (ADR 0003).
const synchronousFull = 2

// A later metric passes when later*100 <= base*110, compared as integers.
const (
	regressNum = 110
	regressDen = 100
)

// The finite byte bounds. FileLimit is the runner's samples cap on one run
// file. RawLimit caps the raw JSON text of one execution workload; its text
// is at most rawValueBytes per value and rawWindowBytes per window.
const (
	FileLimit      = 32 << 20
	RawLimit       = 128 << 20
	RawCodec       = "json+gzip"
	rawValueBytes  = 21
	rawWindowBytes = 64
)

var (
	errOverLimit   = errors.New("storebench: over the byte limit")
	errRawOverflow = errors.New("raw_overflow")
)

// Series is one latency series in nanoseconds, nearest-rank percentiles.
type Series struct{ P50, P95, P99 int64 }

// Percentile is the nearest-rank p-th percentile (1..100) of sorted samples,
// 0 for none.
func Percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max((len(sorted)*p+99)/100, 1)-1]
}

// Summarize sorts samples in place and returns their series.
func Summarize(samples []int64) Series {
	slices.Sort(samples)
	return Series{Percentile(samples, 50), Percentile(samples, 95), Percentile(samples, 99)}
}

// AggregateShape is part of every workload's shape: within an execution the
// series are nearest-rank percentiles over every measured sample of all
// windows taken together, so a baseline built by another estimator is a
// different workload.
const AggregateShape = "aggregate=full-sample-nearest-rank"

// Pool is the series over every sample of every window taken together; no
// window is selected and no percentile is a minimum across windows.
func Pool(windows [][]int64) Series {
	var all []int64
	for _, w := range windows {
		all = append(all, w...)
	}
	return Summarize(all)
}

// Host is the identity of the machine and toolchain; a later run is only
// comparable with a baseline from an equal Host. It holds no path, name or
// cpu list.
type Host struct {
	OS         string
	Arch       string
	GoVersion  string
	CPUs       int
	GOMAXPROCS int
}

// Facts are the eligibility observations of one run.
type Facts struct {
	Race    bool  // a race build
	SwapIn  int64 // pages swapped in across the run (/proc/vmstat pswpin)
	SwapOut int64 // pages swapped out across the run (/proc/vmstat pswpout)
	SwapKiB int64 // the largest VmSwap seen (/proc/self/status)
}

// Settings are the effective settings the pools read back.
type Settings struct {
	Writer, Reader store.ConnPolicy
	ReaderCount    int
	WriterMaxOpen  int
	ReaderMaxOpen  int
}

// Connects counts the physical connections each pool opened across the
// workload: the churn evidence, not a setting.
type Connects struct{ Writer, Reader int64 }

// Allocs are the process allocations across the measured window.
type Allocs struct{ Mallocs, Bytes, Ops int64 }

// WriteAmp is bytes written by the process against the logical bytes the
// mutations carried; Milli is Written*1000/Logical.
type WriteAmp struct{ WrittenBytes, LogicalBytes, Milli int64 }

// WAL is the journal state after the window and what a passive checkpoint
// reports.
type WAL struct {
	FileBytes, DBBytes, PageSize            int64
	CheckpointBusy, LogFrames, Checkpointed int64
}

// Memory is the total memory after the window.
type Memory struct{ VmHWMKiB, VmRSSKiB, GoSysBytes, GoHeapInuseBytes int64 }

// Window is one predeclared window's sample counts and series. Within an
// execution the series of a workload pool every window's samples; the windows
// stay beside them so the spread is visible.
type Window struct {
	Probes, Reads      int64
	Commit, Read, Wait Series
}

// RawWindow is the nanosecond samples of one window, ascending as Summarize
// leaves them: the writer-commit and writer-wait probes and the indexed
// reads. Their counts are the window's Probes, Reads and Probes.
type RawWindow struct{ Commit, Read, Wait []int64 }

// Raw is one execution workload's retained samples: the RawWindow of every
// window as consecutive JSON values, gzip compressed. Data is base64 in a file.
type Raw struct {
	Codec string
	Data  []byte
}

// Workload is one workload's series and everything recorded beside it. The
// workloads of a median run carry only the name, shape, settings and the
// compared series; everything else stays in the executions.
type Workload struct {
	Name     string
	Shape    string // the workload definition; a later run must repeat it
	Commit   Series // writer-commit
	Read     Series // indexed-read
	Wait     Series // writer-wait
	Settings Settings
	Connects Connects
	Allocs   Allocs
	WriteAmp WriteAmp
	WAL      WAL
	Memory   Memory
	Plans    map[string][]string
	Windows  []Window // each predeclared window's counts and series: spread, not compared
	Raw      Raw      // the samples that produced Windows and the series
}

// Run is one series run: a median over its Executions, which are complete
// runs of kind execution in measured order.
type Run struct {
	Kind       string
	Host       Host
	Facts      Facts
	Workloads  []Workload
	Executions []Run
}

// Metric is one compared number, in nanoseconds.
type Metric struct {
	Name  string
	Value int64
}

// metric points at the stored value Metrics()[j] reports.
func (w *Workload) metric(j int) *int64 {
	s := []*Series{&w.Commit, &w.Read, &w.Wait}[j/3]
	return []*int64{&s.P50, &s.P95, &s.P99}[j%3]
}

// Metrics lists the compared numbers of a workload in a fixed order:
// writer-commit, indexed-read and writer-wait at p50, p95 and p99.
func (w Workload) Metrics() []Metric {
	out := make([]Metric, 0, 9)
	for j := range 9 {
		name := [...]string{"writer_commit", "indexed_read", "writer_wait"}[j/3]
		out = append(out, Metric{name + [...]string{"_p50_ns", "_p95_ns", "_p99_ns"}[j%3], *w.metric(j)})
	}
	return out
}

func (w Workload) fullSync() bool {
	return w.Settings.Writer.Synchronous == synchronousFull && w.Settings.Reader.Synchronous == synchronousFull
}

// Median builds the run of the given kind from its executions: each compared
// value is the middle one of the executions' values at the same workload and
// metric, never samples pooled across executions, a minimum, a mean or one
// chosen execution. Host, names, shapes and settings come from the first
// execution; Consistent holds the others to them. Facts are the race OR, the
// swap sums and the largest VmSwap.
func Median(kind string, executions []Run) Run {
	out := Run{Kind: kind, Executions: executions}
	if len(executions) == 0 {
		return out
	}
	out.Host = executions[0].Host
	n := len(executions[0].Workloads)
	for _, e := range executions {
		out.Facts.Race = out.Facts.Race || e.Facts.Race
		out.Facts.SwapIn += e.Facts.SwapIn
		out.Facts.SwapOut += e.Facts.SwapOut
		out.Facts.SwapKiB = max(out.Facts.SwapKiB, e.Facts.SwapKiB)
		n = min(n, len(e.Workloads))
	}
	for i := range n {
		w := executions[0].Workloads[i]
		med := Workload{Name: w.Name, Shape: w.Shape, Settings: w.Settings}
		for j := range 9 {
			v := make([]int64, len(executions))
			for k, e := range executions {
				v[k] = *e.Workloads[i].metric(j)
			}
			slices.Sort(v)
			*med.metric(j) = v[len(v)/2]
		}
		out.Workloads = append(out.Workloads, med)
	}
	return out
}

// Verdict is the outcome of a comparison.
type Verdict string

const (
	Pass    Verdict = "PASS"
	Fail    Verdict = "FAIL"
	Invalid Verdict = "INVALID"
)

// Check is one compared metric. Limit is the largest passing later value.
type Check struct {
	Workload, Metric   string
	Base, Later, Limit int64
	OK                 bool
}

// Result is a verdict with its fixed reason token and, once the two runs are
// comparable, every check.
type Result struct {
	Verdict Verdict
	Reason  string
	Checks  []Check
}

// Line is the outcome line. Only a PASS result prints STORE-FOREGROUND-PASS.
func (r Result) Line() string {
	if r.Verdict == Pass {
		return "STORE-FOREGROUND-PASS"
	}
	return fmt.Sprintf("STORE-FOREGROUND-%s reason=%s", r.Verdict, r.Reason)
}

// within reports later*100 <= base*110 on integers.
func within(base, later int64) bool { return later*regressDen <= base*regressNum }

// eligibleRun is the eligibility of one measured run: Linux, no race build
// and no swap. An execution is judged by this alone.
func eligibleRun(r Run) string {
	switch {
	case r.Host.OS != "linux":
		return "non_linux"
	case r.Facts.Race:
		return "race_build"
	case r.Facts.SwapIn > 0 || r.Facts.SwapOut > 0 || r.Facts.SwapKiB > 0:
		return "swap_activity"
	}
	return ""
}

// Consistent returns "" when r is a median run whose five executions are all
// there, measured alike and eligible, and whose own numbers are the median of
// theirs, else the first token in this order: executions_incomplete, execution_kind,
// execution_ plus an execution's eligibility token, execution_mismatch,
// median_mismatch. Nothing is dropped: one bad execution makes the run bad.
func Consistent(r Run) string {
	if len(r.Executions) != ExecutionsPerRun {
		return "executions_incomplete"
	}
	for _, e := range r.Executions {
		if e.Kind != KindExecution || len(e.Executions) != 0 {
			return "execution_kind"
		}
	}
	for _, e := range r.Executions {
		if t := eligibleRun(e); t != "" {
			return "execution_" + t
		}
	}
	for _, e := range r.Executions {
		if !sameMeasure(e, r) {
			return "execution_mismatch"
		}
	}
	if !reflect.DeepEqual(r, Median(r.Kind, r.Executions)) {
		return "median_mismatch"
	}
	return ""
}

// sameMeasure reports whether e has r's host and, workload by workload, r's
// name, shape and full settings.
func sameMeasure(e, r Run) bool {
	if e.Host != r.Host || len(e.Workloads) != len(r.Workloads) {
		return false
	}
	for i, w := range r.Workloads {
		x := e.Workloads[i]
		if x.Name != w.Name || x.Shape != w.Shape || x.Settings != w.Settings {
			return false
		}
	}
	return true
}

// Eligible returns "" when the run may serve as a baseline or a later run,
// else the reason token it is INVALID.
func Eligible(r Run) string {
	if t := eligibleRun(r); t != "" {
		return t
	}
	return Consistent(r)
}

// Baseline returns "" when r may be recorded as the baseline: eligible, both
// workloads present, every metric positive and synchronous FULL.
func Baseline(r Run) string {
	if t := Eligible(r); t != "" {
		return t
	}
	if r.Kind != KindBaseline {
		return "not_baseline"
	}
	if len(r.Workloads) != 2 || r.Workloads[0].Name != WorkloadChurn || r.Workloads[1].Name != WorkloadSkew {
		return "workload_mismatch"
	}
	for _, w := range r.Workloads {
		for _, m := range w.Metrics() {
			if m.Value <= 0 {
				return "zero_metric"
			}
		}
	}
	for _, w := range r.Workloads {
		if !w.fullSync() {
			return "synchronous_not_full"
		}
	}
	return ""
}

// masked is the settings without synchronous, which a comparison judges apart.
func (s Settings) masked() Settings {
	s.Writer.Synchronous, s.Reader.Synchronous = 0, 0
	return s
}

// Compare judges a later run against the baseline. INVALID (the evidence
// cannot support a comparison) comes first, then FAIL (synchronous other than
// FULL, even when faster, or a metric above base*110/100), else PASS.
func Compare(base, later Run) Result {
	if t := Eligible(later); t != "" {
		return Result{Verdict: Invalid, Reason: "later_" + t}
	}
	if t := Baseline(base); t != "" {
		return Result{Verdict: Invalid, Reason: "base_" + t}
	}
	for _, w := range later.Workloads {
		for _, m := range w.Metrics() {
			if m.Value <= 0 { // an empty or zero series is not an improvement
				return Result{Verdict: Invalid, Reason: "later_zero_metric"}
			}
		}
	}
	if base.Host != later.Host {
		return Result{Verdict: Invalid, Reason: "host_mismatch"}
	}
	if len(later.Workloads) != len(base.Workloads) {
		return Result{Verdict: Invalid, Reason: "workload_mismatch"}
	}
	for i, b := range base.Workloads {
		l := later.Workloads[i]
		if l.Name != b.Name || l.Shape != b.Shape {
			return Result{Verdict: Invalid, Reason: "workload_mismatch"}
		}
		if l.Settings.masked() != b.Settings.masked() {
			return Result{Verdict: Invalid, Reason: "settings_mismatch"}
		}
	}
	res := Result{Verdict: Pass}
	sync := true
	for i, b := range base.Workloads {
		l := later.Workloads[i]
		sync = sync && l.fullSync()
		bm, lm := b.Metrics(), l.Metrics()
		for j := range bm {
			ok := within(bm[j].Value, lm[j].Value)
			res.Checks = append(res.Checks, Check{b.Name, bm[j].Name, bm[j].Value, lm[j].Value, bm[j].Value * regressNum / regressDen, ok})
			if !ok && res.Verdict == Pass {
				res.Verdict, res.Reason = Fail, "regression"
			}
		}
	}
	if !sync {
		res.Verdict, res.Reason = Fail, "synchronous_not_full"
	}
	return res
}

// capped reads at most left bytes of r and fails with errOverLimit on the
// first byte above them and on every read after it, so a reader that buffers
// ahead of its caller cannot lose the failure.
type capped struct {
	r    io.Reader
	left int64
	over bool
}

func (c *capped) Read(p []byte) (int, error) {
	if c.over {
		return 0, errOverLimit
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		n, c.left, c.over = int(c.left), 0, true
		return n, errOverLimit
	}
	c.left -= int64(n)
	return n, err
}

// rawMax is the most values a workload of the given windows may hold under
// limit, so that 21 bytes per value and 64 per window stay inside it, or -1
// when the windows alone do not fit. Nothing is multiplied before the window
// count is checked against its own bound.
func rawMax(windows, limit int64) int64 {
	if windows < 0 || windows > limit/rawWindowBytes {
		return -1
	}
	return (limit - rawWindowBytes*windows) / rawValueBytes
}

// rawFits reports whether values in windows fit under limit: a negative count
// and a count above limit/21 never do.
func rawFits(values, windows, limit int64) bool {
	return values >= 0 && values <= rawMax(windows, limit)
}

// encodeRaw encodes the windows as one JSON value each into a gzip writer at
// BestCompression, refusing before it starts when they do not fit rawLimit
// and while writing when the compressed bytes pass fileLimit.
func encodeRaw(windows []RawWindow, rawLimit, fileLimit int64) (Raw, error) {
	var values int64
	for _, w := range windows {
		values += int64(len(w.Commit) + len(w.Read) + len(w.Wait))
	}
	if !rawFits(values, int64(len(windows)), rawLimit) {
		return Raw{}, fmt.Errorf("storebench: %w: %d values in %d windows above %d bytes", errRawOverflow, values, len(windows), rawLimit)
	}
	var data bytes.Buffer
	zw, err := gzip.NewWriterLevel(&capWriter{w: &data, left: fileLimit}, gzip.BestCompression)
	if err != nil {
		return Raw{}, err
	}
	enc := json.NewEncoder(zw)
	for _, w := range windows {
		if err := enc.Encode(w); err != nil {
			return Raw{}, err
		}
	}
	if err := zw.Close(); err != nil {
		return Raw{}, err
	}
	return Raw{Codec: RawCodec, Data: data.Bytes()}, nil
}

// capWriter writes at most left bytes and fails with errOverLimit on more.
type capWriter struct {
	w    io.Writer
	left int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errOverLimit
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}

// declaredValues is the number of raw values the windows declare (Probes,
// Reads and Probes each), with ok false when a count is negative, the total
// or the window count passes limit, all before the total can overflow.
func declaredValues(windows []Window, limit int64) (n int64, ok bool) {
	for _, w := range windows {
		for _, c := range [...]int64{w.Probes, w.Reads, w.Probes} {
			if c < 0 || c > limit/rawValueBytes {
				return 0, false
			}
			if n += c; n > limit/rawValueBytes {
				return 0, false
			}
		}
	}
	return n, rawFits(n, int64(len(windows)), limit)
}

// decodeRaw strictly decodes exactly windows RawWindows from data through a
// gzip reader that fails on more text than the declared values allow.
func decodeRaw(data []byte, windows int, values int64) ([]RawWindow, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(&capped{r: zr, left: rawValueBytes*values + rawWindowBytes*int64(windows)})
	dec.DisallowUnknownFields()
	var out []RawWindow
	for range windows {
		var w RawWindow
		if err := dec.Decode(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("storebench: data after the raw windows")
	}
	return out, nil
}

// rawMatches reports whether the decoded windows have the declared counts and
// reproduce every stored window series and the pooled series of w.
func rawMatches(w Workload, raw []RawWindow) bool {
	if len(raw) != len(w.Windows) {
		return false
	}
	var commits, reads, waits [][]int64
	for i, win := range w.Windows {
		r := raw[i]
		if int64(len(r.Commit)) != win.Probes || int64(len(r.Read)) != win.Reads || int64(len(r.Wait)) != win.Probes {
			return false
		}
		if Summarize(r.Commit) != win.Commit || Summarize(r.Read) != win.Read || Summarize(r.Wait) != win.Wait {
			return false
		}
		commits, reads, waits = append(commits, r.Commit), append(reads, r.Read), append(waits, r.Wait)
	}
	return Pool(commits) == w.Commit && Pool(reads) == w.Read && Pool(waits) == w.Wait
}

// rawToken returns "" when the workload's raw samples are present, within
// limit by their declared counts, strictly decodable and equal to its windows
// and series, else raw_missing, raw_undecodable or raw_mismatch. A declared
// count above the bound is raw_undecodable before anything is decompressed.
func rawToken(w Workload, limit int64) string {
	if w.Raw.Codec != RawCodec || len(w.Raw.Data) == 0 {
		return "raw_missing"
	}
	values, ok := declaredValues(w.Windows, limit)
	if !ok {
		return "raw_undecodable"
	}
	raw, err := decodeRaw(w.Raw.Data, len(w.Windows), values)
	switch {
	case err != nil:
		return "raw_undecodable"
	case !rawMatches(w, raw):
		return "raw_mismatch"
	}
	return ""
}

// marshalRun is the file bytes of a run: indented JSON and a newline. It
// refuses before marshaling when the base64 of all raw Data alone passes
// limit, and again when the whole encoding does.
func marshalRun(run Run, limit int64) ([]byte, error) {
	var b64 int64
	for _, r := range append([]Run{run}, run.Executions...) {
		for _, w := range r.Workloads {
			b64 += int64(base64.StdEncoding.EncodedLen(len(w.Raw.Data)))
		}
	}
	if b64 > limit {
		return nil, fmt.Errorf("storebench: %d bytes of raw data: %w", b64, errOverLimit)
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return nil, err
	}
	if data = append(data, '\n'); int64(len(data)) > limit {
		return nil, fmt.Errorf("storebench: a run file of %d bytes: %w", len(data), errOverLimit)
	}
	return data, nil
}

// writeRun writes the run to path only when path is absent (O_EXCL; the error
// matches os.ErrExist) and the file fits limit; a refused or failed write
// leaves no file.
func writeRun(path string, run Run, limit int64) error {
	data, err := marshalRun(run, limit)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("storebench: write %s: %w", path, errors.Join(err, f.Close(), os.Remove(path)))
	}
	return f.Close()
}

// decodeRun strictly decodes one recorded run of at most limit bytes: one JSON
// object, no unknown field, nothing after it.
func decodeRun(data []byte, limit int64) (Run, error) {
	if int64(len(data)) > limit {
		return Run{}, errOverLimit
	}
	var r Run
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Run{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Run{}, errors.New("storebench: data after the run")
	}
	return r, nil
}

// DecodeRun strictly decodes one recorded run of at most FileLimit bytes.
func DecodeRun(data []byte) (Run, error) { return decodeRun(data, FileLimit) }

// loadRun reads the run file at path through the byte bound. The reason is ""
// on success, "missing" when the file cannot be read and "undecodable" when it
// is above limit or not a run.
func loadRun(path string, limit int64) (Run, string) {
	f, err := os.Open(path)
	if err != nil {
		return Run{}, "missing"
	}
	data, err := io.ReadAll(&capped{r: f, left: limit})
	if err = errors.Join(err, f.Close()); errors.Is(err, errOverLimit) {
		return Run{}, "undecodable"
	} else if err != nil {
		return Run{}, "missing"
	}
	r, err := decodeRun(data, limit)
	if err != nil {
		return Run{}, "undecodable"
	}
	return r, ""
}

// Recorded returns "" when the run holds ExecutionsPerRun executions and every
// workload of each carries its query plans, allocations, write amplification,
// WAL and checkpoint progress, total memory and raw samples, else the token of
// the first missing record, prefixed execution<i>_. An empty WAL is a
// recorded 0 and a failed observation is negative; a database size or page
// size of 0 was never observed.
func Recorded(r Run) string {
	if len(r.Executions) != ExecutionsPerRun {
		return "executions_incomplete"
	}
	for i, e := range r.Executions {
		if t := recordedExecution(e); t != "" {
			return fmt.Sprintf("execution%d_%s", i, t)
		}
	}
	return ""
}

func recordedExecution(r Run) string {
	if len(r.Workloads) == 0 {
		return "no_workload"
	}
	for _, w := range r.Workloads {
		missing := len(w.Plans) == 0
		for _, lines := range w.Plans {
			missing = missing || len(lines) == 0
		}
		switch {
		case missing:
			return w.Name + "_plans_missing"
		case w.Allocs.Mallocs <= 0 || w.Allocs.Bytes <= 0 || w.Allocs.Ops <= 0:
			return w.Name + "_allocs_missing"
		case w.WriteAmp.WrittenBytes <= 0 || w.WriteAmp.LogicalBytes <= 0 || w.WriteAmp.Milli <= 0:
			return w.Name + "_write_amp_missing"
		case w.WAL.FileBytes < 0 || w.WAL.DBBytes <= 0 || w.WAL.PageSize <= 0 || w.WAL.LogFrames < 0 || w.WAL.Checkpointed < 0:
			return w.Name + "_wal_missing"
		case w.Memory.VmHWMKiB <= 0 || w.Memory.VmRSSKiB <= 0 || w.Memory.GoSysBytes <= 0 || w.Memory.GoHeapInuseBytes <= 0:
			return w.Name + "_memory_missing"
		}
		if t := rawToken(w, RawLimit); t != "" {
			return w.Name + "_" + t
		}
	}
	return ""
}

// Sample runs op and returns its duration and whether it is a sample: only an
// op that started while recording was on. The flag is read before op, so an op
// that finishes after recording turns off keeps its duration and one that
// starts after is dropped.
func Sample(recording *atomic.Bool, op func() error) (d time.Duration, counted bool, err error) {
	return sample(time.Now, recording, op)
}

// sample is Sample with its clock: the duration is the second reading of now
// minus the first, taken around op.
func sample(now func() time.Time, recording *atomic.Bool, op func() error) (d time.Duration, counted bool, err error) {
	counted = recording.Load()
	start := now()
	err = op()
	return now().Sub(start), counted, err
}

// Record prints a record run's outcome and writes the baseline to path only
// when path is absent and the run is an eligible baseline within FileLimit. It
// never prints a pass; an existing file is left byte for byte. A failed write
// of an outcome line is returned after the file decision stands, and joined to
// a refusal, never in its place.
func Record(out io.Writer, path string, run Run) error {
	if reason := Baseline(run); reason != "" {
		_, werr := fmt.Fprintf(out, "STORE-FOREGROUND-INVALID reason=%s\n", reason)
		return errors.Join(fmt.Errorf("storebench: the run is not an eligible baseline: %s", reason), werr)
	}
	err := writeRun(path, run, FileLimit)
	switch {
	case errors.Is(err, os.ErrExist):
		return printLines(out, BaselineLine, "STORE-RECORD baseline_written=false reason=baseline_exists")
	case err != nil:
		return err
	}
	return printLines(out, BaselineLine, "STORE-RECORD baseline_written=true")
}

// printLines writes each line and stops at the first failed write.
func printLines(out io.Writer, lines ...string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

// Capture writes the measured compare run to path only when path is absent and
// the encoding fits FileLimit, and prints CaptureLine. It judges nothing: a
// run that regresses or is INVALID is still the evidence of its measurement,
// and the judge reads it back. Another kind, an existing file or an oversize
// encoding is an error with no file written.
func Capture(out io.Writer, path string, run Run) error {
	if run.Kind != KindCompare {
		return fmt.Errorf("storebench: capture needs a %s run, not %q", KindCompare, run.Kind)
	}
	if err := writeRun(path, run, FileLimit); err != nil {
		return err
	}
	return printLines(out, CaptureLine)
}

// report prints every check of a result and its outcome line, and returns the
// result with the first failed write: an I/O error, never a verdict.
func report(out io.Writer, res Result) (Result, error) {
	for _, c := range res.Checks {
		if _, err := fmt.Fprintf(out, "STORE-RECORD workload=%s metric=%s base=%d later=%d limit=%d ok=%t\n", c.Workload, c.Metric, c.Base, c.Later, c.Limit, c.OK); err != nil {
			return res, err
		}
	}
	return res, printLines(out, res.Line())
}

// CompareFile reads the baseline at path, takes a later run from measure only
// once the baseline decodes, prints every check and the outcome line, and
// returns the result and the first failed write. A missing or undecodable
// baseline is INVALID and measure is not called.
func CompareFile(out io.Writer, path string, measure func() Run) (Result, error) {
	base, reason := loadRun(path, FileLimit)
	if reason != "" {
		return report(out, Result{Verdict: Invalid, Reason: "base_" + reason})
	}
	return report(out, Compare(base, measure()))
}

// JudgeFile measures nothing: it reads the captured compare run at later,
// which must be kind compare and fully recorded with its raw samples, then
// hands it to CompareFile against the baseline at base.
func JudgeFile(out io.Writer, base, later string) (Result, error) {
	run, reason := loadRun(later, FileLimit)
	switch {
	case reason != "":
	case run.Kind != KindCompare:
		reason = "kind"
	default:
		reason = Recorded(run)
	}
	if reason != "" {
		return report(out, Result{Verdict: Invalid, Reason: "later_" + reason})
	}
	return CompareFile(out, base, func() Run { return run })
}

// PrintExecution marks execution i and prints its record lines.
func PrintExecution(out io.Writer, i int, r Run) error {
	if _, err := fmt.Fprintf(out, "STORE-RECORD execution=%d\n", i); err != nil {
		return err
	}
	return PrintRun(out, r)
}

// PrintMedian prints the kind, eligibility, shape and compared metrics of a
// median run.
func PrintMedian(out io.Writer, r Run) error {
	var err error
	rec := recorder(out, &err)
	printHead(rec, r)
	for _, w := range r.Workloads {
		printSeries(rec, w)
	}
	return err
}

// PrintRun prints the record lines of a run: the compared metrics, each
// window's spread, and every field recorded beside the series. It never
// prints raw samples.
func PrintRun(out io.Writer, r Run) error {
	var err error
	rec := recorder(out, &err)
	printHead(rec, r)
	for _, w := range r.Workloads {
		printSeries(rec, w)
		for i, win := range w.Windows {
			rec("workload=%s window=%d probes=%d reads=%d commit=%+v read=%+v wait=%+v", w.Name, i, win.Probes, win.Reads, win.Commit, win.Read, win.Wait)
		}
		rec("workload=%s settings writer=%+v reader=%+v reader_count=%d writer_max_open=%d reader_max_open=%d",
			w.Name, w.Settings.Writer, w.Settings.Reader, w.Settings.ReaderCount, w.Settings.WriterMaxOpen, w.Settings.ReaderMaxOpen)
		rec("workload=%s connects writer=%d reader=%d", w.Name, w.Connects.Writer, w.Connects.Reader)
		rec("workload=%s allocs mallocs=%d bytes=%d ops=%d", w.Name, w.Allocs.Mallocs, w.Allocs.Bytes, w.Allocs.Ops)
		rec("workload=%s write_amplification written_bytes=%d logical_bytes=%d milli=%d", w.Name, w.WriteAmp.WrittenBytes, w.WriteAmp.LogicalBytes, w.WriteAmp.Milli)
		rec("workload=%s wal file_bytes=%d db_bytes=%d page_size=%d checkpoint_busy=%d log_frames=%d checkpointed_frames=%d",
			w.Name, w.WAL.FileBytes, w.WAL.DBBytes, w.WAL.PageSize, w.WAL.CheckpointBusy, w.WAL.LogFrames, w.WAL.Checkpointed)
		rec("workload=%s memory vm_hwm_kib=%d vm_rss_kib=%d go_sys_bytes=%d go_heap_inuse_bytes=%d",
			w.Name, w.Memory.VmHWMKiB, w.Memory.VmRSSKiB, w.Memory.GoSysBytes, w.Memory.GoHeapInuseBytes)
		for _, name := range slices.Sorted(maps.Keys(w.Plans)) {
			for _, line := range w.Plans[name] {
				rec("workload=%s plan=%s detail=%q", w.Name, name, line)
			}
		}
	}
	return err
}

// recorder prints each line through out and keeps the first failed write in
// err; once one has failed it writes nothing more.
func recorder(out io.Writer, err *error) func(string, ...any) {
	return func(format string, args ...any) {
		if *err == nil {
			_, *err = fmt.Fprintf(out, "STORE-RECORD "+format+"\n", args...)
		}
	}
}

func printHead(rec func(string, ...any), r Run) {
	rec("kind=%s os=%s arch=%s go=%s cpus=%d gomaxprocs=%d", r.Kind, r.Host.OS, r.Host.Arch, r.Host.GoVersion, r.Host.CPUs, r.Host.GOMAXPROCS)
	rec("eligibility race=%t swap_in_pages=%d swap_out_pages=%d swap_kib=%d", r.Facts.Race, r.Facts.SwapIn, r.Facts.SwapOut, r.Facts.SwapKiB)
}

func printSeries(rec func(string, ...any), w Workload) {
	rec("workload=%s shape=%q", w.Name, w.Shape)
	for _, m := range w.Metrics() {
		rec("workload=%s metric=%s value=%d", w.Name, m.Name, m.Value)
	}
}
