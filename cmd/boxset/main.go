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
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

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

// Exit codes: 2 is a usage or configuration error, 3 a startup refusal that
// names its reason on stderr, 1 an unhealthy answer from `boxset healthcheck`.
const (
	exitUnready = 1
	exitUsage   = 2
	exitRefused = 3
)

// drainTimeout is how long a stop waits for requests in flight.
const drainTimeout = 30 * time.Second

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

// serve starts the server in the order that keeps the data directory safe: the
// data directory is resolved to one absolute filesystem path, then the storage
// check, then the lifetime lock, then a journal an interrupted restore left
// (finished, discarded, or — where the platform cannot complete one — refused
// before anything else is opened), then the web shell (read once from
// BOXSET_URL_BASE; an invalid base or an unusable embedded build refuses here,
// with nothing listening and the store unopened), then the listener (so /ping
// answers while the store opens), then the store, whose pending migration is
// preceded by a verified snapshot. Nothing opens the database before the lock
// is held, and the lock is released only after the store is closed.
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
	log := slog.New(redact.NewHandler(slog.NewTextHandler(stderr, nil)))
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
	lock, err := install.Acquire(dir)
	if errors.Is(err, install.ErrLockHeld) {
		return refuse(stderr, "lock_held", errors.New("another Boxset server is using this data directory; stop it first"))
	}
	if err != nil {
		return refuse(stderr, "lock_failed", err)
	}
	// Released last: only after the store is closed, so no mutation outlives the lock.
	lockSafe := true
	defer func() {
		if !lockSafe {
			return
		}
		if err := lock.Release(); err != nil {
			say(stderr, "boxset: release lock: %v\n", err)
			return
		}
		log.Info("lock released", "reason", "lock_released")
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The server's own log is the rotating file under the data directory, beside
	// stderr: neither file ever holds more than its cap, so a log flood costs a
	// fixed amount of disk and never blocks the drain.
	rotating, err := logs.Open(dir)
	if err != nil {
		return refuse(stderr, "log_unavailable", err)
	}
	defer func() {
		if err := rotating.Close(); err != nil {
			say(stderr, "boxset: log_close_failed\n")
		}
	}()
	log = slog.New(redact.NewHandler(logs.NewRingHandler(slog.NewTextHandler(logSink{file: rotating, console: stderr}, nil))))

	// The crash monitor is this binary as a child: it drains the runtime's crash
	// output and publishes it only once this process has ended.
	stopCrashMonitor, err := startCrashMonitor(dir)
	if err != nil {
		return refuse(stderr, "crash_monitor_unavailable", err)
	}
	defer stopCrashMonitor()

	layout := install.Layout{Dir: dir}
	// A journal an interrupted restore left is finished or discarded before the
	// shell, the listener and the store exist, so neither a migration nor a
	// request ever sees half a restore. Where the platform cannot complete one,
	// its presence refuses the start before anything else is opened instead of
	// serving state nobody reconciled.
	recovering := ""
	switch present, err := journalPresent(layout); {
	case err != nil:
		return refuse(stderr, "restore_journal_unreadable", err)
	case present:
		if _, err := resumeRestore(ctx, layout, restoreOptions(log)); err != nil {
			if errors.Is(err, install.ErrRestoreUnsupported) {
				return refuse(stderr, "restore_unsupported", err)
			}
			log.Warn("restore did not finish", "reason", server.ReasonRestoreInProgress, "error", err)
			recovering = server.ReasonRestoreInProgress
		}
	}

	srv, err := server.New(os.Getenv("BOXSET_URL_BASE"))
	if err != nil {
		return refuse(stderr, "shell_unavailable", err)
	}
	// `boxset healthcheck` reads this file, so it answers while this server runs
	// and never opens the store the lock protects.
	if err := server.WriteRunState(dir, server.Starting); err != nil {
		return refuse(stderr, "run_state_unavailable", err)
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return refuse(stderr, "listen_failed", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	migrationStart := time.Now()
	db, err := openAccessStore(ctx, dir, log)
	if err != nil {
		_ = drain(srv) // the store failure is the reason reported
		if errors.Is(err, install.ErrSnapshotUnsupported) {
			return refuse(stderr, "snapshot_unsupported", err)
		}
		return refuse(stderr, "store_unavailable", err)
	}
	migrationMs := time.Since(migrationStart).Milliseconds()
	backend, err := prepareAccess(ctx, db, dir, envToken, log)
	if err != nil {
		_ = drain(srv)
		code := refuse(stderr, "setup_unavailable", err)
		if errors.Is(err, auth.ErrEnvTokenInvalid) {
			code = exitUsage
		}
		if cerr := closeStore(db); cerr != nil {
			lockSafe = false
			say(stderr, "boxset: close store: %v\n", cerr)
		}
		return code
	}
	// An installation whose keys cannot be opened never becomes ready: nothing
	// authenticates until every record's key and the current key are usable.
	if recovering == "" {
		reason, err := secret.New(db, dir).Check(ctx)
		switch {
		case err != nil:
			log.Warn("installation key check failed", "reason", secret.ReasonMissing, "error", err)
			recovering = secret.ReasonMissing
		case reason != "":
			recovering = reason
		}
	}
	backend.AllowedHosts = allowedHosts
	backend.TrustedProxies = trusted
	backend.LocalBypass = bypass
	backend.Version = version()
	backend.MigrationMs = migrationMs
	backend.Sessions = backend.Setup.Sessions()
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	var workerStop error
	go func() {
		defer close(workerDone)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-workerCtx.Done():
				workerStop = workerCtx.Err()
				return
			case <-timer.C:
				if err := backend.Sessions.FlushMetadata(workerCtx); err != nil && workerCtx.Err() == nil {
					log.Warn("credential metadata flush failed", "reason", "metadata_flush_failed")
				}
			}
		}
	}()
	backend.Recovering = recovering
	srv.Attach(backend)
	if recovering == "" {
		srv.SetState(server.Ready)
		if err := server.WriteRunState(dir, server.Ready); err != nil {
			log.Warn("run state not recorded", "reason", "run_state_unavailable")
		}
	} else {
		log.Warn("installation is recovering: no credential authenticates until it is healed", "reason", recovering)
		srv.SetState(server.Recovering)
		if err := server.WriteRunState(dir, server.Recovering); err != nil {
			log.Warn("run state not recorded", "reason", "run_state_unavailable")
		}
	}

	exit := 0
	select {
	case <-ctx.Done():
	case err := <-served:
		exit = refuse(stderr, "listen_failed", err)
	}
	// The admission snapshot is read before anything is stopped, so a wait already
	// standing behind a live writer is recorded as it was, not as this stop left it.
	admission := db.Admission()
	log.Info("signal received", "reason", "signal_received", "phase", admission.Phase.String(), "waiting", admission.Waiting, "records", admission.Records, "ordinal", admission.Ordinal, "generation", admission.Generation)
	srv.SetState(server.Draining)
	if err := server.WriteRunState(dir, server.Draining); err != nil {
		log.Warn("run state not recorded", "reason", "run_state_unavailable")
	}
	backend.Sessions.StopMetadata()
	cancelWorker()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		say(stderr, "boxset: drain: %v\n", err)
		exit = exitRefused
	}
	select {
	case <-workerDone:
		log.Info("metadata worker stopped", "reason", "worker_terminal", "cause", stopReason(workerStop))
		log.Info("metadata worker joined", "reason", "worker_joined")
	case <-drainCtx.Done():
		// Exit without unwinding the lock while a mutation capability remains.
		os.Exit(refuse(stderr, "metadata_drain_failed", drainCtx.Err()))
	}
	// Close has no context of its own: own the one call here and join it under the
	// same deadline, so a stuck close neither outlives the stop nor unwinds the lock.
	closed := make(chan error, 1)
	go func() { closed <- closeStore(db) }()
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-drainCtx.Done():
		os.Exit(refuse(stderr, "store_close_failed", drainCtx.Err()))
	}
	if closeErr != nil {
		lockSafe = false
		say(stderr, "boxset: close store: %v\n", closeErr)
		exit = exitRefused
	} else {
		log.Info("store closed", "reason", "store_closed")
	}
	return exit
}

