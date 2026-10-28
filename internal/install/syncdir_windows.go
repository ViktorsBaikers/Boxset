//go:build windows

package install

// namespaceBarrier: Windows has no proven way here to make a rename or a new
// directory entry durable (NTFS journalling is not that proof), so snapshot
// publication is refused up front. Never nil: see docs/portability.md.
func namespaceBarrier() error { return ErrSnapshotUnsupported }

// syncDir cannot flush a directory on Windows and never reports that it did.
func syncDir(string) error { return ErrSnapshotUnsupported }
