package outbound

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingSlot is the Slot a drain proof needs: a take waits for room instead of
// refusing, so a flight can be observed awaiting one, and every take is counted.
type blockingSlot struct {
	permits chan struct{}
	taken   atomic.Int64
	waiting atomic.Int64
}

func newBlockingSlot(capacity int) *blockingSlot {
	return &blockingSlot{permits: make(chan struct{}, capacity)}
}

func (s *blockingSlot) take(ctx context.Context) (func(), error) {
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.permits <- struct{}{}:
		s.taken.Add(1)
		var once sync.Once
		return func() { once.Do(func() { <-s.permits }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// takenSoFar is how many sends have taken a slot.
func (s *blockingSlot) takenSoFar() int64 { return s.taken.Load() }

// holdingPeer is the peer a drain proof holds: it reports each held request and
// answers it only when released or when the requester is gone. Every other request
// is answered at once, so a proof can reach the peer without holding it.
type holdingPeer struct {
	srv     *httptest.Server
	receive chan string
	abort   chan string
	release chan struct{}
	once    sync.Once
	served  atomic.Int64
}

func newHoldingPeer(t *testing.T) *holdingPeer {
	t.Helper()
	peer := &holdingPeer{
		receive: make(chan string, 32),
		abort:   make(chan string, 32),
		release: make(chan struct{}),
	}
	peer.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "held.test") {
			fmt.Fprint(w, "admitted")
			return
		}
		peer.served.Add(1)
		peer.receive <- r.URL.Path
		select {
		case <-peer.release:
			fmt.Fprint(w, "held")
		case <-r.Context().Done():
			peer.abort <- r.URL.Path
		}
	}))
	t.Cleanup(func() {
		peer.releaseAll()
		peer.srv.Close()
	})
	return peer
}

// releaseAll answers every request the peer is holding.
func (p *holdingPeer) releaseAll() { p.once.Do(func() { close(p.release) }) }

// awaiting waits for one message on ch.
func awaiting(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
		return ""
	}
}

// waitUntil waits for cond to hold, failing the test instead of blocking forever.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// readAt is one safe read of host's /items at port.
func readAt(t *testing.T, host string, port int) Op {
	t.Helper()
	return Op{
		Target: &Target{ID: "approved", Origin: Origin{Scheme: "http", Host: host, Port: port}, Private: true},
		Method: http.MethodGet,
		URL:    mustURL(t, "http://"+host+":"+strconv.Itoa(port)+"/items"),
		Safe:   true,
	}
}

