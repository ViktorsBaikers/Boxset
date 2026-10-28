// Command benchaccess measures the access series of a reference-profile run and
// judges a recorded run against the accepted budgets.
//
//	benchaccess [-root DIR] [-control slow] -out DIR   measure: writes DIR/samples.jsonl only
//	benchaccess -judge RUN_DIR                        judge: exit 0 PASS, 1 FAIL, 3 INVALID
//
// Measuring is diagnostic. Only -judge, which reads the recorded samples and the
// run's three V2 host records through the immutable image, may print
// ACCESS-BENCH-PASS.
package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Exit codes follow bench/parser: 2 is usage, 3 an INVALID verdict.
const (
	exitPass    = 0
	exitFail    = 1
	exitUsage   = 2
	exitInvalid = 3

	inputsManifest = "/usr/local/share/boxset/inputs.sha256"
	maxInputsBytes = 8 << 20
	maxStatusBytes = 64 << 10

	// The recorded series' shapes.
	users           = 1_000
	fixturePassword = "synthetic-fixture-password"
	wrongPassword   = "synthetic-wrong-password"
	pingPath        = "/ping"
	sessionPath     = "/api/v1/session"
	statusPath      = "/api/v1/system/status"
	listPath        = "/api/v1/users?limit=50"
	rowListPath     = "/api/v1/users?limit=500"
	userListRateHz  = 20
	listWindow      = 10 * time.Second
	abuseWindow     = 30 * time.Second
	slowSockets     = 64
	floodRequests   = 1_000
	floodInFlight   = 32
	hashLogins      = 50
	keyPairs        = 1_000
	startPoll       = time.Millisecond
	goroutinesPoll  = time.Second
	rssReadInterval = 10 * time.Second
	rssHoldWindow   = 600 * time.Second
	gzipPayloadSize = 128 << 10
	gzipRuns        = 10

	// The recorded per-execution caps.
	buildCap  = 300 * time.Second
	startCap  = 10 * time.Second
	keyCap    = 30 * time.Second
	hashCap   = 30 * time.Second
	abuseCap  = 60 * time.Second
	gzipCap   = 10 * time.Second
	fillCap   = 620 * time.Second
	drainTime = 30 * time.Second

	fillWindowWait = 5 * time.Second

	// keyHeader is the header the route guard reads a principal's API key from.
	// The bearer belongs to the setup credential, never to a user key.
	keyHeader = "X-Api-Key"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchaccess", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "measure: directory that receives samples.jsonl")
	judgeDir := fs.String("judge", "", "judge: recorded run directory")
	control := fs.String("control", "", `measure: "slow" builds and measures the slow-control fixture`)
	root := fs.String("root", ".", "repository root")
	inputs := fs.String("inputs", inputsManifest, "measure: image input manifest to hash into the samples (the judge always reads the image's own)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || (*out == "") == (*judgeDir == "") || (*control != "" && *control != controlSlow) || (*control != "" && *judgeDir != "") {
		say(stderr, "usage: benchaccess [-root DIR] [-control slow] -out DIR | -judge RUN_DIR [-root DIR]\n")
		return exitUsage
	}
	if *judgeDir != "" {
		return judge(*root, *judgeDir, inputsManifest, stdout)
	}
	return measureTo(*root, *inputs, *out, *control, stdout, stderr)
}

// ---- measure ---------------------------------------------------------------------

func measureTo(root, inputs, out, control string, stdout, stderr io.Writer) int {
	abs, err := filepath.Abs(out)
	if err != nil {
		say(stderr, "benchaccess: out_invalid\n")
		return exitFail
	}
	set, err := measure(root, inputs, filepath.Base(filepath.Dir(abs)), control)
	if err != nil {
		say(stderr, "benchaccess: %v\n", err)
		return exitFail
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		say(stderr, "benchaccess: write_failed file=%s\n", samplesName)
		return exitFail
	}
	if err := writeSamples(filepath.Join(out, samplesName), set); err != nil {
		say(stderr, "benchaccess: write_failed file=%s\n", samplesName)
		return exitFail
	}
	say(stdout, "benchaccess: samples=%d executions=%d control=%q purpose=%s goroutines=%s reason=%s (diagnostic only; judge decides)\n",
		set.Header.Samples, set.Header.Executions, set.Header.Control, set.Header.Purpose, set.Header.Goroutines, set.Header.Reason)
	return exitPass
}

