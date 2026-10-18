// Command parserbench measures the production per-call latency of
// parse.ParseTitle over every enabled title case of both corpora, and judges a
// recorded run against the accepted budgets.
//
//	parserbench [-rounds 30 -seed 1] -out DIR       measure: writes DIR/samples.jsonl only
//	parserbench -judge RUN_DIR [-base RUN_DIR]      judge: exit 0 PASS, 1 FAIL, 3 INVALID
//
// Measuring is diagnostic. Only -judge, which reads the recorded samples and the
// three host evidence files of the run, may print PARSER-LATENCY-PASS.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse"
	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

const (
	exitPass    = 0
	exitFail    = 1
	exitUsage   = 2
	exitInvalid = 3

	p50BudgetNS       = 100_000
	p99BudgetNS       = 1_000_000
	regressionPercent = 10

	defaultRounds   = 30
	maxRounds       = 100
	samplesName     = "samples.jsonl"
	samplesSchema   = 1
	samplesKind     = "parser-latency-samples"
	maxSamplesBytes = corpus.MaxFileBytes
	maxSampleLine   = 4 << 20
	maxItems        = 1 << 17
	maxSampleNS     = 1_000_000_000_000
	inputsManifest  = "/usr/local/share/boxset/inputs.sha256"
	maxInputsBytes  = 8 << 20

	outcomeOK           = "ok"
	outcomeUnrecognized = "unrecognized"
	outcomeRejected     = "rejected"
	titleUnrecognized   = "title_unrecognized"
)

