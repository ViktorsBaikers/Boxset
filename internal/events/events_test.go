package events_test

// The event stream's own proofs. The hub is bound to the real auth.Revalidate
// over a real store and served through the real operation, so a frame's
// authority, its framing and its bound are decided by production code: only the
// guard that would mint a ref is replaced, because it is unexported in its own
// package.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const (
	testInstallID = "0123456789ab"
	testPassword  = "synthetic-valid-password"
)

// world is one installation: a real store, the auth owners over it, the one hub
// bound to auth.Revalidate, and the HTTP surface that serves it.
type world struct {
	t        *testing.T
	db       *store.DB
	hasher   *auth.Hasher
	sessions *auth.Sessions
	keys     *auth.Keys
	hub      *events.Hub
	logs     *logSink
	server   *httptest.Server
	client   *http.Client

	admin      auth.Session
	adminToken string
	adminRef   auth.CredentialRef

	viewer    auth.Session
	disabled  auth.Session
	named     map[string]auth.Session
	keyID     int64
	keyRef    auth.CredentialRef
	keyRights []string
}

type logSink struct {
	mu      sync.Mutex
	records []string
}

func (l *logSink) log(reason string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fmt.Sprint(reason, kv))
}

func (l *logSink) find(needle string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, record := range l.records {
		if strings.Contains(record, needle) {
			return record
		}
	}
	return ""
}

// options customizes the hub one world binds.
type options struct {
	now       func() time.Time
	heartbeat time.Duration
	// revoke decides, per current-authority call, that the grant has just ended.
	// The real decision still decides every other call: the wrapper can only make
	// the stream stricter, never more permissive, so a test can end authority at a
	// chosen point in a replay without racing a database write.
	revoke func(call int) bool
}

func newWorld(t *testing.T, opts options) *world {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hasher := auth.NewHasher()
	if _, err := auth.CreateUser(ctx, db, hasher, "admin", testPassword, auth.RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	sessions := auth.NewSetup(db, hasher, dir).Sessions()
	admin, token, _, err := sessions.Login(ctx, "admin", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	w := &world{t: t, db: db, hasher: hasher, sessions: sessions, keys: sessions.Keys(), logs: &logSink{}, named: map[string]auth.Session{}}
	w.admin, w.adminToken = admin, token
	w.adminRef = auth.SessionRef(admin)
	now := opts.now
	if now == nil {
		now = time.Now
	}
	w.hub = events.New(events.Config{
		Boot:         "boot-events",
		InstallID:    testInstallID,
		RestoreEpoch: admin.RestoreEpoch,
		Now:          opts.now,
		Heartbeat:    opts.heartbeat,
		Log:          w.logs.log,
		Revalidate:   w.revalidator(db, now, opts.revoke),
	})
	t.Cleanup(w.hub.Close)
	mux := http.NewServeMux()
	api.New(mux, &eventService{hub: w.hub})
	// The guard is replaced by the identity the test already persisted: it attaches
	// the principal and the ref the guard's own mint functions produced.
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get("X-Test-Cred")
		p, ref, ok := w.credential(cred)
		if ok {
			ctx := auth.WithCredentialRef(auth.WithPrincipal(r.Context(), p), ref)
			r = r.WithContext(ctx)
		}
		mux.ServeHTTP(rw, r)
	})
	w.server = httptest.NewServer(handler)
	t.Cleanup(w.server.Close)
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	w.client = &http.Client{Transport: transport}
	return w
}

// revalidator is the hub's one current-authority decision as these proofs bind
// it: the production auth.Revalidate over the world's store, optionally ended by
// revoke at one chosen call.
func (w *world) revalidator(db *store.DB, now func() time.Time, revoke func(call int) bool) func(context.Context, auth.CredentialRef) (auth.Principal, time.Time, error) {
	decide := func(ctx context.Context, ref auth.CredentialRef) (auth.Principal, time.Time, error) {
		return auth.Revalidate(ctx, db, ref, now())
	}
	if revoke == nil {
		return decide
	}
	calls := 0
	return func(ctx context.Context, ref auth.CredentialRef) (auth.Principal, time.Time, error) {
		calls++
		if revoke(calls) {
			return auth.Principal{}, time.Time{}, auth.ErrSessionInvalid
		}
		return decide(ctx, ref)
	}
}

// credential resolves one test credential name to the principal and ref the
// guard would have produced for it.
func (w *world) credential(name string) (auth.Principal, auth.CredentialRef, bool) {
	if session, ok := w.named[name]; ok {
		return auth.Principal{Rights: session.Rights}, auth.SessionRef(session), true
	}
	switch name {
	case "admin":
		return auth.Principal{Rights: w.admin.Rights}, w.adminRef, true
	case "admin-key":
		if !w.keyRef.Valid() {
			return auth.Principal{}, auth.CredentialRef{}, false
		}
		return auth.Principal{Rights: w.keyRights}, w.keyRef, true
	case "viewer":
		if w.viewer.User.ID == 0 {
			return auth.Principal{}, auth.CredentialRef{}, false
		}
		return auth.Principal{Rights: w.viewer.Rights}, auth.SessionRef(w.viewer), true
	case "disabled":
		if w.disabled.User.ID == 0 {
			return auth.Principal{}, auth.CredentialRef{}, false
		}
		return auth.Principal{Rights: w.disabled.Rights}, auth.SessionRef(w.disabled), true
	case "bypass":
		return w.bypass("127.0.0.1:40000")
	case "trusted-proxy-bypass":
		local := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
		trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
		ref, ok := auth.BypassRef("127.0.0.1:40000", local, trusted)
		if !ok {
			return auth.Principal{}, auth.CredentialRef{}, false
		}
		return auth.Principal{Rights: []string{auth.RightStatusRead}}, ref.WithEpoch(w.admin.RestoreEpoch), true
	}
	// A named bypass peer: the same direct address on the ephemeral port the name
	// carries, which is what the guard's own decision mints for one connection.
	if port, ok := strings.CutPrefix(name, "bypass:"); ok {
		return w.bypass("127.0.0.1:" + port)
	}
	return auth.Principal{}, auth.CredentialRef{}, false
}

// bypass is the direct-peer grant's principal and ref for one peer socket.
func (w *world) bypass(peer string) (auth.Principal, auth.CredentialRef, bool) {
	local := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	ref, ok := auth.BypassRef(peer, local, nil)
	if !ok {
		return auth.Principal{}, auth.CredentialRef{}, false
	}
	return auth.Principal{Rights: []string{auth.RightStatusRead, auth.RightUsersRead, auth.RightUsersWrite}}, ref.WithEpoch(w.admin.RestoreEpoch), true
}

// withViewer persists a second administrator and signs it in: the downgrade
// target, whose accepted grant is wider than the one the downgrade leaves.
func (w *world) withViewer() {
	w.viewer = w.user("viewer", auth.RoleAdmin)
}

// withKey creates one read-scoped key through the parent's own owner and keeps
// the ref its authentication produces.
func (w *world) withKey() {
	id, token, err := w.keys.Create(w.t.Context(), "admin", "test", "read")
	if err != nil {
		w.t.Fatalf("create key: %v", err)
	}
	w.keyID = id
	session, err := w.keys.Authenticate(w.t.Context(), token)
	if err != nil {
		w.t.Fatalf("authenticate key: %v", err)
	}
	w.keyRef, w.keyRights = auth.KeyRef(session), session.Rights
}

// user persists one account through the parent's own owners and signs it in.
func (w *world) user(name, role string) auth.Session {
	w.t.Helper()
	if _, err := auth.CreateUser(w.t.Context(), w.db, w.hasher, name, testPassword, role); err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	session, _, _, err := w.sessions.Login(w.t.Context(), name, testPassword)
	if err != nil {
		w.t.Fatalf("login %s: %v", name, err)
	}
	return session
}

// eventService answers the operations the stream needs. It serves one hub and the
// installation identity; every other operation is unused by these proofs.
type eventService struct {
	hub *events.Hub
}

func (s *eventService) Events() *events.Hub { return s.hub }
func (s *eventService) Installation(context.Context) (string, error) {
	return testInstallID, nil
}
func (s *eventService) ListUsers(context.Context, auth.UserQuery, func(auth.UserCollection) error) error {
	return nil
}
func (s *eventService) UpdateUser(context.Context, auth.Actor, int64, auth.UserPatch) (auth.User, error) {
	return auth.User{}, errors.New("unused")
}
func (s *eventService) Login(context.Context, string, string) (auth.Session, string, error) {
	return auth.Session{}, "", errors.New("unused")
}
func (s *eventService) Logout(context.Context) (string, error) { return "", errors.New("unused") }
func (s *eventService) CreateFirstAdmin(context.Context, auth.Principal, string, string) (auth.User, error) {
	return auth.User{}, errors.New("unused")
}
func (s *eventService) Status(context.Context) (api.Status, error) {
	return api.Status{}, errors.New("unused")
}
func (s *eventService) ListJobs(context.Context, jobs.Query) (jobs.Page, error) {
	return jobs.Page{}, errors.New("unused")
}

// frame is one decoded server-sent frame.
type frame struct {
	id    string
	event string
	data  json.RawMessage
}

// sse is one open stream with its bounded reader.
type sse struct {
	t      *testing.T
	resp   *http.Response
	reader *bufio.Reader
}

// open connects one stream to this world's own server.
func (w *world) open(cred, lastEventID string, header ...string) *sse {
	return w.openAt(w.server.URL, cred, lastEventID, header...)
}

