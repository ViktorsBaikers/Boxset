package store

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// Edge proofs for the migration runner: the refusals it owes a file it does
// not recognize, history it cannot trust or a format it must not share with the
// binary before it, and what a killed process leaves behind.

const (
	migrationChildScenario = "BOXSET_STORE_MIGRATION_CHILD"
	migrationChildDB       = "BOXSET_STORE_MIGRATION_DB"
	migrationReady         = "READY "
)

// TestMain turns the test binary into the migration child when the parent asks.
func TestMain(m *testing.M) {
	if scenario := os.Getenv(migrationChildScenario); scenario != "" {
		os.Exit(migrationChild(scenario))
	}
	os.Exit(m.Run())
}

// migrationChild opens the file at migrationChildDB through the real runner and,
// inside the migration's transaction once the migration body, version row and
// hash row are written, reports READY and waits to be killed. "first" migrates
// an empty file, "later" a migrated file to version 2, "adopt" a predecessor file.
func migrationChild(scenario string) int {
	set, err := firstMigrationSet()
	if err != nil {
		fmt.Fprint(os.Stderr, "child set: "+err.Error()+"\n")
		return 2
	}
	target := int64(1)
	switch scenario {
	case "first", "adopt":
	case "later":
		set["00002_extra.sql"] = &fstest.MapFile{Data: []byte(secondMigration)}
		target = 2
	default:
		fmt.Fprint(os.Stderr, "unknown migration scenario "+scenario+"\n")
		return 2
	}
	opts := Options{
		BeforeMigrate: func(context.Context, Pending) error { return nil },
		failAfterRecord: func(version int64) error {
			if version != target {
				return nil
			}
			if _, err := os.Stdout.WriteString(migrationReady + scenario + "\n"); err != nil {
				fmt.Fprint(os.Stderr, "child ready: "+err.Error()+"\n")
				os.Exit(2)
			}
			time.Sleep(2 * time.Minute) // the parent kills first; this only bounds an orphan
			return errors.New("migration child was not killed")
		},
	}
	_, err = open(context.Background(), os.Getenv(migrationChildDB), opts, set)
	fmt.Fprint(os.Stderr, "child open ended: "+fmt.Sprint(err)+"\n")
	return 3
}

