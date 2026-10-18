package main

// controller.go is the private native lifecycle controller of scripts/dev-run.sh:
//
//	redact -run REPO RUNID TMPD TIMEOUT WRITE CAPTURE -- COMMAND [ARG...]
//
// The bootstrap parses and preflights, builds this binary, then execs it with the
// runner's PID, environment, cwd, umask and stdio. The controller is the sole
// lifetime authority: it alone starts native processes, holds the issued names,
// the sticky create and removal failures and the cancellation state in memory,
// runs the checked closure, decides publication and exits. No file, child exit
// status or pipe content grants or revokes that authority, and no shell survives
// the exec. The redaction engine of main.go runs in process on one immutable
// secret selection for every stream.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	capOut   = 65536
	capPNG   = 2097152
	capMap   = 33554432
	capDraft = 65536

	childUser = "1000:1000"
	supUser   = "65534:65534"

	statusFail = 125

	writeTmpfsOpts = ":rw,nosuid,nodev,noexec,size=33554432,nr_inodes=8,mode=0755,uid=65534,gid=65534"
	captureTmpfs   = "/capture:rw,nosuid,nodev,noexec,size=8454144,nr_inodes=8,mode=0755,uid=65534,gid=65534"
	scratchTmpfs   = "/tmp:rw,nosuid,nodev,exec,size=2g,mode=1777"

	writeMapTokens   = "rw nosuid nodev noexec size=32768k nr_inodes=8 mode=755 uid=65534 gid=65534"
	captureMapTokens = "rw nosuid nodev noexec size=8256k nr_inodes=8 mode=755 uid=65534 gid=65534"

	envArgs = "-e PATH=/usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin -e HOME=/home/boxset -e TMPDIR=/tmp -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 -e GOPATH=/go -e GOCACHE=/var/cache/go-build -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-mod=readonly -e GOPROXY=off -e GOSUMDB=off -e GOTOOLCHAIN=local -e GOTELEMETRY=off -e GOENV=off -e GOLANGCI_LINT_CACHE=/tmp/golangci-lint-cache -e BUN_INSTALL_CACHE_DIR=/tmp/bun-install-cache -e BUN_RUNTIME_TRANSPILER_CACHE_PATH=/tmp/bun-transpiler-cache -e XDG_CACHE_HOME=/tmp/xdg-cache -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright -e PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 -e DO_NOT_TRACK=1"
)

var captureNames = []string{"dev-desktop", "dev-mobile", "preview-desktop", "preview-mobile"}

// In-container scripts, byte for byte as the host runner has always passed them.

// Read-only inspection as 0:0 (zero capabilities, no-new-privileges), before any
// untrusted child runs: nothing under /usr or /etc, nor / /bin /lib /lib64 /sbin
// or the input manifest, is owned by uid/gid 1000 or writable by group/other
// (symlinks are judged by their targets; unreadable or dangling entries reject).
const trustScript = `set -u
judge() {
	[ "$1" != 1000 ] && [ "$2" != 1000 ] || return 1
	m=$3
	o=${m#"${m%?}"}
	g=${m%?}
	g=${g#"${g%?}"}
	case $o in *[2367]) return 1 ;; esac
	case $g in *[2367]) return 1 ;; esac
	return 0
}
bad=$(/usr/bin/find /usr /etc ! -type l \( -uid 1000 -o -gid 1000 -o -perm /022 \) -print -quit) || exit 11
[ -z "$bad" ] || { printf "%s\n" "$bad"; exit 12; }
links=$(/usr/bin/find /usr /etc -type l -exec /usr/bin/stat -L -c "%u %g %a %n" {} +) || exit 13
extra=$(/usr/bin/stat -L -c "%u %g %a %n" / /bin /lib /lib64 /sbin /usr/local/share/boxset/inputs.sha256) || exit 14
while read -r u g a n; do
	[ -n "$u" ] || continue
	judge "$u" "$g" "$a" || { printf "%s\n" "$n"; exit 15; }
done <<EOF
$links
$extra
EOF
exit 0`

// Inspection of one protected tmpfs map as 65534: its mount line, every entry's
// name/inode/owner/mode/links/type/size, the free blocks and the ancestors.
const inspectScript = `r=$1
shift
m=$(/usr/bin/grep -F " $r tmpfs " /proc/self/mounts) || exit 21
printf "MOUNT %s\n" "$m"
f=$(/usr/bin/find "$r" -mindepth 1 -maxdepth 1 -printf "F %f %i %U %G %m %n %y %s\n") || exit 22
[ -z "$f" ] || printf "%s\n" "$f"
b=$(/usr/bin/stat -f -c %f "$r") || exit 23
printf "FREE %s\n" "$b"
if [ $# -gt 0 ]; then
	a=$(/usr/bin/stat -c "A %n %u %g %a" "$@") || exit 24
	printf "%s\n" "$a"
fi`

// Every task whose real or effective Uid is 1000 must show State: Z; anything
// live, unreadable or unparseable rejects. A task that vanished is gone.
const verifyScript = `bad=0
for p in /proc/[0-9]*; do
	for t in "$p"/task/[0-9]*; do
		n=0
		state=
		uids=
		while IFS= read -r line; do
			n=$((n + 1))
			case $line in
			State:*) set -- $line; state=$2 ;;
			Uid:*) set -- $line; uids="$2 $3" ;;
			esac
		done <"$t/status"
		if [ "$n" = 0 ]; then
			[ -e "$t" ] && bad=1
			continue
		fi
		if [ -z "$state" ] || [ -z "$uids" ]; then
			bad=1
			continue
		fi
		set -- $uids
		if [ "$1" = 1000 ] || [ "$2" = 1000 ]; then
			[ "$state" = Z ] || bad=1
		fi
	done
done
exit "$bad"`

// As 65534, empty regular 0666 single-link files.
const precreateScript = `r=$1; shift; for n in "$@"; do /usr/bin/install -m 0666 /dev/null "$r/$n" || exit 1; done`

var (
	errCancelled = errors.New("cancelled at a checkpoint")
	errBadClock  = errors.New("unusable clock reading")
)

// fileID is the device:inode identity of one filesystem object.
type fileID struct{ dev, ino uint64 }

func idOf(fi fs.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{uint64(st.Dev), st.Ino}, true
}

func ownerOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// rootID is the identity of the directory object a root pins.
func rootID(r *os.Root) (fileID, bool) {
	f, err := r.Open(".")
	if err != nil {
		return fileID{}, false
	}
	defer closeQuiet(f)
	fi, err := f.Stat()
	if err != nil {
		return fileID{}, false
	}
	return idOf(fi)
}

// closeQuiet closes a handle the controller never wrote through: a read end, a
// child-side pipe end, a read-only open or one that never started its writer.
// Its close error cannot change a result. A written handle's close error is
// instead part of the existing failure of its writer.
func closeQuiet(c io.Closer) { _ = c.Close() }

// clockGettime is the one CLOCK_MONOTONIC reader (clock id 1), through stdlib
// syscall/unsafe: no runtime linkname, wall-time fallback or epoch conversion.
func clockGettime() (sec, nsec int64, err error) {
	var ts syscall.Timespec
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0, 0, e
	}
	return int64(ts.Sec), int64(ts.Nsec), nil
}

// ctl is one run. Everything but the cancellation state belongs to the single
// lifecycle goroutine; stream workers report observations and never touch it.
type ctl struct {
	repo, runid, tmpd string
	timeout           int
	write, capture    string
	argv              []string

	env  []string
	ac   *automaton
	out  *outWriter
	diag *diagSink

	devnull *os.File

	ctr, img, inputs, challenge string
	mapNames, ancestors         []string
	writeComps                  []string
	slug, leaf                  string

	repoRoot       *os.Root
	wroot, croot   *os.Root
	wid, cid, tid  fileID
	tmpdOwned      bool
	captureClaimed bool
	captureOK      bool

	issued     []string
	createFail []string
	rmFail     []string
	first125   string
	status     int
	statusSet  bool
	closureRan bool
	ctrClosed  bool

	staged   []string
	sizes    map[string]int64
	wInodes  map[string]uint64
	cInodes  map[string]uint64
	partP    map[string]fileID
	partQ    map[string]fileID
	capTotal int64

	cancelled  atomic.Bool
	cancelOnce sync.Once
	cancelCh   chan struct{}
	intrOnce   sync.Once
	lastClock  int64
}

// outWriter is the guarded controller stdout: a sink that is blocked or has been
// abandoned can no longer delay anything but its own worker.
type outWriter struct {
	mu   sync.Mutex
	w    io.Writer
	dead atomic.Bool
}

func (o *outWriter) Write(p []byte) (int, error) {
	if o.dead.Load() {
		return 0, io.ErrClosedPipe
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w.Write(p)
}

// diagCap bounds the diagnostic bytes that may wait for a sink that is slow or blocked. A diagnostic that does not
// fit is dropped, never waited for: the failure itself lives in the controller (first125, the sticky lists and the
// status), so a stderr nobody reads can only lose text, never hold the lifetime owner.
const (
	diagCap   = 1 << 18
	diagFlush = time.Second
)

type diagItem struct {
	b    []byte
	done chan struct{} // a flush marker, closed when the worker reaches it
}

// diagSink delivers stderr bytes in order through one worker at a time, started on demand and gone when the queue
// is empty. The lifecycle owner only enqueues.
type diagSink struct {
	w       io.Writer
	mu      sync.Mutex
	q       []diagItem
	n       int
	running bool
}

func newDiagSink(w io.Writer) *diagSink { return &diagSink{w: w} }

func (d *diagSink) put(it diagItem) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if it.done == nil && d.n+len(it.b) > diagCap {
		return
	}
	d.q, d.n = append(d.q, it), d.n+len(it.b)
	if !d.running {
		d.running = true
		go d.run()
	}
}

