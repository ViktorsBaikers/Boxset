// Package outbound is the one bounded outbound HTTP client. A request is one
// wire exchange: its dial resolves through the injected resolver, refuses every
// address class the request's origin does not authorize and repeats that refusal
// on the address the socket is about to reach, and every redirect hop is a new
// destination validated the same way. A credential travels only to the exact
// origin that owns it. One call spends one budget, whatever it does around the
// wire: it merges with the reads that ask for the same thing, retries only a
// transient answer, and never sends a mutation twice.
package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/ingest"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

const (
	// maxBodyBytes bounds every response body after decoding, the ADR 0004 §6
	// body class cap that ingest.Body enforces while reading.
	maxBodyBytes = 16 << 20
	// maxSafeBody bounds a safe op's own request body, the ADR 0004 §6 ordinary
	// JSON request cap: an op the caller marked Safe that carries more is refused
	// before any key.
	maxSafeBody = 1 << 20
	// maxFlights is how many reads this client may have in flight at once. A call
	// over it is refused at once instead of joining a queue.
	maxFlights = 32
	// queueRetryAfter is how long a caller refused for a full client should wait.
	queueRetryAfter = 2 * time.Second
	// userAgentSuffix identifies this project to a peer. The user agent names a
	// version and the project URL, never a host, a user or an installation.
	userAgentSuffix = "(+https://github.com/ViktorsBaikers/Boxset)"
)

// attemptTimeout bounds one physical exchange, its body read included: a peer that
// stalls after its headers cannot hold the shared slot past the time one attempt
// may take. It is a var only so a test can shorten the wait; nothing outside this
// package sets it.
var attemptTimeout = 15 * time.Second

// Reason tokens. Each names one class of refused or failed call; the reason and
// its parameters are the stable contract a caller switches on.
const (
	// ReasonDialDenied is an address the request's origin does not authorize,
	// refused before the connection it would have made.
	ReasonDialDenied = "dial_denied"
	// ReasonRedirectDenied is a hop refused: a downgrade, a credential in the
	// target's query, a non-HTTP scheme, or any redirect of a mutation.
	ReasonRedirectDenied = "redirect_denied"
	// ReasonHostForbidden is a host matched against a configured digest, refused
	// before it is resolved.
	ReasonHostForbidden = "host_forbidden"
	// ReasonBudgetExhausted is a bound reached with nothing further sent: this
	// call's wire requests, its elapsed budget, or the limiter cache.
	ReasonBudgetExhausted = "budget_exhausted"
	// ReasonAuthFailed is a safe read that could authenticate nothing.
	ReasonAuthFailed = "auth_failed"
	// ReasonRejected is a status a further identical request cannot change.
	ReasonRejected = "rejected"
	// ReasonOutcomeUnknown is an outcome the peer may already have applied.
	ReasonOutcomeUnknown = "outcome_unknown"
	// ReasonNotSent is a mutation no byte of which left this process.
	ReasonNotSent = "not_sent"
	// ReasonClientClosed is a call refused because the client is closing.
	ReasonClientClosed = "client_closed"
	// ReasonQueueFull is a call refused because every flight this client may own
	// is running.
	ReasonQueueFull = "queue_full"
	// ReasonBodyTooLarge is a safe op whose own body is past its cap.
	ReasonBodyTooLarge = "body_too_large"
)

// paramCredentialUnavailable is the fixed parameter of a credential the server
// could not produce. The reader's own error is never rendered because it may hold
// the secret.
const paramCredentialUnavailable = "unavailable"

// Origin is one approved destination: the scheme, the canonical hostname and the
// effective port.
type Origin struct {
	Scheme string
	Host   string
	Port   int
}

// Credential is a secret attached to a request only when the request's origin is
// the target's. Value reads the current secret; Header and Query name where it
// travels, and Query also has to match the peer's convention because it becomes
// part of the request URL.
type Credential struct {
	Header string
	Query  string
	Value  func(context.Context) ([]byte, error)
}

// Target is one destination and what may travel to it. Private admits loopback,
// private, unique-local and carrier-grade NAT addresses, and only for Origin
// itself: an address is never admitted by a suffix, a range or a neighbouring
// port.
type Target struct {
	ID         string
	Origin     Origin
	Private    bool
	Credential *Credential
}

