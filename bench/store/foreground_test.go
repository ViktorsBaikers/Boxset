//go:build !race

package storebench

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The series measures real writes, reads and commits on the wall clock, so it
// is excluded under -race with the other timing tests; the predicate checks
// stay race-eligible in predicate_test.go.

const (
	foregroundEnv = "BOXSET_STORE_FOREGROUND"
	baselineFile  = "baseline.json"
	compareFile   = "compare.json"

	modeRecord  = "record"  // measure and write the baseline
	modeCompare = "compare" // measure and capture the later run
	modeJudge   = "judge"   // measure nothing: judge the captured run

	warmProbes     = 1200
	rounds         = 5    // consecutive windows; each series pools every sample of all of them
	roundProbes    = 4000 // probes per window
	measuredProbes = rounds * roundProbes
	seed           = 20261019
	probeBytes     = 16 // one int64 key and one int64 value per probe write
)

// spec is one workload. Its shape string is part of the recorded identity, so
// a later run must repeat every number here.
type spec struct {
	name     string
	titles   int           // series titles, two slots each
	episodes int           // episodes of every title except the first
	hot      int           // episodes of the first title; 0 means episodes
	writers  int           // goroutines mutating through the library
	readers  int           // goroutines reading through the library
	lifetime time.Duration // pooled connection lifetime: churns physical connections
	skewed   bool          // mutations pile on the first title's low episodes
}

var workloads = []spec{
	{name: WorkloadChurn, titles: 16, episodes: 50, writers: 4, readers: 4, lifetime: 20 * time.Millisecond},
	{name: WorkloadSkew, titles: 8, episodes: 20, hot: 1500, writers: 4, readers: 4, skewed: true},
}

func (s spec) shape() string {
	return fmt.Sprintf("titles=%d episodes=%d hot=%d writers=%d readers=%d lifetime=%s skewed=%t warm=%d rounds=%d probes_per_round=%d seed=%d %s",
		s.titles, s.episodes, s.hot, s.writers, s.readers, s.lifetime, s.skewed, warmProbes, rounds, roundProbes, seed, AggregateShape)
}

// The plan queries are the library's statements; the library keeps its own
// copy, so these are the same text, not a shared symbol.
const (
	qRollups        = `SELECT title_id, slot_id, episodes, monitored, on_disk, missing, size_bytes, next_air_utc, spine, spine_full, spine_omitted, revision FROM title_rollup WHERE title_id = ? ORDER BY slot_id`
	qRollupRow      = `SELECT r.episodes, r.monitored, r.on_disk, r.missing, r.size_bytes, r.next_air_utc, r.spine, r.spine_full, r.spine_omitted, t.kind, s.monitored, s.media_state FROM title_rollup r JOIN slot s ON s.id = r.slot_id JOIN title t ON t.id = r.title_id WHERE r.title_id = ? AND r.slot_id = ?`
	qSlotEpisodeRow = `SELECT se.title_id, se.episode_id, se.monitored, se.state, se.size_bytes FROM slot s JOIN episode e ON e.title_id = s.title_id JOIN slot_episode se ON se.slot_id = s.id AND se.episode_id = e.id WHERE s.id = ? AND e.season = ? AND e.number = ?`
	qSlotEpisodeSet = `UPDATE slot_episode SET monitored = ?, state = ?, size_bytes = ? WHERE slot_id = ? AND episode_id = ?`
	qSearch         = `SELECT -rowid FROM title_search WHERE title_search MATCH ?1 AND rowid < 0 UNION SELECT a.title_id FROM title_search JOIN title_alias a ON a.id = title_search.rowid LEFT JOIN alias_generation g ON g.id = a.generation_id WHERE title_search MATCH ?1 AND title_search.rowid > 0 AND (a.generation_id IS NULL OR g.state = 'active') ORDER BY 1 LIMIT ?2`
)

type target struct {
	id       library.TitleID
	name     string
	slots    []library.SlotID
	episodes int
}

type world struct {
	spec    spec
	db      *store.DB
	lib     *library.Library
	titles  []target
	counter atomic.Int64 // makes every mutation store a value no earlier one did
}

func epKey(ord int) (season, number int) { return (ord-1)/50 + 1, (ord-1)%50 + 1 }

func (w *world) seedTitles(ctx context.Context) error {
	id, err := w.lib.AddLibrary(ctx, library.LibrarySpec{
		Name:     "foreground",
		Defaults: library.LibraryDefaults{Monitored: true, NamingLanguage: "en", SearchLanguage: "en", Profile: "hd"},
		Template: []library.TemplateSlot{{Key: "hd", Primary: true}, {Key: "uhd"}},
	})
	if err != nil {
		return err
	}
	for i := range w.spec.titles {
		n := w.spec.episodes
		if i == 0 && w.spec.hot > 0 {
			n = w.spec.hot
		}
		name := fmt.Sprintf("%s title %04d", w.spec.name, i)
		r, err := w.lib.AddMembership(ctx, library.MembershipRequest{
			LibraryID: id, MetadataID: fmt.Sprintf("tvdb:%s-%d", w.spec.name, i), Kind: "series", Name: name,
		})
		if err != nil {
			return err
		}
		eps := make([]library.EpisodeSpec, n)
		for j := range eps {
			eps[j].Season, eps[j].Number = epKey(j + 1)
			eps[j].AirPrecision = "unknown"
		}
		if err := w.lib.AddEpisodes(ctx, r.TitleID, eps); err != nil {
			return err
		}
		w.titles = append(w.titles, target{id: r.TitleID, name: name, slots: r.SlotIDs, episodes: n})
	}
	return CreateProbe(ctx, w.db)
}

var states = [...]string{"on_disk", "missing", "live", "upgrade", "unaired"}

// pick chooses the title and episode a mutation touches: uniform for churn;
// for the skewed workload nine in ten land on the first title, cubed toward
// its first episodes.
func (w *world) pick(r *rand.Rand) (t target, ord int) {
	if !w.spec.skewed {
		t = w.titles[r.IntN(len(w.titles))]
		return t, r.IntN(t.episodes) + 1
	}
	t = w.titles[0]
	if r.IntN(10) == 0 {
		t = w.titles[1+r.IntN(len(w.titles)-1)]
	}
	u := r.Float64()
	return t, int(u*u*u*float64(t.episodes)) + 1
}

// mutate does one library write and returns the logical bytes it carried.
func (w *world) mutate(ctx context.Context, r *rand.Rand) (int64, error) {
	t, ord := w.pick(r)
	c := w.counter.Add(1)
	op := 0
	if w.spec.skewed {
		op = r.IntN(100)
	}
	switch {
	case op < 70: // churn is all slot-episode state
		s, n := epKey(ord)
		state := states[c%int64(len(states))]
		err := w.lib.SetSlotEpisode(ctx, library.SlotEpisodeChange{
			SlotID: t.slots[r.IntN(len(t.slots))], Season: s, Number: n, Monitored: c%3 != 0, State: state, SizeBytes: c,
		})
		return int64(9 + len(state)), err
	case op < 80:
		s, n := epKey(ord)
		at := int64(1_700_000_000) + c
		err := w.lib.SetEpisodeAir(ctx, t.id, library.EpisodeSpec{Season: s, Number: n, AirUTC: &at, AirPrecision: "instant"})
		return 8 + int64(len("instant")), err
	case op < 90:
		name := fmt.Sprintf("%s r%d", t.name, c)
		return int64(len(name)), w.lib.RenameTitle(ctx, t.id, name)
	default:
		aliases := []string{fmt.Sprintf("%s alias %d-a", w.spec.name, c), fmt.Sprintf("%s alias %d-b", w.spec.name, c)}
		return int64(len(aliases[0]) + len(aliases[1])), w.lib.SetAliases(ctx, t.id, nil, aliases)
	}
}

// read does one indexed read through the library: a title's rollups, then a
// name search, alternating.
func (w *world) read(ctx context.Context, r *rand.Rand, i int) error {
	if i%2 == 0 {
		t, _ := w.pick(r)
		_, err := w.lib.Rollups(ctx, t.id)
		return err
	}
	_, err := w.lib.Search(ctx, w.spec.name+" title", 10)
	return err
}

