package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const password = "correct horse battery staple"

func keyFor(t *testing.T, in *installation, scope string) (int64, string) {
	t.Helper()
	id, token, err := in.s.backend.Load().Sessions.Keys().Create(t.Context(), "owner", "automation", scope)
	if err != nil {
		t.Fatal(err)
	}
	return id, token
}

func TestAPIKeyNeverInHTTP(t *testing.T) {
	in, _ := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/v1/session", 200},
		{"GET", "/api/v1/system/status", 200},
		{"GET", "/api/v1/users", 200},
		{"GET", "/api/v1/openapi.json", 200},
		{"GET", "/api/v1/api-keys", 404},
		{"POST", "/api/v1/api-keys", 404},
		{"GET", "/api/v1/keys", 404},
		{"POST", "/api/v1/keys", 404},
		{"GET", "/api/v1/system/status?apikey=" + key, 400},
	} {
		w := request(in.h, tc.method, tc.path, "", "X-Api-Key", key)
		if w.Code != tc.status {
			t.Fatalf("%s %s status=%d want=%d", tc.method, strings.Split(tc.path, "?")[0], w.Code, tc.status)
		}
		raw := w.Body.String()
		for name, values := range w.Header() {
			raw += name + strings.Join(values, ",")
		}
		for _, secret := range []string{key, "token_sha256", "tokenSha256", "password_phc", "passwordPhc"} {
			if strings.Contains(raw, secret) {
				t.Fatal("credential material in HTTP response")
			}
		}
	}
	if strings.Contains(in.logs.String(), key) {
		t.Fatal("API key in logs")
	}
	t.Log("API-KEY-HTTP surfaces=9 key_management_routes=0 sentinels=0")
}

func TestInvalidKeyWithCookie401(t *testing.T) {
	in, c := signedIn(t, "")
	for _, token := range []string{"", "invalid", "bxk_" + strings.Repeat("A", 43)} {
		w := request(in.h, "GET", "/api/v1/system/status", "", "Cookie", c.String(), "X-Api-Key", token)
		if w.Code != 401 || code(t, w) != "credential_invalid" {
			t.Fatal("key fell back to cookie")
		}
	}
}

func TestKeyCrossOriginOnlyAuthorized(t *testing.T) {
	in, _ := signedIn(t, "")
	_, read := keyFor(t, in, "read")
	_, write := keyFor(t, in, "write")
	for _, tc := range []struct {
		key    string
		status int
	}{{read, 403}, {write, 200}} {
		w := request(in.h, "PATCH", "/api/v1/users/1", `{"expectedRevision":1,"role":"admin"}`, "X-Api-Key", tc.key, "Sec-Fetch-Site", "cross-site")
		if w.Code != tc.status {
			t.Fatalf("cross-origin authorized=%v status=%d body=%s", tc.status == 200, w.Code, w.Body.String())
		}
	}
}

func TestDeleteSessionWithKey403(t *testing.T) {
	in, _ := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	w := request(in.h, "DELETE", "/api/v1/session", "", "X-Api-Key", key, "Sec-Fetch-Site", "cross-site")
	if w.Code != 403 || code(t, w) != "cookie_required" {
		t.Fatal("key logout must require cookie")
	}
}

func TestCSRFCookieOnly(t *testing.T) {
	in, c := signedIn(t, "")
	w := request(in.h, "PATCH", "/api/v1/users/1", `{"expectedRevision":1,"role":"admin"}`, "Cookie", c.Name+"="+c.Value, "Sec-Fetch-Site", "cross-site")
	if w.Code != 403 || code(t, w) != "cross_origin_denied" {
		t.Fatal("cookie CSRF missing")
	}
}

func TestRoleChangeNextRequest(t *testing.T) {
	in, _ := signedIn(t, "")
	if _, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "control", password, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	_, token := keyFor(t, in, "admin")
	if w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", token); w.Code != 200 {
		t.Fatal("authorized control")
	}
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET role='viewer' WHERE id=1"); return e }); err != nil {
		t.Fatal(err)
	}
	if w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", token); w.Code != 403 {
		t.Fatal("cached key role")
	}
	if w := request(in.h, "GET", "/api/v1/system/status", "", "X-Api-Key", token); w.Code != 200 {
		t.Fatal("viewer control")
	}
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET disabled=1 WHERE id=1"); return e }); err != nil {
		t.Fatal(err)
	}
	if w := request(in.h, "GET", "/api/v1/system/status", "", "X-Api-Key", token); w.Code != 401 {
		t.Fatal("disabled key")
	}
}

// TestServedKeyRightPerRoute requires a served key to be admitted for the right
// its route declares and no other: a users:read-only credential serves the user
// collection and is refused the edit route, whose use-time recheck supplies
// users:write, including when the stored scope has narrowed to read by the time
// that recheck reads it.
func TestServedKeyRightPerRoute(t *testing.T) {
	in, _ := signedIn(t, "")
	target, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "control", password, auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/users/" + strconv.FormatInt(target.ID, 10)
	edit := `{"expectedRevision":` + strconv.FormatInt(target.Revision, 10) + `,"role":"viewer"}`
	// untouched requires a refused edit to have changed nothing: the target row
	// and the collection revision stay where they were and Commits does not move.
	untouched := func(t *testing.T, commits, collection int64) {
		t.Helper()
		var role string
		var revision int64
		if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT role, revision FROM user WHERE id = ?", target.ID).Scan(&role, &revision)
		}); err != nil {
			t.Fatal(err)
		}
		if role != target.Role || revision != target.Revision {
			t.Fatalf("target row role=%q revision=%d, want %q and %d", role, revision, target.Role, target.Revision)
		}
		if got := collectionRevision(t, in); got != collection {
			t.Fatalf("collection revision = %d, want %d", got, collection)
		}
		if got := in.db.Commits(); got != commits {
			t.Fatalf("Commits = %d, want %d: the refusal committed", got, commits)
		}
	}

	t.Run("users read only", func(t *testing.T) {
		_, key := keyFor(t, in, "read")
		if w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", key); w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"control"`) {
			t.Fatalf("read-scope collection status=%d body=%s", w.Code, w.Body.String())
		}
		commits, collection := in.db.Commits(), collectionRevision(t, in)
		if w := request(in.h, "PATCH", path, edit, "X-Api-Key", key); w.Code != 403 || code(t, w) != "forbidden" {
			t.Fatalf("read-scope edit status=%d body=%s", w.Code, w.Body.String())
		}
		untouched(t, commits, collection)
	})

	// A scope that has narrowed to read leaves the actor holding users:read and
	// not the route's users:write, so the recheck must refuse on the route's right
	// rather than on a fixed one.
	t.Run("scope narrowed while the edit waited", func(t *testing.T) {
		id, key := keyFor(t, in, "write")
		commits, collection := in.db.Commits(), collectionRevision(t, in)
		w := obstructed(t, in, "UPDATE api_key SET scope='read' WHERE id="+strconv.FormatInt(id, 10), func() *httptest.ResponseRecorder {
			return request(in.h, "PATCH", path, edit, "X-Api-Key", key)
		})
		if w.Code != 403 || code(t, w) != "forbidden" {
			t.Fatalf("narrowed-scope edit status=%d body=%s", w.Code, w.Body.String())
		}
		untouched(t, commits, collection)
	})

	// The write-scope control proves the refusals above are the right and not the
	// routes refusing every key. It carries the same expected revision, so an
	// edit that had been refused after it committed would conflict here.
	_, key := keyFor(t, in, "write")
	if w := request(in.h, "PATCH", path, edit, "X-Api-Key", key); w.Code != 200 {
		t.Fatalf("write-scope control status=%d body=%s", w.Code, w.Body.String())
	}
}

