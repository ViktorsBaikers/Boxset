package main

// The samples below are synthetic: their values are chosen, never measured. What
// they do carry is the series' own identities, and every test gets an image of
// its own — a temporary /src whose run directories are published the way the
// image holds them (files 0444, run and samples directories 0555, the ancestors
// sticky) and whose manifest lists them — so the judge is exercised through the
// same reader a recorded run must pass.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	storebench "github.com/ViktorsBaikers/Boxset/bench/store"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/server"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const (
	testImage  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testInputs = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testDate   = "20261017"

	// Chosen values: well inside the budget, past it, and the control's delay.
	onBudgetNS = int64(20 * 1_000_000)
	slowBudget = int64(120 * 1_000_000)
	controlNS  = int64(150 * 1_000_000)
)

// testBase is the series' instant, fixed: nothing here reads the wall clock.
var testBase = time.Date(2026, 10, 17, 10, 15, 0, 0, time.UTC).Unix()

// testRun names the series' seven runs: one UTC second apart, so the series is
// strictly increasing by construction.
func testRun(i int) string { return fmt.Sprintf("bench-20261017T10150%dZ", i) }

// validRecord is an independent serialized control: four physical same-NUMA
// cores without SMT siblings, no foreign workload, 4 GiB with no container
// swap, no swap activity, a read-only empty ext4 NVMe /config and one UTC date,
// with a data block that passes the data rules.
func validRecord(stage, run string) benchprofile.Record {
	r := benchprofile.Record{
		Envelope: benchprofile.Envelope{Schema: benchprofile.RecordSchema, Stage: stage, Run: run, Image: testImage, Inputs: testInputs},
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

// ---- the synthetic series ---------------------------------------------------------

// series is one synthetic seven-run series: the headers its runs wrote and the
// records they published.
type series struct {
	headers []header
	timings [seriesRuns][]timingRecord
	writers [seriesRuns][]writerRecord
}

// newSeries builds an eligible production series: every position's own ORDER and
// GOMAXPROCS, both A/A arms on budget, a complete writer series and the
// position-7 retention observation.
func newSeries() *series {
	s := &series{}
	for i := range seriesRuns {
		p := seriesPositions[i]
		procs := p.procs
		if procs == 0 {
			procs = 4
		}
		s.headers = append(s.headers, header{
			Schema: samplesSchema, Kind: samplesKind, Run: testRun(i + 1), Protocol: protocolID,
			Order: p.order, Procs: procs, DataDir: dataMount, Inputs: testInputs, Digest: testDigest,
			Class: classProduction, Tags: "", CGO: "0", Trimpath: true, Race: "false", Cover: "",
			Go: "go1.27.2", GOOS: "linux", GOARCH: "amd64",
			StartDate: testDate, EndDate: testDate,
			Streams: streamCounts{Healthy: healthyStreams, Slow: slowStreams},
			Frames:  500, Gaps: 0, SlowClosed: 0, Background: min(2, max(1, procs/2)), Purge: 1_000,
			Peak: 2 << 30, PeakMax: memoryMaxBytes, OOMKill: 0,
		})
		for _, arm := range []string{armA1, armA2} {
			s.timings[i] = append(s.timings[i], timingRecord{
				Kind: recordTiming, Run: testRun(i + 1), Arm: arm, Order: p.order, Procs: procs, Digest: testDigest,
				Starts: repeat(onBudgetNS, 100),
			})
			s.writers[i] = append(s.writers[i], syntheticWriter(testRun(i+1), arm, p.order, procs))
		}
	}
	s.headers[seriesRuns-1].Retention = syntheticRetention()
	return s
}

// syntheticWriter is one arm's complete writer series: 240 in-load probes of
// 1,100 scheduled ticks, attributed to a producer interval the probes started in.
func syntheticWriter(run, arm, order string, procs int) writerRecord {
	const inLoad = 240
	rec := writerRecord{
		Kind: recordWriter, Run: run, Arm: arm, Order: order, Procs: procs, Digest: testDigest,
		Scheduled: writerScheduled, Skipped: writerScheduled - inLoad, InLoad: inLoad,
		FirstNS: int64(time.Second), LastNS: int64(60 * time.Second),
	}
	for i := range inLoad {
		rec.StartsNS = append(rec.StartsNS, int64(time.Second)+int64(i)*int64(10*time.Millisecond))
		rec.WaitRaw = append(rec.WaitRaw, int64(1_000+i))
		rec.CommitRaw = append(rec.CommitRaw, int64(2_000+i))
	}
	rec.WaitP50, rec.WaitP95, rec.WaitP99 = summarize(rec.WaitRaw)
	rec.CommitP50, rec.CommitP95, rec.CommitP99 = summarize(rec.CommitRaw)
	return rec
}

// syntheticRetention is the position-7 saturation as the protocol pins it.
func syntheticRetention() retention {
	return retention{
		Present: true, Phases: append([]string(nil), retentionPhases...),
		HeldReads: retentionReads, QueueFull: "queue_full", QueueRequests: 0,
		Released: retentionReads, BodyBytes: retentionBodies, DecodedBytes: retentionDecoded,
		DecodedSHA: testDigest, ServedSHA: testDigest,
		ProbeSuccesses: retentionProbes - 1, ProbeRequests: retentionProbes,
		KeysAdded: 2 + 2*(retentionProbes-1), ProbeBudget: "budget_exhausted",
		ProbeLimit: "budget_keys", ProbeMax: retentionKeyMax, Repeat: "ok",
		HostsVerified: retentionReads + retentionProbes,
		ChildRSS:      minChildRSS, ChildSlots: 1, ChildReaped: true,
	}
}

func repeat(v int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// setSeries replaces one arm's job-start samples on every timing run.
func (s *series) setStarts(value int64) {
	for i := range timingRuns {
		for arm := range s.timings[i] {
			s.timings[i][arm].Starts = repeat(value, 100)
		}
	}
}

// setSideStarts replaces one arm's job-start samples on every timing run.
func (s *series) setSideStarts(arm string, value int64) {
	for i := range timingRuns {
		for j := range s.timings[i] {
			if s.timings[i][j].Arm == arm {
				s.timings[i][j].Starts = repeat(value, 100)
			}
		}
	}
}

// firstWriter rewrites the first writer record of the series through fn.
func (s *series) firstWriter(fn func(*writerRecord)) { fn(&s.writers[0][0]) }

// shiftWriter adds delta to one arm's commit samples on every timing run and
// re-summarizes them, so the baseline it feeds is the raw series' own.
func (s *series) shiftWriter(arm string, delta int64) {
	for i := range timingRuns {
		for j := range s.writers[i] {
			w := &s.writers[i][j]
			if w.Arm != arm {
				continue
			}
			for k := range w.CommitRaw {
				w.CommitRaw[k] += delta
			}
			w.CommitP50, w.CommitP95, w.CommitP99 = summarize(w.CommitRaw)
		}
	}
}

// ---- the image and its publication -------------------------------------------------

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
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{filepath.Join(dir, "samples"), dir} {
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

// sampleBytes serializes one run's samples, through the transform a case wants
// its wire to carry.
func sampleBytes(t *testing.T, set *sampleSet, edit func([]byte) []byte) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), samplesName)
	if err := writeSamples(p, set); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		data = edit(data)
	}
	return data
}

// publish publishes the whole series as the image's seven recorded runs.
func (s *series) publish(t *testing.T, env *judgeEnv, edit func([]byte) []byte) []string {
	t.Helper()
	dirs := make([]string, 0, seriesRuns)
	for i, run := range s.runs() {
		files := map[string][]byte{"samples/" + samplesName: sampleBytes(t, &sampleSet{Header: s.headers[i], Timings: s.timings[i], Writers: s.writers[i]}, edit)}
		for _, stage := range benchprofile.RecordStages {
			data, err := json.Marshal(validRecord(stage, run))
			if err != nil {
				t.Fatal(err)
			}
			files[stage+benchprofile.FactFileNameSuffix] = data
		}
		dirs = append(dirs, publishFiles(t, env, run, files))
	}
	return dirs
}

