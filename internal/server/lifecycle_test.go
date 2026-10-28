package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/bounded"
	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/logs"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// TestLifecycleGateOrder requires the recorded gate order in every state: the
// public cue and the shell always; no credential read and no direct-peer grant
// while the authority is untrusted; login refused with Retry-After there and in
// key loss; every authenticated request except a current credential's status
// refused with 503 recovering while a key is lost; and in draining a safe read
// still passes while an unsafe method is refused with 503 draining.
func TestLifecycleGateOrder(t *testing.T) {
	in, cookie := signedIn(t, "/media")
	_, key := keyFor(t, in, "admin")
	session := []string{"Cookie", cookie.Name + "=" + cookie.Value}
	keyHeader := []string{"X-Api-Key", key}
	base := "/media"
	target := func(path string) string { return base + path }

	states := []struct {
		name string
		set  func()
	}{
		{"starting", func() { in.s.SetState(Starting) }},
		{"recovering/restore_in_progress", func() { recoveringWith(t, in, ReasonRestoreInProgress) }},
		{"recovering/key_missing", func() { recoveringWith(t, in, secret.ReasonMissing) }},
		{"recovering/key_mismatch", func() { recoveringWith(t, in, secret.ReasonMismatch) }},
		{"ready", func() { recoveringWith(t, in, ""); in.s.SetState(Ready) }},
		{"draining", func() { recoveringWith(t, in, ""); in.s.SetState(Draining) }},
	}
	for _, tc := range states {
		tc.set()
		want := string(in.s.currentState())
		if w := request(in.h, "GET", target("/ping"), ""); w.Code != http.StatusOK || w.Header().Get("X-Boxset-State") != want {
			t.Fatalf("%s: /ping = %d %q, want 200 %q", tc.name, w.Code, w.Header().Get("X-Boxset-State"), want)
		}
		for _, path := range []string{"/", "/setup", "/sign-in"} {
			if w := request(in.h, "GET", target(path), ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("%s: GET %s = %d, want the data-free shell", tc.name, path, w.Code)
			}
		}
	}

	// Untrusted phases: a current credential, an unknown one and the direct-peer
	// grant all read no credential, and login is refused with a retry hint and no
	// session.
	b := *in.s.backend.Load()
	for _, tc := range []struct {
		name string
		set  func()
	}{
		{"starting", func() { in.s.SetState(Starting) }},
		{"recovering/restore_in_progress", func() { recoveringWith(t, in, ReasonRestoreInProgress) }},
	} {
		tc.set()
		grant := *in.s.backend.Load()
		grant.LocalBypass = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		in.s.Attach(grant)
		for _, header := range [][]string{session, keyHeader} {
			for _, path := range []string{"/api/v1/system/status", "/api/v1/users", "/api/v1/session"} {
				w := request(in.h, "GET", target(path), "", header...)
				if w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
					t.Fatalf("%s: %s with a current credential = %d %q, want 401 unauthenticated", tc.name, path, w.Code, w.Body.String())
				}
			}
		}
		if w := request(in.h, "GET", target("/api/v1/users"), ""); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
			t.Fatalf("%s: the direct-peer grant = %d %q, want no grant", tc.name, w.Code, w.Body.String())
		}
		if w := request(in.h, "POST", target("/api/v1/session"), credentials("owner")); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || len(w.Result().Cookies()) != 0 {
			t.Fatalf("%s: login = %d cookies=%d, want 503 with Retry-After and no session", tc.name, w.Code, len(w.Result().Cookies()))
		}
		if w := request(in.h, "POST", target("/api/v1/setup"), credentials("admin"), "Authorization", "Bearer "+in.token); w.Code != http.StatusUnauthorized || code(t, w) != "setup_credential_invalid" {
			t.Fatalf("%s: setup = %d %q, want 401 setup_credential_invalid", tc.name, w.Code, w.Body.String())
		}
	}
	in.s.Attach(b)

	// Key loss: every authenticated request but a current credential's own status is
	// refused as recovering, and login never becomes a session.
	for _, reason := range []string{secret.ReasonMissing, secret.ReasonMismatch} {
		recoveringWith(t, in, reason)
		for _, header := range [][]string{session, keyHeader} {
			for _, path := range []string{"/api/v1/users", "/api/v1/session"} {
				w := request(in.h, "GET", target(path), "", header...)
				if w.Code != http.StatusServiceUnavailable || code(t, w) != string(Recovering) || w.Header().Get("Retry-After") == "" {
					t.Fatalf("%s: %s = %d %q, want 503 %q with Retry-After", reason, path, w.Code, w.Body.String(), Recovering)
				}
			}
			if w := request(in.h, "GET", target("/api/v1/system/status"), "", header...); w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d %q, want the one served request", reason, w.Code, w.Body.String())
			}
		}
		if w := request(in.h, "POST", target("/api/v1/session"), credentials("owner")); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || len(w.Result().Cookies()) != 0 {
			t.Fatalf("%s: login = %d cookies=%d, want 503 with Retry-After and no session", reason, w.Code, len(w.Result().Cookies()))
		}
	}

	// Draining: a safe read passes the lifecycle gate and an unsafe method does not.
	recoveringWith(t, in, "")
	in.s.SetState(Draining)
	if w := request(in.h, "GET", target("/api/v1/users"), "", session...); w.Code != http.StatusOK {
		t.Fatalf("draining: a current admin's GET /api/v1/users = %d %q, want 200", w.Code, w.Body.String())
	}
	if w := request(in.h, "PATCH", target("/api/v1/users/1"), "{}", session...); w.Code != http.StatusServiceUnavailable || code(t, w) != string(Draining) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("draining: PATCH = %d %q, want 503 %q with Retry-After", w.Code, w.Body.String(), Draining)
	}

	// A stop is not a repair: key loss confines the read routes in draining too.
	recoveringWith(t, in, secret.ReasonMissing)
	in.s.SetState(Draining)
	if w := request(in.h, "GET", target("/api/v1/users"), "", session...); w.Code != http.StatusServiceUnavailable || code(t, w) != string(Recovering) {
		t.Fatalf("draining with a lost key: GET /api/v1/users = %d %q, want 503 %q", w.Code, w.Body.String(), Recovering)
	}
	if w := request(in.h, "GET", target("/api/v1/system/status"), "", session...); w.Code != http.StatusOK {
		t.Fatalf("draining with a lost key: status = %d %q, want the one served request", w.Code, w.Body.String())
	}
}