// obstructedRun holds one external write reservation on the installation's file
// and runs run on its own goroutine: run resolves its credential, then waits at
// BEGIN behind the reservation. commit runs inside that reservation once run
// provably reached the write path. A run that never gets there is NOT-RUN, never
// a pass.
func obstructedRun(t *testing.T, in *installation, commit string, run func()) {
	t.Helper()
	peer, err := sql.Open("sqlite", in.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	peer.SetMaxOpenConns(1)
	conn, err := peer.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(t.Context(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	done := make(chan struct{})
	go func() { defer close(done); run() }()
	deadline := time.Now().Add(time.Second)
	for in.db.Stats().Writer.InUse != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("NOT-RUN: the request never reached the write path (Writer.InUse=%d), so the obstruction proves nothing", in.db.Stats().Writer.InUse)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := conn.ExecContext(t.Context(), commit); err != nil {
		t.Fatalf("reservation statement: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatalf("reservation commit: %v", err)
	}
	committed = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the obstructed request did not return after the reservation was released")
	}
}

// obstructed is obstructedRun for one request, whose response it returns.
func obstructed(t *testing.T, in *installation, commit string, do func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var w *httptest.ResponseRecorder
	obstructedRun(t, in, commit, func() { w = do() })
	return w
}

func collectionRevision(t *testing.T, in *installation) int64 {
	t.Helper()
	var n int64
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT revision FROM collection_revision WHERE name = 'users'").Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestActorRecheckInsideObstructedWrite requires a request's authority to be
// re-read inside its writer transaction. An external reservation holds the
// write lock, the request resolves its credential and then waits at BEGIN, and
// the change that reservation commits decides it: a guard-time read is stale.
func TestActorRecheckInsideObstructedWrite(t *testing.T) {
	const edit = `{"expectedRevision":1,"role":"member"}`
	const demote = "UPDATE user SET role='viewer', revision=revision+1 WHERE id=1"
	const newEpoch = "UPDATE installation SET restore_epoch=restore_epoch+1"

	// actorAndTarget signs in the installation's admin and adds the user the
	// edit is aimed at, so the actor and the target are different rows.
	actorAndTarget := func(t *testing.T) (*installation, *http.Cookie, auth.User) {
		t.Helper()
		in, cookie := signedIn(t, "")
		target, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "control", password, auth.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		return in, cookie, target
	}
	// untouched requires the refused request to have changed nothing: the target
	// row and the collection revision stay where the reservation left them, and
	// Commits does not move.
	untouched := func(t *testing.T, in *installation, target auth.User, commits, collection int64) {
		t.Helper()
		var role string
		var revision int64
		if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT role, revision FROM user WHERE id = ?", target.ID).Scan(&role, &revision)
		}); err != nil {
			t.Fatal(err)
		}
		if role != target.Role || revision != target.Revision {
			t.Fatalf("target row role=%q revision=%d, want %q and %d", role, revision, target.Role, target.Revision)
		}
		if got := collectionRevision(t, in); got != collection {
			t.Fatalf("collection revision = %d, want the reservation's %d", got, collection)
		}
		if got := in.db.Commits(); got != commits {
			t.Fatalf("Commits = %d, want %d: the refusal committed", got, commits)
		}
	}

	t.Run("healthy actor", func(t *testing.T) {
		in, _, target := actorAndTarget(t)
		_, key := keyFor(t, in, "admin")
		commits := in.db.Commits()
		w := request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(target.ID, 10), edit, "X-Api-Key", key)
		if w.Code != 200 {
			t.Fatalf("current actor status=%d body=%s", w.Code, w.Body.String())
		}
		if in.db.Commits() != commits+1 {
			t.Fatalf("current actor Commits=%d, want %d", in.db.Commits(), commits+1)
		}
	})

	t.Run("key", func(t *testing.T) {
		in, _, target := actorAndTarget(t)
		_, key := keyFor(t, in, "admin")
		commits, collection := in.db.Commits(), collectionRevision(t, in)
		w := obstructed(t, in, demote, func() *httptest.ResponseRecorder {
			return request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(target.ID, 10), edit, "X-Api-Key", key)
		})
		if w.Code != 403 || code(t, w) != "forbidden" {
			t.Fatalf("demoted key actor status=%d body=%s", w.Code, w.Body.String())
		}
		untouched(t, in, target, commits, collection+1)
		logs := in.logs.String()
		if !strings.Contains(logs, `"credential":"key"`) || !strings.Contains(logs, `"result":"refused"`) || !strings.Contains(logs, `"reason":"forbidden"`) {
			t.Fatalf("audit event missing or off-contract: %s", logs)
		}
		if strings.Contains(logs, key) {
			t.Fatal("audit event carried the credential")
		}
	})

	t.Run("cookie", func(t *testing.T) {
		in, cookie, target := actorAndTarget(t)
		commits, collection := in.db.Commits(), collectionRevision(t, in)
		w := obstructed(t, in, demote, func() *httptest.ResponseRecorder {
			return request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(target.ID, 10), edit, "Cookie", cookie.Name+"="+cookie.Value)
		})
		if w.Code != 403 || code(t, w) != "forbidden" {
			t.Fatalf("demoted cookie actor status=%d body=%s", w.Code, w.Body.String())
		}
		untouched(t, in, target, commits, collection+1)
		if logs := in.logs.String(); !strings.Contains(logs, `"credential":"cookie"`) {
			t.Fatalf("cookie audit event missing: %s", logs)
		}
	})

	t.Run("bypass", func(t *testing.T) {
		in, _, target := actorAndTarget(t)
		b := *in.s.backend.Load()
		b.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		in.s.Attach(b)
		commits, collection := in.db.Commits(), collectionRevision(t, in)
		w := obstructed(t, in, newEpoch, func() *httptest.ResponseRecorder {
			return request(in.h, "PATCH", "/api/v1/users/"+strconv.FormatInt(target.ID, 10), edit)
		})
		if w.Code != 401 || code(t, w) != "credential_invalid" {
			t.Fatalf("stale bypass epoch status=%d body=%s", w.Code, w.Body.String())
		}
		untouched(t, in, target, commits, collection)
		if logs := in.logs.String(); !strings.Contains(logs, `"credential":"local_bypass"`) {
			t.Fatalf("bypass audit event missing: %s", logs)
		}
	})

	t.Run("pretransaction mutant", func(t *testing.T) {
		in, _, target := actorAndTarget(t)
		commits := in.db.Commits()
		role := auth.RoleMember
		var actorRole string
		var edited auth.User
		var editErr error
		obstructedRun(t, in, demote, func() {
			// The discarded arrangement reads the actor by its own read
			// immediately before the write: that read is the pre-demotion row,
			// so the edit is not refused.
			editErr = in.db.Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRow("SELECT role FROM user WHERE id = 1").Scan(&actorRole)
			})
			if editErr != nil {
				return
			}
			edited, editErr = auth.UpdateUser(t.Context(), in.db, target.ID, auth.UserPatch{ExpectedRevision: target.Revision, Role: &role})
		})
		if editErr != nil || actorRole != auth.RoleAdmin {
			t.Fatalf("mutant arrangement read %q and edited with %v; want the stale admin read and its commit", actorRole, editErr)
		}
		if edited.Revision != target.Revision+1 || in.db.Commits() != commits+1 {
			t.Fatalf("mutant revision=%d Commits=%d, want %d and %d", edited.Revision, in.db.Commits(), target.Revision+1, commits+1)
		}
		t.Logf("pretransaction mutant: stale actor read %q, edit committed revision %d and Commits+1; only the in-transaction re-read denies", actorRole, edited.Revision)
	})
}

