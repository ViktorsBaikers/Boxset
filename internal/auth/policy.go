package auth

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// ErrForbidden: the credential is current but its role intersect its scope no
// longer holds the right the request needs.
var ErrForbidden = errors.New("forbidden")

// Policy intersects the current user's rights with the key's scope.
type Policy struct{}

func (Policy) Rights(role, scope string) []string {
	var allowed []string
	switch scope {
	case "read":
		allowed = []string{RightStatusRead, RightUsersRead}
	case "write", "admin":
		allowed = []string{RightStatusRead, RightUsersRead, RightUsersWrite}
	default:
		return nil
	}
	var rights []string
	for _, right := range roleRights(role) {
		if slices.Contains(allowed, right) {
			rights = append(rights, right)
		}
	}
	return rights
}

func (s Session) Has(right string) bool { return slices.Contains(s.Rights, right) }

// CredentialLocalBypass is the credential kind of the direct-peer grant.
const CredentialLocalBypass = "local_bypass"

// Actor is the authority of one request, made by the server apart from the
// response Session. It carries only what a use-time recheck needs: the stored
// verifier and immutable user id of the credential, the kind of credential and
// the restore epoch the guard accepted. A bypass actor carries no identity at
// all. No value of this type carries a raw token.
type Actor struct {
	verifier   string
	userID     int64
	credential string
	epoch      int64
	expires    time.Time
}

// ActorOf is the actor of a validated credential: the session or key that was
// authenticated on this request.
func ActorOf(s Session) Actor {
	return Actor{verifier: s.tokenSHA256, userID: s.User.ID, credential: s.Credential, epoch: s.RestoreEpoch, expires: s.ExpiresAt}
}

// BypassActor is the actor of the direct-peer grant, which holds no credential.
func BypassActor(epoch int64) Actor {
	return Actor{credential: CredentialLocalBypass, epoch: epoch}
}

// Credential is the actor's credential kind: "cookie", "key" or "local_bypass".
func (a Actor) Credential() string { return a.credential }

type actorKey struct{}

// WithActor returns ctx carrying a.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor the guard attached to ctx.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// recheck re-reads the actor's authority inside the writer transaction, after
// any competing writer committed: the installation epoch, the credential row by
// verifier, its matching user and that user's current row. A grant that was
// withdrawn while the request waited at BEGIN never survives it. right is the
// one its calling route authorizes: when the current role intersect the stored
// scope no longer holds that right, the request is ErrForbidden. Deriving it
// here instead would decide another route's requests by this route's right.
func (a Actor) recheck(ctx context.Context, q *storedb.Queries, right string, now time.Time) error {
	i, err := q.GetInstallation(ctx)
	if err != nil {
		return err
	}
	if i.RestoreEpoch != a.epoch {
		return ErrSessionInvalid
	}
	var rights []string
	switch a.credential {
	case "key":
		row, err := q.ReadAPIKey(ctx, a.verifier)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionInvalid
		}
		if err != nil {
			return err
		}
		if row.UserID != a.userID || row.Disabled != 0 || row.Epoch != row.RestoreEpoch {
			return ErrSessionInvalid
		}
		rights = Policy{}.Rights(row.Role, row.Scope)
	case "cookie":
		row, err := q.ReadSession(ctx, a.verifier)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionInvalid
		}
		if err != nil {
			return err
		}
		// The guard's effective expiry already merges the pending sliding
		// extension, so the stored row's older value is not the bound.
		if row.UserID != a.userID || row.Disabled != 0 || row.Epoch != row.RestoreEpoch || !now.Before(a.expires) {
			return ErrSessionInvalid
		}
		rights = roleRights(row.Role)
	case CredentialLocalBypass:
		st, err := readSetup(ctx, q)
		if err != nil {
			return err
		}
		if st.Open {
			return ErrSessionInvalid
		}
		rights = []string{RightStatusRead, RightUsersRead, RightUsersWrite}
	default:
		return ErrSessionInvalid
	}
	if !slices.Contains(rights, right) {
		return ErrForbidden
	}
	return nil
}
