package fixture_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/fixture"
	"github.com/ViktorsBaikers/Boxset/internal/fixture/synthetic"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
)

// resolverFunc is the client's resolver, so a test can answer every name itself.
type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

// loopback answers every name with the loopback address: the throwaway servers of
// these tests listen there.
func loopback(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// transportFunc is one http.RoundTripper written inline.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// newClient is the real outbound client over the given transport wrapper.
func newClient(wrap func(http.RoundTripper) http.RoundTripper) *outbound.Client {
	return outbound.New(outbound.Config{
		Resolver:  resolverFunc(loopback),
		Slot:      func(context.Context) (func(), error) { return func() {}, nil },
		Version:   "1.2.3",
		Transport: wrap,
	})
}

// readOp is one safe read of an approved private target.
func readOp(t *testing.T, target *outbound.Target, address string) outbound.Op {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatalf("parse %q: %v", address, err)
	}
	return outbound.Op{Name: "read", Target: target, Method: http.MethodGet, URL: parsed, Safe: true}
}

// targetOf is the approved origin of a loopback server.
func targetOf(t *testing.T, server *httptest.Server) *outbound.Target {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", server.URL, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", server.URL, err)
	}
	return &outbound.Target{ID: "live", Origin: outbound.Origin{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port}, Private: true}
}

// committedPath is the set the repository ships, named through the one write root
// the recorder publishes under, so a test reads the file the harness writes.
func committedPath() string {
	return filepath.Join("..", "..", fixture.Dir(synthetic.Set), fixture.FileName)
}

// committedSet reads the set the repository ships.
func committedSet(t *testing.T) fixture.Document {
	t.Helper()
	path := committedPath()
	doc, err := fixture.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return doc
}

// start serves handler on a loopback listener and registers its origin with the
// recorder: the recorder holds the socket, which is the one way an origin becomes
// writable in a recording.
func start(t *testing.T, recorder *fixture.Recorder, handler http.Handler) fixture.Target {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	served := server.Listener.Addr().(*net.TCPAddr)
	target := fixture.Target{Scheme: "http", Host: served.IP.String(), Port: served.Port}
	service, err := recorder.Serve(target, server.Listener, handler)
	if err != nil {
		t.Fatalf("serve %s: %v", target.Scheme+"://"+target.Host, err)
	}
	t.Cleanup(service.Close)
	return target
}

// address is one request URL against a recording's own service.
func address(target fixture.Target, path string) string {
	return "http://" + net.JoinHostPort(target.Host, strconv.Itoa(target.Port)) + path
}

// sentinels reports the secrets a saved set may not hold and the placeholders it
// must hold instead.
func sentinels() []fixture.Secret { return synthetic.Secrets() }