// Op is one outbound call: where it goes and how it is scoped. Target and URL are
// required; a Safe op whose method only reads may follow validated redirect hops
// and merges with the reads that ask for the same thing, and every other op is one
// wire request, whatever Safe says.
type Op struct {
	// Name identifies the operation in the merge key: two calls of different
	// operations never share a flight, whatever else they have in common.
	Name   string
	Target *Target
	Method string
	URL    *url.URL
	Body   string
	Safe   bool

	// The scope a read merges on: two reads of this destination and operation
	// share one wire request only when every one of these is equal.
	Representation     string
	Language           string
	ConfigRevision     int64
	CredentialIdentity string

	// Budget is this op's own allowance. An empty Indexer spends the host's.
	Budget Budget
	// Throttle classifies a Retry-After: ScopeHost makes every op to the host
	// wait, ScopeOwn only this op's own key. Nil is ScopeOwn.
	Throttle func(status int, header http.Header) Scope
}

// Config is the client's injected environment. Slot is required: a send path
// that took no shared slot would escape the aggregate ceiling, so New panics
// rather than build one. Now and After are the one clock every budget, hold and
// backoff is read and waited on, so a proof advances time instead of sleeping.
type Config struct {
	// Resolver resolves a hop's hostname. A nil field means the host resolver.
	Resolver Resolver
	// Slot takes one shared resource permit per physical send and returns its
	// release.
	Slot func(context.Context) (func(), error)
	// Now reads the client's clock; nil means the process clock.
	Now func() time.Time
	// After waits on the client's clock; nil means the process clock.
	After func(time.Duration) <-chan time.Time
	// Version is the client's own version in the user agent.
	Version string
	// Transport wraps the transport built for each hop; the record/replay
	// harness installs itself here.
	Transport func(http.RoundTripper) http.RoundTripper
	// ForbiddenHosts holds the SHA-256 digests of the hosts this client refuses
	// outright, a hostname and each of its domain suffixes. Only digests are
	// configured, so a forbidden name appears nowhere in the source.
	ForbiddenHosts []string
}

// flight is one merged safe read: the goroutine that spends its budget, the
// permits of the calls that joined it, and the one result they all receive.
type flight struct {
	cancel context.CancelFunc
	done   chan struct{}
	// guarded by the client's mutex
	permits  int
	response Response
	err      error
}

// Client sends outbound requests. One client is shared by a composition: it holds
// the limiter cache, the merged flights and the lifetime every flight derives
// from, so one Close ends them all.
type Client struct {
	resolver  Resolver
	slot      func(context.Context) (func(), error)
	version   string
	wrap      func(http.RoundTripper) http.RoundTripper
	forbidden map[string]struct{}
	now       func() time.Time
	after     func(time.Duration) <-chan time.Time
	budget    *budget
	permits   chan struct{}

	lifetime    context.Context
	endLifetime context.CancelFunc

	mu      sync.Mutex
	closed  bool
	flights map[[sha256.Size]byte]*flight
	group   sync.WaitGroup
}