func TestProxyCannotBypass(t *testing.T) {
	in, _ := signedIn(t, "")
	b := *in.s.backend.Load()
	b.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	b.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}
	in.s.Attach(b)
	for _, tc := range []struct {
		peer   string
		status int
	}{{"127.0.0.1:1", 401}, {"[::1]:1", 401}, {"[::ffff:127.0.0.1]:1", 401}, {"192.0.2.1:1", 401}, {"127.0.0.2:1", 200}} {
		r := httptest.NewRequest("GET", "http://localhost/api/v1/system/status", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("Forwarded", "for=127.0.0.2")
		r.Header.Set("X-Forwarded-For", "127.0.0.2")
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("peer=%s status=%d want=%d", tc.peer, w.Code, tc.status)
		}
	}
	r := httptest.NewRequest("GET", "http://evil.example/api/v1/system/status", nil)
	r.RemoteAddr = "127.0.0.2:1"
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	if w.Code != 401 || code(t, w) != "unauthenticated" {
		t.Fatal("unapproved Host granted local bypass")
	}
}

func TestCookieGETNoCommits(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		in, cookie := signedIn(t, base)
		now := time.Now().UTC().Truncate(time.Second)
		b := *in.s.backend.Load()
		b.Sessions = b.Setup.SessionsWithClock(func() time.Time { return now })
		in.s.Attach(b)
		readExpiry := func() int64 {
			t.Helper()
			var n int64
			if err := in.db.Read(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow("SELECT expires_at FROM session").Scan(&n) }); err != nil {
				t.Fatal(err)
			}
			return n
		}
		readBytes := func() [32]byte {
			t.Helper()
			var seq int
			var name, filename string
			if err := in.db.Read(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow("PRAGMA database_list").Scan(&seq, &name, &filename) }); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			wal, err := os.ReadFile(filename + "-wal")
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			return sha256.Sum256(append(content, wal...))
		}
		start := now
		for _, elapsed := range []time.Duration{0, time.Hour - time.Second, time.Hour, time.Hour + time.Second, 2 * time.Hour, 29 * 24 * time.Hour, 31 * 24 * time.Hour} {
			now = start.Add(elapsed)
			for _, method := range []string{"GET", "HEAD"} {
				before, expiry := in.db.Commits(), readExpiry()
				stored := readBytes()
				w := request(in.h, method, base+"/api/v1/session", "", "Cookie", cookie.Name+"="+cookie.Value)
				if w.Code != 200 || len(w.Result().Cookies()) != 1 {
					t.Fatalf("read-only %s at %v status=%d", method, elapsed, w.Code)
				}
				if !w.Result().Cookies()[0].Expires.Equal(now.Add(auth.SessionTTL)) {
					t.Fatal("current sliding cookie expiry")
				}
				if in.db.Commits() != before || readExpiry() != expiry || readBytes() != stored {
					t.Fatal("HTTP read committed metadata")
				}
			}
		}
		before := readExpiry()
		if err := b.Sessions.FlushMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
		if readExpiry() <= before {
			t.Fatal("worker did not persist queued activity")
		}
		now = now.Add(auth.SessionTTL)
		beforeCommits := in.db.Commits()
		if w := request(in.h, "GET", base+"/api/v1/session", "", "Cookie", cookie.Name+"="+cookie.Value); w.Code != 401 || in.db.Commits() != beforeCommits {
			t.Fatal("idle session accepted or committed")
		}
		if _, err := auth.CreateUser(t.Context(), in.db, auth.NewHasher(), "control", password, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		login := func() *http.Cookie {
			t.Helper()
			w := request(in.h, "POST", base+"/api/v1/session", credentials("owner"))
			if w.Code != 201 || len(w.Result().Cookies()) != 1 {
				t.Fatal("fresh cookie control")
			}
			return w.Result().Cookies()[0]
		}
		assertRead := func(c *http.Cookie, value string, want int) {
			t.Helper()
			for _, method := range []string{"GET", "HEAD"} {
				before, stored := in.db.Commits(), readBytes()
				w := request(in.h, method, base+"/api/v1/session", "", "Cookie", c.Name+"="+value)
				if w.Code != want || in.db.Commits() != before || readBytes() != stored {
					t.Fatalf("cookie %s status=%d want=%d or request wrote state", method, w.Code, want)
				}
			}
		}
		fresh := login()
		assertRead(fresh, fresh.Value, 200)
		assertRead(fresh, "malformed", 401)
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("UPDATE user SET role='viewer' WHERE id=1"); return err }); err != nil {
			t.Fatal(err)
		}
		assertRead(fresh, fresh.Value, 200)
		w := request(in.h, "GET", base+"/api/v1/users", "", "Cookie", fresh.Name+"="+fresh.Value)
		if w.Code != 403 {
			t.Fatal("cookie retained previous role")
		}
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("UPDATE user SET disabled=1 WHERE id=1"); return err }); err != nil {
			t.Fatal(err)
		}
		assertRead(fresh, fresh.Value, 401)
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("UPDATE user SET disabled=0 WHERE id=1"); return err }); err != nil {
			t.Fatal(err)
		}
		assertRead(fresh, fresh.Value, 200)
		if w := request(in.h, "DELETE", base+"/api/v1/session", "", "Cookie", fresh.Name+"="+fresh.Value); w.Code != 204 {
			t.Fatal("cookie logout control")
		}
		assertRead(fresh, fresh.Value, 401)
		fresh = login()
		assertRead(fresh, fresh.Value, 200)
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE installation SET restore_epoch=restore_epoch+1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertRead(fresh, fresh.Value, 401)
		fresh = login()
		assertRead(fresh, fresh.Value, 200)
	}
}

func TestLoginReachableUnauthenticated(t *testing.T) {
	in := newInstallation(t, "", "")
	created := request(in.h, "POST", "/api/v1/setup", `{"username":"owner","password":"`+password+`"}`, "Authorization", "Bearer "+in.token)
	if created.Code != 201 {
		t.Fatalf("setup status=%d", created.Code)
	}
	w := request(in.h, "POST", "/api/v1/session", `{"username":"owner","password":"`+password+`"}`)
	if w.Code != 201 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%d", w.Code, len(w.Result().Cookies()))
	}
}

func TestLoginCrossOrigin403(t *testing.T) {
	in := newInstallation(t, "", "")
	w := request(in.h, "POST", "/api/v1/session", `{}`, "Sec-Fetch-Site", "cross-site")
	if w.Code != 403 {
		t.Fatalf("cross-origin login status=%d", w.Code)
	}
}

func TestLoginHostGuard421(t *testing.T) {
	in := newInstallation(t, "", "")
	w := request(in.h, "POST", "/api/v1/session", `{}`, "Host", "unapproved.example")
	if w.Code != 421 {
		t.Fatalf("login Host status=%d", w.Code)
	}
}

