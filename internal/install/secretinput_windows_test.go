//go:build windows

package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Compile-only file: none of these runs here (docs/portability.md). The gate
// compiles it for windows/amd64 and windows/arm64 and reports runtime NOT-RUN.

// TestSecretInputWindowsOwnerOnlyAccepted: the file the owner-only writer creates
// is the one the owner-SID and protected-DACL check accepts.
func TestSecretInputWindowsOwnerOnlyAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteSecretFile(path, []byte("synthetic-retained")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSecretInput(path, 1<<20)
	if err != nil || !bytes.Equal(got, []byte("synthetic-retained")) {
		t.Fatalf("owner-only file: %q, %v", got, err)
	}
}

// TestSecretInputWindowsBroadDACLRefused: a null DACL, an Everyone grant, an
// inherited entry, an unprotected DACL and a foreign owner are all refused.
func TestSecretInputWindowsBroadDACLRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteSecretFile(path, []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, sddl string }{
		{"null DACL", "D:NO_ACCESS_CONTROL"},
		{"Everyone grant", "D:P(A;;FA;;;WD)"},
		{"inherited non-owner ACE", "D:P(A;;FA;;;SY)"},
		{"unprotected DACL", "D:(A;;FA;;;BA)"},
		{"foreign owner", "O:WDG:WD"},
	}
	for _, c := range cases {
		replaceSecurity(t, path, c.sddl)
		if _, err := ReadSecretInput(path, 1<<20); !errors.Is(err, ErrSecretInput) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// replaceSecurity writes an explicit security descriptor on path.
func replaceSecurity(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("%s: %v", sddl, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreUnsupportedWindows: every restore and every journal continuation is
// refused before a file is created or changed.
func TestRestoreUnsupportedWindows(t *testing.T) {
	dir := t.TempDir()
	l := Layout{Dir: dir}
	current := filepath.Join(dir, "boxset.db")
	snapshot := filepath.Join(dir, "snapshot.db")
	for _, path := range []string{current, snapshot} {
		if err := os.WriteFile(path, []byte("synthetic-database"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	reconcile := func(context.Context, *store.DB, Authority, Recovery) error {
		t.Error("a reconcile ran on a platform that refuses restore")
		return nil
	}
	if _, err := Restore(t.Context(), l, snapshot, RestoreOptions{Reconcile: reconcile}); !errors.Is(err, ErrRestoreUnsupported) {
		t.Fatalf("restore: %v", err)
	} else if errors.Is(err, ErrSnapshotUnsupported) {
		t.Fatalf("restore reported the snapshot barrier instead of the restore refusal: %v", err)
	}
	for _, name := range []string{"restore.journal", ".restore.db", filepath.Join("recovery", "restore-1.db")} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was created by a refused restore: %v", name, err)
		}
	}
	if after, err := os.ReadFile(current); err != nil || !bytes.Equal(before, after) {
		t.Fatal("a refused restore changed the database")
	}
	for _, phase := range []Phase{PhaseBeforeReplace, PhaseReconcile} {
		journal := []byte(`{"phase":"` + string(phase) + `","snapshot_sha256":"x","recovery_path":"y","recovery_sha256":"z","epoch":2}` + "\n")
		if err := os.WriteFile(l.Journal(), journal, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Resume(t.Context(), l, RestoreOptions{Reconcile: reconcile}); !errors.Is(err, ErrRestoreUnsupported) {
			t.Fatalf("%s journal resume: %v", phase, err)
		} else if errors.Is(err, ErrSnapshotUnsupported) {
			t.Fatalf("%s journal resume reported the snapshot barrier instead of the restore refusal: %v", phase, err)
		}
		if after, err := os.ReadFile(l.Journal()); err != nil || !bytes.Equal(journal, after) {
			t.Fatalf("%s journal was changed or removed: %v", phase, err)
		}
	}
}
