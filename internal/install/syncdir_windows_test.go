//go:build windows

package install

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Windows has no proven namespace barrier: the real barrier and directory flush
// refuse, never return nil, and publication refuses before it creates
// anything. Compile-only in this repository: this runtime is NOT-RUN
// (docs/portability.md).
func TestSnapshotUnsupportedWindows(t *testing.T) {
	if err := namespaceBarrier(); !errors.Is(err, ErrSnapshotUnsupported) {
		t.Errorf("namespaceBarrier() = %v, want ErrSnapshotUnsupported", err)
	}
	if err := syncDir(t.TempDir()); !errors.Is(err, ErrSnapshotUnsupported) {
		t.Errorf("syncDir() = %v, want ErrSnapshotUnsupported", err)
	}

	l := Layout{Dir: t.TempDir()}
	copied := false
	err := PublishSnapshot(context.Background(), l, store.Pending{
		From: 1, To: 2,
		SnapshotInto: func(context.Context, string) error { copied = true; return nil },
	})
	if !errors.Is(err, ErrSnapshotUnsupported) {
		t.Errorf("PublishSnapshot = %v, want ErrSnapshotUnsupported", err)
	}
	if copied {
		t.Error("the snapshot was taken although the barrier is unavailable")
	}
	if _, err := os.Stat(l.Backups()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backups exists after the refusal (%v)", err)
	}
}