// TestFixtureRecordSafety proves AC-040 over a real recording: the set the
// repository ships is the one this code produces, it is scrubbed before it is saved,
// every failure is recorded in the class the transport reported, and a target the
// recorder did not start never receives a request.
func TestFixtureRecordSafety(t *testing.T) {
	ctx := context.Background()

	t.Run("the committed set is the recording this repository produces", func(t *testing.T) {
		fresh, err := synthetic.Record(ctx, synthetic.Set)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		encoded, err := fixture.Encode(fresh)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		saved, err := os.ReadFile(committedPath())
		if err != nil {
			t.Fatalf("read %s: %v", committedPath(), err)
		}
		if !bytes.Equal(encoded, saved) {
			t.Fatalf("the committed set is not this recording:\n%s", encoded)
		}
		want := fixture.Provenance{Source: fixture.SourceSynthetic, Redistribution: fixture.RedistributionRepository}
		if fresh.Set != synthetic.Set || fresh.Provenance != want {
			t.Fatalf("set %q provenance %+v, want %q/%+v", fresh.Set, fresh.Provenance, synthetic.Set, want)
		}
		for _, secret := range sentinels() {
			if held(fresh, secret.Value) || bytes.Contains(saved, []byte(secret.Value)) {
				t.Fatalf("the saved set still holds the %s sentinel", secret.Placeholder)
			}
			if !held(fresh, secret.Placeholder) {
				t.Fatalf("nothing was scrubbed into %s", secret.Placeholder)
			}
		}
		// Every class the replay has to serve is recorded: answers, a refused read, a
		// rate limit, a cut answer, a reset after the write and a timeout after it.
		statuses, faults := classes(fresh)
		if len(statuses) != 3 || statuses[http.StatusOK] != 9 || statuses[http.StatusUnauthorized] != 1 || statuses[http.StatusTooManyRequests] != 1 {
			t.Fatalf("recorded statuses %v", statuses)
		}
		for _, fault := range []string{fixture.FaultPartial, fixture.FaultResetAfterWrite, fixture.FaultTimeoutBeforeResponse} {
			if faults[fault] != 1 {
				t.Fatalf("recorded faults %v, want one %s", faults, fault)
			}
		}
		if len(faults) != 3 {
			t.Fatalf("recorded faults %v, want three classes", faults)
		}
	})

	t.Run("a failure is written down as the class the transport reported", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			fail error
			want string
		}{
			{"a peer that never answered", &url.Error{Op: "Get", URL: "http://" + synthetic.Host + "/read", Err: context.DeadlineExceeded}, fixture.FaultTimeoutBeforeResponse},
			{"a peer that reset the connection", &url.Error{Op: "Get", URL: "http://" + synthetic.Host + "/read", Err: syscall.ECONNRESET}, fixture.FaultResetAfterWrite},
			{"a read the peer reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, fixture.FaultResetAfterWrite},
			{"a peer that closed without an answer", io.ErrUnexpectedEOF, fixture.FaultResetAfterWrite},
		} {
			recorder := fixture.NewRecorder(nil)
			target := start(t, recorder, http.NotFoundHandler())
			transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.fail
			}))
			request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
			if err != nil {
				t.Fatalf("%s: request: %v", tc.name, err)
			}
			if _, err := transport.RoundTrip(request); err == nil {
				t.Fatalf("%s: the failure was swallowed", tc.name)
			}
			doc, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic)
			if err != nil {
				t.Fatalf("%s: document: %v", tc.name, err)
			}
			if len(doc.Exchanges) != 1 || doc.Exchanges[0].Fault != tc.want {
				t.Fatalf("%s: recorded %+v, want the %s fault", tc.name, doc.Exchanges, tc.want)
			}
		}
		// A failure with no reset signature says nothing about a peer that read the
		// request, so it refuses the recording instead of writing a class down.
		for _, tc := range []struct {
			name string
			fail error
		}{
			{"a TLS handshake failure", &url.Error{Op: "Get", URL: "https://" + synthetic.Host + "/read", Err: errors.New("tls: handshake failure")}},
			{"a plain HTTP answer to an HTTPS request", &url.Error{Op: "Get", URL: "https://" + synthetic.Host + "/read", Err: errors.New("http: server gave HTTP response to HTTPS client")}},
			{"a cancelled request", &url.Error{Op: "Get", URL: "http://" + synthetic.Host + "/read", Err: context.Canceled}},
		} {
			recorder := fixture.NewRecorder(nil)
			target := start(t, recorder, http.NotFoundHandler())
			transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.fail
			}))
			request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
			if err != nil {
				t.Fatalf("%s: request: %v", tc.name, err)
			}
			if _, err := transport.RoundTrip(request); err == nil {
				t.Fatalf("%s: the failure was swallowed", tc.name)
			}
			if _, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic); fixture.Token(err) != fixture.ErrRecordingIncomplete.Error() {
				t.Fatalf("%s recorded %q, want %q", tc.name, fixture.Token(err), fixture.ErrRecordingIncomplete)
			}
			if recorder.Len() != 0 {
				t.Fatalf("%s wrote %d exchanges down", tc.name, recorder.Len())
			}
		}
		// A dial that wrote nothing is no wire exchange at all, so it refuses the
		// recording instead of describing a peer that never saw the request.
		recorder := fixture.NewRecorder(nil)
		target := start(t, recorder, http.NotFoundHandler())
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}))
		request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("the dial failure was swallowed")
		}
		if _, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic); fixture.Token(err) != fixture.ErrRecordingIncomplete.Error() {
			t.Fatalf("a dial failure recorded %q, want %q", fixture.Token(err), fixture.ErrRecordingIncomplete)
		}
		if recorder.Len() != 0 {
			t.Fatalf("a dial failure wrote %d exchanges down", recorder.Len())
		}
	})

	t.Run("an origin the recorder does not serve is not writable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		served := listener.Addr().(*net.TCPAddr)
		elsewhere := served.Port - 1
		if elsewhere < 1 {
			elsewhere = served.Port + 1
		}
		// Each target names an origin this recorder cannot prove it holds: a host it
		// never bound, a port the listener does not answer on, and a TLS origin over
		// the plain HTTP it serves.
		for _, tc := range []struct {
			name   string
			target fixture.Target
		}{
			{"a host the listener does not answer on", fixture.Target{Scheme: "http", Host: "elsewhere.test", Port: served.Port}},
			{"a port the listener does not answer on", fixture.Target{Scheme: "http", Host: served.IP.String(), Port: elsewhere}},
			{"a scheme the recorder does not serve", fixture.Target{Scheme: "https", Host: served.IP.String(), Port: served.Port}},
		} {
			inner := 0
			recorder := fixture.NewRecorder(nil)
			if _, err := recorder.Serve(tc.target, listener, http.NotFoundHandler()); fixture.Token(err) != fixture.ErrRecordingIncomplete.Error() {
				t.Fatalf("%s was registered: %v", tc.name, err)
			}
			transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
				inner++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("never"))}, nil
			}))
			request, err := http.NewRequest(http.MethodPost, address(tc.target, "/write"), nil)
			if err != nil {
				t.Fatalf("%s: request: %v", tc.name, err)
			}
			if _, err := transport.RoundTrip(request); err == nil {
				t.Fatalf("%s was writable", tc.name)
			}
			if reason := fixture.Token(recorder.Refused()); reason != fixture.ErrTargetUnclassified.Error() {
				t.Fatalf("%s refusal = %q, want %q", tc.name, reason, fixture.ErrTargetUnclassified)
			}
			if inner != 0 || recorder.Len() != 0 {
				t.Fatalf("%s reached the transport %d times and recorded %d exchanges", tc.name, inner, recorder.Len())
			}
		}
	})

	t.Run("a wildcard socket names no origin", func(t *testing.T) {
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		wildcard := listener.Addr().(*net.TCPAddr)
		target := fixture.Target{Scheme: "http", Host: wildcard.IP.String(), Port: wildcard.Port}
		recorder := fixture.NewRecorder(nil)
		if _, err := recorder.Serve(target, listener, http.NotFoundHandler()); fixture.Token(err) != fixture.ErrRecordingIncomplete.Error() {
			t.Fatalf("a wildcard socket named %q was registered: %v", target.Host, err)
		}
		if recorder.Len() != 0 {
			t.Fatalf("a wildcard socket recorded %d exchanges", recorder.Len())
		}
	})

	t.Run("an IPv6 socket is its own origin", func(t *testing.T) {
		listener, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("this host has no IPv6 loopback: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		served := listener.Addr().(*net.TCPAddr)
		target := fixture.Target{Scheme: "http", Host: served.IP.String(), Port: served.Port}
		recorder := fixture.NewRecorder(nil)
		service, err := recorder.Serve(target, listener, http.NotFoundHandler())
		if err != nil {
			t.Fatalf("serve %s: %v", target.Host, err)
		}
		t.Cleanup(service.Close)
		inner := 0
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			inner++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}))
		request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatalf("an IPv6 origin was refused: %v", err)
		}
		if inner != 1 {
			t.Fatalf("the IPv6 origin reached the transport %d times, want 1", inner)
		}
	})

	t.Run("an origin without a written port is the origin with its default one", func(t *testing.T) {
		recorder := fixture.NewRecorder(nil)
		recorder.Watch(fixture.Target{Scheme: "http", Host: "www.example.test", Port: 80})
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}))
		request, err := http.NewRequest(http.MethodGet, "http://www.example.test/read", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatalf("a default-port target was refused: %v", err)
		}
		// The default port classifies the origin, it does not make it writable.
		write, err := http.NewRequest(http.MethodPost, "http://www.example.test/read", nil)
		if err != nil {
			t.Fatalf("new write: %v", err)
		}
		if _, err := transport.RoundTrip(write); err == nil {
			t.Fatal("a write to a default-port live target was not refused")
		}
		if reason := fixture.Token(recorder.Refused()); reason != fixture.ErrLiveMethodDenied.Error() {
			t.Fatalf("write refusal = %q, want %q", reason, fixture.ErrLiveMethodDenied)
		}
	})

	t.Run("a body of arbitrary bytes survives the file and the replay", func(t *testing.T) {
		body := []byte{0xff, 0xfe, '{', '}'}
		if utf8.Valid(body) {
			t.Fatalf("%q is text, so the case proves nothing", body)
		}
		recorder := fixture.NewRecorder(nil)
		target := start(t, recorder, http.NotFoundHandler())
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
		}))
		request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatalf("recorded exchange: %v", err)
		}
		doc, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic)
		if err != nil {
			t.Fatalf("document: %v", err)
		}
		data, err := fixture.Encode(doc)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		saved, err := fixture.Decode(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !bytes.Equal(saved.Exchanges[0].Response.Body, body) {
			t.Fatalf("the saved body is %q, want the peer's bytes %q", saved.Exchanges[0].Response.Body, body)
		}
		response, err := fixture.NewReplayer(saved).Transport(nil).RoundTrip(request)
		if err != nil {
			t.Fatalf("replayed exchange: %v", err)
		}
		served, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read replayed body: %v", err)
		}
		if !bytes.Equal(served, body) {
			t.Fatalf("the replay served %q, want %q", served, body)
		}
	})

	t.Run("an encoded answer is refused before its body is read", func(t *testing.T) {
		recorder := fixture.NewRecorder(nil)
		target := start(t, recorder, http.NotFoundHandler())
		body := &unreadBody{}
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Encoding": {"br"}},
				Body:       body,
			}, nil
		}))
		request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("a compressed answer was recorded")
		}
		// The scrub reads a saved body as text, so the bytes are never inspected:
		// they are not read at all, and the answer is not the peer's.
		if body.read {
			t.Fatal("the compressed body was read")
		}
		if !body.closed {
			t.Fatal("the compressed answer was left open")
		}
		if recorder.Len() != 0 {
			t.Fatalf("a compressed answer wrote %d exchanges", recorder.Len())
		}
		if _, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic); fixture.Token(err) != fixture.ErrRecordingIncomplete.Error() {
			t.Fatalf("a compressed answer recorded %q, want %q", fixture.Token(err), fixture.ErrRecordingIncomplete)
		}
	})

	t.Run("an unclassified target is refused before any request", func(t *testing.T) {
		var inner int
		recorder := fixture.NewRecorder(sentinels())
		start(t, recorder, http.NotFoundHandler())
		transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			inner++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("never"))}, nil
		}))
		request, err := http.NewRequest(http.MethodGet, "http://unclassified.test:8080/read", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("an unclassified target was not refused")
		}
		// A write is refused the same way: an origin nobody started is not throwaway
		// because a caller said nothing about it.
		write, err := http.NewRequest(http.MethodPost, "http://unclassified.test:8080/write", nil)
		if err != nil {
			t.Fatalf("new write: %v", err)
		}
		if _, err := transport.RoundTrip(write); err == nil {
			t.Fatal("a write to an unclassified target was not refused")
		}
		if inner != 0 {
			t.Fatalf("the unclassified target received %d requests, want 0", inner)
		}
		if reason := fixture.Token(recorder.Refused()); reason != fixture.ErrTargetUnclassified.Error() {
			t.Fatalf("refusal = %q, want %q", reason, fixture.ErrTargetUnclassified)
		}
		refusesWithoutValues(t, recorder.Refused())
	})

	t.Run("a set without its own provenance is refused", func(t *testing.T) {
		recorder := fixture.NewRecorder(nil)
		for _, tc := range []struct{ set, provenance, want string }{
			{synthetic.Set, "", fixture.ErrProvenanceRequired.Error()},
			{synthetic.Set, fixture.SourceLiveGet, fixture.ErrProvenanceRequired.Error()},
			{"Outbound-Contract", fixture.SourceSynthetic, fixture.ErrSetInvalid.Error()},
			{"", fixture.SourceSynthetic, fixture.ErrSetInvalid.Error()},
		} {
			_, err := recorder.Document(tc.set, tc.provenance)
			if reason := fixture.Token(err); reason != tc.want {
				t.Fatalf("Document(%q, %q) refusal = %q, want %q", tc.set, tc.provenance, reason, tc.want)
			}
			refusesWithoutValues(t, err)
		}
		if _, err := synthetic.Record(ctx, "no-such-set"); fixture.Token(err) != fixture.ErrSetUnknown.Error() {
			t.Fatalf("an unknown set refusal = %q, want %q", fixture.Token(err), fixture.ErrSetUnknown)
		}
	})

	t.Run("publication writes the encoded document and nothing else", func(t *testing.T) {
		dir := t.TempDir()
		doc := committedSet(t)
		if err := fixture.Publish(dir, doc); err != nil {
			t.Fatalf("publish: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, fixture.FileName))
		if err != nil {
			t.Fatalf("read published set: %v", err)
		}
		encoded, err := fixture.Encode(doc)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(data, encoded) {
			t.Fatal("the published file is not the encoded document")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != fixture.FileName {
			t.Fatalf("publication left %d entries behind", len(entries))
		}
		// A document that fails its own rules writes nothing at all.
		refused := t.TempDir()
		if err := fixture.Publish(refused, fixture.Document{}); err == nil {
			t.Fatal("an empty document was published")
		}
		if entries, err := os.ReadDir(refused); err != nil || len(entries) != 0 {
			t.Fatalf("a refused publication left %d entries", len(entries))
		}
	})
}

// TestLiveTargetGetOnly proves a live service receives GET and nothing else: every
// other method is refused before the request leaves, and the target sees no
// request at all.
func TestLiveTargetGetOnly(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		_, _ = io.WriteString(w, "live")
	}))
	defer server.Close()
	target := targetOf(t, server)
	recorder := fixture.NewRecorder(nil)
	recorder.Watch(fixture.Target{Scheme: target.Origin.Scheme, Host: target.Origin.Host, Port: target.Origin.Port})
	client := newClient(recorder.Transport)
	defer func() { _ = client.Close(ctx) }()

	response, err := client.Do(ctx, readOp(t, target, server.URL+"/read"), nil)
	if err != nil {
		t.Fatalf("live GET: %v", err)
	}
	if response.Status != http.StatusOK || response.Body != "live" {
		t.Fatalf("live GET answered %d %q, want 200 live", response.Status, response.Body)
	}
	// The capture is the owner's, and the recording proves it: the same bytes may not
	// claim the repository's synthetic source, which is the only one it redistributes.
	if _, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic); fixture.Token(err) != fixture.ErrProvenanceRequired.Error() {
		t.Fatalf("a live capture declared synthetic gave %q, want %q", fixture.Token(err), fixture.ErrProvenanceRequired)
	}
	captured, err := recorder.Document(synthetic.Set, fixture.SourceLiveGet)
	if err != nil {
		t.Fatalf("live capture: %v", err)
	}
	disposition := fixture.Provenance{Source: fixture.SourceLiveGet, Redistribution: fixture.RedistributionOwnerOnly}
	if captured.Provenance != disposition {
		t.Fatalf("provenance %+v, want %+v", captured.Provenance, disposition)
	}
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		op := readOp(t, target, server.URL+"/write")
		op.Method = method
		if _, err := client.Do(ctx, op, nil); err == nil {
			t.Fatalf("%s to a live target was not refused", method)
		}
		if reason := fixture.Token(recorder.Refused()); reason != fixture.ErrLiveMethodDenied.Error() {
			t.Fatalf("%s refusal = %q, want %q", method, reason, fixture.ErrLiveMethodDenied)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("the live target received %v, want one GET", methods)
	}
}

