package server

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// The recorded runtime read bounds (architecture.md § Runtime bounds). A request
// over either in-flight cap is refused at once with 429 read_cap; a request that
// has to wait for one of the reader slots waits at most readWait and is then
// refused with 503 reader_busy. Both refusals carry Retry-After, so a client
// that backs off is served next time. The credential lookup a request performs
// before it is admitted spends the same counts under the same numbers, waits no
// longer than the same readWait and is refused with the recorded 401: see lookup.
const (
	readsPerCredential = 8
	readsGlobal        = 256
	// readerSlots is four times one credential's in-flight cap, so a credential
	// that holds its whole share still leaves the other slots to everyone else.
	readerSlots      = 32
	readWait         = 2 * time.Second
	retrySeconds     = 1
	passwordRequests = 32
)

// routeDeadline is the safety net for every route the guard answers beyond the
// public-doc rows: its read and write deadlines, and the server's own ReadTimeout
// for a body that never arrives, so a request that drips or stalls ends instead
// of holding its reader slot for the life of the server.
const routeDeadline = 30 * time.Second

// admission bounds the read work one credential and the whole server may have in
// flight, so a flood of one key cannot take the reader slots from everyone else
// and a held read cannot grow the goroutine count. Two gates spend it: the
// credential lookup a request performs before anything is resolved (lookup,
// keyed by the direct peer, counted, bounded by its wait and refused 401) and the
// read of the credential that did resolve (admit, keyed by that credential, a
// count and a reader slot, refused 429 read_cap or 503 reader_busy). The route
// deadline covers both, and the store's own reader pool bounds how many reads run
// at once, never how long a waiter behind them may sit.
type admission struct {
	mu        sync.Mutex
	running   map[string]int
	total     int
	slots     chan struct{}
	wait      time.Duration
	passwords chan struct{}
}

func newAdmission() *admission {
	return &admission{running: map[string]int{}, slots: make(chan struct{}, readerSlots), wait: readWait, passwords: make(chan struct{}, passwordRequests)}
}

func (a *admission) password(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if r.Context().Err() != nil {
		return nil, false
	}
	select {
	case a.passwords <- struct{}{}:
		return func() { <-a.passwords }, true
	default:
		api.WriteProblem(w, api.NewProblem(http.StatusTooManyRequests, "hash_busy"), retryAfter(2))
		return nil, false
	}
}

// take counts one in-flight operation for key and returns its release, together
// with the recorded refusal code when key or the server already has its whole
// in-flight cap: read_cap. It is the accounting both gates share; a read then also
// takes a reader slot (reserve), which a lookup never waits for.
func (a *admission) take(key string) (func(), string) {
	a.mu.Lock()
	if a.running[key] >= readsPerCredential || a.total >= readsGlobal {
		a.mu.Unlock()
		return nil, "read_cap"
	}
	a.running[key]++
	a.total++
	a.mu.Unlock()
	return func() { a.done(key) }, ""
}

// reserve is a read's reservation: the shared count, then one reader slot, waited
// for at most the recorded wait. It returns the release, or the recorded refusal
// code when it took none: read_cap for a cap, reader_busy when no slot freed. The
// caller writes the refusal, because the same overflow is 429 read_cap or 503
// reader_busy for a resolved credential's read and 401 unauthenticated for the
// credential lookup that precedes one. A reservation is returned once.
func (a *admission) reserve(key string) (func(), string) {
	release, refusal := a.take(key)
	if refusal != "" {
		return nil, refusal
	}
	timer := time.NewTimer(a.wait)
	defer timer.Stop()
	select {
	case a.slots <- struct{}{}:
		return func() {
			<-a.slots
			release()
		}, ""
	case <-timer.C:
		release()
		return nil, "reader_busy"
	}
}

// admit reserves one reader slot for key, the credential the route's own
// authenticator has just resolved, and returns its release. When the in-flight
// caps are already reached it refuses with 429 read_cap; when no slot frees
// within the wait it refuses with 503 reader_busy. Every other request proceeds
// and must call release exactly once.
func (a *admission) admit(w http.ResponseWriter, key string) (func(), bool) {
	release, refusal := a.reserve(key)
	if refusal == "" {
		return release, true
	}
	status := http.StatusServiceUnavailable
	if refusal == "read_cap" {
		status = http.StatusTooManyRequests
	}
	api.WriteProblem(w, api.NewProblem(status, refusal), retryAfter(retrySeconds))
	return nil, false
}

