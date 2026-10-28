package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

const SessionTTL = 30 * 24 * time.Hour
const sessionMemoryEntries = 4096 // fixed-size expiry metadata, bounded independently of stored sessions

var ErrCredentialsInvalid = errors.New("credentials_invalid")
var ErrSessionInvalid = errors.New("credential_invalid")

// Session contains current identity and policy, never the session credential.
type Session struct {
	ID           int64
	User         User
	Rights       []string
	Credential   string
	RestoreEpoch int64
	ExpiresAt    time.Time
	tokenSHA256  string
	// LocalBypassEnabled reports that the installation configures the direct-peer
	// grant; it is a fact for the warning, never a grant by itself.
	LocalBypassEnabled bool
	// CanLogout reports a cookie validated behind this request. Only a cookie can
	// be revoked, and this flag conveys no identity or grant.
	CanLogout bool
}

type sessionExpiry struct {
	expires, persisted time.Time
	epoch              int64
	generation, turn   uint64
}

type Sessions struct {
	db          *store.DB
	hasher      *Hasher
	now         func() time.Time
	mu          sync.Mutex
	expiry      map[string]sessionExpiry
	lastFlush   time.Time
	started     time.Time
	keys        map[string]sessionExpiry
	flush       chan struct{}
	lastAttempt time.Time
	clock       time.Time
	cadence     time.Time
	epoch       int64
	sequence    uint64
	stopped     bool
	// Fault barriers are nil outside deterministic metadata tests.
	batchFault func(string) error
}

// Sessions shares setup's process-wide hasher and transaction boundary.
func (s *Setup) Sessions() *Sessions {
	return s.SessionsWithClock(time.Now)
}

func (s *Setup) SessionsWithClock(now func() time.Time) *Sessions {
	return &Sessions{db: s.db, hasher: s.hasher, now: now, expiry: make(map[string]sessionExpiry, sessionMemoryEntries), keys: make(map[string]sessionExpiry, sessionMemoryEntries), flush: make(chan struct{}, 1)}
}

func roleRights(role string) []string {
	if role == RoleAdmin {
		return []string{RightStatusRead, RightUsersRead, RightUsersWrite}
	}
	return []string{RightStatusRead}
}

// Login returns a credential only to the cookie writer, after the insert commits.
func (s *Sessions) Login(ctx context.Context, username, password string) (Session, string, string, error) {
	_, key, err := UsernameIdentity(username)
	if err != nil {
		return Session{}, "", "", ErrCredentialsInvalid
	}
	var user storedb.User
	err = s.hasher.readPassword(ctx, s.db, func(ctx context.Context, tx *sql.Tx) error {
		var e error
		user, e = storedb.New(tx).LoginUser(ctx, key)
		return e
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Session{}, "", "", err
	}
	found := err == nil
	phc := user.PasswordPhc
	if !found {
		// Fixed valid parameters ensure unknown accounts use the same hash admission.
		phc = phcPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	valid, err := s.hasher.Verify(ctx, phc, password)
	if err != nil {
		return Session{}, "", "", err
	}
	if !found || !valid || user.Disabled != 0 {
		return Session{}, "", "", ErrCredentialsInvalid
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	v := Verifier(token)
	now := s.now().UTC().Truncate(time.Second)
	result := Session{User: User{ID: user.ID, Username: user.Username, Role: user.Role, Revision: user.Revision, CreatedAt: time.Unix(user.CreatedAt, 0).UTC()}, Rights: roleRights(user.Role), Credential: "cookie", ExpiresAt: now.Add(SessionTTL)}
	var installation string
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		current, e := q.LoginUser(ctx, key)
		if e != nil {
			return e
		}
		if current.Disabled != 0 || current.PasswordPhc != user.PasswordPhc {
			return ErrCredentialsInvalid
		}
		i, e := q.GetInstallation(ctx)
		if e != nil {
			return e
		}
		result.User.Role = current.Role
		result.User.Revision = current.Revision
		result.Rights = roleRights(current.Role)
		result.RestoreEpoch = i.RestoreEpoch
		installation = i.InstallID
		result.ID, e = q.CreateSession(ctx, storedb.CreateSessionParams{TokenSha256: hex.EncodeToString(v[:]), UserID: user.ID, Epoch: i.RestoreEpoch, CreatedAt: now.Unix(), ExpiresAt: result.ExpiresAt.Unix()})
		return e
	})
	if err != nil {
		return Session{}, "", "", err
	}
	s.mu.Lock()
	s.activityTime(result.RestoreEpoch)
	s.mu.Unlock()
	result.tokenSHA256 = hex.EncodeToString(v[:])
	return result, token, installation, nil
}

func (s *Sessions) Installation(ctx context.Context) (string, error) {
	var id string
	err := s.db.Read(ctx, func(tx *sql.Tx) error { i, e := storedb.New(tx).GetInstallation(ctx); id = i.InstallID; return e })
	return id, err
}

// Epoch is the installation's current restore epoch, the value every credential
// is bound to.
func (s *Sessions) Epoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := s.db.Read(ctx, func(tx *sql.Tx) error { i, e := storedb.New(tx).GetInstallation(ctx); epoch = i.RestoreEpoch; return e })
	return epoch, err
}

