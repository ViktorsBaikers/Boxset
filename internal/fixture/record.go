package fixture

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The recorder's refusals. Each is a fixed reason word and nothing else: the
// command prints the word, never a request, a header or a body, so a refused
// recording cannot leak the value that refused it.
var (
	ErrSetInvalid          = errors.New("set_invalid")
	ErrSetUnknown          = errors.New("set_unknown")
	ErrProvenanceRequired  = errors.New("provenance_required")
	ErrTargetUnclassified  = errors.New("target_unclassified")
	ErrLiveMethodDenied    = errors.New("live_method_denied")
	ErrScrubIncomplete     = errors.New("scrub_incomplete")
	ErrRecordingIncomplete = errors.New("recording_incomplete")
)

// tokens is every reason Token reports, in the order the recorder decides them.
var tokens = []error{
	ErrSetInvalid,
	ErrSetUnknown,
	ErrProvenanceRequired,
	ErrTargetUnclassified,
	ErrLiveMethodDenied,
	ErrScrubIncomplete,
	ErrRecordingIncomplete,
}

// Token is the fixed reason word of err, or "" for an error this harness does
// not classify.
func Token(err error) string {
	for _, known := range tokens {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return ""
}

// The runner's cap on one published file bounds a recorded body and the whole
// document: a set over it could never be published, so it is refused here where
// the reason is still the recorder's.
const (
	maxRecordedBody = 8 << 20
	maxDocument     = 8 << 20
)

// A Target is one origin a recording may drive, as the scheme, host and port a
// request addresses. A target is writable only through Serve, which registers it
// only when it names the socket Serve was handed; a watched target is the owner's
// live service and receives reads only.
type Target struct {
	Scheme string
	Host   string
	Port   int
}

func (t Target) origin() string {
	return originKey(t.Scheme, t.Host, strconv.Itoa(t.Port))
}

// originKey is the one form a recorded request and a registered target compare in:
// lower-case scheme and host without its root dot, and the effective port when the
// request names none, so a name with an implicit port is the origin with that port.
func originKey(scheme, host, port string) string {
	return strings.ToLower(scheme) + "://" + strings.TrimSuffix(strings.ToLower(host), ".") + ":" + effectivePort(scheme, port)
}

// effectivePort is the port a scheme answers on when a URL names none.
func effectivePort(scheme, port string) string {
	switch {
	case port != "":
		return port
	case strings.EqualFold(scheme, "https"):
		return "443"
	}
	return "80"
}

func originOf(request *http.Request) string {
	return originKey(request.URL.Scheme, request.URL.Hostname(), request.URL.Port())
}

// Recorder is the client's transport wrapper that writes every exchange down. It
// refuses, before the transport it wraps is called, a request to an origin it does
// not know and every write to an origin it may only read, so a refused target
// receives no request at all.
type Recorder struct {
	// started is what this recorder serves itself, watched is what it may only read.
	// Neither is the caller's claim: Serve serves the socket, Watch names the
	// owner's service, and an origin in neither set is refused.
	started map[string]struct{}
	watched map[string]struct{}
	secrets []Secret

	mu        sync.Mutex
	exchanges []Exchange
	refusal   error
	live      bool
}

// NewRecorder records a set and scrubs secrets out of it. Its targets are the
// services it starts itself and the live origins it watches, so no caller can hand
// it a write target it does not hold.
func NewRecorder(secrets []Secret) *Recorder {
	return &Recorder{
		started: make(map[string]struct{}),
		watched: make(map[string]struct{}),
		secrets: secrets,
	}
}

// Serve starts the set's throwaway service on listener and registers target as an
// origin this recorder drives: it is the one way an origin becomes writable here.
// The target has to name this socket — its own address, its own port, over the
// plain HTTP it serves — so the registration describes a socket the recorder holds
// rather than a name somebody says is throwaway.
func (r *Recorder) Serve(target Target, listener net.Listener, handler http.Handler) (*Service, error) {
	if !holds(listener, target) {
		return nil, fmt.Errorf("%w: %s", ErrRecordingIncomplete, target.origin())
	}
	r.mu.Lock()
	r.started[target.origin()] = struct{}{}
	r.mu.Unlock()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return &Service{server: server}, nil
}

// holds reports whether target names the socket listener answers on. A wildcard
// socket is held by no one origin: its request URL could name any interface the
// process happens to have, so it is refused rather than registered.
func holds(listener net.Listener, target Target) bool {
	address, ok := listener.Addr().(*net.TCPAddr)
	return ok &&
		!address.IP.IsUnspecified() &&
		address.IP.String() == target.Host &&
		address.Port == target.Port &&
		strings.EqualFold(target.Scheme, "http")
}

// Watch registers one of the owner's live services: a recording may read its origin
// and never write to it.
func (r *Recorder) Watch(target Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watched[target.origin()] = struct{}{}
}

// A Service is a throwaway listener this recorder started; Close ends it.
type Service struct{ server *http.Server }

func (s *Service) Close() { _ = s.server.Close() }

// roundTripper adapts a function to http.RoundTripper, one wire exchange each.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// Transport installs the recorder around the client's transport.
func (r *Recorder) Transport(inner http.RoundTripper) http.RoundTripper {
	return roundTripper(func(request *http.Request) (*http.Response, error) {
		if err := r.admit(request); err != nil {
			return nil, err
		}
		response, err := inner.RoundTrip(request)
		if err != nil {
			fault, observed := observedFault(err)
			if !observed {
				// No dial and no reset signature: the request may never have
				// reached a peer that read it (a refused dial, a TLS handshake, a
				// cancelled request), so there is no wire exchange to write down
				// and no recorded class that could replay one.
				_ = r.fail(fmt.Errorf("%w: %s", ErrRecordingIncomplete, request.Method))
				return nil, err
			}
			// The peer had the request and produced no answer: the exchange is
			// recorded as the class the transport observed.
			r.append(Exchange{Request: requestOf(request), Fault: fault})
			return nil, err
		}
		if encoding := contentEncoding(response.Header); encoding != "" {
			// The scrub and the re-read inspect a saved body as text, so an encoded
			// body would be saved with whatever value it carries still inside it.
			// The bytes are not read: there is no exchange to write down.
			_ = response.Body.Close()
			_ = r.fail(fmt.Errorf("%w: %s", ErrRecordingIncomplete, request.Method))
			return nil, ErrRecordingIncomplete
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxRecordedBody+1))
		_ = response.Body.Close()
		exchange := Exchange{
			Request: requestOf(request),
			Response: &Response{
				Status: response.StatusCode,
				Header: stableHeader(response.Header),
				Body:   body,
			},
		}
		switch {
		case len(body) > maxRecordedBody:
			_ = r.fail(fmt.Errorf("%w: %s", ErrRecordingIncomplete, request.Method))
			return nil, ErrRecordingIncomplete
		case readErr != nil:
			// Headers arrived and the body did not: the bytes that did arrive are
			// the whole record of it.
			exchange.Fault = FaultPartial
		}
		r.append(exchange)
		if readErr != nil {
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		return response, nil
	})
}

// observedFault is the class of a failure the inner transport returned, as the
// client's own taxonomy reads it. Only a failure with a positive signature is an
// exchange: a dial that wrote nothing is no peer at all, and neither is a TLS
// handshake, a cancelled request or any other error that says nothing about a peer
// that read the request and dropped the connection.
func observedFault(err error) (string, bool) {
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		return "", false
	}
	if reset(err) {
		return FaultResetAfterWrite, true
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return FaultTimeoutBeforeResponse, true
	}
	return "", false
}

