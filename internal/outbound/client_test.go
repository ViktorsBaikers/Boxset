package outbound

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/ingest"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

// quietLog silences a TLS test server's handshake diagnostics, which are expected
// whenever a certificate is refused.
var quietLog = log.New(io.Discard, "", 0)

// The environment proxy is read once, process-wide, on the first transport that asks.
// It is installed here, before any test dials, so a client that inherited it would
// answer every test by talking to a closed port rather than its origin.
func init() {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		_ = os.Setenv(name, "http://127.0.0.1:9")
	}
	_ = os.Setenv("NO_PROXY", "")
}

// TestOutboundDestinations proves AC-031: a wire request reaches only a validated
// destination. The environment's proxy never sees a request, a forbidden host
// digest is refused before it is resolved, a mutation's redirect is refused after
// exactly one request, a response body is decoded through the ingest owner's cap,
// and a refusal's text carries a marker where a sentinel was.
func TestOutboundDestinations(t *testing.T) {
	t.Run("the approved origin receives one request with the project user agent", func(t *testing.T) {
		var served int
		var agent string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			agent = r.Header.Get("User-Agent")
			fmt.Fprint(w, "ok")
		}))
		defer srv.Close()
		client, slot := newTestClient(loopbackResolver(), nil)

		response, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Status != http.StatusOK || response.Body != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", response.Status, response.Body)
		}
		if served != 1 {
			t.Fatalf("server served %d requests, want 1", served)
		}
		if want := "Boxset/1.2.3 " + userAgentSuffix; agent != want {
			t.Fatalf("user agent = %q, want %q", agent, want)
		}
		if taken, released := slot.counts(); taken != 1 || released != 1 {
			t.Fatalf("slot taken/released = %d/%d, want one permit per physical send", taken, released)
		}
	})

	t.Run("the environment proxy is never used", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
		defer srv.Close()
		port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
		if err != nil {
			t.Fatalf("server port: %v", err)
		}
		// The destination is named, not a loopback literal, so a transport that
		// inherited the environment proxy would send this request to the closed port
		// the environment names instead of dialing the origin.
		resolver := newResolver(map[string][]netip.Addr{"proxy.test": addrs("127.0.0.1")})
		client, _ := newTestClient(resolver, nil)
		if _, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "http", Host: "proxy.test", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://proxy.test:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if served != 1 {
			t.Fatalf("the origin served %d requests, want 1", served)
		}
	})

	t.Run("a forbidden host digest is refused before any lookup", func(t *testing.T) {
		digest := sha256.Sum256([]byte("forbidden.test"))
		resolver := newResolver(map[string][]netip.Addr{"sub.forbidden.test": addrs("192.0.2.10")})
		client := New(Config{
			Resolver:       resolver,
			Slot:           newSlot(4).take,
			Version:        "1.2.3",
			ForbiddenHosts: []string{hex.EncodeToString(digest[:])},
		})
		_, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "http", Host: "sub.forbidden.test", Port: 80}},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://sub.forbidden.test/feed"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonHostForbidden)
		if refused.Attempts != 0 {
			t.Fatalf("attempts = %d, want 0", refused.Attempts)
		}
		if asked := resolver.lookups("sub.forbidden.test"); asked != 0 {
			t.Fatalf("%d lookups, want none: the digest decides before resolution", asked)
		}
	})

	t.Run("a mutation's redirect is refused after one request", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			http.Redirect(w, r, "/moved", http.StatusSeeOther)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodPost,
			URL:    mustURL(t, srv.URL+"/items"),
			Body:   `{"title":"x"}`,
			Safe:   false,
		}, nil)
		refused := wantError(t, err, ReasonRedirectDenied)
		if served != 1 {
			t.Fatalf("server served %d requests, want exactly one", served)
		}
		if refused.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", refused.Attempts)
		}
	})

	t.Run("a mutation marked safe is still one wire request", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			http.Redirect(w, r, "/moved", http.StatusSeeOther)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		// Safe is the caller's promise that the request may be repeated; a POST is
		// a mutation to its peer whatever the caller marked, so a hop would apply
		// its body a second time at another destination.
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodPost,
			URL:    mustURL(t, srv.URL+"/items"),
			Body:   `{"title":"x"}`,
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonRedirectDenied)
		if served != 1 {
			t.Fatalf("server served %d requests, want exactly one", served)
		}
		if refused.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", refused.Attempts)
		}
	})

	t.Run("a gzip body is decoded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(w)
			_, _ = compressed.Write([]byte("hello"))
			_ = compressed.Close()
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		response, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Body != "hello" {
			t.Fatalf("body = %q, want hello", response.Body)
		}
	})

	t.Run("an unsupported encoding is the ingest owner's refusal", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			fmt.Fprint(w, "hello")
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		var refused *ingest.Error
		if !errors.As(err, &refused) || refused.Reason != "body_encoding_unsupported" {
			t.Fatalf("error = %v, want the ingest body_encoding_unsupported refusal", err)
		}
	})

	t.Run("a body past the decoded cap is the ingest owner's refusal", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			chunk := make([]byte, 1<<20)
			for written := 0; written <= maxBodyBytes; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		var refused *ingest.Error
		if !errors.As(err, &refused) || refused.Reason != "body_too_large" {
			t.Fatalf("error = %v, want the ingest body_too_large refusal", err)
		}
	})

	t.Run("a mutation's unusable 2xx body keeps its status and unknown outcome", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			fmt.Fprint(w, "hello")
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodPost,
			URL:    mustURL(t, srv.URL+"/items"),
			Body:   `{"title":"x"}`,
			Safe:   false,
		}, nil)
		// The peer answered the write, so the 2xx and the unknown-outcome marker
		// reach the caller even though the body could not be used.
		refused := wantError(t, err, ReasonOutcomeUnknown)
		if !refused.Unknown {
			t.Fatalf("unknown = false, want the applied write marked unknown")
		}
		if refused.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", refused.Attempts)
		}
		if refused.Params["status"] != strconv.Itoa(http.StatusOK) {
			t.Fatalf("params = %v, want the 200 status", refused.Params)
		}
		if refused.Params["body"] != "body_encoding_unsupported" {
			t.Fatalf("params = %v, want the ingest reason", refused.Params)
		}
	})

	t.Run("a refusal renders a sentinel as a marker", func(t *testing.T) {
		const sentinel = "bxk_sentinel0123456789"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://"+sentinel+".hop.test/next?token="+sentinel)
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonRedirectDenied)
		rendered := refused.Error()
		if strings.Contains(rendered, sentinel) {
			t.Fatalf("rendered refusal = %q, want no sentinel", rendered)
		}
		if !strings.Contains(rendered, redact.Marker) {
			t.Fatalf("rendered refusal = %q, want a redaction marker", rendered)
		}
	})
}