func (d *diagSink) run() {
	for {
		d.mu.Lock()
		if len(d.q) == 0 {
			d.running = false
			d.mu.Unlock()
			return
		}
		it := d.q[0]
		d.q, d.n = d.q[1:], d.n-len(it.b)
		d.mu.Unlock()
		if it.done != nil {
			close(it.done)
		} else {
			// A diagnostic is best effort by design: a closed or failing stderr loses text only; the failure itself is held in the controller state and the exit status.
			_, _ = d.w.Write(it.b)
		}
	}
}

// flush waits for everything queued so far, but never longer than limit.
func (d *diagSink) flush(limit time.Duration) {
	done := make(chan struct{})
	d.put(diagItem{done: done})
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

func hexN(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func cleanAbs(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "//") || strings.HasSuffix(p, "/") && p != "/" {
		return false
	}
	for _, e := range strings.Split(p[1:], "/") {
		if e == "." || e == ".." {
			return false
		}
	}
	return true
}

// parseCapture is the lexical rule of --capture: .devrites/work/<slug>/screenshots-YYYYMMDDTHHMMSSZ.
func parseCapture(dir string) (slug, leaf string, ok bool) {
	rest, found := strings.CutPrefix(dir, ".devrites/work/")
	if !found {
		return "", "", false
	}
	slug, leaf, found = strings.Cut(rest, "/")
	if !found || strings.Contains(leaf, "/") || len(slug) < 1 || len(slug) > 64 {
		return "", "", false
	}
	for i := range len(slug) {
		if c := slug[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && (c != '-' || i <= 0) {
			return "", "", false
		}
	}
	stamp, found := strings.CutPrefix(leaf, "screenshots-")
	if !found || len(stamp) != 16 || stamp[8] != 'T' || stamp[15] != 'Z' {
		return "", "", false
	}
	for i := range len(stamp) {
		if i != 8 && i != 15 && (stamp[i] < '0' || stamp[i] > '9') {
			return "", "", false
		}
	}
	return slug, leaf, true
}

// newCtl is the run before its private argv is bound: just enough state for the
// signal handling that must be installed first.
func newCtl(stderr io.Writer) *ctl {
	return &ctl{
		diag: newDiagSink(stderr), cancelCh: make(chan struct{}),
		sizes: map[string]int64{}, partP: map[string]fileID{}, partQ: map[string]fileID{},
		wInodes: map[string]uint64{}, cInodes: map[string]uint64{},
	}
}

// bind validates the private argv and binds the run. A violation returns an
// error before any native start or host write; no argv-derived path is ever
// cleaned for a rejected argument.
func (c *ctl) bind(args, environ []string, stdout io.Writer) error {
	bad := errors.New("private argv rejected")
	if len(args) < 10 || args[1] != "-run" || args[8] != "--" {
		return bad
	}
	c.repo, c.runid, c.tmpd = args[2], args[3], args[4]
	c.env = append([]string(nil), environ...)
	c.argv = args[9:]
	c.out = &outWriter{w: stdout}
	if !cleanAbs(c.repo) || strings.ContainsAny(c.repo, ",:\" ") {
		return bad
	}
	rfi, err := os.Stat(c.repo)
	if err != nil || !rfi.IsDir() {
		return bad
	}
	if dot, err := os.Stat("."); err != nil || !os.SameFile(rfi, dot) {
		return bad
	}
	if !hexN(c.runid, 16) {
		return bad
	}
	c.ctr = "boxset-dev-" + c.runid
	if !cleanAbs(c.tmpd) || c.tmpd[strings.LastIndexByte(c.tmpd, '/')+1:] != "boxset-dev-run."+c.runid {
		return bad
	}
	tfi, err := os.Lstat(c.tmpd)
	if err != nil || tfi.Mode() != fs.ModeDir|0o700 || ownerOf(tfi) != os.Geteuid() {
		return bad
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return bad
	}
	bin, err := os.Lstat(c.tmpd + "/bin/redact")
	if err != nil || !bin.Mode().IsRegular() || !os.SameFile(self, bin) {
		return bad
	}
	var ok bool
	if c.tid, ok = idOf(tfi); !ok {
		return bad
	}
	t := args[5]
	if len(t) < 1 || len(t) > 3 || t[0] == '0' {
		return bad
	}
	for i := range len(t) {
		if t[i] < '0' || t[i] > '9' {
			return bad
		}
	}
	if c.timeout, err = strconv.Atoi(t); err != nil || c.timeout < 1 || c.timeout > 600 {
		return bad
	}
	switch args[6] {
	case "-":
	case "testdata/parser":
		c.write, c.mapNames = args[6], []string{"series.jsonl", "movie.jsonl"}
	case "docs/spikes/issue-drafts":
		c.write, c.mapNames = args[6], []string{"s5-admission.md", "s5-tools.md"}
	default:
		return bad
	}
	if c.write != "" {
		c.writeComps = strings.Split(c.write, "/")
		c.ancestors = []string{"/"}
		p := "/src"
		for _, comp := range c.writeComps {
			c.ancestors = append(c.ancestors, p)
			p += "/" + comp
		}
	}
	if args[7] != "-" {
		c.capture = args[7]
		if c.slug, c.leaf, ok = parseCapture(c.capture); !ok {
			return bad
		}
	}
	// Run-private cleanup authority exists only now, after the whole binding validated.
	c.tmpdOwned = true
	return nil
}

// runController is the process entry of -run.
func runController(args, environ []string) int {
	return controllerMain(args, environ, os.Stdout, os.Stderr)
}

// controllerMain: signals first, then the private argv, then the secret
// selection and the clock, then the lifecycle.
func controllerMain(args, environ []string, stdout, stderr io.Writer) int {
	c := newCtl(stderr)
	defer c.flushDiag()
	stop := c.installSignals()
	defer stop()
	if err := c.bind(args, environ, stdout); err != nil {
		c.say("redact_unavailable")
		return statusFail
	}
	values, ok := selectValues(environ)
	if !ok {
		c.say("redact_unavailable")
		return c.discard(statusFail)
	}
	c.ac = newAutomaton(values)
	if _, err := c.now(); err != nil {
		c.say("redact_unavailable")
		return c.discard(statusFail)
	}
	var err error
	if c.repoRoot, err = os.OpenRoot(c.repo); err != nil {
		c.say("redact_unavailable")
		return c.discard(statusFail)
	}
	if c.devnull, err = os.OpenFile(os.DevNull, os.O_RDWR, 0); err != nil {
		c.say("redact_unavailable")
		return c.discard(statusFail)
	}
	return c.lifecycle()
}

// discard ends a run that never started a native effect: only the run-private
// directory the bootstrap created is removed.
func (c *ctl) discard(code int) int {
	c.removeTmpd()
	return code
}

func (c *ctl) say(msg string) {
	c.diag.put(diagItem{b: []byte("dev-run: " + msg + "\n")})
}

// flushDiag delivers the queued diagnostics before the process exits, for at most diagFlush.
func (c *ctl) flushDiag() { c.diag.flush(diagFlush) }

// bad records a 125-class failure without leaving the run.
func (c *ctl) bad(msg string) {
	c.say(msg)
	if c.first125 == "" {
		c.first125 = msg
	}
}

func (c *ctl) die(word string, code int) {
	c.say(word)
	c.setStatus(code)
}

func (c *ctl) setStatus(code int) {
	if !c.statusSet {
		c.status, c.statusSet = code, true
	}
}

// installSignals notifies TERM and SIGPIPE, and HUP and INT unless they were
// ignored at entry (an ignored one stays ignored). A handled signal only latches
// the cancellation and acknowledges it once; it never exits the process.
func (c *ctl) installSignals() (stop func()) {
	ch := make(chan os.Signal, 8)
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGPIPE}
	for _, s := range []os.Signal{syscall.SIGHUP, syscall.SIGINT} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				if s != syscall.SIGPIPE {
					c.interrupt()
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func (c *ctl) interrupt() {
	c.latch()
	c.intrOnce.Do(func() { c.say("interrupted") })
}

func (c *ctl) latch() {
	c.cancelled.Store(true)
	c.cancelOnce.Do(func() { close(c.cancelCh) })
}

// pause sleeps unless the run is cancelled meanwhile.
func (c *ctl) pause(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.cancelCh:
	}
}

// clockNS is the closure's raw CLOCK_MONOTONIC reading in exact integer
// nanoseconds, before the -clock formatter floors anything.
func (c *ctl) clockNS() (int64, error) {
	sec, nsec, err := clockGettime()
	if err != nil {
		return 0, err
	}
	if sec < 0 || sec > 9000000000 || nsec < 0 || nsec >= 1000000000 {
		return 0, errBadClock
	}
	return sec*1000000000 + nsec, nil
}

// now is the controller's one clock function: a failed, invalid, negative or
// backward reading is an error, never a time.
func (c *ctl) now() (int64, error) {
	v, err := c.clockNS()
	if err != nil {
		return 0, err
	}
	if v < 0 || v < c.lastClock {
		return 0, errBadClock
	}
	c.lastClock = v
	return v, nil
}

// ---- native calls ------------------------------------------------------------

type callKind int

const (
	kOther     callKind = iota // SIGKILLed with its process group when the run is cancelled
	kCreate                    // a `docker run`: waited to its own native bound
	kReconcile                 // exact-name queries and owned removals: never refused, never interrupted
)

// streamFn consumes one pipe to its end (or its error) and reports a bounded
// observation; it never issues a Docker effect and never touches ISSUED or RMFAIL.
type streamFn func(io.Reader) any

type callSpec struct {
	kind   callKind
	argv   []string
	stdin  *os.File // nil: /dev/null
	stdout streamFn // nil: /dev/null
	stderr streamFn // nil: errOut or /dev/null
	merge  bool     // stderr shares stdout's pipe
	errOut *os.File
	bound  time.Duration // hard bound from the start: the wrapper's own deadline, no grace
	issue  string        // the owned name issued if, and only if, this process starts
	// removeOnOut: when the stdout worker reports a nonzero stop, this owned name is removed by a reconciliation call
	// that wait itself starts and supervises beside the parent, so the parent's deadline and cancellation stay
	// serviced while the removal runs.
	removeOnOut string
}

type callResult struct {
	status   int // exit code, 128+signal, or -1 when unknown
	out, err any
	outOK    bool // the stdout worker reached its end inside the bound
	errOK    bool
	startErr error
}

type workRes struct {
	idx int
	v   any
}

type proc struct {
	c        *ctl
	s        *callSpec
	cmd      *exec.Cmd
	pgid     int
	readers  [2]*os.File
	nwork    int
	waitCh   chan int
	workCh   chan workRes
	deadline time.Time
}

func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

// procStatus is the process status: the exit code, 128 plus the signal number,
// or -1 (unknown) when the wait failed.
func procStatus(ps *os.ProcessState, err error) int {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return -1
	}
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		if ws.Exited() {
			return ws.ExitStatus()
		}
	}
	return -1
}

