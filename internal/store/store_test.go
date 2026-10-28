package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func waitReadPopulation(t *testing.T, db *DB, ordinary, password int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		db.mu.Lock()
		pending, wake := db.readPending, db.readWake
		db.mu.Unlock()
		if pending == [2]int{ordinary, password} {
			return
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatalf("pending readers=%v want=%v", pending, [2]int{ordinary, password})
		}
	}
}

func heldReader(t *testing.T, db *DB, password bool) (chan struct{}, <-chan error) {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	go func() {
		read := db.Read
		if password {
			read = db.ReadPassword
		}
		done <- read(t.Context(), func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow("SELECT 41+1").Scan(&n); err != nil {
				return err
			}
			if n != 42 {
				return errors.New("invalid SELECT result")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("reader failed before entry: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not enter")
	}
	return release, done
}

func TestReaderClassSharing(t *testing.T) {
	previous := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	db := openStore(t, newPath(t), Options{})
	query := func(tx *sql.Tx) error {
		var n int
		err := tx.QueryRow("SELECT 42").Scan(&n)
		if err == nil && n != 42 {
			return errors.New("invalid result")
		}
		return err
	}
	for _, size := range []int{2, 4} {
		runtime.GOMAXPROCS(size)
		release, done := heldReader(t, db, true)
		queued := make(chan error, 1)
		go func() { queued <- db.ReadPassword(t.Context(), query) }()
		waitReadPopulation(t, db, 0, 1)
		if err := db.Read(t.Context(), query); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-queued; err != nil {
			t.Fatal(err)
		}
	}
	runtime.GOMAXPROCS(1)
	for range 8 {
		release, done := heldReader(t, db, false)
		order := make(chan int, 2)
		pwRelease := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-pwRelease:
			default:
				close(pwRelease)
			}
		})
		pwDone, ordinaryDone := make(chan error, 1), make(chan error, 1)
		go func() {
			pwDone <- db.ReadPassword(t.Context(), func(tx *sql.Tx) error { order <- 1; <-pwRelease; return query(tx) })
		}()
		waitReadPopulation(t, db, 0, 1)
		go func() { ordinaryDone <- db.Read(t.Context(), func(tx *sql.Tx) error { order <- 0; return query(tx) }) }()
		waitReadPopulation(t, db, 1, 1)
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := <-order; got != 1 {
			t.Fatalf("password turn stolen by class %d", got)
		}
		close(pwRelease)
		if got := <-order; got != 0 {
			t.Fatalf("ordinary turn stolen by class %d", got)
		}
		if err := <-pwDone; err != nil {
			t.Fatal(err)
		}
		if err := <-ordinaryDone; err != nil {
			t.Fatal(err)
		}
	}
	runtime.GOMAXPROCS(4)
	pwRelease, pwDone := heldReader(t, db, true)
	var releases []chan struct{}
	var completions []<-chan error
	for range 3 {
		release, done := heldReader(t, db, false)
		releases = append(releases, release)
		completions = append(completions, done)
	}
	runtime.GOMAXPROCS(2)
	passwordDone, foreground := make(chan error, 1), make(chan error, 1)
	go func() { passwordDone <- db.ReadPassword(t.Context(), query) }()
	go func() { foreground <- db.Read(t.Context(), query) }()
	waitReadPopulation(t, db, 1, 1)
	for _, release := range releases {
		close(release)
	}
	for _, done := range completions {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-foreground:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground blocked behind the one password holder after shrink")
	}
	close(pwRelease)
	if err := <-pwDone; err != nil {
		t.Fatal(err)
	}
	if err := <-passwordDone; err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 4} {
		runtime.GOMAXPROCS(size)
		if err := db.ReadPassword(t.Context(), query); err != nil {
			t.Fatal(err)
		}
		if err := db.Read(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.readActive != [2]int{} || db.readPending != [2]int{} {
		t.Fatalf("leaked reader grants active=%v pending=%v", db.readActive, db.readPending)
	}
}

func TestReaderClassCancellation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	db := openStore(t, newPath(t), Options{})
	for _, password := range []bool{false, true} {
		release, active := heldReader(t, db, password)
		ctx, cancel := context.WithCancel(t.Context())
		chosen := make(chan error, 1)
		go func() {
			read := db.ReadPassword
			if password {
				read = db.Read
			}
			chosen <- read(ctx, func(*sql.Tx) error { return errors.New("canceled chosen class entered") })
		}()
		if password {
			waitReadPopulation(t, db, 1, 0)
		} else {
			waitReadPopulation(t, db, 0, 1)
		}
		cancel()
		if err := <-chosen; !errors.Is(err, context.Canceled) {
			t.Fatalf("chosen cancellation=%v", err)
		}
		next := make(chan error, 1)
		go func() {
			read := db.Read
			if password {
				read = db.ReadPassword
			}
			next <- read(t.Context(), func(tx *sql.Tx) error { var n int; return tx.QueryRow("SELECT 42").Scan(&n) })
		}()
		if password {
			waitReadPopulation(t, db, 0, 1)
		} else {
			waitReadPopulation(t, db, 1, 0)
		}
		close(release)
		if err := <-active; err != nil {
			t.Fatal(err)
		}
		if err := <-next; err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	opened := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- db.ReadPassword(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, "WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<9223372036854775806) SELECT x FROM n")
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			if !rows.Next() {
				return errors.New("active SQL never yielded its control row")
			}
			var first int
			if err := rows.Scan(&first); err != nil || first != 0 {
				return errors.New("active SQL control row invalid")
			}
			close(opened)
			<-ctx.Done()
			for rows.Next() {
			}
			return rows.Err()
		})
	}()
	<-opened
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled active SQL committed successfully")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("reader panic was swallowed")
			}
		}()
		_ = db.ReadPassword(t.Context(), func(*sql.Tx) error { panic("controlled reader panic") })
	}()
	if err := db.Read(t.Context(), func(tx *sql.Tx) error { var n int; return tx.QueryRow("SELECT 42").Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.readActive != [2]int{} || db.readPending != [2]int{} {
		t.Fatalf("reader lifecycle leaked active=%v pending=%v", db.readActive, db.readPending)
	}
}

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

// rawPeer is an external writer: one plain driver connection with busy_timeout 0,
// pinged before use, so any lock it meets fails at once with SQLITE_BUSY. It is
// never a second store connection.
func rawPeer(t *testing.T, path string) *sql.DB {
	t.Helper()
	peer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.Close(); err != nil {
			t.Error(err)
		}
	})
	peer.SetMaxOpenConns(1)
	if err := peer.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ExecContext(t.Context(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	var ms int
	if err := peer.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&ms); err != nil || ms != 0 {
		t.Fatalf("peer busy_timeout = %d, %v; want 0", ms, err)
	}
	return peer
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

// errParkedWrite is a parked callback's own failure: its transaction rolls back.
var errParkedWrite = errors.New("store_test: parked write rolled back")

// TestAdmissionRefusesCloseWhileWriterHoldsLease requires the writer admission
// lease, not the single connection's own queue, to decide Close. The pool alone
// already parks and cancels a second Write, but it cannot stop Close from
// reporting a closed store while a parked callback could still commit.
func TestAdmissionRefusesCloseWhileWriterHoldsLease(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	if inUse := db.Stats().Writer.InUse; inUse != 0 {
		t.Fatalf("idle writer pool InUse = %d, want 0", inUse)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	parked := make(chan error, 1)
	go func() {
		parked <- db.Write(t.Context(), func(*sql.Tx) error {
			close(entered)
			<-release
			return errParkedWrite
		})
	}()
	<-entered
	before := db.Commits()

	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	second := false
	err := db.Write(waitCtx, func(*sql.Tx) error { second = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Write = %v, want context.DeadlineExceeded", err)
	}
	if second || db.Commits() != before {
		t.Fatalf("second Write entered its callback (%t) or committed (%d, want %d)", second, db.Commits(), before)
	}

	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("Close reported a closed store while the writer lease was held")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return while the writer lease was held")
	}

	after := false
	if err := db.Write(t.Context(), func(*sql.Tx) error { after = true; return nil }); after || !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after a refused Close = %v, callback entered %t; want ErrClosed and no callback", err, after)
	}

	close(release)
	if err := <-parked; !errors.Is(err, errParkedWrite) {
		t.Fatalf("parked Write = %v, want its callback error", err)
	}
	if db.Commits() != before {
		t.Fatalf("Commits = %d after the parked Write released, want %d: it must roll back", db.Commits(), before)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close after the lease was released: %v", err)
	}
}

// waitAdmission samples the bounded admission state until match holds.
func waitAdmission(t *testing.T, db *DB, match func(MetadataAdmission) bool) MetadataAdmission {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if a := db.Admission(); match(a) {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission never matched: %+v", db.Admission())
		}
		time.Sleep(time.Millisecond)
	}
}

