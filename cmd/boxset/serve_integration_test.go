//go:build integration && unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// binary is the built boxset command every test runs as a separate process.
var binary string

func TestMain(m *testing.M) {
	// An in-process serve re-execs this test binary as its crash-monitor guardian, so
	// dispatch that child entry before any test work.
	if handled, code := child.Enter(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "boxset-bin-")
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		binary = filepath.Join(dir, "boxset")
		if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}())
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// proc is one running `boxset serve` process.
type proc struct {
	cmd    *exec.Cmd
	port   string
	stderr bytes.Buffer
	done   chan struct{}
	err    error
}

func start(t *testing.T, dir string) *proc { return startIn(t, "", dir) }

// startIn runs `boxset serve` with the working directory cwd ("" keeps ours) and any extra
// KEY=value environment entries.
func startIn(t *testing.T, cwd, dir string, extra ...string) *proc {
	t.Helper()
	s := &proc{port: freePort(t), done: make(chan struct{})}
	s.cmd = exec.Command(binary, "serve")
	s.cmd.Dir = cwd
	s.cmd.Env = append([]string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=" + s.port}, extra...)
	s.cmd.Stderr = &s.stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		<-s.done
	})
	return s
}

// exited waits for the process and returns its exit status; -1 means a signal ended it.
func (s *proc) exited(t *testing.T) int {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the process did not exit within 30 s")
	}
	var ee *exec.ExitError
	switch {
	case s.err == nil:
		return 0
	case errors.As(s.err, &ee):
		return ee.ExitCode()
	}
	t.Fatalf("wait: %v", s.err)
	return 0
}

// pingState reports the lifecycle state /ping answers on port, or "" when nothing answers.
func pingState(port string) string {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/ping")
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("X-Boxset-State")
}

func (s *proc) ping() string { return pingState(s.port) }

