package main

// The wire and the judge of the effects series. The harness in main.go and
// workload.go writes exactly this wire; nothing else reads it, so the recorded
// shape, the arithmetic and the verdict of an admitted job start live in one
// place.

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
	"strconv"
	"strings"
	"time"

	storebench "github.com/ViktorsBaikers/Boxset/bench/store"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
)

const (
	// dataMount is the runner's writable data mount: every DB, WAL and lock of
	// the run sits under it, and the samples say so.
	dataMount = "/bench-data"

	samplesName   = "samples.jsonl"
	samplesSchema = 1
	samplesKind   = "effects-job-start-samples"
	protocolID    = "effects-job-start-v1"

	// The series is seven serial invocations: five timing, two correctness, and
	// two A/A arms inside each. A run's place in the series is the judge's own
	// derivation from the list it was given, never a self-report.
	seriesRuns = 7
	timingRuns = 5
	executions = 2

	armA1 = "a1"
	armA2 = "a2"

	orderAB = "AB"
	orderBA = "BA"

	// budgetNS is ADR 0005 T08: an admitted interactive job starts within 100 ms.
	budgetNS = int64(100 * time.Millisecond)

	// The writer series: one probe tick every 50 ms from the producer's fifth
	// second to its last request, and at least 200 of them counted in the load.
	writerScheduled = 1_100
	minInLoad       = 200

	// The load's recorded shape.
	healthyStreams = 10
	slowStreams    = 2

	// The bounds of the recorded wire.
	maxSamplesBytes = 32 << 20
	maxLineBytes    = 1 << 20
	maxRecords      = 4_096
	maxRaw          = 2_048
	maxValue        = 1 << 50

	// memoryMaxBytes is the runner's container limit, the sole memory threshold.
	memoryMaxBytes = 4 << 30

	// The position-7 retention shape: 32 held reads of 1 MiB, 16 MiB decoded,
	// 2,048 probe hosts and the limiter cache's 4,096 keys, and one resident
	// child of 256 MiB.
	retentionReads   = 32
	// retentionRetryAllowance bounds safe-read retries of the same probe host: a
	// transient timeout may retry the logical probe, and the retry consumes the
	// same host budget/key. The logical probe count, key count and budget refusal
	// stay exact; only same-host retry requests are tolerated.
	retentionRetryAllowance = 64
	retentionBodies  = 1 << 20
	retentionDecoded = 16 << 20
	retentionProbes  = 2_048
	retentionKeyMax  = 4_096
	minChildRSS      = 256 << 20

	classProduction = "production"
	classControl    = "control"
	tagSlow         = "benchslow"

	recordTiming = "timing"
	recordWriter = "writer"

	exitPass    = 0
	exitFail    = 1
	exitUsage   = 2
	exitInvalid = 3
)

// retentionPhases is the recorded order of the position-7 saturation: the 32
// held reads, the refused 33rd, the release and decode, the probe hosts, then
// the resident child.
var retentionPhases = []string{"hold", "refuse", "release", "probes", "child"}

// position is one run's recorded place in the series: the ORDER its two arms ran
// in, and the GOMAXPROCS it was given (0: whatever the binary defaulted to).
type position struct {
	order string
	procs int
}

// seriesPositions is the recorded design. Its fifth and sixth entries are the
// correctness invocations: one core for progress, 64 cores for memory and the
// aggregate background ceiling.
var seriesPositions = [seriesRuns]position{
	{order: orderAB}, {order: orderBA}, {order: orderAB}, {order: orderBA}, {order: orderAB},
	{order: orderAB, procs: 1}, {order: orderAB, procs: 64},
}

// positionOf is a measured run's place in the recorded series. The explicit
// -position wins; otherwise the GOMAXPROCS the measured command exports names
// the two correctness positions, and a run that exported none is a timing
// position. The position-7 saturation is gated on this place, never on the
// host's core count, so a timing position running on many cores never records
// one.
func positionOf(explicit int, order, exported string) int {
	if explicit != 0 {
		return explicit
	}
	procs, err := strconv.Atoi(exported)
	if err != nil {
		return 0
	}
	for i, p := range seriesPositions {
		if p.procs != 0 && p.order == order && p.procs == procs {
			return i + 1
		}
	}
	return 0
}

// runsRetention reports whether the run at pos measures the position-7
// saturation: only the recorded memory position does.
func runsRetention(pos int) bool { return pos == seriesRuns }

// streamCounts are the streams every run opens: ten that read, two that do not.
type streamCounts struct {
	Healthy int `json:"healthy"`
	Slow    int `json:"slow"`
}

