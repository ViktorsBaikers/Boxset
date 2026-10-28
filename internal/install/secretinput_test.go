//go:build unix

package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSecretInputBounds: a credential file is read only when it is an owner-only
// regular file of 1 byte to the bound, and every other shape is refused before a
// byte is read.
func TestSecretInputBounds(t *testing.T) {
	dir := t.TempDir()
	ownerOnly := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := WriteSecretFile(path, data); err != nil {
			t.Fatal(err)
		}
		return path
	}
	one := ownerOnly("one", []byte{1})
	if got, err := ReadSecretInput(one, 8); err != nil || !bytes.Equal(got, []byte{1}) {
		t.Fatalf("one byte: %v, %v", got, err)
	}
	full := ownerOnly("full", bytes.Repeat([]byte{2}, 4096))
	if got, err := ReadSecretInput(full, 4096); err != nil || len(got) != 4096 {
		t.Fatalf("bound: %d bytes, %v", len(got), err)
	}
	if _, err := ReadSecretInput(full, 4095); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("one byte past the bound: %v", err)
	}
	empty := ownerOnly("empty", nil)
	if _, err := ReadSecretInput(empty, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("empty file: %v", err)
	}
	if _, err := ReadSecretInput(filepath.Join(dir, "absent"), 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("absent file: %v", err)
	}

	broad := ownerOnly("broad", []byte{1})
	if err := os.Chmod(broad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretInput(broad, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("group-readable file: %v", err)
	}
	otherRead := ownerOnly("other", []byte{1})
	if err := os.Chmod(otherRead, 0o604); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretInput(otherRead, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("other-readable file: %v", err)
	}

	foreign := ownerOnly("foreign", []byte{1})
	euid := effectiveUID
	effectiveUID = func() int { return euid() + 1 }
	t.Cleanup(func() { effectiveUID = euid })
	if _, err := ReadSecretInput(foreign, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("foreign-owned file: %v", err)
	}
	effectiveUID = euid

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// O_NONBLOCK is what makes this a judgment rather than a wait: a plain open
	// of a FIFO with no writer blocks here forever.
	if _, err := ReadSecretInput(fifo, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("fifo: %v", err)
	}
	if _, err := ReadSecretInput(dir, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("directory: %v", err)
	}

	outside := t.TempDir()
	target := filepath.Join(outside, "key")
	if err := WriteSecretFile(target, []byte{3}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretInput(link, 8); !errors.Is(err, ErrSecretInput) {
		t.Fatalf("symlink out of the directory: %v", err)
	}
}

// TestVerifySecretFileBarrierStages: a key file is accepted only when its bytes
// are the ones its name promises and the platform barrier for the file and its
// directory completed, and none of these steps changes a byte.
func TestVerifySecretFileBarrierStages(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{5}, 32)
	path := filepath.Join(dir, "key")
	if err := WriteSecretFile(path, key); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(key)
	keyID := hex.EncodeToString(sum[:])
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySecretFile(path, keyID); err != nil {
		t.Fatalf("owner-only key: %v", err)
	}
	if err := VerifySecretFile(path, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrSecretFileUnsafe) {
		t.Fatalf("wrong fingerprint: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := VerifySecretFile(path, keyID); !errors.Is(err, ErrSecretFileUnsafe) {
		t.Fatalf("group-readable key: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	fileBarrier := secretFileBarrier
	secretFileBarrier = func(*os.File) error { return errors.New("synthetic file barrier failure") }
	var sync *SecretSyncError
	if err := VerifySecretFile(path, keyID); !errors.As(err, &sync) || sync.Stage != "file" {
		t.Fatalf("file barrier: %v", err)
	}
	secretFileBarrier = fileBarrier
	dirBarrier := secretDirBarrier
	secretDirBarrier = func(string) error { return errors.New("synthetic directory barrier failure") }
	t.Cleanup(func() { secretDirBarrier = dirBarrier })
	if err := VerifySecretFile(path, keyID); !errors.As(err, &sync) || sync.Stage != "dir" {
		t.Fatalf("directory barrier: %v", err)
	}
	secretDirBarrier = dirBarrier
	if err := VerifySecretFile(path, keyID); err != nil {
		t.Fatalf("barrier restored: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("verification changed the key file's bytes")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := VerifySecretFile(link, keyID); !errors.Is(err, ErrSecretFileUnsafe) {
		t.Fatalf("symlinked key: %v", err)
	}
}