// TestStatusDuringRecovery requires the recovering snapshot for a current
// credential while the key does not match, and refuses a credential that is
// missing, unknown, disabled or from an earlier restore epoch. Serving the
// snapshot only for key_missing would fail this case, which is why the mismatch
// reason is the one it keeps.
func TestStatusDuringRecovery(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	recoveringWith(t, in, secret.ReasonMismatch)
	for _, tc := range []struct {
		name   string
		header []string
	}{
		{"session", []string{"Cookie", cookie.Name + "=" + cookie.Value}},
		{"key", []string{"X-Api-Key", key}},
	} {
		w := request(in.h, "GET", statusPath, "", tc.header...)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
		var got api.Status
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.State != string(Recovering) || got.Reason != secret.ReasonMismatch {
			t.Fatalf("%s state=%q reason=%q, want %q and %q", tc.name, got.State, got.Reason, Recovering, secret.ReasonMismatch)
		}
		if got.GoMemLimitBytes < 1 || got.Timings.MigrationMs < 0 || got.Timings.RecoveryMs < 0 {
			t.Fatalf("%s reported timings or GOMEMLIMIT as %+v", tc.name, got)
		}
	}
	if w := request(in.h, "GET", statusPath, ""); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
		t.Fatalf("no credential status=%d body=%s, want 401 unauthenticated", w.Code, w.Body.String())
	}
	if w := request(in.h, "GET", statusPath, "", "X-Api-Key", "bxk_"+strings.Repeat("A", 43)); w.Code != http.StatusUnauthorized || code(t, w) != "credential_invalid" {
		t.Fatalf("an unknown key status=%d body=%s, want 401 credential_invalid", w.Code, w.Body.String())
	}
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE user SET disabled=1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := request(in.h, "GET", statusPath, "", "Cookie", cookie.Name+"="+cookie.Value); w.Code != http.StatusUnauthorized || code(t, w) != "credential_invalid" {
		t.Fatalf("a disabled credential status=%d body=%s, want 401 credential_invalid", w.Code, w.Body.String())
	}
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE user SET disabled=0"); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE session SET epoch = epoch + 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := request(in.h, "GET", statusPath, "", "Cookie", cookie.Name+"="+cookie.Value); w.Code != http.StatusUnauthorized || code(t, w) != "credential_invalid" {
		t.Fatalf("an earlier-epoch session status=%d body=%s, want 401 credential_invalid", w.Code, w.Body.String())
	}
	if w := request(in.h, "POST", "/api/v1/session", credentials("owner")); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("login status=%d, want 503 with Retry-After", w.Code)
	}
}