// header is the first record of samples.jsonl: the run's provenance.
type header struct {
	Schema       int    `json:"schema"`
	Kind         string `json:"kind"`
	Run          string `json:"run"`
	InputsSHA    string `json:"inputs_sha256"`
	CorpusSeries string `json:"corpus_series_sha256"`
	CorpusMovie  string `json:"corpus_movie_sha256"`
	Lock         string `json:"lock_sha256"`
	Source       string `json:"source_sha256"`
	Names        string `json:"names_sha256"`
	Go           string `json:"go"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
	CGO          string `json:"cgo"`
	Race         string `json:"race"`
	UTCOffset    int    `json:"utc_offset_sec"`
	Cases        int    `json:"cases"`
	Items        int    `json:"items"`
	Rounds       int    `json:"rounds"`
	Seed         int64  `json:"seed"`
	OK           int    `json:"ok"`
	Unrecognized int    `json:"unrecognized"`
	Rejected     int    `json:"rejected"`
	CompileNS    int64  `json:"compile_ns"`
	StartDate    string `json:"start_date"`
	EndDate      string `json:"end_date"`
}

// item is one materialized title input: the enabled title case it belongs to and
// the input's position in that case. Out is the outcome class of the discarded
// warmup call, never of a timed one.
type item struct {
	App  string `json:"app"`
	ID   string `json:"id"`
	N    int    `json:"n"`
	Out  string `json:"o"`
	name string
	kind parse.Kind
}

// round is one shuffled pass: Order lists the item indexes in call order and NS
// the wall-clock nanoseconds of each full ParseTitle call, aligned with Order.
type round struct {
	R     int     `json:"r"`
	Order []int   `json:"order"`
	NS    []int64 `json:"ns"`
}

// sampleSet is the content of one samples.jsonl.
type sampleSet struct {
	Header header
	Items  []item
	Rounds []round
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("parserbench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rounds := fs.Int("rounds", defaultRounds, "shuffled rounds after the discarded warmup")
	seed := fs.Int64("seed", 1, "shuffle seed")
	out := fs.String("out", "", "measure: directory that receives samples.jsonl")
	judgeDir := fs.String("judge", "", "judge: recorded run directory")
	base := fs.String("base", "", "judge: earlier recorded run directory to compare against")
	root := fs.String("root", ".", "repository root")
	inputs := fs.String("inputs", inputsManifest, "measure: image input manifest to hash into the samples (the judge always reads the image's own)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || (*out == "") == (*judgeDir == "") || (*base != "" && *judgeDir == "") || *rounds < 1 || *rounds > maxRounds {
		say(stderr, "usage: parserbench [-rounds N] [-seed S] -out DIR | -judge RUN_DIR [-base RUN_DIR] [-root DIR]\n")
		return exitUsage
	}
	if *judgeDir != "" {
		return judge(*root, *judgeDir, *base, inputsManifest, stdout)
	}
	return measureTo(*root, *inputs, *out, *rounds, *seed, stdout, stderr)
}

// ---- measure ---------------------------------------------------------------------

func measureTo(root, inputs, out string, rounds int, seed int64, stdout, stderr io.Writer) int {
	abs, err := filepath.Abs(out)
	if err != nil {
		say(stderr, "parserbench: out_invalid\n")
		return exitFail
	}
	set, err := measure(root, inputs, filepath.Base(filepath.Dir(abs)), rounds, seed, newProductionParser, time.Now)
	if err != nil {
		say(stderr, "parserbench: %v\n", err)
		return exitFail
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		say(stderr, "parserbench: write_failed file=%s\n", samplesName)
		return exitFail
	}
	if err := writeSamples(filepath.Join(out, samplesName), set); err != nil {
		say(stderr, "parserbench: write_failed file=%s\n", samplesName)
		return exitFail
	}
	st := pooledStats(set)
	say(stdout, "parserbench: samples=%d items=%d cases=%d rounds=%d compile_ns=%d p50_ns=%d p99_ns=%d (diagnostic only; judge decides)\n",
		st.n, set.Header.Items, set.Header.Cases, set.Header.Rounds, set.Header.CompileNS, st.p50, st.p99)
	return exitPass
}

// buildSetting is a recorded go build setting, or "" when the binary has none.
func buildSetting(key string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// parseCall is one full production title parse.
type parseCall func(ctx context.Context, name string, kind parse.Kind) (parse.ParsedTitle, error)

// newProductionParser constructs the production parser, compiling its fixed pattern tables once.
func newProductionParser() (parseCall, error) {
	p, err := parse.NewParser(parse.Options{})
	if err != nil {
		return nil, err
	}
	return p.ParseTitle, nil
}

// measure runs the warmup and the shuffled rounds over every enabled title input.
// A fixed token error means no usable measurement. Construction (compilation) is
// timed and reported on its own; the one discarded warmup pass classifies each
// input; every timed call must then reach the same outcome class as its warmup.
func measure(root, inputs, run string, rounds int, seed int64, build func() (parseCall, error), now func() time.Time) (*sampleSet, error) {
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return nil, errors.New("inputs_unavailable")
	}
	ref, err := loadReference(root)
	if err != nil {
		return nil, errors.New("corpus_invalid")
	}
	start := now()
	items, cases, err := ref.items(corpus.TimeContext{Now: start, Location: time.Local})
	if err != nil {
		return nil, errors.New("corpus_invalid")
	}
	compiled := now()
	call, err := build()
	if err != nil {
		return nil, errors.New("parser_invalid")
	}
	compileNS := max(now().Sub(compiled).Nanoseconds(), 1)
	ctx := context.Background()

	h := header{
		Schema: samplesSchema, Kind: samplesKind, Run: run, InputsSHA: sha256Hex(manifest),
		CorpusSeries: ref.seriesSHA, CorpusMovie: ref.movieSHA, Lock: ref.lockSHA, Source: ref.sourceSHA,
		Names: namesHash(items), Go: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		CGO: orDefault(buildSetting("CGO_ENABLED"), "unknown"), Race: orDefault(buildSetting("-race"), "false"),
		Cases: cases, Items: len(items), Rounds: rounds, Seed: seed, CompileNS: compileNS,
		StartDate: start.Format(factsDateLayout),
	}
	_, h.UTCOffset = start.Zone()

	// One discarded warmup pass in corpus order; it also classifies each input.
	for i := range items {
		_, err := call(ctx, items[i].name, items[i].kind)
		out, cerr := classify(err)
		if cerr != nil {
			return nil, cerr
		}
		items[i].Out = out
		switch out {
		case outcomeOK:
			h.OK++
		case outcomeUnrecognized:
			h.Unrecognized++
		default:
			h.Rejected++
		}
	}

	set := &sampleSet{Header: h, Items: items, Rounds: make([]round, 0, rounds)}
	for r := 1; r <= rounds; r++ {
		order := rand.New(rand.NewPCG(uint64(seed), uint64(r))).Perm(len(items))
		ns := make([]int64, len(order))
		for k, idx := range order {
			it := &items[idx]
			t0 := now()
			_, err := call(ctx, it.name, it.kind)
			ns[k] = max(now().Sub(t0).Nanoseconds(), 1)
			if out, cerr := classify(err); cerr != nil || out != it.Out {
				return nil, errors.New("outcome_changed")
			}
		}
		set.Rounds = append(set.Rounds, round{R: r, Order: order, NS: ns})
	}
	set.Header.EndDate = now().Format(factsDateLayout)
	return set, nil
}

// classify maps a ParseTitle result to its outcome class. A rejection by the
// parser's own reasons is a legitimate production outcome; anything else is not.
func classify(err error) (string, error) {
	if err == nil {
		return outcomeOK, nil
	}
	var re *parse.ReasonError
	if !errors.As(err, &re) {
		return "", errors.New("parse_failed")
	}
	if re.Reason.Code == titleUnrecognized {
		return outcomeUnrecognized, nil
	}
	return outcomeRejected, nil
}

// writeSamples writes samples.jsonl in place: the file is opened with truncate
// (never exclusive create, a temporary name or a rename) and every write, the
// flush and the close are checked.
func writeSamples(path string, set *sampleSet) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	werr := enc.Encode(set.Header)
	for i := 0; werr == nil && i < len(set.Items); i++ {
		werr = enc.Encode(set.Items[i])
	}
	for i := 0; werr == nil && i < len(set.Rounds); i++ {
		werr = enc.Encode(set.Rounds[i])
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

// ---- reference: the corpora, lock and source the samples must come from ------------

type reference struct {
	series, movie *corpus.Corpus
	seriesSHA     string
	movieSHA      string
	lockSHA       string
	sourceSHA     string
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// readBounded reads a regular, non-link file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, errors.New("file_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, errors.New("file_invalid")
	}
	return data, nil
}

// hashFiles is the SHA-256 over each named file as `name NUL size NUL bytes`.
func hashFiles(root string, names []string) (string, error) {
	h := sha256.New()
	for _, n := range names {
		data, err := readBounded(filepath.Join(root, filepath.FromSlash(n)), corpus.MaxFileBytes)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", n, len(data))
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sourceFiles lists the parser and corpus-reader sources a latency claim is about.
func sourceFiles(root string) ([]string, error) {
	var names []string
	for _, dir := range []string{"internal/parse", "internal/parse/corpus"} {
		ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if n := e.Name(); e.Type().IsRegular() && filepath.Ext(n) == ".go" && !strings.HasSuffix(n, "_test.go") {
				names = append(names, dir+"/"+n)
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("source_missing")
	}
	return names, nil
}

// loadReference reads both corpora through the canonical corpus reader and hashes
// the corpora, the module lock files and the parser sources under root.
func loadReference(root string) (*reference, error) {
	ref := &reference{}
	for _, app := range []string{"series", "movie"} {
		data, err := readBounded(filepath.Join(root, "testdata", "parser", app+".jsonl"), corpus.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		c, err := corpus.Read(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if app == "series" {
			ref.series, ref.seriesSHA = c, sha256Hex(data)
		} else {
			ref.movie, ref.movieSHA = c, sha256Hex(data)
		}
	}
	var err error
	if ref.lockSHA, err = hashFiles(root, []string{"go.mod", "go.sum"}); err != nil {
		return nil, err
	}
	names, err := sourceFiles(root)
	if err != nil {
		return nil, err
	}
	if ref.sourceSHA, err = hashFiles(root, names); err != nil {
		return nil, err
	}
	return ref, nil
}

// items materializes every input of every enabled title case, series first, in
// corpus order, through the shared reader and calendar materializer, and counts
// the enabled title cases.
func (r *reference) items(tc corpus.TimeContext) ([]item, int, error) {
	var items []item
	cases := 0
	for _, side := range []struct {
		app  string
		c    *corpus.Corpus
		kind parse.Kind
	}{{"series", r.series, parse.KindSeries}, {"movie", r.movie, parse.KindMovie}} {
		for _, cs := range side.c.Cases {
			if cs.Scope != corpus.ScopeTitle || !cs.UpstreamEnabled {
				continue
			}
			names, err := cs.Inputs(tc)
			if err != nil {
				return nil, 0, err
			}
			cases++
			for n, name := range names {
				items = append(items, item{App: side.app, ID: cs.ID, N: n, name: name, kind: side.kind})
			}
		}
	}
	if len(items) == 0 || len(items) > maxItems {
		return nil, 0, errors.New("items_invalid")
	}
	return items, cases, nil
}

// namesHash identifies the exact materialized names, in order.
func namesHash(items []item) string {
	h := sha256.New()
	for _, it := range items {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\n", it.App, it.ID, it.N, it.name)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- samples reader ------------------------------------------------------------------

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	switch {
	case err == nil:
		return line[:len(line)-1], nil
	case errors.Is(err, io.EOF) && len(line) == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF), errors.Is(err, bufio.ErrBufferFull):
		return nil, invalid("samples_malformed")
	}
	return nil, invalid("samples_unreadable")
}

func decodeLine(line []byte, into any) error {
	if err := requireKeys(line, jsonKeys(reflect.TypeOf(into).Elem()), "samples"); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalid("samples_malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("samples_malformed")
	}
	return nil
}

// noNullElements rejects a round whose arrays hold a null element, which would
// decode as the zero value (a call index 0 or a duration 0).
func noNullElements(line []byte) error {
	var arrs struct {
		Order []json.RawMessage `json:"order"`
		NS    []json.RawMessage `json:"ns"`
	}
	if err := json.Unmarshal(line, &arrs); err != nil {
		return invalid("samples_malformed")
	}
	for _, list := range [2][]json.RawMessage{arrs.Order, arrs.NS} {
		for _, v := range list {
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return invalid("samples_malformed")
			}
		}
	}
	return nil
}

func (h header) validShape() bool {
	return h.Schema == samplesSchema && h.Kind == samplesKind &&
		h.Items >= 1 && h.Items <= maxItems && h.Rounds >= 1 && h.Rounds <= maxRounds &&
		h.Cases >= 1 && h.Cases <= h.Items &&
		h.OK >= 0 && h.Unrecognized >= 0 && h.Rejected >= 0 && h.OK+h.Unrecognized+h.Rejected == h.Items &&
		isLowerHex(h.InputsSHA, hexSHA256Len) && isLowerHex(h.CorpusSeries, hexSHA256Len) &&
		isLowerHex(h.CorpusMovie, hexSHA256Len) && isLowerHex(h.Lock, hexSHA256Len) &&
		isLowerHex(h.Source, hexSHA256Len) && isLowerHex(h.Names, hexSHA256Len) &&
		h.Run != "" && len(h.Run) <= 64 && h.Go != "" && len(h.Go) <= 64 && h.CompileNS >= 1
}

func (it item) validShape() bool {
	return (it.App == "series" || it.App == "movie") && it.ID != "" && len(it.ID) <= 512 && it.N >= 0 &&
		(it.Out == outcomeOK || it.Out == outcomeUnrecognized || it.Out == outcomeRejected)
}

// readSamples reads and structurally validates the samples.jsonl file at path.
func readSamples(path string) (*sampleSet, error) {
	data, _, err := readRegular(path, maxSamplesBytes, "samples")
	if err != nil {
		return nil, err
	}
	return parseSamples(data)
}

// parseSamples structurally validates samples.jsonl bytes: a header, exactly Items
// items and exactly Rounds rounds, every required key present and not null, each
// round a permutation of the item indexes with a positive bounded duration for each.
func parseSamples(data []byte) (*sampleSet, error) {
	br := bufio.NewReaderSize(bytes.NewReader(data), maxSampleLine)

	set := &sampleSet{}
	line, err := readLine(br)
	if err != nil {
		return nil, malformedOr(err)
	}
	if err := decodeLine(line, &set.Header); err != nil {
		return nil, err
	}
	h := set.Header
	if !h.validShape() {
		return nil, invalid("samples_malformed")
	}
	set.Items = make([]item, 0, h.Items)
	for range h.Items {
		line, err := readLine(br)
		if err != nil {
			return nil, malformedOr(err)
		}
		var it item
		if err := decodeLine(line, &it); err != nil {
			return nil, err
		}
		if !it.validShape() {
			return nil, invalid("samples_malformed")
		}
		set.Items = append(set.Items, it)
	}
	seen := make([]bool, h.Items)
	for r := 1; r <= h.Rounds; r++ {
		line, err := readLine(br)
		if err != nil {
			return nil, malformedOr(err)
		}
		var rd round
		if err := decodeLine(line, &rd); err != nil {
			return nil, err
		}
		if err := noNullElements(line); err != nil {
			return nil, err
		}
		if rd.R != r || len(rd.Order) != h.Items || len(rd.NS) != h.Items {
			return nil, invalid("samples_malformed")
		}
		clear(seen)
		for k, idx := range rd.Order {
			if idx < 0 || idx >= h.Items || seen[idx] || rd.NS[k] < 1 || rd.NS[k] > maxSampleNS {
				return nil, invalid("samples_malformed")
			}
			seen[idx] = true
		}
		set.Rounds = append(set.Rounds, rd)
	}
	if _, err := readLine(br); !errors.Is(err, io.EOF) {
		return nil, invalid("samples_malformed")
	}
	return set, nil
}

// malformedOr maps an early end of the file to a malformed verdict.
func malformedOr(err error) error {
	if errors.Is(err, io.EOF) {
		return invalid("samples_malformed")
	}
	return err
}

// ---- verdict ---------------------------------------------------------------------------

type stats struct {
	p50, p99 int64
	n        int
}

// nearestRank is the nearest-rank percentile of an ascending slice.
func nearestRank(sorted []int64, p int) int64 {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// pooledStats pools every round's samples; the warmup is not among them.
func pooledStats(s *sampleSet) stats {
	all := make([]int64, 0, len(s.Rounds)*len(s.Items))
	for _, r := range s.Rounds {
		all = append(all, r.NS...)
	}
	slices.Sort(all)
	return stats{p50: nearestRank(all, 50), p99: nearestRank(all, 99), n: len(all)}
}

// verdictOf applies the absolute budgets and, when a comparable base exists,
// the 10% regression bound; the first breach names the reason. Percentiles only:
// no mean stands in for the 99th.
func verdictOf(head stats, base *stats) (code int, reason string) {
	switch {
	case head.p50 > p50BudgetNS:
		return exitFail, "p50_budget"
	case head.p99 > p99BudgetNS:
		return exitFail, "p99_budget"
	case base != nil && head.p50*100 > base.p50*(100+regressionPercent):
		return exitFail, "p50_regression"
	case base != nil && head.p99*100 > base.p99*(100+regressionPercent):
		return exitFail, "p99_regression"
	}
	return exitPass, ""
}

// checkSamples checks that samples come from the run, the image and the sources
// being judged, on one UTC date, in a clean production build. A base run (head
// false) is held to the same provenance and build identity, except that it may
// have been measured from other lock and parser sources, and from other corpora
// (which then make it incomparable rather than invalid).
func checkSamples(s *sampleSet, recs [3]Facts, run string, ref *reference, head bool) error {
	h := s.Header
	date := factsUTCDate(recs[0].StartUnix)
	switch {
	case h.Run != run:
		return invalid("run_mismatch")
	case h.InputsSHA != recs[0].Inputs:
		return invalid("inputs_mismatch")
	case h.Rounds != defaultRounds:
		return invalid("rounds")
	case h.GOOS != "linux" || h.GOARCH != "amd64":
		return invalid("platform")
	case h.CGO != "0" || h.Race != "false":
		return invalid("build")
	case h.UTCOffset != 0 || h.StartDate != h.EndDate || h.StartDate != date || date != factsUTCDate(recs[2].EndUnix):
		return invalid("date_rollover")
	case head && (h.CorpusSeries != ref.seriesSHA || h.CorpusMovie != ref.movieSHA):
		return invalid("corpus_changed")
	case head && h.Lock != ref.lockSHA:
		return invalid("lock_changed")
	case head && h.Source != ref.sourceSHA:
		return invalid("source_changed")
	}
	day, err := time.Parse(factsDateLayout, h.StartDate)
	if err != nil {
		return invalid("date_rollover")
	}
	items, cases, err := ref.items(corpus.TimeContext{Now: day.Add(12 * time.Hour), Location: time.UTC})
	if err != nil || namesHash(items) != h.Names || h.Cases != cases || len(items) != len(s.Items) {
		return invalid("names_changed")
	}
	for i, it := range items {
		if s.Items[i].App != it.App || s.Items[i].ID != it.ID || s.Items[i].N != it.N {
			return invalid("names_changed")
		}
	}
	return nil
}

// judgeEnv is where the judge finds what it judges. The run directories are the
// immutable image's own: imageRoot is the image's /src, whose manifest lists every
// file it holds with its SHA-256, and uid the owner those files must have (root).
// root locates the corpora, lock files and sources the samples must come from.
type judgeEnv struct {
	root      string
	imageRoot string
	manifest  []byte
	uid       uint32
	ref       *reference
}

const imageSrc = "/src"

func (e *judgeEnv) reference() (*reference, error) {
	if e.ref == nil {
		ref, err := loadReference(e.root)
		if err != nil {
			return nil, err
		}
		e.ref = ref
	}
	return e.ref, nil
}

func (e *judgeEnv) runPath(run string) string {
	return filepath.Join(e.imageRoot, "docs", "spikes", "data", run)
}

// locate maps a run directory argument to its run name: it must be exactly
// docs/spikes/data/bench-<UTC> of the image, nothing else.
func (e *judgeEnv) locate(runDir string) (string, error) {
	abs, err := filepath.Abs(runDir)
	if err != nil {
		return "", invalid("run_location")
	}
	run := filepath.Base(abs)
	if !validRunName(run) || abs != e.runPath(run) {
		return "", invalid("run_location")
	}
	return run, nil
}

// immutable reports whether fi is owned by uid and cannot be written by anyone
// else: no write bit at all, except that a directory that is not itself a record
// parent (strict false) may be sticky, which keeps another user from replacing or
// unlinking an entry it does not own.
func immutable(fi os.FileInfo, uid uint32, strict bool) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uid {
		return false
	}
	if fi.Mode().Perm()&0o222 == 0 {
		return true
	}
	return fi.IsDir() && !strict && fi.Mode()&os.ModeSticky != 0
}

// checkParents requires every directory that could be used to replace the run's
// files (the run directory, its samples directory and every ancestor up to the
// image root) to be a real, root-owned, unwritable or sticky directory.
func (e *judgeEnv) checkParents(run string) error {
	rp := e.runPath(run)
	data := filepath.Dir(rp)
	spikes := filepath.Dir(data)
	docs := filepath.Dir(spikes)
	for _, d := range []struct {
		path             string
		strict, optional bool
	}{{rp, true, false}, {filepath.Join(rp, "samples"), true, true}, {data, false, false}, {spikes, false, false}, {docs, false, false}, {e.imageRoot, false, false}} {
		fi, err := os.Lstat(d.path)
		switch {
		case errors.Is(err, os.ErrNotExist) && d.optional:
			continue
		case errors.Is(err, os.ErrNotExist):
			return invalid("record_missing")
		case err != nil:
			return invalid("record_unreadable")
		case !fi.IsDir():
			return invalid("run_location")
		case !immutable(fi, e.uid, d.strict):
			return invalid("record_mutable")
		}
	}
	return nil
}

// manifestHas reports whether the image manifest lists path exactly once, with the
// SHA-256 of data (sha256sum format: 64 hex digits, two spaces, the path).
func manifestHas(manifest []byte, path string, data []byte) bool {
	want := sha256Hex(data)
	n := 0
	for line := range bytes.Lines(manifest) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > 66 && line[64] == ' ' && line[65] == ' ' && string(line[66:]) == path {
			if string(line[:64]) != want {
				return false
			}
			n++
		}
	}
	return n == 1
}

// readImported reads one record of the image: a regular file that cannot be
// written by anyone else and whose bytes are the ones the image's manifest lists
// for its place in /src. A look-alike made outside the image is not.
func (e *judgeEnv) readImported(path string, max int64, what string) ([]byte, error) {
	data, fi, err := readRegular(path, max, what)
	if err != nil {
		return nil, err
	}
	if !immutable(fi, e.uid, true) {
		return nil, invalid("record_mutable")
	}
	rel, err := filepath.Rel(e.imageRoot, path)
	if err != nil || !manifestHas(e.manifest, imageSrc+"/"+filepath.ToSlash(rel), data) {
		return nil, invalid("image_mismatch")
	}
	return data, nil
}

// loadRun reads one run of the image (its three records and its samples), applies
// the eligibility rules and the structural samples checks.
func (e *judgeEnv) loadRun(run string) (*sampleSet, [3]Facts, error) {
	if err := e.checkParents(run); err != nil {
		return nil, [3]Facts{}, err
	}
	dir := e.runPath(run)
	var datas [3][]byte
	for i, stage := range factStages {
		data, err := e.readImported(filepath.Join(dir, stage+factsFileNameSuffix), maxFactBytes, "record")
		if err != nil {
			return nil, [3]Facts{}, err
		}
		datas[i] = data
	}
	recs, err := decodeFactSet(datas)
	if err != nil {
		return nil, recs, err
	}
	if err := checkEligibility(run, recs); err != nil {
		return nil, recs, err
	}
	data, err := e.readImported(filepath.Join(dir, "samples", samplesName), maxSamplesBytes, "samples")
	if err != nil {
		return nil, recs, err
	}
	set, err := parseSamples(data)
	return set, recs, err
}

// sameItems reports whether a and b are the same case inputs in the same order.
func sameItems(a, b []item) bool {
	return slices.EqualFunc(a, b, func(x, y item) bool { return x.App == y.App && x.ID == y.ID && x.N == y.N })
}

// comparable reports whether base measured the same corpora and the same case
// inputs on the same platform with the same round count as head. Other lock or
// source hashes, dates and names (symbolic dates move) do not matter.
func comparable(head, base *sampleSet) bool {
	h, b := head.Header, base.Header
	return h.CorpusSeries == b.CorpusSeries && h.CorpusMovie == b.CorpusMovie && sameItems(head.Items, base.Items) &&
		h.Rounds == b.Rounds && h.GOOS == b.GOOS && h.GOARCH == b.GOARCH
}

// judge is the only place that may print PARSER-LATENCY-PASS. INVALID (exit 3)
// beats FAIL (exit 1): a budget verdict on evidence that is not eligible is
// never produced.
func judge(root, runDir, baseDir, inputs string, w io.Writer) int {
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return printInvalid(w, invalid("image_manifest"))
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return printInvalid(w, invalid("run_location"))
	}
	return judgeWith(&judgeEnv{root: root, imageRoot: abs, manifest: manifest}, runDir, baseDir, w)
}

func judgeWith(env *judgeEnv, runDir, baseDir string, w io.Writer) int {
	run, err := env.locate(runDir)
	if err != nil {
		return printInvalid(w, err)
	}
	head, recs, err := env.loadRun(run)
	if err != nil {
		return printInvalid(w, err)
	}
	ref, err := env.reference()
	if err != nil {
		return printInvalid(w, invalid("reference_invalid"))
	}
	if err := checkSamples(head, recs, run, ref, true); err != nil {
		return printInvalid(w, err)
	}
	hs := pooledStats(head)
	var base *stats
	baseNote := "none"
	if baseDir != "" {
		baseNote = "incomparable"
		if bs, ok := env.loadBase(baseDir, ref); ok && comparable(head, bs) {
			st := pooledStats(bs)
			base, baseNote = &st, "compared"
		}
	}
	code, reason := verdictOf(hs, base)
	if code != exitPass {
		say(w, "PARSER-LATENCY-FAIL reason=%s p50_ns=%d p99_ns=%d budget_p50_ns=%d budget_p99_ns=%d base=%s\n",
			reason, hs.p50, hs.p99, p50BudgetNS, p99BudgetNS, baseNote)
		return code
	}
	say(w, "PARSER-LATENCY-PASS eligible=VALID p50_ns=%d p99_ns=%d samples=%d rounds=%d base=%s\n",
		hs.p50, hs.p99, hs.n, head.Header.Rounds, baseNote)
	return exitPass
}

// loadBase loads the comparison run: it must itself be a complete eligible run of
// the image with the same provenance and build identity as a head, or it is not a
// base at all.
func (e *judgeEnv) loadBase(baseDir string, ref *reference) (*sampleSet, bool) {
	run, err := e.locate(baseDir)
	if err != nil {
		return nil, false
	}
	set, recs, err := e.loadRun(run)
	if err != nil || checkSamples(set, recs, run, ref, false) != nil {
		return nil, false
	}
	return set, true
}

func printInvalid(w io.Writer, err error) int {
	reason, ok := invalidReason(err)
	if !ok {
		reason = "unreadable"
	}
	say(w, "PARSER-LATENCY-INVALID reason=%s\n", reason)
	return exitInvalid
}

// say writes one line of the tool's own output; a failed write to stdout or
// stderr has nowhere to be reported.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