// procField reads one integer of a /proc file whose lines are "key value" or "key: value [unit]".
func procField(path, key string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.TrimSuffix(f[0], ":") == key {
			return strconv.ParseInt(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s has no %s", path, key)
}

// proc reads the Linux prerequisites; the series needs every one of them.
type proc struct{ t *testing.T }

func (p proc) field(path, key string) int64 {
	p.t.Helper()
	if runtime.GOOS != "linux" {
		return 0 // Host.OS makes the run INVALID
	}
	v, err := procField(path, key)
	if err != nil {
		p.t.Fatalf("STORE-FOREGROUND-INVALID reason=proc_unreadable: %v", err)
	}
	return v
}

func (p proc) swapPages() (in, out int64) {
	return p.field("/proc/vmstat", "pswpin"), p.field("/proc/vmstat", "pswpout")
}

func (p proc) status(key string) int64 { return p.field("/proc/self/status", key) }
func (p proc) written() int64          { return p.field("/proc/self/io", "wchar") }

// memory reads the process's total memory now: the peak and resident set of
// the process and what the Go runtime holds.
func (p proc) memory() Memory {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Memory{VmHWMKiB: p.status("VmHWM"), VmRSSKiB: p.status("VmRSS"), GoSysBytes: int64(ms.Sys), GoHeapInuseBytes: int64(ms.HeapInuse)}
}

func plan(ctx context.Context, db *store.DB, query string, args ...any) ([]string, error) {
	var lines []string
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			lines = append(lines, detail)
		}
		return rows.Err()
	})
	return lines, err
}

// checkpoint asks for a passive checkpoint from a separate connection and
// returns what it reports: busy, frames in the log, frames checkpointed.
func checkpoint(ctx context.Context, path string) (busy, logFrames, done int64, err error) {
	c, err := sqlite.NewConnector(path)
	if err != nil {
		return 0, 0, 0, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	err = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logFrames, &done)
	return
}

// fileSize is the size of the file at path; a file that is absent has none
// and is 0, and any other failure to stat it is the caller's error with a
// size of -1, which no recorded WAL accepts.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, nil
	case err != nil:
		return -1, err
	}
	return fi.Size(), nil
}

// readAllowance is the most counted reads one of readers goroutines may hold:
// an equal share of the values the raw bound leaves after the fixed commit and
// wait probes of every window.
func readAllowance(readers int) int64 {
	return (rawMax(rounds, RawLimit) - 2*measuredProbes) / int64(readers)
}

// collectReads is one reader goroutine's loop: it samples op on the clock now
// until stop and appends each counted duration to the array of the window that
// was current when the sample began. The count of counted samples is cumulative
// across windows and an uncounted sample adds nothing; the sample above
// allowance returns errRawOverflow and ends the loop with nothing more kept.
func collectReads(stop, recording *atomic.Bool, round *atomic.Int64, windows [][]int64, allowance int64, now func() time.Time, op func(n int) error) error {
	var held int64
	for n := 0; !stop.Load(); n++ {
		k := round.Load()
		d, counted, err := sample(now, recording, func() error { return op(n) })
		switch {
		case err != nil:
			return fmt.Errorf("read: %w", err)
		case !counted: // only a read that began while recording counts; one in flight when it stops still does
			continue
		}
		if held++; held > allowance {
			return fmt.Errorf("read: %w", errRawOverflow)
		}
		windows[k] = append(windows[k], int64(d))
	}
	return nil
}

// collectProbes is the probe loop of measure: warm probes first, then windows
// of perWindow measured probes whose wait and commit nanoseconds are appended
// to their window's array in measured order. begin runs once, just before the
// first measured probe, and round before every measured probe with that
// probe's window. The loop ends when stopped reports true or a probe fails;
// that error is returned with the earlier samples kept and no later probe run.
func collectProbes(warm, windows, perWindow int, stopped func() bool, begin func(), round func(k int), probe func() (wait, commit time.Duration, err error)) (waits, commits [][]int64, err error) {
	waits, commits = make([][]int64, windows), make([][]int64, windows)
	for i := 0; i < warm+windows*perWindow && !stopped(); i++ {
		k := i - warm
		if k == 0 {
			begin()
		}
		if k >= 0 {
			round(k / perWindow)
		}
		wait, commit, err := probe()
		if err != nil {
			return waits, commits, fmt.Errorf("probe: %w", err)
		}
		if k >= 0 {
			r := k / perWindow
			waits[r], commits[r] = append(waits[r], int64(wait)), append(commits[r], int64(commit))
		}
	}
	return waits, commits, nil
}

// assemble is the one derivation of a workload's windows, pooled series and
// raw windows from its collected samples: commits and waits per window and
// reads per reader and window. A window without a probe or a read is an error.
// Summarize leaves every array ascending, and the raw windows are those arrays.
func assemble(commits, waits [][]int64, reads [][][]int64) (wl Workload, raw []RawWindow, nReads int64, err error) {
	readWin := make([][]int64, len(commits))
	raw = make([]RawWindow, len(commits))
	for k := range commits {
		for i := range reads {
			readWin[k] = append(readWin[k], reads[i][k]...)
		}
		if len(commits[k]) == 0 || len(readWin[k]) == 0 {
			return Workload{}, nil, 0, fmt.Errorf("window %d measured %d probes and %d reads", k, len(commits[k]), len(readWin[k]))
		}
		nReads += int64(len(readWin[k]))
		wl.Windows = append(wl.Windows, Window{Probes: int64(len(commits[k])), Reads: int64(len(readWin[k])),
			Commit: Summarize(commits[k]), Read: Summarize(readWin[k]), Wait: Summarize(waits[k])})
		raw[k] = RawWindow{Commit: commits[k], Read: readWin[k], Wait: waits[k]}
	}
	wl.Commit, wl.Read, wl.Wait = Pool(commits), Pool(readWin), Pool(waits)
	return wl, raw, nReads, nil
}

// measure runs one workload: the writers and readers run for the whole
// window, and the probe supplies writer-commit and writer-wait while the
// readers supply indexed-read.
func measure(t *testing.T, sp spec, p proc, facts *Facts) Workload {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), sp.name+".db")
	db, err := store.Open(ctx, path, store.Options{ConnMaxLifetime: sp.lifetime})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	w := &world{spec: sp, db: db, lib: library.New(db)}
	if err := w.seedTitles(ctx); err != nil {
		t.Fatalf("seed %s: %v", sp.name, err)
	}

	var stop, recording atomic.Bool
	var firstErr atomic.Pointer[error]
	fail := func(err error) {
		firstErr.CompareAndSwap(nil, &err)
		stop.Store(true)
	}
	var writes, logical, round atomic.Int64
	reads := make([][][]int64, sp.readers) // [reader][round]
	allow := readAllowance(sp.readers)
	var wg sync.WaitGroup
	for i := range sp.writers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(seed, uint64(i)))
			for !stop.Load() {
				n, err := w.mutate(ctx, r)
				if err != nil {
					fail(fmt.Errorf("mutate: %w", err))
					return
				}
				if recording.Load() {
					writes.Add(1)
					logical.Add(n)
				}
			}
		})
	}
	for i := range sp.readers {
		reads[i] = make([][]int64, rounds)
		for k := range reads[i] {
			reads[i][k] = make([]int64, 0, 1<<14)
		}
		wg.Go(func() {
			r := rand.New(rand.NewPCG(seed, uint64(1000+i)))
			if err := collectReads(&stop, &recording, &round, reads[i], allow, time.Now, func(n int) error { return w.read(ctx, r, n) }); err != nil {
				fail(err)
			}
		})
	}

	var before, after runtime.MemStats
	var wrote0, wrote1 int64
	waits, commits, err := collectProbes(warmProbes, rounds, roundProbes,
		func() bool { return firstErr.Load() != nil },
		func() {
			runtime.ReadMemStats(&before)
			wrote0 = p.written()
			recording.Store(true)
		},
		func(k int) { round.Store(int64(k)) },
		func() (time.Duration, time.Duration, error) { return Probe(ctx, time.Now, time.Since, w.db.Write) })
	if err != nil {
		fail(err)
	}
	runtime.ReadMemStats(&after)
	wrote1 = p.written()
	recording.Store(false)
	stop.Store(true)
	wg.Wait()
	if err := firstErr.Load(); err != nil {
		t.Fatalf("%s: %v", sp.name, *err)
	}

	wl, rawWindows, nReads, err := assemble(commits, waits, reads)
	if err != nil {
		t.Fatalf("%s: %v", sp.name, err)
	}
	logicalBytes := logical.Load() + int64(measuredProbes*probeBytes)
	wl.Name, wl.Shape = sp.name, sp.shape()
	wl.Allocs = Allocs{Mallocs: int64(after.Mallocs - before.Mallocs), Bytes: int64(after.TotalAlloc - before.TotalAlloc), Ops: writes.Load() + nReads + measuredProbes}
	wl.WriteAmp = WriteAmp{WrittenBytes: wrote1 - wrote0, LogicalBytes: logicalBytes, Milli: (wrote1 - wrote0) * 1000 / logicalBytes}
	wl.Plans = map[string][]string{}

	pol, st := db.Policy(), db.Stats()
	wl.Settings = Settings{Writer: pol.Writer, Reader: pol.Reader, ReaderCount: store.ReaderCount(),
		WriterMaxOpen: st.Writer.MaxOpenConnections, ReaderMaxOpen: st.Reader.MaxOpenConnections}
	wl.Connects = Connects{Writer: st.WriterConnects, Reader: st.ReaderConnects}

	hot := w.titles[0]
	for name, q := range map[string]struct {
		query string
		args  []any
	}{
		"rollups_of_title": {qRollups, []any{int64(hot.id)}},
		"rollup_row":       {qRollupRow, []any{int64(hot.id), int64(hot.slots[0])}},
		"slot_episode_row": {qSlotEpisodeRow, []any{int64(hot.slots[0]), 1, 1}},
		"slot_episode_set": {qSlotEpisodeSet, []any{1, "missing", 0, int64(hot.slots[0]), 1}},
		"title_search":     {qSearch, []any{`"` + sp.name + ` title"`, 10}},
	} {
		lines, err := plan(ctx, db, q.query, q.args...)
		if err != nil {
			t.Fatalf("plan %s: %v", name, err)
		}
		wl.Plans[name] = lines
	}

	pageSize, err := func() (n int64, err error) {
		err = db.Read(ctx, func(tx *sql.Tx) error { return tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&n) })
		return
	}()
	if err != nil {
		t.Fatalf("page_size: %v", err)
	}
	walBytes, err := fileSize(path + "-wal")
	if err != nil {
		t.Fatalf("wal size: %v", err)
	}
	dbBytes, err := fileSize(path)
	if err != nil {
		t.Fatalf("db size: %v", err)
	}
	wl.WAL = WAL{FileBytes: walBytes, DBBytes: dbBytes, PageSize: pageSize}
	if wl.WAL.CheckpointBusy, wl.WAL.LogFrames, wl.WAL.Checkpointed, err = checkpoint(ctx, path); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}

	wl.Memory = p.memory()
	facts.SwapKiB = max(facts.SwapKiB, p.status("VmSwap"))

	// Raw samples are encoded after the last reading and outside every timed span.
	if wl.Raw, err = encodeRaw(rawWindows, RawLimit, FileLimit); err != nil {
		t.Fatalf("%s: raw samples: %v", sp.name, err)
	}
	return wl
}