// reset reports the peer dropping the connection after it read the request: a
// closed or cut response stream, or the socket's own reset.
func reset(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET)
}

// admit refuses a target the recorder does not know, and any write to an origin it
// may only read, before the wrapped transport could open a connection.
func (r *Recorder) admit(request *http.Request) error {
	started, watched := r.classify(originOf(request))
	if started {
		return nil
	}
	if watched {
		if request.Method != http.MethodGet {
			return r.fail(fmt.Errorf("%w: %s", ErrLiveMethodDenied, request.Method))
		}
		r.markLive()
		return nil
	}
	return r.fail(fmt.Errorf("%w: %s", ErrTargetUnclassified, request.Method))
}

// classify reports what this recorder holds an origin as.
func (r *Recorder) classify(origin string) (started, watched bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, started = r.started[origin]
	_, watched = r.watched[origin]
	return started, watched
}

func (r *Recorder) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refusal == nil {
		r.refusal = err
	}
	return r.refusal
}

func (r *Recorder) append(exchange Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchanges = append(r.exchanges, exchange)
}

// markLive records that the set holds a read of the owner's live service, which is
// where its provenance comes from.
func (r *Recorder) markLive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live = true
}

// Refused is the first refusal of this recording, or nil.
func (r *Recorder) Refused() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusal
}

