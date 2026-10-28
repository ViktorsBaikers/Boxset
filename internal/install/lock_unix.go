//go:build unix

package install

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock without waiting. EWOULDBLOCK is a lock
// another process holds; any other errno is a real failure the caller must see.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLockHeld
	}
	if err != nil {
		return fmt.Errorf("install: lock %s: %w", f.Name(), err)
	}
	return nil
}