// pendingMigration leaves dir with a database that stands at the first
// migration and holds a library, so every later migration is pending. The file
// is built forward from the canonical first migration: the store's per-file
// policy before the first table, goose UpTo 1 and the hash row of that file.
func pendingMigration(t *testing.T, dir string) {
	t.Helper()
	layout := install.Layout{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	migrations := filepath.Join("..", "..", "internal", "store", "migrations")
	first, err := os.ReadFile(filepath.Join(migrations, "00001_library.sql"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA journal_mode=wal"} {
		if _, err := raw.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, raw, os.DirFS(migrations), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(first)
	for _, stmt := range []string{
		"INSERT INTO boxset_schema_hash (version, sha256) VALUES (1, '" + fmt.Sprintf("%x", sum) + "')",
		"INSERT INTO library (id, name, default_monitored, default_naming_language, default_search_language, default_profile) VALUES (1, 'Movies', 1, 'en', 'en', 'hd')",
	} {
		if _, err := raw.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func (s *proc) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			t.Fatalf("the server exited before it was ready (%v): %s", s.err, s.stderr.String())
		default:
		}
		if s.ping() == "ready" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the server was not ready within 30 s: %s", s.stderr.String())
}

func TestVersion(t *testing.T) {
	out, err := exec.Command(binary, "version").Output()
	if err != nil || !bytes.HasPrefix(out, []byte("boxset ")) {
		t.Fatalf("boxset version = %q, %v; want a line starting with the command name", out, err)
	}
	if err := exec.Command(binary, "frobnicate").Run(); err == nil {
		t.Error("an unknown subcommand exited 0")
	} else if ee := (*exec.ExitError)(nil); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Errorf("unknown subcommand: %v, want exit 2", err)
	}
}

func TestServeProxyConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, trusted, bypass, scheme string
		secure                        bool
	}{
		{"unset", "", "", "https", false},
		{"whitespace", "   ", " \t ", "https", false},
		{"trusted", " 127.0.0.0/8 , ::1/128 ", "127.0.0.0/8", "https", true},
		{"mapped", "::ffff:127.0.0.0/104", "", "https", true},
		{"untrusted", "192.0.2.0/24", "", "https", false},
		{"conflicting", "127.0.0.0/8", "", "https,http", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			extra := []string{"BOXSET_URL_BASE=/media"}
			if tc.name != "unset" {
				extra = append(extra, "BOXSET_TRUSTED_PROXIES="+tc.trusted, "BOXSET_LOCAL_BYPASS="+tc.bypass)
			}
			s := startIn(t, "", dir, extra...)
			client := &http.Client{Timeout: 5 * time.Second}
			deadline := time.Now().Add(30 * time.Second)
			for {
				resp, err := client.Get("http://127.0.0.1:" + s.port + "/media/ping")
				if err == nil {
					_ = resp.Body.Close()
					if resp.Header.Get("X-Boxset-State") == "ready" {
						break
					}
				}
				select {
				case <-s.done:
					t.Fatal("server exited before ready")
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("server did not become ready")
				}
				time.Sleep(25 * time.Millisecond)
			}
			token, err := os.ReadFile(filepath.Join(dir, "setup-token"))
			if err != nil {
				t.Fatal(err)
			}
			call := func(method, path, host, credential, scheme string) *http.Response {
				t.Helper()
				body := ""
				if method == "POST" {
					body = `{"username":"owner","password":"synthetic-long-password"}`
				}
				req, err := http.NewRequest(method, "http://127.0.0.1:"+s.port+"/media"+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Forwarded-Proto", scheme)
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				if host != "" {
					req.Host = host
				}
				if credential != "" {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal("HTTP fixture request failed")
				}
				_ = resp.Body.Close()
				return resp
			}
			resp := call("POST", "/api/v1/setup", "", string(token), "")
			clear(token)
			if resp.StatusCode != 201 {
				t.Fatalf("setup=%d", resp.StatusCode)
			}
			resp = call("POST", "/api/v1/session", "", "", tc.scheme)
			if resp.StatusCode != 201 || len(resp.Cookies()) != 1 {
				t.Fatalf("login=%d", resp.StatusCode)
			}
			cookie := resp.Cookies()[0]
			if cookie.Secure != tc.secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatal("CLI cookie policy")
			}
			if tc.secure {
				if cookie.Path != "/" || !strings.HasPrefix(cookie.Name, "__Host-boxset-") {
					t.Fatal("TLS cookie scope")
				}
			} else if cookie.Path != "/media" || !strings.HasPrefix(cookie.Name, "boxset-") {
				t.Fatal("HTTP cookie scope")
			}
			wantHost := 201
			if strings.TrimSpace(tc.bypass) != "" {
				wantHost = 421
			}
			if resp := call("POST", "/api/v1/session", "unapproved.example", "", ""); resp.StatusCode != wantHost {
				t.Fatalf("closed setup Host status=%d want=%d", resp.StatusCode, wantHost)
			}
			if resp := call("GET", "/api/v1/session", "", "", ""); resp.StatusCode != 401 {
				t.Fatal("005 granted bypass authority")
			}
		})
	}
}

func TestServeRejectsInvalidAccessCIDRs(t *testing.T) {
	for _, setting := range []string{"BOXSET_TRUSTED_PROXIES", "BOXSET_LOCAL_BYPASS"} {
		for _, value := range []string{"not-a-cidr-sentinel", "127.0.0.0/8,private-invalid-sentinel", "127.0.0.0/8,,::1/128", ","} {
			dir := filepath.Join(t.TempDir(), "absent")
			s := startIn(t, "", dir, setting+"="+value)
			if exit := s.exited(t); exit != 2 {
				t.Fatalf("%s exit=%d", setting, exit)
			}
			output := s.stderr.String()
			if !strings.Contains(output, "access_config_invalid") || !strings.Contains(output, setting) || strings.Contains(output, value) || len(output) > 128 {
				t.Fatal("configuration refusal diagnostics")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("configuration refusal created data state")
			}
			if state := s.ping(); state != "" {
				t.Fatal("configuration refusal opened listener")
			}
			corrected := startIn(t, "", dir, setting+"=127.0.0.0/8,::1/128")
			corrected.waitReady(t)
		}
	}
}

