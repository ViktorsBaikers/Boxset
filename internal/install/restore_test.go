package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// addUser inserts one account with a synthetic verifier.
func addUser(t *testing.T, db *store.DB, name, role string) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := storedb.New(tx).CreateUser(t.Context(), storedb.CreateUserParams{
			Username:    name,
			UsernameKey: name,
			PasswordPhc: "$argon2id$v=19$m=65536,t=3,p=4$c3ludGhldGlj$c3ludGhldGlj",
			Role:        role,
			CreatedAt:   1,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// installFixture is a migrated data directory with two admins.
func installFixture(t *testing.T) Layout {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), Layout{Dir: dir}.DB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, db, "admin-a", "admin")
	addUser(t, db, "admin-b", "admin")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return Layout{Dir: dir}
}

// snapshotOf copies the database of l as a published snapshot would hold it.
func snapshotOf(t *testing.T, l Layout) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	db, err := sql.Open("sqlite", l.DB())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "VACUUM INTO ?", path); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(conn.Close(), db.Close()); err != nil {
		t.Fatal(err)
	}
	return path
}

// migrationsDir is this repository's immutable migration set.
func migrationsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "store", "migrations")
}

// olderSnapshot writes a store that stands at migration 6 with one admin, the
// shape a binary older than this one left behind.
func olderSnapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "older.db")
	migrations := migrationsDir(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"} {
		if _, err := raw.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, raw, os.DirFS(migrations), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 6); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 6; version++ {
		body, err := os.ReadFile(filepath.Join(migrations, fmt.Sprintf("%05d", version)+"_"+migrationName(t, migrations, version)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if _, err := raw.ExecContext(t.Context(), "INSERT INTO boxset_schema_hash (version, sha256) VALUES (?, ?)", version, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(t.Context(), "INSERT INTO user (username, username_key, password_phc, role, created_at) VALUES ('older-admin', 'older-admin', 'synthetic-verifier', 'admin', 1)"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// migrationName is the file name part of one embedded migration.
func migrationName(t *testing.T, dir string, version int) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var got int
		if _, err := fmt.Sscanf(name, "%05d", &got); err == nil && got == version {
			return name[len("00000_"):]
		}
	}
	t.Fatalf("no migration %d in %s", version, dir)
	return ""
}

// newerSnapshot is a valid snapshot that records a migration this binary does not
// have, which is what a newer Boxset leaves behind.
func newerSnapshot(t *testing.T, l Layout) string {
	t.Helper()
	path := snapshotOf(t, l)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(t.Context(), "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99, 1)"); err != nil {
		t.Fatal(err)
	}
	return path
}

// treeDigest is the SHA-256 of every path and byte below dir.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." || d.Name() == "boxset.lock" {
			// The directory's own size moves with every entry created and removed,
			// and every stopped command creates the lock file: neither is state.
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s %v %d\n", rel, info.Mode(), info.Size())
		if !d.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h.Write(body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// treeNames lists every entry of a data directory with its size, for a failing
// comparison that names what changed.
func treeNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		names = append(names, fmt.Sprintf("%s %v %d", rel, info.Mode(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// recorder is a reconciliation that records how often it ran.
type recorder struct {
	calls   int
	current Authority
	epoch   int64
	fail    error
}

func (r *recorder) reconcile(context.Context, *store.DB, Authority, Recovery) error {
	r.calls++
	if r.fail != nil {
		return r.fail
	}
	return nil
}

func (r *recorder) options() RestoreOptions {
	return RestoreOptions{
		Reconcile: func(ctx context.Context, db *store.DB, current Authority, rec Recovery) error {
			r.current, r.epoch = current, rec.Epoch
			return r.reconcile(ctx, db, current, rec)
		},
	}
}

// users returns the account names of a database file.
func users(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), "SELECT username FROM user ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// refusedRestore requires a refusal that carries this operation's own reason and
// not the platform barrier's: ErrSnapshotUnsupported is translated into the restore
// vocabulary, and a refusal that still matched it would mean the translation never
// ran.
func refusedRestore(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrRestoreUnsupported) {
		t.Fatalf("%s: %v", what, err)
	}
	if errors.Is(err, ErrSnapshotUnsupported) {
		t.Fatalf("%s reported the snapshot barrier instead of the restore refusal: %v", what, err)
	}
}

// TestRestoreUnsupported: where the platform has no namespace barrier every
// restore, same-schema and older-schema alike, is refused before the lock, the
// snapshot file or any journal, and a journal is never resumed or discarded; the
// control shows the same fixtures restore and resume once the barrier is there.
func TestRestoreUnsupported(t *testing.T) {
	// The barrier returns what the platform really returns, so every case below
	// exercises the translation into ErrRestoreUnsupported.
	unavailable := func(RestoreOptions) RestoreOptions {
		opts := osRestoreFS
		opts.namespaceBarrier = func() error { return ErrSnapshotUnsupported }
		return RestoreOptions{Reconcile: (&recorder{}).reconcile, fs: opts}
	}
	for _, c := range []struct {
		name     string
		snapshot func(*testing.T, Layout) string
	}{
		{"same schema", func(t *testing.T, l Layout) string { return snapshotOf(t, l) }},
		{"older schema", func(t *testing.T, l Layout) string { return olderSnapshot(t) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := installFixture(t)
			snapshot := c.snapshot(t, l)
			before := treeDigest(t, l.Dir)
			rec := &recorder{}
			opts := unavailable(RestoreOptions{})
			opts.Reconcile = rec.reconcile
			_, err := Restore(t.Context(), l, snapshot, opts)
			refusedRestore(t, "restore", err)
			if got := treeDigest(t, l.Dir); got != before {
				t.Fatal("a refused restore changed the data directory")
			}
			if rec.calls != 0 {
				t.Fatal("a refused restore reconciled")
			}

			// A journal from either side of the replacement is refused too, and
			// every byte of it stays as it was.
			for _, phase := range []Phase{PhaseBeforeReplace, PhaseReconcile} {
				journal := []byte(`{"phase":"` + string(phase) + `","snapshot_sha256":"` + before + `","recovery_path":"` + filepath.Join(l.RecoveryDir(), "r.db") + `","recovery_sha256":"` + before + `","epoch":7}` + "\n")
				if err := os.WriteFile(l.Journal(), journal, 0o600); err != nil {
					t.Fatal(err)
				}
				frozen := treeDigest(t, l.Dir)
				resume := unavailable(RestoreOptions{})
				resume.Reconcile = rec.reconcile
				_, err := Resume(t.Context(), l, resume)
				refusedRestore(t, string(phase)+" journal", err)
				if got := treeDigest(t, l.Dir); got != frozen {
					t.Fatalf("%s journal resume changed the data directory", phase)
				}
				if after, err := os.ReadFile(l.Journal()); err != nil || !bytes.Equal(after, journal) {
					t.Fatalf("%s journal was resumed or discarded", phase)
				}
			}
			if rec.calls != 0 {
				t.Fatal("a refused journal resumed a reconciliation")
			}
			if err := os.Remove(l.Journal()); err != nil {
				t.Fatal(err)
			}

			// The barrier is checked before the lock: a held lock is not the
			// reason reported, and the holder keeps its lock.
			held, err := Acquire(l.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Release() }()
			beforeHeld := treeDigest(t, l.Dir)
			_, err = Restore(t.Context(), l, snapshot, opts)
			refusedRestore(t, "restore under a held lock", err)
			if _, err := Acquire(l.Dir); !errors.Is(err, ErrLockHeld) {
				t.Fatalf("the holder's lock was released: %v", err)
			}
			if got := treeDigest(t, l.Dir); got != beforeHeld {
				t.Fatal("a refused restore changed the data directory")
			}

			// Control: with the barrier available the same fixture restores, and
			// the lock is then the reason a restore is refused.
			if _, err := Restore(t.Context(), l, snapshot, RestoreOptions{Reconcile: rec.reconcile}); !errors.Is(err, ErrLockHeld) {
				t.Fatalf("restore beside a holder: %v", err)
			}
			if _, err := os.Lstat(l.Journal()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a lock-refused restore left a journal: %v", err)
			}
		})
	}
}

// TestNewerSchemaRefused: a snapshot a newer binary wrote is refused with the
// current database and the data directory unchanged.
func TestNewerSchemaRefused(t *testing.T) {
	l := installFixture(t)
	snapshot := newerSnapshot(t, l)
	before := treeDigest(t, l.Dir)
	rec := &recorder{}
	if _, err := Restore(t.Context(), l, snapshot, rec.options()); !errors.Is(err, ErrSnapshotNewer) {
		t.Fatalf("restore of a newer snapshot: %v", err)
	}
	if got := treeDigest(t, l.Dir); got != before {
		t.Fatalf("a refused snapshot changed the data directory: %v", treeNames(t, l.Dir))
	}
	if rec.calls != 0 {
		t.Fatal("a refused snapshot was reconciled")
	}
}

// TestStaleWALCannotMix: a row that exists only in the replaced file's log is in
// the recovery copy, never in the restored file, and the log and shared-memory
// files of the replaced file are gone. The log is planted, because SQLite
// checkpoints and unlinks its own when the last connection to the file closes.
func TestStaleWALCannotMix(t *testing.T) {
	l := installFixture(t)
	snapshot := snapshotOf(t, l)
	// The install gets a later admin, then a record committed without a
	// checkpoint, so it lives in the replaced file's log.
	db, err := store.Open(t.Context(), l.DB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, db, "later-admin", "admin")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", l.DB())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"PRAGMA journal_mode=wal", "PRAGMA wal_autocheckpoint=0", "INSERT INTO user (username, username_key, password_phc, role, created_at) VALUES ('log-only', 'log-only', 'synthetic', 'admin', 1)"} {
		if _, err := conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// Take the log while the connection still holds it: closing it checkpoints the
	// frames into the database and removes the log, which is why a fixture that
	// only closes a connection plants nothing.
	stale, err := os.ReadFile(l.DB() + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) == 0 {
		t.Fatal("the fixture left no log to plant")
	}
	if err := errors.Join(conn.Close(), raw.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(l.DB() + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the fixture's log outlived its last close: %v", err)
	}
	if err := os.WriteFile(l.DB()+"-wal", stale, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	opts := rec.options()
	opts.fs = osRestoreFS
	// Plant the same log again just before the replacement: the restore's own
	// reads closed their connections and checkpointed the first plant away, and
	// this is the log a stopped write can leave beside the file being replaced.
	opts.fs.beforeReplacement = func() error { return os.WriteFile(l.DB()+"-wal", stale, 0o600) }
	recovery, err := Restore(t.Context(), l, snapshot, opts)
	if err != nil {
		t.Fatal(err)
	}
	restored := users(t, l.DB())
	if contains(restored, "log-only") || contains(restored, "later-admin") || len(restored) != 2 {
		t.Fatalf("restored accounts %v", restored)
	}
	// The planted log was live, so the recovery copy taken through the log holds
	// the row only it had: the refusal is the replacement's, not a log SQLite
	// ignored.
	if !contains(users(t, recovery.RecoveryPath), "log-only") {
		t.Fatalf("the recovery copy missed the replaced file's log: %v", users(t, recovery.RecoveryPath))
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(l.DB() + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s survived the replacement: %v", suffix, err)
		}
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestRestoredFileWAL: the restored file is in WAL mode before any pool uses it.
func TestRestoredFileWAL(t *testing.T) {
	l := installFixture(t)
	snapshot := snapshotOf(t, l)
	rec := &recorder{}
	if _, err := Restore(t.Context(), l, snapshot, rec.options()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", l.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("restored journal_mode %q", mode)
	}
	if rec.calls != 1 {
		t.Fatalf("reconciliations: %d", rec.calls)
	}
}

// TestRestoreInterruptBeforeRename: an interruption before the replacement
// resumes by discarding the staged copy and the journal, with the printed
// database still in place and the recovery copy retained.
func TestRestoreInterruptBeforeRename(t *testing.T) {
	l := installFixture(t)
	snapshot := snapshotOf(t, l)
	db, err := store.Open(t.Context(), l.DB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, db, "retained-admin", "admin")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := users(t, l.DB())
	stop := errors.New("synthetic interruption before the replacement")
	rec := &recorder{}
	opts := rec.options()
	opts.fs = osRestoreFS
	opts.fs.beforeReplacement = func() error { return stop }
	journal, err := Restore(t.Context(), l, snapshot, opts)
	if !errors.Is(err, stop) {
		t.Fatalf("interrupted restore: %v", err)
	}
	if journal.Phase != PhaseBeforeReplace || journal.RecoverySHA256 == "" {
		t.Fatalf("journal %+v", journal)
	}
	if got := users(t, l.DB()); len(got) != len(before) {
		t.Fatalf("an interrupted restore replaced the database: %v", got)
	}
	recovery, err := Resume(t.Context(), l, rec.options())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Phase != PhaseBeforeReplace {
		t.Fatalf("resumed %+v", recovery)
	}
	if rec.calls != 0 {
		t.Fatal("discarding a journal reconciled")
	}
	if _, err := os.Lstat(l.Journal()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the journal survived: %v", err)
	}
	if _, err := os.Lstat(l.RestoreStaged()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staged copy survived: %v", err)
	}
	if sum, err := fileSum(journal.RecoveryPath); err != nil || sum != journal.RecoverySHA256 {
		t.Fatalf("the recovery copy was not retained: %v", err)
	}
	if got := users(t, l.DB()); len(got) != len(before) {
		t.Fatalf("discarding a journal changed the database: %v", got)
	}
}

// TestRestoreRefusesPendingJournal: a second restore over the journal an
// interrupted one left is refused with its own reason, and neither the staged
// copy nor any other byte of the data directory moves.
func TestRestoreRefusesPendingJournal(t *testing.T) {
	for _, c := range []struct {
		name  string
		phase Phase
		seam  func(*restoreFS)
	}{
		{"before the replacement", PhaseBeforeReplace, func(f *restoreFS) { f.beforeReplacement = func() error { return errPending } }},
		{"at the reconciliation", PhaseReconcile, func(f *restoreFS) { f.afterReplacement = func() error { return errPending } }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := installFixture(t)
			snapshot := snapshotOf(t, l)
			opts := (&recorder{}).options()
			opts.fs = osRestoreFS
			c.seam(&opts.fs)
			if _, err := Restore(t.Context(), l, snapshot, opts); !errors.Is(err, errPending) {
				t.Fatalf("interrupted restore: %v", err)
			}
			before := treeDigest(t, l.Dir)
			_, err := Restore(t.Context(), l, snapshot, (&recorder{}).options())
			if !errors.Is(err, ErrRestoreInProgress) {
				t.Fatalf("a second restore over a pending journal: %v", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte("restore_in_progress")) {
				t.Fatalf("the refusal did not name its reason: %v", err)
			}
			if after := treeDigest(t, l.Dir); after != before {
				t.Fatalf("a refused restore changed the data directory: %s, want %s", after, before)
			}
		})
	}
}

// errPending stands in for a crash that leaves the journal behind.
var errPending = errors.New("synthetic interruption")

// TestRestoreInterruptAfterRename: an interruption after the replacement resumes
// the reconciliation from the journal, so the epoch advances and the journal is
// removed only once the write committed.
func TestRestoreInterruptAfterRename(t *testing.T) {
	l := installFixture(t)
	snapshot := snapshotOf(t, l)
	db, err := store.Open(t.Context(), l.DB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, db, "retained-admin", "admin")
	epochBefore := 0
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		installation, err := storedb.New(tx).GetInstallation(t.Context())
		epochBefore = int(installation.RestoreEpoch)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("synthetic interruption after the replacement")
	rec := &recorder{}
	opts := rec.options()
	opts.fs = osRestoreFS
	opts.fs.afterReplacement = func() error { return stop }
	journal, err := Restore(t.Context(), l, snapshot, opts)
	if !errors.Is(err, stop) {
		t.Fatalf("interrupted restore: %v", err)
	}
	if journal.Phase != PhaseReconcile {
		t.Fatalf("journal phase %q", journal.Phase)
	}
	if rec.calls != 0 {
		t.Fatal("the interrupted restore reconciled")
	}
	// Nothing is applied before the journal says the replacement is committed.
	if got := users(t, l.DB()); contains(got, "retained-admin") {
		t.Fatalf("the replaced file kept an account the snapshot never had: %v", got)
	}
	if journal.Epoch <= int64(epochBefore) {
		t.Fatalf("the journal epoch %d did not advance past %d", journal.Epoch, epochBefore)
	}
	if _, err := Resume(t.Context(), l, rec.options()); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 || rec.epoch != journal.Epoch {
		t.Fatalf("resume reconciled %d times at epoch %d, want 1 at %d", rec.calls, rec.epoch, journal.Epoch)
	}
	if _, err := os.Lstat(l.Journal()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the journal survived the resume: %v", err)
	}
}
