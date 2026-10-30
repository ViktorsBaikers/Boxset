package jobs

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The lanes. A lane owns a fixed number of slots, and a slot is one running
// attempt. A claim takes a slot without waiting; a slot handed back serves a
// waiting Slot or a Wait retake before any new claim takes it.

// benchSlowHooks is the slow control's seam. The benchslow build declares the
// attempt delay on *Runtime and every other build declares no method at all, so
// this assertion fails and the site does nothing. The delay lands after the
// claim that made the attempt running and before its handler, which is the
// window a measured job start covers.
type benchSlowHooks interface {
	benchAttemptDelay()
}

// pool is one lane's slot ceiling. waits counts the takers that must be served
// before a claim: a Slot call and a Wait retake, both of which are work that
// already exists.
type pool struct {
	mu    sync.Mutex
	free  int
	waits int
	wake  chan struct{} // closed and replaced by every release
}

func newPool(n int) *pool {
	return &pool{free: n, wake: make(chan struct{})}
}

// take waits for a slot and returns its idempotent release.
func (p *pool) take(ctx context.Context, stop <-chan struct{}) (func(), error) {
	p.mu.Lock()
	p.waits++
	p.mu.Unlock()
	defer p.served()
	return p.acquire(ctx, stop)
}

// acquire takes a slot inside a take, so waits stays counted over the whole wait.
func (p *pool) acquire(ctx context.Context, stop <-chan struct{}) (func(), error) {
	for {
		p.mu.Lock()
		if p.free > 0 {
			p.free--
			p.mu.Unlock()
			return p.release(), nil
		}
		wait := p.wake
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, ErrStopped
		}
	}
}

// claim takes a slot for a new attempt, and only while nothing is waiting for
// one: a freed slot is work that already exists before it is new work.
func (p *pool) claim() (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waits > 0 || p.free == 0 {
		return nil, false
	}
	p.free--
	return p.release(), true
}

func (p *pool) served() {
	p.mu.Lock()
	p.waits--
	p.mu.Unlock()
}

func (p *pool) release() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.free++
			close(p.wake)
			p.wake = make(chan struct{})
			p.mu.Unlock()
		})
	}
}

// work is one lane goroutine: claim a due row, run it, and idle until something
// is due again. A claim that finds nothing writes nothing.
func (r *Runtime) work(ctx context.Context, lane Lane) {
	p := r.pools[lane]
	for {
		if ctx.Err() != nil {
			return
		}
		release, ok := p.claim()
		if !ok {
			if !r.idle(ctx) {
				return
			}
			continue
		}
		job, found, err := r.db.ClaimJob(ctx, string(lane), r.cfg.Boot, r.cfg.Now().Unix())
		if err != nil || !found {
			release()
			if err != nil && ctx.Err() == nil {
				// A claim the store refused is a runtime failure, not an idle lane.
				r.failed("claim_failed", "lane", string(lane), "error", err)
			}
			if ctx.Err() != nil {
				return
			}
			if !r.idle(ctx) {
				return
			}
			continue
		}
		r.run(ctx, p, release, job)
	}
}

// idle waits for a wake or the next sweep. It reports false once the runtime is
// stopping, because the goroutine must then return.
func (r *Runtime) idle(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-r.wakeCh():
	case <-r.cfg.After(sweepEvery):
	}
	return true
}

// sweeper is the bounded fallback: every sweep re-reads the due rows, so a wake
// that never arrived costs at most one sweep period.
func (r *Runtime) sweeper(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.cfg.After(sweepEvery):
		}
		// Every failure of a sweep is recorded where it happened, so the next
		// sweep repeats a symptom that is already visible.
		_ = r.sweepOnce(ctx)
	}
}

// sweepOnce records what a sweep does. An idle sweep writes nothing.
func (r *Runtime) sweepOnce(ctx context.Context) error {
	now := r.cfg.Now()
	// A due schedule fires whatever the job table holds, so the sweep reads it
	// before deciding whether there is queued work to recover.
	if err := r.fireSchedules(ctx, now); err != nil {
		// One refused schedule does not stop the job sweep: the reason is recorded
		// here and the next sweep repeats the symptom.
		_ = r.sweepFailed(ctx, err)
	}
	work, err := r.db.SweepPending(ctx, now.Unix())
	if err != nil {
		return r.sweepFailed(ctx, err)
	}
	if !work {
		return nil
	}
	if _, err := r.db.ExpireOptionalJobs(ctx, now.Unix()); err != nil {
		return r.sweepFailed(ctx, err)
	}
	if err := r.markOverdue(ctx, now.Unix()); err != nil {
		return r.sweepFailed(ctx, err)
	}
	if err := r.reconcile(ctx); err != nil {
		// The reconcile site records it; a sweep ends here.
		return err
	}
	return nil
}

// sweepFailed records one sweep the store refused, with the step's error.
func (r *Runtime) sweepFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		r.failed("sweep_failed", "error", err)
	}
	return err
}

// markOverdue records attempt_overdue on a running attempt that passed its
// deadline and the grace. The row keeps its key: only reconciliation releases it.
func (r *Runtime) markOverdue(ctx context.Context, now int64) error {
	jobs, err := r.db.RunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		h, ok := r.handlers[job.Kind]
		if !ok || job.StartedAt == nil {
			continue
		}
		overdue := *job.StartedAt + int64(h.Deadline/time.Second) + int64(overdueGrace/time.Second)
		if now < overdue {
			continue
		}
		params := map[string]string{"deadline_s": strconv.FormatInt(int64(h.Deadline/time.Second), 10)}
		if _, err := r.db.MarkJobOverdue(ctx, job.ID, params, now); err != nil {
			return err
		}
	}
	return nil
}

// reconcile decides every reconciling row before any of its keys is claimable.
// A kind without a Reconcile function is idempotent, so its row is requeued.
func (r *Runtime) reconcile(ctx context.Context) error {
	jobs, err := r.db.ReconcilingJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.failed("reconcile_failed", "error", err)
		}
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, ok := r.handlers[job.Kind]
		if !ok {
			r.finish(job, r.unrecoverableState(job), "handler_missing", nil)
			continue
		}
		if h.Reconcile == nil {
			// Idempotent kind: the row is requeued with the reason that put it
			// here, so the requeue keeps its evidence.
			r.requeue(job, job.Attempts, r.cfg.Now().Unix(), job.ReasonCode, job.ReasonParams)
			continue
		}
		p := r.pools[LaneBackground]
		release, err := p.take(ctx, r.stopCh)
		if err != nil {
			return err
		}
		actx, cancel := context.WithTimeout(ctx, h.Deadline)
		attempt, settle := r.heldAttempt(job, p, release)
		out := h.Reconcile(actx, attempt)
		settle()
		// The attempt's own end is read before this loop gives its context back:
		// a context cancelled here would make every decision look like it never
		// ran, and the row would stay reconciling for good.
		ended := actx.Err()
		cancel()
		if ended != nil {
			continue
		}
		r.recordOutcome(job, out)
	}
	return nil
}

// run executes one claimed attempt inside its slot.
func (r *Runtime) run(ctx context.Context, p *pool, release func(), job store.Job) {
	h, ok := r.handlers[job.Kind]
	if !ok {
		r.finish(job, r.unrecoverableState(job), "handler_missing", nil)
		release()
		return
	}
	r.live.Add(1)
	defer r.live.Add(-1)
	actx, cancel := context.WithTimeout(ctx, h.Deadline)
	defer cancel()
	if slow, ok := any(r).(benchSlowHooks); ok {
		slow.benchAttemptDelay()
	}
	attempt, settle := r.heldAttempt(job, p, release)
	// The claim is a transition a subscriber acts on, so it publishes one frame
	// for the row it committed: after the slow control's seam and immediately
	// before the handler, so a measured start has a frame to sample and the
	// control's delay falls inside the window that measures it.
	r.publishClaim(job)
	out := h.Run(actx, attempt)
	settle()
	// An attempt that ended because its own deadline passed or the runtime is
	// stopping writes nothing: the row stays running, keeps its key and is
	// reconciled by the next start.
	if actx.Err() != nil {
		return
	}
	r.recordOutcome(job, out)
}

// heldAttempt builds the Attempt a handler sees and the settle that gives its
// slot back, also when the handler never called Wait.
func (r *Runtime) heldAttempt(job store.Job, p *pool, release func()) (*Attempt, func()) {
	held := release
	a := &Attempt{
		JobID:    job.ID,
		Kind:     job.Kind,
		Params:   job.Params,
		Attempts: job.Attempts,
		Wait: func(wctx context.Context, park func(context.Context) error) error {
			// The slot goes back before the park call, which needs none, and is
			// taken again before the handler continues, also when it was cancelled.
			if held != nil {
				held()
				held = nil
			}
			perr := park(wctx)
			next, terr := p.take(wctx, r.stopCh)
			held = next
			if terr != nil {
				return terr
			}
			return perr
		},
	}
	return a, func() {
		if held != nil {
			held()
			held = nil
		}
	}
}

// recordOutcome turns one attempt's report into the row's next state.
func (r *Runtime) recordOutcome(job store.Job, out Outcome) {
	now := r.cfg.Now()
	switch out.Result {
	case Succeeded:
		r.finish(job, store.JobSucceeded, out.Reason, out.Params)
	case Progress:
		// A batch loop that is still working: requeued at once with its attempt
		// count reset, so a long purge never exhausts the budget.
		r.requeue(job, 0, now.Unix(), "progress", nil)
	case Retry:
		if job.Attempts < job.MaxAttempts {
			r.requeue(job, job.Attempts, now.Add(backoff(job.ID, job.Attempts, out.RetryAfter)).Unix(), reasonOr(out.Reason, "retry"), out.Params)
			return
		}
		r.finish(job, r.unrecoverableState(job), "attempts_exhausted", nil)
	case Unknown:
		r.reconciling(job, reasonOr(out.Reason, "outcome_unknown"), out.Params)
	case ResultUnset:
		// A handler that reported nothing is a defect: fail closed, never success.
		r.finish(job, r.unrecoverableState(job), "outcome_invalid", nil)
	default:
		r.finish(job, r.unrecoverableState(job), reasonOr(out.Reason, "handler_failed"), out.Params)
	}
}

// unrecoverableState is where work the runtime cannot finish is left: required
// work stays visible in review with its key held, optional work fails.
func (r *Runtime) unrecoverableState(job store.Job) string {
	if job.Class == store.JobOptional {
		return store.JobFailed
	}
	return store.JobReview
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}