// header is the first record of samples.jsonl: the run's provenance, the build
// identity of the executed binary, and the correctness counts the run observed.
type header struct {
	Schema     int          `json:"schema"`
	Kind       string       `json:"kind"`
	Run        string       `json:"run"`
	Protocol   string       `json:"protocol"`
	Order      string       `json:"order"`
	Procs      int          `json:"procs"`
	DataDir    string       `json:"data_dir"`
	Inputs     string       `json:"inputs_sha256"`
	Digest     string       `json:"binary_sha256"`
	Class      string       `json:"binary_class"`
	Tags       string       `json:"build_tags"`
	CGO        string       `json:"cgo"`
	Trimpath   bool         `json:"trimpath"`
	Race       string       `json:"race"`
	Cover      string       `json:"covermode"`
	Go         string       `json:"go"`
	GOOS       string       `json:"goos"`
	GOARCH     string       `json:"goarch"`
	StartDate  string       `json:"start_date"`
	EndDate    string       `json:"end_date"`
	UTCOffset  int          `json:"utc_offset_sec"`
	Streams    streamCounts `json:"streams"`
	Frames     int64        `json:"healthy_frames"`
	Gaps       int64        `json:"healthy_gaps"`
	SlowClosed int          `json:"slow_closed"`
	Background int          `json:"background_max"`
	Purge      int64        `json:"purge_progress"`
	Peak       int64        `json:"memory_peak_bytes"`
	PeakMax    int64        `json:"memory_max_bytes"`
	OOMKill    int64        `json:"oom_kill"`
	Retention  retention    `json:"retention"`
	Records    int          `json:"records"`
}

// retention is the position-7 saturation's recorded observation. A run that did
// not measure it says present false; a position-7 run that does the same is
// refused, because the phases are evidence rather than a note.
type retention struct {
	Present        bool     `json:"present"`
	Phases         []string `json:"phases"`
	HeldReads      int      `json:"held_reads"`
	QueueFull      string   `json:"queue_full"`
	QueueRequests  int      `json:"queue_full_requests"`
	Released       int      `json:"released_reads"`
	BodyBytes      int64    `json:"body_bytes_each"`
	DecodedBytes   int64    `json:"decoded_bytes"`
	DecodedSHA     string   `json:"decoded_sha256"`
	ServedSHA      string   `json:"served_sha256"`
	ProbeSuccesses int      `json:"probe_successes"`
	ProbeRequests  int      `json:"probe_requests"`
	KeysAdded      int      `json:"keys_added"`
	ProbeBudget    string   `json:"probe_budget"`
	ProbeLimit     string   `json:"probe_limit"`
	ProbeMax       int      `json:"probe_max"`
	Repeat         string   `json:"repeat_success"`
	HostsVerified  int      `json:"hosts_verified"`
	ChildRSS       int64    `json:"child_rss_bytes"`
	ChildSlots     int      `json:"child_slots"`
	ChildReaped    bool     `json:"child_reaped"`
}

// timingRecord is one arm's admitted-job starts: the raw nanoseconds from
// Accept's return to the receipt of the job's running frame, in measured order.
type timingRecord struct {
	Kind   string  `json:"kind"`
	Run    string  `json:"run"`
	Arm    string  `json:"arm"`
	Order  string  `json:"order"`
	Procs  int     `json:"procs"`
	Digest string  `json:"binary_sha256"`
	Starts []int64 `json:"starts_ns"`
}

// writerRecord is one arm's writer series: every scheduled probe tick, the ones
// a probe in flight skipped, the in-load ones with the raw nanoseconds they
// measured and the summaries the judge recomputes rather than trusts.
type writerRecord struct {
	Kind      string  `json:"kind"`
	Run       string  `json:"run"`
	Arm       string  `json:"arm"`
	Order     string  `json:"order"`
	Procs     int     `json:"procs"`
	Digest    string  `json:"binary_sha256"`
	Scheduled int     `json:"scheduled"`
	Skipped   int     `json:"skipped"`
	InLoad    int     `json:"in_load"`
	Open      bool    `json:"open"`
	FirstNS   int64   `json:"producer_first_ns"`
	LastNS    int64   `json:"producer_last_ns"`
	StartsNS  []int64 `json:"starts_ns"`
	WaitRaw   []int64 `json:"wait_raw"`
	WaitP50   int64   `json:"wait_p50"`
	WaitP95   int64   `json:"wait_p95"`
	WaitP99   int64   `json:"wait_p99"`
	CommitRaw []int64 `json:"commit_raw"`
	CommitP50 int64   `json:"commit_p50"`
	CommitP95 int64   `json:"commit_p95"`
	CommitP99 int64   `json:"commit_p99"`
}

