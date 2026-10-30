package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// DrainTimeout is how long one stop waits for requests in flight and for the
// jobs runtime to settle before it closes the store.
const DrainTimeout = 30 * time.Second

// RefusedError is a start refusal: the reason word the command prints and the
// landed error behind it. The command owns the exit code and the diagnostics.
type RefusedError struct {
	Reason string
	Err    error
}

func (e *RefusedError) Error() string { return e.Reason + ": " + e.Err.Error() }

// Unwrap lets the command recognise a landed error behind the reason.
func (e *RefusedError) Unwrap() error { return e.Err }

// StopError is why a stop ended short. The command owns what is printed and
// which exit code follows, so this carries only the reason word and the cause.
type StopError struct {
	Reason string
	Err    error
}

func (e *StopError) Error() string { return e.Reason + ": " + e.Err.Error() }

// Unwrap exposes the cause, so a caller can test for jobs.ErrAttemptsLive.
func (e *StopError) Unwrap() error { return e.Err }

// The reason words a stop reports.
const (
	ReasonDrain              = "drain"
	ReasonMetadataDrain      = "metadata_drain_failed"
	ReasonOutboundDrain      = "outbound_drain_failed"
	ReasonStoreClose         = "store_close_failed"
	ReasonStoreCloseDeadline = "store_close_deadline"
	ReasonAttemptsLive       = "attempts_live"
)

// ServeConfig is one start's inputs. The command owns the environment, its
// usage, the mount check and the crash monitor's process; the fields it keeps
// as functions are the steps it wants to be able to hold for a proof.
type ServeConfig struct {
	// DataDir is the resolved data directory: the mount check already ran.
	DataDir string
	// Port is the listener port; URLBase is BOXSET_URL_BASE.
	Port    string
	URLBase string

	TrustedProxies []netip.Prefix
	LocalBypass    []netip.Prefix
	AllowedHosts   []string
	// EnvSetupToken is BOXSET_SETUP_TOKEN, read once and dropped by the command.
	EnvSetupToken string
	// Version is what status and the User-Agent report.
	Version string
	// SetupHint is the command that prints the setup token here.
	SetupHint string

	// Log opens the installation's retained log and its closing step.
	Log func(dir string) (*slog.Logger, func() error, error)
	// CrashMonitor starts the runtime crash recorder and returns its stop.
	CrashMonitor func(dir string) (func(), error)
	// PublishSnapshot publishes the verified pre-migration snapshot.
	PublishSnapshot func(ctx context.Context, l install.Layout, p store.Pending) error
	// Publish publishes one job transition; nil publishes through this
	// installation's event hub.
	Publish func(jobs.Event)
	// ResumeRestore finishes an interrupted restore; the command supplies it.
	ResumeRestore func(ctx context.Context, l install.Layout, opts install.RestoreOptions) (install.Recovery, error)
	// CloseStore closes the store; the command's proof can hold it.
	CloseStore func(*store.DB) error

	// Boot is this start's lease identity. Empty means one is generated, which
	// is what makes an earlier process's running rows foreign.
	Boot string
	// Now and After are the runtime's clock; nil uses the process clock.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Outbound is the outbound environment the one client is built from: the
	// resolver, the transport wrap and the forbidden host digests. The slot, the
	// clock and the version are this composition's own, so they are not read here.
	Outbound outbound.Config
}

// defaults fills the steps a caller did not supply with the landed ones.
func (c *ServeConfig) defaults() {
	if c.PublishSnapshot == nil {
		c.PublishSnapshot = install.PublishSnapshot
	}
	if c.ResumeRestore == nil {
		c.ResumeRestore = install.Resume
	}
	if c.CloseStore == nil {
		c.CloseStore = (*store.DB).Close
	}
	if c.Port == "" {
		c.Port = "8484"
	}
}

// Serving is one live installation: its state, its effects and the steps that
// end it. Stop owns the drain order and releases the lifetime lock last.
type Serving struct {
	State   State
	Effects *Effects

	cfg       ServeConfig
	log       *slog.Logger
	lock      *install.Lock
	srv       *Server
	db        *store.DB
	sessions  *auth.Sessions
	served    chan error
	worker    chan struct{}
	stopWork  context.CancelFunc
	workerEnd error
	stopLog   func() error
	stopCrash func()

	workerStopReason func(error) string
	done             chan struct{}
	closeErr         error
}

// Served reports the listener's own error, if it failed while it was serving.
func (s *Serving) Served() <-chan error { return s.served }