func signedIn(t *testing.T, base string) (*installation, *http.Cookie) {
	t.Helper()
	in := newInstallation(t, base, "")
	if w := request(in.h, "POST", base+"/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != 201 {
		t.Fatalf("setup=%d", w.Code)
	}
	w := request(in.h, "POST", base+"/api/v1/session", credentials("owner"))
	if w.Code != 201 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login=%d", w.Code)
	}
	return in, w.Result().Cookies()[0]
}

func TestLogoutDenies(t *testing.T) {
	in, c := signedIn(t, "/media")
	header := c.Name + "=" + c.Value
	w := request(in.h, "GET", "/media/api/v1/session", "", "Cookie", header)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"credential":"cookie"`) {
		t.Fatalf("session=%d", w.Code)
	}
	if w := request(in.h, "GET", "/media/api/v1/session", "", "Cookie", header, "X-Api-Key", "invalid"); w.Code != 401 {
		t.Fatal("explicit invalid key fell back to cookie")
	}
	if w := request(in.h, "GET", "/media/api/v1/session", "", "Cookie", header, "X-Api-Key", ""); w.Code != 401 {
		t.Fatal("explicit empty key fell back to cookie")
	}
	if w := request(in.h, "DELETE", "/media/api/v1/session", "", "Cookie", header, "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
		t.Fatal("cookie logout CSRF")
	}
	w = request(in.h, "DELETE", "/media/api/v1/session", "", "Cookie", header)
	if w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout=%d", w.Code)
	}
	if w := request(in.h, "GET", "/media/api/v1/session", "", "Cookie", header); w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
}

func TestExpiredSessionDeniesProtectedHTTP(t *testing.T) {
	in, cookie := signedIn(t, "/media")
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE session SET expires_at=0")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/system/status"} {
		w := request(in.h, "GET", "/media"+path, "", "Cookie", cookie.Name+"="+cookie.Value)
		if w.Code != 401 || code(t, w) != "credential_invalid" {
			t.Fatalf("expired session status=%d", w.Code)
		}
	}
}

func TestCookieAttributesTLSAndHTTP(t *testing.T) {
	in, c := signedIn(t, "/media")
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure || c.Path != "/media" || c.Domain != "" || !strings.HasPrefix(c.Name, "boxset-") {
		t.Fatal("HTTP cookie attributes")
	}
	b := *in.s.backend.Load()
	b.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	in.s.Attach(b)
	for _, tc := range []struct {
		peer, scheme string
		tls          bool
		secure       bool
	}{
		{"127.0.0.1:1", "https", false, true}, {"[::1]:1", "https", false, true}, {"[::ffff:127.0.0.1]:1", "https", false, true},
		{"192.0.2.1:1", "https", false, false}, {"127.0.0.1:1", "https,http", false, false}, {"127.0.0.1:1", "https, https", false, false}, {"127.0.0.1:1", "HTTPS", false, false}, {"127.0.0.1:1", "http", false, false}, {"192.0.2.1:1", "http", true, true},
	} {
		r := httptest.NewRequest("POST", "http://localhost/media/api/v1/session", strings.NewReader(credentials("owner")))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-Proto", tc.scheme)
		r.RemoteAddr = tc.peer
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		if w.Code != 201 || len(w.Result().Cookies()) != 1 {
			t.Fatalf("cookie control status=%d", w.Code)
		}
		c := w.Result().Cookies()[0]
		if c.Secure != tc.secure || !c.HttpOnly || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("cookie mode secure=%v want=%v", c.Secure, tc.secure)
		}
		if tc.secure && (!strings.HasPrefix(c.Name, "__Host-boxset-") || c.Path != "/") {
			t.Fatal("TLS host cookie scope")
		}
	}
	r := httptest.NewRequest("POST", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Add("X-Forwarded-Proto", "https")
	r.Header.Add("X-Forwarded-Proto", "http")
	if secureRequest(r, b.TrustedProxies) {
		t.Fatal("conflicting schemes accepted")
	}
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.RemoteAddr = "192.0.2.1:1"
	r.Header.Set("X-Forwarded-Proto", "https")
	if secureRequest(r, b.TrustedProxies) {
		t.Fatal("forwarded client substituted for TCP peer")
	}
}

func TestAccessConfigSnapshot(t *testing.T) {
	in, _ := signedIn(t, "")
	b := *in.s.backend.Load()
	b.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	b.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	in.s.Attach(b)
	b.TrustedProxies[0] = netip.MustParsePrefix("192.0.2.0/24")
	b.LocalBypass[0] = netip.MustParsePrefix("192.0.2.0/24")
	w := request(in.h, "POST", "/api/v1/session", credentials("owner"), "X-Forwarded-Proto", "https")
	if w.Code != 201 || !w.Result().Cookies()[0].Secure {
		t.Fatal("caller mutation changed trusted peers")
	}
	if w := request(in.h, "POST", "/api/v1/session", credentials("owner"), "Host", "evil.example"); w.Code != 421 {
		t.Fatal("closed setup bypass Host guard missing")
	}
	if w := request(in.h, "GET", "/api/v1/session", ""); w.Code != 401 {
		t.Fatal("bypass principal granted before policy owner")
	}
}

func TestLoginSecretsNeverEscape(t *testing.T) {
	in, _ := signedIn(t, "")
	const sentinel = "synthetic-private-password"
	for _, body := range []string{`{"username":"owner","password":"` + sentinel + `"}`, `{"username":"owner","password":"` + sentinel, `{"username":"owner","password":"` + sentinel + `","extra":"` + sentinel + `"}`} {
		w := request(in.h, "POST", "/api/v1/session", body)
		if w.Code < 400 || strings.Contains(w.Body.String(), sentinel) || strings.Contains(in.logs.String(), sentinel) {
			t.Fatal("login error exposed input")
		}
	}
	w := request(in.h, "POST", "/api/v1/session", credentials("owner"))
	if w.Code != 201 {
		t.Fatal("login control")
	}
	token := w.Result().Cookies()[0].Value
	if strings.Contains(w.Body.String(), token) || strings.Contains(in.logs.String(), token) {
		t.Fatal("session credential escaped cookie")
	}
}

func TestLoginThrottleAndLifecycle(t *testing.T) {
	in := newInstallation(t, "", "")
	for range 10 {
		w := request(in.h, "POST", "/api/v1/session", credentials("absent"))
		if w.Code != 401 {
			t.Fatalf("failure status=%d", w.Code)
		}
	}
	w := request(in.h, "POST", "/api/v1/session", credentials("absent"))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("login throttle missing")
	}
	for _, state := range []State{Starting, Recovering, Draining} {
		in.s.SetState(state)
		w := request(in.h, "POST", "/api/v1/session", credentials("owner"))
		if w.Code != 503 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("lifecycle %s login=%d", state, w.Code)
		}
	}
}

// installation is a server over a real store whose setup is armed, as serve
// leaves it once the store opened.
type installation struct {
	s     *Server
	h     http.Handler
	db    *store.DB
	path  string
	token string
	logs  *bytes.Buffer
}

func newInstallation(t *testing.T, base, envToken string, allowedHosts ...string) *installation {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "boxset.db")
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	setup := auth.NewSetup(db, auth.NewHasher(), dir)
	if open, err := setup.Arm(t.Context(), envToken); !open || err != nil {
		t.Fatalf("Arm = %v, %v", open, err)
	}
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(fakeDist(t), base)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	s.Attach(Backend{
		Setup:        setup,
		Log:          slog.New(redact.NewHandler(slog.NewJSONHandler(logs, nil))),
		AllowedHosts: allowedHosts,
	})
	s.SetState(Ready)
	return &installation{s: s, h: s.http.Handler, db: db, path: path, token: string(token), logs: logs}
}