// sampleSet is the content of one samples.jsonl.
type sampleSet struct {
	Header  header
	Timings []timingRecord
	Writers []writerRecord
	// Span is the measured region's actual span. It is the harness's own
	// diagnostic, never a wire field: the judge reads only what the records
	// carry.
	Span time.Duration
}

var (
	retentionKeys = benchprofile.JSONKeys(reflect.TypeFor[retention]())
	timingKeys    = benchprofile.JSONKeys(reflect.TypeFor[timingRecord]())
	writerKeys    = benchprofile.JSONKeys(reflect.TypeFor[writerRecord]())
)

func (h header) validShape() bool {
	return h.Schema == samplesSchema && h.Kind == samplesKind &&
		benchprofile.ValidRunName(h.Run) &&
		h.DataDir == dataMount &&
		benchprofile.IsLowerHex(h.Inputs, benchprofile.HexSHA256Len) &&
		benchprofile.IsLowerHex(h.Digest, benchprofile.HexSHA256Len) &&
		(h.Order == orderAB || h.Order == orderBA) &&
		h.Procs >= 1 && h.Procs <= 1<<12 &&
		len(h.Go) <= 64 && len(h.Tags) <= 64 && len(h.Race) <= 8 && len(h.Cover) <= 64 &&
		(h.CGO == "0" || h.CGO == "1" || h.CGO == "unknown") &&
		h.Streams.Healthy >= 0 && h.Streams.Slow >= 0 &&
		h.Frames >= 0 && h.Gaps >= 0 && h.SlowClosed >= 0 && h.Background >= 0 &&
		h.Purge >= 0 && h.Peak >= 0 && h.PeakMax >= 0 && h.OOMKill >= 0 &&
		h.Records >= 1 && h.Records <= maxRecords &&
		h.UTCOffset >= -14*3600 && h.UTCOffset <= 14*3600
}

func (h header) validRetention() bool {
	if !h.Retention.Present {
		return true
	}
	r := h.Retention
	return slices.Equal(r.Phases, retentionPhases) &&
		r.HeldReads >= 0 && r.QueueRequests >= 0 && r.Released >= 0 &&
		r.BodyBytes >= 0 && r.DecodedBytes >= 0 && r.ProbeSuccesses >= 0 &&
		r.ProbeRequests >= 0 && r.KeysAdded >= 0 && r.ProbeMax >= 0 &&
		r.HostsVerified >= 0 && r.ChildRSS >= 0 && r.ChildSlots >= 0
}

func (t timingRecord) validShape() bool {
	return t.Kind == recordTiming && benchprofile.ValidRunName(t.Run) && isArm(t.Arm) &&
		(t.Order == orderAB || t.Order == orderBA) && t.Procs >= 1 &&
		benchprofile.IsLowerHex(t.Digest, benchprofile.HexSHA256Len) &&
		len(t.Starts) >= 1 && len(t.Starts) <= maxRaw
}

func (w writerRecord) validShape() bool {
	if w.Kind != recordWriter || !benchprofile.ValidRunName(w.Run) || !isArm(w.Arm) ||
		(w.Order != orderAB && w.Order != orderBA) || w.Procs < 1 ||
		!benchprofile.IsLowerHex(w.Digest, benchprofile.HexSHA256Len) ||
		w.Scheduled < 0 || w.Scheduled > writerScheduled || w.Skipped < 0 || w.InLoad < 0 ||
		w.FirstNS < 0 || w.LastNS < 0 || len(w.StartsNS) > maxRaw || len(w.WaitRaw) > maxRaw || len(w.CommitRaw) > maxRaw {
		return false
	}
	for _, v := range [][]int64{w.StartsNS, w.WaitRaw, w.CommitRaw, {w.WaitP50, w.WaitP95, w.WaitP99, w.CommitP50, w.CommitP95, w.CommitP99}} {
		if !withinValues(v) {
			return false
		}
	}
	return true
}

// withinValues reports whether every recorded number is inside the wire's range.
func withinValues(values []int64) bool {
	for _, v := range values {
		if v < 0 || v > maxValue {
			return false
		}
	}
	return true
}

func isArm(arm string) bool { return arm == armA1 || arm == armA2 }

