package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"modernc.org/sqlite"
)

// firstMigration is the one embedded migration, read from the source tree rather
// than through the embed the production code uses.
const firstMigration = "migrations/00001_library.sql"

// secondMigration is a later migration only these proofs apply.
const secondMigration = "-- +goose Up\nCREATE TABLE extra (id INTEGER PRIMARY KEY) STRICT;\n"

var errInjected = errors.New("injected failure")

// firstMigrationSet is the embedded first migration as a standalone migration
// set. It reads the source tree and needs no *testing.T, so the re-exec'd
// migration children use it too.
func firstMigrationSet() (fstest.MapFS, error) {
	body, err := os.ReadFile(firstMigration)
	if err != nil {
		return nil, err
	}
	return fstest.MapFS{"00001_library.sql": {Data: body}}, nil
}

// oneSet is firstMigrationSet for a test.
func oneSet(t *testing.T) fstest.MapFS {
	t.Helper()
	set, err := firstMigrationSet()
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// twoSet is the first migration plus secondMigration.
func twoSet(t *testing.T) fstest.MapFS {
	t.Helper()
	set := oneSet(t)
	set["00002_extra.sql"] = &fstest.MapFile{Data: []byte(secondMigration)}
	return set
}

// openSet is Open over an explicit migration set. The store is closed with the test.
func openSet(t *testing.T, path string, opts Options, src fs.FS) (*DB, error) {
	t.Helper()
	db, err := open(t.Context(), path, opts, src)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return db, err
}

func mustOpenSet(t *testing.T, path string, opts Options, src fs.FS) *DB {
	t.Helper()
	db, err := openSet(t, path, opts, src)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

// anyBackup is a BeforeMigrate hook that accepts and counts its calls.
func anyBackup(calls *int) func(context.Context, Pending) error {
	return func(context.Context, Pending) error {
		*calls++
		return nil
	}
}

// closeIt closes c and fails the test when it cannot.
func closeIt(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

// rawRows runs query on a plain driver connection and returns each row's columns joined by "|".
func rawRows(t *testing.T, path, query string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIt(t, raw)
	raw.SetMaxOpenConns(1)
	rows, err := raw.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer closeIt(t, rows)
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// catalogOf is the file's own objects, as the adoption check reads them.
func catalogOf(t *testing.T, path string) []catalogEntry {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIt(t, raw)
	got, err := readCatalog(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// tableNames lists every object name in the file.
func tableNames(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(rawRows(t, path, "SELECT name FROM sqlite_schema ORDER BY name"), ",")
}

func gooseVersions(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(rawRows(t, path, "SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY id"), ",")
}

func storedHashes(t *testing.T, path string) []string {
	t.Helper()
	return rawRows(t, path, "SELECT version, sha256 FROM boxset_schema_hash ORDER BY version")
}

func fileSHA(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// bootstrapV1 returns the bytes of the six predecessor schema files in the order the old bootstrap applied them.
func bootstrapV1(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("testdata/bootstrap-v1/*.sql")
	if err != nil || len(names) != 6 {
		t.Fatalf("bootstrap-v1 fixtures = %v (err %v), want six files", names, err)
	}
	slices.Sort(names)
	out := make([]string, len(names))
	for i, n := range names {
		body, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(body)
	}
	return out
}

// newPredecessor builds the file the temporary bootstrap produced: INCREMENTAL
// auto_vacuum, WAL, the six schema files in one go, user_version 1, no version record.
func newPredecessor(t *testing.T) string {
	t.Helper()
	path := newPath(t)
	stmts := append([]string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"}, bootstrapV1(t)...)
	rawExec(t, path, append(stmts, "PRAGMA user_version=1")...)
	return path
}

// populate writes rows through every predecessor table family the triggers touch.
func populate(t *testing.T, path string) {
	t.Helper()
	rawExec(t, path,
		"INSERT INTO title(id, metadata_id, kind, name) VALUES (1, 'tt1', 'series', 'alpha one')",
		"INSERT INTO library(id, name, default_monitored, default_naming_language, default_search_language, default_profile) VALUES (1, 'lib', 1, 'en', 'en', 'p')",
		"INSERT INTO slot(id, title_id, slot_key, label, profile, is_primary, owner, monitored, naming_language, search_language) VALUES (1, 1, 'main', 'Main', 'p', 1, 'local', 1, 'en', 'en')",
		"INSERT INTO alias_generation(id, dataset, state) VALUES (1, 'd', 'active')",
		"INSERT INTO title_alias(id, title_id, generation_id, alias) VALUES (1, 1, 1, 'bravo two')",
	)
}

func TestMigrationHashes(t *testing.T) {
	path := newPath(t)
	db := openStore(t, path, Options{})
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files = %v (err %v)", files, err)
	}
	slices.Sort(files)
	var wantRows []string
	wantVersions := []string{"0"}
	for i, f := range files {
		wantRows = append(wantRows, fmt.Sprintf("%d|%s", i+1, fileSHA(t, f)))
		wantVersions = append(wantVersions, fmt.Sprint(i+1))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := storedHashes(t, path); !slices.Equal(got, wantRows) {
		t.Errorf("stored hashes = %v, want the SHA-256 of each embedded file %v", got, wantRows)
	}
	if got := gooseVersions(t, path); got != strings.Join(wantVersions, ",") {
		t.Errorf("applied versions = %q, want %q", got, strings.Join(wantVersions, ","))
	}
	if got := rawRows(t, path, "PRAGMA user_version"); !slices.Equal(got, []string{fmt.Sprint(SchemaVersion)}) {
		t.Errorf("user_version = %v, want the managed format [%d]", got, SchemaVersion)
	}
	// An untouched file reopens with nothing pending, so it needs no backup hook.
	mustOpenSet(t, path, Options{}, mustSub(t))
}

func mustSub(t *testing.T) fs.FS {
	t.Helper()
	src, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// The first migration carries the six predecessor files byte for byte; only
// goose annotation lines, the hash table before them and the user_version stamp after are new.
func TestMigrationCarriesBootstrapBytes(t *testing.T) {
	body, err := os.ReadFile(firstMigration)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.SplitAfter(string(body), "\n") {
		if !strings.HasPrefix(line, "-- +goose ") {
			kept = append(kept, line)
		}
	}
	want := strings.Join(bootstrapV1(t), "\n")
	if !strings.Contains(strings.Join(kept, ""), want) {
		t.Fatal("the first migration, without goose annotations, does not contain the six bootstrap-v1 files byte for byte, in order")
	}
}

func TestMigrationHashMismatchRefused(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, path string) fs.FS // returns the set to reopen with
	}{
		{"stored hash edited", func(t *testing.T, path string) fs.FS {
			rawExec(t, path, "UPDATE boxset_schema_hash SET sha256 = '"+strings.Repeat("0", 64)+"' WHERE version = 1")
			return oneSet(t)
		}},
		{"stored hash deleted", func(t *testing.T, path string) fs.FS {
			rawExec(t, path, "DELETE FROM boxset_schema_hash")
			return oneSet(t)
		}},
		{"embedded file edited", func(t *testing.T, path string) fs.FS {
			set := oneSet(t)
			set["00001_library.sql"].Data = append(set["00001_library.sql"].Data, "\n-- edited after merge\n"...)
			return set
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newPath(t)
			closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
			src := tc.tamper(t, path)
			beforeNames, beforeRows := tableNames(t, path), storedHashes(t, path)
			calls := 0
			db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, src)
			if db != nil || !errors.Is(err, ErrSchemaHash) {
				t.Fatalf("open = %v, %v; want no DB and ErrSchemaHash", db, err)
			}
			if calls != 0 {
				t.Errorf("BeforeMigrate ran %d times before the hash refusal, want 0", calls)
			}
			if got := tableNames(t, path); got != beforeNames {
				t.Errorf("refusal changed the objects: %q, was %q", got, beforeNames)
			}
			if got := storedHashes(t, path); !slices.Equal(got, beforeRows) {
				t.Errorf("refusal changed the stored hashes: %v, was %v", got, beforeRows)
			}
		})
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := newPath(t)
	closeIt(t, mustOpenSet(t, path, Options{}, twoSet(t)))
	before := tableNames(t, path)
	calls := 0
	db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, oneSet(t))
	if db != nil || !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("open with the older binary = %v, %v; want no DB and ErrSchemaNewer", db, err)
	}
	if calls != 0 {
		t.Errorf("BeforeMigrate ran %d times before the newer-schema refusal, want 0", calls)
	}
	if got := tableNames(t, path); got != before {
		t.Errorf("refusal changed the objects: %q, was %q", got, before)
	}
	if got := gooseVersions(t, path); got != "0,1,2" {
		t.Errorf("applied versions after the refusal = %q, want 0,1,2", got)
	}
}

// Pending migrations of a file that has no goose_db_version are found by
// read-only SQL: at the hook, and after a refused hook, the file has no version table.
func TestPendingReadDoesNotCreateVersionTable(t *testing.T) {
	path := newPredecessor(t)
	populate(t, path)
	before := tableNames(t, path)
	var atHook string
	var pending Pending
	hook := func(_ context.Context, p Pending) error {
		pending = p
		atHook = tableNames(t, path)
		return errInjected
	}
	db, err := openSet(t, path, Options{BeforeMigrate: hook}, oneSet(t))
	if db != nil || !errors.Is(err, errInjected) {
		t.Fatalf("open with a refusing hook = %v, %v; want no DB and the hook's error", db, err)
	}
	if pending.From != 0 || pending.To != 1 || pending.SnapshotInto == nil {
		t.Errorf("Pending = {From %d To %d SnapshotInto nil:%v}, want From 0, To 1 and a snapshot function", pending.From, pending.To, pending.SnapshotInto == nil)
	}
	if atHook != before {
		t.Errorf("objects at the hook = %q, want the untouched %q", atHook, before)
	}
	if after := tableNames(t, path); after != before {
		t.Errorf("objects after the refused hook = %q, want %q: no version or hash table may exist", after, before)
	}
	for _, name := range []string{"goose_db_version", "boxset_schema_hash"} {
		if strings.Contains(tableNames(t, path), name) {
			t.Errorf("%s exists on a file whose pending migrations were only read", name)
		}
	}
}

func TestPendingWithoutHookRefused(t *testing.T) {
	t.Run("predecessor file", func(t *testing.T) {
		path := newPredecessor(t)
		before := tableNames(t, path)
		db, err := openSet(t, path, Options{}, oneSet(t))
		if db != nil || !errors.Is(err, ErrBackupRequired) {
			t.Fatalf("open without a hook = %v, %v; want no DB and ErrBackupRequired", db, err)
		}
		if got := tableNames(t, path); got != before {
			t.Errorf("refusal changed the objects: %q, was %q", got, before)
		}
	})
	t.Run("migrated file with a newer embedded migration", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
		before := tableNames(t, path)
		db, err := openSet(t, path, Options{}, twoSet(t))
		if db != nil || !errors.Is(err, ErrBackupRequired) {
			t.Fatalf("open without a hook = %v, %v; want no DB and ErrBackupRequired", db, err)
		}
		if got := tableNames(t, path); got != before {
			t.Errorf("refusal changed the objects: %q, was %q", got, before)
		}
	})
	t.Run("empty file needs no hook", func(t *testing.T) {
		mustOpenSet(t, newPath(t), Options{}, twoSet(t))
	})
}

// A failure injected after the migration body, version row and hash row were
// written rolls all three back: no table, version or hash row of the failed migration remains.
func TestMigrationAtomicCrash(t *testing.T) {
	failAt := func(version int64) func(int64) error {
		return func(v int64) error {
			if v == version {
				return errInjected
			}
			return nil
		}
	}

	t.Run("later migration", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
		beforeNames, beforeRows, beforeVersions := tableNames(t, path), storedHashes(t, path), gooseVersions(t, path)
		calls := 0
		db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls), failAfterRecord: failAt(2)}, twoSet(t))
		if db != nil || !errors.Is(err, errInjected) {
			t.Fatalf("open = %v, %v; want no DB and the injected failure", db, err)
		}
		if calls != 1 {
			t.Errorf("BeforeMigrate ran %d times, want 1", calls)
		}
		if got := tableNames(t, path); got != beforeNames {
			t.Errorf("objects after the failed migration = %q, want %q (no table from the body)", got, beforeNames)
		}
		if got := storedHashes(t, path); !slices.Equal(got, beforeRows) {
			t.Errorf("hash rows = %v, want %v", got, beforeRows)
		}
		if got := gooseVersions(t, path); got != beforeVersions {
			t.Errorf("applied versions = %q, want %q", got, beforeVersions)
		}
		// The same file migrates once the failure is gone.
		mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, twoSet(t))
		if got := gooseVersions(t, path); got != "0,1,2" {
			t.Errorf("applied versions after the retry = %q, want 0,1,2", got)
		}
		if got := rawRows(t, path, "SELECT count(*) FROM sqlite_schema WHERE name = 'extra'"); got[0] != "1" {
			t.Errorf("table extra after the retry: count %v, want 1", got)
		}
	})

	t.Run("first migration of an empty file", func(t *testing.T) {
		path := newPath(t)
		db, err := openSet(t, path, Options{failAfterRecord: failAt(1)}, oneSet(t))
		if db != nil || !errors.Is(err, errInjected) {
			t.Fatalf("open = %v, %v; want no DB and the injected failure", db, err)
		}
		// goose commits its own version table before the first migration; nothing of the migration remains.
		if got := tableNames(t, path); got != "goose_db_version,sqlite_sequence" {
			t.Errorf("objects after the failed first migration = %q, want only goose's version table", got)
		}
		if got := gooseVersions(t, path); got != "0" {
			t.Errorf("applied versions = %q, want only goose's zero row", got)
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != "0" {
			t.Errorf("user_version = %v, want 0: the stamp is part of the rolled-back body", got)
		}
		// Not bricked: the next Open completes the first migration.
		mustOpenSet(t, path, Options{}, oneSet(t))
		if got := gooseVersions(t, path); got != "0,1" {
			t.Errorf("applied versions after the retry = %q, want 0,1", got)
		}
		if got := storedHashes(t, path); len(got) != 1 {
			t.Errorf("hash rows after the retry = %v, want one", got)
		}
	})

	t.Run("adoption of a predecessor file", func(t *testing.T) {
		path := newPredecessor(t)
		populate(t, path)
		before := tableNames(t, path)
		calls := 0
		db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls), failAfterRecord: failAt(1)}, oneSet(t))
		if db != nil || !errors.Is(err, errInjected) {
			t.Fatalf("open = %v, %v; want no DB and the injected failure", db, err)
		}
		if got := tableNames(t, path); got != before {
			t.Errorf("objects after the failed adoption = %q, want the untouched %q", got, before)
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != "1" {
			t.Errorf("user_version after the failed adoption = %v, want the legacy 1: the stamp commits with the history", got)
		}
		mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, oneSet(t))
		if got := gooseVersions(t, path); got != "0,1" {
			t.Errorf("applied versions after the retry = %q, want 0,1", got)
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != fmt.Sprint(SchemaVersion) {
			t.Errorf("user_version after the adoption = %v, want the managed %d", got, SchemaVersion)
		}
	})
}