// openAt connects one stream to base as cred, with an optional Last-Event-ID.
func (w *world) openAt(base, cred, lastEventID string, header ...string) *sse {
	w.t.Helper()
	request, err := http.NewRequestWithContext(w.t.Context(), http.MethodGet, base+"/api/v1/events", nil)
	if err != nil {
		w.t.Fatalf("request: %v", err)
	}
	request.Header.Set("X-Test-Cred", cred)
	request.Header.Set("Accept-Encoding", "identity")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	for i := 0; i+1 < len(header); i += 2 {
		request.Header.Set(header[i], header[i+1])
	}
	resp, err := w.client.Do(request)
	if err != nil {
		w.t.Fatalf("open %s: %v", cred, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		w.t.Fatalf("open %s: %d %s", cred, resp.StatusCode, body)
	}
	return &sse{t: w.t, resp: resp, reader: bufio.NewReaderSize(resp.Body, 1<<16)}
}

// status opens a stream and returns the refusal status without reading frames.
func (w *world) status(cred, lastEventID string) (int, string, string) {
	w.t.Helper()
	request, _ := http.NewRequestWithContext(w.t.Context(), http.MethodGet, w.server.URL+"/api/v1/events", nil)
	request.Header.Set("X-Test-Cred", cred)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := w.client.Do(request)
	if err != nil {
		w.t.Fatalf("status %s: %v", cred, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header.Get("Retry-After"), string(body)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), ""
}

// streamRefusal asks for one stream and returns its refusal, or 200 when one was
// admitted. The request carries its own bound and its body is never read, so a
// probe can never block on a stream it did not expect.
func (w *world) streamRefusal(cred string) (int, string, string) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(w.t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, w.server.URL+"/api/v1/events", nil)
	if err != nil {
		w.t.Fatalf("request: %v", err)
	}
	request.Header.Set("X-Test-Cred", cred)
	request.Header.Set("Accept-Encoding", "identity")
	resp, err := w.client.Do(request)
	if err != nil {
		w.t.Fatalf("stream probe %s: %v", cred, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, "", ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, resp.Header.Get("Retry-After"), string(body)
}

// read returns the next frame, skipping heartbeat comments, under the recorded
// reader bounds: no line over 8,199 raw bytes, at most one id, event and data
// line, and no partial dispatch.
func (s *sse) read() (frame, error) {
	var out frame
	var id, name, data int
	for {
		line, err := readLine(s.reader)
		if err != nil {
			return frame{}, err
		}
		if len(line) == 0 {
			if id+name+data == 0 {
				continue
			}
			if data != 1 || id > 1 || name > 1 {
				return frame{}, errors.New("malformed frame")
			}
			return out, nil
		}
		text := string(line)
		switch {
		case text[0] == ':':
			// A comment, the heartbeat included, carries nothing.
		case strings.HasPrefix(text, "id:"):
			id++
			out.id = strings.TrimPrefix(strings.TrimPrefix(text, "id:"), " ")
		case strings.HasPrefix(text, "event:"):
			name++
			out.event = strings.TrimPrefix(strings.TrimPrefix(text, "event:"), " ")
		case strings.HasPrefix(text, "data:"):
			data++
			payload := strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " ")
			if len(payload) > 8192 || !json.Valid([]byte(payload)) {
				return frame{}, errors.New("malformed data")
			}
			out.data = json.RawMessage(payload)
		default:
			// An unknown field is discarded as soon as its line ends.
		}
	}
}

// readLine reads one CRLF- or LF-terminated line, refusing one over the recorded
// 8,199 raw bytes excluding the LF.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > 8200 {
			return nil, errors.New("line too long")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 8199 {
			return nil, errors.New("line too long")
		}
		return line, nil
	}
}

func (s *sse) close() { _ = s.resp.Body.Close() }

// TestEventReplayHandshake is the cursor contract: a watermark race yields
// complete ordered state, an in-ring cursor replays gaplessly, and another
// epoch or boot, an evicted cursor and a malformed or ahead one each resync with
// its own reason.
func TestEventReplayHandshake(t *testing.T) {
	w := newWorld(t, options{})
	for i := range 3 {
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(int64(i + 1)))})
	}

	// A first connection snapshots at the watermark the hello carries.
	live := w.open("admin", "")
	first, err := live.read()
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	var hello events.EventHello
	if err := json.Unmarshal(first.data, &hello); err != nil {
		t.Fatalf("hello payload: %v", err)
	}
	if first.event != "hello" || hello.Watermark != 3 || hello.InstallID != testInstallID || hello.RestoreEpoch != 1 || hello.UserID != w.admin.User.ID {
		t.Fatalf("hello = %+v (event %q)", hello, first.event)
	}
	// The watermark handshake: the events published after subscribing are the
	// ones this client sees, in order, with no gap and no duplicate.
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(4))})
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(5))})
	for want := int64(4); want <= 5; want++ {
		next, err := live.read()
		if err != nil {
			t.Fatalf("live frame %d: %v", want, err)
		}
		if got := jobID(t, next); got != want {
			t.Fatalf("live frame job = %d, want %d", got, want)
		}
	}
	live.close()

	// An in-ring cursor replays exactly what it missed, in order.
	replay := w.open("admin", "1.boot-events.1")
	if _, err := replay.read(); err != nil {
		t.Fatalf("replay hello: %v", err)
	}
	for want := int64(2); want <= 5; want++ {
		next, err := replay.read()
		if err != nil {
			t.Fatalf("replay frame %d: %v", want, err)
		}
		if got := jobID(t, next); got != want {
			t.Fatalf("replay frame job = %d, want %d", got, want)
		}
	}
	replay.close()

	// A cursor at the watermark has nothing to replay.
	current := w.open("admin", "1.boot-events.5")
	if _, err := current.read(); err != nil {
		t.Fatalf("current hello: %v", err)
	}
	current.close()

	for _, tc := range []struct {
		name   string
		cursor string
		reason string
	}{
		{"other epoch", "2.boot-events.5", "epoch"},
		{"other boot", "1.other-boot.5", "epoch"},
		{"malformed", "not-a-frame-id", "invalid_id"},
		{"ahead", "1.boot-events.9", "invalid_id"},
		// A cursor longer than any header bound the route could declare still
		// reaches the hub: it is a malformed cursor, answered by the stream's own
		// resync, never a validation refusal before the handshake.
		{"over-long", strings.Repeat("x", 4096), "invalid_id"},
		// A cursor of nothing but separators is the shape that would allocate one
		// slice per part if the hub split it: it is refused as malformed instead.
		{"dot flood", strings.Repeat(".", 4096), "invalid_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := w.open("admin", tc.cursor)
			defer stream.close()
			if _, err := stream.read(); err != nil {
				t.Fatalf("hello: %v", err)
			}
			resync, err := stream.read()
			if err != nil {
				t.Fatalf("resync: %v", err)
			}
			var body events.EventResync
			if err := json.Unmarshal(resync.data, &body); err != nil {
				t.Fatalf("resync payload: %v", err)
			}
			if resync.event != "resync" || body.Reason != tc.reason {
				t.Fatalf("resync = %+v, want reason %q", body, tc.reason)
			}
		})
	}
	// An evicted cursor is one the ring's lower bound passed: the unrepresentable
	// event raises it, and a cursor at or below it can only resync as a gap.
	t.Run("evicted", func(t *testing.T) {
		evicted := newWorld(t, options{})
		evicted.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(1))})
		evicted.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(2))})
		evicted.hub.Publish(events.Event{Name: events.NameJob, Payload: map[string]any{"heavy": strings.Repeat("x", 9000)}})
		stream := evicted.open("admin", "1.boot-events.2")
		defer stream.close()
		if _, err := stream.read(); err != nil {
			t.Fatalf("hello: %v", err)
		}
		resync, err := stream.read()
		if err != nil {
			t.Fatalf("resync: %v", err)
		}
		var body events.EventResync
		_ = json.Unmarshal(resync.data, &body)
		if body.Reason != "gap" {
			t.Fatalf("resync = %+v, want gap", body)
		}
	})
}

// TestEventAdmitFloodCost proves what a hostile cursor may cost the hub: the 64
// KiB dot flood the handshake case above refuses is run through the admission
// path a client's Last-Event-ID takes, with the hub, the stream it admits and the
// stream's notification channel counted in every run. The cursor parser lives in
// the hub package and is unexported, so the cost is observed here, on the path
// that calls it: two cuts hand back substrings of the cursor, while a split would
// keep one string header per separator, 65,536 of them, in a slice of its own. The
// wire-level dot-flood case proves the refusal a client sees; this proves its size.
func TestEventAdmitFloodCost(t *testing.T) {
	w := newWorld(t, options{})
	principal, ref, ok := w.credential("admin")
	if !ok {
		t.Fatal("no admin credential")
	}
	cursor := strings.Repeat(".", 1<<16)
	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			hub := events.New(events.Config{
				Boot: "boot-events", RestoreEpoch: 1, InstallID: testInstallID,
				Revalidate: func(context.Context, auth.CredentialRef) (auth.Principal, time.Time, error) {
					return auth.Principal{}, time.Time{}, nil
				},
			})
			if _, err := hub.Admit(principal, ref, cursor); err != nil {
				b.Fatalf("a fresh hub's first stream: %v", err)
			}
		}
	})
	t.Logf("ADMIT-FLOOD-COST allocs_per_admit=%d bytes_per_admit=%d", result.AllocsPerOp(), result.AllocedBytesPerOp())
	// The bound is the admission path's own bookkeeping in bytes. A split cursor
	// would keep a 65,536-header slice (about 1 MiB) per run, three orders of
	// magnitude above it.
	if bytes := result.AllocedBytesPerOp(); bytes > 8<<10 {
		t.Fatalf("a 64 KiB dot flood cost %d bytes per admission, want the path's own bookkeeping (a split would keep one string header per separator, 65,536 of them)", bytes)
	}
}

// jobEvent is one committed job transition shaped like the runtime's own.
func jobEvent(id int64) jobs.Event {
	return jobs.Event{
		JobID: id, Kind: "alias.purge", Class: "required", Lane: "background",
		State: "succeeded", Reason: "purged", Params: map[string]string{"generation": "7"},
		Attempts: 1, MaxAttempts: 5, DueAt: 1_800_000_000, CreatedAt: 1_799_999_000,
		UpdatedAt: 1_800_000_100, Revision: 2,
	}
}

func jobID(t *testing.T, f frame) int64 {
	t.Helper()
	var body api.EventJob
	if err := json.Unmarshal(f.data, &body); err != nil {
		t.Fatalf("job payload: %v (%s)", err, f.data)
	}
	return body.Job.ID
}