func (in *installation) users(t *testing.T) int {
	t.Helper()
	var n int
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), "SELECT count(*) FROM user").Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// request serves one request from a loopback peer to Host 127.0.0.1:8484; headers are
// name/value pairs, and a "Host" pair replaces the host.
func request(h http.Handler, method, target, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "127.0.0.1:8484"
	r.RemoteAddr = "127.0.0.1:50000"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i] == "Host" {
			r.Host = header[i+1]
			continue
		}
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func credentials(username string) string {
	return `{"username":"` + username + `","password":"` + password + `"}`
}

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct{ Code string }
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		return ""
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body %q: %v", w.Body.String(), err)
	}
	return p.Code
}

func notShell(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(w.Body.String(), "<!doctype html>") || strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("%s: answered with the shell", what)
	}
}

// documentRoutes is every path x method of the committed OpenAPI document with
// its x-boxset-class.
func documentRoutes(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "api", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Paths map[string]map[string]struct {
			Class string `json:"x-boxset-class"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for path, item := range d.Paths {
		for method, op := range item {
			out[strings.ToUpper(method)+" "+path] = op.Class
		}
	}
	return out
}

var huma4 = []string{"/api/v1/openapi.json", "/api/v1/openapi-3.0.json", "/api/v1/openapi.yaml", "/api/v1/openapi-3.0.yaml"}

// The route-class table is exactly the document's operations, the four
// document routes and the public-doc rows, and each pattern resolves to itself.
func TestRouteClassTable(t *testing.T) {
	mux := http.NewServeMux()
	sh, err := loadShell(fakeDist(t), "", "native")
	if err != nil {
		t.Fatal(err)
	}
	public := append([]string{"GET /ping"}, sh.register(mux)...)
	mux.HandleFunc("GET /ping", func(http.ResponseWriter, *http.Request) {})
	_, routes := api.New(mux, nil)

	want := documentRoutes(t)
	for _, p := range huma4 {
		want["GET "+p] = api.ClassPrincipal
	}
	for _, p := range public {
		want[p] = api.ClassPublicDoc
	}
	if !slices.Equal(public, []string{"GET /ping", "GET /{$}", "GET /setup", "GET /sign-in", "GET /_app/"}) {
		t.Errorf("public-doc rows %v", public)
	}
	got := map[string]string{}
	for _, p := range public {
		got[p] = api.ClassPublicDoc
	}
	for _, r := range routes {
		if _, dup := got[r.Pattern]; dup {
			t.Errorf("pattern %s has two classes", r.Pattern)
		}
		got[r.Pattern] = r.Class
	}
	if len(got) != len(want) {
		t.Errorf("table has %d rows, want %d (operations + document routes + public-doc)", len(got), len(want))
	}
	for p, class := range want {
		if got[p] != class {
			t.Errorf("%s: class %q, want %q", p, got[p], class)
		}
		method, path, _ := strings.Cut(p, " ")
		path = strings.Replace(path, "{$}", "", 1)
		if path == "/_app/" {
			path = "/_app/x.js"
		}
		if _, pattern := mux.Handler(httptest.NewRequest(method, path, nil)); pattern != p {
			t.Errorf("%s %s resolves to pattern %q, want %q", method, path, pattern, p)
		}
	}
	t.Logf("ROUTE-CLASS-TABLE rows=%d", len(got))
}

// Every principal operation, the four document routes and unknown or
// compatibility API paths answer 401 unauthenticated without a credential, in
// every lifecycle state the installation can be in — including both recovering
// reasons, where no credential is read at all; the public-doc rows keep their own
// outcomes and nothing under /api/ is ever the shell.
func TestUnauthenticatedMatrixAllStates(t *testing.T) {
	in := newInstallation(t, "/media", "")
	var principal []string
	for p, class := range documentRoutes(t) {
		if class == api.ClassPrincipal {
			principal = append(principal, p)
		}
	}
	for _, p := range huma4 {
		principal = append(principal, "GET "+p, "HEAD "+p)
	}
	for _, p := range []string{"/api/v1/nope", "/api/v1/events", "/api/v3/series", "/api/v1/system/status", "/api", "/api/", "/api/v1/setup"} {
		principal = append(principal, "GET "+p)
	}
	principal = append(principal, "DELETE /api/v1/system/status", "PUT /api/v1/users/1", "POST /api/v3/command")
	rows := 0
	states := []struct {
		name string
		set  func()
	}{
		{"starting", func() { in.s.SetState(Starting) }},
		{"recovering/restore_in_progress", func() { recoveringWith(t, in, ReasonRestoreInProgress) }},
		{"recovering/key_missing", func() { recoveringWith(t, in, secret.ReasonMissing) }},
		{"ready", func() { recoveringWith(t, in, ""); in.s.SetState(Ready) }},
		{"draining", func() { recoveringWith(t, in, ""); in.s.SetState(Draining) }},
	}
	for _, state := range states {
		state.set()
		current := in.s.currentState()
		for _, row := range principal {
			method, path, _ := strings.Cut(row, " ")
			w := request(in.h, method, "/media"+path, "", "Accept", "text/html")
			if w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
				t.Errorf("%s: %s = %d %q, want 401 unauthenticated", state.name, row, w.Code, w.Body.String())
			}
			notShell(t, row, w)
			rows++
			if current == Ready {
				if w := request(in.h, method, "/media"+path, "", "X-Api-Key", "bxk_not-a-key"); w.Code != http.StatusUnauthorized || code(t, w) != "credential_invalid" {
					t.Errorf("%s with an unknown key = %d %q, want 401 credential_invalid", row, w.Code, w.Body.String())
				}
			}
		}
		// The exact allowances keep their own outcomes.
		if w := request(in.h, "GET", "/media/ping", ""); w.Code != http.StatusOK || w.Header().Get("X-Boxset-State") != string(current) {
			t.Errorf("%s: /ping = %d", state.name, w.Code)
		}
		for _, p := range []string{"/media/", "/media/setup", "/media/sign-in"} {
			if w := request(in.h, "GET", p, ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				t.Errorf("%s: GET %s = %d, want the shell", state.name, p, w.Code)
			}
		}
		if w := request(in.h, "POST", "/media/api/v1/setup", credentials("admin")); w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
			t.Errorf("%s: setup without bearer = %d %q", state.name, w.Code, w.Body.String())
		}
		if w := request(in.h, "POST", "/media/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+strings.Repeat("A", 43)); w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
			t.Errorf("%s: setup with a wrong bearer = %d %q", state.name, w.Code, w.Body.String())
		}
		if w := request(in.h, "GET", "/media/unlisted", ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: unlisted non-API path = %d, want 404", state.name, w.Code)
		} else {
			notShell(t, "unlisted", w)
		}
	}
	t.Logf("UNAUTHENTICATED-MATRIX rows=%d states=%d", rows, len(states))
}

func TestUnknownAndCompatAPINeverShell(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		in := newInstallation(t, base, "")
		for _, p := range []string{
			"/api/v3/series", "/api/v3/movie", "/api/v3/system/status", "/api/v1/system/status/", "/api/v2.0/indexers",
			"/api/v1/", "/api/v9/anything", "/api/v1/setup/extra", "/api/V1/setup",
		} {
			for _, method := range []string{"GET", "POST", "HEAD"} {
				w := request(in.h, method, base+p, "", "Accept", "text/html,application/xhtml+xml")
				if w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
					t.Errorf("base %q %s %s = %d, want 401 unauthenticated", base, method, p, w.Code)
				}
				notShell(t, method+" "+p, w)
			}
		}
		// Compatibility-style prefixes outside /api/ are unlisted paths: 404, never the shell.
		for _, p := range []string{"/series/api/v3/system/status", "/movie/api/", "/indexer/api/v2.0/indexers", "/settings"} {
			w := request(in.h, "GET", base+p, "", "Accept", "text/html")
			if w.Code != http.StatusNotFound {
				t.Errorf("base %q GET %s = %d, want 404", base, p, w.Code)
			}
			notShell(t, p, w)
		}
	}
}

func TestSetupRouteNotPublic(t *testing.T) {
	in := newInstallation(t, "", "")
	for name, header := range map[string][]string{
		"no credential":          nil,
		"wrong bearer":           {"Authorization", "Bearer " + strings.Repeat("A", 43)},
		"empty bearer":           {"Authorization", "Bearer "},
		"basic scheme":           {"Authorization", "Basic " + in.token},
		"token without a scheme": {"Authorization", in.token},
		"token as API key":       {"X-Api-Key", in.token},
		"token as cookie":        {"Cookie", "boxset=" + in.token},
	} {
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), header...)
		if w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
			t.Errorf("%s: %d %q, want 401 setup_credential_invalid", name, w.Code, w.Body.String())
		}
	}
	// A credential in the query is refused before routing.
	if w := request(in.h, "POST", "/api/v1/setup?token="+in.token, credentials("admin")); w.Code != http.StatusBadRequest || code(t, w) != "secret_in_url" {
		t.Errorf("token in the query: %d %q", w.Code, w.Body.String())
	}
	// Before the store is attached and ready, the right bearer is not even read.
	cold, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if w := request(cold.http.Handler, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token); w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
		t.Errorf("starting: %d %q", w.Code, w.Body.String())
	}
	in.s.SetState(Starting)
	if w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token); w.Code != http.StatusUnauthorized {
		t.Errorf("attached but starting: %d", w.Code)
	}
	in.s.SetState(Ready)
	if n := in.users(t); n != 0 {
		t.Fatalf("%d users after refused requests", n)
	}
	// The setup credential grants only setup:create.
	if w := request(in.h, "GET", "/api/v1/system/status", "", "Authorization", "Bearer "+in.token); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
		t.Errorf("status with the setup bearer: %d %q", w.Code, w.Body.String())
	}
	if w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("the setup bearer: %d %q", w.Code, w.Body.String())
	}
}

func TestSetupThrottled(t *testing.T) {
	in := newInstallation(t, "", "")
	for i := 1; i <= 10; i++ {
		if w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer wrong-token"); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
	if w.Code != http.StatusTooManyRequests || code(t, w) != "setup_throttled" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("11th attempt within 5 min: %d %q Retry-After %q, want 429 setup_throttled", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	// Another peer is not throttled.
	r := httptest.NewRequest("POST", "/api/v1/setup", strings.NewReader(credentials("admin")))
	r.Host = "127.0.0.1:8484"
	r.RemoteAddr = "192.0.2.7:4000"
	r.Header.Set("Authorization", "Bearer "+in.token)
	r.Header.Set("Content-Type", "application/json")
	other := httptest.NewRecorder()
	in.h.ServeHTTP(other, r)
	if other.Code != http.StatusCreated {
		t.Fatalf("another peer: %d %q", other.Code, other.Body.String())
	}
}

func TestSetupThrottleExpiry(t *testing.T) {
	in := newInstallation(t, "", "")
	now := time.Unix(1_700_000_000, 0)
	in.s.throttle.now = func() time.Time { return now }
	for range throttleFailures {
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer wrong")
		if w.Code != 401 {
			t.Fatalf("initial credential failure=%d", w.Code)
		}
	}
	for _, tc := range []struct {
		elapsed time.Duration
		retry   string
	}{
		{0, "300"}, {throttleWindow - time.Nanosecond, "1"},
	} {
		now = time.Unix(1_700_000_000, 0).Add(tc.elapsed)
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
		if w.Code != 429 || code(t, w) != "setup_throttled" || w.Header().Get("Retry-After") != tc.retry {
			t.Fatalf("elapsed %v: status=%d code=%s Retry-After=%q", tc.elapsed, w.Code, code(t, w), w.Header().Get("Retry-After"))
		}
	}
	now = time.Unix(1_700_000_000, 0).Add(throttleWindow)
	w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
	if w.Code != 201 || in.users(t) != 1 {
		t.Fatalf("same peer at expiry: status=%d users=%d", w.Code, in.users(t))
	}
}

func TestSetupThrottleFullTableRecovery(t *testing.T) {
	in := newInstallation(t, "", "")
	now := time.Unix(1_700_000_000, 0)
	in.s.throttle.now = func() time.Time { return now }
	for i := range throttlePeers {
		in.s.throttle.fail(strconv.Itoa(i), now)
	}
	in.s.throttle.fail("overflow", now)
	if len(in.s.throttle.peers) != throttlePeers {
		t.Fatalf("full table grew to %d entries", len(in.s.throttle.peers))
	}
	w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
	if w.Code != 429 || w.Header().Get("Retry-After") != "300" {
		t.Fatalf("full table: status=%d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	now = now.Add(throttleWindow)
	w = request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
	if w.Code != 201 || len(in.s.throttle.peers) != 0 || in.users(t) != 1 {
		t.Fatalf("expired full table: status=%d peers=%d users=%d", w.Code, len(in.s.throttle.peers), in.users(t))
	}
}

func TestSetupCrossOrigin403(t *testing.T) {
	in := newInstallation(t, "", "")
	for name, header := range map[string][]string{
		"cross-site fetch": {"Sec-Fetch-Site", "cross-site"},
		"foreign origin":   {"Origin", "http://evil.example"},
	} {
		h := append([]string{"Authorization", "Bearer " + in.token}, header...)
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), h...)
		if w.Code != http.StatusForbidden || code(t, w) != "cross_origin_denied" {
			t.Errorf("%s: %d %q, want 403 cross_origin_denied", name, w.Code, w.Body.String())
		}
	}
	if n := in.users(t); n != 0 {
		t.Fatalf("a cross-origin request created %d users", n)
	}
	w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token, "Sec-Fetch-Site", "same-origin", "Origin", "http://127.0.0.1:8484")
	if w.Code != http.StatusCreated {
		t.Fatalf("same-origin control: %d %q", w.Code, w.Body.String())
	}
}

func TestHostGuard(t *testing.T) {
	in := newInstallation(t, "", "", "boxset.home.example")
	for _, host := range []string{"evil.example", "evil.example:8484", "boxset.home.example.evil.example", "localhost.evil.example"} {
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Host", host, "Authorization", "Bearer "+in.token)
		if w.Code != http.StatusMisdirectedRequest || code(t, w) != "host_not_allowed" {
			t.Errorf("Host %s: %d %q, want 421 host_not_allowed", host, w.Code, w.Body.String())
		}
	}
	for _, host := range []string{"127.0.0.1:8484", "[::1]:8484", "localhost:8484", "LOCALHOST", "192.168.1.20", "BOXSET.home.example:8484"} {
		w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Host", host, "Authorization", "Bearer wrong")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Host %s: %d, want past the Host guard (401 for the wrong bearer)", host, w.Code)
		}
	}
	if n := in.users(t); n != 0 {
		t.Fatalf("%d users", n)
	}
	if w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Host", "boxset.home.example", "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("allowed host: %d %q", w.Code, w.Body.String())
	}
	// Once setup is closed the Host guard no longer applies to setup: a replay is 409.
	if w := request(in.h, "POST", "/api/v1/setup", credentials("other"), "Host", "evil.example", "Authorization", "Bearer "+in.token); w.Code != http.StatusConflict {
		t.Errorf("closed setup from another host: %d %q, want 409", w.Code, w.Body.String())
	}
}

func TestSetupRaceOneAdmin(t *testing.T) {
	in := newInstallation(t, "", "")
	ts := httptest.NewServer(in.h)
	t.Cleanup(ts.Close)
	const n = 20
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			req, err := http.NewRequest("POST", ts.URL+"/api/v1/setup", strings.NewReader(credentials("admin")))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+in.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}
	close(start)
	wg.Wait()
	created, completed := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			completed++
		default:
			t.Errorf("status %d among the concurrent submissions", s)
		}
	}
	if created != 1 || completed != n-1 || in.users(t) != 1 {
		t.Fatalf("%d concurrent valid submissions: %d x 201, %d x 409, %d users; want 1 x 201, %d x 409, 1 user", n, created, completed, in.users(t), n-1)
	}
	t.Logf("SETUP-RACE submissions=%d created=1 completed=%d users=1", n, completed)
}

func TestSetupTokenReplay(t *testing.T) {
	in := newInstallation(t, "", "")
	w := request(in.h, "POST", "/api/v1/setup", credentials("admin"), "Authorization", "Bearer "+in.token)
	if w.Code != http.StatusCreated {
		t.Fatalf("first: %d %q", w.Code, w.Body.String())
	}
	var created struct{ User api.User }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.User.Role != "admin" || created.User.Username != "admin" || created.User.ID < 1 {
		t.Fatalf("201 body %q (%v)", w.Body.String(), err)
	}
	for _, body := range []string{credentials("admin"), credentials("second")} {
		if w := request(in.h, "POST", "/api/v1/setup", body, "Authorization", "Bearer "+in.token); w.Code != http.StatusConflict || code(t, w) != "setup_completed" {
			t.Errorf("replay: %d %q, want 409 setup_completed", w.Code, w.Body.String())
		}
	}
	for _, bearer := range []string{"Bearer another-token-entirely", "Bearer " + in.token + "x", ""} {
		w := request(in.h, "POST", "/api/v1/setup", credentials("second"), "Authorization", bearer)
		if w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
			t.Errorf("bearer %q after setup: %d %q, want 401", bearer, w.Code, w.Body.String())
		}
	}
	if n := in.users(t); n != 1 {
		t.Fatalf("%d users after replays, want 1", n)
	}
}

func TestSetupSecretsNeverEscape(t *testing.T) {
	const tokenSentinel = "TOKEN-S3NT1NEL-7f3e9a"
	const passwordSentinel = "PASSWORD-S3NT1NEL-c41d"
	in := newInstallation(t, "", tokenSentinel)
	if in.token != tokenSentinel {
		t.Fatal("the env token was not armed")
	}
	responses := []*httptest.ResponseRecorder{
		request(in.h, "POST", "/api/v1/setup", `{"username":"admin","password":"`+passwordSentinel, "Authorization", "Bearer "+tokenSentinel),
		request(in.h, "POST", "/api/v1/setup", `{"username":"admin","password":"`+passwordSentinel+`","extra":"`+passwordSentinel+`"}`, "Authorization", "Bearer "+tokenSentinel),
		request(in.h, "POST", "/api/v1/setup", `{"username":"`+passwordSentinel+"\\u0000"+`","password":"`+passwordSentinel+`"}`, "Authorization", "Bearer "+tokenSentinel),
		request(in.h, "POST", "/api/v1/setup?password="+passwordSentinel, credentials("admin"), "Authorization", "Bearer "+tokenSentinel),
		request(in.h, "GET", "/api/v1/system/status?token="+tokenSentinel, ""),
		request(in.h, "POST", "/api/v1/setup", `{"username":"admin","password":"`+passwordSentinel+`"}`, "Authorization", "Bearer "+tokenSentinel+"-wrong"),
		request(in.h, "POST", "/api/v1/setup", `{"username":"admin","password":"`+passwordSentinel+`"}`, "Authorization", "Bearer "+tokenSentinel),
		request(in.h, "POST", "/api/v1/setup", `{"username":"admin","password":"`+passwordSentinel+`"}`, "Authorization", "Bearer "+tokenSentinel),
	}
	wantStatus := []int{400, 422, 422, 400, 400, 401, 201, 409}
	for i, w := range responses {
		if w.Code != wantStatus[i] {
			t.Errorf("request %d: %d %q, want %d", i, w.Code, w.Body.String(), wantStatus[i])
		}
		raw := w.Body.String()
		for k, v := range w.Header() {
			raw += k + strings.Join(v, ",")
		}
		if strings.Contains(raw, tokenSentinel) || strings.Contains(raw, passwordSentinel) || strings.Contains(w.Body.String(), `"value"`) {
			t.Errorf("request %d: a sentinel or an input value escaped: %q", i, raw)
		}
	}
	logs := in.logs.String()
	if strings.Contains(logs, tokenSentinel) || strings.Contains(logs, passwordSentinel) {
		t.Errorf("a sentinel reached the log: %s", logs)
	}
	for _, reason := range []string{`"reason":"setup_created"`, `"reason":"setup_completed"`} {
		if !strings.Contains(logs, reason) {
			t.Errorf("log lacks %s: %s", reason, logs)
		}
	}
	// The stored forms are a verifier and a PHC string only.
	var tokenColumn, phc string
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), "SELECT token_sha256 FROM setup").Scan(&tokenColumn); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), "SELECT password_phc FROM user").Scan(&phc)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tokenColumn+phc, "S3NT1NEL") || len(tokenColumn) != 64 || !strings.HasPrefix(phc, "$argon2id$") {
		t.Errorf("stored credential forms: verifier %d chars, phc prefix %.10q", len(tokenColumn), phc)
	}
	t.Logf("SETUP-SECRETS responses=%d log_bytes=%d sentinels=0", len(responses), len(logs))
}