// TestReadCaps429 requires the recorded in-flight read caps to refuse at once with
// 429 read_cap and Retry-After: 8 for one credential and 256 for the server.
func TestReadCaps429(t *testing.T) {
	a := newAdmission()
	// The recorded acceptance number, not the constant that defines it: eight reads
	// of one credential are admitted and the ninth is refused.
	var release []func()
	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		done, ok := a.admit(rec, "cookie:1")
		if !ok {
			t.Fatalf("the %dth read of one credential = %d, want admitted", i+1, rec.Code)
		}
		release = append(release, done)
	}
	rec := httptest.NewRecorder()
	if _, ok := a.admit(rec, "cookie:1"); ok {
		t.Fatal("a ninth read of one credential was admitted")
	}
	if rec.Code != http.StatusTooManyRequests || code(t, rec) != "read_cap" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("ninth read = %d %q, want 429 read_cap with Retry-After", rec.Code, rec.Body.String())
	}
	// Another credential is not affected by that one's cap, and the slot pool has
	// headroom above one credential's share: a credential holding its whole cap
	// cannot take every reader slot from everyone else.
	rec = httptest.NewRecorder()
	other, ok := a.admit(rec, "cookie:2")
	if !ok {
		t.Fatalf("another credential's read while one credential holds its cap = %d %q, want admitted", rec.Code, rec.Body.String())
	}
	other()
	single := newAdmission()
	for _, key := range []string{"cookie:1", "cookie:2"} {
		rec := httptest.NewRecorder()
		done, ok := single.admit(rec, key)
		if !ok {
			t.Fatalf("%s = %d, want admitted", key, rec.Code)
		}
		done()
	}
	for _, done := range release {
		done()
	}
	rec = httptest.NewRecorder()
	done, ok := a.admit(rec, "cookie:1")
	if !ok {
		t.Fatalf("after the reads finished = %d, want admitted", rec.Code)
	}
	done()

	// The server's own cap refuses the request after all 256 are in flight.
	full := &admission{running: map[string]int{}, slots: make(chan struct{}, 256), wait: time.Second, total: 255}
	rec = httptest.NewRecorder()
	done, ok = full.admit(rec, "cookie:3")
	if !ok {
		t.Fatalf("the 256th read = %d, want admitted", rec.Code)
	}
	done()
	full.total = 256
	rec = httptest.NewRecorder()
	if _, ok := full.admit(rec, "cookie:4"); ok {
		t.Fatal("a read past the server's 256 was admitted")
	}
	if rec.Code != http.StatusTooManyRequests || code(t, rec) != "read_cap" {
		t.Fatalf("a read past the server cap = %d %q, want 429 read_cap", rec.Code, rec.Body.String())
	}
}

// TestRecordedRuntimeBounds requires the recorded acceptance numbers themselves
// rather than the identifiers that name them, so changing a bound fails here.
func TestRecordedRuntimeBounds(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.http.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", s.http.ReadHeaderTimeout)
	}
	if s.http.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout = %v, want 120s", s.http.IdleTimeout)
	}
	if s.http.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want 64 KiB", s.http.MaxHeaderBytes)
	}
	if s.routeDeadline != 30*time.Second {
		t.Fatalf("route deadline = %v, want 30s", s.routeDeadline)
	}
	// The server's own read timeout is the second half of that bound: a request
	// that sends its head and drips its body ends on any path, public-doc included.
	if s.http.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s", s.http.ReadTimeout)
	}
	a := newAdmission()
	if a.wait != 2*time.Second {
		t.Fatalf("reader wait = %v, want 2s", a.wait)
	}
	if cap(a.slots) != 32 {
		t.Fatalf("reader slots = %d, want 32", cap(a.slots))
	}
	if cap(a.slots) <= readsPerCredential*3 {
		t.Fatalf("%d reader slots for a %d in-flight cap leaves one credential most of the pool", cap(a.slots), readsPerCredential)
	}
}