func (s *series) runs() []string {
	runs := make([]string, 0, seriesRuns)
	for i := range seriesRuns {
		runs = append(runs, testRun(i+1))
	}
	return runs
}

// judged runs the final judge over one series of the image.
func judged(t *testing.T, env *judgeEnv, dirs []string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	return judgeWith(env, dirs, &out), out.String()
}

// ---- the command and the composition ----------------------------------------------

func TestCommandUsageExitCode(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"extra"},
		{"-out", "x", "-order", "AB", "-judge", "y"},
		{"-out", "x"},
		{"-order", "AB", "-child-hold", "256MiB"},
		{"-out", "x", "-order", "AC"},
		{"-out", "x", "-order", "AB", "-position", "8"},
	} {
		if code := run(args, io.Discard, io.Discard); code != exitUsage {
			t.Fatalf("args %q: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// TestHarnessServeComposition proves the step main runs after its data-dir device
// and eligibility gate: the harness starts through server.Start, reaches ready,
// probes through that composition's store, accepts a job through its runtime and
// stops with the data directory's lock released.
func TestHarnessServeComposition(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	serving, err := startServing(ctx, dir, outbound.Config{}, port)
	if err != nil {
		t.Fatalf("startServing: %v", err)
	}
	if serving.State != server.Ready {
		t.Fatalf("State = %q, want ready", serving.State)
	}
	if serving.Effects == nil || serving.Effects.DB == nil || serving.Effects.Jobs == nil || serving.Effects.Outbound == nil {
		t.Fatalf("the composition = %+v, want the one store, runtime and client", serving.Effects)
	}
	// The probe loop runs through the composition's own store: exactly the
	// committed increments the store foreground series counts.
	if err := storebench.CreateProbe(ctx, serving.Effects.DB); err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	before := committedProbes(t, serving.Effects.DB)
	for range 5 {
		if _, _, err := storebench.Probe(ctx, time.Now, time.Since, serving.Effects.DB.Write); err != nil {
			t.Fatalf("Probe: %v", err)
		}
	}
	if got := committedProbes(t, serving.Effects.DB); got != before+5 {
		t.Fatalf("committed probes = %d, want %d", got, before+5)
	}

	// An accepted job through the composition's runtime reaches succeeded: the
	// harness seeds its admin and spends that caller's share.
	hasher := auth.NewHasher()
	admin, err := auth.CreateUser(ctx, serving.Effects.DB, hasher, "harness-admin", benchPassword, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// The composition's own stream, opened with an admin key and left with a nil
	// Publish: the default NewEffects selects is what must publish the accepted
	// job's frames.
	_, key, err := serving.Effects.Keys.Create(ctx, admin.Username, "harness-stream", "admin")
	if err != nil {
		t.Fatalf("Keys.Create: %v", err)
	}
	stream := newFrameStream(true)
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	go stream.open(streamCtx, newEventClient(), fmt.Sprintf("http://127.0.0.1:%d", port), key, nil)
	if err := stream.hello(ctx); err != nil {
		t.Fatalf("the composition's stream: %v", err)
	}
	job, err := serving.Effects.Jobs.Accept(ctx, jobs.Caller{UserID: admin.ID}, jobs.KindJobsPrune)
	if err != nil {
		t.Fatalf("Accept(%s): %v", jobs.KindJobsPrune, err)
	}
	// One attempt publishes its claim and its finish; the claim's running frame
	// names the accepted job, so a default that publishes nothing fails here.
	if err := stream.await(ctx, fmt.Sprintf("%d", job.ID), store.JobRunning); err != nil {
		t.Fatalf("job %d's running frame: %v", job.ID, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		page, err := serving.Effects.Jobs.List(ctx, jobs.Query{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		state := ""
		for _, row := range page.Items {
			if row.ID == job.ID {
				state = row.State
			}
		}
		if state == store.JobSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d state = %q, want %q", job.ID, state, store.JobSucceeded)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := stopServing(serving); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	again, err := install.Acquire(dir)
	if err != nil {
		t.Fatalf("the data directory after Stop: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestBenchLogKeepsOnlyWarnings proves the harness logger's level: the measured
// run's own per-request lines are what the runner's output cap measures, so the
// log keeps warnings and errors and nothing quieter.
func TestBenchLogKeepsOnlyWarnings(t *testing.T) {
	log, closeLog, err := benchLog("bench-effects")
	if err != nil {
		t.Fatalf("benchLog: %v", err)
	}
	defer func() { _ = closeLog() }()
	if log.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("a measured run's log keeps per-request info lines")
	}
	if !log.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("a measured run's log drops warnings")
	}
}

// committedProbes is the probe table's own count, read through the store.
func committedProbes(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), `SELECT n FROM bench_probe WHERE id = 1`).Scan(&n)
	}); err != nil {
		t.Fatalf("read bench_probe: %v", err)
	}
	return n
}

// ---- the arm's own sample ----------------------------------------------------------

// TestArmEndsOnAnUnobservedStart proves the arm's own refusal: an accepted job
// whose start no frame ever announced leaves the sampler with the recorded reason
// rather than a number standing in for it, and a start frame that did arrive is
// the sample even when its finish was read first.
func TestArmEndsOnAnUnobservedStart(t *testing.T) {
	t.Run("a start that was never framed ends the arm", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), startWait+time.Second)
		defer cancel()
		s := newFrameStream(true)
		// What a runtime that publishes no running transition still sends: the
		// finish of the attempt whose start was never an event.
		s.frame("job", `{"job":{"id":7,"state":"succeeded"}}`, "1.boot.9")
		if err := s.await(ctx, "7", store.JobRunning); err == nil || err.Error() != "job_start_unobserved" {
			t.Fatalf("await = %v, want job_start_unobserved", err)
		}
	})
	t.Run("a start read before its finish is still the sample", func(t *testing.T) {
		s := newFrameStream(true)
		s.frame("job", `{"job":{"id":8,"state":"running"}}`, "1.boot.10")
		s.frame("job", `{"job":{"id":8,"state":"succeeded"}}`, "1.boot.11")
		if err := s.await(t.Context(), "8", store.JobRunning); err != nil {
			t.Fatalf("await = %v, want the recorded start", err)
		}
	})
}

// TestFrameStreamWaitsForHello pins the hello wait that gates the accept loop:
// the hello branch sets started, a job frame only wakes the wait, the stream's
// own end is hello_unobserved, and neither arriving is hello_deadline.
func TestFrameStreamWaitsForHello(t *testing.T) {
	t.Run("a job frame wakes the wait without satisfying it", func(t *testing.T) {
		s := newFrameStream(true)
		s.frame("job", `{"job":{"id":7,"state":"running"}}`, "1.boot.1")
		// A job frame wakes the wait but never starts the stream: the hello
		// branch is the only writer of started, and it is read under the mutex.
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if started {
			t.Fatal("a job frame set started, want the hello frame to")
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.hello(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("hello = %v on a job frame, want it to keep waiting", err)
		}
		s.frame("hello", "", "1.boot.2")
		if err := s.hello(t.Context()); err != nil {
			t.Fatalf("hello = %v, want nil after the hello frame", err)
		}
	})
	t.Run("a stream that ended first is hello_unobserved", func(t *testing.T) {
		s := newFrameStream(true)
		close(s.done)
		start := time.Now()
		err := s.hello(t.Context())
		if err == nil || err.Error() != "hello_unobserved" {
			t.Fatalf("hello = %v, want hello_unobserved", err)
		}
		if elapsed := time.Since(start); elapsed >= startWait/2 {
			t.Fatalf("hello_unobserved took %s, want well under %s", elapsed, startWait)
		}
	})
	t.Run("neither arriving is hello_deadline", func(t *testing.T) {
		s := newFrameStream(true)
		start := time.Now()
		err := s.hello(t.Context())
		if err == nil || err.Error() != "hello_deadline" {
			t.Fatalf("hello = %v, want hello_deadline", err)
		}
		if elapsed := time.Since(start); elapsed < startWait {
			t.Fatalf("hello_deadline after %s, want the %s bound", elapsed, startWait)
		}
	})
}

// TestSlowEventClientConnects pins the slow streams' small receive window: a
// refused socket option would leave both slow streams unopened, which the run
// would report as an absent resync rather than a bounded-queue result.
func TestSlowEventClientConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	resp, err := newSlowEventClient().Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("slow client: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("slow client status = %d, want 200", resp.StatusCode)
	}
}

// TestSlowStreamStallsPastTheQueueAndDrainsToResync pins the slow stream's read
// policy: after hello it stops consuming until the hold ends, so the bounded
// delivery can end it, and only the drain that follows reaches the resync frame
// the protocol accepts as that stream's close.
func TestSlowStreamStallsPastTheQueueAndDrainsToResync(t *testing.T) {
	s := newFrameStream(false)
	s.hold = 250 * time.Millisecond
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	go func() {
		defer func() { _ = writer.Close() }()
		fmt.Fprint(writer, "id: 1.b.0\nevent: hello\ndata: {\"watermark\":0}\n\n")
		fmt.Fprint(writer, "id: 1.b.1\nevent: users\ndata: {\"revision\":1}\n\n")
		fmt.Fprint(writer, "id: 1.b.1\nevent: resync\ndata: {\"reason\":\"overflow\"}\n\n")
	}()
	done := make(chan struct{})
	go func() {
		s.read(t.Context(), reader)
		close(done)
	}()
	if err := s.hello(t.Context()); err != nil {
		t.Fatalf("hello: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n, _, resync := s.totals(); n != 0 || resync {
		t.Fatalf("before the hold ended the stream had read %d frames (resync %t), want the hello alone", n, resync)
	}
	<-done
	n, gaps, resync := s.totals()
	if n != 1 || gaps != 0 || !resync {
		t.Fatalf("after the drain: frames=%d gaps=%d resync=%t, want the one frame and its resync", n, gaps, resync)
	}
}

// TestStopReadersAtCommonFrameEqualisesTheTail pins the arm's end: the stop
// target is the highest frame any reader parsed, so a reader one behind waits
// for that frame instead of being cancelled mid-tail.
func TestStopReadersAtCommonFrameEqualisesTheTail(t *testing.T) {
	a, b := newFrameStream(false), newFrameStream(false)
	a.count, a.last = 5, 5
	b.count, b.last = 5, 6
	stopReadersAtCommonFrame([]*frameStream{a, b})
	for i, s := range []*frameStream{a, b} {
		if s.stopAt != 6 {
			t.Fatalf("reader %d stopAt = %d, want the common target 6", i, s.stopAt)
		}
	}
	if a.atTarget() || !b.atTarget() {
		t.Fatalf("atTarget = %t/%t, want only the reader on the target to stop", a.atTarget(), b.atTarget())
	}
}

// TestWaitConvergedHoldsForTheLaggard pins the arm's end: a reader one frame
// ahead is not the comparison's answer until the others catch up, so the wait
// cannot return before the laggard does.
func TestWaitConvergedHoldsForTheLaggard(t *testing.T) {
	lead, lag := newFrameStream(false), newFrameStream(false)
	set := func(s *frameStream, n int64) {
		s.mu.Lock()
		s.count, s.last = n, n
		s.mu.Unlock()
	}
	set(lead, 5)
	set(lag, 4)
	caught := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		set(lag, 5)
		close(caught)
	}()
	waitConverged(t.Context(), []*frameStream{lead, lag}, 10*time.Millisecond, time.Second)
	select {
	case <-caught:
	default:
		t.Fatal("waitConverged returned before the laggard caught up")
	}
}

// TestStreamTotalsRefusesADesyncedReader pins the ten reading streams' control:
// equal counts are the arm's evidence that no reader fell behind, so a resync or
// a different count is the arm's answer rather than the fewest frames.
func TestStreamTotalsRefusesADesyncedReader(t *testing.T) {
	a, b := newFrameStream(false), newFrameStream(false)
	a.count, b.count = 5, 5
	if n, gaps, err := streamTotals([]*frameStream{a, b}, nil); err != nil || n != 5 || gaps != 0 {
		t.Fatalf("totals = %d/%d, %v, want the equal count and no gaps", n, gaps, err)
	}
	b.count = 6
	if _, _, err := streamTotals([]*frameStream{a, b}, nil); err == nil ||
		!strings.HasPrefix(err.Error(), "stream_mismatch ") ||
		!strings.Contains(err.Error(), "healthy=[0:frames=5 gaps=0 resync=false 1:frames=6 gaps=0 resync=false]") {
		t.Fatalf("mismatched counts = %v, want stream_mismatch naming both readers", err)
	}
	b.count, b.resync = 5, "resync"
	slow := newFrameStream(false)
	close(slow.done)
	slow.resync = "resync"
	if _, _, err := streamTotals([]*frameStream{a, b}, []*frameStream{slow}); err == nil ||
		!strings.HasPrefix(err.Error(), "stream_resync ") ||
		!strings.Contains(err.Error(), "1:frames=5 gaps=0 resync=true") ||
		!strings.Contains(err.Error(), "slow=[0:closed=true resync=true") {
		t.Fatalf("a resynced reader = %v, want stream_resync naming the reader and the slow close", err)
	}
}

// TestWriterRecordIsAttributed pins what a writer series carries into a run:
// its arm and order, the executing GOMAXPROCS, and the world's own run and
// digest — the two values the world is constructed with, so the record is seeded
// from the series' identity rather than holding an empty or foreign one. A
// cancelled context ends the probe before its first tick, so the record is the
// seed.
func TestWriterRecordIsAttributed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &world{run: testRun(1), digest: testDigest}
	rec := w.probeWriters(ctx, nil, armA1, orderAB, time.Now(), nil)
	if rec.Run != testRun(1) || rec.Arm != armA1 || rec.Order != orderAB || rec.Procs != runtime.GOMAXPROCS(0) || rec.Digest != testDigest {
		t.Fatalf("writer record = %+v, want this run's identity", rec)
	}
	if !rec.validShape() {
		t.Fatalf("writer record = %+v, want a shape the wire accepts", rec)
	}
}

// TestRecordStartIsBounded pins the write side of the wire's raw cap: a series
// already at maxRaw is not extended and the truncation is surfaced, because a
// judge would otherwise accept the short series and a truncated arm could pass.
func TestRecordStartIsBounded(t *testing.T) {
	full := repeat(1, maxRaw)
	if _, err := recordStart(full, 7); err == nil || err.Error() != "starts_truncated" {
		t.Fatalf("a full series was extended without a word: %v", err)
	}
	if got, err := recordStart(nil, 9); err != nil || len(got) != 1 || got[0] != 9 {
		t.Fatalf("starts = %v, err %v, want the one admitted start", got, err)
	}
}

// TestPaceSpansTheArmUnderTheRawBound pins the accept loop's schedule: a stub
// that samples the instant it is allowed still writes a series under the wire's
// raw bound and spanning the arm, and the real-shaped arm still carries at least
// the in-load minimum.
func TestPaceSpansTheArmUnderTheRawBound(t *testing.T) {
	var instants []time.Time
	start := time.Unix(0, 0)
	wait := func(_ context.Context, at time.Time) error {
		instants = append(instants, at)
		return nil
	}
	if err := pace(context.Background(), start, armWindow, sampleInterval, wait, func() error { return nil }); err != nil {
		t.Fatalf("pace = %v, want the schedule to run out the arm", err)
	}
	t.Logf("samples = %d over %s at %s", len(instants), armWindow, sampleInterval)
	if len(instants) > maxRaw {
		t.Fatalf("samples = %d, want at most the wire's raw bound %d", len(instants), maxRaw)
	}
	if len(instants) < minInLoad {
		t.Fatalf("samples = %d, want at least the in-load minimum %d", len(instants), minInLoad)
	}
	for i, at := range instants {
		if want := start.Add(time.Duration(i) * sampleInterval); !at.Equal(want) {
			t.Fatalf("sample %d at %s, want %s", i, at, want)
		}
	}
	if last := instants[len(instants)-1].Sub(start); last+sampleInterval < armWindow {
		t.Fatalf("last sample at %s, want the series to span the %s arm", last, armWindow)
	}
}

// TestProbeAttributionCutsTheProducerInterval pins the harness side of the
// judge's writer_interval rule: a probe that started before the producer's first
// request or after its last is not in load, so it is counted with the skipped
// ticks instead of being written as a sample the judge would refuse.
func TestProbeAttributionCutsTheProducerInterval(t *testing.T) {
	starts := []int64{10, 20, 30, 40}
	waits := []int64{1, 2, 3, 4}
	commits := []int64{5, 6, 7, 8}
	gotStarts, gotWaits, gotCommits, outside := attributeProbes(starts, waits, commits, 20, 30)
	if outside != 2 {
		t.Fatalf("outside = %d, want the probe before first and the one after last", outside)
	}
	if !slices.Equal(gotStarts, []int64{20, 30}) || !slices.Equal(gotWaits, []int64{2, 3}) || !slices.Equal(gotCommits, []int64{6, 7}) {
		t.Fatalf("in-load series = %v/%v/%v, want 20,30 / 2,3 / 6,7", gotStarts, gotWaits, gotCommits)
	}
}

// TestRetentionCountsFollowThePeer pins the saturation's derived counts against
// the synthetic peer that answers its phases: the probe series is what the peer
// answered after the held reads, every served probe charges its host and ceiling
// key beside the reads' two, and the verified hosts are the peer's own count.
func TestRetentionCountsFollowThePeer(t *testing.T) {
	peer, err := newSyntheticPeer()
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	defer func() { _ = peer.close() }()
	base := "http://127.0.0.1:" + strconv.Itoa(peer.port)
	held := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, base+"/hold/0", nil)
		if err != nil {
			held <- err
			return
		}
		req.Host = barrierHost
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			held <- err
			return
		}
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		held <- err
	}()
	deadline := time.Now().Add(startWait)
	for peer.heldReads() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the peer never held the read")
		}
		time.Sleep(time.Millisecond)
	}
	peer.releaseHeld()
	if err := <-held; err != nil {
		t.Fatalf("held read: %v", err)
	}
	probe := func(host string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+"/probe/1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("probe %s: %v", host, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("probe %s: status %d", host, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	before := peer.requests()
	probe("probe-1.effects.test")
	probe("probe-1.effects.test")
	probeRequests, keysAdded, hostsVerified := retentionFrom(retentionCounts{
		probes:    peer.requests() - before,
		successes: 2,
		answered:  peer.requests(),
	})
	if probeRequests != 2 || keysAdded != 2+2*2 || hostsVerified != 1+2 || hostsVerified != peer.requests() {
		t.Fatalf("counts = %d probes, %d keys, %d hosts, want 2/6/3 from the peer's %d requests",
			probeRequests, keysAdded, hostsVerified, peer.requests())
	}
	// The protocol's own numbers must satisfy the judge's clauses; a drift here
	// is a record the judge would refuse.
	if _, keys, hosts := retentionFrom(retentionCounts{probes: retentionProbes, successes: retentionProbes - 1, answered: retentionReads + retentionProbes}); keys != retentionKeyMax || hosts != retentionReads+retentionProbes {
		t.Fatalf("protocol counts = %d keys, %d hosts, want %d/%d", keys, hosts, retentionKeyMax, retentionReads+retentionProbes)
	}
}

