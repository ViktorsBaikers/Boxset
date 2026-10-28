package main

// The wire, the recorded arithmetic and the judge of the access series. The
// producer in main.go writes exactly this wire; nothing else reads it, so the
// budgets, the sample shape and the verdict live in one place.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
)

const (
	// dataMount is the runner's writable data mount: every fixture copy, DB,
	// WAL and lock of the run sits under it, and the samples say so.
	dataMount = "/bench-data"

	samplesName   = "samples.jsonl"
	samplesSchema = 1
	samplesKind   = "access-timing-samples"

	maxSamplesBytes = 8 << 20
	maxSampleLine   = 1 << 20
	maxSamples      = 1 << 14
	maxRaw          = 1 << 20
	maxValue        = 1 << 50

	// executions is the recorded count on each of the two interleaved sides.
	executions = 5
	sideA1     = "a1"
	sideA2     = "a2"

	// purpose* is a binary's build identity, read back from its build info.
	purposeProduction = "production"
	purposeFill       = "fill-fixture"
	purposeSlow       = "slow-control"
	tagFill           = "benchfill"
	tagSlow           = "benchslow"

	controlSlow = "slow"

	// The goroutine observation is the ABUSE window's own 1 Hz poll of the
	// authorized status route: the wire records whether the run took it instead of
	// recording a zero that would read as a healthy measurement. no_observation_surface
	// is left for a build that cannot read the route at all — the owner's decision on
	// q-2026-10-27-002 put the reading on that route, so a normal run never records
	// it — and observation_failed is a window whose poll was refused, timed out or
	// read a count below one.
	goroutinesObserved    = "observed"
	goroutinesUnavailable = "unavailable"
	goroutinesNoSource    = "no_observation_surface"
	goroutinesPollFailed  = "observation_failed"
)

// The series the protocol records.
const (
	seriesStart      = "start"
	seriesKey        = "key"
	seriesHash       = "hash"
	seriesAbuse      = "abuse"
	seriesGZIP       = "gzip"
	seriesRSS        = "rss"
	seriesMemoryPeak = "memory_peak"
	seriesOOMKill    = "oom_kill"
	seriesGoroutines = "goroutines"
)

// The recorded budgets, in nanoseconds, bytes or counts.
const (
	startBudgetNS   = 250 * 1_000_000 // /ping first 200 within 250 ms
	keyBudgetNS     = 100_000         // paired key overhead under 100 µs
	listBudgetNS    = 20 * 1_000_000  // other-user list of 50 within 20 ms
	rssBudgetBytes  = 60_000_000      // idle RSS with the buffers at their caps
	peakBudgetBytes = 4 << 30         // cgroup memory.peak under 4 GiB
	goroutineCap    = 1_000

	rssWindowStartMS = 600_000 // the verdict sample lies inside 600–605 s
	rssWindowEndMS   = 605_000
)

// timingSpec is one A/A series: the recorded budget plus the two directions the
// record writes differently. report marks a report-only series, whose samples
// are required but whose values carry no verdict.
type timingSpec struct {
	series    string
	budget    int64
	strict    bool // the record writes "<" rather than "≤"
	report    bool
	unit      string
	execution bool // a per-execution sample of this series exists on both sides
}

// timingSeries is the recorded series order: it decides which breach a verdict
// names first.
var timingSeries = []timingSpec{
	{series: seriesStart, budget: startBudgetNS, unit: "ns", execution: true},
	{series: seriesKey, budget: keyBudgetNS, strict: true, unit: "ns", execution: true},
	{series: seriesHash, budget: listBudgetNS, unit: "ns", execution: true},
	{series: seriesAbuse, budget: listBudgetNS, unit: "ns", execution: true},
	{series: seriesGZIP, report: true, unit: "ns", execution: true},
}

// capSeries are the run's cap readings. The RSS series keeps every hold reading
// and is judged on the one inside the verdict window.
type capSeries struct {
	series string
	limit  int64
	strict bool
	window bool
	unit   string
}

