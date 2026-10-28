// Package secret owns the installation's recoverable credentials: the key
// files kept outside the database and the AES-256-GCM records they protect.
// A key file is 32 random bytes named by their own SHA-256, so a record names
// the key that opens it and a misplaced or corrupted file can never be
// mistaken for the key it should hold. Nothing here regenerates a key over
// existing records: a record's key is either present or the record waits for
// an imported or re-entered credential.
package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

const (
	// KeySize is the length of a key file: 256 random bits.
	KeySize = 32
	// NonceSize is the AES-GCM nonce of every record.
	NonceSize = 12
	// DirName is the data-directory entry that holds the key files.
	DirName = "keys"
	// FileSuffix is the extension of every key file.
	FileSuffix = ".key"
	// ReasonMissing and ReasonMismatch are the recovering reasons of a start
	// that cannot open every key its records need.
	ReasonMissing  = "key_missing"
	ReasonMismatch = "key_mismatch"
)

var (
	// ErrKeySize: the key is not exactly KeySize bytes.
	ErrKeySize = errors.New("secret: a key is 32 bytes")
	// ErrNonceSize: the nonce is not exactly NonceSize bytes.
	ErrNonceSize = errors.New("secret: a record nonce is 12 bytes")
	// ErrOpen: the ciphertext does not authenticate under this key, record and purpose.
	ErrOpen = errors.New("secret: the record does not open under this key")
	// ErrKeyUnknown: no record and no installation key names these bytes.
	ErrKeyUnknown = errors.New("secret: key_unknown: no record names this key")
)

// Dir is the key-file directory of a data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// KeyPath is where the key whose fingerprint is keyID lives.
func KeyPath(dataDir, keyID string) string {
	return filepath.Join(Dir(dataDir), keyID+FileSuffix)
}

// Fingerprint is the key's name: the lowercase hex SHA-256 of its bytes.
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// Mint returns KeySize fresh random bytes.
func Mint() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secret: mint a key: %w", err)
	}
	return key, nil
}

// AAD is the additional data a record binds: the record id, its version and its
// purpose, so a ciphertext cannot be moved to another row, version or use.
func AAD(id, version int64, purpose string) []byte {
	aad := make([]byte, 16, 16+len(purpose))
	binary.BigEndian.PutUint64(aad[0:8], uint64(id))
	binary.BigEndian.PutUint64(aad[8:16], uint64(version))
	return append(aad, purpose...)
}

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext as record id/version under purpose and key, with a
// fresh random nonce.
func Seal(key []byte, id, version int64, purpose string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, AAD(id, version, purpose)), nil
}

