//go:build windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadSecretInput reads at most max bytes of an owner-only regular file at path
// and refuses every other shape: a file whose owner is not the current user, one
// whose DACL is unprotected, inherits an entry or grants anyone but that user, a
// symlink, a FIFO or any other non-regular entry, and a file that is empty or
// larger than max. The owner and DACL checks run on the opened handle before any
// byte is read; the name is opened through an os.Root of its own directory, so
// nothing outside that directory can be reached. Compile-only: its runtime is
// not run (docs/portability.md).
func ReadSecretInput(path string, max int64) ([]byte, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	base = filepath.Clean(base)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: %s is not a file name", ErrSecretInput, path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSecretInput, path, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(base, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSecretInput, path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSecretInput, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrSecretInput, path)
	}
	if err := checkSecretFileHandle(f); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSecretInput, err)
	}
	return readBounded(f, max)
}

// The two ACE facts this check needs, spelled here so the check does not depend
// on a constant that a dependency release may rename.
const (
	accessAllowedAceType = 0x0
	inheritedAceFlag     = 0x10
)

// aclHeader is the fixed head of an ACL, read from the descriptor the kernel
// returned because x/sys exposes the ACL only through GetAce.
type aclHeader struct {
	revision byte
	sbz1     byte
	size     uint16
	aceCount uint16
	sbz2     uint16
}

// checkSecretFileHandle requires the opened file's owner to be the current token
// user and its DACL to be protected and to grant that user alone, with no
// inherited entry. Compile-only: its runtime is not run.
func checkSecretFileHandle(f *os.File) error {
	return requireOwnerOnlyDACL(windows.Handle(f.Fd()), f.Name())
}

func requireOwnerOnlyDACL(h windows.Handle, name string) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("install: %s: owner unreadable: %w", name, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("install: %s: current user unreadable: %w", name, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("install: %s: owner unreadable: %w", name, err)
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("%w: %s belongs to another user", ErrSecretFileUnsafe, name)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("install: %s: DACL unreadable: %w", name, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%w: %s inherits its access entries", ErrSecretFileUnsafe, name)
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("install: %s: DACL unreadable: %w", name, err)
	}
	if dacl == nil || defaulted {
		return fmt.Errorf("%w: %s has no explicit owner-only access list", ErrSecretFileUnsafe, name)
	}
	count := (*aclHeader)(unsafe.Pointer(dacl)).aceCount
	for i := uint32(0); i < uint32(count); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("install: %s: access entry unreadable: %w", name, err)
		}
		if ace.Header.AceFlags&inheritedAceFlag != 0 {
			return fmt.Errorf("%w: %s inherits an access entry", ErrSecretFileUnsafe, name)
		}
		if ace.Header.AceType != accessAllowedAceType {
			return fmt.Errorf("%w: %s has a non-allow access entry", ErrSecretFileUnsafe, name)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) {
			return fmt.Errorf("%w: %s grants another principal", ErrSecretFileUnsafe, name)
		}
	}
	return nil
}

// secretFileBarrier flushes the file and its associated metadata.
var secretFileBarrier = func(f *os.File) error {
	return windows.FlushFileBuffers(windows.Handle(f.Fd()))
}

// secretDirBarrier reports success without a call: there is no directory handle
// to flush on Windows, and FlushFileBuffers on the published file already
// flushed the metadata that names it.
var secretDirBarrier = func(string) error { return nil }
