//go:build integration && unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

func keyCLI(t *testing.T, dir, arguments string) ([]byte, int) {
	t.Helper()
	utility, err := exec.LookPath("script")
	if err != nil {
		t.Fatal("script is required for TTY proof")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, utility, "-q", "-e", "-c", binary+" api-key "+arguments, "/dev/null")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=dumb"}
	var output keyOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	if output.Len() > 4096 {
		t.Fatal("CLI output exceeded bound")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return output.Bytes(), exit.ExitCode()
	}
	if err != nil {
		t.Fatal("CLI execution failed")
	}
	return output.Bytes(), 0
}

type keyOutput struct{ bytes.Buffer }

func (b *keyOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("key_cli_output_limit")
	}
	return b.Buffer.Write(p)
}

// logStream bounds what a child's output may keep and can be read while the
// child runs: an arrangement must see an observation before it acts, never a
// line that could belong to either side of the action.
const logStreamCap = 64 << 10

type logStream struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := logStreamCap - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil // the rest is dropped, never buffered
}

func (l *logStream) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines is every retained line, the last one included only once its newline
// arrived is not required: a record is one write.
func (l *logStream) lines() []string { return strings.Split(l.String(), "\n") }

// await waits until a retained line carries every fragment and reports its index.
func (l *logStream) await(bound time.Duration, fragments ...string) int {
	deadline := time.Now().Add(bound)
	for {
		if i := indexOfFrom(l.lines(), 0, fragments...); i >= 0 {
			return i
		}
		if time.Now().After(deadline) {
			return -1
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// indexOfFrom reports the first line at or after from carrying every fragment.
func indexOfFrom(lines []string, from int, fragments ...string) int {
	for i := max(from, 0); i < len(lines); i++ {
		if carries(lines[i], fragments...) {
			return i
		}
	}
	return -1
}

func carries(line string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(line, fragment) {
			return false
		}
	}
	return true
}

// attr returns the value of one `key=value` field of a log line, or "".
func attr(line, key string) string {
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			return value
		}
	}
	return ""
}

