package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// Effects is what one started installation composes: the one store, the one
// jobs runtime and the owners that serve work from it. Every field that has
// landed is built here, never left nil for a later slice to fill in.
type Effects struct {
	DB       *store.DB
	Jobs     *jobs.Runtime
	Outbound *outbound.Client
	Library  *library.Library
	Setup    *auth.Setup
	Sessions *auth.Sessions
	Keys     *auth.Keys
	Hub      *events.Hub
	Log      *slog.Logger
}

// NewEffects builds the composition around an open store and the installation's
// one setup owner. It starts nothing: the jobs runtime begins in the parent's
// ready transition, so no effect is dispatched before the installation is ready.
func NewEffects(db *store.DB, setup *auth.Setup, cfg ServeConfig, log *slog.Logger) (*Effects, error) {
	if db == nil || setup == nil {
		return nil, errors.New("server: effects without a store")
	}
	lib := library.New(db)
	// The hub is built after the runtime, because a frame id names the start the
	// runtime runs under; a transition can only be published once the runtime
	// started, which is after this assignment.
	var hub *events.Hub
	// The landed publish closure selects the hub declared here, so the default
	// is chosen here and not in ServeConfig.defaults(), where the hub is out of
	// scope. A caller that supplies its own Publish replaces it.
	publish := cfg.Publish
	if publish == nil {
		publish = func(ev jobs.Event) {
			if hub != nil {
				hub.Publish(events.Event{Name: events.NameJob, Payload: api.JobEvent(ev)})
			}
		}
	}
	runtime, err := jobs.New(db, jobs.Config{
		Now:     cfg.Now,
		After:   cfg.After,
		Boot:    cfg.Boot,
		Publish: publish,
		// The composition binds the runtime's failure records to the
		// installation's redacting logger, so a refused claim, sweep, transition
		// or reconciliation is one reason-coded line and never silence.
		Log: func(reason string, kv ...any) {
			if log == nil {
				return
			}
			log.Warn("job runtime", append([]any{"reason", reason}, kv...)...)
		},
		Zone: installZone(log),
	}, []jobs.Handler{jobs.PurgeHandler(lib), jobs.PruneHandler(db)})
	if err != nil {
		return nil, err
	}
	// The one outbound client this installation has: the runtime's own background
	// slot is its slot, so a physical send spends the same ceiling the attempts do,
	// and the composition's clock is its clock, so a budget wait is the one the
	// runtime already measures.
	client := outbound.New(outbound.Config{
		Resolver:       cfg.Outbound.Resolver,
		Slot:           runtime.Slot,
		Now:            cfg.Now,
		After:          cfg.After,
		Version:        cfg.Version,
		Transport:      cfg.Outbound.Transport,
		ForbiddenHosts: cfg.Outbound.ForbiddenHosts,
	})
	// The ready-time installation identity: the same read the cookie name and the
	// cursors use, so a hello frame and a session response name the same install.
	var installID string
	var restoreEpoch int64
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		installation, err := storedb.New(tx).GetInstallation(context.Background())
		installID, restoreEpoch = installation.InstallID, installation.RestoreEpoch
		return err
	}); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	hub = events.New(events.Config{
		Boot:         runtime.Boot(),
		InstallID:    installID,
		RestoreEpoch: restoreEpoch,
		Now:          cfg.Now,
		// The one current-authority decision, over the same store the guard reads:
		// every frame and heartbeat re-runs it, and nothing here caches a grant.
		Revalidate: func(ctx context.Context, ref auth.CredentialRef) (auth.Principal, time.Time, error) {
			return auth.Revalidate(ctx, db, ref, now())
		},
		Log: func(reason string, kv ...any) {
			if log != nil {
				log.Warn("event stream", append([]any{"reason", reason}, kv...)...)
			}
		},
	})
	return &Effects{
		DB:       db,
		Jobs:     runtime,
		Outbound: client,
		Library:  lib,
		Setup:    setup,
		Sessions: setup.Sessions(),
		Keys:     setup.Sessions().Keys(),
		Hub:      hub,
		Log:      log,
	}, nil
}

// installZone is the zone the installation's daily rules keep their wall clock
// in: TZ when it names a loadable zone, UTC otherwise. The command imports
// time/tzdata, so a container without a zone database still resolves one.
func installZone(log *slog.Logger) *time.Location {
	name := os.Getenv("TZ")
	if name == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		// A zone that cannot be loaded is recorded, not guessed at: the rule keeps
		// UTC until the owner fixes TZ.
		if log != nil {
			log.Warn("job runtime", "reason", "zone_unavailable", "zone", name)
		}
		return time.UTC
	}
	return location
}

// stopDeadline bounds the stop of the jobs runtime at two seconds before the
// parent's drain deadline, so the store still has time to close after it.
func stopDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, deadline.Add(-2*time.Second))
	}
	return context.WithCancel(ctx)
}
