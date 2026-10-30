package main

// The effects load one run measures: the accounts and staged alias generations
// the installation is seeded with, the event streams and the PATCH producer that
// make its fan-out, the job the admin R accepts after every succeeded frame, the
// writer probe that ticks beside them, and the position-7 saturation of the
// outbound client and the child ceiling.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	storebench "github.com/ViktorsBaikers/Boxset/bench/store"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/server"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const (
	// The seeded installation: thirteen admin credentials (the admin R, ten
	// reading streams and two that never read), one viewer V, and six staged
	// alias generations of 200,000 aliases per arm. Activating them retires
	// five, which is the background purge load of one execution.
	adminAccounts        = 13
	benchPassword        = "synthetic-fixture-password"
	generationsPerArm    = 6
	armedGenerations     = 2 * generationsPerArm
	aliasesPerGeneration = 200_000
	datasetName          = "effects-dataset"

	// The measured window: the streams are up, the producer runs 50/s for 60 s,
	// and the writer probe ticks every 50 ms from its fifth second.
	patchWindow  = 60 * time.Second
	patchRate    = 50
	writerStart  = 5 * time.Second
	writerPeriod = 50 * time.Millisecond

	// The accept loop's sampling pace: one admitted start per interval, so the
	// series spans the whole window at half the wire's raw bound instead of
	// reaching that bound in the arm's first seconds.
	sampleInterval = patchWindow / (maxRaw / 2)

	// slowHold is the slow streams' read policy: after hello they stop
	// consuming until two thirds of the window, so the bounded delivery ends
	// them — by the hub's overflow resync or by the product's frame deadline —
	// before the arm's own end. The rest of the window is that drain.
	slowHold = patchWindow * 2 / 3

	// streamSettle is how long the healthy readers must hold the same last
	// frame after publishing stops, so the stop target is the arm's final frame
	// and not one still in flight.
	streamSettle = 500 * time.Millisecond

	// startWait bounds one admitted job's running frame: a sample that never
	// arrives fails the execution instead of recording a zero.
	startWait = 2 * time.Second
	drainWait = 10 * time.Second

	// armWindow bounds one arm: the producer's window plus the drain that lets
	// the readers converge after publishing stops. The harness records the
	// invocation's actual timed span beside it, so the two-arm total is a
	// measured number rather than a bound the run is assumed to meet.
	armWindow = patchWindow + drainWait

	// slowReceiveWindow is the slow streams' requested receive buffer: small
	// enough that the publisher's stream fills the kernel buffers instead of the
	// loopback autotuner absorbing it, so the hub's own bounded queue is what
	// overflows.
	slowReceiveWindow = 4096

	eventsPath = "/api/v1/events"
	usersPath  = "/api/v1/users/"
	keyHeader  = "X-Api-Key"

	// openLookups bounds the harness's concurrent stream opens: the server admits
	// eight credential lookups per peer at once, so thirteen streams opened from
	// one peer are presented in smaller waves instead of all at the gate.
	openLookups = 4

	// The pinned retention names and the phase bound.
	barrierHost = "barrier.effects.test"
	probeHost   = "probe-%d.effects.test"
	phaseBudget = 40 * time.Second
)

// credential is one seeded admin: its row and the API key it speaks as.
type credential struct {
	user auth.User
	key  string
}

// viewerTarget is the viewer V the producer edits, with the revision its next
// PATCH must carry.
type viewerTarget struct {
	id       int64
	revision int64
	disabled bool
}

// world is one run's seeded installation and the identities the load speaks as.
// It holds no store, runtime or client of its own: every one of those is the
// composition's.
type world struct {
	run    string
	data   string
	port   int
	env    outbound.Config
	peer   *syntheticPeer
	digest string
	admins []credential
	viewer viewerTarget
	title  library.TitleID
	staged []library.GenerationID
	// firstAccept is armed only by the out-of-band arm case: the accept loop
	// closes it after its first Accept returns, so a test can tell whether a
	// job was accepted before R's hello. A measured run leaves it nil.
	firstAccept chan struct{}
}

// seedWorld seeds one installation through the production composition and stops
// it: what the measured composition starts on is a routine installation with
// accounts, staged alias generations and the probe table, and nothing running.
func seedWorld(ctx context.Context, dir string, id identity) (*world, error) {
	peer, err := newSyntheticPeer()
	if err != nil {
		return nil, err
	}
	w := &world{data: dir, env: outbound.Config{Resolver: peerResolver{}}, peer: peer, digest: id.digest}
	if w.port, err = freePort(); err != nil {
		return nil, err
	}
	serving, err := startServing(ctx, dir, w.env, w.port)
	if err != nil {
		return nil, err
	}
	if serving.State != server.Ready {
		_ = stopServing(serving)
		return nil, errors.New("seed_not_ready")
	}
	if err := w.seed(ctx, serving); err != nil {
		_ = stopServing(serving)
		return nil, err
	}
	if err := stopServing(serving); err != nil {
		return nil, err
	}
	return w, nil
}

// seed creates what the load is measured against, all of it through the
// composition's own owners: accounts and keys through the auth owner, the
// library, its generations and their aliases through the library owner, and the
// probe table through the store the composition runs.
func (w *world) seed(ctx context.Context, serving *server.Serving) error {
	hasher := auth.NewHasher()
	for i := range adminAccounts {
		name := fmt.Sprintf("effects%02d", i+1)
		user, err := auth.CreateUser(ctx, serving.Effects.DB, hasher, name, benchPassword, auth.RoleAdmin)
		if err != nil {
			return errors.New("seed_user")
		}
		// The admin R edits users, so its key carries the write scope; a stream
		// only reads, and no credential here holds more than the load spends.
		scope := "read"
		if i == 0 {
			scope = "admin"
		}
		_, key, err := serving.Effects.Keys.Create(ctx, name, fmt.Sprintf("effects-%02d", i+1), scope)
		if err != nil {
			return errors.New("seed_key")
		}
		w.admins = append(w.admins, credential{user: user, key: key})
	}
	viewer, err := auth.CreateUser(ctx, serving.Effects.DB, hasher, "effects-viewer", benchPassword, auth.RoleViewer)
	if err != nil {
		return errors.New("seed_viewer")
	}
	w.viewer = viewerTarget{id: viewer.ID, revision: viewer.Revision}

	lib := serving.Effects.Library
	libraryID, err := lib.AddLibrary(ctx, library.LibrarySpec{
		Name:     "effects",
		Defaults: library.LibraryDefaults{Monitored: true, NamingLanguage: "en", SearchLanguage: "en", Profile: "hd"},
		Template: []library.TemplateSlot{{Key: "hd", Primary: true}},
	})
	if err != nil {
		return errors.New("seed_library")
	}
	membership, err := lib.AddMembership(ctx, library.MembershipRequest{
		LibraryID: libraryID, MetadataID: "tvdb:effects-1", Kind: "movie", Name: "effects title",
	})
	if err != nil {
		return errors.New("seed_title")
	}
	w.title = membership.TitleID
	// The generations stay staged: activating one retires the one it replaced
	// and commits the purge that deletes it, so the load starts inside the
	// measured window instead of during the seed.
	aliases := make([]string, aliasesPerGeneration)
	for g := range armedGenerations {
		gen, err := lib.StageGeneration(ctx, datasetName)
		if err != nil {
			return errors.New("seed_generation")
		}
		for i := range aliases {
			aliases[i] = fmt.Sprintf("effects g%02d alias %06d", g, i)
		}
		if err := lib.SetAliases(ctx, w.title, &gen, aliases); err != nil {
			return errors.New("seed_aliases")
		}
		w.staged = append(w.staged, gen)
	}
	return storebench.CreateProbe(ctx, serving.Effects.DB)
}

