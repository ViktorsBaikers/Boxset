package auth

// Current-authority revalidation. One read-only operation re-runs the guard's
// own decision for the provenance it accepted, so a long-lived consumer (the
// event stream) decides every frame from the stored rows instead of a grant it
// remembered. A ref carries only what the decision needs — the matched row's
// stored SHA-256 index, the user, the accepted rights, the restore epoch and the
// effective session expiry — plus the direct peer and configuration for the
// direct-peer bypass, which holds no row at all. Nothing here writes: no sliding
// expiry, no last-used stamp, no cache.

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// CredentialRef is the provenance the route guard authenticated one request
// with. Its fields are unexported and comparable, it is minted only by the
// guard's own authentication step, and it is never logged, encoded or framed.
type CredentialRef struct {
	credential string
	// lookup is the stored SHA-256 index of the matched session or key row, so a
	// reused row id can never alias another credential.
	lookup   string
	user     int64
	rights   string // the accepted right set, sorted and space-joined
	epoch    int64
	expires  time.Time
	peer     string // bypass: the direct TCP peer the decision matched
	allowed  string // bypass: the local-bypass prefixes in use, canonical and comma-joined
	trusted  string // bypass: the trusted-proxy prefixes in use, canonical and comma-joined
	bypasses bool
}

// SessionRef is the ref of a validated session cookie.
func SessionRef(s Session) CredentialRef {
	return CredentialRef{credential: s.Credential, lookup: s.tokenSHA256, user: s.User.ID, rights: rightsKey(s.Rights), epoch: s.RestoreEpoch, expires: s.ExpiresAt}
}

// KeyRef is the ref of a validated API key.
func KeyRef(s Session) CredentialRef {
	return CredentialRef{credential: s.Credential, lookup: s.tokenSHA256, user: s.User.ID, rights: rightsKey(s.Rights), epoch: s.RestoreEpoch}
}

// BypassRef is the ref of the direct-peer grant: the peer the decision matched
// and the configuration it read. It reports false for a peer the grant does not
// cover, so the guard mints no ref from a provenance it would not serve.
func BypassRef(remote string, local, trusted []netip.Prefix) (CredentialRef, bool) {
	if !peerMatches(remote, local) || peerMatches(remote, trusted) {
		return CredentialRef{}, false
	}
	return CredentialRef{
		credential: CredentialLocalBypass,
		rights:     rightsKey([]string{RightStatusRead, RightUsersRead, RightUsersWrite}),
		peer:       remote,
		allowed:    prefixKey(local),
		trusted:    prefixKey(trusted),
		bypasses:   true,
	}, true
}

// WithEpoch binds the ref to the restore epoch the guard accepted with it. The
// direct-peer grant holds no row, so the epoch is read beside the decision.
func (r CredentialRef) WithEpoch(epoch int64) CredentialRef {
	r.epoch = epoch
	return r
}

// Credential is the ref's credential kind: "cookie", "key" or "local_bypass".
func (r CredentialRef) Credential() string { return r.credential }

// User is the ref's user id; zero for the direct-peer grant, which has none.
func (r CredentialRef) User() int64 { return r.user }

// Identity names the provenance for counting, never with a raw credential: a
// row's stored digest, or the bypass peer's address. The port is left out, so the
// same direct peer on a new connection is one identity and a per-identity cap
// cannot be rotated by reconnecting.
func (r CredentialRef) Identity() string {
	if r.credential == CredentialLocalBypass {
		return r.credential + ":" + peerAddr(r.peer)
	}
	return r.credential + ":" + r.lookup
}

// peerAddr is one socket peer's address without its ephemeral port.
func peerAddr(remote string) string {
	if addr, err := netip.ParseAddrPort(remote); err == nil {
		return addr.Addr().Unmap().String()
	}
	return remote
}

// Valid reports whether r names a provenance Revalidate can decide.
func (r CredentialRef) Valid() bool {
	switch r.credential {
	case "cookie", "key":
		return r.lookup != ""
	case CredentialLocalBypass:
		return r.bypasses && r.peer != ""
	}
	return false
}

// Revalidate is the one current-authority decision: it re-runs the guard's own
// post-lookup decision for the provenance the ref kept, inside a read, and
// refuses a credential that is no longer current. That decision is Actor.recheck,
// the same function the guard applies inside its writer transaction, so the rule
// has one implementation. Every right the guard accepted must still be granted:
// a downgrade or a narrower scope ends the grant, and a right this ref never held
// is never added. The direct-peer grant holds no row, so its peer and
// configuration are re-decided from what the ref kept before that call.
func Revalidate(ctx context.Context, db *store.DB, ref CredentialRef, now time.Time) (Principal, time.Time, error) {
	if db == nil || !ref.Valid() {
		return Principal{}, time.Time{}, ErrSessionInvalid
	}
	accepted := strings.Fields(ref.rights)
	if len(accepted) == 0 {
		return Principal{}, time.Time{}, ErrSessionInvalid
	}
	if ref.credential == CredentialLocalBypass &&
		(!peerMatches(ref.peer, parsePrefixes(ref.allowed)) || peerMatches(ref.peer, parsePrefixes(ref.trusted))) {
		return Principal{}, time.Time{}, ErrSessionInvalid
	}
	err := db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		actor := ref.actor()
		for _, right := range accepted {
			if err := actor.recheck(ctx, q, right, now); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrForbidden) {
		return Principal{}, time.Time{}, ErrSessionInvalid
	}
	if err != nil {
		return Principal{}, time.Time{}, err
	}
	return Principal{Rights: accepted}, ref.expires, nil
}

// actor is the ref in the guard's own provenance form, so the one decision can be
// re-run for it.
func (r CredentialRef) actor() Actor {
	return Actor{verifier: r.lookup, userID: r.user, credential: r.credential, epoch: r.epoch, expires: r.expires}
}

// rightsKey is the comparable form of one right set.
func rightsKey(rights []string) string {
	sorted := slices.Clone(rights)
	slices.Sort(sorted)
	return strings.Join(slices.Compact(sorted), " ")
}

// prefixKey is the comparable form of one prefix list, in the order it is used.
func prefixKey(prefixes []netip.Prefix) string {
	parts := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		parts = append(parts, p.Masked().String())
	}
	return strings.Join(parts, ",")
}

func parsePrefixes(key string) []netip.Prefix {
	var prefixes []netip.Prefix
	for _, part := range strings.Split(key, ",") {
		if p, err := netip.ParsePrefix(part); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// peerMatches reports whether the socket peer is covered by one of prefixes,
// including an IPv4-mapped IPv6 peer and a 4-in-6 prefix. It is the rule the
// direct-peer grant is decided by, so the guard and Revalidate share it.
func peerMatches(remote string, prefixes []netip.Prefix) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	return addrMatches(peer.Addr(), prefixes)
}

func addrMatches(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

type credentialRefKey struct{}

// WithCredentialRef returns ctx carrying the ref the guard minted for it.
func WithCredentialRef(ctx context.Context, ref CredentialRef) context.Context {
	return context.WithValue(ctx, credentialRefKey{}, ref)
}

// CredentialRefFrom returns the ref the guard attached to ctx.
func CredentialRefFrom(ctx context.Context) (CredentialRef, bool) {
	ref, ok := ctx.Value(credentialRefKey{}).(CredentialRef)
	return ref, ok
}