// Start runs one installation from the lifetime lock to the parent's ready
// transition, and returns it live. The order is the landed one: lock, retained
// log, crash monitor, restore journal, shell, run state, listener, store with
// its snapshot-before-migrate, setup and installation facts, effects, and the
// ready (or landed recovering) transition. The jobs runtime starts inside that
// ready transition, so a ready Serving has already recovered, schedules and
// reconciles and has begun claiming; a recovering one dispatches nothing.
func Start(ctx context.Context, cfg ServeConfig) (*Serving, error) {
	cfg.defaults()
	if cfg.DataDir == "" {
		return nil, &RefusedError{Reason: "data_dir_invalid", Err: errors.New("no data directory")}
	}
	if cfg.Log == nil {
		return nil, &RefusedError{Reason: "log_unavailable", Err: errors.New("the composition was started without a log")}
	}
	if cfg.CrashMonitor == nil {
		return nil, &RefusedError{Reason: "crash_monitor_unavailable", Err: errors.New("the composition was started without a crash monitor")}
	}
	lock, err := install.Acquire(cfg.DataDir)
	if errors.Is(err, install.ErrLockHeld) {
		return nil, &RefusedError{Reason: "lock_held", Err: errors.New("another Boxset server is using this data directory; stop it first")}
	}
	if err != nil {
		return nil, &RefusedError{Reason: "lock_failed", Err: err}
	}
	s := &Serving{cfg: cfg, lock: lock, done: make(chan struct{}), workerStopReason: stopReason}
	// Every failure below unwinds in the landed order: the listener first, then
	// the store, then the lock.
	fail := func(reason string, err error) (*Serving, error) {
		s.unwind()
		return nil, &RefusedError{Reason: reason, Err: err}
	}

	log, stopLog, err := cfg.Log(cfg.DataDir)
	if err != nil {
		return fail("log_unavailable", err)
	}
	s.log, s.stopLog = log, stopLog
	stopCrash, err := cfg.CrashMonitor(cfg.DataDir)
	if err != nil {
		return fail("crash_monitor_unavailable", err)
	}
	s.stopCrash = stopCrash

	layout := install.Layout{Dir: cfg.DataDir}
	recovering := ""
	switch present, err := journalPresent(layout); {
	case err != nil:
		return fail("restore_journal_unreadable", err)
	case present:
		// A journal an interrupted restore left is finished or discarded before
		// the shell, the listener and the store exist.
		opts := install.RestoreOptions{Reconcile: auth.ReconcileRestore, Snapshot: cfg.PublishSnapshot, Log: log}
		if _, err := cfg.ResumeRestore(ctx, layout, opts); err != nil {
			if errors.Is(err, install.ErrRestoreUnsupported) {
				return fail("restore_unsupported", err)
			}
			log.Warn("restore did not finish", "reason", ReasonRestoreInProgress, "error", err)
			recovering = ReasonRestoreInProgress
		}
	}

	srv, err := New(cfg.URLBase)
	if err != nil {
		return fail("shell_unavailable", err)
	}
	s.srv = srv
	if err := WriteRunState(cfg.DataDir, Starting); err != nil {
		return fail("run_state_unavailable", err)
	}
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fail("listen_failed", err)
	}
	s.served = make(chan error, 1)
	go func() { s.served <- srv.Serve(ln) }()

	migrationStart := time.Now()
	db, err := openStore(ctx, layout, log, cfg.PublishSnapshot)
	if err != nil {
		_ = s.drainListener()
		if errors.Is(err, install.ErrSnapshotUnsupported) {
			return fail("snapshot_unsupported", err)
		}
		return fail("store_unavailable", err)
	}
	s.db = db
	migrationMs := time.Since(migrationStart).Milliseconds()
	backend, err := prepareAccess(ctx, db, cfg.DataDir, cfg.EnvSetupToken, log, cfg.SetupHint)
	if err != nil {
		return fail("setup_unavailable", err)
	}
	// An installation whose keys cannot be opened never becomes ready: nothing
	// authenticates until every record's key and the current key are usable.
	if recovering == "" {
		reason, err := secret.New(db, cfg.DataDir).Check(ctx)
		switch {
		case err != nil:
			log.Warn("installation key check failed", "reason", secret.ReasonMissing, "error", err)
			recovering = secret.ReasonMissing
		case reason != "":
			recovering = reason
		}
	}
	backend.AllowedHosts = cfg.AllowedHosts
	backend.TrustedProxies = cfg.TrustedProxies
	backend.LocalBypass = cfg.LocalBypass
	backend.Version = cfg.Version
	backend.MigrationMs = migrationMs
	backend.Sessions = backend.Setup.Sessions()
	s.sessions = backend.Sessions

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	s.stopWork = cancelWorker
	s.worker = make(chan struct{})
	go func() {
		defer close(s.worker)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-workerCtx.Done():
				s.workerEnd = workerCtx.Err()
				return
			case <-timer.C:
				if err := backend.Sessions.FlushMetadata(workerCtx); err != nil && workerCtx.Err() == nil {
					log.Warn("credential metadata flush failed", "reason", "metadata_flush_failed")
				}
			}
		}
	}()
	backend.Recovering = recovering
	// The one composition of this installation. Nothing in it runs yet.
	effects, err := NewEffects(db, backend.Setup, cfg, log)
	if err != nil {
		return fail("effects_unavailable", err)
	}
	s.Effects = effects
	// The jobs read model reads the runtime this composition runs, so the backend
	// is attached once the whole composition exists and not before it: a route
	// answered by a half-built composition would refuse work that already has an
	// owner.
	backend.Jobs = effects.Jobs
	// The read model and the stream read what the composition runs, and the
	// users event reads its revision from the same store.
	backend.Hub = effects.Hub
	backend.DB = db
	srv.Attach(backend)

	if recovering != "" {
		log.Warn("installation is recovering: no credential authenticates until it is healed", "reason", recovering)
		srv.SetState(Recovering)
		if err := WriteRunState(cfg.DataDir, Recovering); err != nil {
			log.Warn("run state not recorded", "reason", "run_state_unavailable")
		}
		s.State = Recovering
		return s, nil
	}
	// The ready transition starts the runtime: recovery, reconciliation, then
	// claims. The state says ready only after that call completed: a start that
	// refused here leaves the server and the recorded state at starting, so
	// neither /ping nor healthcheck reports a usable installation.
	if err := effects.Jobs.Start(ctx); err != nil {
		return fail("jobs_unavailable", err)
	}
	srv.SetState(Ready)
	if err := WriteRunState(cfg.DataDir, Ready); err != nil {
		log.Warn("run state not recorded", "reason", "run_state_unavailable")
	}
	s.State = Ready
	return s, nil
}

