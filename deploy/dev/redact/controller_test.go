package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuf is a stderr double the signal goroutine and the test may both touch.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// diag delivers the controller's queued diagnostics into its stderr double and returns everything that arrived.
func diag(c *ctl, errb *syncBuf) string {
	c.flushDiag()
	return errb.String()
}

// testCtl is a controller bound to nothing but real local processes: no Docker, no
// private argv.
func testCtl(t *testing.T) (*ctl, *syncBuf) {
	t.Helper()
	errb := &syncBuf{}
	c := newCtl(errb)
	c.env = os.Environ()
	dn, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dn.Close() })
	c.devnull = dn
	c.out = &outWriter{w: io.Discard}
	return c, errb
}

func shArgv(script string, args ...string) []string {
	return append([]string{"/bin/sh", "-c", script, "sh"}, args...)
}

func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

const testRunID = "0123456789abcdef"

type runFixture struct{ repo, parent, tmpd string }

// newRunFixture is a repository directory and a bootstrap-shaped private directory
// whose bin/redact is a copy of this very test binary.
func newRunFixture(t *testing.T) runFixture {
	t.Helper()
	f := runFixture{repo: t.TempDir(), parent: t.TempDir()}
	f.tmpd = filepath.Join(f.parent, "boxset-dev-run."+testRunID)
	if err := os.MkdirAll(filepath.Join(f.tmpd, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.tmpd, 0o700); err != nil {
		t.Fatal(err)
	}
	copyExecutable(t, os.Args[0], filepath.Join(f.tmpd, "bin", "redact"))
	if err := os.WriteFile(filepath.Join(f.parent, "canary"), []byte("canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// args is the private argv after the program name.
func (f runFixture) args() []string {
	return []string{"-run", f.repo, testRunID, f.tmpd, "5", "0", "-", "-", "--", "true"}
}

func listing(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			out = append(out, p+" "+fi.Mode().String()+" "+strconv.FormatInt(fi.Size(), 10))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// runBinary runs exe as the controller binary entry point in dir, with an empty PATH.
func runBinary(t *testing.T, exe, dir string, args []string) (status int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = []string{"REDACT_TEST_MAIN=1", "PATH="}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		status = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return status, so.String(), se.String()
}

func TestRunArgvRejectedBeforeAnyEffect(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f runFixture)
		edit  func(a []string, f runFixture)
		exe   func(f runFixture) string
	}{
		{name: "missing separator", edit: func(a []string, _ runFixture) { a[8] = "x" }},
		{name: "missing command", edit: func(a []string, _ runFixture) { a[8] = "--" }, exe: nil},
		{name: "relative repo", edit: func(a []string, _ runFixture) { a[1] = "." }},
		{name: "repo with a trailing slash", edit: func(a []string, f runFixture) { a[1] = f.repo + "/" }},
		{name: "another directory as repo", edit: func(a []string, _ runFixture) { a[1] = t.TempDir() }},
		{name: "missing repo", edit: func(a []string, f runFixture) { a[1] = f.repo + "/missing" }},
		{name: "short runid", edit: func(a []string, _ runFixture) { a[2] = "0123" }},
		{name: "uppercase runid", edit: func(a []string, _ runFixture) { a[2] = "0123456789ABCDEF" }},
		{name: "long runid", edit: func(a []string, _ runFixture) { a[2] = testRunID + "0" }},
		{name: "relative tmpd", edit: func(a []string, _ runFixture) { a[3] = "boxset-dev-run." + testRunID }},
		{name: "tmpd with another basename", edit: func(a []string, f runFixture) { a[3] = filepath.Join(f.parent, "boxset-dev-run.ffffffffffffffff") }},
		{name: "tmpd with dot dot", edit: func(a []string, f runFixture) { a[3] = f.parent + "/../" + filepath.Base(f.parent) + "/boxset-dev-run." + testRunID }},
		{name: "tmpd mode 0755", setup: func(t *testing.T, f runFixture) {
			if err := os.Chmod(f.tmpd, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tmpd a symlink", setup: func(t *testing.T, f runFixture) {
			real := filepath.Join(f.parent, "real")
			if err := os.Rename(f.tmpd, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, f.tmpd); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tmpd without bin/redact", setup: func(t *testing.T, f runFixture) {
			if err := os.Remove(filepath.Join(f.tmpd, "bin", "redact")); err != nil {
				t.Fatal(err)
			}
		}, exe: func(runFixture) string { return os.Args[0] }}, // the surviving real test binary: only the private directory is malformed
		{name: "bin/redact is not the running executable", exe: func(f runFixture) string { return os.Args[0] }},
		{name: "timeout 0", edit: func(a []string, _ runFixture) { a[4] = "0" }},
		{name: "timeout 601", edit: func(a []string, _ runFixture) { a[4] = "601" }},
		{name: "timeout with a leading zero", edit: func(a []string, _ runFixture) { a[4] = "005" }},
		{name: "timeout with a sign", edit: func(a []string, _ runFixture) { a[4] = "+5" }},
		{name: "timeout not a number", edit: func(a []string, _ runFixture) { a[4] = "x" }},
		{name: "empty timeout", edit: func(a []string, _ runFixture) { a[4] = "" }},
		{name: "ref 2", edit: func(a []string, _ runFixture) { a[5] = "2" }},
		{name: "write outside the map table", edit: func(a []string, _ runFixture) { a[6] = "docs" }},
		{name: "write traversal", edit: func(a []string, _ runFixture) { a[6] = "../x" }},
		{name: "write with a trailing slash", edit: func(a []string, _ runFixture) { a[6] = "testdata/parser/" }},
		{name: "capture not a screenshots directory", edit: func(a []string, _ runFixture) { a[7] = "x" }},
		{name: "capture with an uppercase slug", edit: func(a []string, _ runFixture) { a[7] = ".devrites/work/UP/screenshots-20260101T000000Z" }},
		{name: "capture with a short stamp", edit: func(a []string, _ runFixture) { a[7] = ".devrites/work/a/screenshots-2026" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			a := f.args()
			if tc.name == "missing command" {
				a = a[:9]
			}
			if tc.edit != nil {
				tc.edit(a, f)
			}
			exe := filepath.Join(f.tmpd, "bin", "redact")
			if tc.exe != nil {
				exe = tc.exe(f)
			}
			before := listing(t, f.repo, f.parent)
			status, stdout, stderr := runBinary(t, exe, f.repo, a)
			if status != 125 || stdout != "" || stderr != "dev-run: redact_unavailable\n" {
				t.Fatalf("status %d, stdout %q, stderr %q, want 125 and exactly dev-run: redact_unavailable", status, stdout, stderr)
			}
			if after := listing(t, f.repo, f.parent); !slices.Equal(before, after) {
				t.Fatalf("a rejected private argv changed the filesystem:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

// A valid private argv reaches the first native start and removes only its own
// run-private directory; without any PATH that start is the image build, which
// fails before a container could exist.
func TestAcceptedBindingRemovesOnlyItsOwnDirectory(t *testing.T) {
	f := newRunFixture(t)
	before := listing(t, f.repo)
	status, stdout, stderr := runBinary(t, filepath.Join(f.tmpd, "bin", "redact"), f.repo, f.args())
	if status != 125 || stdout != "" || stderr != "dev-run: image_build_failed\n" {
		t.Fatalf("status %d, stdout %q, stderr %q, want 125 and exactly dev-run: image_build_failed", status, stdout, stderr)
	}
	if _, err := os.Lstat(f.tmpd); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the acquired run directory survived: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(f.parent, "canary")); err != nil || string(b) != "canary\n" {
		t.Fatalf("an unrelated file beside the run directory changed: %q %v", b, err)
	}
	if after := listing(t, f.repo); !slices.Equal(before, after) {
		t.Fatalf("the repository listing changed: %q then %q", before, after)
	}
}

func TestStartIssuesOnlyStartedNames(t *testing.T) {
	c, _ := testCtl(t)
	r := c.do(&callSpec{kind: kOther, argv: []string{"/nonexistent/argv0"}, issue: "boxset-dev-never", bound: 5 * time.Second})
	if r.startErr == nil || len(c.issued) != 0 {
		t.Fatalf("a start error issued %q (error %v), want nothing", c.issued, r.startErr)
	}
	r = c.do(&callSpec{kind: kOther, argv: shArgv("exit 3"), issue: "boxset-dev-started", bound: 5 * time.Second})
	if r.startErr != nil || r.status != 3 || !slices.Equal(c.issued, []string{"boxset-dev-started"}) {
		t.Fatalf("status %d, issued %q, error %v, want 3 and the started name", r.status, c.issued, r.startErr)
	}
	r = c.do(&callSpec{kind: kOther, argv: shArgv("kill -KILL $$"), bound: 5 * time.Second})
	if r.status != 137 {
		t.Fatalf("a signalled process has status %d, want 128+9", r.status)
	}
}

func TestCancelledCheckpointStartsNothing(t *testing.T) {
	c, _ := testCtl(t)
	c.latch()
	marker := filepath.Join(t.TempDir(), "marker")
	r := c.do(&callSpec{kind: kOther, argv: shArgv("touch " + marker), issue: "boxset-dev-cancelled", bound: 5 * time.Second})
	if !errors.Is(r.startErr, errCancelled) || len(c.issued) != 0 {
		t.Fatalf("a cancelled start returned %v and issued %q", r.startErr, c.issued)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a cancelled non-reconciliation start ran its process")
	}
	r = c.do(&callSpec{kind: kReconcile, argv: shArgv("touch " + marker), bound: 5 * time.Second})
	if r.startErr != nil || r.status != 0 {
		t.Fatalf("reconciliation after a latch: status %d, error %v", r.status, r.startErr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a reconciliation-kind start did not run: %v", err)
	}
}

// taskInfo is one task as /proc/<pid>/stat reports it.
type taskInfo struct {
	state           byte
	ppid, pgrp, sid int
}

// readTask observes one task. A task that no longer exists is gone; a read or parse failure of anything else
// fails the test and is never read as a task that is gone.
func readTask(t *testing.T, pid int) (ti taskInfo, gone bool) {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return taskInfo{}, true
	}
	if err != nil {
		t.Fatalf("task %d could not be observed: %v", pid, err)
	}
	s := string(b)
	var f []string
	if end := strings.LastIndexByte(s, ')'); end >= 0 {
		f = strings.Fields(s[end+1:])
	}
	if !strings.HasPrefix(s, strconv.Itoa(pid)+" (") || len(f) < 4 || len(f[0]) != 1 || !strings.Contains("RSDZTtXxKWPI", f[0]) {
		t.Fatalf("task %d has an unreadable stat record %q", pid, s)
	}
	ti.state = f[0][0]
	for i, p := range []*int{&ti.ppid, &ti.pgrp, &ti.sid} {
		if *p, err = strconv.Atoi(f[1+i]); err != nil {
			t.Fatalf("task %d has an unreadable stat record %q: %v", pid, s, err)
		}
	}
	return ti, false
}

// taskDead is a task that is gone or a zombie. A killed process its adopting parent has not reaped yet (PID 1 of the
// provider container is a sleep that never waits) keeps its /proc entry and answers kill(pid, 0), so only its state
// tells it from a live task.
func taskDead(t *testing.T, pid int) bool {
	t.Helper()
	ti, gone := readTask(t, pid)
	return gone || ti.state == 'Z' || ti.state == 'X' || ti.state == 'x'
}

// pipeInodes is every pipe this process holds open, as the "pipe:[inode]" link text of its descriptors.
func pipeInodes(t *testing.T) map[string]bool {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	pipes := map[string]bool{}
	for _, e := range ents {
		l, err := os.Readlink("/proc/self/fd/" + e.Name())
		if errors.Is(err, fs.ErrNotExist) { // a descriptor closed since the listing
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, "pipe:[") {
			pipes[l] = true
		}
	}
	return pipes
}

func TestHandledSignalKillsOnlyNonCreateWork(t *testing.T) {
	c, errb := testCtl(t)
	stop := c.installSignals()
	defer stop()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")

	createDone := make(chan callResult, 1)
	go func() {
		createDone <- c.do(&callSpec{kind: kCreate, argv: shArgv("sleep 1; exit 0"), bound: 10 * time.Second})
	}()
	otherDone := make(chan callResult, 1)
	go func() {
		otherDone <- c.do(&callSpec{kind: kOther, argv: shArgv("sleep 30 & echo $! >" + pidfile + "; wait"), bound: 40 * time.Second})
	}()
	var gpid int
	for i := 0; i < 100 && gpid == 0; i++ {
		if b, err := os.ReadFile(pidfile); err == nil {
			gpid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gpid == 0 {
		t.Fatal("the in-flight process never recorded its grandchild")
	}
	t.Cleanup(func() {
		if !taskDead(t, gpid) {
			_ = syscall.Kill(gpid, syscall.SIGKILL)
		}
	})
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil { // a repeated signal is idempotent
		t.Fatal(err)
	}
	select {
	case r := <-otherDone:
		if r.status != 137 {
			t.Fatalf("the in-flight non-create call ended with status %d, want its group SIGKILLed (137)", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight non-create call survived the signal")
	}
	dead := false
	for i := 0; i < 100 && !dead; i++ {
		if dead = taskDead(t, gpid); !dead {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !dead {
		t.Fatal("the grandchild of the killed group survived as a live task")
	}
	select {
	case r := <-createDone:
		if r.status != 0 {
			t.Fatalf("the in-flight create was not waited to its own status: %d", r.status)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the in-flight create never returned")
	}
	if !c.cancelled.Load() {
		t.Fatal("the cancellation was not latched")
	}
	got := diag(c, errb)
	if n := strings.Count(got, "dev-run: interrupted\n"); n != 1 {
		t.Fatalf("%d interrupted lines in %q, want exactly one", n, got)
	}
}

// sigCellEnv marks the re-executed test binary that is one real installSignals cell. Its inherited HUP and INT
// dispositions are set before exec (env --default-signal or --ignore-signal, as the driver does) and read back from
// the kernel, so an ignored disposition is never reset inside the process under test and nothing is skipped.
const (
	sigCellEnv  = "REDACT_SIGNAL_CELL"
	sigCellTest = "TestHupAndIntLatchTheCancellationUnlessInheritedIgnored"
)

// signalCell is the child half of a cell (mode:signal). It announces itself, waits for the parent to read its
// dispositions back, installs the real handlers, signals itself and reports what latched; it never returns.
func signalCell(t *testing.T, cell string) {
	mode, name, _ := strings.Cut(cell, ":")
	sig := map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT}[name]
	if sig == 0 || (mode != "default" && mode != "ignored") {
		t.Fatalf("malformed signal cell %q", cell)
	}
	if signal.Ignored(sig) != (mode == "ignored") {
		t.Fatalf("%v is ignored at entry: %v, the cell is %s", sig, signal.Ignored(sig), mode)
	}
	os.Stdout.WriteString("ready\n")
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	c, errb := testCtl(t)
	stop := c.installSignals()
	lines := func() int { return strings.Count(diag(c, errb), "dev-run: interrupted\n") }
	latched := func(window time.Duration) string {
		select {
		case <-c.cancelCh:
			for i := 0; i < 250 && lines() == 0; i++ { // the acknowledgement follows the latch
				time.Sleep(20 * time.Millisecond)
			}
			return "latched"
		case <-time.After(window):
			return "unlatched"
		}
	}
	window := 5 * time.Second
	if mode == "ignored" {
		window = 500 * time.Millisecond
	}
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
	res := "sig=" + latched(window) + " interrupted=" + strconv.Itoa(lines())
	if mode == "ignored" {
		// The same installed handlers still latch: the unlatched answer above is the signal's disposition, not a dead handler.
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		res += " term=" + latched(5*time.Second) + " interrupted=" + strconv.Itoa(lines())
	}
	stop()
	os.Stdout.WriteString(res + "\n")
	os.Exit(0)
}

// runSignalCell is the parent half: it establishes the disposition before exec, reads the child's SigIgn mask back
// from the kernel and only then lets the child install the controller's handlers and signal itself.
func runSignalCell(t *testing.T, mode, sig string) {
	t.Helper()
	flag := "--default-signal=HUP,INT"
	if mode == "ignored" {
		flag = "--ignore-signal=HUP,INT"
	}
	cmd := exec.Command("/usr/bin/env", flag, os.Args[0], "-test.run=^"+sigCellTest+"$")
	cmd.Env = append(os.Environ(), sigCellEnv+"="+mode+":"+sig)
	var se syncBuf
	cmd.Stderr = &se
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	guard := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
	defer guard.Stop()
	br := bufio.NewReader(out)
	if line, err := br.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the cell never announced itself: %q, %v, stderr %q", line, err, se.String())
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/status")
	if err != nil {
		t.Fatalf("the cell's signal dispositions could not be read: %v", err)
	}
	f, ok := statusField(string(status), "SigIgn")
	if !ok || len(f) != 1 {
		t.Fatalf("the cell's SigIgn mask is missing from %q", status)
	}
	mask, err := strconv.ParseUint(f[0], 16, 64)
	if err != nil {
		t.Fatalf("the cell's SigIgn mask %q is unreadable: %v", f[0], err)
	}
	wantMask := uint64(0)
	if mode == "ignored" {
		wantMask = 3
	}
	if mask&3 != wantMask {
		t.Fatalf("the inherited disposition (%s) was not established: SigIgn mask %d, want %d", mode, mask&3, wantMask)
	}
	if _, err := in.Write([]byte("g")); err != nil {
		t.Fatal(err)
	}
	in.Close()
	rest, err := io.ReadAll(br)
	waited = true
	if werr := cmd.Wait(); err != nil || werr != nil {
		t.Fatalf("the cell failed: read %v, wait %v, stdout %q, stderr %q", err, werr, rest, se.String())
	}
	want := "sig=latched interrupted=1\n"
	if mode == "ignored" {
		want = "sig=unlatched interrupted=0 term=latched interrupted=1\n"
	}
	if string(rest) != want {
		t.Fatalf("%s %s: the cell reported %q, want %q (stderr %q)", mode, sig, rest, want, se.String())
	}
}

// HUP and INT latch the same cancellation as TERM unless they were ignored at entry (an ignored one stays ignored
// and is never registered). Each cell is a real process whose disposition is inherited, never reset.
func TestHupAndIntLatchTheCancellationUnlessInheritedIgnored(t *testing.T) {
	if cell := os.Getenv(sigCellEnv); cell != "" {
		signalCell(t, cell)
	}
	for _, mode := range []string{"default", "ignored"} {
		for _, sig := range []string{"HUP", "INT"} {
			t.Run(mode+" "+sig, func(t *testing.T) { runSignalCell(t, mode, sig) })
		}
	}
}

// holderScript starts a holder that leaves the shell's process group for a session of its own, holding the call's
// stdout pipe, and exits only after the holder has recorded its pid: the group kill of the finished shell can never
// race the escape.
const holderScript = `setsid sh -c 'echo $$ >"$1"; exec sleep 30' sh "$1" & while [ ! -s "$1" ]; do sleep 0.01; done`

func TestEscapedPipeHolderIsBounded(t *testing.T) {
	const bound = 2 * time.Second
	c, _ := testCtl(t)
	pidfile := filepath.Join(t.TempDir(), "holder.pid")
	before := pipeInodes(t)
	type outcome struct {
		r    callResult
		took time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		began := time.Now()
		r := c.do(&callSpec{kind: kOther, argv: shArgv(holderScript, pidfile), stdout: captureFn(1024), bound: bound})
		done <- outcome{r, time.Since(began)}
	}()
	holder := 0
	for i := 0; i < 150 && holder == 0; i++ {
		if b, err := os.ReadFile(pidfile); err == nil {
			holder, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if holder == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if holder == 0 {
		<-done
		t.Fatal("the escaped holder never recorded its pid")
	}
	t.Cleanup(func() {
		if !taskDead(t, holder) {
			_ = syscall.Kill(holder, syscall.SIGKILL)
		}
	})
	// The adversary is real before anything is observed: alive, alone in its own session and holding the pipe this
	// very call created.
	ti, gone := readTask(t, holder)
	if gone || ti.state == 'Z' || ti.state == 'X' || ti.state == 'x' {
		t.Fatalf("the escaped holder %d is not a live task", holder)
	}
	if ti.pgrp != holder || ti.sid != holder {
		t.Fatalf("the escaped holder %d is in process group %d, session %d, not its own", holder, ti.pgrp, ti.sid)
	}
	link, err := os.Readlink("/proc/" + strconv.Itoa(holder) + "/fd/1")
	if err != nil {
		t.Fatalf("the escaped holder's stdout could not be read: %v", err)
	}
	if now := pipeInodes(t); !strings.HasPrefix(link, "pipe:[") || !now[link] || before[link] {
		t.Fatalf("the escaped holder's stdout is %q, not the pipe the controller created for this call", link)
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(3 * bound):
		t.Fatal("the call never returned")
	}
	if o.r.startErr != nil || o.r.status != 0 {
		t.Fatalf("status %d, error %v, want the shell's own 0", o.r.status, o.r.startErr)
	}
	if o.took > 2*bound {
		t.Fatalf("the call took %v with an escaped descendant holding its pipe, want it bounded by its 2 s", o.took)
	}
	if o.took < bound {
		t.Fatalf("the call took %v, so it ended before its 2 s bound while a live holder owned its pipe", o.took)
	}
	if _, ok := o.r.text(); ok {
		t.Fatal("an escaped pipe holder still produced a complete observation")
	}
	if taskDead(t, holder) {
		t.Fatalf("the escaped holder %d did not outlive the call", holder)
	}
}

// blockedWriter is a sink that never accepts a byte until it is released; entered is told when a Write first reached
// it, so a test establishes that its producer really arrived at the sink. After the release it forwards to to when
// there is one.
type blockedWriter struct {
	release chan struct{}
	entered chan struct{}
	to      io.Writer
}

func newBlockedWriter(to io.Writer) blockedWriter {
	return blockedWriter{release: make(chan struct{}), entered: make(chan struct{}, 1), to: to}
}

func (b blockedWriter) Write(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	if b.to != nil {
		return b.to.Write(p)
	}
	return 0, errors.New("released")
}

// errWriter is a sink that is already gone.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBlockedSinkIsBounded(t *testing.T) {
	c, _ := testCtl(t)
	sink := newBlockedWriter(nil)
	defer close(sink.release)
	pidfile := filepath.Join(t.TempDir(), "producer.pid")
	began := time.Now()
	done := make(chan callResult, 1)
	go func() {
		done <- c.do(&callSpec{
			kind: kOther,
			argv: shArgv(`echo $$ >"$1"; head -c 1000000 /dev/zero; exec sleep 30`, pidfile),
			stdout: func(in io.Reader) any {
				return redactStream(nil, streamOpts{capN: 1 << 30, raw: -1, tail: -1}, in, sink)
			},
			bound: 2 * time.Second,
		})
	}()
	select {
	case <-sink.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no byte ever reached the sink: the call proves nothing about a blocked one")
	}
	b, err := os.ReadFile(pidfile)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || perr != nil {
		t.Fatalf("the producer left no pid (%v, %v)", err, perr)
	}
	if taskDead(t, pid) {
		t.Fatalf("the producer %d was not live while the sink was blocked", pid)
	}
	var r callResult
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the call is not bounded by its 2 s with a blocked sink")
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Fatalf("the call took %v with a blocked sink, want it bounded by its 2 s", took)
	}
	if r.startErr != nil || r.status != 137 {
		t.Fatalf("start error %v, status %d, want a started producer killed at its bound (137)", r.startErr, r.status)
	}
	if r.outOK {
		t.Fatal("a sink that accepted nothing reported a complete stream")
	}
	dead := false
	for i := 0; i < 100 && !dead; i++ {
		if dead = taskDead(t, pid); !dead {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !dead {
		t.Fatalf("the producer %d outlived the bounded call", pid)
	}
}

// A diagnostic sink that is blocked, or flooded past its byte bound, never holds the lifecycle owner: the owner only
// enqueues, the failure state stays in the controller, and a flush at exit is itself bounded.
func TestDiagnosticSinkNeverHoldsTheLifecycleOwner(t *testing.T) {
	for _, n := range []int{1000, 10000} {
		var got syncBuf
		sink := newBlockedWriter(&got)
		c := newCtl(sink)
		began := time.Now()
		c.say("first")
		select {
		case <-sink.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the first diagnostic never reached the sink")
		}
		for range n {
			c.say("cleanup_failed container=boxset-dev-0123456789abcdef")
		}
		c.raw(bytes.Repeat([]byte("x"), 1<<20))
		c.bad("quiesce_failed")
		c.interrupt()
		if took := time.Since(began); took > time.Second {
			t.Fatalf("%d diagnostics took the owner %v with a blocked sink", n, took)
		}
		if c.first125 != "quiesce_failed" || !c.cancelled.Load() {
			t.Fatalf("the failure state was lost with the sink blocked: first125 %q, cancelled %v", c.first125, c.cancelled.Load())
		}
		flushBegan := time.Now()
		c.flushDiag()
		if took := time.Since(flushBegan); took > diagFlush+time.Second {
			t.Fatalf("the exit flush took %v, want it bounded by %v", took, diagFlush)
		}
		close(sink.release)
		c.flushDiag()
		text := got.String()
		if !strings.HasPrefix(text, "dev-run: first\n") || strings.Contains(text, "xxxx") || len(text) > diagCap+64 {
			t.Fatalf("%d diagnostics: delivered %d bytes, prefix %q, want the first line, no oversized raw block and at most %d bytes", n, len(text), text[:min(len(text), 20)], diagCap)
		}
		if n == 1000 && (strings.Count(text, "cleanup_failed") != n || !strings.Contains(text, "dev-run: quiesce_failed\n") || strings.Count(text, "dev-run: interrupted\n") != 1) {
			t.Fatalf("%d diagnostics within the bound were not all delivered in %d bytes", n, len(text))
		}
	}
}

func TestAnnounceReportsAClosedOrBlockedSink(t *testing.T) {
	c, _ := testCtl(t)
	var buf bytes.Buffer
	c.out = &outWriter{w: &buf}
	if !c.announce("line\n") || buf.String() != "line\n" {
		t.Fatalf("an open sink: delivered %q", buf.String())
	}
	c.out = &outWriter{w: errWriter{}}
	if c.announce("line\n") {
		t.Fatal("a closed sink reported a delivered locator")
	}
	sink := newBlockedWriter(nil)
	defer close(sink.release)
	c.out = &outWriter{w: sink}
	began := time.Now()
	if c.announce("line\n") {
		t.Fatal("a blocked sink reported a delivered locator")
	}
	if took := time.Since(began); took < announceBound || took > announceBound+2*time.Second || !c.out.dead.Load() {
		t.Fatalf("the blocked delivery took %v (abandoned %v), want its bound %v", took, c.out.dead.Load(), announceBound)
	}
}

// An output failure starts the owned removal inside the parent's own wait: a removal that is held must not stop the
// parent's deadline, and its status stays sticky. The stand-in `timeout` is the first word of every removal.
func TestOutputFailureRemovalKeepsTheParentSupervised(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		failed     bool
	}{{"removal exits 0", "0", false}, {"removal exits 7", "7", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bindir := filepath.Join(dir, "bin")
			if err := os.Mkdir(bindir, 0o755); err != nil {
				t.Fatal(err)
			}
			release := filepath.Join(dir, "release")
			tool := "#!/bin/sh\necho \"$@\" >" + filepath.Join(dir, "rm.args") + "\necho $$ >" + filepath.Join(dir, "rm.pid") +
				"\nwhile [ ! -s " + release + " ]; do sleep 0.02; done\nexit \"$(cat " + release + ")\"\n"
			if err := os.WriteFile(filepath.Join(bindir, "timeout"), []byte(tool), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bindir+":"+os.Getenv("PATH"))
			c, _ := testCtl(t)
			c.ctr = "boxset-dev-" + testRunID
			parent := filepath.Join(dir, "parent.pid")
			began := time.Now()
			done := make(chan callResult, 1)
			go func() {
				done <- c.do(&callSpec{
					kind: kOther,
					argv: shArgv(`echo $$ >"$1"; head -c 100000 /dev/zero; exec sleep 30`, parent),
					stdout: func(in io.Reader) any {
						return redactStream(nil, streamOpts{capN: 1 << 30, raw: -1, tail: -1}, in, errWriter{})
					},
					bound:       time.Second,
					removeOnOut: c.ctr,
				})
			}()
			readPid := func(name string) int {
				for range 200 {
					if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
							return pid
						}
					}
					time.Sleep(50 * time.Millisecond)
				}
				t.Fatalf("%s was never recorded", name)
				return 0
			}
			parentPid, rmPid := readPid("parent.pid"), readPid("rm.pid")
			for i := 0; i < 100 && !taskDead(t, parentPid); i++ {
				time.Sleep(50 * time.Millisecond)
			}
			if !taskDead(t, parentPid) {
				t.Fatalf("the parent %d survived its 1 s bound while its removal was held", parentPid)
			}
			select {
			case <-done:
				t.Fatal("the call returned before its held removal ended")
			default:
			}
			if took := time.Since(began); took > 5*time.Second || taskDead(t, rmPid) {
				t.Fatalf("after %v the removal %d is dead: %v, want it held while the parent bound was serviced", took, rmPid, taskDead(t, rmPid))
			}
			if err := os.WriteFile(release, []byte(tc.code), 0o644); err != nil {
				t.Fatal(err)
			}
			var r callResult
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the call never returned after its removal was released")
			}
			if st, _ := r.out.(int); st != exitWrite {
				t.Fatalf("the output stop is %v, want exitWrite", r.out)
			}
			if args, _ := os.ReadFile(filepath.Join(dir, "rm.args")); string(args) != "30 docker rm -f "+c.ctr+"\n" {
				t.Fatalf("the removal was asked %q", args)
			}
			if got := slices.Equal(c.rmFail, []string{c.ctr}); got != tc.failed || (!tc.failed && len(c.rmFail) != 0) {
				t.Fatalf("sticky removal failures %q, want failed=%v", c.rmFail, tc.failed)
			}
		})
	}
}

func TestRelativePathEntryResolvesAgainstTheOriginalCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("bin/dev-run-fixture-tool", []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "bin:/usr/bin:/bin")
	if _, err := exec.LookPath("dev-run-fixture-tool"); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("premise: the lookup through a relative PATH entry gave %v, want exec.ErrDot", err)
	}
	c, _ := testCtl(t)
	r := c.do(&callSpec{kind: kOther, argv: []string{"dev-run-fixture-tool"}, bound: 5 * time.Second})
	if r.startErr != nil || r.status != 7 {
		t.Fatalf("status %d, error %v, want the relative entry's tool to run and exit 7", r.status, r.startErr)
	}
}

func abortFixture(t *testing.T) (c *ctl, repo string) {
	t.Helper()
	repo = t.TempDir()
	c, _ = testCtl(t)
	c.repo = repo
	var err error
	if c.repoRoot, err = os.OpenRoot(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.repoRoot.Close() })
	c.slug, c.leaf = "feat", "screenshots-20260101T000000Z"
	c.capture = ".devrites/work/feat/" + c.leaf
	if err := os.MkdirAll(filepath.Join(repo, ".devrites/work/feat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".devrites/work/elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.repoRoot.Mkdir(c.capture, 0o700); err != nil {
		t.Fatal(err)
	}
	if c.croot, err = c.repoRoot.OpenRoot(c.capture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.croot.Close() })
	var ok bool
	if c.cid, ok = rootID(c.croot); !ok {
		t.Fatal("no identity for the pinned directory")
	}
	c.captureClaimed = true
	for _, n := range []string{"dev-desktop.png", ".dev-mobile.png.part", ".capture.manifest.part"} {
		if err := os.WriteFile(filepath.Join(repo, c.capture, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return c, repo
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestCaptureAbortUnmovedRemovesItsOwnDirectory(t *testing.T) {
	c, repo := abortFixture(t)
	c.captureAbort()
	if exists(filepath.Join(repo, c.capture)) {
		t.Fatal("an unmoved, runner-owned capture directory survived the abort")
	}
}

func TestCaptureAbortMovedDirectoryUsesItsActualParent(t *testing.T) {
	c, repo := abortFixture(t)
	moved := filepath.Join(repo, ".devrites/work/elsewhere", c.leaf)
	if err := os.Rename(filepath.Join(repo, c.capture), moved); err != nil {
		t.Fatal(err)
	}
	// A foreign directory now stands where the approved path was.
	if err := os.Mkdir(filepath.Join(repo, c.capture), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, c.capture, "keep"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.captureAbort()
	if exists(moved) {
		t.Fatal("the moved object was not removed through its actual parent")
	}
	if b, err := os.ReadFile(filepath.Join(repo, c.capture, "keep")); err != nil || string(b) != "foreign" {
		t.Fatalf("the foreign replacement at the old lexical path did not survive: %q %v", b, err)
	}
}

func TestCaptureAbortMovedUnderAnotherNameKeepsTheDirectoryEmptied(t *testing.T) {
	c, repo := abortFixture(t)
	moved := filepath.Join(repo, ".devrites/work/elsewhere/other")
	if err := os.Rename(filepath.Join(repo, c.capture), moved); err != nil {
		t.Fatal(err)
	}
	c.captureAbort()
	entries, err := os.ReadDir(moved)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the renamed object holds %v (%v), want it kept and emptied", entries, err)
	}
}

func TestCaptureAbortKeepsAForeignEntryAndItsDirectory(t *testing.T) {
	c, repo := abortFixture(t)
	foreign := filepath.Join(repo, c.capture, "foreign.txt")
	if err := os.WriteFile(foreign, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.captureAbort()
	if !exists(foreign) {
		t.Fatal("an entry the runner does not own was removed")
	}
}

func publishFixture(t *testing.T) (c *ctl, dir string) {
	t.Helper()
	repo := t.TempDir()
	c, _ = testCtl(t)
	c.repo, c.write, c.runid = repo, "testdata/parser", testRunID
	c.writeComps = strings.Split(c.write, "/")
	c.mapNames = []string{"sonarr.jsonl", "radarr.jsonl"}
	dir = filepath.Join(repo, c.write)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	if c.repoRoot, err = os.OpenRoot(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.repoRoot.Close() })
	if c.wroot, err = c.repoRoot.OpenRoot(c.write); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.wroot.Close() })
	var ok bool
	if c.wid, ok = rootID(c.wroot); !ok {
		t.Fatal("no identity for the pinned write root")
	}
	c.staged = []string{"sonarr.jsonl", "radarr.jsonl"}
	return c, dir
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// A failure after the first rename took effect restores every prior state: the
// prior inode and bytes of the replaced name return and nothing owned remains.
func TestPublishRollsBackWhenALaterRenameFails(t *testing.T) {
	c, dir := publishFixture(t)
	for n, body := range map[string]string{"sonarr.jsonl": "prior sonarr", "radarr.jsonl": "prior radarr"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s0, r0 := inode(t, filepath.Join(dir, "sonarr.jsonl")), inode(t, filepath.Join(dir, "radarr.jsonl"))
	if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory cannot replace a regular file: the second rename fails.
	if err := os.Mkdir(filepath.Join(dir, ".radarr.jsonl."+testRunID+".part"), 0o755); err != nil {
		t.Fatal(err)
	}
	if reason := c.publishInRoot(); reason != "write_publish_failed" {
		t.Fatalf("reason %q, want write_publish_failed", reason)
	}
	for n, want := range map[string]struct {
		body string
		ino  uint64
	}{"sonarr.jsonl": {"prior sonarr", s0}, "radarr.jsonl": {"prior radarr", r0}} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || string(b) != want.body || inode(t, filepath.Join(dir, n)) != want.ino {
			t.Fatalf("%s is %q (%v) on inode %d, want its prior bytes on inode %d", n, b, err, inode(t, filepath.Join(dir, n)), want.ino)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("an owned %s was left behind", e.Name())
		}
	}
}

// A name replaced by a foreign file after this run's rename is never overwritten
// by the rollback; its backup is kept and the conflict is reported.
func TestPublishRollbackKeepsAForeignReplacement(t *testing.T) {
	c, dir := publishFixture(t)
	c.staged = []string{"sonarr.jsonl"}
	if err := os.WriteFile(filepath.Join(dir, "sonarr.jsonl"), []byte("prior sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	errb := &syncBuf{}
	c.diag = newDiagSink(errb)
	// The sequence publishInRoot follows, with a foreign file standing in for the name
	// between the rename and the failure that triggers the rollback.
	q := inode(t, filepath.Join(dir, "sonarr.jsonl"))
	qid, _ := c.identOf("sonarr.jsonl")
	pid, _ := c.identOf(".sonarr.jsonl." + testRunID + ".part")
	c.partP["sonarr.jsonl"], c.partQ["sonarr.jsonl"] = pid, qid
	if err := c.wroot.Link("sonarr.jsonl", ".sonarr.jsonl."+testRunID+".prev"); err != nil {
		t.Fatal(err)
	}
	if err := c.renameOne(".sonarr.jsonl."+testRunID+".part", "sonarr.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".foreign"), []byte("foreign bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, ".foreign"), filepath.Join(dir, "sonarr.jsonl")); err != nil {
		t.Fatal(err)
	}
	c.rollback()
	if b, _ := os.ReadFile(filepath.Join(dir, "sonarr.jsonl")); string(b) != "foreign bytes" {
		t.Fatalf("the rollback overwrote the foreign replacement: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")); string(b) != "prior sonarr" || inode(t, filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")) != q {
		t.Fatalf("the prior backup was not kept byte-exact on its inode: %q", b)
	}
	if got := diag(c, errb); !strings.Contains(got, "dev-run: publish_rollback_conflict root=testdata/parser name=sonarr.jsonl\n") {
		t.Fatalf("no conflict line in %q", got)
	}
}

func TestPublishRefusesAnythingButARegularTarget(t *testing.T) {
	c, dir := publishFixture(t)
	c.staged = []string{"sonarr.jsonl"}
	if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "sonarr.jsonl")); err != nil {
		t.Fatal(err)
	}
	if reason := c.publishInRoot(); reason != "write_publish_failed" {
		t.Fatalf("reason %q, want write_publish_failed for a symlink target", reason)
	}
	if got, err := os.Readlink(filepath.Join(dir, "sonarr.jsonl")); err != nil || got != "/etc/passwd" {
		t.Fatalf("the symlink target was changed: %q %v", got, err)
	}
}

func TestProcOKGroupsPredicate(t *testing.T) {
	status := func(groups string) string {
		return "Uid:\t1000\t1000\t1000\t1000\nGid:\t1000\t1000\t1000\t1000\nGroups:" + groups + "\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\n"
	}
	for name, tc := range map[string]struct {
		text string
		want bool
	}{
		"empty":     {status("\t"), true},
		"primary":   {status("\t1000 "), true},
		"extra":     {status("\t1000 4242 "), false},
		"duplicate": {status("\t1000 1000 "), false},
		"foreign":   {status("\t4242 "), false},
		"malformed": {status("\tx"), false},
		"missing":   {strings.Replace(status("\t"), "Groups:\t\n", "", 1), false},
		"caps":      {strings.Replace(status("\t"), "CapBnd:\t0000000000000000", "CapBnd:\t00000000a80425fb", 1), false},
		"newprivs":  {strings.Replace(status("\t"), "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1), false},
		"other uid": {strings.Replace(status("\t"), "Uid:\t1000\t1000\t1000\t1000", "Uid:\t1000\t1000\t1000\t0", 1), false},
	} {
		if got := procOK(tc.text, "1000"); got != tc.want {
			t.Errorf("%s: procOK = %v, want %v", name, got, tc.want)
		}
	}
}

func TestCheckMapFacts(t *testing.T) {
	mount := "MOUNT tmpfs /src/testdata/parser tmpfs rw,nosuid,nodev,noexec,relatime,size=32768k,nr_inodes=8,mode=755,uid=65534,gid=65534 0 0\n"
	files := "F sonarr.jsonl 11 65534 65534 666 1 f 0\nF radarr.jsonl 12 65534 65534 666 1 f 0\n"
	anc := "A / 0 0 755\nA /src 0 0 1777\nA /src/testdata 0 0 755\n"
	names := []string{"sonarr.jsonl", "radarr.jsonl"}
	free, sizes, inodes, ok := checkMap(mount+files+"FREE 8192\n"+anc, true, writeMapTokens, capMap, names, nil)
	if !ok || free != "8192" || len(sizes) != 2 || inodes["sonarr.jsonl"] != 11 || inodes["radarr.jsonl"] != 12 {
		t.Fatalf("a correct preflight was refused: %v %q %v %v", ok, free, sizes, inodes)
	}
	for name, text := range map[string]string{
		"no size option":      strings.Replace(mount, "size=32768k,", "", 1) + files + "FREE 1\n" + anc,
		"writable ancestor":   mount + files + "FREE 1\nA / 0 0 777\n",
		"owned ancestor":      mount + files + "FREE 1\nA / 1000 0 755\n",
		"missing file":        mount + "F sonarr.jsonl 11 65534 65534 666 1 f 0\nFREE 1\n" + anc,
		"extra name":          mount + files + "F other 13 65534 65534 666 1 f 0\nFREE 1\n" + anc,
		"two links":           mount + strings.Replace(files, "666 1 f", "666 2 f", 1) + "FREE 1\n" + anc,
		"child owned":         mount + strings.Replace(files, "65534 65534 666", "1000 1000 666", 1) + "FREE 1\n" + anc,
		"nonzero at preflight": mount + strings.Replace(files, "f 0\nF radarr", "f 5\nF radarr", 1) + "FREE 1\n" + anc,
		"two mounts":          mount + mount + files + "FREE 1\n" + anc,
		"no free blocks line": mount + files + anc,
	} {
		if _, _, _, ok := checkMap(text, true, writeMapTokens, capMap, names, nil); ok {
			t.Errorf("%s: a wrong preflight was accepted", name)
		}
	}
	audit := mount + "F sonarr.jsonl 11 65534 65534 666 1 f 100\nF radarr.jsonl 12 65534 65534 666 1 f 0\nFREE 0\n"
	if free, sizes, _, ok := checkMap(audit, false, "", capMap, names, map[string]uint64{"sonarr.jsonl": 11, "radarr.jsonl": 12}); !ok || free != "0" || sizes["sonarr.jsonl"] != 100 {
		t.Fatalf("a correct audit was refused: %v %q %v", ok, free, sizes)
	}
	if _, _, _, ok := checkMap(audit, false, "", capMap, names, map[string]uint64{"sonarr.jsonl": 99, "radarr.jsonl": 12}); ok {
		t.Error("a changed inode was accepted by the audit")
	}
	big := mount + "F sonarr.jsonl 11 65534 65534 666 1 f 20971520\nF radarr.jsonl 12 65534 65534 666 1 f 20971520\nFREE 0\n"
	if _, _, _, ok := checkMap(big, false, "", capMap, names, map[string]uint64{"sonarr.jsonl": 11, "radarr.jsonl": 12}); ok {
		t.Error("sizes summing over the cap were accepted by the audit")
	}
}

func TestInputManifestLineRule(t *testing.T) {
	good := strings.Repeat("a", 64) + "  /src/x"
	for _, l := range []string{good, strings.Repeat("0", 64) + "  /src/a b/c.go"} {
		if !inputLineOK([]byte(l)) {
			t.Errorf("%q rejected", l)
		}
	}
	for _, l := range []string{"", good[:len(good)-1], strings.Repeat("A", 64) + "  /src/x", strings.Repeat("a", 64) + " /src/xx",
		strings.Repeat("a", 64) + "  /srcx/x", strings.Repeat("a", 63) + "  /src/xy", strings.Repeat("a", 64) + "  /src/"} {
		if inputLineOK([]byte(l)) {
			t.Errorf("%q accepted", l)
		}
	}
}

func TestAncestorRule(t *testing.T) {
	for _, tc := range []struct {
		uid, gid, mode string
		want           bool
	}{
		{"0", "0", "755", true}, {"0", "0", "1777", true}, {"0", "0", "3755", true}, {"0", "0", "0777", false},
		{"0", "0", "775", false}, {"0", "0", "757", false}, {"1000", "0", "755", false}, {"0", "0", "770", false},
	} {
		if got := ancestorOK(tc.uid, tc.gid, tc.mode); got != tc.want {
			t.Errorf("ancestorOK(%s, %s, %s) = %v, want %v", tc.uid, tc.gid, tc.mode, got, tc.want)
		}
	}
}

func TestClockReaderRejectsBackwardReadings(t *testing.T) {
	c, _ := testCtl(t)
	first, err := c.now()
	if err != nil || first <= 0 {
		t.Fatalf("the monotonic reader returned %d, %v", first, err)
	}
	c.lastClock = first + 3600*1000000000
	if v, err := c.now(); err == nil {
		t.Fatalf("a backward reading %d was accepted after %d", v, c.lastClock)
	}
}

// A backup name that already stands there is not this run's: the failed no-overwrite link records no ownership, so
// neither rollback nor cleanup may remove it, whether it holds foreign bytes or is a second link to the target.
func TestPublishNeverRemovesAPreExistingBackup(t *testing.T) {
	for _, shape := range []string{"foreign", "target-link"} {
		t.Run(shape, func(t *testing.T) {
			c, dir := publishFixture(t)
			c.staged = []string{"sonarr.jsonl"}
			target := filepath.Join(dir, "sonarr.jsonl")
			bak := filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")
			if err := os.WriteFile(target, []byte("prior sonarr"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
				t.Fatal(err)
			}
			wantBytes := "collision"
			if shape == "target-link" {
				wantBytes = "prior sonarr"
				if err := os.Link(target, bak); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(bak, []byte(wantBytes), 0o644); err != nil {
				t.Fatal(err)
			}
			t0, b0 := inode(t, target), inode(t, bak)
			if reason := c.publishInRoot(); reason != "write_publish_failed" {
				t.Fatalf("reason %q, want write_publish_failed for a backup name that already exists", reason)
			}
			c.stagingCleanup()
			if b, err := os.ReadFile(bak); err != nil || string(b) != wantBytes || inode(t, bak) != b0 {
				t.Fatalf("the pre-existing backup is %q (%v) on inode %d, want %q on inode %d", b, err, inode(t, bak), wantBytes, b0)
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != "prior sonarr" || inode(t, target) != t0 {
				t.Fatalf("the target is %q (%v), want its prior bytes on inode %d", b, err, t0)
			}
		})
	}
	// the control: with no collision the run's own backup is made, published over and removed after the commit point
	c, dir := publishFixture(t)
	c.staged = []string{"sonarr.jsonl"}
	if err := os.WriteFile(filepath.Join(dir, "sonarr.jsonl"), []byte("prior sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason := c.publishInRoot(); reason != "" {
		t.Fatalf("reason %q without a collision, want success", reason)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sonarr.jsonl")); string(b) != "new sonarr" || exists(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")) {
		t.Fatalf("the published target is %q or the run's own backup survived the commit point", b)
	}
}

// A cancellation latched while the backups are being prepared, before any rename, restores every prior state: the
// run's own backups are removed, nothing is published and no part remains.
func TestPublishCancelledDuringBackupPreparationRestoresPriorState(t *testing.T) {
	c, dir := publishFixture(t)
	prior := map[string]string{"sonarr.jsonl": "prior sonarr", "radarr.jsonl": "prior radarr"}
	inodes := map[string]uint64{}
	for n, body := range prior {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "."+n+"."+testRunID+".part"), []byte("new "+n), 0o644); err != nil {
			t.Fatal(err)
		}
		inodes[n] = inode(t, filepath.Join(dir, n))
	}
	c.latch()
	if c.publishWrite() {
		t.Fatal("a cancelled publication reported success")
	}
	for n, body := range prior {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err != nil || string(b) != body || inode(t, filepath.Join(dir, n)) != inodes[n] {
			t.Fatalf("%s is %q (%v), want its prior bytes on inode %d", n, b, err, inodes[n])
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("an owned %s was left behind", e.Name())
		}
	}
}

// An absent target with a backup name that already stands there: this run acquired no backup, so a cancellation
// latched at the backup checkpoint rolls back to the absent target and leaves the foreign file on its inode.
func TestPublishCancelledWithAbsentTargetKeepsAForeignBackup(t *testing.T) {
	c, dir := publishFixture(t)
	c.staged = []string{"sonarr.jsonl"}
	bak := filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")
	if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("foreign backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	b0 := inode(t, bak)
	c.latch()
	if reason := c.publishInRoot(); reason != reasonCancelled {
		t.Fatalf("reason %q, want %q", reason, reasonCancelled)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sonarr.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the absent target was not preserved: %v", err)
	}
	if b, err := os.ReadFile(bak); err != nil || string(b) != "foreign backup" || inode(t, bak) != b0 {
		t.Fatalf("the foreign backup is %q (%v) on inode %d, want its bytes on inode %d", b, err, inode(t, bak), b0)
	}
	if exists(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part")) {
		t.Fatal("the run's own part was left behind")
	}
}

// A backup this run acquired but whose name a foreign file has since replaced is not this run's to consume: the
// rollback restores nothing from it, removes nothing of it and never removes the new target for an unrestorable prior.
func TestPublishRollbackNeverConsumesAReplacedBackup(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		name := "before the rename"
		if renamed {
			name = "after the rename"
		}
		t.Run(name, func(t *testing.T) {
			c, dir := publishFixture(t)
			c.staged = []string{"sonarr.jsonl"}
			target := filepath.Join(dir, "sonarr.jsonl")
			bak := filepath.Join(dir, ".sonarr.jsonl."+testRunID+".prev")
			if err := os.WriteFile(target, []byte("prior sonarr"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".sonarr.jsonl."+testRunID+".part"), []byte("new sonarr"), 0o644); err != nil {
				t.Fatal(err)
			}
			errb := &syncBuf{}
			c.diag = newDiagSink(errb)
			t0 := inode(t, target)
			// the acquisition publishInRoot makes: the no-overwrite link to the identity just observed
			qid, _ := c.identOf("sonarr.jsonl")
			pid, _ := c.identOf(".sonarr.jsonl." + testRunID + ".part")
			if err := c.wroot.Link("sonarr.jsonl", ".sonarr.jsonl."+testRunID+".prev"); err != nil {
				t.Fatal(err)
			}
			c.partP["sonarr.jsonl"], c.partQ["sonarr.jsonl"] = pid, qid
			if renamed {
				if err := c.renameOne(".sonarr.jsonl."+testRunID+".part", "sonarr.jsonl"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".foreign"), []byte("foreign backup"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(dir, ".foreign"), bak); err != nil {
				t.Fatal(err)
			}
			b0, g0 := inode(t, bak), inode(t, target)
			c.rollback()
			if b, err := os.ReadFile(bak); err != nil || string(b) != "foreign backup" || inode(t, bak) != b0 {
				t.Fatalf("the replaced backup is %q (%v) on inode %d, want its bytes on inode %d", b, err, inode(t, bak), b0)
			}
			want, wantIno := "prior sonarr", t0
			if renamed {
				want, wantIno = "new sonarr", g0
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != want || inode(t, target) != wantIno {
				t.Fatalf("the target is %q (%v), want %q on inode %d", b, err, want, wantIno)
			}
			if renamed {
				if got := diag(c, errb); !strings.Contains(got, "dev-run: publish_rollback_conflict root=testdata/parser name=sonarr.jsonl\n") {
					t.Fatalf("no conflict line in %q", got)
				}
			}
		})
	}
}
