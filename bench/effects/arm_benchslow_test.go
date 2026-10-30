//go:build benchslow

package main

// The arm's out-of-band cases: the harness composition's own refusals drive an
// arm to its failure, the streams one peer opens all reach hello, and the
// runner's refusal mapping answers exitFail without a store, so none of this
// cost is in the unit and race cohorts.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storebench "github.com/ViktorsBaikers/Boxset/bench/store"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/server"
)

// TestArmRefusesAPublishlessRunArm drives one full arm of the harness
// composition with ServeConfig.Publish replaced by a no-op: the accepted job's
// running frame never arrives, so the arm ends with job_start_unobserved rather
// than a substitute sample.
func TestArmRefusesAPublishlessRunArm(t *testing.T) {
	w, serving := armWorld(t, func(jobs.Event) {}, false)
	defer stopArmWorld(t, serving)
	_, timing, writer, err := w.runArm(context.Background(), serving, armA1, orderAB, true)
	if err == nil || err.Error() != "job_start_unobserved" {
		t.Fatalf("runArm = %v, want job_start_unobserved", err)
	}
	// The refusal must come from an arm whose accept loop really ran: a dead loop
	// leaves the same empty starts, so the committed job is asserted first.
	if rows := pruneRows(t, serving, w.admins[0].user.ID); rows != 1 {
		t.Fatalf("jobs.prune rows for the harness admin = %d, want the one admitted job", rows)
	}
	if len(timing.Starts) != 0 {
		t.Fatalf("starts = %d, want none when no running frame is published", len(timing.Starts))
	}
	if timing.Run != w.run || timing.Arm != armA1 || timing.Order != orderAB || timing.Digest != w.digest {
		t.Fatalf("timing record = %+v, want this arm's identity", timing)
	}
	if writer.Run != w.run || writer.Arm != armA1 || writer.Order != orderAB || writer.Digest != w.digest {
		t.Fatalf("writer record = %+v, want this arm's identity", writer)
	}
}

// TestArmRefusesASamplingLoopThatEndsMidArm lets the first transition's two
// frames through and drops every later one: one start is sampled, then the
// accept loop's wait for the next running frame ends. The arm answers with that
// failure instead of returning the sample it did collect.
func TestArmRefusesASamplingLoopThatEndsMidArm(t *testing.T) {
	var hub *events.Hub
	var forwarded atomic.Int64
	publish := func(ev jobs.Event) {
		if ev.Kind != jobs.KindJobsPrune || forwarded.Add(1) > 2 {
			return
		}
		hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(ev)})
	}
	w, serving := armWorld(t, publish, false)
	defer stopArmWorld(t, serving)
	hub = serving.Effects.Hub

	_, timing, _, err := w.runArm(context.Background(), serving, armA1, orderAB, true)
	if err == nil || err.Error() != "job_start_unobserved" {
		t.Fatalf("runArm = %v, want job_start_unobserved", err)
	}
	if len(timing.Starts) != 1 {
		t.Fatalf("starts = %d, want the one sampled start collected before the loop ended", len(timing.Starts))
	}
	if rows := pruneRows(t, serving, w.admins[0].user.ID); rows != 2 {
		t.Fatalf("jobs.prune rows = %d, want the second admitted job that was never framed", rows)
	}
}

// countingTransport records how many requests are inside RoundTrip at once, so
// the harness's own cap on concurrent opens is observable rather than inferred
// from whether the server happened to refuse one.
type countingTransport struct {
	base http.RoundTripper

	mu       sync.Mutex
	inFlight int
	most     int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.inFlight++
	c.most = max(c.most, c.inFlight)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()
	return c.base.RoundTrip(r)
}

func (c *countingTransport) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.most
}

// TestArmOpensEveryStreamWithinTheAdmissionGate opens R's thirteen streams the
// way an arm does and requires every one to reach hello: the server admits eight
// credential lookups per peer at once, so the harness presents its requests in
// waves no wider than openLookups instead of all at the gate together.
func TestArmOpensEveryStreamWithinTheAdmissionGate(t *testing.T) {
	w, serving := armWorld(t, nil, false)
	defer stopArmWorld(t, serving)
	ctx, cancel := context.WithTimeout(context.Background(), 2*startWait)
	defer cancel()
	base := "http://127.0.0.1:" + strconv.Itoa(w.port)
	client := newEventClient()
	capped := &countingTransport{base: client.Transport}
	client.Transport = capped
	lookup := make(chan struct{}, openLookups)
	start := make(chan struct{})
	streams := make([]*frameStream, 0, adminAccounts)
	var opened sync.WaitGroup
	for i := range adminAccounts {
		s := newFrameStream(false)
		streams = append(streams, s)
		key := w.admins[i].key
		opened.Add(1)
		go func() {
			defer opened.Done()
			<-start
			s.open(ctx, client, base, key, lookup)
		}()
	}
	close(start)
	for _, s := range streams {
		if err := s.hello(ctx); err != nil {
			t.Fatalf("a stream opened from one peer never reached hello: %v", err)
		}
	}
	cancel()
	opened.Wait()
	if most := capped.peak(); most > openLookups {
		t.Fatalf("concurrent opens = %d, want no more than the admission gate's %d", most, openLookups)
	}
}

