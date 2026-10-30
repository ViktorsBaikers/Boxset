//go:build integration && effectsrollback

package main

// The recovery rehearsal: a genuine parent binary runs on the schema it embeds,
// the current binary migrates that installation through its normal path, the
// newer effects schema refuses the parent, and a stopped snapshot restore brings
// the domain data back under the current installation's own authority.
//
// Everything observed here is read from a closed file through a read-only
// connection; the parent binary's only post-migration use is the refusal
// control. The parent build and its source tree are supplied by the caller
// (gate REG-ROLLBACK), never produced by this test.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

var (
	parentBinary = flag.String("parent-bin", "", "the genuine parent build the refusal control uses")
	parentSource = flag.String("parent-tree", "", "the captured parent source tree the seed fixture builds in")
)

// The rehearsal's own admin and the API key it creates on the parent binary.
const (
	rollbackAdmin   = "user-a"
	rollbackKeyName = "rollback-key"
)

// reconciledTables is the set R a restore reconciles or resets: their rows are
// compared by the current-authority rule, never by whole-row equality.
var reconciledTables = map[string]bool{
	"user": true, "setup": true, "installation": true, "session": true, "api_key": true,
}

func TestEffectsRollbackFromParentSnapshot(t *testing.T) {
	parent, tree := *parentBinary, *parentSource
	requireParentInputs(t, parent, tree)
	t.Logf("ARGV=%q", os.Args)
	t.Logf("PARENT path=%s sha256=%s", parent, sum(t, parent))
	buildSettings(t, "PARENT", parent)
	requireParentChain(t, tree)

	current := buildCurrentBinary(t)
	t.Logf("CURRENT path=%s sha256=%s", current, sum(t, current))
	buildSettings(t, "CURRENT", current)

	seed := buildParentSeed(t, tree)
	t.Logf("SEED path=%s sha256=%s", seed, sum(t, seed))
	buildSettings(t, "SEED", seed)

	dir := t.TempDir()
	layout := install.Layout{Dir: dir}

	// The genuine parent on its own schema: first admin, one API key, then stop.
	server := startBinary(t, parent, dir)
	server.waitReady(t)
	setupAdmin(t, server, rollbackAdmin)
	drain(t, server)
	// The key is issued with the installation stopped, as an owner's would be.
	key := keyToken(t, keyCommand(t, parent, dir, "create --user "+rollbackAdmin+" --name "+rollbackKeyName+" --scope admin"))
	keyID := keyIDFrom(t, dir, rollbackKeyName)
	if keyID == "" {
		t.Fatal("the parent binary created no key row")
	}
	t.Logf("PARENT-BASELINE admin=%s key=%s ready=1", rollbackAdmin, keyID)

	// The parent-era seed: the S2 fixture world plus one active and one staging
	// alias generation.
	t.Logf("PARENT-SEED %s", oneLine(runSeed(t, seed, layout.DB())))
	o := observe(t, layout.DB(), "O")
	domainTables(t, o)
	if got, want := len(o.rows["title"]), fixtureTitles(t); got != want {
		t.Fatalf("the parent-era world holds %d titles, the fixture expects %d", got, want)
	}
	active := generationInState(t, o, "active")
	staged := generationInState(t, o, "staging")
	if active.aliases < 1 || staged.aliases < 1 {
		t.Fatalf("parent generations = %+v %+v, want the active and the staging one to hold aliases", active, staged)
	}

	// The current binary publishes the pre-migration snapshot and migrates.
	server = start(t, dir)
	server.waitReady(t)
	published := preMigrationSnapshots(t, dir)
	if len(published) != 1 {
		t.Fatalf("the current binary published %d pre-migration snapshots (%v), want exactly one", len(published), published)
	}
	snapshot := published[0]
	s := observe(t, snapshot, "S")
	requireChain(t, "S", s, o)
	requireDomain(t, "S", s, o)

	tSession, status := signIn(t, server.port, rollbackAdmin)
	if status != http.StatusCreated {
		t.Fatalf("the admin signs in on the migrated installation: %d", status)
	}
	// A restore beside a running server is refused and changes nothing.
	backups := dirNames(layout.Backups())
	out, code := restoreCLI(t, dir, snapshot)
	if code != 3 || !strings.Contains(out, "lock_held") {
		t.Fatalf("a restore beside the running server: exit %d, want 3 lock_held: %s", code, oneLine(out))
	}
	requireNoRestoreLeftovers(t, layout)
	if got := dirNames(layout.Backups()); !slices.Equal(got, backups) {
		t.Fatalf("the refused restore changed the backups listing: %v, was %v", got, backups)
	}
	t.Logf("LOCK-HELD restore exit=%d reason=lock_held journal=absent recovery=absent backups=%d", code, len(backups))
	drain(t, server)

	// The current binary activates the staging generation on the stopped file:
	// the retire of the active one, the alias purge intent and its job commit
	// together, and the next start finishes the purge.
	activateStaged(t, layout.DB(), staged.id)
	server = start(t, dir)
	server.waitReady(t)
	awaitPurgeWithin(t, dir, active.id, 30*time.Second)
	if status, body := get(t, tSession, server.port, "/api/v1/system/status", ""); status != http.StatusOK {
		t.Fatalf("the session from before the restore: %d %s", status, body)
	}
	if status, body := get(t, client(t), server.port, "/api/v1/system/status", key); status != http.StatusOK {
		t.Fatalf("the key from before the restore: %d %s", status, body)
	}
	t.Logf("PRE-RESTORE session=status-ok key=status-ok purge=succeeded")
	drain(t, server)

	// Revoke the key while stopped, so the restore must deny it by epoch alone.
	if _, exit := keyCLI(t, dir, "revoke --id "+keyID); exit != 0 {
		t.Fatalf("revoke %s: exit %d", rollbackKeyName, exit)
	}
	p := observe(t, layout.DB(), "P")
	domainTables(t, p)
	pSum, sSum := sum(t, layout.DB()), sum(t, snapshot)

	// The parent binary's only post-migration use: it refuses the newer schema
	// without changing the database or the snapshot.
	parentRefusals(t, parent, dir, snapshot, pSum, sSum, backups)

	// The current binary restores the snapshot while stopped, under the landed
	// policy, and publishes the pre-migration snapshot of the restored file.
	out, code = restoreCLI(t, dir, snapshot)
	if code != 0 {
		t.Fatalf("the current restore from the snapshot: exit %d: %s", code, oneLine(out))
	}
	epoch, recovery, ok := restored(out)
	if !ok {
		t.Fatalf("restore output: %s", oneLine(out))
	}
	if want := max(p.epoch, s.epoch) + 1; epoch != want {
		t.Fatalf("restore epoch = %d, want max(P, S)+1 = %d", epoch, want)
	}
	server = start(t, dir)
	server.waitReady(t)
	drain(t, server)

	// Q: the restored installation before any credential control.
	q := observe(t, layout.DB(), "Q")
	requireChain(t, "Q", q, p)
	requireObjects(t, "Q", q, p)
	requireDomain(t, "Q", q, o)
	if q.integrity != "ok" || q.foreignKeys != 0 {
		t.Fatalf("the restored file: integrity_check %q, foreign_key_check %d violations", q.integrity, q.foreignKeys)
	}
	if q.purgeJobs != 0 {
		t.Fatalf("the restored file holds %d alias.purge job rows, want none", q.purgeJobs)
	}
	if got := sum(t, snapshot); got != sSum {
		t.Fatalf("the snapshot changed: %s, was %s", got, sSum)
	}
	t.Logf("Q restored_file_sha256=%s recorded_only=1", sum(t, layout.DB()))

	// The recovery copy keeps the replaced database: the activation and its
	// accepted job record survive there.
	recovered := observe(t, recovery, "RECOVERY")
	requireChain(t, "RECOVERY", recovered, p)
	requireObjects(t, "RECOVERY", recovered, p)
	requireAllRows(t, "RECOVERY", recovered, p)
	if recovered.integrity != "ok" || recovered.foreignKeys != 0 {
		t.Fatalf("the recovery copy: integrity_check %q, foreign_key_check %d violations", recovered.integrity, recovered.foreignKeys)
	}
	t.Logf("RECOVERY path=%s equals_P=1", recovery)

	requireReconciled(t, p, s, q, epoch)

	// A revoked key and a pre-restore session are denied; a fresh admin signs in
	// and reaches the status and the users list.
	server = start(t, dir)
	server.waitReady(t)
	if status, _ := get(t, client(t), server.port, "/api/v1/system/status", key); status != http.StatusUnauthorized {
		t.Fatalf("the revoked key after the restore: %d, want 401", status)
	}
	if status, _ := get(t, tSession, server.port, "/api/v1/system/status", ""); status != http.StatusUnauthorized {
		t.Fatalf("the session from before the restore: %d, want 401", status)
	}
	fresh, status := signIn(t, server.port, rollbackAdmin)
	if status != http.StatusCreated {
		t.Fatalf("the first admin signs in after the restore: %d", status)
	}
	status, body := get(t, fresh, server.port, "/api/v1/system/status", "")
	if status != http.StatusOK {
		t.Fatalf("the first admin reaches the status: %d %s", status, body)
	}
	var envelope struct {
		RestoreEpoch int64 `json:"restoreEpoch"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if envelope.RestoreEpoch != epoch {
		t.Fatalf("status restoreEpoch = %d, want the restored epoch %d", envelope.RestoreEpoch, epoch)
	}
	if status, body := get(t, fresh, server.port, "/api/v1/users", ""); status != http.StatusOK {
		t.Fatalf("the first admin reaches the users list: %d %s", status, body)
	}
	t.Logf("AUTH revoked_key=401 session=401 fresh_admin=status:users epoch=%d", envelope.RestoreEpoch)
	drain(t, server)
}

// requireParentInputs fails the rehearsal when the gate did not build the
// parent: a missing input is never a skip.
func requireParentInputs(t *testing.T, parent, tree string) {
	t.Helper()
	if parent == "" || tree == "" {
		t.Fatalf("parent_build_required: -parent-bin=%q and -parent-tree=%q must name the captured parent build and its source tree", parent, tree)
	}
	info, err := os.Stat(parent)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("parent_build_required: %s is not an executable file: %v", parent, err)
	}
	if info, err := os.Stat(tree); err != nil || !info.IsDir() {
		t.Fatalf("parent_build_required: %s is not a source tree: %v", tree, err)
	}
}

// buildCurrentBinary builds the current command with the parent's build flags.
// The package's shared TestMain build carries no -trimpath, and this rehearsal
// compares both binaries' build settings, so it runs the one built like the
// parent. Only this tag selects the file, so the other integration tests never
// see the replaced binary.
func buildCurrentBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "boxset")
	runTool(t, "", []string{"CGO_ENABLED=0"}, "go", "build", "-trimpath", "-o", out, ".")
	binary = out
	return out
}

// buildParentSeed copies the seed source into the captured parent tree and
// builds it there, so it links the parent's own store and library.
func buildParentSeed(t *testing.T, tree string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("testdata", "parentseed", "main.go"))
	if err != nil {
		t.Fatalf("read the seed source: %v", err)
	}
	dir := filepath.Join(tree, "cmd", "boxset", "testdata", "parentseed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), source, 0o600); err != nil {
		t.Fatalf("write the seed source: %v", err)
	}
	out := filepath.Join(t.TempDir(), "parentseed")
	runTool(t, tree, []string{"CGO_ENABLED=0"}, "go", "build", "-trimpath", "-o", out, "./cmd/boxset/testdata/parentseed")
	return out
}

// buildSettings records one binary's build information and requires the flags
// the gate pins: the running toolchain, CGO_ENABLED=0, -trimpath and no tags.
func buildSettings(t *testing.T, label, path string) {
	t.Helper()
	out := runTool(t, "", nil, "go", "version", "-m", path)
	first, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[len(fields)-1] != runtime.Version() {
		t.Fatalf("%s build toolchain = %q, want %s", label, first, runtime.Version())
	}
	for _, setting := range []string{"\tbuild\tCGO_ENABLED=0", "\tbuild\t-trimpath=true", "\tbuild\tGOOS=linux"} {
		if !strings.Contains(out, setting) {
			t.Fatalf("%s build info lacks %q:\n%s", label, strings.TrimSpace(setting), out)
		}
	}
	if strings.Contains(out, "\tbuild\t-tags=") {
		t.Fatalf("%s build info carries -tags:\n%s", label, out)
	}
	t.Logf("%s build=%s", label, oneLine(out))
}

// requireParentChain proves the captured tree is the parent: its migrations stop
// before the effects schema.
func requireParentChain(t *testing.T, tree string) {
	t.Helper()
	dir := filepath.Join(tree, "internal", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the parent migrations: %v", err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "00008") || strings.HasPrefix(name, "00009") {
			t.Fatalf("the parent tree holds the effects migration %s", name)
		}
		if strings.HasSuffix(name, ".sql") {
			names = append(names, name)
		}
	}
	if len(names) != 7 {
		t.Fatalf("the parent tree holds %d migrations (%v), want the parent chain of 7", len(names), names)
	}
	t.Logf("PARENT-TREE migrations=%s", strings.Join(names, ","))
}

// fixtureTitles is the title count the S2 fixture itself expects.
func fixtureTitles(t *testing.T) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "s2-fixture.json"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	var fx struct {
		Expect struct {
			Titles int `json:"titles"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(body, &fx); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	return fx.Expect.Titles
}

// runSeed applies the S2 fixture world to the stopped parent-era installation.
func runSeed(t *testing.T, seed, database string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "store", "s2-fixture.json"))
	if err != nil {
		t.Fatalf("locate the fixture: %v", err)
	}
	return runTool(t, "", nil, seed, "-db", database, "-fixture", fixture)
}

