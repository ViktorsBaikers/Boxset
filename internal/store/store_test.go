package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func openStore(t *testing.T, path string, opts Options) *DB {
	t.Helper()
	db, err := Open(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func newPath(t *testing.T) string { return filepath.Join(t.TempDir(), "store.db") }

// settings reads every policy PRAGMA back, by name, on the connection tx runs on.
func settings(t *testing.T, tx *sql.Tx) map[string]string {
	t.Helper()
	got := make(map[string]string, len(pragmaNames))
	for _, name := range pragmaNames {
		var v string
		if err := tx.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		got[name] = v
	}
	return got
}

func rowCount(t *testing.T, tx *sql.Tx, table string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func mustWrite(t *testing.T, db *DB, fn func(*sql.Tx) error) {
	t.Helper()
	if err := db.Write(t.Context(), fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestEveryPhysicalConnectionHasPolicy(t *testing.T) {
	db := openStore(t, newPath(t), Options{ConnMaxLifetime: time.Millisecond})
	want, wantReader := wantSettings("0"), wantSettings("1")
	t.Logf("mmap_cap_bytes=%d effective mmap_size=%s", MmapLimit(64), want["mmap_size"])
	rounds := 3*ReaderCount() + 3
	for range rounds {
		time.Sleep(5 * time.Millisecond) // retire the pooled connections: every round dials a new physical one
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			checkSettings(t, "reader connection", settings(t, tx), wantReader)
			return nil
		}); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			checkSettings(t, "writer connection", settings(t, tx), want)
			return nil
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	st := db.Stats()
	t.Logf("WriterConnects=%d ReaderConnects=%d ReaderCount()=%d", st.WriterConnects, st.ReaderConnects, ReaderCount())
	if st.WriterConnects <= 1 {
		t.Errorf("WriterConnects = %d, churn did not force new writer connections", st.WriterConnects)
	}
	if st.ReaderConnects <= int64(ReaderCount()) {
		t.Errorf("ReaderConnects = %d, churn did not force more than ReaderCount() = %d reader connections", st.ReaderConnects, ReaderCount())
	}
	p := db.Policy()
	checkSettings(t, "Policy().Writer", policySettings(p.Writer), want)
	checkSettings(t, "Policy().Reader", policySettings(p.Reader), wantReader)
}

// pragmaNames are the settings every physical connection reads back, by PRAGMA name.
var pragmaNames = []string{
	"foreign_keys", "busy_timeout", "cache_size", "journal_size_limit", "journal_mode", "synchronous",
	"query_only", "auto_vacuum", "mmap_size", "fullfsync", "checkpoint_fullfsync",
}

// wantSettings is the acceptance policy as literals, never derived from production code.
// The flush settings are asserted on darwin only, where they are enforced.
func wantSettings(queryOnly string) map[string]string {
	want := map[string]string{
		"foreign_keys":       "1",
		"busy_timeout":       "5000",
		"cache_size":         "-8192",
		"journal_size_limit": "67108864",
		"journal_mode":       "wal",
		"synchronous":        "2",
		"query_only":         queryOnly,
		"auto_vacuum":        "2",
		"mmap_size":          strconv.FormatInt(MmapLimit(strconv.IntSize), 10),
	}
	if runtime.GOOS == "darwin" {
		want["fullfsync"], want["checkpoint_fullfsync"] = "1", "1"
	}
	return want
}

// policySettings names the fields of a recorded ConnPolicy like the PRAGMAs.
func policySettings(p ConnPolicy) map[string]string {
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	return map[string]string{
		"foreign_keys": n(p.ForeignKeys), "busy_timeout": n(p.BusyTimeoutMS), "cache_size": n(p.CacheSize),
		"journal_size_limit": n(p.JournalSizeLimit), "journal_mode": p.JournalMode, "synchronous": n(p.Synchronous),
		"query_only": n(p.QueryOnly), "auto_vacuum": n(p.AutoVacuum), "mmap_size": n(p.MmapSize),
		"fullfsync": n(p.FullFsync), "checkpoint_fullfsync": n(p.CheckpointFullFsync),
	}
}

// checkSettings reports every wanted setting that differs from what was read back.
func checkSettings(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%s: PRAGMA %s = %q, want %q", what, name, g, w)
		}
	}
}

// closeCounter records the physical connections the driver opened and closed.
type closeCounter struct {
	driver.Connector
	opened, closed *int
}

func (c closeCounter) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	*c.opened++
	return countedConn{conn, c.closed}, nil
}

type countedConn struct {
	driver.Conn
	closed *int
}

func (c countedConn) Close() error {
	*c.closed++
	return c.Conn.Close()
}

func (c countedConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}

func (c countedConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
}

// rawExec runs statements on a plain driver connection, outside the policy.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestPolicyMismatchRefusesConnection(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string
		setting string
		want    string
	}{
		{"journal mode", []string{"PRAGMA auto_vacuum=2", "PRAGMA journal_mode=delete", "CREATE TABLE t(x)", "PRAGMA user_version=1"}, "journal_mode", "want wal got delete"},
		{"auto vacuum", []string{"PRAGMA journal_mode=wal", "CREATE TABLE t(x)", "PRAGMA user_version=1"}, "auto_vacuum", "want 2 got 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newPath(t)
			rawExec(t, path, tc.setup...)

			base, err := sqlite.NewConnector(path)
			if err != nil {
				t.Fatal(err)
			}
			var opened, closed int
			c := &connector{base: closeCounter{base, &opened, &closed}, want: newPolicy(0)}
			conn, err := c.Connect(t.Context())
			if conn != nil || !errors.Is(err, ErrPolicyMismatch) {
				t.Fatalf("Connect = %v, %v; want no connection and ErrPolicyMismatch", conn, err)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.setting) || !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not name %s and %q", msg, tc.setting, tc.want)
			}
			if opened != 1 || closed != 1 {
				t.Errorf("driver connections opened %d, closed %d; the refused connection must be closed", opened, closed)
			}

			db, err := Open(t.Context(), path, Options{})
			if db != nil || !errors.Is(err, ErrPolicyMismatch) {
				t.Fatalf("Open = %v, %v; want no DB and ErrPolicyMismatch", db, err)
			}
		})
	}
}