// TestRecordedBufferBounds requires the recorded runtime buffer numbers themselves
// rather than the identifiers that name them, so changing a bound fails here exactly
// as a changed server bound fails above.
func TestRecordedBufferBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"bounded.LogRingCap", bounded.LogRingCap, 2 << 20},
		{"bounded.LogRingAge", bounded.LogRingAge, 24 * time.Hour},
		{"bounded.ReplayRingCap", bounded.ReplayRingCap, 1 << 20},
		{"bounded.ReplayRingAge", bounded.ReplayRingAge, 5 * time.Minute},
		{"bounded.ReleaseCacheCap", bounded.ReleaseCacheCap, 8 << 20},
		{"bounded.ReleaseTTL", bounded.ReleaseTTL, 15 * time.Minute},
		{"logs.FileCap", logs.FileCap, 8 << 20},
		{"logs.CrashCap", logs.CrashCap, 1 << 20},
		{"child.Cap", child.Cap, 64 << 10},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %v, want the recorded %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestReadCapOverHTTP drives nine authenticated reads of one credential through
// the server's own read admission over real HTTP: eight are held and the ninth
// answers 429 read_cap with Retry-After, so the bound holds in the assembled
// stack and not only in a bare counter.
func TestReadCapOverHTTP(t *testing.T) {
	in, cookie := signedIn(t, "")
	b := in.s.backend.Load()
	release := make(chan struct{})
	var held sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The credential is resolved exactly as the guard resolves it; the read then
		// holds its slot until this case releases it.
		value, err := r.Cookie(cookie.Name)
		if err != nil {
			http.Error(w, "no credential", http.StatusUnauthorized)
			return
		}
		identity, err := b.Sessions.AuthenticateReadOnly(r.Context(), value.Value)
		if err != nil {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}
		in.s.admit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			held.Done()
			<-release
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, r.WithContext(auth.WithSession(r.Context(), identity)))
	}))
	defer srv.Close()
	http.DefaultClient.CloseIdleConnections()
	held.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			req, err := http.NewRequest("GET", srv.URL+"/api/v1/users", nil)
			if err != nil {
				return
			}
			req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	held.Wait()
	req, err := http.NewRequest("GET", srv.URL+"/api/v1/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "read_cap") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the ninth read = %d %q, want 429 read_cap with Retry-After", resp.StatusCode, body)
	}
	close(release)
}

// TestReadCapIsPerCredential requires the recorded per-credential cap at the one
// admission placement: two resolved credentials behind one peer are budgeted apart,
// so one credential's eight in-flight reads do not refuse the other, the credential
// holding its whole share has its next read refused with 429 read_cap, and a request
// that resolves to no credential is answered 401 instead of being charged.
func TestReadCapIsPerCredential(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	sessionHeader := []string{"Cookie", cookie.Name + "=" + cookie.Value}
	keyHeader := []string{"X-Api-Key", key}

	// Eight reads of the key's resolved credential are in flight.
	identity, err := in.s.backend.Load().Keys.Authenticate(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for range readsPerCredential {
		rec := httptest.NewRecorder()
		done, ok := in.s.admission.admit(rec, admittedKey(identity))
		if !ok {
			t.Fatalf("holding a read of the key's credential = %d %q, want admitted", rec.Code, rec.Body.String())
		}
		held = append(held, done)
	}

	// The other credential on the same peer keeps its own budget.
	if w := request(in.h, "GET", "/api/v1/users", "", sessionHeader...); w.Code != http.StatusOK {
		t.Fatalf("another credential's read on the same peer = %d %q, want 200", w.Code, w.Body.String())
	}
	// A request that resolves to nothing is charged to no credential: it keeps its
	// recorded 401 even while another credential holds its whole cap.
	if w := request(in.h, "GET", "/api/v1/users", ""); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
		t.Fatalf("a credential-less read = %d %q, want 401 unauthenticated", w.Code, w.Body.String())
	}
	// The credential holding its cap is the one refused.
	w := request(in.h, "GET", "/api/v1/users", "", keyHeader...)
	if w.Code != http.StatusTooManyRequests || code(t, w) != "read_cap" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the key's ninth read = %d %q, want 429 read_cap with Retry-After", w.Code, w.Body.String())
	}
	// A credential that resolves to nothing keeps its own 401 as well.
	if w := request(in.h, "GET", "/api/v1/users", "", "Cookie", cookie.Name+"=no-such-session"); w.Code != http.StatusUnauthorized || code(t, w) != "credential_invalid" {
		t.Fatalf("a credential that resolves to no session = %d %q, want 401 credential_invalid", w.Code, w.Body.String())
	}
	// Its reads finished, the same credential is served again.
	for _, done := range held {
		done()
	}
	if w := request(in.h, "GET", "/api/v1/users", "", keyHeader...); w.Code != http.StatusOK {
		t.Fatalf("the key's read after its earlier reads finished = %d %q, want 200", w.Code, w.Body.String())
	}
}