// measure runs the whole protocol: one pre-migrated fixture, ten fresh servers on
// copies of it — two interleaved sides of five — and one fill-fixture hold. Every
// directory it writes sits directly below the runner's data mount.
func measure(root, inputs, run, control string) (*sampleSet, error) {
	start := time.Now().UTC()
	mount, err := dataDir()
	if err != nil {
		return nil, err
	}
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return nil, errors.New("inputs_unavailable")
	}
	dir, err := os.MkdirTemp(mount, "access-")
	if err != nil {
		return nil, errors.New("data_dir_invalid")
	}
	ctx := context.Background()
	timing, fill, err := buildBinaries(ctx, root, dir, control)
	if err != nil {
		return nil, err
	}
	fx, err := makeFixture(ctx, timing, filepath.Join(dir, "fixture"))
	if err != nil {
		return nil, err
	}
	set := &sampleSet{}
	var unobserved string
	for exec := 1; exec <= executions; exec++ {
		for _, side := range []string{sideA1, sideA2} {
			got, reading, err := runExecution(ctx, timing, fx, filepath.Join(dir, side+"-"+strconv.Itoa(exec)), side, exec)
			if err != nil {
				return nil, err
			}
			if reading.reason != "" && unobserved == "" {
				unobserved = reading.reason
			}
			set.Samples = append(set.Samples, got...)
		}
	}
	rss, err := holdFill(ctx, fill, fx, filepath.Join(dir, "rss"))
	if err != nil {
		return nil, err
	}
	set.Samples = append(set.Samples, rss...)
	// The ABUSE windows' status polls are the run's goroutine observation. One
	// window that could not read the route leaves the run with no observation at
	// all — the reason is recorded and the partial maximum is dropped — so the cap
	// is either judged on a whole run's readings or refused as missing, never
	// passed on the windows that happened to answer. no_observation_surface stays
	// for a build that cannot read the route at all: the owner's decision on
	// q-2026-10-27-002 put the reading on the authorized status route, so a normal
	// run of this harness never records it.
	goroutines, reason := goroutinesObserved, ""
	if unobserved != "" {
		goroutines, reason = goroutinesUnavailable, unobserved
		set.Samples = slices.DeleteFunc(set.Samples, func(s sample) bool { return s.Series == seriesGoroutines })
	}
	set.Header = header{
		Schema: samplesSchema, Kind: samplesKind, Run: run, InputsSHA: sha256Hex(manifest),
		DataDir: dataMount, Control: control, Purpose: timing.purpose, Tags: timing.tags,
		CGO: timing.cgo, Trimpath: timing.trimpath == "true", Go: timing.version, GOOS: timing.goos, GOARCH: timing.goarch,
		Goroutines: goroutines, Reason: reason,
		Executions: executions, StartDate: start.Format(benchprofile.DateLayout),
		EndDate: time.Now().UTC().Format(benchprofile.DateLayout),
	}
	_, set.Header.UTCOffset = start.Zone()
	return set, nil
}

// dataDir is the runner's data mount, and it must be a device of its own: the
// measured storage is never the container's overlay or its tmpfs.
func dataDir() (string, error) {
	dir := os.Getenv("BOXSET_DATA_DIR")
	if dir != dataMount {
		return "", errors.New("data_dir")
	}
	dev, err := deviceOf(dir)
	if err != nil {
		return "", errors.New("data_dir_device")
	}
	for _, other := range []string{"/", "/tmp"} {
		d, err := deviceOf(other)
		if err != nil || d == dev {
			return "", errors.New("data_dir_device")
		}
	}
	return dir, nil
}

func deviceOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("device_unknown")
	}
	return uint64(st.Dev), nil
}

// sha256Hex is the lower-case hexadecimal SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readBounded reads a regular, non-link file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, errors.New("file_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, errors.New("file_invalid")
	}
	return data, nil
}

// binary is one built server: its path and the build identity its own build info
// reports.
type binary struct {
	path, purpose, tags, cgo, trimpath, goos, goarch, version string
}

// buildBinaries builds the binary the timing series measure — the ordinary
// production build, or the slow control when -control slow asks for it — and the
// fill fixture the RSS hold measures.
func buildBinaries(ctx context.Context, root, dir, control string) (timing, fill binary, err error) {
	bdir := filepath.Join(dir, "build")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return timing, fill, errors.New("build_dir")
	}
	tags := ""
	if control == controlSlow {
		tags = tagSlow
	}
	timing, err = build(ctx, root, filepath.Join(bdir, "timing"), tags)
	if err != nil {
		return timing, fill, err
	}
	want := purposeProduction
	if control == controlSlow {
		want = purposeSlow
	}
	if timing.purpose != want {
		// The purpose comes from the binary's own build info, so a build that
		// did not carry the requested settings is refused here instead of being
		// recorded under a purpose it does not have.
		return timing, fill, errors.New("binary_purpose")
	}
	fill, err = build(ctx, root, filepath.Join(bdir, "fill"), tagFill)
	if err != nil {
		return timing, fill, err
	}
	if fill.purpose != purposeFill {
		return timing, fill, errors.New("binary_purpose")
	}
	return timing, fill, nil
}