// activateStaged activates one staging alias generation on the stopped file
// through the current library, which commits the retire and the purge intent
// together.
func activateStaged(t *testing.T, database string, id int64) {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, database, store.Options{})
	if err != nil {
		t.Fatalf("open the stopped installation: %v", err)
	}
	if err := library.New(db).ActivateGeneration(ctx, library.GenerationID(id)); err != nil {
		_ = db.Close()
		t.Fatalf("activate generation %d: %v", id, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the stopped installation: %v", err)
	}
}

// awaitPurgeWithin waits for the retired generation's aliases to be gone and
// its required job to have succeeded.
func awaitPurgeWithin(t *testing.T, dir string, generation int64, bound time.Duration) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if aliasCount(t, dir, generation) == 0 {
			if row := jobRow(t, dir); row.state == "succeeded" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the purge of generation %d did not succeed within %s: %+v, aliases left %d",
		generation, bound, jobRow(t, dir), aliasCount(t, dir, generation))
}

// parentRefusals runs the parent binary against the newer schema: its serve on
// the installation and its restore of the snapshot. The restore runs on a copy
// of the installation because a refusal is still a started restore: it journals
// and copies before the schema probe refuses, and the installation must stay as
// the rehearsal left it.
func parentRefusals(t *testing.T, parent, dir, snapshot, pSum, sSum string, backups []string) {
	t.Helper()
	layout := install.Layout{Dir: dir}

	server := startBinary(t, parent, dir)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if server.ping() == "ready" {
			t.Fatal("the parent binary served a database written by a newer binary")
		}
		select {
		case <-server.done:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the parent binary did not exit within 60 s")
			}
			continue
		}
		break
	}
	code := server.exited(t)
	out := server.stderr.String()
	if code == 0 {
		t.Fatalf("the parent binary served the newer schema: exit 0: %s", oneLine(out))
	}
	requireRefusal(t, out, "store_unavailable")
	t.Logf("PARENT-REFUSAL serve exit=%d reason=store_unavailable ready=never", code)

	copied := copyDataDir(t, dir)
	copiedSum := sum(t, filepath.Join(copied, "boxset.db"))
	out, code = runCommand(t, parent, copied, "restore", "--from", snapshot)
	if code != 2 {
		t.Fatalf("the parent restore from the snapshot over the effects database: exit %d, want 2: %s", code, oneLine(out))
	}
	requireRefusal(t, out, "snapshot_newer")
	if got := sum(t, filepath.Join(copied, "boxset.db")); got != copiedSum {
		t.Fatalf("the refused parent restore changed the database: %s, was %s", got, copiedSum)
	}
	t.Logf("PARENT-REFUSAL restore exit=%d reason=snapshot_newer copy_db_unchanged=1", code)

	if got := sum(t, layout.DB()); got != pSum {
		t.Fatalf("the parent controls changed the database: %s, was %s", got, pSum)
	}
	if got := sum(t, snapshot); got != sSum {
		t.Fatalf("the parent controls changed the snapshot: %s, was %s", got, sSum)
	}
	if got := dirNames(layout.Backups()); !slices.Equal(got, backups) {
		t.Fatalf("the parent controls changed the backups listing: %v, was %v", got, backups)
	}
	requireNoRestoreLeftovers(t, layout)
}