// writeSamples writes samples.jsonl in place: the file is opened with truncate
// and every write, the flush and the close are checked. The header's count is
// the writer's, never the caller's.
func writeSamples(path string, set *sampleSet) error {
	set.Header.Records = len(set.Timings) + len(set.Writers)
	// The wire carries an array, never a null: a nil slice is the empty list of
	// its kind, and the decoder refuses a null as a missing field.
	if set.Header.Retention.Phases == nil {
		set.Header.Retention.Phases = []string{}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	werr := enc.Encode(set.Header)
	for _, t := range set.Timings {
		if werr != nil {
			break
		}
		t.Starts = nonNil(t.Starts)
		werr = enc.Encode(t)
	}
	for _, w := range set.Writers {
		if werr != nil {
			break
		}
		w.StartsNS, w.WaitRaw, w.CommitRaw = nonNil(w.StartsNS), nonNil(w.WaitRaw), nonNil(w.CommitRaw)
		werr = enc.Encode(w)
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

// nonNil is the empty list of a nil one: a recorded array is never null.
func nonNil(values []int64) []int64 {
	if values == nil {
		return []int64{}
	}
	return values
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
func decodeLine(line []byte, keys []string, into any) error {
	if err := benchprofile.RequireKeys(line, keys, "samples"); err != nil {
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
// the records the header counts, each of a kind the wire has and with the shape
// of that kind. A file that ends before its header's count is incomplete; a line
// after the last record is malformed.
func parseSamples(data []byte) (*sampleSet, error) {
	br := bufio.NewReaderSize(bytes.NewReader(data), maxLineBytes)
	set := &sampleSet{}
	line, err := readLine(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, benchprofile.Invalid("samples_incomplete")
		}
		return nil, err
	}
	var blocks struct {
		Retention json.RawMessage `json:"retention"`
	}
	if err := json.Unmarshal(line, &blocks); err != nil {
		return nil, benchprofile.Invalid("samples_malformed")
	}
	if err := benchprofile.RequireKeys(blocks.Retention, retentionKeys, "samples"); err != nil {
		return nil, err
	}
	if err := decodeLine(line, benchprofile.JSONKeys(reflect.TypeFor[header]()), &set.Header); err != nil {
		return nil, err
	}
	if !set.Header.validShape() || !set.Header.validRetention() {
		return nil, benchprofile.Invalid("samples_malformed")
	}
	for range set.Header.Records {
		line, err := readLine(br)
		switch {
		case errors.Is(err, io.EOF):
			return nil, benchprofile.Invalid("samples_incomplete")
		case err != nil:
			return nil, err
		}
		if err := decodeRecord(line, set); err != nil {
			return nil, err
		}
	}
	if _, err := readLine(br); !errors.Is(err, io.EOF) {
		return nil, benchprofile.Invalid("samples_malformed")
	}
	return set, nil
}

// decodeRecord decodes one record of the kind its own kind field names. A record
// whose kind is not on the wire is refused, so a self-reported verdict or any
// other field a producer invented has no place to arrive in.
func decodeRecord(line []byte, set *sampleSet) error {
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &kind); err != nil {
		return benchprofile.Invalid("samples_malformed")
	}
	switch kind.Kind {
	case recordTiming:
		var rec timingRecord
		if err := decodeLine(line, timingKeys, &rec); err != nil {
			return err
		}
		if !rec.validShape() {
			return benchprofile.Invalid("samples_malformed")
		}
		set.Timings = append(set.Timings, rec)
	case recordWriter:
		var rec writerRecord
		if err := decodeLine(line, writerKeys, &rec); err != nil {
			return err
		}
		if !rec.validShape() {
			return benchprofile.Invalid("samples_malformed")
		}
		set.Writers = append(set.Writers, rec)
	default:
		return benchprofile.Invalid("samples_malformed")
	}
	return nil
}

// ---- the judge -----------------------------------------------------------------

// judgeEnv is where the judge finds what it judges: the immutable image, read
// through the reader it shares with the other judges. A plain directory read
// proves nothing about where the bytes came from.
type judgeEnv struct {
	img *benchprofile.Image
}

// runData is one loaded run of the series: its three V2 records, its own
// identity and the samples it published.
type runData struct {
	run     string
	header  header
	timings []timingRecord
	writers []writerRecord
	recs    [3]benchprofile.Record
}

// loadRun reads one run of the image: its three V2 records through the
// data-then-host eligibility predicate, and then its samples.
func (e *judgeEnv) loadRun(run string) (*runData, error) {
	if err := e.img.CheckParents(run); err != nil {
		return nil, err
	}
	dir := e.img.RunPath(run)
	var datas [3][]byte
	for i, stage := range benchprofile.RecordStages {
		data, err := e.img.ReadImported(filepath.Join(dir, stage+benchprofile.FactFileNameSuffix), benchprofile.MaxFactBytes, "record")
		if err != nil {
			return nil, err
		}
		datas[i] = data
	}
	recs, err := benchprofile.DecodeRecordSet(datas)
	if err != nil {
		return nil, err
	}
	if err := benchprofile.CheckDataEligibility(run, recs); err != nil {
		return nil, err
	}
	data, err := e.img.ReadImported(filepath.Join(dir, "samples", samplesName), maxSamplesBytes, "samples")
	if err != nil {
		return nil, err
	}
	set, err := parseSamples(data)
	if err != nil {
		return nil, err
	}
	if set.Header.Run != run {
		return nil, benchprofile.Invalid("run_mismatch")
	}
	return &runData{run: run, header: set.Header, timings: set.Timings, writers: set.Writers, recs: recs}, nil
}

// judge is the only place that may print EFFECTS-BENCH-PASS.
func judge(root, runs, inputs string, w io.Writer) int {
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return printInvalid(w, benchprofile.Invalid("image_manifest"))
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return printInvalid(w, benchprofile.Invalid("run_location"))
	}
	// The child runs as uid 1000: the image's files are root's only because the
	// image build and the runner's publication wrote them, so a record the
	// benchmarked process could write is not one.
	return judgeWith(&judgeEnv{img: benchprofile.NewImage(abs, 0, manifest)}, strings.Split(runs, ","), w)
}

// judgeWith judges one seven-run series of env's image. INVALID (exit 3) beats
// FAIL (exit 1): an ineligible series is never given a budget verdict.
func judgeWith(env *judgeEnv, dirs []string, w io.Writer) int {
	set, err := env.loadSeries(dirs)
	if err != nil {
		return printInvalid(w, err)
	}
	result := judgeSeries(set)
	if result.invalid != "" {
		return printInvalid(w, benchprofile.Invalid(result.invalid))
	}
	printBaselines(w, result)
	if result.fail != "" {
		say(w, "%s\n", result.line("EFFECTS-BENCH-FAIL"))
		return exitFail
	}
	say(w, "%s\n", result.line("EFFECTS-BENCH-PASS"))
	return exitPass
}

// loadSeries reads the seven runs of one series: each entry is exactly
// docs/spikes/data/bench-<UTC> of the image, the list is distinct and strictly
// increasing, and every run passes its own eligibility predicate before the
// series is judged.
func (e *judgeEnv) loadSeries(dirs []string) ([]*runData, error) {
	if len(dirs) != seriesRuns {
		return nil, benchprofile.Invalid("run_count")
	}
	set := make([]*runData, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for i, dir := range dirs {
		name, err := e.img.Locate(dir)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, benchprofile.Invalid("run_duplicate")
		}
		if i > 0 && name <= set[i-1].run {
			return nil, benchprofile.Invalid("run_order")
		}
		seen[name] = true
		data, err := e.loadRun(name)
		if err != nil {
			return nil, err
		}
		set = append(set, data)
	}
	return set, nil
}

// verdicts is the whole series' answer: the one token a verdict line carries and
// the numbers it was decided on.
type verdicts struct {
	invalid  string
	fail     string
	startNS  [2]int64
	noiseNS  int64
	baseline [2][6]int64 // [arm][commit p50/p95/p99 then wait p50/p95/p99]
	spread   [6]int64
}

func (v verdicts) line(kind string) string {
	var b strings.Builder
	b.WriteString(kind)
	if v.fail != "" {
		fmt.Fprintf(&b, " reason=%s", v.fail)
	}
	fmt.Fprintf(&b, " job_start_p95_a1_ns=%d job_start_p95_a2_ns=%d noise_ns=%d", v.startNS[0], v.startNS[1], v.noiseNS)
	return b.String()
}

// judgeSeries applies the recorded rules to a loaded series. INVALUABLE order:
// identity first (one protocol, digest and settings set), then each run's own
// shape, then the budget and the correctness checks.
func judgeSeries(set []*runData) verdicts {
	var out verdicts
	if err := checkSeries(set); err != nil {
		out.invalid = reasonOf(err)
		return out
	}
	if err := checkWriters(set); err != nil {
		out.invalid = reasonOf(err)
		return out
	}
	if err := checkRetention(set); err != nil {
		out.invalid = reasonOf(err)
		return out
	}
	for _, d := range set {
		if err := memoryObservation(d); err != nil {
			out.invalid = reasonOf(err)
			return out
		}
	}
	start, noise, err := judgeJobStart(set)
	if err != nil {
		out.invalid = reasonOf(err)
		return out
	}
	out.startNS, out.noiseNS = start, noise
	out.baseline, out.spread = writerBaseline(set)
	for _, d := range set {
		if fail := correctnessFailure(d); fail != "" {
			out.fail = fail
			return out
		}
	}
	// The A/A sides' larger median plus the run's own measured noise is the
	// number the budget is compared with: a series whose sides disagree by the
	// budget itself is INVALID, and only one with both sides past it fails.
	switch {
	case set[0].header.Class == classControl:
		// A control series that meets the budget proves nothing: the judge would
		// pass a slow binary instead of failing it.
		if budgetMet(start, noise) {
			out.invalid = "control"
			return out
		}
		out.fail = "job_start_budget"
		return out
	case set[0].header.Class != classProduction:
		out.invalid = "binary_class"
		return out
	case budgetMet(start, noise):
		return out
	case min(start[0], start[1]) > budgetNS:
		out.fail = "job_start_budget"
		return out
	}
	out.invalid = "within_noise"
	return out
}

func budgetMet(start [2]int64, noise int64) bool {
	return max(start[0], start[1])+noise <= budgetNS
}

// reasonOf is one refused rule's recorded token.
func reasonOf(err error) string {
	if reason, ok := benchprofile.InvalidReason(err); ok {
		return reason
	}
	return "unreadable"
}

// checkSeries requires the seven runs to be one series: one protocol, one
// binary digest, one settings set, and one class derived from the executed
// build's own settings rather than from anything the wire claims.
func checkSeries(set []*runData) error {
	first := set[0].header
	for i, d := range set {
		h := d.header
		p := seriesPositions[i]
		switch {
		case h.Protocol != protocolID:
			return benchprofile.Invalid("protocol")
		case h.Inputs != d.recs[0].Inputs:
			return benchprofile.Invalid("inputs_mismatch")
		case h.GOOS != "linux" || h.GOARCH != "amd64":
			return benchprofile.Invalid("platform")
		case h.UTCOffset != 0 || h.StartDate != h.EndDate || h.StartDate != benchprofile.FactsUTCDate(d.recs[0].Host.StartUnix):
			return benchprofile.Invalid("date_rollover")
		case h.Class != classOf(h):
			return benchprofile.Invalid("binary_class")
		case h.Digest != first.Digest || !sameSettings(h, first):
			return benchprofile.Invalid("identity_mismatch")
		case h.Order != p.order:
			return benchprofile.Invalid("order")
		case p.procs != 0 && h.Procs != p.procs:
			return benchprofile.Invalid("procs")
		case p.procs == 0 && h.Procs != first.Procs:
			return benchprofile.Invalid("procs")
		}
	}
	return nil
}

// classOf derives a binary's class from its own build settings: production is
// the absence of a -tags setting with CGO disabled and trimpath on, and the
// control is that same build with the benchslow tag. Anything else — a race or
// cover build, another tag, CGO enabled — has no class at all.
func classOf(h header) string {
	if h.Race == "true" || h.Cover != "" {
		return ""
	}
	switch {
	case h.CGO == "0" && h.Trimpath && h.Tags == "":
		return classProduction
	case h.CGO == "0" && h.Trimpath && h.Tags == tagSlow:
		return classControl
	}
	return ""
}

// sameSettings reports whether two runs were executed by one build: the digest
// is compared beside this, so the settings are the argv and toolchain.
func sameSettings(a, b header) bool {
	return a.Class == b.Class && a.Tags == b.Tags && a.CGO == b.CGO && a.Trimpath == b.Trimpath &&
		a.Race == b.Race && a.Cover == b.Cover && a.Go == b.Go && a.GOOS == b.GOOS && a.GOARCH == b.GOARCH
}

// judgeJobStart decides the budget on the ten timing executions: each arm's
// p95 per run, each side's median of the five, and the measured noise between
// them.
func judgeJobStart(set []*runData) (start [2]int64, noise int64, err error) {
	var sides [2][]int64
	for i := range timingRuns {
		for arm := range executions {
			name := [...]string{armA1, armA2}[arm]
			rec, ok := timingOf(set[i], name)
			if !ok {
				return start, 0, benchprofile.Invalid("timing_missing")
			}
			if rec.Run != set[i].run || rec.Order != set[i].header.Order || rec.Procs != set[i].header.Procs || rec.Digest != set[i].header.Digest {
				return start, 0, benchprofile.Invalid("timing_attribution")
			}
			p95 := storebench.Summarize(slices.Clone(rec.Starts)).P95
			if p95 <= 0 || p95 > maxValue {
				return start, 0, benchprofile.Invalid("timing_missing")
			}
			sides[arm] = append(sides[arm], p95)
		}
	}
	start[0], start[1] = median(sides[0]), median(sides[1])
	if start[0] <= 0 || start[1] <= 0 {
		return start, 0, benchprofile.Invalid("timing_missing")
	}
	noise = max(start[0]-start[1], start[1]-start[0])
	if noise >= budgetNS {
		return start, noise, benchprofile.Invalid("noise")
	}
	return start, noise, nil
}

// timingOf returns the one timing record of one arm: a second record of the same
// arm is not a second measurement.
func timingOf(d *runData, arm string) (timingRecord, bool) {
	var found timingRecord
	n := 0
	for _, rec := range d.timings {
		if rec.Arm == arm {
			found = rec
			n++
		}
	}
	return found, n == 1
}

// checkWriters requires both arms of every run to carry a complete, attributed
// writer series: every scheduled tick accounted for, none open at the end, at
// least 200 of them counted inside the producer interval, and the summaries the
// raw samples actually produce.
func checkWriters(set []*runData) error {
	for _, d := range set {
		for _, arm := range []string{armA1, armA2} {
			rec, ok := writerOf(d, arm)
			if !ok {
				return benchprofile.Invalid("writer_missing")
			}
			if rec.Run != d.run || rec.Order != d.header.Order || rec.Procs != d.header.Procs || rec.Digest != d.header.Digest {
				return benchprofile.Invalid("writer_attribution")
			}
			switch {
			case rec.Open:
				return benchprofile.Invalid("writer_open")
			case rec.Scheduled != writerScheduled || rec.Skipped+rec.InLoad != rec.Scheduled:
				return benchprofile.Invalid("writer_short")
			case rec.InLoad < minInLoad:
				return benchprofile.Invalid("writer_short")
			case len(rec.StartsNS) != rec.InLoad || len(rec.WaitRaw) != rec.InLoad || len(rec.CommitRaw) != rec.InLoad:
				return benchprofile.Invalid("writer_raw")
			case rec.FirstNS <= 0 || rec.LastNS <= rec.FirstNS:
				return benchprofile.Invalid("writer_interval")
			}
			if !slices.IsSorted(rec.StartsNS) {
				return benchprofile.Invalid("writer_interval")
			}
			for _, at := range rec.StartsNS {
				if at < rec.FirstNS || at > rec.LastNS {
					return benchprofile.Invalid("writer_interval")
				}
			}
			if err := checkSummary(rec.WaitRaw, rec.WaitP50, rec.WaitP95, rec.WaitP99); err != nil {
				return err
			}
			if err := checkSummary(rec.CommitRaw, rec.CommitP50, rec.CommitP95, rec.CommitP99); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSummary recomputes one metric's percentiles from its raw samples: a
// summary without raw samples, or one the samples do not produce, is not a
// measurement.
func checkSummary(raw []int64, p50, p95, p99 int64) error {
	s := storebench.Summarize(slices.Clone(raw))
	if s.P50 != p50 || s.P95 != p95 || s.P99 != p99 {
		return benchprofile.Invalid("writer_summary")
	}
	return nil
}

func writerOf(d *runData, arm string) (writerRecord, bool) {
	var found writerRecord
	n := 0
	for _, rec := range d.writers {
		if rec.Arm == arm {
			found = rec
			n++
		}
	}
	return found, n == 1
}

// checkRetention requires the recorded saturation of the position that measures
// memory, and nothing of the positions that do not: the phases in their order,
// the counts the protocol pins, and a resident child that was actually reaped.
func checkRetention(set []*runData) error {
	for i, d := range set {
		r := d.header.Retention
		if i != seriesRuns-1 {
			if r.Present {
				return benchprofile.Invalid("retention_unexpected")
			}
			continue
		}
		switch {
		case !r.Present:
			return benchprofile.Invalid("retention_missing")
		case r.HeldReads != retentionReads || r.Released != retentionReads:
			return benchprofile.Invalid("retention_reads")
		case r.QueueFull != "queue_full" || r.QueueRequests != 0:
			return benchprofile.Invalid("retention_queue")
		case r.BodyBytes != retentionBodies || r.DecodedBytes != retentionDecoded:
			return benchprofile.Invalid("retention_decode")
		case !benchprofile.IsLowerHex(r.DecodedSHA, benchprofile.HexSHA256Len) || r.DecodedSHA != r.ServedSHA:
			return benchprofile.Invalid("retention_digest")
		case r.ProbeSuccesses != retentionProbes-1 || r.ProbeBudget != "budget_exhausted":
			return benchprofile.Invalid("retention_probes")
		case r.ProbeLimit != "budget_keys" || r.ProbeMax != retentionKeyMax:
			return benchprofile.Invalid("retention_budget")
		case r.KeysAdded != 2+2*(retentionProbes-1):
			return benchprofile.Invalid("retention_keys")
		case r.ProbeRequests != retentionProbes:
			return benchprofile.Invalid("retention_requests")
		case r.Repeat != "ok":
			return benchprofile.Invalid("retention_repeat")
		case r.HostsVerified < retentionReads+r.ProbeRequests ||
			r.HostsVerified > retentionReads+r.ProbeRequests+retentionRetryAllowance:
			return benchprofile.Invalid("retention_hosts")
		case r.ChildRSS < minChildRSS:
			return benchprofile.Invalid("child_resident")
		case r.ChildSlots < 1 || r.ChildSlots > retentionReads:
			return benchprofile.Invalid("child_slots")
		case !r.ChildReaped:
			return benchprofile.Invalid("child_unreaped")
		}
	}
	return nil
}

// correctnessFailure is one run's correctness answer: the first violated clause,
// or "" when every clause held. A missing container observation is not a
// violation: it is refused earlier as INVALID.
func correctnessFailure(d *runData) string {
	h := d.header
	if h.PeakMax != memoryMaxBytes || h.Peak <= 0 {
		return ""
	}
	switch {
	case h.Streams.Healthy != healthyStreams:
		return "streams"
	// The slow streams are opened as the load's slow consumers. Their bounded
	// delivery path (overflow resync or the frame deadline's disconnect) is
	// proved causally by the gated-peer T14 test, not by these samples: a host
	// whose TCP send buffer absorbs the whole 60 s stream never fills the
	// 256-frame hub queue, so requiring a close here would measure the kernel,
	// not the product. Owner-authorized protocol amendment 2026-10-30 (DEC-060).
	case h.Streams.Slow != slowStreams:
		return "slow_streams"
	case h.Frames < 1 || h.Gaps != 0:
		return "stream_gaps"
	case h.Background > max(1, h.Procs/2):
		return "background"
	case h.Purge <= 0:
		return "purge_progress"
	case h.OOMKill != 0:
		return "oom_kill"
	case h.Peak >= h.PeakMax:
		return "memory_peak"
	}
	return ""
}

// memoryObservation refuses a run that did not read its container's own memory
// facts: an absent observation is INVALID, never a zero that reads as healthy.
func memoryObservation(d *runData) error {
	h := d.header
	if h.PeakMax != memoryMaxBytes {
		return benchprofile.Invalid("memory_limit")
	}
	if h.Peak <= 0 {
		return benchprofile.Invalid("memory_unobserved")
	}
	return nil
}

// writerBaseline is the descriptive first baseline: each arm's median of the
// five timing runs per metric and percentile, and the A/A spread of the two arm
// medians. It carries no budget and decides nothing.
func writerBaseline(set []*runData) (baseline [2][6]int64, spread [6]int64) {
	var values [2][6][]int64
	for i := range timingRuns {
		for arm, name := range [2]string{armA1, armA2} {
			rec, ok := writerOf(set[i], name)
			if !ok {
				continue
			}
			values[arm][0] = append(values[arm][0], rec.CommitP50)
			values[arm][1] = append(values[arm][1], rec.CommitP95)
			values[arm][2] = append(values[arm][2], rec.CommitP99)
			values[arm][3] = append(values[arm][3], rec.WaitP50)
			values[arm][4] = append(values[arm][4], rec.WaitP95)
			values[arm][5] = append(values[arm][5], rec.WaitP99)
		}
	}
	for arm := range 2 {
		for m := range 6 {
			baseline[arm][m] = median(values[arm][m])
		}
	}
	for m := range 6 {
		lo, hi := min(baseline[0][m], baseline[1][m]), max(baseline[0][m], baseline[1][m])
		if lo > 0 {
			spread[m] = 100 * (hi - lo) / lo
		}
	}
	return baseline, spread
}

// writerMetrics are the recorded metric names, in the order the lines print.
var writerMetrics = [6]string{"commit_p50_ns", "commit_p95_ns", "commit_p99_ns", "wait_p50_ns", "wait_p95_ns", "wait_p99_ns"}

// printBaselines prints the descriptive writer lines of an otherwise eligible
// series: never a budget, never a verdict on the writer.
func printBaselines(w io.Writer, v verdicts) {
	for m, metric := range writerMetrics {
		for arm, name := range [2]string{armA1, armA2} {
			say(w, "EFFECTS-WRITER-BASELINE arm=%s metric=%s median=%d\n", name, metric, v.baseline[arm][m])
		}
		say(w, "EFFECTS-WRITER-SPREAD metric=%s spread_pct=%d\n", metric, v.spread[m])
	}
}

// median is one side's median of its per-run statistics.
func median(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	s := slices.Clone(values)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func printInvalid(w io.Writer, err error) int {
	say(w, "EFFECTS-BENCH-INVALID reason=%s\n", reasonOf(err))
	return exitInvalid
}

// say writes one line of the tool's own output; a failed write to stdout or
// stderr has nowhere to be reported.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
