package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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

// newClockClientWith is newClockClient with the slot capacity the proof needs: a
// call holds one permit per physical send, so a proof that holds many sends at once
// gives the slot that much room.
func newClockClientWith(t *testing.T, clock *testClock, resolver Resolver, wrap func(http.RoundTripper) http.RoundTripper, capacity int) (*Client, *testSlot) {
	t.Helper()
	slot := newSlot(capacity)
	return New(Config{
		Resolver:  resolver,
		Slot:      slot.take,
		Now:       clock.now,
		After:     clock.after,
		Version:   "1.2.3",
		Transport: wrap,
	}), slot
}

// parked is a caller's park seam that reports the moment the call joined its
// flight, so a proof starts the second caller only once the first has one.
func parked(joined chan struct{}) func(context.Context, func(context.Context) error) error {
	return func(ctx context.Context, wait func(context.Context) error) error {
		select {
		case joined <- struct{}{}:
		default:
		}
		return wait(ctx)
	}
}

// readOp is one read of host's /items at port, scoped as the ops a proof merges.
func readOp(t *testing.T, host string, port int) Op {
	t.Helper()
	return Op{
		Name:               "items",
		Target:             &Target{ID: "approved", Origin: Origin{Scheme: "http", Host: host, Port: port}, Private: true},
		Method:             http.MethodGet,
		URL:                mustURL(t, "http://"+host+":"+strconv.Itoa(port)+"/items?page=2"),
		Safe:               true,
		ConfigRevision:     9,
		CredentialIdentity: "key-9",
		Language:           "en",
		Representation:     "json",
	}
}

// queryRead is opFor's read of srv's /items with the query a proof writes.
func queryRead(t *testing.T, srv *httptest.Server, query string) Op {
	t.Helper()
	op := opFor(t, srv)
	op.URL = mustURL(t, srv.URL+"/items?"+query)
	return op
}

