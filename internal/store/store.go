// Package store owns the SQLite file: its connections, the policy every
// connection reads back, the hash-recorded migrations and the transaction
// helpers.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// SchemaVersion is the user_version of every file this package manages: the
// first migration (or the adoption of a predecessor file, in the same
// transaction as its migration records) stamps it and no later migration
// changes it. It is not the predecessor's stamp: the binary that bootstrapped
// files with the temporary schema owner admits only user_version 1
// (legacySchemaVersion) and so refuses every file that carries migration
// records. A file with tables and any other stamp is refused before anything
// else is read.
const SchemaVersion = 2

// legacySchemaVersion is the stamp the temporary bootstrap left on its files.
// It is accepted only on a file that has no migration record and exactly the
// first migration's schema, which the adoption then restamps SchemaVersion.
const legacySchemaVersion = 1

var (
	// ErrInvalidPath: the path is empty or holds '?' or '#', which the driver
	// would read as a query or fragment even in a plain file name.
	ErrInvalidPath = errors.New("store: invalid database path")
	// ErrSchemaMismatch: the file is not one this package may migrate: it has
	// tables and neither format stamp, or a stamp that contradicts its records,
	// or a schema, record tables or interrupted-bootstrap residue that are not exactly what a
	// migration of this package leaves.
	ErrSchemaMismatch = errors.New("store: database schema version mismatch")
	// ErrClosed: Close began, so no writer may mutate any more.
	ErrClosed = errors.New("store: closed")
	// ErrWriterBusy: Close found the writer admission lease held, so the store
	// was left open: the operation holding it may still commit.
	ErrWriterBusy = errors.New("store: writer admission lease held")
	// ErrMetadataBatch: WriteMetadata was handed a batch size outside
	// 1..MaxMetadataRecords, so no lease was taken.
	ErrMetadataBatch = errors.New("store: metadata batch size out of range")
)

// MaxMetadataRecords is the largest batch the credential-metadata coordinator
// may select for one WriteMetadata call.
const MaxMetadataRecords = 256

//go:embed migrations/*.sql
var migrationFS embed.FS

