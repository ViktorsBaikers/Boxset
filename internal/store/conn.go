package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"sync/atomic"
)

// mmapCap is the finite 64-bit memory-map cap in bytes (ADR 0005 T01, HZN-006).
const mmapCap = 64 << 20

// MmapLimit is the mmap_size every connection must read back for a platform
// whose int is intSize bits wide: 0 on 32-bit, where the address space is too
// small to map a database, and mmapCap on 64-bit.
func MmapLimit(intSize int) int64 {
	if intSize < 64 {
		return 0
	}
	return mmapCap
}

// ReaderCount is the size of the reader pool: max(1, min(4, GOMAXPROCS)).
func ReaderCount() int {
	return max(1, min(4, runtime.GOMAXPROCS(0)))
}

// ErrPolicyMismatch is returned, wrapped with the setting, the wanted and the
// read-back value, when a connection does not read back the policy it was
// given. The connection is closed and never handed out.
var ErrPolicyMismatch = errors.New("store: connection policy mismatch")

// ConnPolicy is the set of settings one physical connection carries. A wanted
// policy is applied and read back; Policy reports what the first connection of
// each pool read back.
type ConnPolicy struct {
	ForeignKeys         int64
	BusyTimeoutMS       int64
	CacheSize           int64 // negative: KiB
	MmapSize            int64
	JournalSizeLimit    int64
	JournalMode         string
	Synchronous         int64 // 2 = FULL
	QueryOnly           int64
	AutoVacuum          int64 // 2 = INCREMENTAL
	FullFsync           int64
	CheckpointFullFsync int64
}

func newPolicy(queryOnly int64) ConnPolicy {
	p := ConnPolicy{
		ForeignKeys:      1,
		BusyTimeoutMS:    5000,
		CacheSize:        -8192,
		MmapSize:         MmapLimit(strconv.IntSize),
		JournalSizeLimit: 64 << 20,
		JournalMode:      "wal",
		Synchronous:      2,
		QueryOnly:        queryOnly,
		AutoVacuum:       2,
	}
	if runtime.GOOS == "darwin" {
		p.FullFsync, p.CheckpointFullFsync = 1, 1
	}
	return p
}

// intField names one integer setting of a ConnPolicy. set: the connection
// applies it; enforced: a differing read-back refuses the connection. The
// per-file settings (journal_mode, auto_vacuum) are read back on every
// connection but fixed once at bootstrap, and the flush settings are enforced
// on darwin only and recorded everywhere.
type intField struct {
	name     string
	v        *int64
	set      bool
	enforced bool
}

func (p *ConnPolicy) intFields() []intField {
	darwin := runtime.GOOS == "darwin"
	return []intField{
		{"busy_timeout", &p.BusyTimeoutMS, true, true},
		{"foreign_keys", &p.ForeignKeys, true, true},
		{"cache_size", &p.CacheSize, true, true},
		{"mmap_size", &p.MmapSize, true, true},
		{"journal_size_limit", &p.JournalSizeLimit, true, true},
		{"synchronous", &p.Synchronous, true, true},
		{"auto_vacuum", &p.AutoVacuum, false, true},
		{"fullfsync", &p.FullFsync, darwin, darwin},
		{"checkpoint_fullfsync", &p.CheckpointFullFsync, darwin, darwin},
		{"query_only", &p.QueryOnly, true, true}, // last: it forbids writes
	}
}

// connector wraps the driver's connector. Connect applies the policy to the
// fresh physical connection, reads every setting back and closes the
// connection when any enforced setting differs, so database/sql never holds a
// connection that was not checked.
type connector struct {
	base     driver.Connector
	want     ConnPolicy
	connects atomic.Int64
	first    atomic.Pointer[ConnPolicy]
}

func (c *connector) Driver() driver.Driver { return c.base.Driver() }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.connects.Add(1)
	got, err := c.check(ctx, conn)
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	c.first.CompareAndSwap(nil, &got)
	return conn, nil
}

func (c *connector) check(ctx context.Context, conn driver.Conn) (ConnPolicy, error) {
	ex, okE := conn.(driver.ExecerContext)
	q, okQ := conn.(driver.QueryerContext)
	if !okE || !okQ {
		return ConnPolicy{}, fmt.Errorf("store: driver connection %T lacks ExecerContext or QueryerContext", conn)
	}
	want := c.want
	for _, f := range want.intFields() {
		if !f.set {
			continue
		}
		stmt := fmt.Sprintf("PRAGMA %s=%d", f.name, *f.v)
		if _, err := ex.ExecContext(ctx, stmt, nil); err != nil {
			return ConnPolicy{}, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	var got ConnPolicy
	gotFields := got.intFields()
	for i, f := range want.intFields() {
		v, err := pragma(ctx, q, f.name)
		if err != nil {
			return ConnPolicy{}, err
		}
		n, ok := v.(int64)
		if v != nil && !ok {
			return ConnPolicy{}, fmt.Errorf("store: PRAGMA %s read back %v (%T), want an integer", f.name, v, v)
		}
		*gotFields[i].v = n
		if f.enforced && n != *f.v {
			return got, fmt.Errorf("%w: %s want %d got %d", ErrPolicyMismatch, f.name, *f.v, n)
		}
	}
	v, err := pragma(ctx, q, "journal_mode")
	if err != nil {
		return got, err
	}
	got.JournalMode = text(v)
	if got.JournalMode != want.JournalMode {
		return got, fmt.Errorf("%w: journal_mode want %s got %s", ErrPolicyMismatch, want.JournalMode, got.JournalMode)
	}
	return got, nil
}

// pragma runs PRAGMA name (or name=value) and returns the first column of the first row; a
// pragma that returns no row reads back as nil.
func pragma(ctx context.Context, q driver.QueryerContext, name string) (driver.Value, error) {
	return queryFirst(ctx, q, "PRAGMA "+name)
}

func queryFirst(ctx context.Context, q driver.QueryerContext, query string) (driver.Value, error) {
	rows, err := q.QueryContext(ctx, query, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", query, err)
	}
	dest := make([]driver.Value, len(rows.Columns()))
	nerr := rows.Next(dest)
	cerr := rows.Close()
	eof := errors.Is(nerr, io.EOF)
	if nerr != nil && !eof {
		return nil, fmt.Errorf("%s: %w", query, errors.Join(nerr, cerr))
	}
	if eof || len(dest) == 0 {
		return nil, cerr
	}
	return dest[0], cerr
}

func text(v driver.Value) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}
