package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
)

// headStream answers a HEAD of a stream route: ServeMux resolves HEAD to the GET
// pattern, so the stream handler would otherwise be written frames net/http
// discards. It advertises the answer the GET would give — the stream's own
// headers when the hub would admit one, the route's own refusal (503 draining,
// 429 stream_cap) when it would not — and holds no stream slot.
func (s *Server) headStream(w http.ResponseWriter, r *http.Request) {
	ref, ok := auth.CredentialRefFrom(r.Context())
	if !ok || !ref.Valid() {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "credential_invalid"), nil)
		return
	}
	hub := s.Events()
	if hub == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, "internal_error"), nil)
		return
	}
	if refusal := hub.Refusal(ref); refusal != nil {
		problem, retry := api.StreamRefusal(refusal)
		var header http.Header
		if retry > 0 {
			header = retryAfter(retry)
		}
		api.WriteProblem(w, problem, header)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// guard is the route guard of architecture § Route guard order, after the
// server limits and the base strip: a secret in the query is refused, the mux
// resolves the pattern and the route-class table classifies it. public-doc rows
// serve in every state; an unlisted non-API path gets the mux's own 404 or 405;
// everything else needs current authority, then its class authenticator, the
// cross-origin check, the lifecycle gate and the route's right. Missing or
// invalid credentials get 401 in every state, never 503 and never the shell.
func (s *Server) guard(mux *http.ServeMux, routes map[string]api.Route) http.Handler {
	// serve is the authorized dispatch: it arms the route deadline and charges the
	// resolved credential's read budget, so it is entered only after the gate has
	// authorized the route. A stream route is dispatched straight to the mux
	// instead: it holds its credential's stream for the life of the connection,
	// which the hub's own per-credential and global stream caps bound, so it never
	// spends a short-lived read reservation on every other request of that
	// credential. A HEAD of it is one request and no stream, so it pays that
	// reservation and is answered where the hub's refusal can precede the head.
	// The gate arms the route deadline on it either way, with the read side left
	// off a stream: its frames and heartbeats keep refreshing the write side.
	serve := s.admit(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redact.QueryHasSecret(r.URL.RawQuery) {
			// The refusal carries the route deadline like every other guarded path, so it
			// is bounded once the route is classified instead of answering outside the
			// deadline the rest of the request would have carried.
			s.routeBound(w)
			api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, "secret_in_url"), nil)
			return
		}
		h, pattern := mux.Handler(r)
		route, listed := routes[pattern]
		isAPI := r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
		switch {
		case listed && route.Class == api.ClassPublicDoc:
			s.routeBound(w)
			h.ServeHTTP(w, r)
			return
		case !listed && !isAPI:
			if pattern != "" { // registered but unclassified: never served
				s.routeBound(w)
				http.NotFound(w, r)
				return
			}
			s.routeBound(w)
			h.ServeHTTP(w, r)
			return
		case !listed:
			route = api.Route{Class: api.ClassPrincipal}
		}
		// Every remaining route is the post-public-doc gate. It answers inside the
		// route deadline but outside the read budget: the budget is charged once the
		// gate's own authenticator has resolved the credential, so a request that
		// presents nothing or presents a dead credential still gets its recorded 401.
		dispatch := serve
		if route.Stream {
			dispatch = mux
			if r.Method == http.MethodHead {
				dispatch = s.admit(http.HandlerFunc(s.headStream))
			}
		}
		s.gate(w, r, mux, listed, route, dispatch)
	})
}