func TestSecondServerRefused(t *testing.T) {
	t.Run("a running server holds the directory", func(t *testing.T) {
		dir := t.TempDir()
		first := start(t, dir)
		first.waitReady(t)

		second := start(t, dir)
		if code := second.exited(t); code != 3 {
			t.Fatalf("second server exit = %d, want 3: %s", code, second.stderr.String())
		}
		if !bytes.Contains(second.stderr.Bytes(), []byte("lock_held")) {
			t.Errorf("second server stderr = %q, want the reason lock_held", second.stderr.String())
		}
		if second.ping() != "" {
			t.Error("the refused server answered /ping: it listened before it held the lock")
		}
		if got := first.ping(); got != "ready" {
			t.Errorf("first server /ping state = %q after the refusal, want ready", got)
		}
		t.Logf("TWO-SERVER second=exit 3 reason=lock_held first=%s", first.ping())
	})

	// The lock must come before the database: with the lock held elsewhere and
	// something that is not a database at the database path, the refusal is
	// still lock_held and the path is untouched.
	t.Run("the database is not opened", func(t *testing.T) {
		dir := t.TempDir()
		held, err := install.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Release() }()
		poison := install.Layout{Dir: dir}.DB()
		if err := os.Mkdir(poison, 0o700); err != nil {
			t.Fatal(err)
		}
		s := start(t, dir)
		if code := s.exited(t); code != 3 || !bytes.Contains(s.stderr.Bytes(), []byte("lock_held")) {
			t.Fatalf("exit = %d, stderr = %q; want exit 3 lock_held", code, s.stderr.String())
		}
		if _, err := os.Stat(install.Layout{Dir: dir}.Backups()); err == nil {
			t.Error("the refused server created the backups directory")
		}
	})
}

func TestLockReacquiredAfterExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		code int
	}{
		{"terminated", syscall.SIGTERM, 0},
		{"killed", syscall.SIGKILL, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			first := start(t, dir)
			first.waitReady(t)
			if err := first.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := first.exited(t); code != tc.code {
				t.Fatalf("exit after %v = %d, want %d: %s", tc.sig, code, tc.code, first.stderr.String())
			}
			second := start(t, dir)
			second.waitReady(t)
			t.Logf("LOCK-REACQUIRED after %v: second server ready", tc.sig)
		})
	}
}

// A base that is not a usable absolute path refuses at the shell, after the lock and before the
// listener: nothing answers on the port, the store is never opened and the lock is free for
// the next server, which serves the real shell under a valid base.
func TestServeRefusesBadBaseBeforeListen(t *testing.T) {
	for _, base := range []string{"media", "//", "///", strings.Repeat("/", 128), strings.Repeat("/", 129)} {
		t.Run(fmt.Sprintf("%d-%s", len(base), strings.Trim(base, "/")), func(t *testing.T) {
			dir := t.TempDir()
			bad := startIn(t, "", dir, "BOXSET_URL_BASE="+base)
			if code := bad.exited(t); code != 3 {
				t.Fatalf("exit = %d, want 3: %s", code, bad.stderr.String())
			}
			if !bytes.Contains(bad.stderr.Bytes(), []byte("shell_unavailable")) || !bytes.Contains(bad.stderr.Bytes(), []byte("base_invalid")) {
				t.Errorf("stderr = %q, want shell_unavailable naming base_invalid", bad.stderr.String())
			}
			if bytes.Contains(bad.stderr.Bytes(), []byte("panic:")) {
				t.Errorf("invalid base panicked: %s", bad.stderr.String())
			}
			if bad.ping() != "" {
				t.Error("the refused server answered /ping: it listened before it built the shell")
			}
			if _, err := os.Stat(install.Layout{Dir: dir}.DB()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the refused server opened the store or its absence is unproven: %v", err)
			}

			next := startIn(t, "", dir, "BOXSET_URL_BASE=/media")
			// /ping lives under the base too, so readiness is read there.
			ready := false
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !ready; time.Sleep(50 * time.Millisecond) {
				if r, err := http.Get("http://127.0.0.1:" + next.port + "/media/ping"); err == nil {
					ready = r.Header.Get("X-Boxset-State") == "ready"
					_ = r.Body.Close()
				}
			}
			if !ready {
				t.Fatalf("the server was not ready under /media within 30 s: %s", next.stderr.String())
			}
			resp, err := http.Get("http://127.0.0.1:" + next.port + "/media/")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if cerr := resp.Body.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte("__boxset_base__")) || !bytes.Contains(body, []byte(`base: "/media"`)) {
				t.Errorf("GET /media/ = %d: not the shell rewritten for /media", resp.StatusCode)
			}
			t.Logf("SHELL-BASE bad base refused before listen; next server ready and serving /media/")
		})
	}
}

