// Command bencheffects measures the admitted start of an interactive job under
// the effects load of one reference-profile run, and judges a recorded series.
//
//	bencheffects -out DIR -order AB|BA [-position 1-7]  measure: writes DIR/samples.jsonl only
//	bencheffects -judge RUN1,...,RUN7       judge: exit 0 PASS, 1 FAIL, 3 INVALID
//	bencheffects -child-hold 256MiB         the resident child the memory position holds
//
// Measuring is diagnostic. The measurement runs this binary's own composition —
// the server.Start, store, jobs runtime and outbound client `cmd/boxset serve`
// starts — so the series measures the product and never a harness-only copy of
// it. Only -judge, which reads the recorded runs through the immutable image,
// may print EFFECTS-BENCH-PASS.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ViktorsBaikers/Boxset/bench/internal/benchprofile"
	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
	"github.com/ViktorsBaikers/Boxset/internal/server"
)

const (
	// inputsManifest is the image's own file manifest. The judge always reads the
	// image's; the measuring path records its hash into the samples.
	inputsManifest = "/usr/local/share/boxset/inputs.sha256"
	maxInputsBytes = 8 << 20

	// childHoldSize and childHoldFor are the resident child's recorded shape: the
	// memory position holds it through one shared slot and reads its VmRSS.
	childHoldSize = "256MiB"
	childHoldFor  = 5 * time.Second
	childTimeout  = 10 * time.Second
)