func keyFixture(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateUser(t.Context(), db, auth.NewHasher(), name, "synthetic-key-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAPIKeyCLI(t *testing.T) {
	dir := keyFixture(t, "Émilie")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "api-key", "create", "--user", "Émilie", "--name", "automation", "--scope", "read")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir}
	var captured keyOutput
	cmd.Stdout, cmd.Stderr = &captured, &captured
	err := cmd.Run()
	out := captured.Bytes()
	defer clear(out)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || bytes.Contains(out, []byte("bxk_")) {
		t.Fatal("non-TTY command did not refuse safely")
	}
	const diagnosticSentinel = "synthetic-private-diagnostic"
	diagnostic := exec.CommandContext(ctx, binary, "serve")
	diagnostic.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	diagnostic.Cancel = func() error { return syscall.Kill(-diagnostic.Process.Pid, syscall.SIGKILL) }
	diagnostic.WaitDelay = 2 * time.Second
	diagnostic.Env = []string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=https://invalid.example/?apikey=" + diagnosticSentinel}
	var diagnosticOutput keyOutput
	diagnostic.Stdout, diagnostic.Stderr = &diagnosticOutput, &diagnosticOutput
	diagnosticError := diagnostic.Run()
	defer clear(diagnosticOutput.Bytes())
	var diagnosticExit *exec.ExitError
	if !errors.As(diagnosticError, &diagnosticExit) || diagnosticExit.ExitCode() != 2 || bytes.Contains(diagnosticOutput.Bytes(), []byte(diagnosticSentinel)) || !bytes.Contains(diagnosticOutput.Bytes(), []byte("[REDACTED]")) {
		t.Fatal("startup diagnostic bypassed shared redaction")
	}
	for _, redirect := range []string{" </dev/null", " >/dev/null"} {
		partial, status := keyCLI(t, dir, "create --user 'Émilie' --name automation --scope read"+redirect)
		defer clear(partial)
		if status != 2 || bytes.Contains(partial, []byte("bxk_")) {
			t.Fatal("each missing terminal must refuse before issuance")
		}
	}
	for _, spelling := range []string{"Émilie", "ÉMILIE"} {
		out, status := keyCLI(t, dir, "create --user '"+spelling+"' --name automation --scope read")
		defer clear(out)
		if status != 0 {
			t.Fatalf("TTY create status=%d", status)
		}
		tokens := regexp.MustCompile(`bxk_[A-Za-z0-9_-]{43}`).FindAll(out, -1)
		if len(tokens) != 1 {
			t.Fatal("token must print exactly once")
		}
		v := sha256.Sum256(tokens[0])
		var id, user int64
		var verifier string
		db, err := sql.Open("sqlite", filepath.Join(dir, "boxset.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT id,user_id,token_sha256 FROM api_key WHERE token_sha256=?", hex.EncodeToString(v[:])).Scan(&id, &user, &verifier); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if user != 1 || len(verifier) != 64 || strings.Contains(verifier, string(tokens[0])) {
			t.Fatal("key storage identity/verifier")
		}
		s := start(t, dir)
		s.waitReady(t)
		body, status := keyHTTP(t, s.port, string(tokens[0]))
		if status != 200 || bytes.Contains(body, tokens[0]) {
			t.Fatal("key authorized control or HTTP secrecy")
		}
		if spelling == "Émilie" {
			read, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "boxset.db"))+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			read.SetMaxOpenConns(1)
			ticker := time.NewTicker(50 * time.Millisecond)
			deadline := time.NewTimer(130 * time.Second)
			observed := false
			for !observed {
				var used int64
				if err := read.QueryRowContext(t.Context(), "SELECT last_used_at FROM api_key WHERE id=?", id).Scan(&used); err != nil {
					t.Fatal(err)
				}
				if used > 0 {
					observed = true
					break
				}
				select {
				case <-s.done:
					t.Fatal("server exited before metadata worker observation")
				case <-deadline.C:
					t.Fatal("worker did not persist queued key activity")
				case <-ticker.C:
				}
			}
			ticker.Stop()
			deadline.Stop()
			if err := read.Close(); err != nil {
				t.Fatal(err)
			}
		}
		locked, status := keyCLI(t, dir, fmt.Sprintf("revoke --id %d", id))
		defer clear(locked)
		if status != 3 || !bytes.Contains(locked, []byte("lock_held")) {
			t.Fatal("running server lock refusal")
		}
		if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if s.exited(t) != 0 {
			t.Fatal("worker shutdown")
		}
		lock, err := install.Acquire(dir)
		if err != nil {
			t.Fatal("worker retained lock after exit")
		}
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
		revoked, status := keyCLI(t, dir, fmt.Sprintf("revoke --id %d", id))
		defer clear(revoked)
		if status != 0 {
			t.Fatal("stopped revoke")
		}
		s = start(t, dir)
		s.waitReady(t)
		if _, status := keyHTTP(t, s.port, string(tokens[0])); status != 401 {
			t.Fatal("revoked credential accepted")
		}
		if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if s.exited(t) != 0 {
			t.Fatal("second shutdown")
		}
		if strings.Contains(s.stderr.String(), string(tokens[0])) {
			t.Fatal("key reached server logs")
		}
	}
	refused, status := keyCLI(t, dir, "create --user Emilie --name automation --scope read")
	defer clear(refused)
	if status != 3 || bytes.Contains(refused, []byte("bxk_")) {
		t.Fatal("distinct Unicode name accepted")
	}
	// A database with migrations still pending is ordinary CLI work: the store
	// opens, the interrupted-or-dead server does not leave it half migrated, and
	// the refusal names the key command rather than the store.
	t.Run("pending migration", func(t *testing.T) {
		pending := t.TempDir()
		pendingMigration(t, pending)
		created, status := keyCLI(t, pending, "create --user owner --name pending --scope read")
		defer clear(created)
		if status != 3 || bytes.Contains(created, []byte("store_unavailable")) || bytes.Contains(created, []byte("bxk_")) {
			t.Fatalf("pending migration status=%d, want 3 from the key command alone", status)
		}
		layout := install.Layout{Dir: pending}
		raw, err := sql.Open("sqlite", layout.DB())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := raw.Close(); err != nil {
				t.Error(err)
			}
		}()
		var version int64
		if err := raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 2 {
			t.Fatalf("pending migration left user_version %d, want the migrated 2", version)
		}
	})
	t.Log("API-KEY-CLI cases=2 unicode_ids=1 non_tty=2 lock_held=3 issued_once=true revoked=401")

	// The pending-migration case above passes whether the open waits on the
	// signal context or on a fixed drain deadline, because nothing interrupts it.
	// An external reservation obstructs the migration's BEGIN, so the open only
	// ends early if the stop reaches its context: the same obstructed open without
	// a stop stays there for the connection's busy timeout.
	t.Run("stop during the open", func(t *testing.T) {
		pending := t.TempDir()
		pendingMigration(t, pending)
		reserved, err := sql.Open("sqlite", install.Layout{Dir: pending}.DB())
		if err != nil {
			t.Fatal(err)
		}
		reserved.SetMaxOpenConns(1)
		t.Cleanup(func() {
			if err := reserved.Close(); err != nil {
				t.Error(err)
			}
		})
		holder, err := reserved.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
			t.Fatalf("external reservation: %v", err)
		}
		released := false
		release := func() {
			if released {
				return
			}
			released = true
			if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Error(err)
			}
			if err := holder.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(release)
		utility, err := exec.LookPath("script")
		if err != nil {
			t.Fatal("script is required for TTY proof")
		}
		layout := install.Layout{Dir: pending}
		// The stop must reach the CLI itself, so the shell keeps it as a background
		// job with its terminal explicitly restored (`/dev/tty` is the pseudo
		// terminal) and waits for it, reporting the CLI's own status.
		const pidMark = "BOXSET-CLI-PID="
		command := "'" + binary + "' api-key create --user owner --name open --scope read </dev/tty & echo " + pidMark + "$!; wait $!"
		launch := func(ctx context.Context) (*logStream, *exec.Cmd, int) {
			stream := &logStream{}
			cli := exec.CommandContext(ctx, utility, "-q", "-e", "-c", command, "/dev/null")
			cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cli.Cancel = func() error { return syscall.Kill(-cli.Process.Pid, syscall.SIGKILL) }
			cli.WaitDelay = 2 * time.Second
			cli.Env = []string{"BOXSET_DATA_DIR=" + pending, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=dumb"}
			cli.Stdout, cli.Stderr = stream, stream
			if err := cli.Start(); err != nil {
				t.Fatal(err)
			}
			marked := stream.await(10*time.Second, pidMark)
			pid, pidErr := strconv.Atoi(attr(stream.lines()[max(marked, 0)], pidMark[:len(pidMark)-1]))
			if marked < 0 || pidErr != nil || pid <= 0 {
				_ = cli.Process.Kill()
				_ = cli.Wait()
				t.Fatalf("the arrangement never reported the CLI process: %s", stream.String())
			}
			// The reservation holds the migration for the connection's busy timeout,
			// so the command is inside the open from here on.
			time.Sleep(200 * time.Millisecond)
			return stream, cli, pid
		}
		wait := func(cli *exec.Cmd) (int, error) {
			var exit *exec.ExitError
			switch err := cli.Wait(); {
			case err == nil:
				return 0, nil
			case errors.As(err, &exit):
				return exit.ExitCode(), nil
			default:
				return 0, err
			}
		}

		// The reservation holds the open for the connection's busy timeout: that it
		// lasts is what makes the stop land inside the open rather than before it.
		controlCtx, controlCancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer controlCancel()
		control, controlCLI, _ := launch(controlCtx)
		controlFrom := time.Now()
		controlCode, controlErr := wait(controlCLI)
		controlElapsed := time.Since(controlFrom)
		if controlErr != nil || controlCode != 3 || controlElapsed < 3*time.Second || !strings.Contains(control.String(), "store_unavailable") {
			t.Fatalf("the reservation did not obstruct the open: exit=%d after %v output=%q", controlCode, controlElapsed, control.String())
		}

		// The stop arrives while the open waits on the reservation. The busy wait
		// itself ignores it, so releasing the reservation lets the open reach its
		// next step: a context the stop canceled fails there, while one bounded by
		// a fixed deadline would apply the migration and issue a key.
		stopCtx, stopCancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer stopCancel()
		stream, cli, pid := launch(stopCtx)
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			t.Fatalf("the CLI was gone before the stop: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		from := time.Now()
		release()
		code, waitErr := wait(cli)
		elapsed := time.Since(from)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		out := stream.String()
		if code != 3 || !strings.Contains(out, "store_unavailable") || strings.Contains(out, "bxk_") {
			t.Fatalf("a stop during the open: exit=%d after %v, want 3 store_unavailable without a token: %q", code, elapsed, out)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("the open outlived the stop by %v: it must follow the signal context, not a drain deadline", elapsed)
		}
		// The canceled open issued nothing: the key table a completed open would have
		// created is absent, whatever migration each transaction managed to apply.
		raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(layout.DB())+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := raw.Close(); err != nil {
				t.Error(err)
			}
		}()
		var keys int64
		if err := raw.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name='api_key'").Scan(&keys); err != nil {
			t.Fatal(err)
		}
		if keys != 0 {
			t.Fatalf("a stopped open created the key table, so the open ran past the context the stop canceled")
		}
		t.Logf("OPEN-STOP obstructed_ms=%d stopped_ms=%d exit=3 store_unavailable token=0 key_table=absent", controlElapsed.Milliseconds(), elapsed.Milliseconds())
	})
}