// timeWindows runs one run's two A/A arms inside the measured region, through
// one composition on the seeded installation. pos is the run's recorded place in
// the series; only the memory position runs the retention saturation.
func timeWindows(ctx context.Context, w *world, run, order string, id identity, pos int) (*sampleSet, error) {
	w.run = run
	serving, err := startServing(ctx, w.data, w.env, w.port)
	if err != nil {
		return nil, err
	}
	if serving.State != server.Ready {
		_ = stopServing(serving)
		return nil, errors.New("composition_not_ready")
	}
	defer func() { _ = stopServing(serving) }()
	set := &sampleSet{Header: header{
		Schema: samplesSchema, Kind: samplesKind, Run: run, Protocol: protocolID,
		Order: order, Procs: runtime.GOMAXPROCS(0), DataDir: dataMount,
		Digest: id.digest, Class: id.class, Tags: id.tags, CGO: id.cgo, Trimpath: id.trimpath,
		Race: id.race, Cover: id.cover, Go: id.version, GOOS: id.goos, GOARCH: id.goarch,
		Streams: streamCounts{Healthy: healthyStreams},
	}}
	memory := runsRetention(pos)
	saturated := make(chan saturation, 1)
	if memory {
		go func() {
			ret, err := w.saturate(ctx, serving)
			saturated <- saturation{ret: ret, err: err}
		}()
	}
	var worst armCounts
	for i, arm := range orderOf(order) {
		counts, timing, writer, err := w.runArm(ctx, serving, arm, order, i == 0)
		if err != nil {
			return nil, err
		}
		set.Timings = append(set.Timings, timing)
		set.Writers = append(set.Writers, writer)
		if i == 0 {
			worst = counts
			continue
		}
		worst = worst.merge(counts)
	}
	peak, limit, oom, err := cgroupCounters()
	if err != nil {
		return nil, err
	}
	set.Header.Frames, set.Header.Gaps = worst.frames, worst.gaps
	set.Header.Streams.Slow = worst.slow
	set.Header.SlowClosed, set.Header.Background, set.Header.Purge = worst.slowClosed, worst.background, worst.purge
	set.Header.Peak, set.Header.PeakMax, set.Header.OOMKill = peak, limit, oom
	// The saturation of the client and the child ceiling belongs to the memory
	// position alone: the recorded position gates it, never the host's core
	// count, so a timing position running on many cores never records one. It
	// ran beside the arms, so the container's peak covers it.
	if memory {
		sat := <-saturated
		if sat.err != nil {
			return nil, sat.err
		}
		set.Header.Retention = sat.ret
	}
	return set, nil
}

// saturation is the position-7 phases' answer.
type saturation struct {
	ret retention
	err error
}

// orderOf is the order the two arms run in: ORDER names the first arm's slot
// first, so BA runs A2 before A1.
func orderOf(order string) [2]string {
	if order == orderBA {
		return [2]string{armA2, armA1}
	}
	return [2]string{armA1, armA2}
}

// armCounts is what one arm observed of the load's own guarantees.
type armCounts struct {
	slow       int
	frames     int64
	gaps       int64
	slowClosed int
	background int
	purge      int64
}

// merge keeps the worse of two arms: the fewest opened slow streams, the fewest
// frames, the most gaps and background claims, the fewest closed slow streams
// and the least purge progress, so one arm that missed a clause is the run's
// answer.
func (a armCounts) merge(b armCounts) armCounts {
	return armCounts{
		slow:       min(a.slow, b.slow),
		frames:     min(a.frames, b.frames),
		gaps:       max(a.gaps, b.gaps),
		slowClosed: min(a.slowClosed, b.slowClosed),
		background: max(a.background, b.background),
		purge:      min(a.purge, b.purge),
	}
}