func TestNoTransactionRejected(t *testing.T) {
	for _, annotation := range []string{"-- +goose NO TRANSACTION", "-- +goose no transaction", "--+goose  NO TRANSACTION "} {
		t.Run(annotation, func(t *testing.T) {
			path := newPath(t)
			src := fstest.MapFS{"00001_x.sql": {Data: []byte("-- +goose Up\n" + annotation + "\nCREATE TABLE t (x INTEGER) STRICT;\n")}}
			db, err := openSet(t, path, Options{}, src)
			if db != nil || !errors.Is(err, errNoTransaction) {
				t.Fatalf("open = %v, %v; want no DB and errNoTransaction", db, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a refused migration set still created the file: stat err %v", err)
			}
		})
	}
	// A comment that is not a goose annotation is not refused.
	ok := fstest.MapFS{"00001_x.sql": {Data: []byte("-- +goose Up\n-- NO TRANSACTION is mentioned here\nCREATE TABLE t (x INTEGER) STRICT;\n")}}
	if _, err := loadMigrations(ok); err != nil {
		t.Errorf("loadMigrations on a plain comment = %v, want nil", err)
	}
}

func TestMigrationFileNames(t *testing.T) {
	body := &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")}
	bad := map[string]fstest.MapFS{
		"gap":         {"00002_a.sql": body},
		"short":       {"1_a.sql": body},
		"not digits":  {"0000a_a.sql": body},
		"no name":     {"00001.sql": body},
		"duplicate":   {"00001_a.sql": body, "00001_b.sql": body},
		"second gap":  {"00001_a.sql": body, "00003_c.sql": body},
		"empty":       {},
		"only others": {"README.md": body},
	}
	for name, src := range bad {
		if set, err := loadMigrations(src); err == nil {
			t.Errorf("%s: loadMigrations = %+v, want an error", name, set)
		}
	}
	set, err := loadMigrations(fstest.MapFS{"00001_a.sql": body, "00002_b.sql": body, "notes.txt": body})
	if err != nil || set.max() != 2 {
		t.Fatalf("loadMigrations of a clean set = %+v, %v; want two migrations", set, err)
	}
}

// Auto-vacuum is fixed in the bootstrap, before goose creates its version table
// or any table exists.
func TestFreshAutoVacuumBeforeVersionTable(t *testing.T) {
	path := newPath(t)
	base, err := sqlite.NewConnector(path + "?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := bootstrap(t.Context(), base)
	if err != nil || !fresh {
		t.Fatalf("bootstrap = %v, %v; want a fresh file", fresh, err)
	}
	if got := rawRows(t, path, "PRAGMA auto_vacuum"); got[0] != "2" {
		t.Errorf("auto_vacuum before any table = %v, want 2", got)
	}
	if got := rawRows(t, path, "PRAGMA journal_mode"); got[0] != "wal" {
		t.Errorf("journal_mode before any table = %v, want wal", got)
	}
	if got := tableNames(t, path); got != "" {
		t.Errorf("objects after the bootstrap = %q, want none: goose_db_version must not exist yet", got)
	}

	mustOpenSet(t, path, Options{}, oneSet(t))
	if !strings.Contains(tableNames(t, path), "goose_db_version") {
		t.Error("the migration did not create goose_db_version")
	}
	if got := rawRows(t, path, "PRAGMA auto_vacuum"); got[0] != "2" {
		t.Errorf("auto_vacuum after the migration = %v, want 2", got)
	}
}

// policyProbe is a migration that records the settings of the connection the
// migration runner gave it, so the proof reads what the provider's own
// connection carried rather than a separately constructed one.
const policyProbe = `-- +goose Up
CREATE TABLE migration_policy AS SELECT
    (SELECT journal_size_limit FROM pragma_journal_size_limit) AS journal_size_limit,
    (SELECT foreign_keys FROM pragma_foreign_keys) AS foreign_keys,
    (SELECT synchronous FROM pragma_synchronous) AS synchronous,
    (SELECT timeout FROM pragma_busy_timeout) AS busy_timeout,
    (SELECT cache_size FROM pragma_cache_size) AS cache_size,
    (SELECT query_only FROM pragma_query_only) AS query_only,
    (SELECT auto_vacuum FROM pragma_auto_vacuum) AS auto_vacuum;
`

// Every physical connection the store opens reads journal_size_limit back as
// 64 MiB: both pools under connection churn, and the connection a migration
// body itself runs on.
func TestJournalSizeLimitEveryConnection(t *testing.T) {
	const want = "67108864" // 64 MiB, independent of newPolicy
	path := newPath(t)
	db := openStore(t, path, Options{ConnMaxLifetime: time.Millisecond})
	for range 3*ReaderCount() + 3 {
		for _, writer := range []bool{false, true} {
			fn := func(tx *sql.Tx) error {
				var got string
				if err := tx.QueryRow("PRAGMA journal_size_limit").Scan(&got); err != nil {
					return err
				}
				if got != want {
					t.Errorf("writer=%v: journal_size_limit = %s, want %s", writer, got, want)
				}
				return nil
			}
			var err error
			if writer {
				err = db.Write(t.Context(), fn)
			} else {
				err = db.Read(t.Context(), fn)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(5 * time.Millisecond) // retire the pooled connections: every round dials new physical ones
	}
	if st := db.Stats(); st.WriterConnects <= 1 || st.ReaderConnects <= int64(ReaderCount()) {
		t.Errorf("connects writer %d reader %d: churn did not open new physical connections", st.WriterConnects, st.ReaderConnects)
	}

	// The migration connection, on a fresh file and on an adopted predecessor.
	for name, newFile := range map[string]func(*testing.T) string{
		"fresh file":       newPath,
		"predecessor file": newPredecessor,
	} {
		t.Run(name, func(t *testing.T) {
			path := newFile(t)
			src := oneSet(t)
			src["00002_policy.sql"] = &fstest.MapFile{Data: []byte(policyProbe)}
			calls := 0
			db := mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, src)
			recorded := map[string]string{}
			if err := db.Read(t.Context(), func(tx *sql.Tx) error {
				var jsl, fk, sync, busy, cache, qo, av string
				if err := tx.QueryRow("SELECT journal_size_limit, foreign_keys, synchronous, busy_timeout, cache_size, query_only, auto_vacuum FROM migration_policy").
					Scan(&jsl, &fk, &sync, &busy, &cache, &qo, &av); err != nil {
					return err
				}
				recorded = map[string]string{"journal_size_limit": jsl, "foreign_keys": fk, "synchronous": sync, "busy_timeout": busy, "cache_size": cache, "query_only": qo, "auto_vacuum": av}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := recorded["journal_size_limit"]; got != want {
				t.Errorf("migration connection journal_size_limit = %s, want %s", got, want)
			}
			// The migration connection is a writer: the rest of the writer policy, from literals.
			wantWriter := wantSettings("0")
			for name, got := range recorded {
				if w := wantWriter[name]; got != w {
					t.Errorf("migration connection %s = %s, want %s", name, got, w)
				}
			}
		})
	}
}

func TestPredecessorFileAdopted(t *testing.T) {
	path := newPredecessor(t)
	populate(t, path)
	before := catalogOf(t, path)

	var pending []Pending
	snapshot := filepath.Join(t.TempDir(), "pre-migration.db")
	hook := func(ctx context.Context, p Pending) error {
		pending = append(pending, p)
		if err := p.SnapshotInto(ctx, snapshot); err != nil {
			return err
		}
		if got := tableNames(t, snapshot); strings.Contains(got, "goose_db_version") || strings.Contains(got, "boxset_schema_hash") {
			t.Errorf("the snapshot taken at the hook already has migration tables: %q", got)
		}
		return nil
	}
	db := mustOpenSet(t, path, Options{BeforeMigrate: hook}, oneSet(t))
	if len(pending) != 1 || pending[0].From != 0 || pending[0].To != 1 {
		t.Fatalf("Pending calls = %+v, want one {From 0 To 1}", pending)
	}
	if got := rawRows(t, snapshot, "SELECT count(*) FROM title"); got[0] != "1" {
		t.Errorf("snapshot title rows = %v, want the populated 1", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Version 1 and its hash were recorded; nothing existing changed.
	if got, want := storedHashes(t, path), []string{"1|" + fileSHA(t, firstMigration)}; !slices.Equal(got, want) {
		t.Errorf("stored hashes = %v, want %v", got, want)
	}
	if got := gooseVersions(t, path); got != "0,1" {
		t.Errorf("applied versions = %q, want 0,1", got)
	}
	if got := rawRows(t, path, "PRAGMA user_version"); got[0] != fmt.Sprint(SchemaVersion) {
		t.Errorf("user_version after the adoption = %v, want the managed %d", got, SchemaVersion)
	}
	after := catalogOf(t, path)
	if !slices.Equal(before, after) {
		t.Errorf("sqlite_schema changed by the adoption:\nbefore %+v\nafter  %+v", before, after)
	}
	// Schema equivalence: a freshly migrated file has the same objects.
	freshPath := newPath(t)
	closeIt(t, mustOpenSet(t, freshPath, Options{}, oneSet(t)))
	if fresh := catalogOf(t, freshPath); !slices.Equal(fresh, after) {
		t.Errorf("adopted file's sqlite_schema differs from a freshly migrated file's")
	}
	if got, want := rawRows(t, path, "SELECT name FROM sqlite_schema WHERE name IN ('goose_db_version', 'boxset_schema_hash') ORDER BY name"), []string{"boxset_schema_hash", "goose_db_version"}; !slices.Equal(got, want) {
		t.Errorf("record tables = %v, want %v", got, want)
	}

	// Retained data, and the triggers still work on it.
	for query, want := range map[string]string{
		"SELECT count(*) FROM title":                                         "1",
		"SELECT count(*) FROM title_rollup":                                  "1",
		"SELECT count(*) FROM title_search WHERE title_search MATCH 'alp'":   "1",
		"SELECT count(*) FROM title_search WHERE title_search MATCH 'bravo'": "1",
		"SELECT alias_count FROM alias_generation WHERE id = 1":              "1",
		"SELECT count(*) FROM pragma_foreign_key_check":                      "0",
		"SELECT integrity_check FROM pragma_integrity_check":                 "ok",
		"SELECT count(*) FROM slot WHERE owner = 'local' AND is_primary = 1": "1",
	} {
		if got := rawRows(t, path, query); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %s", query, got, want)
		}
	}

	// Reopening needs no hook: nothing is pending.
	reopened := mustOpenSet(t, path, Options{}, oneSet(t))
	if err := reopened.Read(t.Context(), func(tx *sql.Tx) error {
		if n := rowCount(t, tx, "title"); n != 1 {
			t.Errorf("reopened title rows = %d, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A predecessor file whose binary also embeds a later migration is adopted at
// version 1 and then migrated, behind one hook call.
func TestPredecessorFileAdoptedThenMigrated(t *testing.T) {
	path := newPredecessor(t)
	populate(t, path)
	var pending []Pending
	hook := func(_ context.Context, p Pending) error {
		pending = append(pending, p)
		return nil
	}
	closeIt(t, mustOpenSet(t, path, Options{BeforeMigrate: hook}, twoSet(t)))
	if len(pending) != 1 || pending[0].From != 0 || pending[0].To != 2 {
		t.Fatalf("Pending calls = %+v, want one {From 0 To 2}", pending)
	}
	if got := gooseVersions(t, path); got != "0,1,2" {
		t.Errorf("applied versions = %q, want 0,1,2", got)
	}
	want := []string{"1|" + fileSHA(t, firstMigration), "2|" + hashOf(secondMigration)}
	if got := storedHashes(t, path); !slices.Equal(got, want) {
		t.Errorf("stored hashes = %v, want %v", got, want)
	}
	if got := rawRows(t, path, "SELECT count(*) FROM title"); got[0] != "1" {
		t.Errorf("title rows = %v, want 1", got)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A file with tables, no record tables and objects other than the first
// migration's is neither a predecessor file nor adoptable.
func TestForeignFileRefused(t *testing.T) {
	cases := map[string][]string{
		"extra table": {"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal", "CREATE TABLE t (x)", "PRAGMA user_version=1"},
		"missing table": append(append([]string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"}, bootstrapV1(t)...),
			"DROP TABLE persona", "PRAGMA user_version=1"),
		"only one record table": {"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal", "CREATE TABLE boxset_schema_hash (version INTEGER PRIMARY KEY, sha256 TEXT)", "PRAGMA user_version=1"},
	}
	for name, stmts := range cases {
		t.Run(name, func(t *testing.T) {
			path := newPath(t)
			rawExec(t, path, stmts...)
			before := tableNames(t, path)
			calls := 0
			db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, oneSet(t))
			if db != nil || !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("open = %v, %v; want no DB and ErrSchemaMismatch", db, err)
			}
			if calls != 0 {
				t.Errorf("BeforeMigrate ran %d times for a file that is not adoptable", calls)
			}
			if got := tableNames(t, path); got != before {
				t.Errorf("refusal changed the objects: %q, was %q", got, before)
			}
		})
	}
}