// Stop drains the installation in the landed order: run state, streams, the
// credential worker, the listener, then the jobs runtime (claims stop, attempts
// are cancelled and joined) and only then the store, with the lock released
// last. A stop that could not join every attempt leaves the store open and
// returns the reason, so the command can exit without an early close.
func (s *Serving) Stop(ctx context.Context) error {
	select {
	case <-s.done:
		return s.closeErr
	default:
	}
	defer close(s.done)

	stop := func(err error) error {
		s.closeErr = err
		return err
	}
	var failures []error
	if s.srv != nil {
		s.srv.SetState(Draining)
	}
	if err := WriteRunState(s.cfg.DataDir, Draining); err != nil && s.log != nil {
		s.log.Warn("run state not recorded", "reason", "run_state_unavailable")
	}
	// The stream ends before anything it reports on is torn down: the drain's
	// system frame is published, every stream is closed, and no new stream is
	// admitted while the rest of the stop runs.
	if s.Effects != nil && s.Effects.Hub != nil {
		s.Effects.Hub.Publish(events.Event{Name: events.NameSystem, Payload: api.EventSystem{State: string(Draining)}})
		s.Effects.Hub.Close()
	}
	if s.sessions != nil {
		s.sessions.StopMetadata()
	}
	if s.stopWork != nil {
		s.stopWork()
	}
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			failures = append(failures, &StopError{Reason: ReasonDrain, Err: err})
		}
	}
	if s.worker != nil {
		// The joined worker is tested before the caller's own deadline: once it is
		// done no mutation capability remains, so an overrun stop still finishes and
		// releases the lock instead of a coin flip between two ready cases.
		select {
		case <-s.worker:
		default:
			select {
			case <-s.worker:
			case <-ctx.Done():
				// Exit without unwinding the lock while a mutation capability remains.
				return stop(&StopError{Reason: ReasonMetadataDrain, Err: ctx.Err()})
			}
		}
		if s.log != nil {
			s.log.Info("metadata worker stopped", "reason", "worker_terminal", "cause", s.workerStopReason(s.workerEnd))
			s.log.Info("metadata worker joined", "reason", "worker_joined")
		}
	}
	// Outbound closes before the runtime stops: the client ends its own flights
	// first, so no physical send is still running when the slot it borrowed is
	// given up.
	if s.Effects != nil && s.Effects.Outbound != nil {
		if err := s.Effects.Outbound.Close(ctx); err != nil {
			return stop(&StopError{Reason: ReasonOutboundDrain, Err: err})
		}
	}
	if s.Effects != nil && s.Effects.Jobs != nil {
		jobsCtx, cancel := stopDeadline(ctx)
		err := s.Effects.Jobs.Stop(jobsCtx)
		cancel()
		if err != nil {
			// A live attempt keeps its row and its key for the next start's
			// reconciliation, so the database is not closed under it.
			return stop(&StopError{Reason: ReasonAttemptsLive, Err: err})
		}
	}
	if s.db != nil {
		closed := make(chan error, 1)
		// Close has no context of its own: own the one call here and join it under
		// the same deadline, so a stuck close never outlives the stop.
		go func() { closed <- s.cfg.CloseStore(s.db) }()
		var closeErr error
		select {
		case closeErr = <-closed:
		case <-ctx.Done():
			return stop(&StopError{Reason: ReasonStoreCloseDeadline, Err: ctx.Err()})
		}
		if closeErr != nil {
			// The store did not close, so the lock stays held with it.
			return stop(&StopError{Reason: ReasonStoreClose, Err: closeErr})
		}
		s.db = nil
		if s.log != nil {
			s.log.Info("store closed", "reason", "store_closed")
		}
	}
	// The tail (crash monitor, log) closes before the lock, and the lock is the
	// last step of a stop: nothing that could still record outlives it.
	s.closeTail()
	s.releaseLock()
	s.closeErr = errors.Join(failures...)
	return s.closeErr
}