// Options carries the test seam and the pre-migration hook.
type Options struct {
	// ConnMaxLifetime, when positive, retires pooled connections so a proof
	// can force new physical connections; it is not a production tuning knob.
	ConnMaxLifetime time.Duration

	// BeforeMigrate runs once, after the pending migrations were found by
	// read-only SQL and before the first statement that changes the file, on a
	// file that already holds tables. A returned error aborts Open with the
	// file unchanged. Nil refuses every migration of a nonempty file with
	// ErrBackupRequired; an empty file has nothing to back up and never calls it.
	BeforeMigrate func(context.Context, Pending) error

	// Log, when set, receives the fixed credential-metadata admission
	// observations: one wait and one terminal record per attempt, through the
	// caller's already redacted logger. Nil keeps the store silent.
	Log *slog.Logger

	failAfterRecord func(version int64) error // test seam: see hashStore
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

// MetadataPhase is where the one credential-metadata attempt stands. It is a
// process-local fact for the coordinator's shutdown record, never authority.
type MetadataPhase uint8

const (
	MetadataIdle     MetadataPhase = iota // no wait or grant of an attempt was classified yet
	MetadataWaiting                       // the attempt waits behind another operation's lease
	MetadataAdmitted                      // the attempt itself holds the lease
	MetadataTerminal                      // the attempt ended with its MetadataReason
)

func (p MetadataPhase) String() string {
	switch p {
	case MetadataWaiting:
		return "waiting"
	case MetadataAdmitted:
		return "admitted"
	case MetadataTerminal:
		return "terminal"
	default:
		return "idle"
	}
}

// MetadataReason is why the attempt ended.
type MetadataReason uint8

const (
	MetadataNoReason MetadataReason = iota // the attempt has not ended
	MetadataSuccess
	MetadataCanceled
	MetadataDeadline
	MetadataFailed
)

func (r MetadataReason) String() string {
	switch r {
	case MetadataSuccess:
		return "success"
	case MetadataCanceled:
		return "canceled"
	case MetadataDeadline:
		return "deadline"
	case MetadataFailed:
		return "error"
	default:
		return "pending"
	}
}

// MetadataAdmission is the bounded current state of the one metadata write: the
// records the coordinator selected, its process-local ordinal, the admission
// generation it waited behind or holds, and where it stands. It carries no
// credential, role, statement, path or error text.
type MetadataAdmission struct {
	Records    int
	Ordinal    uint64
	Generation uint64
	Phase      MetadataPhase
	Reason     MetadataReason
	// Waiting is true only while the batch is nonempty and the exact holder
	// generation the wait registered against still occupies the lease.
	Waiting bool
}

// metadataAttempt is one attempt's identity: its selected count and its
// process-local ordinal, plus the generation it waited behind or was granted.
type metadataAttempt struct {
	records    int
	ordinal    uint64
	generation uint64
}

// metadataState is the single current attempt, overwritten in place.
type metadataState struct {
	records    int
	ordinal    uint64
	generation uint64
	phase      MetadataPhase
	reason     MetadataReason
}

// DB is one writer connection and ReaderCount() reader connections over one file.
type DB struct {
	writer, reader   *sql.DB
	writerC, readerC *connector

	// lease is the writer admission: its one token is held by the Write or
	// direct pooled-writer operation that may mutate the file, from before its
	// connection is taken until the connection is returned. It is built with the
	// pools, never on the first Write, so Close can tell a held writer from an
	// idle one from Open onwards.
	lease chan struct{}
	// closing records that a Close began. It admits no further writer, so a
	// store whose Close was refused because the lease was held mutates nothing
	// more.
	closing bool // guarded by state

	// state guards closing, holder and metadata: the generation of the operation
	// that holds the lease now (0 when idle; Close holds no generation, so it never
	// counts as a metadata holder) and the one current metadata attempt. Neither
	// is persisted and neither grows.
	state    sync.Mutex
	grants   uint64
	attempts uint64
	holder   uint64
	metadata metadataState
	// log receives the fixed admission observations; nil keeps the store silent.
	log *slog.Logger

	mu          sync.Mutex // serializes sizeReaders so no Read acquires a reader before the resize finished
	readers     int        // the bound the reader pool carries now
	readActive  [2]int
	readPending [2]int
	readTurn    int
	readWake    chan struct{}

	beforeSample func() // test seam: runs under mu, immediately before the ReaderCount() sample; nil outside tests

	afterReclaimBatch   func(batch int)       // test seam: runs after Reclaim committed batch number batch; nil outside tests
	beforeReclaimCommit func(batch int) error // test seam: runs inside Reclaim's write transaction after the final page counts of batch number batch, before its commit; an error rolls the batch back; nil outside tests
}

// Open validates path, bootstraps an empty file (auto_vacuum and WAL are per
// file, so they are fixed once, before the first table), applies the embedded
// migrations and returns the pools. A file that already holds tables is
// migrated only after its recorded hashes verify and BeforeMigrate returned.
// Every physical connection of either pool reads its policy back or is
// refused.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	src, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	return open(ctx, path, opts, src)
}

// open is Open over an explicit migration set, so a proof can run a set the
// binary does not embed.
func open(ctx context.Context, path string, opts Options, src fs.FS) (*DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, ErrInvalidPath
	}
	set, err := loadMigrations(src)
	if err != nil {
		return nil, err
	}
	// Immediate begin takes the write lock up front: a deferred read-to-write upgrade can fail with SQLITE_BUSY without waiting on busy_timeout.
	base, err := sqlite.NewConnector(path + "?_txlock=immediate")
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
		lease:   make(chan struct{}, 1),
		log:     opts.Log,
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
	if err := d.migrate(ctx, base, set, fresh, opts); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	if err := d.reader.PingContext(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
}