func measureRun(t *testing.T, kind string) Run {
	t.Helper()
	p := proc{t}
	run := Run{Kind: kind, Host: Host{OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(),
		CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0)}} // this file is !race: Facts.Race stays false
	in0, out0 := p.swapPages()
	run.Facts.SwapKiB = p.status("VmSwap")
	for _, sp := range workloads {
		runtime.GC()
		run.Workloads = append(run.Workloads, measure(t, sp, p, &run.Facts))
	}
	in1, out1 := p.swapPages()
	run.Facts.SwapIn, run.Facts.SwapOut = in1-in0, out1-out0
	return run
}

// seriesFacts are the machine observations the series takes beside its
// executions: the swap counters and the process's total memory.
type seriesFacts struct {
	swap   func() (in, out int64)
	memory func() Memory
}

// errSeriesSwap is a swap counter that moved anywhere in the series' bracket.
var errSeriesSwap = errors.New("storebench: swap activity across the series")

// runSeries runs the INV-006 series: ExecutionsPerRun executions in measured
// order, each printed as it ends, then the median recorded as the baseline
// (record) or captured as the compare run (compare) at path, then the series
// memory and, last, the swap change. The swap counters are read once before
// the first execution and again only after the file is written and the memory
// read, so the bracket spans the executions, the gaps between them, their raw
// encoding and the serialization; the peak memory covers every one of them
// too. Memory and swap lines are never written into the file.
//
// A moved swap counter means a different thing in each mode. A record that saw
// swap returns errSeriesSwap: the baseline must not stand as clean. A compare
// capture has already completed by then and is the evidence of its own
// measurement, so it returns success after printing the truthful swap line;
// transport success is not a pass, and the judge, not the capture, rules on a
// run whose executions carry swap facts. Every failure to produce, encode or
// write still returns an error.
//
// An output failure is never the whole story and never a verdict. The first
// failed line is remembered and nothing more is printed; before the final
// execution ends it stops the series with no file, and from then on the
// completed run is still validated and written once, with its own output
// discarded, and memory and swap are still read. The output error is returned
// joined with any later error.
func runSeries(out io.Writer, mode, path string, execute func(i int) Run, f seriesFacts) error {
	var kind string
	switch mode {
	case modeRecord:
		kind = KindBaseline
	case modeCompare:
		kind = KindCompare
	default:
		return fmt.Errorf("storebench: series mode %q", mode)
	}
	in0, out0 := f.swap()
	executions := make([]Run, 0, ExecutionsPerRun)
	var outErr error
	for i := range ExecutionsPerRun {
		e := execute(i)
		executions = append(executions, e)
		if outErr = PrintExecution(out, i, e); outErr != nil && i < ExecutionsPerRun-1 {
			return outErr
		}
	}
	run := Median(kind, executions)
	if outErr == nil {
		outErr = PrintMedian(out, run)
	}
	if reason := Recorded(run); reason != "" {
		return errors.Join(outErr, fmt.Errorf("the series misses a recorded field: %s", reason))
	}
	sink := out
	if outErr != nil {
		sink = io.Discard
	}
	var err error
	if mode == modeRecord {
		err = Record(sink, path, run)
	} else {
		err = Capture(sink, path, run)
	}
	outErr = errors.Join(outErr, err)
	m := f.memory()
	if outErr == nil {
		_, outErr = fmt.Fprintf(out, "STORE-RECORD series memory vm_hwm_kib=%d vm_rss_kib=%d go_sys_bytes=%d go_heap_inuse_bytes=%d\n",
			m.VmHWMKiB, m.VmRSSKiB, m.GoSysBytes, m.GoHeapInuseBytes)
	}
	in1, out1 := f.swap()
	if outErr == nil {
		_, outErr = fmt.Fprintf(out, "STORE-RECORD series swap_in_pages=%d swap_out_pages=%d\n", in1-in0, out1-out0)
	}
	if mode == modeRecord && (in1 != in0 || out1 != out0) {
		outErr = errors.Join(outErr, errSeriesSwap)
	}
	return outErr
}

// TestForegroundSeries records, captures or judges the INV-006 series. Without
// the foreground environment variable it skips, and a skip is not evidence.
func TestForegroundSeries(t *testing.T) {
	mode := os.Getenv(foregroundEnv)
	switch mode {
	case "":
		t.Skip(foregroundEnv + " is unset: this skip is not evidence")
	case modeRecord, modeCompare, modeJudge:
	default:
		t.Fatalf("%s=%q: want record, compare or judge", foregroundEnv, mode)
	}
	if mode == modeJudge { // measures nothing: it reads the captured compare run
		res, err := JudgeFile(os.Stdout, baselineFile, compareFile)
		if err != nil {
			t.Fatal(err)
		}
		if res.Verdict != Pass {
			t.Fatalf("%s: %s", res.Verdict, res.Reason)
		}
		return
	}
	p := proc{t}
	path := map[string]string{modeRecord: baselineFile, modeCompare: compareFile}[mode]
	execute := func(int) Run { return measureRun(t, KindExecution) }
	if err := runSeries(os.Stdout, mode, path, execute, seriesFacts{swap: p.swapPages, memory: p.memory}); err != nil {
		t.Fatal(err)
	}
}

