package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"modernc.org/sqlite"

	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

var (
	// ErrSchemaHash: a migration the file records as applied has no stored
	// SHA-256, or a different one than the embedded file of the same version.
	ErrSchemaHash = errors.New("store: stored migration hash differs from the embedded migration")
	// ErrSchemaNewer: the file records an applied migration this binary does not embed.
	ErrSchemaNewer = errors.New("store: database was migrated by a newer version")
	// ErrBackupRequired: a pending migration of a nonempty file needs the
	// pre-migration backup and Options.BeforeMigrate is nil.
	ErrBackupRequired = errors.New("store: a pending migration needs Options.BeforeMigrate")

	errNoTransaction = errors.New("store: a migration may not be annotated NO TRANSACTION")
)

// stampManaged writes SchemaVersion, the managed format stamp, when a
// predecessor file is adopted; the first migration ends with the same PRAGMA.
const stampManaged = "PRAGMA user_version = 2"

// Pending describes the migrations Open is about to apply to a nonempty file.
type Pending struct {
	// From is the highest applied version, 0 for a file that holds the
	// predecessor's schema without any version record. To is the newest embedded one.
	From, To int64
	// SnapshotInto copies the file as it stands now to dest (VACUUM INTO on a
	// connection outside any transaction); dest must not exist.
	SnapshotInto func(ctx context.Context, dest string) error
}

// migration is one embedded file: its version and the SHA-256 of its exact bytes.
type migration struct {
	version int64
	sha256  string
}

// migrationSet is the embedded migrations in version order, 1, 2, ... without a gap.
type migrationSet struct {
	src  fs.FS
	list []migration
}

func (s *migrationSet) max() int64 { return int64(len(s.list)) }

// loadMigrations reads every NNNNN_name.sql file of src. A file is refused when
// its version breaks the sequence or it asks for NO TRANSACTION: a migration
// body, its version row and its hash row must commit as one.
func loadMigrations(src fs.FS) (*migrationSet, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	set := &migrationSet{src: src}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		digits, _, ok := strings.Cut(name, "_")
		version, err := strconv.ParseInt(digits, 10, 64)
		if !ok || len(digits) != 5 || strings.Trim(digits, "0123456789") != "" || err != nil || version != set.max()+1 {
			return nil, fmt.Errorf("store: migration file %q: want NNNNN_name.sql with the versions 1, 2, ... in order", name)
		}
		body, err := fs.ReadFile(src, name)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if declaresNoTransaction(body) {
			return nil, fmt.Errorf("%w: %s", errNoTransaction, name)
		}
		sum := sha256.Sum256(body)
		set.list = append(set.list, migration{version: version, sha256: hex.EncodeToString(sum[:])})
	}
	if len(set.list) == 0 {
		return nil, errors.New("store: no migrations")
	}
	return set, nil
}

// declaresNoTransaction reports a `-- +goose NO TRANSACTION` annotation by the
// rule goose parses it with: a comment line that holds +goose, minus the
// comment marks and that word, spells the annotation in any letter case.
func declaresNoTransaction(body []byte) bool {
	for line := range strings.SplitSeq(string(body), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") || !strings.Contains(line, "+goose") {
			continue
		}
		cmd := strings.ReplaceAll(line, "--", "")
		cmd = strings.Replace(cmd, "+goose", "", 1)
		if strings.EqualFold(strings.TrimSpace(cmd), "NO TRANSACTION") {
			return true
		}
	}
	return false
}

// hashStore is goose's SQLite version store plus the hash row: Insert runs in
// the transaction that applied the migration body, so the body, the goose
// version row and the hash row commit together or not at all.
type hashStore struct {
	database.Store
	hashes map[int64]string

	failAfterRecord func(version int64) error // test seam: runs after both rows are written; a returned error rolls the migration back
}

func newHashStore(set *migrationSet) (*hashStore, error) {
	store, err := database.NewStore(goose.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return nil, err
	}
	hs := &hashStore{Store: store, hashes: make(map[int64]string, len(set.list))}
	for _, m := range set.list {
		hs.hashes[m.version] = m.sha256
	}
	return hs, nil
}