// runArm runs one arm: the background purge load, the streams, the producer and
// the writer probe, with R accepting a job after every succeeded frame.
func (w *world) runArm(ctx context.Context, serving *server.Serving, arm, order string, first bool) (armCounts, timingRecord, writerRecord, error) {
	var counts armCounts
	timing := timingRecord{Kind: recordTiming, Run: w.run, Arm: arm, Order: order, Procs: runtime.GOMAXPROCS(0), Digest: w.digest}
	var writer writerRecord

	armCtx, cancel := context.WithTimeout(ctx, armWindow)
	defer cancel()
	purged, err := w.purgeTargets(serving, first)
	if err != nil {
		return counts, timing, writer, err
	}
	before, err := w.aliasesOf(ctx, serving, purged)
	if err != nil {
		return counts, timing, writer, err
	}

	base := "http://127.0.0.1:" + strconv.Itoa(w.port)
	client := newEventClient()
	slowClient := newSlowEventClient()
	lookup := make(chan struct{}, openLookups)
	var healthy, slow []*frameStream
	var opened sync.WaitGroup
	rStream := newFrameStream(true)
	for i := 1; i < adminAccounts; i++ {
		s := newFrameStream(false)
		streamClient := client
		if i <= healthyStreams {
			healthy = append(healthy, s)
		} else {
			// The two slow readers stall after hello over a small receive
			// window: the bounded delivery must end them before the arm does, or
			// the protocol's accepted close never comes.
			s.hold = slowHold
			streamClient = slowClient
			slow = append(slow, s)
		}
		key := w.admins[i].key
		opened.Add(1)
		go func() { defer opened.Done(); s.open(armCtx, streamClient, base, key, lookup) }()
	}
	opened.Add(1)
	go func() { defer opened.Done(); rStream.open(armCtx, client, base, w.admins[0].key, lookup) }()

	// R's stream must be subscribed before the accept loop commits a job: the
	// running frame is published once, and a subscriber admitted later has no
	// frame to sample. The gate carries the hello wait's answer, so a stream
	// that ended before it joins without accepting.
	hello := make(chan error, 1)
	firstAccept := w.firstAccept
	w.firstAccept = nil
	accepted := make(chan struct{})
	acceptErr := make(chan error, 1)
	acceptCtx, cancelAccept := context.WithCancel(armCtx)
	defer cancelAccept()
	go func() {
		defer close(accepted)
		if err := <-hello; err != nil {
			return
		}
		acceptErr <- rStream.accept(acceptCtx, serving, w.admins[0].user.ID, &timing, firstAccept)
	}()

	if err := rStream.hello(armCtx); err != nil {
		hello <- err
		<-accepted
		return counts, timing, writer, err
	}
	// Every reading stream must be subscribed before any frame is published:
	// the ten are compared frame for frame, and one admitted after the first
	// patch would miss it. The background purge and the producer start only
	// after all of them have their hello.
	for _, s := range healthy {
		if err := s.hello(armCtx); err != nil {
			hello <- err
			<-accepted
			return counts, timing, writer, err
		}
	}
	// The two slow streams are the DEC-060 clause's own observation: each is
	// opened only when its hello was parsed over a connected request. The arm
	// answers with the count it observed, never the recorded shape, and names
	// the stream's open error so one run identifies it.
	for _, s := range slow {
		_ = s.hello(armCtx)
	}
	openedSlow, err := slowOpened(slow)
	if err != nil {
		hello <- err
		<-accepted
		return counts, timing, writer, err
	}
	counts.slow = openedSlow
	if err := w.activate(ctx, serving, first); err != nil {
		hello <- err
		<-accepted
		return counts, timing, writer, err
	}
	epoch := time.Now()
	producer := newProducer(armCtx, client, base, w.admins[0].key, &w.viewer, epoch)
	opened.Add(1)
	go func() { defer opened.Done(); producer.run() }()
	hello <- nil
	if err := producer.waitFirst(armCtx); err != nil {
		// The producer stopped before its first request: there is no interval to
		// attribute the writer series to, so the arm ends here instead of
		// recording one.
		cancelAccept()
		<-accepted
		cancel()
		opened.Wait()
		return counts, timing, writer, err
	}

	background := w.observeBackground(armCtx, serving)
	probed := make(chan writerRecord, 1)
	opened.Add(1)
	go func() { defer opened.Done(); probed <- w.probeWriters(armCtx, serving, arm, order, epoch, producer) }()

	// The arm ends either when the producer's window closes or when R's
	// sampling loop refuses a start it cannot attribute: a loop that stopped
	// mid-arm is the arm's answer, never a success carrying the samples it
	// happened to collect. Once publishing has stopped, the accept loop is
	// cancelled and the healthy readers are held until all ten sit on the same
	// last frame, so the cancellation cannot truncate one reader's tail.
	var armFailure error
	select {
	case <-producer.done:
		cancelAccept()
		<-accepted
		armFailure = <-acceptErr
		if armFailure == nil {
			// A producer that stopped before its window — a refused PATCH or a
			// cancelled context — is a named failure, never a short series the
			// writer interval quietly adopts.
			armFailure = producer.stopped()
		}
		if armFailure == nil {
			// The fan-out's count is the producer's own: every successful PATCH
			// published one users frame before its answer, so a reader short of
			// that number is the arm's answer.
			armFailure = waitUsersPublished(armCtx, healthy, producer.successes.Load(), startWait)
		}
		if armFailure == nil {
			waitPruneIdle(armCtx, serving, startWait)
			waitConverged(armCtx, healthy, streamSettle, startWait)
			stopReadersAtCommonFrame(healthy)
			waitStreamsEnded(armCtx, healthy, startWait)
		}
	case <-armCtx.Done():
		// The arm did not finish inside its own bound: a truncated arm is the
		// bound's answer, not a measured one.
		armFailure = armCtx.Err()
	case armFailure = <-acceptErr:
	}
	cancel()
	opened.Wait()
	<-accepted
	writer = <-probed
	if armFailure != nil {
		return counts, timing, writer, armFailure
	}
	counts.frames, counts.gaps, err = streamTotals(healthy, slow)
	if err != nil {
		return counts, timing, writer, err
	}
	counts.slowClosed = drainSlow(slow)
	observed := <-background
	if observed.err != nil {
		return counts, timing, writer, observed.err
	}
	counts.background = observed.most
	after, err := w.aliasesOf(ctx, serving, purged)
	if err != nil {
		return counts, timing, writer, err
	}
	counts.purge = before - after
	if len(timing.Starts) == 0 {
		return counts, timing, writer, errors.New("job_start_unobserved")
	}
	if writer.FirstNS <= 0 || writer.LastNS <= writer.FirstNS {
		return counts, timing, writer, errors.New("producer_unobserved")
	}
	return counts, timing, writer, nil
}

// purgeTargets are the generations this arm's activation retires: every staged
// generation of the arm but the last, which becomes the active one.
func (w *world) purgeTargets(serving *server.Serving, first bool) ([]library.GenerationID, error) {
	start := 0
	if !first {
		start = generationsPerArm
	}
	if start+generationsPerArm > len(w.staged) {
		return nil, errors.New("generations_missing")
	}
	return w.staged[start : start+generationsPerArm-1], nil
}

