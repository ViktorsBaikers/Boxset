// Package install owns the data directory: its layout, the lifetime lock that
// admits one server per directory, the refusal of storage that cannot lock or
// flush, owner-only secret files and the verified snapshot published before a
// migration. It does not import the store's internals and never runs inside a
// store transaction.
package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Layout names the files a data directory holds.
type Layout struct {
	Dir string
}

// DB is the SQLite file the store opens.
func (l Layout) DB() string { return filepath.Join(l.Dir, "boxset.db") }

// Lock is the file the lifetime lock is taken on. It is never the database
// file: BSD flock and fcntl locks interact, and SQLite takes fcntl locks on
// its own file.
func (l Layout) Lock() string { return filepath.Join(l.Dir, "boxset.lock") }

// Backups is the directory that holds the published pre-migration snapshots.
func (l Layout) Backups() string { return filepath.Join(l.Dir, "backups") }

// ErrLockHeld: another process holds the lifetime lock of the data directory.
var ErrLockHeld = errors.New("install: the data directory is locked by another process")

// ErrSnapshotUnsupported: this platform has no proven namespace-durability
// barrier, so a pre-migration snapshot cannot be published safely. The
// migration is refused before anything is created or changed.
var ErrSnapshotUnsupported = errors.New("install: snapshot_unsupported: this platform cannot yet make a pre-migration backup durable, so a database that needs migrating is not touched")

// ResolveDir returns the one absolute, symlink-free filesystem spelling of a
// data directory: the mount check, the lock and the SQLite file name all use
// it, so a value shaped like a SQLite URI ("file:...") is a relative directory
// name, never an alias of another path. The part of dir that does not exist
// yet is kept as written below its nearest existing ancestor.
func ResolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	existing, rest := abs, ""
	for {
		if _, statErr := os.Stat(existing); statErr == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	if existing, err = filepath.EvalSymlinks(existing); err != nil {
		return "", err
	}
	return filepath.Join(existing, rest), nil
}

// MountRefusedError: the data directory lies on a filesystem that cannot give
// Boxset the lock and flush guarantees it relies on. Fix says, in plain words,
// where the data directory has to live instead.
type MountRefusedError struct {
	FS  string
	Fix string
}

func (e *MountRefusedError) Error() string {
	return "mount_refused fs=" + e.FS + ": the data directory is on a filesystem Boxset cannot lock or flush reliably. " + e.Fix
}

// Lock is the held lifetime lock. The process keeps it until Release: the
// operating system drops it when the process exits for any reason.
type Lock struct {
	f *os.File // referenced for the process lifetime: a finalizer would close it and release the lock
}

// Acquire creates dir when it is missing and takes the exclusive lifetime lock
// on its lock file without waiting. A held lock is ErrLockHeld.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("install: create data directory: %w", err)
	}
	f, err := os.OpenFile(Layout{Dir: dir}.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("install: open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. Call it only after every use of the data directory
// has ended, and once.
func (l *Lock) Release() error { return l.f.Close() }

// unlinkRegular removes path when it is a regular file and does nothing when it
// does not exist. A symlink, directory or any other kind of entry is refused
// and left alone.
func unlinkRegular(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("install: %s is not a regular file", path)
	}
	return os.Remove(path)
}