func main() {
	// A child is never a server, a runner or a judge: the guardian entry returns
	// here before any of the tool's own work happens.
	if handled, code := child.Enter(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bencheffects", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "measure: directory that receives samples.jsonl")
	order := fs.String("order", "", "measure: the order its two A/A arms run in, AB or BA")
	position := fs.Int("position", 0, "measure: the run's place in the seven-position series (1-7); 0 derives it from ORDER and GOMAXPROCS")
	judgeRuns := fs.String("judge", "", "judge: the seven comma-separated run directories")
	hold := fs.String("child-hold", "", "the resident child's size, e.g. 256MiB")
	root := fs.String("root", ".", "repository root")
	inputs := fs.String("inputs", inputsManifest, "measure: image input manifest to hash into the samples (the judge always reads the image's own)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	modes := 0
	for _, set := range []bool{*out != "", *judgeRuns != "", *hold != ""} {
		if set {
			modes++
		}
	}
	if fs.NArg() != 0 || modes != 1 || (*order != "" && *out == "") || (*order != "" && *order != orderAB && *order != orderBA) || (*out != "" && *order == "") ||
		*position < 0 || *position > seriesRuns || (*position != 0 && *out == "") {
		say(stderr, "usage: bencheffects -out DIR -order AB|BA [-position 1-7] | -judge RUN1,...,RUN7 [-root DIR] | -child-hold 256MiB\n")
		return exitUsage
	}
	if *judgeRuns != "" {
		return judge(*root, *judgeRuns, inputsManifest, stdout)
	}
	if *hold != "" {
		if err := childHold(*hold); err != nil {
			say(stderr, "bencheffects: %v\n", err)
			return exitFail
		}
		return exitPass
	}
	return measureTo(*root, *inputs, *out, *order, *position, stdout, stderr)
}

// ---- measure -------------------------------------------------------------------

// measureTo writes one run's samples: the measurement is diagnostic and prints
// only what it did, never a verdict.
func measureTo(root, inputs, out, order string, position int, stdout, stderr io.Writer) int {
	abs, err := filepath.Abs(out)
	if err != nil {
		say(stderr, "bencheffects: out_invalid\n")
		return exitFail
	}
	run := filepath.Base(filepath.Dir(abs))
	if !benchprofile.ValidRunName(run) {
		say(stderr, "bencheffects: run_invalid\n")
		return exitFail
	}
	if position == 0 {
		position = positionOf(0, order, os.Getenv("GOMAXPROCS"))
	}
	set, err := measure(context.Background(), inputs, run, order, position)
	if err != nil {
		say(stderr, "bencheffects: %v\n", err)
		return exitFail
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		say(stderr, "bencheffects: write_failed file=%s\n", samplesName)
		return exitFail
	}
	if err := writeSamples(filepath.Join(out, samplesName), set); err != nil {
		say(stderr, "bencheffects: write_failed file=%s\n", samplesName)
		return exitFail
	}
	say(stdout, "bencheffects: run=%s order=%s position=%d procs=%d span=%s records=%d class=%s digest=%s (diagnostic only; judge decides)\n",
		set.Header.Run, set.Header.Order, position, set.Header.Procs, set.Span, set.Header.Records, set.Header.Class, set.Header.Digest)
	return exitPass
}

// identity is a binary's own build identity, read back from its build info: the
// class comes from the executed build's settings, never from an argument.
type identity struct {
	digest   string
	class    string
	tags     string
	cgo      string
	trimpath bool
	race     string
	cover    string
	goos     string
	goarch   string
	version  string
}

// buildSetting is a recorded go build setting, or "" when the binary has none.
func buildSetting(key string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// buildIdentity reads this binary's digest and settings. A build with no class —
// a race, cover or tagged-but-not-control build — is refused before anything is
// measured, because its numbers answer a different question.
func buildIdentity() (identity, error) {
	self, err := os.Executable()
	if err != nil {
		return identity{}, errors.New("executable_unavailable")
	}
	data, err := readBounded(self, 1<<30)
	if err != nil {
		return identity{}, errors.New("executable_unreadable")
	}
	id := identity{
		digest:   sha256Hex(data),
		tags:     buildSetting("-tags"),
		cgo:      orDefault(buildSetting("CGO_ENABLED"), "unknown"),
		trimpath: buildSetting("-trimpath") == "true",
		race:     orDefault(buildSetting("-race"), "false"),
		cover:    buildSetting("-covermode"),
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
		version:  runtime.Version(),
	}
	h := header{Tags: id.tags, CGO: id.cgo, Trimpath: id.trimpath, Race: id.race, Cover: id.cover}
	if id.class = classOf(h); id.class == "" {
		return identity{}, errors.New("binary_class")
	}
	return id, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// measure runs one whole run: the untimed seed, then the two arms of its order
// inside the timed region. The region's actual span is recorded beside the
// samples, so the harness reports what it took instead of asserting a bound.
func measure(ctx context.Context, inputs, run, order string, pos int) (*sampleSet, error) {
	mount, err := dataDir()
	if err != nil {
		return nil, err
	}
	id, err := buildIdentity()
	if err != nil {
		return nil, err
	}
	manifest, err := readBounded(inputs, maxInputsBytes)
	if err != nil {
		return nil, errors.New("inputs_unavailable")
	}
	dir, err := os.MkdirTemp(mount, "effects-")
	if err != nil {
		return nil, errors.New("data_dir_invalid")
	}
	start := time.Now().UTC()
	// The seed is a routine composition on the same directory the measured one
	// starts on: it creates the accounts, the retired generations and the probe
	// table, and it is stopped before anything is timed.
	world, err := seedWorld(ctx, filepath.Join(dir, "work"), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = world.peer.close() }()
	timed := time.Now()
	set, err := timeWindows(ctx, world, run, order, id, pos)
	if err != nil {
		return nil, err
	}
	set.Span = time.Since(timed)
	set.Header.Inputs = sha256Hex(manifest)
	set.Header.StartDate = start.Format(benchprofile.DateLayout)
	set.Header.EndDate = time.Now().UTC().Format(benchprofile.DateLayout)
	_, set.Header.UTCOffset = start.Zone()
	return set, nil
}

// dataDir is the runner's data mount, and it must be a device of its own: the
// measured storage is never the container's overlay or its tmpfs.
func dataDir() (string, error) {
	return dataDirAt(dataMount)
}

// dataDirAt applies the mount and device rules to mount: the wire records the
// runner's mount, and a test drives the same rules on a directory of its own.
func dataDirAt(mount string) (string, error) {
	dir := os.Getenv("BOXSET_DATA_DIR")
	if dir != mount {
		return "", errors.New("data_dir")
	}
	if err := distinctDevice(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// distinctDevice refuses a path that does not exist or shares the root's or
// /tmp's device: the measured database is never on the container's own fs.
func distinctDevice(dir string) error {
	dev, err := deviceOf(dir)
	if err != nil {
		return errors.New("data_dir_device")
	}
	for _, other := range []string{"/", "/tmp"} {
		d, err := deviceOf(other)
		if err != nil || d == dev {
			return errors.New("data_dir_device")
		}
	}
	return nil
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

// startServing starts one installation through the production composition on
// dir. The harness never opens a store, a runtime or an outbound client of its
// own: everything it measures is reached through the returned Serving.
func startServing(ctx context.Context, dir string, env outbound.Config, port int) (*server.Serving, error) {
	return server.Start(ctx, server.ServeConfig{
		DataDir:      dir,
		Port:         strconv.Itoa(port),
		Version:      "bench-effects",
		SetupHint:    "boxset setup-token",
		Log:          benchLog,
		CrashMonitor: noCrashMonitor,
		Outbound:     env,
	})
}

// benchLog is the composition's log: one redacted warning or failure on the
// harness's own stderr, and nothing quieter — the measured load's per-request
// lines alone would spend the runner's whole output cap. The measurement does
// not keep a second log file, and the shared redactor is the only handler that
// may see a URL or a credential.
func benchLog(string) (*slog.Logger, func() error, error) {
	opts := &slog.HandlerOptions{Level: slog.LevelWarn}
	return slog.New(redact.NewHandler(slog.NewTextHandler(os.Stderr, opts))), func() error { return nil }, nil
}

// noCrashMonitor is the composition's crash recorder in a measured run: the run
// is one process whose whole point is that it ends, so nothing outlives it to
// record.
func noCrashMonitor(string) (func(), error) { return func() {}, nil }

// stopServing drains one composition within the recorded drain bound.
func stopServing(serving *server.Serving) error {
	ctx, cancel := context.WithTimeout(context.Background(), server.DrainTimeout)
	defer cancel()
	return serving.Stop(ctx)
}

// freePort asks the kernel for an unused loopback port. Its ceiling: the port is
// free only until the composition binds it, and a composition that loses the
// race fails its own start rather than measuring an idle port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ---- the resident child ---------------------------------------------------------

// childHold is the position-7 child: it takes the recorded resident size, holds
// every page of it, waits out the recorded hold and exits 0.
func childHold(size string) error {
	bytes, err := parseSize(size)
	if err != nil {
		return err
	}
	held := make([]byte, bytes)
	for i := 0; i < len(held); i += os.Getpagesize() {
		held[i] = 1
	}
	time.Sleep(childHoldFor)
	runtime.KeepAlive(held)
	return nil
}

// parseSize reads the recorded child size: a number of MiB with its unit, e.g.
// 256MiB. A size the protocol does not pin is refused rather than rounded.
func parseSize(size string) (int, error) {
	mib, ok := strings.CutSuffix(size, "MiB")
	if !ok {
		return 0, errors.New("child_size")
	}
	n, err := strconv.Atoi(mib)
	if err != nil || n != 256 {
		return 0, errors.New("child_size")
	}
	return n << 20, nil
}

// ---- the run's own cgroup ---------------------------------------------------------

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

// cgroupCounters reads the run's peak memory and its oom_kill counter: the
// container's own answer for every process, mapping, page of cache and child.
func cgroupCounters() (peak, limit, oomKill int64, err error) {
	peak, err = readCgroupInt("memory.peak")
	if err != nil {
		return 0, 0, 0, errors.New("memory_peak_unavailable")
	}
	limit, err = readCgroupInt("memory.max")
	if err != nil {
		return 0, 0, 0, errors.New("memory_limit_unavailable")
	}
	data, err := os.ReadFile(filepath.Join(cgroupDir(), "memory.events"))
	if err != nil {
		return 0, 0, 0, errors.New("memory_events_unavailable")
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
			if _, err := fmt.Sscanf(rest, "%d", &oomKill); err != nil {
				return 0, 0, 0, errors.New("memory_events_unavailable")
			}
			return peak, limit, oomKill, nil
		}
	}
	return 0, 0, 0, errors.New("memory_events_unavailable")
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

// ---- shared small helpers ---------------------------------------------------------

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