// TestClientCloseDrain is the client's own drain: Close stops admission, ends the
// flights it owns — the one in a budget wait, the one awaiting a slot and the one
// mid-send — and waits for exactly those, leaving a caller-owned mutation to the
// runtime that started it.
func TestClientCloseDrain(t *testing.T) {
	peer := newHoldingPeer(t)
	port := portOf(t, peer.srv)
	resolver := newResolver(map[string][]netip.Addr{
		"held.test":  addrs("127.0.0.1"),
		"delay.test": addrs("127.0.0.1"),
	})
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	// The budget wait never completes on its own: only the client's own lifetime
	// can end it, which is what Close has to do.
	// gates is the wait the client is promised: it is closed only when the proof
	// decides the budget wait may end.
	gates := make(chan time.Time)
	slot := newBlockingSlot(3)
	client := New(Config{
		Resolver: resolver,
		Slot:     slot.take,
		Now:      clock.now,
		After:    func(time.Duration) <-chan time.Time { return gates },
		Version:  "1.2.3",
	})

	// The first read of the delaying host is admitted at once, so its key exists
	// and can be held for the flight this proof parks.
	if _, err := client.Do(t.Context(), readAt(t, "delay.test", port), nil); err != nil {
		t.Fatalf("the admitted read: %v", err)
	}
	// The hold fits the call's own budget, so the flight really waits in it: only
	// Close can end that wait.
	client.budget.hold(keyHost, "delay.test", clock.now().Add(10*time.Second))

	// One slot of the three belongs to the test, so the second read below must wait
	// for one while the other two send.
	holdOne, err := slot.take(t.Context())
	if err != nil {
		t.Fatalf("hold a slot: %v", err)
	}
	defer holdOne()

	type result struct {
		response Response
		err      error
	}
	launch := func(op Op) (context.CancelFunc, chan result) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan result, 1)
		go func() {
			response, err := client.Do(ctx, op, nil)
			done <- result{response, err}
		}()
		return cancel, done
	}
	cancelDelay, delayed := launch(readAt(t, "delay.test", port))
	cancelWaiting, waiting := launch(readAt(t, "held.test", port))
	mutation := readAt(t, "held.test", port)
	mutation.Safe = false
	mutation.Method = http.MethodPost
	mutation.Body = `{"title":"synthetic"}`
	cancelMutation, mutating := launch(mutation)

	// The read at the peer took its slot and the mutation the last one, so the
	// other read waits for a slot and the delaying one for its budget.
	awaiting(t, peer.receive, "the read at the peer")
	awaiting(t, peer.receive, "the mutation at the peer")
	// The two reads own a permit each; the mutation is the caller's own and holds none.
	waitUntil(t, func() bool { return len(client.permits) == 2 }, "the two joined flights")

	// Two callers give up before Close: their flights keep running without them.
	cancelDelay()
	cancelWaiting()
	for _, done := range []chan result{delayed, waiting} {
		select {
		case got := <-done:
			if got.err == nil {
				t.Fatal("a cancelled caller returned a result")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a cancelled caller did not return")
		}
	}
	if got := len(client.permits); got != 2 {
		t.Fatalf("permits = %d after the callers returned, want the two flights still owned", got)
	}

	closeCtx, cancelClose := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelClose()
	if err := client.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(client.permits); got != 0 {
		t.Fatalf("permits = %d after Close, want every owned flight settled", got)
	}
	if got := len(client.flights); got != 0 {
		t.Fatalf("flights = %d after Close, want none live", got)
	}
	// The flight mid-send was aborted at the peer; the caller-owned mutation was
	// not, because it is the runtime's to end.
	if got := awaiting(t, peer.abort, "the aborted read"); got != "/items" {
		t.Fatalf("aborted %q, want the read's own request", got)
	}
	select {
	case got := <-peer.abort:
		t.Fatalf("the caller-owned mutation was aborted by Close (%q)", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Admission is closed for both kinds of call, and neither takes a slot.
	takenBefore := slot.takenSoFar()
	_, err = client.Do(t.Context(), readAt(t, "held.test", port), nil)
	if refused := wantError(t, err, ReasonClientClosed); refused.Attempts != 0 {
		t.Fatalf("a refused read counted %d attempts, want none", refused.Attempts)
	}
	_, err = client.Do(t.Context(), mutation, nil)
	if refused := wantError(t, err, ReasonClientClosed); refused.Unknown {
		t.Fatal("a mutation refused before it started was marked unknown")
	}
	if got := slot.takenSoFar(); got != takenBefore {
		t.Fatalf("slots taken = %d after Close, want no send", got)
	}
	if got := peer.served.Load(); got != 2 {
		t.Fatalf("peer served %d held requests, want the two Close ended and left", got)
	}

	// The caller-owned mutation ends only on its own terms: releasing the peer
	// answers it, and its result reaches a caller Close never touched.
	peer.releaseAll()
	select {
	case got := <-mutating:
		if got.err != nil {
			t.Fatalf("the caller-owned mutation after Close: %v", got.err)
		}
		if got.response.Body != "held" {
			t.Fatalf("mutation body = %q, want the peer's answer", got.response.Body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the caller-owned mutation never returned")
	}
	cancelMutation()
	// Close is idempotent, including once its flights are already gone.
	if err := client.Close(closeCtx); err != nil {
		t.Fatalf("the second Close: %v", err)
	}

	t.Run("a client that is not closed keeps its flights", func(t *testing.T) {
		second := newHoldingPeer(t)
		live := New(Config{
			Resolver: newResolver(map[string][]netip.Addr{"held.test": addrs("127.0.0.1")}),
			Slot:     newBlockingSlot(1).take,
			Now:      clock.now,
			After:    clock.after,
			Version:  "1.2.3",
		})
		done := make(chan error, 1)
		go func() {
			_, err := live.Do(context.Background(), readAt(t, "held.test", portOf(t, second.srv)), nil)
			done <- err
		}()
		awaiting(t, second.receive, "the held read")
		if got := len(live.permits); got != 1 {
			t.Fatalf("permits = %d, want the one running flight", got)
		}
		select {
		case err := <-done:
			t.Fatalf("the held read returned %v without Close", err)
		case <-time.After(50 * time.Millisecond):
		}
		second.releaseAll()
		if err := <-done; err != nil {
			t.Fatalf("the released read: %v", err)
		}
		if err := live.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := len(live.permits); got != 0 {
			t.Fatalf("permits = %d after Close, want none", got)
		}
	})

	// A slot the Close itself refuses is the client's own reason, not a spent
	// budget: the ceiling was never the decision, and no wire request was started.
	t.Run("a slot refused by the Close is client_closed", func(t *testing.T) {
		peer := newHoldingPeer(t)
		port := portOf(t, peer.srv)
		slot := newBlockingSlot(1)
		hold, err := slot.take(t.Context())
		if err != nil {
			t.Fatalf("hold the only slot: %v", err)
		}
		defer hold()
		client := New(Config{
			Resolver: newResolver(map[string][]netip.Addr{"held.test": addrs("127.0.0.1")}),
			Slot:     slot.take,
			Now:      clock.now,
			After:    clock.after,
			Version:  "1.2.3",
		})
		done := make(chan error, 1)
		go func() {
			_, err := client.Do(context.Background(), readAt(t, "held.test", port), nil)
			done <- err
		}()
		waitUntil(t, func() bool { return slot.waiting.Load() == 1 }, "the flight awaiting a slot")

		closeCtx, cancelClose := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancelClose()
		if err := client.Close(closeCtx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-done:
			refused := wantError(t, err, ReasonClientClosed)
			if refused.Attempts != 0 {
				t.Fatalf("attempts = %d, want 0: the refused slot started no wire request", refused.Attempts)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the flight awaiting a slot outlived the Close")
		}
		if got := peer.served.Load(); got != 0 {
			t.Fatalf("the peer served %d requests, want none", got)
		}
	})
}
