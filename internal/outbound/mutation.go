package outbound

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// A mutation is one wire request and nothing else: this client never repeats it,
// because only the peer knows whether it applied the last one. Its outcome is
// classified with the same words the jobs runtime records, so a caller never has
// to read a status to know whether a resend is safe.
//
// The classification is the durable contract:
//
//	not_sent       no byte left this process, so nothing can have been applied
//	(applied)      a 2xx: the peer applied it
//	rejected       a 4xx a further identical request cannot change
//	outcome_unknown anything else: the peer may already have applied it, so the
//	               caller reconciles before any resend

// isMutation reports whether method is one the peer applies. An empty method is
// GET to net/http, and only GET and HEAD change nothing a hop could repeat, so
// every other method is a mutation whatever the caller marked Safe.
func isMutation(method string) bool {
	switch {
	case method == "", strings.EqualFold(method, http.MethodGet), strings.EqualFold(method, http.MethodHead):
		return false
	}
	return true
}

// mutationStatus classifies a final response a mutation received. A 2xx applied
// it, a status a further identical request cannot change is rejected, and 408, 429
// and 5xx leave the outcome unknown, because the peer answered this attempt but
// the effect may already have landed.
func mutationStatus(status, attempts int, host string) *Error {
	params := map[string]string{"status": strconv.Itoa(status)}
	switch {
	case status >= 200 && status < 300:
		return nil
	case isRedirect(status):
		return &Error{Reason: ReasonRedirectDenied, Params: map[string]string{"host": host}, Attempts: attempts}
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return &Error{Reason: ReasonOutcomeUnknown, Params: params, Attempts: attempts, Unknown: true}
	}
	return &Error{Reason: ReasonRejected, Params: params, Attempts: attempts}
}

// mutationFailure classifies a mutation that produced no usable response. A dial
// that never reached the peer wrote nothing, so it is not_sent and safe to send
// again; an exchange that got far enough to write may have been applied, so it is
// unknown until a lookup decides.
func mutationFailure(err error, host string, attempts int) *Error {
	if wroteNothing(err) {
		return &Error{Reason: ReasonNotSent, Params: map[string]string{"host": host}, Attempts: attempts}
	}
	return &Error{
		Reason:   ReasonOutcomeUnknown,
		Params:   map[string]string{"host": host},
		Attempts: attempts,
		Unknown:  true,
	}
}

// wroteNothing reports an exchange that never reached the peer: the failure is the
// dial itself, which happens before any byte of the request is written.
func wroteNothing(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "dial"
	}
	return false
}