// waitGate blocks one credential-metadata wait observation, once armed, so the
// store can be read between the attempt registering its wait and the grant that
// ends it. Every other record passes straight through.
type waitGate struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (g *waitGate) Enabled(context.Context, slog.Level) bool { return true }

func (g *waitGate) Handle(_ context.Context, r slog.Record) error {
	wait := false
	r.Attrs(func(a slog.Attr) bool {
		wait = a.Key == "reason" && a.Value.String() == "metadata_wait"
		return !wait
	})
	if !wait || !g.armed.Swap(false) {
		return nil
	}
	close(g.reached)
	<-g.release
	return nil
}

func (g *waitGate) WithAttrs([]slog.Attr) slog.Handler { return g }

func (g *waitGate) WithGroup(string) slog.Handler { return g }

// TestMetadataAdmissionReportsWaitBehindLiveHolder requires the metadata entry
// into the writer lease to name the live holder it waits behind, to keep one
// bounded record with a monotone ordinal, and to publish its own end reason.
func TestMetadataAdmissionReportsWaitBehindLiveHolder(t *testing.T) {
	gate := &waitGate{reached: make(chan struct{}), release: make(chan struct{})}
	db := openStore(t, newPath(t), Options{Log: slog.New(gate)})
	if a := db.Admission(); a.Records != 0 || a.Phase != MetadataIdle || a.Reason != MetadataNoReason || a.Waiting {
		t.Fatalf("fresh admission = %+v, want an idle record", a)
	}
	for _, records := range []int{0, -1, MaxMetadataRecords + 1} {
		entered := false
		if err := db.WriteMetadata(t.Context(), records, func(*sql.Tx) error { entered = true; return nil }); !errors.Is(err, ErrMetadataBatch) || entered {
			t.Fatalf("WriteMetadata(%d) = %v, entered %t; want ErrMetadataBatch and no transaction", records, err, entered)
		}
	}

	// One parked ordinary write holds the lease across both attempts: each must
	// name that same holder generation while it waits, whatever its own ordinal,
	// and only its own later grant once the lease is its.
	entered, release := make(chan struct{}), make(chan struct{})
	parked := make(chan error, 1)
	go func() {
		parked <- db.Write(t.Context(), func(*sql.Tx) error {
			close(entered)
			<-release
			return errParkedWrite
		})
	}()
	<-entered
	before := db.Commits()

	// The first attempt waits behind that holder and is canceled there: it never
	// takes the lease, so the holder generation it named is all it reports.
	waitCtx, cancel := context.WithCancel(t.Context())
	canceled := make(chan error, 1)
	go func() {
		canceled <- db.WriteMetadata(waitCtx, 1, func(*sql.Tx) error {
			t.Error("a canceled attempt ran its transaction")
			return nil
		})
	}()
	first := waitAdmission(t, db, func(a MetadataAdmission) bool { return a.Waiting && a.Ordinal == 1 })
	if first.Records != 1 || first.Phase != MetadataWaiting || first.Generation == 0 {
		t.Fatalf("waiting admission = %+v, want the first attempt waiting behind a live holder", first)
	}
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WriteMetadata = %v, want context.Canceled", err)
	}
	if a := db.Admission(); a.Phase != MetadataTerminal || a.Reason != MetadataCanceled || a.Records != 1 || a.Ordinal != 1 || a.Waiting || a.Generation != first.Generation {
		t.Fatalf("canceled admission = %+v, want a terminal canceled first attempt still naming generation %d", a, first.Generation)
	}

	// The second attempt waits behind the same live holder, so it must report
	// that generation again even though its ordinal advanced: a wait names the
	// operation that holds the lease, never the attempt's own identity.
	committed := make(chan error, 1)
	go func() {
		committed <- db.WriteMetadata(t.Context(), 2, func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO title(metadata_id, kind, name) VALUES ('metadata-admission', 'movie', 'm')")
			return err
		})
	}()
	waiting := waitAdmission(t, db, func(a MetadataAdmission) bool { return a.Waiting && a.Ordinal == 2 })
	if waiting.Records != 2 || waiting.Phase != MetadataWaiting || waiting.Generation != first.Generation {
		t.Fatalf("waiting admission = %+v, want the second ordinal behind the same live holder generation %d", waiting, first.Generation)
	}
	close(release)
	if err := <-parked; !errors.Is(err, errParkedWrite) {
		t.Fatalf("parked Write = %v, want its own callback error", err)
	}
	if err := <-committed; err != nil {
		t.Fatalf("WriteMetadata after the holder released: %v", err)
	}
	done := db.Admission()
	if done.Phase != MetadataTerminal || done.Reason != MetadataSuccess || done.Records != 2 || done.Ordinal != 2 || done.Waiting {
		t.Fatalf("terminal admission = %+v, want success without a wait", done)
	}
	if done.Generation <= waiting.Generation {
		t.Fatalf("admitted generation %d, want later than the waited holder %d", done.Generation, waiting.Generation)
	}
	if db.Commits() != before+1 {
		t.Fatalf("Commits = %d, want %d: only the metadata transaction commits", db.Commits(), before+1)
	}

	// The generation match is the whole "behind a live holder" claim: a wait
	// names the holder it registered against, and once that holder released the
	// grant the attempt is still owed is not published yet, so the record is a
	// stale wait rather than a wait behind a live holder. The gate holds the
	// attempt inside its wait observation, so both reads happen between its
	// registration and its grant.
	gate.armed.Store(true)
	held, letGo := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- db.Write(t.Context(), func(*sql.Tx) error {
			close(held)
			<-letGo
			return nil
		})
	}()
	<-held
	third := make(chan error, 1)
	go func() { third <- db.WriteMetadata(t.Context(), 1, func(*sql.Tx) error { return nil }) }()
	<-gate.reached
	behind := db.Admission()
	if !behind.Waiting || behind.Phase != MetadataWaiting || behind.Generation == 0 {
		t.Fatalf("registration admission = %+v, want the attempt waiting behind the live holder", behind)
	}
	close(letGo)
	if err := <-holder; err != nil {
		t.Fatalf("holder Write = %v", err)
	}
	stale := db.Admission()
	if stale.Phase != MetadataWaiting || stale.Generation != behind.Generation || stale.Waiting {
		t.Fatalf("admission after the named holder released = %+v, want generation %d in phase %s without Waiting: the wait names a released holder, not a live one", stale, behind.Generation, MetadataWaiting)
	}
	close(gate.release)
	if err := <-third; err != nil {
		t.Fatalf("metadata attempt after the release: %v", err)
	}
	if end := db.Admission(); end.Phase != MetadataTerminal || end.Reason != MetadataSuccess || end.Waiting {
		t.Fatalf("terminal admission = %+v, want success without a wait", end)
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

	// Raise GOMAXPROCS back to 4 on the same Options{} store, which retires no
	// idle connection. One Read resizes the pool before any hold. database/sql
	// keeps its default idle cap of 2 unless sizeReaders raises it too, so only
	// the idle resize leaves four released readers idle.
	runtime.GOMAXPROCS(4)
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var one int
		return tx.QueryRow("SELECT 1").Scan(&one)
	}); err != nil {
		t.Fatalf("Read after the grow: %v", err)
	}
	if got := db.Stats().Reader.MaxOpenConnections; got != 4 {
		t.Fatalf("reader MaxOpenConnections = %d after GOMAXPROCS 2 -> 4 and one Read, want 4", got)
	}
	hold4, open4 := holdReads(t, db, 4)
	if open4 != 4 {
		t.Fatalf("after GOMAXPROCS 2 -> 4: %d open reader connections with 4 reads inside, want 4", open4)
	}
	hold4()
	if st := db.Stats().Reader; st.Idle != 4 || st.OpenConnections != 4 {
		t.Errorf("after GOMAXPROCS 2 -> 4 and release: reader idle %d, open %d, want 4 and 4", st.Idle, st.OpenConnections)
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

// TestWriteReservesBeforeCallback proves a writable transaction owns the write lock
// before its callback runs: an external writer that cannot wait is refused inside it.
func TestWriteReservesBeforeCallback(t *testing.T) {
	path := newPath(t)
	db := openStore(t, path, Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE probe(id INTEGER PRIMARY KEY, who TEXT NOT NULL)")
		return err
	})
	peer := rawPeer(t, path)
	calls := 0
	mustWrite(t, db, func(tx *sql.Tx) error {
		calls++
		_, err := peer.ExecContext(t.Context(), "INSERT INTO probe(id, who) VALUES (1, 'peer')")
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != 5 { // SQLITE_BUSY primary code
			t.Errorf("peer insert inside the Write callback = %v, want *sqlite.Error with primary code 5 (SQLITE_BUSY)", err)
		}
		_, err = tx.Exec("INSERT INTO probe(id, who) VALUES (2, 'callback')")
		return err
	})
	if calls != 1 {
		t.Errorf("Write callback ran %d times, want 1", calls)
	}
	var who []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT who FROM probe ORDER BY id")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var w string
			if err := rows.Scan(&w); err != nil {
				return err
			}
			who = append(who, w)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(who, ","); got != "callback" {
		t.Errorf("probe rows after Write = %q, want only the callback row", got)
	}
}