// command resolves argv[0] through the inherited PATH exactly as the shell did: a
// relative entry resolves against the original working directory, so an
// exec.ErrDot result is a found executable, never a missing tool. No GODEBUG
// override is involved.
//
// argv[0] is never remote or user data. Every callSpec.argv of this file is built
// here from fixed literals: each Docker call is `timeout ... docker ...` (the sup,
// queryArgv, rmArgv, runArgv, validatorArgv, capCatArgv and exportArgv builders
// and the literal slices of buildImage, supervisorPreflight, childStatus,
// runChild, quiesce and finish), so argv[0] is always the constant "timeout" and
// no shell syntax is ever composed. The only caller-chosen words are the user
// COMMAND of the private argv (bind: c.argv), which runChild appends after
// `docker exec ... CONTAINER /usr/bin/timeout -- N`, i.e. as arguments executed
// inside the container as 1000:1000, never as a host executable. The remaining
// interpolated words are validated by bind before any start: REPO is a clean
// absolute path without separators or quotes that is the working directory, RUNID
// is 16 lowercase hex digits, TMPD the exact run-private directory, TIMEOUT
// 1-600 digits, WRITE and CAPTURE fixed lexical forms.
func (c *ctl) command(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	cmd.Env = c.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// start is the one start primitive of every native process: the checkpoint
// (unless the call is reconciliation), an os/exec start in its own process group
// with the controller's environment and per-call stdio, the issuance of its owned
// name immediately after a successful start, and the closing of the controller's
// copies of the child-side pipe ends. A start error issues nothing.
func (c *ctl) start(s *callSpec) (*proc, error) {
	if s.kind != kReconcile && c.cancelled.Load() {
		return nil, errCancelled
	}
	cmd := c.command(s.argv)
	cmd.Stdin = c.devnull
	if s.stdin != nil {
		cmd.Stdin = s.stdin
	}
	if s.bound <= 0 {
		s.bound = time.Hour
	}
	p := &proc{c: c, s: s, cmd: cmd, waitCh: make(chan int, 1), workCh: make(chan workRes, 2)}
	var childEnds []*os.File
	closeAll := func() {
		for _, f := range childEnds {
			closeQuiet(f)
		}
		for _, r := range p.readers {
			if r != nil {
				closeQuiet(r)
			}
		}
	}
	cmd.Stdout = c.devnull
	if s.stdout != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		p.readers[0] = r
		childEnds = append(childEnds, w)
		cmd.Stdout = w
		if s.merge {
			cmd.Stderr = w
		}
	}
	switch {
	case s.merge && s.stdout != nil:
	case s.stderr != nil:
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		p.readers[1] = r
		childEnds = append(childEnds, w)
		cmd.Stderr = w
	case s.errOut != nil:
		cmd.Stderr = s.errOut
	default:
		cmd.Stderr = c.devnull
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	if s.issue != "" {
		c.issued = append(c.issued, s.issue)
	}
	for _, f := range childEnds {
		closeQuiet(f)
	}
	p.pgid = cmd.Process.Pid
	p.deadline = time.Now().Add(s.bound)
	for _, r := range p.readers {
		if r != nil {
			_ = r.SetReadDeadline(p.deadline)
		}
	}
	go func() {
		err := cmd.Wait()
		killGroup(p.pgid)
		p.waitCh <- procStatus(cmd.ProcessState, err)
	}()
	for i, fn := range [2]streamFn{s.stdout, s.stderr} {
		if fn == nil {
			continue
		}
		p.nwork++
		r := p.readers[i]
		go func() { p.workCh <- workRes{i, fn(r)} }()
	}
	return p, nil
}

func (p *proc) abandon() {
	for _, r := range p.readers {
		if r != nil {
			closeQuiet(r)
		}
	}
}

// wait returns when the process has been waited and every stream worker has
// reached its end, or at the call's hard bound: then the group is killed, the
// pipe ends are closed and the affected stream is reported incomplete rather than
// waited for. Cancellation SIGKILLs the group of a kOther call only.
func (p *proc) wait() callResult {
	c, s := p.c, p.s
	res := callResult{status: -1, outOK: s.stdout == nil, errOK: s.stderr == nil}
	timer := time.NewTimer(time.Until(p.deadline))
	defer timer.Stop()
	var cancelCh <-chan struct{}
	if s.kind == kOther {
		cancelCh = c.cancelCh
	}
	procDone, cancelKilled, abandoned := false, false, false
	pending := p.nwork
	// rm is the owned removal started because the output failed. It runs in this same select, and the call returns
	// only after it has ended or been killed at its own bound.
	var rm *proc
	var rmTimer *time.Timer
	var rmCh <-chan int
	var rmDue <-chan time.Time
	defer func() {
		if rmTimer != nil {
			rmTimer.Stop()
		}
	}()
	for !procDone || (pending > 0 && !abandoned) || rm != nil {
		select {
		case st := <-p.waitCh:
			procDone, res.status = true, st
			if cancelKilled {
				p.abandon()
				abandoned = true
			}
		case w := <-p.workCh:
			pending--
			if w.idx == 0 {
				res.out, res.outOK = w.v, true
			} else {
				res.err, res.errOK = w.v, true
			}
			closeQuiet(p.readers[w.idx])
			if st, _ := w.v.(int); s.removeOnOut != "" && w.idx == 0 && st != 0 && rm == nil {
				if rm = c.startRm(s.removeOnOut); rm != nil {
					rmCh, rmTimer = rm.waitCh, time.NewTimer(time.Until(rm.deadline))
					rmDue = rmTimer.C
				}
			}
		case st := <-rmCh:
			c.rmNote(s.removeOnOut, st == 0)
			rm, rmCh, rmDue = nil, nil, nil
		case <-rmDue:
			killGroup(rm.pgid)
			rmDue = nil
		case <-cancelCh:
			cancelCh = nil
			cancelKilled = true
			killGroup(p.pgid)
		case <-timer.C:
			killGroup(p.pgid)
			p.abandon()
			abandoned = true
		}
	}
	return res
}

// do starts one call and waits for it.
func (c *ctl) do(s *callSpec) callResult {
	p, err := c.start(s)
	if err != nil {
		return callResult{status: -1, startErr: err}
	}
	return p.wait()
}

// capRes is one capture: the bytes up to the limit, whether more followed, and the
// read error that ended it early (a bound that expired, for one).
type capRes struct {
	data []byte
	over bool
	err  error
}

func captureFn(limit int) streamFn {
	return func(r io.Reader) any {
		var b bytes.Buffer
		n, err := io.Copy(&b, io.LimitReader(r, int64(limit)+1))
		cr := capRes{err: err}
		if n > int64(limit) {
			cr.over = true
			b.Truncate(limit)
		}
		cr.data = b.Bytes()
		return cr
	}
}

// text is the captured stdout of a call that succeeded, completed and stayed inside its cap.
func (r callResult) text() (string, bool) {
	cr, ok := r.out.(capRes)
	if !ok || r.startErr != nil || r.status != 0 || !r.outOK || cr.over || cr.err != nil {
		return "", false
	}
	return string(cr.data), true
}

type tailRes struct {
	out    []byte
	status int
}

// tailFn redacts a stream in process into its last n bytes.
func (c *ctl) tailFn(n int64) streamFn {
	return func(r io.Reader) any {
		var b bytes.Buffer
		st := redactStream(c.ac, streamOpts{capN: -1, raw: -1, tail: n}, r, &b)
		return tailRes{b.Bytes(), st}
	}
}

func (r callResult) tail() ([]byte, bool) {
	t, ok := r.out.(tailRes)
	if !ok {
		t, ok = r.err.(tailRes)
	}
	if !ok {
		return nil, false
	}
	return t.out, t.status == 0
}

// ---- Docker argv (every byte as the shell runner has always sent it) ----------

// sup is a supervisor exec as 65534 on a trusted absolute binary.
func (c *ctl) sup(secs int, args ...string) []string {
	a := []string{"timeout", "--kill-after=2s", strconv.Itoa(secs) + "s", "docker", "exec", "-u", supUser, c.ctr}
	return append(a, args...)
}

func supBound(secs int) time.Duration { return time.Duration(secs+2) * time.Second }

// supCall is one captured supervisor exec, its output read with a 65,536-byte cap.
func (c *ctl) supCall(secs int, args ...string) callResult {
	return c.do(&callSpec{kind: kOther, argv: c.sup(secs, args...), stdout: captureFn(capOut), bound: supBound(secs)})
}

// queryArgv is one exact-name query; phase is first or confirm.
func (c *ctl) queryArgv(secs, name, phase string) []string {
	return []string{"timeout", secs, "docker", "ps", "-a", "--filter", "name=^" + name + "$", "--format", "{{.Names}}"}
}

func (c *ctl) rmArgv(name string) []string {
	return []string{"timeout", "30", "docker", "rm", "-f", name}
}

// queryName is one status-checked exact-name query: the name when present, empty
// when absent; a failed, timed-out or oversized (over 4,096 bytes) query is not ok.
func (c *ctl) queryName(secs string, bound time.Duration, name, phase string) (string, bool) {
	r := c.do(&callSpec{kind: kReconcile, argv: c.queryArgv(secs, name, phase), stdout: captureFn(4096), bound: bound})
	out, ok := r.text()
	if !ok {
		return "", false
	}
	return strings.TrimRight(out, "\n"), true
}

func (c *ctl) rmSpec(name string) *callSpec {
	return &callSpec{kind: kReconcile, argv: c.rmArgv(name), bound: 30 * time.Second}
}

// rmNote retains a failed owned removal: a nonzero status is a sticky failure even when the name is later absent.
func (c *ctl) rmNote(name string, ok bool) bool {
	if !ok {
		c.rmFail = append(c.rmFail, name)
	}
	return ok
}

// startRm starts one owned removal without waiting for it; a start error is the removal's failure.
func (c *ctl) startRm(name string) *proc {
	p, err := c.start(c.rmSpec(name))
	if err != nil {
		c.rmNote(name, false)
		return nil
	}
	return p
}

// rmOwned is one owned removal, waited here.
func (c *ctl) rmOwned(name string) bool {
	r := c.do(c.rmSpec(name))
	return c.rmNote(name, r.startErr == nil && r.status == 0)
}

// failCreate retains one failed, timed-out or unknown create of an issued name.
func (c *ctl) failCreate(name string) {
	c.createFail = append(c.createFail, name)
}

// ---- lifecycle ---------------------------------------------------------------

// reasonCancelled is the internal result of a step that stopped at a cancelled
// checkpoint: the interruption was already announced, so it names no reason word.
const reasonCancelled = "interrupted"

func (c *ctl) lifecycle() int {
	c.stages()
	return c.finish()
}

// stepDie ends the run with a step's fixed reason, unless a cancellation already
// explains the stopped step.
func (c *ctl) stepDie(word string) bool {
	if !c.cancelled.Load() {
		c.die(word, statusFail)
	}
	return false
}

func pngNames() []string {
	names := make([]string, 0, len(captureNames))
	for _, n := range captureNames {
		names = append(names, n+".png")
	}
	return names
}

func (c *ctl) stages() {
	if !c.acquireWriteRoot() || !c.claimCapture() {
		return
	}
	if !c.buildImage() || !c.startContainer() {
		return
	}
	if c.write != "" && !c.precreate("/src/"+c.write, c.mapNames) {
		c.stepDie("write_root_invalid")
		return
	}
	if c.capture != "" && !c.precreate("/capture", pngNames()) {
		c.stepDie("capture_capability_missing")
		return
	}
	if !c.supervisorPreflight() {
		return
	}
	if c.write != "" && !c.writePreflight() {
		c.stepDie("write_root_invalid")
		return
	}
	if c.capture != "" && !c.capturePreflight() {
		c.stepDie("capture_capability_missing")
		return
	}
	if !c.inputHash() {
		c.stepDie("inputs_unavailable")
		return
	}
	cs, rs, tmo, ok := c.runChild()
	if !ok {
		return
	}
	switch {
	case tmo:
		c.closure()
		c.die("timeout", 124)
		return
	case rs != 0:
		c.closure()
		if rs == exitCap {
			c.die("output_overflow", statusFail)
		} else {
			c.die("output_write_failed", statusFail)
		}
		return
	case cs < 0:
		c.closure()
		c.die("child_status_unknown", statusFail)
		return
	}

	stageOK := true
	writeStaged, captureStaged := false, false
	if !c.quiesce() {
		if !c.cancelled.Load() {
			c.bad("quiesce_failed")
		}
		stageOK = false
	}
	// quiescence is established; no map has been audited yet
	if stageOK && c.write != "" {
		switch c.stageWrite(cs) {
		case 0:
			writeStaged = true
		case 1:
			stageOK = false
		}
	}
	if stageOK && c.capture != "" && cs == 0 && !c.captureAudit() {
		if !c.cancelled.Load() {
			c.bad("capture_map_changed")
		}
		stageOK = false
	}
	if stageOK && c.capture != "" && cs == 0 {
		if reason := c.captureStage(); reason == "" {
			captureStaged = true
		} else {
			if reason != reasonCancelled {
				c.bad(reason)
			}
			stageOK = false
		}
	}
	closedOK := c.closure()

	// The publication checkpoint: a cancellation latched here publishes nothing.
	if stageOK && closedOK && !c.cancelled.Load() {
		if writeStaged && !c.publishWrite() {
			stageOK = false
		}
		if stageOK && captureStaged {
			if reason := c.publishCapture(); reason == "" {
				c.captureOK = true
			} else if reason != reasonCancelled {
				c.bad(reason)
			}
		}
	} else if c.write != "" {
		c.stagingCleanup()
	}

	switch {
	case c.cancelled.Load():
		c.setStatus(statusFail)
	case cs != 0:
		c.setStatus(cs)
	case c.first125 != "":
		c.setStatus(statusFail)
	default:
		if c.capture != "" && c.captureOK && !c.announce(fmt.Sprintf("DEV-RUN-CAPTURE-PASS files=4 bytes=%d dir=%s challenge=%s\n", c.capTotal, c.capture, c.challenge)) {
			// the capture stays published: nothing after the commit point rolls back
			c.die("output_write_failed", statusFail)
		} else {
			c.setStatus(0)
		}
	}
}

// announceBound is the one bound of the final locator line on a stdout that is blocked or gone.
const announceBound = 5 * time.Second

// announce writes the final locator line through the guarded stdout in a worker the lifecycle owner waits for at
// most announceBound: a failed or blocked delivery is reported, and the sink is abandoned.
func (c *ctl) announce(line string) bool {
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c.out, line)
		done <- err
	}()
	t := time.NewTimer(announceBound)
	defer t.Stop()
	select {
	case err := <-done:
		return err == nil
	case <-t.C:
		c.out.dead.Store(true)
		return false
	}
}