// gate answers a route that is not a public-doc row, in the recorded order. Every
// answer it gives is inside the route deadline. The credential lookup it performs
// runs inside the lookup gate — the same accounting as a read, keyed by the direct
// peer, and 401 once its wait is out — and the read reservation for what the lookup
// resolved is taken by the authorized dispatch, and by the status read a confined
// installation serves, once a credential is resolved.
func (s *Server) gate(w http.ResponseWriter, r *http.Request, mux *http.ServeMux, listed bool, route api.Route, serve http.Handler) {
	if route.Stream {
		// A stream outlives the route deadline: its frames and heartbeats refresh
		// the write side, so it carries no read deadline.
		s.streamBound(w)
	} else {
		s.routeBound(w)
	}
	state := s.currentState()
	b := s.backend.Load()
	// A pending restore refuses in every state, draining included: it has not
	// confirmed the epoch it is about to pass, so every credential in the
	// database may have been issued at or below it and none of them is served.
	pendingRestore := b != nil && b.Recovering == ReasonRestoreInProgress
	// A key-broken installation confines every credential to the one status
	// read in every state, draining included: a stop is not a repair, so a
	// drain must not reopen the read routes a lost key closed.
	keyLoss := b != nil && (b.Recovering == secret.ReasonMissing || b.Recovering == secret.ReasonMismatch)
	if b == nil || pendingRestore || state == Starting || (state != Ready && state != Draining && state != Recovering) {
		// No credential is read while the authority is not trusted.
		refuseWithoutCredential(w, state, route)
		return
	}
	if state == Recovering || keyLoss {
		// A recovering installation answers one request: a current credential's
		// own status, which names why nothing else works. The word names the
		// cause, so a key-broken installation's drain reports the key and not
		// the stop.
		reason := string(state)
		if keyLoss {
			reason = string(Recovering)
		}
		s.confined(w, r, b, route, listed, reason, mux)
		return
	}

	if route.Class == api.ClassLogin || route.Class == api.ClassSetup {
		release, ok := s.admission.password(w, r)
		if !ok {
			return
		}
		defer release()
	}
	p, next, ok := s.authenticate(w, r, b, route, listed)
	if !ok {
		return
	}
	r = next

	identity, authenticated := auth.SessionFrom(r.Context())
	keyAuthorized := authenticated && identity.Credential == "key" && (route.Right == "" || p.Has(route.Right))
	if unsafeMethod(r.Method) {
		if keyAuthorized && r.Method == "DELETE" && route.Pattern == "DELETE /api/v1/session" {
			api.WriteProblem(w, api.NewProblem(403, "cookie_required"), nil)
			return
		}
		if !keyAuthorized {
			if err := s.cop.Check(r); err != nil {
				api.WriteProblem(w, api.NewProblem(http.StatusForbidden, "cross_origin_denied"), nil)
				return
			}
		}
		if state == Draining {
			api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, "draining"), retryAfter(5))
			return
		}
	}
	if route.Right != "" && !p.Has(route.Right) {
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden, "forbidden"), nil)
		return
	}
	if identity, ok := auth.SessionFrom(r.Context()); ok && identity.Credential == "cookie" && !unsafeMethod(r.Method) {
		id, err := b.Sessions.Installation(r.Context())
		if err != nil {
			api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
			return
		}
		name := s.sessionCookie(r, b, id, "", time.Time{}).Name
		cookie, err := r.Cookie(name)
		if err != nil {
			api.WriteProblem(w, api.NewProblem(401, "credential_invalid"), nil)
			return
		}
		http.SetCookie(w, s.sessionCookie(r, b, id, cookie.Value, identity.ExpiresAt))
	}
	ctx := context.WithValue(auth.WithPrincipal(r.Context(), p), requestKey{}, r)
	// ServeMux assigns path values when it dispatches, after the guard has
	// authorized the resolved method and pattern.
	serve.ServeHTTP(w, r.WithContext(ctx))
}

// refuseWithoutCredential reports a request that arrives while no authority is
// trusted. The classes keep their own outcomes and every principal row answers
// 401 unauthenticated, never 503 and never the shell.
func refuseWithoutCredential(w http.ResponseWriter, state State, route api.Route) {
	switch route.Class {
	case api.ClassLogin:
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, string(state)), retryAfter(5))
	case api.ClassSetup:
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "setup_credential_invalid"), nil)
	default:
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "unauthenticated"), nil)
	}
}