// readPair runs two callers of their own read at once over one client and one peer.
// The peer answers only once both callers wait, and names the query it was asked, so
// what it received is the whole truth about the pair: one request is one flight both
// callers shared and two requests are two flights, with no timer deciding either way.
func readPair(t *testing.T, build func(srv *httptest.Server) [2]Op) ([2]Op, [2]Response, []string) {
	t.Helper()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		<-gate
		fmt.Fprint(w, r.URL.RawQuery)
	}))
	defer srv.Close()
	// A failed assertion releases the peer before the server waits for it.
	defer release()
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	client, _ := newClockClient(t, clock, loopbackResolver(), nil)
	ops := build(srv)

	type answer struct {
		index    int
		response Response
		err      error
	}
	joined := make(chan struct{}, 2)
	answers := make(chan answer, 2)
	for i, op := range ops {
		go func() {
			response, err := client.Do(t.Context(), op, parked(joined))
			answers <- answer{index: i, response: response, err: err}
		}()
		// The first caller is at its wait before the second starts, so a shared
		// flight is one the first caller's own op started.
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Fatalf("caller %d never reached its wait", i)
		}
	}
	release()
	var responses [2]Response
	for range ops {
		select {
		case got := <-answers:
			if got.err != nil {
				t.Fatalf("caller %d: %v", got.index, got.err)
			}
			responses[got.index] = got.response
		case <-time.After(30 * time.Second):
			t.Fatal("a caller never answered")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return ops, responses, asked
}

// TestRetrySpendsOneBudget proves AC-032's read budget: one call spends at most
// three wire requests and fifteen seconds, waits and hops included, and a peer's
// Retry-After is honoured only while the call can still afford it.
func TestRetrySpendsOneBudget(t *testing.T) {
	t.Run("a peer answering 503 spends the three wire requests", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, slot := newClockClient(t, clock, loopbackResolver(), nil)

		_, err := client.Do(t.Context(), opFor(t, srv), nil)
		refused := wantError(t, err, ReasonBudgetExhausted)
		if refused.Params["limit"] != "wire_requests" || refused.Params["max"] != strconv.Itoa(maxWireRequests) {
			t.Fatalf("params = %v, want limit=wire_requests max=%d", refused.Params, maxWireRequests)
		}
		if refused.Attempts != maxWireRequests {
			t.Fatalf("attempts = %d, want the three wire requests the refusal counted", refused.Attempts)
		}
		if got := served.Load(); got != maxWireRequests {
			t.Fatalf("peer served %d requests, want %d", got, maxWireRequests)
		}
		if waited := clock.waitedSoFar(); waited > flightBudget {
			t.Fatalf("waited %s, want the call's %s budget", waited, flightBudget)
		}
		if taken, released := slot.counts(); taken != maxWireRequests || released != maxWireRequests {
			t.Fatalf("slot taken/released = %d/%d, want one permit per wire request", taken, released)
		}
	})

	t.Run("a Retry-After past the budget is one request", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			w.Header().Set("Retry-After", "20")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)

		_, err := client.Do(t.Context(), opFor(t, srv), nil)
		refused := wantError(t, err, ReasonBudgetExhausted)
		if refused.Params["retry_after"] != "20s" {
			t.Fatalf("params = %v, want the peer's 20s named", refused.Params)
		}
		if refused.Attempts != 1 || served.Load() != 1 {
			t.Fatalf("attempts = %d served = %d, want exactly one wire request", refused.Attempts, served.Load())
		}
	})

	t.Run("a Retry-After inside the budget is honoured", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if served.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, "ok")
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)

		response, err := client.Do(t.Context(), opFor(t, srv), nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Body != "ok" || served.Load() != 2 {
			t.Fatalf("response = %q after %d requests, want ok after two", response.Body, served.Load())
		}
		if waited := clock.waitedSoFar(); waited < 2*time.Second {
			t.Fatalf("waited %s, want the peer's 2s honoured", waited)
		}
	})

	t.Run("a redirect hop charges its own host and its own wire request", func(t *testing.T) {
		var first, hop atomic.Int64
		var port int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == net.JoinHostPort("first.test", strconv.Itoa(port)) {
				first.Add(1)
				w.Header().Set("Location", "http://second.test:"+strconv.Itoa(port)+"/next")
				w.WriteHeader(http.StatusFound)
				return
			}
			hop.Add(1)
			fmt.Fprint(w, "hop")
		}))
		defer srv.Close()
		port = portOf(t, srv)
		resolver := newResolver(map[string][]netip.Addr{})
		resolver.answer("first.test", "127.0.0.1")
		resolver.answer("second.test", "127.0.0.1")
		// The hop is another origin, so it is not the approved one either: the
		// recorder sees its request and refuses it, which is what makes the hop's
		// budget charge observable without a second listener.
		recorder := &recorder{allow: 1}
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, resolver, recorder.wrap())

		if _, err := client.Do(t.Context(), readOp(t, "first.test", port), nil); err == nil {
			t.Fatal("Do succeeded, want the recorded hop not to be forwarded")
		}
		if first.Load() != 1 || hop.Load() != 0 {
			t.Fatalf("served %d then %d, want the original request and a recorded hop", first.Load(), hop.Load())
		}
		if got := len(recorder.requests()); got != 2 {
			t.Fatalf("recorded %d requests, want the request and its hop", got)
		}
		// Two hosts, each with its own key and the host's ceiling key.
		if keys := client.budget.size(); keys != 4 {
			t.Fatalf("budget keys = %d, want the hop's host charged as its own", keys)
		}
	})
}

// TestRecordedBoundsAreTheSpecLiterals pins the numbers AC-032 and AC-033 record
// beside the constants this package enforces: a bound that drifts from the contract
// reddens here instead of quietly redefining what the proofs around it accept.
func TestRecordedBoundsAreTheSpecLiterals(t *testing.T) {
	for _, bound := range []struct {
		name string
		got  int64
		want int64
	}{
		{"one read's wire requests", maxWireRequests, 3},
		{"one read's budget in seconds", int64(flightBudget / time.Second), 15},
		{"the limiter cache's keys", budgetKeys, 4096},
		{"a client's flights in flight", maxFlights, 32},
		{"a safe op's request body", maxSafeBody, 1 << 20},
	} {
		if bound.got != bound.want {
			t.Errorf("%s = %d, want the recorded %d", bound.name, bound.got, bound.want)
		}
	}
}