// build compiles ./cmd/boxset with the recorded production settings and reads the
// settings back out of the binary.
func build(ctx context.Context, root, out, tags string) (binary, error) {
	ctx, cancel := context.WithTimeout(ctx, buildCap)
	defer cancel()
	args := []string{"build", "-trimpath", "-o", out}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/boxset")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return binary{}, errors.New("build_failed")
	}
	data, err := exec.CommandContext(ctx, "go", "version", "-m", out).Output()
	if err != nil {
		return binary{}, errors.New("build_info_unavailable")
	}
	b, err := readBuildInfo(data)
	if err != nil {
		return binary{}, err
	}
	b.path = out
	return b, nil
}

// readBuildInfo parses `go version -m` output: the Go version on the first line
// and one build setting per following line.
func readBuildInfo(data []byte) (binary, error) {
	var b binary
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		line = strings.TrimPrefix(line, "\t")
		if i == 0 {
			if _, after, ok := strings.Cut(line, ": "); ok {
				b.version = strings.TrimSpace(after)
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "build\t-tags":
			b.tags = val
		case "build\tCGO_ENABLED":
			b.cgo = val
		case "build\t-trimpath":
			b.trimpath = val
		case "build\tGOOS":
			b.goos = val
		case "build\tGOARCH":
			b.goarch = val
		}
	}
	if b.version == "" || b.cgo == "" || b.goos == "" || b.goarch == "" {
		return binary{}, errors.New("build_info_incomplete")
	}
	b.purpose = purposeOf(b.tags)
	return b, nil
}

// purposeOf derives a binary's purpose from its build tags: production identity
// is the absence of a -tags setting, so every tagged build is something else.
func purposeOf(tags string) string {
	switch tags {
	case "":
		return purposeProduction
	case tagFill:
		return purposeFill
	case tagSlow:
		return purposeSlow
	}
	return "other"
}

// fixture is the one pre-migrated data directory every execution is a copy of: a
// routine start, so the migrations ran and the installation row exists, with
// 1,000 users, one of them an admin, and no pending migration, plus the keys the
// series authenticate as.
type fixture struct {
	dir    string
	keyA   string
	keyB   string
	keyObs string
}

func username(i int) string { return fmt.Sprintf("bench%04d", i) }

func makeFixture(ctx context.Context, timing binary, dir string) (*fixture, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("fixture_dir")
	}
	if err := serveOnce(ctx, timing.path, dir); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, install.Layout{Dir: dir}.DB(), store.Options{})
	if err != nil {
		return nil, errors.New("fixture_unavailable")
	}
	defer func() { _ = db.Close() }() // the paths that fail before the close below; a second Close is a no-op
	// The account seam: no HTTP operation creates a user, so the fixture is
	// seeded through the same store the server bootstrapped.
	hasher := auth.NewHasher()
	for i := 1; i <= users; i++ {
		// Reading user administration is admin-only, so the list principal — key
		// B, user 2 — is the fixture's one admin. Every other account is a member.
		role := auth.RoleMember
		if i == 2 {
			role = auth.RoleAdmin
		}
		if _, err := auth.CreateUser(ctx, db, hasher, username(i), fixturePassword, role); err != nil {
			return nil, errors.New("fixture_user")
		}
	}
	keys := auth.NewKeys(db)
	fx := &fixture{dir: dir}
	if _, fx.keyA, err = keys.Create(ctx, username(1), "access-a", "read"); err != nil {
		return nil, errors.New("fixture_key")
	}
	if _, fx.keyB, err = keys.Create(ctx, username(2), "access-b", "read"); err != nil {
		return nil, errors.New("fixture_key")
	}
	// The observation principal is its own credential: the ABUSE poll is then
	// charged to a read budget neither the flood nor user B's list is spending.
	if _, fx.keyObs, err = keys.Create(ctx, username(3), "access-observer", "read"); err != nil {
		return nil, errors.New("fixture_key")
	}
	// Every execution opens its own copy, never this handle, so the fixture is
	// settled here: the store offers no checkpoint of its own, and it does not need
	// one, because the last connection to a WAL database checks the log back into
	// the file and removes it with the shared-memory index. What reach the copies
	// is then one self-contained boxset.db rather than a directory a stopped write
	// left half its state in.
	if err := db.Close(); err != nil {
		return nil, errors.New("fixture_close")
	}
	return fx, nil
}