// TestReadDoesNotReserveWriter proves a Read stays a deferred read-only snapshot: an
// external writer that cannot wait still commits while the Read is open, and the
// open Read keeps its snapshot.
func TestReadDoesNotReserveWriter(t *testing.T) {
	path := newPath(t)
	db := openStore(t, path, Options{})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE probe(v TEXT NOT NULL); INSERT INTO probe(v) VALUES ('v0')")
		return err
	})
	peer := rawPeer(t, path)
	value := func(tx *sql.Tx) string {
		t.Helper()
		var v string
		if err := tx.QueryRow("SELECT v FROM probe").Scan(&v); err != nil {
			t.Fatalf("read probe: %v", err)
		}
		return v
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		if got := value(tx); got != "v0" {
			t.Errorf("probe before the peer update = %q, want v0", got)
		}
		if _, err := peer.ExecContext(t.Context(), "UPDATE probe SET v = 'v1'"); err != nil {
			t.Errorf("peer update while a Read is open: %v", err)
		}
		if got := value(tx); got != "v0" {
			t.Errorf("probe after the peer update, same Read = %q, want the v0 snapshot", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		if got := value(tx); got != "v1" {
			t.Errorf("probe in a later Read = %q, want v1", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWriteBeginFailureKeepsErrors proves a Write that waits at begin on an external
// writer and fails keeps the native SQLite error and reports the context's own identity
// (a deadline, an explicit cancel, or neither for a live context), never runs its
// callback, and leaves the store usable once the peer releases.
func TestWriteBeginFailureKeepsErrors(t *testing.T) {
	cases := []struct {
		name         string
		newContext   func(t *testing.T) context.Context
		wantDeadline bool
		wantCanceled bool
	}{
		{"live", func(t *testing.T) context.Context { return t.Context() }, false, false},
		{"deadline", func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			t.Cleanup(cancel)
			return ctx
		}, true, false},
		{"explicit-cancel", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			timer := time.AfterFunc(time.Second, cancel)
			t.Cleanup(func() {
				timer.Stop()
				cancel()
			})
			return ctx
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newPath(t)
			db := openStore(t, path, Options{})
			mustWrite(t, db, func(tx *sql.Tx) error {
				_, err := tx.Exec("CREATE TABLE probe(v TEXT NOT NULL); INSERT INTO probe(v) VALUES ('v0')")
				return err
			})
			peer := rawPeer(t, path)
			conn, err := peer.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			release := func() error {
				c := conn
				if c == nil {
					return nil
				}
				conn = nil
				_, err := c.ExecContext(context.Background(), "ROLLBACK")
				return errors.Join(err, c.Close())
			}
			t.Cleanup(func() {
				if err := release(); err != nil {
					t.Error(err)
				}
			})
			if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}

			ctx := tc.newContext(t)
			calls := 0
			err = db.Write(ctx, func(tx *sql.Tx) error {
				calls++
				_, err := tx.Exec("UPDATE probe SET v = 'callback'")
				return err
			})
			var se *sqlite.Error
			if !errors.As(err, &se) {
				t.Errorf("Write begin error = %v, want it to wrap a *sqlite.Error", err)
			} else {
				t.Logf("Write begin error = %v; native primary code %d", err, se.Code()&0xff)
			}
			if got := errors.Is(err, context.DeadlineExceeded); got != tc.wantDeadline {
				t.Errorf("Write begin error = %v, errors.Is context.DeadlineExceeded = %t, want %t", err, got, tc.wantDeadline)
			}
			if got := errors.Is(err, context.Canceled); got != tc.wantCanceled {
				t.Errorf("Write begin error = %v, errors.Is context.Canceled = %t, want %t", err, got, tc.wantCanceled)
			}
			if calls != 0 {
				t.Errorf("Write callback ran %d times, want 0", calls)
			}

			if err := release(); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, db, func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE probe SET v = 'v1'")
				return err
			})
			if err := db.Read(t.Context(), func(tx *sql.Tx) error {
				var v string
				if err := tx.QueryRow("SELECT v FROM probe").Scan(&v); err != nil {
					return err
				}
				if v != "v1" {
					t.Errorf("probe after the peer released = %q, want v1", v)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
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

// Commits counts committed Write transactions only: not the migrations Open
// ran, not reads, not a Write whose callback failed.
func TestCommitsCountsWrites(t *testing.T) {
	db := openStore(t, newPath(t), Options{})
	if got := db.Commits(); got != 0 {
		t.Fatalf("Commits() after Open = %d, want 0", got)
	}
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE probe(v TEXT NOT NULL)")
		return err
	})
	mustWrite(t, db, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO probe(v) VALUES ('a')")
		return err
	})
	if err := db.Write(t.Context(), func(*sql.Tx) error { return errInjected }); !errors.Is(err, errInjected) {
		t.Fatalf("Write with a failing callback = %v, want the callback's error", err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM probe").Scan(new(int))
	}); err != nil {
		t.Fatal(err)
	}
	if got := db.Commits(); got != 2 {
		t.Errorf("Commits() = %d after two committed writes, one failed write and one read, want 2", got)
	}
}