// TestForegroundProducer drives the producer's own code, the series
// orchestration and the reader collection, with small deterministic inputs
// and controlled fact readers. No workload runs and nothing is measured.
func TestForegroundProducer(t *testing.T) {
	quiet := seriesFacts{
		swap:   func() (int64, int64) { return 0, 0 },
		memory: func() Memory { return Memory{VmHWMKiB: 1, VmRSSKiB: 1, GoSysBytes: 1, GoHeapInuseBytes: 1} },
	}
	files := map[string]string{modeRecord: baselineFile, modeCompare: compareFile}
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }

	t.Run("observations cover the whole series", func(t *testing.T) {
		arrays := testArraysSet(shifted(0))
		execs := executionsOf(arrays)
		for _, mode := range []string{modeRecord, modeCompare} {
			observe := func(t *testing.T, execute func(int) Run, swap func(path string) (int64, int64), memory func(path string) Memory) (string, string, error) {
				t.Helper()
				path := filepath.Join(t.TempDir(), files[mode])
				var out bytes.Buffer
				err := runSeries(&out, mode, path, execute, seriesFacts{
					swap:   func() (int64, int64) { return swap(path) },
					memory: func() Memory { return memory(path) },
				})
				return out.String(), path, err
			}
			pick := func(i int) Run { return execs[i] }
			still := func(string) (int64, int64) { return 0, 0 }
			const peakLine = "STORE-RECORD series memory vm_hwm_kib=900 vm_rss_kib=90 go_sys_bytes=9 go_heap_inuse_bytes=8\n"
			peak := func(path string) Memory {
				if exists(path) { // the serialization is done: its peak is the run's
					return Memory{VmHWMKiB: 900, VmRSSKiB: 90, GoSysBytes: 9, GoHeapInuseBytes: 8}
				}
				return Memory{VmHWMKiB: 100, VmRSSKiB: 10, GoSysBytes: 2, GoHeapInuseBytes: 1}
			}
			lastLine := func(out string) string {
				lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
				return lines[len(lines)-1]
			}

			t.Run(mode+" clean", func(t *testing.T) {
				out, path, err := observe(t, pick, still, peak)
				if err != nil {
					t.Fatalf("a clean series: %v", err)
				}
				got, reason := loadRun(path, FileLimit)
				if reason != "" || got.Kind != map[string]string{modeRecord: KindBaseline, modeCompare: KindCompare}[mode] || !reflect.DeepEqual(got.Executions, execs) {
					t.Fatalf("the file is not the median of the measured executions: %q", reason)
				}
				if strings.Contains(out, "PASS") {
					t.Fatalf("the series output says PASS:\n%s", out)
				}
				at := -1
				for i := range ExecutionsPerRun {
					next := strings.Index(out, fmt.Sprintf("STORE-RECORD execution=%d\n", i))
					if next <= at {
						t.Fatalf("execution %d is not printed in order after execution %d:\n%s", i, i-1, out)
					}
					at = next
				}
				if mem := strings.Index(out, "STORE-RECORD series memory "); mem < at || lastLine(out) != "STORE-RECORD series swap_in_pages=0 swap_out_pages=0" {
					t.Fatalf("the series memory and swap records do not close the transcript:\n%s", out)
				}
			})

			// A swap counter that moves anywhere in the bracket is in the final
			// delta. The counters start away from zero, so the delta is the true
			// change and no absolute reading; each direction moves alone. Record
			// reports errSeriesSwap; a compare capture is complete evidence of
			// its measurement and still succeeds, with every execution retained.
			const in0, out0 = 1000, 2000
			moves := []struct {
				name    string
				in, out int64
			}{{"swap-in", 7, 0}, {"swap-out", 0, 3}}
			events := []struct {
				name      string
				execution int // the execution whose call moves the counters; -1 for none
				write     bool
				memory    bool
			}{
				{"in the first execution", 0, false, false},
				{"between executions", 3, false, false}, // after execution 2 ended, before 3 begins
				{"during the file write", -1, true, false},
				{"at the memory reading", -1, false, true},
			}
			for _, ev := range events {
				for _, mv := range moves {
					t.Run(mode+" "+mv.name+" "+ev.name, func(t *testing.T) {
						moved := false
						execute := func(i int) Run {
							moved = moved || i == ev.execution
							return execs[i]
						}
						swap := func(path string) (int64, int64) {
							moved = moved || ev.write && exists(path)
							if moved {
								return in0 + mv.in, out0 + mv.out
							}
							return in0, out0
						}
						memory := func(path string) Memory {
							m := peak(path)
							moved = moved || ev.memory
							return m
						}
						out, path, err := observe(t, execute, swap, memory)
						want := fmt.Sprintf("STORE-RECORD series swap_in_pages=%d swap_out_pages=%d", mv.in, mv.out)
						if lastLine(out) != want || !strings.Contains(out, peakLine) || strings.Contains(out, "PASS") {
							t.Fatalf("the transcript does not close with the memory peak and %q:\n%s", want, out)
						}
						if mode == modeRecord {
							if !errors.Is(err, errSeriesSwap) {
								t.Fatalf("a recorded series with swap: %v", err)
							}
							return
						}
						if err != nil || !strings.Contains(out, CaptureLine+"\n") {
							t.Fatalf("a completed capture with swap: %v\n%s", err, out)
						}
						got, reason := loadRun(path, FileLimit)
						if reason != "" || got.Kind != KindCompare || !reflect.DeepEqual(got.Executions, execs) {
							t.Fatalf("the capture is not the measured executions: %q", reason)
						}
						if err := matchArrays(got, arrays); err != nil {
							t.Fatalf("the capture does not retain the measured samples: %v", err)
						}
					})
				}
			}

			t.Run(mode+" memory peak of the file write", func(t *testing.T) {
				out, _, err := observe(t, pick, still, peak)
				if err != nil || !strings.Contains(out, peakLine) {
					t.Fatalf("the memory record misses the peak of the write: %v\n%s", err, out)
				}
			})
		}
	})

	t.Run("collection keeps one allowance across windows", func(t *testing.T) {
		// Nine samples: 0-2 begin in window 0, sample 2 moves to window 1,
		// sample 3 turns recording off while in flight (it counts), 4-6 begin
		// after that and are dropped, 6 turns recording on again, 7 and 8 count.
		// Six samples count: 0-2 in window 0, 3, 7 and 8 in window 1. Each
		// sample's duration is its own value of the clock, so the arrays show
		// which samples were kept, in which window and in what order.
		durations := [...]time.Duration{100, 200, 300, 400, 500, 600, 700, 800, 900}
		script := func(allowance int64) (windows [][]int64, calls int, err error) {
			var stop, recording atomic.Bool
			var round atomic.Int64
			recording.Store(true)
			windows = make([][]int64, 2)
			// Two readings around every sample: the second is its duration after the first.
			readings := 0
			now := func() time.Time {
				n := readings / 2
				at := time.Unix(0, 0).Add(time.Duration(n) * time.Hour)
				if readings%2 == 1 {
					at = at.Add(durations[n])
				}
				readings++
				return at
			}
			err = collectReads(&stop, &recording, &round, windows, allowance, now, func(n int) error {
				calls = n + 1
				switch n {
				case 2:
					round.Store(1)
				case 3:
					recording.Store(false)
				case 6:
					recording.Store(true)
				case 8:
					stop.Store(true)
				}
				return nil
			})
			return
		}
		windows, calls, err := script(6)
		if want := [][]int64{{100, 200, 300}, {400, 800, 900}}; err != nil || !reflect.DeepEqual(windows, want) || calls != 9 {
			t.Fatalf("the exact allowance: windows %v, want %v, %d samples, %v", windows, want, calls, err)
		}
		// The counted sample in flight (400) is kept; the three dropped ones
		// (500-700) never appear; the one above the allowance (900) is not appended.
		windows, calls, err = script(5)
		if want := [][]int64{{100, 200, 300}, {400, 800}}; !errors.Is(err, errRawOverflow) || !reflect.DeepEqual(windows, want) || calls != 9 {
			t.Fatalf("one counted sample above the allowance: windows %v, want %v, %d samples, %v", windows, want, calls, err)
		}

		failing := errors.New("boom")
		var stop, recording atomic.Bool
		var round atomic.Int64
		recording.Store(true)
		if err := collectReads(&stop, &recording, &round, make([][]int64, 1), 5, time.Now, func(int) error { return failing }); !errors.Is(err, failing) {
			t.Fatalf("a failed read: %v", err)
		}

		// The allowance of one reader is its equal share of what the fixed
		// probes leave of the raw bound.
		readers := int64(workloads[0].readers)
		allow := readAllowance(int(readers))
		if !rawFits(2*measuredProbes+readers*allow, rounds, RawLimit) || rawFits(2*measuredProbes+readers*(allow+1), rounds, RawLimit) {
			t.Fatalf("the allowance %d per reader is not the largest equal share of the raw bound", allow)
		}
	})

	t.Run("the measured executions reach the compare capture", func(t *testing.T) {
		base := testRun(KindBaseline)
		arrays := testArraysSet(shifted(23))
		later := executionsOf(arrays)
		path := filepath.Join(t.TempDir(), compareFile)
		var out bytes.Buffer
		if err := runSeries(&out, modeCompare, path, func(i int) Run { return later[i] }, quiet); err != nil {
			t.Fatalf("compare series: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), CaptureLine+"\n") || strings.Contains(out.String(), "PASS") {
			t.Fatalf("a compare series prints its capture and no pass:\n%s", out.String())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeRun(data)
		if err != nil {
			t.Fatalf("the captured bytes: %v", err)
		}
		if got.Kind != KindCompare || Consistent(got) != "" || Recorded(got) != "" {
			t.Fatalf("captured run: kind %q, consistent %q, recorded %q", got.Kind, Consistent(got), Recorded(got))
		}
		// The raw samples, every execution and every median are derived again from
		// the arrays the five executions were measured from.
		if err := matchArrays(got, arrays); err != nil {
			t.Fatalf("the capture is not the measured series: %v", err)
		}
		metrics := func(r Run) []Metric {
			var all []Metric
			for _, w := range r.Workloads {
				all = append(all, w.Metrics()...)
			}
			return all
		}
		if reflect.DeepEqual(metrics(got), metrics(base)) {
			t.Fatal("the captured median equals the retained baseline's")
		}
		for i, e := range got.Executions {
			if reflect.DeepEqual(metrics(e), metrics(got)) {
				t.Fatalf("the captured median equals execution %d's", i)
			}
		}
	})

	t.Run("a real empty WAL reaches the judge and a failed observation stops the series", func(t *testing.T) {
		ctx := t.Context()
		dir := t.TempDir()
		must := func(what string, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}

		// A real store with one library written and read back, then closed: its
		// log is absent or empty and a passive checkpoint finds nothing in it.
		dbPath := filepath.Join(dir, "empty.db")
		db, err := store.Open(ctx, dbPath, store.Options{})
		must("store.Open", err)
		_, err = library.New(db).AddLibrary(ctx, library.LibrarySpec{
			Name:     "foreground",
			Defaults: library.LibraryDefaults{Monitored: true, NamingLanguage: "en", SearchLanguage: "en", Profile: "hd"},
			Template: []library.TemplateSlot{{Key: "hd", Primary: true}, {Key: "uhd"}},
		})
		must("AddLibrary", err)
		var pageSize int64
		must("page_size", db.Read(ctx, func(tx *sql.Tx) error { return tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize) }))
		must("Close", db.Close())
		walBytes, err := fileSize(dbPath + "-wal")
		must("wal size", err)
		dbBytes, err := fileSize(dbPath)
		must("db size", err)
		busy, frames, done, err := checkpoint(ctx, dbPath)
		must("wal_checkpoint", err)
		empty := WAL{FileBytes: walBytes, DBBytes: dbBytes, PageSize: pageSize, CheckpointBusy: busy, LogFrames: frames, Checkpointed: done}
		if walBytes != 0 || dbBytes <= 0 || pageSize <= 0 || busy != 0 || frames != 0 || done != 0 {
			t.Fatalf("the observation of a closed store: %+v", empty)
		}

		// Those values as execution 1's churn WAL in five executions of a later
		// run inside the predicate: the capture holds them and the judge passes it.
		series := func(wal WAL, path string) (string, error) {
			execs := executionsOf(testArraysSet(shifted(11)))
			execs[1].Workloads[0].WAL = wal
			var out bytes.Buffer
			err := runSeries(&out, modeCompare, path, func(i int) Run { return execs[i] }, quiet)
			return out.String(), err
		}
		basePath := filepath.Join(dir, baselineFile)
		must("record baseline", Record(io.Discard, basePath, testRun(KindBaseline)))
		path := filepath.Join(dir, compareFile)
		out, err := series(empty, path)
		if err != nil || !strings.Contains(out, CaptureLine+"\n") {
			t.Fatalf("a series with an empty WAL: %v\n%s", err, out)
		}
		data, err := os.ReadFile(path)
		must("read capture", err)
		got, err := DecodeRun(data)
		must("decode capture", err)
		if !reflect.DeepEqual(got.Executions[1].Workloads[0].WAL, empty) || Recorded(got) != "" {
			t.Fatalf("the capture does not carry the empty WAL: %+v, recorded %q", got.Executions[1].Workloads[0].WAL, Recorded(got))
		}
		var judged bytes.Buffer
		if res, jerr := JudgeFile(&judged, basePath, path); jerr != nil || res.Verdict != Pass || !strings.HasSuffix(judged.String(), "STORE-FOREGROUND-PASS\n") {
			t.Fatalf("the judge of a capture with an empty WAL: %+v %v\n%s", res, jerr, judged.String())
		}

		// A stat that fails for any reason but absence is an error, never a zero.
		regular := filepath.Join(dir, "regular")
		must("write regular file", os.WriteFile(regular, nil, 0o600))
		failedSize, statErr := fileSize(filepath.Join(regular, "child"))
		if statErr == nil || errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the size of a child of a regular file: %d, %v", failedSize, statErr)
		}
		// Whatever size a failed stat returns, a series that loses the error
		// still refuses it: the observation is never published.
		statPath := filepath.Join(dir, "failed-stat-"+compareFile)
		stat := empty
		stat.FileBytes = failedSize
		out, err = series(stat, statPath)
		if err == nil || !strings.Contains(err.Error(), "execution1_churn_wal_missing") || strings.Contains(out, CaptureLine) || exists(statPath) {
			t.Fatalf("a series with a failed WAL size: %v, file %t\n%s", err, exists(statPath), out)
		}
		if n, err := fileSize(filepath.Join(dir, "absent")); n != 0 || err != nil {
			t.Errorf("the size of an absent file: %d, %v", n, err)
		}

		// A database that is not in WAL mode reports -1 frames: that failed
		// observation stops the series and no file is written.
		legacy := filepath.Join(dir, "rollback.db")
		c, err := sqlite.NewConnector(legacy)
		must("connector", err)
		ldb := sql.OpenDB(c)
		_, err = ldb.ExecContext(ctx, "CREATE TABLE t (x)")
		must("create table", errors.Join(err, ldb.Close()))
		lbusy, lframes, ldone, err := checkpoint(ctx, legacy)
		must("rollback wal_checkpoint", err)
		if lframes != -1 || ldone != -1 {
			t.Fatalf("the checkpoint of a rollback-journal database: busy %d, frames %d, checkpointed %d", lbusy, lframes, ldone)
		}
		lbytes, err := fileSize(legacy)
		must("rollback db size", err)
		failedPath := filepath.Join(dir, "failed-"+compareFile)
		out, err = series(WAL{DBBytes: lbytes, PageSize: pageSize, CheckpointBusy: lbusy, LogFrames: lframes, Checkpointed: ldone}, failedPath)
		if err == nil || !strings.Contains(err.Error(), "execution1_churn_wal_missing") || strings.Contains(out, CaptureLine) || exists(failedPath) {
			t.Fatalf("a series with a failed WAL observation: %v, file %t\n%s", err, exists(failedPath), out)
		}
	})

	t.Run("an output failure reaches the series without losing a completed run", func(t *testing.T) {
		execs := executionsOf(testArraysSet(shifted(0)))
		kinds := map[string]string{modeRecord: KindBaseline, modeCompare: KindCompare}

		// series drives the real runSeries through out and reports which
		// executions it asked for and how many writes had been made before each.
		type outcome struct {
			err     error
			started []int
			begins  []int
		}
		series := func(mode, path string, out *failWriter, facts seriesFacts) outcome {
			var o outcome
			o.err = runSeries(out, mode, path, func(i int) Run {
				o.started = append(o.started, i)
				o.begins = append(o.begins, out.writes)
				return execs[i]
			}, facts)
			return o
		}

		for _, mode := range []string{modeRecord, modeCompare} {
			t.Run(mode, func(t *testing.T) {
				dir := t.TempDir()
				want := Median(kinds[mode], execs)

				// One run through a writer that never fails gives the write
				// positions; how many there are is coverage, not an assertion.
				counted := &failWriter{out: io.Discard}
				cleanPath := filepath.Join(dir, "clean.json")
				clean := series(mode, cleanPath, counted, quiet)
				if clean.err != nil || !slices.Equal(clean.started, []int{0, 1, 2, 3, 4}) || counted.writes <= clean.begins[ExecutionsPerRun-1] {
					t.Fatalf("the counting run: %v, executions %v, %d writes", clean.err, clean.started, counted.writes)
				}
				if err := checkRunFile(cleanPath, want); err != nil {
					t.Fatalf("the counting run: %v", err)
				}
				final := clean.begins[ExecutionsPerRun-1] // writes made before the final execution began

				// The k-th write and every one after it fails. An output that
				// breaks before the final execution ends the series with no file;
				// from the final execution's first write on, the completed run is
				// still written once, with every execution and raw byte.
				for k := 1; k <= counted.writes; k++ {
					path := filepath.Join(dir, fmt.Sprintf("fail-%d.json", k))
					got := series(mode, path, &failWriter{out: io.Discard, failAt: k}, quiet)
					owner := 0 // the execution whose output holds write k
					for i, begun := range clean.begins {
						if begun < k {
							owner = i
						}
					}
					if !errors.Is(got.err, errOutput) {
						t.Fatalf("write %d, in execution %d: the series returned %v, not the output failure", k, owner, got.err)
					}
					if wantStarted := clean.started[:owner+1]; !slices.Equal(got.started, wantStarted) {
						t.Fatalf("write %d, in execution %d: executions %v ran, want %v", k, owner, got.started, wantStarted)
					}
					if owner < ExecutionsPerRun-1 {
						if exists(path) {
							t.Fatalf("write %d, in execution %d: an incomplete series left a file", k, owner)
						}
						continue
					}
					if err := checkRunFile(path, want); err != nil {
						t.Fatalf("write %d, in the final execution or after: %v", k, err)
					}
				}

				// A failed output and a real publication failure (a regular file
				// where the directory should be gives ENOTDIR) keep both errors,
				// and all five executions still ran. The final memory and swap are
				// still read although nothing was published: the swap counter is 0
				// at the start and 7 after the executions, whatever the file is.
				blocked := filepath.Join(dir, "regular")
				if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				var swaps, memories int
				seen := seriesFacts{
					swap: func() (int64, int64) {
						swaps++
						if swaps == 1 {
							return 0, 0
						}
						return 7, 0
					},
					memory: func() Memory { memories++; return quiet.memory() },
				}
				blockedPath := filepath.Join(blocked, files[mode])
				both := series(mode, blockedPath, &failWriter{out: io.Discard, failAt: final + 1}, seen)
				if !errors.Is(both.err, errOutput) || !errors.Is(both.err, syscall.ENOTDIR) || !slices.Equal(both.started, clean.started) || exists(blockedPath) {
					t.Fatalf("an output failure in the final execution with a failed publication: %v after executions %v", both.err, both.started)
				}
				if swaps != 2 || memories != 1 || errors.Is(both.err, errSeriesSwap) != (mode == modeRecord) {
					t.Fatalf("a failed publication: %d swap readings and %d memory readings, errSeriesSwap %t in %s mode", swaps, memories, errors.Is(both.err, errSeriesSwap), mode)
				}

				// A failed output at every write from the final execution on,
				// including the publication outcome line, with the swap counters
				// moving once the file exists: memory and the final swap are
				// still read after the write, the completed file stands, and the
				// sentinel is returned. A record that saw swap also returns
				// errSeriesSwap; a compare capture does not.
				for k := final + 1; k <= counted.writes; k++ {
					path := filepath.Join(dir, fmt.Sprintf("late-%d.json", k))
					var memoryAfterWrite, swapAfterWrite bool
					facts := seriesFacts{
						swap: func() (int64, int64) {
							if exists(path) {
								swapAfterWrite = true
								return 7, 0
							}
							return 0, 0
						},
						memory: func() Memory {
							memoryAfterWrite = exists(path)
							return quiet.memory()
						},
					}
					got := series(mode, path, &failWriter{out: io.Discard, failAt: k}, facts)
					if !errors.Is(got.err, errOutput) || errors.Is(got.err, errSeriesSwap) != (mode == modeRecord) {
						t.Fatalf("write %d failing with swap moved: the series returned %v, want the output failure and errSeriesSwap only in record mode", k, got.err)
					}
					if !memoryAfterWrite || !swapAfterWrite || !slices.Equal(got.started, clean.started) {
						t.Fatalf("write %d failing with swap moved: memory read after the write %t, swap read after the write %t, executions %v", k, memoryAfterWrite, swapAfterWrite, got.started)
					}
					if err := checkRunFile(path, want); err != nil {
						t.Fatalf("write %d failing with swap moved: %v", k, err)
					}
				}

				// A failed output in the final execution and a completed series
				// that independently misses a recorded field keep both causes,
				// run all five executions and publish nothing.
				short := slices.Clone(execs)
				short[ExecutionsPerRun-1].Workloads = slices.Clone(short[ExecutionsPerRun-1].Workloads)
				short[ExecutionsPerRun-1].Workloads[0].WAL.DBBytes = 0
				var ran []int
				missingPath := filepath.Join(dir, "missing-"+files[mode])
				err := runSeries(&failWriter{out: io.Discard, failAt: final + 1}, mode, missingPath, func(i int) Run {
					ran = append(ran, i)
					return short[i]
				}, quiet)
				if !errors.Is(err, errOutput) || err == nil || !strings.Contains(err.Error(), "execution4_churn_wal_missing") || !slices.Equal(ran, clean.started) || exists(missingPath) {
					t.Fatalf("an output failure with a missing recorded field: %v after executions %v, file %t", err, ran, exists(missingPath))
				}
			})
		}
	})

	t.Run("the judge caller rejects a failed report", func(t *testing.T) {
		dir := t.TempDir()
		for name, run := range map[string]Run{baselineFile: testRun(KindBaseline), compareFile: testRun(KindCompare)} {
			if err := writeRun(filepath.Join(dir, name), run, FileLimit); err != nil {
				t.Fatalf("fixture %s: %v", name, err)
			}
		}
		snapshot := func() map[string][]byte {
			t.Helper()
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			held := map[string][]byte{}
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				held[e.Name()] = data
			}
			return held
		}
		before := snapshot()

		// The actual TestForegroundSeries judge mode in a child over those two
		// files, with a closed environment and its own process group, which the
		// deadline kills: it measures nothing and reports through the child's
		// own stdout.
		judge := func(stdout io.Writer) (code int) {
			t.Helper()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForegroundSeries$")
			cmd.Dir = dir
			cmd.Env = []string{foregroundEnv + "=" + modeJudge}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.Stdout = stdout
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("the judge child did not end: %v", err)
			}
			var exit *exec.ExitError
			switch {
			case err == nil:
				return 0
			case errors.As(err, &exit):
				return exit.ExitCode()
			}
			t.Fatalf("the judge child: %v", err)
			return 0
		}

		var ok bytes.Buffer
		if code := judge(&ok); code != 0 || !strings.Contains(ok.String(), "STORE-FOREGROUND-PASS\n") {
			t.Fatalf("the judge over valid files with a working stdout: exit %d\n%s", code, ok.String())
		}

		// /dev/full accepts the open and fails every write. The framework's own
		// output goes to the same sink, so no diagnostic is expected here: the
		// child must only exit with a failure status of its own.
		full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("the failing sink: %v", err)
		}
		t.Cleanup(func() { _ = full.Close() })
		if code := judge(full); code <= 0 {
			t.Fatalf("the judge reported to a stdout that fails every write and exited %d, want a failure status", code)
		}
		if after := snapshot(); !reflect.DeepEqual(before, after) {
			t.Fatal("a judge call changed the baseline or compare files, or added one")
		}
	})

	t.Run("the writer probe spans reach the compare capture", func(t *testing.T) {
		ctx := t.Context()
		const warm, windows, perWindow = 2, 2, 3
		const perCollection = warm + windows*perWindow
		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"), store.Options{})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		w := &world{db: db, lib: library.New(db)}
		if err := w.seedTitles(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
		readN := func(tx *sql.Tx) (n int64) {
			if err := tx.QueryRowContext(ctx, `SELECT n FROM bench_probe WHERE id = 1`).Scan(&n); err != nil {
				t.Fatalf("read n: %v", err)
			}
			return n
		}
		committed := func() (n int64) {
			if err := db.Read(ctx, func(tx *sql.Tx) error { n = readN(tx); return nil }); err != nil {
				t.Fatalf("committed n: %v", err)
			}
			return n
		}

		// One probe's three spans. Its four clock readings are scripted as the
		// start, start plus admission, plus UPDATE and plus commit.
		type spanSet struct{ admit, update, commit time.Duration }
		type rig struct {
			now      func() time.Time
			since    func(time.Time) time.Duration
			write    func(context.Context, func(*sql.Tx) error) error
			readings int   // clock readings taken
			writes   int   // write calls made
			n        int64 // the committed n the clock expects
		}
		rolledBack := errors.New("storebench test: probe write rolled back")
		epoch := time.Unix(0, 0)

		// newRig scripts probe g's readings from spans[g] and checks at every
		// reading where the probe is: the write adapter calls the real
		// DB.Write and keeps the callback's active tx. A probe at index
		// rollbackAt returns rolledBack after its UPDATE, so Write rolls back.
		newRig := func(spans []spanSet, rollbackAt int) *rig {
			r := &rig{n: committed()}
			var active *sql.Tx
			reading := func() (g, place int, s spanSet, start time.Time) {
				g, place = r.readings/4, r.readings%4
				r.readings++
				if g >= len(spans) {
					t.Fatalf("reading %d is beyond the %d scripted probes", r.readings, len(spans))
				}
				return g, place, spans[g], epoch.Add(time.Duration(g) * time.Hour)
			}
			r.now = func() time.Time {
				g, place, s, start := reading()
				switch place {
				case 0:
					if active != nil || committed() != r.n {
						t.Errorf("probe %d: the first reading is not before the write call with n unchanged", g)
					}
					return start
				case 1:
					if active == nil || readN(active) != r.n {
						t.Errorf("probe %d: the second reading is not inside the callback before the UPDATE", g)
					}
					return start.Add(s.admit)
				case 2:
					if active == nil || readN(active) != r.n+1 {
						t.Errorf("probe %d: the third reading is not inside the callback after the UPDATE", g)
					}
					return start.Add(s.admit + s.update)
				}
				t.Fatalf("probe %d: the fourth reading is not the elapsed one", g)
				return time.Time{}
			}
			r.since = func(from time.Time) time.Duration {
				g, place, s, start := reading()
				updated := start.Add(s.admit + s.update)
				if place != 3 || !from.Equal(updated) {
					t.Errorf("probe %d: the elapsed reading is not reading 4 taken since the reading after the UPDATE", g)
				}
				want := r.n + 1
				if g == rollbackAt {
					want = r.n
				}
				if active != nil || committed() != want {
					t.Errorf("probe %d: the elapsed reading is not after the real Write returned with n %d committed", g, want)
				}
				r.n = want
				return updated.Add(s.commit).Sub(from)
			}
			r.write = func(ctx context.Context, fn func(*sql.Tx) error) error {
				g := r.writes
				r.writes++
				err := db.Write(ctx, func(tx *sql.Tx) error {
					active = tx
					if err := fn(tx); err != nil {
						return err
					}
					if g == rollbackAt {
						return rolledBack
					}
					return nil
				})
				active = nil
				return err
			}
			return r
		}

		// collect runs the actual collectProbes over the scripted probes and
		// checks what any collection owes: the arrays hold exactly the
		// admission and commit spans of the measured probes that ended
		// without an error, in measured order and without a warm-up span, the
		// observers ran at their places, the committed n counts only the
		// probes that committed, and a probe that ended took four readings.
		// stopAfter above 0 ends the loop after that many probes.
		collect := func(spans []spanSet, rollbackAt, stopAfter int) (waits, commits [][]int64, r *rig, err error) {
			t.Helper()
			r = newRig(spans, rollbackAt)
			n0 := r.n
			var begins, beganAfter int
			var rounds []int
			waits, commits, err = collectProbes(warm, windows, perWindow,
				func() bool { return stopAfter > 0 && r.writes >= stopAfter },
				func() { begins, beganAfter = begins+1, r.writes },
				func(k int) { rounds = append(rounds, k) },
				func() (time.Duration, time.Duration, error) { return Probe(ctx, r.now, r.since, r.write) })
			done := r.writes
			if err != nil {
				if rollbackAt < 0 {
					t.Fatalf("a collection over a working write: %v", err)
				}
				done--
			}
			wantWaits, wantCommits := make([][]int64, windows), make([][]int64, windows)
			for j := warm; j < done; j++ {
				k := (j - warm) / perWindow
				wantWaits[k] = append(wantWaits[k], int64(spans[j].admit))
				wantCommits[k] = append(wantCommits[k], int64(spans[j].commit))
			}
			if !reflect.DeepEqual(waits, wantWaits) {
				t.Errorf("collected waits %v, want the admission spans %v", waits, wantWaits)
			}
			if !reflect.DeepEqual(commits, wantCommits) {
				t.Errorf("collected commits %v, want the commit spans %v", commits, wantCommits)
			}
			wantRounds := make([]int, max(r.writes-warm, 0))
			for k := range wantRounds {
				wantRounds[k] = k / perWindow
			}
			wantBegins := 0
			if r.writes > warm {
				wantBegins = 1
			}
			if begins != wantBegins || begins == 1 && beganAfter != warm || !slices.Equal(rounds, wantRounds) {
				t.Errorf("the begin observer ran %d times, after %d probes; the round observer saw %v, want %v", begins, beganAfter, rounds, wantRounds)
			}
			if err == nil && r.readings != 4*r.writes {
				t.Errorf("%d clock readings for %d probes, want four each", r.readings, r.writes)
			}
			if got := committed(); got != n0+int64(done) {
				t.Errorf("committed n is %d, want %d: only a probe that committed counts", got, n0+int64(done))
			}
			return waits, commits, r, err
		}

		// Five collections with distinct span tables. The spans are chosen
		// independently of the code under test: three disjoint magnitudes,
		// each a permutation of 1..40 over the 40 probes, so the measured
		// order is not the sorted order.
		table := make([][]spanSet, ExecutionsPerRun)
		all := map[time.Duration]bool{}
		for i := range table {
			for j := range perCollection {
				g := i*perCollection + j
				s := spanSet{
					admit:  time.Duration((g*17+5)%40+1)*time.Millisecond + 11,
					update: time.Duration((g*23+11)%40+1)*time.Microsecond + 7,
					commit: time.Duration((g*29+3)%40+1)*13 + 3,
				}
				table[i] = append(table[i], s)
				for _, d := range []time.Duration{s.admit, s.update, s.commit} {
					if all[d] {
						t.Fatalf("span %v repeats", d)
					}
					all[d] = true
				}
			}
		}
		for _, spans := range table {
			for _, s := range spans {
				for _, sum := range []time.Duration{s.admit + s.update, s.admit + s.commit, s.update + s.commit, s.admit + s.update + s.commit} {
					if all[sum] {
						t.Fatalf("the sum %v of spans of one probe is a span", sum)
					}
				}
			}
		}
		type collected struct{ waits, commits [][]int64 }
		got := make([]collected, len(table))
		for i := range table {
			ws, cs, _, _ := collect(table[i], -1, 0)
			got[i] = collected{ws, cs}
		}

		// Collections that differ from the first only in their UPDATE spans
		// give equal arrays; only in admission spans, equal commits and other
		// waits; only in commit spans, equal waits and other commits.
		vary := func(f func(*spanSet)) []spanSet {
			v := slices.Clone(table[0])
			for i := range v {
				f(&v[i])
			}
			return v
		}
		ws, cs, _, _ := collect(vary(func(s *spanSet) { s.update = s.update*3 + 1 }), -1, 0)
		if !reflect.DeepEqual(ws, got[0].waits) || !reflect.DeepEqual(cs, got[0].commits) {
			t.Errorf("collections that differ only in UPDATE spans differ: waits %v and %v, commits %v and %v", ws, got[0].waits, cs, got[0].commits)
		}
		ws, cs, _, _ = collect(vary(func(s *spanSet) { s.admit = s.admit*2 + 1 }), -1, 0)
		if reflect.DeepEqual(ws, got[0].waits) || !reflect.DeepEqual(cs, got[0].commits) {
			t.Errorf("collections that differ only in admission spans do not change only the waits: waits %v and %v, commits %v and %v", ws, got[0].waits, cs, got[0].commits)
		}
		ws, cs, _, _ = collect(vary(func(s *spanSet) { s.commit = s.commit*2 + 1 }), -1, 0)
		if !reflect.DeepEqual(ws, got[0].waits) || reflect.DeepEqual(cs, got[0].commits) {
			t.Errorf("collections that differ only in commit spans do not change only the commits: waits %v and %v, commits %v and %v", ws, got[0].waits, cs, got[0].commits)
		}

		// A measured probe whose real Write rolls back after a successful
		// UPDATE ends the collection with its error: the earlier samples
		// stay and no later probe runs.
		const failing = warm + 4
		ws, _, r, err := collect(table[1], failing, 0)
		if !errors.Is(err, rolledBack) || r.writes != failing+1 || len(ws[0]) != perWindow || len(ws[1]) != 1 {
			t.Errorf("a rolled-back probe: error %v, %d probes ran, waits %v", err, r.writes, ws)
		}

		// The loop also ends, without an error, when its stop observer says
		// so; a window left without a probe is then no workload.
		ws, cs, r, err = collect(table[2], -1, warm+1)
		if err != nil || r.writes != warm+1 || len(ws[0]) != 1 || len(ws[1]) != 0 {
			t.Errorf("a stopped collection: error %v, %d probes ran, waits %v", err, r.writes, ws)
		}

		// Each execution's churn windows, pools and raw come from the actual
		// assembly and encodeRaw over its collection; the expected arrays
		// come from the span tables alone.
		spanArrays := func(spans []spanSet, k int) (admits, commits []int64) {
			for _, s := range spans[warm+k*perWindow : warm+(k+1)*perWindow] {
				admits, commits = append(admits, int64(s.admit)), append(commits, int64(s.commit))
			}
			slices.Sort(admits)
			slices.Sort(commits)
			return admits, commits
		}
		arrays := testArraysSet(shifted(0))
		for i := range arrays {
			for k := range windows {
				arrays[i][0][k].Wait, arrays[i][0][k].Commit = spanArrays(table[i], k)
			}
		}
		execs := executionsOf(arrays)
		// Reads come from three readers whose samples partition each window's
		// expected array, each reader's share in descending order, so the
		// assembled window is whole only when every reader is merged.
		const readers = 3
		splitReads := func(ws []RawWindow) [][][]int64 {
			reads := make([][][]int64, readers)
			for r := range reads {
				reads[r] = make([][]int64, windows)
			}
			for k, w := range ws {
				for j, v := range w.Read {
					reads[j%readers][k] = append(reads[j%readers][k], v)
				}
			}
			for r := range reads {
				for k := range reads[r] {
					slices.Reverse(reads[r][k])
				}
			}
			return reads
		}
		for i := range execs {
			churn := &execs[i].Workloads[0]
			wl, raw, nReads, err := assemble(got[i].commits, got[i].waits, splitReads(arrays[i][0]))
			if err != nil {
				t.Fatalf("assemble execution %d: %v", i, err)
			}
			if want := int64(len(arrays[i][0][0].Read) + len(arrays[i][0][1].Read)); nReads != want {
				t.Errorf("execution %d: assembly returned %d reads, want %d", i, nReads, want)
			}
			for k := range windows {
				want := arrays[i][0][k].Read
				if !slices.Equal(raw[k].Read, want) {
					t.Errorf("execution %d window %d: the assembled Read is %v, want %v", i, k, raw[k].Read, want)
				}
				if wl.Windows[k].Reads != int64(len(want)) || wl.Windows[k].Read != seriesOf(want) {
					t.Errorf("execution %d window %d: %d reads with summary %+v, want %d with %+v", i, k, wl.Windows[k].Reads, wl.Windows[k].Read, len(want), seriesOf(want))
				}
			}
			if want := pooled([][]int64{arrays[i][0][0].Read, arrays[i][0][1].Read}); wl.Read != want {
				t.Errorf("execution %d: the pooled Read is %+v, want %+v", i, wl.Read, want)
			}
			churn.Windows, churn.Commit, churn.Read, churn.Wait = wl.Windows, wl.Commit, wl.Read, wl.Wait
			if churn.Raw, err = encodeRaw(raw, RawLimit, FileLimit); err != nil {
				t.Fatalf("encode execution %d: %v", i, err)
			}
		}

		// The stopped collection left its second window without a probe: with
		// every reader supplying reads for both windows, assembly still rejects it.
		if wl, raw, n, err := assemble(cs, ws, splitReads(arrays[2][0])); err == nil || len(wl.Windows) != 0 || raw != nil || n != 0 {
			t.Errorf("assembling a stopped collection with a probe-empty window: error %v, %d windows, %d raw windows, %d reads; want an error and nothing else", err, len(wl.Windows), len(raw), n)
		}
		// A window whose readers together supply no read is rejected although
		// every window has its probes; the identical collection whose readers
		// together do supply it succeeds, though no reader supplies both windows.
		first, second := arrays[0][0][0].Read, arrays[0][0][1].Read
		sparse := func(last []int64) [][][]int64 { return [][][]int64{{first, nil}, {nil, last}} }
		if wl, _, n, err := assemble(got[0].commits, got[0].waits, sparse(second)); err != nil || n != int64(len(first)+len(second)) ||
			wl.Windows[0].Reads != int64(len(first)) || wl.Windows[1].Reads != int64(len(second)) {
			t.Errorf("assembling windows whose reads come from different readers: error %v, %d reads", err, n)
		}
		if wl, raw, n, err := assemble(got[0].commits, got[0].waits, sparse(nil)); err == nil || len(wl.Windows) != 0 || raw != nil || n != 0 {
			t.Errorf("assembling a read-empty window with every probe present: error %v, %d windows, %d raw windows, %d reads; want an error and nothing else", err, len(wl.Windows), len(raw), n)
		}
		path := filepath.Join(t.TempDir(), compareFile)
		var out bytes.Buffer
		if err := runSeries(&out, modeCompare, path, func(i int) Run { return execs[i] }, quiet); err != nil {
			t.Fatalf("compare series: %v\n%s", err, out.String())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		capture, err := DecodeRun(data)
		if err != nil {
			t.Fatalf("the captured bytes: %v", err)
		}
		if len(capture.Executions) != len(table) {
			t.Fatalf("the capture holds %d executions", len(capture.Executions))
		}
		for i, e := range capture.Executions {
			churn := e.Workloads[0]
			raw, err := gunzipWindows(churn.Raw)
			if churn.Name != WorkloadChurn || err != nil || len(raw) != windows || len(churn.Windows) != windows {
				t.Fatalf("execution %d %s: %d raw windows and %d windows: %v", i, churn.Name, len(raw), len(churn.Windows), err)
			}
			for k := range raw {
				admits, commits := spanArrays(table[i], k)
				if !slices.Equal(raw[k].Wait, admits) {
					t.Errorf("execution %d window %d: the captured raw Wait is %v, want the admission spans %v", i, k, raw[k].Wait, admits)
				}
				if !slices.Equal(raw[k].Commit, commits) {
					t.Errorf("execution %d window %d: the captured raw Commit is %v, want the commit spans %v", i, k, raw[k].Commit, commits)
				}
				if !slices.Equal(raw[k].Read, arrays[i][0][k].Read) {
					t.Errorf("execution %d window %d: the captured raw Read is %v, want %v", i, k, raw[k].Read, arrays[i][0][k].Read)
				}
				if want := arrays[i][0][k].Read; churn.Windows[k].Reads != int64(len(want)) || churn.Windows[k].Read != seriesOf(want) {
					t.Errorf("execution %d window %d: the captured window has %d reads with summary %+v, want %d with %+v", i, k, churn.Windows[k].Reads, churn.Windows[k].Read, len(want), seriesOf(want))
				}
				if churn.Windows[k].Probes != perWindow || len(raw[k].Wait) != perWindow || len(raw[k].Commit) != perWindow {
					t.Errorf("execution %d window %d: %d probes, %d waits and %d commits, want %d", i, k, churn.Windows[k].Probes, len(raw[k].Wait), len(raw[k].Commit), perWindow)
				}
			}
		}
		if capture.Kind != KindCompare || Consistent(capture) != "" || Recorded(capture) != "" {
			t.Fatalf("captured run: kind %q, consistent %q, recorded %q", capture.Kind, Consistent(capture), Recorded(capture))
		}
		// The windows, pools and every median are derived again from the
		// span tables' arrays.
		if err := matchArrays(capture, arrays); err != nil {
			t.Fatalf("the capture is not what the independent spans imply: %v", err)
		}
		// The writer medians are not one execution's: each metric's third
		// smallest value comes from the execution that holds it.
		holders := map[int]bool{}
		for _, pick := range []func(RawWindow) []int64{func(a RawWindow) []int64 { return a.Wait }, func(a RawWindow) []int64 { return a.Commit }} {
			for m := range 3 {
				value := func(i int) int64 {
					s := pooled([][]int64{pick(arrays[i][0][0]), pick(arrays[i][0][1])})
					return [...]int64{s.P50, s.P95, s.P99}[m]
				}
				values := make([]int64, len(arrays))
				for i := range values {
					values[i] = value(i)
				}
				slices.Sort(values)
				for i := range values {
					if value(i) == values[len(values)/2] {
						holders[i] = true
					}
				}
			}
		}
		if len(holders) < 2 {
			t.Fatalf("every churn writer median comes from execution %v", holders)
		}
	})
}