// TestScrubIncompleteRefusesAndKeepsPrior proves a value the scrub leaves behind
// refuses the recording, publishes nothing, keeps the file that already described
// the set, and is never printed.
func TestScrubIncompleteRefusesAndKeepsPrior(t *testing.T) {
	dir := t.TempDir()
	prior := filepath.Join(dir, fixture.FileName)
	if err := os.WriteFile(prior, []byte("committed\n"), 0o644); err != nil {
		t.Fatalf("write prior: %v", err)
	}
	secret := fixture.Secret{Value: "SYNTHETIC-PLANTED-3a9c", Placeholder: "<planted>"}
	recorder := fixture.NewRecorder([]fixture.Secret{secret})
	target := start(t, recorder, http.NotFoundHandler())
	// The value hides in a field the scrub rules do not rewrite: a header name.
	transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{secret.Value: {"leaked"}},
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	}))
	request, err := http.NewRequest(http.MethodGet, address(target, "/read"), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("recorded exchange: %v", err)
	}
	doc, refused := recorder.Document(synthetic.Set, fixture.SourceSynthetic)
	if refused == nil {
		t.Fatalf("a surviving sentinel published %d exchanges", len(doc.Exchanges))
	}
	if reason := fixture.Token(refused); reason != fixture.ErrScrubIncomplete.Error() {
		t.Fatalf("refusal = %q, want %q", reason, fixture.ErrScrubIncomplete)
	}
	// Nothing was written: the recorder returns before Publish, and the bytes that
	// already described the set are untouched.
	kept, err := os.ReadFile(prior)
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	if string(kept) != "committed\n" {
		t.Fatalf("the committed file reads %q, want it untouched", kept)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != fixture.FileName {
		t.Fatalf("the refused recording left %d entries behind", len(entries))
	}
	// The printed report is the reason word and nothing else: the word the recorder
	// reports is the one its command prints, and no value of the set is in it.
	if !strings.HasPrefix(refused.Error(), fixture.ErrScrubIncomplete.Error()) || strings.Contains(refused.Error(), secret.Value) {
		t.Fatalf("the refusal printed %q, want only the %q reason", refused, fixture.ErrScrubIncomplete)
	}
}