// capWriter keeps the first max bytes written to it and discards the rest.
type capWriter struct {
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// killMigrationChild runs scenario in a child on the file at path, waits until
// the child reports its migration transaction written and still open, then kills
// the child's process group. The child's environment is closed to its two
// inputs, its output is capped, and a deadline bounds it.
func killMigrationChild(t *testing.T, scenario, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = []string{migrationChildScenario + "=" + scenario, migrationChildDB + "=" + path}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stderr := &capWriter{max: 4 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	line, readErr := bufio.NewReader(io.LimitReader(stdout, 1<<10)).ReadString('\n')
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("child did not die by signal: %v", waitErr)
	}
	if readErr != nil || line != migrationReady+scenario+"\n" {
		t.Fatalf("child never reported its open migration transaction: line %q err %v stderr %q", line, readErr, stderr.buf)
	}
}

// stateDigest is a digest of everything a file holds: its format stamp, every
// schema object and every row of every table. Two digests are equal exactly when
// the file's logical state is.
func stateDigest(t *testing.T, path string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "user_version=%v\n", rawRows(t, path, "PRAGMA user_version"))
	for _, line := range rawRows(t, path, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY type, name") {
		b.WriteString("object " + line + "\n")
	}
	for _, name := range rawRows(t, path, "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name") {
		rows := rawRows(t, path, fmt.Sprintf("SELECT * FROM %q", name))
		slices.Sort(rows)
		for _, r := range rows {
			b.WriteString("row " + name + " " + r + "\n")
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// legacyFile is a predecessor file built from the given schema file texts the way the temporary bootstrap built it.
func legacyFile(t *testing.T, files []string) string {
	t.Helper()
	path := newPath(t)
	stmts := append([]string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"}, files...)
	rawExec(t, path, append(stmts, "PRAGMA user_version=1")...)
	return path
}

// failAt is a failAfterRecord that fails the migration of version.
func failAt(version int64) func(int64) error {
	return func(v int64) error {
		if v == version {
			return errInjected
		}
		return nil
	}
}

// interruptedFirstMigration is the file a first migration leaves behind when it
// fails after goose created its version table: goose's table with its zero row and nothing else.
func interruptedFirstMigration(t *testing.T) string {
	t.Helper()
	path := newPath(t)
	if db, err := openSet(t, path, Options{failAfterRecord: failAt(1)}, oneSet(t)); db != nil || !errors.Is(err, errInjected) {
		t.Fatalf("fixture: open = %v, %v; want no DB and the injected failure", db, err)
	}
	if got := tableNames(t, path); got != "goose_db_version,sqlite_sequence" {
		t.Fatalf("fixture: objects = %q, want only goose's version table", got)
	}
	return path
}

// openRefused opens path over src with a counting hook and requires want as the
// only migration sentinel in the error, no DB, no hook call and no change to the file.
func openRefused(t *testing.T, path string, src fs.FS, want error) {
	t.Helper()
	before := stateDigest(t, path)
	calls := 0
	db, err := openSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, src)
	if db != nil {
		t.Fatalf("open returned a usable DB over a file that must be refused (err %v)", err)
	}
	for _, sentinel := range []error{ErrSchemaHash, ErrSchemaMismatch, ErrSchemaNewer, ErrBackupRequired, ErrPolicyMismatch} {
		if got := errors.Is(err, sentinel); got != (sentinel == want) {
			t.Errorf("open error %v: Is(%v) = %v, want %v", err, sentinel, got, sentinel == want)
		}
	}
	if calls != 0 {
		t.Errorf("BeforeMigrate ran %d times for a refused file, want 0", calls)
	}
	if after := stateDigest(t, path); after != before {
		t.Errorf("a refused open changed the file")
	}
}

// F11: a file is fresh only when it is empty or exactly the interrupted first
// migration; every other file that carries goose's table names is refused with
// no backup skipped, no persistent write and no usable DB.
func TestFreshClassification(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) string
		want  error
	}{
		{"payload beside goose's zero row", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "CREATE TABLE user_data(note TEXT)", "INSERT INTO user_data VALUES ('keep me')")
			return path
		}, ErrSchemaMismatch},
		{"recorded version 1 without the schema", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO goose_db_version(version_id, is_applied) VALUES (1, 1)")
			return path
		}, ErrSchemaMismatch},
		{"recorded versions 1 and a newer 2 without the schema", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO goose_db_version(version_id, is_applied) VALUES (1, 1)", "INSERT INTO goose_db_version(version_id, is_applied) VALUES (2, 1)")
			return path
		}, ErrSchemaMismatch},
		{"a second zero row", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO goose_db_version(version_id, is_applied) VALUES (0, 1)")
			return path
		}, ErrSchemaMismatch},
		{"a zero row that is not applied", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "UPDATE goose_db_version SET is_applied = 0")
			return path
		}, ErrSchemaMismatch},
		{"a second sequence row under goose's name holding text", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO sqlite_sequence(name, seq) VALUES ('goose_db_version', 'payload text')")
			return path
		}, ErrSchemaMismatch},
		{"a sequence row with a NULL name holding text", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO sqlite_sequence(name, seq) VALUES (NULL, 'secret note')")
			return path
		}, ErrSchemaMismatch},
		{"goose's sequence value replaced by text", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "UPDATE sqlite_sequence SET seq = 'secret note'")
			return path
		}, ErrSchemaMismatch},
		{"goose's sequence value not 1", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "UPDATE sqlite_sequence SET seq = 9")
			return path
		}, ErrSchemaMismatch},
		{"a sequence row of another table", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "INSERT INTO sqlite_sequence(name, seq) VALUES ('notes', 7)")
			return path
		}, ErrSchemaMismatch},
		{"no sequence row", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "DELETE FROM sqlite_sequence")
			return path
		}, ErrSchemaMismatch},
		{"a version row with another id", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "UPDATE goose_db_version SET id = 7")
			return path
		}, ErrSchemaMismatch},
		{"goose's table with another definition", func(t *testing.T) string {
			path := newPath(t)
			rawExec(t, path, "PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal",
				"CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')), note TEXT)",
				"INSERT INTO goose_db_version(version_id, is_applied) VALUES (0, 1)")
			return path
		}, ErrSchemaMismatch},
		{"an application file with user data", func(t *testing.T) string {
			path := newPath(t)
			rawExec(t, path, "CREATE TABLE app(x)", "INSERT INTO app VALUES (1)")
			return path
		}, ErrSchemaMismatch},
		{"an empty file with a format stamp", func(t *testing.T) string {
			path := newPath(t)
			rawExec(t, path, "PRAGMA user_version=5")
			return path
		}, ErrSchemaMismatch},
		{"the interrupted first migration outside the bootstrap policy", func(t *testing.T) string {
			path := interruptedFirstMigration(t)
			rawExec(t, path, "PRAGMA journal_mode=delete")
			return path
		}, ErrPolicyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t)
			modeBefore := rawRows(t, path, "PRAGMA journal_mode")
			vacuumBefore := rawRows(t, path, "PRAGMA auto_vacuum")
			openRefused(t, path, oneSet(t), tc.want)
			if got := rawRows(t, path, "PRAGMA journal_mode"); !slices.Equal(got, modeBefore) {
				t.Errorf("journal_mode %v after the refusal, was %v: a refused file got a persistent PRAGMA write", got, modeBefore)
			}
			if got := rawRows(t, path, "PRAGMA auto_vacuum"); !slices.Equal(got, vacuumBefore) {
				t.Errorf("auto_vacuum %v after the refusal, was %v: a refused file got a persistent PRAGMA write", got, vacuumBefore)
			}
		})
	}

	// A malformed goose table in DELETE journal mode is the sharpest case: the
	// old bootstrap converted the file to WAL before failing. Now not one byte changes.
	t.Run("malformed goose table keeps its bytes and journal mode", func(t *testing.T) {
		path := newPath(t)
		rawExec(t, path, "CREATE TABLE goose_db_version (x)")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		openRefused(t, path, oneSet(t), ErrSchemaMismatch)
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("the database file's bytes changed under a refused open")
		}
		if got := rawRows(t, path, "PRAGMA journal_mode"); got[0] != "delete" {
			t.Errorf("journal_mode = %v, want delete: no WAL conversion before a refusal", got)
		}
		if _, err := os.Stat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a refused open left a WAL file: stat err %v", err)
		}
	})

	// The legitimate retry is unchanged: the interrupted first migration needs no
	// backup, completes, and an empty file still bootstraps.
	t.Run("interrupted first migration retries without a hook", func(t *testing.T) {
		path := interruptedFirstMigration(t)
		mustOpenSet(t, path, Options{}, oneSet(t))
		if got := gooseVersions(t, path); got != "0,1" {
			t.Errorf("applied versions after the retry = %q, want 0,1", got)
		}
		if got := rawRows(t, path, "SELECT name || '=' || seq FROM sqlite_sequence"); !slices.Equal(got, []string{"goose_db_version=2"}) {
			t.Errorf("sequence rows after the retry = %v, want only goose's own advanced to 2", got)
		}
	})

	// The bypass this closes: text stored in the sequence table under goose's
	// name was accepted as an interrupted first migration, no backup hook ran, and
	// goose's own autoincrement then overwrote the stored text. The refused open
	// must leave every sequence row and its text exactly as it was.
	t.Run("text in the sequence table is neither accepted nor overwritten", func(t *testing.T) {
		path := interruptedFirstMigration(t)
		rawExec(t, path, "INSERT INTO sqlite_sequence(name, seq) VALUES ('goose_db_version', 'payload text')", "INSERT INTO sqlite_sequence(name, seq) VALUES (NULL, 'secret note')")
		want := rawRows(t, path, "SELECT coalesce(name, 'NULL') || '=' || seq FROM sqlite_sequence ORDER BY rowid")
		if len(want) != 3 {
			t.Fatalf("fixture: sequence rows = %v, want three", want)
		}
		openRefused(t, path, oneSet(t), ErrSchemaMismatch)
		if got := rawRows(t, path, "SELECT coalesce(name, 'NULL') || '=' || seq FROM sqlite_sequence ORDER BY rowid"); !slices.Equal(got, want) {
			t.Errorf("sequence rows after the refusal = %v, want the untouched %v", got, want)
		}
	})
}

