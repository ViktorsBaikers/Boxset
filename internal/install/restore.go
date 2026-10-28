package install

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	_ "modernc.org/sqlite" // the driver ReadAuthority and the recovery copy open the files with
)

var (
	// ErrRestoreUnsupported: this platform has no proven namespace-durability
	// barrier for the restore journal, the recovery copy and the replacement, so
	// every restore and every journal continuation is refused before anything is
	// created or changed.
	ErrRestoreUnsupported = errors.New("install: restore_unsupported: restore is not supported on Windows yet; nothing was changed")
	// ErrRestoreInProgress: a restore journal exists, so the installation is
	// still recovering. Start the server to finish it.
	ErrRestoreInProgress = errors.New("install: restore_in_progress: a restore journal is present; start the server to finish it")
	// ErrAuthorizationStateUnavailable: the current database does not hold the
	// identity and access rows a restore must preserve, so there is nothing to
	// restore over.
	ErrAuthorizationStateUnavailable = errors.New("install: authorization_state_unavailable: the current database has no identity and user state to restore over")
	// ErrInstallationMismatch: the snapshot names no installation or another one
	// than the current database does.
	ErrInstallationMismatch = errors.New("install: installation_mismatch: the snapshot does not belong to this installation")
	// ErrSnapshotInvalid: the snapshot is not a database this binary can restore.
	ErrSnapshotInvalid = errors.New("install: snapshot_invalid: the file is not a restorable Boxset database")
	// ErrSnapshotNewer: the snapshot was written by a newer Boxset.
	ErrSnapshotNewer = errors.New("install: snapshot_newer: the snapshot was written by a newer Boxset than this binary")
	// ErrFileNotAuthority: the file cannot be read as an installation database.
	ErrFileNotAuthority = errors.New("install: the file is not a readable installation database")
	// ErrSecretFileUnsafe: the file that holds a key is not an owner-only regular
	// file of the bytes its name promises.
	ErrSecretFileUnsafe = errors.New("install: key_unsafe: the key file is not an owner-only regular file of the named bytes")
	// ErrSecretInput: the file a credential is read from is not an owner-only
	// regular file within the byte bound.
	ErrSecretInput = errors.New("install: input_invalid: the file is not an owner-only regular file of 1 byte to the bound")
	// errProbePending stops the store's migration inspection at the first pending
	// migration, so a probe never changes the file it reads.
	errProbePending = errors.New("install: probe stopped at a pending migration")
)

// SecretSyncError: a key file's platform barrier did not complete. Stage names
// which one (file or dir on unix, metadata on Windows) so the operator knows what
// to fix before retrying.
type SecretSyncError struct {
	Stage string
	Err   error
}

func (e *SecretSyncError) Error() string {
	return "install: key_sync_failed stage=" + e.Stage + ": " + e.Err.Error()
}

func (e *SecretSyncError) Unwrap() error { return e.Err }

// Phase is where a restore journal stands.
type Phase string

const (
	// PhaseBeforeReplace: the recovery copy is being made; the replacement has
	// not been committed. A journal found in this phase is discarded.
	PhaseBeforeReplace Phase = "before-replacement"
	// PhaseReconcile: the replacement is committed to happen. A journal found in
	// this phase is completed: the staged snapshot is put in place, migrated and
	// reconciled before access resumes.
	PhaseReconcile Phase = "reconcile"
)

// recoverystamp is the UTC layout of a recovery copy's name.
const recoverystamp = snapshotStamp

