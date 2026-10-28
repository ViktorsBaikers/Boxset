package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

const fixturePassword = "synthetic-valid-password"

// access is one migrated installation with its open store and key-flow services.
type access struct {
	t      *testing.T
	layout install.Layout
	db     *store.DB
	hasher *Hasher
}

func openAccess(t *testing.T, dir string) *access {
	t.Helper()
	a := &access{t: t, layout: install.Layout{Dir: dir}, hasher: NewHasher()}
	a.open()
	t.Cleanup(func() { _ = a.db.Close() })
	return a
}

// open (re)connects the store to the file in the data directory. Every restore
// replaces that file, so the fixture reopens it as a restarted server would.
func (a *access) open() {
	a.t.Helper()
	db, err := store.Open(a.t.Context(), a.layout.DB(), store.Options{})
	if err != nil {
		a.t.Fatal(err)
	}
	a.db = db
}

// reopen closes the store and opens the replaced file.
func (a *access) reopen() {
	a.t.Helper()
	_ = a.db.Close()
	a.open()
}

// admin creates one account with a real verifier.
func (a *access) admin(name, role string) User {
	a.t.Helper()
	u, err := CreateUser(a.t.Context(), a.db, a.hasher, name, fixturePassword, role)
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

// signIn returns the session token of one account, or the error of the attempt.
func (a *access) signIn(name string) (string, error) {
	a.t.Helper()
	_, token, _, err := NewSetup(a.db, a.hasher, a.layout.Dir).Sessions().Login(a.t.Context(), name, fixturePassword)
	return token, err
}

// role is the current role and disabled flag of an account.
func (a *access) role(name string) (string, bool) {
	a.t.Helper()
	var role string
	var disabled int64
	err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(a.t.Context(), "SELECT role, disabled FROM user WHERE username = ?", name).Scan(&role, &disabled)
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return role, disabled == 1
}

// revisions is every account's revision by name.
func (a *access) revisions() map[string]int64 {
	a.t.Helper()
	revisions := map[string]int64{}
	if err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(a.t.Context(), "SELECT username, revision FROM user")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			var revision int64
			if err := rows.Scan(&name, &revision); err != nil {
				return err
			}
			revisions[name] = revision
		}
		return rows.Err()
	}); err != nil {
		a.t.Fatal(err)
	}
	return revisions
}

// collectionRevision is the users collection's current revision.
func (a *access) collectionRevision() int64 {
	a.t.Helper()
	var revision int64
	if err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		var err error
		revision, err = storedb.New(tx).UsersCollectionRevision(a.t.Context())
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return revision
}

// epoch is the installation's current restore epoch.
func (a *access) epoch() int64 {
	a.t.Helper()
	var epoch int64
	err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		installation, err := storedb.New(tx).GetInstallation(a.t.Context())
		epoch = installation.RestoreEpoch
		return err
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return epoch
}

// setupRow is the stored first-admin credential.
func (a *access) setupRow() (token string, consumed int64, ok bool) {
	a.t.Helper()
	err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		row, err := storedb.New(tx).GetSetup(a.t.Context())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		token, ok = row.TokenSha256, true
		if row.ConsumedAt.Valid {
			consumed = row.ConsumedAt.Int64
		}
		return nil
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return token, consumed, ok
}

// snapshot copies the database of a as a published snapshot holds it.
func (a *access) snapshot() string {
	a.t.Helper()
	path := filepath.Join(a.t.TempDir(), "snapshot.db")
	raw, err := sql.Open("sqlite", a.layout.DB())
	if err != nil {
		a.t.Fatal(err)
	}
	conn, err := raw.Conn(a.t.Context())
	if err != nil {
		a.t.Fatal(err)
	}
	if _, err := conn.ExecContext(a.t.Context(), "VACUUM INTO ?", path); err != nil {
		a.t.Fatal(err)
	}
	if err := errors.Join(conn.Close(), raw.Close()); err != nil {
		a.t.Fatal(err)
	}
	return path
}

// edit runs one statement against the open store.
func (a *access) edit(statement string, args ...any) {
	a.t.Helper()
	if err := a.db.Write(a.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(a.t.Context(), statement, args...)
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
}

// restoreOptions is the production restore of an auth-level proof.
func restoreOptions(record *[]install.Authority) install.RestoreOptions {
	return install.RestoreOptions{
		Reconcile: func(ctx context.Context, db *store.DB, current install.Authority, r install.Recovery) error {
			if record != nil {
				*record = append(*record, current)
			}
			return ReconcileRestore(ctx, db, current, r)
		},
		Snapshot: func(context.Context, install.Layout, store.Pending) error { return nil },
	}
}

// fileSum is the SHA-256 of a file.
func fileSum(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// TestRestoreReconcilesCurrentUsers: the restored file takes the current
// installation's accounts, so an account disabled or demoted after the snapshot
// stays that way while an account created after it is kept, and no credential
// from before the restore authenticates again.
func TestRestoreReconcilesCurrentUsers(t *testing.T) {
	dir := t.TempDir()
	a := openAccess(t, dir)
	userU := a.admin("user-u", RoleAdmin)
	userW := a.admin("user-w", RoleAdmin)
	snapshot := a.snapshot()
	// A later admin disables U and demotes W.
	userV := a.admin("user-v", RoleAdmin)
	tokenBefore, err := a.signIn("user-v")
	if err != nil {
		t.Fatal(err)
	}
	a.edit("UPDATE user SET disabled = 1, revision = revision + 1 WHERE id = ?", userU.ID)
	a.edit("UPDATE user SET role = ?, revision = revision + 1 WHERE id = ?", RoleMember, userW.ID)

	epochBefore := a.epoch()
	var pinned []install.Authority
	if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(&pinned)); err != nil {
		t.Fatal(err)
	}
	a.reopen()
	if len(pinned) != 1 {
		t.Fatalf("reconciliations: %d", len(pinned))
	}
	if _, disabled := a.role("user-u"); !disabled {
		t.Fatal("the restored file re-enabled an account the installation disabled")
	}
	if role, _ := a.role("user-w"); role != RoleMember {
		t.Fatalf("the restored file restored a demoted account to %q", role)
	}
	if role, disabled := a.role("user-v"); role != RoleAdmin || disabled {
		t.Fatalf("the later admin was not kept: %q disabled=%v", role, disabled)
	}
	if _, err := a.signIn("user-u"); err == nil {
		t.Fatal("a disabled account signed in after the restore")
	}
	_, token, _, err := NewSetup(a.db, a.hasher, dir).Sessions().Login(t.Context(), "user-w", fixturePassword)
	if err != nil {
		t.Fatalf("the demoted account could not sign in: %v", err)
	}
	identity, err := NewSetup(a.db, a.hasher, dir).Sessions().AuthenticateReadOnly(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if (Principal{Rights: identity.Rights}).Has(RightUsersWrite) {
		t.Fatalf("the demoted account kept %v", identity.Rights)
	}
	if _, err := a.signIn("user-v"); err != nil {
		t.Fatalf("the later admin could not sign in: %v", err)
	}
	if _, err := NewSetup(a.db, a.hasher, dir).Sessions().AuthenticateReadOnly(t.Context(), tokenBefore); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("a pre-restore session survived: %v", err)
	}
	if a.epoch() <= epochBefore {
		t.Fatalf("restore_epoch %d did not advance past %d", a.epoch(), epochBefore)
	}
	_ = userV
}

// fileRevisions is every account's revision and the users collection revision of a
// database file.
func fileRevisions(t *testing.T, path string) (map[string]int64, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), "SELECT username, revision FROM user")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	revisions := map[string]int64{}
	for rows.Next() {
		var name string
		var revision int64
		if err := rows.Scan(&name, &revision); err != nil {
			t.Fatal(err)
		}
		revisions[name] = revision
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var collection int64
	if err := db.QueryRowContext(t.Context(), "SELECT revision FROM collection_revision WHERE name = 'users'").Scan(&collection); err != nil {
		t.Fatal(err)
	}
	return revisions, collection
}

// TestRestoreAdvancesRevisions: every account the restore keeps and the users
// collection itself end above the revisions of both sides, so no representation or
// collection validator a client held before the restore validates after it.
func TestRestoreAdvancesRevisions(t *testing.T) {
	a := openAccess(t, t.TempDir())
	a.admin("user-u", RoleAdmin)
	a.admin("user-w", RoleAdmin)
	snapshot := a.snapshot()
	snapshotRevisions, snapshotCollection := fileRevisions(t, snapshot)
	// The installation moves on after the snapshot: W is demoted and V is created.
	a.admin("user-v", RoleAdmin)
	a.edit("UPDATE user SET role = ?, revision = revision + 1 WHERE username = ?", RoleMember, "user-w")
	before, beforeCollection := a.revisions(), a.collectionRevision()

	if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); err != nil {
		t.Fatal(err)
	}
	a.reopen()
	after, afterCollection := a.revisions(), a.collectionRevision()
	for name, revision := range before {
		restored := snapshotRevisions[name] // zero for an account newer than the snapshot
		if want := max(revision, restored) + 1; after[name] < want {
			t.Fatalf("%s revision %d after the restore, want at least %d (current %d, snapshot %d)", name, after[name], want, revision, restored)
		}
	}
	if want := max(beforeCollection, snapshotCollection) + 1; afterCollection < want {
		t.Fatalf("users collection revision %d after the restore, want at least %d (current %d, snapshot %d)", afterCollection, want, beforeCollection, snapshotCollection)
	}
}

// TestRestoreDisablesSnapshotGrant: an account the snapshot holds and the current
// installation never created ends disabled and cannot sign in, so a replaced file
// never grants what the pinned copy does not know.
func TestRestoreDisablesSnapshotGrant(t *testing.T) {
	a := openAccess(t, t.TempDir())
	a.admin("user-u", RoleAdmin)
	stray := a.admin("user-stray", RoleAdmin)
	snapshot := a.snapshot()
	// The current installation does not know the account: only the snapshot grants it.
	exec(t, a.layout.DB(), "DELETE FROM user WHERE id = ?", stray.ID)
	if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); err != nil {
		t.Fatal(err)
	}
	a.reopen()
	if _, disabled := a.role("user-stray"); !disabled {
		t.Fatal("the restored file kept the snapshot's own grant")
	}
	if _, err := a.signIn("user-stray"); err == nil {
		t.Fatal("a snapshot-only account signed in after the restore")
	}
	if _, disabled := a.role("user-u"); disabled {
		t.Fatal("the restore disabled an account the installation kept")
	}
}

// TestRestoreReconcileInterrupted: a reconciliation interrupted before or after
// its Write restores no grant, and a retry from the journal repeats it. Until it
// commits the replaced file still carries the snapshot's grants, which is why the
// start reports recovering instead of ready.
func TestRestoreReconcileInterrupted(t *testing.T) {
	for _, c := range []struct {
		name string
		// write runs the reconciliation, when the case reaches it at all.
		write bool
	}{
		{"before the write", false},
		{"after the write", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			a := openAccess(t, dir)
			userU := a.admin("user-u", RoleAdmin)
			snapshot := a.snapshot()
			tokenBefore, err := a.signIn("user-u")
			if err != nil {
				t.Fatal(err)
			}
			a.edit("UPDATE user SET disabled = 1, revision = revision + 1 WHERE id = ?", userU.ID)
			var applied int
			opts := restoreOptions(nil)
			// The returned error stands in for the crash: the journal stays either
			// way, and only the case that reached the write has applied it.
			opts.Reconcile = func(ctx context.Context, db *store.DB, current install.Authority, r install.Recovery) error {
				if c.write {
					if err := ReconcileRestore(ctx, db, current, r); err != nil {
						return err
					}
					applied++
				}
				return errInterrupted
			}
			if _, err := install.Restore(t.Context(), a.layout, snapshot, opts); !errors.Is(err, errInterrupted) {
				t.Fatalf("interrupted restore: %v", err)
			}
			want := 0
			if c.write {
				want = 1
			}
			if applied != want {
				t.Fatalf("reconciliations applied before the interruption: %d, want %d", applied, want)
			}
			if _, err := os.Lstat(a.layout.Journal()); err != nil {
				t.Fatalf("the journal was removed before the write committed: %v", err)
			}
			// The replaced file carries the snapshot's grant until the current
			// authority is applied; only the recovering state keeps it unused.
			a.reopen()
			if _, disabled := a.role("user-u"); disabled != c.write {
				t.Fatalf("%s: account disabled=%v after the interruption", c.name, disabled)
			}
			if _, err := install.Resume(t.Context(), a.layout, restoreOptions(nil)); err != nil {
				t.Fatal(err)
			}
			a.reopen()
			if _, disabled := a.role("user-u"); !disabled {
				t.Fatal("the resumed reconciliation restored the snapshot's grant")
			}
			if _, err := NewSetup(a.db, a.hasher, dir).Sessions().AuthenticateReadOnly(t.Context(), tokenBefore); !errors.Is(err, ErrSessionInvalid) {
				t.Fatalf("a pre-restore session survived the resume: %v", err)
			}
			if _, err := os.Lstat(a.layout.Journal()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the journal survived the resume: %v", err)
			}
		})
	}
}

