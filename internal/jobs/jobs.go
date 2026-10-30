// Package jobs runs the durable work internal/store records. It claims one row
// at a time per key, runs the kind's handler inside a bounded slot, records the
// transition in one Write and publishes it after the commit. A process that dies
// while an attempt runs leaves its row running: the next start moves it to
// reconciling and reconciles it before anything is claimed again, so a key is
// never released while a late attempt could still mutate.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Class is how hard a kind's work is: required work is never expired and its
// exhaustion stays visible in review; optional work may expire and fails.
type Class string

// Lane is the slot pool a kind runs in.
type Lane string

const (
	ClassRequired Class = "required"
	ClassOptional Class = "optional"

	LaneInteractive Lane = "interactive"
	LaneBackground  Lane = "background"
)

// The recorded bounds of one runtime.
const (
	// sweepEvery is how long a missed wake may wait: the sweep re-reads the due
	// rows and reconciles before the lanes claim again.
	sweepEvery = 30 * time.Second
	// overdueGrace is how long past its deadline a running attempt is recorded
	// as attempt_overdue; the row keeps its key either way.
	overdueGrace = 5 * time.Second
	// interactiveSlots is the fixed number of interactive attempts.
	interactiveSlots = 16
	// maxBackoff and backoffBase bound the retry delay.
	maxBackoff  = 600 * time.Second
	backoffBase = 10 * time.Second
	// maxAttempts is the schema's attempt ceiling.
	maxAttempts = 10
)

var (
	// ErrConfig: the runtime was handed a configuration it cannot run.
	ErrConfig = errors.New("jobs: config refused")
	// ErrStopped: the runtime is stopping, so it takes no new work.
	ErrStopped = errors.New("jobs: runtime is stopping")
	// ErrAttemptsLive: Stop reached its deadline with an attempt still running.
	// Its row stays running and reconciles on the next start.
	ErrAttemptsLive = errors.New("jobs: attempts still live at the stop deadline")
	// ErrNotStarted / ErrStarted guard the two lifecycle calls.
	ErrNotStarted = errors.New("jobs: runtime is not started")
	ErrStarted    = errors.New("jobs: runtime is already started")

	// errNotOpen: the transition matched no open row, so another writer moved it
	// first. The record keeps that apart from a store that refused the write.
	errNotOpen = errors.New("jobs: no open row to transition")
)

// Result is what a handler reports about one attempt. The zero value is not
// success: an Outcome a handler forgot to fill in is recorded as a failed
// attempt, never as done work.
type Result uint8

const (
	// ResultUnset: the handler reported no outcome. It is a defect in the
	// handler, so the attempt fails closed instead of looking successful.
	ResultUnset Result = iota
	// Succeeded: the work is done.
	Succeeded
	// Progress: part of the work is done. The row is requeued at once and its
	// attempt count starts again, so a long purge is never counted as failures.
	Progress
	// Retry: the attempt failed and another one may succeed.
	Retry
	// Unknown: the effect may have happened. The row goes to reconciling, keeps
	// its key, and no new attempt starts before reconciliation decided.
	Unknown
	// Failed: the attempt failed and no retry will help.
	Failed
)

// Outcome is one attempt's result, with the reason word and parameters the row
// records (reason_params is bounded to 1024 bytes).
type Outcome struct {
	Result     Result
	Reason     string
	Params     map[string]string
	RetryAfter time.Duration
}

// Event is what a committed transition publishes: the row as it now stands, so a
// consumer frames the committed state without reading the store again. Publish
// runs after the commit and never before it.
type Event struct {
	JobID       int64
	Kind        string
	Class       string
	Lane        string
	State       string
	Reason      string
	Params      map[string]string
	Attempts    int
	MaxAttempts int
	DueAt       int64
	CreatedAt   int64
	UpdatedAt   int64
	Revision    int64
}