// TestFinalStatusOutcomes proves the outcome contract of a final non-2xx: a read
// that authenticated nothing is auth_failed, a status a further identical request
// cannot change is rejected, a transient status a safe read repeats ends as the
// wire request budget those repeats spent, and 408/429/5xx is an unknown outcome
// that only a mutation's peer could already have applied.
func TestFinalStatusOutcomes(t *testing.T) {
	for _, outcome := range []struct {
		name    string
		method  string
		safe    bool
		status  int
		reason  string
		unknown bool
		// retried is a transient answer: a safe read sends it again until its wire
		// requests are spent, so the refusal is the call's budget rather than the
		// status the last attempt was answered with.
		retried bool
	}{
		{name: "a read answering 401", method: http.MethodGet, safe: true, status: http.StatusUnauthorized, reason: ReasonAuthFailed},
		{name: "a read answering 400", method: http.MethodGet, safe: true, status: http.StatusBadRequest, reason: ReasonRejected},
		{name: "a read answering 500", method: http.MethodGet, safe: true, status: http.StatusInternalServerError, reason: ReasonBudgetExhausted, retried: true},
		{name: "a read answering 429", method: http.MethodGet, safe: true, status: http.StatusTooManyRequests, reason: ReasonBudgetExhausted, retried: true},
		{name: "a mutation answering 401", method: http.MethodPost, status: http.StatusUnauthorized, reason: ReasonRejected},
		{name: "a mutation answering 400", method: http.MethodPost, status: http.StatusBadRequest, reason: ReasonRejected},
		{name: "a mutation answering 500", method: http.MethodPost, status: http.StatusInternalServerError, reason: ReasonOutcomeUnknown, unknown: true},
		{name: "a mutation answering 429", method: http.MethodPost, status: http.StatusTooManyRequests, reason: ReasonOutcomeUnknown, unknown: true},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			var served int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served++
				w.WriteHeader(outcome.status)
			}))
			defer srv.Close()
			client, _ := newTestClient(loopbackResolver(), nil)

			_, err := client.Do(context.Background(), Op{
				Target: approvedTarget(t, srv),
				Method: outcome.method,
				URL:    mustURL(t, srv.URL+"/items"),
				Body:   bodyFor(outcome.method),
				Safe:   outcome.safe,
			}, nil)
			refused := wantError(t, err, outcome.reason)
			// A status answered at once is one wire request; a transient one is
			// sent again until the call may send no more.
			attempts, answered := 1, 1
			if outcome.retried {
				attempts, answered = maxWireRequests, maxWireRequests
			}
			if refused.Attempts != attempts {
				t.Fatalf("attempts = %d, want %d", refused.Attempts, attempts)
			}
			if refused.Unknown != outcome.unknown {
				t.Fatalf("unknown = %t, want %t", refused.Unknown, outcome.unknown)
			}
			if outcome.retried {
				if refused.Params["limit"] != "wire_requests" || refused.Params["max"] != strconv.Itoa(maxWireRequests) {
					t.Fatalf("params = %v, want the exhausted wire request budget", refused.Params)
				}
			} else if refused.Params["status"] != strconv.Itoa(outcome.status) {
				t.Fatalf("params = %v, want the %d status", refused.Params, outcome.status)
			}
			if served != answered {
				t.Fatalf("server served %d requests, want %d", served, answered)
			}
		})
	}

	t.Run("a request that cannot be built is refused before the wire", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: "GET /items",
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		// Nothing can leave this process for a request net/http refuses to build,
		// so no attempt is counted and no outcome is unknown.
		refused := wantError(t, err, ReasonRejected)
		if refused.Attempts != 0 {
			t.Fatalf("attempts = %d, want 0: no wire request was started", refused.Attempts)
		}
		if refused.Unknown {
			t.Fatalf("unknown = true, want a call that applied nothing")
		}
		if served != 0 {
			t.Fatalf("server served %d requests, want 0", served)
		}
	})
}