// serveOnce starts one server on dir and stops it once it reports ready, so the
// fixture is what a routine start leaves behind.
func serveOnce(ctx context.Context, path, dir string) error {
	port, err := freePort()
	if err != nil {
		return errors.New("port_unavailable")
	}
	srv, err := startServer(ctx, path, dir, port)
	if err != nil {
		return err
	}
	defer srv.stop()
	if _, err := awaitPing(ctx, port, time.Now(), startCap); err != nil {
		return err
	}
	return awaitReady(ctx, port, startCap)
}

// copyFixture reproduces the fixture at src inside the fresh directory dst, every
// entry keeping the permission bits it has in the fixture: the installation key is
// owner-only there, and a copy that widened it would be refused at startup. An
// entry that is neither a directory nor a regular file is refused, never followed.
//
// A database's -shm shared-memory index and -wal log are skipped: they are the
// state of the processes writing it, not data, and a copy that carried a stale
// index would make the first connection on the copy recover against it. Only the
// entries beside a database file are dropped, so an ordinary file that happens to
// end the same way is still copied.
func copyFixture(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && isSQLiteEphemeral(filepath.Dir(path), d.Name()) {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch perm := info.Mode().Perm(); {
		case info.IsDir():
			return os.MkdirAll(target, perm)
		case info.Mode().IsRegular():
			return copyFixtureFile(path, target, perm)
		default:
			return fmt.Errorf("%s is not a directory or a regular file", rel)
		}
	})
}

// isSQLiteEphemeral reports whether name is one of the two entries SQLite keeps
// beside the database file base in dir: its shared-memory index (-shm) and its
// write-ahead log (-wal). Both are named by their exact suffix, and only count as
// ephemeral while that database file is there to own them.
func isSQLiteEphemeral(dir, name string) bool {
	base, ok := strings.CutSuffix(name, "-wal")
	if !ok {
		base, ok = strings.CutSuffix(name, "-shm")
	}
	if !ok {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, base))
	return err == nil && info.Mode().IsRegular()
}

// copyFixtureFile writes the bytes of src to the new file dst with perm.
func copyFixtureFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, out.Close(), os.Remove(dst))
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// runExecution measures one fresh server process on a fresh copy of the fixture.
// It also hands back the ABUSE window's goroutine reading: the window's counts, or
// the reason the window took none.
func runExecution(ctx context.Context, timing binary, fx *fixture, dir, side string, exec int) ([]sample, goroutinesReading, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, goroutinesReading{}, errors.New("exec_dir")
	}
	if err := copyFixture(fx.dir, dir); err != nil {
		return nil, goroutinesReading{}, errors.New("fixture_copy")
	}
	port, err := freePort()
	if err != nil {
		return nil, goroutinesReading{}, errors.New("port_unavailable")
	}
	started := time.Now()
	srv, err := startServer(ctx, timing.path, dir, port)
	if err != nil {
		return nil, goroutinesReading{}, err
	}
	defer srv.stop()
	startNS, err := awaitPing(ctx, port, started, startCap)
	if err != nil {
		return nil, goroutinesReading{}, err
	}
	if err := awaitReady(ctx, port, startCap); err != nil {
		return nil, goroutinesReading{}, err
	}
	c := newClient(port, fx.keyA, fx.keyB, fx.keyObs)
	out := []sample{{Series: seriesStart, Side: side, Exec: exec, Purpose: timing.purpose, Stat: startNS}}
	var reading goroutinesReading
	for _, series := range []struct {
		name string
		run  func() (int64, []int64, error)
	}{
		{seriesKey, func() (int64, []int64, error) { return c.keyPairs(ctx) }},
		{seriesHash, func() (int64, []int64, error) { return c.hashBurst(ctx) }},
		{seriesAbuse, func() (int64, []int64, error) {
			stat, raw, got, err := c.abuse(ctx)
			reading = got
			return stat, raw, err
		}},
		{seriesGZIP, measureGZIP},
	} {
		stat, raw, err := series.run()
		if err != nil {
			return nil, goroutinesReading{}, err
		}
		out = append(out, sample{Series: series.name, Side: side, Exec: exec, Purpose: timing.purpose, Stat: stat, Raw: raw})
	}
	if len(reading.counts) > 0 {
		out = append(out, sample{Series: seriesGoroutines, Side: side, Exec: exec, Purpose: timing.purpose,
			Stat: slices.Max(reading.counts), Raw: reading.counts})
	}
	peak, oom, err := cgroupCounters()
	if err != nil {
		return nil, goroutinesReading{}, err
	}
	out = append(out,
		sample{Series: seriesMemoryPeak, Side: side, Exec: exec, Purpose: timing.purpose, Stat: peak},
		sample{Series: seriesOOMKill, Side: side, Exec: exec, Purpose: timing.purpose, Stat: oom},
	)
	return out, reading, nil
}