func (s *Sessions) Authenticate(ctx context.Context, token string) (Session, error) {
	identity, err := s.AuthenticateReadOnly(ctx, token)
	if err != nil {
		return Session{}, err
	}
	if err := s.FlushMetadata(ctx); err != nil {
		return Session{}, err
	}
	return identity, nil
}

func (s *Sessions) AuthenticateReadOnly(ctx context.Context, token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrSessionInvalid
	}
	v := Verifier(token)
	var row storedb.ReadSessionRow
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var e error
		row, e = storedb.New(tx).ReadSession(ctx, hex.EncodeToString(v[:]))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, err
	}
	want, e := hex.DecodeString(row.TokenSha256)
	if e != nil || subtle.ConstantTimeCompare(v[:], want) != 1 || row.Epoch != row.RestoreEpoch {
		s.forget(false, row.TokenSha256)
		return Session{}, ErrSessionInvalid
	}
	if row.Disabled != 0 {
		return Session{}, ErrSessionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.activityTime(row.RestoreEpoch)
	expires := time.Unix(row.ExpiresAt, 0).UTC()
	key := row.TokenSha256
	pending, exists := s.expiry[key]
	if exists && pending.expires.After(expires) {
		expires = pending.expires
	}
	if !now.Before(expires) {
		delete(s.expiry, key)
		return Session{}, ErrSessionInvalid
	}
	if !exists {
		for id, item := range s.expiry {
			if !now.Before(item.expires) {
				delete(s.expiry, id)
			}
		}
		if len(s.expiry) >= sessionMemoryEntries {
			return Session{}, errors.New("session_capacity")
		}
		s.sequence++
		pending = sessionExpiry{persisted: expires.Add(-SessionTTL), epoch: row.Epoch, generation: s.sequence, turn: s.sequence}
		// Stored expiry records activity, not when another session flushed it. A
		// reconstructed service waits a full hour before its next expiry write.
		if pending.persisted.Before(s.started) {
			pending.persisted = s.started
		}
		exists = true
	}
	if exists {
		pending.expires = now.Truncate(time.Second).Add(SessionTTL)
		s.expiry[key] = pending
		expires = pending.expires
	}
	return Session{ID: row.ID, User: User{ID: row.UserID, Username: row.Username, Role: row.Role, Revision: row.Revision, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()}, Rights: roleRights(row.Role), Credential: "cookie", RestoreEpoch: row.RestoreEpoch, ExpiresAt: expires, tokenSHA256: key}, nil
}

func (s *Sessions) Logout(ctx context.Context, session Session) error {
	if err := s.db.Write(ctx, func(tx *sql.Tx) error { return storedb.New(tx).DeleteSession(ctx, session.tokenSHA256) }); err != nil {
		return err
	}
	s.forget(false, session.tokenSHA256)
	return nil
}

type sessionKey struct{}

func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

type cookieSessionKey struct{}

// WithCookieSession keeps the cookie validated behind this request apart from
// the response Session, so a bypass grant can log out that exact verifier
// without claiming the bypass is the cookie's user.
func WithCookieSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, cookieSessionKey{}, s)
}

// CookieSessionFrom returns the validated cookie behind the request, if any.
func CookieSessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(cookieSessionKey{}).(Session)
	return s, ok
}