// releaseLock gives the data directory back: always the last step of a stop.
func (s *Serving) releaseLock() {
	if s.lock == nil {
		return
	}
	err := s.lock.Release()
	s.lock = nil
	if err != nil {
		if s.log != nil {
			s.log.Warn("lock release failed", "reason", "lock_release_failed")
		}
		return
	}
	if s.log != nil {
		s.log.Info("lock released", "reason", "lock_released")
	}
}

// closeTail closes the log and stops the crash monitor, in that order.
func (s *Serving) closeTail() {
	if s.stopCrash != nil {
		s.stopCrash()
		s.stopCrash = nil
	}
	if s.stopLog != nil {
		_ = s.stopLog()
		s.stopLog = nil
	}
}

// unwind is the failure path of a start: listener, worker, store, lock.
func (s *Serving) unwind() {
	if s.stopWork != nil {
		s.stopWork()
	}
	if s.worker != nil {
		<-s.worker
		s.worker = nil
	}
	_ = s.drainListener()
	if s.db != nil {
		if err := s.cfg.CloseStore(s.db); err == nil {
			s.db = nil
		}
	}
	if s.db == nil {
		s.releaseLock()
	}
	s.closeTail()
}

// drainListener stops the listener without waiting for a caller's deadline.
func (s *Serving) drainListener() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), DrainTimeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// stopReason names the cause a worker or attempt ended with, in the fixed
// vocabulary the shutdown record carries.
func stopReason(err error) string {
	switch {
	case err == nil:
		return "done"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

// openStore opens the one store under ctx, publishing the verified snapshot
// first when the file needs migrating.
func openStore(ctx context.Context, layout install.Layout, log *slog.Logger, publish func(context.Context, install.Layout, store.Pending) error) (*store.DB, error) {
	return store.Open(ctx, layout.DB(), store.Options{
		Log:           log,
		BeforeMigrate: func(ctx context.Context, pending store.Pending) error { return publish(ctx, layout, pending) },
	})
}

// prepareAccess arms setup while it is open and reads the facts the status
// route reports. The setup token itself is never logged.
func prepareAccess(ctx context.Context, db *store.DB, dir, envToken string, log *slog.Logger, hint string) (Backend, error) {
	b := Backend{Setup: auth.NewSetup(db, auth.NewHasher(), dir), Log: log}
	start := time.Now()
	open, err := b.Setup.Arm(ctx, envToken)
	if err != nil {
		return b, err
	}
	if open {
		b.SetupMs = time.Since(start).Milliseconds()
		log.Info("setup is open: create the first admin at /setup with the setup token", "reason", "setup_open", "command", hint)
	}
	err = db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		hashes, err := q.ListSchemaHashes(ctx)
		if err != nil {
			return err
		}
		if len(hashes) > 0 {
			b.SchemaVersion = hashes[len(hashes)-1].Version
		}
		inst, err := q.GetInstallation(ctx)
		b.RestoreEpoch = inst.RestoreEpoch
		return err
	})
	return b, err
}

// journalPresent reports whether the data directory holds a restore journal.
func journalPresent(l install.Layout) (bool, error) {
	_, err := os.Lstat(l.Journal())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}
