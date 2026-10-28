//go:build windows

package install

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive LockFileEx lock on the first byte without
// waiting. The errno a contended lock reports is unproven here, so every
// failure counts as a held lock; the lock is released when the handle closes.
func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err != nil {
		return ErrLockHeld
	}
	return nil
}