// TestRunnerRefusesAMeasurementItCannotRun drives the runner's refusal mapping:
// a measure that ends at its own ineligible inputs answers exitFail with the
// measurement's reason, so the error-to-FAIL path is exercised.
func TestRunnerRefusesAMeasurementItCannotRun(t *testing.T) {
	t.Setenv("BOXSET_DATA_DIR", "")
	out := filepath.Join(t.TempDir(), testRun(2), "samples")
	var stderr bytes.Buffer
	if code := run([]string{"-out", out, "-order", orderAB}, io.Discard, &stderr); code != exitFail {
		t.Fatalf("exit %d, want the measurement's refusal mapped to FAIL: %s", code, stderr.String())
	}
	if got := stderr.String(); got != "bencheffects: data_dir\n" {
		t.Fatalf("stderr = %q, want the measurement's own data_dir refusal", got)
	}
}

// pruneRows counts the jobs.prune rows one caller committed.
func pruneRows(t *testing.T, serving *server.Serving, userID int64) int64 {
	t.Helper()
	var rows int64
	if err := serving.Effects.DB.Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT count(*) FROM job WHERE kind = 'jobs.prune' AND created_by = ?`, userID).Scan(&rows)
	}); err != nil {
		t.Fatalf("count jobs.prune: %v", err)
	}
	return rows
}

// TestArmWaitsForHelloBeforeAccept refuses R's stream credential, so its stream
// ends before hello. The accept loop is spawned first but gated on that wait: a
// build that starts accepting without it commits a jobs.prune row, which the
// armed firstAccept signal and the store both see.
func TestArmWaitsForHelloBeforeAccept(t *testing.T) {
	w, serving := armWorld(t, nil, true)
	defer stopArmWorld(t, serving)
	first := make(chan struct{})
	w.firstAccept = first

	start := time.Now()
	_, _, _, err := w.runArm(context.Background(), serving, armA1, orderAB, true)
	if err == nil || err.Error() != "hello_unobserved" {
		t.Fatalf("runArm = %v, want hello_unobserved", err)
	}
	if elapsed := time.Since(start); elapsed >= patchWindow {
		t.Fatalf("hello-error arm took %s, want it to stop at the wait error", elapsed)
	}
	select {
	case <-first:
		t.Fatal("the accept loop ran before R's hello")
	default:
	}
	var rows int64
	if err := serving.Effects.DB.Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT count(*) FROM job WHERE kind = 'jobs.prune' AND created_by = ?`, w.admins[0].user.ID).Scan(&rows)
	}); err != nil {
		t.Fatalf("count jobs.prune: %v", err)
	}
	if rows != 0 {
		t.Fatalf("jobs.prune rows written before hello = %d, want none", rows)
	}
}

// armWorld seeds one installation of the arm's own shape: the composition, the
// accounts and keys, one viewer the producer edits, the staged generations and
// the probe table. The alias payloads are the measured load's own, so staging
// them without aliases is enough for the arm's guard.
func armWorld(t *testing.T, publish func(jobs.Event), refuseR bool) (*world, *server.Serving) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	serving, err := server.Start(ctx, server.ServeConfig{
		DataDir:      dir,
		Port:         strconv.Itoa(port),
		Version:      "bench-effects",
		SetupHint:    "boxset setup-token",
		Log:          benchLog,
		CrashMonitor: noCrashMonitor,
		Publish:      publish,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	if serving.State != server.Ready {
		stopArmWorld(t, serving)
		t.Fatalf("State = %q, want ready", serving.State)
	}
	w := &world{run: testRun(1), data: dir, port: port, digest: testDigest}
	hasher := auth.NewHasher()
	for i := range adminAccounts {
		name := fmt.Sprintf("arm%02d", i+1)
		user, err := auth.CreateUser(ctx, serving.Effects.DB, hasher, name, benchPassword, auth.RoleAdmin)
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
		scope := "read"
		if i == 0 {
			scope = "admin"
		}
		id, key, err := serving.Effects.Keys.Create(ctx, name, fmt.Sprintf("arm-%02d", i+1), scope)
		if err != nil {
			t.Fatalf("Keys.Create(%s): %v", name, err)
		}
		if refuseR && i == 0 {
			if err := serving.Effects.Keys.Revoke(ctx, id); err != nil {
				t.Fatalf("Revoke(%s): %v", name, err)
			}
		}
		w.admins = append(w.admins, credential{user: user, key: key})
	}
	viewer, err := auth.CreateUser(ctx, serving.Effects.DB, hasher, "arm-viewer", benchPassword, auth.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser(arm-viewer): %v", err)
	}
	w.viewer = viewerTarget{id: viewer.ID, revision: viewer.Revision}
	for range armedGenerations {
		gen, err := serving.Effects.Library.StageGeneration(ctx, datasetName)
		if err != nil {
			t.Fatalf("StageGeneration: %v", err)
		}
		w.staged = append(w.staged, gen)
	}
	if err := storebench.CreateProbe(ctx, serving.Effects.DB); err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	return w, serving
}

// stopArmWorld drains the composition within the recorded bound.
func stopArmWorld(t *testing.T, serving *server.Serving) {
	t.Helper()
	if err := stopServing(serving); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
