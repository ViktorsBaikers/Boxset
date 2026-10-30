package server

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// fixedResolver answers one table of names, so an approved Private origin is reached
// on loopback without the host resolver.
type fixedResolver map[string][]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, &net.DNSError{Err: "unsupported network", Name: host}
	}
	answer, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return answer, nil
}

// TestEffectsOutboundComposition proves the one composition client: NewEffects
// builds Effects.Outbound on the jobs runtime's own slot, so a send spends the
// background ceiling the attempts share, and the slot the config named is never
// called.
func TestEffectsOutboundComposition(t *testing.T) {
	ctx := t.Context()
	// One background slot, so holding it is observable: the runtime derives its
	// ceiling from the available cores.
	cores := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(cores) })
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	setup := auth.NewSetup(db, auth.NewHasher(), dir)
	if _, err := setup.Arm(ctx, ""); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	held := make(chan struct{})
	releasePeer := sync.OnceFunc(func() { close(held) })
	var served atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		<-held
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
	target := &outbound.Target{
		ID:      "peer",
		Origin:  outbound.Origin{Scheme: "http", Host: "peer.test", Port: port},
		Private: true,
	}

	var callerSlot atomic.Int64
	effects, err := NewEffects(db, setup, ServeConfig{
		Version: "test",
		Outbound: outbound.Config{
			Resolver: fixedResolver{"peer.test": {netip.MustParseAddr("127.0.0.1")}},
			Slot: func(context.Context) (func(), error) {
				callerSlot.Add(1)
				return func() {}, nil
			},
		},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if effects.Outbound == nil || effects.Jobs == nil {
		t.Fatalf("the composition = %+v, want the one runtime and the one client", effects)
	}
	opURL, err := url.Parse("http://peer.test:" + strconv.Itoa(port) + "/items")
	if err != nil {
		t.Fatalf("op URL: %v", err)
	}
	op := outbound.Op{Target: target, Method: http.MethodGet, URL: opURL, Safe: true}

	// A read holds the runtime's only background slot while the peer holds it.
	done := make(chan error, 1)
	go func() {
		_, err := effects.Outbound.Do(context.Background(), op, nil)
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for served.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the read never reached the peer")
		}
		time.Sleep(time.Millisecond)
	}
	heldCtx, cancelHeld := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelHeld()
	if release, err := effects.Jobs.Slot(heldCtx); err == nil {
		release()
		t.Fatal("the runtime still had a background slot while the client was sending")
	}
	releasePeer()
	if err := <-done; err != nil {
		t.Fatalf("the read: %v", err)
	}
	// The runtime hands its slot back after the send and the caller's own slot
	// function was never used: the client's slot is the runtime's.
	release, err := effects.Jobs.Slot(ctx)
	if err != nil {
		t.Fatalf("the runtime slot after the send: %v", err)
	}
	release()
	if got := callerSlot.Load(); got != 0 {
		t.Fatalf("the config slot was called %d times, want the composition's runtime slot", got)
	}
}

// TestEffectsMutationSendsFromAnAttempt proves the one permit a write needs is the
// attempt's own: the attempt hands its slot back while it waits, and the send takes
// that same slot for its exchange. On a one-slot runtime the send therefore reaches
// the peer instead of waiting for the permit the attempt would otherwise still
// hold.
func TestEffectsMutationSendsFromAnAttempt(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var served atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer peer.Close()
	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatalf("peer URL: %v", err)
	}
	port, err := strconv.Atoi(peerURL.Port())
	if err != nil {
		t.Fatalf("peer port: %v", err)
	}
	op := outbound.Op{
		Name:   "test.mutation",
		Target: &outbound.Target{ID: "peer", Origin: outbound.Origin{Scheme: "http", Host: "peer.test", Port: port}, Private: true},
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "http", Host: "peer.test:" + strconv.Itoa(port), Path: "/items"},
		Body:   `{"title":"synthetic"}`,
	}

	// The handler sends through the client the composition builds, and the client
	// needs the runtime's slot, so it is bound after the runtime exists.
	var client *outbound.Client
	sent := make(chan error, 1)
	runtime, err := jobs.New(db, jobs.Config{
		BackgroundSlots: 1,
		Log:             func(string, ...any) {},
	}, []jobs.Handler{{
		Kind:        "test.mutation",
		Class:       jobs.ClassRequired,
		Lane:        jobs.LaneBackground,
		MaxAttempts: 1,
		Deadline:    10 * time.Second,
		Run: func(ctx context.Context, attempt *jobs.Attempt) jobs.Outcome {
			_, err := client.Do(ctx, op, attempt.Wait)
			sent <- err
			if err != nil {
				return jobs.Outcome{Result: jobs.Retry, Reason: "send_failed"}
			}
			return jobs.Outcome{Result: jobs.Succeeded, Reason: "sent"}
		},
	}})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	client = outbound.New(outbound.Config{
		Resolver: fixedResolver{"peer.test": {netip.MustParseAddr("127.0.0.1")}},
		Slot:     runtime.Slot,
		Version:  "test",
	})
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runtime.Stop(stopCtx)
	})

	var id int64
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		created, _, err := store.EnqueueJob(ctx, tx, store.JobIntent{
			Kind:           "test.mutation",
			Class:          store.JobRequired,
			Lane:           store.JobBackground,
			DedupeKey:      "one",
			ConcurrencyKey: "one",
			MaxAttempts:    1,
			DueAt:          time.Now().Unix(),
		})
		id = created
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	runtime.Wake()

	// The send is the first thing that can go wrong: on a one-slot runtime it must
	// reach the peer instead of waiting for the permit the attempt holds.
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("the attempt's send with %d peer requests: %v", served.Load(), err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the attempt's send never returned with %d peer requests", served.Load())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		row, err := db.ReadJob(ctx, id)
		if err != nil {
			t.Fatalf("ReadJob: %v", err)
		}
		if row.State == store.JobSucceeded {
			break
		}
		if row.State == store.JobReview || row.State == store.JobFailed {
			t.Fatalf("the attempt's row ended %s/%s with %d peer requests, want the one send recorded as done",
				row.State, row.ReasonCode, served.Load())
		}
		if time.Now().After(deadline) {
			t.Fatalf("the row stayed %s with %d peer requests, want the completed send recorded", row.State, served.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("the peer served %d requests, want the attempt's one send", got)
	}
}