// TestScrubRewritesEverySpelling proves the scrub covers a value that rides in the
// request URL — the field a credential reaches a peer in — both as written and
// percent-encoded, and that the re-read refuses either form when it survives.
func TestScrubRewritesEverySpelling(t *testing.T) {
	secret := fixture.Secret{Value: "SYNTHETIC/QUERY+8b1d", Placeholder: "<planted>"}
	encoded := url.QueryEscape(secret.Value)
	if encoded == secret.Value {
		t.Fatalf("%q needs no encoding, so the case proves nothing", secret.Value)
	}
	recorder := fixture.NewRecorder([]fixture.Secret{secret})
	target := start(t, recorder, http.NotFoundHandler())
	transport := recorder.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	// Both spellings travel in the URL: a query value is where a credential rides.
	request, err := http.NewRequest(http.MethodGet, address(target, "/read")+"?key="+secret.Value+"&alt="+encoded, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("recorded exchange: %v", err)
	}
	doc, err := recorder.Document(synthetic.Set, fixture.SourceSynthetic)
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	recorded := doc.Exchanges[0].Request.URL
	if strings.Contains(recorded, secret.Value) || strings.Contains(recorded, encoded) {
		t.Fatalf("the recorded URL %q still holds the value", recorded)
	}
	if strings.Count(recorded, secret.Placeholder) != 2 {
		t.Fatalf("the recorded URL %q, want the placeholder in both spellings", recorded)
	}
	// The re-read is what catches a spelling the rules do not reach: here the value
	// survives percent-encoded in a field Scrub never rewrites, a header name, while
	// the request URL holds neither spelling.
	survivor := fixture.Document{
		Schema:     fixture.Schema,
		Set:        synthetic.Set,
		Provenance: fixture.Provenance{Source: fixture.SourceSynthetic, Redistribution: fixture.RedistributionRepository},
		Exchanges: []fixture.Exchange{{
			Request:  fixture.Request{Method: http.MethodGet, URL: address(target, "/read")},
			Response: &fixture.Response{Status: http.StatusOK, Header: map[string][]string{encoded: {"leaked"}}, Body: []byte("ok")},
		}},
	}
	data, err := fixture.Encode(survivor)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if recorded := survivor.Exchanges[0].Request.URL; strings.Contains(recorded, encoded) || strings.Contains(recorded, secret.Value) {
		t.Fatalf("the survivor's URL %q holds the value", recorded)
	}
	if !bytes.Contains(data, []byte(encoded)) {
		t.Fatal("the field spelling is not in the file, so nothing proves the field arm")
	}
	if err := fixture.Check(data, []fixture.Secret{secret}); fixture.Token(err) != fixture.ErrScrubIncomplete.Error() {
		t.Fatalf("the encoded spelling survived the re-read: %v", err)
	}
	// A value inside a saved body is base64 in the file, so the file's text does not
	// spell it at all: only the decoded bytes show it, and that arm refuses it.
	buried := fixture.Document{
		Schema:     fixture.Schema,
		Set:        synthetic.Set,
		Provenance: fixture.Provenance{Source: fixture.SourceSynthetic, Redistribution: fixture.RedistributionRepository},
		Exchanges: []fixture.Exchange{{
			Request:  fixture.Request{Method: http.MethodGet, URL: address(target, "/read")},
			Response: &fixture.Response{Status: http.StatusOK, Body: []byte("prefix " + secret.Value + " suffix")},
		}},
	}
	data, err = fixture.Encode(buried)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bytes.Contains(data, []byte(secret.Value)) {
		t.Fatal("the body's spelling is in the file's text, so the body arm is unproven")
	}
	if err := fixture.Check(data, []fixture.Secret{secret}); fixture.Token(err) != fixture.ErrScrubIncomplete.Error() {
		t.Fatalf("a value inside a saved body survived the re-read: %v", err)
	}
}