// TestEventPayloadsMatchOpenAPI proves every frame's data is one of the
// document's components, that a maximum valid EventJob is delivered whole or
// announced as an overflow, and that the reader refuses what the wire forbids.
func TestEventPayloadsMatchOpenAPI(t *testing.T) {
	doc := openAPIDocument(t)
	w := newWorld(t, options{})

	// Every component the stream can emit is in the document and validates.
	stream := w.open("admin", "")
	t.Cleanup(stream.close)
	hello, err := stream.read()
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	validate(t, doc, "EventHello", hello.data)

	// A maximum valid job: the row's own bounds (a 64-character kind and reason
	// code, int64 identity and revision) and the eight reason_params entries whose
	// escaped JSON fills the store's 1,024-byte column, so this is the largest
	// payload a stored row can hand the stream.
	params := map[string]string{}
	for i := range 8 {
		params[fmt.Sprintf("p%d", i)] = strings.Repeat("<>&\"\\", 5)
	}
	if stored, err := json.Marshal(params); err != nil || len(stored) > 1024 {
		t.Fatalf("the fixture's reason_params = %d stored bytes (err %v), want the 1,024-byte column or less", len(stored), err)
	}
	event := jobEvent(42)
	event.JobID = math.MaxInt64
	event.Kind = "k" + strings.Repeat("0", 63)
	event.Reason = "r" + strings.Repeat("_", 63)
	event.Revision = math.MaxInt64
	event.MaxAttempts = 10
	event.Params = params
	payload := api.JobEvent(event)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) > 8192 {
		t.Fatalf("the maximum valid EventJob encodes to %d bytes; the fixture must fit", len(encoded))
	}
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: payload})
	frameJob, err := stream.read()
	if err != nil {
		t.Fatalf("job frame: %v", err)
	}
	if !bytes.Equal(normalise(t, frameJob.data), normalise(t, encoded)) {
		t.Fatalf("the delivered job differs from its encoding:\n%s\n%s", frameJob.data, encoded)
	}
	validate(t, doc, "EventJob", frameJob.data)
	t.Logf("EVENT-JOB encoded=%d bytes", len(encoded))

	// A users frame and the drain's system frame carry their own components.
	w.hub.Publish(events.Event{Name: events.NameUsers, Payload: api.EventUsers{CollectionRevision: 7}})
	users, err := stream.read()
	if err != nil {
		t.Fatalf("users frame: %v", err)
	}
	validate(t, doc, "EventUsers", users.data)
	// The drain publishes the system frame and then closes the streams, so the
	// frame is delivered before the stream ends.
	w.hub.Publish(events.Event{Name: events.NameSystem, Payload: api.EventSystem{State: "draining"}})
	w.hub.Close()
	system, err := stream.read()
	if err != nil {
		t.Fatalf("system frame: %v", err)
	}
	validate(t, doc, "EventSystem", system.data)
}

// TestEventUnrepresentable proves an oversized or unencodable event consumes its
// sequence, raises the replay bound and closes the stream with resync{overflow}:
// none of its bytes reach the ring, a queue or the wire.
func TestEventUnrepresentable(t *testing.T) {
	const sentinel = "SENTINEL-OVERFLOW-0451"
	for _, tc := range []struct {
		name    string
		payload any
		reason  string
	}{
		{"oversize", map[string]any{"heavy": strings.Repeat(sentinel, 400)}, "overflow"},
		{"encode failure", func() {}, "encode_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, options{})
			stream := w.open("admin", "")
			defer stream.close()
			helloFrame, err := stream.read()
			if err != nil {
				t.Fatalf("hello: %v", err)
			}
			var hello events.EventHello
			_ = json.Unmarshal(helloFrame.data, &hello)

			w.hub.Publish(events.Event{Name: events.NameJob, Payload: tc.payload})
			resync, err := stream.read()
			if err != nil {
				t.Fatalf("resync: %v", err)
			}
			var body events.EventResync
			if err := json.Unmarshal(resync.data, &body); err != nil {
				t.Fatalf("resync payload: %v", err)
			}
			if resync.event != "resync" || body.Reason != "overflow" || body.Watermark != hello.Watermark+1 {
				t.Fatalf("resync = %+v, want overflow at the consumed sequence", body)
			}
			if strings.Contains(string(resync.data), sentinel) {
				t.Fatal("the unrepresentable payload reached the wire")
			}
			if _, err := stream.read(); err == nil {
				t.Fatal("the stream did not close after resync{overflow}")
			}
			// The sequence's replay bound was raised: an older cursor is a gap.
			older := w.open("admin", fmt.Sprintf("%d.boot-events.%d", hello.RestoreEpoch, hello.Watermark))
			defer older.close()
			if _, err := older.read(); err != nil {
				t.Fatalf("hello: %v", err)
			}
			gap, err := older.read()
			if err != nil {
				t.Fatalf("gap resync: %v", err)
			}
			var gapBody events.EventResync
			_ = json.Unmarshal(gap.data, &gapBody)
			if gapBody.Reason != "gap" {
				t.Fatalf("the raised bound did not evict the old cursor: %+v", gapBody)
			}
			if record := w.logs.find("event_unrepresentable"); record == "" || !strings.Contains(record, "encoded") {
				t.Fatalf("the encoded length was not recorded: %q", record)
			}
		})
	}
}

// TestEventReaderBoundary proves the recorded wire bounds from the reader side:
// a full-size payload passes in both forms, and every malformed line is refused
// instead of truncated or dispatched.
func TestEventReaderBoundary(t *testing.T) {
	payload := `"` + strings.Repeat("a", 8190) + `"`
	if len(payload) != 8192 {
		t.Fatalf("payload %d bytes", len(payload))
	}
	good := []struct{ name, raw string }{
		{"provider form", "data: " + payload + "\n\n"},
		{"crlf form", "data: " + payload + "\r\n\r\n"},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			stream := &sse{t: t, reader: bufio.NewReaderSize(strings.NewReader(tc.raw), 1<<16)}
			f, err := stream.read()
			if err != nil {
				t.Fatalf("a full-size payload was refused: %v", err)
			}
			if len(f.data) != 8192 {
				t.Fatalf("payload %d bytes", len(f.data))
			}
		})
	}
	bad := []struct{ name, raw string }{
		{"over-cap payload", "data: " + `"` + strings.Repeat("a", 8192) + `"` + "\n\n"},
		{"unterminated line", "data: " + payload},
		{"non-json data", "data: not-json\n\n"},
		{"duplicate data", "data: {}\ndata: {}\n\n"},
		{"duplicate id", "id: 1.boot-events.1\nid: 1.boot-events.2\ndata: {}\n\n"},
		{"overlong field name", strings.Repeat("x", 8200) + ": value\n\n"},
		{"overlong comment", ": " + strings.Repeat("c", 8200) + "\n\n"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			stream := &sse{t: t, reader: bufio.NewReaderSize(strings.NewReader(tc.raw), 1<<16)}
			if f, err := stream.read(); err == nil {
				t.Fatalf("a malformed frame was dispatched: %+v", f)
			}
		})
	}
	// A run of comments is discarded as it arrives, never accumulated.
	repeated := strings.Repeat(": hb\n", 64) + "data: {}\n\n"
	stream := &sse{t: t, reader: bufio.NewReaderSize(strings.NewReader(repeated), 1<<16)}
	if _, err := stream.read(); err != nil {
		t.Fatalf("comments before a frame: %v", err)
	}
}

// openAPIDocument reads the committed document these proofs validate against.
func openAPIDocument(t *testing.T) document {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "api", "openapi.json"))
	if err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	return doc
}

type document struct {
	Components struct {
		Schemas map[string]schema `json:"schemas"`
	} `json:"components"`
}

type schema struct {
	Ref                  string            `json:"$ref"`
	Type                 string            `json:"type"`
	Properties           map[string]schema `json:"properties"`
	Items                *schema           `json:"items"`
	Required             []string          `json:"required"`
	Enum                 []any             `json:"enum"`
	Pattern              string            `json:"pattern"`
	Minimum              *float64          `json:"minimum"`
	AdditionalProperties json.RawMessage   `json:"additionalProperties"`
	OneOf                []schema          `json:"oneOf"`
}

// validate checks one payload against its component, as a client's schema
// validation would: required members, types, enums, patterns and bounds.
func validate(t *testing.T, doc document, component string, payload []byte) {
	t.Helper()
	root, ok := doc.Components.Schemas[component]
	if !ok {
		t.Fatalf("component %s is not in the document", component)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	// Numbers are read exactly: an int64 maximum must not lose precision.
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("%s: payload is not JSON: %v", component, err)
	}
	checkSchema(t, doc, component, root, value)
}

func checkSchema(t *testing.T, doc document, path string, s schema, value any) {
	t.Helper()
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		checkSchema(t, doc, name, doc.Components.Schemas[name], value)
		return
	}
	switch s.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: %T is not an object", path, value)
		}
		for _, name := range s.Required {
			if _, present := object[name]; !present {
				t.Fatalf("%s: required member %q missing in %s", path, name, value)
			}
		}
		for name, member := range object {
			property, declared := s.Properties[name]
			if !declared {
				additional := additionalSchema(t, s.AdditionalProperties)
				if additional == nil {
					t.Fatalf("%s: member %q is not in the component", path, name)
				}
				property = *additional
			}
			checkSchema(t, doc, path+"."+name, property, member)
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			t.Fatalf("%s: %T is not an array", path, value)
		}
		for i, item := range items {
			checkSchema(t, doc, fmt.Sprintf("%s[%d]", path, i), *s.Items, item)
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			t.Fatalf("%s: %T is not a string", path, value)
		}
		if s.Pattern != "" && !regexp.MustCompile(s.Pattern).MatchString(text) {
			t.Fatalf("%s: %q does not match %q", path, text, s.Pattern)
		}
		if len(s.Enum) > 0 && !inEnum(s.Enum, text) {
			t.Fatalf("%s: %q is not one of %v", path, text, s.Enum)
		}
	case "integer":
		number, ok := value.(json.Number)
		integer, err := number.Int64()
		if !ok || err != nil {
			t.Fatalf("%s: %T is not an integer", path, value)
		}
		if s.Minimum != nil && float64(integer) < *s.Minimum {
			t.Fatalf("%s: %v is below the minimum %v", path, integer, *s.Minimum)
		}
	case "":
		if len(s.OneOf) > 0 {
			for _, option := range s.OneOf {
				if schemaAccepts(option, value) {
					return
				}
			}
			t.Fatalf("%s: %v matches none of the declared shapes", path, value)
		}
		// A schema without a type constrains nothing here.
	default:
		t.Fatalf("%s: unhandled schema type %q", path, s.Type)
	}
}

// additionalSchema reads an additionalProperties member: a schema, or the
// literal false that forbids undeclared members.
func additionalSchema(t *testing.T, raw json.RawMessage) *schema {
	t.Helper()
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "false" || trimmed == "null" {
		return nil
	}
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("additionalProperties: %v", err)
	}
	return &s
}

// schemaAccepts reports whether one option of a oneOf would accept value.
func schemaAccepts(s schema, value any) bool {
	switch s.Type {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	}
	return false
}

func inEnum(values []any, text string) bool {
	for _, value := range values {
		if value == text {
			return true
		}
	}
	return false
}

// normalise re-encodes a payload so member order and spacing do not matter.
func normalise(t *testing.T, payload []byte) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("payload: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return encoded
}

// fakeClock is one installation's clock for these proofs: advancing it is the
// only way a credential's expiry passes.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// readWithin reads one frame, failing the test when nothing arrives in d.
func readWithin(t *testing.T, s *sse, d time.Duration) (frame, error) {
	t.Helper()
	type result struct {
		frame frame
		err   error
	}
	done := make(chan result, 1)
	go func() {
		f, err := s.read()
		done <- result{f, err}
	}()
	select {
	case r := <-done:
		return r.frame, r.err
	case <-time.After(d):
		t.Fatalf("no frame within %s", d)
		return frame{}, errors.New("timeout")
	}
}