// TestUnauthenticatedReadsKeepTheir401 requires AC-028's unauthenticated answer to
// survive saturation: with one credential's whole read cap and every reader slot
// held, a request that presents nothing, an unknown /api/ path and a dead cookie
// are still answered their recorded 401 through the assembled handler — never 429
// read_cap, never 503 reader_busy and never the shell.
func TestUnauthenticatedReadsKeepTheir401(t *testing.T) {
	in, cookie := signedIn(t, "")
	identity, err := in.s.backend.Load().Sessions.AuthenticateReadOnly(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	defer func() {
		for _, done := range held {
			done()
		}
	}()
	hold := func(key string, reads int) {
		t.Helper()
		for range reads {
			rec := httptest.NewRecorder()
			done, ok := in.s.admission.admit(rec, key)
			if !ok {
				t.Fatalf("holding a read of %s = %d %q, want admitted", key, rec.Code, rec.Body.String())
			}
			held = append(held, done)
		}
	}
	// One credential spends its whole read cap and every reader slot is taken.
	hold(admittedKey(identity), readsPerCredential)
	for i := 0; i < readerSlots-readsPerCredential; i++ {
		hold("cookie:slot-"+strconv.Itoa(i), 1)
	}
	in.s.admission.mu.Lock()
	full := in.s.admission.total
	in.s.admission.mu.Unlock()
	if full != readerSlots {
		t.Fatalf("%d reader slots are held, want all %d", full, readerSlots)
	}

	for _, tc := range []struct {
		what, path string
		header     []string
		want       string
	}{
		{"a request that presents nothing", "/api/v1/users", nil, "unauthenticated"},
		{"an unknown API path", "/api/v1/unknown", nil, "unauthenticated"},
		{"a dead cookie", "/api/v1/users", []string{"Cookie", cookie.Name + "=no-such-session"}, "credential_invalid"},
	} {
		w := request(in.h, "GET", tc.path, "", tc.header...)
		if w.Code != http.StatusUnauthorized || code(t, w) != tc.want {
			t.Fatalf("%s under saturation = %d %q, want 401 %s", tc.what, w.Code, w.Body.String(), tc.want)
		}
		notShell(t, tc.what, w)
	}
}

// TestLookupWaitIsBounded requires the credential read a request performs before it
// is admitted to be bounded: with every reader connection of the store held by a
// real read, more unauthenticated principal requests than the lookup gate admits
// each answer the recorded 401 unauthenticated inside the read wait, none of them
// waits for a connection, and every lookup hands its reservation back.
func TestLookupWaitIsBounded(t *testing.T) {
	in, _ := signedIn(t, "")

	// Every reader connection is taken by a read that only this case ends, so a
	// credential lookup that reached the store would queue inside database/sql.
	held := make(chan struct{})
	var holders sync.WaitGroup
	taken := make(chan struct{}, store.ReaderCount())
	for range store.ReaderCount() {
		holders.Add(1)
		go func() {
			defer holders.Done()
			_ = in.db.Read(context.Background(), func(*sql.Tx) error {
				taken <- struct{}{}
				<-held
				return nil
			})
		}()
	}
	t.Cleanup(func() {
		close(held)
		holders.Wait()
	})
	for range store.ReaderCount() {
		select {
		case <-taken:
		case <-time.After(5 * time.Second):
			t.Fatal("a reader connection was never taken, so no lookup could queue")
		}
	}

	const lookups = 2 * readsPerCredential // more than the per-peer lookup cap admits
	answered := make(chan *httptest.ResponseRecorder, lookups)
	for range lookups {
		go func() { answered <- request(in.h, "GET", "/api/v1/users", "") }()
	}
	// The bound the case allows: the recorded wait, plus the same again for the
	// deadline to be observed and every answer to arrive.
	bound := 2 * readWait
	expired := time.After(bound)
	for i := range lookups {
		select {
		case w := <-answered:
			if w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
				t.Fatalf("unauthenticated request %d behind a held reader = %d %q, want 401 unauthenticated", i+1, w.Code, w.Body.String())
			}
			notShell(t, "an unauthenticated request behind a held reader", w)
		case <-expired:
			t.Fatalf("%d of %d unauthenticated requests answered within %v, want all of them: a lookup waited for a reader connection instead of its own bound", i, lookups, bound)
		}
	}

	// Every lookup returned its reservation, so the reads of a resolved credential are
	// not left queueing behind a lookup that already ended.
	in.s.admission.mu.Lock()
	buckets, reservations := len(in.s.admission.running), in.s.admission.total
	in.s.admission.mu.Unlock()
	if buckets != 0 || reservations != 0 {
		t.Fatalf("the lookups left %d peer buckets and %d reservations behind, want none", buckets, reservations)
	}
}