// requireRefusal requires the landed newer-schema reason in one command's output.
func requireRefusal(t *testing.T, out, reason string) {
	t.Helper()
	if !strings.Contains(out, reason) || !strings.Contains(out, "newer version") {
		t.Fatalf("refusal = %s, want %s and the newer-schema reason", oneLine(out), reason)
	}
}

// requireNoRestoreLeftovers requires that no restore journal and no recovery
// copy is present.
func requireNoRestoreLeftovers(t *testing.T, layout install.Layout) {
	t.Helper()
	for _, path := range []string{layout.Journal(), layout.RestoreStaged()} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("the installation holds %s", path)
		}
	}
	if names := dirNames(layout.RecoveryDir()); len(names) != 0 {
		t.Fatalf("the installation holds recovery copies: %v", names)
	}
}

// copyDataDir copies the installation's database to a fresh directory, so a
// control that expects a refusal never risks the installation the rehearsal
// still needs.
func copyDataDir(t *testing.T, dir string) string {
	t.Helper()
	out := t.TempDir()
	vacuumCopy(t, install.Layout{Dir: dir}.DB(), filepath.Join(out, "boxset.db"))
	return out
}

// startBinary runs one built binary as `serve` on its own port.
func startBinary(t *testing.T, bin, dir string) *proc {
	t.Helper()
	server := &proc{port: freePort(t), done: make(chan struct{})}
	server.cmd = exec.Command(bin, "serve")
	server.cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=" + server.port}
	server.cmd.Stderr = &server.stderr
	if err := server.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	go func() { server.err = server.cmd.Wait(); close(server.done) }()
	t.Cleanup(func() {
		_ = server.cmd.Process.Kill()
		<-server.done
	})
	return server
}