// Config is the runtime's clock, identity, log and resource bounds. Now and
// After default to the process clock; Boot, when empty, is generated per start.
// Zone is the installation's schedule zone (nil is UTC): a daily rule keeps its
// wall clock there whatever the process clock does. Log receives one
// reason-coded record per runtime failure: a claim, sweep, schedule, transition
// or reconciliation that the store refused is never silent. The composition
// binds it to the installation's redacting logger.
type Config struct {
	Now             func() time.Time
	After           func(time.Duration) <-chan time.Time
	Log             func(reason string, kv ...any)
	Boot            string
	BackgroundSlots int
	Publish         func(Event)
	Zone            *time.Location
}

// Handler is one kind of work: how it is claimed, how long one attempt may run
// and what the attempt does. Reconcile nil means the kind is idempotent, so a
// reconciling row is simply requeued.
type Handler struct {
	Kind        string
	Class       Class
	Lane        Lane
	RunNow      bool
	MaxAttempts int
	Deadline    time.Duration
	Run         func(context.Context, *Attempt) Outcome
	Reconcile   func(context.Context, *Attempt) Outcome
}

// Attempt is what a handler sees of the claimed row. Wait frees the attempt's
// slot for a park call that needs none, and retakes it before returning, also
// when the attempt is cancelled.
type Attempt struct {
	JobID    int64
	Kind     string
	Params   map[string]string
	Attempts int
	Wait     func(ctx context.Context, park func(context.Context) error) error
}

// Runtime is the started scheduler: one sweep, one worker per slot.
type Runtime struct {
	db       *store.DB
	cfg      Config
	handlers map[string]Handler

	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	done     chan struct{} // closed when Start's goroutines all returned
	pools    map[Lane]*pool
	stopCh   chan struct{} // closed when Stop begins
	stopOnce sync.Once

	wakeMu sync.Mutex
	wake   chan struct{}

	publish func(Event)
	live    atomic.Int64
}

// New validates the handlers and the bounds. It starts nothing: Start binds the
// runtime to its context.
func New(db *store.DB, cfg Config, handlers []Handler) (*Runtime, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: no store", ErrConfig)
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("%w: no log: a refused claim, sweep or transition would be silent", ErrConfig)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Boot == "" {
		boot, err := newBoot()
		if err != nil {
			return nil, err
		}
		cfg.Boot = boot
	}
	if cfg.BackgroundSlots <= 0 {
		cfg.BackgroundSlots = max(1, runtime.GOMAXPROCS(0)/2)
	}
	byKind := make(map[string]Handler, len(handlers))
	for _, h := range handlers {
		if err := checkHandler(h); err != nil {
			return nil, err
		}
		if _, dup := byKind[h.Kind]; dup {
			return nil, fmt.Errorf("%w: kind %q declared twice", ErrConfig, h.Kind)
		}
		byKind[h.Kind] = h
	}
	return &Runtime{
		db:       db,
		cfg:      cfg,
		handlers: byKind,
		pools: map[Lane]*pool{
			LaneBackground:  newPool(cfg.BackgroundSlots),
			LaneInteractive: newPool(interactiveSlots),
		},
		stopCh:  make(chan struct{}),
		wake:    make(chan struct{}),
		publish: cfg.Publish,
	}, nil
}

// newBoot is one start's identity: 16 hex characters, so a row leased by an
// earlier process is recognisable as foreign.
func newBoot() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("%w: boot: %v", ErrConfig, err)
	}
	return hex.EncodeToString(b[:]), nil
}

// checkHandler refuses a handler the scheduler could not run.
func checkHandler(h Handler) error {
	if h.Kind == "" || len(h.Kind) > 64 {
		return fmt.Errorf("%w: handler kind %q", ErrConfig, h.Kind)
	}
	if h.Class != ClassRequired && h.Class != ClassOptional {
		return fmt.Errorf("%w: handler %s has class %q", ErrConfig, h.Kind, h.Class)
	}
	if h.Lane != LaneInteractive && h.Lane != LaneBackground {
		return fmt.Errorf("%w: handler %s has lane %q", ErrConfig, h.Kind, h.Lane)
	}
	if h.MaxAttempts < 1 || h.MaxAttempts > maxAttempts {
		return fmt.Errorf("%w: handler %s has %d attempts", ErrConfig, h.Kind, h.MaxAttempts)
	}
	if h.Deadline <= 0 {
		return fmt.Errorf("%w: handler %s has no deadline", ErrConfig, h.Kind)
	}
	if h.Run == nil {
		return fmt.Errorf("%w: handler %s has no run", ErrConfig, h.Kind)
	}
	return nil
}

