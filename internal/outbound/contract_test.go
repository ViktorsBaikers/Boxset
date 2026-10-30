package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/fixture"
	"github.com/ViktorsBaikers/Boxset/internal/ingest"
)

// committedFixtureDir is the set the repository ships, addressed from this package.
// fixture.Dir is the recorder's write root of the same set, so the test replays the
// file the harness publishes.
const committedFixtureDir = "testdata/fixtures/outbound-contract"

// rejectingResolver refuses every name: a replay never dials, so a lookup here
// means the request escaped the fixture.
type rejectingResolver struct{}

func (rejectingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("the fixture replay asked for a name")
}

// committed is the set under test and the destination it was recorded against. The
// origin is read out of the set's own request URLs, so the test cannot name a host
// or a port the recording does not use.
type committed struct {
	doc    fixture.Document
	target *Target
}

// loadCommitted reads the shipped set strictly.
func loadCommitted(t *testing.T) committed {
	t.Helper()
	doc, err := fixture.Load(filepath.Join(committedFixtureDir, fixture.FileName))
	if err != nil {
		t.Fatalf("load the committed set: %v", err)
	}
	if want := filepath.Base(committedFixtureDir); doc.Set != want {
		t.Fatalf("the set names itself %q, want %q", doc.Set, want)
	}
	if !strings.HasSuffix(fixture.Dir(doc.Set), "/"+filepath.ToSlash(committedFixtureDir)) {
		t.Fatalf("%q is not the recorded set's directory %q", committedFixtureDir, fixture.Dir(doc.Set))
	}
	return committed{doc: doc, target: &Target{ID: doc.Set, Origin: recordedOrigin(t, doc)}}
}

// recordedOrigin is the service the set was recorded against.
func recordedOrigin(t *testing.T, doc fixture.Document) Origin {
	t.Helper()
	if len(doc.Exchanges) == 0 {
		t.Fatal("the committed set holds no exchange")
	}
	parsed, err := url.Parse(doc.Exchanges[0].Request.URL)
	if err != nil {
		t.Fatalf("recorded URL %q: %v", doc.Exchanges[0].Request.URL, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", doc.Exchanges[0].Request.URL, err)
	}
	return Origin{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port}
}

// address is one request URL against the recorded service.
func (c committed) address(path string) string {
	return c.target.Origin.Scheme + "://" + c.target.Origin.Host + ":" + strconv.Itoa(c.target.Origin.Port) + path
}

// op is one call against the recorded service.
func (c committed) op(t *testing.T, path string) Op {
	t.Helper()
	parsed, err := url.Parse(c.address(path))
	if err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}
	return Op{Name: "fixture", Target: c.target, Method: http.MethodGet, URL: parsed, Safe: true}
}

// faultOp is the recorded call of one fault class, as the set holds it.
func (c committed) faultOp(t *testing.T, fault string) Op {
	t.Helper()
	for _, exchange := range c.doc.Exchanges {
		if exchange.Fault != fault {
			continue
		}
		parsed, err := url.Parse(exchange.Request.URL)
		if err != nil {
			t.Fatalf("recorded URL %q: %v", exchange.Request.URL, err)
		}
		return Op{Name: fault, Target: c.target, Method: exchange.Request.Method, URL: parsed, Safe: exchange.Request.Method == http.MethodGet}
	}
	t.Fatalf("the committed set holds no %s exchange", fault)
	return Op{}
}

