package outbound

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The recorded bounds of one safe read: its physical wire requests, the time those
// requests and the waits between them may take, and the largest wait a peer can
// ask for (architecture § Outbound contract).
const (
	// maxWireRequests bounds one call, redirect hops and retries included: a hop
	// is a wire request too, so a chain of them cannot extend the call.
	maxWireRequests = 3
	// flightBudget is how long a call's wire requests and its waits may take
	// together. It is read on the client's clock, so a proof advances it instead of
	// sleeping.
	flightBudget = 15 * time.Second
	// maxRetryAfter caps the wait a peer may ask for; a peer naming more than this
	// is not going to answer this call.
	maxRetryAfter = time.Hour
	// jitterBase and jitterCap bound the spread added to a retry wait, so callers
	// that failed together do not return together.
	jitterBase = 500 * time.Millisecond
	jitterCap  = 4 * time.Second
)

// retryable reports a status a safe read may repeat: the transient ones, which a
// later identical request can answer.
func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryWait is how long a safe read waits before its next wire request: what the
// peer asked for, or the spread this attempt earns, whichever is longer. A peer
// that named a wait longer than the cap is waited for the cap only.
func retryWait(header http.Header, attempts int, seed int64, now time.Time) time.Duration {
	wait := jitter(attempts, seed)
	if held := retryAfter(header, now); held > wait {
		wait = held
	}
	return wait
}

// retryAfter is the wait a response asked for, in seconds or as a date the client's
// own clock is measured against. A response that names none, or names one this
// client cannot read, asks for nothing beyond the retry's own spread.
func retryAfter(header http.Header, now time.Time) time.Duration {
	written := strings.TrimSpace(header.Get("Retry-After"))
	if written == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(written); err == nil {
		if seconds <= 0 {
			return 0
		}
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if at, err := http.ParseTime(written); err == nil {
		return min(max(at.Sub(now), 0), maxRetryAfter)
	}
	return 0
}

// jitter is the spread attempt n earns: at most jitterBase doubled per earlier
// attempt, never past jitterCap. seed makes it the same for calls that share a
// flight, so a proof can repeat a retry exactly.
func jitter(attempts int, seed int64) time.Duration {
	span := jitterCap
	if attempts > 0 {
		if doubled := jitterBase << (attempts - 1); doubled < span && doubled > 0 {
			span = doubled
		}
	}
	if span <= 0 {
		return 0
	}
	spread := time.Duration((seed + int64(attempts)*1_103_515_245) % int64(span+1))
	if spread < 0 {
		spread = -spread
	}
	return spread
}