// done returns key's reservation.
func (a *admission) done(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running[key]--
	a.total--
	if a.running[key] <= 0 {
		delete(a.running, key)
	}
}

// admittedKey names who is spending read capacity: the kind and row id of the
// credential the route's own authenticator has just resolved. It is called after
// resolution only, so the bucket is the caller's and not a value the request can
// rotate, and it holds no verifier: a session is named by its kind and row, never
// by its token or its digest. The running map stays bounded because an entry is
// deleted when its count returns to zero.
func admittedKey(session auth.Session) string {
	return session.Credential + ":" + strconv.FormatInt(session.ID, 10)
}

// routeBound arms the route deadline on a response: its read side ends a request
// that drips its body, its write side a handler that stalls or a body that never
// drains. It is the safety net for every route the guard answers beyond the
// public-doc rows, and it is armed once when the route is classified.
func (s *Server) routeBound(w http.ResponseWriter) {
	if s.routeDeadline <= 0 {
		return
	}
	c := http.NewResponseController(w)
	at := time.Now().Add(s.routeDeadline)
	_ = c.SetReadDeadline(at)
	_ = c.SetWriteDeadline(at)
}

// admit is the post-resolution admission placement: a resolved route runs inside
// it, so its response is inside the route deadline and its read work inside the
// read budget. The reservation is taken once the route's own authenticator has
// resolved a credential, which is the point at which the request is one caller's
// read: a request that presents nothing, or presents a dead credential, is still
// answered its recorded 401 and never 429 read_cap or 503 reader_busy. Setup and
// login resolve no stored representation — their own throttles bound them, and a
// concurrent setup submission is legitimate — so they take no slot; the credential
// lookup an authenticated route performs before this point has its own gate
// (lookup).
func (s *Server) admit(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.routeBound(w)
		session, resolved := auth.SessionFrom(r.Context())
		if !resolved {
			h.ServeHTTP(w, r)
			return
		}
		release, ok := s.admission.admit(w, admittedKey(session))
		if !ok {
			return
		}
		defer release()
		h.ServeHTTP(w, r)
	})
}

// lookup bounds the credential read a request performs before it is admitted, and
// returns the request that read must use. It spends the same counts as a read under
// the same numbers, keyed by the direct peer because no credential is resolved yet,
// and the read it covers runs under the recorded wait as its deadline — so a lookup
// cannot queue behind the store's reader pool past the bound: that pool bounds
// concurrent readers, never the waiters behind them. It takes no reader slot, which
// belongs to the read of a credential that did resolve, so a request that presents a
// dead credential still resolves to its own recorded 401 under that saturation. A
// lookup that takes no count, or that outlives it, is a credential that could not be
// validated, so it answers the recorded 401 unauthenticated — never 429 read_cap and
// never 503 reader_busy, which belong to a resolved credential.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	release, refusal := s.admission.take(lookupKey(r))
	if refusal != "" {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, "unauthenticated"), nil)
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.admission.wait)
	return r.WithContext(ctx), func() {
		cancel()
		release()
	}, true
}

// lookupKey names the peer whose credential lookups share one budget. It is the
// direct peer, never a forwarded header and never a presented value: no
// credential is resolved yet, so there is nothing else to charge, and a client
// cannot mint buckets by rotating what it presents.
func lookupKey(r *http.Request) string {
	return "lookup:" + peerIP(r.RemoteAddr)
}

// identityOn returns the request the route is served on: the context the request
// arrived with, carrying the session, the actor and the cookie session the
// authenticator resolved inside the lookup. The lookup's bound covers the
// credential read only, so the route that follows keeps the cancellation and the
// deadline of the request itself instead of expiring with the lookup.
func identityOn(arrived, resolved *http.Request) *http.Request {
	ctx := arrived.Context()
	if session, ok := auth.SessionFrom(resolved.Context()); ok {
		ctx = auth.WithSession(ctx, session)
	}
	if actor, ok := auth.ActorFrom(resolved.Context()); ok {
		ctx = auth.WithActor(ctx, actor)
	}
	if session, ok := auth.CookieSessionFrom(resolved.Context()); ok {
		ctx = auth.WithCookieSession(ctx, session)
	}
	return arrived.WithContext(ctx)
}
