//go:build unix

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ReadSecretInput reads at most max bytes of an owner-only regular file at path
// and refuses every other shape: a group- or other-readable file, a file owned
// by someone else, a symlink, a FIFO or any other non-regular entry, and a file
// that is empty or larger than max. The name is opened through an os.Root of its
// own directory, so nothing outside that directory can be reached, and with
// O_NONBLOCK, so a FIFO is judged rather than waited on.
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
	f, err := root.OpenFile(base, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSecretInput, path, err)
	}
	defer func() { _ = f.Close() }()
	if err := requireOwnerOnly(f); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSecretInput, err)
	}
	return readBounded(f, max)
}

// secretFileBarrier makes the file's bytes and metadata durable; a proof
// replaces it to fail on demand.
var secretFileBarrier = func(f *os.File) error { return f.Sync() }

// secretDirBarrier makes the directory entry durable. On unix it is the
// directory flush every secret-file publication needs.
var secretDirBarrier = func(dir string) error { return syncDir(dir) }