// chainOK: every component of the relative chain is a real directory, not a
// symlink, by Lstat through REPO.
func (c *ctl) chainOK(comps ...string) bool {
	cur := ""
	for _, comp := range comps {
		if cur != "" {
			cur += "/"
		}
		cur += comp
		fi, err := c.repoRoot.Lstat(cur)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return false
		}
	}
	return true
}

// writeRootCheck: the lexical chain is real directories (write_root_symlink) and
// its leaf is the object pinned at acquisition (write_root_invalid).
func (c *ctl) writeRootCheck() string {
	if !c.chainOK(c.writeComps...) {
		return "write_root_symlink"
	}
	fi, err := c.repoRoot.Lstat(c.write)
	if err != nil {
		return "write_root_invalid"
	}
	if id, ok := idOf(fi); !ok || id != c.wid {
		return "write_root_invalid"
	}
	return ""
}

func (c *ctl) rootUnchanged() bool {
	return c.write == "" || c.writeRootCheck() == ""
}

// acquireWriteRoot: every lexical component is Lstat'ed through REPO (a symlink is
// write_root_symlink, a non-directory write_root_invalid), a missing one is
// created 0755 and must then be a real directory; the leaf is pinned as an
// os.Root and its identity checked.
func (c *ctl) acquireWriteRoot() bool {
	if c.write == "" {
		return true
	}
	cur := ""
	for _, comp := range c.writeComps {
		if cur != "" {
			cur += "/"
		}
		cur += comp
		if fi, err := c.repoRoot.Lstat(cur); err == nil {
			if fi.Mode()&fs.ModeSymlink != 0 {
				c.die("write_root_symlink", statusFail)
				return false
			}
			if !fi.IsDir() {
				c.die("write_root_invalid", statusFail)
				return false
			}
			continue
		}
		if err := c.repoRoot.Mkdir(cur, 0o755); err != nil {
			c.die("write_root_create_failed", statusFail)
			return false
		}
		if fi, err := c.repoRoot.Lstat(cur); err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			c.die("write_root_symlink", statusFail)
			return false
		}
	}
	wroot, err := c.repoRoot.OpenRoot(c.write)
	if err != nil {
		reason := "write_root_invalid"
		if !c.chainOK(c.writeComps...) {
			reason = "write_root_symlink"
		}
		c.die(reason, statusFail)
		return false
	}
	c.wroot = wroot
	id, ok := rootID(wroot)
	if !ok {
		c.die("write_root_invalid", statusFail)
		return false
	}
	c.wid = id
	if r := c.writeRootCheck(); r != "" {
		c.die(r, statusFail)
		return false
	}
	return true
}

// claimCapture: DIR is claimed with a non-recursive Mkdir 0700 and pinned as an
// os.Root; the challenge is 64 fresh bits from /dev/urandom.
func (c *ctl) claimCapture() bool {
	if c.capture == "" {
		return true
	}
	if err := c.repoRoot.Mkdir(c.capture, 0o700); err != nil {
		c.die("capture_destination_exists", statusFail)
		return false
	}
	c.captureClaimed = true
	croot, err := c.repoRoot.OpenRoot(c.capture)
	if err != nil {
		c.die("capture_root_changed", statusFail)
		return false
	}
	c.croot = croot
	id, ok := rootID(croot)
	if !ok {
		c.die("capture_root_changed", statusFail)
		return false
	}
	c.cid = id
	var b [8]byte
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, err = io.ReadFull(f, b[:])
		closeQuiet(f)
	}
	if err != nil {
		c.die("capture_host_tool_missing", statusFail)
		return false
	}
	c.challenge = hex.EncodeToString(b[:])
	return true
}