// A file that stands at the first migration is migrated by serve only after a
// verified snapshot of it was published.
func TestServeSnapshotsBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	layout := install.Layout{Dir: dir}
	pendingMigration(t, dir)

	s := start(t, dir)
	s.waitReady(t)

	snaps, err := filepath.Glob(filepath.Join(layout.Backups(), "pre-migration-1-*.db"))
	if err != nil || len(snaps) != 1 {
		t.Fatalf("published snapshots = %v (err %v), want exactly one pre-migration-1", snaps, err)
	}
	snap, err := sql.Open("sqlite", snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	var libraries int
	var installation string
	if err := snap.QueryRowContext(t.Context(), "SELECT count(*) FROM library").Scan(&libraries); err != nil || libraries != 1 {
		t.Errorf("the snapshot holds %d libraries (err %v), want the 1 the file held", libraries, err)
	}
	if err := snap.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name = 'installation'").Scan(&installation); err != nil || installation != "0" {
		t.Errorf("the snapshot has the installation table (%q, %v): it is not the pre-migration state", installation, err)
	}
	t.Logf("SERVE-MIGRATION snapshot %s published before the installation table was created", filepath.Base(snaps[0]))
}

// dirState describes the data directory: every entry name below it and the
// SHA-256 of the database file, so a refused contender can be shown to have
// created, removed or changed nothing.
func dirState(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	if err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		_, _ = fmt.Fprintln(&sb, rel)
		return relErr
	}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(install.Layout{Dir: dir}.DB()); err == nil {
		_, _ = fmt.Fprintf(&sb, "boxset.db sha256=%x\n", sha256.Sum256(b))
	}
	return sb.String()
}

// serveHere runs serve in this test process, so a proof can hold it inside a
// step through the seams of main.go. Contenders are real processes.
func serveHere(t *testing.T, dir string) (port string, exit <-chan int, stderr *bytes.Buffer) {
	t.Helper()
	port = freePort(t)
	t.Setenv("BOXSET_DATA_DIR", dir)
	t.Setenv("BOXSET_PORT", port)
	stderr = new(bytes.Buffer)
	ch := make(chan int, 1)
	go func() { ch <- run([]string{"serve"}, io.Discard, stderr) }()
	return port, ch, stderr
}

func awaitExit(t *testing.T, exit <-chan int) int {
	t.Helper()
	select {
	case code := <-exit:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return within 30 s")
		return -1
	}
}

func awaitState(t *testing.T, port, want string, exit <-chan int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-exit:
			t.Fatalf("serve returned %d before /ping reported %s", code, want)
		default:
		}
		if pingState(port) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/ping did not report %s within 30 s", want)
}