// Open decrypts one record. A record moved to another id, version or purpose,
// or a key that is not the one it was sealed under, fails to authenticate.
func Open(key []byte, id, version int64, purpose string, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != NonceSize {
		return nil, ErrNonceSize
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, AAD(id, version, purpose))
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

// Store is the key material of one installation's data directory.
type Store struct {
	db      *store.DB
	dataDir string
	// verify is the owner-only, fingerprint and platform-barrier check every key
	// passes before anything references it. A proof replaces it to fail a step.
	verify func(path, keyID string) error
}

// New returns the key store of the data directory dataDir.
func New(db *store.DB, dataDir string) *Store {
	return &Store{db: db, dataDir: dataDir, verify: install.VerifySecretFile}
}

// KeyPath is where this store keeps the key whose fingerprint is keyID.
func (s *Store) KeyPath(keyID string) string { return KeyPath(s.dataDir, keyID) }

// RecordedKeyIDs lists every key a record or the installation names, current
// and archived: an import accepts exactly these.
func (s *Store) RecordedKeyIDs(ctx context.Context) (ids []string, err error) {
	err = s.db.Read(ctx, func(tx *sql.Tx) error {
		ids, err = storedb.New(tx).ListRecordedKeyIDs(ctx)
		return err
	})
	return ids, err
}

// CurrentKeyID is the key new writes use, and whether the installation names one.
func (s *Store) CurrentKeyID(ctx context.Context) (string, bool, error) {
	var id string
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = storedb.New(tx).GetInstallationKey(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// ReadKey returns the key whose fingerprint is keyID, after the owner-only and
// fingerprint checks, and never writes.
func (s *Store) ReadKey(keyID string) ([]byte, error) {
	path := s.KeyPath(keyID)
	if err := s.verify(path, keyID); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// PublishKey puts exactly these bytes at the absent path of their fingerprint
// and completes the platform barrier before it reports success. It never
// replaces a file and never rewrites bytes already there.
func (s *Store) PublishKey(key []byte) (keyID string, err error) {
	if len(key) != KeySize {
		return "", ErrKeySize
	}
	keyID = Fingerprint(key)
	path := s.KeyPath(keyID)
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		// Already published: the bytes must be these, and the barrier still
		// has to complete before anything references them.
		if err := s.verify(path, keyID); err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("secret: %s is not a regular file", path)
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secret: read %s: %w", path, err)
		}
		if !slices.Equal(current, key) {
			return "", fmt.Errorf("secret: %s holds other bytes", path)
		}
		return keyID, nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return "", fmt.Errorf("secret: %s: %w", path, statErr)
	}
	if err := os.MkdirAll(Dir(s.dataDir), 0o700); err != nil {
		return "", fmt.Errorf("secret: create %s: %w", Dir(s.dataDir), err)
	}
	if err := install.WriteSecretFile(path, key); err != nil {
		return "", err
	}
	if err := s.verify(path, keyID); err != nil {
		return "", err
	}
	return keyID, nil
}

// Import publishes a key only when its fingerprint is one a record or the
// installation already names: an unknown key is never written, because nothing
// could ever open it.
func (s *Store) Import(ctx context.Context, key []byte) (keyID string, err error) {
	if len(key) != KeySize {
		return "", ErrKeySize
	}
	keyID = Fingerprint(key)
	recorded, err := s.RecordedKeyIDs(ctx)
	if err != nil {
		return "", err
	}
	if !slices.Contains(recorded, keyID) {
		return "", ErrKeyUnknown
	}
	return s.PublishKey(key)
}

// MintKey mints and publishes a key at an absent path without referencing it.
func (s *Store) MintKey() (keyID string, key []byte, err error) {
	if key, err = Mint(); err != nil {
		return "", nil, err
	}
	if keyID, err = s.PublishKey(key); err != nil {
		return "", nil, err
	}
	return keyID, key, nil
}

// Check reports why the installation cannot open every key its records need, or
// "" when it can. With no record at all it mints and publishes one key and names
// it for new writes, which is the only key this method ever writes.
func (s *Store) Check(ctx context.Context) (reason string, err error) {
	var current string
	var hasCurrent bool
	var needed []string
	var records int
	err = s.db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		rows, err := q.ListCurrentSecretKeys(ctx)
		if err != nil {
			return err
		}
		records = len(rows)
		for _, row := range rows {
			needed = append(needed, row.KeyID)
		}
		current, err = q.GetInstallationKey(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		hasCurrent = true
		return nil
	})
	if err != nil {
		return "", err
	}
	if hasCurrent {
		needed = append(needed, current)
	}
	slices.Sort(needed)
	needed = slices.Compact(needed)
	missing, mismatch := 0, 0
	for _, id := range needed {
		switch err := s.verify(s.KeyPath(id), id); {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			missing++
		default:
			mismatch++
		}
	}
	switch {
	case mismatch > 0:
		reason = ReasonMismatch
	case missing > 0:
		reason = ReasonMissing
	}
	// A record is never re-encrypted over a key that is not there, and a file that
	// holds other bytes is never overwritten: both stay refused until an operator
	// imports or re-enters.
	if records > 0 {
		return reason, nil
	}
	if hasCurrent {
		switch reason {
		case "":
			return "", nil
		case ReasonMismatch:
			return reason, nil
		}
	}
	// Nothing is encrypted yet: publish the one key new writes will use.
	keyID, _, err := s.MintKey()
	if err != nil {
		return "", err
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		return storedb.New(tx).SetInstallationKey(ctx, keyID)
	})
	if err != nil {
		return "", err
	}
	return "", nil
}
