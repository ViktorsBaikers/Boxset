//go:build windows

package install

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WriteSecretFile writes data to a new file at path that only its owner can
// read. Unix permission bits mean nothing on Windows, so the file is created
// with a protected DACL (no inherited entries) that grants full control to the
// current user alone, in the same call that creates it. An existing regular
// file is replaced by unlinking it and creating it again (CREATE_NEW); a
// symlink or any other kind of entry is refused. Compile-only: its runtime is
// not run (docs/portability.md).
func WriteSecretFile(path string, data []byte) (err error) {
	if err := unlinkRegular(path); err != nil {
		return err
	}
	f, err := createOwnerOnly(path)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close(), os.Remove(path))
		}
	}()
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
// bytes and metadata. It never writes and never changes bytes: a file that is
// already durable is verified again, never rewritten. Compile-only: its runtime
// is not run (docs/portability.md).
func VerifySecretFile(path, keyID string) error {
	f, err := openSecretFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := checkSecretFileHandle(f); err != nil {
		return fmt.Errorf("%w: %w", ErrSecretFileUnsafe, err)
	}
	if err := requireFingerprint(f, path, keyID); err != nil {
		return err
	}
	if err := secretFileBarrier(f); err != nil {
		return &SecretSyncError{Stage: "metadata", Err: err}
	}
	return nil
}

func createOwnerOnly(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// D:P: a protected DACL; (A;;FA;;;<sid>): allow FILE_ALL_ACCESS to the current user only.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