// TestRecoveringStatusLookupIsBounded requires the credential read the one route a
// confined installation serves to be inside the same lookup gate as the ready path:
// with every reader connection of the store held, more unauthenticated status reads
// than the gate admits per peer each answer the recorded 401 unauthenticated inside
// the read wait, the request over the count is refused at once instead of waiting
// that wait out, and every lookup hands its reservation back.
func TestRecoveringStatusLookupIsBounded(t *testing.T) {
	in, _ := signedIn(t, "")
	recoveringWith(t, in, secret.ReasonMissing)

	// Every reader connection is taken by a read that only this case ends, so a
	// credential lookup that reached the store would queue inside database/sql.
	held := make(chan struct{})
	var holders sync.WaitGroup
	taken := make(chan struct{}, store.ReaderCount())
	for range store.ReaderCount() {
		holders.Add(1)
		go func() {
			defer holders.Done()
			_ = in.db.Read(context.Background(), func(*sql.Tx) error {
				taken <- struct{}{}
				<-held
				return nil
			})
		}()
	}
	t.Cleanup(func() {
		close(held)
		holders.Wait()
	})
	for range store.ReaderCount() {
		select {
		case <-taken:
		case <-time.After(5 * time.Second):
			t.Fatal("a reader connection was never taken, so no lookup could queue")
		}
	}

	// One more than the per-peer count: the lookups the gate admits wait out their
	// own bound behind the held readers, so only the count can answer sooner.
	const lookups = readsPerCredential + 1
	answered := make(chan *httptest.ResponseRecorder, lookups)
	start := time.Now()
	for range lookups {
		go func() { answered <- request(in.h, "GET", statusPath, "") }()
	}

	// The bound the case allows: the recorded wait, plus the same again for the
	// deadline to be observed and every answer to arrive.
	bound := time.After(2 * readWait)
	var first time.Duration
	for i := range lookups {
		select {
		case w := <-answered:
			if i == 0 {
				first = time.Since(start)
			}
			if w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
				t.Fatalf("unauthenticated status read %d behind a held reader = %d %q, want 401 unauthenticated", i+1, w.Code, w.Body.String())
			}
			notShell(t, "an unauthenticated status read behind a held reader", w)
		case <-bound:
			t.Fatalf("%d of %d unauthenticated status reads answered within %v, want all of them: a lookup waited for a reader connection instead of its own bound", i, lookups, 2*readWait)
		}
	}

	// The count refusal is the first answer: a lookup that took no count waits the
	// recorded read wait behind the held readers like the rest, so it would answer
	// only after the wait the refusal is supposed to spare it.
	if first > readWait/4 {
		t.Fatalf("the first unauthenticated status read answered after %v, want the per-peer count refused well inside the %v wait", first, readWait)
	}

	// Every lookup returned its reservation, so the read of the credential this route
	// resolved is not left queueing behind a lookup that already ended.
	in.s.admission.mu.Lock()
	buckets, reservations := len(in.s.admission.running), in.s.admission.total
	in.s.admission.mu.Unlock()
	if buckets != 0 || reservations != 0 {
		t.Fatalf("the lookups left %d peer buckets and %d reservations behind, want none", buckets, reservations)
	}
}