var caps = []capSeries{
	{series: seriesRSS, limit: rssBudgetBytes, window: true, unit: "bytes"},
	{series: seriesMemoryPeak, limit: peakBudgetBytes, strict: true, unit: "bytes"},
	{series: seriesOOMKill, limit: 0},
	{series: seriesGoroutines, limit: goroutineCap},
}

// header is the first record of samples.jsonl: the run's provenance and the
// build identity of the binary the timing series measured.
type header struct {
	Schema     int    `json:"schema"`
	Kind       string `json:"kind"`
	Run        string `json:"run"`
	InputsSHA  string `json:"inputs_sha256"`
	DataDir    string `json:"data_dir"`
	Control    string `json:"control"`
	Purpose    string `json:"binary_purpose"`
	Tags       string `json:"build_tags"`
	CGO        string `json:"cgo"`
	Trimpath   bool   `json:"trimpath"`
	Go         string `json:"go"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	Goroutines string `json:"goroutines"`
	Reason     string `json:"goroutines_reason"`
	Executions int    `json:"executions"`
	Samples    int    `json:"samples"`
	StartDate  string `json:"start_date"`
	EndDate    string `json:"end_date"`
	UTCOffset  int    `json:"utc_offset_sec"`
}

// sample is one recorded statistic of one series, with the raw values it came
// from. side and exec place it in the interleaved A/A design; the run-level RSS
// hold has no side and keeps its elapsed milliseconds in raw.
type sample struct {
	Series  string  `json:"series"`
	Side    string  `json:"side"`
	Exec    int     `json:"exec"`
	Purpose string  `json:"purpose"`
	Stat    int64   `json:"stat"`
	Raw     []int64 `json:"raw"`
}

// sampleSet is the content of one samples.jsonl.
type sampleSet struct {
	Header  header
	Samples []sample
}

func isPurpose(s string) bool {
	return s == purposeProduction || s == purposeFill || s == purposeSlow
}

func isSeries(name string) bool {
	switch name {
	case seriesStart, seriesKey, seriesHash, seriesAbuse, seriesGZIP,
		seriesRSS, seriesMemoryPeak, seriesOOMKill, seriesGoroutines:
		return true
	}
	return false
}

// isTiming reports whether name is one of the A/A timing series.
func isTiming(name string) bool {
	switch name {
	case seriesStart, seriesKey, seriesHash, seriesAbuse, seriesGZIP:
		return true
	}
	return false
}

func (h header) validShape() bool {
	return h.Schema == samplesSchema && h.Kind == samplesKind &&
		h.Run != "" && len(h.Run) <= 64 &&
		benchprofile.IsLowerHex(h.InputsSHA, benchprofile.HexSHA256Len) &&
		h.DataDir != "" && len(h.DataDir) <= 256 &&
		(h.Control == "" || h.Control == controlSlow) &&
		isPurpose(h.Purpose) && len(h.Tags) <= 64 && len(h.Go) <= 64 &&
		(h.CGO == "0" || h.CGO == "1" || h.CGO == "unknown") &&
		(h.Goroutines == goroutinesObserved || h.Goroutines == goroutinesUnavailable) &&
		(h.Goroutines == goroutinesObserved) == (h.Reason == "") &&
		h.Reason != goroutinesUnavailable &&
		h.Executions == executions && h.Samples >= 1 && h.Samples <= maxSamples &&
		h.UTCOffset >= -14*3600 && h.UTCOffset <= 14*3600
}

func (s sample) validShape() bool {
	if !isSeries(s.Series) || !isPurpose(s.Purpose) || len(s.Raw) > maxRaw {
		return false
	}
	if s.Series == seriesRSS {
		if s.Side != "" || s.Exec != 0 {
			return false
		}
	} else if (s.Side != sideA1 && s.Side != sideA2) || s.Exec < 1 || s.Exec > executions {
		return false
	}
	if s.Stat < -maxValue || s.Stat > maxValue {
		return false
	}
	for _, v := range s.Raw {
		if v < -maxValue || v > maxValue {
			return false
		}
	}
	return true
}

// writeSamples writes samples.jsonl in place: the file is opened with truncate
// and every write, the flush and the close are checked. The header's count is
// the writer's, never the caller's.
func writeSamples(path string, set *sampleSet) error {
	set.Header.Samples = len(set.Samples)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	werr := enc.Encode(set.Header)
	for i := 0; werr == nil && i < len(set.Samples); i++ {
		// The wire carries an array, never a null: a nil slice is the empty list
		// of raw values, and the decoder refuses a null as a missing field.
		s := set.Samples[i]
		if s.Raw == nil {
			s.Raw = []int64{}
		}
		werr = enc.Encode(s)
	}
	if werr == nil {
		werr = bw.Flush()
	}
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	switch {
	case err == nil:
		return line[:len(line)-1], nil
	case errors.Is(err, io.EOF) && len(line) == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF), errors.Is(err, bufio.ErrBufferFull):
		return nil, benchprofile.Invalid("samples_malformed")
	}
	return nil, benchprofile.Invalid("samples_unreadable")
}

// decodeLine requires the exact key set of one record, refuses a null value and
// any field the wire does not have, and refuses trailing bytes.
func decodeLine(line []byte, into any) error {
	if err := benchprofile.RequireKeys(line, benchprofile.JSONKeys(reflect.TypeOf(into).Elem()), "samples"); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return benchprofile.Invalid("samples_malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return benchprofile.Invalid("samples_malformed")
	}
	return nil
}

// parseSamples structurally validates samples.jsonl bytes: a header and exactly
// the samples the header counts, each with the shape of its series. A file that
// ends before its header's count is incomplete; a line after the last sample is
// malformed.
func parseSamples(data []byte) (*sampleSet, error) {
	br := bufio.NewReaderSize(bytes.NewReader(data), maxSampleLine)
	set := &sampleSet{}
	line, err := readLine(br)
	if err != nil {
		return nil, incompleteOr(err)
	}
	if err := decodeLine(line, &set.Header); err != nil {
		return nil, err
	}
	h := set.Header
	if !h.validShape() {
		return nil, benchprofile.Invalid("samples_malformed")
	}
	set.Samples = make([]sample, 0, h.Samples)
	for range h.Samples {
		line, err := readLine(br)
		switch {
		case errors.Is(err, io.EOF):
			return nil, benchprofile.Invalid("samples_incomplete")
		case err != nil:
			return nil, err
		}
		var s sample
		if err := decodeLine(line, &s); err != nil {
			return nil, err
		}
		if !s.validShape() {
			return nil, benchprofile.Invalid("samples_malformed")
		}
		set.Samples = append(set.Samples, s)
	}
	if _, err := readLine(br); !errors.Is(err, io.EOF) {
		return nil, benchprofile.Invalid("samples_malformed")
	}
	return set, nil
}

// incompleteOr maps an empty file to an incomplete one.
func incompleteOr(err error) error {
	if errors.Is(err, io.EOF) {
		return benchprofile.Invalid("samples_incomplete")
	}
	return err
}

// distance is the absolute difference of the two sides' medians: the noise the
// run itself measured, never a percentage of it.
func distance(a, b int64) int64 {
	if a < b {
		return b - a
	}
	return a - b
}

// median is the A/A side's per-execution median.
func median(values []int64) int64 {
	s := slices.Clone(values)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// verdict is one series' or one cap's recorded answer.
type verdict int

const (
	seriesPass verdict = iota
	seriesFail
	seriesInvalid
)

// seriesOutcome is one series' answer with the numbers it was decided on.
type seriesOutcome struct {
	series  string
	unit    string
	verdict verdict
	reason  string
	value   int64 // the larger side median, or the measured maximum of a cap
	noise   int64
}

func (o seriesOutcome) field() string {
	if o.unit == "" {
		return fmt.Sprintf("%s=%d", o.series, o.value)
	}
	return fmt.Sprintf("%s_%s=%d", o.series, o.unit, o.value)
}

func (s timingSpec) meets(v int64) bool {
	if s.strict {
		return v < s.budget
	}
	return v <= s.budget
}

// judgeTiming applies the recorded A/A arithmetic to one series: the median of
// each side's five per-execution statistics, the measured noise between them —
// never an assumed percentage — and one budget. A noise at or above the budget
// is INVALID because the threshold sits below the run's own noise; only a
// smaller side median past the budget fails, so a straddling pair is INVALID
// with within_noise rather than a verdict either way.
func judgeTiming(spec timingSpec, samples []sample) seriesOutcome {
	out := seriesOutcome{series: spec.series, unit: spec.unit}
	var sides [2][]int64
	for _, s := range samples {
		if s.Series != spec.series {
			continue
		}
		switch s.Side {
		case sideA1:
			sides[0] = append(sides[0], s.Stat)
		case sideA2:
			sides[1] = append(sides[1], s.Stat)
		}
	}
	a1, a2 := median(sides[0]), median(sides[1])
	out.value, out.noise = max(a1, a2), distance(a1, a2)
	if spec.report {
		out.verdict = seriesPass
		return out
	}
	switch {
	case out.noise >= spec.budget:
		out.verdict, out.reason = seriesInvalid, "noise"
	case spec.meets(out.value + out.noise):
		out.verdict = seriesPass
	case !spec.meets(min(a1, a2)):
		out.verdict, out.reason = seriesFail, spec.series+"_budget"
	default:
		out.verdict, out.reason = seriesInvalid, "within_noise"
	}
	return out
}

// judgeCap applies one cap to the run's readings of its series: the RSS hold is
// judged on the reading inside 600–605 s, every other cap on the run's maximum.
func judgeCap(spec capSeries, samples []sample) seriesOutcome {
	out := seriesOutcome{series: spec.series, unit: spec.unit}
	seen := false
	for _, s := range samples {
		if s.Series != spec.series {
			continue
		}
		if spec.window && (len(s.Raw) == 0 || s.Raw[0] < rssWindowStartMS || s.Raw[0] > rssWindowEndMS) {
			continue
		}
		if !seen || s.Stat > out.value {
			out.value = s.Stat
		}
		seen = true
	}
	if !seen {
		out.verdict, out.reason = seriesInvalid, "samples_missing"
		return out
	}
	if (spec.strict && out.value < spec.limit) || (!spec.strict && out.value <= spec.limit) {
		out.verdict = seriesPass
		return out
	}
	out.verdict, out.reason = seriesFail, spec.series
	return out
}

// verdicts is the whole run's answer: every series' outcome plus the one token a
// verdict line carries.
type verdicts struct {
	outcomes []seriesOutcome
	invalid  string
	fail     string
	noise    int64
}

// judgeSamples applies the recorded rules to a structurally valid sample set.
// INVALID beats FAIL: a budget verdict on evidence the run itself made unusable
// is never produced. A control run must fail all four budgeted timing series,
// and failing fewer is the finding.
func judgeSamples(set *sampleSet) verdicts {
	var out verdicts
	for _, spec := range timingSeries {
		o := judgeTiming(spec, set.Samples)
		out.outcomes = append(out.outcomes, o)
		out.noise = max(out.noise, o.noise)
	}
	for _, spec := range caps {
		out.outcomes = append(out.outcomes, judgeCap(spec, set.Samples))
	}
	for _, o := range out.outcomes {
		if o.verdict == seriesInvalid {
			if out.invalid == "" {
				out.invalid = o.reason
			}
			return out
		}
	}
	var failing []seriesOutcome
	for _, o := range out.outcomes {
		if o.verdict == seriesFail {
			failing = append(failing, o)
		}
	}
	if set.Header.Control == controlSlow {
		if len(failing) != len(timingSeries)-1 {
			out.fail = "control"
			return out
		}
		out.fail = failing[0].reason
		return out
	}
	if len(failing) > 0 {
		out.fail = failing[0].reason
	}
	return out
}

// checkSlots requires every timing slot of both interleaved sides exactly once:
// a missing execution is INVALID, and a repeated one is not a second measurement.
func checkSlots(samples []sample) error {
	type slot struct {
		series, side string
		exec         int
	}
	seen := make(map[slot]bool, len(samples))
	for _, s := range samples {
		if !isTiming(s.Series) {
			continue
		}
		k := slot{series: s.Series, side: s.Side, exec: s.Exec}
		if seen[k] {
			return benchprofile.Invalid("samples_malformed")
		}
		seen[k] = true
	}
	for _, spec := range timingSeries {
		for _, side := range []string{sideA1, sideA2} {
			for exec := 1; exec <= executions; exec++ {
				if !seen[slot{series: spec.series, side: side, exec: exec}] {
					return benchprofile.Invalid("samples_missing")
				}
			}
		}
	}
	return nil
}

// checkBuild requires the producing binary to be the one the run claims, and a
// non-control run to have measured the ordinary production build. Production
// identity is the absence of a build -tags setting, so the bench tags and the
// tree's own integration and e2e tags are all non-production.
func checkBuild(h header) error {
	if h.Control == controlSlow {
		if h.Purpose != purposeSlow || h.Tags != tagSlow {
			return benchprofile.Invalid("build")
		}
		return nil
	}
	if h.Purpose != purposeProduction {
		return benchprofile.Invalid("binary_purpose")
	}
	if h.Tags != "" {
		return benchprofile.Invalid("build")
	}
	return nil
}

// checkPurpose requires every sample's producing binary to be the one its series
// may claim: a fill fixture never counts toward a timing series.
func checkPurpose(set *sampleSet) error {
	for _, s := range set.Samples {
		if s.Series == seriesRSS {
			if s.Purpose != purposeFill {
				return benchprofile.Invalid("binary_purpose")
			}
			continue
		}
		if s.Purpose != set.Header.Purpose {
			return benchprofile.Invalid("binary_purpose")
		}
	}
	return nil
}

// checkSamples ties the samples to the run, the image's inputs, the data mount
// and one UTC date the records also show, and requires the slots and the build
// identity of the recorded protocol.
func checkSamples(set *sampleSet, recs [3]benchprofile.Record, run string) error {
	h := set.Header
	date := benchprofile.FactsUTCDate(recs[0].Host.StartUnix)
	switch {
	case h.Run != run:
		return benchprofile.Invalid("run_mismatch")
	case h.InputsSHA != recs[0].Inputs:
		return benchprofile.Invalid("inputs_mismatch")
	case h.DataDir != dataMount:
		return benchprofile.Invalid("data_dir")
	case h.GOOS != "linux" || h.GOARCH != "amd64":
		return benchprofile.Invalid("platform")
	case h.CGO != "0" || !h.Trimpath:
		return benchprofile.Invalid("build")
	case h.UTCOffset != 0 || h.StartDate != h.EndDate || h.StartDate != date || date != benchprofile.FactsUTCDate(recs[2].Host.EndUnix):
		return benchprofile.Invalid("date_rollover")
	}
	if err := checkBuild(h); err != nil {
		return err
	}
	if err := checkSlots(set.Samples); err != nil {
		return err
	}
	return checkPurpose(set)
}

// judgeEnv is where the judge finds what it judges: the immutable image, read
// through the reader it shares with the parser judge. A plain directory read
// proves nothing about where the bytes came from.
type judgeEnv struct {
	img *benchprofile.Image
}

// loadRun reads one run of the image: its three V2 records, the data-then-host
// eligibility predicate, and then its samples.
func (e *judgeEnv) loadRun(run string) (*sampleSet, [3]benchprofile.Record, error) {
	if err := e.img.CheckParents(run); err != nil {
		return nil, [3]benchprofile.Record{}, err
	}
	dir := e.img.RunPath(run)
	var datas [3][]byte
	for i, stage := range benchprofile.RecordStages {
		data, err := e.img.ReadImported(filepath.Join(dir, stage+benchprofile.FactFileNameSuffix), benchprofile.MaxFactBytes, "record")
		if err != nil {
			return nil, [3]benchprofile.Record{}, err
		}
		datas[i] = data
	}
	recs, err := benchprofile.DecodeRecordSet(datas)
	if err != nil {
		return nil, recs, err
	}
	if err := benchprofile.CheckDataEligibility(run, recs); err != nil {
		return nil, recs, err
	}
	data, err := e.img.ReadImported(filepath.Join(dir, "samples", samplesName), maxSamplesBytes, "samples")
	if err != nil {
		return nil, recs, err
	}
	set, err := parseSamples(data)
	return set, recs, err
}

// judge is the only place that may print ACCESS-BENCH-PASS.
func judge(root, runDir, inputs string, w io.Writer) int {
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return printInvalid(w, benchprofile.Invalid("image_manifest"), "")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return printInvalid(w, benchprofile.Invalid("run_location"), "")
	}
	// The child runs as uid 1000: the image's files are root's only because the
	// image build wrote them, so a record the benchmarked process could write is
	// not one.
	return judgeWith(&judgeEnv{img: benchprofile.NewImage(abs, 0, manifest)}, runDir, w)
}

// judgeWith judges one run of env's image. INVALID (exit 3) beats FAIL (exit 1):
// an ineligible run's samples are never given a budget verdict.
func judgeWith(env *judgeEnv, runDir string, w io.Writer) int {
	run, err := env.img.Locate(runDir)
	if err != nil {
		return printInvalid(w, err, "")
	}
	set, recs, err := env.loadRun(run)
	if err != nil {
		return printInvalid(w, err, "")
	}
	if err := checkSamples(set, recs, run); err != nil {
		return printInvalid(w, err, observation(set))
	}
	result := judgeSamples(set)
	if result.invalid != "" {
		return printInvalid(w, benchprofile.Invalid(result.invalid), observation(set))
	}
	if result.fail != "" {
		say(w, "%s\n", verdictLine("ACCESS-BENCH-FAIL", result.fail, result))
		return exitFail
	}
	say(w, "%s\n", verdictLine("ACCESS-BENCH-PASS", "", result))
	return exitPass
}

// observation is the samples header's own account of the goroutine observation:
// a run that could not take it says so on the line that refuses it, so the
// absence is a reported reason rather than a bare missing sample.
func observation(set *sampleSet) string {
	if set.Header.Goroutines == goroutinesUnavailable {
		return set.Header.Reason
	}
	return ""
}

func verdictLine(kind, reason string, result verdicts) string {
	var b strings.Builder
	b.WriteString(kind)
	if reason != "" {
		fmt.Fprintf(&b, " reason=%s", reason)
	}
	for _, o := range result.outcomes {
		fmt.Fprintf(&b, " %s", o.field())
	}
	fmt.Fprintf(&b, " noise_ns_max=%d", result.noise)
	return b.String()
}

func printInvalid(w io.Writer, err error, observation string) int {
	reason, ok := benchprofile.InvalidReason(err)
	if !ok {
		reason = "unreadable"
	}
	if observation != "" {
		say(w, "ACCESS-BENCH-INVALID reason=%s observation=%s\n", reason, observation)
		return exitInvalid
	}
	say(w, "ACCESS-BENCH-INVALID reason=%s\n", reason)
	return exitInvalid
}

// say writes one line of the tool's own output; a failed write to stdout or
// stderr has nowhere to be reported.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