// New builds a client from its injected environment. A nil Slot is a
// programmer error and panics: no send path may exist without the shared slot.
func New(c Config) *Client {
	if c.Slot == nil {
		panic("outbound: Slot is required")
	}
	client := &Client{
		resolver:  c.Resolver,
		slot:      c.Slot,
		version:   c.Version,
		wrap:      c.Transport,
		forbidden: make(map[string]struct{}, len(c.ForbiddenHosts)),
		now:       c.Now,
		after:     c.After,
		permits:   make(chan struct{}, maxFlights),
		flights:   make(map[[sha256.Size]byte]*flight),
	}
	if client.resolver == nil {
		client.resolver = net.DefaultResolver
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.after == nil {
		client.after = time.After
	}
	for _, digest := range c.ForbiddenHosts {
		client.forbidden[strings.ToLower(strings.TrimSpace(digest))] = struct{}{}
	}
	client.lifetime, client.endLifetime = context.WithCancel(context.Background())
	client.budget = newBudget()
	return client
}

// Response is a successful call: the peer's status, its header and the body read
// under the ingest cap. Header is this caller's own copy and Body is immutable, so
// neither can be changed under another caller.
type Response struct {
	Status int
	Header http.Header
	Body   string
}

// Error is a refused or failed call. Reason is one of the tokens above and Params
// holds the decision's inputs, never credential bytes. Attempts is the number of
// wire requests this client started for the op, so a refusal decided before the
// first byte is written is 0 for the attempt it refused. Unknown marks an outcome
// the peer may already have applied, which only a mutation can have.
type Error struct {
	Reason   string
	Params   map[string]string
	Attempts int
	Unknown  bool
}

// Error renders the reason and its parameters through the shared redactor, so a
// URL, header or credential sentinel in a parameter reaches a log as a marker.
func (e *Error) Error() string {
	keys := make([]string, 0, len(e.Params))
	for key := range e.Params {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var message strings.Builder
	message.WriteString("outbound: ")
	message.WriteString(e.Reason)
	for _, key := range keys {
		fmt.Fprintf(&message, " %s=%s", key, e.Params[key])
	}
	if e.Attempts > 0 {
		fmt.Fprintf(&message, " attempts=%d", e.Attempts)
	}
	if e.Unknown {
		message.WriteString(" unknown")
	}
	return redact.String(message.String())
}

// wireResponse is one physical exchange whose body was already read under the
// ingest cap and closed.
type wireResponse struct {
	status int
	header http.Header
	body   string
}

// Do runs one op. A Safe read merges with the reads that ask for the same thing
// and spends one budget of at most maxWireRequests wire requests across its
// validated hops and retries; a mutation is one wire request the caller's own
// context and slot own. park is the caller's slot-releasing wait seam
// (jobs.Attempt.Wait): the call waits on it, so a slot is never held while
// nothing is sent. Nil runs the blocking call directly.
func (c *Client) Do(ctx context.Context, op Op, park func(context.Context, func(context.Context) error) error) (Response, error) {
	if refusal := c.admission(); refusal != nil {
		return Response{}, refusal
	}
	snapshot := snapshotOp(op)
	if snapshot.Safe && len(snapshot.Body) > maxSafeBody {
		return Response{}, &Error{
			Reason: ReasonBodyTooLarge,
			Params: map[string]string{"limit": "body", "max": strconv.Itoa(maxSafeBody)},
		}
	}
	// Safe is the caller's own claim that the op changes nothing, and the method is
	// the second authority over it: only a read merges with the calls that ask for
	// the same thing and repeats a transient answer, and every other op is one wire
	// request, whatever Safe says. A write a caller marked safe would otherwise be
	// resent and collapse with another caller's write.
	if !snapshot.Safe || isMutation(snapshot.Method) {
		return c.send(ctx, snapshot, park)
	}
	return c.merge(ctx, snapshot, park)
}

// Close stops admission and ends the flights this client owns. Every later call,
// safe or mutation, is refused client_closed before it takes a key, a permit, a
// budget or a dial. It is idempotent, and it waits only for the flights it owns: a
// caller's own mutation, its park and its slot reacquisition belong to the runtime
// that started it.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.endLifetime()
	done := make(chan struct{})
	go func() {
		c.group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// admission is the one refusal every call starts with: nothing is admitted once
// the client began closing.
func (c *Client) admission() *Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return &Error{Reason: ReasonClientClosed, Attempts: 0}
	}
	return nil
}

// closed reports whether Close began, for a call already in its own work.
func (c *Client) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// interrupted is why a call ended while it was not sending: the client is closing,
// or the caller's own context ended.
func (c *Client) interrupted(ctx context.Context, attempts int) error {
	if c.stopped() {
		return &Error{Reason: ReasonClientClosed, Attempts: attempts}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return &Error{Reason: ReasonClientClosed, Attempts: attempts}
}

// send runs one caller-owned mutation: one wire request, its budget waited on
// through the caller's park, and no retry whatever the outcome. The classification
// says whether a resend would be safe.
//
// The whole send is one park window. A caller inside an attempt already holds that
// attempt's slot, and the exchange takes its own permit from the same pool, so a
// send that kept the attempt's permit while asking the pool for another would wait
// for itself on a one-slot runtime: the park hands the attempt's slot back, the
// send spends and exchanges on the permit it takes instead, and the park retakes
// the attempt's own before returning.
func (c *Client) send(ctx context.Context, op Op, park func(context.Context, func(context.Context) error) error) (Response, error) {
	hop := op.URL
	if c.isForbidden(hop.Hostname()) {
		return Response{}, &Error{Reason: ReasonHostForbidden, Params: map[string]string{"host": canonicalHost(hop.Hostname())}}
	}
	deliver := func(sctx context.Context) (Response, error) {
		if err := c.spend(sctx, op, hop, time.Time{}, 0, nil); err != nil {
			return Response{}, err
		}
		response, err := c.exchange(sctx, op, hop, approvedOrigin(op.Target.Origin), 0)
		if err != nil {
			var refusal *Error
			if errors.As(err, &refusal) {
				return Response{}, refusal
			}
			return Response{}, mutationFailure(err, canonicalHost(hop.Hostname()), 1)
		}
		if refusal := mutationStatus(response.status, 1, canonicalHost(hop.Hostname())); refusal != nil {
			return Response{}, refusal
		}
		return Response{Status: response.status, Header: response.header, Body: response.body}, nil
	}
	if park == nil {
		return deliver(ctx)
	}
	var response Response
	if err := park(ctx, func(pctx context.Context) error {
		var err error
		response, err = deliver(pctx)
		return err
	}); err != nil {
		return Response{}, err
	}
	return response, nil
}

// merge runs one safe read. It takes one client permit without waiting, joins the
// flight that already asks for the same thing or starts it, and then waits on the
// caller's park for the one result. The permit is released when that flight
// settles, so a caller that gave up early never leaves a flight unaccounted for.
func (c *Client) merge(ctx context.Context, op Op, park func(context.Context, func(context.Context) error) error) (Response, error) {
	select {
	case c.permits <- struct{}{}:
	default:
		return Response{}, &Error{
			Reason: ReasonQueueFull,
			Params: map[string]string{"retry_after_s": strconv.Itoa(int(queueRetryAfter / time.Second))},
		}
	}
	key := flightKey(op)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.permits
		return Response{}, &Error{Reason: ReasonClientClosed}
	}
	f, joined := c.flights[key]
	if !joined {
		f = c.start(key, op, jitterSeed(key))
	}
	f.permits++
	c.mu.Unlock()

	wait := func(wctx context.Context) error {
		select {
		case <-f.done:
			return nil
		case <-wctx.Done():
			return wctx.Err()
		}
	}
	settled := wait
	if park != nil {
		settled = func(wctx context.Context) error { return park(wctx, wait) }
	}
	if err := settled(ctx); err != nil {
		// The caller gave up. Its permit stays with the flight, which keeps
		// running for the callers that are still joined to it.
		return Response{}, err
	}
	return f.response, f.err
}

// start runs one flight on its own goroutine, derived from the client's lifetime
// so Close ends it. The caller holds the client's mutex.
func (c *Client) start(key [sha256.Size]byte, op Op, seed int64) *flight {
	ctx, cancel := context.WithTimeout(c.lifetime, flightBudget)
	f := &flight{cancel: cancel, done: make(chan struct{})}
	c.flights[key] = f
	c.group.Add(1)
	go func() {
		defer c.group.Done()
		response, err := c.call(ctx, op, seed, c.now().Add(flightBudget))
		// The flight settles under the mutex, so a call that joins it either sees
		// it running and adds its permit, or finds it gone and starts its own.
		c.mu.Lock()
		delete(c.flights, key)
		f.response, f.err = response, err
		permits := f.permits
		f.permits = 0
		close(f.done)
		c.mu.Unlock()
		for range permits {
			<-c.permits
		}
		cancel()
	}()
	return f
}

// call is the whole safe read inside one flight: at most maxWireRequests wire
// requests across its hops and retries, and the waits between them spending the
// same deadline.
func (c *Client) call(ctx context.Context, op Op, seed int64, deadline time.Time) (Response, error) {
	approved := approvedOrigin(op.Target.Origin)
	credentialQuery := ""
	if op.Target.Credential != nil {
		credentialQuery = op.Target.Credential.Query
	}
	hop := op.URL
	sent := 0
	for {
		if c.isForbidden(hop.Hostname()) {
			return Response{}, &Error{
				Reason:   ReasonHostForbidden,
				Params:   map[string]string{"host": canonicalHost(hop.Hostname())},
				Attempts: sent,
			}
		}
		if err := c.spend(ctx, op, hop, deadline, sent, nil); err != nil {
			return Response{}, err
		}
		response, err := c.exchange(ctx, op, hop, approved, sent)
		sent++
		if err != nil {
			var refusal *Error
			if errors.As(err, &refusal) {
				return Response{}, refusal
			}
			var refused *ingest.Error
			if errors.As(err, &refused) {
				// A body this client refused is not a transient answer: another
				// identical request reads the same bytes.
				return Response{}, err
			}
			// Only a failure before the peer could answer is worth another
			// attempt: a dial that never reached it, or one that never answered in
			// time. An exchange that wrote the request and then failed is over,
			// because a repeat would ask the peer twice for one read.
			if !retryableFailure(err) || sent >= maxWireRequests {
				return Response{}, exchangeFailure(canonicalHost(hop.Hostname()), sent)
			}
			if refusal := c.backoff(op, hop, 0, nil, sent, seed, deadline); refusal != nil {
				return Response{}, refusal
			}
			continue
		}
		switch {
		case response.status >= 200 && response.status < 300:
			return Response{Status: response.status, Header: response.header, Body: response.body}, nil
		case isRedirect(response.status):
			if !op.Safe || isMutation(op.Method) {
				return Response{}, &Error{
					Reason:   ReasonRedirectDenied,
					Params:   map[string]string{"host": canonicalHost(hop.Hostname())},
					Attempts: sent,
				}
			}
			if sent >= maxWireRequests {
				return Response{}, wireBudget(sent)
			}
			next, refusal := nextHop(hop, response.header, sent, credentialQuery)
			if refusal != nil {
				return Response{}, refusal
			}
			hop = next
		case retryable(response.status):
			if sent >= maxWireRequests {
				return Response{}, wireBudget(sent)
			}
			if refusal := c.backoff(op, hop, response.status, response.header, sent, seed, deadline); refusal != nil {
				return Response{}, refusal
			}
		default:
			return Response{}, statusError(response.status, false, sent)
		}
	}
}

// wireBudget is the refusal of a call whose wire requests are all spent.
func wireBudget(attempts int) *Error {
	return budgetExhausted(map[string]string{"limit": "wire_requests", "max": strconv.Itoa(maxWireRequests)}, attempts)
}

// backoff holds the key the next attempt charges until the wait this answer earned
// has passed, so the wait happens once, in the next attempt's own budget. A wait
// that would outlast the call's deadline is the call's last word: nothing further
// is sent.
func (c *Client) backoff(op Op, hop *url.URL, status int, header http.Header, attempts int, seed int64, deadline time.Time) *Error {
	at := c.now()
	wait := retryWait(header, attempts, seed, at)
	if wait <= 0 {
		return nil
	}
	if at.Add(wait).After(deadline) {
		params := map[string]string{"budget": flightBudget.String(), "wait": wait.String()}
		if asked := retryAfter(header, at); asked > 0 {
			params["retry_after"] = asked.String()
		}
		return budgetExhausted(params, attempts)
	}
	kind, id := holdKey(op, hop, status, header)
	c.budget.hold(kind, id, at.Add(wait))
	return nil
}

// holdKey is the key a Retry-After holds: the host's shared ceiling when the op
// says the throttle is host-wide, and the op's own key otherwise.
func holdKey(op Op, hop *url.URL, status int, header http.Header) (string, string) {
	host := canonicalHost(hop.Hostname())
	if op.Throttle != nil && op.Throttle(status, header) == ScopeHost {
		return keyCeiling, host
	}
	if op.Budget.Indexer != "" {
		return keyIndexer, op.Budget.Indexer
	}
	return keyHost, host
}

// spend waits for the budget the next physical request at hop spends: its own key
// and the host's ceiling, whichever waits longer. The wait covers a hold a peer
// asked for, so a Retry-After is never waited twice. A deadline that the wait
// would pass is budget_exhausted, and a caller that gave up instead is its own
// reason. The wait's accounting ends with the wait, whatever the caller does next:
// the token stays spent, and the key is reclaimable again once nothing waits on it.
func (c *Client) spend(ctx context.Context, op Op, hop *url.URL, deadline time.Time, attempts int, park func(context.Context, func(context.Context) error) error) error {
	delay, release, refusal := c.charge(op, hop, attempts)
	if refusal != nil {
		return refusal
	}
	defer release()
	if delay > 0 && !deadline.IsZero() && !c.now().Add(delay).Before(deadline) {
		return budgetExhausted(map[string]string{"budget": flightBudget.String(), "wait": delay.String()}, attempts)
	}
	pause := func(wctx context.Context) error { return c.wait(wctx, delay) }
	waited := pause
	if park != nil {
		waited = func(wctx context.Context) error { return park(wctx, pause) }
	}
	if err := waited(ctx); err != nil {
		return c.interrupted(ctx, attempts)
	}
	return nil
}

// charge reserves one token of the op's own key and of the host's ceiling at hop,
// and returns the longer of the two waits with the release of that wait's
// accounting. A refusal keeps the attempt count it was decided in.
func (c *Client) charge(op Op, hop *url.URL, attempts int) (time.Duration, func(), *Error) {
	at := c.now()
	host := canonicalHost(hop.Hostname())
	kind, id, perSecond, burst := keyHost, host, hostPerSecond, hostBurst
	if op.Budget.Indexer != "" {
		kind, id, perSecond, burst = keyIndexer, op.Budget.Indexer, indexerPerSecond, indexerBurst
	}
	delay, releaseOwn, refusal := c.budget.reserve(kind, id, at, perSecond, burst)
	if refusal != nil {
		refusal.Attempts = attempts
		return 0, nil, refusal
	}
	ceiling, releaseCeiling, refusal := c.budget.reserve(keyCeiling, host, at, ceilingPerSecond, ceilingBurst)
	if refusal != nil {
		releaseOwn()
		refusal.Attempts = attempts
		return 0, nil, refusal
	}
	return max(delay, ceiling), func() {
		releaseOwn()
		releaseCeiling()
	}, nil
}

// wait spends d on the client's clock. It reports the context's own end, so a
// closing client or a finished caller stops waiting instead of holding the budget.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.after(d):
		return ctx.Err()
	}
}