// stopReason names the cause a worker or attempt ended with, in the fixed
// vocabulary the shutdown record carries.
func stopReason(err error) string {
	switch {
	case err == nil:
		return "done"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

// prepareAccess arms setup while it is open and reads the facts the status
// route reports. The setup token itself is never logged: the log names only
// the command that prints it.
func prepareAccess(ctx context.Context, db *store.DB, dir, envToken string, log *slog.Logger) (server.Backend, error) {
	b := server.Backend{Setup: auth.NewSetup(db, auth.NewHasher(), dir), Log: log}
	start := time.Now()
	open, err := b.Setup.Arm(ctx, envToken)
	if err != nil {
		return b, err
	}
	if open {
		b.SetupMs = time.Since(start).Milliseconds()
		log.Info("setup is open: create the first admin at /setup with the setup token", "reason", "setup_open", "command", setupHint())
	}
	err = db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		hashes, err := q.ListSchemaHashes(ctx)
		if err != nil {
			return err
		}
		if len(hashes) > 0 {
			b.SchemaVersion = hashes[len(hashes)-1].Version
		}
		inst, err := q.GetInstallation(ctx)
		b.RestoreEpoch = inst.RestoreEpoch
		return err
	})
	return b, err
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

// drain stops the listener and waits up to drainTimeout for requests in flight.
func drain(srv *server.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
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