// TestRestoreReconcilePinnedCopyRefused: a reconcile journal whose recovery copy
// is not this installation's is refused without a Write and stays recovering.
func TestRestoreReconcilePinnedCopyRefused(t *testing.T) {
	dir := t.TempDir()
	a := openAccess(t, dir)
	a.admin("user-u", RoleAdmin)
	snapshot := a.snapshot()
	stop := errors.New("synthetic interruption")
	opts := restoreOptions(nil)
	opts.Reconcile = func(context.Context, *store.DB, install.Authority, install.Recovery) error { return stop }
	journal, err := install.Restore(t.Context(), a.layout, snapshot, opts)
	if !errors.Is(err, stop) {
		t.Fatalf("interrupted restore: %v", err)
	}
	// Replace the recovery copy with another installation's database, and pin the
	// journal to its bytes so only the continuity check can refuse it.
	other := openAccess(t, t.TempDir())
	other.admin("other-admin", RoleAdmin)
	foreign, err := os.ReadFile(other.layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal.RecoveryPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	journal.RecoverySHA256 = fileSum(t, journal.RecoveryPath)
	journal.SnapshotSHA256 = fileSum(t, snapshot)
	if err := os.WriteFile(a.layout.Journal(), journalBytes(t, journal), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := false
	resume := restoreOptions(nil)
	resume.Reconcile = func(ctx context.Context, db *store.DB, current install.Authority, r install.Recovery) error {
		err := ReconcileRestore(ctx, db, current, r)
		wrote = err == nil
		return err
	}
	if _, err := install.Resume(t.Context(), a.layout, resume); err == nil {
		t.Fatal("a foreign recovery copy was reconciled")
	}
	if wrote {
		t.Fatal("a foreign recovery copy reached the reconciliation")
	}
	// The installation stays recovering: the refused journal is still there.
	if _, err := os.Lstat(a.layout.Journal()); err != nil {
		t.Fatalf("the journal: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(a.layout.Journal()) })
}

// errInterrupted stands in for a crash inside the restore.
var errInterrupted = errors.New("synthetic interruption")

// readRecovery reads the restore journal of a data directory.
func readRecovery(l install.Layout) (install.Recovery, bool, error) {
	body, err := os.ReadFile(l.Journal())
	if errors.Is(err, fs.ErrNotExist) {
		return install.Recovery{}, false, nil
	}
	if err != nil {
		return install.Recovery{}, false, err
	}
	var r install.Recovery
	if err := json.Unmarshal(body, &r); err != nil {
		return r, false, err
	}
	return r, true, nil
}

// journalBytes is the on-disk form of a recovery record.
func journalBytes(t *testing.T, r install.Recovery) []byte {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestRestoreInstallationMismatch: a snapshot that names no installation, or
// another one, is refused with nothing replaced, whatever either side's users are.
func TestRestoreInstallationMismatch(t *testing.T) {
	for _, c := range []struct {
		name   string
		mangle func(*testing.T, string)
	}{
		{"no installation row", func(t *testing.T, path string) {
			exec(t, path, "DELETE FROM installation")
		}},
		{"another installation", func(t *testing.T, path string) {
			exec(t, path, "UPDATE installation SET install_id = '00112233aabb' WHERE id = 1")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := openAccess(t, t.TempDir())
			a.admin("user-u", RoleAdmin)
			snapshot := a.snapshot()
			c.mangle(t, snapshot)
			before := a.users()
			if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrInstallationMismatch) {
				t.Fatalf("restore: %v", err)
			}
			if got := a.users(); !slices.Equal(got, before) {
				t.Fatalf("a refused restore replaced the database: %v", got)
			}
			if _, err := os.Lstat(a.layout.Journal()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a refused restore left a journal: %v", err)
			}
			// The same refusal over a freshly initialized, user-free installation.
			fresh := openAccess(t, t.TempDir())
			if _, err := install.Restore(t.Context(), fresh.layout, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrInstallationMismatch) {
				t.Fatalf("restore over a fresh installation: %v", err)
			}
		})
	}
}

// users lists the account names of the open store.
func (a *access) users() []string {
	a.t.Helper()
	var names []string
	err := a.db.Read(a.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(a.t.Context(), "SELECT username FROM user ORDER BY id")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return names
}

// TestRestoreEmptyAuthorityRefused: a current database with no identity or no
// access state is refused before anything is journaled or created.
func TestRestoreEmptyAuthorityRefused(t *testing.T) {
	t.Run("absent database", func(t *testing.T) {
		source := openAccess(t, t.TempDir())
		source.admin("user-u", RoleAdmin)
		snapshot := source.snapshot()
		dir := t.TempDir()
		l := install.Layout{Dir: dir}
		if _, err := install.Restore(t.Context(), l, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrAuthorizationStateUnavailable) {
			t.Fatalf("restore: %v", err)
		}
		for _, name := range []string{"boxset.db", "restore.journal"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s was created by a refused restore: %v", name, err)
			}
		}
	})
	t.Run("zero-length database", func(t *testing.T) {
		source := openAccess(t, t.TempDir())
		source.admin("user-u", RoleAdmin)
		snapshot := source.snapshot()
		dir := t.TempDir()
		l := install.Layout{Dir: dir}
		if err := os.WriteFile(l.DB(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := install.Restore(t.Context(), l, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrAuthorizationStateUnavailable) {
			t.Fatalf("restore: %v", err)
		}
		info, err := os.Stat(l.DB())
		if err != nil || info.Size() != 0 {
			t.Fatalf("the empty database changed: %v, %d bytes", err, info.Size())
		}
		if _, err := os.Lstat(l.Journal()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a refused restore left a journal: %v", err)
		}
	})
	t.Run("unverifiable database", func(t *testing.T) {
		source := openAccess(t, t.TempDir())
		source.admin("user-u", RoleAdmin)
		snapshot := source.snapshot()
		dir := t.TempDir()
		l := install.Layout{Dir: dir}
		// Bytes that are no store at all: the authority cannot be read, so the
		// restore is refused without bootstrapping a current installation.
		if err := os.WriteFile(l.DB(), []byte("synthetic-not-a-database"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := fileSum(t, l.DB())
		if _, err := install.Restore(t.Context(), l, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrAuthorizationStateUnavailable) {
			t.Fatalf("restore: %v", err)
		}
		if after := fileSum(t, l.DB()); after != before {
			t.Fatal("a refused restore rewrote the unreadable database")
		}
		if _, err := os.Lstat(l.Journal()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a refused restore left a journal: %v", err)
		}
	})
	t.Run("no current authority", func(t *testing.T) {
		a := openAccess(t, t.TempDir())
		a.admin("user-u", RoleAdmin)
		snapshot := a.snapshot()
		// The same installation, with its accounts removed: identity without any
		// access state to restore over.
		exec(t, a.layout.DB(), "DELETE FROM user")
		if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); !errors.Is(err, install.ErrAuthorizationStateUnavailable) {
			t.Fatalf("restore: %v", err)
		}
		if len(a.users()) != 0 {
			t.Fatalf("a refused restore replaced the database: %v", a.users())
		}
	})
	t.Run("control: user-free snapshot over a user-free installation", func(t *testing.T) {
		a := openAccess(t, t.TempDir())
		snapshot := a.snapshot()
		recovery, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil))
		if err != nil {
			t.Fatal(err)
		}
		if recovery.Epoch <= 1 {
			t.Fatalf("epoch %d", recovery.Epoch)
		}
		a.reopen()
		state, err := NewSetup(a.db, a.hasher, a.layout.Dir).State(t.Context())
		if err != nil || !state.Open {
			t.Fatalf("setup after a user-free restore: open=%v, %v", state.Open, err)
		}
	})
}

// TestRestoreSetupClosureMonotone: setup never reopens. A consumed side wins over
// an open one in either direction, and a reconcile journal whose restored file
// carries a grant the pinned copy cannot revoke writes nothing.
func TestRestoreSetupClosureMonotone(t *testing.T) {
	const verifier = "1111111111111111111111111111111111111111111111111111111111111111"
	t.Run("current consumed over an open snapshot", func(t *testing.T) {
		a := openAccess(t, t.TempDir())
		snapshot := a.snapshot()
		a.admin("user-u", RoleAdmin)
		a.edit("INSERT INTO setup (id, token_sha256, consumed_at) VALUES (1, ?, 99) ON CONFLICT (id) DO UPDATE SET token_sha256 = excluded.token_sha256, consumed_at = excluded.consumed_at", verifier)
		if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); err != nil {
			t.Fatal(err)
		}
		a.reopen()
		token, consumed, ok := a.setupRow()
		if !ok || consumed != 99 || token != verifier {
			t.Fatalf("setup after the restore: %q consumed=%d ok=%v", token, consumed, ok)
		}
		if _, disabled := a.role("user-u"); disabled {
			t.Fatal("the account created after the snapshot was not kept")
		}
	})
	t.Run("open current over a consumed snapshot", func(t *testing.T) {
		a := openAccess(t, t.TempDir())
		a.admin("user-u", RoleAdmin)
		a.edit("INSERT INTO setup (id, token_sha256, consumed_at) VALUES (1, ?, 77) ON CONFLICT (id) DO UPDATE SET token_sha256 = excluded.token_sha256, consumed_at = excluded.consumed_at", verifier)
		snapshot := a.snapshot()
		a.edit("UPDATE setup SET consumed_at = NULL, token_sha256 = ? WHERE id = 1", strings.Repeat("2", 64))
		if _, err := install.Restore(t.Context(), a.layout, snapshot, restoreOptions(nil)); err != nil {
			t.Fatal(err)
		}
		a.reopen()
		token, consumed, ok := a.setupRow()
		if !ok || consumed != 77 || token != verifier {
			t.Fatalf("setup after the restore: %q consumed=%d ok=%v", token, consumed, ok)
		}
	})
	t.Run("snapshot grant without current authority", func(t *testing.T) {
		a := openAccess(t, t.TempDir())
		a.admin("user-u", RoleAdmin)
		snapshot := a.snapshot()
		opts := restoreOptions(nil)
		opts.Reconcile = func(context.Context, *store.DB, install.Authority, install.Recovery) error { return errInterrupted }
		if _, err := install.Restore(t.Context(), a.layout, snapshot, opts); !errors.Is(err, errInterrupted) {
			t.Fatalf("interrupted restore: %v", err)
		}
		// The pinned copy loses its authority: the restored file's account can no
		// longer be revoked, so the retry must not write.
		journal, ok, err := readRecovery(a.layout)
		if err != nil || !ok {
			t.Fatalf("journal: %v, %v", ok, err)
		}
		exec(t, journal.RecoveryPath, "DELETE FROM user")
		journal.RecoverySHA256 = fileSum(t, journal.RecoveryPath)
		if err := os.WriteFile(a.layout.Journal(), journalBytes(t, journal), 0o600); err != nil {
			t.Fatal(err)
		}
		wrote := false
		resume := restoreOptions(nil)
		resume.Reconcile = func(ctx context.Context, db *store.DB, current install.Authority, r install.Recovery) error {
			err := ReconcileRestore(ctx, db, current, r)
			wrote = err == nil
			return err
		}
		if _, err := install.Resume(t.Context(), a.layout, resume); !errors.Is(err, install.ErrAuthorizationStateUnavailable) {
			t.Fatalf("resume: %v", err)
		}
		if wrote {
			t.Fatal("a reconcile without current authority reached the write")
		}
	})
}

// exec runs one statement against a database file.
func exec(t *testing.T, path, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