func TestReferrerPolicyEverywhere(t *testing.T) {
	in := newInstallation(t, "/media", "")
	cases := []struct{ method, target, body string }{
		{"GET", "/media/ping", ""},
		{"GET", "/media/", ""},
		{"GET", "/media/setup", ""},
		{"GET", "/media/_app/immutable/plain.js", ""},
		{"GET", "/media/_app/nope.js", ""},
		{"GET", "/media/unlisted", ""},
		{"POST", "/media/", ""},
		{"GET", "/media/%2e%2e/", ""},
		{"GET", "/elsewhere", ""},
		{"GET", "/media/api/v1/nope", ""},
		{"GET", "/media/api/v1/openapi.json", ""},
		{"GET", "/media/api/v1/system/status?apikey=x", ""},
		{"POST", "/media/api/v1/setup", credentials("admin")},
		{"POST", "/media/api/v1/setup", `{"username":"admin"}`},
		{"POST", "/media/api/v1/setup", credentials("admin")},
		{"POST", "/media/api/v1/setup", credentials("admin")},
	}
	statuses := map[int]bool{}
	for i, c := range cases {
		header := []string{}
		if i >= 13 {
			header = []string{"Authorization", "Bearer " + in.token}
		}
		w := request(in.h, c.method, c.target, c.body, header...)
		statuses[w.Code] = true
		if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s (%d): Referrer-Policy %q nosniff %q", c.method, c.target, w.Code, w.Header().Get("Referrer-Policy"), w.Header().Get("X-Content-Type-Options"))
		}
	}
	for _, s := range []int{200, 201, 400, 401, 404, 405, 409, 422} {
		if !statuses[s] {
			t.Errorf("no response with status %d was checked", s)
		}
	}
}