// ---- the judge's controls ----------------------------------------------------------

// refuse builds a fresh image and series, lets edit mutate the series before it is
// published and wire rewrite the samples bytes, and returns the judge's answer.
func refuse(t *testing.T, edit func(*series), wire func([]byte) []byte) (int, string) {
	t.Helper()
	env := testEnv(t)
	s := newSeries()
	if edit != nil {
		edit(s)
	}
	return judged(t, env, s.publish(t, env, wire))
}

// wantInvalid requires the judge to refuse one series with one reason token.
func wantInvalid(t *testing.T, reason string, edit func(*series), wire func([]byte) []byte) {
	t.Helper()
	code, out := refuse(t, edit, wire)
	if want := "EFFECTS-BENCH-INVALID reason=" + reason + "\n"; code != exitInvalid || out != want {
		t.Fatalf("exit %d: %q, want INVALID %s", code, out, reason)
	}
}

// wantFail requires the judge to fail one series for one reason token.
func wantFail(t *testing.T, reason string, edit func(*series)) {
	t.Helper()
	code, out := refuse(t, edit, nil)
	if code != exitFail || !strings.Contains(out, "EFFECTS-BENCH-FAIL reason="+reason+" ") {
		t.Fatalf("exit %d: %q, want FAIL %s", code, out, reason)
	}
}

