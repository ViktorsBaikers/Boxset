package secret

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// testStore is a migrated data directory with its key store.
func testStore(t *testing.T) (*Store, *store.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, dir), db, dir
}

// record inserts one secret version and, when current, makes it the record's
// current version.
func record(t *testing.T, db *store.DB, id, version int64, purpose, keyID string, nonce, ciphertext []byte, flag bool) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		q := storedb.New(tx)
		current := int64(0)
		if flag {
			current = 1
		}
		if err := q.InsertSecretVersion(t.Context(), storedb.InsertSecretVersionParams{
			ID: id, Version: version, Purpose: purpose, KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext, IsCurrent: current, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		if flag {
			return q.SetInstallationKey(t.Context(), keyID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func current(t *testing.T, db *store.DB, id int64) storedb.GetCurrentSecretRow {
	t.Helper()
	var row storedb.GetCurrentSecretRow
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		row, err = storedb.New(tx).GetCurrentSecret(t.Context(), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func installationKey(t *testing.T, db *store.DB) (string, bool) {
	t.Helper()
	var id string
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		id, err = storedb.New(tx).GetInstallationKey(t.Context())
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, true
}

// TestSealOpenBindsRecord: a record opens only under its own key, id, version and
// purpose, so a ciphertext can never be moved to another row or use.
func TestSealOpenBindsRecord(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeySize)
	plaintext := []byte("synthetic-provider-credential")
	nonce, ciphertext, err := Seal(key, 3, 1, "indexer", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != NonceSize || len(ciphertext) != len(plaintext)+16 {
		t.Fatalf("nonce=%d ciphertext=%d", len(nonce), len(ciphertext))
	}
	if !bytes.Contains(ciphertext, nonce) {
		t.Log("nonce is carried beside the ciphertext, not inside it")
	}
	got, err := Open(key, 3, 1, "indexer", nonce, ciphertext)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("open: %q, %v", got, err)
	}
	for _, moved := range []struct {
		name    string
		id, ver int64
		purpose string
		key     []byte
	}{
		{"id", 4, 1, "indexer", key},
		{"version", 3, 2, "indexer", key},
		{"purpose", 3, 1, "download-client", key},
		{"key", 3, 1, "indexer", bytes.Repeat([]byte{8}, KeySize)},
	} {
		if _, err := Open(moved.key, moved.id, moved.ver, moved.purpose, nonce, ciphertext); !errors.Is(err, ErrOpen) {
			t.Fatalf("a record moved to another %s opened: %v", moved.name, err)
		}
	}
	if _, err := Open(key, 3, 1, "indexer", nonce[:8], ciphertext); !errors.Is(err, ErrNonceSize) {
		t.Fatalf("short nonce: %v", err)
	}
	if _, _, err := Seal(key[:16], 1, 1, "indexer", nil); !errors.Is(err, ErrKeySize) {
		t.Fatalf("short key: %v", err)
	}
}

// TestFingerprintNamesKeyFile: a key's name is the SHA-256 of its own bytes.
func TestFingerprintNamesKeyFile(t *testing.T) {
	key := bytes.Repeat([]byte{9}, KeySize)
	sum := sha256.Sum256(key)
	if got := Fingerprint(key); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %q", got)
	}
	if got := KeyPath("/config", "abc"); got != filepath.Join("/config", "keys", "abc.key") {
		t.Fatalf("key path %q", got)
	}
	key2, err := Mint()
	if err != nil || len(key2) != KeySize || bytes.Equal(key2, key) {
		t.Fatalf("mint: %d bytes, %v", len(key2), err)
	}
}

// TestStoreMintsOneKeyAndKeepsIt: an installation with no record and no key mints
// exactly one key, and the check writes nothing more once it is there.
func TestStoreMintsOneKeyAndKeepsIt(t *testing.T) {
	keys, db, _ := testStore(t)
	reason, err := keys.Check(t.Context())
	if err != nil || reason != "" {
		t.Fatalf("check: %q, %v", reason, err)
	}
	id, ok := installationKey(t, db)
	if !ok {
		t.Fatal("no installation key was named")
	}
	info, err := os.Stat(keys.KeyPath(id))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", info, err)
	}
	before, err := os.Stat(keys.KeyPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := keys.Check(t.Context()); err != nil || reason != "" {
		t.Fatalf("second check: %q, %v", reason, err)
	}
	after, err := os.Stat(keys.KeyPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a second check rewrote the key file")
	}
	if key, err := keys.ReadKey(id); err != nil || Fingerprint(key) != id {
		t.Fatalf("read key: %v, %v", err, err)
	}
}

// TestKeyImportFingerprint: only the bytes of a recorded key are published.
func TestKeyImportFingerprint(t *testing.T) {
	keys, db, dir := testStore(t)
	if _, err := keys.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, _ := installationKey(t, db)
	key, err := keys.ReadKey(id)
	if err != nil {
		t.Fatal(err)
	}
	// Move the file aside: the operator's own action, never the program's.
	if err := os.Remove(keys.KeyPath(id)); err != nil {
		t.Fatal(err)
	}
	// Bytes no record names are refused and never written.
	other := bytes.Repeat([]byte{3}, KeySize)
	if _, err := keys.Import(t.Context(), other); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("an unrecorded key was accepted: %v", err)
	}
	if _, err := os.Stat(KeyPath(dir, Fingerprint(other))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an unrecorded key was published: %v", err)
	}
	if _, err := keys.Import(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if got, err := keys.ReadKey(id); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("imported key: %v", err)
	}
}

// TestKeyImportHistorical: the key an archived version was sealed under is
// importable, and importing it reactivates nothing.
func TestKeyImportHistorical(t *testing.T) {
	keys, db, _ := testStore(t)
	old, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := keys.PublishKey(old)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := Seal(old, 1, 1, "indexer", []byte("synthetic-old"))
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, 1, 1, "indexer", oldID, nonce, ciphertext, false)
	fresh, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	freshID, err := keys.PublishKey(fresh)
	if err != nil {
		t.Fatal(err)
	}
	nonce2, ciphertext2, err := Seal(fresh, 1, 2, "indexer", []byte("synthetic-new"))
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, 1, 2, "indexer", freshID, nonce2, ciphertext2, true)
	// The archived key's file is lost; the record still names it.
	if err := os.Remove(keys.KeyPath(oldID)); err != nil {
		t.Fatal(err)
	}
	recorded, err := keys.RecordedKeyIDs(t.Context())
	if err != nil || !slices.Contains(recorded, oldID) {
		t.Fatalf("recorded keys %v: %v", recorded, err)
	}
	if _, err := keys.Import(t.Context(), old); err != nil {
		t.Fatalf("importing a historical key: %v", err)
	}
	row := current(t, db, 1)
	if row.Version != 2 || row.KeyID != freshID {
		t.Fatalf("import changed the current version: %+v", row)
	}
	if got, err := Open(fresh, row.ID, row.Version, row.Purpose, row.Nonce, row.Ciphertext); err != nil || !bytes.Equal(got, []byte("synthetic-new")) {
		t.Fatalf("current version after the import: %q, %v", got, err)
	}
}

// TestNoKeyRegeneration: a record whose key is missing is never re-encrypted
// under a new key, and no key file or record changes while it waits.
func TestNoKeyRegeneration(t *testing.T) {
	keys, db, dir := testStore(t)
	key, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	id, err := keys.PublishKey(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := Seal(key, 1, 1, "indexer", []byte("synthetic-preserved"))
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, 1, 1, "indexer", id, nonce, ciphertext, true)
	before := current(t, db, 1)
	if err := os.Remove(keys.KeyPath(id)); err != nil {
		t.Fatal(err)
	}
	reason, err := keys.Check(t.Context())
	if err != nil || reason != ReasonMissing {
		t.Fatalf("check: %q, %v", reason, err)
	}
	after := current(t, db, 1)
	if after.KeyID != before.KeyID || !bytes.Equal(after.Nonce, before.Nonce) || !bytes.Equal(after.Ciphertext, before.Ciphertext) {
		t.Fatal("a missing key rewrote the record")
	}
	if named, _ := installationKey(t, db); named != id {
		t.Fatalf("installation key moved to %q", named)
	}
	entries, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a missing key minted another file: %v", entries)
	}
	// Wrong bytes at the right name are a mismatch, and are never overwritten.
	if err := install.WriteSecretFile(keys.KeyPath(id), bytes.Repeat([]byte{1}, KeySize)); err != nil {
		t.Fatal(err)
	}
	if reason, err := keys.Check(t.Context()); err != nil || reason != ReasonMismatch {
		t.Fatalf("check over a wrong file: %q, %v", reason, err)
	}
	stored, err := os.ReadFile(keys.KeyPath(id))
	if err != nil || !bytes.Equal(stored, bytes.Repeat([]byte{1}, KeySize)) {
		t.Fatal("a mismatched key file was rewritten")
	}
}