// keyCommand runs one `api-key` subcommand under a terminal, because the command
// prints a token nowhere else.
func keyCommand(t *testing.T, bin, dir, arguments string) []byte {
	t.Helper()
	utility, err := exec.LookPath("script")
	if err != nil {
		t.Fatalf("script is required for the terminal: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, utility, "-q", "-e", "-c", bin+" api-key "+arguments, "/dev/null")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 2 * time.Second
	command.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=dumb"}
	var output keyOutput
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		t.Fatalf("api-key %s: %v: %s", arguments, err, output.String())
	}
	return output.Bytes()
}

// runCommand runs one command of a binary against a stopped installation.
func runCommand(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bin, args...)
	command.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin"}
	var out bytes.Buffer
	command.Stdout, command.Stderr = &out, &out
	err := command.Run()
	return out.String(), exitCodeOf(t, err)
}

// runTool runs one tool to completion and returns its merged output. It
// fails the test on a nonzero exit.
func runTool(t *testing.T, dir string, extra []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), extra...)
	var out bytes.Buffer
	command.Stdout, command.Stderr = &out, &out
	if err := command.Run(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, oneLine(out.String()))
	}
	return out.String()
}

// restored parses `restored epoch=N recovery=PATH` from a restore's output.
func restored(out string) (int64, string, bool) {
	match := regexp.MustCompile(`(?m)^restored epoch=(\d+) recovery=(\S+)$`).FindStringSubmatch(out)
	if match == nil {
		return 0, "", false
	}
	epoch, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return epoch, match[2], true
}