// TestSingleFlightOriginScope proves a read merges only with the reads of its own
// destination: callers that name different origins each get their own physical
// request and their own answer, and the merge key is the canonical origin of the
// request's URL, not a spelling of it.
func TestSingleFlightOriginScope(t *testing.T) {
	t.Run("each origin sends once and answers its own callers", func(t *testing.T) {
		gate := make(chan struct{})
		held := make(chan struct{}, 2)
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			select {
			case held <- struct{}{}:
			default:
			}
			<-gate
			fmt.Fprint(w, r.Host)
		}))
		defer srv.Close()
		port := portOf(t, srv)
		resolver := newResolver(map[string][]netip.Addr{})
		resolver.answer("one.test", "127.0.0.1")
		resolver.answer("two.test", "127.0.0.1")
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, resolver, nil)

		type answer struct {
			response Response
			err      error
		}
		joined := make(chan struct{}, 1)
		first := make(chan answer, 1)
		go func() {
			response, err := client.Do(t.Context(), readOp(t, "one.test", port), parked(joined))
			first <- answer{response, err}
		}()
		// The first caller of one origin is held by the peer, so the second caller
		// of it joins a running flight instead of starting another.
		<-held
		second := make(chan answer, 1)
		go func() {
			response, err := client.Do(t.Context(), readOp(t, "one.test", port), parked(joined))
			second <- answer{response, err}
		}()
		<-joined
		other := make(chan answer, 1)
		go func() {
			response, err := client.Do(t.Context(), readOp(t, "two.test", port), nil)
			other <- answer{response, err}
		}()
		<-held
		close(gate)

		for i, answers := range []chan answer{first, second, other} {
			got := <-answers
			if got.err != nil {
				t.Fatalf("caller %d: %v", i, got.err)
			}
			want := net.JoinHostPort("one.test", strconv.Itoa(port))
			if i == 2 {
				want = net.JoinHostPort("two.test", strconv.Itoa(port))
			}
			if got.response.Body != want {
				t.Fatalf("caller %d body = %q, want its own origin %q", i, got.response.Body, want)
			}
		}
		if served.Load() != 2 {
			t.Fatalf("peer served %d requests, want one per origin", served.Load())
		}
	})

	t.Run("the merge key is the canonical origin", func(t *testing.T) {
		base := func(written string) Op {
			return Op{
				Name:               "items",
				Target:             &Target{ID: "approved", Origin: Origin{Scheme: "https", Host: "peer.test", Port: 443}},
				Method:             http.MethodGet,
				URL:                mustURL(t, written),
				Safe:               true,
				ConfigRevision:     9,
				CredentialIdentity: "key-9",
				Language:           "en",
				Representation:     "json",
			}
		}
		same := []string{
			"https://peer.test/items?b=2&a=1",
			"https://PEER.test:443/items?a=1&b=2",
			"https://peer.test./items?a=1&b=2",
		}
		want := flightKey(base(same[0]))
		for _, written := range same[1:] {
			if got := flightKey(base(written)); got != want {
				t.Fatalf("%s has a different merge key than %s", written, same[0])
			}
		}
		for _, pair := range [][]string{
			{"http://peer.test/items?a=1&b=2", "http://PEER.test:80/items?a=1&b=2"},
			{"http://peer.test:80/items?a=1&b=2", "http://peer.test./items?b=2&a=1"},
		} {
			if flightKey(base(pair[0])) != flightKey(base(pair[1])) {
				t.Fatalf("%s has a different merge key than %s", pair[1], pair[0])
			}
		}
		for _, other := range []struct {
			name string
			op   Op
		}{
			{"another scheme", base("http://peer.test/items?a=1&b=2")},
			{"another port", base("https://peer.test:8443/items?a=1&b=2")},
			{"another hostname", base("https://other.test/items?a=1&b=2")},
		} {
			if flightKey(other.op) == want {
				t.Fatalf("%s merged with the read", other.name)
			}
		}
		for _, changed := range []struct {
			name string
			op   Op
		}{
			{"language", func() Op { op := base(same[0]); op.Language = "de"; return op }()},
			{"representation", func() Op { op := base(same[0]); op.Representation = "xml"; return op }()},
			{"configuration revision", func() Op { op := base(same[0]); op.ConfigRevision = 10; return op }()},
			{"credential", func() Op { op := base(same[0]); op.CredentialIdentity = "key-10"; return op }()},
			{"operation", func() Op { op := base(same[0]); op.Name = "other"; return op }()},
			{"body", func() Op { op := base(same[0]); op.Body = "q=1"; return op }()},
		} {
			if flightKey(changed.op) == want {
				t.Fatalf("another %s merged with the read", changed.name)
			}
		}
	})

	t.Run("a differing query is two keys and two requests", func(t *testing.T) {
		// The two reads differ in nothing but their query: one builder, one
		// server, and each caller's answer names the query its own request asked.
		ops, responses, asked := readPair(t, func(srv *httptest.Server) [2]Op {
			return [2]Op{queryRead(t, srv, "page=1"), queryRead(t, srv, "page=2")}
		})
		if flightKey(ops[0]) == flightKey(ops[1]) {
			t.Fatal("two queries share one merge key, want the query part of the read's scope")
		}
		if len(asked) != 2 {
			t.Fatalf("peer served %d requests, want one per query: %v", len(asked), asked)
		}
		for i, response := range responses {
			if want := ops[i].URL.RawQuery; response.Body != want {
				t.Fatalf("caller %d body = %q, want its own query %q", i, response.Body, want)
			}
		}
	})

	t.Run("a credential-named query is one key and one request", func(t *testing.T) {
		// Both reads name the credential's parameter in the URL's own query, with a
		// value of its own each: only a key that exempts the named parameter merges
		// them, and the peer's answer proves the value did travel.
		const alpha, omega = "bxk_cred_alpha0123456789", "bxk_cred_omega9876543210"
		ops, responses, asked := readPair(t, func(srv *httptest.Server) [2]Op {
			var pair [2]Op
			for i, value := range []string{alpha, omega} {
				pair[i] = queryRead(t, srv, "api_key="+value+"&page=2")
				target := *pair[i].Target
				target.Credential = &Credential{
					Header: "Authorization",
					Query:  "api_key",
					Value:  func(context.Context) ([]byte, error) { return []byte(value), nil },
				}
				pair[i].Target = &target
			}
			return pair
		})
		if flightKey(ops[0]) != flightKey(ops[1]) {
			t.Fatal("two values of the credential's own query separated, want the credential exempt from the merge key")
		}
		want := "api_key=" + alpha + "&page=2"
		if len(asked) != 1 || asked[0] != want {
			t.Fatalf("peer was asked %v, want one request with %q", asked, want)
		}
		for i, response := range responses {
			if response.Body != want {
				t.Fatalf("caller %d body = %q, want the one answer both callers share", i, response.Body)
			}
		}
	})
}