// holdFill is the one 600 s hold of the fill fixture: its VmRSS every 10 s after
// a routine start, so the verdict sample lies inside the recorded 600–605 s
// window.
func holdFill(ctx context.Context, fill binary, fx *fixture, dir string) ([]sample, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("fill_dir")
	}
	if err := copyFixture(fx.dir, dir); err != nil {
		return nil, errors.New("fixture_copy")
	}
	port, err := freePort()
	if err != nil {
		return nil, errors.New("port_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, fillCap)
	defer cancel()
	started := time.Now()
	srv, err := startServer(ctx, fill.path, dir, port)
	if err != nil {
		return nil, err
	}
	defer srv.stop()
	if _, err := awaitPing(ctx, port, started, startCap); err != nil {
		return nil, err
	}
	if err := awaitReady(ctx, port, startCap); err != nil {
		return nil, err
	}
	var out []sample
	for {
		elapsed := time.Since(started)
		rss, err := vmRSS(srv.cmd.Process.Pid)
		if err != nil {
			return nil, errors.New("rss_unreadable")
		}
		out = append(out, sample{Series: seriesRSS, Purpose: purposeFill, Stat: rss, Raw: []int64{elapsed.Milliseconds()}})
		if elapsed >= rssHoldWindow {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("rss_cap")
		case <-time.After(rssReadInterval):
		}
	}
}

// ---- one execution's series ---------------------------------------------------------

// client speaks to one execution's server.
type client struct {
	hc                       *http.Client
	base, keyA, keyB, keyObs string
}

func newClient(port int, keyA, keyB, keyObs string) *client {
	return &client{
		hc:   &http.Client{Transport: &http.Transport{DisableKeepAlives: false, MaxIdleConnsPerHost: floodInFlight}},
		base: "http://127.0.0.1:" + strconv.Itoa(port), keyA: keyA, keyB: keyB, keyObs: keyObs,
	}
}

// get times one GET and returns its nanoseconds, status and the reason word a
// refusal carried. The word is decoded from at most maxStatusBytes of the body;
// the transport error is never returned, because it prints the request URL.
func (c *client) get(ctx context.Context, path, key string) (int64, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, 0, "", err
	}
	if key != "" {
		req.Header.Set(keyHeader, key)
	}
	t := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	var reason string
	if resp.StatusCode != http.StatusOK {
		reason = problemCode(resp.Body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return time.Since(t).Nanoseconds(), resp.StatusCode, reason, nil
}

// problemCode is the reason word an error response's problem model carried. A
// body that is absent, oversized, not JSON or not a problem yields no word; the
// printer clamps whatever comes back.
func problemCode(body io.Reader) string {
	var p struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxStatusBytes)).Decode(&p); err != nil {
		return ""
	}
	return p.Code
}