// TestReplayDeterministic proves a set answers in the order it was recorded and
// that two replays of it are the same transcript.
func TestReplayDeterministic(t *testing.T) {
	doc := committedSet(t)
	transcript := func() []string {
		replay := fixture.NewReplayer(doc)
		transport := replay.Transport(nil)
		var lines []string
		for range 2 {
			request, err := http.NewRequest(http.MethodGet, address(fixture.Target{Scheme: "http", Host: synthetic.Host, Port: synthetic.Port}, "/throttled"), nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatalf("throttled reply: %v", err)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			_ = response.Body.Close()
			lines = append(lines, fmt.Sprintf("%d %s", response.StatusCode, body))
		}
		return lines
	}
	first, second := transcript(), transcript()
	if len(first) != 2 || !strings.HasPrefix(first[0], "429 ") || !strings.HasPrefix(first[1], "200 ") {
		t.Fatalf("responses %v, want a rate limit and then its answer", first)
	}
	if !equalStrings(first, second) {
		t.Fatalf("two replays differ: %v and %v", first, second)
	}
}

// TestFixtureOfflineReplay proves AC-041: a request the set does not hold is refused
// rather than delegated, each recorded failure replays as the class the client reads,
// and the shipped file is the canonical document the replay reads.
func TestFixtureOfflineReplay(t *testing.T) {
	t.Run("a request the set does not hold is refused and never delegated", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, address(fixture.Target{Scheme: "http", Host: synthetic.Host, Port: synthetic.Port}, "/not-recorded"), nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if _, err := fixture.Replay(t, committedPath()).RoundTrip(request); !errors.Is(err, fixture.ErrUnrecorded) {
			t.Fatalf("an unrecorded request gave %v, want %v", err, fixture.ErrUnrecorded)
		}
		// The counters the contract test reads come from the same replay, whose
		// wrapped transport is never called.
		var inner int
		replay := fixture.NewReplayer(committedSet(t))
		transport := replay.Transport(transportFunc(func(*http.Request) (*http.Response, error) {
			inner++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("fallback"))}, nil
		}))
		if _, err := transport.RoundTrip(request); !errors.Is(err, fixture.ErrUnrecorded) {
			t.Fatalf("the replay gave %v, want %v", err, fixture.ErrUnrecorded)
		}
		if inner != 0 || replay.Refused() != 1 || replay.Served() != 0 {
			t.Fatalf("inner=%d refused=%d served=%d, want 0/1/0", inner, replay.Refused(), replay.Served())
		}
	})

	t.Run("a recorded failure replays as the class the client reads", func(t *testing.T) {
		doc := committedSet(t)
		seen := make(map[string]int, 2)
		for _, exchange := range doc.Exchanges {
			if exchange.Fault != fixture.FaultTimeoutBeforeResponse && exchange.Fault != fixture.FaultResetAfterWrite {
				continue
			}
			seen[exchange.Fault]++
			replay := fixture.NewReplayer(doc)
			request, err := http.NewRequest(exchange.Request.Method, exchange.Request.URL, strings.NewReader(""))
			if err != nil {
				t.Fatalf("%s request: %v", exchange.Fault, err)
			}
			_, err = replay.Transport(nil).RoundTrip(request)
			if err == nil {
				t.Fatalf("%s replayed as an answer", exchange.Fault)
			}
			var timeout net.Error
			if got, want := errors.As(err, &timeout) && timeout.Timeout(), exchange.Fault == fixture.FaultTimeoutBeforeResponse; got != want {
				t.Fatalf("%s replayed as a deadline=%v, want %v", exchange.Fault, got, want)
			}
			// A failure the replay serves wrote the request: the client may not read it
			// as one that never left, which is the only class a resend is free of.
			var dial *net.OpError
			if errors.As(err, &dial) && dial.Op == "dial" {
				t.Fatalf("%s replayed as a failed dial", exchange.Fault)
			}
		}
		for _, fault := range []string{fixture.FaultTimeoutBeforeResponse, fixture.FaultResetAfterWrite} {
			if seen[fault] != 1 {
				t.Fatalf("the committed set holds %d %s exchanges, want one", seen[fault], fault)
			}
		}
	})

	t.Run("a recorded partial answer keeps the length its peer declared", func(t *testing.T) {
		doc := committedSet(t)
		for _, exchange := range doc.Exchanges {
			if exchange.Fault != fixture.FaultPartial {
				continue
			}
			declared := 0
			for name, values := range exchange.Response.Header {
				if strings.EqualFold(name, "Content-Length") && len(values) == 1 {
					declared, _ = strconv.Atoi(values[0])
				}
			}
			if declared <= len(exchange.Response.Body) {
				t.Fatalf("the recorded partial answer declares %d bytes for a %d-byte body: nothing is cut", declared, len(exchange.Response.Body))
			}
			request, err := http.NewRequest(exchange.Request.Method, exchange.Request.URL, nil)
			if err != nil {
				t.Fatalf("partial request: %v", err)
			}
			response, err := fixture.NewReplayer(doc).Transport(nil).RoundTrip(request)
			if err != nil {
				t.Fatalf("partial replay: %v", err)
			}
			// The length is the peer's declaration, not the bytes that arrived, so a
			// consumer that trusts Content-Length still sees the cut the fault names;
			// the body reader is the only thing that fails.
			if response.ContentLength != int64(declared) {
				t.Fatalf("the replay declared %d bytes, want the recorded %d", response.ContentLength, declared)
			}
			if _, err := io.ReadAll(response.Body); err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("the cut body read as %v, want the cut response's own failure", err)
			}
			return
		}
		t.Fatal("the committed set holds no partial exchange")
	})

	t.Run("the shipped file is the canonical document", func(t *testing.T) {
		path := committedPath()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc, err := fixture.Decode(data)
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		encoded, err := fixture.Encode(doc)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(data, encoded) {
			t.Fatalf("%s is not the canonical encoding of its own document", path)
		}
		// The replay reads the directory the recorder publishes to: one root, so a
		// recorded set and the test that replays it cannot drift apart.
		relative, err := filepath.Rel(filepath.Join("..", ".."), filepath.Dir(path))
		if err != nil {
			t.Fatalf("relative path of %s: %v", path, err)
		}
		if filepath.ToSlash(relative) != fixture.Dir(synthetic.Set) {
			t.Fatalf("the replay reads %q, want %q", filepath.ToSlash(relative), fixture.Dir(synthetic.Set))
		}
		// A saved body is the peer's own length, except where the fault is that it is
		// not: a set whose header disagrees with its body misdescribes the wire.
		for i, exchange := range doc.Exchanges {
			if exchange.Response == nil || exchange.Fault == fixture.FaultPartial {
				continue
			}
			for name, values := range exchange.Response.Header {
				if !strings.EqualFold(name, "Content-Length") || len(values) != 1 {
					continue
				}
				length, err := strconv.Atoi(values[0])
				if err != nil || length != len(exchange.Response.Body) {
					t.Fatalf("exchange %d declares Content-Length %q for %d bytes", i, values[0], len(exchange.Response.Body))
				}
			}
		}
		// The runner publishes through the host redactor, which covers every query
		// and fragment value of a URL: a recorded request URL carrying one would be
		// published mangled and would no longer match the request it answers.
		for _, exchange := range doc.Exchanges {
			target, err := url.Parse(exchange.Request.URL)
			if err != nil {
				t.Fatalf("request URL %q: %v", exchange.Request.URL, err)
			}
			if target.RawQuery != "" || target.Fragment != "" {
				t.Fatalf("recorded %s carries a query or fragment value the export redacts", exchange.Request.URL)
			}
		}
		if bytes.Contains(data, []byte("[REDACTED]")) {
			t.Fatal("the published set holds a redacted run: the export changed the bytes it recorded")
		}
		// No shadow schema: a field this harness does not know is a different file.
		shadow := bytes.Replace(data, []byte(`"schema": 1,`), []byte(`"schema": 1, "shadow": true,`), 1)
		if bytes.Equal(shadow, data) {
			t.Fatal("the document holds no schema line to shadow")
		}
		if _, err := fixture.Decode(shadow); err == nil {
			t.Fatal("a document with an unknown field decoded")
		}
		// A partial answer without the bytes that arrived is not a document: a replay
		// of it has nothing to hand the client.
		truncated := bytes.Replace(data, []byte(`"fault": "timeout_before_response"`), []byte(`"fault": "partial"`), 1)
		if bytes.Equal(truncated, data) {
			t.Fatal("the document holds no timeout fault to reclassify")
		}
		if _, err := fixture.Decode(truncated); err == nil {
			t.Fatal("a partial fault without a response decoded")
		}
	})
}