// TestSafeWriteIsSentOnce proves the method is the second authority over the
// caller's Safe claim: a write a caller marked safe is one wire request whatever
// the peer answers, so nothing resends it.
func TestSafeWriteIsSentOnce(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	client, _ := newClockClient(t, clock, loopbackResolver(), nil)
	op := opFor(t, srv)
	op.Method = http.MethodPost
	op.Body = `{"title":"synthetic"}`

	_, err := client.Do(t.Context(), op, nil)
	refused := wantError(t, err, ReasonOutcomeUnknown)
	if !refused.Unknown {
		t.Fatal("unknown = false, want a write the peer may already have applied marked unknown")
	}
	if served.Load() != 1 || refused.Attempts != 1 {
		t.Fatalf("peer served %d requests in %d attempts, want the write sent exactly once", served.Load(), refused.Attempts)
	}
	if waited := clock.waitedSoFar(); waited != 0 {
		t.Fatalf("waited %s, want no retry of a write", waited)
	}
}

// TestSafeWriteBodyScope proves what a caller's Safe claim still buys a write: its
// body is the ordinary request cap and nothing more. Two identical safe writes stay
// two writes with their own results, and a body past the cap is refused before any
// request.
func TestSafeWriteBodyScope(t *testing.T) {
	t.Run("identical safe bodies are two writes", func(t *testing.T) {
		gate := make(chan struct{})
		held := make(chan struct{}, 2)
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			held <- struct{}{}
			<-gate
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		// A failed assertion releases the peer before the server waits for it.
		releasePeer := sync.OnceFunc(func() { close(gate) })
		defer releasePeer()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)
		op := opFor(t, srv)
		op.Method = http.MethodPost
		op.Body = `{"query":"synthetic"}`

		bodies := make(chan string, 2)
		for range 2 {
			go func() {
				response, err := client.Do(t.Context(), op, nil)
				if err != nil {
					bodies <- "error: " + err.Error()
					return
				}
				bodies <- response.Body
			}()
		}
		select {
		case <-held:
		case <-time.After(30 * time.Second):
			t.Fatal("the write never reached the peer")
		}
		// A write joins no flight, so it owns no client permit at the peer: two
		// callers' identical writes cannot collapse into one.
		if got := len(client.permits); got != 0 {
			t.Fatalf("flight permits = %d with a write at the peer, want a write to own no flight", got)
		}
		select {
		case <-held:
		case <-time.After(30 * time.Second):
			t.Fatal("the second identical write never reached the peer")
		}
		releasePeer()
		for i := range 2 {
			if got := <-bodies; got != op.Body {
				t.Fatalf("caller %d body = %q, want its own request body", i, got)
			}
		}
		if served.Load() != 2 {
			t.Fatalf("peer served %d requests, want two callers' writes kept apart", served.Load())
		}
	})

	t.Run("identical unsafe writes are two writes whatever the flag says", func(t *testing.T) {
		gate := make(chan struct{})
		held := make(chan struct{}, 2)
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			held <- struct{}{}
			<-gate
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		// A failed assertion releases the peer before the server waits for it.
		releasePeer := sync.OnceFunc(func() { close(gate) })
		defer releasePeer()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)
		op := opFor(t, srv)
		op.Method = http.MethodPost
		op.Safe = false
		op.Body = `{"query":"synthetic"}`

		bodies := make(chan string, 2)
		for range 2 {
			go func() {
				response, err := client.Do(t.Context(), op, nil)
				if err != nil {
					bodies <- "error: " + err.Error()
					return
				}
				bodies <- response.Body
			}()
		}
		// Both writes are at the peer at once: the answer is held until both
		// arrived, so neither caller waited for the other.
		for i := range 2 {
			select {
			case <-held:
			case <-time.After(30 * time.Second):
				t.Fatalf("write %d never reached the peer", i)
			}
		}
		client.mu.Lock()
		flights, permits := len(client.flights), len(client.permits)
		client.mu.Unlock()
		if flights != 0 || permits != 0 {
			t.Fatalf("flights = %d permits = %d with two writes at the peer, want a write to own no flight", flights, permits)
		}
		releasePeer()
		for i := range 2 {
			if got := <-bodies; got != op.Body {
				t.Fatalf("caller %d body = %q, want its own request body", i, got)
			}
		}
		if served.Load() != 2 {
			t.Fatalf("peer served %d requests, want two callers' writes kept apart", served.Load())
		}
	})

	t.Run("a safe body past the cap is refused before any request", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1) }))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)
		op := opFor(t, srv)
		op.Method = http.MethodPost
		op.Body = strings.Repeat("x", maxSafeBody+1)

		_, err := client.Do(t.Context(), op, nil)
		refused := wantError(t, err, ReasonBodyTooLarge)
		if refused.Params["max"] != strconv.Itoa(maxSafeBody) {
			t.Fatalf("params = %v, want max=%d", refused.Params, maxSafeBody)
		}
		if served.Load() != 0 {
			t.Fatalf("peer served %d requests, want the refusal before any dial", served.Load())
		}
	})
}

