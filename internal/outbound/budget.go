package outbound

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// The recorded budget bounds (architecture § Outbound contract). A budget is the
// one allowance a physical wire request spends, whatever the call around it does:
// the limiter that decides when it may go, the ceiling its host cannot exceed, and
// the cache those limiters live in.
const (
	// budgetKeys bounds the limiter cache. A new identity at this count with no
	// idle entry is refused rather than growing the map.
	budgetKeys = 4096
	// budgetIdle is how long an entry may go unused before it may be evicted.
	budgetIdle = 15 * time.Minute

	// One indexer's own allowance, and the host allowance a call without an
	// indexer spends instead.
	indexerPerSecond = 1
	indexerBurst     = 2
	hostPerSecond    = 20
	hostBurst        = 20
	// ceilingPerSecond bounds everything one host sends, whatever the own keys
	// above allow.
	ceilingPerSecond = 50
	ceilingBurst     = 50
)

// The budget key kinds: the tag a key is hashed from, so two identities that
// spell the same bytes under different kinds stay distinct.
const (
	keyIndexer = "indexer"
	keyHost    = "host"
	keyCeiling = "ceiling"
)

// Scope is which budget key a Retry-After hits. The peer's guidance is the op's
// own unless the caller says it is the host's: that holds every op to the host,
// which is what a host-wide throttle means.
type Scope uint8

const (
	// ScopeOwn holds only the key the op itself charged, so a sibling indexer on
	// the same host keeps its own allowance.
	ScopeOwn Scope = iota
	// ScopeHost holds the host's shared ceiling, so both indexers wait.
	ScopeHost
)

// Budget is an op's own allowance: a non-empty Indexer is one indexer's
// independent budget, and an empty one spends the host's.
type Budget struct {
	Indexer string
}

// budget is the client's limiter cache. One entry per key kind and identity, and
// the map holds only the limiter and when it was last used, never the identity the
// key was made from.
type budget struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*limiter
}

// limiter is one cache entry: the limiter that charges the key, the instant a
// hold the peer asked for ends, and the accounting eviction needs.
type limiter struct {
	mu    sync.Mutex
	held  *rate.Limiter
	hold  time.Time
	last  time.Time
	waits int
}

// newBudget builds an empty cache. Every charge hands it the client's own instant,
// so the cache keeps no clock of its own.
func newBudget() *budget {
	return &budget{entries: make(map[[sha256.Size]byte]*limiter)}
}

// budgetKey is the fixed 32-byte identity of one key: the kind, then the length of
// its value and the value, so no two kind/identity pairs spell the same bytes and
// no raw identity is retained.
func budgetKey(kind, id string) [sha256.Size]byte {
	digest := sha256.New()
	digest.Write([]byte(kind))
	digest.Write([]byte{0})
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(id)))
	digest.Write(length[:])
	digest.Write([]byte(id))
	var key [sha256.Size]byte
	copy(key[:], digest.Sum(nil))
	return key
}

// budgetExhausted is the refusal of a call whose next key would overrun the cache.
func budgetExhausted(params map[string]string, attempts int) *Error {
	return &Error{Reason: ReasonBudgetExhausted, Params: params, Attempts: attempts}
}

// keysFull is the recorded refusal of a new identity at the cache bound.
func keysFull(attempts int) *Error {
	return budgetExhausted(map[string]string{"limit": "budget_keys", "max": strconv.Itoa(budgetKeys)}, attempts)
}

// entry returns the limiter of kind/id, creating it while the cache has room and
// some entry is idle enough to go. An entry that is being waited on, or that
// carries a live hold, is never evicted: its allowance is not reset behind a call
// that already reserved against it.
func (b *budget) entry(kind, id string, at time.Time, perSecond, burst int) (*limiter, *Error) {
	key := budgetKey(kind, id)
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.entries[key]; ok {
		return entry, nil
	}
	if len(b.entries) >= budgetKeys {
		if !b.evictIdle(at) {
			return nil, keysFull(0)
		}
	}
	entry := &limiter{held: rate.NewLimiter(rate.Limit(perSecond), burst), last: at}
	b.entries[key] = entry
	return entry, nil
}

// evictIdle removes every entry nothing is waiting on and nothing holds. It
// reports whether it removed one.
func (b *budget) evictIdle(at time.Time) bool {
	removed := false
	for key, entry := range b.entries {
		entry.mu.Lock()
		idle := entry.waits == 0 && !at.Before(entry.hold) && at.Sub(entry.last) >= budgetIdle
		entry.mu.Unlock()
		if !idle {
			continue
		}
		delete(b.entries, key)
		removed = true
	}
	return removed
}

// reserve spends one token of kind/id at at and returns the wait it asked for with
// the release of that wait's accounting. The wait is the larger of the limiter's
// own delay and a hold the peer asked for, so a Retry-After is waited once, here,
// and not again by the caller. The token stays spent whatever the caller does next;
// the release is called once the wait is over, so an entry nothing waits on any
// more can be evicted and a completed request never pins its key.
func (b *budget) reserve(kind, id string, at time.Time, perSecond, burst int) (time.Duration, func(), *Error) {
	entry, refusal := b.entry(kind, id, at, perSecond, burst)
	if refusal != nil {
		return 0, nil, refusal
	}
	entry.mu.Lock()
	reservation := entry.held.ReserveN(at, 1)
	if !reservation.OK() {
		entry.mu.Unlock()
		return 0, nil, budgetExhausted(map[string]string{"limit": kind, "burst": strconv.Itoa(burst)}, 0)
	}
	delay := reservation.DelayFrom(at)
	if held := entry.hold.Sub(at); held > delay {
		delay = held
	}
	entry.waits++
	entry.last = at
	entry.mu.Unlock()
	return delay, func() {
		entry.mu.Lock()
		entry.waits--
		entry.mu.Unlock()
	}, nil
}

// hold keeps kind/id from charging anything before until. A peer that named a
// Retry-After has said when it will answer again, and a key nothing has charged
// yet needs no hold.
func (b *budget) hold(kind, id string, until time.Time) {
	key := budgetKey(kind, id)
	b.mu.Lock()
	entry, ok := b.entries[key]
	b.mu.Unlock()
	if !ok {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if until.After(entry.hold) {
		entry.hold = until
	}
}

// size is how many keys the cache holds, for a proof that the bound is the
// recorded one.
func (b *budget) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}
