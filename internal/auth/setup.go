package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// TokenFileName is the owner-only file below the data directory that holds the
// setup token while setup is open. Only `boxset setup-token` reads it.
const TokenFileName = "setup-token"

// Bounds of a user-chosen BOXSET_SETUP_TOKEN (ADR 0008 I05, ADR 0009 J04).
const (
	MinEnvTokenRunes = 16
	MaxEnvTokenBytes = 1024
)

var (
	// ErrSetupCredential: the bearer is not the setup credential (401
	// setup_credential_invalid).
	ErrSetupCredential = errors.New("setup_credential_invalid")
	// ErrSetupCompleted: the bearer is the consumed setup credential; the first
	// admin exists and setup never reopens (409 setup_completed).
	ErrSetupCompleted = errors.New("setup_completed")
	// ErrEnvTokenInvalid: BOXSET_SETUP_TOKEN is set while setup is open but is
	// shorter than MinEnvTokenRunes characters or longer than MaxEnvTokenBytes.
	ErrEnvTokenInvalid = errors.New("setup_token_invalid: BOXSET_SETUP_TOKEN must be at least 16 characters")
)

// SetupState is what the setup credential is checked against.
type SetupState struct {
	// Open: no user row exists and the setup row is not consumed.
	Open bool
	// Armed: a verifier is stored (open, or consumed and kept for replays).
	Armed    bool
	verifier [32]byte
}

// Matches reports, in constant time, whether v is the stored verifier.
func (s SetupState) Matches(v [32]byte) bool {
	return s.Armed && subtle.ConstantTimeCompare(v[:], s.verifier[:]) == 1
}

// Verifier is the stored form of a setup token: its SHA-256.
func Verifier(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// Setup is the one-time first-admin flow of one installation.
type Setup struct {
	db        *store.DB
	hasher    *Hasher
	tokenPath string
	turn      chan struct{} // one creation at a time: the losers of a race are told 409 without hashing
	now       func() time.Time
}

// NewSetup returns the setup flow for the store of the data directory dir.
func NewSetup(db *store.DB, hasher *Hasher, dir string) *Setup {
	return &Setup{
		db:        db,
		hasher:    hasher,
		tokenPath: filepath.Join(dir, TokenFileName),
		turn:      make(chan struct{}, 1),
		now:       time.Now,
	}
}

// State reads whether setup is open and the stored verifier.
func (s *Setup) State(ctx context.Context) (SetupState, error) {
	var st SetupState
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		st, err = readSetup(ctx, storedb.New(tx))
		return err
	})
	return st, err
}

// StateForPassword spends the shared password-read deadline and population.
func (s *Setup) StateForPassword(ctx context.Context) (SetupState, error) {
	var st SetupState
	err := s.hasher.readPassword(ctx, s.db, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		st, err = readSetup(ctx, storedb.New(tx))
		return err
	})
	return st, err
}

func readSetup(ctx context.Context, q *storedb.Queries) (SetupState, error) {
	anyUser, err := q.AnyUser(ctx)
	if err != nil {
		return SetupState{}, fmt.Errorf("auth: read users: %w", err)
	}
	row, err := q.GetSetup(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SetupState{Open: !anyUser}, nil
	}
	if err != nil {
		return SetupState{}, fmt.Errorf("auth: read setup: %w", err)
	}
	st := SetupState{Open: !anyUser && !row.ConsumedAt.Valid, Armed: true}
	b, err := hex.DecodeString(row.TokenSha256)
	if err != nil || len(b) != len(st.verifier) {
		return SetupState{}, errors.New("auth: the stored setup verifier is not a SHA-256")
	}
	copy(st.verifier[:], b)
	return st, nil
}

// Arm prepares the setup credential at startup, only while setup is open: the
// user-chosen envToken when set, else 32 fresh random bytes, is written to
// the owner-only token file and its verifier stored. A closed installation
// never writes, rearms or re-hashes a token; envToken is then ignored. It
// reports whether setup is open.
func (s *Setup) Arm(ctx context.Context, envToken string) (bool, error) {
	st, err := s.State(ctx)
	if err != nil || !st.Open {
		return false, err
	}
	token := envToken
	if token != "" {
		if utf8.RuneCountInString(token) < MinEnvTokenRunes || len(token) > MaxEnvTokenBytes {
			return true, ErrEnvTokenInvalid
		}
	} else {
		b := make([]byte, 32)
		_, _ = rand.Read(b) // crypto/rand.Read never fails
		token = base64.RawURLEncoding.EncodeToString(b)
	}
	if err := install.WriteSecretFile(s.tokenPath, []byte(token)); err != nil {
		return true, fmt.Errorf("auth: write the setup token file: %w", err)
	}
	v := Verifier(token)
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		now, err := readSetup(ctx, q)
		if err != nil || !now.Open {
			return err
		}
		return q.ArmSetup(ctx, hex.EncodeToString(v[:]))
	})
	return true, err
}

// Create makes the first admin for the holder of the open setup credential
// whose verifier is v. The password is hashed before the one Write that
// consumes setup and inserts the admin; that Write commits only while no user
// exists and setup is unconsumed, so concurrent valid submissions create
// exactly one admin and every other is told ErrSetupCompleted.
func (s *Setup) Create(ctx context.Context, v [32]byte, username, password string) (User, error) {
	if _, _, err := UsernameIdentity(username); err != nil {
		return User{}, err
	}
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return User{}, ErrPasswordLength
	}
	select {
	case s.turn <- struct{}{}:
		defer func() { <-s.turn }()
	case <-ctx.Done():
		return User{}, ctx.Err()
	}
	st, err := s.StateForPassword(ctx)
	if err != nil {
		return User{}, err
	}
	if err := st.check(v); err != nil {
		return User{}, err
	}
	phc, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		now := s.now()
		n, err := q.ConsumeSetup(ctx, storedb.ConsumeSetupParams{
			ConsumedAt:  sql.NullInt64{Int64: now.Unix(), Valid: true},
			TokenSha256: hex.EncodeToString(v[:]),
		})
		if err != nil {
			return fmt.Errorf("auth: consume setup: %w", err)
		}
		if n == 0 {
			st, err := readSetup(ctx, q)
			if err != nil {
				return err
			}
			if err := st.check(v); err != nil {
				return err
			}
			return ErrSetupCredential
		}
		u, err = createUser(ctx, tx, username, phc, RoleAdmin, now)
		return err
	})
	return u, err
}

// check classifies verifier v against st: nil only for the open credential.
func (st SetupState) check(v [32]byte) error {
	switch {
	case !st.Matches(v):
		return ErrSetupCredential
	case !st.Open:
		return ErrSetupCompleted
	}
	return nil
}