// login posts one credential pair. A wrong password is a legitimate answer: the
// flood exists to put the hasher under load, not to authenticate.
func (c *client) login(ctx context.Context) error {
	body := `{"username":"` + username(1) + `","password":"` + wrongPassword + `"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+sessionPath, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// list drives user B's list of 50 rows at the recorded rate for the window and
// hands back every latency it measured.
func (c *client) list(ctx context.Context, window time.Duration) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, window+fillWindowWait)
	defer cancel()
	tick := time.NewTicker(time.Second / userListRateHz)
	defer tick.Stop()
	started := time.Now()
	deadline := started.Add(window)
	lat := make([]int64, 0, int(window.Seconds())*userListRateHz+1)
	for at := started; at.Before(deadline); at = <-tick.C {
		ns, status, reason, err := c.get(ctx, listPath, c.keyB)
		if err != nil || status != http.StatusOK {
			return nil, seriesFailed("list_failed", status, reason, time.Since(started))
		}
		lat = append(lat, ns)
	}
	return lat, nil
}

// keyPairs is the KEY series: 1,000 interleaved pairs of an authenticated session
// read and a public ping, reduced to the p95 of their paired differences.
func (c *client) keyPairs(ctx context.Context) (int64, []int64, error) {
	ctx, cancel := context.WithTimeout(ctx, keyCap)
	defer cancel()
	started := time.Now()
	diffs := make([]int64, 0, keyPairs)
	for range keyPairs {
		auth, status, reason, err := c.get(ctx, sessionPath, c.keyA)
		if err != nil || status != http.StatusOK {
			return 0, nil, seriesFailed("key_session_failed", status, reason, time.Since(started))
		}
		ping, status, reason, err := c.get(ctx, pingPath, "")
		if err != nil || status != http.StatusOK {
			return 0, nil, seriesFailed("key_ping_failed", status, reason, time.Since(started))
		}
		diffs = append(diffs, auth-ping)
	}
	return percentile(diffs, 95), diffs, nil
}

// hashBurst is the HASH series: 50 concurrent wrong-password logins while user B
// lists 50 rows at the recorded rate, reduced to the p95 of B's latency.
func (c *client) hashBurst(ctx context.Context) (int64, []int64, error) {
	ctx, cancel := context.WithTimeout(ctx, hashCap)
	defer cancel()
	lat, err := c.underLoad(ctx, listWindow, func(loadCtx context.Context) {
		var wg sync.WaitGroup
		for range hashLogins {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.login(loadCtx)
			}()
		}
		wg.Wait()
	})
	if err != nil {
		return 0, nil, err
	}
	return percentile(lat, 95), lat, nil
}

// goroutinesReading is one ABUSE window's poll of the status route: the counts it
// read, or the reason it read none. A window that read nothing is a recorded
// failure, never a zero, and the run that contains one has no goroutine
// observation for the judge's cap to read.
type goroutinesReading struct {
	counts []int64
	reason string
}

// abuse is the ABUSE series: 64 slowloris sockets, a 1,000-request single-key
// flood and one stalled reader of a 500-row list while user B lists 50 rows,
// reduced to the p95 of B's latency. Through the same window it polls the status
// route once per recorded interval and hands back that window's goroutine reading.
func (c *client) abuse(ctx context.Context) (int64, []int64, goroutinesReading, error) {
	ctx, cancel := context.WithTimeout(ctx, abuseCap)
	defer cancel()
	// The poll is one request per second on its own credential, so it spends the
	// observation principal's read budget: neither the flood's key nor user B's
	// list traffic shares that budget, and B's 20 req/s cadence measures the same
	// traffic with or without it. Only the peer's shared lookup budget sees the
	// poll, at one request per second.
	polled := make(chan goroutinesReading, 1)
	go func() { polled <- c.observeGoroutines(ctx, abuseWindow) }()
	lat, err := c.underLoad(ctx, abuseWindow, func(loadCtx context.Context) {
		hold, err := slowloris(c.base)
		if err != nil {
			return
		}
		defer closeSockets(hold)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); c.flood(loadCtx) }()
		go func() { defer wg.Done(); stall(loadCtx, c) }()
		wg.Wait()
	})
	if err != nil {
		return 0, nil, goroutinesReading{}, err
	}
	reading := <-polled
	return percentile(lat, 95), lat, reading, nil
}

// observeGoroutines reads the status route's goroutine count once per recorded
// interval for the window and returns every reading it took.
func (c *client) observeGoroutines(ctx context.Context, window time.Duration) goroutinesReading {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var r goroutinesReading
	for deadline := time.Now().Add(window); time.Now().Before(deadline); {
		n, err := c.statusGoroutines(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break // the window closed under the poll; the readings it took stand
			}
			return goroutinesReading{reason: goroutinesPollFailed}
		}
		r.counts = append(r.counts, n)
		select {
		case <-ctx.Done():
		case <-time.After(min(time.Until(deadline), goroutinesPoll)):
		}
	}
	if len(r.counts) == 0 {
		r.reason = goroutinesPollFailed
	}
	return r
}

// statusGoroutines reads one authorized status response's goroutine count: the
// route a principal may read in every lifecycle state, decoded only as far as the
// reading the run records. A refusal, a timeout, a malformed body and a count
// below one are all the same failure: no observation.
func (c *client) statusGoroutines(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+statusPath, nil)
	if err != nil {
		return 0, errors.New("status_unreachable")
	}
	req.Header.Set(keyHeader, c.keyObs)
	resp, err := c.hc.Do(req)
	if err != nil {
		// The transport error is not returned: it carries the request URL, and no
		// path here may hand a URL to a printer.
		return 0, errors.New("status_unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxStatusBytes))
		return 0, errors.New("status_refused")
	}
	var status struct {
		Goroutines int `json:"goroutines"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBytes)).Decode(&status); err != nil {
		return 0, errors.New("status_malformed")
	}
	if status.Goroutines < 1 {
		return 0, errors.New("status_unobserved")
	}
	return int64(status.Goroutines), nil
}

