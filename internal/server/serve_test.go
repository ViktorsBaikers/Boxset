package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// TestServeComposition is the one production composition's own proof, on a real
// data directory: the lock is held while it lives, Start returns a ready Serving
// only once the ready transition's Runtime.Start completed, a recovering start
// keeps serving and dispatches nothing, Effects.DB and Effects.Jobs are the same
// store the domain wrote to, and Stop's last step is the lock release.
func TestServeComposition(t *testing.T) {
	ctx := t.Context()

	t.Run("ready start recovers and dispatches the accepted intent", func(t *testing.T) {
		dir := t.TempDir()
		layout := install.Layout{Dir: dir}
		// An earlier process wrote the domain change and its intent, claimed the
		// intent and died: the file carries the state the next start must recover.
		db, err := store.Open(ctx, layout.DB(), store.Options{})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}
		lib := library.New(db)
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO title (id, metadata_id, kind, name) VALUES (1, 'tmdb:603', 'movie', 'Synthetic Movie')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		active, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &active, []string{"Alpha Alias"}); err != nil {
			t.Fatal(err)
		}
		if err := lib.ActivateGeneration(ctx, active); err != nil {
			t.Fatal(err)
		}
		staging, err := lib.StageGeneration(ctx, "dataset-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.SetAliases(ctx, 1, &staging, []string{"Bravo Alias"}); err != nil {
			t.Fatal(err)
		}
		if err := lib.ActivateGeneration(ctx, staging); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := db.ClaimJob(ctx, store.JobBackground, "earlier-boot", time.Now().Unix())
		if err != nil || !ok {
			t.Fatalf("seed claim: ok %v err %v", ok, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("seed Close: %v", err)
		}

		closeStore := func(db *store.DB) error { return db.Close() }
		serving, err := Start(ctx, ServeConfig{
			DataDir:      dir,
			Port:         "0",
			URLBase:      "",
			Version:      "test",
			SetupHint:    "boxset setup-token",
			Log:          testLog,
			CrashMonitor: func(string) (func(), error) { return func() {}, nil },
			CloseStore:   closeStore,
			Boot:         "this-boot",
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if serving.State != Ready {
			t.Fatalf("State = %q, want ready", serving.State)
		}
		if serving.Effects == nil || serving.Effects.DB == nil || serving.Effects.Jobs == nil {
			t.Fatalf("the composition = %+v, want the one store and the one runtime", serving.Effects)
		}
		// Runtime.Start ran inside Start: the foreign lease is gone and the row moved
		// on, which only the synchronous recovery and reconciliation of
		// Runtime.Start write before Start returns.
		row, err := serving.Effects.DB.ReadJob(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("ReadJob after Start: %v", err)
		}
		if row.LeaseOwner == "earlier-boot" || row.Revision <= claimed.Revision {
			t.Fatalf("the row after Start = %+v, want the earlier attempt recovered before Start returned", row)
		}
		// Effects.DB and Effects.Jobs are one composition: the job the domain wrote
		// runs on the same store and deletes exactly the retired generation's aliases.
		deadline := time.Now().Add(30 * time.Second)
		for {
			row, err := serving.Effects.DB.ReadJob(ctx, claimed.ID)
			if err == nil && row.State == store.JobSucceeded {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the accepted purge did not finish after Start: %+v (err %v)", row, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		var aliases int64
		if err := serving.Effects.DB.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM title_alias WHERE generation_id = ?`, int64(active)).Scan(&aliases)
		}); err != nil || aliases != 0 {
			t.Fatalf("aliases of the retired generation = %d (err %v), want none: the runtime is the same store", aliases, err)
		}

		// The lifetime lock belongs to this Serving while it lives.
		if held, err := install.Acquire(dir); !errors.Is(err, install.ErrLockHeld) {
			if held != nil {
				_ = held.Release()
			}
			t.Fatalf("a second acquire while serving = %v, want ErrLockHeld", err)
		}

		// The store close blocks; the lock must still be held, and only after the
		// close returns is it released: the lock release is the last step.
		closed, releaseClose := make(chan struct{}), make(chan struct{})
		serving.cfg.CloseStore = func(db *store.DB) error {
			close(closed)
			<-releaseClose
			return db.Close()
		}
		stopDone := make(chan error, 1)
		go func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
			defer cancel()
			stopDone <- serving.Stop(stopCtx)
		}()
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatal("Stop did not reach the store close")
		}
		if held, err := install.Acquire(dir); !errors.Is(err, install.ErrLockHeld) {
			if held != nil {
				_ = held.Release()
			}
			t.Fatalf("the lock was released before the store closed: %v", err)
		}
		close(releaseClose)
		if err := <-stopDone; err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if state, err := ReadRunState(dir); err != nil || state != Draining {
			t.Fatalf("the last recorded run state = %q (err %v), want draining", state, err)
		}
		again, err := install.Acquire(dir)
		if err != nil {
			t.Fatalf("the data directory after Stop: %v", err)
		}
		if err := again.Release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a recovering start serves and dispatches nothing", func(t *testing.T) {
		dir := t.TempDir()
		layout := install.Layout{Dir: dir}
		db, err := store.Open(ctx, layout.DB(), store.Options{})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}
		// Accepted work that a ready installation would claim.
		var id int64
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			var err error
			id, _, err = store.EnqueueJob(ctx, tx, store.JobIntent{
				Kind: "alias.purge", Class: store.JobRequired, Lane: store.JobBackground,
				DedupeKey: "alias-generation:1", ConcurrencyKey: "alias-generation:1",
				Params: map[string]string{"generation": "1"}, MaxAttempts: 5, DueAt: time.Now().Unix(),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("seed Close: %v", err)
		}
		if err := os.WriteFile(layout.Journal(), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		port := compositionPort(t)
		serving, err := Start(ctx, ServeConfig{
			DataDir:      dir,
			Port:         port,
			Version:      "test",
			SetupHint:    "boxset setup-token",
			Log:          testLog,
			CrashMonitor: func(string) (func(), error) { return func() {}, nil },
			ResumeRestore: func(context.Context, install.Layout, install.RestoreOptions) (install.Recovery, error) {
				return install.Recovery{}, errors.New("synthetic interrupted restore")
			},
			Boot: "this-boot",
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if serving.State != Recovering {
			t.Fatalf("State = %q, want recovering", serving.State)
		}
		// It keeps serving: /ping answers the recovering state.
		state := compositionPingState(t, port)
		for i := 0; state == "" && i < 200; i++ {
			time.Sleep(10 * time.Millisecond)
			state = compositionPingState(t, port)
		}
		if state != string(Recovering) {
			t.Fatalf("/ping state = %q, want %q: a recovering installation keeps serving", state, Recovering)
		}
		// Nothing is dispatched before canonical ready.
		time.Sleep(100 * time.Millisecond)
		row, err := serving.Effects.DB.ReadJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != store.JobQueued || row.Attempts != 0 {
			t.Fatalf("the accepted intent before ready = %+v, want it queued and unclaimed", row)
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
		defer cancel()
		if err := serving.Stop(stopCtx); err != nil {
			t.Fatalf("Stop of a recovering installation: %v", err)
		}
		released, err := install.Acquire(dir)
		if err != nil {
			t.Fatalf("the data directory after Stop: %v", err)
		}
		if err := released.Release(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStopOverrunKeepsTheJoinedWorkerFirst proves an overrun stop still finishes
// and releases the lifetime lock once the metadata worker has joined: the worker
// is decided before the caller's own deadline, so the two ready cases cannot
// decide the lock by a coin flip. Thirty-two independent stops make a lost flip
// visible rather than a single run that might have gone either way.
func TestStopOverrunKeepsTheJoinedWorkerFirst(t *testing.T) {
	for range 32 {
		dir := t.TempDir()
		lock, err := install.Acquire(dir)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		joined := make(chan struct{})
		close(joined)
		serving := &Serving{cfg: ServeConfig{DataDir: dir}, lock: lock, done: make(chan struct{}), worker: joined}
		overrun, cancel := context.WithCancel(context.Background())
		cancel()
		if err := serving.Stop(overrun); err != nil {
			t.Fatalf("an overrun stop with the worker joined = %v, want it to finish", err)
		}
		again, err := install.Acquire(dir)
		if err != nil {
			t.Fatalf("the data directory after the overrun stop: %v", err)
		}
		if err := again.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestServeStartReportsReadyOnlyAfterTheRuntime proves the ready state follows
// the runtime's start: while the runtime is still starting, /ping does not report
// ready; once Start returns, it does.
func TestServeStartReportsReadyOnlyAfterTheRuntime(t *testing.T) {
	dir := t.TempDir()
	port := compositionPort(t)
	// The runtime reads the clock once inside Start, before it claims: holding that
	// call holds the ready transition open without a second seam.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	now := func() time.Time {
		once.Do(func() {
			close(entered)
			<-release
		})
		return time.Now()
	}
	type started struct {
		serving *Serving
		err     error
	}
	done := make(chan started, 1)
	go func() {
		serving, err := Start(t.Context(), ServeConfig{
			DataDir:      dir,
			Port:         port,
			Version:      "test",
			SetupHint:    "boxset setup-token",
			Log:          testLog,
			CrashMonitor: func(string) (func(), error) { return func() {}, nil },
			Now:          now,
			Boot:         "this-boot",
		})
		done <- started{serving, err}
	}()
	select {
	case <-entered:
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start before the runtime started: %v", r.err)
		}
		t.Fatal("Start returned without the runtime reading its clock")
	case <-time.After(30 * time.Second):
		t.Fatal("the runtime never started")
	}
	if state := compositionPingState(t, port); state != string(Starting) {
		t.Fatalf("the state while the runtime starts = %q, want %q", state, Starting)
	}
	close(release)
	var serving *Serving
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		serving = r.serving
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
		defer cancel()
		if err := serving.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	if serving.State != Ready {
		t.Fatalf("State = %q, want ready", serving.State)
	}
	if state := compositionPingState(t, port); state != string(Ready) {
		t.Fatalf("the state after the runtime started = %q, want %q", state, Ready)
	}
}

// testLog is the composition's log for the tests: nothing is retained.
func testLog(string) (*slog.Logger, func() error, error) {
	return slog.New(slog.NewTextHandler(io.Discard, nil)), func() error { return nil }, nil
}

// freePort is a port nothing is listening on now.
func compositionPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// pingState is the lifecycle state /ping answers, or "" when nothing answers.
func compositionPingState(t *testing.T, port string) string {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/ping")
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("X-Boxset-State")
}

// TestServeJobsRoute proves the jobs read model is answered by the composition that
// runs it: Start's own handler serves GET /api/v1/system/jobs to a signed-in
// administrator instead of refusing a route whose runtime nothing attached.
func TestServeJobsRoute(t *testing.T) {
	dir := t.TempDir()
	port := compositionPort(t)
	serving, err := Start(t.Context(), ServeConfig{
		DataDir:      dir,
		Port:         port,
		Version:      "test",
		SetupHint:    "boxset setup-token",
		Log:          testLog,
		CrashMonitor: func(string) (func(), error) { return func() {}, nil },
		Boot:         "this-boot",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 30 * time.Second}
	post := func(path, body string, header ...string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		request.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(header); i += 2 {
			request.Header.Set(header[i], header[i+1])
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return response
	}
	// The installation's own setup flow, then its own sign-in: no injected identity.
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if err != nil {
		t.Fatalf("setup token: %v", err)
	}
	setup := post("/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+strings.TrimSpace(string(token)))
	_ = setup.Body.Close()
	if setup.StatusCode != http.StatusCreated {
		t.Fatalf("setup = %d, want 201", setup.StatusCode)
	}
	login := post("/api/v1/session", credentials("owner"))
	_ = login.Body.Close()
	cookies := login.Cookies()
	if login.StatusCode != http.StatusCreated || len(cookies) != 1 {
		t.Fatalf("sign-in = %d with %d cookies, want one", login.StatusCode, len(cookies))
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/system/jobs", nil)
	if err != nil {
		t.Fatalf("jobs request: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: cookies[0].Name, Value: cookies[0].Value})
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/system/jobs: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/system/jobs = %d, want 200: the route reads the composition's runtime", response.StatusCode)
	}
	var page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("jobs page body: %v", err)
	}
	if page.Items == nil {
		t.Fatal("the jobs page carried no items list, want an empty page rather than a refusal")
	}

	// Stop drains in the landed order, and a first start's outbound client is closed
	// by it: the same client refuses admission after the stop.
	stopCtx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
	defer cancel()
	if err := serving.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	opURL, err := url.Parse("http://127.0.0.1:1/closing")
	if err != nil {
		t.Fatalf("op URL: %v", err)
	}
	_, err = serving.Effects.Outbound.Do(t.Context(), outbound.Op{
		Target: &outbound.Target{ID: "closed", Origin: outbound.Origin{Scheme: "http", Host: "127.0.0.1", Port: 1}, Private: true},
		Method: http.MethodGet,
		URL:    opURL,
		Safe:   true,
	}, nil)
	var refused *outbound.Error
	if !errors.As(err, &refused) || refused.Reason != outbound.ReasonClientClosed {
		t.Fatalf("Do after Stop = %v, want the drained client's client_closed", err)
	}
}

// TestServeOutboundDrain proves the stop order on a live flight: the composition's
// client ends its own flight before the runtime stops, and the store is closed only
// after both, with no flight left at the close.
func TestServeOutboundDrain(t *testing.T) {
	held := make(chan struct{})
	releasePeer := sync.OnceFunc(func() { close(held) })
	var served atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		select {
		case <-held:
			fmt.Fprint(w, "held")
		case <-r.Context().Done():
		}
	}))
	defer peer.Close()
	defer releasePeer()
	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatalf("peer URL: %v", err)
	}
	port, err := strconv.Atoi(peerURL.Port())
	if err != nil {
		t.Fatalf("peer port: %v", err)
	}

	dir := t.TempDir()
	// The wrap probes the runtime where the abort happens: the transport returns as
	// the client's Close ends the flight, which is before Close itself returns and
	// so before the runtime can have begun stopping.
	var serving *Serving
	var runtimeStoppedAtAbort atomic.Bool
	serving, err = Start(t.Context(), ServeConfig{
		DataDir:      dir,
		Port:         compositionPort(t),
		Version:      "test",
		SetupHint:    "boxset setup-token",
		Log:          testLog,
		CrashMonitor: func(string) (func(), error) { return func() {}, nil },
		Boot:         "this-boot",
		Outbound: outbound.Config{
			Resolver: fixedResolver{"peer.test": {netip.MustParseAddr("127.0.0.1")}},
			Transport: func(inner http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(request *http.Request) (*http.Response, error) {
					response, err := inner.RoundTrip(request)
					if err != nil {
						if release, slotErr := serving.Effects.Jobs.Slot(request.Context()); slotErr != nil {
							runtimeStoppedAtAbort.Store(errors.Is(slotErr, jobs.ErrStopped))
						} else {
							release()
						}
					}
					return response, err
				})
			},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	opURL, err := url.Parse("http://peer.test:" + strconv.Itoa(port) + "/items")
	if err != nil {
		t.Fatalf("op URL: %v", err)
	}
	flight := make(chan error, 1)
	go func() {
		_, err := serving.Effects.Outbound.Do(context.Background(), outbound.Op{
			Target: &outbound.Target{ID: "peer", Origin: outbound.Origin{Scheme: "http", Host: "peer.test", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    opURL,
			Safe:   true,
		}, nil)
		flight <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for served.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the flight never reached the peer")
		}
		time.Sleep(time.Millisecond)
	}

	// The store's close is the last step: by then the client has ended its flight
	// and the runtime has stopped.
	var clientClosed, runtimeStopped atomic.Bool
	serving.cfg.CloseStore = func(db *store.DB) error {
		_, err := serving.Effects.Outbound.Do(context.Background(), outbound.Op{}, nil)
		var refused *outbound.Error
		clientClosed.Store(errors.As(err, &refused) && refused.Reason == outbound.ReasonClientClosed)
		_, slotErr := serving.Effects.Jobs.Slot(context.Background())
		runtimeStopped.Store(errors.Is(slotErr, jobs.ErrStopped))
		return db.Close()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
	defer cancel()
	if err := serving.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-flight:
		if err == nil {
			t.Fatal("the held flight returned a result, want the drain to end it")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the held flight outlived the stop")
	}
	if !clientClosed.Load() {
		t.Fatal("the store closed before the outbound client did")
	}
	if runtimeStoppedAtAbort.Load() {
		t.Fatal("the runtime was already stopping when the client aborted its flight, want the client's Close first")
	}
	if !runtimeStopped.Load() {
		t.Fatal("the store closed before the jobs runtime stopped")
	}
}

// roundTripFunc is the transport seam as one function: the drain proof wraps the
// transport the client built for each hop.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// TestServeDrainClosesStreams is the composition's drain order for the event
// stream: the drain publishes the system state, closes every stream and only then
// runs outbound close, the runtime stop and the store close. The store-close hook
// is reached only after the one open stream has ended, and that stream saw the
// system frame before it did.
func TestServeDrainClosesStreams(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	port := compositionPort(t)
	serving, err := Start(ctx, ServeConfig{
		DataDir:      dir,
		Port:         port,
		Version:      "test",
		SetupHint:    "boxset setup-token",
		Log:          testLog,
		CrashMonitor: func(string) (func(), error) { return func() {}, nil },
		Boot:         "this-boot",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/api/v1/setup", strings.NewReader(`{"username":"admin","password":"synthetic-valid-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+string(token))
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", response.StatusCode)
	}
	// The setup flow creates the account; a sign-in is what issues the cookie the
	// stream is served under.
	login, err := client.Post("http://127.0.0.1:"+port+"/api/v1/session", "application/json", strings.NewReader(`{"username":"admin","password":"synthetic-valid-password"}`))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	_ = login.Body.Close()
	if login.StatusCode != http.StatusCreated || len(jar.Cookies(login.Request.URL)) == 0 {
		t.Fatalf("sign in: %d", login.StatusCode)
	}

	streamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The stream is served uncompressed: a client that accepts gzip receives the
	// frames themselves, never an encoded body.
	streamRequest.Header.Set("Accept-Encoding", "gzip")
	stream, err := client.Do(streamRequest)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	if encoding := stream.Header.Get("Content-Encoding"); encoding != "" {
		t.Fatalf("the event stream was compressed: %q", encoding)
	}

	var (
		ended     = make(chan struct{})
		mu        sync.Mutex
		lastEvent string
		lastData  string
		helloSeen bool
	)
	reader := bufio.NewReaderSize(stream.Body, 1<<16)
	go func() {
		defer close(ended)
		pending := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				pending = name
				if name == "hello" {
					mu.Lock()
					helloSeen = true
					mu.Unlock()
				}
				continue
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				mu.Lock()
				lastEvent, lastData = pending, data
				mu.Unlock()
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		hello := helloSeen
		mu.Unlock()
		if hello {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no hello frame before the drain")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The store close is the last step: when it is reached the stream must have
	// ended already, and it must have seen the drain's system frame.
	var endedBeforeClose atomic.Bool
	serving.cfg.CloseStore = func(db *store.DB) error {
		select {
		case <-ended:
			endedBeforeClose.Store(true)
		default:
		}
		return db.Close()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
	defer cancel()
	if err := serving.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-ended
	mu.Lock()
	saw, data := lastEvent, lastData
	mu.Unlock()
	if saw != "system" {
		t.Fatalf("the last frame before the drain closed the stream = %q, want the system state", saw)
	}
	var system struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(data), &system); err != nil {
		t.Fatalf("the system frame's payload %q: %v", data, err)
	}
	if system.State != string(Draining) {
		t.Fatalf("the system frame's state = %q, want %q", system.State, Draining)
	}
	if !endedBeforeClose.Load() {
		t.Fatal("the store was closed while a stream was still open")
	}
}

// TestEventStreamLeavesReadCapacity proves the stream route is served outside the
// read budget: the reader slots a charged stream would hold for its whole life
// stay free, so a population of permitted streams refuses neither an ordinary read
// nor an ordinary write of the same installation.
func TestEventStreamLeavesReadCapacity(t *testing.T) {
	in := newInstallation(t, "", "")
	if w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("setup = %d %q", w.Code, w.Body.String())
	}
	attachHub(t, in)
	server := httptest.NewServer(in.h)
	t.Cleanup(server.Close)

	// Four streams on each of eight accounts: the hub's per-user cap holds for
	// every one of them, and together they are as many open streams as the
	// installation has reader slots, so any charged stream would take the last one.
	var streams []*http.Response
	t.Cleanup(func() {
		for _, stream := range streams {
			_ = stream.Body.Close()
		}
	})
	hasher := auth.NewHasher()
	for i := range 8 {
		account := fmt.Sprintf("streamer-%d", i)
		if _, err := auth.CreateUser(t.Context(), in.db, hasher, account, password, auth.RoleAdmin); err != nil {
			t.Fatalf("create %s: %v", account, err)
		}
		token := keyForUser(t, in, account, "read")
		for range 4 {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Api-Key", token)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatalf("stream %d: %v", i, err)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				t.Fatalf("stream %d = %d %s", i, resp.StatusCode, body)
			}
			streams = append(streams, resp)
		}
	}
	in.s.admission.mu.Lock()
	held, slots := in.s.admission.total, len(in.s.admission.slots)
	in.s.admission.mu.Unlock()
	if held != 0 || slots != 0 {
		t.Fatalf("%d streams hold %d read counts and %d reader slots, want none", len(streams), held, slots)
	}

	// An ordinary read and an ordinary write of another credential are admitted
	// while every stream is open.
	_, writer := keyFor(t, in, "admin")
	if w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", writer); w.Code != http.StatusOK {
		t.Fatalf("a read beside %d streams = %d %q", len(streams), w.Code, w.Body.String())
	}
	if _, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "member", password, auth.RoleViewer); err != nil {
		t.Fatalf("create member: %v", err)
	}
	var memberID int64
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM user WHERE username = 'member'`).Scan(&memberID)
	}); err != nil {
		t.Fatalf("member id: %v", err)
	}
	if w := request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(memberID, 10), `{"expectedRevision":1,"disabled":true}`, "X-Api-Key", writer); w.Code != http.StatusOK {
		t.Fatalf("a write beside %d streams = %d %q", len(streams), w.Code, w.Body.String())
	}
}

// attachHub binds the composition's own hub over in's store: every frame is
// decided by the same auth.Revalidate the guard's credential read uses, so a
// stream these proofs open is served by production code.
func attachHub(t *testing.T, in *installation) {
	t.Helper()
	backend := *in.s.backend.Load()
	installID, err := backend.Sessions.Installation(t.Context())
	if err != nil {
		t.Fatalf("installation: %v", err)
	}
	epoch, err := backend.Sessions.Epoch(t.Context())
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	hub := events.New(events.Config{
		Boot: "boot-admission", InstallID: installID, RestoreEpoch: epoch,
		Revalidate: func(ctx context.Context, ref auth.CredentialRef) (auth.Principal, time.Time, error) {
			return auth.Revalidate(ctx, in.db, ref, time.Now())
		},
		Log: func(string, ...any) {},
	})
	backend.Hub = hub
	backend.DB = in.db
	in.s.Attach(backend)
	t.Cleanup(hub.Close)
}

// keyForUser creates one key of the given scope for an account this test made, so
// streams opened with it count against that account's cap.
func keyForUser(t *testing.T, in *installation, username, scope string) string {
	t.Helper()
	_, token, err := in.s.backend.Load().Sessions.Keys().Create(t.Context(), username, "automation", scope)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestEventStreamHeadReservesNothing proves a HEAD of the stream route is answered
// without a stream. Go's ServeMux resolves HEAD to the GET pattern, so without its
// own answer the HEAD would enter the stream handler, hold one of the credential's
// four stream slots and write its frames into a body net/http discards: the
// credential's next GET would be refused 429 stream_cap. The HEAD advertises the
// GET it aliases instead, whatever that GET's answer is. It is one request and no
// stream, so it still pays that request's read reservation.
func TestEventStreamHeadReservesNothing(t *testing.T) {
	in := newInstallation(t, "", "")
	if w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("setup = %d %q", w.Code, w.Body.String())
	}
	attachHub(t, in)
	server := httptest.NewServer(in.h)
	t.Cleanup(server.Close)
	_, token := keyFor(t, in, "read")

	// An unauthenticated HEAD is refused as it is everywhere else: the answer
	// carries no authority, so it cannot stand in for a stream either.
	if w := request(in.h, "HEAD", "/api/v1/events", ""); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
		t.Fatalf("an unauthenticated HEAD = %d %q", w.Code, code(t, w))
	}
	head := func() *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+"/api/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("HEAD: %v", err)
		}
		_ = resp.Body.Close()
		return resp
	}

	// Four HEADs of one credential: each answers the stream's own headers, and none
	// of them spends a stream slot.
	for range 4 {
		if resp := head(); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("HEAD = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
		}
	}

	// A HEAD is one request and no stream, so it pays the read reservation of the
	// request it is. The credential holds no stream here, so a refusal at its whole
	// in-flight read share can only be that reservation: the HEAD is answered the
	// cap instead of promising a stream, and once the share is returned the next
	// HEAD is admitted again.
	identity, err := in.s.backend.Load().Keys.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("the HEAD's credential: %v", err)
	}
	var share []func()
	releaseShare := func() {
		for _, release := range share {
			release()
		}
		share = nil
	}
	t.Cleanup(releaseShare)
	for range readsPerCredential {
		rec := httptest.NewRecorder()
		release, ok := in.s.admission.admit(rec, admittedKey(identity))
		if !ok {
			t.Fatalf("holding a read of the HEAD's credential = %d %q, want admitted", rec.Code, rec.Body.String())
		}
		share = append(share, release)
	}
	if resp := head(); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a HEAD at the credential's whole read share = %d retry=%q, want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	releaseShare()
	if resp := head(); resp.StatusCode != http.StatusOK {
		t.Fatalf("a HEAD after the credential's reads finished = %d", resp.StatusCode)
	}

	// The credential's four slots are still free: four GETs open, and the fifth is
	// the recorded refusal, so the HEADs reserved nothing and the cap still holds.
	var streams []*http.Response
	t.Cleanup(func() {
		for _, stream := range streams {
			_ = stream.Body.Close()
		}
	})
	open := func() (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}
		streams = append(streams, resp)
		return resp.StatusCode, ""
	}
	for i := range 4 {
		if status, body := open(); status != http.StatusOK {
			t.Fatalf("a stream after four HEADs (%d) = %d %s", i, status, body)
		}
	}
	if status, body := open(); status != http.StatusTooManyRequests || !strings.Contains(body, "stream_cap") {
		t.Fatalf("the fifth stream of the credential = %d %s", status, body)
	}

	// At the cap the HEAD answers the GET's 429 stream_cap, with the retry the GET
	// carries, instead of promising a stream the hub would refuse.
	if resp := head(); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "5" || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("a HEAD at the credential's stream cap = %d retry=%q %q", resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Content-Type"))
	}
	// Once the drain has closed the hub the GET answers 503 draining, so the HEAD
	// answers that too; the installation's state is still Ready here, so the answer
	// is the hub's and not the lifecycle gate's.
	in.s.Events().Close()
	if resp := head(); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("a HEAD on a draining installation = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// nextFrame reads one server-sent frame: its event name and its data line, with
// comments and the lines that end a frame skipped.
func nextFrame(reader *bufio.Reader) (name, data string, err error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if name != "" || data != "" {
				return name, data, nil
			}
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// TestEventStreamOutlivesRouteDeadline proves a stream route carries no read
// deadline: every other guarded route ends at the route deadline, while a healthy
// stream still delivers a frame published well past it.
func TestEventStreamOutlivesRouteDeadline(t *testing.T) {
	in := newInstallation(t, "", "")
	if w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("setup = %d %q", w.Code, w.Body.String())
	}
	attachHub(t, in)
	// Short enough that the stream outlives it several times over.
	in.s.routeDeadline = 250 * time.Millisecond
	server := httptest.NewServer(in.h)
	t.Cleanup(server.Close)
	_, token := keyFor(t, in, "read")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", token)
	req.Header.Set("Accept-Encoding", "identity")
	stream, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", stream.StatusCode)
	}
	reader := bufio.NewReaderSize(stream.Body, 1<<16)
	if name, _, err := nextFrame(reader); err != nil || name != "hello" {
		t.Fatalf("hello = %q, %v", name, err)
	}

	// Past several route deadlines the stream is still open and still served.
	time.Sleep(3 * in.s.routeDeadline)
	in.s.Events().Publish(events.Event{Name: events.NameJob, Payload: map[string]any{"jobId": 1}})
	got := make(chan struct {
		name string
		err  error
	}, 1)
	go func() {
		name, _, err := nextFrame(reader)
		got <- struct {
			name string
			err  error
		}{name, err}
	}()
	select {
	case f := <-got:
		if f.err != nil {
			t.Fatalf("the stream ended at the route deadline: %v", f.err)
		}
		if f.name != "job" {
			t.Fatalf("the frame past the route deadline = %q, want job", f.name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame was delivered past the route deadline")
	}
}

// TestUserEditLogsDroppedUsersFrame proves a failed users-frame revision read is
// recorded and does not change the principal: the edit still succeeds and the
// edited principal's open stream still closes.
func TestUserEditLogsDroppedUsersFrame(t *testing.T) {
	in := newInstallation(t, "", "")
	if w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("setup = %d %q", w.Code, w.Body.String())
	}
	attachHub(t, in)
	if _, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "member", password, auth.RoleViewer); err != nil {
		t.Fatalf("create member: %v", err)
	}
	var memberID int64
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM user WHERE username = 'member'`).Scan(&memberID)
	}); err != nil {
		t.Fatalf("member id: %v", err)
	}
	_, memberKey, err := in.s.backend.Load().Keys.Create(t.Context(), "member", "stream", "read")
	if err != nil {
		t.Fatalf("member key: %v", err)
	}

	server := httptest.NewServer(in.h)
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", memberKey)
	req.Header.Set("Accept-Encoding", "identity")
	stream, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReaderSize(stream.Body, 1<<16)
	if name, _, err := nextFrame(reader); err != nil || name != "hello" {
		t.Fatalf("hello = %q, %v", name, err)
	}

	// The revision read fails while the edit itself commits: the backend's store is
	// closed under it, and the credential owner keeps its own open store.
	closed, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "closed.db"), store.Options{})
	if err != nil {
		t.Fatalf("closed store: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("close the revision store: %v", err)
	}
	backend := *in.s.backend.Load()
	backend.DB = closed
	in.s.Attach(backend)

	_, writer := keyFor(t, in, "admin")
	if w := request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(memberID, 10), `{"expectedRevision":1,"disabled":true}`, "X-Api-Key", writer); w.Code != http.StatusOK {
		t.Fatalf("edit = %d %q", w.Code, w.Body.String())
	}
	// The principal's stream still closes, whatever the users frame could carry.
	ended := make(chan error, 1)
	go func() { _, _, err := nextFrame(reader); ended <- err }()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the edited principal's stream stayed open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edited principal's stream stayed open")
	}
	if logs := in.logs.String(); !strings.Contains(logs, "collection_revision_unavailable") {
		t.Fatalf("the dropped users frame was not recorded: %s", logs)
	}
}