// TestLookupCountDoesNotOverlapTheRoute requires the lookup's count to end before the
// route takes its own: a request served while the server has one in-flight
// reservation left is admitted by both gates in turn, so it is served and leaves the
// counters as it found them, where a lookup that kept its count would have the
// route's own read refused past the global cap.
func TestLookupCountDoesNotOverlapTheRoute(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	for _, header := range [][]string{
		{"Cookie", cookie.Name + "=" + cookie.Value},
		{"X-Api-Key", key},
	} {
		// The last global reservation is free: the lookup takes it and must give it back
		// before the route's read takes its own.
		in.s.admission.mu.Lock()
		in.s.admission.total = readsGlobal - 1
		in.s.admission.mu.Unlock()
		if w := request(in.h, "GET", "/api/v1/users", "", header...); w.Code != http.StatusOK {
			t.Fatalf("an authorized read with the last reservation free = %d %q, want 200", w.Code, w.Body.String())
		}
		in.s.admission.mu.Lock()
		buckets, reservations := len(in.s.admission.running), in.s.admission.total
		in.s.admission.total = 0
		in.s.admission.mu.Unlock()
		if buckets != 0 || reservations != readsGlobal-1 {
			t.Fatalf("the served request left %d buckets and %d reservations, want none past the seeded %d", buckets, reservations, readsGlobal-1)
		}
	}
}

// TestReaderWait503 requires a request that has to wait for a reader slot to wait
// at most the recorded 2 s and then be refused with 503 reader_busy and a retry
// hint, not held for the life of the read that is in front of it.
func TestReaderWait503(t *testing.T) {
	a := newAdmission()
	a.wait = 50 * time.Millisecond
	var release []func()
	for i := range readerSlots {
		rec := httptest.NewRecorder()
		done, ok := a.admit(rec, "cookie:"+strconv.Itoa(i))
		if !ok {
			t.Fatalf("reader slot %d = %d, want admitted", i, rec.Code)
		}
		release = append(release, done)
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	_, ok := a.admit(rec, "cookie:waiting")
	waited := time.Since(start)
	if ok {
		t.Fatal("a read was admitted with every reader slot held")
	}
	if rec.Code != http.StatusServiceUnavailable || code(t, rec) != "reader_busy" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("a held reader = %d %q, want 503 reader_busy with Retry-After", rec.Code, rec.Body.String())
	}
	if waited < a.wait {
		t.Fatalf("refused after %v, before the %v wait was over", waited, a.wait)
	}
	// A released slot serves the next request: the refusal is the wait, not the key.
	release[0]()
	rec = httptest.NewRecorder()
	done, ok := a.admit(rec, "cookie:waiting")
	if !ok {
		t.Fatalf("after a slot freed = %d, want admitted", rec.Code)
	}
	done()
	for _, done := range release[1:] {
		done()
	}
}

// TestSlowlorisBounded requires held slow sockets to cost a bounded number of
// goroutines and no service: a sibling request is still answered while every slow
// socket sits in its header read.
func TestSlowlorisBounded(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	base := runtime.NumGoroutine()
	const slow = 32
	var conns []net.Conn
	for range slow {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		if _, err := conn.Write([]byte("GET /ping HTTP/1.1\r\nHost: 127.0.0.1\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/ping")
	if err != nil {
		t.Fatalf("a slow socket starved the server: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Boxset-State") != string(Starting) {
		t.Fatalf("sibling /ping = %d %q", resp.StatusCode, resp.Header.Get("X-Boxset-State"))
	}
	if got, want := runtime.NumGoroutine(), base+slow+2*slow; got > want {
		t.Fatalf("%d goroutines with %d slow sockets and a base of %d, want at most %d", got, slow, base, want)
	}
}

// TestRouteDeadline requires an authorized route's response to carry a write
// deadline: a handler that outlives it does not deliver its body.
func TestRouteDeadline(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	s.routeDeadline = 50 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	srv := &http.Server{Handler: s.admit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, err := w.Write([]byte("late"))
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		written <- err
	}))}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if writeErr := <-written; writeErr == nil {
		t.Fatal("a response written past the route deadline was delivered")
	}
}