// preMigrationSnapshots lists the snapshots the store published, oldest first.
func preMigrationSnapshots(t *testing.T, dir string) []string {
	t.Helper()
	backups := install.Layout{Dir: dir}.Backups()
	var paths []string
	for _, name := range dirNames(backups) {
		if strings.HasPrefix(name, "pre-migration-") && strings.HasSuffix(name, ".db") {
			paths = append(paths, filepath.Join(backups, name))
		}
	}
	return paths
}

// dirNames lists one directory, sorted; a missing directory lists nothing.
func dirNames(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// oneLine joins the non-empty lines of one command's output for a log line.
func oneLine(out string) string {
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " | ")
}

// settle makes one closed file self-contained: committed frames a clean stop
// left in its write-ahead log are checkpointed into the file, so the read-only
// immutable observation sees everything the stop left and the file's bytes do
// not move under a later reader. A file published by VACUUM INTO has no log and
// is untouched.
func settle(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("stat %s-wal: %v", path, err)
	}
	if info.Size() == 0 {
		return
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s to checkpoint: %v", path, err)
	}
	raw.SetMaxOpenConns(1)
	var busy, frames, done int64
	if err := raw.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done); err != nil {
		_ = raw.Close()
		t.Fatalf("%s: wal_checkpoint: %v", path, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("%s: close after checkpoint: %v", path, err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("%s: the write-ahead log still holds %d bytes after a truncating checkpoint (busy=%d frames=%d done=%d)", path, info.Size(), busy, frames, done)
	}
}

// observation is one read-only reading of a closed database file.
type observation struct {
	label       string
	path        string
	sha256      string
	versions    []string
	hashes      []string
	userVersion int64
	objects     []string
	tables      []string
	rows        map[string][]string
	generations []observedGeneration
	users       []observedUser
	setup       string
	collection  int64
	installID   string
	epoch       int64
	integrity   string
	foreignKeys int
	purgeJobs   int64
}

type observedGeneration struct {
	id      int64
	dataset string
	state   string
	aliases int64
	counted int64
}

type observedUser struct {
	id       int64
	username string
	phc      string
	role     string
	disabled int64
	revision int64
}

// observe reads one closed file through a read-only, immutable connection and
// prints its chain, schema objects, domain rows and reconciled-set state as
// digests, so a root run has its transcript.
func observe(t *testing.T, path, label string) *observation {
	t.Helper()
	settle(t, path)
	o := &observation{label: label, path: path, sha256: sum(t, path), rows: make(map[string][]string)}
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatalf("%s: open %s read-only: %v", label, path, err)
	}
	defer func() { _ = raw.Close() }()
	raw.SetMaxOpenConns(1)
	ctx := t.Context()
	if err := raw.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&o.integrity); err != nil {
		t.Fatalf("%s: integrity_check: %v", label, err)
	}
	verdicts, err := raw.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("%s: foreign_key_check: %v", label, err)
	}
	for verdicts.Next() {
		o.foreignKeys++
	}
	if err := errors.Join(verdicts.Err(), verdicts.Close()); err != nil {
		t.Fatalf("%s: foreign_key_check: %v", label, err)
	}
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&o.userVersion); err != nil {
		t.Fatalf("%s: user_version: %v", label, err)
	}
	o.versions = stringsOf(t, raw, "SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY id")
	o.hashes = stringsOf(t, raw, "SELECT version || ':' || sha256 FROM boxset_schema_hash ORDER BY version")
	o.objects = rowsOf(t, raw, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY type, name")
	o.tables = stringsOf(t, raw, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	for _, table := range o.tables {
		switch table {
		case "goose_db_version", "boxset_schema_hash":
			continue
		}
		// The collection-revision row of the users collection is reconciled; the
		// table's other rows stay in the domain set.
		query := `SELECT * FROM "` + table + `"`
		if table == "collection_revision" {
			query += " WHERE name <> 'users'"
		}
		o.rows[table] = rowsOf(t, raw, query)
	}
	if _, ok := o.rows["alias_generation"]; ok {
		o.generations = readGenerations(t, raw)
	}
	if _, ok := o.rows["user"]; ok {
		o.users = readUsers(t, raw)
	}
	if _, ok := o.rows["setup"]; ok {
		if rows := stringsOf(t, raw, "SELECT token_sha256 || ':' || coalesce(consumed_at, 0) FROM setup WHERE id = 1"); len(rows) == 1 {
			o.setup = rows[0]
		}
	}
	o.collection = int64Of(t, raw, "SELECT revision FROM collection_revision WHERE name = 'users'")
	if ids := stringsOf(t, raw, "SELECT install_id FROM installation WHERE id = 1"); len(ids) == 1 {
		o.installID = ids[0]
	}
	o.epoch = int64Of(t, raw, "SELECT restore_epoch FROM installation WHERE id = 1")
	if slices.Contains(o.tables, "job") {
		o.purgeJobs = int64Of(t, raw, "SELECT count(*) FROM job WHERE kind = 'alias.purge'")
	}
	t.Logf("%s path=%s sha256=%s integrity=%s foreign_keys=%d", label, path, o.sha256, o.integrity, o.foreignKeys)
	t.Logf("%s chain versions=%s hashes=%d user_version=%d digest=%s",
		label, strings.Join(o.versions, ","), len(o.hashes), o.userVersion, digest(o.versions, o.hashes))
	t.Logf("%s objects=%d digest=%s", label, len(o.objects), digest(o.objects))
	for _, table := range o.tables {
		if rows, ok := o.rows[table]; ok {
			t.Logf("%s table=%s rows=%d digest=%s", label, table, len(rows), digest(rows))
		}
	}
	t.Logf("%s domain=%s digest=%s", label, strings.Join(domainOf(t, o), ","), digest(domainRows(t, o)))
	for _, g := range o.generations {
		t.Logf("%s generation=%s id=%d state=%s aliases=%d counted=%d", label, g.dataset, g.id, g.state, g.aliases, g.counted)
	}
	for _, u := range o.users {
		t.Logf("%s user=%s id=%d role=%s disabled=%d revision=%d phc=%s", label, u.username, u.id, u.role, u.disabled, u.revision, u.phc)
	}
	t.Logf("%s setup=%s collection=%d install=%s epoch=%d purge_jobs=%d", label, o.setup, o.collection, o.installID, o.epoch, o.purgeJobs)
	return o
}

// readGenerations reads every alias generation with the alias count the schema
// declares and the count its alias rows prove.
func readGenerations(t *testing.T, raw *sql.DB) []observedGeneration {
	t.Helper()
	rows, err := raw.QueryContext(t.Context(), `SELECT g.id, g.dataset, g.state, g.alias_count,
		(SELECT count(*) FROM title_alias a WHERE a.generation_id = g.id) FROM alias_generation g ORDER BY g.id`)
	if err != nil {
		t.Fatalf("read the alias generations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []observedGeneration
	for rows.Next() {
		var g observedGeneration
		if err := rows.Scan(&g.id, &g.dataset, &g.state, &g.aliases, &g.counted); err != nil {
			t.Fatalf("read an alias generation: %v", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the alias generations: %v", err)
	}
	return out
}

// readUsers reads the account rows a restore reconciles.
func readUsers(t *testing.T, raw *sql.DB) []observedUser {
	t.Helper()
	rows, err := raw.QueryContext(t.Context(), "SELECT id, username, password_phc, role, disabled, revision FROM user ORDER BY id")
	if err != nil {
		t.Fatalf("read the accounts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []observedUser
	for rows.Next() {
		var u observedUser
		if err := rows.Scan(&u.id, &u.username, &u.phc, &u.role, &u.disabled, &u.revision); err != nil {
			t.Fatalf("read an account: %v", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the accounts: %v", err)
	}
	return out
}

// generationInState is the one observed generation in state.
func generationInState(t *testing.T, o *observation, state string) observedGeneration {
	t.Helper()
	for _, g := range o.generations {
		if g.state == state {
			return g
		}
	}
	t.Fatalf("%s holds no %s alias generation", o.label, state)
	return observedGeneration{}
}

// stringsOf reads one text column.
func stringsOf(t *testing.T, raw *sql.DB, query string) []string {
	t.Helper()
	rows, err := raw.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// rowsOf reads every row of one query as a sorted JSON array, so a table's rows
// compare without a schema-specific scan.
func rowsOf(t *testing.T, raw *sql.DB, query string) []string {
	t.Helper()
	rows, err := raw.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		line, err := json.Marshal(values)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		out = append(out, string(line))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	slices.Sort(out)
	return out
}

// int64Of reads one integer, 0 when the query names no row.
func int64Of(t *testing.T, raw *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	err := raw.QueryRowContext(t.Context(), query).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// domainOf is the domain set D of one observation: every table of the file
// except the migration chain and the reconciled set R. Every parent-era library
// table is required to be in it, so a narrowed domain set reddens.
func domainOf(t *testing.T, o *observation) []string {
	t.Helper()
	var domain []string
	for _, table := range o.tables {
		if _, observed := o.rows[table]; !observed || reconciledTables[table] {
			continue
		}
		domain = append(domain, table)
	}
	slices.Sort(domain)
	return domain
}

// domainRows is D's rows as one sorted list, table by table.
func domainRows(t *testing.T, o *observation) []string {
	t.Helper()
	var out []string
	for _, table := range domainOf(t, o) {
		for _, row := range o.rows[table] {
			out = append(out, table+"\t"+row)
		}
	}
	slices.Sort(out)
	return out
}

// domainTables owns the parent-era domain contract: D covers every table the
// parent's first migration creates.
func domainTables(t *testing.T, o *observation) []string {
	t.Helper()
	migration, err := os.ReadFile(filepath.Join("..", "..", "internal", "store", "migrations", "00001_library.sql"))
	if err != nil {
		t.Fatalf("read the parent library migration: %v", err)
	}
	domain := domainOf(t, o)
	for _, match := range regexp.MustCompile(`CREATE (?:VIRTUAL )?TABLE (\w+)`).FindAllStringSubmatch(string(migration), -1) {
		// The hash record is the chain's, never the domain's.
		if match[1] == "boxset_schema_hash" {
			continue
		}
		if !slices.Contains(domain, match[1]) {
			t.Fatalf("the domain set omits %s, a parent library table", match[1])
		}
	}
	return domain
}

// digest is one stable digest of rows, for the transcript.
func digest(lists ...[]string) string {
	hash := sha256.New()
	for _, list := range lists {
		for _, line := range list {
			_, _ = hash.Write([]byte(line))
			_, _ = hash.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// requireChain requires two observations to hold the same migration chain.
func requireChain(t *testing.T, label string, got, want *observation) {
	t.Helper()
	if !slices.Equal(got.versions, want.versions) {
		t.Fatalf("%s chain versions = %v, want %s's %v", label, got.versions, want.label, want.versions)
	}
	if !slices.Equal(got.hashes, want.hashes) {
		t.Fatalf("%s chain hashes differ from %s's", label, want.label)
	}
	if got.userVersion != want.userVersion {
		t.Fatalf("%s user_version = %d, want %s's %d", label, got.userVersion, want.label, want.userVersion)
	}
	t.Logf("%s chain equals %s", label, want.label)
}

// requireObjects requires two observations to hold the same schema objects.
func requireObjects(t *testing.T, label string, got, want *observation) {
	t.Helper()
	requireLines(t, label+" objects", got.objects, want.objects)
	t.Logf("%s objects equal %s", label, want.label)
}

// requireDomain requires two observations to hold the same domain rows.
func requireDomain(t *testing.T, label string, got, want *observation) {
	t.Helper()
	for _, table := range domainOf(t, want) {
		requireLines(t, label+" table "+table, got.rows[table], want.rows[table])
	}
	t.Logf("%s domain equals %s tables=%d", label, want.label, len(domainOf(t, want)))
}

// requireAllRows requires every table of one observation to equal another's,
// reconciled rows included: the recovery copy is a copy, not a reconciliation.
func requireAllRows(t *testing.T, label string, got, want *observation) {
	t.Helper()
	if !slices.Equal(got.tables, want.tables) {
		t.Fatalf("%s tables = %v, want %s's %v", label, got.tables, want.label, want.tables)
	}
	for _, table := range want.tables {
		requireLines(t, label+" table "+table, got.rows[table], want.rows[table])
	}
	t.Logf("%s rows equal %s tables=%d", label, want.label, len(want.tables))
}

// requireLines requires two row lists to be equal and names the first
// difference.
func requireLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	for i := range max(len(got), len(want)) {
		have, expect := "", ""
		if i < len(got) {
			have = got[i]
		}
		if i < len(want) {
			expect = want[i]
		}
		if have != expect {
			t.Fatalf("%s: row %d = %s, want %s (got %d rows, want %d)", what, i, have, expect, len(got), len(want))
		}
	}
}

// requireReconciled proves the restored file follows the current-authority
// rule: the pre-restore database P is authority, the snapshot S only supplies
// data, and the epoch the journal recorded invalidates every earlier credential.
func requireReconciled(t *testing.T, p, s, q *observation, epoch int64) {
	t.Helper()
	if q.installID != p.installID {
		t.Fatalf("restored install_id = %s, want %s", q.installID, p.installID)
	}
	if q.epoch != epoch {
		t.Fatalf("restored epoch = %d, want the journal's %d", q.epoch, epoch)
	}
	if q.setup != p.setup {
		t.Fatalf("restored setup = %s, want the consumed side of P %s", q.setup, p.setup)
	}
	restored := make(map[int64]observedUser, len(q.users))
	for _, user := range q.users {
		restored[user.id] = user
	}
	inSnapshot := make(map[int64]int64, len(s.users))
	for _, user := range s.users {
		inSnapshot[user.id] = user.revision
	}
	current := make(map[int64]bool, len(p.users))
	for _, user := range p.users {
		current[user.id] = true
		got, ok := restored[user.id]
		if !ok {
			t.Fatalf("the restored file drops account %d", user.id)
		}
		if got.username != user.username || got.phc != user.phc || got.role != user.role || got.disabled != user.disabled {
			t.Fatalf("restored account %s: username=%s phc=%s role=%s disabled=%d, want P's %s/%s/%s/%d",
				user.username, got.username, got.phc, got.role, got.disabled, user.username, user.phc, user.role, user.disabled)
		}
		if want := max(user.revision, inSnapshot[user.id]) + 1; got.revision != want {
			t.Fatalf("account %s revision = %d, want max(P, S)+1 = %d", user.username, got.revision, want)
		}
		t.Logf("RECONCILED user=%s username=%s phc=%s role=%s disabled=%d revision=%d", user.username, got.username, got.phc, got.role, got.disabled, got.revision)
	}
	for _, user := range s.users {
		if current[user.id] {
			continue
		}
		got := restored[user.id]
		if got.disabled != 1 {
			t.Fatalf("account %d, absent from P, is not disabled in the restored file", user.id)
		}
		if want := max(user.revision, 1) + 1; got.revision != want {
			t.Fatalf("account %d revision = %d, want %d", user.id, got.revision, want)
		}
	}
	if want := max(p.collection, s.collection) + 1; q.collection < want {
		t.Fatalf("users collection revision = %d, want at least max(P, S)+1 = %d", q.collection, want)
	}
	t.Logf("RECONCILED setup=%s install=%s epoch=%d collection=%d (P epoch=%d collection=%d, S epoch=%d collection=%d)",
		q.setup, q.installID, q.epoch, q.collection, p.epoch, p.collection, s.epoch, s.collection)
}
