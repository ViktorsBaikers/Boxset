package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// Roles a user row may hold.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

// Rights a principal may carry. A request is authorized for a route only when
// its principal holds the route's right.
const (
	RightSetupCreate = "setup:create"
	RightStatusRead  = "status:read"
	RightUsersRead   = "users:read"
	RightUsersWrite  = "users:write"
)

// MaxUsernameRunes bounds a username in Unicode characters.
const MaxUsernameRunes = 64

// ErrUsernameInvalid: a username that is empty, longer than MaxUsernameRunes,
// not valid UTF-8, or holds a control or format character.
var ErrUsernameInvalid = errors.New("username_invalid")

// User is a user row without its password hash. No value of this type ever
// carries a credential.
type User struct {
	ID        int64
	Username  string
	Role      string
	Disabled  bool
	Revision  int64
	CreatedAt time.Time
}

// Principal is the authenticated identity of one request. The setup
// credential yields a principal holding only RightSetupCreate together with
// the verifier it matched; nothing else about the token is kept.
type Principal struct {
	Rights        []string
	SetupVerifier [32]byte
}

// Has reports whether p holds right.
func (p Principal) Has(right string) bool { return slices.Contains(p.Rights, right) }

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal the guard attached to ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// UsernameIdentity preserves the NFC display spelling and derives the private
// Unicode case-folded identity without trimming or removing accents.
func UsernameIdentity(name string) (display, key string, err error) {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxUsernameRunes {
		return "", "", ErrUsernameInvalid
	}
	if strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return "", "", ErrUsernameInvalid
	}
	display = norm.NFC.String(name)
	if n := utf8.RuneCountInString(display); n < 1 || n > MaxUsernameRunes {
		return "", "", ErrUsernameInvalid
	}
	return display, norm.NFC.String(cases.Fold().String(display)), nil
}

// createUser inserts a user with an already hashed password inside tx.
func createUser(ctx context.Context, tx *sql.Tx, username, phc, role string, now time.Time) (User, error) {
	display, key, err := UsernameIdentity(username)
	if err != nil {
		return User{}, err
	}
	row, err := storedb.New(tx).CreateUser(ctx, storedb.CreateUserParams{
		Username:    display,
		UsernameKey: key,
		PasswordPhc: phc,
		Role:        role,
		CreatedAt:   now.Unix(),
	})
	if err != nil {
		return User{}, fmt.Errorf("auth: create user: %w", err)
	}
	return User{
		ID:        row.ID,
		Username:  row.Username,
		Role:      row.Role,
		Disabled:  row.Disabled != 0,
		Revision:  row.Revision,
		CreatedAt: time.Unix(row.CreatedAt, 0).UTC(),
	}, nil
}

// CreateUser is the account fixture/setup seam; no HTTP operation exposes it.
func CreateUser(ctx context.Context, db *store.DB, hasher *Hasher, username, password, role string) (User, error) {
	if _, _, err := UsernameIdentity(username); err != nil {
		return User{}, err
	}
	if role != RoleAdmin && role != RoleMember && role != RoleViewer {
		return User{}, errors.New("role_invalid")
	}
	phc, err := hasher.Hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	var user User
	err = db.Write(ctx, func(tx *sql.Tx) error {
		var e error
		user, e = createUser(ctx, tx, username, phc, role, time.Now())
		return e
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

var ErrUserNotFound = errors.New("user_not_found")
var ErrLastAdmin = errors.New("last_admin")

type RevisionConflict struct{ CurrentRevision int64 }

func (e *RevisionConflict) Error() string { return "revision_conflict" }

type UserPatch struct {
	ExpectedRevision int64
	Role             *string
	Disabled         *bool
}

// updateUser is the one target revision, last-admin and update path. recheck,
// when set, runs first inside the same writer transaction, so a change
// committed while this request waited for the writer decides it.
func updateUser(ctx context.Context, db *store.DB, id int64, patch UserPatch, recheck func(context.Context, *storedb.Queries) error) (User, error) {
	var user User
	err := db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		if recheck != nil {
			if err := recheck(ctx, q); err != nil {
				return err
			}
		}
		current, err := q.GetUser(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		if current.Revision != patch.ExpectedRevision {
			return &RevisionConflict{CurrentRevision: current.Revision}
		}
		role, disabled := current.Role, current.Disabled
		if patch.Role != nil {
			role = *patch.Role
		}
		if patch.Disabled != nil {
			disabled = 0
			if *patch.Disabled {
				disabled = 1
			}
		}
		if current.Role == RoleAdmin && current.Disabled == 0 && (role != RoleAdmin || disabled != 0) {
			n, err := q.CountActiveAdmins(ctx)
			if err != nil {
				return err
			}
			if n == 1 {
				return ErrLastAdmin
			}
		}
		row, err := q.UpdateUser(ctx, storedb.UpdateUserParams{Role: role, Disabled: disabled, ID: id, Revision: patch.ExpectedRevision})
		if err != nil {
			return err
		}
		user = User{ID: row.ID, Username: row.Username, Role: row.Role, Disabled: row.Disabled != 0, Revision: row.Revision, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// UpdateUser is the trusted setup/fixture seam: it carries no request authority
// and never runs on an HTTP request. Only the target's revision and the
// last-admin rule hold it.
func UpdateUser(ctx context.Context, db *store.DB, id int64, patch UserPatch) (User, error) {
	return updateUser(ctx, db, id, patch, nil)
}

type UserQuery struct {
	Limit                     int
	Sort, Order, Search, Role string
	AfterName                 string
	AfterCreated, AfterID     int64
}

type UserCollection struct {
	Items                                 []User
	InstallID                             string
	RestoreEpoch, SchemaVersion, Revision int64
}

// ListUsers keeps identity, collection revision and the bounded page in one snapshot.
// visit constructs its validator within that transaction; callers publish only on success.
func (s *Sessions) ListUsers(ctx context.Context, query UserQuery, visit func(UserCollection) error) error {
	return s.db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		i, err := q.GetInstallation(ctx)
		if err != nil {
			return err
		}
		revision, err := q.UsersCollectionRevision(ctx)
		if err != nil {
			return err
		}
		rows, err := q.ListUsers(ctx, storedb.ListUsersParams{
			RoleFilter: query.Role, Search: query.Search, SortBy: query.Sort, SortOrder: query.Order,
			AfterID: query.AfterID, AfterName: query.AfterName, AfterCreated: query.AfterCreated, PageLimit: int64(query.Limit + 1),
		})
		if err != nil {
			return err
		}
		result := UserCollection{Items: make([]User, 0, len(rows)), InstallID: i.InstallID, RestoreEpoch: i.RestoreEpoch, Revision: revision}
		for _, row := range rows {
			result.Items = append(result.Items, User{ID: row.ID, Username: row.Username, Role: row.Role, Disabled: row.Disabled != 0, Revision: row.Revision, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()})
		}
		return visit(result)
	})
}

// UpdateUser is the HTTP edit: actor is the authority the guard validated for
// this request, and it is re-read inside the same writer transaction before the
// target is read or changed. The recheck decides the right this route
// authorizes, RightUsersWrite; another write route supplies its own. There is no
// unauthenticated fallback.
func (s *Sessions) UpdateUser(ctx context.Context, actor Actor, id int64, patch UserPatch) (User, error) {
	now := s.now
	return updateUser(ctx, s.db, id, patch, func(ctx context.Context, q *storedb.Queries) error {
		return actor.recheck(ctx, q, RightUsersWrite, now().UTC())
	})
}
