package api

import (
	"errors"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/jobs"
)

// The error model's one admission mapping (architecture § Admission service).
// Refused interactive work is a bounded 429 with the refusal's reason word and
// the wait it asks for, never a 500 and never an effect.
//
// It is the only place the mapping is written: every operation that admits work
// returns api.AdmissionRefusal(err) and inherits it.
func AdmissionRefusal(err error) error {
	var refusal *jobs.AdmissionError
	if !errors.As(err, &refusal) {
		return err
	}
	return WithRetryAfter(NewProblem(429, refusal.Reason), retryAfterSeconds(refusal.RetryAfter))
}

// retryAfterSeconds is a retry wait as whole seconds, rounded up: a sub-second
// wait still asks for the next second instead of writing a Retry-After of 0.
func retryAfterSeconds(wait time.Duration) int {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}
