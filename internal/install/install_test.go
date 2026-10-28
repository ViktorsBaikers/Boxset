//go:build unix

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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// rawExec runs statements on a plain driver connection to path.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := db.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// rawString returns the first column of the first row of query on path.
func rawString(t *testing.T, path, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var s string
	if err := db.QueryRowContext(t.Context(), query).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s
}

// existingInstallation is a data directory whose database stands at the first
// migration and holds a title and its library membership, so every later
// embedded migration is pending and there is data worth backing up. The file
// is built forward: the store's per-file policy before the first table, then
// the canonical first migration alone through goose with its hash row.
func existingInstallation(t *testing.T) Layout {
	t.Helper()
	l := Layout{Dir: t.TempDir()}
	versionOneFile(t, l.DB())
	rawExec(t, l.DB(),
		"INSERT INTO library (id, name, default_monitored, default_naming_language, default_search_language, default_profile) VALUES (1, 'Movies', 1, 'en', 'en', 'hd')",
		"INSERT INTO title (id, metadata_id, kind, name) VALUES (1, 'tmdb:1', 'movie', 'Synthetic Movie')",
		"INSERT INTO library_title (library_id, title_id) VALUES (1, 1)",
	)
	return l
}

// versionOneFile creates path as a store file at version 1: auto_vacuum and WAL
// set before any table, internal/store/migrations/00001_library.sql applied by
// goose UpTo 1, and the version-1 hash row the SHA-256 of that file's bytes.
func versionOneFile(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Join("..", "store", "migrations")
	first, err := os.ReadFile(filepath.Join(dir, "00001_library.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, os.DirFS(dir), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(first)
	if _, err := db.ExecContext(t.Context(), "INSERT INTO boxset_schema_hash (version, sha256) VALUES (1, ?)", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// openWith opens the installation through the store with the publication
// hook, over the given filesystem seam, after wrap changed the Pending the
// store hands the hook.
func openWith(t *testing.T, l Layout, fsys snapshotFS, wrap func(store.Pending) store.Pending) (*store.DB, error) {
	t.Helper()
	db, err := store.Open(t.Context(), l.DB(), store.Options{BeforeMigrate: func(ctx context.Context, p store.Pending) error {
		if wrap != nil {
			p = wrap(p)
		}
		return publish(ctx, fsys, l, p)
	}})
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return db, err
}

// seedBackups writes plain files with distinct content into the backups
// directory and returns their SHA-256 by name.
func seedBackups(t *testing.T, l Layout, names ...string) map[string]string {
	t.Helper()
	if err := os.MkdirAll(l.Backups(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(l.Backups(), n), []byte("earlier snapshot "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return backupHashes(t, l)
}

// backupHashes is the SHA-256 of every entry of the backups directory, by name.
func backupHashes(t *testing.T, l Layout) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(l.Backups())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(l.Backups(), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// requireNotMigrated fails when the database moved past the first migration.
func requireNotMigrated(t *testing.T, l Layout) {
	t.Helper()
	if got := rawString(t, l.DB(), "SELECT max(version_id) FROM goose_db_version"); got != "1" {
		t.Errorf("highest applied migration = %s, want 1: the migration must not have started", got)
	}
	if got := rawString(t, l.DB(), "SELECT count(*) FROM sqlite_schema WHERE name = 'installation'"); got != "0" {
		t.Errorf("installation table exists (%s): DDL ran after a failed snapshot", got)
	}
}

var earlier = []string{
	"pre-migration-0-20260101T000000.000000000Z.db",
	"pre-migration-1-20260102T000000.000000000Z.db",
	"pre-migration-1-20260103T000000.000000000Z.db",
}

// corruptAfterSnapshot lets the real VACUUM INTO write the copy and then
// damages three of its b-tree pages, as a failing disk would.
func corruptAfterSnapshot(p store.Pending) store.Pending {
	real := p.SnapshotInto
	p.SnapshotInto = func(ctx context.Context, dest string) error {
		if err := real(ctx, dest); err != nil {
			return err
		}
		b, err := os.ReadFile(dest)
		if err != nil {
			return err
		}
		const page = 4096
		for i := 2 * page; i < 5*page && i < len(b); i++ {
			b[i] = 0xff
		}
		return os.WriteFile(dest, b, 0o600)
	}
	return p
}

// diskFullDuringCopy leaves the partial file a VACUUM INTO that ran out of
// space leaves, and reports ENOSPC.
func diskFullDuringCopy(p store.Pending) store.Pending {
	p.SnapshotInto = func(_ context.Context, dest string) error {
		if err := os.WriteFile(dest, make([]byte, 4096), 0o600); err != nil {
			return err
		}
		return &fs.PathError{Op: "write", Path: dest, Err: syscall.ENOSPC}
	}
	return p
}

func TestSnapshotFaults(t *testing.T) {
	cases := []struct {
		name  string
		stage string // the stage word the error must name
		want  error  // an error the failure must wrap, when the fault is an errno
		seed  func(t *testing.T, l Layout)
		fsys  func(snapshotFS) snapshotFS
		wrap  func(store.Pending) store.Pending
	}{
		{name: "enospc", stage: "snapshot_create", want: syscall.ENOSPC, wrap: diskFullDuringCopy},
		{name: "permission", stage: "snapshot_create", want: fs.ErrPermission, fsys: func(f snapshotFS) snapshotFS {
			f.mkdirAll = func(path string, _ os.FileMode) error {
				return &fs.PathError{Op: "mkdir", Path: path, Err: syscall.EACCES}
			}
			return f
		}},
		{name: "integrity", stage: "snapshot_verify", wrap: corruptAfterSnapshot},
		{name: "foreign_key", stage: "snapshot_verify", seed: func(t *testing.T, l Layout) {
			rawExec(t, l.DB(), "INSERT INTO library_title (library_id, title_id) VALUES (999, 999)")
		}},
		{name: "fsync", stage: "snapshot_flush", want: syscall.EIO, fsys: func(f snapshotFS) snapshotFS {
			f.syncFile = func(string) error { return syscall.EIO }
			return f
		}},
		{name: "rename", stage: "snapshot_publish", want: syscall.EXDEV, fsys: func(f snapshotFS) snapshotFS {
			f.rename = func(oldpath, newpath string) error {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := existingInstallation(t)
			if tc.seed != nil {
				tc.seed(t, l)
			}
			before := seedBackups(t, l, earlier...)
			fsys := osFS
			if tc.fsys != nil {
				fsys = tc.fsys(osFS)
			}
			db, err := openWith(t, l, fsys, tc.wrap)
			if err == nil || db != nil {
				t.Fatalf("Open = %v, %v; want the migration aborted by the snapshot fault", db, err)
			}
			if !strings.Contains(err.Error(), tc.stage) {
				t.Errorf("error %q does not name the stage %s", err, tc.stage)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error %q does not wrap %v", err, tc.want)
			}
			requireNotMigrated(t, l)
			if after := backupHashes(t, l); !maps.Equal(before, after) {
				t.Errorf("backups changed by the failed snapshot:\n before %v\n after  %v", names(before), names(after))
			}
			t.Logf("SNAPSHOT-FAULT %s: migration aborted at %s: %v", tc.name, tc.stage, err)
		})
	}
}

// A directory entry that could not be flushed is not a snapshot to rely on:
// the renamed file is removed again and the migration does not start.
func TestSnapshotDirSyncFailureUnpublishes(t *testing.T) {
	l := existingInstallation(t)
	before := seedBackups(t, l, earlier...)
	fsys := osFS
	fsys = failOnly(l.Backups(), fsys)
	if db, err := openWith(t, l, fsys, nil); err == nil || db != nil || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "snapshot_publish") {
		t.Fatalf("Open = %v, %v; want the migration aborted at snapshot_publish with EIO", db, err)
	}
	requireNotMigrated(t, l)
	if after := backupHashes(t, l); !maps.Equal(before, after) {
		t.Errorf("backups after the failed directory flush = %v, want the earlier %v", names(after), names(before))
	}
}

func TestSnapshotRetentionThree(t *testing.T) {
	l := existingInstallation(t)
	// The names carry from-versions of different digit counts on purpose: the
	// newest by time is not the greatest by name. The two other files are not
	// snapshots and are never counted or removed.
	old := []string{
		"pre-migration-10-20190101T000000.000000000Z.db",
		"pre-migration-9-20200101T000000.000000000Z.db",
		"pre-migration-2-20210101T000000.000000000Z.db",
	}
	other := []string{"scheduled-20260101.db", "notes.txt"}
	before := seedBackups(t, l, append(slices.Clone(old), other...)...)

	clock := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	fsys := osFS
	fsys.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	db, err := store.Open(t.Context(), l.DB(), store.Options{BeforeMigrate: func(ctx context.Context, p store.Pending) error {
		for range 2 {
			if err := publish(ctx, fsys, l, p); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	after := backupHashes(t, l)
	fresh := []string{
		"pre-migration-1-20261022T000001.000000000Z.db",
		"pre-migration-1-20261022T000002.000000000Z.db",
	}
	want := append(append(slices.Clone(fresh), old[2]), other...)
	slices.Sort(want)
	if got := names(after); !slices.Equal(got, want) {
		t.Fatalf("backups = %v, want exactly the newest three snapshots plus the untouched others %v", got, want)
	}
	for _, n := range append([]string{old[2]}, other...) {
		if after[n] != before[n] {
			t.Errorf("%s changed", n)
		}
	}
	for _, n := range fresh {
		p := filepath.Join(l.Backups(), n)
		if got := rawString(t, p, "PRAGMA integrity_check"); got != "ok" {
			t.Errorf("%s integrity_check = %s", n, got)
		}
		if got := rawString(t, p, "SELECT count(*) FROM title"); got != "1" {
			t.Errorf("%s holds %s titles, want the 1 the file held before the migration", n, got)
		}
		if got := rawString(t, p, "SELECT count(*) FROM sqlite_schema WHERE name = 'installation'"); got != "0" {
			t.Errorf("%s already has the installation table: the snapshot is not the pre-migration state", n)
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v (err %v), want 0600", n, fi.Mode().Perm(), err)
		}
	}
	if got := rawString(t, l.DB(), "SELECT count(*) FROM installation"); got != "1" {
		t.Errorf("installation rows after the migration = %s, want 1", got)
	}
}

// A temporary file an interrupted publication left is removed by the next one,
// and an interrupted publication leaves none of its own.
func TestSnapshotSweepsInterruptedTemp(t *testing.T) {
	l := existingInstallation(t)
	before := seedBackups(t, l, earlier...)
	if err := os.WriteFile(filepath.Join(l.Backups(), ".tmp-deadbeefdeadbeef"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}

	interrupted := func(_ context.Context, p store.Pending) error {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return publish(ctx, osFS, l, p)
	}
	if db, err := store.Open(t.Context(), l.DB(), store.Options{BeforeMigrate: interrupted}); err == nil || db != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, %v; want the migration aborted by the cancelled snapshot", db, err)
	}
	if after := backupHashes(t, l); !maps.Equal(before, after) {
		t.Errorf("backups after the interrupted snapshot = %v, want the earlier %v and no temporary file", names(after), names(before))
	}
	requireNotMigrated(t, l)

	if _, err := openWith(t, l, osFS, nil); err != nil {
		t.Fatal(err)
	}
	for n := range backupHashes(t, l) {
		if strings.HasPrefix(n, ".tmp-") {
			t.Errorf("temporary file %s survived a successful publication", n)
		}
	}
}

func TestSecretFileOwnerOnly(t *testing.T) {
	read := func(t *testing.T, path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("new file is owner only", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "token")
		if err := WriteSecretFile(p, []byte("secret")); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 || int(fi.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() || read(t, p) != "secret" {
			t.Errorf("mode %v owner %d content %q; want 0600, uid %d, secret", fi.Mode().Perm(), fi.Sys().(*syscall.Stat_t).Uid, read(t, p), os.Geteuid())
		}
		t.Logf("SECRET-FILE owner-only: mode=%v uid=%d", fi.Mode().Perm(), fi.Sys().(*syscall.Stat_t).Uid)
	})

	t.Run("an existing file is replaced, never written through", func(t *testing.T) {
		dir := t.TempDir()
		p, kept := filepath.Join(dir, "token"), filepath.Join(dir, "other-name")
		if err := os.WriteFile(p, []byte("old"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(p, kept); err != nil {
			t.Fatal(err)
		}
		if err := WriteSecretFile(p, []byte("new")); err != nil {
			t.Fatal(err)
		}
		if read(t, p) != "new" || read(t, kept) != "old" {
			t.Errorf("token = %q, other name of the old inode = %q; want new and old: the old inode must not be written", read(t, p), read(t, kept))
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("replacement mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
		}
	})

	t.Run("a symlink is refused and its target untouched", func(t *testing.T) {
		dir := t.TempDir()
		target, p := filepath.Join(dir, "target"), filepath.Join(dir, "token")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if err := WriteSecretFile(p, []byte("secret")); err == nil {
			t.Fatal("WriteSecretFile through a symlink succeeded")
		}
		if read(t, target) != "target" {
			t.Errorf("symlink target = %q, want it untouched", read(t, target))
		}
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the symlink was replaced (%v, %v)", fi, err)
		}
	})

	t.Run("other kinds of entry are refused", func(t *testing.T) {
		dir := t.TempDir()
		sub, fifo := filepath.Join(dir, "dir"), filepath.Join(dir, "fifo")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{sub, fifo} {
			if err := WriteSecretFile(p, []byte("secret")); err == nil {
				t.Errorf("WriteSecretFile over %s succeeded", filepath.Base(p))
			}
			if _, err := os.Lstat(p); err != nil {
				t.Errorf("%s was removed: %v", filepath.Base(p), err)
			}
		}
	})

	t.Run("a foreign owner is refused before any byte is written", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "token")
		checks, sizeAtCheck := 0, int64(-1)
		effectiveUID = func() int {
			checks++
			if fi, err := os.Stat(p); err == nil {
				sizeAtCheck = fi.Size()
			}
			return os.Geteuid() + 1
		}
		t.Cleanup(func() { effectiveUID = os.Geteuid })
		if err := WriteSecretFile(p, []byte("secret")); err == nil {
			t.Fatal("WriteSecretFile accepted a file owned by another user")
		}
		if checks != 1 || sizeAtCheck != 0 {
			t.Errorf("owner check ran %d times with %d bytes in the file, want once with the empty new file", checks, sizeAtCheck)
		}
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused file remains (%v)", err)
		}
	})

	t.Run("a group or other bit is refused", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "token")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		if err := requireOwnerOnly(f); err != nil {
			t.Fatalf("0600 file refused: %v", err)
		}
		for _, mode := range []os.FileMode{0o640, 0o604, 0o660} {
			if err := f.Chmod(mode); err != nil {
				t.Fatal(err)
			}
			if err := requireOwnerOnly(f); err == nil {
				t.Errorf("file with mode %v accepted", mode)
			}
		}
	})
}

func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second Acquire = %v, want ErrLockHeld", err)
	}
	if fi, err := os.Stat(Layout{Dir: dir}.DB()); err == nil {
		t.Errorf("the database file exists (%v): taking the lock must not touch it", fi.Name())
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release = %v, want the lock", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	t.Log("LOCK-EXCLUSIVE second holder refused: lock_held")
}

// events records what a publication does, in order, through the same seams the
// fault tests use.
type events struct{ log []string }

func (e *events) add(s string) { e.log = append(e.log, s) }

func (e *events) fs(l Layout, f snapshotFS) snapshotFS {
	rel := func(p string) string {
		r, err := filepath.Rel(l.Dir, p)
		if err != nil {
			return p
		}
		return r
	}
	realSync := f.syncDir
	f.syncDir = func(p string) error { e.add("syncdir:" + rel(p)); return realSync(p) }
	realRename := f.rename
	f.rename = func(o, n string) error { e.add("rename"); return realRename(o, n) }
	return f
}

func (e *events) copy(p store.Pending) store.Pending {
	real := p.SnapshotInto
	p.SnapshotInto = func(ctx context.Context, dest string) error { e.add("copy"); return real(ctx, dest) }
	return p
}

// failOnly makes syncDir fail with EIO for the one directory, and only it.
func failOnly(dir string, f snapshotFS) snapshotFS {
	real := f.syncDir
	f.syncDir = func(p string) error {
		if p == dir {
			return syscall.EIO
		}
		return real(p)
	}
	return f
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The data directory that holds a newly created backups entry is flushed before
// the copy, the backups directory after the rename; either flush failing, or a
// platform with no barrier at all, stops the migration before any DDL.
func TestSnapshotNamespaceBarrier(t *testing.T) {
	t.Run("parent then backups, around the copy", func(t *testing.T) {
		l := existingInstallation(t)
		if _, err := os.Stat(l.Backups()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backups exists before the first publication (%v)", err)
		}
		var ev events
		if _, err := openWith(t, l, ev.fs(l, osFS), ev.copy); err != nil {
			t.Fatal(err)
		}
		want := []string{"syncdir:.", "copy", "rename", "syncdir:backups"}
		if !slices.Equal(ev.log, want) {
			t.Errorf("publication order = %v, want %v", ev.log, want)
		}
		t.Logf("NAMESPACE-BARRIER order: %v", ev.log)
	})

	for _, tc := range []struct {
		name, stage string
		dir         func(Layout) string
		copied      bool
	}{
		{"installation directory flush fails", "snapshot_create", func(l Layout) string { return l.Dir }, false},
		{"backups directory flush fails", "snapshot_publish", Layout.Backups, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := existingInstallation(t)
			before := seedBackups(t, l, earlier...)
			var ev events
			db, err := openWith(t, l, failOnly(tc.dir(l), ev.fs(l, osFS)), ev.copy)
			if err == nil || db != nil || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("Open = %v, %v; want the migration aborted at %s with EIO", db, err, tc.stage)
			}
			if copied := slices.Contains(ev.log, "copy"); copied != tc.copied {
				t.Errorf("copy ran = %v, want %v (events %v)", copied, tc.copied, ev.log)
			}
			requireNotMigrated(t, l)
			if after := backupHashes(t, l); !maps.Equal(before, after) {
				t.Errorf("backups after the failed flush = %v, want the earlier %v", names(after), names(before))
			}
		})
	}

	t.Run("an unavailable barrier refuses before anything is created", func(t *testing.T) {
		l := existingInstallation(t)
		dbBefore := fileBytes(t, l.DB())
		unavailable := osFS
		unavailable.namespaceBarrier = func() error { return ErrSnapshotUnsupported }
		var ev events
		db, err := openWith(t, l, ev.fs(l, unavailable), ev.copy)
		if db != nil || !errors.Is(err, ErrSnapshotUnsupported) || !strings.Contains(err.Error(), "snapshot_unsupported") {
			t.Fatalf("Open = %v, %v; want the migration refused with snapshot_unsupported", db, err)
		}
		if len(ev.log) != 0 {
			t.Errorf("the refused publication did %v, want nothing", ev.log)
		}
		if _, err := os.Stat(l.Backups()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("backups exists after the refusal (%v)", err)
		}
		if !slices.Equal(fileBytes(t, l.DB()), dbBefore) {
			t.Error("the database bytes changed")
		}
		requireNotMigrated(t, l)

		// An empty file has nothing to back up and an up-to-date one nothing pending: both open.
		empty := Layout{Dir: t.TempDir()}
		if _, err := openWith(t, empty, unavailable, nil); err != nil {
			t.Errorf("empty file: %v", err)
		}
		migrated, err := openWith(t, l, osFS, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrated.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := openWith(t, l, unavailable, nil); err != nil {
			t.Errorf("up-to-date file: %v", err)
		}
	})
}

// A clock set back must not make the snapshot just published the oldest one:
// it is kept, with the two newest earlier ones, and is the real pre-migration state.
func TestSnapshotRetentionClockRollback(t *testing.T) {
	l := existingInstallation(t)
	future := []string{
		"pre-migration-1-20300101T000000.000000000Z.db",
		"pre-migration-1-20300102T000000.000000000Z.db",
		"pre-migration-1-20300103T000000.000000000Z.db",
	}
	before := seedBackups(t, l, future...)
	fsys := osFS
	fsys.now = func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }
	if _, err := openWith(t, l, fsys, nil); err != nil {
		t.Fatal(err)
	}
	after := backupHashes(t, l)
	var fresh []string
	for n := range after {
		if _, ok := before[n]; !ok {
			fresh = append(fresh, n)
		}
	}
	if len(after) != 3 || len(fresh) != 1 {
		t.Fatalf("backups = %v, want the new snapshot and the two newest earlier ones", names(after))
	}
	for _, n := range future[1:] {
		if after[n] != before[n] {
			t.Errorf("%s missing or changed", n)
		}
	}
	p := filepath.Join(l.Backups(), fresh[0])
	if got := rawString(t, p, "PRAGMA integrity_check"); got != "ok" {
		t.Errorf("new snapshot integrity_check = %s", got)
	}
	if got := rawString(t, p, "SELECT count(*) FROM title"); got != "1" {
		t.Errorf("new snapshot holds %s titles, want the 1 the file held before the migration", got)
	}
	if got := rawString(t, p, "SELECT count(*) FROM sqlite_schema WHERE name = 'installation'"); got != "0" {
		t.Error("new snapshot is not the pre-migration state")
	}
}

// A failed look-up of the target name is not proof that it is free, and an
// existing name is never replaced: both stop before the rename.
func TestSnapshotDestinationLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lstat  func(l Layout) func(string) (os.FileInfo, error)
		wantIs error
		want   string
	}{
		{"lookup fails", func(Layout) func(string) (os.FileInfo, error) {
			return func(p string) (os.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.EACCES}
			}
		}, fs.ErrPermission, "snapshot_publish"},
		{"name already published", func(l Layout) func(string) (os.FileInfo, error) {
			return func(string) (os.FileInfo, error) { return os.Lstat(filepath.Join(l.Backups(), earlier[0])) }
		}, nil, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := existingInstallation(t)
			before := seedBackups(t, l, earlier...)
			fsys := osFS
			fsys.lstat = tc.lstat(l)
			renamed := false
			fsys.rename = func(o, n string) error { renamed = true; return os.Rename(o, n) }
			db, err := openWith(t, l, fsys, nil)
			if db != nil || err == nil || !strings.Contains(err.Error(), tc.want) || (tc.wantIs != nil && !errors.Is(err, tc.wantIs)) {
				t.Fatalf("Open = %v, %v; want the migration aborted with %q", db, err, tc.want)
			}
			if renamed {
				t.Error("the snapshot was renamed although the destination was not shown to be free")
			}
			requireNotMigrated(t, l)
			if after := backupHashes(t, l); !maps.Equal(before, after) {
				t.Errorf("backups after the refused publication = %v, want the earlier %v", names(after), names(before))
			}
		})
	}
}

// installation is the one row a migrated file carries.
type installation struct{ id, epoch, createdAt string }

func readInstallation(t *testing.T, l Layout) installation {
	t.Helper()
	if got := rawString(t, l.DB(), "SELECT count(*) FROM installation"); got != "1" {
		t.Fatalf("installation rows = %s, want 1", got)
	}
	return installation{
		id:        rawString(t, l.DB(), "SELECT install_id FROM installation WHERE id = 1"),
		epoch:     rawString(t, l.DB(), "SELECT restore_epoch FROM installation WHERE id = 1"),
		createdAt: rawString(t, l.DB(), "SELECT created_at FROM installation WHERE id = 1"),
	}
}

// The migration seeds the installation row once with a random 12-digit
// lowercase hex identity, restore epoch 1 and the creation time in UTC
// seconds; the row survives a reopen and its constraints reject bad values.
func TestInstallationRow(t *testing.T) {
	idShape := regexp.MustCompile(`^[0-9a-f]{12}$`)
	fresh := func(t *testing.T) Layout {
		l := Layout{Dir: t.TempDir()}
		db, err := store.Open(t.Context(), l.DB(), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return l
	}
	opened := map[string]installation{}
	for _, tc := range []struct {
		name string
		open func(t *testing.T) Layout
	}{
		{"fresh a", fresh},
		{"fresh b", fresh},
		{"upgraded", func(t *testing.T) Layout {
			l := existingInstallation(t)
			db, err := openWith(t, l, osFS, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			return l
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			low := time.Now().Unix()
			l := tc.open(t)
			high := time.Now().Unix()
			row := readInstallation(t, l)
			if !idShape.MatchString(row.id) {
				t.Errorf("install_id = %q, want 12 lowercase hex digits", row.id)
			}
			if row.epoch != "1" {
				t.Errorf("restore_epoch = %s, want 1", row.epoch)
			}
			if at, err := strconv.ParseInt(row.createdAt, 10, 64); err != nil || at < low || at > high {
				t.Errorf("created_at = %s (%v), want UTC seconds within [%d, %d]", row.createdAt, err, low, high)
			}
			for other, o := range opened {
				if o.id == row.id {
					t.Errorf("install_id %s equals the one of %s: identities must be distinct", row.id, other)
				}
			}
			opened[tc.name] = row

			// A reopen keeps the very same row.
			db, err := store.Open(t.Context(), l.DB(), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if again := readInstallation(t, l); again != row {
				t.Errorf("row after reopen = %+v, want %+v", again, row)
			}

			// The constraints hold: nothing below changes the row.
			for _, stmt := range []string{
				"UPDATE installation SET restore_epoch = 0",
				"UPDATE installation SET install_id = 'ABCDEF012345'",
				"UPDATE installation SET install_id = 'abcdef01234'",
				"UPDATE installation SET install_id = 'abcdef0123zz'",
				"UPDATE installation SET created_at = 'yesterday'",
				"INSERT INTO installation (id, install_id, restore_epoch, created_at) VALUES (2, 'abcdef012345', 1, 1)",
			} {
				raw, err := sql.Open("sqlite", l.DB())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := raw.ExecContext(t.Context(), stmt); err == nil {
					t.Errorf("%s was accepted", stmt)
				}
				if err := raw.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if after := readInstallation(t, l); after != row {
				t.Errorf("row after rejected statements = %+v, want %+v", after, row)
			}
			t.Logf("INSTALLATION-ROW %s id=%s epoch=%s created_at=%s", tc.name, row.id, row.epoch, row.createdAt)
		})
	}
}

// The child side of TestSnapshotProcessInterruption: a real opening of the
// installation that stops inside the snapshot at a chosen stage.
const (
	childEnv = "BOXSET_INSTALL_TEST_STAGE"
	dirEnv   = "BOXSET_INSTALL_TEST_DIR"
	markEnv  = "BOXSET_INSTALL_TEST_MARK"

	// exitMidCopy is the status of a child that ended itself while the real
	// VACUUM INTO was still writing; copyProgress is how many bytes of that
	// copy had to be on disk when it did.
	exitMidCopy  = 75
	copyProgress = 1 << 20
)

func TestMain(m *testing.M) {
	if stage := os.Getenv(childEnv); stage != "" {
		os.Exit(interruptChild(stage))
	}
	os.Exit(m.Run())
}

// interruptChild opens the installation with the real publisher and stops
// inside the snapshot at a chosen stage. At stage "copy" a watcher ends the
// process the moment the temporary copy holds copyProgress bytes while the
// real VACUUM INTO is still running (no parent signal could be that prompt);
// at "publish" it tells the parent after the rename, before the backups
// directory flush, and waits to be killed.
func interruptChild(stage string) int {
	l := Layout{Dir: os.Getenv(dirEnv)}
	hold := func() {
		_ = os.WriteFile(os.Getenv(markEnv), nil, 0o600)
		time.Sleep(time.Hour)
	}
	fsys := osFS
	if stage == "publish" {
		fsys.syncDir = func(p string) error {
			if p == l.Backups() {
				hold()
			}
			return syncDir(p)
		}
	}
	_, err := store.Open(context.Background(), l.DB(), store.Options{BeforeMigrate: func(ctx context.Context, p store.Pending) error {
		if stage == "copy" {
			real := p.SnapshotInto
			p.SnapshotInto = func(ctx context.Context, dest string) error {
				go func() {
					for {
						if fi, err := os.Stat(dest); err == nil && fi.Size() >= copyProgress {
							_, _ = fmt.Fprintf(os.Stderr, "child: %d bytes of the copy on disk, copy still running\n", fi.Size())
							os.Exit(exitMidCopy)
						}
						time.Sleep(100 * time.Microsecond)
					}
				}()
				if err := real(ctx, dest); err != nil {
					return err
				}
				return errors.New("the copy finished before its progress was seen")
			}
		}
		return publish(ctx, fsys, l, p)
	}})
	_, _ = fmt.Fprintf(os.Stderr, "child: store.Open returned %v without being stopped\n", err)
	return 1
}

// killInSnapshot runs the child to its stage and stops it there.
func killInSnapshot(t *testing.T, l Layout, stage string) {
	t.Helper()
	mark := filepath.Join(t.TempDir(), "reached")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"="+stage, dirEnv+"="+l.Dir, markEnv+"="+mark)
	var out bytes.Buffer
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if stage == "copy" {
		select {
		case err := <-done:
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != exitMidCopy {
				t.Fatalf("the child did not end inside the copy (%v): %s", err, out.String())
			}
			t.Log(strings.TrimSpace(out.String()))
		case <-time.After(60 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("the child did not reach the copy: %s", out.String())
		}
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the child ended before stage %s (%v): %s", stage, err, out.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("the child did not reach stage %s: %s", stage, out.String())
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
}

// A process stopped inside the snapshot (while the copy is half written, or
// after the rename and before the directory flush) leaves the earlier backups
// byte-identical, no unfinished snapshot among the published ones and the
// database unmigrated; the next start recovers and publishes a verified
// snapshot first.
func TestSnapshotProcessInterruption(t *testing.T) {
	published := func(t *testing.T, l Layout) []string {
		t.Helper()
		var out []string
		for n := range backupHashes(t, l) {
			if snapshotName.MatchString(n) {
				out = append(out, n)
			}
		}
		slices.Sort(out)
		return out
	}
	requireSnapshot := func(t *testing.T, l Layout, name string) {
		t.Helper()
		p := filepath.Join(l.Backups(), name)
		if got := rawString(t, p, "PRAGMA integrity_check"); got != "ok" {
			t.Errorf("%s integrity_check = %s", name, got)
		}
		if got := rawString(t, p, "SELECT count(*) FROM title"); got != "1" {
			t.Errorf("%s holds %s titles, want the 1 of the pre-migration file", name, got)
		}
		if got := rawString(t, p, "SELECT count(*) FROM sqlite_schema WHERE name = 'installation'"); got != "0" {
			t.Errorf("%s is not the pre-migration state", name)
		}
	}

	for _, stage := range []string{"copy", "publish"} {
		t.Run(stage, func(t *testing.T) {
			l := existingInstallation(t)
			before := seedBackups(t, l, earlier...)
			if stage == "copy" {
				// Enough data that the copy outgrows the pager cache and reaches disk before it ends.
				rawExec(t, l.DB(), "UPDATE title SET name = 'Synthetic Movie' || hex(zeroblob(25165824))")
			}
			killInSnapshot(t, l, stage)

			killed := published(t, l)
			now := backupHashes(t, l)
			for n, h := range before {
				if now[n] != h {
					t.Errorf("%s changed by the interrupted snapshot", n)
				}
			}
			requireNotMigrated(t, l)
			var interrupted string
			switch stage {
			case "copy":
				if !slices.Equal(killed, earlier) {
					t.Errorf("published after the kill during the copy = %v, want only the earlier %v", killed, earlier)
				}
				// Positive evidence the copy was under way and unfinished: the temporary copy
				// (the largest .tmp- file; SQLite may leave small side files) holds real data
				// pages, far short of the size a finished copy has (the pages in use).
				entries, err := os.ReadDir(l.Backups())
				if err != nil {
					t.Fatal(err)
				}
				var copySize int64
				var temps []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".tmp-") {
						fi, err := e.Info()
						if err != nil {
							t.Fatal(err)
						}
						copySize = max(copySize, fi.Size())
						temps = append(temps, fmt.Sprintf("%s=%d", e.Name(), fi.Size()))
					}
				}
				whole, err := strconv.ParseInt(rawString(t, l.DB(), "SELECT (page_count - freelist_count) * page_size FROM pragma_page_count, pragma_freelist_count, pragma_page_size"), 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if copySize < copyProgress || copySize*4 >= whole*3 {
					t.Errorf("temporary files after the kill = %v, want a copy of at least %d bytes and under three quarters of the %d of a finished one", temps, copyProgress, whole)
				}
			case "publish":
				// Complete and verified before the rename, so whatever stands published is a whole snapshot.
				if len(killed) != len(earlier)+1 {
					t.Fatalf("published after the kill = %v, want the earlier ones and the one just renamed", killed)
				}
				for _, n := range killed {
					if _, old := before[n]; !old {
						interrupted = n
						requireSnapshot(t, l, n)
					}
				}
			}

			if _, err := openWith(t, l, osFS, nil); err != nil {
				t.Fatalf("restart after the kill: %v", err)
			}
			if got := rawString(t, l.DB(), "SELECT count(*) FROM installation"); got != "1" {
				t.Errorf("installation rows after the restart = %s, want the migration done once", got)
			}
			for n := range backupHashes(t, l) {
				if strings.HasPrefix(n, ".tmp-") {
					t.Errorf("temporary file %s survived the restart", n)
				}
			}
			got := published(t, l)
			if len(got) != keepSnapshots {
				t.Fatalf("published after the restart = %v, want exactly %d", got, keepSnapshots)
			}
			var fresh []string
			for _, n := range got {
				if _, old := before[n]; !old && n != interrupted {
					fresh = append(fresh, n)
				}
			}
			if len(fresh) != 1 {
				t.Fatalf("published after the restart = %v, want one new snapshot from the restart", got)
			}
			requireSnapshot(t, l, fresh[0])
			if interrupted != "" && !slices.Contains(got, interrupted) {
				t.Errorf("the snapshot of the interrupted start %s was dropped", interrupted)
			}
			t.Logf("SNAPSHOT-INTERRUPTED stage=%s: earlier backups intact, restart published %s", stage, fresh[0])
		})
	}
}
