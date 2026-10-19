// Package store owns the SQLite file: its connections, the policy every
// connection reads back, the embedded schema and the transaction helpers.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// SchemaVersion is the user_version of a file this package bootstrapped.
const SchemaVersion = 1

// setUserVersion must say SchemaVersion; TestSchemaMismatchRefused reads the stored value back.
const setUserVersion = "PRAGMA user_version=1"

var (
	// ErrInvalidPath: the path is empty or holds '?' or '#', which the driver
	// would read as a query or fragment even in a plain file name.
	ErrInvalidPath = errors.New("store: invalid database path")
	// ErrSchemaMismatch: the file has tables and a user_version other than SchemaVersion.
	ErrSchemaMismatch = errors.New("store: database schema version mismatch")
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Options carries test seams only.
type Options struct {
	// ConnMaxLifetime, when positive, retires pooled connections so a proof
	// can force new physical connections; it is not a production tuning knob.
	ConnMaxLifetime time.Duration
}

// Policy is what the first connection of each pool read back.
type Policy struct {
	Writer, Reader ConnPolicy
}

// Stats counts the physical connections opened and snapshots both pools.
type Stats struct {
	WriterConnects, ReaderConnects int64
	Writer, Reader                 sql.DBStats
}

// DB is one writer connection and ReaderCount() reader connections over one file.
type DB struct {
	writer, reader   *sql.DB
	writerC, readerC *connector

	mu      sync.Mutex // serializes sizeReaders so no Read acquires a reader before the resize finished
	readers int        // the bound the reader pool carries now

	beforeSample func() // test seam: runs under mu, immediately before the ReaderCount() sample; nil outside tests
}

// Open validates path, bootstraps an empty file (auto_vacuum and WAL are per
// file, so they are fixed once, before the first table), applies the embedded
// schema in one writer transaction and returns the pools. Every physical
// connection of either pool reads its policy back or is refused.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, ErrInvalidPath
	}
	base, err := sqlite.NewConnector(path)
	if err != nil {
		return nil, err
	}
	fresh, err := bootstrap(ctx, base)
	if err != nil {
		return nil, err
	}
	d := &DB{
		writerC: &connector{base: base, want: newPolicy(0)},
		readerC: &connector{base: base, want: newPolicy(1)},
	}
	d.writer = sql.OpenDB(d.writerC)
	d.writer.SetMaxOpenConns(1)
	d.writer.SetMaxIdleConns(1)
	d.reader = sql.OpenDB(d.readerC)
	d.sizeReaders()
	if opts.ConnMaxLifetime > 0 {
		d.writer.SetConnMaxLifetime(opts.ConnMaxLifetime)
		d.reader.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	if err := d.writer.PingContext(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	if fresh {
		if err := d.Write(ctx, func(tx *sql.Tx) error { return applySchema(ctx, tx) }); err != nil {
			return nil, errors.Join(err, d.Close())
		}
	}
	if err := d.reader.PingContext(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
}

// bootstrap runs on a short-lived raw connection. An empty file gets
// auto_vacuum=INCREMENTAL and WAL and is reported fresh; a file with tables
// must carry SchemaVersion. The pools read both back on every connection.
func bootstrap(ctx context.Context, base driver.Connector) (fresh bool, err error) {
	conn, err := base.Connect(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	ex, okE := conn.(driver.ExecerContext)
	q, okQ := conn.(driver.QueryerContext)
	if !okE || !okQ {
		return false, fmt.Errorf("store: driver connection %T lacks ExecerContext or QueryerContext", conn)
	}
	tables, err := queryInt(ctx, q, "SELECT count(*) FROM sqlite_schema")
	if err != nil {
		return false, err
	}
	if tables > 0 {
		version, err := queryInt(ctx, q, "PRAGMA user_version")
		if err != nil {
			return false, err
		}
		if version != SchemaVersion {
			return false, fmt.Errorf("%w: file has user_version %d, want %d", ErrSchemaMismatch, version, SchemaVersion)
		}
		return false, nil
	}
	if _, err := ex.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL", nil); err != nil {
		return false, err
	}
	mode, err := pragma(ctx, q, "journal_mode=wal")
	if err != nil {
		return false, err
	}
	if text(mode) != "wal" {
		return false, fmt.Errorf("%w: journal_mode want wal got %v", ErrPolicyMismatch, mode)
	}
	return true, nil
}

func queryInt(ctx context.Context, q driver.QueryerContext, query string) (int64, error) {
	v, err := queryFirst(ctx, q, query)
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("store: %s returned %T, want an integer", query, v)
	}
	return n, nil
}

func applySchema(ctx context.Context, tx *sql.Tx) error {
	files, err := fs.ReadDir(schemaFS, "schema")
	if err != nil {
		return err
	}
	for _, f := range files {
		body, err := schemaFS.ReadFile("schema/" + f.Name())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("schema %s: %w", f.Name(), err)
		}
	}
	_, err = tx.ExecContext(ctx, setUserVersion)
	return err
}

// Write runs fn in one transaction on the single writer connection. fn uses
// only its tx and never opens a reader.
func (d *DB) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	return run(ctx, d.writer, nil, fn)
}

// Read runs fn in one read-only transaction on a reader connection. The reader
// pool is first resized to the live ReaderCount(), so a GOMAXPROCS change after
// Open bounds the readers from the next Read on.
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	d.sizeReaders()
	return run(ctx, d.reader, &sql.TxOptions{ReadOnly: true}, fn)
}

// sizeReaders sets the reader pool's max open and max idle connections to
// ReaderCount(). The sample is taken under the mutex, so an older sample never
// overwrites a newer one.
func (d *DB) sizeReaders() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.beforeSample != nil {
		d.beforeSample()
	}
	n := ReaderCount()
	if n == d.readers {
		return
	}
	d.reader.SetMaxOpenConns(n)
	d.reader.SetMaxIdleConns(n) // closes the idle connections above n
	d.readers = n
}

func run(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished { // fn panicked; the panic continues
			_ = tx.Rollback()
		}
	}()
	err = fn(tx)
	finished = true
	if err != nil {
		if rerr := tx.Rollback(); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return err
	}
	return tx.Commit()
}

// Policy reports what the first writer and first reader connection read back.
func (d *DB) Policy() Policy {
	var p Policy
	if w := d.writerC.first.Load(); w != nil {
		p.Writer = *w
	}
	if r := d.readerC.first.Load(); r != nil {
		p.Reader = *r
	}
	return p
}

// Stats returns the connect counters and both pools' statistics.
func (d *DB) Stats() Stats {
	return Stats{
		WriterConnects: d.writerC.connects.Load(),
		ReaderConnects: d.readerC.connects.Load(),
		Writer:         d.writer.Stats(),
		Reader:         d.reader.Stats(),
	}
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.reader.Close(), d.writer.Close())
}