// F10: the managed format is refused by the binary that preceded migrations.
func TestManagedFormatRefusedByPredecessor(t *testing.T) {
	// predecessorAdmits is the admission rule of the pre-migration binary (base
	// commit 8f122d5, internal/store/store.go bootstrap), transcribed: a file with
	// any schema object must say user_version 1, and nothing else about its history is read.
	predecessorAdmits := func(t *testing.T, path string) error {
		t.Helper()
		if objects := rawRows(t, path, "SELECT count(*) FROM sqlite_schema"); objects[0] == "0" {
			return nil // it would bootstrap a fresh file
		}
		if v := rawRows(t, path, "PRAGMA user_version"); v[0] != "1" {
			return fmt.Errorf("%w: file has user_version %s, want 1", ErrSchemaMismatch, v[0])
		}
		return nil
	}

	if SchemaVersion == legacySchemaVersion || legacySchemaVersion != 1 || SchemaVersion != 2 {
		t.Fatalf("formats: managed %d, legacy %d; want managed 2 and the predecessor's literal 1", SchemaVersion, legacySchemaVersion)
	}

	t.Run("freshly migrated file", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, mustSub(t)))
		if err := predecessorAdmits(t, path); !errors.Is(err, ErrSchemaMismatch) {
			t.Fatalf("the predecessor's rule on a migrated file = %v, want it refused", err)
		}
	})
	t.Run("adopted predecessor file", func(t *testing.T) {
		path := newPredecessor(t)
		populate(t, path)
		if err := predecessorAdmits(t, path); err != nil {
			t.Fatalf("control: the predecessor's rule on the legacy file = %v, want admitted", err)
		}
		calls := 0
		closeIt(t, mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, mustSub(t)))
		if err := predecessorAdmits(t, path); !errors.Is(err, ErrSchemaMismatch) {
			t.Fatalf("the predecessor's rule on an adopted file = %v, want it refused", err)
		}
		if got := rawRows(t, path, "SELECT count(*) FROM title"); got[0] != "1" {
			t.Errorf("adopted title rows = %v, want the retained 1", got)
		}
	})
	t.Run("migration records under the legacy stamp", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
		rawExec(t, path, "PRAGMA user_version=1")
		openRefused(t, path, oneSet(t), ErrSchemaMismatch)
	})
	t.Run("legacy schema under the managed stamp", func(t *testing.T) {
		path := newPredecessor(t)
		rawExec(t, path, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion))
		openRefused(t, path, oneSet(t), ErrSchemaMismatch)
	})
	t.Run("managed stamp without any record", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
		rawExec(t, path, "DROP TABLE boxset_schema_hash", "DROP TABLE goose_db_version")
		openRefused(t, path, oneSet(t), ErrSchemaMismatch)
	})
}