// aliasesOf counts the aliases of the named generations, read through the
// composition's own store: the purge progress of an arm is the difference.
func (w *world) aliasesOf(ctx context.Context, serving *server.Serving, gens []library.GenerationID) (int64, error) {
	ids := make([]string, len(gens))
	for i, gen := range gens {
		ids[i] = strconv.FormatInt(int64(gen), 10)
	}
	var n int64
	err := serving.Effects.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM title_alias WHERE generation_id IN (`+strings.Join(ids, ",")+`)`).Scan(&n)
	})
	if err != nil {
		return 0, errors.New("alias_count")
	}
	return n, nil
}

// activate starts one arm's background purge load: the arm's staged generations
// activate in order, so the last one is active and five are retired with their
// aliases committed for deletion.
func (w *world) activate(ctx context.Context, serving *server.Serving, first bool) error {
	start := 0
	if !first {
		start = generationsPerArm
	}
	for _, gen := range w.staged[start : start+generationsPerArm] {
		if err := serving.Effects.Library.ActivateGeneration(ctx, gen); err != nil {
			return errors.New("activate_generation")
		}
	}
	return nil
}

// backgroundObservation is one arm's aggregate background reading: the most
// running attempts any tick saw, or the store's own failure. A failed tick is
// reported, never dropped: a maximum read from the ticks that succeeded would
// record a subset that reads healthy.
type backgroundObservation struct {
	most int
	err  error
}

// observeBackground samples the background lane's running attempts once a
// second and reports the most it saw: the runtime's own aggregate ceiling, read
// back where it is enforced.
func (w *world) observeBackground(ctx context.Context, serving *server.Serving) <-chan backgroundObservation {
	observed := make(chan backgroundObservation, 1)
	go func() {
		highest := 0
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				observed <- backgroundObservation{most: highest}
				return
			case <-ticker.C:
				rows, err := serving.Effects.DB.RunningJobs(ctx)
				if err != nil {
					// A cancelled context ends the sample, not the observation:
					// only a live context's failure is the arm's answer.
					if ctx.Err() != nil {
						observed <- backgroundObservation{most: highest}
						return
					}
					observed <- backgroundObservation{most: highest, err: fmt.Errorf("background_unobserved: %w", err)}
					return
				}
				n := 0
				for _, row := range rows {
					if row.Lane == store.JobBackground {
						n++
					}
				}
				highest = max(highest, n)
			}
		}
	}()
	return observed
}

// probeWriters is one arm's writer series: a probe tick every 50 ms from the
// producer's fifth second to its last request, a tick during an in-flight probe
// recorded as skipped, and every started probe completed and counted.
func (w *world) probeWriters(ctx context.Context, serving *server.Serving, arm, order string, epoch time.Time, producer *producer) writerRecord {
	rec := writerRecord{
		Kind: recordWriter, Run: w.run, Arm: arm, Order: order,
		Procs: runtime.GOMAXPROCS(0), Digest: w.digest, Scheduled: writerScheduled,
	}
	if err := sleepUntilAt(ctx, epoch.Add(writerStart)); err != nil {
		rec.Open = true
		return rec
	}
	var inFlight atomic.Bool
	var mu sync.Mutex
	var started, waits, commits []int64
	var probes sync.WaitGroup
	for tick := range writerScheduled {
		if err := sleepUntilAt(ctx, epoch.Add(writerStart+time.Duration(tick)*writerPeriod)); err != nil {
			break
		}
		at := time.Since(epoch).Nanoseconds()
		if !inFlight.CompareAndSwap(false, true) {
			rec.Skipped++
			continue
		}
		probes.Add(1)
		go func(at int64) {
			defer probes.Done()
			wait, commit, err := storebench.Probe(ctx, time.Now, time.Since, serving.Effects.DB.Write)
			mu.Lock()
			inFlight.Store(false)
			if err == nil {
				started, waits, commits = append(started, at), append(waits, int64(wait)), append(commits, int64(commit))
			}
			mu.Unlock()
		}(at)
	}
	settled := make(chan struct{})
	go func() { probes.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(drainWait):
		rec.Open = true
	}
	mu.Lock()
	// The producer's own bounds: a probe started outside them is not in load.
	rec.FirstNS, rec.LastNS = producer.first.Load(), producer.last.Load()
	loadStarts, loadWaits, loadCommits, outside := attributeProbes(started, waits, commits, rec.FirstNS, rec.LastNS)
	rec.Skipped += outside
	rec.InLoad = len(loadStarts)
	rec.StartsNS, rec.WaitRaw, rec.CommitRaw = loadStarts, loadWaits, loadCommits
	mu.Unlock()
	if rec.InLoad > 0 {
		rec.WaitP50, rec.WaitP95, rec.WaitP99 = summarize(loadWaits)
		rec.CommitP50, rec.CommitP95, rec.CommitP99 = summarize(loadCommits)
	}
	return rec
}

// attributeProbes keeps the probes that started inside the producer's interval
// and reports how many started outside it: such a tick was not made in the load
// and is counted with the skipped ones rather than in the series.
func attributeProbes(starts, waits, commits []int64, first, last int64) (loadStarts, loadWaits, loadCommits []int64, outside int) {
	for i, at := range starts {
		if at < first || at > last {
			outside++
			continue
		}
		loadStarts = append(loadStarts, at)
		loadWaits = append(loadWaits, waits[i])
		loadCommits = append(loadCommits, commits[i])
	}
	return loadStarts, loadWaits, loadCommits, outside
}

// summarize is the recorded estimator of one metric's raw samples: the store
// foreground series' own nearest-rank percentiles.
func summarize(raw []int64) (p50, p95, p99 int64) {
	s := storebench.Summarize(append([]int64(nil), raw...))
	return s.P50, s.P95, s.P99
}

// sleepUntilAt waits for one instant; a closed context ends the wait, which the
// caller records rather than treats as a measured zero.
func sleepUntilAt(ctx context.Context, at time.Time) error {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// streamTotals is the frames and gaps of the streams that read. The ten are the
// load's own control: every one must have received the same frames and none may
// be resynced, so a reader that fell behind is the arm's answer instead of a
// count the run quietly adopts. A refused arm names every reader's totals and
// the stalled streams' close, so one run identifies the reader that diverged.
func streamTotals(streams, slow []*frameStream) (int64, int64, error) {
	least, gaps := int64(-1), int64(0)
	for _, s := range streams {
		n, g, resync := s.totals()
		if resync {
			return 0, 0, fmt.Errorf("stream_resync %s", streamReport(streams, slow))
		}
		if least < 0 {
			least = n
		} else if n != least {
			return 0, 0, fmt.Errorf("stream_mismatch %s", streamReport(streams, slow))
		}
		gaps += g
	}
	return max(least, 0), gaps, nil
}

// waitPruneIdle waits until no accepted `jobs.prune` row can still publish a
// transition, so the readers' convergence is not raced by a late finish frame.
// The long alias purge is not waited for: its own finish is either inside the
// convergence window or after the cancellation, where it cannot split the
// readers. The bound is the arm's own answer.
func waitPruneIdle(ctx context.Context, serving *server.Serving, bound time.Duration) {
	deadline := time.Now().Add(bound)
	for {
		idle := true
		for _, state := range []string{store.JobQueued, store.JobRunning, store.JobReconciling} {
			rows, err := serving.Effects.DB.JobsInState(ctx, state)
			if err != nil {
				idle = false
				break
			}
			for _, row := range rows {
				if row.Kind == jobs.KindJobsPrune {
					idle = false
					break
				}
			}
			if !idle {
				break
			}
		}
		if idle || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// stopReadersAtCommonFrame stops every healthy reader at the highest frame any
// of them has parsed. Frames are sequential and delivered to all, so each
// parses that same frame and stops; counts are equal even if a publisher is
// still in flight. All reader locks are held across the read and the write, so
// no reader can advance between choosing the target and setting it.
func stopReadersAtCommonFrame(streams []*frameStream) {
	for _, s := range streams {
		s.mu.Lock()
	}
	target := int64(0)
	for _, s := range streams {
		target = max(target, s.last)
	}
	for _, s := range streams {
		s.stopAt = target
		s.mu.Unlock()
	}
}

// waitStreamsEnded waits for the streams to end on their own, bounded by one
// deadline shared by all of them.
func waitStreamsEnded(ctx context.Context, streams []*frameStream, bound time.Duration) {
	deadline := time.After(bound)
	for _, s := range streams {
		select {
		case <-s.done:
		case <-ctx.Done():
			return
		case <-deadline:
			return
		}
	}
}

// waitConverged holds the healthy readers after publishing stops until every
// one reports the same place in the stream for settle, bounded by bound. Equal
// signatures are the arm's evidence that no reader is a frame behind; a stream
// already resynced is left for streamTotals to name. The bound is the arm's own
// answer when a reader never catches up.
func waitConverged(ctx context.Context, streams []*frameStream, settle, bound time.Duration) {
	deadline := time.Now().Add(bound)
	var signature string
	var since time.Time
	for {
		next, converged, resynced := commonSignature(streams)
		if resynced {
			return
		}
		if converged && next == signature {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= settle {
				return
			}
		} else {
			signature, since = next, time.Time{}
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitUsersPublished holds the healthy readers until each has parsed one users
// frame per successful producer PATCH, bounded. The producer's own answers are
// the fan-out's count, so a reader short of it is the arm's answer instead of a
// number the run quietly adopts.
func waitUsersPublished(ctx context.Context, streams []*frameStream, want int64, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		least := int64(-1)
		for _, s := range streams {
			n := s.usersCount()
			if least < 0 || n < least {
				least = n
			}
		}
		if least >= want {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("producer_frames %d/%d", max(least, 0), want)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("producer_frames %d/%d", max(least, 0), want)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// commonSignature is the readers' shared place in the stream. converged is
// false while any two differ; resynced ends the wait because streamTotals will
// refuse the arm and name the reader.
func commonSignature(streams []*frameStream) (signature string, converged, resynced bool) {
	if len(streams) == 0 {
		return "", true, false
	}
	signature, ok := streams[0].signature()
	if !ok {
		return "", false, true
	}
	for _, s := range streams[1:] {
		next, ok := s.signature()
		if !ok {
			return "", false, true
		}
		if next != signature {
			return "", false, false
		}
	}
	return signature, true, false
}

// signature is one reader's count and last sequence. A resynced stream has no
// comparable place: the arm refuses it instead of waiting for it.
func (s *frameStream) signature() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resync != "" {
		return "", false
	}
	return strconv.FormatInt(s.count, 10) + ":" + strconv.FormatInt(s.last, 10), true
}

func streamReport(healthy, slow []*frameStream) string {
	read := make([]string, len(healthy))
	for i, s := range healthy {
		n, g, resync := s.totals()
		read[i] = fmt.Sprintf("%d:frames=%d gaps=%d resync=%t", i, n, g, resync)
	}
	closed := make([]string, len(slow))
	for i, s := range slow {
		closed[i] = fmt.Sprintf("%d:closed=%t resync=%t server_closed=%t open_error=%q", i, s.slowClosed(), s.resynced(), s.serverClosed(), s.err())
	}
	return fmt.Sprintf("healthy=[%s] slow=[%s]", strings.Join(read, " "), strings.Join(closed, " "))
}

// drainSlow waits for the two non-reading streams to end and reports how many
// of them the server overflowed and closed: the only close the protocol accepts.
func drainSlow(streams []*frameStream) int {
	closed := 0
	for _, s := range streams {
		if s.slowClosed() {
			closed++
		}
	}
	return closed
}

// slowOpened is the DEC-060 clause's own observation: a slow stream is opened
// when its hello was parsed over a connected request, so the count is what the
// header records and a refused or unconnected stream fails the arm with its
// open error named. The bounded close is recorded separately and not gated.
func slowOpened(streams []*frameStream) (int, error) {
	opened := 0
	var missing []string
	for i, s := range streams {
		s.mu.Lock()
		ok := s.started && s.openErr == ""
		openErr := s.openErr
		s.mu.Unlock()
		if ok {
			opened++
			continue
		}
		missing = append(missing, fmt.Sprintf("%d:opened=%t open_error=%q", i, ok, openErr))
	}
	if opened != slowStreams {
		return opened, fmt.Errorf("slow_open %d/%d %s", opened, slowStreams, strings.Join(missing, " "))
	}
	return opened, nil
}

// ---- the event stream -------------------------------------------------------------

// frameStream is one stream's own reading of the event stream: the frames it
// received, the sequence gaps it found, the resync it was sent, and — for R's
// stream — every job state a frame named, so the accepted job's start stays the
// sample even when its finish was read first.
type frameStream struct {
	keep bool
	// hold is a slow stream's stall after hello: it stops consuming the body
	// until the hold ends, so the bounded delivery overflows its queue or the
	// frame deadline ends it. A reading stream leaves it zero.
	hold time.Duration
	mu   sync.Mutex

	count      int64
	users      int64
	gaps       int64
	last       int64
	started    bool
	resync     string
	openErr    string
	serverDone bool
	stopAt     int64
	seen       map[string]bool
	// receipts is the instant each named job state's frame was parsed, keyed
	// like seen: the running frame's receipt is the admission sample.
	receipts map[string]time.Time

	notify chan struct{}
	done   chan struct{}
}

func newFrameStream(keep bool) *frameStream {
	return &frameStream{keep: keep, seen: map[string]bool{}, receipts: map[string]time.Time{}, notify: make(chan struct{}), done: make(chan struct{})}
}

// open requests one stream and reads it to its end. lookup is the shared cap on
// in-flight requests, so one peer's streams never present more lookups than the
// server admits; a nil lookup sends the request unthrottled.
func (s *frameStream) open(ctx context.Context, client *http.Client, base, key string, lookup chan struct{}) {
	defer close(s.done)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+eventsPath, nil)
	if err != nil {
		s.setErr("request: " + err.Error())
		return
	}
	req.Header.Set(keyHeader, key)
	if lookup != nil {
		select {
		case lookup <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	resp, err := client.Do(req)
	if lookup != nil {
		<-lookup
	}
	if err != nil {
		s.setErr("do: " + err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.setErr("status: " + resp.Status)
		return
	}
	s.read(ctx, resp.Body)
	if ctx.Err() == nil {
		s.mu.Lock()
		s.serverDone = true
		s.mu.Unlock()
	}
}

// atTarget reports whether the stream has reached the arm's common stop frame.
func (s *frameStream) atTarget() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopAt != 0 && s.last >= s.stopAt
}

// setErr records one open failure for the refusal's diagnostic.
func (s *frameStream) setErr(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openErr = text
}

// read parses the stream's frames: the id's sequence must follow the one before
// it, a resync is recorded with its reason, and a job frame is handed to R. A
// slow stream parses its hello and then stops consuming for its hold, so the
// hub's queue past the socket buffers can overflow; the drain that follows
// reaches the bounded delivery's resync or the frame deadline's close.
func (s *frameStream) read(ctx context.Context, body io.Reader) {
	reader := bufio.NewReaderSize(body, maxLineBytes)
	var event, data, id string
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" || data != "" || id != "" {
				s.frame(event, data, id)
				if s.atTarget() {
					return
				}
				if event == "hello" && s.hold > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(s.hold):
					}
				}
			}
			event, data, id = "", "", ""
		} else if rest, ok := strings.CutPrefix(line, "id: "); ok {
			id = rest
		} else if rest, ok := strings.CutPrefix(line, "event: "); ok {
			event = rest
		} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
			data = rest
		}
		if err != nil {
			return
		}
	}
}

// frame accounts for one complete frame. The receipt is the instant the frame
// was read, before its payload is decoded, because it is the admission sample.
func (s *frameStream) frame(event, data, id string) {
	receipt := time.Now()
	seq := frameSeq(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event {
	case "hello":
		s.started, s.last = true, seq
		close(s.notify)
		s.notify = make(chan struct{})
		return
	case "resync":
		s.resync = "resync"
		return
	}
	s.count++
	if event == "users" {
		s.users++
	}
	if s.started && seq != s.last+1 {
		s.gaps++
	}
	s.last = seq
	if !s.keep || event != "job" {
		return
	}
	var payload struct {
		Job struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}
	key := jobFrameKey(strconv.FormatInt(payload.Job.ID, 10), payload.Job.State)
	s.seen[key] = true
	s.receipts[key] = receipt
	close(s.notify)
	s.notify = make(chan struct{})
}

// jobFrameKey names one job state a frame carried.
func jobFrameKey(id, state string) string {
	return id + "." + state
}

// frameSeq is the sequence of one frame id: epoch.boot.seq, whose last part is
// the hub's own counter.
func frameSeq(id string) int64 {
	_, after, ok := strings.Cut(id, ".")
	if !ok {
		return 0
	}
	_, seq, ok := strings.Cut(after, ".")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (s *frameStream) totals() (count, gaps int64, resync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.gaps, s.resync != ""
}

// resynced reports whether the stream recorded the server's resync frame.
func (s *frameStream) resynced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resync != ""
}

// err is the stream's open failure, empty when it connected.
func (s *frameStream) err() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openErr
}

// serverClosed reports whether the server ended the body before the arm did.
func (s *frameStream) serverClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serverDone
}

// slowClosed reports whether the bounded delivery ended a slow stream: either
// the queue overflow's resync{overflow} or the product's own frame deadline
// closing a reader that stopped reading. Both are the disconnect AC-035 names;
// a stream still open when the arm cancels is not.
func (s *frameStream) slowClosed() bool {
	select {
	case <-s.done:
	case <-time.After(drainWait):
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resync != "" || s.serverDone
}

// hello waits for the stream's first frame, bounded by startWait. The hello
// branch sets started and closes notify; the job-frame signal stays notify, so
// a job frame wakes the wait without satisfying it. A stream that ended first
// is the distinct hello_unobserved, and neither arriving is hello_deadline.
func (s *frameStream) hello(ctx context.Context) error {
	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		started, notify := s.started, s.notify
		s.mu.Unlock()
		if started {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return errors.New("hello_unobserved")
		case <-deadline.C:
			return errors.New("hello_deadline")
		case <-notify:
		}
	}
}

// accept is the admin R's loop: one admitted job at a time, its running frame
// sampled from Accept's return, and the next Accept after its succeeded frame.
// An answer that is not a fresh admission — an open row of the same kind — is
// waited out, never sampled: its running frame was sent before this Accept. A
// refusal while the arm runs is returned; one the arm's own end caused is not.
func (s *frameStream) accept(ctx context.Context, serving *server.Serving, userID int64, timing *timingRecord, firstAccept chan struct{}) error {
	first := firstAccept
	stop := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	sample := func() error {
		job, err := serving.Effects.Jobs.Accept(ctx, jobs.Caller{UserID: userID}, jobs.KindJobsPrune)
		if err != nil {
			return err
		}
		// The sample starts when Accept returned, and the armed signal closes
		// only here, after the commit, so a reader pool sees the row.
		accepted := time.Now()
		if first != nil {
			close(first)
			first = nil
		}
		id := strconv.FormatInt(job.ID, 10)
		if job.State != store.JobQueued {
			return s.await(ctx, id, store.JobSucceeded)
		}
		ns, err := s.startSample(ctx, accepted, id)
		if err != nil {
			return err
		}
		starts, err := recordStart(timing.Starts, ns)
		if err != nil {
			return err
		}
		timing.Starts = starts
		return s.await(ctx, id, store.JobSucceeded)
	}
	if err := pace(ctx, time.Now(), armWindow, sampleInterval, sleepUntilAt, sample); err != nil {
		return stop(err)
	}
	return nil
}

// pace runs sample once per interval across the window: the accept loop's
// starts span the arm, where an unpaced loop would fill the wire's raw bound in
// the arm's first seconds and truncate the series. wait is the sleep the
// instants are taken at.
func pace(ctx context.Context, start time.Time, window, interval time.Duration, wait func(context.Context, time.Time) error, sample func() error) error {
	for i := 0; time.Duration(i)*interval < window; i++ {
		if err := wait(ctx, start.Add(time.Duration(i)*interval)); err != nil {
			return err
		}
		if err := sample(); err != nil {
			return err
		}
	}
	return nil
}

// recordStart appends one admitted start, or reports that the wire's raw cap is
// reached: a longer series decodes as malformed, and one that silently stopped
// growing would let a truncated arm read as a measured pass.
func recordStart(starts []int64, ns int64) ([]int64, error) {
	if len(starts) >= maxRaw {
		return starts, errors.New("starts_truncated")
	}
	return append(starts, ns), nil
}

// await waits for one job's state to arrive on a frame, within the bound. The
// running frame's receipt is the protocol's sample; a runtime that publishes no
// committed running transition never sends one, so this ends the arm instead of
// recording a number the product did not produce.
func (s *frameStream) await(ctx context.Context, id, state string) error {
	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	want := jobFrameKey(id, state)
	for {
		s.mu.Lock()
		seen := s.seen[want]
		notify := s.notify
		s.mu.Unlock()
		if seen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("job_start_unobserved")
		case <-notify:
		}
	}
}

// startSample is one admitted start's measured nanoseconds: Accept's return to
// the receipt of the job's running frame. The wait's own return is not the
// sample, so a reader that is slow to decode cannot inflate it.
func (s *frameStream) startSample(ctx context.Context, accepted time.Time, id string) (int64, error) {
	if err := s.await(ctx, id, store.JobRunning); err != nil {
		return 0, err
	}
	s.mu.Lock()
	receipt, ok := s.receipts[jobFrameKey(id, store.JobRunning)]
	s.mu.Unlock()
	if !ok {
		return 0, errors.New("job_start_unobserved")
	}
	elapsed := receipt.Sub(accepted).Nanoseconds()
	if elapsed < 0 {
		// The claim can publish before Accept returns; a receipt that precedes
		// the return is a zero sample, never a negative one.
		elapsed = 0
	}
	return elapsed, nil
}

// usersCount is the users frames one reader parsed: the fan-out's own count,
// compared with the producer's successful PATCHes.
func (s *frameStream) usersCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users
}

// newEventClient is the load's HTTP client: one connection per stream, no proxy
// and no reuse of a stream's connection for anything else.
func newEventClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: adminAccounts * 2,
	}}
}

// slowDialer returns the slow streams' dialer: its socket control sets the
// recorded receive window, so a build that drops the setsockopt leaves the
// kernel's default and the bounded-queue path is never exercised.
func slowDialer() *net.Dialer {
	return &net.Dialer{Control: func(_, _ string, conn syscall.RawConn) error {
		var setErr error
		if err := conn.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, slowReceiveWindow)
		}); err != nil {
			return err
		}
		return setErr
	}}
}

// newSlowEventClient is the slow streams' client: its receive window is small
// enough that a reader slower than the publisher fills the kernel buffers, so
// the hub's own queue is what overflows. Without it, loopback auto-tuning
// absorbs the whole stream and the bounded-queue path is never exercised.
func newSlowEventClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: adminAccounts * 2,
		DialContext:         slowDialer().DialContext,
	}}
}

// ---- the producer -----------------------------------------------------------------

// producer is the 50/s PATCH run that makes the event fan-out: it toggles the
// viewer V and records its own interval, which is what the writer series is
// attributed to.
type producer struct {
	ctx    context.Context
	client *http.Client
	base   string
	key    string
	target *viewerTarget

	first atomic.Int64
	last  atomic.Int64
	// successes is the answered PATCHes the fan-out's own count is compared
	// with. err and completed are written by run before done closes and read
	// only after a receive on done, so a producer that stopped early is the
	// arm's answer rather than a short series.
	successes atomic.Int64
	err       error
	completed bool
	epoch     time.Time
	ready     chan struct{}
	done      chan struct{}
	once      sync.Once
}

func newProducer(ctx context.Context, client *http.Client, base, key string, target *viewerTarget, epoch time.Time) *producer {
	return &producer{ctx: ctx, client: client, base: base, key: key, target: target, epoch: epoch, ready: make(chan struct{}), done: make(chan struct{})}
}

// waitFirst blocks until the producer's first request went out or it stopped:
// the interval the writer series is attributed to starts at the first request,
// and a producer that ended first is the arm's own answer.
func (p *producer) waitFirst(ctx context.Context) error {
	select {
	case <-p.ready:
		return nil
	case <-p.done:
		if err := p.stopped(); err != nil {
			return err
		}
		return errors.New("producer_incomplete")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopped is the producer's own refusal when it ended before its window. A
// completed window, or one the arm's own cancellation ended, is not a failure.
func (p *producer) stopped() error {
	if p.completed || p.err == nil {
		return nil
	}
	return fmt.Errorf("producer_incomplete %v", p.err)
}

// run drives one PATCH per rate period for the window, toggling V's disabled
// flag with the revision the previous answer named.
func (p *producer) run() {
	defer close(p.done)
	period := time.Second / patchRate
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	deadline := p.epoch.Add(patchWindow)
	for at := p.epoch; at.Before(deadline); at = <-ticker.C {
		if p.ctx.Err() != nil {
			return
		}
		if err := p.patch(); err != nil {
			p.err = err
			return
		}
		p.successes.Add(1)
		p.once.Do(func() { close(p.ready) })
	}
	p.completed = true
}

// patch is one PATCH of the viewer, and the revision its answer names.
func (p *producer) patch() error {
	body := fmt.Sprintf(`{"disabled":%t,"expectedRevision":%d}`, !p.target.disabled, p.target.revision)
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPatch, p.base+usersPath+strconv.FormatInt(p.target.id, 10), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(keyHeader, p.key)
	req.Header.Set("Content-Type", "application/json")
	p.mark()
	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New("patch_failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("patch_refused")
	}
	var answer struct {
		Revision int64 `json:"revision"`
		Disabled bool  `json:"disabled"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLineBytes)).Decode(&answer); err != nil {
		return errors.New("patch_malformed")
	}
	p.target.revision, p.target.disabled = answer.Revision, answer.Disabled
	return nil
}