// buildImage builds the work image: stdout and stderr go through the in-process
// tail, a failure prints it, and the image id must be sha256: plus 64 characters.
func (c *ctl) buildImage() bool {
	argv := []string{"timeout", "--kill-after=10s", "1800s", "docker", "build", "--file", "deploy/dev/Dockerfile", "--target", "work",
		"--iidfile", c.tmpd + "/iid", "--progress", "plain", "."}
	r := c.do(&callSpec{kind: kOther, argv: argv, stdout: c.tailFn(capOut), merge: true, bound: 1810 * time.Second})
	if errors.Is(r.startErr, errCancelled) || c.cancelled.Load() {
		return false
	}
	tail, tok := r.tail()
	if r.startErr != nil || r.status != 0 || !r.outOK || !tok {
		c.raw(tail)
		return c.stepDie("image_build_failed")
	}
	f, err := os.Open(c.tmpd + "/iid")
	if err != nil {
		return c.stepDie("image_build_failed")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	closeQuiet(f)
	img := strings.TrimRight(string(data), "\n")
	if err != nil || len(img) != 71 || !strings.HasPrefix(img, "sha256:") {
		return c.stepDie("image_build_failed")
	}
	c.img = img
	return true
}

func (c *ctl) raw(b []byte) {
	c.diag.put(diagItem{b: bytes.Clone(b)})
}

func (c *ctl) runArgv(args []string) []string {
	return append([]string{"timeout", "--kill-after=10s", "60s", "docker"}, args...)
}

// startContainer starts the owned labelled container. Its name is issued if and
// only if the native process of its `docker run` started; a start error issues
// nothing, a failed or unknown create is retained.
func (c *ctl) startContainer() bool {
	args := []string{"run", "-d", "--rm", "--name", c.ctr, "--label", "boxset.dev-run=" + c.runid, "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", supUser,
		"--tmpfs", scratchTmpfs}
	if c.capture != "" {
		args = append(args, "--tmpfs", captureTmpfs)
	}
	if c.write != "" {
		args = append(args, "--tmpfs", "/src/"+c.write+writeTmpfsOpts)
	}
	args = append(args, c.img, "/usr/bin/sleep", strconv.Itoa(c.timeout+60))
	r := c.do(&callSpec{
		kind:   kCreate,
		argv:   c.runArgv(args),
		stderr: c.tailFn(4096),
		bound:  70 * time.Second,
		issue:  c.ctr,
	})
	if r.startErr != nil {
		if errors.Is(r.startErr, errCancelled) {
			return false
		}
		return c.stepDie("container_start_failed")
	}
	tail, tok := r.tail()
	if r.status != 0 || !r.errOK || !tok {
		c.raw(tail)
		c.failCreate(c.ctr)
		c.closure()
		return c.stepDie("container_start_failed")
	}
	return true
}

// precreate makes the map files as 65534: empty regular 0666 single-link files.
func (c *ctl) precreate(root string, names []string) bool {
	args := append([]string{"/usr/bin/sh", "-c", precreateScript, "sh", root}, names...)
	r := c.supCall(10, args...)
	return r.startErr == nil && r.status == 0 && r.outOK
}

func (c *ctl) inspectMap(root string, ancestors []string) (string, bool) {
	args := append([]string{"/usr/bin/sh", "-c", inspectScript, "sh", root}, ancestors...)
	return c.supCall(10, args...).text()
}

func (c *ctl) childStatus() (string, bool) {
	r := c.do(&callSpec{kind: kOther, argv: []string{"timeout", "10", "docker", "exec", "-u", childUser, c.ctr, "/usr/bin/cat", "/proc/self/status"},
		stdout: captureFn(capOut), bound: 10 * time.Second})
	return r.text()
}

func (c *ctl) pid1Status() (string, bool) {
	return c.supCall(10, "/usr/bin/cat", "/proc/1/status").text()
}

// statusField is the rest of the first /proc status line "KEY:".
func statusField(text, key string) ([]string, bool) {
	for _, l := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(l, key+":"); ok {
			return strings.Fields(rest), true
		}
	}
	return nil, false
}

// procOK: all four Uid and Gid fields are id, a Groups line that is present and
// either empty or exactly the primary GID id, every capability set zero and
// NoNewPrivs 1.
func procOK(text, id string) bool {
	for _, k := range []string{"Uid", "Gid"} {
		f, ok := statusField(text, k)
		if !ok || len(f) != 4 || f[0] != id || f[1] != id || f[2] != id || f[3] != id {
			return false
		}
	}
	g, ok := statusField(text, "Groups")
	if !ok || (len(g) != 0 && (len(g) != 1 || g[0] != id)) {
		return false
	}
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		f, ok := statusField(text, k)
		if !ok || len(f) != 1 || f[0] != "0000000000000000" {
			return false
		}
	}
	f, ok := statusField(text, "NoNewPrivs")
	return ok && len(f) == 1 && f[0] == "1"
}

func (c *ctl) supervisorPreflight() bool {
	t, ok := c.childStatus()
	if !ok || !procOK(t, "1000") {
		return c.stepDie("supervisor_untrusted")
	}
	t, ok = c.pid1Status()
	if !ok || !procOK(t, "65534") {
		return c.stepDie("supervisor_untrusted")
	}
	r := c.do(&callSpec{kind: kOther,
		argv:   []string{"timeout", "--kill-after=2s", "10s", "docker", "exec", "-u", "0:0", c.ctr, "/usr/bin/sh", "-c", trustScript},
		stdout: c.tailFn(1024), merge: true, bound: 12 * time.Second})
	tail, tok := r.tail()
	if r.startErr != nil || r.status != 0 || !r.outOK || !tok {
		if !c.cancelled.Load() {
			c.raw(tail)
		}
		return c.stepDie("supervisor_untrusted")
	}
	return true
}

func mountOK(f []string, tokens string) bool {
	if len(f) != 7 || f[0] != "MOUNT" || f[3] != "tmpfs" {
		return false
	}
	opts := "," + f[4] + ","
	for _, tok := range strings.Fields(tokens) {
		if !strings.Contains(opts, ","+tok+",") {
			return false
		}
	}
	return true
}

// ancestorOK: root-owned and sticky or not group/other writable.
func ancestorOK(uid, gid, mode string) bool {
	if uid != "0" {
		return false
	}
	if len(mode) == 4 && strings.ContainsRune("1357", rune(mode[0])) {
		return true
	}
	for _, off := range []int{1, 2} {
		if len(mode) >= off && strings.ContainsRune("2367", rune(mode[len(mode)-off])) {
			return false
		}
	}
	return true
}

// checkMap judges one inspection of a protected map: the mount (its options, for
// a preflight), every entry's name, owner, mode, link count, type and size, the
// free blocks and, for a preflight, the ancestors. A preflight records the
// inodes; an audit requires the recorded ones.
func checkMap(text string, pre bool, tokens string, capNow int64, names []string, want map[string]uint64) (free string, sizes map[string]int64, inodes map[string]uint64, ok bool) {
	sizes, inodes = map[string]int64{}, map[string]uint64{}
	mounts := 0
	var sum int64
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			return "", nil, nil, false
		}
		switch f[0] {
		case "MOUNT":
			mounts++
			if mounts != 1 || (pre && !mountOK(f, tokens)) {
				return "", nil, nil, false
			}
		case "F":
			if len(f) != 9 {
				return "", nil, nil, false
			}
			name := f[1]
			if _, dup := sizes[name]; dup || !slices.Contains(names, name) {
				return "", nil, nil, false
			}
			ino, e1 := strconv.ParseUint(f[2], 10, 64)
			size, e2 := strconv.ParseUint(f[8], 10, 63)
			if e1 != nil || e2 != nil {
				return "", nil, nil, false
			}
			if f[3] != "65534" || f[4] != "65534" || f[5] != "666" || f[6] != "1" || f[7] != "f" || size > uint64(capNow) {
				return "", nil, nil, false
			}
			sum += int64(size)
			if pre {
				if size != 0 {
					return "", nil, nil, false
				}
				inodes[name] = ino
			} else if w, found := want[name]; !found || w != ino {
				return "", nil, nil, false
			}
			sizes[name] = int64(size)
		case "FREE":
			if len(f) < 2 {
				return "", nil, nil, false
			}
			free = f[1]
		case "A":
			if !pre {
				continue
			}
			if len(f) != 5 || !ancestorOK(f[2], f[3], f[4]) {
				return "", nil, nil, false
			}
		default:
			return "", nil, nil, false
		}
	}
	if mounts != 1 || len(sizes) != len(names) || sum > capNow || free == "" {
		return "", nil, nil, false
	}
	for i := range len(free) {
		if free[i] < '0' || free[i] > '9' {
			return "", nil, nil, false
		}
	}
	return free, sizes, inodes, true
}

func (c *ctl) writePreflight() bool {
	t, ok := c.inspectMap("/src/"+c.write, c.ancestors)
	if !ok {
		return false
	}
	_, _, inodes, ok := checkMap(t, true, writeMapTokens, capMap, c.mapNames, nil)
	if !ok {
		return false
	}
	c.wInodes = inodes
	return true
}

func (c *ctl) capturePreflight() bool {
	t, ok := c.inspectMap("/capture", nil)
	if !ok {
		return false
	}
	_, _, inodes, ok := checkMap(t, true, captureMapTokens, capPNG, pngNames(), nil)
	if !ok {
		return false
	}
	c.cInodes = inodes
	return true
}

