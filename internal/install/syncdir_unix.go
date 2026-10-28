//go:build unix

package install

import (
	"errors"
	"os"
)

// namespaceBarrier reports whether a directory-entry change can be made
// durable here. On unix fsync of the directory does it.
func namespaceBarrier() error { return nil }

// syncDir flushes a directory entry change to stable storage.
func syncDir(path string) (err error) {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	return d.Sync()
}