func keyHTTP(t *testing.T, port, token string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+port+"/api/v1/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", token)
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil || len(body) > 8192 {
		t.Fatal("HTTP response bound")
	}
	return body, resp.StatusCode
}

func TestKeysOverLoopbackProxy(t *testing.T) {
	dir := keyFixture(t, "owner")
	s := startIn(t, "", dir, "BOXSET_TRUSTED_PROXIES=127.0.0.1/32", "BOXSET_LOCAL_BYPASS=127.0.0.0/8")
	s.waitReady(t)
	target, err := url.Parse("http://127.0.0.1:" + s.port)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}).DialContext}
	front := httptest.NewServer(proxy)
	defer front.Close()
	for _, tc := range []struct {
		url   string
		local string
		want  int
	}{{front.URL, "127.0.0.1", 401}, {target.String(), "127.0.0.2", 200}} {
		tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(tc.local)}}).DialContext}
		client := http.Client{Transport: tr, Timeout: 5 * time.Second}
		req, err := http.NewRequest("GET", tc.url+"/api/v1/system/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Forwarded", "for=127.0.0.2")
		req.Header.Set("X-Forwarded-For", "127.0.0.2")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		if resp.StatusCode != tc.want {
			t.Fatalf("proxy bypass status=%d want=%d", resp.StatusCode, tc.want)
		}
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if s.exited(t) != 0 {
		t.Fatal("bounded worker shutdown")
	}

	// The stop record and the store's admission snapshot are what proves the
	// once-per-minute worker was drained, not that it happened to be idle: an
	// external reservation obstructs the ordinary edit's BEGIN, so the edit holds
	// the production writer admission lease while the metadata attempt arrives
	// behind it, and the stop must end that attempt under the same deadline.
	t.Run("blocked writer drain", func(t *testing.T) {
		blockedWriterDrain(t)
	})
}