// tamper rewrites one published run file, keeping the image's lock and manifest
// the way publication would.
func tamper(t *testing.T, env *judgeEnv, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	for _, d := range []string{filepath.Dir(p), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(p, 0o644); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Publication locks a file after it wrote it: the mode argument of an
	// existing file is ignored, so the lock is its own step.
	for _, locked := range []string{p, filepath.Dir(p), dir} {
		if err := os.Chmod(locked, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Dir(p), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	syncManifest(t, env)
}

func TestJudgePassesEligibleSeries(t *testing.T) {
	env := testEnv(t)
	dirs := newSeries().publish(t, env, nil)
	code, out := judged(t, env, dirs)
	if code != exitPass || !strings.Contains(out, "EFFECTS-BENCH-PASS ") {
		t.Fatalf("exit %d: %q", code, out)
	}
	if strings.Contains(out, "EFFECTS-BENCH-FAIL") || strings.Contains(out, "EFFECTS-BENCH-INVALID") {
		t.Fatalf("a passing series printed another verdict: %q", out)
	}
	// The writer lines are descriptive: one per arm and metric, and the A/A
	// spread of the two arm medians beside it.
	if got := strings.Count(out, "EFFECTS-WRITER-BASELINE arm="); got != 12 {
		t.Fatalf("writer baseline lines = %d, want 12: %q", got, out)
	}
	if got := strings.Count(out, "EFFECTS-WRITER-SPREAD metric="); got != 6 {
		t.Fatalf("writer spread lines = %d, want 6: %q", got, out)
	}
	for _, want := range []string{"job_start_p95_a1_ns=20000000", "job_start_p95_a2_ns=20000000", "noise_ns=0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verdict line %q lacks %q", out, want)
		}
	}
	if a1, a2 := baselineMedian(t, out, armA1, "commit_p50_ns"), baselineMedian(t, out, armA2, "commit_p50_ns"); a1 == 0 || a2 == 0 {
		t.Fatalf("commit_p50 medians = %d/%d, want both arms present: %q", a1, a2, out)
	}
}

// TestWriterBaselineFollowsTheRaw pins the descriptive writer baseline: both
// arms print for one metric, and shifting one arm's raw samples moves its median
// by that shift while the other arm's stays put — the line is derived from the
// samples, never from a stored constant.
func TestWriterBaselineFollowsTheRaw(t *testing.T) {
	env := testEnv(t)
	code, out := judged(t, env, newSeries().publish(t, env, nil))
	if code != exitPass {
		t.Fatalf("exit %d: %q", code, out)
	}
	before := baselineMedian(t, out, armA2, "commit_p50_ns")
	other := baselineMedian(t, out, armA1, "commit_p50_ns")

	const shift = 5_000
	shifted := newSeries()
	shifted.shiftWriter(armA2, shift)
	env = testEnv(t)
	code, out = judged(t, env, shifted.publish(t, env, nil))
	if code != exitPass {
		t.Fatalf("exit %d: %q", code, out)
	}
	if got := baselineMedian(t, out, armA2, "commit_p50_ns"); got != before+shift {
		t.Fatalf("shifted median = %d, want %d from raw %d", got, before+shift, before)
	}
	if got := baselineMedian(t, out, armA1, "commit_p50_ns"); got != other {
		t.Fatalf("unshifted median = %d, want %d", got, other)
	}
}

// baselineMedian reads one descriptive writer line's median from the judge's
// output, and fails the test when that arm and metric did not print.
func baselineMedian(t *testing.T, out, arm, metric string) int64 {
	t.Helper()
	prefix := "EFFECTS-WRITER-BASELINE arm=" + arm + " metric=" + metric + " median="
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			t.Fatalf("median %q: %v", rest, err)
		}
		return n
	}
	t.Fatalf("no baseline line for arm=%s metric=%s: %q", arm, metric, out)
	return 0
}