// TestSingleFlightCancellation proves a caller's own cancellation is its own: the
// flight keeps running for the callers still joined to it, the request the peer
// received is the snapshot taken before Do, and a client with every flight busy
// refuses the next call instead of queueing it.
func TestSingleFlightCancellation(t *testing.T) {
	t.Run("a cancelled caller leaves the result to the others", func(t *testing.T) {
		gate := make(chan struct{})
		held := make(chan struct{}, 1)
		var served atomic.Int64
		var mu sync.Mutex
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			select {
			case held <- struct{}{}:
			default:
			}
			<-gate
			fmt.Fprint(w, "ok")
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)
		// The first caller's own op: changing it after the call returned changes
		// nothing that was already sent.
		op := opFor(t, srv)

		callerCtx, cancelCaller := context.WithCancel(t.Context())
		joined := make(chan struct{}, 1)
		var cancelled error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancelled = client.Do(callerCtx, op, parked(joined))
		}()
		<-held
		// The caller gives up while its flight is still at the peer.
		cancelCaller()
		wg.Wait()
		if !errors.Is(cancelled, context.Canceled) {
			t.Fatalf("the cancelled caller returned %v, want its own context error", cancelled)
		}
		// Its op is the caller's own memory: changing it now changes nothing that
		// was already sent.
		op.URL.Path = "/tampered"
		op.Target.ID = "tampered"

		// The second caller asks for the same thing with its own op, so it joins the
		// flight the first caller's cancellation left running.
		second := make(chan error, 1)
		var body string
		go func() {
			response, err := client.Do(t.Context(), opFor(t, srv), nil)
			body = response.Body
			second <- err
		}()
		close(gate)
		if err := <-second; err != nil {
			t.Fatalf("the joined caller: %v", err)
		}
		if body != "ok" {
			t.Fatalf("the joined caller body = %q, want the flight's own result", body)
		}
		mu.Lock()
		sent := append([]string(nil), paths...)
		mu.Unlock()
		if len(sent) != 1 || sent[0] != "/items" {
			t.Fatalf("the peer received %v, want the one path the flight was started with", sent)
		}
		if served.Load() != 1 {
			t.Fatalf("peer served %d requests, want the cancelled caller's flight reused", served.Load())
		}
	})

	t.Run("a full client refuses the next call and frees it when the flights settle", func(t *testing.T) {
		release := make(chan struct{})
		var served atomic.Int64
		arrived := make(chan struct{}, maxFlights+8)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-release
			fmt.Fprint(w, "held")
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClientWith(t, clock, loopbackResolver(), nil, maxFlights)

		ops := make([]Op, maxFlights)
		for i := range maxFlights {
			ops[i] = opFor(t, srv)
			ops[i].URL = mustURL(t, srv.URL+"/items?page="+strconv.Itoa(i))
		}
		var wg sync.WaitGroup
		for _, op := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Each caller gives up while its own flight waits at the peer.
				ctx, cancel := context.WithCancel(t.Context())
				go func() {
					<-time.After(20 * time.Millisecond)
					cancel()
				}()
				defer cancel()
				_, _ = client.Do(ctx, op, nil)
			}()
		}
		for range maxFlights {
			select {
			case <-arrived:
			case <-time.After(30 * time.Second):
				t.Fatal("the held requests never reached the peer")
			}
		}
		wg.Wait()

		over := opFor(t, srv)
		over.URL = mustURL(t, srv.URL+"/items?page=over")
		_, err := client.Do(t.Context(), over, nil)
		refused := wantError(t, err, ReasonQueueFull)
		if refused.Params["retry_after_s"] != "2" || refused.Attempts != 0 {
			t.Fatalf("queue refusal = %v, want retry_after_s=2 and no attempt", refused.Params)
		}
		if served.Load() != maxFlights {
			t.Fatalf("peer served %d requests, want the held flights only", served.Load())
		}

		close(release)
		waitForPermits(t, client)
		after := opFor(t, srv)
		after.URL = mustURL(t, srv.URL+"/items?page=after")
		if _, err := client.Do(t.Context(), after, nil); err != nil {
			t.Fatalf("the call after the flights settled: %v", err)
		}
		if served.Load() != maxFlights+1 {
			t.Fatalf("peer served %d requests, want the settled flights then this one", served.Load())
		}
	})
}