// confined answers a recovering installation, and a key-broken one that is
// draining. The one served request is a current credential's own status. Login
// is refused with the reason and setup without a credential read; a principal
// route reads the credential only to tell a current one that the installation is
// recovering rather than that it is unauthenticated, and an invalid one is
// refused exactly as it is everywhere else.
func (s *Server) confined(w http.ResponseWriter, r *http.Request, b *Backend, route api.Route, listed bool, reason string, mux *http.ServeMux) {
	// The one route a confined installation still answers reads a credential and a
	// stored snapshot, so it takes its own admission reservation inside its own
	// authenticator, once it knows which credential it serves.
	if route.Pattern == "GET "+api.Prefix+"/system/status" {
		s.recoveringStatus(w, r, b, mux)
		return
	}
	switch route.Class {
	case api.ClassLogin:
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, reason), retryAfter(5))
	case api.ClassSetup:
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "setup_credential_invalid"), nil)
	default:
		if _, _, ok := s.authenticate(w, r, b, route, listed); !ok {
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, reason), retryAfter(5))
	}
}

// authenticate is the class authenticator of the route guard: setup for the
// setup class, the Host guard for login, and the key or the session cookie for
// every principal route. It returns the request it authorized on the context the
// request arrived with, because the credential it resolved travels in the request
// context. The principal class reads a stored credential, so that read is bounded
// by the lookup gate and refused 401 rather than held behind the store's readers.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, b *Backend, route api.Route, listed bool) (auth.Principal, *http.Request, bool) {
	var p auth.Principal
	// ref is the provenance this authentication accepted, for the consumers that
	// revalidate it later; it is minted in the branch that decided it, never
	// rebuilt from the request afterwards.
	var ref auth.CredentialRef
	// configured is the installation's direct-peer grant, a warning fact and
	// never a grant on an ineligible peer.
	configured := len(b.LocalBypass) > 0
	switch route.Class {
	case api.ClassSetup:
		p, ok := s.authenticateSetup(w, r, b)
		return p, r, ok
	case api.ClassLogin:
		st, err := b.Setup.StateForPassword(r.Context())
		if err != nil {
			if errors.Is(err, auth.ErrHashBusy) {
				api.WriteProblem(w, api.NewProblem(429, "hash_busy"), retryAfter(2))
				return p, r, false
			}
			api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
			return p, r, false
		}
		if (st.Open || len(b.LocalBypass) > 0) && !hostAllowed(r.Host, b.AllowedHosts) {
			api.WriteProblem(w, api.NewProblem(421, "host_not_allowed"), nil)
			return p, r, false
		}
	default:
		// The credential read below is this request's lookup, so it runs inside the
		// lookup gate: reserved against the direct peer, and released when this
		// function returns — before the authorized route takes its own reservation,
		// so one request never holds two.
		arrived := r
		lookup, release, ok := s.lookup(w, r)
		if !ok {
			return p, r, false
		}
		defer release()
		r = lookup
		if values, explicitKey := r.Header["X-Api-Key"]; explicitKey {
			if len(values) != 1 {
				api.WriteProblem(w, api.NewProblem(401, "credential_invalid"), nil)
				return p, r, false
			}
			// The slow control's key delay sits on this key verification call, which is
			// the one the key band measures. The confined status route reads a key too,
			// in status.go, and stays untimed: it serves one request while the
			// installation is not ready, so it is outside that band.
			if h, ok := any(s).(benchSlowHooks); ok {
				h.benchKeyDelay()
			}
			identity, err := b.Keys.Authenticate(r.Context(), values[0])
			if err != nil {
				if lookupExpired(w, r) {
					return p, r, false
				}
				status, reason := 500, "internal_error"
				if errors.Is(err, auth.ErrKeyInvalid) {
					status, reason = 401, "credential_invalid"
				}
				api.WriteProblem(w, api.NewProblem(status, reason), nil)
				return p, r, false
			}
			identity.LocalBypassEnabled = configured
			p = auth.Principal{Rights: identity.Rights}
			ref = auth.KeyRef(identity)
			r = r.WithContext(auth.WithActor(auth.WithSession(r.Context(), identity), auth.ActorOf(identity)))
		} else if bypassRef, granted := auth.BypassRef(r.RemoteAddr, b.LocalBypass, b.TrustedProxies); granted && hostAllowed(r.Host, b.AllowedHosts) {
			st, err := b.Setup.State(r.Context())
			if err != nil {
				if lookupExpired(w, r) {
					return p, r, false
				}
				api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
				return p, r, false
			}
			if st.Open {
				api.WriteProblem(w, api.NewProblem(401, "unauthenticated"), nil)
				return p, r, false
			}
			epoch, err := b.Sessions.Epoch(r.Context())
			if err != nil {
				if lookupExpired(w, r) {
					return p, r, false
				}
				api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
				return p, r, false
			}
			ref = bypassRef.WithEpoch(epoch)
			p = auth.Principal{Rights: []string{auth.RightStatusRead, auth.RightUsersRead, auth.RightUsersWrite}}
			session := auth.Session{Credential: auth.CredentialLocalBypass, Rights: p.Rights, RestoreEpoch: epoch, LocalBypassEnabled: configured}
			ctx := auth.WithActor(r.Context(), auth.BypassActor(epoch))
			// A cookie presented beside the grant is the logout authority: it is
			// validated and kept apart, so it never becomes the bypass identity and
			// its absence or invalidity never defeats the grant.
			if token, ok, err := s.presentedCookie(r, b); err == nil && ok {
				if identity, err := b.Sessions.AuthenticateReadOnly(r.Context(), token); err == nil {
					identity.LocalBypassEnabled = configured
					identity.CanLogout = true
					ctx = auth.WithCookieSession(ctx, identity)
					session.CanLogout = true
					// Sign-out revokes that exact verifier, never the grant, so only the
					// logout request itself answers as the cookie.
					if route.Pattern == "DELETE "+api.Prefix+"/session" {
						session = identity
					}
				}
			}
			r = r.WithContext(auth.WithSession(ctx, session))
		} else {
			token, ok, err := s.presentedCookie(r, b)
			if err != nil {
				if lookupExpired(w, r) {
					return p, r, false
				}
				api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
				return p, r, false
			}
			if !ok {
				api.WriteProblem(w, api.NewProblem(401, "unauthenticated"), nil)
				return p, r, false
			}
			identity, err := b.Sessions.AuthenticateReadOnly(r.Context(), token)
			if err != nil {
				if lookupExpired(w, r) {
					return p, r, false
				}
				if errors.Is(err, auth.ErrSessionInvalid) {
					api.WriteProblem(w, api.NewProblem(401, "credential_invalid"), nil)
				} else {
					api.WriteProblem(w, api.NewProblem(500, "internal_error"), nil)
				}
				return p, r, false
			}
			identity.LocalBypassEnabled = configured
			identity.CanLogout = true
			p = auth.Principal{Rights: identity.Rights}
			ref = auth.SessionRef(identity)
			ctx := auth.WithActor(auth.WithSession(r.Context(), identity), auth.ActorOf(identity))
			r = r.WithContext(auth.WithCookieSession(ctx, identity))
		}
		if !listed {
			api.WriteProblem(w, api.NewProblem(404, "not_found"), nil)
			return p, r, false
		}
		// The lookup is over: the route is served on the context the request arrived
		// with, carrying the identity and the provenance one of the three branches
		// above just resolved.
		r = identityOn(arrived, r)
		r = r.WithContext(auth.WithCredentialRef(r.Context(), ref))
	}
	return p, r, true
}

