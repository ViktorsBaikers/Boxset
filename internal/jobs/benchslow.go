//go:build benchslow

package jobs

// The slow control of the effects timing protocol. lane.go asserts this seam
// untagged, beside the attempt it delays; only this build declares the method,
// so an ordinary binary has no delay at all.
//
// The delay lands inside the measured window: after the claim that made the row
// running and before the handler that does its work, which is the span a
// measured admitted-job start covers. It delays every attempt the same way, and
// it copies nothing from the store: the probe the effects harness samples with
// stays the store package's own.

import "time"

// benchSlowDelay is the recorded control delay: half again the 100 ms budget, so
// a control series is unmistakably past it.
const benchSlowDelay = 150 * time.Millisecond

// A signature drift would make lane.go's assertion fail and the control silently
// lose its delay; this one fails the control build instead.
var _ benchSlowHooks = (*Runtime)(nil)

// benchAttemptDelay holds one attempt at the seam. It takes no lock and touches
// no store, so the control measures the wait it was given and nothing else.
func (r *Runtime) benchAttemptDelay() { time.Sleep(benchSlowDelay) }
