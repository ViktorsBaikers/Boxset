package auth

// The provider side of the current-authority contract: a ref is decided by the
// same stored rows the guard read, and re-running the decision returns what the
// guard's own rule grants. A downgrade, disable, sign-out, key revocation, epoch
// change or expiry makes it fail closed; an untouched administrator and the
// direct-peer grant keep their rights.

import (
	"database/sql"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const revalidatePassword = "synthetic-valid-password"

type revalidateFixture struct {
	t        *testing.T
	db       *store.DB
	sessions *Sessions
	keys     *Keys
	now      time.Time
	admin    Session
	viewer   Session
}

func newRevalidateFixture(t *testing.T) *revalidateFixture {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hasher := NewHasher()
	if _, err := CreateUser(ctx, db, hasher, "admin", revalidatePassword, RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := CreateUser(ctx, db, hasher, "viewer", revalidatePassword, RoleAdmin); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	sessions := NewSetup(db, hasher, dir).Sessions()
	admin, _, _, err := sessions.Login(ctx, "admin", revalidatePassword)
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	viewer, _, _, err := sessions.Login(ctx, "viewer", revalidatePassword)
	if err != nil {
		t.Fatalf("login viewer: %v", err)
	}
	return &revalidateFixture{t: t, db: db, sessions: sessions, keys: sessions.Keys(), now: time.Now().UTC(), admin: admin, viewer: viewer}
}

// TestRevalidateCurrentAuthority is the one revalidator's own proof: the rights
// it returns are the guard's decision for the same provenance, and every way a
// grant ends makes it refuse.
func TestRevalidateCurrentAuthority(t *testing.T) {
	ctx := t.Context()
	f := newRevalidateFixture(t)

	sessionPrincipal, expires, err := Revalidate(ctx, f.db, SessionRef(f.admin), f.now)
	if err != nil || !sessionPrincipal.Has(RightUsersWrite) || !sessionPrincipal.Has(RightUsersRead) || !sessionPrincipal.Has(RightStatusRead) {
		t.Fatalf("admin session ref = %+v, %v, want the admin rights", sessionPrincipal, err)
	}
	if !expires.Equal(f.admin.ExpiresAt) {
		t.Fatalf("session expiry = %s, want %s", expires, f.admin.ExpiresAt)
	}

	keyID, token, err := f.keys.Create(ctx, "admin", "test", "read")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keySession, err := f.keys.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate key: %v", err)
	}
	keyPrincipal, keyExpires, err := Revalidate(ctx, f.db, KeyRef(keySession), f.now)
	if err != nil || !keyPrincipal.Has(RightStatusRead) || !keyPrincipal.Has(RightUsersRead) || keyPrincipal.Has(RightUsersWrite) {
		t.Fatalf("key ref = %+v, %v, want the read scope's rights", keyPrincipal, err)
	}
	if !keyExpires.IsZero() {
		t.Fatalf("key expiry = %s, want none", keyExpires)
	}

	// The direct-peer grant is re-decided from the peer and the configuration the
	// ref kept; a peer only a trusted proxy presents gets no ref at all.
	local := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	bypass, ok := BypassRef("127.0.0.1:40000", local, trusted)
	if !ok {
		t.Fatal("a direct peer in the bypass list got no ref")
	}
	bypass = bypass.WithEpoch(f.admin.RestoreEpoch)
	bypassPrincipal, bypassExpires, err := Revalidate(ctx, f.db, bypass, f.now)
	if err != nil || !bypassPrincipal.Has(RightUsersWrite) || !bypassExpires.IsZero() {
		t.Fatalf("bypass ref = %+v, %v, want the grant's admin rights", bypassPrincipal, err)
	}
	if _, ok := BypassRef("10.1.2.3:40000", local, trusted); ok {
		t.Fatal("a trusted-proxy peer minted a direct-peer ref")
	}
	if _, ok := BypassRef("127.0.0.1:40000", local, append(trusted, netip.MustParsePrefix("127.0.0.1/32"))); ok {
		t.Fatal("a peer that is also a trusted proxy minted a direct-peer ref")
	}
	// The grant's counting identity is the address, never the ephemeral port: the
	// same peer on a new connection stays one identity, so a per-identity cap
	// cannot be rotated by reconnecting.
	reconnected, ok := BypassRef("127.0.0.1:50001", local, trusted)
	if !ok {
		t.Fatal("the same direct peer on another port got no ref")
	}
	if bypass.Identity() != reconnected.Identity() {
		t.Fatalf("the direct-peer identity changed with the port: %q and %q", bypass.Identity(), reconnected.Identity())
	}
	if strings.Contains(bypass.Identity(), ":40000") {
		t.Fatalf("the direct-peer identity carries a port: %q", bypass.Identity())
	}

	// A downgrade the guard would refuse is refused here: the accepted right set
	// no longer matches the current role.
	viewerRef := SessionRef(f.viewer)
	if _, err := UpdateUser(ctx, f.db, f.viewer.User.ID, UserPatch{ExpectedRevision: f.viewer.User.Revision, Role: ptrValue(RoleViewer)}); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if _, _, err := Revalidate(ctx, f.db, viewerRef, f.now); err == nil {
		t.Fatal("a downgraded session stayed current")
	}

	// A disable is refused even for a still-existing row and a still-wide role.
	disabled, token2, _, err := f.sessions.Login(ctx, "viewer", revalidatePassword)
	if err != nil {
		t.Fatalf("re-login viewer: %v", err)
	}
	_ = token2
	disabledRef := SessionRef(disabled)
	if _, err := UpdateUser(ctx, f.db, disabled.User.ID, UserPatch{ExpectedRevision: disabled.User.Revision, Disabled: ptrValue(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := Revalidate(ctx, f.db, disabledRef, f.now); err == nil {
		t.Fatal("a disabled session stayed current")
	}

	// Signing out deletes the row.
	loggedOut := SessionRef(f.viewer)
	if err := f.sessions.Logout(ctx, f.viewer); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, _, err := Revalidate(ctx, f.db, loggedOut, f.now); err == nil {
		t.Fatal("a signed-out session stayed current")
	}

	// Revoking the key deletes its current row, so the same ref is refused.
	keyRef := KeyRef(keySession)
	if err := f.keys.Revoke(ctx, keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := Revalidate(ctx, f.db, keyRef, f.now); err == nil {
		t.Fatal("a revoked key stayed current")
	}

	// Expiry is decided by the accepted effective expiry, not by the stored
	// row's older value.
	if _, _, err := Revalidate(ctx, f.db, SessionRef(f.admin), f.now.Add(SessionTTL)); err == nil {
		t.Fatal("an expired session stayed current")
	}

	// A restore epoch the credential was not issued under ends it.
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE installation SET restore_epoch = restore_epoch + 1`)
		return err
	}); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	if _, _, err := Revalidate(ctx, f.db, SessionRef(f.admin), f.now); err == nil {
		t.Fatal("a credential from another epoch stayed current")
	}
	if _, _, err := Revalidate(ctx, f.db, bypass, f.now); err == nil {
		t.Fatal("a direct-peer ref from another epoch stayed current")
	}
	// The same grant re-minted under the new epoch is current.
	fresh, _, _, err := f.sessions.Login(ctx, "admin", revalidatePassword)
	if err != nil {
		t.Fatalf("fresh login: %v", err)
	}
	if principal, _, err := Revalidate(ctx, f.db, SessionRef(fresh), f.now); err != nil || !principal.Has(RightUsersWrite) {
		t.Fatalf("a fresh session after the epoch change = %+v, %v", principal, err)
	}

	// The same grant re-minted under the current epoch is current.
	current, _ := BypassRef("127.0.0.1:40000", local, trusted)
	current = current.WithEpoch(fresh.RestoreEpoch)
	if principal, _, err := Revalidate(ctx, f.db, current, f.now); err != nil || !principal.Has(RightUsersWrite) {
		t.Fatalf("the direct-peer grant under the current epoch = %+v, %v", principal, err)
	}

	// Setup being open grants no direct-peer identity: the one-time setup
	// credential is the only authority an installation without a user has.
	open := t.TempDir()
	openDB, err := store.Open(ctx, filepath.Join(open, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = openDB.Close() }()
	setup := NewSetup(openDB, NewHasher(), open)
	if _, err := setup.Arm(ctx, ""); err != nil {
		t.Fatalf("arm setup: %v", err)
	}
	epoch, err := setup.Sessions().Epoch(ctx)
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	openRef, _ := BypassRef("127.0.0.1:40000", local, trusted)
	if _, _, err := Revalidate(ctx, openDB, openRef.WithEpoch(epoch), f.now); err == nil {
		t.Fatal("an installation with setup open accepted the direct-peer grant")
	}
}

func ptrValue[T any](v T) *T { return &v }