// TestBypassSessionProjection: the direct-peer grant answers as its own
// credential with no invented user or expiry, reports the configured warning
// and, separately, whether a cookie behind it can still be revoked.
func TestBypassSessionProjection(t *testing.T) {
	in, cookie := signedIn(t, "")
	b := *in.s.backend.Load()
	b.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	in.s.Attach(b)

	session := func(t *testing.T, w *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("session=%d body=%s", w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	// elsewhere serves the session from a peer outside the configured ranges,
	// where the presented cookie is what gets authenticated.
	elsewhere := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/session", nil)
		r.Host = "127.0.0.1:8484"
		r.RemoteAddr = "192.0.2.9:50000"
		r.Header.Set("Cookie", cookie.Name+"="+cookie.Value)
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		return w
	}
	header := cookie.Name + "=" + cookie.Value

	// Bypass alone: explicit credential, no user, no expiry, no logout.
	body := session(t, request(in.h, "GET", "/api/v1/session", ""))
	if got := string(body["credential"]); got != `"local_bypass"` {
		t.Fatalf("credential=%s", got)
	}
	if _, exists := body["user"]; exists {
		t.Fatal("bypass fabricated a user")
	}
	if _, exists := body["expiresAt"]; exists {
		t.Fatal("bypass claimed an expiry")
	}
	if _, exists := body["restoreEpoch"]; !exists {
		t.Fatal("bypass omitted the restore epoch")
	}
	if string(body["localBypassEnabled"]) != "true" || string(body["canLogout"]) != "false" {
		t.Fatalf("bypass warning=%s logout=%s, want true and false", body["localBypassEnabled"], body["canLogout"])
	}

	// A valid cookie beside the grant reports only the separate logout fact.
	body = session(t, request(in.h, "GET", "/api/v1/session", "", "Cookie", header))
	if string(body["canLogout"]) != "true" {
		t.Fatalf("bypass with a validated cookie canLogout=%s", body["canLogout"])
	}
	if _, exists := body["user"]; exists {
		t.Fatal("bypass claimed the cookie's user")
	}

	// An invalid cookie never becomes authority and never defeats the grant.
	body = session(t, request(in.h, "GET", "/api/v1/session", "", "Cookie", cookie.Name+"=invalid"))
	if string(body["credential"]) != `"local_bypass"` || string(body["canLogout"]) != "false" {
		t.Fatalf("invalid cookie changed the grant: %v", body)
	}

	if w := elsewhere(); w.Code != 200 {
		t.Fatalf("cookie denied on an unconfigured peer before logout: %d", w.Code)
	}
	// Logout revokes that exact cookie; the bypass survives on this peer and the
	// revoked cookie is denied where no grant applies.
	if w := request(in.h, "DELETE", "/api/v1/session", "", "Cookie", header); w.Code != 204 {
		t.Fatalf("logout with a bypass cookie=%d body=%s", w.Code, w.Body.String())
	}
	if w := elsewhere(); w.Code != 401 {
		t.Fatalf("revoked cookie accepted on an unconfigured peer: %d", w.Code)
	}
	// A cookie-bearing principal on an ineligible peer still carries the warning
	// fact, its real user and its own logout authority.
	if w := request(in.h, "POST", "/api/v1/session", credentials("owner")); w.Code != 201 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login=%d", w.Code)
	} else {
		cookie = w.Result().Cookies()[0]
	}
	body = session(t, elsewhere())
	if string(body["credential"]) != `"cookie"` || string(body["canLogout"]) != "true" || string(body["localBypassEnabled"]) != "true" {
		t.Fatalf("cookie projection %v", body)
	}
	if _, exists := body["user"]; !exists {
		t.Fatal("cookie session lost its user")
	}

	// A key never carries logout authority, and the warning fact is independent.
	_, key := keyFor(t, in, "read")
	body = session(t, request(in.h, "GET", "/api/v1/session", "", "X-Api-Key", key))
	if string(body["credential"]) != `"key"` || string(body["canLogout"]) != "false" || string(body["localBypassEnabled"]) != "true" {
		t.Fatalf("key projection %v", body)
	}

	// Setup-open protected requests deny on a matching direct peer and never
	// consume the grant.
	fresh := newInstallation(t, "", "")
	fb := *fresh.s.backend.Load()
	fb.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	fresh.s.Attach(fb)
	if w := request(fresh.h, "GET", "/api/v1/session", ""); w.Code != 401 || code(t, w) != "unauthenticated" {
		t.Fatalf("setup-open direct peer session=%d body=%s", w.Code, w.Body.String())
	}
	t.Log("BYPASS-SESSION grant=local_bypass user=absent expiry=absent logout=cookie_only")
}