// Boot is this start's lease identity.
func (r *Runtime) Boot() string { return r.cfg.Boot }

// Start recovers the rows an earlier process left running, reconciles every
// reconciling row and only then lets the lanes claim. It is called from the
// parent's ready transition, so nothing is dispatched before it.
func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return ErrStarted
	}
	runCtx, cancel := context.WithCancel(ctx)
	now := r.cfg.Now().Unix()
	// The built-in schedules are seeded once, before the first sweep can see
	// them, and a restart that finds one keeps its stored instant.
	if err := r.seedSchedules(runCtx, time.Unix(now, 0).UTC()); err != nil {
		cancel()
		return err
	}
	if _, err := r.db.RecoverForeignRunning(runCtx, r.cfg.Boot, now); err != nil {
		cancel()
		return err
	}
	// Reconciling before claiming: a row that may still be mutating keeps its
	// key until its reconciliation decided what happened.
	if err := r.reconcile(runCtx); err != nil {
		cancel()
		return err
	}
	r.cancel = cancel
	r.done = make(chan struct{})
	r.started = true
	var wg sync.WaitGroup
	wg.Add(r.cfg.BackgroundSlots + interactiveSlots + 1)
	for range r.cfg.BackgroundSlots {
		go func() { defer wg.Done(); r.work(runCtx, LaneBackground) }()
	}
	for range interactiveSlots {
		go func() { defer wg.Done(); r.work(runCtx, LaneInteractive) }()
	}
	go func() { defer wg.Done(); r.sweeper(runCtx) }()
	go func() { wg.Wait(); close(r.done) }()
	return nil
}

// Stop takes no new work, cancels every attempt and waits for them until ctx
// ends. A live attempt at the deadline is ErrAttemptsLive: its row stays
// running, keeps its key and reconciles on the next start.
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = false
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stopCh) })
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if n := r.live.Load(); n > 0 {
			return fmt.Errorf("%w: %d live", ErrAttemptsLive, n)
		}
		return ctx.Err()
	}
}

// Wake re-reads the due rows now instead of at the next sweep. It is called
// after the commit that made work claimable.
func (r *Runtime) Wake() {
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	close(r.wake)
	r.wake = make(chan struct{})
}

// wakeCh is the channel a worker waits on; a Wake closes it, so every worker
// sees it.
func (r *Runtime) wakeCh() <-chan struct{} {
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	return r.wake
}

// Slot takes one background slot for work that runs outside the lanes, from the
// same ceiling the background attempts use. A freed slot serves a waiting Slot
// or a Wait retake before a new claim. Stop refuses it.
func (r *Runtime) Slot(ctx context.Context) (func(), error) {
	if r.stopping() {
		return nil, ErrStopped
	}
	return r.pools[LaneBackground].take(ctx, r.stopCh)
}

func (r *Runtime) stopping() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// publishEvent hands a committed transition to the composition. Nil keeps the
// runtime silent, which only a build before the event stream is allowed to be.
func (r *Runtime) publishEvent(ev Event) {
	if r.publish != nil {
		r.publish(ev)
	}
}

// failed records one runtime failure with its reason word. The runtime's own
// failures are never silent, and never the caller's to guess from a state it
// did not observe.
func (r *Runtime) failed(reason string, kv ...any) {
	r.cfg.Log(reason, kv...)
}

// transitionFailed records a row's refused transition, with the row it was about.
func (r *Runtime) transitionFailed(job store.Job, target string, err error) {
	r.failed("transition_failed", "job", job.ID, "kind", job.Kind, "state", job.State, "target", target, "error", err)
}