func TestPoolBounds(t *testing.T) {
	db := openStore(t, newPath(t), Options{ConnMaxLifetime: 2 * time.Millisecond})
	readers := ReaderCount()
	if want := max(1, min(4, runtime.GOMAXPROCS(0))); readers != want {
		t.Fatalf("ReaderCount() = %d, want max(1, min(4, GOMAXPROCS)) = %d", readers, want)
	}
	st := db.Stats()
	if st.Writer.MaxOpenConnections != 1 || st.Reader.MaxOpenConnections != readers {
		t.Fatalf("max open writer %d reader %d, want 1 and %d", st.Writer.MaxOpenConnections, st.Reader.MaxOpenConnections, readers)
	}

	var mu sync.Mutex
	maxWriter, maxReader := 0, 0
	sample := func() {
		s := db.Stats()
		mu.Lock()
		maxWriter = max(maxWriter, s.Writer.OpenConnections)
		maxReader = max(maxReader, s.Reader.OpenConnections)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40+30*readers)
	for i := range 4 {
		wg.Go(func() {
			for j := range 10 {
				errs <- db.Write(t.Context(), func(tx *sql.Tx) error {
					sample()
					_, err := tx.Exec("INSERT INTO title(metadata_id, kind, name) VALUES (?, 'movie', 'm')", "w"+strconv.Itoa(i)+"-"+strconv.Itoa(j))
					return err
				})
				time.Sleep(time.Millisecond)
			}
		})
	}
	for range 3 * readers {
		wg.Go(func() {
			for range 10 {
				errs <- db.Read(t.Context(), func(tx *sql.Tx) error {
					sample()
					var n int
					return tx.QueryRow("SELECT count(*) FROM title").Scan(&n)
				})
				time.Sleep(time.Millisecond)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	sample()
	t.Logf("peak open writer connections %d (max 1), reader connections %d (max %d)", maxWriter, maxReader, readers)
	if maxWriter > 1 || maxReader > readers {
		t.Errorf("open connections exceeded the bounds: writer %d reader %d, want at most 1 and %d", maxWriter, maxReader, readers)
	}
	mustWrite(t, db, func(tx *sql.Tx) error {
		if n := rowCount(t, tx, "title"); n != 40 {
			t.Errorf("title rows = %d, want 40", n)
		}
		return nil
	})
}

// holdReads starts n concurrent reads that each signal they are inside their
// transaction, then wait for release. It returns once all n are inside, with the
// open reader connections at that moment. release lets them finish and checks
// their errors; the test cleanup calls it too, so no reader outlives the test.
func holdReads(t *testing.T, db *DB, n int) (release func(), open int) {
	t.Helper()
	inside := make(chan struct{}, n)
	gate := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			errs <- db.Read(t.Context(), func(tx *sql.Tx) error {
				inside <- struct{}{}
				<-gate
				var one int
				return tx.QueryRow("SELECT 1").Scan(&one)
			})
		})
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(gate)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	t.Cleanup(release)
	for range n {
		select {
		case <-inside:
		case <-time.After(10 * time.Second):
			t.Fatalf("only some of %d reads got inside a transaction", n)
		}
	}
	return release, db.Stats().Reader.OpenConnections
}

func TestReaderPoolFollowsGOMAXPROCS(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	db := openStore(t, newPath(t), Options{})
	release4, open := holdReads(t, db, 4)
	if open != 4 {
		t.Fatalf("with GOMAXPROCS 4: %d open reader connections with 4 reads inside, want 4", open)
	}

	// Lower GOMAXPROCS while the four are inside. The next Read resizes the pool
	// before it acquires a connection, then waits for one: the four in flight keep
	// their connections until they return.
	runtime.GOMAXPROCS(2)
	extra := make(chan error, 1)
	go func() {
		extra <- db.Read(t.Context(), func(tx *sql.Tx) error {
			var one int
			return tx.QueryRow("SELECT 1").Scan(&one)
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for db.Stats().Reader.MaxOpenConnections != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("reader MaxOpenConnections = %d, want 2 while four reads are still inside", db.Stats().Reader.MaxOpenConnections)
		}
		runtime.Gosched()
	}
	t.Logf("GOMAXPROCS 2 with four reads inside: MaxOpenConnections %d, OpenConnections %d", db.Stats().Reader.MaxOpenConnections, db.Stats().Reader.OpenConnections)
	if st := db.Stats().Reader; st.OpenConnections != 4 || st.MaxOpenConnections != 2 {
		t.Errorf("after the shrink with four reads inside: reader open %d, max open %d, want 4 and 2", st.OpenConnections, st.MaxOpenConnections)
	}

	release4()
	if err := <-extra; err != nil {
		t.Fatalf("Read after the shrink: %v", err)
	}
	release2, open := holdReads(t, db, 2)
	t.Logf("GOMAXPROCS 2 after release: open reader connections %d with 2 reads inside", open)
	if open > 2 {
		t.Errorf("after GOMAXPROCS 4 -> 2 and release: %d open reader connections, want at most 2", open)
	}
	release2()
	if st := db.Stats(); st.Reader.MaxOpenConnections != 2 || st.Reader.OpenConnections > 2 {
		t.Errorf("reader pool max open %d, open %d, want 2 and at most 2", st.Reader.MaxOpenConnections, st.Reader.OpenConnections)
	}
}

// TestReaderPoolFollowsGOMAXPROCSStaleSample lowers GOMAXPROCS from inside the
// seam that runs under the mutex right before sizeReaders samples ReaderCount().
// A sample taken before that point would still see 4 and apply it.
func TestReaderPoolFollowsGOMAXPROCSStaleSample(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	db := openStore(t, newPath(t), Options{})
	if got := db.Stats().Reader.MaxOpenConnections; got != 4 {
		t.Fatalf("after Open at GOMAXPROCS 4: reader MaxOpenConnections = %d, want 4", got)
	}
	db.beforeSample = func() { runtime.GOMAXPROCS(2) }
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var one int
		return tx.QueryRow("SELECT 1").Scan(&one)
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := db.Stats().Reader.MaxOpenConnections; got != 2 {
		t.Errorf("reader MaxOpenConnections = %d, want 2 (a sample taken before the seam applied the stale 4)", got)
	}
}

func TestForeignKeyViolationFails(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO library(id, name, default_monitored, default_naming_language, default_search_language, default_profile) VALUES (1, 'a', 1, 'en', 'en', 'p')"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO title(id, metadata_id, kind, name) VALUES (1, 'tt1', 'movie', 'one')")
		return err
	})
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO title(id, metadata_id, kind, name) VALUES (2, 'tt2', 'movie', 'two')"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO library_title(library_id, title_id) VALUES (1, 99)")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("FK-invalid insert error = %v, want FOREIGN KEY constraint failed", err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		if titles, links := rowCount(t, tx, "title"), rowCount(t, tx, "library_title"); titles != 1 || links != 0 {
			t.Errorf("after the failed write: %d titles, %d library_title rows; want 1 and 0", titles, links)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReaderWriteFails(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO title(id, metadata_id, kind, name) VALUES (1, 'tt1', 'movie', 'one')")
		return err
	})
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO title(id, metadata_id, kind, name) VALUES (2, 'tt2', 'movie', 'two')")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("reader insert error = %v, want a readonly database error", err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		if n := rowCount(t, tx, "title"); n != 1 {
			t.Errorf("titles after the failed reader insert = %d, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMmapPolicy(t *testing.T) {
	if got := MmapLimit(32); got != 0 {
		t.Errorf("MmapLimit(32) = %d, want 0", got)
	}
	capBytes := MmapLimit(64)
	t.Logf("mmap_cap_bytes=%d", capBytes)
	if capBytes <= 0 || capBytes != 64<<20 {
		t.Errorf("MmapLimit(64) = %d, want the finite 64 MiB cap", capBytes)
	}
	db := openStore(t, newPath(t), Options{})
	want := MmapLimit(strconv.IntSize)
	t.Logf("IntSize=%d effective mmap_size=%d", strconv.IntSize, db.Policy().Writer.MmapSize)
	for _, kind := range []string{"writer", "reader"} {
		fn := func(tx *sql.Tx) error {
			var got int64
			if err := tx.QueryRow("PRAGMA mmap_size").Scan(&got); err != nil {
				return err
			}
			if got != want {
				t.Errorf("%s mmap_size = %d, want %d", kind, got, want)
			}
			return nil
		}
		var err error
		if kind == "writer" {
			err = db.Write(t.Context(), fn)
		} else {
			err = db.Read(t.Context(), fn)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := db.Policy(); p.Writer.MmapSize != want || p.Reader.MmapSize != want {
		t.Errorf("Policy mmap_size writer %d reader %d, want %d", p.Writer.MmapSize, p.Reader.MmapSize, want)
	}
}

func TestInvalidPathRefused(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"", filepath.Join(dir, "a?b.db"), filepath.Join(dir, "a#b.db"), "file:" + filepath.Join(dir, "c.db") + "?mode=ro"} {
		db, err := Open(t.Context(), path, Options{})
		if db != nil || !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Open(%q) = %v, %v; want ErrInvalidPath", path, db, err)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a refused path left %v (err %v) behind", entries, err)
	}
}

func TestSchemaMismatchRefused(t *testing.T) {
	path := newPath(t)
	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO title(id, metadata_id, kind, name) VALUES (1, 'tt1', 'movie', 'one')")
		return err
	})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, path, Options{}) // a file with tables and the current version reopens with its rows
	if err := reopened.Read(t.Context(), func(tx *sql.Tx) error {
		if n := rowCount(t, tx, "title"); n != 1 {
			t.Errorf("reopened title rows = %d, want 1", n)
		}
		var version int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Errorf("stored user_version = %d (err %v), want SchemaVersion %d", version, err, SchemaVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	rawExec(t, path, "PRAGMA user_version=99")
	got, err := Open(t.Context(), path, Options{})
	if got != nil || !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Open with user_version 99 = %v, %v; want ErrSchemaMismatch", got, err)
	}
}