// TestGatedRoutesShareReadBudget requires the credential reads of gated routes to
// share one credential's read budget: a credential holding its whole cap has its
// authorized read refused with 429 read_cap, and the one status read a key-broken
// installation still answers is refused by the same budget and served once it has
// freed, so the cap and not the confinement is what refuses it.
func TestGatedRoutesShareReadBudget(t *testing.T) {
	in, cookie := signedIn(t, "")
	identity, err := in.s.backend.Load().Sessions.AuthenticateReadOnly(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	current := []string{"Cookie", cookie.Name + "=" + cookie.Value}
	var held []func()
	release := func() {
		for _, done := range held {
			done()
		}
		held = nil
	}
	defer release()
	// hold fills the whole read budget of the session's credential.
	for range readsPerCredential {
		rec := httptest.NewRecorder()
		done, ok := in.s.admission.admit(rec, admittedKey(identity))
		if !ok {
			t.Fatalf("holding a read of the credential = %d %q, want admitted", rec.Code, rec.Body.String())
		}
		held = append(held, done)
	}

	// The credential's own authorized read is refused by its cap.
	w := request(in.h, "GET", "/api/v1/users", "", current...)
	if w.Code != http.StatusTooManyRequests || code(t, w) != "read_cap" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("an authorized read past its own cap = %d %q, want 429 read_cap with Retry-After", w.Code, w.Body.String())
	}

	// The one status read a key-broken installation still answers is charged the same
	// way: refused by its credential's cap while that is full, and served once it is
	// not, so the cap and not the confinement is what refused it.
	recoveringWith(t, in, secret.ReasonMissing)
	w = request(in.h, "GET", statusPath, "", current...)
	if w.Code != http.StatusTooManyRequests || code(t, w) != "read_cap" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the confined status read past its own cap = %d %q, want 429 read_cap with Retry-After", w.Code, w.Body.String())
	}
	release()
	if w := request(in.h, "GET", statusPath, "", current...); w.Code != http.StatusOK {
		t.Fatalf("the confined status read with budget left = %d %q, want 200", w.Code, w.Body.String())
	}
}
