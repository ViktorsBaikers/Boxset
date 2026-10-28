package install

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver the verification opens
)

// keepSnapshots is how many published pre-migration snapshots survive a
// publication, independent of any scheduled-backup retention.
const keepSnapshots = 3

// snapshotStamp is the UTC time layout of a published name. It is fixed width,
// so names sort in time order.
const snapshotStamp = "20060102T150405.000000000Z"

// snapshotName matches exactly the files this package publishes; nothing else
// in the backups directory is ever counted or removed as a snapshot.
var snapshotName = regexp.MustCompile(`^pre-migration-(\d+)-(\d{8}T\d{6}\.\d{9}Z)\.db$`)

// snapshotFS is the filesystem seam of publication: the steps a proof needs to
// fail on demand (the runner has no mount capability to fill a disk, and
// Windows is only injected on Linux).
type snapshotFS struct {
	namespaceBarrier func() error
	mkdirAll         func(path string, perm os.FileMode) error
	syncFile         func(path string) error
	syncDir          func(path string) error
	lstat            func(path string) (os.FileInfo, error)
	rename           func(oldpath, newpath string) error
	now              func() time.Time
}

var osFS = snapshotFS{
	namespaceBarrier: namespaceBarrier,
	mkdirAll:         os.MkdirAll,
	syncFile:         syncFile,
	syncDir:          syncDir,
	lstat:            os.Lstat,
	rename:           os.Rename,
	now:              time.Now,
}

// PublishSnapshot is the store's BeforeMigrate hook. Where the platform has no
// namespace barrier it refuses with ErrSnapshotUnsupported before it creates
// anything. Otherwise it creates the backups directory and flushes the data
// directory that holds its entry, writes the file as it stands to a temporary
// name in the backups directory, checks that copy with integrity_check and
// foreign_key_check, flushes it, renames it to pre-migration-<from>-<UTC>.db,
// flushes the backups directory and then keeps the newest three published
// snapshots.
//
// Publication is complete once the backups directory flush succeeded. A
// failure before that point returns an error naming the stage, which aborts
// the migration before it changes the database; the temporary file or renamed
// snapshot is removed and no earlier snapshot is touched. Retention runs after
// publication: if removing an older snapshot fails the error still aborts the
// migration, but the snapshot just published stays and the older ones are not
// restored.
func PublishSnapshot(ctx context.Context, l Layout, p store.Pending) error {
	return publish(ctx, osFS, l, p)
}

func publish(ctx context.Context, fsys snapshotFS, l Layout, p store.Pending) (err error) {
	if err := fsys.namespaceBarrier(); err != nil {
		return err
	}
	dir := l.Backups()
	if err := fsys.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	// The backups entry itself must survive a crash before a migration relies on it.
	if err := fsys.syncDir(l.Dir); err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	// A temporary file left by an interrupted earlier start is ours: the lifetime lock is held.
	if err := removeTemps(dir); err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	stamp, err := stampAfter(dir, fsys.now().UTC())
	if err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(suffix[:]))
	final := filepath.Join(dir, fmt.Sprintf("pre-migration-%d-%s.db", p.From, stamp.Format(snapshotStamp)))
	renamed := false
	defer func() {
		if err == nil {
			return
		}
		if renamed {
			err = errors.Join(err, removeIfPresent(final))
		}
		err = errors.Join(err, removeIfPresent(tmp))
	}()

	if err := p.SnapshotInto(ctx, tmp); err != nil {
		return fmt.Errorf("install: snapshot_create: %w", err)
	}
	if err := verify(ctx, tmp); err != nil {
		return fmt.Errorf("install: snapshot_verify: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("install: snapshot_flush: %w", err)
	}
	if err := fsys.syncFile(tmp); err != nil {
		return fmt.Errorf("install: snapshot_flush: %w", err)
	}
	// Never replace a snapshot that is already published, and never treat a
	// failed lookup as proof that the name is free.
	switch _, statErr := fsys.lstat(final); {
	case statErr == nil:
		return fmt.Errorf("install: snapshot_publish: %s already exists", final)
	case !errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("install: snapshot_publish: %w", statErr)
	}
	if err := fsys.rename(tmp, final); err != nil {
		return fmt.Errorf("install: snapshot_publish: %w", err)
	}
	renamed = true
	if err := fsys.syncDir(dir); err != nil {
		return fmt.Errorf("install: snapshot_publish: %w", err)
	}
	renamed = false // published and durable: the retention step below never unpublishes it
	if err := prune(dir); err != nil {
		return fmt.Errorf("install: snapshot_retention: %w", err)
	}
	return nil
}

// verify opens the copy through a query-only connection (a snapshot is never
// written) and requires a clean integrity_check and no foreign-key violation.
func verify(ctx context.Context, path string) (err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return errors.Join(err, rows.Close())
		}
		problems = append(problems, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return fmt.Errorf("integrity_check: %s", strings.Join(problems, "; "))
	}
	rows, err = conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	violations := 0
	for rows.Next() {
		violations++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if violations > 0 {
		return fmt.Errorf("foreign_key_check: %d violations", violations)
	}
	return nil
}

// published lists the snapshots this package published in dir, newest first.
// The stamp decides, the whole name breaks a tie.
func published(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && snapshotName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := strings.Compare(snapshotName.FindStringSubmatch(b)[2], snapshotName.FindStringSubmatch(a)[2]); c != 0 {
			return c
		}
		return strings.Compare(b, a)
	})
	return names, nil
}

// prune removes every published snapshot but the newest keepSnapshots. The
// snapshot just published carries the newest stamp (see stampAfter), so it is
// always one of the kept ones.
func prune(dir string) error {
	names, err := published(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names[min(len(names), keepSnapshots):] {
		errs = append(errs, removeIfPresent(filepath.Join(dir, name)))
	}
	return errors.Join(errs...)
}

// stampAfter is now, moved just past the newest published stamp when the clock
// stands at or before it (a clock set back): names then keep the order the
// snapshots were published in, so retention never counts the one just made as
// the oldest.
func stampAfter(dir string, now time.Time) (time.Time, error) {
	names, err := published(dir)
	if err != nil || len(names) == 0 {
		return now, err
	}
	newest, err := time.Parse(snapshotStamp, snapshotName.FindStringSubmatch(names[0])[2])
	if err != nil {
		return now, err
	}
	if !now.After(newest) {
		return newest.Add(time.Nanosecond), nil
	}
	return now, nil
}

// removeTemps removes the temporary files an interrupted publication left.
func removeTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			errs = append(errs, removeIfPresent(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// syncFile flushes the file at path to stable storage.
func syncFile(path string) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}