// TestSecretInURLKeepsItsReasonCode requires the secret-in-query refusal to reach
// a keep-alive caller that has already made a request on the same connection, with
// its recorded reason code.
func TestSecretInURLKeepsItsReasonCode(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	guard := s.guard(http.NewServeMux(), nil)
	srv := httptest.NewServer(guard)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatalf("the request before the refusal: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Get(srv.URL + "/api/v1/users?token=s3cret")
	if err != nil {
		t.Fatalf("the secret-in-query refusal did not reach the caller: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "secret_in_url") {
		t.Fatalf("the secret-in-query refusal = %d %q, want 400 secret_in_url", resp.StatusCode, body)
	}
}

// TestSecretInURLIsRouteBounded requires the secret-in-query refusal to run under
// the route deadline: with a deadline that has already passed, the refusal is not
// delivered, so that refusal is bounded by the same deadline as every other guarded
// path instead of answering outside it.
func TestSecretInURLIsRouteBounded(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	s.routeDeadline = time.Nanosecond
	srv := httptest.NewServer(s.guard(http.NewServeMux(), nil))
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(srv.URL + "/api/v1/users?token=s3cret")
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil && resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "secret_in_url") {
			t.Fatal("the secret-in-query refusal was delivered past the route deadline")
		}
	}
}

// TestReadDeadlineEndsUnfinishedBody requires a request that sent its head and
// never sends its body to end inside the route deadline and hand its reader slot
// back: without it, that request holds the slot until the client goes away.
func TestReadDeadlineEndsUnfinishedBody(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	s.routeDeadline = 100 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	// The read has to be one the admission placement charges, or it would hold no
	// reader slot and prove nothing: a resolved credential is put in the context
	// exactly as the guard puts it there.
	identity := auth.Session{ID: 1, Credential: "cookie"}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.admit(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, err := io.Copy(io.Discard, r.Body)
			read <- err
		})).ServeHTTP(w, r.WithContext(auth.WithSession(r.Context(), identity)))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The head promises a body this client never sends.
	if _, err := conn.Write([]byte("POST / HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a body that never arrived was read to completion")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read deadline did not end a body that never arrived")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.admission.mu.Lock()
		holding := s.admission.total
		s.admission.mu.Unlock()
		if holding == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reader slots stayed reserved after the deadline ended the read", holding)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPublicDocDeadline requires the guard's own fast path to carry the route
// deadline too: a public-doc response that stalls past it is not delivered, so the
// rows that serve in every state are bounded like an authorized route.
func TestPublicDocDeadline(t *testing.T) {
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	s.routeDeadline = 50 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stalled", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, err := w.Write([]byte("late"))
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		written <- err
	})
	routes := map[string]api.Route{"GET /stalled": {Pattern: "GET /stalled", Class: api.ClassPublicDoc}}
	srv := &http.Server{Handler: s.guard(mux, routes)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	client := &http.Client{Timeout: 5 * time.Second}
	if resp, err := client.Get("http://" + ln.Addr().String() + "/stalled"); err == nil {
		_ = resp.Body.Close()
	}
	if writeErr := <-written; writeErr == nil {
		t.Fatal("a public-doc response written past the route deadline was delivered")
	}
}

// TestRunStateRecord requires the run state `boxset healthcheck` reads to
// round-trip every lifecycle state, and an unreadable word to be refused rather
// than reported as a state.
func TestRunStateRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadRunState(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a directory no server started in = %v, want fs.ErrNotExist", err)
	}
	for _, st := range []State{Starting, Recovering, Ready, Draining} {
		if err := WriteRunState(dir, st); err != nil {
			t.Fatal(err)
		}
		got, err := ReadRunState(dir)
		if err != nil || got != st {
			t.Fatalf("recorded %q read as %q, %v", st, got, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, RunStateFileName), []byte("readyy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRunState(dir); !errors.Is(err, ErrRunState) {
		t.Fatalf("an unknown state word = %v, want ErrRunState", err)
	}
	if err := WriteRunState(dir, State("nonsense")); !errors.Is(err, ErrRunState) {
		t.Fatalf("recording an unknown state = %v, want ErrRunState", err)
	}
}
