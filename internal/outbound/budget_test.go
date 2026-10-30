package outbound

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is the client's one clock. After advances it by exactly what the
// client asked to wait for and returns at once, so a budget is measured on the
// recorded waits instead of the wall.
type testClock struct {
	mu     sync.Mutex
	at     time.Time
	waited []time.Duration
}

func newClock(at time.Time) *testClock { return &testClock{at: at} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.waited = append(c.waited, d)
	at := c.at
	c.mu.Unlock()
	ready := make(chan time.Time, 1)
	ready <- at
	return ready
}

// waitedSoFar is how long the client asked to wait in total.
func (c *testClock) waitedSoFar() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total time.Duration
	for _, d := range c.waited {
		total += d
	}
	return total
}

// advance moves the clock without a wait, so a proof can reach an idle cache.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newClockClient is the client under test on the fake clock, with a slot that
// answers at once.
func newClockClient(t *testing.T, clock *testClock, resolver Resolver, wrap func(http.RoundTripper) http.RoundTripper) (*Client, *testSlot) {
	t.Helper()
	slot := newSlot(4)
	return New(Config{
		Resolver:  resolver,
		Slot:      slot.take,
		Now:       clock.now,
		After:     clock.after,
		Version:   "1.2.3",
		Transport: wrap,
	}), slot
}

// portOf is the port srv listens on.
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
	if err != nil {
		t.Fatalf("server port in %q: %v", srv.URL, err)
	}
	return port
}

// opFor is one read of srv's /items.
func opFor(t *testing.T, srv *httptest.Server) Op {
	t.Helper()
	return Op{
		Target: approvedTarget(t, srv),
		Method: http.MethodGet,
		URL:    mustURL(t, srv.URL+"/items"),
		Safe:   true,
	}
}

// aliasCount is the number of approved hosts a proof fills the limiter cache with:
// each spends a host key and the host's ceiling key, so half of them fill all 4,096
// entries.
const aliasCount = budgetKeys / 2