// lookupExpired reports whether the credential read ran out the lookup's bound,
// and answers the request when it did: a credential that could not be read inside
// the bound was never validated, so it is unauthenticated — the recorded 401 that
// every lifecycle state requires — and not a 500, which would blame this
// installation for a reader wait.
func lookupExpired(w http.ResponseWriter, r *http.Request) bool {
	if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		return false
	}
	api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "unauthenticated"), nil)
	return true
}

// presentedCookie returns the session cookie this request presents, if any.
func (s *Server) presentedCookie(r *http.Request, b *Backend) (string, bool, error) {
	id, err := b.Sessions.Installation(r.Context())
	if err != nil {
		return "", false, err
	}
	cookie, err := r.Cookie(s.sessionCookie(r, b, id, "", time.Time{}).Name)
	if err != nil {
		return "", false, nil
	}
	return cookie.Value, true, nil
}

// authenticateSetup is the setup class authenticator: the throttle, the Host
// guard while setup is open, then the bearer against the stored verifier.
func (s *Server) authenticateSetup(w http.ResponseWriter, r *http.Request, b *Backend) (auth.Principal, bool) {
	refuse := func(status int, code string, header http.Header) (auth.Principal, bool) {
		if b.Log != nil {
			b.Log.Info("setup request refused", "reason", code, "status", status)
		}
		api.WriteProblem(w, api.NewProblem(status, code), header)
		return auth.Principal{}, false
	}
	peer := peerIP(r.RemoteAddr)
	now := s.throttle.now()
	if wait := s.throttle.blocked(peer, now); wait > 0 {
		return refuse(http.StatusTooManyRequests, "setup_throttled", retryAfter(int((wait+time.Second-1)/time.Second)))
	}
	st, err := b.Setup.StateForPassword(r.Context())
	if err != nil {
		if errors.Is(err, auth.ErrHashBusy) {
			return refuse(http.StatusTooManyRequests, "hash_busy", retryAfter(2))
		}
		return refuse(http.StatusInternalServerError, "internal_error", nil)
	}
	if st.Open && !hostAllowed(r.Host, b.AllowedHosts) {
		return refuse(http.StatusMisdirectedRequest, "host_not_allowed", nil)
	}
	token, ok := bearer(r)
	v := auth.Verifier(token)
	if !ok || !st.Matches(v) {
		s.throttle.fail(peer, now)
		return refuse(http.StatusUnauthorized, "setup_credential_invalid", nil)
	}
	if !st.Open {
		return refuse(http.StatusConflict, "setup_completed", nil)
	}
	return auth.Principal{Rights: []string{auth.RightSetupCreate}, SetupVerifier: v}, true
}

