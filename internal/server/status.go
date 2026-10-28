package server

import (
	"errors"
	"net/http"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// ReasonRestoreInProgress is the recovering reason a start reports while a
// restore journal is still pending. The installation is not served to any
// credential then: the restore is about to raise the epoch above every one of
// them.
const ReasonRestoreInProgress = "restore_in_progress"

// recoveringStatus answers the one request a recovering installation serves: an
// authorized status read, which names the reason nothing else works. Every other
// route stays refused before any credential is read, and a request without a
// current credential is refused here too. The reason is a fixed word, never a
// path, a key or an error string.
func (s *Server) recoveringStatus(w http.ResponseWriter, r *http.Request, b *Backend, mux *http.ServeMux) {
	// A pending restore has not replaced the database yet, so every credential in
	// it was issued at or below the epoch the restore is about to pass: none of
	// them is served, and the reason is not confirmed to one.
	if b.Recovering == ReasonRestoreInProgress {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "unauthenticated"), nil)
		return
	}
	p, identity, ok := s.confinedCredential(w, r, b)
	if !ok {
		return
	}
	if !p.Has(auth.RightStatusRead) {
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden, "forbidden"), nil)
		return
	}
	ctx := auth.WithPrincipal(auth.WithSession(r.Context(), identity), p)
	// The one request a confined installation serves is charged to the credential
	// that just resolved, so it shares the route deadline, the per-credential cap
	// and the global cap with every other credential read; a refusal here is the
	// recorded 429 or 503 and serves nothing.
	release, admitted := s.admission.admit(w, admittedKey(identity))
	if !admitted {
		return
	}
	defer release()
	mux.ServeHTTP(w, r.WithContext(ctx))
}

// confinedCredential reads the credential the confined status route serves, inside
// the same lookup gate as the ready path: the key, the installation row behind the
// cookie and the session are this request's lookup, so they are counted against the
// direct peer and run under the recorded wait. A request that presents nothing reads
// the installation row too, so it is bounded here as well. A read the bound ended is
// a credential that could not be validated, so it answers the recorded 401
// unauthenticated, never the 500 a failed store read is mapped to. The count is
// returned when the read is over, before the route takes its own reservation.
func (s *Server) confinedCredential(w http.ResponseWriter, r *http.Request, b *Backend) (auth.Principal, auth.Session, bool) {
	var p auth.Principal
	var identity auth.Session
	lookup, release, ok := s.lookup(w, r)
	if !ok {
		return p, identity, false
	}
	defer release()
	if values, explicitKey := lookup.Header["X-Api-Key"]; explicitKey {
		if len(values) != 1 || b.Keys == nil {
			api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "credential_invalid"), nil)
			return p, identity, false
		}
		got, err := b.Keys.Authenticate(lookup.Context(), values[0])
		if err != nil {
			if lookupExpired(w, lookup) {
				return p, identity, false
			}
			// A key that is not valid is the caller's problem, anything else is this
			// installation's: the same mapping every other protected route uses.
			status, reason := http.StatusInternalServerError, "internal_error"
			if errors.Is(err, auth.ErrKeyInvalid) {
				status, reason = http.StatusUnauthorized, "credential_invalid"
			}
			api.WriteProblem(w, api.NewProblem(status, reason), nil)
			return p, identity, false
		}
		return auth.Principal{Rights: got.Rights}, got, true
	}
	token, ok, err := s.presentedCookie(lookup, b)
	if err != nil {
		if lookupExpired(w, lookup) {
			return p, identity, false
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, "internal_error"), nil)
		return p, identity, false
	}
	if !ok {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "unauthenticated"), nil)
		return p, identity, false
	}
	got, err := b.Sessions.AuthenticateReadOnly(lookup.Context(), token)
	if err != nil {
		if lookupExpired(w, lookup) {
			return p, identity, false
		}
		status, reason := http.StatusInternalServerError, "internal_error"
		if err == auth.ErrSessionInvalid {
			status, reason = http.StatusUnauthorized, "credential_invalid"
		}
		api.WriteProblem(w, api.NewProblem(status, reason), nil)
		return p, identity, false
	}
	return auth.Principal{Rights: got.Rights}, got, true
}