// exchange makes exactly one physical wire request and returns its decoded body.
// It takes one shared slot for the whole exchange, bounds that exchange, builds the
// hop's own transport, attaches the credential only when the hop is the approved
// origin, and reads and closes the body on every path, so no reader escapes.
func (c *Client) exchange(ctx context.Context, op Op, hop *url.URL, approved Origin, sent int) (wireResponse, error) {
	origin := canonicalOrigin(hop)
	release, err := c.slot(ctx)
	if err != nil {
		// A slot refused because this client is closing, or because the caller its
		// send belongs to ended, is that reason and not a spent budget: the ceiling
		// itself was never the decision.
		if c.stopped() || ctx.Err() != nil {
			return wireResponse{}, c.interrupted(ctx, sent)
		}
		return wireResponse{}, &Error{
			Reason:   ReasonBudgetExhausted,
			Params:   map[string]string{"limit": "slot"},
			Attempts: sent,
		}
	}
	defer release()
	attempt, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	// A URL's own userinfo would be sent as a Basic credential this client never
	// scoped, so the copy this request is built from carries none: only Credential
	// attaches one, and only to the approved origin.
	target := cloneURL(hop)
	target.User = nil
	var credentialHeader, credentialValue string
	if credential := op.Target.Credential; credential != nil && origin == approved {
		value, err := credential.Value(attempt)
		if err != nil {
			// The reader's own error may hold the secret: it is not rendered, and
			// the op fails as an unauthenticated request would.
			return wireResponse{}, &Error{
				Reason:   ReasonAuthFailed,
				Params:   map[string]string{"credential": paramCredentialUnavailable},
				Attempts: sent,
			}
		}
		credentialHeader, credentialValue = credential.Header, string(value)
		if credential.Query != "" {
			query := target.Query()
			query.Set(credential.Query, credentialValue)
			target.RawQuery = query.Encode()
		}
	}

	var body io.Reader
	if op.Body != "" {
		body = strings.NewReader(op.Body)
	}
	request, err := http.NewRequestWithContext(attempt, op.Method, target.String(), body)
	if err != nil {
		// A request that cannot be built never left this process, so it applied
		// nothing and is a refusal of this call's own inputs.
		return wireResponse{}, &Error{
			Reason:   ReasonRejected,
			Params:   map[string]string{"host": origin.Host},
			Attempts: sent,
		}
	}
	request.Header.Set("User-Agent", c.userAgent())
	if credentialHeader != "" {
		request.Header.Set(credentialHeader, credentialValue)
	}

	var roundTripper http.RoundTripper = newTransport(origin, op.Target.Private && origin == approved, c.resolver)
	if c.wrap != nil {
		roundTripper = c.wrap(roundTripper)
	}
	client := &http.Client{Transport: roundTripper, CheckRedirect: noRedirect}

	response, err := client.Do(request)
	if err != nil {
		var refusal *Error
		if errors.As(err, &refusal) {
			// The dial refused before this exchange wrote a byte, so the attempt
			// never became a wire request and the count stays with the ones sent.
			refusal.Attempts = sent
			return wireResponse{}, refusal
		}
		return wireResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	decoded, err := ingest.Body(response.Body, response.Header.Get("Content-Encoding"), maxBodyBytes)
	if err != nil {
		if !op.Safe && isMutation(op.Method) {
			// The peer answered this attempt before its body could be used: the
			// status and the unknown-outcome marker travel with the refusal.
			params := map[string]string{"status": strconv.Itoa(response.StatusCode)}
			var refused *ingest.Error
			if errors.As(err, &refused) {
				params["body"] = refused.Reason
			}
			return wireResponse{}, &Error{
				Reason:   ReasonOutcomeUnknown,
				Params:   params,
				Attempts: sent + 1,
				Unknown:  true,
			}
		}
		if attempt.Err() != nil {
			// The attempt or the caller ended the exchange, not the peer's
			// encoding: this is a failed exchange, not an ingest refusal, and the
			// response headers already arrived, so a repeat would ask again for a
			// body this attempt could not read.
			return wireResponse{}, exchangeFailure(origin.Host, sent+1)
		}
		return wireResponse{}, err
	}
	return wireResponse{status: response.StatusCode, header: response.Header.Clone(), body: string(decoded)}, nil
}

// retryableFailure reports a transport failure a further attempt may change: the
// dial never reached the peer, or the peer never answered in time.
func retryableFailure(err error) bool {
	if wroteNothing(err) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// exchangeFailure is a safe read that produced no usable response. A read applies
// nothing, so its outcome is unknown but never marked as a mutation's would be.
func exchangeFailure(host string, attempts int) *Error {
	return &Error{
		Reason:   ReasonOutcomeUnknown,
		Params:   map[string]string{"host": host},
		Attempts: attempts,
	}
}

// statusError is the contract reason for a final non-2xx response. A read that
// authenticated nothing is auth_failed, a status a further identical request
// cannot change is rejected, and 408/429/5xx is an unknown outcome for either
// kind of call — marked unknown only for a mutation, whose peer may already have
// applied it while a read applies nothing.
func statusError(status int, mutating bool, attempts int) *Error {
	params := map[string]string{"status": strconv.Itoa(status)}
	switch {
	case !mutating && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		return &Error{Reason: ReasonAuthFailed, Params: params, Attempts: attempts}
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return &Error{Reason: ReasonOutcomeUnknown, Params: params, Attempts: attempts, Unknown: mutating}
	}
	return &Error{Reason: ReasonRejected, Params: params, Attempts: attempts}
}

// isForbidden reports whether host, or any domain it belongs to, is one of the
// configured digests. Only the digest is compared, so a forbidden name appears
// nowhere in this package.
func (c *Client) isForbidden(host string) bool {
	if len(c.forbidden) == 0 {
		return false
	}
	name := canonicalHost(host)
	for {
		sum := sha256.Sum256([]byte(name))
		if _, ok := c.forbidden[hex.EncodeToString(sum[:])]; ok {
			return true
		}
		_, rest, found := strings.Cut(name, ".")
		if !found || rest == "" {
			return false
		}
		name = rest
	}
}

// userAgent is the one user agent this client sends.
func (c *Client) userAgent() string { return "Boxset/" + c.version + " " + userAgentSuffix }

// flightKey is the merge identity of one safe read: fixed 32 bytes over the op's
// scope, every field length-prefixed so distinct scopes never spell the same
// bytes. It carries no credential bytes and no caller-supplied scope token: the
// CredentialIdentity names the credential's identity, and the origin is the
// canonical form of the request's own URL.
func flightKey(op Op) [sha256.Size]byte {
	digest := sha256.New()
	field := func(text string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(text)))
		digest.Write(size[:])
		digest.Write([]byte(text))
	}
	origin := canonicalOrigin(op.URL)
	field(op.Name)
	field(op.Target.ID)
	field(strconv.FormatInt(op.ConfigRevision, 10))
	field(op.CredentialIdentity)
	field(origin.Scheme)
	field(origin.Host)
	field(strconv.Itoa(origin.Port))
	field(op.Method)
	field(op.URL.EscapedPath())
	field(credentialFreeQuery(op))
	field(op.Body)
	field(op.Language)
	field(op.Representation)
	var key [sha256.Size]byte
	copy(key[:], digest.Sum(nil))
	return key
}