// client is the real client over a fresh replay of the committed set, on a clock
// that answers a retry wait at once.
func (c committed) client(t *testing.T) (*Client, *fixture.Replayer, *testClock) {
	t.Helper()
	clock := newClock(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	replay := fixture.NewReplayer(c.doc)
	slot := newSlot(4)
	client := New(Config{
		Resolver:  rejectingResolver{},
		Slot:      slot.take,
		Now:       clock.now,
		After:     clock.after,
		Version:   "1.2.3",
		Transport: replay.Transport,
	})
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, replay, clock
}

// recordedPage is the page shape the recorded bodies carry. It is the test's own
// decoding of the peer's bytes, not a second fixture schema.
type recordedPage struct {
	Items []string `json:"items"`
	Next  string   `json:"next"`
}

// locateOver walks recorded pages through the real client, one read per page.
func locateOver(t *testing.T, client *Client, set committed, base, identity string, maxPages int) (Presence, error) {
	t.Helper()
	return Locate(context.Background(), func(ctx context.Context, cursor string) (Page, error) {
		response, err := client.Do(ctx, set.op(t, base+cursor), nil)
		if err != nil {
			return Page{}, err
		}
		var page recordedPage
		if err := json.Unmarshal([]byte(response.Body), &page); err != nil {
			return Page{}, err
		}
		return Page{Identities: page.Items, Next: page.Next}, nil
	}, identity, maxPages)
}

// TestFixtureReplayContract proves AC-041 over the committed set: valid, invalid
// shape, partial, unauthenticated, rate-limited, duplicate identity, repeated
// cursor, a reset after the write and timeout-after-write answers all reach the real
// client, which decides each the way its own contract says, and a request the set
// does not hold is refused without a dial.
func TestFixtureReplayContract(t *testing.T) {
	ctx := context.Background()

	t.Run("a recorded page is a complete walk", func(t *testing.T) {
		set := loadCommitted(t)
		client, replay, _ := set.client(t)
		response, err := client.Do(ctx, set.op(t, "/catalog"), nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		var page recordedPage
		if err := json.Unmarshal([]byte(response.Body), &page); err != nil {
			t.Fatalf("decode the page: %v", err)
		}
		if response.Status != http.StatusOK || len(page.Items) != 2 || page.Next != "" {
			t.Fatalf("page = %d %+v, want one complete page of two identities", response.Status, page)
		}
		if replay.Served() != 1 {
			t.Fatalf("the replay served %d exchanges, want 1", replay.Served())
		}
		// A fresh replay: the direct read above already spent this queue's answer.
		walker, _, _ := set.client(t)
		presence, err := locateOver(t, walker, set, "/catalog", "alpha", 3)
		if err != nil {
			t.Fatalf("Locate: %v", err)
		}
		if presence != PresencePresent {
			t.Fatalf("presence = %v, want present", presence)
		}
	})

	t.Run("a recorded body of the wrong shape reaches the caller whole", func(t *testing.T) {
		set := loadCommitted(t)
		client, _, _ := set.client(t)
		response, err := client.Do(ctx, set.op(t, "/shape"), nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		var page recordedPage
		if err := json.Unmarshal([]byte(response.Body), &page); err == nil {
			t.Fatalf("the wrong-shaped body decoded into %+v", page)
		}
		if response.Body != `{"items":"not-a-list"}` {
			t.Fatalf("body = %q, want the recorded bytes untouched", response.Body)
		}
	})

	t.Run("a recorded partial answer is a body refusal", func(t *testing.T) {
		set := loadCommitted(t)
		client, replay, _ := set.client(t)
		if _, err := client.Do(ctx, set.op(t, "/partial"), nil); err == nil {
			t.Fatal("a truncated body was accepted")
		} else {
			var refused *ingest.Error
			if !errors.As(err, &refused) || refused.Reason != "body_partial" {
				t.Fatalf("refusal = %v, want a body_partial ingest refusal", err)
			}
		}
		if replay.Served() != 1 {
			t.Fatalf("the replay served %d exchanges, want the one recorded answer", replay.Served())
		}
	})

	t.Run("a recorded 401 is an authentication failure", func(t *testing.T) {
		set := loadCommitted(t)
		client, _, _ := set.client(t)
		_, err := client.Do(ctx, set.op(t, "/secure"), nil)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Reason != ReasonAuthFailed {
			t.Fatalf("refusal = %v, want %s", err, ReasonAuthFailed)
		}
		if refusal.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", refusal.Attempts)
		}
	})

	t.Run("a recorded rate limit is retried after the wait it asked for", func(t *testing.T) {
		set := loadCommitted(t)
		client, replay, clock := set.client(t)
		response, err := client.Do(ctx, set.op(t, "/throttled"), nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Status != http.StatusOK {
			t.Fatalf("status = %d, want the answer after the rate limit", response.Status)
		}
		if replay.Served() != 2 {
			t.Fatalf("the replay served %d exchanges, want the rate limit and its answer", replay.Served())
		}
		if waited := clock.waitedSoFar(); waited < time.Second {
			t.Fatalf("waited %v, want at least the recorded second", waited)
		}
	})

	t.Run("a recorded duplicate identity is ambiguous", func(t *testing.T) {
		set := loadCommitted(t)
		client, _, _ := set.client(t)
		presence, err := locateOver(t, client, set, "/duplicates/", "alpha", 4)
		if err != nil {
			t.Fatalf("Locate: %v", err)
		}
		if presence != PresenceAmbiguous {
			t.Fatalf("presence = %v, want ambiguous", presence)
		}
	})

	t.Run("a recorded repeated cursor decides nothing", func(t *testing.T) {
		set := loadCommitted(t)
		client, _, _ := set.client(t)
		presence, err := locateOver(t, client, set, "/repeat/", "delta", 4)
		if err != nil {
			t.Fatalf("Locate: %v", err)
		}
		if presence != PresenceUnknown {
			t.Fatalf("presence = %v, want unknown", presence)
		}
	})

	t.Run("a recorded answer that never came is an unknown write", func(t *testing.T) {
		set := loadCommitted(t)
		client, replay, _ := set.client(t)
		op := set.faultOp(t, fixture.FaultTimeoutBeforeResponse)
		op.Body = `{"action":"toggle"}`
		_, err := client.Do(ctx, op, nil)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Reason != ReasonOutcomeUnknown || !refusal.Unknown {
			t.Fatalf("refusal = %v, want an unknown outcome", err)
		}
		if refusal.Attempts != 1 || replay.Served() != 1 {
			t.Fatalf("attempts = %d, served = %d, want one written attempt", refusal.Attempts, replay.Served())
		}
	})

	t.Run("a recorded reset after the write is one wire request", func(t *testing.T) {
		set := loadCommitted(t)
		client, replay, clock := set.client(t)
		_, err := client.Do(ctx, set.faultOp(t, fixture.FaultResetAfterWrite), nil)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Reason != ReasonOutcomeUnknown || refusal.Unknown {
			t.Fatalf("refusal = %v, want a failed exchange", err)
		}
		// The peer read the request, so the read is not repeated: one wire request, no
		// wait, and nothing merged into a second attempt.
		if refusal.Attempts != 1 || replay.Served() != 1 {
			t.Fatalf("attempts = %d, served = %d, want one wire request", refusal.Attempts, replay.Served())
		}
		if waited := clock.waitedSoFar(); waited != 0 {
			t.Fatalf("waited %v, want no backoff after a reset", waited)
		}
	})

	t.Run("a request the set does not hold is refused", func(t *testing.T) {
		set := loadCommitted(t)
		// At the transport: the replay's own refusal, not a dial that failed.
		request, err := http.NewRequest(http.MethodGet, set.address("/unrecorded"), nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if _, err := fixture.NewReplayer(set.doc).Transport(nil).RoundTrip(request); !errors.Is(err, fixture.ErrUnrecorded) {
			t.Fatalf("the replay gave %v, want %v", err, fixture.ErrUnrecorded)
		}
		client, replay, _ := set.client(t)
		_, err = client.Do(ctx, set.op(t, "/unrecorded"), nil)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Reason != ReasonOutcomeUnknown || refusal.Attempts != 1 {
			t.Fatalf("refusal = %v, want one failed read", err)
		}
		if replay.Refused() != 1 || replay.Served() != 0 {
			t.Fatalf("refused = %d, served = %d, want the request refused and nothing served", replay.Refused(), replay.Served())
		}
	})
}