// expectClosed requires the stream to end before another frame: the read returns
// an error and never a frame.
func expectClosed(t *testing.T, s *sse, d time.Duration) {
	t.Helper()
	got, err := readWithin(t, s, d)
	if err == nil {
		t.Fatalf("the stream delivered a frame after its authority ended: %+v", got)
	}
}

// TestEventCurrentAuthority is the consumer side of the auth contract: the hub is
// bound to auth.Revalidate over the same persisted store, and a downgrade, disable,
// sign-out, key revocation or key-owner downgrade and expiry each end the affected
// stream before another frame, while the other authorized controls keep receiving.
func TestEventCurrentAuthority(t *testing.T) {
	clock := &fakeClock{now: time.Now().UTC()}
	w := newWorld(t, options{now: clock.Now, heartbeat: 40 * time.Millisecond})
	w.withViewer()
	w.withKey()

	open := func(cred string) *sse {
		stream := w.open(cred, "")
		if _, err := stream.read(); err != nil {
			t.Fatalf("%s hello: %v", cred, err)
		}
		return stream
	}
	viewer, admin, key, bypass := open("viewer"), open("admin"), open("admin-key"), open("bypass")
	defer viewer.close()
	defer admin.close()
	defer key.close()
	defer bypass.close()
	receive := func(s *sse, want int64) {
		t.Helper()
		f, err := readWithin(t, s, 5*time.Second)
		if err != nil {
			t.Fatalf("frame for job %d: %v", want, err)
		}
		if got := jobID(t, f); got != want {
			t.Fatalf("frame job = %d, want %d", got, want)
		}
	}

	// Every accepted provenance reaches a job frame.
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(1))})
	for _, s := range []*sse{viewer, admin, key, bypass} {
		receive(s, 1)
	}

	// A real downgrade ends that viewer's stream before its next frame.
	if _, err := auth.UpdateUser(t.Context(), w.db, w.viewer.User.ID, auth.UserPatch{ExpectedRevision: w.viewer.User.Revision, Role: ptr(auth.RoleMember)}); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(2))})
	expectClosed(t, viewer, 5*time.Second)
	for _, s := range []*sse{admin, key, bypass} {
		receive(s, 2)
	}

	// Disable ends an idle stream without any event at all: the heartbeat's
	// unconditional re-read decides it.
	w.disabled = w.user("disabled", auth.RoleViewer)
	idle := w.open("disabled", "")
	if _, err := idle.read(); err != nil {
		t.Fatalf("disabled hello: %v", err)
	}
	defer idle.close()
	if _, err := auth.UpdateUser(t.Context(), w.db, w.disabled.User.ID, auth.UserPatch{ExpectedRevision: w.disabled.User.Revision, Disabled: ptr(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	expectClosed(t, idle, 5*time.Second)

	// Revoking the key ends its stream; the trusted-local bypass keeps receiving.
	if err := w.keys.Revoke(t.Context(), w.keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(3))})
	expectClosed(t, key, 5*time.Second)
	receive(admin, 3)
	receive(bypass, 3)

	// Signing out deletes the session row, so the cookie stream ends too.
	if err := w.sessions.Logout(t.Context(), w.admin); err != nil {
		t.Fatalf("logout: %v", err)
	}
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(4))})
	expectClosed(t, admin, 5*time.Second)
	receive(bypass, 4)

	// A key's stream ends when its own user is downgraded: the current role
	// intersection no longer holds the rights the key was accepted with.
	keyed := w.user("keyed", auth.RoleAdmin)
	_, keyedToken, err := w.keys.Create(t.Context(), "keyed", "test", "read")
	if err != nil {
		t.Fatalf("keyed key: %v", err)
	}
	keyedSession, err := w.keys.Authenticate(t.Context(), keyedToken)
	if err != nil {
		t.Fatalf("authenticate the keyed key: %v", err)
	}
	w.named["keyed-key"] = keyedSession
	keyedStream := open("keyed-key")
	defer keyedStream.close()
	if _, err := auth.UpdateUser(t.Context(), w.db, keyed.User.ID, auth.UserPatch{ExpectedRevision: keyed.User.Revision, Role: ptr(auth.RoleMember)}); err != nil {
		t.Fatalf("keyed downgrade: %v", err)
	}
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(5))})
	expectClosed(t, keyedStream, 5*time.Second)
	receive(bypass, 5)

	// An idle stream whose credential expires is closed by the stream's own
	// decision: the clock passes the accepted expiry while the stream sits with
	// nothing queued, and the heartbeat's unconditional re-read ends it. The
	// control key holds no expiry, so it keeps receiving: the close is this
	// credential's expiry, not the hub ending every stream.
	w.named["expiring"] = w.user("expiring", auth.RoleAdmin)
	_, controlToken, err := w.keys.Create(t.Context(), "expiring", "expiry-control", "read")
	if err != nil {
		t.Fatalf("control key: %v", err)
	}
	controlSession, err := w.keys.Authenticate(t.Context(), controlToken)
	if err != nil {
		t.Fatalf("authenticate the control key: %v", err)
	}
	w.named["expiry-control"] = controlSession
	expiring, control := open("expiring"), open("expiry-control")
	defer expiring.close()
	defer control.close()
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(6))})
	receive(expiring, 6)
	receive(control, 6)
	clock.advance(auth.SessionTTL + time.Hour)
	expectClosed(t, expiring, 5*time.Second)
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(7))})
	receive(control, 7)

	// A credential already past its expiry when the stream is opened is refused at
	// the first decision: the clock is beyond the accepted expiry before the
	// request, so not even the hello frame is written.
	w.named["stale"] = w.user("stale", auth.RoleAdmin)
	stale := w.open("stale", "")
	defer stale.close()
	expectClosed(t, stale, 5*time.Second)

	// A peer only in the trusted-proxy list is never granted the bypass: no ref
	// is minted for it, so no stream is opened.
	if code, _, body := w.status("trusted-proxy-bypass", ""); code != http.StatusUnauthorized || !strings.Contains(body, "unauthenticated") {
		t.Fatalf("trusted-proxy bypass attempt = %d %s", code, body)
	}
}

func ptr[T any](v T) *T { return &v }

// TestEventReplayStopsAtAuthority proves a replay is delivered under the same
// decision as a live frame: a cursor inside the ring is queued at admission, and
// when the grant ends while those frames are still being written the stream stops
// and the frames it had not written yet are never sent.
func TestEventReplayStopsAtAuthority(t *testing.T) {
	// Admit reads the sequence, then Serve decides once before the head, once for
	// the hello and once per replayed frame: the fourth decision is the second
	// replayed frame, so the grant ends mid-replay.
	w := newWorld(t, options{revoke: func(call int) bool { return call > 3 }})
	for i := 1; i <= 4; i++ {
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(int64(i)))})
	}
	// A cursor at sequence 1 queues the frames of sequences 2, 3 and 4 for replay.
	stream := w.open("admin", "1.boot-events.1")
	defer stream.close()
	hello, err := stream.read()
	if err != nil || hello.event != "hello" {
		t.Fatalf("hello = %q %v", hello.event, err)
	}
	first, err := stream.read()
	if err != nil {
		t.Fatalf("the first replayed frame: %v", err)
	}
	if got := jobID(t, first); got != 2 {
		t.Fatalf("the first replayed frame = job %d, want 2", got)
	}
	// The rest of the replay is refused: the grant that authorized it ended, so no
	// further frame reaches the wire and the stream closes.
	expectClosed(t, stream, 5*time.Second)
	if record := w.logs.find("stream_closed"); record == "" || !strings.Contains(record, "credential_invalid") {
		t.Fatalf("the refused replay was not recorded: %q", record)
	}
}

// TestEventReplayStopsWhenHalted proves a stopped stream ends its replay: a halt
// that lands while the hello write is held closes the stream and writes none of
// the queued replay frames, so a drain does not rest on the per-frame write bound.
func TestEventReplayStopsWhenHalted(t *testing.T) {
	w := newWorld(t, options{})
	for i := 1; i <= 3; i++ {
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(int64(i)))})
	}
	addr, gated, openGate := gatedStream(t, w)
	defer openGate()
	conn := blockedPeer(t, addr, gated, "admin", "Last-Event-ID", fmt.Sprintf("%d.boot-events.0", w.admin.RestoreEpoch), "Connection", "close")
	// The hello write is held, so the replay has not begun: the halt is recorded
	// before the stream's goroutine reaches the frames it queued.
	w.hub.PrincipalChanged(w.admin.User.ID)
	openGate()

	stream := readHead(t, conn)
	if hello, err := stream.read(); err != nil || hello.event != "hello" {
		t.Fatalf("hello: %q %v", hello.event, err)
	}
	// The connection closes with the halted response, and no replayed frame is
	// written before it does.
	expectClosed(t, stream, 5*time.Second)
}

// gatedConn wraps one server-side connection: a request carrying the blocked
// marker makes every response write wait for a release, so a peer that stops
// reading is visibly stuck and each of its real progress events is stamped.
type gatedConn struct {
	net.Conn
	stamps     *stampLog
	gate       chan struct{}
	expired    chan struct{}
	closed     chan struct{}
	closedOnce sync.Once
	once       sync.Once
	scan       []byte
	blocked    atomicBool
	released   atomicBool
	waiting    atomicBool
}

type atomicBool struct {
	mu sync.Mutex
	on bool
}

func (b *atomicBool) set() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.on
	b.on = true
	return !was
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.on
}

func (c *gatedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.blocked.get() {
		c.scan = append(c.scan, p[:n]...)
		if bytes.Contains(c.scan, []byte("X-Test-Blocked: 1")) {
			c.blocked.set()
		}
		if len(c.scan) > 4096 {
			c.scan = nil
		}
	}
	return n, err
}

func (c *gatedConn) Write(p []byte) (int, error) {
	if !c.blocked.get() {
		return c.Conn.Write(p)
	}
	if c.released.get() {
		return c.Conn.Write(p)
	}
	// A held write is the stream's own first response write, which happens only
	// after its admission: a proof that waits for this one knows the stream is
	// subscribed and every later frame is its queue's.
	c.waiting.set()
	select {
	case <-c.gate:
	case <-c.expired:
	case <-c.closed:
	}
	c.once.Do(func() {
		c.released.set()
		if c.stamps != nil {
			c.stamps.release()
		}
	})
	return c.Conn.Write(p)
}