// captureAudit: after quiescence and before any export, exactly the four fixed
// names on their recorded inodes, 65534:65534, 0666, one link, regular.
func (c *ctl) captureAudit() bool {
	t, ok := c.inspectMap("/capture", nil)
	if !ok {
		return false
	}
	_, _, _, ok = checkMap(t, false, "", 8454144, pngNames(), c.cInodes)
	return ok
}

type inputsRes struct {
	ok  bool
	sum string
}

// inputLineOK is ^[0-9a-f]{64}  /src/.+$.
func inputLineOK(l []byte) bool {
	if len(l) < 72 || l[64] != ' ' || l[65] != ' ' || string(l[66:71]) != "/src/" {
		return false
	}
	for _, ch := range l[:64] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// inputHash streams the input manifest through SHA-256 and the line rule without
// buffering it: non-empty, every line a SHA-256, two spaces and an absolute /src/
// path, no line over 65,536 bytes.
func (c *ctl) inputHash() bool {
	r := c.do(&callSpec{kind: kOther, argv: c.sup(10, "/usr/bin/cat", "/usr/local/share/boxset/inputs.sha256"), bound: supBound(10),
		stdout: func(in io.Reader) any {
			h := sha256.New()
			br := bufio.NewReaderSize(io.TeeReader(in, h), capOut+1)
			var total int
			for {
				line, err := br.ReadSlice('\n')
				total += len(line)
				if len(line) > 0 && !inputLineOK(bytes.TrimSuffix(line, []byte("\n"))) {
					return inputsRes{}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return inputsRes{}
				}
			}
			return inputsRes{ok: total > 0, sum: hex.EncodeToString(h.Sum(nil))}
		}})
	ir, _ := r.out.(inputsRes)
	if r.startErr != nil || r.status != 0 || !r.outOK || !ir.ok {
		return false
	}
	c.inputs = ir.sum
	return true
}

// runChild runs the child exec under the host deadline: its stdout and stderr go
// through the in-process redactor (cap 65,536) to the controller's stdout, and
// `timeout --verbose` keeps its own stderr in TMPD/tmo (non-empty: the deadline
// fired). A redactor stop closes the read end, then removes the owned container
// through a removal the same wait supervises beside the child's own deadline.
func (c *ctl) runChild() (cs, rs int, tmo, ok bool) {
	fields := strings.Fields(envArgs)
	if c.capture != "" {
		fields = append(fields, "-e", "BOXSET_CAPTURE_DIR=/capture", "-e", "BOXSET_CAPTURE_CHALLENGE="+c.challenge)
	}
	c.say(fmt.Sprintf("image=%s inputs=%s timeout=%ds container=%s", c.img, c.inputs, c.timeout, c.ctr))
	argv := []string{"timeout", "--verbose", "--kill-after=5s", strconv.Itoa(c.timeout) + "s", "sh", "-c", `exec docker exec "$@" 2>&1`, "sh", "-u", childUser}
	argv = append(argv, fields...)
	argv = append(argv, c.ctr, "/usr/bin/timeout", "--kill-after=5s", "--", strconv.Itoa(c.timeout+5)+"s")
	argv = append(argv, c.argv...)
	tmoFile, err := os.OpenFile(c.tmpd+"/tmo", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		c.stepDie("child_status_unknown")
		return 0, 0, false, false
	}
	r := c.do(&callSpec{
		kind: kOther,
		argv: argv,
		stdout: func(in io.Reader) any {
			return redactStream(c.ac, streamOpts{capN: capOut, raw: -1, tail: -1}, in, c.out)
		},
		errOut:      tmoFile,
		bound:       time.Duration(c.timeout+5) * time.Second,
		removeOnOut: c.ctr, // output-error removal, owned by the controller alone
	})
	// tmoFile is a written handle (the child's stderr): a close error leaves its
	// deadline note unknown, so the child status is unknown too.
	if err := tmoFile.Close(); err != nil {
		if !c.cancelled.Load() {
			c.closure()
			c.die("child_status_unknown", statusFail)
		}
		return 0, 0, false, false
	}
	if r.startErr != nil {
		if !errors.Is(r.startErr, errCancelled) {
			c.closure()
			c.die("child_status_unknown", statusFail)
		}
		return 0, 0, false, false
	}
	if c.cancelled.Load() {
		return 0, 0, false, false
	}
	rs, _ = r.out.(int)
	if !r.outOK {
		rs = exitReadFail
		c.out.dead.Store(true)
	}
	if fi, err := os.Stat(c.tmpd + "/tmo"); err == nil && fi.Size() > 0 {
		tmo = true
	}
	return r.status, rs, tmo, true
}

// quiesce: every process of the child user is killed, then a 65534 exec requires
// every task of that user to be a zombie (three rounds, one second apart).
func (c *ctl) quiesce() bool {
	k := c.do(&callSpec{kind: kOther,
		argv:  []string{"timeout", "--kill-after=2s", "12s", "docker", "exec", "-u", childUser, c.ctr, "/usr/bin/sh", "-c", "kill -KILL -1"},
		bound: 14 * time.Second})
	if errors.Is(k.startErr, errCancelled) {
		return false
	}
	for round := 1; round <= 3; round++ {
		v := c.do(&callSpec{kind: kOther, argv: c.sup(10, "/usr/bin/sh", "-c", verifyScript, "sh"), bound: supBound(10)})
		if errors.Is(v.startErr, errCancelled) {
			return false
		}
		if v.startErr == nil && v.status == 0 {
			return true
		}
		if round < 3 {
			c.pause(time.Second)
		}
	}
	return false
}

// closure covers exactly the owned names whose start was issued, whatever that
// call returned. Each issued name gets one status-checked exact-name query; a
// present name needs an owned removal that exits 0. Then one absolute 15 s window,
// read as exact monotonic nanoseconds when the removal step ends and never reset,
// must re-confirm every issued name absent: a confirmation is admitted with at
// least 1 ms left, bounded by the time left floored to milliseconds with no grace,
// and must finish strictly before the window ends. A failed, malformed or late
// query, a name still present, a nonzero removal and a failed or timed-out create
// stay failures even when a later query shows absence. Cancellation never
// interrupts it.
func (c *ctl) closure() bool {
	c.closureRan = true
	failed := map[string]bool{}
	fail := func(n string) {
		if !failed[n] {
			failed[n] = true
			c.bad("cleanup_failed container=" + n)
		}
	}
	names := append([]string(nil), c.issued...)
	for _, n := range names {
		out, ok := c.queryName("10", 10*time.Second, n, "first")
		switch {
		case !ok:
			fail(n)
		case out == "":
		case out == n:
			c.rmOwned(n)
		default:
			fail(n)
		}
	}
	pend := names
	t0, err := c.now()
	end := t0 + 15000000000
confirm:
	for err == nil && len(pend) > 0 { // no reconciliation window without a reading
		var next []string
		for i, n := range pend {
			at, err := c.now()
			if err != nil { // an unusable clock admits no further confirmation of any name
				pend = append(next, pend[i:]...)
				break confirm
			}
			left := end - at
			if left < 1000000 {
				next = append(next, n)
				continue
			}
			ms := left / 1000000
			out, ok := c.queryName(fmt.Sprintf("%d.%03d", ms/1000, ms%1000), time.Duration(ms)*time.Millisecond, n, "confirm")
			if !ok {
				fail(n) // a failed confirmation stays a failure
				next = append(next, n)
				continue
			}
			if done, err := c.now(); err != nil || done >= end {
				fail(n)
				if err != nil {
					pend = append(next, pend[i:]...)
					break confirm
				}
				next = append(next, n)
				continue
			}
			switch out {
			case "":
			case n:
				next = append(next, n)
			default:
				fail(n) // a foreign answer is not absence
				next = append(next, n)
			}
		}
		pend = next
		if len(pend) == 0 {
			break
		}
		at, err := c.now()
		if err != nil || end-at <= 1000000000 {
			break
		}
		time.Sleep(time.Second)
	}
	for _, n := range pend {
		fail(n)
	}
	for _, n := range c.createFail {
		fail(n)
	}
	for _, n := range c.rmFail {
		fail(n)
	}
	if len(failed) != 0 {
		return false
	}
	c.ctrClosed = true
	return true
}

// ---- write root staging and publication ----------------------------------------

func (c *ctl) exportArgv(name string) []string {
	return c.sup(30, "/usr/bin/cat", "--", "/src/"+c.write+"/"+name)
}

type copyRes struct {
	n   int64
	err error
}

// exportPart streams one produced file through the exporter and an in-process
// 33,554,433-byte limiter into its staged part, created exclusively inside the
// pinned write root. A corpus or sample part must hold exactly the audited raw
// size; a draft passes from the limiter through the redactor, which counts the
// raw bytes and holds the 65,536-byte cap, and its part must be 1-65,536 bytes.
func (c *ctl) exportPart(name string, size int64) bool {
	part := "." + name + "." + c.runid + ".part"
	f, err := c.wroot.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false
	}
	drafts := c.write == "docs/spikes/issue-drafts"
	r := c.do(&callSpec{
		kind: kOther,
		argv: c.exportArgv(name),
		stdout: func(in io.Reader) any {
			lim := io.LimitReader(in, capMap+1)
			if drafts {
				st := redactStream(c.ac, streamOpts{capN: capDraft, raw: size, tail: -1}, lim, f)
				// f is a written handle: its close error is a failed export.
				if err := f.Close(); err != nil && st == exitOK {
					st = exitWrite
				}
				return st
			}
			n, err := io.Copy(f, lim)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return copyRes{n, err}
		},
		bound: supBound(30),
	})
	if r.startErr != nil {
		closeQuiet(f) // never written: the exporter did not start
		return false
	}
	if r.status != 0 || !r.outOK {
		return false
	}
	if drafts {
		if st, _ := r.out.(int); st != 0 { // the redactor counted the raw bytes and held the cap
			return false
		}
		fi, err := c.wroot.Lstat(part)
		return err == nil && fi.Size() >= 1 && fi.Size() <= capDraft
	}
	cr, _ := r.out.(copyRes)
	return cr.err == nil && cr.n == size
}