// waitForPermits waits until the client holds none of its flight permits.
func waitForPermits(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for len(client.permits) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d flight permits still held", len(client.permits))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMutationClasses proves AC-032's mutation contract: one wire request, no
// retry, and an outcome that says whether a resend could duplicate the effect.
func TestMutationClasses(t *testing.T) {
	closedPort := func(t *testing.T) int {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return port
	}
	for _, tc := range []struct {
		name    string
		status  int
		reason  string
		unknown bool
	}{
		{name: "an applied write", status: http.StatusCreated},
		{name: "a rejected write", status: http.StatusUnprocessableEntity, reason: ReasonRejected},
		{name: "a peer that may have applied it", status: http.StatusInternalServerError, reason: ReasonOutcomeUnknown, unknown: true},
		{name: "a peer asking for another request", status: http.StatusTooManyRequests, reason: ReasonOutcomeUnknown, unknown: true},
		{name: "an unsafe redirect", status: http.StatusFound, reason: ReasonRedirectDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var served atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served.Add(1)
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			client, _ := newClockClient(t, clock, loopbackResolver(), nil)
			op := opFor(t, srv)
			op.Method = http.MethodPost
			op.Safe = false
			op.Body = `{"title":"synthetic"}`

			_, err := client.Do(t.Context(), op, nil)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Do: %v", err)
				}
			} else {
				refused := wantError(t, err, tc.reason)
				if refused.Unknown != tc.unknown {
					t.Fatalf("unknown = %t, want %t", refused.Unknown, tc.unknown)
				}
			}
			if served.Load() != 1 {
				t.Fatalf("peer served %d requests, want a mutation sent exactly once", served.Load())
			}
			if clock.waitedSoFar() != 0 {
				t.Fatalf("waited %s, want no retry of a mutation", clock.waitedSoFar())
			}
		})
	}

	t.Run("a dial that never reached the peer is not_sent", func(t *testing.T) {
		port := closedPort(t)
		host := "refused.test"
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, newResolver(map[string][]netip.Addr{host: addrs("127.0.0.1")}), nil)
		_, err := client.Do(t.Context(), Op{
			Target: &Target{ID: "refused", Origin: Origin{Scheme: "http", Host: host, Port: port}, Private: true},
			Method: http.MethodPost,
			URL:    mustURL(t, "http://"+host+":"+strconv.Itoa(port)+"/items"),
			Body:   `{"title":"synthetic"}`,
		}, nil)
		refused := wantError(t, err, ReasonNotSent)
		if refused.Unknown {
			t.Fatal("not_sent marked unknown, want a write no byte of which left")
		}
	})
}