// underLoad runs load beside user B's list loop for the window and returns the
// list latencies. The load runs until the loop is done, so the whole window is
// measured under it.
func (c *client) underLoad(ctx context.Context, window time.Duration, load func(context.Context)) ([]int64, error) {
	loadCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		load(loadCtx)
	}()
	lat, err := c.list(ctx, window)
	stop()
	select {
	case <-done:
	case <-time.After(fillWindowWait):
	}
	return lat, err
}

// flood is the single-key flood: 1,000 requests over 32 connections in flight.
func (c *client) flood(ctx context.Context) {
	queue := make(chan struct{}, floodInFlight)
	var wg sync.WaitGroup
	for range floodRequests {
		queue <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-queue }()
			_, _, _, _ = c.get(ctx, sessionPath, c.keyA)
		}()
	}
	wg.Wait()
}

// slowloris opens the recorded sockets and writes a request head without its
// terminating blank line, so every one of them holds a reader wait open.
func slowloris(base string) ([]net.Conn, error) {
	host := strings.TrimPrefix(base, "http://")
	hold := make([]net.Conn, 0, slowSockets)
	for range slowSockets {
		conn, err := net.DialTimeout("tcp", host, time.Second)
		if err != nil {
			closeSockets(hold)
			return nil, errors.New("slowloris_failed")
		}
		if _, err := io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: "+host+"\r\n"); err != nil {
			_ = conn.Close()
			closeSockets(hold)
			return nil, errors.New("slowloris_failed")
		}
		hold = append(hold, conn)
	}
	return hold, nil
}

func closeSockets(conns []net.Conn) {
	for _, c := range conns {
		_ = c.Close()
	}
}

// stall reads a 500-row list only far enough to hold the response open.
func stall(ctx context.Context, c *client) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+rowListPath, nil)
	if err != nil {
		return
	}
	req.Header.Set(keyHeader, c.keyB)
	resp, err := c.hc.Do(req)
	if err != nil {
		return
	}
	buf := make([]byte, 1)
	_, _ = resp.Body.Read(buf)
	<-ctx.Done()
	_ = resp.Body.Close()
}

// measureGZIP reports the cost of one 128 KiB compression at gzip level 1.
func measureGZIP() (int64, []int64, error) {
	payload := make([]byte, gzipPayloadSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	raw := make([]int64, 0, gzipRuns)
	for range gzipRuns {
		t := time.Now()
		zw, err := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		if err != nil {
			return 0, nil, errors.New("gzip_writer")
		}
		if _, err := zw.Write(payload); err != nil {
			return 0, nil, errors.New("gzip_write")
		}
		if err := zw.Close(); err != nil {
			return 0, nil, errors.New("gzip_close")
		}
		raw = append(raw, time.Since(t).Nanoseconds())
	}
	return median(raw), raw, nil
}

// percentile is the nearest-rank percentile of one series' values.
func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	s := slices.Clone(values)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100
	return s[max(rank, 1)-1]
}

// ---- the execution's server ---------------------------------------------------------

// server is one execution's fresh server process: its own process group, a
// minimal environment, and its diagnostics in the execution's own directory.
type server struct {
	cmd *exec.Cmd
	log *os.File
}

func startServer(ctx context.Context, path, dir string, port int) (*server, error) {
	logFile, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, errors.New("log_unavailable")
	}
	cmd := exec.CommandContext(ctx, path, "serve")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"BOXSET_DATA_DIR=" + dir,
		"BOXSET_PORT=" + strconv.Itoa(port),
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, errors.New("serve_failed")
	}
	return &server{cmd: cmd, log: logFile}, nil
}

// stop ends the server: SIGTERM to its own process group, then its group dies
// with it when the drain does not finish.
func (s *server) stop() {
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(drainTime):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	_ = s.log.Close()
}

// awaitPing is the START series: the first 200 on /ping, polled every
// millisecond from the moment the process started.
func awaitPing(ctx context.Context, port int, started time.Time, limit time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + pingPath
	hc := &http.Client{}
	for {
		resp, err := hc.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return time.Since(started).Nanoseconds(), nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, errors.New("start_cap")
		case <-time.After(startPoll):
		}
	}
}