// mark records one request's instant inside the producer's interval.
func (p *producer) mark() {
	at := time.Since(p.epoch).Nanoseconds()
	p.first.CompareAndSwap(0, at)
	p.last.Store(at)
}

// ---- the synthetic peer -----------------------------------------------------------

// peerResolver resolves only the names the protocol pins, and always to this
// machine's loopback: the phases reach a harness listener through the
// composition's own client, never a network destination.
type peerResolver struct{}

func (peerResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if host != barrierHost && !strings.HasPrefix(host, "probe-") {
		return nil, errors.New("unresolvable")
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// syntheticPeer is the in-harness service the retention phases talk to: one
// loopback listener, the requests each host actually received, and the payloads
// the held reads were served.
type syntheticPeer struct {
	ln   net.Listener
	port int
	srv  *http.Server

	mu        sync.Mutex
	hosts     map[string]int
	probeHits map[string]int
	badHosts  int
	payloads  [retentionReads]string
	served    map[int]bool

	held    atomic.Int64
	release chan struct{}
}

func newSyntheticPeer() (*syntheticPeer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("peer_unavailable")
	}
	p := &syntheticPeer{
		ln: ln, port: ln.Addr().(*net.TCPAddr).Port,
		hosts: map[string]int{}, probeHits: map[string]int{}, served: map[int]bool{},
		release: make(chan struct{}),
	}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: drainWait}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// handle answers one phase request: a held read waits for the release and is
// served one full body, and a probe host is answered at once. Every request's
// actual Host header is checked against the name its path belongs to.
func (p *syntheticPeer) handle(w http.ResponseWriter, r *http.Request) {
	name, index, isProbe, ok := peerPath(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	host := r.Host
	if name != "" && hostOnly(host) != name {
		p.mu.Lock()
		p.badHosts++
		p.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.hosts[host]++
	p.mu.Unlock()
	if isProbe {
		p.mu.Lock()
		p.probeHits[host]++
		p.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
		return
	}
	p.held.Add(1)
	select {
	case <-p.release:
	case <-r.Context().Done():
		return
	}
	body := heldBody()
	p.mu.Lock()
	p.payloads[index] = heldPayload()
	p.served[index] = true
	p.mu.Unlock()
	_, _ = io.WriteString(w, body)
}

// releaseHeld lets the held reads answer.
func (p *syntheticPeer) releaseHeld() { close(p.release) }

func (p *syntheticPeer) heldReads() int { return int(p.held.Load()) }

// peerPath names the host and index one path belongs to; a probe host carries
// its own number.
func peerPath(path string) (name string, index int, probe, ok bool) {
	rest, found := strings.CutPrefix(path, "/")
	if !found {
		return "", 0, false, false
	}
	kind, tail, found := strings.Cut(rest, "/")
	if !found {
		return "", 0, false, false
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n < 0 {
		return "", 0, false, false
	}
	switch kind {
	case "hold":
		return barrierHost, n, false, n < retentionReads
	case "probe":
		return fmt.Sprintf(probeHost, n), n, true, true
	}
	return "", 0, false, false
}

// hostOnly is a request's host without the port it arrived on.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// heldPayload is the decoded payload of one held reply, and heldBody its wire
// form: one body of exactly the recorded size carrying that payload.
func heldPayload() string { return strings.Repeat("p", retentionDecoded/retentionReads) }

func heldBody() string {
	const prefix, middle, suffix = `{"payload":"`, `","padding":"`, `"}`
	pad := retentionBodies - len(prefix) - len(heldPayload()) - len(middle) - len(suffix)
	return prefix + heldPayload() + middle + strings.Repeat("x", pad) + suffix
}

// retentionCounts are the figures the peer itself observed at the end of the
// saturation: the requests it answered after the held reads were released, the
// probes whose replies the client accepted, and every request it answered.
type retentionCounts struct {
	probes    int
	successes int
	answered  int
}

// retentionFrom derives the record's counts from the peer's own figures: the
// probe series includes the repeat that proves the first host was not evicted,
// each served probe charged its host and ceiling key beside the held reads' two,
// and the verified hosts are the requests the peer actually answered.
func retentionFrom(c retentionCounts) (probeRequests, keysAdded, hostsVerified int) {
	return c.probes, 2 + 2*c.successes, c.answered
}

// saturate runs the position-7 phases in the recorded order and reports what
// they observed. Everything it measures is the composition's: its own outbound
// client, its own runtime slots and its own store.
func (w *world) saturate(ctx context.Context, serving *server.Serving) (retention, error) {
	ctx, cancel := context.WithTimeout(ctx, phaseBudget)
	defer cancel()
	out := retention{Present: true, Phases: append([]string(nil), retentionPhases...), BodyBytes: retentionBodies}
	peer := w.peer

	// (1) 32 held reads on one host: 2 limiter keys (that host and its ceiling),
	// each a 1 MiB body the peer keeps open until it counts every one of them.
	var reads sync.WaitGroup
	bodies := make([][]byte, retentionReads)
	held := make(chan struct{})
	for k := range retentionReads {
		reads.Add(1)
		go func(k int) {
			defer reads.Done()
			resp, err := serving.Effects.Outbound.Do(ctx, w.op(barrierHost, "/hold/"+strconv.Itoa(k)), nil)
			if err != nil {
				return
			}
			bodies[k] = []byte(resp.Body)
		}(k)
	}
	go func() {
		for peer.heldReads() < retentionReads && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		close(held)
	}()
	select {
	case <-held:
	case <-ctx.Done():
		return out, errors.New("retention_hold")
	}
	out.HeldReads = peer.heldReads()

	// (2) a 33rd read on that host is refused before any request reaches the
	// peer: the client owns every flight it may have.
	before := peer.requests()
	if _, err := serving.Effects.Outbound.Do(ctx, w.op(barrierHost, "/hold/"+strconv.Itoa(retentionReads)), nil); err == nil {
		return out, errors.New("retention_queue")
	} else if refusal := asOutboundError(err); refusal == nil || refusal.Reason != outbound.ReasonQueueFull {
		return out, errors.New("retention_queue")
	}
	out.QueueFull, out.QueueRequests = outbound.ReasonQueueFull, peer.requests()-before

	// (3) release, and every reply decodes to the recorded payload; the digests
	// the peer served and the client decoded are one.
	peer.releaseHeld()
	reads.Wait()
	decoded, err := decodeHeld(bodies)
	if err != nil {
		return out, err
	}
	out.Released, out.DecodedBytes, out.DecodedSHA = retentionReads, int64(len(decoded)), sha256HexOf(decoded)
	if out.DecodedSHA != peer.servedDigest() {
		return out, errors.New("retention_decode")
	}

	// (4) one probe per host: each adds its host and ceiling key under its own
	// budgets, the 2048th is refused at the cache bound before any dial, and the
	// first host still succeeds afterwards.
	probesBefore := peer.requests()
	for k := 1; k <= retentionProbes; k++ {
		host := fmt.Sprintf(probeHost, k)
		_, err := serving.Effects.Outbound.Do(ctx, w.op(host, "/probe/"+strconv.Itoa(k)), nil)
		refusal := asOutboundError(err)
		switch {
		case k < retentionProbes && err == nil:
			out.ProbeSuccesses++
		case k < retentionProbes:
			return out, errors.New("retention_probes")
		default:
			if refusal == nil || refusal.Reason != outbound.ReasonBudgetExhausted ||
				refusal.Params["limit"] != "budget_keys" || refusal.Params["max"] != strconv.Itoa(retentionKeyMax) {
				return out, errors.New("retention_budget")
			}
		}
	}
	if _, err := serving.Effects.Outbound.Do(ctx, w.op(fmt.Sprintf(probeHost, 1), "/probe/1"), nil); err != nil {
		return out, errors.New("retention_repeat")
	}
	out.Repeat = "ok"
	out.ProbeBudget, out.ProbeLimit, out.ProbeMax = outbound.ReasonBudgetExhausted, "budget_keys", retentionKeyMax
	out.ProbeRequests, out.KeysAdded, out.HostsVerified = retentionFrom(retentionCounts{
		probes:    peer.requests() - probesBefore,
		successes: out.ProbeSuccesses,
		answered:  peer.requests(),
	})
	out.ServedSHA = peer.servedDigest()
	out.ChildRSS, out.ChildSlots, out.ChildReaped, err = w.residentChild(ctx, serving)
	// The held bodies and their decoded payloads stay referenced through the
	// child phase: the recorded facts are that they were retained, not that the
	// collector was free to drop them before the next phase began.
	runtime.KeepAlive(bodies)
	runtime.KeepAlive(decoded)
	if err != nil {
		return out, err
	}
	return out, nil
}

// op is one safe read of the peer through the composition's client: a private
// origin the synthetic resolver reaches, with no product kind or indexer.
func (w *world) op(host, path string) outbound.Op {
	target := &outbound.Target{
		ID:      "effects-retention",
		Origin:  outbound.Origin{Scheme: "http", Host: host, Port: w.peer.port},
		Private: true,
	}
	parsed := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(w.peer.port)), Path: path}
	return outbound.Op{Name: "effects.retention", Target: target, Method: http.MethodGet, URL: parsed, Safe: true}
}

// decodeHeld decodes the held replies' payloads in path order: the decoded bytes
// are what the protocol bounds, so a body that decoded short is a failure.
func decodeHeld(bodies [][]byte) ([]byte, error) {
	decoded := make([]byte, 0, retentionDecoded)
	for _, body := range bodies {
		if len(body) != retentionBodies {
			return nil, errors.New("retention_body")
		}
		var payload struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, errors.New("retention_body")
		}
		if len(payload.Payload) != retentionDecoded/retentionReads {
			return nil, errors.New("retention_decode")
		}
		decoded = append(decoded, payload.Payload...)
	}
	return decoded, nil
}

// servedDigest is the SHA-256 of the payloads the peer served, in path order.
func (p *syntheticPeer) servedDigest() string {
	sum := sha256.New()
	for i := range retentionReads {
		p.mu.Lock()
		payload, ok := p.payloads[i], p.served[i]
		p.mu.Unlock()
		if !ok {
			return ""
		}
		sum.Write([]byte(payload))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// close ends the peer's listener and its server: nothing outlives the run.
func (p *syntheticPeer) close() error { return errors.Join(p.srv.Close(), p.ln.Close()) }

// requests is how many requests the peer has answered so far.
func (p *syntheticPeer) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.hosts {
		total += n
	}
	return total
}

// asOutboundError is one refused call's own error, or nil.
func asOutboundError(err error) *outbound.Error {
	var refusal *outbound.Error
	if errors.As(err, &refusal) {
		return refusal
	}
	return nil
}

// sha256HexOf is the lower-case hexadecimal SHA-256 of data.
func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// residentChild runs the recorded child through the parent's own launcher while
// holding one of the runtime's shared slots, reads its VmRSS from the kernel and
// reaps it.
func (w *world) residentChild(ctx context.Context, serving *server.Serving) (int64, int, bool, error) {
	if err := child.PrepareManagedOwner(); err != nil {
		return 0, 0, false, errors.New("child_owner")
	}
	self, err := os.Executable()
	if err != nil {
		return 0, 0, false, errors.New("child_executable")
	}
	slot, err := serving.Effects.Jobs.Slot(ctx)
	if err != nil {
		return 0, 0, false, errors.New("child_slot")
	}
	slots := 1
	var attempt child.AttemptID
	copy(attempt[:], sha256HexOf([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))[:32])
	proc, err := child.Start(ctx, child.Spec{
		Path: self, Args: []string{"-child-hold", childHoldSize}, Timeout: childTimeout, AttemptID: attempt,
	}, nil)
	if err != nil {
		slot()
		return 0, slots, false, errors.New("child_start")
	}
	rss, err := awaitResident(ctx, childHoldSize)
	if err != nil {
		proc.Stop()
		slot()
		return 0, slots, false, err
	}
	result, err := proc.Wait()
	slot()
	if err != nil || result.Exit != 0 || result.TimedOut || result.Completion.State != child.TreeQuiescent {
		return rss, slots, false, nil
	}
	return rss, slots, true, nil
}

// awaitResident finds the child by the argument it was started with and reads
// its resident set from the kernel until it holds the recorded size or the hold
// it was given ends.
func awaitResident(ctx context.Context, size string) (int64, error) {
	deadline := time.Now().Add(childHoldFor)
	for {
		if pid, ok := findChildPID(size); ok {
			rss, err := vmRSS(pid)
			if err == nil && rss >= minChildRSS {
				return rss, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, errors.New("child_resident")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// findChildPID is the recorded child's process, found by the argument only it
// runs with: the kernel's own answer, not the launcher's word.
func findChildPID(size string) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	want := "-child-hold\x00" + size
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), want) {
			return pid, true
		}
	}
	return 0, false
}