// Insert records version and, for every version above goose's own zero row, the
// SHA-256 of its embedded file.
func (s *hashStore) Insert(ctx context.Context, db database.DBTxConn, req database.InsertRequest) error {
	if err := s.Store.Insert(ctx, db, req); err != nil {
		return err
	}
	if req.Version == 0 {
		return nil
	}
	sum, ok := s.hashes[req.Version]
	if !ok {
		return fmt.Errorf("store: no embedded migration %d to hash", req.Version)
	}
	dbtx, ok := db.(storedb.DBTX)
	if !ok {
		return fmt.Errorf("store: goose passed %T, which cannot run generated queries", db)
	}
	if err := storedb.New(dbtx).InsertSchemaHash(ctx, storedb.InsertSchemaHashParams{Version: req.Version, Sha256: sum}); err != nil {
		return fmt.Errorf("store: record hash of migration %d: %w", req.Version, err)
	}
	if s.failAfterRecord != nil {
		return s.failAfterRecord(req.Version)
	}
	return nil
}

// migrate brings the file to the newest embedded version. An empty file is
// migrated straight away. A file with tables is first inspected by read-only
// SQL: no goose method runs before the stored hashes verified and, when
// something is pending, BeforeMigrate returned.
func (d *DB) migrate(ctx context.Context, base driver.Connector, set *migrationSet, fresh bool, opts Options) (err error) {
	hs, err := newHashStore(set)
	if err != nil {
		return err
	}
	hs.failAfterRecord = opts.failAfterRecord
	var st fileState
	if !fresh {
		if st, err = d.inspect(ctx, set, hs); err != nil {
			return err
		}
		if !st.adopt && st.applied == set.max() {
			return nil
		}
		if opts.BeforeMigrate == nil {
			return ErrBackupRequired
		}
		if err := opts.BeforeMigrate(ctx, Pending{From: st.applied, To: set.max(), SnapshotInto: d.snapshotInto}); err != nil {
			return fmt.Errorf("store: before migrate: %w", err)
		}
	}

	// The provider owns its handle and closes it. SQL migrations need one connection.
	mdb := sql.OpenDB(&connector{base: base, want: newPolicy(0)})
	mdb.SetMaxOpenConns(1)
	p, err := goose.NewProvider(goose.DialectCustom, mdb, set.src, goose.WithStore(hs), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return errors.Join(fmt.Errorf("store: migrate: %w", err), mdb.Close())
	}
	defer func() { err = errors.Join(err, p.Close()) }()
	if st.adopt {
		if err := adopt(ctx, mdb, hs, st.hashDDL); err != nil {
			return err
		}
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// snapshotInto is Pending.SnapshotInto: VACUUM INTO on the writer's one
// connection, taken outside any transaction under the writer admission lease.
func (d *DB) snapshotInto(ctx context.Context, dest string) (err error) {
	if err := d.admit(ctx, nil); err != nil {
		return err
	}
	defer d.release()
	conn, err := d.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	_, err = conn.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}

// fileState is what the read-only inspection found.
type fileState struct {
	applied int64  // highest applied version, 0 for a predecessor file
	adopt   bool   // the file holds exactly the first migration's schema and no record of it
	hashDDL string // adopt only: the hash table's DDL, taken from the first migration
}

// inspect reads the format stamp, sqlite_schema, goose_db_version and
// boxset_schema_hash and creates nothing: goose's own methods create its
// version table, so none of them may run on a file that has not been backed up.
// The stamp must agree with the records: the legacy stamp belongs to a file
// with neither record table, the managed stamp to a file with both.
func (d *DB) inspect(ctx context.Context, set *migrationSet, hs *hashStore) (fileState, error) {
	var st fileState
	err := d.Read(ctx, func(tx *sql.Tx) error {
		var version int64
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		hasHash, hasGoose, err := recordTables(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case !hasHash && !hasGoose:
			if version != legacySchemaVersion {
				return fmt.Errorf("%w: file has user_version %d and no migration record, want the legacy %d", ErrSchemaMismatch, version, legacySchemaVersion)
			}
			return inspectPredecessor(ctx, tx, set, &st)
		case hasHash != hasGoose:
			return fmt.Errorf("%w: the file has only one of boxset_schema_hash and goose_db_version", ErrSchemaMismatch)
		case version != SchemaVersion:
			return fmt.Errorf("%w: file has migration records and user_version %d, want %d", ErrSchemaMismatch, version, SchemaVersion)
		}
		return verifyRecorded(ctx, tx, set, hs, &st)
	})
	return st, err
}

// gooseVersionTableSQL is the DDL goose's own SQLite store creates for its
// version table, read from a scratch in-memory database so it is never copied here.
func gooseVersionTableSQL(ctx context.Context) (ddl string, err error) {
	store, err := database.NewStore(goose.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return "", err
	}
	base, err := sqlite.NewConnector(":memory:")
	if err != nil {
		return "", err
	}
	scratch := sql.OpenDB(base)
	scratch.SetMaxOpenConns(1) // a second connection would be another, empty database
	defer func() { err = errors.Join(err, scratch.Close()) }()
	if err := store.CreateVersionTable(ctx, scratch); err != nil {
		return "", err
	}
	err = scratch.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE name = ?", goose.DefaultTablename).Scan(&ddl)
	return ddl, err
}

// interruptedInitialization reports nil only for the file a first migration
// leaves when it stops after goose committed its version table: goose's table
// with its canonical definition and exactly its one applied zero row (id 1,
// integer version 0, applied), and nothing else but that table's own sequence
// row (name goose_db_version, integer value 1). Every comparison is written so
// that a NULL or a value of another type counts as a difference, and the row
// counts are exact, so no duplicate or extra row, and no text stored in the
// sequence table, can pass. It reads only. Any other file that happens to use
// goose's names, with payload, history or another definition, is an error
// naming what differs.
func interruptedInitialization(ctx context.Context, q driver.QueryerContext) error {
	want, err := gooseVersionTableSQL(ctx)
	if err != nil {
		return err
	}
	checks := []struct {
		what, query string
		want        int64
	}{
		{"objects other than goose's version table", "SELECT count(*) FROM sqlite_schema WHERE name NOT IN ('goose_db_version', 'sqlite_sequence')", 0},
		{"no goose_db_version table", "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'goose_db_version'", 1},
		{"a version table with another definition", "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'goose_db_version' AND sql <> '" + strings.ReplaceAll(want, "'", "''") + "'", 0},
		{"a number of version rows other than one", "SELECT count(*) FROM goose_db_version", 1},
		{"a version row other than the applied zero version", "SELECT count(*) FROM goose_db_version WHERE NOT (id = 1 AND typeof(version_id) = 'integer' AND version_id = 0 AND typeof(is_applied) = 'integer' AND is_applied = 1)", 0},
		{"a number of sequence rows other than one", "SELECT count(*) FROM sqlite_sequence", 1},
		{"a sequence row other than goose's own with value 1", "SELECT count(*) FROM sqlite_sequence WHERE NOT (typeof(name) = 'text' AND name = 'goose_db_version' AND typeof(seq) = 'integer' AND seq = 1)", 0},
	}
	for _, c := range checks {
		n, err := queryInt(ctx, q, c.query)
		if err != nil {
			return err
		}
		if n != c.want {
			return fmt.Errorf("it has %s (%d)", c.what, n)
		}
	}
	return nil
}

func recordTables(ctx context.Context, tx *sql.Tx) (hasHash, hasGoose bool, err error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name IN ('boxset_schema_hash', 'goose_db_version')")
	if err != nil {
		return false, false, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, err
		}
		hasHash = hasHash || name == "boxset_schema_hash"
		hasGoose = hasGoose || name == goose.DefaultTablename
	}
	return hasHash, hasGoose, rows.Err()
}

// verifyRecorded checks the history a file records against the embedded
// migrations: nothing newer than the binary, and for every applied version the
// stored SHA-256 of the embedded file.
func verifyRecorded(ctx context.Context, tx *sql.Tx, set *migrationSet, hs *hashStore, st *fileState) error {
	stored, err := storedb.New(tx).ListSchemaHashes(ctx)
	if err != nil {
		return err
	}
	recorded, err := hs.ListMigrations(ctx, tx)
	if err != nil {
		return err
	}
	applied := map[int64]bool{}
	hashes := make(map[int64]string, len(stored))
	var top int64
	for _, m := range recorded {
		if m.IsApplied && m.Version > 0 {
			applied[m.Version] = true
			top = max(top, m.Version)
		}
	}
	for _, h := range stored {
		hashes[h.Version] = h.Sha256
		top = max(top, h.Version)
	}
	if top > set.max() {
		return fmt.Errorf("%w: the file records version %d, this binary embeds up to %d", ErrSchemaNewer, top, set.max())
	}
	for v := int64(1); v <= top; v++ {
		got, hashed := hashes[v]
		switch {
		case !applied[v] && !hashed:
			return fmt.Errorf("%w: migration %d is missing from the applied history", ErrSchemaMismatch, v)
		case !applied[v]:
			return fmt.Errorf("%w: migration %d has a stored hash but no applied version", ErrSchemaHash, v)
		case !hashed:
			return fmt.Errorf("%w: migration %d is applied without a stored hash", ErrSchemaHash, v)
		case got != hs.hashes[v]:
			return fmt.Errorf("%w: migration %d stored %s, embedded %s", ErrSchemaHash, v, got, hs.hashes[v])
		}
	}
	st.applied = top
	return nil
}

// inspectPredecessor accepts a file that has neither record table only when its
// objects are exactly the ones the first migration creates (the temporary
// bootstrap schema this migration replaced), so adopting it changes no
// existing object.
func inspectPredecessor(ctx context.Context, tx *sql.Tx, set *migrationSet, st *fileState) error {
	want, hashDDL, err := set.reference(ctx)
	if err != nil {
		return err
	}
	got, err := readCatalog(ctx, tx)
	if err != nil {
		return err
	}
	for i := range max(len(got), len(want)) {
		switch {
		case i >= len(got):
			return fmt.Errorf("%w: the file lacks %s %s of the first migration", ErrSchemaMismatch, want[i].typ, want[i].name)
		case i >= len(want):
			return fmt.Errorf("%w: the file holds %s %s that the first migration does not create", ErrSchemaMismatch, got[i].typ, got[i].name)
		case got[i] != want[i]:
			return fmt.Errorf("%w: the file's %s %s differs from the first migration's %s %s", ErrSchemaMismatch, got[i].typ, got[i].name, want[i].typ, want[i].name)
		}
	}
	st.adopt, st.hashDDL = true, hashDDL
	return nil
}

// adopt records the first migration of a predecessor file in one transaction:
// goose's version table with its zero row, the hash table, the managed format
// stamp and the rows for version 1, with the hash written by hashStore like
// any migration's. The stamp commits with the history or not at all, so the
// binary before migrations, which reads only the stamp, refuses the file the
// moment it carries records.
func adopt(ctx context.Context, db *sql.DB, hs *hashStore, hashDDL string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once Commit ran
	if err := hs.CreateVersionTable(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, hashDDL); err != nil {
		return fmt.Errorf("store: adopt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, stampManaged); err != nil {
		return fmt.Errorf("store: adopt: %w", err)
	}
	for _, v := range []int64{0, 1} {
		if err := hs.Insert(ctx, tx, database.InsertRequest{Version: v}); err != nil {
			return fmt.Errorf("store: adopt: %w", err)
		}
	}
	return tx.Commit()
}

// catalogEntry is one sqlite_schema row of the file's own objects.
type catalogEntry struct{ typ, name, table, sql string }

// readCatalog lists every application object, in a fixed order. It leaves out
// goose's table, the hash table and what SQLite itself maintains beside any
// schema: the sequence table of AUTOINCREMENT and the statistics tables that
// ANALYZE and PRAGMA optimize create, which are routine state of a healthy
// file and say nothing about the application's definitions.
func readCatalog(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (out []catalogEntry, err error) {
	rows, err := q.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema WHERE tbl_name NOT IN ('boxset_schema_hash', 'goose_db_version', 'sqlite_sequence', 'sqlite_stat1', 'sqlite_stat2', 'sqlite_stat3', 'sqlite_stat4') ORDER BY type, name")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var e catalogEntry
		if err := rows.Scan(&e.typ, &e.name, &e.table, &e.sql); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// reference applies the first migration to a scratch in-memory database and
// returns its objects and the DDL of its hash table. The migration file is
// the one description of the predecessor schema, so nothing is copied from it.
func (s *migrationSet) reference(ctx context.Context) (catalog []catalogEntry, hashDDL string, err error) {
	base, err := sqlite.NewConnector(":memory:")
	if err != nil {
		return nil, "", err
	}
	hs, err := newHashStore(s)
	if err != nil {
		return nil, "", err
	}
	rdb := sql.OpenDB(base)
	rdb.SetMaxOpenConns(1) // a second connection would be another, empty database
	p, err := goose.NewProvider(goose.DialectCustom, rdb, s.src, goose.WithStore(hs), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return nil, "", errors.Join(err, rdb.Close())
	}
	defer func() { err = errors.Join(err, p.Close()) }()
	if _, err := p.UpTo(ctx, 1); err != nil {
		return nil, "", fmt.Errorf("store: apply the first migration to a scratch database: %w", err)
	}
	if catalog, err = readCatalog(ctx, rdb); err != nil {
		return nil, "", err
	}
	if err := rdb.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE name = 'boxset_schema_hash'").Scan(&hashDDL); err != nil {
		return nil, "", fmt.Errorf("store: the first migration has no boxset_schema_hash table: %w", err)
	}
	return catalog, hashDDL, nil
}
