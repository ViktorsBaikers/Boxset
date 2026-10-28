// Package bounded holds the byte- and age-capped buffers the server retains:
// the log ring, the release cache and the SSE replay ring. Every collection in
// this package has a cap that its producer cannot exceed, whatever it writes,
// so a slow or hostile source costs a fixed amount of memory.
package bounded

import (
	"sync"
	"time"
)

const (
	// LogRingCap is the most the log ring retains, whatever the server is asked
	// to log. Its producer is the server's own logger.
	LogRingCap = 2 << 20
	// LogRingAge is how long the log ring keeps a record.
	LogRingAge = 24 * time.Hour
	// ReplayRingCap is the most the SSE replay ring retains. Its producer is the
	// event stream, which does not exist yet, so nothing fills it today.
	ReplayRingCap = 1 << 20
	// ReplayRingAge is how long the SSE replay ring keeps an event.
	ReplayRingAge = 5 * time.Minute
)

// Ring is a first-in-first-out byte ring with a byte cap and an age cap. An
// append past the byte cap drops the oldest entries; an entry older than the age
// cap is dropped before it is read. It copies what it retains, so a caller may
// reuse its buffer.
type Ring struct {
	mu      sync.Mutex
	max     int
	age     time.Duration
	now     func() time.Time
	entries []ringEntry
	bytes   int
}

type ringEntry struct {
	data []byte
	at   time.Time
}

// NewRing returns a ring that retains at most maxBytes of appended bytes and
// nothing older than maxAge.
func NewRing(maxBytes int, maxAge time.Duration) *Ring {
	return NewRingAt(maxBytes, maxAge, time.Now)
}

// NewRingAt is NewRing with the clock its age cap is measured by, for a producer
// that keeps its own.
func NewRingAt(maxBytes int, maxAge time.Duration, now func() time.Time) *Ring {
	return &Ring{max: maxBytes, age: maxAge, now: now}
}

// Append copies p into the ring and drops the oldest entries it must to stay
// inside the byte cap. One append larger than the cap is truncated to its last
// max bytes, so no single write can leave the ring over its own bound.
func (r *Ring) Append(p []byte) {
	if r.max <= 0 || len(p) == 0 {
		return
	}
	if len(p) > r.max {
		p = p[len(p)-r.max:]
	}
	entry := ringEntry{data: append([]byte(nil), p...), at: r.now()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	r.bytes += len(entry.data)
	r.drop(r.now())
	for r.bytes > r.max {
		r.bytes -= len(r.entries[0].data)
		r.entries = r.entries[1:]
	}
}

// Entries returns a copy of the entries inside the age cap, oldest first.
func (r *Ring) Entries() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop(r.now())
	out := make([][]byte, len(r.entries))
	for i, entry := range r.entries {
		out[i] = append([]byte(nil), entry.data...)
	}
	return out
}

// Bytes is the retained byte count.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop(r.now())
	return r.bytes
}

// Len is the retained entry count.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop(r.now())
	return len(r.entries)
}

// drop removes the entries older than the age cap. The ring is ordered by
// append time, so it stops at the first entry still inside the cap.
func (r *Ring) drop(now time.Time) {
	if r.age <= 0 {
		return
	}
	i := 0
	for i < len(r.entries) && now.Sub(r.entries[i].at) >= r.age {
		i++
	}
	if i == 0 {
		return
	}
	for _, entry := range r.entries[:i] {
		r.bytes -= len(entry.data)
	}
	r.entries = r.entries[i:]
}