// F12: SQLite's own statistics are routine maintenance state of a predecessor
// file, never application drift. They stay, with the data; a changed definition is still refused.
func TestAdoptionKeepsStatistics(t *testing.T) {
	path := newPredecessor(t)
	populate(t, path)
	rawExec(t, path, "ANALYZE")
	stats := func() []string {
		return rawRows(t, path, "SELECT tbl, coalesce(idx, ''), stat FROM sqlite_stat1 ORDER BY tbl, idx")
	}
	before := stats()
	if len(before) == 0 {
		t.Fatal("fixture: ANALYZE left no statistics")
	}
	titleBefore := rawRows(t, path, "SELECT id, metadata_id, kind, name FROM title ORDER BY id")

	calls := 0
	db := mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, oneSet(t))
	if calls != 1 {
		t.Errorf("BeforeMigrate ran %d times, want 1", calls)
	}
	closeIt(t, db)
	if got := stats(); !slices.Equal(got, before) {
		t.Errorf("statistics changed by the adoption:\n got %v\nwant %v", got, before)
	}
	if got := rawRows(t, path, "SELECT id, metadata_id, kind, name FROM title ORDER BY id"); !slices.Equal(got, titleBefore) {
		t.Errorf("application rows changed by the adoption: %v, want %v", got, titleBefore)
	}
	if got := gooseVersions(t, path); got != "0,1" {
		t.Errorf("applied versions = %q, want 0,1", got)
	}
	closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t))) // nothing pending: no hook
}