// TestKeyImportExistingBytesSyncs: bytes already at the path are not rewritten,
// and the platform barrier still observes the file before anything uses it.
func TestKeyImportExistingBytesSyncs(t *testing.T) {
	keys, db, _ := testStore(t)
	if _, err := keys.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, _ := installationKey(t, db)
	before, err := os.Stat(keys.KeyPath(id))
	if err != nil {
		t.Fatal(err)
	}
	key := mustReadKey(t, keys, id)
	verified := 0
	keys.verify = func(path, keyID string) error {
		verified++
		return install.VerifySecretFile(path, keyID)
	}
	if _, err := keys.PublishKey(key); err != nil {
		t.Fatal(err)
	}
	if verified == 0 {
		t.Fatal("the existing bytes were accepted without the platform barrier")
	}
	after, err := os.Stat(keys.KeyPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("bytes already at the path were rewritten")
	}
}

func mustReadKey(t *testing.T, keys *Store, id string) []byte {
	t.Helper()
	key, err := keys.ReadKey(id)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestKeyDurabilityFaults: a barrier that does not complete refuses every key use
// — minted, imported, already present and reused current — and changes no record
// while it persists; a retry that completes the barrier is what lets the use
// proceed.
func TestKeyDurabilityFaults(t *testing.T) {
	keys, db, dir := testStore(t)
	if _, err := keys.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	currentID, _ := installationKey(t, db)
	currentKey := mustReadKey(t, keys, currentID)
	nonce, ciphertext, err := Seal(currentKey, 1, 1, "indexer", []byte("synthetic-kept"))
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, 1, 1, "indexer", currentID, nonce, ciphertext, true)
	keyBefore, err := os.ReadFile(keys.KeyPath(currentID))
	if err != nil {
		t.Fatal(err)
	}
	fault := func(path, _ string) error {
		return &install.SecretSyncError{Stage: "file", Err: errors.New("synthetic barrier failure")}
	}
	keys.verify = fault
	var sync *install.SecretSyncError
	cases := []struct {
		name string
		use  func() error
	}{
		{"minted", func() error { _, _, err := keys.MintKey(); return err }},
		{"imported", func() error {
			fresh, err := Mint()
			if err != nil {
				return err
			}
			_, err = keys.PublishKey(fresh)
			return err
		}},
		{"already present", func() error { _, err := keys.PublishKey(currentKey); return err }},
		{"reused current", func() error { _, err := keys.ReadKey(currentID); return err }},
	}
	for _, c := range cases {
		err := c.use()
		if !errors.As(err, &sync) || sync.Stage != "file" {
			t.Fatalf("%s key with a failing barrier: %v", c.name, err)
		}
		if named, _ := installationKey(t, db); named != currentID {
			t.Fatalf("%s key moved the installation key to %q", c.name, named)
		}
		row := currentOrMissing(t, db, 1)
		if row.Version != 1 || row.KeyID != currentID {
			t.Fatalf("%s key wrote a version: %+v", c.name, row)
		}
	}
	// A refused publication may leave an unreferenced file behind and never
	// deletes it; nothing names it, so nothing ever uses it.
	entries, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	referenced := map[string]bool{}
	if ids, err := keys.RecordedKeyIDs(t.Context()); err == nil {
		for _, id := range ids {
			referenced[id+FileSuffix] = true
		}
	}
	for _, e := range entries {
		if !referenced[e.Name()] {
			t.Logf("unreferenced key file retained: %s", e.Name())
		}
	}
	// The fault clears: the same use now completes and its barrier ran first.
	keys.verify = install.VerifySecretFile
	if _, err := keys.PublishKey(currentKey); err != nil {
		t.Fatalf("retry after the fault cleared: %v", err)
	}
	keyAfter, err := os.ReadFile(keys.KeyPath(currentID))
	if err != nil || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("the retry rewrote the current key's bytes")
	}
}

// currentOrMissing returns the current version of a record, or a zero value when
// it has none.
func currentOrMissing(t *testing.T, db *store.DB, id int64) storedb.GetCurrentSecretRow {
	t.Helper()
	var row storedb.GetCurrentSecretRow
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		row, err = storedb.New(tx).GetCurrentSecret(t.Context(), id)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return storedb.GetCurrentSecretRow{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return row
}