// Close ends the connection and releases a held write. The server closes it too
// when the handler returns, so the channel is closed exactly once however many
// callers get here.
func (c *gatedConn) Close() error {
	c.closedOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// SetWriteDeadline records nothing: the proofs' fake clock decides release, and
// the underlying connection keeps the real bound.
func (c *gatedConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(t) }

// gateListener wraps every accepted connection so one marked peer's writes are
// observable.
type gateListener struct {
	net.Listener
	stamps *stampLog
	gate   chan struct{}
	mu     sync.Mutex
	expire []chan struct{}
	conns  []*gatedConn
}

func (l *gateListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &gatedConn{Conn: conn, stamps: l.stamps, gate: l.gate, expired: make(chan struct{}), closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, wrapped)
	l.expire = append(l.expire, wrapped.expired)
	l.mu.Unlock()
	return wrapped, nil
}

// held reports the marked peers whose stream has already tried to write: each is
// admitted, because the first response write follows admission.
func (l *gateListener) held() []*gatedConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*gatedConn
	for _, conn := range l.conns {
		if conn.waiting.get() {
			out = append(out, conn)
		}
	}
	return out
}

// expireAll is the fake clock's deadline release for every waiting write.
func (l *gateListener) expireAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, channel := range l.expire {
		close(channel)
	}
	l.expire = nil
}

// disconnectAll closes every server-side connection: the peer went away.
func (l *gateListener) disconnectAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}

// stampLog is one monotonic sequence of the writer's successes, the frames it
// published and the blocked peers' real progress, so "the writer advanced while
// the peers were stalled" is decidable.
type stampLog struct {
	mu           sync.Mutex
	n            int64
	success      int64
	firstRelease int64
	before       int64     // frames published before the peers' first progress
	bound        time.Time // when the peers' queues were offered the frame past their bound
}

func (s *stampLog) next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.n
}

func (s *stampLog) ok() int64 {
	stamp := s.next()
	s.mu.Lock()
	s.success = max(s.success, stamp)
	s.mu.Unlock()
	return stamp
}

// published records one frame the writer handed the hub, while the blocked peers
// have made no progress at all, and reports whether their queues have now been
// offered the frame past their bound. The runtime publishes from several
// goroutines, so the count, the time it reached the bound and the decision are one
// step under the one lock.
func (s *stampLog) published() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstRelease == 0 {
		s.before++
	}
	if s.before == queueFrames+1 {
		if s.bound.IsZero() {
			s.bound = time.Now()
		}
		return true
	}
	return false
}

func (s *stampLog) release() int64 {
	stamp := s.next()
	s.mu.Lock()
	if s.firstRelease == 0 || stamp < s.firstRelease {
		s.firstRelease = stamp
	}
	s.mu.Unlock()
	return stamp
}

// boundAt is when the peers' queues were offered the frame past their bound, and
// zero when a writer that waited for the peer's own progress never got that far.
// The count beside it is what the non-blocking claim asserts; this is the moment
// the passing run logs.
func (s *stampLog) boundAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound
}

// totals reads the stamps under the one lock.
func (s *stampLog) totals() (success, release, before int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.success, s.firstRelease, s.before
}

