// Package synthetic is the outbound-contract recording: the throwaway loopback
// service the recorder drives, the recipe of operations it drives it with, and
// the secrets the scrub must leave out of the set it produces. Everything here is
// generated in the repository; no provider, account or credential is involved, so
// the set's provenance is synthetic and it reproduces byte for byte.
package synthetic

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ViktorsBaikers/Boxset/internal/fixture"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
)

const (
	// Set is the recorded set's name, which is also its runner write root's last
	// element.
	Set = "outbound-contract"
	// Host is the throwaway service's own address. The recorder registers the socket
	// it serves, so the saved request URLs name that address and nothing else.
	Host = "127.0.0.1"
	// Port is the throwaway service's port, and the set's recorded URLs name it. The
	// client dials the request URL's port, so the recipe, the service and the saved
	// bytes agree on one number, and a host that already holds it reports
	// `recording_incomplete` rather than losing byte-for-byte reproducibility.
	Port = 8080
)

// The sentinels stand where a provider keeps its private values: a credential, a
// session cookie, a user name and a provider hostname. The scrub replaces them
// with fixed placeholders, and a recorded set may not hold any of them.
const (
	credentialSentinel = "SYNTHETIC-CREDENTIAL-2f7c"
	cookieSentinel     = "SYNTHETIC-COOKIE-91be"
	userSentinel       = "SYNTHETIC-USER-4d0a"
	hostSentinel       = "SYNTHETIC-HOST-7c31"
)

// Secrets exposes the sentinel values a recording must not save.
func Secrets() []fixture.Secret {
	return []fixture.Secret{
		{Value: credentialSentinel, Placeholder: "<credential>"},
		{Value: cookieSentinel, Placeholder: "<cookie>"},
		{Value: userSentinel, Placeholder: "<user>"},
		{Value: hostSentinel, Placeholder: "<hostname>"},
	}
}

// Target is the origin a recording drives: the one throwaway service this package
// starts. No live target is part of a recording.
func Target() fixture.Target {
	return fixture.Target{Scheme: "http", Host: Host, Port: Port}
}

// An operation is one client call of the recipe, with the number of wire
// exchanges it leaves behind: a read answered with a rate limit spends two.
type operation struct {
	name   string
	method string
	path   string
	body   string
	wire   int
	// fault is the one failure the recipe plans for the call, or "" for a call
	// that must produce an answer. It is checked against what the wire recorded.
	fault string
}

// recipe is the set's fixed program. The call order is the recording order.
func recipe(set string) ([]operation, error) {
	if set != Set {
		return nil, fmt.Errorf("%w: %s", fixture.ErrSetUnknown, set)
	}
	// Every path is a location, never a query: the runner publishes a recording
	// through the host redactor, which covers every query and fragment value of a
	// URL, so a recorded URL carrying one would be published mangled and would no
	// longer match the request it answers.
	return []operation{
		{name: "page", method: http.MethodGet, path: "/catalog", wire: 1},
		{name: "shape", method: http.MethodGet, path: "/shape", wire: 1},
		{name: "auth", method: http.MethodGet, path: "/secure", wire: 1},
		{name: "throttle", method: http.MethodGet, path: "/throttled", wire: 2},
		{name: "duplicate-first", method: http.MethodGet, path: "/duplicates/", wire: 1},
		{name: "duplicate-second", method: http.MethodGet, path: "/duplicates/second", wire: 1},
		{name: "cursor-first", method: http.MethodGet, path: "/repeat/", wire: 1},
		{name: "cursor-second", method: http.MethodGet, path: "/repeat/second", wire: 1},
		{name: "partial", method: http.MethodGet, path: "/partial", wire: 1, fault: fixture.FaultPartial},
		{name: "reset", method: http.MethodGet, path: "/reset", wire: 1, fault: fixture.FaultResetAfterWrite},
		{name: "apply", method: http.MethodPost, path: "/apply", body: `{"action":"toggle"}`, wire: 1, fault: fixture.FaultTimeoutBeforeResponse},
		{name: "account", method: http.MethodGet, path: "/account", wire: 1},
	}, nil
}

