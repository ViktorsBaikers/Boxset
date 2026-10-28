package bounded

import (
	"sync"
	"time"
)

const (
	// ReleaseCacheCap is the most the release cache retains, keys and values
	// together. Its producer is release search, which does not exist yet, so
	// nothing fills it today.
	ReleaseCacheCap = 8 << 20
	// ReleaseTTL is how long a release cache entry lives.
	ReleaseTTL = 15 * time.Minute
)

// Cache is a time-to-live cache: an entry is gone ttl after it was stored, and
// the keys and values it retains never exceed maxBytes together. A value larger
// than the whole cap is not stored, so one caller cannot displace the cache.
// There is no eviction of fresh entries: a cache that is full simply stops
// admitting, which keeps a hot entry from being dropped by a cold one.
type Cache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[string]cacheEntry
	bytes   int
	seq     uint64
}

type cacheEntry struct {
	value []byte
	at    time.Time
	seq   uint64
}

// NewCache returns a cache that retains at most maxBytes and forgets an entry
// ttl after it was stored.
func NewCache(maxBytes int, ttl time.Duration) *Cache {
	return &Cache{max: maxBytes, ttl: ttl, now: time.Now, entries: map[string]cacheEntry{}}
}

// Get returns the stored value and whether it is still inside its lifetime.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.expired(entry, c.now()) {
		c.remove(key, entry)
		return nil, false
	}
	return append([]byte(nil), entry.value...), true
}

// Put stores value under key until its lifetime ends. A value larger than the
// cache, or one that does not fit beside the fresh entries, is not stored.
func (c *Cache) Put(key string, value []byte) {
	if c.max <= 0 || len(value) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if entry, ok := c.entries[key]; ok {
		c.remove(key, entry)
	}
	c.expire(now)
	if c.bytes+len(key)+len(value) > c.max {
		return
	}
	c.seq++
	c.entries[key] = cacheEntry{value: append([]byte(nil), value...), at: now, seq: c.seq}
	c.bytes += len(key) + len(value)
}

// Len is the number of retained entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire(c.now())
	return len(c.entries)
}

// Bytes is the retained byte count, keys and values together.
func (c *Cache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire(c.now())
	return c.bytes
}

func (c *Cache) expired(entry cacheEntry, now time.Time) bool {
	return c.ttl > 0 && now.Sub(entry.at) >= c.ttl
}

// expire drops every entry whose lifetime ended.
func (c *Cache) expire(now time.Time) {
	for key, entry := range c.entries {
		if c.expired(entry, now) {
			c.remove(key, entry)
		}
	}
}

func (c *Cache) remove(key string, entry cacheEntry) {
	delete(c.entries, key)
	c.bytes -= len(key) + len(entry.value)
}