// TestStalledBodyReleasesSlot proves one attempt's whole exchange is bounded: a
// peer that answers and then stalls its body ends the call, releases the shared
// permit it held, and is reported by what the peer may have applied, not by an
// ingest refusal blaming the body's encoding.
func TestStalledBodyReleasesSlot(t *testing.T) {
	bounded := attemptTimeout
	attemptTimeout = 100 * time.Millisecond
	t.Cleanup(func() { attemptTimeout = bounded })

	release := make(chan struct{})
	stalling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		fmt.Fprint(w, "half")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer stalling.Close()
	defer close(release)

	for _, stalled := range []struct {
		name    string
		method  string
		safe    bool
		body    string
		unknown bool
	}{
		{name: "a stalled read", method: http.MethodGet, safe: true},
		{name: "a stalled mutation", method: http.MethodPost, body: `{"title":"x"}`, unknown: true},
	} {
		t.Run(stalled.name, func(t *testing.T) {
			client, slot := newTestClient(loopbackResolver(), nil)
			started := time.Now()
			_, err := client.Do(context.Background(), Op{
				Target: approvedTarget(t, stalling),
				Method: stalled.method,
				URL:    mustURL(t, stalling.URL+"/items"),
				Body:   stalled.body,
				Safe:   stalled.safe,
			}, nil)
			refused := wantError(t, err, ReasonOutcomeUnknown)
			if refused.Unknown != stalled.unknown {
				t.Fatalf("unknown = %t, want %t", refused.Unknown, stalled.unknown)
			}
			if refused.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", refused.Attempts)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("call took %s, want the attempt bound to end it", elapsed)
			}
			if taken, released := slot.counts(); taken != 1 || released != 1 {
				t.Fatalf("slot taken/released = %d/%d, want the stalled exchange to release its permit", taken, released)
			}
		})
	}
}

// bodyFor is the body a method is exercised with, so a mutation carries one and a
// read does not.
func bodyFor(method string) string {
	if isMutation(method) {
		return `{"title":"x"}`
	}
	return ""
}