// releaseStamped waits until a blocked peer's first real progress is stamped. The
// stamp is recorded on that peer's own write goroutine, after the run released it,
// so a run that reads it waits rather than calling an unstamped zero an absence.
// It is the observation the control's classification and the runs that release
// their peer after the batch both rest on.
func releaseStamped(t *testing.T, stamps *stampLog, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, first, _ := stamps.totals(); first != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the peer's release was never stamped", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestEventStreamCaps proves the recorded stream bounds: a fifth stream of one
// user, a fifth stream of one direct peer and a stream asked for after the hub
// closed each get their route's refusal instead of a frame, while another account
// still streams.
func TestEventStreamCaps(t *testing.T) {
	w := newWorld(t, options{})
	opened := make([]*sse, 0, 4)
	for range 4 {
		stream := w.open("admin", "")
		if _, err := stream.read(); err != nil {
			t.Fatalf("hello: %v", err)
		}
		opened = append(opened, stream)
	}
	defer func() {
		for _, stream := range opened {
			stream.close()
		}
	}()
	if status, retry, body := w.status("admin", ""); status != http.StatusTooManyRequests || retry != "5" || !strings.Contains(body, "stream_cap") {
		t.Fatalf("the fifth stream = %d retry=%q %s", status, retry, body)
	}
	// Another account still streams: the cap counts a user, not an installation.
	w.named["other"] = w.user("other", auth.RoleAdmin)
	other := w.open("other", "")
	if _, err := other.read(); err != nil {
		t.Fatalf("the other account's control: %v", err)
	}
	other.close()

	// The direct-peer grant is one credential whatever port its connection uses:
	// reconnecting from a new ephemeral port does not multiply its cap.
	var peers []*sse
	for i := range 4 {
		stream := w.open(fmt.Sprintf("bypass:%d", 41000+i), "")
		if _, err := stream.read(); err != nil {
			t.Fatalf("direct-peer stream %d: %v", i, err)
		}
		peers = append(peers, stream)
	}
	defer func() {
		for _, stream := range peers {
			stream.close()
		}
	}()
	if status, _, body := w.streamRefusal("bypass:41004"); status != http.StatusTooManyRequests || !strings.Contains(body, "stream_cap") {
		t.Fatalf("a fifth direct-peer stream on a new port = %d %s", status, body)
	}

	// After the drain closes the hub, a new stream is refused 503 draining
	// rather than served.
	w.hub.Close()
	if status, _, body := w.status("admin", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "draining") {
		t.Fatalf("a stream after the drain = %d %s", status, body)
	}
}

// TestEventStreamCapPerUser proves the per-credential bound counts the account,
// not the login: two sessions of one user share one cap, so a second sign-in
// cannot multiply the open streams one user holds.
func TestEventStreamCapPerUser(t *testing.T) {
	w := newWorld(t, options{})
	w.named["shared-a"] = w.user("shared", auth.RoleAdmin)
	second, _, _, err := w.sessions.Login(t.Context(), "shared", testPassword)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	w.named["shared-b"] = second
	var opened []*sse
	defer func() {
		for _, stream := range opened {
			stream.close()
		}
	}()
	for _, cred := range []string{"shared-a", "shared-a", "shared-b", "shared-b"} {
		stream := w.open(cred, "")
		if _, err := stream.read(); err != nil {
			t.Fatalf("%s hello: %v", cred, err)
		}
		opened = append(opened, stream)
	}
	if status, retry, body := w.streamRefusal("shared-b"); status != http.StatusTooManyRequests || retry != "5" || !strings.Contains(body, "stream_cap") {
		t.Fatalf("a fifth stream across the account's two sessions = %d retry=%q %s", status, retry, body)
	}
}

// TestEventGlobalStreamCap proves the hub's installation-wide bound: with the
// recorded number of streams open, one more is refused 429 stream_cap whatever
// credential asks, and closing a stream returns the capacity so a control stream
// on a fresh credential opens.
func TestEventGlobalStreamCap(t *testing.T) {
	w := newWorld(t, options{})
	const capGlobal = 64
	opened := make([]*sse, 0, capGlobal)
	defer func() {
		for _, stream := range opened {
			stream.close()
		}
	}()
	// Four streams for each of sixteen accounts: every account is inside its own
	// cap, so only the installation-wide one can refuse the next.
	for i := range capGlobal / 4 {
		name := fmt.Sprintf("cap-%d", i)
		w.named[name] = w.user(name, auth.RoleAdmin)
		for range 4 {
			stream := w.open(name, "")
			if _, err := stream.read(); err != nil {
				t.Fatalf("stream of %s: %v", name, err)
			}
			opened = append(opened, stream)
		}
	}
	if status, retry, body := w.streamRefusal("admin"); status != http.StatusTooManyRequests || retry != "5" || !strings.Contains(body, "stream_cap") {
		t.Fatalf("a stream after %d open = %d retry=%q %s", capGlobal, status, retry, body)
	}
	// Closing one returns its capacity: a fresh credential's control stream opens.
	opened[0].close()
	control := w.openWhenRoom("bypass")
	control.close()
}

// openWhenRoom opens one stream on cred once the hub has capacity again, so a
// freed stream's capacity is what the caller observes rather than a fixed wait.
func (w *world) openWhenRoom(cred string) *sse {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		request, err := http.NewRequestWithContext(w.t.Context(), http.MethodGet, w.server.URL+"/api/v1/events", nil)
		if err != nil {
			w.t.Fatalf("request: %v", err)
		}
		request.Header.Set("X-Test-Cred", cred)
		request.Header.Set("Accept-Encoding", "identity")
		resp, err := w.client.Do(request)
		if err != nil {
			w.t.Fatalf("open %s: %v", cred, err)
		}
		if resp.StatusCode == http.StatusOK {
			return &sse{t: w.t, resp: resp, reader: bufio.NewReaderSize(resp.Body, 1<<16)}
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			w.t.Fatalf("a control stream after freeing capacity = %d %s", resp.StatusCode, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEventFanoutBounded is the overload contract: 50 events a second reach ten
// healthy readers completely and in order while two non-reading peers overflow,
// and the committed writer is never delayed by either of them. The transitions
// are the real runtime's own, wired to the hub exactly as the composition wires
// it; the blocked peer's server-side connection makes every response write wait
// for one release, which each run supplies by gate drain, peer disconnect or a
// fake-clock deadline.
func TestEventFanoutBounded(t *testing.T) {
	const healthyReaders = 10
	for _, tc := range []struct {
		name     string
		events   int
		release  string
		blocking bool
	}{
		{"gate drain", 500, "gate", false},
		{"peer disconnect", 300, "disconnect", false},
		{"fake-clock deadline", 300, "deadline", false},
		{"a publisher waiting for the slow peer is refused", 300, "gate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runFanout(t, tc.events, healthyReaders, tc.release, tc.blocking)
		})
	}
}

func runFanout(t *testing.T, transitions, healthyReaders int, release string, blockingPublisher bool) {
	t.Helper()
	ctx := t.Context()
	w := newWorld(t, options{})
	stamps := &stampLog{}
	gate := make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }

	// The real runtime over the real store, publishing through the hub the way
	// the composition does. Every published frame is one the blocked peers' queues
	// are offered, so the queue's recorded bound trips at the 257th: from there a
	// gate run releases its peers, and the resync that overflow announced is
	// delivered inside the frame deadline the stalled write is still inside.
	released := make(chan struct{})
	var releaseOnce sync.Once
	releasePublisher := func() { releaseOnce.Do(func() { close(released) }) }
	defer releasePublisher()
	// The hub's published sequence is counted here, at the one call site every
	// frame goes through: the last value is the last sequence a reader must
	// reach, and a transition that published a third frame shows in the count.
	var publishedMu sync.Mutex
	published := 0
	publishedCount := func() int {
		publishedMu.Lock()
		defer publishedMu.Unlock()
		return published
	}
	rt, err := jobs.New(w.db, jobs.Config{
		Boot: "boot-fanout", BackgroundSlots: 4,
		Log: func(string, ...any) {},
		Publish: func(ev jobs.Event) {
			if blockingPublisher {
				// The failure control: a publisher that waits for the slow peer's
				// real progress before every frame it owes, the claim as well as
				// the finish, since one transition now carries both.
				<-released
			}
			if ev.State == store.JobSucceeded {
				stamps.ok()
			}
			w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(ev)})
			publishedMu.Lock()
			published++
			publishedMu.Unlock()
			if past := stamps.published(); release == "gate" && past {
				openGate()
			}
		},
	}, []jobs.Handler{{
		Kind: "events.fanout", Class: jobs.ClassOptional, Lane: jobs.LaneBackground,
		MaxAttempts: 1, Deadline: 10 * time.Second,
		Run: func(context.Context, *jobs.Attempt) jobs.Outcome {
			return jobs.Outcome{Result: jobs.Succeeded, Reason: "completed"}
		},
	}})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	}()

	// Every viewer is a distinct account: the cap is per user, and the event load
	// is per subscriber.
	for i := range healthyReaders + 2 {
		w.named[fmt.Sprintf("fanout-%d", i)] = w.user(fmt.Sprintf("fanout-%d", i), auth.RoleAdmin)
	}
	for i := range healthyReaders {
		w.named[fmt.Sprintf("healthy-%d", i)] = w.named[fmt.Sprintf("fanout-%d", i)]
	}

	// The server with its gated connections.
	mux := http.NewServeMux()
	api.New(mux, &eventService{hub: w.hub})
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p, ref, ok := w.credential(r.Header.Get("X-Test-Cred"))
		if ok {
			r = r.WithContext(auth.WithCredentialRef(auth.WithPrincipal(r.Context(), p), ref))
		}
		mux.ServeHTTP(rw, r)
	})
	server := httptest.NewUnstartedServer(handler)
	gated := &gateListener{Listener: server.Listener, stamps: stamps, gate: gate}
	server.Listener = gated
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		openGate()
		gated.disconnectAll()
		releasePublisher()
	})

	// The two blocked peers connect first and do not read: their first response
	// write waits on the release, so the request is sent by hand and nothing is
	// read from the peer until this run releases it.
	var blockedConns []net.Conn
	for i := range 2 {
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("blocked dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		blockedConns = append(blockedConns, conn)
		if _, err := fmt.Fprintf(conn, "GET /api/v1/events HTTP/1.1\r\nHost: %s\r\nX-Test-Cred: fanout-%d\r\nX-Test-Blocked: 1\r\n\r\n", server.Listener.Addr().String(), i); err != nil {
			t.Fatalf("blocked request: %v", err)
		}
	}
	// The peer is subscribed once its first response write is waiting, and that
	// write is what this run holds: from there the queue's own bound is what its
	// delivery shows. held is when both writes were confirmed waiting, from about
	// when the recorded frame deadline each of them carries is armed.
	deadline := time.Now().Add(10 * time.Second)
	for len(gated.held()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the blocked peers were never admitted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	held := time.Now()
	// Each peer is read from now on by its own goroutine: a peer released mid-batch
	// must be read while its stream writes, or its socket would fill and the write
	// deadline would close it instead of delivering its resync. What it delivered
	// is asserted once the batch is over.
	deliveries := make(chan blockedReading, len(blockedConns))
	if !blockingPublisher {
		for _, conn := range blockedConns {
			go func(c net.Conn) { deliveries <- readBlocked(t, c) }(conn)
		}
	}

	// Ten healthy readers, each reading every frame in the sequence its own id
	// carries: one committed transition publishes a claim and a finish frame, so a
	// reader takes two frames per transition. The control's writer waits for the
	// slow peer, which publishes its successes in whatever order its goroutines
	// are released: job-ID order is not a claim in that run, but the hub's
	// sequence is, in every run. A reader reads until the marker published after
	// the whole batch — the hub's last sequence — so a transition that published a
	// third frame is counted rather than left unread.
	type reading struct {
		err   error
		frame int
	}
	readings := make(chan reading, healthyReaders)
	for i := range healthyReaders {
		stream := w.openAt(server.URL, fmt.Sprintf("healthy-%d", i), "")
		hello, err := stream.read()
		if err != nil {
			t.Fatalf("healthy hello: %v", err)
		}
		last, ok := frameSeq(hello.id)
		if !ok {
			t.Fatalf("healthy hello id %q", hello.id)
		}
		t.Cleanup(stream.close)
		go func(s *sse) {
			for seen := 0; ; seen++ {
				f, err := s.read()
				if err != nil {
					readings <- reading{err: err, frame: seen}
					return
				}
				seq, ok := frameSeq(f.id)
				if !ok {
					readings <- reading{err: fmt.Errorf("frame %d id %q", seen, f.id), frame: seen}
					return
				}
				if seq != last+1 {
					readings <- reading{err: fmt.Errorf("frame %d has sequence %d after %d", seen, seq, last), frame: seen}
					return
				}
				last = seq
				if f.event == events.NameSystem {
					readings <- reading{frame: seen}
					return
				}
			}
		}(stream)
	}

	// Start the runtime, then drive the committed transitions at 50/s.
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime start: %v", err)
	}
	pacer := time.NewTicker(20 * time.Millisecond)
	defer pacer.Stop()
	for i := range transitions {
		key := fmt.Sprintf("fanout-%d", i)
		if err := w.db.Write(ctx, func(tx *sql.Tx) error {
			_, _, err := store.EnqueueJob(ctx, tx, store.JobIntent{
				Kind: "events.fanout", Class: store.JobOptional, Lane: store.JobBackground,
				DedupeKey: key, ConcurrencyKey: key, MaxAttempts: 1, DueAt: time.Now().Unix(),
			})
			return err
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		// The committed row is what wakes the runtime, as the accepting path does:
		// without the wake the batch would wait for the next recorded sweep.
		rt.Wake()
		<-pacer.C
	}
	if blockingPublisher {
		// The control: the publisher waits for the slow peer, so the peer is
		// released first and its progress stamped before the publisher may stamp or
		// publish anything.
		openGate()
		releaseStamped(t, stamps, "the blocking publisher's control")
		releasePublisher()
	}
	// A committed revision of every job is what a separate read sees.
	var succeeded int
	committedBy := time.Now().Add(30 * time.Second)
	for {
		if err := w.db.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM job WHERE kind = 'events.fanout' AND state = 'succeeded'`).Scan(&succeeded)
		}); err != nil {
			t.Fatalf("count succeeded rows: %v", err)
		}
		if succeeded == transitions {
			break
		}
		if time.Now().After(committedBy) {
			t.Fatalf("succeeded rows = %d, want %d", succeeded, transitions)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Every committed transition published its frames. The marker published after
	// them is the hub's last sequence, so each reader reads to the end of the
	// stream's sequence instead of stopping at an assumed frame count.
	publishedBy := time.Now().Add(30 * time.Second)
	for publishedCount() < 2*transitions {
		if time.Now().After(publishedBy) {
			t.Fatalf("published frames = %d, want at least two per committed transition", publishedCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.hub.Publish(events.Event{Name: events.NameSystem, Payload: struct{}{}})

	// Every committed transition is published, and the reader sees each one.
	for range healthyReaders {
		result := <-readings
		if result.err != nil {
			t.Fatalf("healthy reader stopped at frame %d: %v", result.frame, result.err)
		}
		if result.frame != 2*transitions {
			t.Fatalf("healthy reader took %d job frames, want two per committed transition (%d)", result.frame, 2*transitions)
		}
	}

	// Release the blocked peers by this run's own mechanism. The gate was already
	// opened when the queues' bound tripped; the runs that take the peer away or
	// leave its stalled write to the frame deadline apply theirs here, which is what
	// their names assert.
	switch release {
	case "gate":
		openGate()
	case "disconnect":
		gated.disconnectAll()
	case "deadline":
		// The stalled write is left past its own recorded 10 s bound, so the stream
		// ends by that bound: the peers were admitted together and their first write
		// followed, so waiting past the bound from here reaches it.
		if wait := frameDeadline + time.Second - time.Since(held); wait > 0 {
			time.Sleep(wait)
		}
		gated.expireAll()
	}
	if !blockingPublisher && release != "gate" {
		// This run releases its peer after the whole batch, so the release stamp is
		// the one the oracle below reads.
		releaseStamped(t, stamps, "release="+release)
	}
	if blockingPublisher {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if success, _, _ := stamps.totals(); success >= int64(transitions) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the blocking publisher never reported its transitions")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Each blocked peer's own outcome: the resync its overflow announced, at the
	// last frame it accepted, when the gate released it inside the frame deadline;
	// or the end this run's own mechanism gave it, which is the peer gone or its
	// stalled write left past the deadline.
	if !blockingPublisher {
		for range blockedConns {
			select {
			case delivery := <-deliveries:
				if release == "gate" {
					expectOverflow(t, delivery)
					continue
				}
				expectEnded(t, delivery, release)
			case <-time.After(30 * time.Second):
				t.Fatalf("a blocked peer's delivery never ended (release=%s)", release)
			}
		}
	}

	successes, firstRelease, beforeProgress := stamps.totals()
	bound := stamps.boundAt()
	if blockingPublisher {
		// The control's own classification: its publisher waits for the slow peer's
		// real progress, so it publishes nothing at all before that progress is
		// stamped. A classifier that called a blocked publisher unblocked would read
		// a count above zero here.
		if beforeProgress != 0 {
			t.Fatalf("the blocking publisher published %d frames before the slow peer's first progress, want none", beforeProgress)
		}
		t.Logf("FANOUT-CONTROL result=blocked success=%d release=%d published=%d", successes, firstRelease, beforeProgress)
		return
	}
	// The discriminating observation: a whole queue bound of frames was published
	// while the peers had made no progress at all. A writer that waited for a peer's
	// progress publishes none of them, so it could never reach the bound first, and
	// the count is the run's own rather than the wall clock's.
	if beforeProgress < queueFrames+1 {
		t.Fatalf("the writer published %d frames before the peers' first progress, want a whole queue bound (%d) with none", beforeProgress, queueFrames+1)
	}
	if release != "gate" {
		// The recorded oracle where it applies: this run releases its peer after the
		// batch, so the last success stamp precedes the first release stamp. The gate
		// run releases at the queue's bound, inside the batch, and its claim is the
		// count above.
		if successes >= firstRelease {
			t.Fatalf("the last success stamp %d does not precede the first release stamp %d", successes, firstRelease)
		}
	}
	elapsed := bound.Sub(held)
	t.Logf("FANOUT-PASS events=%d healthy=%d blocked=2 success=%d release_first=%d published=%d bound_after=%s", transitions, healthyReaders, successes, firstRelease, beforeProgress, elapsed.Round(time.Millisecond))
}

// queueFrames and frameDeadline are the recorded per-subscriber bounds: the frame
// an overflow proof publishes past the first is the one the queue refuses, and the
// second is how long one frame's write may take before its stream ends.
const (
	queueFrames   = 256
	frameDeadline = 10 * time.Second
)

// blockedReading is one blocked peer's whole delivery: the job frames it was sent
// in their own sequence, the frame that announced its overflow, or the end its
// connection reached instead.
type blockedReading struct {
	frames int                 // job frames delivered, each the next in sequence
	last   int64               // the sequence the last delivered frame carried
	resync *events.EventResync // the announced refusal, when one was delivered
	closed bool                // whether the stream ended after that refusal
	ended  error               // how the delivery ended, nil when the refusal ended it
}

// readBlocked reads one blocked peer's response to its end. It runs on the peer's
// own goroutine, so it returns what it found instead of failing the proof here: a
// stalled peer released mid-batch must be read while its stream writes, or its
// socket would fill and the write deadline would close it again.
func readBlocked(t *testing.T, conn net.Conn) blockedReading {
	t.Helper()
	var r blockedReading
	reader := bufio.NewReaderSize(conn, 1<<16)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			r.ended = err
			return r
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	stream := &sse{t: t, reader: reader}
	hello, err := stream.read()
	if err != nil {
		r.ended = err
		return r
	}
	if hello.event != "hello" {
		r.ended = fmt.Errorf("first frame is %q, want hello", hello.event)
		return r
	}
	mark, ok := frameSeq(hello.id)
	if !ok {
		r.ended = fmt.Errorf("hello id %q", hello.id)
		return r
	}
	for {
		next, err := stream.read()
		if err != nil {
			r.ended = err
			return r
		}
		switch next.event {
		case "job":
			seq, ok := frameSeq(next.id)
			if !ok || seq != mark+1 {
				r.ended = fmt.Errorf("frame %q follows sequence %d: the peer's frames are not consecutive", next.id, mark)
				return r
			}
			mark = seq
			r.frames++
			r.last = seq
		case "resync":
			var body events.EventResync
			if err := json.Unmarshal(next.data, &body); err != nil {
				r.ended = fmt.Errorf("resync payload: %w", err)
				return r
			}
			r.resync = &body
			// The refusal is the stream's last write: the response's terminal chunk
			// follows it and then nothing, so a stream that kept delivering would
			// hand over another frame line instead.
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				r.ended = fmt.Errorf("resync read bound: %w", err)
				return r
			}
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if streamEnded(err) {
						r.closed = true
						return r
					}
					r.ended = fmt.Errorf("the resync was not the response's last write: %w", err)
					return r
				}
				switch {
				case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "data:"):
					r.ended = fmt.Errorf("frame line %q followed the overflow resync", strings.TrimSpace(line))
					return r
				case strings.TrimSpace(line) == "0":
					r.closed = true
					return r
				}
			}
		default:
			r.ended = fmt.Errorf("frame %q in the blocked peer's stream", next.event)
			return r
		}
	}
}

// frameSeq is the sequence one frame's own id carries, in the recorded
// `<restoreEpoch>.<boot>.<seq>` form.
func frameSeq(id string) (int64, bool) {
	_, rest, ok := strings.Cut(id, ".")
	if !ok {
		return 0, false
	}
	_, seqPart, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseInt(seqPart, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// streamEnded reports that the peer's delivery ended because its connection did,
// not because this proof's own read deadline ran out.
func streamEnded(err error) bool {
	var netErr net.Error
	return err != nil && (!errors.As(err, &netErr) || !netErr.Timeout())
}

// expectOverflow requires the peer this run released inside the frame deadline to
// have received the resync its queue's overflow announced, at the last frame it
// accepted, and then been closed: a refusal that kept delivering would be an
// announcement a client could not trust.
func expectOverflow(t *testing.T, r blockedReading) {
	t.Helper()
	if r.resync == nil {
		t.Fatalf("the released peer ended with %v after %d frames, want the resync its overflow announced", r.ended, r.frames)
	}
	if r.resync.Reason != "overflow" {
		t.Fatalf("the released peer's resync = %+v, want the queue's overflow", *r.resync)
	}
	if r.resync.Watermark != r.last {
		t.Fatalf("the recovery watermark %d is not the last frame the peer accepted %d", r.resync.Watermark, r.last)
	}
	if r.frames != queueFrames {
		t.Fatalf("the peer accepted %d frames before its overflow, want its queue's whole bound %d", r.frames, queueFrames)
	}
	if !r.closed {
		t.Fatalf("the peer's stream did not end after its resync: %v", r.ended)
	}
	t.Logf("FANOUT-BLOCKED outcome=resync accepted=%d watermark=%d", r.frames, r.resync.Watermark)
}

// expectEnded requires the peer this run ended to have been ended by that run and
// not by a resync: its stalled write never completed, so not one frame reached it,
// and its connection is closed rather than still waiting.
func expectEnded(t *testing.T, r blockedReading, release string) {
	t.Helper()
	if r.resync != nil {
		t.Fatalf("a peer this run ended (%s) was announced a resync it could not be sent: %+v", release, *r.resync)
	}
	if r.frames != 0 {
		t.Fatalf("a peer this run ended (%s) delivered %d frames, want none: its stalled write never completed", release, r.frames)
	}
	if !streamEnded(r.ended) {
		t.Fatalf("a peer this run ended (%s) is still waiting: %v", release, r.ended)
	}
	t.Logf("FANOUT-BLOCKED outcome=closed release=%s", release)
}

// TestEventRightsFilterFrames proves a frame reaches only the subscribers whose
// accepted rights carry it: a reader with the status right alone never receives a
// users frame, live or from its cursor's replay.
func TestEventRightsFilterFrames(t *testing.T) {
	w := newWorld(t, options{})
	w.named["limited"] = w.user("limited", auth.RoleViewer)
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(1))})
	w.hub.Publish(events.Event{Name: events.NameUsers, Payload: api.EventUsers{CollectionRevision: 1}})
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(2))})

	// A users frame published after the hello is not this reader's to see, so the
	// next frame it receives is the job one behind it.
	live := w.open("limited", "")
	defer live.close()
	if hello, err := live.read(); err != nil || hello.event != "hello" {
		t.Fatalf("hello: %q %v", hello.event, err)
	}
	w.hub.Publish(events.Event{Name: events.NameUsers, Payload: api.EventUsers{CollectionRevision: 2}})
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(3))})
	next, err := live.read()
	if err != nil {
		t.Fatalf("live frame: %v", err)
	}
	if next.event != "job" || jobID(t, next) != 3 {
		t.Fatalf("the reader's live frame = %q %s, want the job frame alone", next.event, next.data)
	}

	// The same filter applies to a cursor's replay: the missed users frame is
	// skipped and the job frames follow in their sequence order.
	replay := w.open("limited", "1.boot-events.0")
	defer replay.close()
	if _, err := replay.read(); err != nil {
		t.Fatalf("replay hello: %v", err)
	}
	for want := int64(1); want <= 2; want++ {
		next, err := replay.read()
		if err != nil {
			t.Fatalf("replay frame %d: %v", want, err)
		}
		if next.event != "job" || jobID(t, next) != want {
			t.Fatalf("replay frame %q %s, want job %d", next.event, next.data, want)
		}
	}
}

// gatedStream serves this world's events operation behind a listener whose marked
// peer holds every response write until the returned release is called, so a peer
// that stops reading is visibly stuck while its queue fills.
func gatedStream(t *testing.T, w *world) (addr string, gate *gateListener, open func()) {
	t.Helper()
	released := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	api.New(mux, &eventService{hub: w.hub})
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p, ref, ok := w.credential(r.Header.Get("X-Test-Cred"))
		if ok {
			r = r.WithContext(auth.WithCredentialRef(auth.WithPrincipal(r.Context(), p), ref))
		}
		mux.ServeHTTP(rw, r)
	})
	server := httptest.NewUnstartedServer(handler)
	gated := &gateListener{Listener: server.Listener, gate: released}
	server.Listener = gated
	server.Start()
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), gated, func() { once.Do(func() { close(released) }) }
}

// blockedPeer sends one stream request by hand from a peer that never reads, and
// waits until the server's first write to it is held. Extra headers are sent
// before the blank line that ends the request.
func blockedPeer(t *testing.T, addr string, gated *gateListener, cred string, header ...string) net.Conn {
	t.Helper()
	held := len(gated.held())
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf("GET /api/v1/events HTTP/1.1\r\nHost: %s\r\nX-Test-Cred: %s\r\nX-Test-Blocked: 1\r\nAccept-Encoding: identity\r\n", addr, cred)
	for i := 0; i+1 < len(header); i += 2 {
		request += header[i] + ": " + header[i+1] + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		t.Fatalf("request: %v", err)
	}
	// The stream's own first write is what is held: the peer is subscribed, so
	// every frame published from here on is its queue's to accept or refuse.
	deadline := time.Now().Add(10 * time.Second)
	for len(gated.held()) <= held {
		if time.Now().After(deadline) {
			t.Fatal("the peer was never admitted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return conn
}

// readHead consumes one stream's response head and returns its frame reader.
func readHead(t *testing.T, conn net.Conn) *sse {
	t.Helper()
	reader := bufio.NewReaderSize(conn, 1<<16)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("response head: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			return &sse{t: t, reader: reader}
		}
	}
}

// TestEventQueueOverflowResync proves the queue's own overflow: a peer that stops
// reading overflows its bounded queue, and the resync it receives names the last
// frame it was sent, so its recovery cursor never falls behind a delivered frame.
// A frame published after the overflow is refused rather than accepted behind the
// announced watermark.
func TestEventQueueOverflowResync(t *testing.T) {
	w := newWorld(t, options{})
	addr, gated, openGate := gatedStream(t, w)
	defer openGate()
	conn := blockedPeer(t, addr, gated, "admin")
	// One frame more than the queue holds: the last accepted frame is its bound.
	for i := 1; i <= queueFrames+1; i++ {
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(int64(i)))})
	}
	openGate()

	stream := readHead(t, conn)
	if hello, err := stream.read(); err != nil || hello.event != "hello" {
		t.Fatalf("hello: %q %v", hello.event, err)
	}
	var last int64
	for want := int64(1); want <= 10; want++ {
		next, err := stream.read()
		if err != nil {
			t.Fatalf("frame %d: %v", want, err)
		}
		if last = jobID(t, next); last != want {
			t.Fatalf("frame job = %d, want %d (id=%q event=%q data=%.60s)", last, want, next.id, next.event, next.data)
		}
	}
	// Published while the queue is draining: behind the announced watermark.
	w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(9999))})
	for {
		next, err := stream.read()
		if err != nil {
			t.Fatalf("resync: %v", err)
		}
		if next.event == "resync" {
			var body events.EventResync
			if err := json.Unmarshal(next.data, &body); err != nil {
				t.Fatalf("resync payload: %v", err)
			}
			if body.Reason != "overflow" {
				t.Fatalf("resync = %+v, want the queue's overflow", body)
			}
			if body.Watermark < last {
				t.Fatalf("the recovery watermark %d is behind the delivered frame %d", body.Watermark, last)
			}
			t.Logf("QUEUE-OVERFLOW forced=%d last=%d watermark=%d", queueFrames+1, last, body.Watermark)
			return
		}
		if next.event != "job" {
			t.Fatalf("frame %q after the overflow", next.event)
		}
		last = jobID(t, next)
	}
}

// TestEventQueueBytesOverflow proves the queue's byte bound, not only its frame
// count: a blocked peer whose frames are large overflows on the 256 KiB of encoded
// frame bytes while far fewer than 256 frames are queued, and its resync names the
// last frame it accepted.
func TestEventQueueBytesOverflow(t *testing.T) {
	w := newWorld(t, options{})
	addr, gated, openGate := gatedStream(t, w)
	defer openGate()
	conn := blockedPeer(t, addr, gated, "admin")
	// Forty frames of about 7 KiB each: 256 KiB is reached after roughly 36 of
	// them, so the byte bound is the one that trips.
	const published = 40
	heavy := map[string]string{"blob": strings.Repeat("b", 7000)}
	for i := 1; i <= published; i++ {
		event := jobEvent(int64(i))
		event.Params = heavy
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(event)})
	}
	openGate()

	stream := readHead(t, conn)
	if hello, err := stream.read(); err != nil || hello.event != "hello" {
		t.Fatalf("hello: %q %v", hello.event, err)
	}
	var accepted int64
	for {
		next, err := readWithin(t, stream, 10*time.Second)
		if err != nil {
			t.Fatalf("no resync after %d accepted frames of %d published: %v", accepted, published, err)
		}
		if next.event == "resync" {
			var body events.EventResync
			if err := json.Unmarshal(next.data, &body); err != nil {
				t.Fatalf("resync payload: %v", err)
			}
			if body.Reason != "overflow" {
				t.Fatalf("resync = %+v, want the queue's overflow", body)
			}
			if body.Watermark != accepted {
				t.Fatalf("the recovery watermark %d is not the last accepted frame %d", body.Watermark, accepted)
			}
			if accepted >= published {
				t.Fatalf("the byte bound did not trip before the frame count: %d accepted", accepted)
			}
			t.Logf("QUEUE-BYTES published=%d accepted=%d watermark=%d", published, accepted, body.Watermark)
			return
		}
		if next.event != "job" {
			t.Fatalf("frame %q before the overflow", next.event)
		}
		accepted = jobID(t, next)
	}
}

// TestEventCloseBoundsTheDrain proves the drain is bounded: a reader that stopped
// reading holds its stream's write, so Close waits for the recorded drain bound
// and then stops what is left instead of holding the installation's stop open.
func TestEventCloseBoundsTheDrain(t *testing.T) {
	w := newWorld(t, options{})
	addr, gated, openGate := gatedStream(t, w)
	defer openGate()
	conn := blockedPeer(t, addr, gated, "admin")
	// A whole queue plus the frame that overflows it: the stream has nothing left
	// to accept, and its held write keeps it from draining on its own.
	for i := 1; i <= queueFrames+1; i++ {
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(int64(i)))})
	}
	started := time.Now()
	closed := make(chan struct{})
	go func() {
		w.hub.Close()
		close(closed)
	}()
	select {
	case <-closed:
		if elapsed := time.Since(started); elapsed > frameDeadline+time.Second {
			t.Fatalf("Close waited %v on a reader that stopped reading, want at most the drain bound", elapsed)
		}
		t.Logf("CLOSE-BOUND waited=%v", time.Since(started).Round(time.Millisecond))
	case <-time.After(frameDeadline + 5*time.Second):
		t.Fatal("Close waited past the drain bound on a reader that stopped reading")
	}
	// The held stream was stopped, not waited for: releasing its write lets it end,
	// and the connection is closed rather than left holding the queue.
	openGate()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("the stopped stream did not end: %v", err)
	}
}

// burstBounds is one publishing burst's sequence range: the last sequence before
// it started and the last one it published, so a subscriber's watermark can be
// placed inside it or outside it.
type burstBounds struct {
	first, last int64
}

// TestEventWatermarkRace proves the handshake under a racing writer: the writer
// publishes continuously while clients subscribe, so every subscription is made
// while the sequence is moving. Each trial must then receive the complete, ordered
// run from the watermark its hello carries through the last frame the writer
// published before the trial caught up: none missing and none repeated. A frame
// published between the sequence the hub read for the hello and the subscription
// it made is one such trial would lose, so a trial proves its watermark landed
// inside the burst's own sequence range; one whose open outran the burst asserts
// nothing there and is opened again within the budget.
func TestEventWatermarkRace(t *testing.T) {
	const (
		trials   = 50
		attempts = 20
		burst    = 32
		gap      = 250 * time.Microsecond
	)
	w := newWorld(t, options{})
	// One burst per trial: the writer publishes burst frames a gap apart while the
	// trial opens its stream, then reports the range it covered.
	bursts := make(chan chan burstBounds)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var seq int64
		for {
			select {
			case <-stop:
				return
			case done := <-bursts:
				first := seq
				for range burst {
					seq++
					w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(seq))})
					time.Sleep(gap)
				}
				done <- burstBounds{first: first, last: seq}
			}
		}
	}()
	defer func() {
		close(stop)
		<-finished
	}()
	raced, frames, opens, last := 0, 0, 0, int64(0)
	for trial := range trials {
		var (
			stream    *sse
			watermark int64
			end       int64
		)
		for attempt := 1; ; attempt++ {
			if attempt > attempts {
				t.Fatalf("trial %d: %d opens and none subscribed inside a burst, so the trial cannot race", trial, attempts)
			}
			opens++
			done := make(chan burstBounds, 1)
			bursts <- done
			stream = w.openWhenRoom("admin")
			hello, err := stream.read()
			if err != nil {
				stream.close()
				t.Fatalf("trial %d hello: %v", trial, err)
			}
			if hello.event != "hello" {
				stream.close()
				t.Fatalf("trial %d first frame = %q, want hello", trial, hello.event)
			}
			var body events.EventHello
			if err := json.Unmarshal(hello.data, &body); err != nil {
				stream.close()
				t.Fatalf("trial %d hello payload: %v", trial, err)
			}
			covered := <-done
			if covered.first < body.Watermark && body.Watermark < covered.last {
				watermark, end = body.Watermark, covered.last
				break
			}
			// The hello was read before the burst started or after it finished, so no
			// frame followed it: this open is not a race and the trial tries again.
			stream.close()
		}
		raced++
		for watermark < end {
			next, err := readWithin(t, stream, 5*time.Second)
			if err != nil {
				stream.close()
				t.Fatalf("trial %d frame after watermark %d (the burst published to %d): %v", trial, watermark, end, err)
			}
			if next.event != "job" {
				stream.close()
				t.Fatalf("trial %d frame %q after watermark %d", trial, next.event, watermark)
			}
			got := jobID(t, next)
			if got != watermark+1 {
				stream.close()
				t.Fatalf("trial %d: frame job = %d after watermark %d, want the next one (no gap, no duplicate)", trial, got, watermark)
			}
			watermark = got
			frames++
		}
		last = watermark
		stream.close()
	}
	t.Logf("WATERMARK-RACE trials=%d raced_trials=%d raced_frames=%d opens=%d last_watermark=%d", trials, raced, frames, opens, last)
}

// TestEventRingBounds proves the replay window's own bounds: a cursor older than
// the ring's bytes, or older than its age, is a gap rather than a partial replay,
// while a cursor still inside the window replays in order.
func TestEventRingBounds(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		w := newWorld(t, options{})
		// Each frame carries close to the wire maximum, so the ring's byte bound
		// evicts the oldest ones.
		heavy := map[string]string{"blob": strings.Repeat("b", 7000)}
		for i := 1; i <= 160; i++ {
			event := jobEvent(int64(i))
			event.Params = heavy
			w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(event)})
		}
		evicted := w.open("admin", "1.boot-events.1")
		defer evicted.close()
		if _, err := evicted.read(); err != nil {
			t.Fatalf("hello: %v", err)
		}
		resync, err := evicted.read()
		if err != nil {
			t.Fatalf("resync: %v", err)
		}
		var body events.EventResync
		if err := json.Unmarshal(resync.data, &body); err != nil {
			t.Fatalf("resync payload: %v", err)
		}
		if body.Reason != "gap" {
			t.Fatalf("a cursor the ring's bytes evicted = %+v, want gap", body)
		}
		// A cursor still inside the window replays what it missed, in order.
		inside := w.open("admin", "1.boot-events.150")
		defer inside.close()
		if _, err := inside.read(); err != nil {
			t.Fatalf("hello: %v", err)
		}
		for want := int64(151); want <= 160; want++ {
			next, err := readWithin(t, inside, 5*time.Second)
			if err != nil {
				t.Fatalf("replay frame %d: %v", want, err)
			}
			if got := jobID(t, next); got != want {
				t.Fatalf("replay frame job = %d, want %d", got, want)
			}
		}
	})
	t.Run("age", func(t *testing.T) {
		clock := &fakeClock{now: time.Now().UTC()}
		w := newWorld(t, options{now: clock.Now})
		w.hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(jobEvent(1))})
		// Past the recorded five-minute window the entry is gone even though
		// nothing was appended since.
		clock.advance(6 * time.Minute)
		stream := w.open("admin", "1.boot-events.0")
		defer stream.close()
		if _, err := stream.read(); err != nil {
			t.Fatalf("hello: %v", err)
		}
		resync, err := stream.read()
		if err != nil {
			t.Fatalf("resync: %v", err)
		}
		var body events.EventResync
		if err := json.Unmarshal(resync.data, &body); err != nil {
			t.Fatalf("resync payload: %v", err)
		}
		if body.Reason != "gap" {
			t.Fatalf("a cursor older than the window = %+v, want gap", body)
		}
	})
}
