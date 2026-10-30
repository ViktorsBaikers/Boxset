//go:build integration && unix

package main

// The event stream over the built binary: the composition's own guard mints the
// credential ref, binds auth.Revalidate and closes a downgraded viewer's stream
// while another administrator keeps receiving. The session response and the
// stream's hello name the same installation identity.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

var installIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// sseStream is one open event stream with a bounded reader: no line over the
// recorded raw bound, and at most one id, event and data line per frame.
type sseStream struct {
	t      *testing.T
	body   io.ReadCloser
	reader *bufio.Reader
}

func openStream(t *testing.T, c *http.Client, port string) *sseStream {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+port+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := c.Do(request)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		_ = response.Body.Close()
		t.Fatalf("stream: %d %s", response.StatusCode, body)
	}
	s := &sseStream{t: t, body: response.Body, reader: bufio.NewReaderSize(response.Body, 1<<16)}
	t.Cleanup(func() { _ = response.Body.Close() })
	return s
}

// next returns the next frame as its event name and data line.
func (s *sseStream) next() (string, []byte, error) {
	var name, data string
	var ids, names, datas int
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		if len(line) > 8200 {
			return "", nil, errors.New("line too long")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if ids+names+datas == 0 {
				continue
			}
			if datas != 1 || ids > 1 || names > 1 {
				return "", nil, errors.New("malformed frame")
			}
			return name, []byte(data), nil
		}
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			ids++
		case strings.HasPrefix(line, "event:"):
			names++
			name = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
		case strings.HasPrefix(line, "data:"):
			datas++
			data = strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if len(data) > 8192 || !json.Valid([]byte(data)) {
				return "", nil, errors.New("malformed data")
			}
		}
	}
}

// frameWithin reads one frame, failing when nothing arrives in d.
func (s *sseStream) frameWithin(d time.Duration) (string, []byte, error) {
	s.t.Helper()
	type result struct {
		name string
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		name, data, err := s.next()
		done <- result{name, data, err}
	}()
	select {
	case r := <-done:
		return r.name, r.data, r.err
	case <-time.After(d):
		s.t.Fatal("no frame within the bound")
		return "", nil, errors.New("timeout")
	}
}

func expectStreamEnded(t *testing.T, s *sseStream, d time.Duration) {
	t.Helper()
	name, data, err := s.frameWithin(d)
	if err == nil {
		t.Fatalf("the stream delivered %q after its authority ended: %s", name, data)
	}
}

// TestEventsOverHTTP proves the built binary's composition: a real PATCH
// downgrade closes that viewer's stream while another administrator's stream
// keeps receiving, over the production handler.
func TestEventsOverHTTP(t *testing.T) {
	dir := t.TempDir()
	seedUsers(t, dir)
	s := start(t, dir)
	s.waitReady(t)

	admin, status := signIn(t, s.port, "admin")
	if status != http.StatusCreated {
		t.Fatalf("admin sign-in: %d", status)
	}
	viewer, status := signIn(t, s.port, "viewer")
	if status != http.StatusCreated {
		t.Fatalf("viewer sign-in: %d", status)
	}

	adminStream := openStream(t, admin, s.port)
	if name, _, err := adminStream.next(); err != nil || name != "hello" {
		t.Fatalf("admin hello: %q %v", name, err)
	}
	viewerStream := openStream(t, viewer, s.port)
	if name, _, err := viewerStream.next(); err != nil || name != "hello" {
		t.Fatalf("viewer hello: %q %v", name, err)
	}

	// A real PATCH downgrade: the committed edit publishes its users frame and
	// closes the affected viewer's stream before its next frame.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, "http://127.0.0.1:"+s.port+"/api/v1/users/2", strings.NewReader(`{"expectedRevision":1,"role":"member"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := admin.Do(request)
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("downgrade: %d %s", response.StatusCode, body)
	}

	name, data, err := adminStream.frameWithin(10 * time.Second)
	if err != nil || name != "users" {
		t.Fatalf("the admin's next frame: %q %s %v", name, data, err)
	}
	expectStreamEnded(t, viewerStream, 10*time.Second)
	t.Logf("EVENTS-OVER-HTTP admin_frame=%s viewer=closed", name)
}