// startObserved is startIn with the server's output readable while it runs.
func startObserved(t *testing.T, dir string, extra ...string) (*proc, *logStream) {
	t.Helper()
	stream := &logStream{}
	s := &proc{port: freePort(t), done: make(chan struct{})}
	s.cmd = exec.Command(binary, "serve")
	s.cmd.Env = append([]string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=" + s.port}, extra...)
	s.cmd.Stderr = stream
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		<-s.done
	})
	return s, stream
}

func blockedWriterDrain(t *testing.T) {
	t.Helper()
	dir := keyFixture(t, "owner")
	created, status := keyCLI(t, dir, "create --user owner --name drain --scope read")
	defer clear(created)
	tokens := regexp.MustCompile(`bxk_[A-Za-z0-9_-]{43}`).FindAll(created, -1)
	if status != 0 || len(tokens) != 1 {
		t.Fatalf("key create status=%d tokens=%d", status, len(tokens))
	}
	s, stream := startObserved(t, dir)
	s.waitReady(t)
	base := "http://127.0.0.1:" + s.port + "/api/v1"
	client := &http.Client{Timeout: 10 * time.Second}
	// A cookie session and one key request queue both kinds of eligible activity.
	login, err := client.Post(base+"/session", "application/json", strings.NewReader(`{"username":"owner","password":"synthetic-key-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, login.Body)
	_ = login.Body.Close()
	if login.StatusCode != 201 || len(login.Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%d", login.StatusCode, len(login.Cookies()))
	}
	cookie := login.Cookies()[0]
	if body, status := keyHTTP(t, s.port, string(tokens[0])); status != 200 || bytes.Contains(body, tokens[0]) {
		t.Fatalf("key request status=%d", status)
	}

	// One external write reservation obstructs every writer of the store the
	// server owns; the ordinary same-value edit then blocks in BEGIN holding the
	// lease. A rollback is the only way out of the reservation.
	reserved, err := sql.Open("sqlite", install.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	reserved.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := reserved.Close(); err != nil {
			t.Error(err)
		}
	})
	holder, err := reserved.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("external reservation: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Error(err)
		}
		if err := holder.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(release)

	// Keep the lease occupied across the next eligible minute attempt: each edit
	// blocks in BEGIN while the reservation holds, so the edit in flight is the
	// lease holder the metadata attempt waits behind.
	stopEdits := make(chan struct{})
	editsDone := make(chan int, 1)
	go func() {
		edits := 0
		for {
			select {
			case <-stopEdits:
				editsDone <- edits
				return
			default:
			}
			req, err := http.NewRequest("PATCH", base+"/users/1", strings.NewReader(`{"expectedRevision":1,"role":"admin"}`))
			if err != nil {
				editsDone <- edits
				return
			}
			req.AddCookie(cookie)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			edits++
		}
	}()

	wait := stream.await(150*time.Second, "reason=metadata_wait")
	if wait < 0 {
		close(stopEdits)
		edits := <-editsDone
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		_ = s.exited(t)
		t.Fatalf("no metadata attempt waited behind the obstructed writer (edits=%d): %s", edits, stream.String())
	}
	waitLine := stream.lines()[wait]
	records := attr(waitLine, "records")
	generation := attr(waitLine, "generation")
	if records == "0" || records == "" || generation == "0" || generation == "" {
		t.Fatalf("metadata wait without a selected batch and a live holder: %s", waitLine)
	}

	// The stop arrives while the reservation still obstructs the writer.
	close(stopEdits)
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("stop exit=%d, want 0", code)
	}
	edits := <-editsDone
	release()

	lines := stream.lines()
	signal := indexOfFrom(lines, wait+1, "reason=signal_received")
	if signal < 0 {
		t.Fatalf("no signal-time admission snapshot: %s", stream.String())
	}
	signalLine := lines[signal]
	if attr(signalLine, "waiting") != "true" || attr(signalLine, "phase") != "waiting" || attr(signalLine, "records") != records || attr(signalLine, "ordinal") != attr(waitLine, "ordinal") || attr(signalLine, "generation") != generation {
		t.Fatalf("signal snapshot %q does not match the wait it must record (attempt %s with %s records behind generation %s)", signalLine, attr(waitLine, "ordinal"), records, generation)
	}
	if i := indexOfFrom(lines, 0, "reason=metadata_terminal", "outcome=deadline"); i >= 0 {
		t.Fatalf("the attempt ended on its own deadline before the stop: %s", lines[i])
	}
	terminal := indexOfFrom(lines, signal+1, "reason=metadata_terminal")
	if terminal < 0 {
		t.Fatalf("no metadata terminal record after the stop: %s", stream.String())
	}
	terminalLine := lines[terminal]
	if attr(terminalLine, "outcome") != "canceled" || attr(terminalLine, "records") != records || attr(terminalLine, "generation") != generation {
		t.Fatalf("terminal record %q, want canceled for the waited batch", terminalLine)
	}
	worker := indexOfFrom(lines, terminal+1, "reason=worker_terminal")
	joined := indexOfFrom(lines, worker+1, "reason=worker_joined")
	closed := indexOfFrom(lines, joined+1, "reason=store_closed")
	lockReleased := indexOfFrom(lines, closed+1, "reason=lock_released")
	if worker < 0 || joined < 0 || closed < 0 || lockReleased < 0 {
		t.Fatalf("the stop record is incomplete or out of order: %s", stream.String())
	}
	if cause := attr(lines[worker], "cause"); cause != "canceled" {
		t.Fatalf("worker terminal cause %q, want canceled", cause)
	}

	// No metadata value advanced: the attempt was ended, never committed, and the
	// obstructed edits rolled back with it.
	read, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "boxset.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := read.Close(); err != nil {
			t.Error(err)
		}
	}()
	read.SetMaxOpenConns(1)
	var used, revision int64
	if err := read.QueryRowContext(t.Context(), "SELECT last_used_at FROM api_key").Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := read.QueryRowContext(t.Context(), "SELECT revision FROM user WHERE id=1").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if used != 0 || revision != 1 {
		t.Fatalf("metadata advanced (last_used_at=%d) or an edit committed (revision=%d)", used, revision)
	}

	// The lock is reacquired only now that the process is gone, and a fresh
	// authorized request proves the store it left behind is still usable.
	lock, err := install.Acquire(dir)
	if err != nil {
		t.Fatal("the stop retained the lock")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	again := start(t, dir)
	again.waitReady(t)
	if _, status := keyHTTP(t, again.port, string(tokens[0])); status != 200 {
		t.Fatalf("restart control status=%d", status)
	}
	if err := again.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := again.exited(t); code != 0 {
		t.Fatalf("restart stop exit=%d", code)
	}
	t.Logf("METADATA-DRAIN edits=%d records=%s ordinal=%s generation=%s outcome=canceled sequence=signal,terminal,worker_joined,store_closed,lock_released last_used_at=0", edits, records, attr(waitLine, "ordinal"), generation)
}