// F06: a predecessor whose object names and count match but whose definition
// differs is not adopted, with or without SQLite statistics beside it.
func TestAlteredDefinitionRefused(t *testing.T) {
	type mutation struct {
		name     string
		file     int // index into the sorted bootstrap-v1 files
		old, new string
	}
	mutations := []mutation{
		{"check constraint widened", 0, "CHECK (kind IN ('movie', 'series'))", "CHECK (kind IN ('movie', 'series', 'other'))"},
		{"foreign key action changed", 2, "acquisition_id INTEGER NOT NULL REFERENCES acquisition (id) ON DELETE CASCADE", "acquisition_id INTEGER NOT NULL REFERENCES acquisition (id) ON DELETE RESTRICT"},
		{"partial index predicate changed", 0, "CREATE UNIQUE INDEX slot_one_primary ON slot (title_id) WHERE is_primary = 1;", "CREATE UNIQUE INDEX slot_one_primary ON slot (title_id) WHERE is_primary = 0;"},
		{"trigger body changed", 4, "VALUES (NEW.title_id, NEW.id, 0, 0, 0, 0, 0, NULL, '', 0, 0, 0);", "VALUES (NEW.title_id, NEW.id, 1, 0, 0, 0, 0, NULL, '', 0, 0, 0);"},
		{"trigger condition changed", 5, "WHEN NEW.generation_id IS NOT NULL", "WHEN NEW.generation_id IS NULL"},
	}
	for _, m := range mutations {
		for _, analyze := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s analyze=%v", m.name, analyze), func(t *testing.T) {
				files := bootstrapV1(t)
				if n := strings.Count(files[m.file], m.old); n != 1 {
					t.Fatalf("fixture: %q occurs %d times in bootstrap file %d, want 1", m.old, n, m.file)
				}
				files[m.file] = strings.Replace(files[m.file], m.old, m.new, 1)
				path := legacyFile(t, files)
				populate(t, path)
				if analyze {
					rawExec(t, path, "ANALYZE")
				}
				openRefused(t, path, oneSet(t), ErrSchemaMismatch)
				if names := tableNames(t, path); strings.Contains(names, "goose_db_version") || strings.Contains(names, "boxset_schema_hash") {
					t.Errorf("a refused predecessor got migration tables: %q", names)
				}
			})
		}
	}
}

// F07: every way the recorded history can disagree with itself or with the
// binary is refused with its own error before the backup hook runs.
func TestRecordedHistoryRefused(t *testing.T) {
	migrated := func(t *testing.T) string {
		t.Helper()
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, twoSet(t)))
		populate(t, path)
		return path
	}
	zeros := strings.Repeat("0", 64)
	cases := []struct {
		name string
		edit []string
		want error
	}{
		{"applied version 2 deleted, its hash kept", []string{"DELETE FROM goose_db_version WHERE version_id = 2"}, ErrSchemaHash},
		{"applied version 1 deleted, its hash kept", []string{"DELETE FROM goose_db_version WHERE version_id = 1"}, ErrSchemaHash},
		{"version 2 recorded as not applied", []string{"UPDATE goose_db_version SET is_applied = 0 WHERE version_id = 2"}, ErrSchemaHash},
		{"hash 2 deleted, its version kept", []string{"DELETE FROM boxset_schema_hash WHERE version = 2"}, ErrSchemaHash},
		{"version and hash 1 removed, 2 retained", []string{"DELETE FROM goose_db_version WHERE version_id = 1", "DELETE FROM boxset_schema_hash WHERE version = 1"}, ErrSchemaMismatch},
		{"hash table dropped", []string{"DROP TABLE boxset_schema_hash"}, ErrSchemaMismatch},
		{"goose table dropped", []string{"DROP TABLE goose_db_version"}, ErrSchemaMismatch},
		{"newer hash only", []string{"INSERT INTO boxset_schema_hash(version, sha256) VALUES (3, '" + zeros + "')"}, ErrSchemaNewer},
		{"newer version only", []string{"INSERT INTO goose_db_version(version_id, is_applied) VALUES (3, 1)"}, ErrSchemaNewer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := migrated(t)
			rawExec(t, path, tc.edit...)
			openRefused(t, path, twoSet(t), tc.want)
		})
	}
	t.Run("control: the untouched history opens", func(t *testing.T) {
		path := migrated(t)
		closeIt(t, mustOpenSet(t, path, Options{}, twoSet(t)))
	})
}