// bootstrap runs on a short-lived raw connection and, until it has classified
// the file, only reads. A file with no schema object and no format stamp is
// empty and gets auto_vacuum=INCREMENTAL and WAL, which are per file and so
// fixed once, before the first table. The one other file reported fresh is the
// interrupted first migration (see interruptedInitialization), whose policy
// was fixed then and is only checked now. A file that carries either format
// stamp is an existing store: inspection decides whether it needs migrating.
// Anything else (foreign tables, a stamp on an empty file, goose's table names
// with other content) is refused unchanged. The pools read the policy back on
// every connection.
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
	objects, err := queryInt(ctx, q, "SELECT count(*) FROM sqlite_schema")
	if err != nil {
		return false, err
	}
	version, err := queryInt(ctx, q, "PRAGMA user_version")
	if err != nil {
		return false, err
	}
	switch {
	case objects == 0 && version == 0:
		// empty: the policy below is the only write before the migrations.
	case objects == 0:
		return false, fmt.Errorf("%w: file has no table but user_version %d", ErrSchemaMismatch, version)
	case version == legacySchemaVersion || version == SchemaVersion:
		return false, nil
	case version == 0:
		if err := interruptedInitialization(ctx, q); err != nil {
			return false, fmt.Errorf("%w: file has tables and no format stamp and is not an interrupted first migration: %v", ErrSchemaMismatch, err)
		}
		if err := requireBootstrapPolicy(ctx, q); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("%w: file has user_version %d, want %d (or the legacy %d)", ErrSchemaMismatch, version, SchemaVersion, legacySchemaVersion)
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

// requireBootstrapPolicy reads, without writing, that a file the bootstrap
// already configured still carries auto_vacuum=INCREMENTAL and WAL.
func requireBootstrapPolicy(ctx context.Context, q driver.QueryerContext) error {
	vacuum, err := queryInt(ctx, q, "PRAGMA auto_vacuum")
	if err != nil {
		return err
	}
	mode, err := pragma(ctx, q, "journal_mode")
	if err != nil {
		return err
	}
	if vacuum != 2 || text(mode) != "wal" {
		return fmt.Errorf("%w: interrupted first migration with auto_vacuum %d and journal_mode %v, want 2 and wal", ErrPolicyMismatch, vacuum, mode)
	}
	return nil
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

// Write runs fn in one transaction on the single writer connection, holding the
// writer admission lease from before BEGIN until the transaction ended. fn uses
// only its tx and never opens a reader.
func (d *DB) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := d.admit(ctx, nil); err != nil {
		return err
	}
	defer d.release()
	return run(ctx, d.writer, nil, fn, d.writerC)
}

// WriteMetadata runs the credential-metadata coordinator's transaction under the
// same writer admission lease as Write. records is the batch the coordinator
// selected (1..MaxMetadataRecords) and the only fact the store keeps about the
// attempt; ordinary writers use Write.
func (d *DB) WriteMetadata(ctx context.Context, records int, fn func(*sql.Tx) error) error {
	if records < 1 || records > MaxMetadataRecords {
		return fmt.Errorf("%w: %d records, want 1..%d", ErrMetadataBatch, records, MaxMetadataRecords)
	}
	meta := d.nextMetadata(records)
	if err := d.admit(ctx, meta); err != nil {
		d.endMetadata(meta, err)
		return err
	}
	defer d.release()
	err := run(ctx, d.writer, nil, fn, d.writerC)
	d.endMetadata(meta, err)
	return err
}

// Admission returns the current metadata-admission state without taking the
// lease, so a signal-time reader is never delayed by a blocked writer.
func (d *DB) Admission() MetadataAdmission {
	d.state.Lock()
	defer d.state.Unlock()
	m := d.metadata
	return MetadataAdmission{
		Records:    m.records,
		Ordinal:    m.ordinal,
		Generation: m.generation,
		Phase:      m.phase,
		Reason:     m.reason,
		Waiting:    m.records > 0 && m.phase == MetadataWaiting && m.generation != 0 && m.generation == d.holder,
	}
}

// nextMetadata classifies a new attempt: every attempt gets the next ordinal.
func (d *DB) nextMetadata(records int) *metadataAttempt {
	d.state.Lock()
	defer d.state.Unlock()
	d.attempts++
	return &metadataAttempt{records: records, ordinal: d.attempts}
}

// admit takes the writer admission lease for one operation, waiting at most
// until ctx ends. A closed store admits nobody, before and after the wait: a
// grant that arrives while a waiter is parked releases it again. A cancellation
// that becomes ready together with a grant wins, so an abandoned request never
// runs its callback. meta, when set, is the metadata attempt this grant serves:
// its wait is registered only once the immediate grant failed and another real
// operation still holds the lease.
func (d *DB) admit(ctx context.Context, meta *metadataAttempt) error {
	d.state.Lock()
	closing := d.closing
	d.state.Unlock()
	if closing {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if meta != nil {
		select {
		case d.lease <- struct{}{}:
			return d.grant(ctx, meta)
		default:
		}
		if holder, ok := d.registerWait(meta); ok {
			d.observeWait(meta, holder)
		}
	}
	select {
	case d.lease <- struct{}{}:
		return d.grant(ctx, meta)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// grant finishes one admission: a store closed or a context ended during the
// wait releases the lease again, so an abandoned request never runs its
// callback. Otherwise the new holder's generation is published.
func (d *DB) grant(ctx context.Context, meta *metadataAttempt) error {
	if err := ctx.Err(); err != nil {
		d.release()
		return err
	}
	d.state.Lock()
	if d.closing {
		d.state.Unlock()
		d.release()
		return ErrClosed
	}
	d.grants++
	d.holder = d.grants
	if meta != nil {
		meta.generation = d.grants
		d.metadata = metadataState{records: meta.records, ordinal: meta.ordinal, generation: meta.generation, phase: MetadataAdmitted}
	}
	d.state.Unlock()
	return nil
}

// registerWait publishes the attempt's wait behind the lease's live holder and
// reports the generation it named. It reports false, registering nothing, when
// the lease went idle first: a wait is never claimed without a real operation
// holding the lease.
func (d *DB) registerWait(meta *metadataAttempt) (uint64, bool) {
	d.state.Lock()
	defer d.state.Unlock()
	if d.holder == 0 {
		return 0, false
	}
	meta.generation = d.holder
	d.metadata = metadataState{records: meta.records, ordinal: meta.ordinal, generation: meta.generation, phase: MetadataWaiting}
	return meta.generation, true
}

// endMetadata publishes the attempt's terminal state and its one terminal
// observation. The reason comes from the attempt's own error, so an attempt
// that ended on its deadline keeps that reason through any later stop.
func (d *DB) endMetadata(meta *metadataAttempt, err error) {
	reason := MetadataSuccess
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		reason = MetadataDeadline
	case errors.Is(err, context.Canceled):
		reason = MetadataCanceled
	default:
		reason = MetadataFailed
	}
	d.state.Lock()
	d.metadata = metadataState{records: meta.records, ordinal: meta.ordinal, generation: meta.generation, phase: MetadataTerminal, reason: reason}
	d.state.Unlock()
	d.observeTerminal(meta, reason)
}

// observeWait and observeTerminal each emit one fixed-field record through the
// optional logger, never under a lock. Missing output is a proof gap, never a
// runtime decision.
func (d *DB) observeWait(meta *metadataAttempt, holder uint64) {
	if d.log == nil {
		return
	}
	d.log.Info("credential metadata waits for the writer lease", "reason", "metadata_wait", "records", meta.records, "ordinal", meta.ordinal, "generation", holder)
}

func (d *DB) observeTerminal(meta *metadataAttempt, reason MetadataReason) {
	if d.log == nil {
		return
	}
	d.log.Info("credential metadata attempt ended", "reason", "metadata_terminal", "outcome", reason.String(), "records", meta.records, "ordinal", meta.ordinal, "generation", meta.generation)
}

// release returns the writer admission lease and clears its holder, so a wait
// can no longer name the operation that just ended.
func (d *DB) release() {
	d.state.Lock()
	d.holder = 0
	d.state.Unlock()
	<-d.lease
}

// Read runs fn in one read-only transaction on a reader connection. The reader
// pool is first resized to the live ReaderCount(), so a GOMAXPROCS change after
// Open bounds the readers from the next Read on.
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return d.read(ctx, 0, fn)
}

// ReadPassword shares the physical pool while reserving ordinary-read progress.
func (d *DB) ReadPassword(ctx context.Context, fn func(*sql.Tx) error) error {
	return d.read(ctx, 1, fn)
}

func (d *DB) read(ctx context.Context, class int, fn func(*sql.Tx) error) error {
	if err := d.acquireReader(ctx, class); err != nil {
		return err
	}
	defer d.releaseReader(class)
	conn, err := d.reader.Conn(ctx)
	if err != nil {
		return err
	}
	// Closing this held connection joins database/sql's cancellation rollback
	// before the class grant can be reused.
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			err = errors.Join(err, cerr)
		}
		return err
	}
	finished := false
	defer func() {
		if !finished {
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

func (d *DB) acquireReader(ctx context.Context, class int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readWake == nil {
		d.readWake = make(chan struct{})
	}
	d.readPending[class]++
	d.wakeReaders()
	for {
		d.sizeReadersLocked()
		if err := ctx.Err(); err != nil {
			d.readPending[class]--
			d.wakeReaders()
			return err
		}
		if d.readerEligible(class) {
			d.readPending[class]--
			d.readActive[class]++
			d.readTurn = 1 - class
			d.wakeReaders()
			return nil
		}
		wake := d.readWake
		d.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
		}
		d.mu.Lock()
	}
}

func (d *DB) readerEligible(class int) bool {
	active := d.readActive[0] + d.readActive[1]
	if active >= d.readers {
		return false
	}
	if d.readers == 1 {
		return d.readTurn == class || d.readPending[d.readTurn] == 0
	}
	if class == 1 {
		return d.readActive[1] == 0
	}
	return d.readActive[1] != 0 || d.readPending[1] == 0
}

func (d *DB) releaseReader(class int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readActive[class]--
	d.sizeReadersLocked()
	d.wakeReaders()
}

func (d *DB) wakeReaders() {
	close(d.readWake)
	d.readWake = make(chan struct{})
}

func (d *DB) sizeReaders() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sizeReadersLocked()
}

// sizeReadersLocked samples and resizes before any class can receive a grant.
func (d *DB) sizeReadersLocked() {
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

// run commits fn's transaction and then counts the commit on c, when set.
func run(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error, c *connector) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			err = errors.Join(err, cerr) // keep the native error; add the context identity
		}
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
	if err := tx.Commit(); err != nil {
		return err
	}
	c.countCommit()
	return nil
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

// Close closes both pools once no writer holds the admission lease. A Close
// that finds the lease held refuses with ErrWriterBusy instead of reporting a
// closed store that the writer could still commit into; from then on no writer
// is admitted, so the next Close, after that lease is released, succeeds.
func (d *DB) Close() error {
	d.state.Lock()
	d.closing = true
	d.state.Unlock()
	select {
	case d.lease <- struct{}{}:
	default:
		return ErrWriterBusy
	}
	defer d.release()
	return errors.Join(d.reader.Close(), d.writer.Close())
}