func (c *ctl) stagingCleanup() {
	if c.wroot == nil {
		return
	}
	for _, n := range c.mapNames {
		_ = c.wroot.Remove("." + n + "." + c.runid + ".part")
	}
}

// stageWrite audits the protected map and stages every produced file. It returns
// 0 staged (c.staged lists the names), 1 failed, 2 nothing to publish.
func (c *ctl) stageWrite(cs int) int {
	t, ok := c.inspectMap("/src/"+c.write, nil)
	var free string
	var sizes map[string]int64
	if ok {
		free, sizes, _, ok = checkMap(t, false, "", capMap, c.mapNames, c.wInodes)
	}
	if !ok {
		if !c.cancelled.Load() {
			c.bad("write_audit_failed root=" + c.write)
		}
		return 1
	}
	if cs != 0 {
		if free == "0" {
			c.say("write_cap_reached root=" + c.write)
			return 2
		}
		if c.write != "docs/spikes/issue-drafts" {
			return 2
		}
	}
	if !c.rootUnchanged() {
		c.bad("write_root_changed root=" + c.write)
		return 1
	}
	c.staged = nil
	for _, n := range c.mapNames {
		if sizes[n] <= 0 {
			continue
		}
		if !c.exportPart(n, sizes[n]) {
			c.stagingCleanup()
			if !c.cancelled.Load() {
				c.bad("write_audit_failed root=" + c.write)
			}
			return 1
		}
		c.staged = append(c.staged, n)
	}
	if len(c.staged) == 0 {
		return 2
	}
	return 0
}

// identOf is the identity of NAME itself, never following a symlink.
func (c *ctl) identOf(name string) (fileID, bool) {
	fi, err := c.wroot.Lstat(name)
	if err != nil {
		return fileID{}, false
	}
	return idOf(fi)
}

// partIdent is the identity of one staged part; no rename is made for a part whose
// identity cannot be held.
func (c *ctl) partIdent(part string) (fileID, bool) {
	return c.identOf(part)
}

func (c *ctl) isSymlink(name string) bool {
	fi, err := c.wroot.Lstat(name)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// renameOne publishes one staged part over its map name; Rename never writes an
// existing inode, so a hard-linked outside file keeps its bytes.
func (c *ctl) renameOne(src, dst string) error { return c.wroot.Rename(src, dst) }

// unlinkBackup drops one backup after the commit point.
func (c *ctl) unlinkBackup(name string) error { return c.wroot.Remove(name) }

func (c *ctl) conflict(name string) {
	c.say("publish_rollback_conflict root=" + c.write + " name=" + name)
}

// ownsBackup reports whether the backup name still holds the very inode this run linked there. A backup name that was
// never acquired by this run, or that a foreign file has since replaced, is never usable by rollback or cleanup.
func (c *ctl) ownsBackup(n string) bool {
	q, ok := c.partQ[n]
	if !ok {
		return false
	}
	got, ok := c.identOf("." + n + "." + c.runid + ".prev")
	return ok && got == q
}

// rollback undoes this run's renames by current state, in reverse map order.
// A name that is not in a state this run produced is a foreign replacement: it is
// left alone and its backup is kept. A backup is consumed only when this run acquired
// it and it still has the acquired identity.
func (c *ctl) rollback() {
	for i := len(c.staged) - 1; i >= 0; i-- {
		n := c.staged[i]
		p := c.partP[n]
		if p == (fileID{}) {
			continue
		}
		q, hasQ := c.partQ[n]
		part, bak := "."+n+"."+c.runid+".part", "."+n+"."+c.runid+".prev"
		cur, curOK := c.identOf(n)
		have := c.ownsBackup(n)
		switch {
		case curOK && cur == p:
			if have {
				if err := c.wroot.Rename(bak, n); err != nil {
					c.conflict(n)
					continue
				}
			} else if hasQ { // the acquired backup is gone or replaced: the new file is not removed for an unrestorable prior
				c.conflict(n)
				continue
			} else if err := c.wroot.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) { // the new inode of a name that had none
				c.conflict(n)
			}
		case !curOK && !c.isSymlink(n):
			if hasQ {
				if !have {
					c.conflict(n)
				} else if err := c.wroot.Link(bak, n); err != nil {
					c.conflict(n)
				} else if err := c.wroot.Remove(bak); err != nil && !errors.Is(err, fs.ErrNotExist) {
					c.conflict(n)
				}
			}
		case hasQ && curOK && cur == q:
			if have {
				if err := c.wroot.Remove(bak); err != nil && !errors.Is(err, fs.ErrNotExist) {
					c.conflict(n)
				}
			}
		default:
			c.conflict(n)
			continue
		}
		if pid, ok := c.identOf(part); ok && pid == p {
			_ = c.wroot.Remove(part)
		}
	}
}

func (c *ctl) failPub(reason string) string {
	c.rollback()
	return reason
}

// publishInRoot runs inside the pinned write root and returns the failing reason
// word, "" on success, publish_cleanup_failed after the commit point (reported but
// never rolled back) or reasonCancelled. The P and Q identities live in memory.
func (c *ctl) publishInRoot() string {
	if !c.rootUnchanged() {
		return "write_root_changed"
	}
	clear(c.partP)
	clear(c.partQ)
	for _, n := range c.staged {
		part, bak := "."+n+"."+c.runid+".part", "."+n+"."+c.runid+".prev"
		fi, err := c.wroot.Lstat(n)
		exists := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) || exists && !fi.Mode().IsRegular() {
			return c.failPub("write_publish_failed")
		}
		pid, ok := c.partIdent(part)
		if !ok { // an unheld part identity authorises no rename
			return c.failPub("write_publish_failed")
		}
		if exists {
			qid, ok := idOf(fi)
			if !ok {
				return c.failPub("write_publish_failed")
			}
			// A backup that already stands there is not this run's: nothing is recorded for it, so no rollback or
			// cleanup can ever remove it. Only the link made here, to the identity just observed, is owned.
			if err := c.wroot.Link(n, bak); err != nil {
				return c.failPub("write_publish_failed")
			}
			if got, ok := c.identOf(bak); !ok || got != qid {
				return c.failPub("write_publish_failed")
			}
			c.partQ[n] = qid
		}
		c.partP[n] = pid
		if c.cancelled.Load() { // after the accounted backup of this name
			return c.failPub(reasonCancelled)
		}
	}
	for _, n := range c.staged {
		if err := c.renameOne("."+n+"."+c.runid+".part", n); err != nil {
			return c.failPub("write_publish_failed")
		}
		if c.cancelled.Load() {
			return c.failPub(reasonCancelled)
		}
	}
	if !c.rootUnchanged() { // the identity re-check after the renames
		return c.failPub("write_root_changed")
	}
	for _, n := range c.staged {
		if got, ok := c.identOf(n); !ok || got != c.partP[n] {
			return c.failPub("write_publish_failed")
		}
		if _, had := c.partQ[n]; had && !c.ownsBackup(n) {
			return c.failPub("write_publish_failed")
		}
	}
	if c.cancelled.Load() {
		return c.failPub(reasonCancelled)
	}
	// Commit point: every rename returned 0, the identities re-checked and every
	// backup still holds its acquired identity. From here nothing rolls back.
	reason := ""
	for _, n := range c.staged {
		if _, had := c.partQ[n]; !had {
			continue
		}
		bak := "." + n + "." + c.runid + ".prev"
		if !c.ownsBackup(n) { // replaced since the check: the foreign file is not this run's to remove
			c.say("publish_cleanup_failed root=" + c.write + " name=" + bak)
			reason = "publish_cleanup_failed"
		} else if err := c.unlinkBackup(bak); err != nil {
			c.say("publish_cleanup_failed root=" + c.write + " name=" + bak)
			reason = "publish_cleanup_failed"
		}
	}
	return reason
}

func (c *ctl) publishWrite() bool {
	switch reason := c.publishInRoot(); reason {
	case "":
		return true
	case "publish_cleanup_failed":
		if c.first125 == "" {
			c.first125 = "publish_cleanup_failed"
		}
		return true
	case reasonCancelled:
		c.stagingCleanup()
		return false
	default:
		c.stagingCleanup()
		c.bad(reason + " root=" + c.write)
		return false
	}
}

// ---- capture staging and publication -------------------------------------------

func (c *ctl) capCatArgv(left int, name string) []string {
	return c.sup(left, "/usr/bin/cat", "--", "/capture/"+name+".png")
}