func TestJudgeFailsSlowControl(t *testing.T) {
	env := testEnv(t)
	s := newSeries()
	for i := range s.headers {
		s.headers[i].Class, s.headers[i].Tags = classControl, tagSlow
	}
	s.setStarts(controlNS)
	code, out := judged(t, env, s.publish(t, env, nil))
	if code != exitFail || !strings.Contains(out, "EFFECTS-BENCH-FAIL reason=job_start_budget ") {
		t.Fatalf("exit %d: %q, want the control failed", code, out)
	}
}

func TestJudgeFailsJobStartBudget(t *testing.T) {
	env := testEnv(t)
	s := newSeries()
	s.setStarts(slowBudget)
	code, out := judged(t, env, s.publish(t, env, nil))
	if code != exitFail || !strings.Contains(out, "EFFECTS-BENCH-FAIL reason=job_start_budget ") {
		t.Fatalf("exit %d: %q, want the budget failed", code, out)
	}
}

func TestJudgeInvalidJobStartSeries(t *testing.T) {
	t.Run("noise above the budget", func(t *testing.T) {
		wantInvalid(t, "noise", func(s *series) {
			s.setStarts(controlNS)
			s.setSideStarts(armA1, controlNS-budgetNS-1)
		}, nil)
	})
	t.Run("straddling sides", func(t *testing.T) {
		wantInvalid(t, "within_noise", func(s *series) {
			s.setSideStarts(armA1, budgetNS*3/5)
			s.setSideStarts(armA2, budgetNS+budgetNS/5)
		}, nil)
	})
	t.Run("a missing arm", func(t *testing.T) {
		wantInvalid(t, "timing_missing", func(s *series) { s.timings[3] = s.timings[3][:1] }, nil)
	})
	t.Run("a repeated arm", func(t *testing.T) {
		wantInvalid(t, "timing_missing", func(s *series) { s.timings[2] = append(s.timings[2], s.timings[2][0]) }, nil)
	})
	t.Run("an unattributed arm", func(t *testing.T) {
		wantInvalid(t, "timing_attribution", func(s *series) { s.timings[1][0].Procs = 7 }, nil)
	})
	t.Run("a timing series from another digest", func(t *testing.T) {
		wantInvalid(t, "timing_attribution", func(s *series) {
			s.timings[1][0].Digest = strings.Repeat("d", benchprofile.HexSHA256Len)
		}, nil)
	})
	t.Run("a control series that meets the budget", func(t *testing.T) {
		wantInvalid(t, "control", func(s *series) {
			for i := range s.headers {
				s.headers[i].Class, s.headers[i].Tags = classControl, tagSlow
			}
		}, nil)
	})
}

func TestJudgeRefusesIneligibleRuns(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		run    func(t *testing.T, env *judgeEnv) []string
	}{
		{
			name: "V1 records only", reason: "record_incomplete",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				facts := benchprofile.Facts{
					Envelope: benchprofile.Envelope{Schema: benchprofile.FactSchema, Stage: benchprofile.StageBefore, Run: testRun(1), Image: testImage, Inputs: testInputs},
				}
				data, err := json.Marshal(facts)
				if err != nil {
					t.Fatal(err)
				}
				tamper(t, env, dirs[0], benchprofile.StageBefore+benchprofile.FactFileNameSuffix, data)
				return dirs
			},
		},
		{
			name: "a missing record", reason: "record_missing",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				p := filepath.Join(dirs[2], benchprofile.StageDuring+benchprofile.FactFileNameSuffix)
				if err := os.Chmod(dirs[2], 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dirs[2], 0o555); err != nil {
					t.Fatal(err)
				}
				syncManifest(t, env)
				return dirs
			},
		},
		{
			name: "a partial record set", reason: "record_incomplete",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				v1, err := json.Marshal(benchprofile.Facts{
					Envelope: benchprofile.Envelope{Schema: benchprofile.FactSchema, Stage: benchprofile.StageDuring, Run: testRun(4), Image: testImage, Inputs: testInputs},
				})
				if err != nil {
					t.Fatal(err)
				}
				tamper(t, env, dirs[3], benchprofile.StageDuring+benchprofile.FactFileNameSuffix, v1)
				return dirs
			},
		},
		{
			name: "ineligible data storage", reason: "data_filesystem",
			run: func(t *testing.T, env *judgeEnv) []string {
				s := newSeries()
				dirs := s.publish(t, env, nil)
				rec := validRecord(benchprofile.StageDuring, testRun(5))
				rec.Data.FS = benchprofile.ConfigFSOther
				data, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				tamper(t, env, dirs[4], benchprofile.StageDuring+benchprofile.FactFileNameSuffix, data)
				return dirs
			},
		},
		{
			name: "six runs", reason: "run_count",
			run: func(t *testing.T, env *judgeEnv) []string {
				return newSeries().publish(t, env, nil)[:seriesRuns-1]
			},
		},
		{
			name: "a repeated run under another spelling", reason: "run_duplicate",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				dirs[1] = filepath.Join(dirs[0], ".")
				return dirs
			},
		},
		{
			name: "an unordered series", reason: "run_order",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				dirs[0], dirs[1] = dirs[1], dirs[0]
				return dirs
			},
		},
		{
			name: "a run reached through a link", reason: "run_location",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				link := env.img.RunPath(testRun(seriesRuns))
				if err := os.Chmod(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{filepath.Join(link, "samples", samplesName), filepath.Join(link, "samples"), link} {
					if err := os.Chmod(p, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.RemoveAll(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dirs[0], link); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(link), os.ModeSticky|0o777); err != nil {
					t.Fatal(err)
				}
				dirs[seriesRuns-1] = link
				return dirs
			},
		},
		{
			name: "a run outside the image", reason: "run_location",
			run: func(t *testing.T, env *judgeEnv) []string {
				dirs := newSeries().publish(t, env, nil)
				dirs[6] = filepath.Join(t.TempDir(), testRun(7))
				return dirs
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testEnv(t)
			dirs := c.run(t, env)
			code, out := judged(t, env, dirs)
			if want := "EFFECTS-BENCH-INVALID reason=" + c.reason + "\n"; code != exitInvalid || out != want {
				t.Fatalf("exit %d: %q, want INVALID %s", code, out, c.reason)
			}
		})
	}
}

