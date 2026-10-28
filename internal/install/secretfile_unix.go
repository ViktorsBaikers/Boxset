//go:build unix

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// effectiveUID is the owner every secret file must end up with; a proof
// replaces it to present a foreign owner.
var effectiveUID = os.Geteuid

// WriteSecretFile writes data to a new file at path that only its owner can
// read. An existing regular file is replaced by unlinking it and creating the
// path again with O_EXCL, so the old inode, its mode and every other name for
// it are never written through; a symlink or any other kind of entry is
// refused. The created file must have no group or other permission bit and the
// current user as owner, checked before any byte is written; otherwise it is
// removed and nothing is written.
func WriteSecretFile(path string, data []byte) (err error) {
	if err := unlinkRegular(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close(), os.Remove(path))
		}
	}()
	if err := requireOwnerOnly(f); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// VerifySecretFile requires path to be the owner-only regular file that holds the
// key whose fingerprint is keyID, and completes the platform barrier for its
// bytes and its directory entry. It never writes and never changes bytes: a file
// that is already durable is verified again, never rewritten. Compile-time kind
// only: the Windows branch is not run (docs/portability.md).
func VerifySecretFile(path, keyID string) error {
	f, err := openSecretFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := requireOwnerOnly(f); err != nil {
		return fmt.Errorf("%w: %w", ErrSecretFileUnsafe, err)
	}
	if err := requireFingerprint(f, path, keyID); err != nil {
		return err
	}
	if err := secretFileBarrier(f); err != nil {
		return &SecretSyncError{Stage: "file", Err: err}
	}
	if err := secretDirBarrier(filepath.Dir(path)); err != nil {
		return &SecretSyncError{Stage: "dir", Err: err}
	}
	return nil
}

// requireOwnerOnly refuses a file that is not a regular file, has any group or
// other permission bit, or belongs to another user.
func requireOwnerOnly(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("install: %s: owner unreadable", f.Name())
	}
	switch {
	case !fi.Mode().IsRegular():
		return fmt.Errorf("install: %s is not a regular file", f.Name())
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("install: %s has group or other permissions (%v)", f.Name(), fi.Mode().Perm())
	case int(st.Uid) != effectiveUID():
		return fmt.Errorf("install: %s belongs to uid %d, not the current user", f.Name(), st.Uid)
	}
	return nil
}