// TestLocateDecidesAfterTheWalk proves the reconciliation read: a presence is
// decided only once the whole bounded walk is in, and an unfinished walk decides
// nothing at all.
func TestLocateDecidesAfterTheWalk(t *testing.T) {
	pages := func(walk map[string]Page) func(context.Context, string) (Page, error) {
		return func(_ context.Context, cursor string) (Page, error) {
			page, ok := walk[cursor]
			if !ok {
				return Page{}, fmt.Errorf("no page %q", cursor)
			}
			return page, nil
		}
	}
	full := map[string]Page{
		"":   {Identities: []string{"a", "b"}, Next: "p2"},
		"p2": {Identities: []string{"d"}},
	}
	for _, tc := range []struct {
		name string
		walk map[string]Page
		cap  int
		want Presence
	}{
		{name: "absent after the whole walk", walk: full, cap: 5, want: PresenceAbsent},
		{name: "present on a later page", walk: map[string]Page{
			"":   {Identities: []string{"a"}, Next: "p2"},
			"p2": {Identities: []string{"c"}},
		}, cap: 5, want: PresencePresent},
		{name: "duplicated on a later page", walk: map[string]Page{
			"":   {Identities: []string{"c"}, Next: "p2"},
			"p2": {Identities: []string{"c"}},
		}, cap: 5, want: PresenceAmbiguous},
		{name: "the cap reached with more to read", walk: full, cap: 1, want: PresenceUnknown},
		{name: "a repeated cursor", walk: map[string]Page{
			"":   {Identities: []string{"a"}, Next: "p2"},
			"p2": {Identities: []string{"c"}, Next: "p2"},
		}, cap: 5, want: PresenceUnknown},
		{name: "a walk of no pages", walk: full, cap: 0, want: PresenceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Locate(t.Context(), pages(tc.walk), "c", tc.cap)
			if err != nil {
				t.Fatalf("Locate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("presence = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a failed read decides nothing", func(t *testing.T) {
		failure := errors.New("lookup unavailable")
		got, err := Locate(t.Context(), func(context.Context, string) (Page, error) {
			return Page{}, failure
		}, "c", 5)
		if !errors.Is(err, failure) || got != PresenceUnknown {
			t.Fatalf("Locate = %v %v, want the failure with an undecided presence", got, err)
		}
	})
}

// countingBody counts the bodies the client closed.
type countingBody struct {
	io.ReadCloser
	closed *atomic.Int64
}

func (b *countingBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

// closeCounter is the transport wrap that counts every response body the client
// closed, whatever path it ended on.
type closeCounter struct {
	inner  http.RoundTripper
	closed atomic.Int64
}

func (c *closeCounter) wrap(transport http.RoundTripper) http.RoundTripper {
	c.inner = transport
	return c
}

func (c *closeCounter) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.inner.RoundTrip(request)
	if err != nil {
		return response, err
	}
	response.Body = &countingBody{ReadCloser: response.Body, closed: &c.closed}
	return response, nil
}

// TestFlightKeepsNoSecretAndClosesBodies proves the retained state of one call: no
// refusal or retained structure holds the request body, and every physical response
// body was closed whatever path the call ended on.
func TestFlightKeepsNoSecretAndClosesBodies(t *testing.T) {
	const sentinel = "bxk_body_sentinel0123456789"
	t.Run("every physical body is closed", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if served.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, "ok")
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		counter := &closeCounter{}
		client, _ := newClockClient(t, clock, loopbackResolver(), counter.wrap)

		op := opFor(t, srv)
		go func() {
			if _, err := client.Do(context.Background(), op, nil); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
		deadline := time.Now().Add(30 * time.Second)
		for counter.closed.Load() < 2 {
			if time.Now().After(deadline) {
				t.Fatalf("closed %d bodies, want one per physical response", counter.closed.Load())
			}
			time.Sleep(time.Millisecond)
		}
	})

	t.Run("no retained state holds the body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}))
		defer srv.Close()
		clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		client, _ := newClockClient(t, clock, loopbackResolver(), nil)
		op := opFor(t, srv)
		op.Method = http.MethodPost
		op.Body = `{"title":"` + sentinel + `"}`
		// The body is an input to a fixed digest, never a payload carried beside it.
		other := op
		other.Body = op.Body + " "
		if flightKey(other) == flightKey(op) {
			t.Fatal("the merge key ignores the body, want the body to be one of its inputs")
		}

		_, err := client.Do(t.Context(), op, nil)
		refused := wantError(t, err, ReasonRejected)
		if strings.Contains(refused.Error(), sentinel) {
			t.Fatalf("the rendered refusal = %q, want no body", refused.Error())
		}
		for name, value := range refused.Params {
			if strings.Contains(value, sentinel) {
				t.Fatalf("refusal param %s = %q, want no body", name, value)
			}
		}
		// The client keeps no flight for the settled call, and exactly the two
		// limiter keys this write spent: nothing it retains is the request.
		client.mu.Lock()
		live := len(client.flights)
		client.mu.Unlock()
		if live != 0 {
			t.Fatalf("flights retained = %d, want none", live)
		}
		if keys := client.budget.size(); keys != 2 {
			t.Fatalf("budget keys = %d, want the host's own key and its ceiling", keys)
		}
	})
}