func (c *ctl) validatorArgv(left int, vname, name string) []string {
	return []string{"timeout", "--kill-after=2s", strconv.Itoa(left) + "s", "docker", "run", "--rm", "-i", "--name", vname, "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", supUser, "--memory", "64m", "--memory-swap", "64m", "--pids-limit", "64",
		"--label", "boxset.dev-run=" + c.runid, c.img,
		"/usr/bin/timeout", "--kill-after=1s", "10s", "/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}
}

func validatorBound(left int) time.Duration { return time.Duration(left+2) * time.Second }

// captureAuthority: the lexical chain .devrites/work/<slug>/DIR is real
// directories and its leaf is the pinned object, owned by the effective UID.
func (c *ctl) captureAuthority() bool {
	if !c.chainOK(".devrites", "work", c.slug, c.leaf) {
		return false
	}
	fi, err := c.repoRoot.Lstat(c.capture)
	if err != nil {
		return false
	}
	id, ok := idOf(fi)
	return ok && id == c.cid && ownerOf(fi) == os.Geteuid()
}

func (c *ctl) capEntries() ([]string, bool) {
	f, err := c.croot.Open(".")
	if err != nil {
		return nil, false
	}
	defer closeQuiet(f)
	names, err := f.Readdirnames(-1)
	return names, err == nil
}

// pngWriter counts, hashes and remembers the first 8 and last 12 bytes it writes.
type pngWriter struct {
	w     io.Writer
	h     hash.Hash
	n     int64
	first [8]byte
	tail  [12]byte
}

func (p *pngWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	b = b[:n]
	p.h.Write(b)
	if p.n < int64(len(p.first)) {
		copy(p.first[p.n:], b)
	}
	if len(b) >= len(p.tail) {
		copy(p.tail[:], b[len(b)-len(p.tail):])
	} else {
		copy(p.tail[:], p.tail[len(b):])
		copy(p.tail[len(p.tail)-len(b):], b)
	}
	p.n += int64(n)
	return n, err
}

type pngRes struct {
	n     int64
	first [8]byte
	tail  [12]byte
	sum   [32]byte
	err   error
}

var (
	pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	iendTrailer  = []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}
)

// writeManifest stages the manifest bytes exclusively inside the pinned DIR.
func (c *ctl) writeManifest(content string) error {
	f, err := c.croot.OpenFile(".capture.manifest.part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, content); err != nil {
		_ = f.Close() // the write error already fails the call
		return err
	}
	return f.Close()
}

// captureStage exports, validates and stages every file plus the manifest inside
// the pinned DIR; nothing is published yet. The transfer budget keeps whole wall
// seconds: 30 from the start, whole remaining seconds per step, no final check.
func (c *ctl) captureStage() string {
	endSec := time.Now().Unix() + 30
	if !c.captureAuthority() {
		return "capture_root_changed"
	}
	if names, ok := c.capEntries(); !ok || len(names) != 0 {
		return "capture_root_changed"
	}
	manifest := "boxset-capture 1\nchallenge " + c.challenge + "\n"
	var sum int64
	for _, name := range captureNames {
		left := int(endSec - time.Now().Unix())
		if left <= 0 {
			return "capture_copy_failed"
		}
		raw, part := "."+name+".png.raw", "."+name+".png.part"
		rf, err := c.croot.OpenFile(raw, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "capture_copy_failed"
		}
		// the staged raw file exists; the exporter has not run
		xr := c.do(&callSpec{
			kind: kOther,
			argv: c.capCatArgv(left, name),
			stdout: func(in io.Reader) any {
				n, err := io.Copy(rf, io.LimitReader(in, capPNG+1))
				// rf is a written handle: its close error is a failed copy.
				if cerr := rf.Close(); err == nil {
					err = cerr
				}
				return copyRes{n, err}
			},
			bound: supBound(left),
		})
		if xr.startErr != nil {
			closeQuiet(rf) // never written: the exporter did not start
			if errors.Is(xr.startErr, errCancelled) {
				return reasonCancelled
			}
			return "capture_copy_failed"
		}
		cr, _ := xr.out.(copyRes)
		limOK := xr.outOK && cr.err == nil
		if !limOK {
			return "capture_copy_failed"
		}
		if cr.n != capPNG+1 && xr.status != 0 {
			return "capture_copy_failed"
		}
		left = int(endSec - time.Now().Unix())
		if left <= 0 {
			return "capture_copy_failed"
		}
		vname := c.ctr + "-v-" + name
		in, err := c.croot.Open(raw)
		if err != nil {
			return "capture_copy_failed"
		}
		pf, err := c.croot.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			closeQuiet(in)
			return "capture_copy_failed"
		}
		vr := c.do(&callSpec{
			kind:  kCreate,
			argv:  c.validatorArgv(left, vname, name),
			stdin: in,
			stdout: func(src io.Reader) any {
				pw := &pngWriter{w: pf, h: sha256.New()}
				_, err := io.Copy(pw, io.LimitReader(src, capPNG+1))
				// pf is a written handle: its close error is a failed stage.
				if cerr := pf.Close(); err == nil {
					err = cerr
				}
				res := pngRes{n: pw.n, first: pw.first, tail: pw.tail, err: err}
				copy(res.sum[:], pw.h.Sum(nil))
				return res
			},
			bound: validatorBound(left),
			issue: vname,
		})
		closeQuiet(in)
		_ = c.croot.Remove(raw)
		if vr.startErr != nil {
			closeQuiet(pf)
			if errors.Is(vr.startErr, errCancelled) {
				return reasonCancelled
			}
			return "capture_copy_failed"
		}
		if vs := vr.status; vs < 0 || vs == 124 || vs == 125 || vs == 126 || vs == 127 || vs == 137 {
			c.failCreate(vname) // a failed, timed-out or unknown create is retained
		}
		pr, _ := vr.out.(pngRes)
		if !vr.outOK || pr.err != nil {
			return "capture_copy_failed"
		}
		switch vr.status {
		case 0:
		case 10:
			return "capture_missing"
		case 11:
			return "capture_oversize"
		case 12:
			return "capture_not_png"
		case 13:
			return "capture_not_static"
		case 14:
			return "capture_dimensions"
		case 15:
			return "capture_challenge_mismatch"
		default:
			return "capture_copy_failed" // any other validator status
		}
		if pr.n == capPNG+1 {
			return "capture_oversize"
		}
		if pr.n < 20 || !bytes.Equal(pr.first[:], pngSignature) || !bytes.Equal(pr.tail[:], iendTrailer) {
			return "capture_copy_failed"
		}
		manifest += fmt.Sprintf("%x %d %s.png\n", pr.sum, pr.n, name)
		sum += pr.n // staged and checked
	}
	if err := c.writeManifest(manifest); err != nil {
		return "capture_copy_failed"
	}
	data, err := c.croot.ReadFile(".capture.manifest.part")
	if err != nil || len(data) != len(manifest) || sha256.Sum256(data) != sha256.Sum256([]byte(manifest)) {
		return "capture_copy_failed"
	}
	c.capTotal = sum
	return ""
}

// publishCapture hard-links each staged part to its fixed name inside the pinned
// DIR without overwrite, then the manifest, and requires the acquisition check and
// exactly five names. Cancellation is checked between operations.
func (c *ctl) publishCapture() string {
	if !c.captureAuthority() {
		return "capture_root_changed"
	}
	for _, name := range captureNames {
		if c.cancelled.Load() {
			return reasonCancelled
		}
		if err := c.croot.Link("."+name+".png.part", name+".png"); err != nil {
			return "capture_copy_failed"
		}
		_ = c.croot.Remove("." + name + ".png.part")
	}
	if c.cancelled.Load() {
		return reasonCancelled
	}
	if err := c.croot.Link(".capture.manifest.part", "capture.manifest"); err != nil {
		return "capture_copy_failed"
	}
	_ = c.croot.Remove(".capture.manifest.part")
	if !c.captureAuthority() {
		return "capture_root_changed"
	}
	names, ok := c.capEntries()
	if !ok || len(names) != 5 {
		return "capture_root_changed"
	}
	for _, n := range names {
		if n != "capture.manifest" && !slices.Contains(pngNames(), n) {
			return "capture_root_changed"
		}
	}
	if c.cancelled.Load() { // the last check: past it the capture is committed
		return reasonCancelled
	}
	return ""
}

// captureAbort removes only runner-owned fixed names through the pinned DIR, so a
// moved object is emptied, then DIR itself through its actual current parent: the
// parent is acquired as an os.Root capability, and only the fixed basename whose
// identity still equals the acquired DIR is removed. A foreign replacement is never
// removed.
func (c *ctl) captureAbort() {
	if c.croot == nil { // the object was never pinned: nothing is owned by name
		return
	}
	for _, name := range captureNames {
		for _, n := range []string{"." + name + ".png.raw", "." + name + ".png.part", name + ".png"} {
			_ = c.croot.Remove(n)
		}
	}
	_ = c.croot.Remove(".capture.manifest.part")
	_ = c.croot.Remove("capture.manifest")
	f, err := c.croot.Open(".")
	if err != nil {
		return
	}
	defer closeQuiet(f)
	parent, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(f.Fd())) + "/..")
	if err != nil {
		return
	}
	defer closeQuiet(parent)
	if fi, err := parent.Lstat(c.leaf); err == nil && fi.IsDir() {
		if id, ok := idOf(fi); ok && id == c.cid {
			_ = parent.Remove(c.leaf)
		}
	}
}

// removeTmpd removes the run-private directory, and only the one acquired.
func (c *ctl) removeTmpd() {
	if !c.tmpdOwned {
		return
	}
	if fi, err := os.Lstat(c.tmpd); err == nil {
		if id, ok := idOf(fi); ok && id == c.tid {
			_ = os.RemoveAll(c.tmpd)
		}
	}
}

// finish is the only exit: closure runs if names were issued and it has not, then
// one unchecked removal of an unconfirmed container, then staging cleanup, capture
// abort and the run-private directory.
func (c *ctl) finish() int {
	if len(c.issued) > 0 && !c.closureRan {
		c.closure()
	}
	if slices.Contains(c.issued, c.ctr) && !c.ctrClosed {
		c.do(&callSpec{kind: kReconcile, argv: []string{"timeout", "30", "docker", "rm", "-f", c.ctr}, bound: 30 * time.Second})
	}
	if c.write != "" {
		c.stagingCleanup()
	}
	if c.captureClaimed && !c.captureOK {
		c.captureAbort()
	}
	c.removeTmpd()
	if c.cancelled.Load() || !c.statusSet {
		return statusFail
	}
	return c.status
}