// TestCredentialsStayOnApprovedOrigin proves AC-031's credential scope: the secret
// reaches only the exact origin that owns it, in the header and the query that
// credential names, and a request bound for another origin is formed without it.
func TestCredentialsStayOnApprovedOrigin(t *testing.T) {
	const secret = "bxk_credential0123456789"
	newCredential := func() *Credential {
		return &Credential{
			Header: "Authorization",
			Query:  "apikey",
			Value:  func(context.Context) ([]byte, error) { return []byte(secret), nil },
		}
	}

	t.Run("the approved origin receives the header and the query", func(t *testing.T) {
		var authorization, apikey, page string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = r.Header.Get("Authorization")
			apikey = r.URL.Query().Get("apikey")
			page = r.URL.Query().Get("page")
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		target := approvedTarget(t, srv)
		target.Credential = newCredential()
		if _, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items?page=2"),
			Safe:   true,
		}, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if authorization != secret {
			t.Fatalf("authorization = %q, want the credential", authorization)
		}
		if apikey != secret {
			t.Fatalf("apikey = %q, want the query credential", apikey)
		}
		if page != "2" {
			t.Fatalf("page = %q, want the caller's own query kept", page)
		}
	})

	t.Run("a hop to another origin is formed without the credential", func(t *testing.T) {
		var served int
		var authorization string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			authorization = r.Header.Get("Authorization")
			w.Header().Set("Location", "https://other.test/next")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		recorder := &recorder{allow: 1}
		client, _ := newTestClient(newResolver(map[string][]netip.Addr{
			"127.0.0.1":  addrs("127.0.0.1"),
			"other.test": addrs("192.0.2.10"),
		}), recorder.wrap())
		target := approvedTarget(t, srv)
		target.Credential = newCredential()
		if _, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil); err == nil {
			t.Fatal("Do succeeded, want the recorded hop not to be forwarded")
		}
		if served != 1 || authorization != secret {
			t.Fatalf("approved origin served %d requests with %q, want one with the credential", served, authorization)
		}
		requests := recorder.requests()
		if len(requests) != 2 {
			t.Fatalf("recorded %d requests, want the request and its hop", len(requests))
		}
		hop := requests[1]
		if header := hop.header.Get("Authorization"); header != "" {
			t.Fatalf("hop authorization = %q, want none", header)
		}
		if strings.Contains(hop.url, secret) || strings.Contains(hop.url, "apikey") {
			t.Fatalf("hop url = %q, want no credential", hop.url)
		}
	})

	t.Run("a cookie credential stays on the approved origin", func(t *testing.T) {
		var served int
		var cookie string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			cookie = r.Header.Get("Cookie")
			w.Header().Set("Location", "https://other.test/next")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		recorder := &recorder{allow: 1}
		client, _ := newTestClient(newResolver(map[string][]netip.Addr{
			"127.0.0.1":  addrs("127.0.0.1"),
			"other.test": addrs("192.0.2.10"),
		}), recorder.wrap())
		target := approvedTarget(t, srv)
		// A peer may carry its credential in a cookie; the request itself carries
		// none otherwise, so the approved origin having one is what makes the hop's
		// absence discriminating.
		target.Credential = &Credential{
			Header: "Cookie",
			Value:  func(context.Context) ([]byte, error) { return []byte("session=" + secret), nil },
		}
		if _, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil); err == nil {
			t.Fatal("Do succeeded, want the recorded hop not to be forwarded")
		}
		if served != 1 || cookie != "session="+secret {
			t.Fatalf("approved origin served %d requests with cookie %q, want one with the credential", served, cookie)
		}
		requests := recorder.requests()
		if len(requests) != 2 {
			t.Fatalf("recorded %d requests, want the request and its hop", len(requests))
		}
		if header := requests[1].header.Get("Cookie"); header != "" {
			t.Fatalf("hop cookie = %q, want none", header)
		}
	})

	t.Run("a request whose origin is not the target's carries nothing", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
		defer srv.Close()
		server := mustURL(t, srv.URL)
		port, err := strconv.Atoi(server.Port())
		if err != nil {
			t.Fatalf("server port: %v", err)
		}
		recorder := &recorder{allow: 0}
		client, _ := newTestClient(loopbackResolver(), recorder.wrap())
		target := &Target{
			Origin:     Origin{Scheme: server.Scheme, Host: server.Hostname(), Port: port + 1},
			Private:    true,
			Credential: newCredential(),
		}
		if _, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil); err == nil {
			t.Fatal("Do succeeded, want the recorded request not to be forwarded")
		}
		requests := recorder.requests()
		if len(requests) != 1 {
			t.Fatalf("recorded %d requests, want 1", len(requests))
		}
		if header := requests[0].header.Get("Authorization"); header != "" {
			t.Fatalf("authorization = %q, want none for another origin", header)
		}
		if strings.Contains(requests[0].url, secret) || strings.Contains(requests[0].url, "apikey") {
			t.Fatalf("url = %q, want no credential", requests[0].url)
		}
		if served != 0 {
			t.Fatalf("server served %d requests, want 0", served)
		}
	})

	t.Run("userinfo in the request URL is never sent", func(t *testing.T) {
		var authorization string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = r.Header.Get("Authorization")
		}))
		defer srv.Close()
		port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
		if err != nil {
			t.Fatalf("server port: %v", err)
		}
		client, _ := newTestClient(loopbackResolver(), nil)
		if _, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "http", Host: "127.0.0.1", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://"+secret+":"+secret+"@127.0.0.1:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if authorization != "" {
			t.Fatalf("authorization = %q, want none: URL userinfo is not a scoped credential", authorization)
		}
	})

	t.Run("a trailing root dot names the same origin", func(t *testing.T) {
		var authorization string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = r.Header.Get("Authorization")
		}))
		defer srv.Close()
		port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
		if err != nil {
			t.Fatalf("server port: %v", err)
		}
		client, _ := newTestClient(loopbackResolver(), nil)
		target := &Target{
			Origin:     Origin{Scheme: "http", Host: "127.0.0.1", Port: port},
			Private:    true,
			Credential: newCredential(),
		}
		if _, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, "http://127.0.0.1.:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if authorization != secret {
			t.Fatalf("authorization = %q, want the credential: the root dot names the approved origin", authorization)
		}
	})

	t.Run("a homograph of the approved host is another origin", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
		defer srv.Close()
		port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
		if err != nil {
			t.Fatalf("server port: %v", err)
		}
		client, _ := newTestClient(loopbackResolver(), nil)
		target := &Target{
			Origin:     Origin{Scheme: "http", Host: "127.0.0.1", Port: port},
			Private:    true,
			Credential: newCredential(),
		}
		_, err = client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, "http://127.0.0.1\u3002:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonDialDenied)
		if served != 0 {
			t.Fatalf("the approved origin served %d requests, want 0: a homograph is not approved", served)
		}
	})

	t.Run("a credential that cannot be produced is refused without its text", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
		defer srv.Close()
		client, slot := newTestClient(loopbackResolver(), nil)
		// The reader's own text is not a token the redactor masks, so its absence
		// proves the refusal never renders it rather than that it was masked late.
		const readerText = "qa-Zx9-reader-probe"
		target := approvedTarget(t, srv)
		target.Credential = &Credential{
			Header: "Authorization",
			Value:  func(context.Context) ([]byte, error) { return nil, errors.New("reader failed for " + readerText) },
		}
		_, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonAuthFailed)
		if strings.Contains(refused.Error(), readerText) {
			t.Fatalf("rendered refusal = %q, want no reader text", refused.Error())
		}
		if refused.Params["credential"] != "unavailable" {
			t.Fatalf("params = %v, want the fixed unavailable credential", refused.Params)
		}
		if served != 0 {
			t.Fatalf("server served %d requests, want 0", served)
		}
		if taken, released := slot.counts(); taken != 1 || released != 1 {
			t.Fatalf("slot taken/released = %d/%d, want the refused send to release its permit", taken, released)
		}
	})
}

// wantError requires err to be the client's refusal with reason.
func wantError(t *testing.T, err error, reason string) *Error {
	t.Helper()
	var refused *Error
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want *outbound.Error with reason %s", err, reason)
	}
	if refused.Reason != reason {
		t.Fatalf("reason = %s, want %s (%v)", refused.Reason, reason, err)
	}
	return refused
}

// addrs parses one resolver answer.
func addrs(written ...string) []netip.Addr {
	parsed := make([]netip.Addr, 0, len(written))
	for _, text := range written {
		parsed = append(parsed, netip.MustParseAddr(text))
	}
	return parsed
}

// tableResolver is the injected Resolver: a name maps to a fixed answer, a test can
// change that answer between lookups to model a DNS flip, and every question is
// recorded so a refusal before resolution is provable.
type tableResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	asked   []string
}

func newResolver(answers map[string][]netip.Addr) *tableResolver {
	return &tableResolver{answers: answers}
}

// loopbackResolver answers the loopback name an httptest server is reached by.
func loopbackResolver() *tableResolver {
	return newResolver(map[string][]netip.Addr{"127.0.0.1": addrs("127.0.0.1")})
}

func (r *tableResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, fmt.Errorf("lookup network = %q, want ip", network)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, host)
	answer, found := r.answers[host]
	if !found {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return answer, nil
}