// jitterSeed is the spread seed of one flight: derived from its key, so calls that
// share a flight spread identically and a proof repeats a retry exactly.
func jitterSeed(key [sha256.Size]byte) int64 {
	return int64(binary.BigEndian.Uint64(key[:8]) & 0x7fffffffffffffff)
}

// credentialFreeQuery is the op's query without the credential's own parameter,
// sorted by name, so two spellings of the same query share one key and no key
// holds a credential the peer's convention puts in the URL.
func credentialFreeQuery(op Op) string {
	query := op.URL.Query()
	if op.Target.Credential != nil && op.Target.Credential.Query != "" {
		query.Del(op.Target.Credential.Query)
	}
	return query.Encode()
}

// snapshotOp copies the caller's pointers — Target, Origin, Credential and URL,
// URL.User included — so nothing the caller does to its Op after Do returns can
// change a request already in flight. Body is a string and is shared unmodified.
func snapshotOp(op Op) Op {
	target := *op.Target
	if op.Target.Credential != nil {
		credential := *op.Target.Credential
		target.Credential = &credential
	}
	copied := op
	copied.Target = &target
	copied.URL = cloneURL(op.URL)
	return copied
}

// canonicalOrigin is the one origin form the client compares: lower-case scheme,
// hostname without its root dot and the effective port. Credential attachment and
// the exact private-origin approval both read it, so neither can be widened by
// another spelling of an origin nor narrowed by a different one.
func canonicalOrigin(u *url.URL) Origin {
	scheme := strings.ToLower(u.Scheme)
	port := defaultPort(scheme)
	if written, err := strconv.Atoi(u.Port()); err == nil {
		port = written
	}
	return Origin{Scheme: scheme, Host: canonicalHost(u.Hostname()), Port: port}
}

// approvedOrigin is a configured origin in the same canonical form, with the
// scheme's own port when none was written.
func approvedOrigin(origin Origin) Origin {
	scheme := strings.ToLower(origin.Scheme)
	port := origin.Port
	if port == 0 {
		port = defaultPort(scheme)
	}
	return Origin{Scheme: scheme, Host: canonicalHost(origin.Host), Port: port}
}

// defaultPort is the effective port of a scheme with none written.
func defaultPort(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// canonicalHost is a hostname in the one form origins, budget keys and digests
// compare: lower case and without its trailing root dot.
func canonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// cloneURL copies u, its userinfo included, so attaching a query credential or
// dropping the userinfo never changes the caller's URL.
func cloneURL(u *url.URL) *url.URL {
	copied := *u
	if u.User != nil {
		user := *u.User
		copied.User = &user
	}
	return &copied
}