// Len is how many wire exchanges the recorder has written down.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exchanges)
}

// Faulted is how many exchanges ended without a usable answer. A driver compares it
// against the faults its recipe expects, so a service that answered a call the
// recipe planned a fault for (or failed one it planned an answer for) is refused
// instead of saved as a file that misdescribes the wire.
func (r *Recorder) Faulted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	faults := 0
	for _, exchange := range r.exchanges {
		if exchange.Fault != "" {
			faults++
		}
	}
	return faults
}

// Document closes the recording: the set's exchanges scrubbed of every secret and
// re-read for one the scrub left behind, with the provenance the targets it drove
// prove. A refusal recorded by the transport is returned instead, and a provenance
// the recording does not support is refused before anything is assembled.
func (r *Recorder) Document(set, provenance string) (Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refusal != nil {
		return Document{}, r.refusal
	}
	if !ValidSet(set) {
		return Document{}, fmt.Errorf("%w: %s", ErrSetInvalid, set)
	}
	derived := Provenance{Source: SourceSynthetic, Redistribution: RedistributionRepository}
	if r.live {
		derived = Provenance{Source: SourceLiveGet, Redistribution: RedistributionOwnerOnly}
	}
	if provenance != derived.Source {
		return Document{}, fmt.Errorf("%w: %s", ErrProvenanceRequired, set)
	}
	doc := Document{Schema: Schema, Set: set, Provenance: derived, Exchanges: r.exchanges}
	Scrub(&doc, r.secrets)
	data, err := Encode(doc)
	if err != nil {
		return Document{}, err
	}
	if err := Check(data, r.secrets); err != nil {
		return Document{}, err
	}
	if len(data) > maxDocument {
		return Document{}, fmt.Errorf("%w: %d bytes", ErrRecordingIncomplete, len(data))
	}
	return doc, nil
}

// contentEncoding is the response's declared encoding, or "" when the body is the
// identity the scrub can read.
func contentEncoding(header http.Header) string {
	for _, value := range header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(value, ",") {
			if encoding = strings.TrimSpace(encoding); encoding != "" && !strings.EqualFold(encoding, "identity") {
				return encoding
			}
		}
	}
	return ""
}

func requestOf(request *http.Request) Request {
	return Request{Method: request.Method, URL: request.URL.String()}
}

// stableHeader copies the peer's headers without the one value the clock puts in
// every answer: two recordings of the same synthetic set are the same bytes.
func stableHeader(header http.Header) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	stable := make(map[string][]string, len(header))
	for name, values := range header {
		if strings.EqualFold(name, "Date") {
			continue
		}
		stable[name] = append([]string(nil), values...)
	}
	return stable
}