func TestJudgeRefusesIneligibleSamples(t *testing.T) {
	t.Run("a V1-only data mode", func(t *testing.T) {
		wantInvalid(t, "samples_malformed", func(s *series) { s.headers[0].Schema = samplesSchema + 1 }, nil)
	})
	t.Run("samples outside the data mount", func(t *testing.T) {
		wantInvalid(t, "samples_malformed", func(s *series) { s.headers[2].DataDir = "/tmp/effects" }, nil)
	})
	t.Run("a non-production build", func(t *testing.T) {
		wantInvalid(t, "binary_class", func(s *series) {
			for i := range s.headers {
				s.headers[i].CGO = "1"
			}
		}, nil)
	})
	t.Run("a label-only class", func(t *testing.T) {
		wantInvalid(t, "binary_class", func(s *series) { s.headers[3].Tags = tagSlow }, nil)
	})
	t.Run("a race build", func(t *testing.T) {
		wantInvalid(t, "binary_class", func(s *series) {
			for i := range s.headers {
				s.headers[i].Race = "true"
			}
		}, nil)
	})
	t.Run("a cover build", func(t *testing.T) {
		wantInvalid(t, "binary_class", func(s *series) {
			for i := range s.headers {
				s.headers[i].Cover = "set"
			}
		}, nil)
	})
	t.Run("mixed digests", func(t *testing.T) {
		wantInvalid(t, "identity_mismatch", func(s *series) {
			s.headers[3].Digest = strings.Repeat("d", benchprofile.HexSHA256Len)
			for j := range s.timings[3] {
				s.timings[3][j].Digest = s.headers[3].Digest
			}
			for j := range s.writers[3] {
				s.writers[3][j].Digest = s.headers[3].Digest
			}
		}, nil)
	})
	t.Run("mixed settings", func(t *testing.T) {
		wantInvalid(t, "identity_mismatch", func(s *series) { s.headers[1].Go = "go1.26.0" }, nil)
	})
	t.Run("mixed protocols", func(t *testing.T) {
		wantInvalid(t, "protocol", func(s *series) { s.headers[5].Protocol = "effects-job-start-v0" }, nil)
	})
	t.Run("another run's number", func(t *testing.T) {
		wantInvalid(t, "run_mismatch", func(s *series) { s.headers[2].Run = testRun(4) }, nil)
	})
	t.Run("a wrong ORDER for the position", func(t *testing.T) {
		wantInvalid(t, "order", func(s *series) {
			s.headers[1].Order = orderAB
			for j := range s.timings[1] {
				s.timings[1][j].Order = orderAB
			}
			for j := range s.writers[1] {
				s.writers[1][j].Order = orderAB
			}
		}, nil)
	})
	t.Run("a wrong GOMAXPROCS for the position", func(t *testing.T) {
		wantInvalid(t, "procs", func(s *series) {
			s.headers[5].Procs = 2
			for j := range s.timings[5] {
				s.timings[5][j].Procs = 2
			}
			for j := range s.writers[5] {
				s.writers[5][j].Procs = 2
			}
		}, nil)
	})
	t.Run("a start the run never sampled", func(t *testing.T) {
		wantInvalid(t, "samples_malformed", func(s *series) { s.timings[4][1].Starts = nil }, nil)
	})
	t.Run("more starts than the wire carries", func(t *testing.T) {
		wantInvalid(t, "samples_malformed", func(s *series) {
			s.timings[4][1].Starts = repeat(onBudgetNS, maxRaw+1)
		}, nil)
	})
	t.Run("an empty writer series", func(t *testing.T) {
		wantInvalid(t, "writer_missing", func(s *series) { s.writers[4] = s.writers[4][1:] }, nil)
	})
	t.Run("a short writer series", func(t *testing.T) {
		wantInvalid(t, "writer_short", func(s *series) {
			s.firstWriter(func(w *writerRecord) {
				w.InLoad, w.Skipped = minInLoad-1, writerScheduled-minInLoad+1
				w.StartsNS, w.WaitRaw, w.CommitRaw = w.StartsNS[:minInLoad-1], w.WaitRaw[:minInLoad-1], w.CommitRaw[:minInLoad-1]
			})
		}, nil)
	})
	t.Run("a probe open at the end", func(t *testing.T) {
		wantInvalid(t, "writer_open", func(s *series) { s.firstWriter(func(w *writerRecord) { w.Open = true }) }, nil)
	})
	t.Run("a sample started before the interval", func(t *testing.T) {
		wantInvalid(t, "writer_interval", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.StartsNS[0] = w.FirstNS - 1 })
		}, nil)
	})
	t.Run("a sample started after the interval", func(t *testing.T) {
		wantInvalid(t, "writer_interval", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.StartsNS[len(w.StartsNS)-1] = w.LastNS + 1 })
		}, nil)
	})
	t.Run("an unattributed writer series", func(t *testing.T) {
		wantInvalid(t, "writer_attribution", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.Order = orderBA })
		}, nil)
	})
	t.Run("a writer series copied from another run", func(t *testing.T) {
		wantInvalid(t, "writer_attribution", func(s *series) { s.writers[4] = s.writers[2] }, nil)
	})
	t.Run("a writer series from another digest", func(t *testing.T) {
		wantInvalid(t, "writer_attribution", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.Digest = strings.Repeat("d", benchprofile.HexSHA256Len) })
		}, nil)
	})
	t.Run("a summary without raw", func(t *testing.T) {
		wantInvalid(t, "writer_raw", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.WaitRaw = nil })
		}, nil)
	})
	t.Run("a summary the raw does not produce", func(t *testing.T) {
		wantInvalid(t, "writer_summary", func(s *series) {
			s.firstWriter(func(w *writerRecord) { w.CommitP99++ })
		}, nil)
	})
	t.Run("a self-reported writer verdict", func(t *testing.T) {
		wantInvalid(t, "samples_incomplete", nil, func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"kind":"writer"`), []byte(`"kind":"writer","writer_verdict":"pass"`), 1)
		})
	})
	t.Run("a container memory limit that is not the runner's", func(t *testing.T) {
		wantInvalid(t, "memory_limit", func(s *series) { s.headers[1].PeakMax = memoryMaxBytes / 2 }, nil)
	})
	t.Run("no container memory observation", func(t *testing.T) {
		wantInvalid(t, "memory_unobserved", func(s *series) { s.headers[2].Peak = 0 }, nil)
	})
	t.Run("no position-7 retention", func(t *testing.T) {
		wantInvalid(t, "retention_missing", func(s *series) { s.headers[seriesRuns-1].Retention = retention{} }, nil)
	})
	t.Run("retention on another position", func(t *testing.T) {
		wantInvalid(t, "retention_unexpected", func(s *series) { s.headers[2].Retention = syntheticRetention() }, nil)
	})
	t.Run("retention phases out of order", func(t *testing.T) {
		wantInvalid(t, "samples_malformed", func(s *series) {
			s.headers[seriesRuns-1].Retention.Phases = []string{"refuse", "hold", "release", "probes", "child"}
		}, nil)
	})
	t.Run("a retention count that lies", func(t *testing.T) {
		wantInvalid(t, "retention_keys", func(s *series) {
			s.headers[seriesRuns-1].Retention.KeysAdded = 2 + 2*(retentionProbes-2)
		}, nil)
	})
	t.Run("a probe count that lies", func(t *testing.T) {
		wantInvalid(t, "retention_probes", func(s *series) {
			s.headers[seriesRuns-1].Retention.ProbeSuccesses = retentionProbes
		}, nil)
	})
	t.Run("a probe budget that never exhausted", func(t *testing.T) {
		wantInvalid(t, "retention_probes", func(s *series) {
			s.headers[seriesRuns-1].Retention.ProbeBudget = "budget_hosts"
		}, nil)
	})
	t.Run("a queue refusal that reached the peer", func(t *testing.T) {
		wantInvalid(t, "retention_queue", func(s *series) {
			s.headers[seriesRuns-1].Retention.QueueRequests = 1
		}, nil)
	})
	t.Run("a queue refusal of another reason", func(t *testing.T) {
		wantInvalid(t, "retention_queue", func(s *series) {
			s.headers[seriesRuns-1].Retention.QueueFull = "overflow"
		}, nil)
	})
	t.Run("a decoded payload from another service", func(t *testing.T) {
		wantInvalid(t, "retention_digest", func(s *series) {
			s.headers[seriesRuns-1].Retention.ServedSHA = strings.Repeat("e", benchprofile.HexSHA256Len)
		}, nil)
	})
	t.Run("a decoded digest that is not hex", func(t *testing.T) {
		wantInvalid(t, "retention_digest", func(s *series) {
			s.headers[seriesRuns-1].Retention.DecodedSHA = "not-a-digest"
			s.headers[seriesRuns-1].Retention.ServedSHA = "not-a-digest"
		}, nil)
	})
	t.Run("a held read that was not released", func(t *testing.T) {
		wantInvalid(t, "retention_reads", func(s *series) {
			s.headers[seriesRuns-1].Retention.Released = retentionReads - 1
		}, nil)
	})
	t.Run("a held read that was never observed", func(t *testing.T) {
		wantInvalid(t, "retention_reads", func(s *series) {
			s.headers[seriesRuns-1].Retention.HeldReads = retentionReads - 1
		}, nil)
	})
	t.Run("a body size the protocol did not pin", func(t *testing.T) {
		wantInvalid(t, "retention_decode", func(s *series) {
			s.headers[seriesRuns-1].Retention.BodyBytes = retentionBodies - 1
		}, nil)
	})
	t.Run("a decoded size the protocol did not pin", func(t *testing.T) {
		wantInvalid(t, "retention_decode", func(s *series) {
			s.headers[seriesRuns-1].Retention.DecodedBytes = retentionDecoded - 1
		}, nil)
	})
	t.Run("a probe limit from another budget", func(t *testing.T) {
		wantInvalid(t, "retention_budget", func(s *series) {
			s.headers[seriesRuns-1].Retention.ProbeLimit = "budget_hosts"
		}, nil)
	})
	t.Run("a probe ceiling that is not the cache bound", func(t *testing.T) {
		wantInvalid(t, "retention_budget", func(s *series) {
			s.headers[seriesRuns-1].Retention.ProbeMax = retentionKeyMax - 1
		}, nil)
	})
	t.Run("a probe count that is not the requests", func(t *testing.T) {
		wantInvalid(t, "retention_requests", func(s *series) {
			s.headers[seriesRuns-1].Retention.ProbeRequests = retentionProbes - 1
		}, nil)
	})
	t.Run("a repeat that was not served", func(t *testing.T) {
		wantInvalid(t, "retention_repeat", func(s *series) {
			s.headers[seriesRuns-1].Retention.Repeat = "refused"
		}, nil)
	})
	t.Run("a host count the peer did not verify", func(t *testing.T) {
		wantInvalid(t, "retention_hosts", func(s *series) {
			s.headers[seriesRuns-1].Retention.HostsVerified = retentionReads + retentionProbes - 1
		}, nil)
	})
	t.Run("a child that held no slot", func(t *testing.T) {
		wantInvalid(t, "child_slots", func(s *series) {
			s.headers[seriesRuns-1].Retention.ChildSlots = 0
		}, nil)
	})
	t.Run("a child that held more slots than the reads", func(t *testing.T) {
		wantInvalid(t, "child_slots", func(s *series) {
			s.headers[seriesRuns-1].Retention.ChildSlots = retentionReads + 1
		}, nil)
	})
	t.Run("an honest non-production build", func(t *testing.T) {
		wantInvalid(t, "binary_class", func(s *series) {
			for i := range s.headers {
				s.headers[i].Class, s.headers[i].Race = "", "true"
			}
		}, nil)
	})
	t.Run("a child that never became resident", func(t *testing.T) {
		wantInvalid(t, "child_resident", func(s *series) {
			s.headers[seriesRuns-1].Retention.ChildRSS = minChildRSS - 1
		}, nil)
	})
	t.Run("a child that was never reaped", func(t *testing.T) {
		wantInvalid(t, "child_unreaped", func(s *series) {
			s.headers[seriesRuns-1].Retention.ChildReaped = false
		}, nil)
	})
}

func TestJudgeFailsCorrectness(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		edit   func(*series)
	}{
		{"a missing healthy stream", "streams", func(s *series) { s.headers[0].Streams.Healthy = healthyStreams - 1 }},
		{"a missing slow stream", "slow_streams", func(s *series) { s.headers[6].Streams.Slow = slowStreams - 1 }},
		{"a gap in a healthy stream", "stream_gaps", func(s *series) { s.headers[3].Gaps = 1 }},
		{"a stream that received nothing", "stream_gaps", func(s *series) { s.headers[4].Frames = 0 }},
		{"background work past the ceiling", "background", func(s *series) { s.headers[6].Background = 64/2 + 1 }},
		{"no purge progress", "purge_progress", func(s *series) { s.headers[5].Purge = 0 }},
		{"an oom kill", "oom_kill", func(s *series) { s.headers[6].OOMKill = 1 }},
		{"memory at the container limit", "memory_peak", func(s *series) { s.headers[6].Peak = memoryMaxBytes }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantFail(t, c.reason, c.edit) })
	}
}

func TestJudgePrintsNoBaselineForAnInvalidSeries(t *testing.T) {
	code, out := refuse(t, func(s *series) { s.headers[3].Digest = strings.Repeat("f", benchprofile.HexSHA256Len) }, nil)
	if code != exitInvalid || strings.Contains(out, "EFFECTS-WRITER-BASELINE") {
		t.Fatalf("exit %d: %q, want an INVALID series without writer lines", code, out)
	}
}

// TestMeasureRefusesIneligibleInputs is the harness's own gate: before it runs
// anything it refuses a data directory that is not the runner's own device, a
// build with no class, and a child size the protocol does not pin.
func TestMeasureRefusesIneligibleInputs(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv("BOXSET_DATA_DIR", absent)
	if _, err := dataDir(); err == nil || err.Error() != "data_dir" {
		t.Fatalf("a data directory outside the runner's mount: %v, want data_dir", err)
	}
	if err := distinctDevice(absent); err == nil || err.Error() != "data_dir_device" {
		t.Fatalf("a data directory that does not exist: %v, want data_dir_device", err)
	}
	// /tmp is one of the devices the rule compares, so a directory made there
	// shares its device wherever the test runs.
	sameDevice, err := os.MkdirTemp("/tmp", "effects-device-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sameDevice) })
	if err := distinctDevice(sameDevice); err == nil || err.Error() != "data_dir_device" {
		t.Fatalf("a data directory sharing /tmp's device: %v, want data_dir_device", err)
	}
	for _, build := range []header{
		{Tags: "integration", CGO: "0", Trimpath: true},
		{CGO: "1", Trimpath: true},
		{CGO: "0", Trimpath: false},
		{CGO: "0", Trimpath: true, Race: "true"},
		{CGO: "0", Trimpath: true, Cover: "set"},
	} {
		if classOf(build) != "" {
			t.Fatalf("build %+v was given a class", build)
		}
	}
	if class := classOf(header{CGO: "0", Trimpath: true}); class != classProduction {
		t.Fatalf("the production build's class = %q", class)
	}
	if class := classOf(header{CGO: "0", Trimpath: true, Tags: tagSlow}); class != classControl {
		t.Fatalf("the control build's class = %q", class)
	}
	for _, size := range []string{"128MiB", "256", "", "256MiB "} {
		if _, err := parseSize(size); err == nil {
			t.Fatalf("child size %q was accepted", size)
		}
	}
	if _, err := parseSize(childHoldSize); err != nil {
		t.Fatalf("the recorded child size: %v", err)
	}
	// A measurement whose output does not name a run directory is refused before
	// any store, port or composition exists.
	if code := run([]string{"-out", filepath.Join(t.TempDir(), "notarun", "samples"), "-order", orderAB}, io.Discard, io.Discard); code != exitFail {
		t.Fatalf("exit %d, want a refusal for a run directory that is not one", code)
	}
}

// ---- the harness's own oracles ----------------------------------------------------

// TestSlowOpenClauseObservesTheStreams pins the DEC-060 clause's harness half: a
// slow stream counts only when its hello was parsed over a connected request, so
// a stream refused before hello fails the arm with its open error named, and the
// observed count — never the recorded shape — is what the run records.
func TestSlowOpenClauseObservesTheStreams(t *testing.T) {
	t.Run("a stream refused before hello fails the arm", func(t *testing.T) {
		opened := newFrameStream(false)
		opened.frame("hello", "", "1.boot.0")
		refused := newFrameStream(false)
		refused.setErr("status: 401 Unauthorized")
		close(refused.done)
		count, err := slowOpened([]*frameStream{opened, refused})
		if count != 1 {
			t.Fatalf("opened = %d, want only the connected stream", count)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "slow_open") || !strings.Contains(err.Error(), "401 Unauthorized") {
			t.Fatalf("slowOpened = %v, want slow_open naming the refusal", err)
		}
	})
	t.Run("both streams opened is the observed count", func(t *testing.T) {
		a, b := newFrameStream(false), newFrameStream(false)
		a.frame("hello", "", "1.boot.0")
		b.frame("hello", "", "1.boot.1")
		count, err := slowOpened([]*frameStream{a, b})
		if err != nil || count != slowStreams {
			t.Fatalf("slowOpened = %d, %v, want the %d opened streams", count, err, slowStreams)
		}
	})
}

// TestAdmissionSampleIsTheFrameReceipt pins the pinned quantity: the sample is
// Accept's return to the running frame's receipt, so a wait that ends later than
// the frame it waited for cannot inflate it, and only the frame naming this job
// and state is read.
func TestAdmissionSampleIsTheFrameReceipt(t *testing.T) {
	s := newFrameStream(true)
	accepted := time.Unix(1, 0)
	running := jobFrameKey("7", store.JobRunning)
	s.mu.Lock()
	s.seen[running] = true
	s.receipts[running] = accepted.Add(7 * time.Millisecond)
	other := jobFrameKey("8", store.JobRunning)
	s.seen[other] = true
	s.receipts[other] = accepted.Add(50 * time.Millisecond)
	s.mu.Unlock()
	// The observation instant is far later than the frame's receipt.
	time.Sleep(20 * time.Millisecond)
	ns, err := s.startSample(t.Context(), accepted, "7")
	if err != nil {
		t.Fatalf("startSample: %v", err)
	}
	if ns != int64(7*time.Millisecond) {
		t.Fatalf("sample = %d ns, want the running frame's receipt (7 ms), not the instant the wait ended", ns)
	}
	// The claim can publish before Accept returns: such a receipt is a zero
	// sample, never a negative one.
	early := jobFrameKey("9", store.JobRunning)
	s.mu.Lock()
	s.seen[early] = true
	s.receipts[early] = accepted.Add(-time.Millisecond)
	s.mu.Unlock()
	if ns, err := s.startSample(t.Context(), accepted, "9"); err != nil || ns != 0 {
		t.Fatalf("a receipt before the return = %d ns, %v, want 0", ns, err)
	}
}

// TestProducerStopFailsTheArmBeforeItsWindow pins the producer's completion: a
// failed PATCH before the window is the arm's named failure, never a short series
// the writer interval quietly adopts, while a completed window and one the arm's
// own cancellation ended are not failures.
func TestProducerStopFailsTheArmBeforeItsWindow(t *testing.T) {
	p := newProducer(context.Background(), nil, "", "", nil, time.Now())
	p.err = errors.New("patch_refused")
	close(p.done)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := p.waitFirst(ctx)
	if err == nil || !strings.HasPrefix(err.Error(), "producer_incomplete") || !strings.Contains(err.Error(), "patch_refused") {
		t.Fatalf("waitFirst = %v, want producer_incomplete naming patch_refused", err)
	}
	if err := (&producer{completed: true}).stopped(); err != nil {
		t.Fatalf("a completed producer: %v", err)
	}
	if err := (&producer{}).stopped(); err != nil {
		t.Fatalf("a producer the arm's cancellation ended: %v", err)
	}
}

// TestUsersFramesFollowProducerSuccesses pins the publisher-side count: every
// successful PATCH published one users frame, so a reader short of the producer's
// own answers is the arm's answer once the bound ends.
func TestUsersFramesFollowProducerSuccesses(t *testing.T) {
	a, b := newFrameStream(false), newFrameStream(false)
	a.frame("users", `{"collectionRevision":1}`, "1.boot.1")
	b.frame("users", `{"collectionRevision":1}`, "1.boot.2")
	if err := waitUsersPublished(t.Context(), []*frameStream{a, b}, 1, 100*time.Millisecond); err != nil {
		t.Fatalf("one PATCH per reader: %v", err)
	}
	err := waitUsersPublished(t.Context(), []*frameStream{a, b}, 2, 20*time.Millisecond)
	if err == nil || !strings.HasPrefix(err.Error(), "producer_frames 1/2") {
		t.Fatalf("a reader short of the producer's answers: %v, want producer_frames 1/2", err)
	}
}

// TestRetentionRunsOnlyAtTheRecordedMemoryPosition pins the gating: the
// position-7 saturation follows the run's recorded place, so a timing position
// never records one however many cores the host has, and the memory position
// always does.
func TestRetentionRunsOnlyAtTheRecordedMemoryPosition(t *testing.T) {
	cases := []struct {
		name     string
		explicit int
		order    string
		exported string
		want     int
	}{
		{"a timing position exports no GOMAXPROCS", 0, orderAB, "", 0},
		{"the other timing order", 0, orderBA, "", 0},
		{"an unrecorded core count is a timing position", 0, orderAB, "128", 0},
		{"the progress position", 0, orderAB, "1", 6},
		{"the memory position", 0, orderAB, "64", 7},
		{"an explicit position wins over the environment", 3, orderAB, "64", 3},
		{"an explicit memory position", 7, orderAB, "", 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos := positionOf(c.explicit, c.order, c.exported)
			if pos != c.want {
				t.Fatalf("positionOf(%d, %s, %q) = %d, want %d", c.explicit, c.order, c.exported, pos, c.want)
			}
			if got, want := runsRetention(pos), pos == seriesRuns; got != want {
				t.Fatalf("runsRetention(%d) = %t, want %t", pos, got, want)
			}
		})
	}
}

// TestWaitConvergedSettlesOrStopsAtItsBound pins both ends of the convergence
// wait: equal readers hold for the settle it was given, and a bound shorter than
// the settle ends the wait instead of waiting the settle out.
func TestWaitConvergedSettlesOrStopsAtItsBound(t *testing.T) {
	equal := func(n int64) []*frameStream {
		a, b := newFrameStream(false), newFrameStream(false)
		for _, s := range []*frameStream{a, b} {
			s.mu.Lock()
			s.count, s.last = n, n
			s.mu.Unlock()
		}
		return []*frameStream{a, b}
	}
	t.Run("equal readers hold for the settle", func(t *testing.T) {
		start := time.Now()
		waitConverged(t.Context(), equal(5), 50*time.Millisecond, 10*time.Second)
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed >= 10*time.Second {
			t.Fatalf("waitConverged returned after %s, want the 50 ms settle inside the 10 s bound", elapsed)
		}
	})
	t.Run("a bound shorter than the settle wins", func(t *testing.T) {
		start := time.Now()
		waitConverged(t.Context(), equal(5), 5*time.Second, 20*time.Millisecond)
		if elapsed := time.Since(start); elapsed >= 5*time.Second || elapsed < 10*time.Millisecond {
			t.Fatalf("waitConverged returned after %s, want the 20 ms bound before the 5 s settle", elapsed)
		}
	})
}

// TestStopReadersAtCommonFrameHoldsEveryReaderLock pins the common-frame claim: a
// reader advancing under its lock cannot slip between the target being chosen and
// written, so the target includes the advance and the stop cannot complete while
// a reader holds the lock.
func TestStopReadersAtCommonFrameHoldsEveryReaderLock(t *testing.T) {
	a, b := newFrameStream(false), newFrameStream(false)
	a.count, a.last = 5, 5
	b.count, b.last = 5, 6
	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		a.mu.Lock()
		a.count, a.last = 6, 7
		close(locked)
		<-release
		a.mu.Unlock()
	}()
	<-locked
	stopped := make(chan struct{})
	go func() {
		stopReadersAtCommonFrame([]*frameStream{a, b})
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stopReadersAtCommonFrame completed while a reader held its lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stopReadersAtCommonFrame did not finish after the reader released its lock")
	}
	if a.stopAt != 7 || b.stopAt != 7 {
		t.Fatalf("stopAt = %d/%d, want the target including the advance under the lock", a.stopAt, b.stopAt)
	}
}

// TestSlowClientSmallReceiveWindow pins the slow streams' kernel oracle: the
// socket carries the recorded small receive window, not the host's autotuned
// default, so a build that drops the setsockopt fails here.
func TestSlowClientSmallReceiveWindow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	conn, err := slowDialer().DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("slow dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dial returned %T, want *net.TCPConn", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		got, getErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatalf("SO_RCVBUF: %v", getErr)
	}
	// Linux reports twice the requested buffer, so twice the recorded window is
	// the ceiling a default (autotuned) buffer cannot meet.
	if got <= 0 || got > 2*slowReceiveWindow {
		t.Fatalf("SO_RCVBUF = %d, want the recorded %d-byte window (at most %d)", got, slowReceiveWindow, 2*slowReceiveWindow)
	}
}

// TestSleepUntilAtHonoursCancellation pins one bounded wait's error path: a
// closed context ends the wait with its error rather than the instant, and a past
// instant returns at once.
func TestSleepUntilAtHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepUntilAt(ctx, time.Now().Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepUntilAt = %v, want the cancelled wait", err)
	}
	if err := sleepUntilAt(t.Context(), time.Now().Add(-time.Millisecond)); err != nil {
		t.Fatalf("sleepUntilAt on a past instant = %v, want nil", err)
	}
}