// TestBudgetKeysBound proves the limiter cache bound: approved hosts fill it key by
// key, the next new host is refused before its dial, and every allowance that was
// already there stays.
func TestBudgetKeysBound(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	port := portOf(t, srv)

	answers := make(map[string][]netip.Addr, aliasCount)
	for i := range aliasCount {
		answers[aliasName(i)] = addrs("127.0.0.1")
	}
	answers["fresh.test"] = addrs("127.0.0.1")
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	client, _ := newClockClient(t, clock, newResolver(answers), nil)

	read := func(host string) (Response, error) {
		return client.Do(t.Context(), Op{
			Target: &Target{ID: host, Origin: Origin{Scheme: "http", Host: host, Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://"+host+":"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil)
	}
	for i := range aliasCount {
		if _, err := read(aliasName(i)); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if keys := client.budget.size(); keys != budgetKeys {
		t.Fatalf("budget keys = %d, want the recorded %d", keys, budgetKeys)
	}
	before := served.Load()

	_, err := read("fresh.test")
	refused := wantError(t, err, ReasonBudgetExhausted)
	if refused.Params["limit"] != "budget_keys" || refused.Params["max"] != strconv.Itoa(budgetKeys) {
		t.Fatalf("params = %v, want limit=budget_keys max=%d", refused.Params, budgetKeys)
	}
	if refused.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: the refusal is decided before any dial", refused.Attempts)
	}
	if after := served.Load(); after != before {
		t.Fatalf("the peer served %d more requests, want the refused call to dial nothing", after-before)
	}
	// Every allowance that was already there is untouched: the first host answers
	// again without waiting, and no idle entry was evicted behind it.
	if _, err := read(aliasName(0)); err != nil {
		t.Fatalf("the first approved host after the refusal: %v", err)
	}
	if clock.waitedSoFar() != 0 {
		t.Fatalf("waited %s, want the recorded burst to admit every read", clock.waitedSoFar())
	}

	// A refusal decided inside an attempt keeps the attempt count it was decided
	// in: a charge that drops it hides how far the call had already got.
	t.Run("a refused charge keeps the attempt count", func(t *testing.T) {
		fresh := Op{
			Target: &Target{ID: "fresh", Origin: Origin{Scheme: "http", Host: "fresh.test", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://fresh.test:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}
		_, release, refusal := client.charge(fresh, fresh.URL, 2)
		if refusal == nil {
			release()
			t.Fatal("the charge over the cache bound was admitted")
		}
		if refusal.Attempts != 2 {
			t.Fatalf("attempts = %d, want the 2 the charge was decided in", refusal.Attempts)
		}
	})

	// The refusal above is the bound at that instant, not a cache nothing may
	// leave: once every entry has been idle for the recorded idle window, the next
	// new host reclaims keys instead of being refused.
	t.Run("an idle key is reclaimed instead of refusing a new host", func(t *testing.T) {
		clock.advance(budgetIdle)
		before := served.Load()
		response, err := read("fresh.test")
		if err != nil {
			t.Fatalf("the read after every key went idle: %v", err)
		}
		if response.Body != "ok" {
			t.Fatalf("body = %q, want the peer's answer", response.Body)
		}
		if after := served.Load(); after != before+1 {
			t.Fatalf("the peer served %d requests, want the reclaimed host's one", after-before)
		}
		if keys := client.budget.size(); keys != 2 {
			t.Fatalf("budget keys = %d after the reclaim, want the reclaimed host's own key and its ceiling", keys)
		}
	})
}

// aliasName is one approved synthetic host.
func aliasName(i int) string { return fmt.Sprintf("alias%04d.test", i) }

// retryHeader is a peer's Retry-After answer.
func retryHeader(seconds int) http.Header {
	header := make(http.Header, 1)
	header.Set("Retry-After", strconv.Itoa(seconds))
	return header
}

// TestBudgetIndexersStayIndependent proves the shared host ceiling without merging
// budgets: the Retry-After one indexer received holds only its own key, and a
// host-wide throttle holds the shared key so both wait.
func TestBudgetIndexersStayIndependent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	client, _ := newClockClient(t, clock, loopbackResolver(), nil)
	hop := mustURL(t, srv.URL+"/items")
	first := opFor(t, srv)
	first.Budget = Budget{Indexer: "indexer-a"}
	sibling := opFor(t, srv)
	sibling.Budget = Budget{Indexer: "indexer-b"}
	deadline := clock.now().Add(time.Hour)
	seed := int64(7)

	// The first attempt spends each op's own key and the host ceiling.
	for _, op := range []Op{first, sibling} {
		delay, release, refusal := client.charge(op, hop, 0)
		if refusal != nil || delay != 0 {
			t.Fatalf("charge %s = %s %v, want an immediate allowance", op.Budget.Indexer, delay, refusal)
		}
		release()
	}
	if refusal := client.backoff(first, hop, http.StatusTooManyRequests, retryHeader(30), 1, seed, deadline); refusal != nil {
		t.Fatalf("backoff = %v, want the peer's wait to be inside the call's budget", refusal)
	}
	if delay, release, refusal := client.charge(sibling, hop, 0); refusal != nil || delay != 0 {
		t.Fatalf("the sibling indexer waited %s (%v), want its own allowance untouched", delay, refusal)
	} else {
		release()
	}
	if delay, release, refusal := client.charge(first, hop, 0); refusal != nil || delay < 30*time.Second {
		t.Fatalf("the throttled indexer waited %s (%v), want the peer's 30s", delay, refusal)
	} else {
		release()
	}

	// A host-wide throttle is the host's own key: both indexers wait for it.
	first.Throttle = func(int, http.Header) Scope { return ScopeHost }
	if refusal := client.backoff(first, hop, http.StatusTooManyRequests, retryHeader(45), 1, seed, deadline); refusal != nil {
		t.Fatalf("host-wide backoff = %v", refusal)
	}
	for _, op := range []Op{first, sibling} {
		delay, release, refusal := client.charge(op, hop, 0)
		if refusal != nil {
			t.Fatalf("charge %s: %v", op.Budget.Indexer, refusal)
		}
		if delay < 45*time.Second {
			t.Fatalf("%s waited %s, want the host-wide 45s", op.Budget.Indexer, delay)
		}
		release()
	}
}

// TestBudgetCeilingBoundsTheSum proves the host ceiling is charged in both modes:
// independent indexer allowances do not add up to more than the host's own bound.
func TestBudgetCeilingBoundsTheSum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	client, _ := newClockClient(t, clock, loopbackResolver(), nil)
	hop := mustURL(t, srv.URL+"/items")

	var spent []func()
	t.Cleanup(func() {
		for _, release := range spent {
			release()
		}
	})
	for i := range ceilingBurst {
		op := opFor(t, srv)
		op.Budget = Budget{Indexer: "indexer-" + strconv.Itoa(i)}
		delay, release, refusal := client.charge(op, hop, 0)
		if refusal != nil {
			t.Fatalf("charge %d: %v", i, refusal)
		}
		if delay != 0 {
			t.Fatalf("charge %d waited %s, want the ceiling's own burst", i, delay)
		}
		// Only the wait's accounting is released at cleanup: the token stays
		// spent, so these fifty charges are the ceiling's whole burst.
		spent = append(spent, release)
	}
	// One more indexer, with its own allowance untouched, waits for the host.
	op := opFor(t, srv)
	op.Budget = Budget{Indexer: "indexer-late"}
	delay, release, refusal := client.charge(op, hop, 0)
	if refusal != nil {
		t.Fatalf("charge over the ceiling: %v", refusal)
	}
	defer release()
	if delay <= 0 {
		t.Fatal("the indexer over the host ceiling was admitted at once, want the ceiling to bound it")
	}
}
