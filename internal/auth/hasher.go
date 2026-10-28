// Package auth owns users and credentials: the argon2id password hasher and its
// global admission bound, the one-time first-admin setup and the principal a
// request carries once its credential verified. It never trusts forwarded
// headers and never returns or logs a secret.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"

	"golang.org/x/crypto/argon2"
)

// The fixed password parameters (ADR 0006 G06; architecture § Credentials).
// Verify accepts only a PHC string with exactly these: argon2.IDKey allocates
// the memory parameter it is given, so a foreign string must never reach it.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	saltBytes    = 16
	keyBytes     = 32
)

// phcPrefix is the PHC string up to the salt for the fixed parameters.
const phcPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"

// Password length bounds, in bytes.
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 1024
)

// HashSlots is how many hashes run at once in the whole process; HashWait is
// how long a hash waits for a slot before it is refused with ErrHashBusy.
const (
	HashSlots          = 2
	HashWait           = 2 * time.Second
	passwordPopulation = 32
)

var (
	// ErrHashBusy: every hash slot stayed taken for HashWait. The caller answers
	// 429 hash_busy with Retry-After.
	ErrHashBusy = errors.New("hash_busy")
	// ErrPHCInvalid: a stored password string is not argon2id with the fixed
	// parameters; it is never verified.
	ErrPHCInvalid = errors.New("password_hash_invalid")
	// ErrPasswordLength: a password outside MinPasswordBytes..MaxPasswordBytes.
	ErrPasswordLength = errors.New("password_length_invalid")
)

// Hasher hashes and verifies passwords under the process-wide admission bound.
// Create one per process and share it.
type Hasher struct {
	slots     chan struct{}
	wait      time.Duration
	waiters   chan struct{}
	passwords chan struct{}
	readTurn  chan struct{}
}

// NewHasher returns the process's hasher: HashSlots concurrent hashes, each
// waiter refused after HashWait.
func NewHasher() *Hasher {
	return &Hasher{slots: make(chan struct{}, HashSlots), wait: HashWait, waiters: make(chan struct{}, passwordPopulation), passwords: make(chan struct{}, passwordPopulation), readTurn: make(chan struct{}, 1)}
}

// acquire takes a slot, or fails with ErrHashBusy after the wait or with the
// context's error.
func (h *Hasher) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			h.release()
			return err
		}
		return nil
	default:
	}
	select {
	case h.waiters <- struct{}{}:
	default:
		return ErrHashBusy
	}
	defer func() { <-h.waiters }()
	t := time.NewTimer(h.wait)
	defer t.Stop()
	select {
	case h.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			h.release()
			return err
		}
		return nil
	case <-t.C:
		return ErrHashBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.slots }

func (h *Hasher) readPassword(ctx context.Context, db *store.DB, fn func(context.Context, *sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return passwordReadError(err)
	}
	select {
	case h.passwords <- struct{}{}:
	default:
		return ErrHashBusy
	}
	defer func() { <-h.passwords }()
	ctx, cancel := context.WithTimeout(ctx, HashWait)
	defer cancel()
	select {
	case h.readTurn <- struct{}{}:
		defer func() { <-h.readTurn }()
	case <-ctx.Done():
		return passwordReadError(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return passwordReadError(err)
	}
	var callbackErr error
	err := db.ReadPassword(ctx, func(tx *sql.Tx) error { callbackErr = fn(ctx, tx); return callbackErr })
	if callbackErr != nil && !errors.Is(callbackErr, context.Canceled) && !errors.Is(callbackErr, context.DeadlineExceeded) {
		return callbackErr
	}
	if ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err()) || errors.Is(err, sql.ErrTxDone)) {
		return passwordReadError(ctx.Err())
	}
	return passwordReadError(err)
}

func passwordReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrHashBusy
	}
	return err
}

// Hash returns the PHC string of password with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return "", ErrPasswordLength
	}
	salt := make([]byte, saltBytes)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyBytes)
	h.release()
	enc := base64.RawStdEncoding
	return phcPrefix + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
}

// Verify reports whether password matches phc. A string with any other
// algorithm, version or parameter is ErrPHCInvalid before any hashing.
func (h *Hasher) Verify(ctx context.Context, phc, password string) (bool, error) {
	salt, want, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordBytes {
		return false, nil
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyBytes)
	h.release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// parsePHC returns the salt and key of a PHC string with exactly the fixed
// parameters and lengths.
func parsePHC(phc string) (salt, key []byte, err error) {
	rest, ok := strings.CutPrefix(phc, phcPrefix)
	if !ok {
		return nil, nil, ErrPHCInvalid
	}
	s, k, ok := strings.Cut(rest, "$")
	if !ok {
		return nil, nil, ErrPHCInvalid
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err1 := enc.DecodeString(s)
	key, err2 := enc.DecodeString(k)
	if err1 != nil || err2 != nil || len(salt) != saltBytes || len(key) != keyBytes {
		return nil, nil, ErrPHCInvalid
	}
	return salt, key, nil
}