// Record drives the set's recipe through the real outbound client against the
// service the recorder starts, and returns the set only when every exchange the
// recipe names was written down and the bytes are clean. The client's own
// classification of a 401, a rate limit or a fault is the recording's subject, so
// it is not an error here; a refusal of this harness is.
func Record(ctx context.Context, set string) (fixture.Document, error) {
	operations, err := recipe(set)
	if err != nil {
		return fixture.Document{}, err
	}
	listener, err := listen()
	if err != nil {
		return fixture.Document{}, err
	}
	recorder := fixture.NewRecorder(Secrets())
	service, err := recorder.Serve(Target(), listener, handler())
	if err != nil {
		_ = listener.Close()
		return fixture.Document{}, err
	}
	defer service.Close()
	client := outbound.New(outbound.Config{
		Resolver:  loopback{},
		Slot:      permit,
		Version:   "0.0.0",
		Transport: recorder.Transport,
	})
	defer func() { _ = client.Close(ctx) }()

	wire := 0
	for _, op := range operations {
		wire += op.wire
		address, err := url.Parse("http://" + net.JoinHostPort(Host, strconv.Itoa(Port)) + op.path)
		if err != nil {
			return fixture.Document{}, err
		}
		faulted := recorder.Faulted()
		_, _ = client.Do(ctx, outbound.Op{
			Name:   op.name,
			Target: target(),
			Method: op.method,
			URL:    address,
			Body:   op.body,
			Safe:   op.method == http.MethodGet,
		}, nil)
		if err := recorder.Refused(); err != nil {
			return fixture.Document{}, err
		}
		planned := 0
		if op.fault != "" {
			planned = 1
		}
		if recorder.Faulted() != faulted+planned {
			return fixture.Document{}, fmt.Errorf("%w: %s", fixture.ErrRecordingIncomplete, op.name)
		}
	}
	if recorder.Len() != wire {
		return fixture.Document{}, fmt.Errorf("%w: %d of %d exchanges", fixture.ErrRecordingIncomplete, recorder.Len(), wire)
	}
	return recorder.Document(set, fixture.SourceSynthetic)
}

// Publish writes a recorded set under the runner's write root, relative to the
// process's working directory, which is the repository root in the child.
func Publish(doc fixture.Document) error {
	return fixture.Publish(fixture.Dir(doc.Set), doc)
}

// target is the one destination the recording drives: the throwaway service, at
// the address it holds, with the sentinel credential attached.
func target() *outbound.Target {
	return &outbound.Target{
		ID:      Set,
		Origin:  outbound.Origin{Scheme: "http", Host: Host, Port: Port},
		Private: true,
		Credential: &outbound.Credential{
			Header: "X-Api-Key",
			Value:  func(context.Context) ([]byte, error) { return []byte(credentialSentinel), nil },
		},
	}
}

// permit is the one slot every wire exchange takes; the recording owns it for its
// whole run, so a send here never queues behind another.
func permit(context.Context) (func(), error) { return func() {}, nil }

// loopback resolves the recorded service's own address to itself and refuses every
// other name, so a recording cannot reach anything else.
type loopback struct{}

func (loopback) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" || !strings.EqualFold(host, Host) {
		return nil, &net.DNSError{Err: "not the recorded service", Name: host, IsNotFound: true}
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// listen is the throwaway service's socket. It cannot be a kernel-chosen port: the
// recorded URLs name this one, and two recordings have to be the same bytes, so a
// host that already holds the port refuses the recording instead of saving a set
// nothing replays.
func listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(Host, strconv.Itoa(Port)))
	if err != nil {
		return nil, fmt.Errorf("%w: the throwaway service could not start on port %d", fixture.ErrRecordingIncomplete, Port)
	}
	return listener, nil
}

// handler answers the recipe's paths. Its bodies are fixed and its one clock
// header is dropped by the recorder, so two recordings are the same bytes.
func handler() http.Handler {
	var mu sync.Mutex
	throttled := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			json(w, http.StatusOK, `{"items":["alpha","beta"],"next":""}`)
		case "/shape":
			// A body the recorded shape does not fit: valid JSON, wrong type.
			json(w, http.StatusOK, `{"items":"not-a-list"}`)
		case "/secure":
			json(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		case "/throttled":
			mu.Lock()
			throttled++
			first := throttled == 1
			mu.Unlock()
			if first {
				w.Header().Set("Retry-After", "1")
				json(w, http.StatusTooManyRequests, `{"error":"slow_down"}`)
				return
			}
			json(w, http.StatusOK, `{"items":[],"next":""}`)
		case "/duplicates/":
			json(w, http.StatusOK, `{"items":["alpha"],"next":"second"}`)
		case "/duplicates/second":
			json(w, http.StatusOK, `{"items":["alpha"],"next":""}`)
		case "/repeat/":
			json(w, http.StatusOK, `{"items":["delta"],"next":"second"}`)
		case "/repeat/second":
			json(w, http.StatusOK, `{"items":[],"next":"second"}`)
		case "/partial":
			// A body declared longer than it is: the peer answered, and the answer
			// was cut short.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"items":[`)
		case "/reset":
			// The peer read the request and then dropped the connection: no answer, and
			// no deadline to bound it either.
			drop(w)
		case "/apply":
			// The peer read the write and never answered: whether it landed is exactly
			// what the caller cannot know, and only the client's own attempt bound ends
			// the wait.
			<-r.Context().Done()
		case "/account":
			w.Header().Set("Set-Cookie", "session="+cookieSentinel)
			json(w, http.StatusOK, fmt.Sprintf(`{"key":%q,"cookie":%q,"user":%q,"host":%q}`,
				credentialSentinel, cookieSentinel, userSentinel, hostSentinel))
		default:
			http.NotFound(w, r)
		}
	})
}

func json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}

// drop ends the exchange with nothing written to it: the peer had the request and
// the connection is gone.
func drop(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}