// F09: a COMMIT the database rejects is not a commit. The callback succeeds; the
// deferred foreign key it violated fails the commit; nothing is counted and no
// invalid row remains.
func TestFailedCommitIsNotCounted(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY) STRICT;
			CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER NOT NULL REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED) STRICT`)
		return err
	})
	childRows := func() int {
		var n int
		if err := db.Read(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow("SELECT count(*) FROM child").Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}

	base := db.Commits()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO child(id, pid) VALUES (1, 99)") // accepted now, refused at COMMIT
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("Write whose COMMIT violates a deferred foreign key = %v, want that constraint error", err)
	}
	if got := db.Commits(); got != base {
		t.Errorf("Commits() = %d after a failed COMMIT, want the unchanged %d", got, base)
	}
	if n := childRows(); n != 0 {
		t.Errorf("child rows = %d after a failed COMMIT, want 0: the invalid row must not persist", n)
	}

	// The store is usable and a valid commit of the same shape counts once.
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO parent(id) VALUES (5); INSERT INTO child(id, pid) VALUES (2, 5)")
		return err
	})
	if got := db.Commits(); got != base+1 {
		t.Errorf("Commits() = %d after one valid commit, want %d", got, base+1)
	}
	if n := childRows(); n != 1 {
		t.Errorf("child rows = %d, want the one valid row", n)
	}
}

// F08: a process killed inside a migration transaction, after the body, version
// row and hash row were written and with no orderly rollback or Close, leaves
// the file at its previous state, loses no row, and the next open completes the migration.
func TestMigrationProcessDeath(t *testing.T) {
	t.Run("first migration", func(t *testing.T) {
		path := newPath(t)
		killMigrationChild(t, "first", path)
		if got := tableNames(t, path); got != "goose_db_version,sqlite_sequence" {
			t.Errorf("objects after the kill = %q, want only goose's version table: no body, hash or stamp of the first migration", got)
		}
		if got := gooseVersions(t, path); got != "0" {
			t.Errorf("applied versions after the kill = %q, want only goose's zero row", got)
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != "0" {
			t.Errorf("user_version after the kill = %v, want 0", got)
		}
		mustOpenSet(t, path, Options{}, oneSet(t))
		if got := gooseVersions(t, path); got != "0,1" {
			t.Errorf("applied versions after the retry = %q, want 0,1", got)
		}
		if got, want := storedHashes(t, path), []string{"1|" + fileSHA(t, firstMigration)}; !slices.Equal(got, want) {
			t.Errorf("hash rows after the retry = %v, want %v", got, want)
		}
	})

	t.Run("later migration", func(t *testing.T) {
		path := newPath(t)
		closeIt(t, mustOpenSet(t, path, Options{}, oneSet(t)))
		populate(t, path)
		before := stateDigest(t, path)
		killMigrationChild(t, "later", path)
		if after := stateDigest(t, path); after != before {
			t.Errorf("the killed migration changed the file: no table extra, hash 2 or version 2 may remain (objects %q, versions %q)", tableNames(t, path), gooseVersions(t, path))
		}
		calls := 0
		mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, twoSet(t))
		if calls != 1 {
			t.Errorf("BeforeMigrate ran %d times on the retry, want 1", calls)
		}
		if got := gooseVersions(t, path); got != "0,1,2" {
			t.Errorf("applied versions after the retry = %q, want 0,1,2", got)
		}
		if got := rawRows(t, path, "SELECT count(*) FROM title"); got[0] != "1" {
			t.Errorf("title rows after the retry = %v, want the retained 1", got)
		}
	})

	t.Run("predecessor adoption", func(t *testing.T) {
		path := newPredecessor(t)
		populate(t, path)
		before := stateDigest(t, path)
		killMigrationChild(t, "adopt", path)
		if after := stateDigest(t, path); after != before {
			t.Errorf("the killed adoption changed the file: it must keep the legacy stamp and no record table (objects %q)", tableNames(t, path))
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != "1" {
			t.Errorf("user_version after the kill = %v, want the legacy 1", got)
		}
		calls := 0
		mustOpenSet(t, path, Options{BeforeMigrate: anyBackup(&calls)}, oneSet(t))
		if calls != 1 {
			t.Errorf("BeforeMigrate ran %d times on the retry, want 1", calls)
		}
		if got := gooseVersions(t, path); got != "0,1" {
			t.Errorf("applied versions after the retry = %q, want 0,1", got)
		}
		if got := rawRows(t, path, "PRAGMA user_version"); got[0] != fmt.Sprint(SchemaVersion) {
			t.Errorf("user_version after the retry = %v, want the managed %d", got, SchemaVersion)
		}
		if got := rawRows(t, path, "SELECT count(*) FROM title"); got[0] != "1" {
			t.Errorf("title rows after the retry = %v, want the retained 1", got)
		}
	})
}