// Recovery is the durable record of one restore, written before the recovery
// copy exists and kept until reconciliation committed.
type Recovery struct {
	Phase          Phase  `json:"phase"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	RecoveryPath   string `json:"recovery_path"`
	RecoverySHA256 string `json:"recovery_sha256"`
	Epoch          int64  `json:"epoch"`
}

// Journal is the restore journal of the data directory.
func (l Layout) Journal() string { return filepath.Join(l.Dir, "restore.journal") }

// RestoreStaged is where the snapshot is staged before it replaces the database.
func (l Layout) RestoreStaged() string { return filepath.Join(l.Dir, ".restore.db") }

// RecoveryDir holds the recovery copies a restore retained.
func (l Layout) RecoveryDir() string { return filepath.Join(l.Dir, "recovery") }

// AuthorityUser is one account as a database file holds it.
type AuthorityUser struct {
	ID          int64
	Username    string
	UsernameKey string
	PasswordPHC string
	Role        string
	Disabled    int64
	Revision    int64
	CreatedAt   int64
}

// AuthoritySetup is the one-time first-admin credential as a file holds it.
// ConsumedAt is 0 while setup is open.
type AuthoritySetup struct {
	TokenSHA256 string
	ConsumedAt  int64
}

// Authority is the identity and access state of one database file: what a
// restore must preserve from the current installation and never take from the
// snapshot.
type Authority struct {
	InstallID string
	Epoch     int64
	Users     []AuthorityUser
	Setup     *AuthoritySetup
}

// HasAuthority reports whether a file already holds access state: any account,
// or a consumed setup credential. A file without it is never authority for
// another file's snapshot.
func (a Authority) HasAuthority() bool {
	return len(a.Users) > 0 || (a.Setup != nil && a.Setup.ConsumedAt > 0)
}

// ReadAuthority reads the identity and access rows of an existing database file.
// It creates nothing, changes nothing and refuses a file that is not a readable
// installation database.
func ReadAuthority(ctx context.Context, path string) (a Authority, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return a, fmt.Errorf("%w: %s is not a non-empty regular file", ErrFileNotAuthority, path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	var verdict []string
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return a, errors.Join(fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err), rows.Close())
		}
		verdict = append(verdict, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	if len(verdict) != 1 || verdict[0] != "ok" {
		return a, fmt.Errorf("%w: %s: integrity_check %s", ErrFileNotAuthority, path, strings.Join(verdict, "; "))
	}
	switch err := conn.QueryRowContext(ctx, "SELECT install_id, restore_epoch FROM installation WHERE id = 1").Scan(&a.InstallID, &a.Epoch); {
	case errors.Is(err, sql.ErrNoRows):
		a.InstallID, a.Epoch = "", 0
	case err != nil:
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	var setup AuthoritySetup
	switch err := conn.QueryRowContext(ctx, "SELECT token_sha256, coalesce(consumed_at, 0) FROM setup WHERE id = 1").Scan(&setup.TokenSHA256, &setup.ConsumedAt); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	default:
		a.Setup = &setup
	}
	rows, err = conn.QueryContext(ctx, "SELECT id, username, username_key, password_phc, role, disabled, revision, created_at FROM user ORDER BY id")
	if err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	for rows.Next() {
		var u AuthorityUser
		if err := rows.Scan(&u.ID, &u.Username, &u.UsernameKey, &u.PasswordPHC, &u.Role, &u.Disabled, &u.Revision, &u.CreatedAt); err != nil {
			return a, errors.Join(fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err), rows.Close())
		}
		a.Users = append(a.Users, u)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return a, fmt.Errorf("%w: %s: %w", ErrFileNotAuthority, path, err)
	}
	return a, nil
}

// RestoreOptions carries what a restore needs from its caller.
type RestoreOptions struct {
	// Reconcile applies the pinned pre-replacement authority to the restored file
	// in one Write before the installation is ready. Required.
	Reconcile func(ctx context.Context, db *store.DB, current Authority, r Recovery) error
	// Snapshot publishes the pre-migration snapshot a restored file needs before
	// it is migrated. Nil refuses such a migration with store.ErrBackupRequired.
	Snapshot func(ctx context.Context, l Layout, p store.Pending) error
	// Log receives the fixed restore observations; it must be a redacting logger.
	Log *slog.Logger

	fs restoreFS // platform seam; the tests fail a step on demand
}

// requireNamespace runs the platform barrier. It is the same barrier a snapshot
// takes, and its own error says only that the platform cannot make a namespace
// change durable; a restore reports its own reason, so `boxset restore` and a
// start over a journal refuse with one vocabulary.
func (f restoreFS) requireNamespace() error {
	if err := f.namespaceBarrier(); err != nil {
		if errors.Is(err, ErrSnapshotUnsupported) {
			return ErrRestoreUnsupported
		}
		return err
	}
	return nil
}

// restoreFS is the filesystem seam of a restore: the steps a proof needs to stop
// inside (an interrupted replacement) and the platform barrier.
type restoreFS struct {
	namespaceBarrier  func() error
	syncFile          func(path string) error
	syncDir           func(path string) error
	rename            func(oldpath, newpath string) error
	lstat             func(path string) (os.FileInfo, error)
	now               func() time.Time
	beforeReplacement func() error // test seam: after the recovery copy, before the committed replacement
	afterReplacement  func() error // test seam: after the replacement, before the pre-ready Write
	afterReconcile    func() error // test seam: after the pre-ready Write, before the journal is removed
}

var osRestoreFS = restoreFS{
	namespaceBarrier: namespaceBarrier,
	syncFile:         syncFile,
	syncDir:          syncDir,
	rename:           os.Rename,
	lstat:            os.Lstat,
	now:              time.Now,
}

func (o RestoreOptions) withDefaults() RestoreOptions {
	if o.fs.namespaceBarrier == nil {
		o.fs = osRestoreFS
	}
	return o
}

// Restore replaces the database of the stopped installation l with snapshot,
// keeping a recovery copy of the current state and applying the current
// authority to the restored file before access resumes. It calls the platform
// namespace barrier first, then takes the lifetime lock without waiting, so a
// running server is refused and every later step runs under the lock.
//
// Nothing about the current installation is taken from the snapshot: the
// installation identity must match, the current file must already hold the
// access state, and the pinned recovery copy is the only revocation authority
// the reconciliation applies.
func Restore(ctx context.Context, l Layout, snapshot string, opts RestoreOptions) (out Recovery, err error) {
	opts = opts.withDefaults()
	if opts.Reconcile == nil {
		return out, errors.New("install: restore needs the reconciliation step")
	}
	// Windows refuses here, before the lock, the snapshot file or any journal.
	if err := opts.fs.requireNamespace(); err != nil {
		return out, err
	}
	lock, err := Acquire(l.Dir)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	if _, err := opts.fs.lstat(l.Journal()); err == nil {
		return out, ErrRestoreInProgress
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, fmt.Errorf("install: restore: %w", err)
	}

	// Stage the snapshot and validate its schema before anything is journaled: a
	// newer snapshot, a broken hash record or a file that is not a store is
	// refused with the current database untouched and no journal left behind.
	staged := l.RestoreStaged()
	// A staged copy and the log a previous attempt left are this installation's
	// own leftovers: a stale log would otherwise be replayed into the file this
	// attempt stages.
	if err := removeStaged(staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := copyFile(snapshot, staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	discardStaged := true
	defer func() {
		if discardStaged {
			err = errors.Join(err, removeStaged(staged))
		}
	}()
	if err := opts.fs.syncFile(staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	// VACUUM INTO and any plain copy write the file in rollback-journal mode; the
	// staged file must already be the WAL file the pools and the restored
	// installation assume, and its log must be empty before it is renamed.
	if err := requireWAL(ctx, staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if _, err := probeSnapshot(ctx, staged); err != nil {
		return out, err
	}
	if err := checkpointAndDropLog(ctx, staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	snapSum, err := fileSum(snapshot)
	if err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	snap, err := ReadAuthority(ctx, staged)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrSnapshotInvalid, err)
	}
	if snap.InstallID == "" {
		return out, ErrInstallationMismatch
	}

	current, err := ReadAuthority(ctx, l.DB())
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrAuthorizationStateUnavailable, err)
	}
	if current.InstallID == "" {
		return out, fmt.Errorf("%w: the current database names no installation", ErrAuthorizationStateUnavailable)
	}
	if current.InstallID != snap.InstallID {
		return out, ErrInstallationMismatch
	}
	if snap.HasAuthority() && !current.HasAuthority() {
		return out, fmt.Errorf("%w: the snapshot holds access state the current installation does not", ErrAuthorizationStateUnavailable)
	}

	out = Recovery{
		Phase:          PhaseBeforeReplace,
		SnapshotSHA256: snapSum,
		RecoveryPath:   filepath.Join(l.RecoveryDir(), "restore-"+opts.fs.now().UTC().Format(recoverystamp)+".db"),
		Epoch:          max(current.Epoch, snap.Epoch) + 1,
	}
	if err := writeJournal(opts.fs, l, out); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	// The recovery copy is the current state, WAL frames included: a main-file
	// copy would miss every committed frame still in the log.
	if err := os.MkdirAll(l.RecoveryDir(), 0o700); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := vacuumInto(ctx, l.DB(), out.RecoveryPath); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := requireWAL(ctx, out.RecoveryPath); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := checkpointAndDropLog(ctx, out.RecoveryPath); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := opts.fs.syncFile(out.RecoveryPath); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := opts.fs.syncDir(l.RecoveryDir()); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if out.RecoverySHA256, err = fileSum(out.RecoveryPath); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if err := writeJournal(opts.fs, l, out); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	// The copy must read back as the installation's own current authority; a copy
	// that does not is never authority for the replacement.
	pinned, err := ReadAuthority(ctx, out.RecoveryPath)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrAuthorizationStateUnavailable, err)
	}
	if pinned.InstallID != current.InstallID {
		return out, ErrInstallationMismatch
	}
	if _, err := probeSnapshot(ctx, out.RecoveryPath); err != nil {
		return out, err
	}

	if opts.fs.beforeReplacement != nil {
		if err := opts.fs.beforeReplacement(); err != nil {
			return out, err
		}
	}
	// The replacement is committed to happen from here: the journal names the
	// reconcile phase before the database file changes, so a crash on either side
	// of the rename resumes rather than discards.
	out.Phase = PhaseReconcile
	if err := writeJournal(opts.fs, l, out); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	// The journal now names the staged copy, so a replacement that fails here
	// leaves it for the resume that finishes what the journal committed to.
	discardStaged = false
	if err := replace(ctx, opts.fs, l, staged); err != nil {
		return out, fmt.Errorf("install: restore: %w", err)
	}
	if opts.fs.afterReplacement != nil {
		if err := opts.fs.afterReplacement(); err != nil {
			return out, err
		}
	}
	if err := finish(ctx, l, out, pinned, opts); err != nil {
		return out, err
	}
	return out, nil
}

// Resume finishes or discards the restore a journal records. The caller holds
// the lifetime lock. Where the platform has no namespace barrier every journal is
// refused before it is read, so nothing is discarded or resumed there.
func Resume(ctx context.Context, l Layout, opts RestoreOptions) (out Recovery, err error) {
	opts = opts.withDefaults()
	if opts.Reconcile == nil {
		return out, errors.New("install: resume needs the reconciliation step")
	}
	if err := opts.fs.requireNamespace(); err != nil {
		return out, err
	}
	out, ok, err := readJournal(opts.fs, l)
	if err != nil || !ok {
		return out, err
	}
	switch out.Phase {
	case PhaseBeforeReplace:
		// Nothing was replaced: the staged copy and the journal are this
		// installation's own leftovers.
		errs := []error{removeStaged(l.RestoreStaged())}
		if err := removeIfPresent(l.Journal()); err != nil {
			return out, fmt.Errorf("install: resume: %w", err)
		}
		if err := opts.fs.syncDir(l.Dir); err != nil {
			return out, fmt.Errorf("install: resume: %w", err)
		}
		return out, errors.Join(errs...)
	case PhaseReconcile:
		sum, err := fileSum(out.RecoveryPath)
		if err != nil {
			return out, fmt.Errorf("install: resume: %w", err)
		}
		if out.RecoverySHA256 == "" || sum != out.RecoverySHA256 {
			return out, fmt.Errorf("install: resume: the recovery copy is not the pinned one")
		}
		pinned, err := ReadAuthority(ctx, out.RecoveryPath)
		if err != nil {
			return out, fmt.Errorf("%w: %w", ErrAuthorizationStateUnavailable, err)
		}
		if err := replace(ctx, opts.fs, l, l.RestoreStaged()); err != nil {
			return out, fmt.Errorf("install: resume: %w", err)
		}
		if err := finish(ctx, l, out, pinned, opts); err != nil {
			return out, err
		}
		return out, nil
	default:
		return out, fmt.Errorf("install: resume: unknown journal phase %q", out.Phase)
	}
}

// replace puts the staged snapshot in place of the database. A close of every
// earlier connection is the caller's: a -wal or -shm left beside the database
// would otherwise be replayed into the restored file and mix two states.
func replace(ctx context.Context, fsys restoreFS, l Layout, staged string) error {
	if _, err := fsys.lstat(staged); errors.Is(err, fs.ErrNotExist) {
		return nil // already replaced before an interruption
	} else if err != nil {
		return err
	}
	// The store's own connections are closed; these two are the log a stopped
	// write may have left behind.
	if err := checkpointAndDropLog(ctx, l.DB()); err != nil {
		return err
	}
	if err := fsys.syncFile(staged); err != nil {
		return err
	}
	// The validation reads reopened the staged file in WAL mode, so it has a log
	// of its own again: a log is never renamed into place with the file it logged.
	if err := dropLog(staged); err != nil {
		return err
	}
	if err := fsys.rename(staged, l.DB()); err != nil {
		return err
	}
	return fsys.syncDir(l.Dir)
}

// finish brings a replaced file up to the binary's schema and applies the pinned
// current authority in one pre-ready Write, then removes the journal.
func finish(ctx context.Context, l Layout, r Recovery, pinned Authority, opts RestoreOptions) (err error) {
	// The restored file is in WAL mode before any pool reads it: VACUUM INTO and a
	// plain copy both write rollback-journal files, and every policy below assumes
	// the log.
	if err := requireWAL(ctx, l.DB()); err != nil {
		return fmt.Errorf("install: restore: %w", err)
	}
	db, err := store.Open(ctx, l.DB(), store.Options{
		Log: opts.Log,
		BeforeMigrate: func(ctx context.Context, p store.Pending) error {
			if opts.Snapshot == nil {
				return store.ErrBackupRequired
			}
			return opts.Snapshot(ctx, l, p)
		},
	})
	if err != nil {
		return fmt.Errorf("install: restore: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, db.Close())
		}
	}()
	if err := opts.Reconcile(ctx, db, pinned, r); err != nil {
		return err
	}
	if opts.fs.afterReconcile != nil {
		if err := opts.fs.afterReconcile(); err != nil {
			return err
		}
	}
	if err := db.Close(); err != nil {
		closed = true
		return fmt.Errorf("install: restore: %w", err)
	}
	closed = true
	if err := removeIfPresent(l.Journal()); err != nil {
		return fmt.Errorf("install: restore: %w", err)
	}
	return opts.fs.syncDir(l.Dir)
}

// requireWAL asserts that the database file is in WAL mode, the mode every pool
// policy and every credential write assumes.
func requireWAL(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("install: the restored file reads journal_mode %q, want wal", mode)
	}
	return nil
}

// checkpointAndDropLog closes the database's write-ahead log and removes the log
// and shared-memory files a stopped process may have left, so they cannot be
// replayed into a different file.
func checkpointAndDropLog(ctx context.Context, path string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			return errors.Join(err, db.Close())
		}
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			return errors.Join(err, conn.Close(), db.Close())
		}
		if strings.EqualFold(mode, "wal") {
			if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				return errors.Join(err, conn.Close(), db.Close())
			}
		}
		if err := errors.Join(conn.Close(), db.Close()); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return dropLog(path)
}

// probeSnapshot validates a file through the store's own migration inspection:
// every recorded hash must match the embedded migration, a file a newer binary
// wrote is refused, and a pending migration is reported without being applied.
func probeSnapshot(ctx context.Context, path string) (pending bool, err error) {
	db, err := store.Open(ctx, path, store.Options{BeforeMigrate: func(context.Context, store.Pending) error { return errProbePending }})
	switch {
	case errors.Is(err, errProbePending):
		return true, nil
	case errors.Is(err, store.ErrSchemaNewer):
		return false, fmt.Errorf("%w: %w", ErrSnapshotNewer, err)
	case err != nil:
		return false, fmt.Errorf("%w: %w", ErrSnapshotInvalid, err)
	}
	if err := db.Close(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrSnapshotInvalid, err)
	}
	return false, nil
}

// vacuumInto copies the database at src as it stands, committed log frames
// included, to the not-yet-existing dest.
func vacuumInto(ctx context.Context, src, dest string) error {
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}

// writeJournal replaces the restore journal atomically: the bytes are flushed
// before the name exists and the directory is flushed after it does.
func writeJournal(fsys restoreFS, l Layout, r Recovery) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := l.Journal() + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	if err := fsys.syncFile(tmp); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	if err := fsys.rename(tmp, l.Journal()); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	return fsys.syncDir(l.Dir)
}

// readJournal returns the journal of l, and whether there is one.
func readJournal(fsys restoreFS, l Layout) (r Recovery, ok bool, err error) {
	info, err := fsys.lstat(l.Journal())
	if errors.Is(err, fs.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("install: restore journal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return r, false, fmt.Errorf("install: restore journal: %s is not a regular file", l.Journal())
	}
	body, err := os.ReadFile(l.Journal())
	if err != nil {
		return r, false, fmt.Errorf("install: restore journal: %w", err)
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return r, false, fmt.Errorf("install: restore journal: %w", err)
	}
	if r.Phase != PhaseBeforeReplace && r.Phase != PhaseReconcile {
		return r, false, fmt.Errorf("install: restore journal: unknown phase %q", r.Phase)
	}
	return r, true, nil
}

// dropLog removes the write-ahead log and the shared-memory file a stopped write
// may have left beside path, so they cannot be replayed into a different file.
func dropLog(path string) error {
	return errors.Join(removeIfPresent(path+"-wal"), removeIfPresent(path+"-shm"))
}

// removeStaged removes a staged snapshot and the journal and log files a probe or
// a stopped write may have left beside it.
func removeStaged(staged string) error {
	return errors.Join(
		removeIfPresent(staged),
		dropLog(staged),
		removeIfPresent(staged+"-journal"),
	)
}

// fileSum is the lowercase hex SHA-256 of the file at path.
func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile writes the bytes of src to the new file dst and flushes it.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := removeIfPresent(dst); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, out.Close(), removeIfPresent(dst))
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

// readBounded reads 1..max bytes from an already checked handle.
func readBounded(f *os.File, max int64) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("%w: the byte bound %d is not positive", ErrSecretInput, max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSecretInput, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: the file is empty", ErrSecretInput)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: the file is larger than %d bytes", ErrSecretInput, max)
	}
	return data, nil
}

// openSecretFile opens path for reading and writing after requiring a regular
// file, never a symlink: the platform barrier of a key file needs a writable
// handle, and a link would make the checked file another one.
func openSecretFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrSecretFileUnsafe, path)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// requireFingerprint requires the opened file's bytes to hash to keyID.
func requireFingerprint(f *os.File, path, keyID string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("install: %s: %w", path, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("install: %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != keyID {
		return fmt.Errorf("%w: %s holds the bytes of %s", ErrSecretFileUnsafe, path, got)
	}
	return nil
}