// finish records a terminal state and publishes it after the commit. A reason
// the schema cannot hold (reason_params is bounded) must never lose the
// transition itself, so the write is retried without its parameters.
func (r *Runtime) finish(job store.Job, state, reason string, params map[string]string) {
	now := r.cfg.Now().Unix()
	written := params
	ok, err := r.db.FinishJob(context.Background(), job.ID, state, reason, params, now)
	if err != nil {
		written = nil
		ok, err = r.db.FinishJob(context.Background(), job.ID, state, reason, nil, now)
	}
	if err != nil {
		r.transitionFailed(job, state, err)
		return
	}
	if !ok {
		r.transitionFailed(job, state, errNotOpen)
		return
	}
	r.publishEvent(r.event(job, state, reason, written, job.Attempts, job.DueAt, now))
}

// requeue returns a row to queued with a new attempt count and due time.
func (r *Runtime) requeue(job store.Job, attempts int, dueAt int64, reason string, params map[string]string) {
	now := r.cfg.Now().Unix()
	written := params
	ok, err := r.db.RequeueJob(context.Background(), job.ID, attempts, dueAt, reason, params, now)
	if err != nil {
		written = nil
		ok, err = r.db.RequeueJob(context.Background(), job.ID, attempts, dueAt, reason, nil, now)
	}
	if err != nil {
		r.transitionFailed(job, store.JobQueued, err)
		return
	}
	if !ok {
		r.transitionFailed(job, store.JobQueued, errNotOpen)
		return
	}
	r.publishEvent(r.event(job, store.JobQueued, reason, written, attempts, dueAt, now))
	r.Wake()
}

// reconciling leaves the row owned and paused until its reconciliation runs.
func (r *Runtime) reconciling(job store.Job, reason string, params map[string]string) {
	now := r.cfg.Now().Unix()
	written := params
	ok, err := r.db.SetJobReconciling(context.Background(), job.ID, reason, params, now)
	if err != nil {
		written = nil
		ok, err = r.db.SetJobReconciling(context.Background(), job.ID, reason, nil, now)
	}
	if err != nil {
		r.transitionFailed(job, store.JobReconciling, err)
		return
	}
	if !ok {
		r.transitionFailed(job, store.JobReconciling, errNotOpen)
		return
	}
	r.publishEvent(r.event(job, store.JobReconciling, reason, written, job.Attempts, job.DueAt, now))
}

// publishClaim publishes the claim that made a claimed row running. The claim
// read its row inside its own Write, so that row already carries the committed
// revision: the event helper's +1 belongs to the transitions that publish a
// pre-write snapshot. The claim does not clear the row's recorded outcome, so
// the frame carries the row's own reason and params: the read model and the
// frame name the same state.
func (r *Runtime) publishClaim(job store.Job) {
	ev := r.event(job, store.JobRunning, job.ReasonCode, job.ReasonParams, job.Attempts, job.DueAt, job.UpdatedAt)
	ev.Revision = job.Revision
	r.publishEvent(ev)
}

// event is the published form of one committed transition: the row's own fields
// with the state, reason and instant this write recorded.
func (r *Runtime) event(job store.Job, state, reason string, params map[string]string, attempts int, dueAt, now int64) Event {
	return Event{
		JobID: job.ID, Kind: job.Kind, Class: job.Class, Lane: job.Lane,
		State: state, Reason: reason, Params: params,
		Attempts: attempts, MaxAttempts: job.MaxAttempts,
		DueAt: dueAt, CreatedAt: job.CreatedAt, UpdatedAt: now,
		Revision: job.Revision + 1,
	}
}

// backoff is the retry delay of attempt n: a bounded exponential spread that
// never falls below the handler's own RetryAfter.
func backoff(jobID int64, attempts int, after time.Duration) time.Duration {
	span := backoffBase << (attempts - 1)
	if span > maxBackoff || span <= 0 {
		span = maxBackoff
	}
	// A spread derived from the row and its attempt count: uniform enough to
	// keep retries of one batch apart, and identical for the same row in a test.
	jitter := time.Duration((jobID*1103515245 + int64(attempts)*12345) % int64(span+1))
	if jitter < after {
		return after
	}
	return jitter
}
