package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ErrUnrecorded is a request the set does not hold: the replay answers an
// exchange it recorded or nothing at all, so a test that reaches past its
// fixture fails instead of falling through to the network.
var ErrUnrecorded = errors.New("fixture: unrecorded request")

// ErrNoNetwork is the offline guard's own refusal: a process under NoNetwork
// resolves nothing, so a test that reaches for the network fails here.
var ErrNoNetwork = errors.New("fixture: offline")

// Replayer serves one recorded set as the client's transport. Exchanges of the same
// method and URL are served in the order they were recorded, and the transport it is
// installed around is never called: there is no path from a replay to a peer.
type Replayer struct {
	mu      sync.Mutex
	queues  map[string][]Exchange
	used    map[string]int
	served  int
	refused int
}

// NewReplayer indexes a set by the request each exchange answers.
func NewReplayer(doc Document) *Replayer {
	queues := make(map[string][]Exchange, len(doc.Exchanges))
	for _, exchange := range doc.Exchanges {
		key := exchangeKey(exchange.Request)
		queues[key] = append(queues[key], exchange)
	}
	return &Replayer{queues: queues, used: make(map[string]int, len(queues))}
}

// Replay serves the set file at path, the one the recorder publishes under the
// runner's write root. A provider test names that path once and replays what the
// repository ships, so a missing or unreadable set fails the test that needed
// rather than skips it.
func Replay(tb testing.TB, path string) http.RoundTripper {
	tb.Helper()
	doc, err := Load(path)
	if err != nil {
		tb.Fatalf("replay %s: %v", path, err)
	}
	return NewReplayer(doc).Transport(nil)
}

// NoNetwork installs this process's offline guard and returns the exit status a
// TestMain has to pass on. With it installed the process's own resolver answers
// nothing, so an ordinary test that reaches for the network fails in-process
// instead of passing on whatever the host happens to resolve: replay is the
// default, and an escape fails wherever the suite runs.
func NoNetwork(m *testing.M) int {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, ErrNoNetwork
		},
	}
	return m.Run()
}

// Transport installs the replay around the client's transport, which it ignores.
func (r *Replayer) Transport(http.RoundTripper) http.RoundTripper {
	return roundTripper(func(request *http.Request) (*http.Response, error) {
		key := exchangeKey(Request{Method: request.Method, URL: request.URL.String()})
		r.mu.Lock()
		queue, used := r.queues[key], r.used[key]
		if used >= len(queue) {
			r.refused++
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: %s %s", ErrUnrecorded, request.Method, request.URL.Path)
		}
		r.used[key] = used + 1
		exchange := queue[used]
		r.served++
		r.mu.Unlock()
		return answer(exchange, request)
	})
}

// Served is how many recorded exchanges a replay has answered.
func (r *Replayer) Served() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.served
}

// Refused is how many requests the set did not hold.
func (r *Replayer) Refused() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

// answer rebuilds one exchange as the client's transport would have received it.
func answer(exchange Exchange, request *http.Request) (*http.Response, error) {
	switch exchange.Fault {
	case FaultTimeoutBeforeResponse:
		return nil, timeoutError{}
	case FaultResetAfterWrite:
		return nil, resetError{}
	}
	body := exchange.Response.Body
	reader := io.Reader(bytes.NewReader(body))
	contentLength := int64(len(body))
	if exchange.Fault == FaultPartial {
		// The peer declared a length its body did not reach: that mismatch is the
		// fault, so the replay keeps the declaration and the cut body is the only
		// thing that fails.
		reader = partialReader{reader}
		if declared, ok := recordedLength(exchange.Response); ok {
			contentLength = declared
		}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", exchange.Response.Status, http.StatusText(exchange.Response.Status)),
		StatusCode:    exchange.Response.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header(exchange.Response.Header).Clone(),
		Body:          io.NopCloser(reader),
		ContentLength: contentLength,
		Request:       request,
	}, nil
}

// recordedLength is the Content-Length the peer declared, when it recorded one.
func recordedLength(response *Response) (int64, bool) {
	for name, values := range response.Header {
		if !strings.EqualFold(name, "Content-Length") || len(values) != 1 {
			continue
		}
		length, err := strconv.ParseInt(values[0], 10, 64)
		return length, err == nil && length >= 0
	}
	return 0, false
}

// timeoutError is a peer that took the request and never answered, so the client's
// own attempt bound ended the wait. It reports itself as a deadline, which is how
// the client classifies a send whose bytes may already have landed.
type timeoutError struct{}

func (timeoutError) Error() string   { return "fixture: the peer never answered" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// resetError is a peer that read the request and dropped the connection. It is no
// deadline, so the client spends one attempt on it and never repeats a read the
// peer may already have asked for.
type resetError struct{}

func (resetError) Error() string { return "fixture: the peer reset the connection" }

// partialReader hands out the recorded bytes and then fails: the exchange's
// headers arrived and its body did not.
type partialReader struct{ reader io.Reader }

func (r partialReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("fixture: the body ended early")
	}
	return n, err
}

func exchangeKey(request Request) string { return request.Method + " " + request.URL }