// terminateHere asks the in-process serve to stop, as an operator's SIGTERM does.
func terminateHere(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

// refusedWhileHeld starts a real second server on the directory and requires
// exit 3 lock_held with the directory exactly as it was.
func refusedWhileHeld(t *testing.T, dir, where string) {
	t.Helper()
	before := dirState(t, dir)
	s := start(t, dir)
	if code := s.exited(t); code != 3 || !bytes.Contains(s.stderr.Bytes(), []byte("lock_held")) {
		t.Fatalf("contender %s: exit %d, stderr %q; want exit 3 lock_held", where, code, s.stderr.String())
	}
	if after := dirState(t, dir); after != before {
		t.Errorf("contender %s changed the data directory:\n before %s\n after  %s", where, before, after)
	}
	t.Logf("LOCK-HELD %s: contender exit 3 lock_held, directory unchanged", where)
}

// The lock is held from before the startup migration until the store is
// closed: a contender is refused, and changes nothing, while the first server
// sits inside the migration step and again while it sits at its final store
// close; once the first server is gone the directory is acquired safely.
func TestLockHeldThroughMigrationAndClose(t *testing.T) {
	t.Run("startup migration", func(t *testing.T) {
		dir := t.TempDir()
		pendingMigration(t, dir)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		freeIt := func() { once.Do(func() { close(release) }) }
		real := publishSnapshot
		publishSnapshot = func(ctx context.Context, l install.Layout, p store.Pending) error {
			close(entered)
			<-release
			return real(ctx, l, p)
		}
		t.Cleanup(func() { publishSnapshot = real; freeIt() })

		port, exit, stderr := serveHere(t, dir)
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("serve did not reach the startup migration")
		}
		refusedWhileHeld(t, dir, "during the startup migration")
		if _, err := os.Stat(install.Layout{Dir: dir}.Backups()); err == nil {
			t.Error("a backups directory exists before the held migration step ran")
		}

		freeIt()
		awaitState(t, port, "ready", exit)
		terminateHere(t)
		if code := awaitExit(t, exit); code != 0 {
			t.Fatalf("serve exit = %d: %s", code, stderr.String())
		}
		if snaps, err := filepath.Glob(filepath.Join(install.Layout{Dir: dir}.Backups(), "pre-migration-1-*.db")); err != nil || len(snaps) != 1 {
			t.Errorf("snapshots after the released migration = %v (err %v), want 1", snaps, err)
		}
		again, err := install.Acquire(dir)
		if err != nil {
			t.Fatalf("the directory could not be acquired after the server ended: %v", err)
		}
		if err := again.Release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("final store close", func(t *testing.T) {
		dir := t.TempDir()
		closing, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		freeIt := func() { once.Do(func() { close(release) }) }
		real := closeStore
		closeStore = func(db *store.DB) error {
			close(closing)
			<-release
			return real(db)
		}
		t.Cleanup(func() { closeStore = real; freeIt() })

		port, exit, stderr := serveHere(t, dir)
		awaitState(t, port, "ready", exit)
		terminateHere(t)
		select {
		case <-closing:
		case <-time.After(30 * time.Second):
			t.Fatal("serve did not reach the final store close")
		}
		refusedWhileHeld(t, dir, "at the final store close")

		freeIt()
		if code := awaitExit(t, exit); code != 0 {
			t.Fatalf("serve exit = %d: %s", code, stderr.String())
		}
		second := start(t, dir)
		second.waitReady(t)
		t.Log("LOCK-HELD-THROUGH-CLOSE released only after the store closed; reacquired by a new server")
	})
}

// A data directory on a refused filesystem stops serve before it takes the lock
// or touches the store, and the same path with an accepted filesystem starts.
// The real CheckMount runs on the directory serve resolved; only the kernel's
// mount table is replaced, because the dev container has no network share to
// mount. A serve that ignored the refusal would run until stopped, so each run
// is bounded.
func TestMountCheckedThroughServe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the mount table is read from /proc on Linux only")
	}
	table := func(fstype string, points ...string) func() (io.ReadCloser, error) {
		var b strings.Builder
		b.WriteString("1 0 8:1 / / rw - ext4 /dev/sda1 rw\n")
		for i, p := range points {
			_, _ = fmt.Fprintf(&b, "%d 1 0:%d / %s rw - %s src rw\n", 10+i, 50+i, strings.ReplaceAll(p, " ", `\040`), fstype)
		}
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(b.String())), nil }
	}
	resolved := func(t *testing.T) string {
		t.Helper()
		p, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	realTable := install.OpenMountinfo
	t.Cleanup(func() { install.OpenMountinfo = realTable })

	refused := func(t *testing.T, dataDir string) string {
		t.Helper()
		_, exit, stderr := serveHere(t, dataDir)
		code := awaitExit(t, exit)
		if code != 3 || !strings.Contains(stderr.String(), "mount_refused fs=nfs4") || !strings.Contains(stderr.String(), "local disk") {
			t.Fatalf("serve = exit %d, stderr %q; want exit 3 naming mount_refused fs=nfs4 and the fix", code, stderr.String())
		}
		return strings.TrimSpace(stderr.String())
	}

	t.Run("existing directory", func(t *testing.T) {
		dir := resolved(t)
		install.OpenMountinfo = table("nfs4", dir)
		msg := refused(t, dir)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("data directory holds %v (err %v) after the refusal, want it untouched", entries, err)
		}
		t.Logf("MOUNT-REFUSED existing: %s", msg)
	})

	t.Run("missing child directory", func(t *testing.T) {
		parent := resolved(t)
		install.OpenMountinfo = table("nfs4", parent)
		msg := refused(t, filepath.Join(parent, "missing", "child"))
		if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
			t.Errorf("%s holds %v (err %v) after the refusal, want the missing child not created", parent, entries, err)
		}
		t.Logf("MOUNT-REFUSED missing child: %s", msg)
	})

	t.Run("through a symlink", func(t *testing.T) {
		target, links := resolved(t), resolved(t)
		link := filepath.Join(links, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		install.OpenMountinfo = table("nfs4", target)
		msg := refused(t, filepath.Join(link, "data"))
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Errorf("%s holds %v (err %v) after the refusal, want it untouched", target, entries, err)
		}
		t.Logf("MOUNT-REFUSED symlink: %s", msg)
	})

	t.Run("an accepted filesystem starts", func(t *testing.T) {
		dir := resolved(t)
		install.OpenMountinfo = table("nfs4", "/somewhere/else")
		port, exit, stderr := serveHere(t, dir)
		awaitState(t, port, "ready", exit)
		terminateHere(t)
		if code := awaitExit(t, exit); code != 0 {
			t.Fatalf("serve exit = %d: %s", code, stderr.String())
		}
		for _, name := range []string{"boxset.lock", "boxset.db"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// A data directory shaped like a SQLite URI ("file:/x") is a relative
// directory, not an alias of "/x": two servers on "/x" and on "file:/x" each
// hold their own lock and their own database.
func TestFileShapedDirectoryIsNotAnAlias(t *testing.T) {
	cwd, ordinary := t.TempDir(), t.TempDir()
	shaped := "file:" + ordinary

	first := start(t, ordinary)
	first.waitReady(t)
	second := startIn(t, cwd, shaped)
	second.waitReady(t)

	own := filepath.Join(cwd, shaped)
	for _, p := range []string{filepath.Join(own, "boxset.lock"), filepath.Join(own, "boxset.db")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the file-shaped server did not use the directory %s: %v", own, err)
		}
	}
	id := func(dir string) string {
		db, err := sql.Open("sqlite", install.Layout{Dir: dir}.DB())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var v string
		if err := db.QueryRowContext(t.Context(), "SELECT install_id FROM installation").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if a, b := id(ordinary), id(own); a == b {
		t.Errorf("both servers run on installation %s: the file-shaped directory aliased the ordinary one", a)
	}
	t.Logf("FILE-SHAPED-DIR own directory %s, installations differ", own)
}
