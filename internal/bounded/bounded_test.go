package bounded

import (
	"bytes"
	"testing"
	"time"
)

// TestRingByteAndAge requires both caps to hold: appends past the byte cap drop
// the oldest entries, and an entry older than the age cap is gone even while the
// byte cap has room. The clock is injected so the age bound is exercised without
// waiting.
func TestRingByteAndAge(t *testing.T) {
	now := time.Unix(0, 0)
	r := NewRing(10, time.Minute)
	r.now = func() time.Time { return now }
	for _, entry := range []string{"aaaa", "bbbb", "cccc"} {
		r.Append([]byte(entry))
	}
	if got := r.Len(); got != 2 {
		t.Fatalf("byte cap: %d entries retained, want 2", got)
	}
	if got := r.Bytes(); got != 8 {
		t.Fatalf("byte cap: %d bytes retained, want 8", got)
	}
	if got := r.Entries(); len(got) != 2 || !bytes.Equal(got[0], []byte("bbbb")) || !bytes.Equal(got[1], []byte("cccc")) {
		t.Fatalf("byte cap keeps the newest entries, got %q", got)
	}

	now = now.Add(2 * time.Minute)
	if got := r.Len(); got != 0 {
		t.Fatalf("age cap: %d entries retained, want 0", got)
	}
	if got := r.Bytes(); got != 0 {
		t.Fatalf("age cap: %d bytes retained, want 0", got)
	}

	// One append larger than the whole cap is truncated, never stored whole.
	r.Append(bytes.Repeat([]byte("x"), 100))
	if got := r.Bytes(); got != 10 {
		t.Fatalf("oversized append retained %d bytes, want 10", got)
	}

	// A fresh entry survives the age of the entries before it.
	now = now.Add(time.Minute)
	r.Append([]byte("dddd"))
	if got := r.Len(); got != 1 {
		t.Fatalf("fresh entry: %d entries retained, want 1", got)
	}
}

// TestCacheTTL requires an entry to be gone once its lifetime ends and the cache
// to stay inside its byte cap, admitting no value larger than the cap itself.
func TestCacheTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewCache(16, 15*time.Minute)
	c.now = func() time.Time { return now }
	c.Put("k", []byte("12345678"))
	if value, ok := c.Get("k"); !ok || !bytes.Equal(value, []byte("12345678")) {
		t.Fatalf("Get = %q, %v", value, ok)
	}
	now = now.Add(14 * time.Minute)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("an entry inside its 15 min lifetime was dropped")
	}
	now = now.Add(time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatal("an entry past its 15 min lifetime was served")
	}
	if got := c.Bytes(); got != 0 {
		t.Fatalf("%d bytes retained after expiry, want 0", got)
	}

	// The cap counts keys and values together: a second value that does not fit
	// is refused, and the entry already stored stays readable.
	c.Put("k", []byte("123456789"))
	c.Put("k2", []byte("abcdefghij"))
	if _, ok := c.Get("k"); !ok {
		t.Fatal("a refused value displaced the retained one")
	}
	if _, ok := c.Get("k2"); ok {
		t.Fatal("a value past the byte cap was stored")
	}
	if got := c.Bytes(); got > 16 {
		t.Fatalf("%d bytes retained, want at most 16", got)
	}

	// A value larger than the whole cache is never stored.
	c.Put("large", bytes.Repeat([]byte("x"), 17))
	if _, ok := c.Get("large"); ok {
		t.Fatal("a value larger than the cache was stored")
	}
}
