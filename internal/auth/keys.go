package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

var ErrKeyInvalid = errors.New("credential_invalid")
var ErrKeyInput = errors.New("key_input_invalid")

type Keys struct{ sessions *Sessions }

func NewKeys(db *store.DB) *Keys { return NewSetup(db, nil, "").Sessions().Keys() }
func (s *Sessions) Keys() *Keys  { return &Keys{sessions: s} }

func (k *Keys) Create(ctx context.Context, username, name, scope string) (int64, string, error) {
	_, key, err := UsernameIdentity(username)
	if err != nil || !utf8.ValidString(name) || len(name) == 0 || len(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 || (scope != "read" && scope != "write" && scope != "admin") {
		return 0, "", ErrKeyInput
	}
	var random [32]byte
	_, _ = rand.Read(random[:])
	token := "bxk_" + base64.RawURLEncoding.EncodeToString(random[:])
	v := Verifier(token)
	var id int64
	err = k.sessions.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		u, err := q.LoginUser(ctx, key)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && u.Disabled != 0) {
			return ErrKeyInput
		}
		if err != nil {
			return err
		}
		i, err := q.GetInstallation(ctx)
		if err != nil {
			return err
		}
		id, err = q.CreateAPIKey(ctx, storedb.CreateAPIKeyParams{TokenSha256: hex.EncodeToString(v[:]), UserID: u.ID, Name: name, Scope: scope, Epoch: i.RestoreEpoch, CreatedAt: k.sessions.now().UTC().Unix()})
		return err
	})
	if err != nil {
		return 0, "", err
	}
	return id, token, nil
}

func (k *Keys) Revoke(ctx context.Context, id int64) error {
	var verifier string
	err := k.sessions.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		verifier, err = storedb.New(tx).RevokeAPIKey(ctx, id)
		return err
	})
	if err == nil {
		k.sessions.forget(true, verifier)
	}
	return err
}

func (k *Keys) Authenticate(ctx context.Context, token string) (Session, error) {
	if len(token) != 47 || !strings.HasPrefix(token, "bxk_") {
		return Session{}, ErrKeyInvalid
	}
	v := Verifier(token)
	var row storedb.ReadAPIKeyRow
	err := k.sessions.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		row, err = storedb.New(tx).ReadAPIKey(ctx, hex.EncodeToString(v[:]))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrKeyInvalid
	}
	if err != nil {
		return Session{}, err
	}
	want, err := hex.DecodeString(row.TokenSha256)
	if err != nil || subtle.ConstantTimeCompare(v[:], want) != 1 || row.Disabled != 0 || row.Epoch != row.RestoreEpoch {
		k.sessions.forget(true, row.TokenSha256)
		return Session{}, ErrKeyInvalid
	}
	s := k.sessions
	s.mu.Lock()
	now := s.activityTime(row.RestoreEpoch)
	item, exists := s.keys[row.TokenSha256]
	if !exists {
		for key, pending := range s.keys {
			if now.Sub(pending.expires) >= SessionTTL {
				delete(s.keys, key)
			}
		}
		if len(s.keys) < sessionMemoryEntries {
			s.sequence++
			item = sessionExpiry{persisted: time.Unix(row.LastUsedAt, 0).UTC(), epoch: row.Epoch, generation: s.sequence, turn: s.sequence}
			exists = true
		}
	}
	if exists && !s.stopped {
		item.expires = now
		s.keys[row.TokenSha256] = item
	}
	s.mu.Unlock()
	return Session{ID: row.ID, User: User{ID: row.UserID, Username: row.Username, Role: row.Role, Revision: row.Revision, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()}, Rights: (Policy{}).Rights(row.Role, row.Scope), Credential: "key", RestoreEpoch: row.RestoreEpoch, tokenSHA256: row.TokenSha256}, nil
}