// bearer returns the token of an `Authorization: Bearer <token>` header.
func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// hostAllowed admits an IP literal, localhost or a configured host name, so a
// DNS-rebinding page cannot reach setup through a name the owner never chose.
func hostAllowed(hostport string, allowed []string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(host, strings.TrimSuffix(strings.TrimSpace(a), ".")) {
			return true
		}
	}
	return false
}

// peerIP is the direct peer's address; forwarded headers are never read here.
func peerIP(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

func retryAfter(seconds int) http.Header {
	return http.Header{"Retry-After": {strconv.Itoa(seconds)}}
}

// Setup throttle: the 11th invalid setup credential from one peer within five
// minutes is refused with 429 until the oldest failure ages out (ADR 0009 J04).
const (
	throttleFailures = 10
	throttleWindow   = 5 * time.Minute
	throttlePeers    = 1024 // bound on tracked peers; a full table refuses new failing peers
)

type throttle struct {
	mu    sync.Mutex
	peers map[string][]time.Time
	now   func() time.Time
}

func newThrottle() *throttle { return &throttle{peers: map[string][]time.Time{}, now: time.Now} }

// blocked returns how long peer must wait, or 0.
func (t *throttle) blocked(peer string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	fails := t.prune(peer, now)
	if len(fails) < throttleFailures {
		if len(fails) == 0 && len(t.peers) >= throttlePeers {
			t.sweep(now)
			if len(t.peers) >= throttlePeers {
				return throttleWindow
			}
		}
		return 0
	}
	return fails[0].Add(throttleWindow).Sub(now)
}

// fail records one invalid credential from peer.
func (t *throttle) fail(peer string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fails := t.prune(peer, now)
	if len(fails) == 0 && len(t.peers) >= throttlePeers {
		t.sweep(now)
		if len(t.peers) >= throttlePeers {
			return
		}
	}
	if len(fails) == throttleFailures {
		fails = fails[1:]
	}
	t.peers[peer] = append(fails, now)
}

// prune drops peer's failures older than the window and returns the rest.
func (t *throttle) prune(peer string, now time.Time) []time.Time {
	fails := t.peers[peer]
	i := 0
	for i < len(fails) && now.Sub(fails[i]) >= throttleWindow {
		i++
	}
	fails = fails[i:]
	if len(fails) == 0 {
		delete(t.peers, peer)
		return nil
	}
	t.peers[peer] = fails
	return fails
}

// sweep drops every peer whose failures all aged out.
func (t *throttle) sweep(now time.Time) {
	for peer := range t.peers {
		t.prune(peer, now)
	}
}