// seedUsers creates two administrators in a stopped data directory, so the test
// owns a downgrade target that is not the last administrator.
func seedUsers(t *testing.T, dir string) {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, install.Layout{Dir: dir}.DB(), store.Options{})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	defer func() { _ = db.Close() }()
	hasher := auth.NewHasher()
	for _, name := range []string{"admin", "viewer"} {
		if _, err := auth.CreateUser(ctx, db, hasher, name, syntheticPassword, auth.RoleAdmin); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

// sessionBody is the part of GET /api/v1/session these proofs read.
type sessionBody struct {
	User struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Rights       []string `json:"rights"`
	Credential   string   `json:"credential"`
	RestoreEpoch int64    `json:"restoreEpoch"`
	InstallID    string   `json:"installId"`
}

// helloBody is the stream's first frame.
type helloBody struct {
	Watermark    int64  `json:"watermark"`
	InstallID    string `json:"installId"`
	RestoreEpoch int64  `json:"restoreEpoch"`
	UserID       int64  `json:"userId"`
	Credential   string `json:"credential"`
}

// TestSessionConfirmsHello proves the two installations' identities: each
// authenticated session carries its own installation's installId, and that
// triple equals the same cookie's stream hello; the two installations differ
// only in installId.
func TestSessionConfirmsHello(t *testing.T) {
	type triple struct {
		installID    string
		userID       int64
		restoreEpoch int64
	}
	installations := make([]triple, 0, 2)
	for range 2 {
		dir := t.TempDir()
		s := start(t, dir)
		s.waitReady(t)
		if c := setupAdmin(t, s, "admin"); c == nil {
			t.Fatal("setup issued no client")
		}
		sessionClient, status := signIn(t, s.port, "admin")
		if status != http.StatusCreated {
			t.Fatalf("sign-in: %d", status)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+s.port+"/api/v1/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := sessionClient.Do(request)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("session: %d %s", response.StatusCode, body)
		}
		var session sessionBody
		if err := json.Unmarshal(body, &session); err != nil {
			t.Fatalf("session body: %v", err)
		}
		// The authenticated response is the Session component: every required
		// member present and the installation identity well formed.
		if !installIDPattern.MatchString(session.InstallID) || session.User.ID != 1 || session.RestoreEpoch != 1 || session.Credential != "cookie" || len(session.Rights) == 0 {
			t.Fatalf("session = %+v", session)
		}
		// The cookie name carries the same installation identity.
		for _, cookie := range sessionClient.Jar.Cookies(request.URL) {
			if !strings.HasSuffix(cookie.Name, session.InstallID) {
				t.Fatalf("the cookie %q does not name installation %q", cookie.Name, session.InstallID)
			}
		}

		stream := openStream(t, sessionClient, s.port)
		name, data, err := stream.next()
		if err != nil || name != "hello" {
			t.Fatalf("hello: %q %v", name, err)
		}
		var hello helloBody
		if err := json.Unmarshal(data, &hello); err != nil {
			t.Fatalf("hello body: %v", err)
		}
		if hello.InstallID != session.InstallID || hello.UserID != session.User.ID || hello.RestoreEpoch != session.RestoreEpoch {
			t.Fatalf("hello = %+v, session install=%q user=%d epoch=%d", hello, session.InstallID, session.User.ID, session.RestoreEpoch)
		}
		// The installation's own row is the value the session and the hello carry.
		var installed string
		if err := readOnly(t, dir).QueryRowContext(t.Context(), `SELECT install_id FROM installation`).Scan(&installed); err != nil {
			t.Fatalf("installation row: %v", err)
		}
		if installed != session.InstallID || installed != hello.InstallID {
			t.Fatalf("installation row %q, session %q, hello %q", installed, session.InstallID, hello.InstallID)
		}
		installations = append(installations, triple{installID: hello.InstallID, userID: hello.UserID, restoreEpoch: hello.RestoreEpoch})
	}
	if installations[0].installID == installations[1].installID {
		t.Fatalf("two installations shared the identity %q", installations[0].installID)
	}
	if installations[0].userID != installations[1].userID || installations[0].restoreEpoch != installations[1].restoreEpoch {
		t.Fatalf("the installations differ in more than installId: %+v", installations)
	}
	t.Logf("SESSION-CONFIRMS-HELLO installA=%s installB=%s userId=%d restoreEpoch=%d", installations[0].installID, installations[1].installID, installations[0].userID, installations[0].restoreEpoch)
}
