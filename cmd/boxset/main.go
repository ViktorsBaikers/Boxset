// Command boxset is the Boxset server binary: `boxset serve` runs the server,
// `boxset setup-token` prints the first-admin setup token to a terminal,
// `boxset api-key` creates or revokes a key while the server is stopped,
// `boxset openapi` prints the native API's OpenAPI document and
// `boxset version` prints the build version.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	// The binary carries the zone database, so a schedule keeps its IANA wall
	// clock in a container that ships no zoneinfo files.
	_ "time/tzdata"

	"github.com/mattn/go-isatty"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/logs"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/server"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// drainTimeout is how long a stop waits for requests in flight; the drain order
// itself belongs to the composition.
const drainTimeout = server.DrainTimeout

// Exit codes: 2 is a usage or configuration error, 3 a startup refusal that
// names its reason on stderr, 1 an unhealthy answer from `boxset healthcheck`.
const (
	exitUnready = 1
	exitUsage   = 2
	exitRefused = 3
)

// Proof seams: a test wraps the real step to hold the process inside it. The
// publisher runs inside the startup migration; the store close is the last
// step before the lock is released.
var (
	publishSnapshot = install.PublishSnapshot
	resumeRestore   = install.Resume
	closeStore      = (*store.DB).Close
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if handled, code := child.Enter(args); handled {
		return code
	}
	if len(args) == 1 && args[0] == "version" {
		say(stdout, "boxset %s\n", version())
		return 0
	}
	if len(args) == 1 && args[0] == "serve" {
		return serve(stderr)
	}
	if len(args) == 1 && args[0] == "setup-token" {
		return setupToken(stdout, stderr)
	}
	if len(args) == 1 && args[0] == "healthcheck" {
		return healthcheck(stdout, stderr)
	}
	if len(args) > 0 && args[0] == "api-key" {
		return apiKey(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "restore" {
		return restore(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "key" && len(args) > 1 && args[1] == "import" {
		return keyImport(args[2:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "secret" && len(args) > 1 && args[1] == "reenter" {
		return secretReenter(args[2:], stdout, stderr)
	}
	if len(args) == 1 && args[0] == "openapi" {
		doc, err := api.Document()
		if err != nil {
			say(stderr, "boxset: openapi: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(doc); err != nil {
			return 1
		}
		return 0
	}
	say(stderr, "usage: boxset serve | boxset setup-token | boxset healthcheck | boxset api-key create|revoke | boxset restore --from FILE | boxset key import FILE | boxset secret reenter --id ID --from FILE | boxset openapi | boxset version\n")
	return exitUsage
}

// dataDir is BOXSET_DATA_DIR (default /config) as one absolute filesystem path.
func dataDir() (string, error) {
	dir := os.Getenv("BOXSET_DATA_DIR")
	if dir == "" {
		dir = "/config"
	}
	// One spelling for the mount check, the lock and the SQLite file name: a
	// value such as "file:/x" is a relative directory, never a SQLite URI.
	return install.ResolveDir(dir)
}

// terminal reports whether f is an interactive terminal.
func terminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// setupToken prints the setup token, and only when both standard input and
// standard output are terminals: the token never reaches a pipe, a file or a
// log. A terminal proves interactivity, not locality; the owner-only token
// file is the boundary.
func setupToken(stdout, stderr io.Writer) int {
	out, ok := stdout.(*os.File)
	if !ok || !terminal(os.Stdin) || !terminal(out) {
		say(stderr, "boxset: setup-token prints only to an interactive terminal; run it with a terminal attached (for example docker exec -it)\n")
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if errors.Is(err, os.ErrNotExist) {
		return refuse(stderr, "setup_token_missing", errors.New("no setup token: start the server first; once the first admin exists setup is closed"))
	}
	if err != nil {
		return refuse(stderr, "setup_token_unreadable", err)
	}
	say(stdout, "%s\n", token)
	return 0
}

// setupHint is the command that fetches the setup token here.
func setupHint() string {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return "docker exec -it <container> /boxset setup-token"
		}
	}
	return "boxset setup-token"
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// say writes a diagnostic. A failed write to stdout or stderr has nowhere
// better to be reported, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func refuse(stderr io.Writer, reason string, err error) int {
	return refuseCode(stderr, exitRefused, reason, err)
}

// refuseCode prints one refusal with its reason and returns exit.
func refuseCode(stderr io.Writer, exit int, reason string, err error) int {
	say(stderr, "boxset: %s: %s\n", reason, redact.String(err.Error()))
	return exit
}

func accessPrefixes(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for item := range strings.SplitSeq(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil {
			return nil, errors.New("access_config_invalid")
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// openAccessStore opens the one store under the signal context. log, when set,
// receives the store's fixed metadata-admission observations.
func openAccessStore(ctx context.Context, dir string, log *slog.Logger) (*store.DB, error) {
	layout := install.Layout{Dir: dir}
	return store.Open(ctx, layout.DB(), store.Options{Log: log, BeforeMigrate: func(ctx context.Context, pending store.Pending) error { return publishSnapshot(ctx, layout, pending) }})
}

func apiKey(args []string, stdout, stderr io.Writer) int {
	out, ok := stdout.(*os.File)
	if !ok || !terminal(os.Stdin) || !terminal(out) {
		say(stderr, "boxset: api-key requires an interactive terminal on stdin and stdout\n")
		return exitUsage
	}
	if len(args) == 0 || (args[0] != "create" && args[0] != "revoke") {
		return exitUsage
	}
	flags := flag.NewFlagSet("api-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var username, name, scope string
	var id int64
	if args[0] == "create" {
		flags.StringVar(&username, "user", "", "")
		flags.StringVar(&name, "name", "", "")
		flags.StringVar(&scope, "scope", "", "")
	} else {
		flags.Int64Var(&id, "id", 0, "")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return exitUsage
	}
	if args[0] == "create" && (username == "" || name == "" || (scope != "read" && scope != "write" && scope != "admin")) {
		return exitUsage
	}
	if args[0] == "revoke" && id < 1 {
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	if err := install.CheckMount(dir); err != nil {
		return refuse(stderr, "mount_check_failed", err)
	}
	lock, err := install.Acquire(dir)
	if errors.Is(err, install.ErrLockHeld) {
		return refuse(stderr, "lock_held", errors.New("stop the running server first"))
	}
	if err != nil {
		return refuse(stderr, "lock_failed", err)
	}
	lockClosed := true
	defer func() {
		if !lockClosed {
			return
		}
		if err := lock.Release(); err != nil {
			say(stderr, "boxset: lock_release_failed\n")
		}
	}()
	// Opened under the signal context, not the drain deadline that bounds a
	// stop: applying a pending migration is ordinary work here, so only an
	// interrupt or a stop request ends it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := openAccessStore(ctx, dir, nil)
	if err != nil {
		return refuse(stderr, "store_unavailable", err)
	}
	exit := 0
	keys := auth.NewKeys(db)
	if args[0] == "create" {
		var token string
		id, token, err = keys.Create(ctx, username, name, scope)
		if err == nil {
			say(stderr, "boxset: key_created id=%d\n", id)
			if _, err = fmt.Fprintf(out, "%s\n", token); err != nil {
				err = errors.New("token print failed after commit; revoke the reported key id")
			}
		}
	} else {
		err = keys.Revoke(ctx, id)
	}
	if err != nil {
		exit = refuse(stderr, "key_command_failed", err)
	}
	if err := closeStore(db); err != nil {
		lockClosed = false
		exit = refuse(stderr, "store_close_failed", err)
	}
	return exit
}

// serve starts the server through the one production composition: it reads the
// environment, refuses a configuration it cannot use, checks the mount and
// hands the rest to server.Start, which owns the data directory from the
// lifetime lock to the ready transition. A signal ends it through
// Serving.Stop, which owns the drain order and releases the lock last. Exit
// codes, diagnostics and the crash monitor stay here.
func serve(stderr io.Writer) int {
	// A child is never a server: the marker the runner puts in a child's whole
	// environment keeps a helper from starting a second lock holder, a scheduler
	// or a listener of its own.
	if os.Getenv(child.Name) != "" {
		return refuseCode(stderr, exitUsage, "child_cannot_serve", errors.New("this process runs as a Boxset child; start the server as the top-level process"))
	}
	if err := child.PrepareManagedOwner(); err != nil {
		return refuse(stderr, "child_owner_unavailable", err)
	}
	trusted, err := accessPrefixes(os.Getenv("BOXSET_TRUSTED_PROXIES"))
	if err != nil {
		say(stderr, "boxset: access_config_invalid: BOXSET_TRUSTED_PROXIES\n")
		return 2
	}
	bypass, err := accessPrefixes(os.Getenv("BOXSET_LOCAL_BYPASS"))
	if err != nil {
		say(stderr, "boxset: access_config_invalid: BOXSET_LOCAL_BYPASS\n")
		return 2
	}
	// Read once and dropped from the environment, so no child can inherit it.
	envToken := os.Getenv("BOXSET_SETUP_TOKEN")
	_ = os.Unsetenv("BOXSET_SETUP_TOKEN")
	var allowedHosts []string
	for h := range strings.SplitSeq(os.Getenv("BOXSET_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allowedHosts = append(allowedHosts, h)
		}
	}
	port := os.Getenv("BOXSET_PORT")
	if port == "" {
		port = "8484"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		say(stderr, "boxset: BOXSET_PORT must be a port number from 1 to 65535, got %q\n", redact.String(port))
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	if err := install.CheckMount(dir); err != nil {
		var refused *install.MountRefusedError
		if errors.As(err, &refused) {
			say(stderr, "boxset: %v\n", refused)
			return exitRefused
		}
		return refuse(stderr, "mount_check_failed", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := server.ServeConfig{
		DataDir:         dir,
		Port:            port,
		URLBase:         os.Getenv("BOXSET_URL_BASE"),
		TrustedProxies:  trusted,
		LocalBypass:     bypass,
		AllowedHosts:    allowedHosts,
		EnvSetupToken:   envToken,
		Version:         version(),
		SetupHint:       setupHint(),
		Log:             serveLog(stderr),
		CrashMonitor:    startCrashMonitor,
		PublishSnapshot: publishSnapshot,
		ResumeRestore:   resumeRestore,
		CloseStore:      func(db *store.DB) error { return closeStore(db) },
	}
	serving, err := server.Start(ctx, cfg)
	if err != nil {
		return serveRefusal(stderr, err)
	}

	exit := 0
	select {
	case <-ctx.Done():
	case err := <-serving.Served():
		exit = refuse(stderr, "listen_failed", err)
	}
	// The admission snapshot is read before anything is stopped, so a wait already
	// standing behind a live writer is recorded as it was, not as this stop left it.
	if serving.Effects != nil && serving.Effects.Log != nil {
		admission := serving.Effects.DB.Admission()
		serving.Effects.Log.Info("signal received", "reason", "signal_received", "phase", admission.Phase.String(), "waiting", admission.Waiting, "records", admission.Records, "ordinal", admission.Ordinal, "generation", admission.Generation)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()
	if err := serving.Stop(drainCtx); err != nil {
		exit = stopFailure(stderr, err, exit)
	}
	return exit
}

// serveLog opens the installation's retained log: the rotating file under the
// data directory, beside stderr. Neither file ever holds more than its cap, so
// a log flood costs a fixed amount of disk and never blocks the drain.
func serveLog(stderr io.Writer) func(dir string) (*slog.Logger, func() error, error) {
	return func(dir string) (*slog.Logger, func() error, error) {
		rotating, err := logs.Open(dir)
		if err != nil {
			return nil, nil, err
		}
		log := slog.New(redact.NewHandler(logs.NewRingHandler(slog.NewTextHandler(logSink{file: rotating, console: stderr}, nil))))
		return log, func() error {
			if err := rotating.Close(); err != nil {
				say(stderr, "boxset: log_close_failed\n")
				return err
			}
			return nil
		}, nil
	}
}

// serveRefusal reports why a start was refused, with the landed reason word and
// exit code. A setup token the environment hands over but auth refuses is a
// configuration error, not a refusal to start.
func serveRefusal(stderr io.Writer, err error) int {
	var refused *server.RefusedError
	if errors.As(err, &refused) {
		if refused.Reason == "setup_unavailable" && errors.Is(refused.Err, auth.ErrEnvTokenInvalid) {
			return refuseCode(stderr, exitUsage, refused.Reason, refused.Err)
		}
		return refuse(stderr, refused.Reason, refused.Err)
	}
	return refuse(stderr, "serve_failed", err)
}

// stopFailure puts one stopped installation's failure into the landed exit code
// and diagnostics. A store that could not be closed, or a credential worker that
// could not be joined, exits without unwinding the lock: a mutation capability
// may still exist.
func stopFailure(stderr io.Writer, err error, exit int) int {
	var stopped *server.StopError
	if !errors.As(err, &stopped) {
		return refuse(stderr, "stop_failed", err)
	}
	switch stopped.Reason {
	case server.ReasonDrain:
		say(stderr, "boxset: drain: %v\n", stopped.Err)
		return exitRefused
	case server.ReasonMetadataDrain:
		os.Exit(refuse(stderr, "metadata_drain_failed", stopped.Err))
	case server.ReasonStoreCloseDeadline:
		os.Exit(refuse(stderr, "store_close_failed", stopped.Err))
	case server.ReasonStoreClose:
		say(stderr, "boxset: close store: %v\n", stopped.Err)
		return exitRefused
	case server.ReasonAttemptsLive:
		say(stderr, "boxset: attempts_live: %v\n", stopped.Err)
		return exitRefused
	}
	return refuse(stderr, "stop_failed", err)
}

// logSink writes the server log to the rotating file and to stderr. The two are
// independent: a file that cannot take a line (already closed, or full) never
// takes the stderr copy with it, so the record of a stop is complete where an
// operator reads it.
type logSink struct{ file, console io.Writer }

func (s logSink) Write(p []byte) (int, error) {
	_, _ = s.file.Write(p)
	_, _ = s.console.Write(p)
	return len(p), nil
}

// startCrashMonitor points the runtime's crash output at a child of this binary
// and returns the stop that closes the pipe and kills it. The monitor writes
// logs/crash.log once this process has ended, so a crash record is never on disk
// while the server is still running. A monitor that cannot start refuses the
// start: a crash nobody could read is worse than an honest refusal.
func startCrashMonitor(dir string) (func(), error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	proc, err := child.StartCrashMonitor(context.Background(), dir, read)
	_ = read.Close() // the child holds its own descriptor
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	if err := debug.SetCrashOutput(write, debug.CrashOptions{}); err != nil {
		proc.Stop()
		_ = write.Close()
		return nil, err
	}
	// The runtime duplicated the descriptor, so this process no longer needs its
	// own copy: the monitor sees EOF when the process dies, crashed or not.
	_ = write.Close()
	return proc.Stop, nil
}

// healthcheck runs `boxset healthcheck`: it reports the lifecycle state this data
// directory last recorded, so a container health check distinguishes a live
// process from a usable service without opening the store or taking the lock.
// Ready exits 0, every other state and an unreadable record exit non-zero.
func healthcheck(stdout, stderr io.Writer) int {
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	state, err := server.ReadRunState(dir)
	if err != nil {
		return refuse(stderr, "healthcheck_unavailable", err)
	}
	say(stdout, "%s\n", state)
	if state == server.Ready {
		return 0
	}
	return exitUnready
}

// journalPresent reports whether the data directory holds a restore journal.
func journalPresent(l install.Layout) (bool, error) {
	_, err := os.Lstat(l.Journal())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// restoreOptions is what a restore and a journal resume need from this binary.
func restoreOptions(log *slog.Logger) install.RestoreOptions {
	return install.RestoreOptions{
		Reconcile: auth.ReconcileRestore,
		Snapshot:  func(ctx context.Context, l install.Layout, p store.Pending) error { return publishSnapshot(ctx, l, p) },
		Log:       log,
	}
}

// restore runs `boxset restore --from FILE`: the stopped installation keeps a
// recovery copy of its current state and takes the snapshot's data, with the
// current installation's own accounts and consumed setup applied on top.
func restore(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var from string
	flags.StringVar(&from, "from", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || from == "" {
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	if err := install.CheckMount(dir); err != nil {
		return refuse(stderr, "mount_check_failed", err)
	}
	log := slog.New(redact.NewHandler(slog.NewTextHandler(stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	recovery, err := install.Restore(ctx, install.Layout{Dir: dir}, from, restoreOptions(log))
	if err != nil {
		return restoreRefusal(stderr, err)
	}
	say(stdout, "restored epoch=%d recovery=%s\n", recovery.Epoch, recovery.RecoveryPath)
	return 0
}

// restoreRefusal names why a restore was refused, in the fixed reason vocabulary.
func restoreRefusal(stderr io.Writer, err error) int {
	switch {
	case errors.Is(err, install.ErrRestoreUnsupported):
		return refuse(stderr, "restore_unsupported", err)
	case errors.Is(err, install.ErrRestoreInProgress):
		return refuse(stderr, "restore_in_progress", err)
	case errors.Is(err, install.ErrSnapshotNewer):
		return refuseCode(stderr, exitUsage, "snapshot_newer", err)
	case errors.Is(err, install.ErrSnapshotInvalid):
		return refuseCode(stderr, exitUsage, "snapshot_invalid", err)
	case errors.Is(err, install.ErrInstallationMismatch):
		return refuseCode(stderr, exitUsage, "installation_mismatch", err)
	case errors.Is(err, install.ErrAuthorizationStateUnavailable):
		return refuseCode(stderr, exitUsage, "authorization_state_unavailable", err)
	case errors.Is(err, install.ErrLockHeld):
		return refuse(stderr, "lock_held", err)
	}
	return refuse(stderr, "restore_failed", err)
}

// keyRefusal names why a key file was refused: a barrier that did not complete is
// its own exit status, because the bytes are published but not yet durable.
func keyRefusal(stderr io.Writer, err error) int {
	var syncErr *install.SecretSyncError
	switch {
	case errors.As(err, &syncErr):
		say(stderr, "boxset: key_sync_failed stage=%s: %s\n", syncErr.Stage, redact.String(syncErr.Err.Error()))
		return 1
	case errors.Is(err, install.ErrSecretFileUnsafe):
		return refuseCode(stderr, exitUsage, "key_unsafe", err)
	case errors.Is(err, install.ErrSecretInput):
		return refuseCode(stderr, exitUsage, "input_invalid", err)
	}
	return refuse(stderr, "key_command_failed", err)
}

// stoppedStore takes the lifetime lock and opens the store of a command that runs
// with no server: a running server is refused, and so is an installation that a
// restore left recovering.
func stoppedStore(ctx context.Context, stderr io.Writer, dir string) (db *store.DB, lock *install.Lock, code int, ok bool) {
	if err := install.CheckMount(dir); err != nil {
		return nil, nil, refuse(stderr, "mount_check_failed", err), false
	}
	lock, err := install.Acquire(dir)
	if errors.Is(err, install.ErrLockHeld) {
		return nil, nil, refuse(stderr, "lock_held", errors.New("stop the running server first")), false
	}
	if err != nil {
		return nil, nil, refuse(stderr, "lock_failed", err), false
	}
	switch present, err := journalPresent(install.Layout{Dir: dir}); {
	case err != nil:
		_ = lock.Release()
		return nil, nil, refuse(stderr, "restore_journal_unreadable", err), false
	case present:
		_ = lock.Release()
		return nil, nil, refuse(stderr, "restore_in_progress", install.ErrRestoreInProgress), false
	}
	db, err = openAccessStore(ctx, dir, nil)
	if err != nil {
		_ = lock.Release()
		if errors.Is(err, install.ErrSnapshotUnsupported) {
			return nil, nil, refuse(stderr, "snapshot_unsupported", err), false
		}
		return nil, nil, refuse(stderr, "store_unavailable", err), false
	}
	return db, lock, 0, true
}

// releaseStopped closes the store and releases the lock of stoppedStore. A store
// that cannot close keeps the lock, so no other process mutates beside it.
func releaseStopped(stderr io.Writer, db *store.DB, lock *install.Lock) {
	if err := closeStore(db); err != nil {
		say(stderr, "boxset: store_close_failed: %s\n", redact.String(err.Error()))
		return
	}
	if err := lock.Release(); err != nil {
		say(stderr, "boxset: lock_release_failed\n")
	}
}

// keyImport runs `boxset key import FILE`: the bytes of an existing key are
// published again at the path its fingerprint names, so a record whose key file
// was lost opens again. Bytes that no record names are refused.
func keyImport(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, lock, code, ok := stoppedStore(ctx, stderr, dir)
	if !ok {
		return code
	}
	defer releaseStopped(stderr, db, lock)
	key, err := install.ReadSecretInput(args[0], 1<<20)
	if err != nil {
		return keyRefusal(stderr, err)
	}
	if len(key) != secret.KeySize {
		return refuseCode(stderr, exitUsage, "input_invalid", errors.New("a key file holds exactly 32 bytes"))
	}
	keyID, err := secret.New(db, dir).Import(ctx, key)
	if errors.Is(err, secret.ErrKeyUnknown) {
		return refuseCode(stderr, exitUsage, "key_unknown", err)
	}
	if err != nil {
		return keyRefusal(stderr, err)
	}
	say(stdout, "imported key=%s\n", keyID)
	return 0
}

// secretReenter runs `boxset secret reenter --id ID --from FILE`: the file's
// bytes become version N+1 of that record under the current key, or under a newly
// published one when the current key cannot be used. Version N and every earlier
// version keep their key, nonce and ciphertext.
func secretReenter(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var from string
	var id int64
	flags.Int64Var(&id, "id", 0, "")
	flags.StringVar(&from, "from", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || from == "" || id < 1 {
		return exitUsage
	}
	dir, err := dataDir()
	if err != nil {
		return refuse(stderr, "data_dir_invalid", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, lock, code, ok := stoppedStore(ctx, stderr, dir)
	if !ok {
		return code
	}
	defer releaseStopped(stderr, db, lock)
	plaintext, err := install.ReadSecretInput(from, 1<<20)
	if err != nil {
		return keyRefusal(stderr, err)
	}
	defer clearBytes(plaintext)
	keys := secret.New(db, dir)
	var current storedb.GetCurrentSecretRow
	err = db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		current, err = storedb.New(tx).GetCurrentSecret(ctx, id)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return refuseCode(stderr, exitUsage, "secret_not_found", errors.New("no current version of that record"))
	}
	if err != nil {
		return refuse(stderr, "secret_reenter_failed", err)
	}
	keyID, key, minted, err := usableKey(ctx, keys)
	if err != nil {
		return keyRefusal(stderr, err)
	}
	nonce, ciphertext, err := secret.Seal(key, id, current.Version+1, current.Purpose, plaintext)
	if err != nil {
		return refuse(stderr, "secret_reenter_failed", err)
	}
	err = db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		if err := q.RetireSecretVersion(ctx, storedb.RetireSecretVersionParams{ID: id, Version: current.Version}); err != nil {
			return err
		}
		if err := q.InsertSecretVersion(ctx, storedb.InsertSecretVersionParams{
			ID:         id,
			Version:    current.Version + 1,
			Purpose:    current.Purpose,
			KeyID:      keyID,
			Nonce:      nonce,
			Ciphertext: ciphertext,
			IsCurrent:  1,
			UpdatedAt:  time.Now().Unix(),
		}); err != nil {
			return err
		}
		if minted {
			return q.SetInstallationKey(ctx, keyID)
		}
		return nil
	})
	if err != nil {
		return refuse(stderr, "secret_reenter_failed", err)
	}
	say(stdout, "reentered id=%d version=%d\n", id, current.Version+1)
	return 0
}

// usableKey returns the current key when its file passes the barrier, and a
// freshly minted and published one otherwise. Only a minted key is ever pointed
// at by the caller, and only after this returned.
func usableKey(ctx context.Context, keys *secret.Store) (keyID string, key []byte, minted bool, err error) {
	if id, ok, err := keys.CurrentKeyID(ctx); err != nil {
		return "", nil, false, err
	} else if ok {
		if key, err := keys.ReadKey(id); err == nil {
			return id, key, false, nil
		}
	}
	keyID, key, err = keys.MintKey()
	if err != nil {
		return "", nil, false, err
	}
	return keyID, key, true, nil
}

// clearBytes overwrites a buffer that held a credential.
func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
