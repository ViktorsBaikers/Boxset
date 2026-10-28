// Package server is Boxset's HTTP surface. It answers the public liveness
// route from the moment its listener starts, in every lifecycle state, the
// public web shell under the runtime base and, once the store is attached,
// the native API behind the route guard.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/bounded"
)

// State is the lifecycle state /ping reports. The values are the only text the
// public route ever carries besides its fixed body.
type State string

const (
	Starting   State = "starting"
	Recovering State = "recovering"
	Ready      State = "ready"
	Draining   State = "draining"
)

// Backend is what the API needs once the store is open: the setup flow, the
// facts the status route reports and the hosts the Host guard admits.
type Backend struct {
	Setup          *auth.Setup
	Sessions       *auth.Sessions
	Keys           *auth.Keys
	TrustedProxies []netip.Prefix
	LocalBypass    []netip.Prefix
	// Log receives one structured line per setup decision; it must be a
	// redacting logger (redact.NewHandler).
	Log *slog.Logger
	// AllowedHosts (BOXSET_ALLOWED_HOSTS) are host names, besides IP literals
	// and localhost, that may reach setup while it is open.
	AllowedHosts  []string
	Version       string
	SchemaVersion int64
	RestoreEpoch  int64
	MigrationMs   int64
	SetupMs       int64
	// Recovering names why the installation is not ready: key_missing,
	// key_mismatch or restore_in_progress. Empty in every other state.
	Recovering string
}

// Server is one http.Server with the recorded runtime bounds and a lifecycle state.
type Server struct {
	http          *http.Server
	state         atomic.Value // State
	backend       atomic.Pointer[Backend]
	started       time.Time
	readyAt       atomic.Int64 // unix nanoseconds of the first Ready, 0 before
	throttle      *throttle
	loginThrottle *throttle
	base          string
	cop           *http.CrossOriginProtection
	admission     *admission
	routeDeadline time.Duration
	// retainedLog, retainedCache and retainedReplay are the benchfill fixture's
	// own full-cap collections, kept here so they stay reachable for the process
	// lifetime; every other build leaves them nil. No status or metrics reader may
	// touch them while they are held: reading a bounded collection drops what has
	// aged out, so the replay ring would report 0 B at a sample taken after its
	// five-minute age while its whole cap is still retained.
	retainedLog    *bounded.Ring
	retainedCache  *bounded.Cache
	retainedReplay *bounded.Ring
}

// benchSlowHooks is the slow control's seam: the benchslow build defines the
// three recorded delays on *Server, and every other build declares no method at
// all, so each hook site's assertion fails and the site does nothing.
type benchSlowHooks interface {
	benchStartDelay()
	benchKeyDelay()
	benchListDelay()
}

// benchFillHooks is the fill fixture's seam: the benchfill build fills three
// collections of its own to their recorded caps and hands them back, and every
// other build declares no method at all, so the hook site keeps nothing. The
// return is typed, so a fill that hands back the wrong collections, or none,
// cannot compile.
type benchFillHooks interface {
	benchFill() (*bounded.Ring, *bounded.Cache, *bounded.Ring)
}

// New returns a server in the Starting state that serves the embedded web build under
// base (BOXSET_URL_BASE: "" or an absolute path such as "/media"). An invalid base
// (ErrBaseInvalid) or an unusable embedded build (ErrShellMissing, ErrShellInconsistent) is an
// error: there is no headless or fallback mode. Every API route is registered here and
// answers 401 until Attach supplies the store-backed services and the state is Ready.
func New(base string) (*Server, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShellMissing, err)
	}
	return newServer(dist, base)
}