// answer changes one name's answer.
func (r *tableResolver) answer(host string, written ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[host] = addrs(written...)
}

// lookups counts the questions asked about host.
func (r *tableResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, asked := range r.asked {
		if asked == host {
			count++
		}
	}
	return count
}

// testSlot is the counted Slot these tests pass: a fixed capacity with no queue, so
// a send either takes its one permit or is refused, and the counts show one permit
// per physical send.
type testSlot struct {
	mu       sync.Mutex
	free     int
	taken    int
	released int
}

func newSlot(capacity int) *testSlot { return &testSlot{free: capacity} }

func (s *testSlot) take(context.Context) (func(), error) {
	s.mu.Lock()
	if s.free == 0 {
		s.mu.Unlock()
		return nil, errors.New("no free slot")
	}
	s.free--
	s.taken++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.free++
			s.released++
		})
	}, nil
}

func (s *testSlot) counts() (taken, released int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taken, s.released
}

// newTestClient is the client under test: it resolves through the injected table,
// takes one permit per send from a slot with room, and lets a test wrap the
// transport.
func newTestClient(resolver Resolver, wrap func(http.RoundTripper) http.RoundTripper) (*Client, *testSlot) {
	slot := newSlot(4)
	return New(Config{Resolver: resolver, Slot: slot.take, Version: "1.2.3", Transport: wrap}), slot
}

func mustURL(t *testing.T, written string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(written)
	if err != nil {
		t.Fatalf("parse %q: %v", written, err)
	}
	return parsed
}

// approvedTarget is the exact origin of srv, approved for private addresses because
// these tests dial loopback.
func approvedTarget(t *testing.T, srv *httptest.Server) *Target {
	t.Helper()
	server := mustURL(t, srv.URL)
	port, err := strconv.Atoi(server.Port())
	if err != nil {
		t.Fatalf("server port in %q: %v", srv.URL, err)
	}
	return &Target{
		ID:      "approved",
		Origin:  Origin{Scheme: server.Scheme, Host: server.Hostname(), Port: port},
		Private: true,
	}
}

// trustServer is the Do-level TLS seam: it installs that server's own certificate
// as the only root so a call's own behavior is observed without a handshake
// failing on an unknown authority. It is never the proof of verification itself:
// TestTransportVerifiesCertificate exercises newTransport with no wrap at all.
func trustServer(srv *httptest.Server) func(http.RoundTripper) http.RoundTripper {
	return func(transport http.RoundTripper) http.RoundTripper {
		built, ok := transport.(*http.Transport)
		if !ok {
			return transport
		}
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		built.TLSClientConfig = &tls.Config{RootCAs: pool}
		return built
	}
}

// sentRequest is one request the transport was given.
type sentRequest struct {
	url    string
	header http.Header
}

// errRecordedOnly stops a request after it was recorded, so a hop can be observed
// without dialing it.
var errRecordedOnly = errors.New("recorder: recorded and not forwarded")

// recorder is the transport wrap seam: it records the request the transport is about
// to send and forwards it while allow is not used up. A later request is recorded and
// then refused.
type recorder struct {
	allow int
	inner http.RoundTripper
	mu    sync.Mutex
	sent  []sentRequest
}

// wrap installs the recorder on the transport the client built.
func (r *recorder) wrap() func(http.RoundTripper) http.RoundTripper {
	return func(transport http.RoundTripper) http.RoundTripper {
		r.inner = transport
		return r
	}
}

func (r *recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent = append(r.sent, sentRequest{url: request.URL.String(), header: request.Header.Clone()})
	forward := len(r.sent) <= r.allow
	r.mu.Unlock()
	if !forward {
		return nil, errRecordedOnly
	}
	return r.inner.RoundTrip(request)
}

// requests is what the transport was given so far.
func (r *recorder) requests() []sentRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}
