package auth

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func sessionFixture(t *testing.T, name string) *Sessions {
	t.Helper()
	dir := t.TempDir()
	db := openStore(t, dir)
	t.Cleanup(func() { _ = db.Close() })
	setup := NewSetup(db, NewHasher(), dir)
	if _, err := setup.Arm(t.Context(), "synthetic-session-setup"); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Create(t.Context(), Verifier("synthetic-session-setup"), name, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return setup.Sessions()
}

func TestWrongPasswordGeneric(t *testing.T) {
	for _, pair := range [][2]string{{"Émilie", "E\u0301MILIE"}, {"Σίσυφος", "ΣΊΣΥΦΟΣ"}, {"Straße", "STRASSE"}} {
		t.Run(pair[0], func(t *testing.T) {
			s := sessionFixture(t, pair[0])
			identity, token, _, err := s.Login(t.Context(), pair[1], "correct horse battery")
			if err != nil || identity.User.ID != 1 || identity.User.Username != pair[0] {
				t.Fatalf("canonical login id=%d err=%v", identity.User.ID, err)
			}
			if len(token) != 43 {
				t.Fatal("session token entropy encoding")
			}
			for _, attempt := range [][2]string{{pair[1], "wrong password"}, {"absent", "correct horse battery"}, {"Emilie", "correct horse battery"}} {
				if _, _, _, err := s.Login(t.Context(), attempt[0], attempt[1]); !errors.Is(err, ErrCredentialsInvalid) {
					t.Fatalf("generic denial: %v", err)
				}
			}
			if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET disabled=1 WHERE id=1"); return e }); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := s.Login(t.Context(), pair[1], "correct horse battery"); !errors.Is(err, ErrCredentialsInvalid) {
				t.Fatal("disabled login not denied")
			}
			if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
				t.Fatal("disabled session not denied")
			}
		})
	}
}

func TestSlidingExpiry(t *testing.T) {
	s := sessionFixture(t, "owner")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	identity, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	readExpiry := func() int64 {
		t.Helper()
		var expires int64
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT expires_at FROM session WHERE id=?", identity.ID).Scan(&expires)
		}); err != nil {
			t.Fatal(err)
		}
		return expires
	}
	initial := readExpiry()
	now = now.Add(59 * time.Minute)
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if readExpiry() != initial {
		t.Fatal("expiry persisted before hour")
	}
	now = now.Add(time.Minute)
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if readExpiry() != time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC).Unix() {
		t.Fatal("hourly sliding expiry not persisted")
	}
	for range 59 {
		now = now.Add(time.Second)
		if _, err := s.Authenticate(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	if readExpiry() != time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC).Unix() {
		t.Fatal("per-request expiry write")
	}
	now = now.Add(30 * 24 * time.Hour)
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("expired session accepted")
	}
}

func TestSessionCurrentRightsEpochAndStorage(t *testing.T) {
	s := sessionFixture(t, "owner")
	_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	var verifier string
	if err := s.db.Read(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow("SELECT token_sha256 FROM session").Scan(&verifier) }); err != nil {
		t.Fatal(err)
	}
	if len(verifier) != 64 || strings.Contains(verifier, token) {
		t.Fatal("plaintext credential persisted")
	}
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET role='viewer',revision=revision+1"); return e }); err != nil {
		t.Fatal(err)
	}
	identity, err := s.Authenticate(t.Context(), token)
	if err != nil || len(identity.Rights) != 1 || identity.Rights[0] != "status:read" {
		t.Fatal("current rights not applied")
	}
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE installation SET restore_epoch=restore_epoch+1")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("old epoch accepted")
	}
}

func TestDelayedLogoutCannotRevokeReusedSessionID(t *testing.T) {
	s := sessionFixture(t, "owner")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	_, oldToken, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Authenticate(t.Context(), oldToken)
	if err != nil {
		t.Fatal(err)
	}
	delayed, err := s.Authenticate(t.Context(), oldToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	fresh, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID != delayed.ID {
		t.Fatal("fixture did not reuse maximum row ID")
	}
	now = now.Add(30 * time.Minute)
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(t.Context(), delayed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("stale logout revoked new session: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), oldToken); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("original session was not revoked")
	}
	// The delayed logout must also preserve the fresh session's pending activity.
	now = now.Add(SessionTTL - 15*time.Minute)
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("stale logout erased new expiry metadata: %v", err)
	}
}

func TestSlidingExpiryAfterDelayedBatchAndReconstruction(t *testing.T) {
	s := sessionFixture(t, "owner")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	a, tokenA, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	_, tokenB, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	readExpiry := func() int64 {
		t.Helper()
		var expires int64
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT expires_at FROM session WHERE id=?", a.ID).Scan(&expires)
		}); err != nil {
			t.Fatal(err)
		}
		return expires
	}
	now = now.Add(time.Minute)
	if _, err := s.Authenticate(t.Context(), tokenA); err != nil {
		t.Fatal(err)
	}
	activity := now
	now = now.Add(59 * time.Minute)
	if _, err := s.Authenticate(t.Context(), tokenB); err != nil {
		t.Fatal(err)
	}
	persisted := readExpiry()
	if persisted != activity.Add(SessionTTL).Unix() {
		t.Fatal("cross-session batch did not retain A activity")
	}
	// Recreate exactly the public Setup.Sessions service, retaining the database.
	s = NewSetup(s.db, s.hasher, t.TempDir()).Sessions()
	s.now = func() time.Time { return now }
	now = now.Add(time.Minute)
	current, err := s.Authenticate(t.Context(), tokenA)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ExpiresAt.Equal(now.Add(SessionTTL)) {
		t.Fatal("reconstruction lost sliding activity")
	}
	if readExpiry() != persisted {
		t.Fatal("reconstruction persisted A twice one minute apart")
	}
	now = now.Add(59*time.Minute + 59*time.Second)
	if _, err := s.Authenticate(t.Context(), tokenA); err != nil {
		t.Fatal(err)
	}
	if readExpiry() != persisted {
		t.Fatal("reconstruction persisted before conservative hour")
	}
	now = now.Add(time.Second)
	if _, err := s.Authenticate(t.Context(), tokenA); err != nil {
		t.Fatal(err)
	}
	if readExpiry() != now.Add(SessionTTL).Unix() {
		t.Fatal("reconstructed session never persisted useful activity")
	}
}

func TestSessionExpiryBatchMinuteBound(t *testing.T) {
	s := sessionFixture(t, "owner")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }
	_, tokenA, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(30 * time.Second)
	b, tokenB, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Hour)
	if _, err := s.Authenticate(t.Context(), tokenA); err != nil {
		t.Fatal(err)
	}
	readExpiry := func() int64 {
		t.Helper()
		var expiry int64
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT expires_at FROM session WHERE id=?", b.ID).Scan(&expiry)
		}); err != nil {
			t.Fatal(err)
		}
		return expiry
	}
	initial := readExpiry()
	for _, seconds := range []int{30, 59} {
		now = start.Add(time.Hour + time.Duration(seconds)*time.Second)
		if _, err := s.Authenticate(t.Context(), tokenB); err != nil {
			t.Fatal(err)
		}
		if readExpiry() != initial {
			t.Fatal("cross-session expiry commit before minute boundary")
		}
	}
	now = start.Add(time.Hour + time.Minute)
	if _, err := s.Authenticate(t.Context(), tokenB); err != nil {
		t.Fatal(err)
	}
	if readExpiry() != now.Add(SessionTTL).Unix() {
		t.Fatal("eligible expiry batch did not commit at minute boundary")
	}
}