// awaitReady holds the authenticated series until the lifecycle reports ready:
// while it is starting or recovering every principal request is refused.
func awaitReady(ctx context.Context, port int, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	started := time.Now()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + pingPath
	hc := &http.Client{}
	// The last state /ping named outlives a poll that fails, so a run that reached
	// recovering and then lost its listener still reports recovering.
	var state string
	for {
		resp, err := hc.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			got := resp.Header.Get("X-Boxset-State")
			_ = resp.Body.Close()
			if got != "" {
				state = got
			}
			if got == "ready" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return readyCap(state, time.Since(started), err)
		case <-time.After(startPoll):
		}
	}
}

// readyCap is the ready_cap failure: the word every caller matches, then what the
// last poll saw — the state /ping named, the elapsed milliseconds, and the
// transport's own class — so a slow start, a lifecycle that never left recovering
// and a listener that died no longer read alike. Every field is bounded: the state
// is the /ping enum (a word nobody recognises folds into other, a state never
// observed at all into none) and the class is one of four words, never the error's
// text or a URL.
func readyCap(state string, elapsed time.Duration, transport error) error {
	if state == "" {
		state = "none"
	}
	switch state {
	case "starting", "recovering", "ready", "draining", "none":
	default:
		state = "other"
	}
	class := "status"
	switch {
	case transport == nil:
	case errors.Is(transport, syscall.ECONNREFUSED):
		class = "refused"
	case errors.Is(transport, context.DeadlineExceeded), errors.Is(transport, os.ErrDeadlineExceeded):
		class = "timeout"
	default:
		class = "other"
	}
	return fmt.Errorf("ready_cap state=%s elapsed_ms=%d ping=%s", state, elapsed.Milliseconds(), class)
}

// seriesFailed is one series step's failure: the word every caller matches, then
// what the request saw — the HTTP status (none when no response arrived), the
// API's own reason word clamped to the codes this repository writes, and the
// elapsed milliseconds into the step. No URL, path, port, header, key, body or
// transport text reaches it.
func seriesFailed(word string, status int, reason string, elapsed time.Duration) error {
	statusWord := "none"
	if status > 0 {
		statusWord = strconv.Itoa(status)
	}
	return fmt.Errorf("%s status=%s reason=%s elapsed_ms=%d", word, statusWord, reasonWord(reason), elapsed.Milliseconds())
}

// reasonWord clamps an error response's word to the problem codes this
// repository's API and server write. A proxy's page, a truncated body or a code
// nobody has added yet folds into other rather than reaching a printer verbatim.
func reasonWord(code string) string {
	switch code {
	case "body_invalid", "body_too_large", "cookie_required", "credential_invalid",
		"credentials_invalid", "cross_origin_denied", "cursor_invalid", "cursor_stale",
		"draining", "forbidden", "hash_busy", "host_not_allowed", "internal_error",
		"last_admin", "login_throttled", "patch_empty", "read_cap", "reader_busy",
		"recovering", "revision_conflict", "secret_in_url", "setup_completed",
		"setup_credential_invalid", "setup_throttled", "starting", "unauthenticated",
		"user_not_found", "validation_failed":
		return code
	}
	return "other"
}

// freePort asks the kernel for an unused loopback port. Its ceiling: the port is
// free only until the child binds it, and a child that loses the race fails its
// own series rather than measuring an idle port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ---- the run's own cgroup -------------------------------------------------------------

// cgroupDir is the cgroup the measured processes run in: their own, when the
// container has a private cgroup namespace.
func cgroupDir() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup"
	}
	if _, after, ok := strings.Cut(strings.TrimSpace(string(data)), "::"); ok {
		return filepath.Join("/sys/fs/cgroup", after)
	}
	return "/sys/fs/cgroup"
}

// cgroupCounters reads the run's peak memory and its oom_kill counter: the two
// recorded run-level caps of the HASH series.
func cgroupCounters() (peak, oomKill int64, err error) {
	peak, err = readCgroupInt("memory.peak")
	if err != nil {
		return 0, 0, errors.New("memory_peak_unavailable")
	}
	data, err := os.ReadFile(filepath.Join(cgroupDir(), "memory.events"))
	if err != nil {
		return 0, 0, errors.New("memory_events_unavailable")
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
			if _, err := fmt.Sscanf(rest, "%d", &oomKill); err != nil {
				return 0, 0, errors.New("memory_events_unavailable")
			}
			return peak, oomKill, nil
		}
	}
	return 0, 0, errors.New("memory_events_unavailable")
}

func readCgroupInt(name string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(cgroupDir(), name))
	if err != nil {
		return 0, err
	}
	var v int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

// vmRSS is one process's resident set size, read from its own status file.
func vmRSS(pid int) (int64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			var kib int64
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d", &kib); err != nil {
				return 0, err
			}
			return kib * 1024, nil
		}
	}
	return 0, errors.New("vmrss_missing")
}