// unreadBody is a response body no recorder may read: an encoded answer is refused
// before its bytes are inspected.
type unreadBody struct{ read, closed bool }

func (b *unreadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (b *unreadBody) Close() error {
	b.closed = true
	return nil
}

// refusesWithoutValues fails when a refusal's own text holds a value the scrub
// removes: the printed reason word is the whole report.
func refusesWithoutValues(t *testing.T, err error) {
	t.Helper()
	for _, secret := range sentinels() {
		if strings.Contains(err.Error(), secret.Value) {
			t.Fatalf("the refusal %q holds the %s sentinel", err, secret.Placeholder)
		}
	}
}

// held reports whether any recorded field carries text.
func held(doc fixture.Document, text string) bool {
	for _, exchange := range doc.Exchanges {
		if strings.Contains(exchange.Request.URL, text) {
			return true
		}
		if exchange.Response == nil {
			continue
		}
		if strings.Contains(string(exchange.Response.Body), text) {
			return true
		}
		for name, values := range exchange.Response.Header {
			if strings.Contains(name, text) {
				return true
			}
			for _, value := range values {
				if strings.Contains(value, text) {
					return true
				}
			}
		}
	}
	return false
}

// classes counts the recorded statuses and faults of a set.
func classes(doc fixture.Document) (map[int]int, map[string]int) {
	statuses := make(map[int]int, len(doc.Exchanges))
	faults := make(map[string]int, 3)
	for _, exchange := range doc.Exchanges {
		if exchange.Response != nil {
			statuses[exchange.Response.Status]++
		}
		if exchange.Fault != "" {
			faults[exchange.Fault]++
		}
	}
	return statuses, faults
}

// equalStrings reports whether two string slices are equal in order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