func newServer(dist fs.FS, base string) (*Server, error) {
	sh, err := loadShell(dist, base, hostEnv())
	if err != nil {
		return nil, err
	}
	s := &Server{started: time.Now(), throttle: newThrottle(), loginThrottle: newThrottle(), base: sh.base, cop: http.NewCrossOriginProtection(), admission: newAdmission(), routeDeadline: routeDeadline}
	s.state.Store(Starting)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", s.ping)
	routes := map[string]api.Route{"GET /ping": {Pattern: "GET /ping", Class: api.ClassPublicDoc}}
	for _, p := range sh.register(mux) {
		routes[p] = api.Route{Pattern: p, Class: api.ClassPublicDoc}
	}
	_, apiRoutes := api.New(mux, s)
	for _, r := range apiRoutes {
		routes[r.Pattern] = r
	}
	s.http = &http.Server{
		Handler:           withSecurityHeaders(stripBase(sh.base, compress(s.guard(mux, routes)))),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds the whole request head and body on every path, so a client
		// that sends a Content-Length and never sends the body ends inside the route
		// deadline instead of holding its reader slot. There is no WriteTimeout: an SSE
		// stream is legitimately long-lived, and the guard arms its own write deadline
		// per route (ADR 0008 I03).
		ReadTimeout:    routeDeadline,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	// The fill fixture hands back the collections it filled to their caps, so the
	// fields hold them for the process lifetime. Only the tagged build has
	// anything to hand back, and its own check runs inside the fill.
	if f, ok := any(s).(benchFillHooks); ok {
		s.retainedLog, s.retainedCache, s.retainedReplay = f.benchFill()
	}
	return s, nil
}

// Attach supplies the store-backed services after store.Open. Until it is
// called no credential is read and every gated route answers 401.
func (s *Server) Attach(b Backend) {
	b.TrustedProxies = slices.Clone(b.TrustedProxies)
	b.LocalBypass = slices.Clone(b.LocalBypass)
	b.AllowedHosts = slices.Clone(b.AllowedHosts)
	if b.Sessions == nil && b.Setup != nil {
		b.Sessions = b.Setup.Sessions()
	}
	if b.Sessions != nil {
		b.Keys = b.Sessions.Keys()
	}
	s.backend.Store(&b)
}

// SetState records the lifecycle state the next /ping reports.
func (s *Server) SetState(st State) {
	if st == Ready {
		s.readyAt.CompareAndSwap(0, time.Now().UnixNano())
	}
	s.state.Store(st)
}

func (s *Server) currentState() State { return s.state.Load().(State) }

func (s *Server) ListUsers(ctx context.Context, query auth.UserQuery, visit func(auth.UserCollection) error) error {
	b := s.backend.Load()
	if b == nil || b.Sessions == nil {
		return errors.New("server: not attached")
	}
	// The slow control's list delay sits on this one list response path.
	if h, ok := any(s).(benchSlowHooks); ok {
		h.benchListDelay()
	}
	return b.Sessions.ListUsers(ctx, query, func(collection auth.UserCollection) error {
		collection.SchemaVersion = b.SchemaVersion
		return visit(collection)
	})
}

// UpdateUser runs one authorized user edit and records its audit event: the
// credential kind, action, result and reason only, never the verifier, a name,
// the request or the target's payload.
func (s *Server) UpdateUser(ctx context.Context, actor auth.Actor, id int64, patch auth.UserPatch) (auth.User, error) {
	b := s.backend.Load()
	if b == nil || b.Sessions == nil {
		return auth.User{}, errors.New("server: not attached")
	}
	u, err := b.Sessions.UpdateUser(ctx, actor, id, patch)
	if b.Log != nil {
		result, reason := "edited", "none"
		var conflict *auth.RevisionConflict
		switch {
		case err == nil:
		case errors.Is(err, auth.ErrForbidden):
			result, reason = "refused", "forbidden"
		case errors.Is(err, auth.ErrSessionInvalid):
			result, reason = "refused", "credential_invalid"
		case errors.As(err, &conflict):
			result, reason = "refused", "revision_conflict"
		case errors.Is(err, auth.ErrLastAdmin):
			result, reason = "refused", "last_admin"
		case errors.Is(err, auth.ErrUserNotFound):
			result, reason = "refused", "user_not_found"
		default:
			result, reason = "refused", "internal_error"
		}
		b.Log.Info("user edit", "credential", actor.Credential(), "action", "user.update", "result", result, "reason", reason)
	}
	return u, err
}

// ping is public: 200 "ok" with the lifecycle enum, never cached, in every state.
func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Boxset-State", string(s.state.Load().(State)))
	_, _ = w.Write([]byte("ok"))
}

// CreateFirstAdmin is the setup operation's service: the guard has verified
// the setup credential, so p holds its verifier.
func (s *Server) CreateFirstAdmin(ctx context.Context, p auth.Principal, username, password string) (auth.User, error) {
	b := s.backend.Load()
	if b == nil {
		return auth.User{}, auth.ErrSetupCredential
	}
	u, err := b.Setup.Create(ctx, p.SetupVerifier, username, password)
	reason := "setup_created"
	switch {
	case errors.Is(err, auth.ErrSetupCompleted):
		reason = "setup_completed"
	case errors.Is(err, auth.ErrSetupCredential):
		reason = "setup_credential_invalid"
	case errors.Is(err, auth.ErrHashBusy):
		reason = "hash_busy"
	case err != nil:
		reason = "setup_failed"
	}
	if b.Log != nil {
		if err == nil {
			b.Log.Info("first admin created", "reason", reason, "user_id", u.ID)
		} else {
			b.Log.Info("first admin not created", "reason", reason)
		}
	}
	return u, err
}

// Status is the status operation's service.
func (s *Server) Status(context.Context) (api.Status, error) {
	b := s.backend.Load()
	if b == nil {
		return api.Status{}, errors.New("server: not attached")
	}
	st := api.Status{
		State:           string(s.currentState()),
		Version:         b.Version,
		SchemaVersion:   b.SchemaVersion,
		RestoreEpoch:    b.RestoreEpoch,
		StartedAt:       s.started.UTC().Truncate(time.Second),
		Timings:         api.Timings{MigrationMs: b.MigrationMs, SetupMs: b.SetupMs},
		GoMemLimitBytes: debug.SetMemoryLimit(-1),
	}
	if st.State == string(Recovering) {
		st.Reason = b.Recovering
	}
	if ns := s.readyAt.Load(); ns != 0 {
		st.ReadyAt = time.Unix(0, ns).UTC().Truncate(time.Second)
	}
	return st, nil
}

// Serve accepts connections on l until Shutdown or a listener failure.
func (s *Server) Serve(l net.Listener) error {
	// The slow control's start delay runs before the first Accept, so a client
	// measures it as start latency.
	if h, ok := any(s).(benchSlowHooks); ok {
		h.benchStartDelay()
	}
	return s.http.Serve(l)
}

// Shutdown stops accepting connections and waits for requests in flight, until ctx ends.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }
