package library

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Crash proof by re-exec. The parent starts this test binary as a child that
// opens its own store, runs one registered scenario inside a single Write
// closure and, with the transaction still open, reports the row counts it sees
// and sleeps. The parent kills the child at that point and checks the file.
// A process kill proves application-crash recovery only (ADR 0003): it says
// nothing about power loss or a failing disk.

const (
	crashEnvScenario = "BOXSET_CRASH_SCENARIO"
	crashEnvDB       = "BOXSET_CRASH_DB"
	crashEnvArgs     = "BOXSET_CRASH_ARGS"
	crashReady       = "READY "
)

// crashScenario does its writes inside one l.db.Write closure and calls ready
// with that closure's tx once they are made; it must not return before the kill.
type crashScenario func(ctx context.Context, l *Library, args []string, ready func(tx *sql.Tx)) error

var crashScenarios = map[string]crashScenario{
	// enrolls a new title through a library's template and requests an
	// acquisition for every slot the enrollment selected. args: library id.
	"membership-acquisition": func(ctx context.Context, l *Library, args []string, ready func(tx *sql.Tx)) error {
		lib, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		return l.db.Write(ctx, func(tx *sql.Tx) error {
			def, err := loadDefaults(ctx, tx, LibraryID(lib))
			if err != nil {
				return err
			}
			keys, err := templateKeys(ctx, tx, LibraryID(lib))
			if err != nil {
				return err
			}
			m, _, err := enroll(ctx, tx, LibraryID(lib), def, crashMetadataID, "movie", "Crash Movie", keys, OwnerLocal)
			if err != nil {
				return err
			}
			if _, err := requestAcquisition(ctx, tx, AcquisitionRequest{ClientID: "qbit", ReleaseKey: crashRelease, Slots: m.SlotIDs}); err != nil {
				return err
			}
			ready(tx)
			time.Sleep(2 * time.Minute) // the parent kills first; this only bounds an orphan
			return errors.New("crash scenario was not killed")
		})
	},
	// requests one acquisition for a slot that pauses (and so writes a hold) and
	// a slot that attaches. args: paused slot id, eligible slot id.
	"hold-acquisition": func(ctx context.Context, l *Library, args []string, ready func(tx *sql.Tx)) error {
		var slots []SlotID
		for _, a := range args {
			id, err := strconv.ParseInt(a, 10, 64)
			if err != nil {
				return err
			}
			slots = append(slots, SlotID(id))
		}
		return l.db.Write(ctx, func(tx *sql.Tx) error {
			if _, err := requestAcquisition(ctx, tx, AcquisitionRequest{ClientID: "qbit", ReleaseKey: crashRelease, Slots: slots}); err != nil {
				return err
			}
			ready(tx)
			time.Sleep(2 * time.Minute) // the parent kills first; this only bounds an orphan
			return errors.New("crash scenario was not killed")
		})
	},
	// activates a staging generation, which retires the active one and enqueues
	// the required purge of the retired row in the same transaction.
	// args: staging generation id.
	"activate-generation": func(ctx context.Context, l *Library, args []string, ready func(tx *sql.Tx)) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		return l.db.Write(ctx, func(tx *sql.Tx) error {
			if err := activateGeneration(ctx, tx, GenerationID(id)); err != nil {
				return err
			}
			// The intent is in the same open transaction as the retire.
			var intents int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM job`).Scan(&intents); err != nil {
				return err
			}
			if intents != 1 {
				return fmt.Errorf("the activation wrote %d intents, want 1", intents)
			}
			ready(tx)
			time.Sleep(2 * time.Minute) // the parent kills first; this only bounds an orphan
			return errors.New("crash scenario was not killed")
		})
	},
	// changes episode and movie state through the transaction-level slot-state
	// writers, checks the rollups equal the recompute inside the open
	// transaction, and waits. args: series slot id, movie slot id.
	"slot-state": func(ctx context.Context, l *Library, args []string, ready func(tx *sql.Tx)) error {
		series, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		movie, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		return l.db.Write(ctx, func(tx *sql.Tx) error {
			var before int64
			if err := tx.QueryRowContext(ctx, `SELECT sum(revision) FROM title_rollup`).Scan(&before); err != nil {
				return err
			}
			for _, c := range []SlotEpisodeChange{
				{SlotID: SlotID(series), Season: 1, Number: 2, Monitored: true, State: "live", SizeBytes: 77},
				{SlotID: SlotID(series), Season: 2, Number: 1, Monitored: true, State: "on_disk", SizeBytes: 55},
			} {
				if err := setSlotEpisode(ctx, tx, c); err != nil {
					return err
				}
			}
			if err := setMovieSlot(ctx, tx, MovieSlotChange{SlotID: SlotID(movie), State: "on_disk", SizeBytes: 9}); err != nil {
				return err
			}
			var after int64
			if err := tx.QueryRowContext(ctx, `SELECT sum(revision) FROM title_rollup`).Scan(&after); err != nil {
				return err
			}
			if after <= before {
				return errors.New("slot-state scenario moved no rollup")
			}
			if err := oracleCheck(ctx, tx); err != nil {
				return err
			}
			ready(tx)
			time.Sleep(2 * time.Minute) // the parent kills first; this only bounds an orphan
			return errors.New("crash scenario was not killed")
		})
	},
}

const (
	crashMetadataID = "tmdb:777"
	crashRelease    = "Crash.Movie.1080p.WEB-DL-GRP"
)

// TestMain turns the test binary into the crash child when the parent asks.
func TestMain(m *testing.M) {
	if name := os.Getenv(crashEnvScenario); name != "" {
		os.Exit(crashChild(name))
	}
	os.Exit(m.Run())
}

func crashChild(name string) int {
	ctx := context.Background()
	sc, ok := crashScenarios[name]
	if !ok {
		fmt.Fprint(os.Stderr, "unknown crash scenario "+name+"\n")
		return 2
	}
	db, err := store.Open(ctx, os.Getenv(crashEnvDB), store.Options{})
	if err != nil {
		fmt.Fprint(os.Stderr, "child store.Open: "+err.Error()+"\n")
		return 2
	}
	ready := func(tx *sql.Tx) {
		counts, err := countTables(tx, acqTables)
		if err != nil {
			fmt.Fprint(os.Stderr, "child count: "+err.Error()+"\n")
			os.Exit(2)
		}
		line, _ := json.Marshal(counts)
		if _, err := os.Stdout.WriteString(crashReady + string(line) + "\n"); err != nil {
			fmt.Fprint(os.Stderr, "child ready: "+err.Error()+"\n")
			os.Exit(2)
		}
	}
	err = sc(ctx, New(db), strings.Split(os.Getenv(crashEnvArgs), "\n"), ready)
	fmt.Fprint(os.Stderr, "child scenario ended: "+err.Error()+"\n")
	return 3
}

// killChild runs scenario in a child on the store at path, waits until the
// child reports its transaction open, kills it and returns the row counts the
// child saw inside that transaction.
func killChild(t *testing.T, scenario, path string, args ...string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := crashCommand(ctx, scenario, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("child did not die by signal: %v", waitErr)
	}
	if readErr != nil || !strings.HasPrefix(line, crashReady) {
		t.Fatalf("child never reported an open transaction: line %q err %v stderr %q", line, readErr, stderr.String())
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, crashReady)), &counts); err != nil {
		t.Fatalf("child report %q: %v", line, err)
	}
	return counts
}

// crashCommand is the child of scenario on the store at path, bound to ctx.
// Its environment is closed to the scenario inputs, and it runs in its own
// process group, which the deadline kills.
func crashCommand(ctx context.Context, scenario, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = []string{crashEnvScenario + "=" + scenario, crashEnvDB + "=" + path, crashEnvArgs + "=" + strings.Join(args, "\n")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

// A child that cannot write its READY report must say so on stderr and exit
// with status 2: the parent was never told the transaction is open, and the
// child would otherwise sleep on until the deadline. /dev/full takes the open
// and fails every write.
func TestCrashReadyWriteFailure(t *testing.T) {
	w := applyS2(t)
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("the failing stdout: %v", err)
	}
	t.Cleanup(func() { _ = full.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := crashCommand(ctx, "membership-acquisition", dbPath(t, w.db), strconv.FormatInt(int64(w.libs["Collector"]), 10))
	cmd.Stdout = full
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("child stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	text, readErr := io.ReadAll(io.LimitReader(stderr, 4<<10))
	waitErr := cmd.Wait()
	if readErr != nil {
		t.Fatalf("read child stderr: %v", readErr)
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("the child ignored its failed READY write and was still running at the deadline: %v, stderr %q", waitErr, text)
	case !errors.As(waitErr, &exit) || exit.ExitCode() != 2:
		t.Fatalf("the child after a failed READY write: %v, want exit status 2, stderr %q", waitErr, text)
	}
	diagnostic := strings.ToLower(string(text))
	if !strings.Contains(diagnostic, "ready") || !strings.Contains(diagnostic, syscall.ENOSPC.Error()) {
		t.Fatalf("the child's stderr does not name the failed READY write and its cause: %q", text)
	}
}

// dbPath returns the file behind db.
func dbPath(t *testing.T, db *store.DB) string {
	t.Helper()
	var p string
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&p)
	})
	if err != nil || p == "" {
		t.Fatalf("database path: %q %v", p, err)
	}
	return filepath.Clean(p)
}

func TestKilledWriterLeavesNoPartialRows(t *testing.T) {
	w := applyS2Eligible(t)
	ctx := t.Context()
	// One committed acquisition must survive the kill untouched.
	hd := w.slotID("tmdb:603", "hd")
	if out := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{hd}}); !out.Created || !slices.Equal(out.Attached, []SlotID{hd}) || len(out.Paused) != 0 {
		t.Fatalf("setup outcome %+v, want created with hd attached", out)
	}
	before := acqSnapshot(t, w.db)
	beforeCounts := acqCounts(t, w.db)

	seen := killChild(t, "membership-acquisition", dbPath(t, w.db), strconv.FormatInt(int64(w.libs["Collector"]), 10))

	// The child had really written the title, two slots, the membership and the
	// acquisition with its two download_slot rows inside the open transaction.
	want := maps.Clone(beforeCounts)
	want["title"]++
	want["slot"] += 2
	want["library_title"]++
	want["acquisition"]++
	want["download_slot"] += 2
	if !maps.Equal(seen, want) {
		t.Fatalf("child saw %v inside its transaction, want %v", seen, want)
	}

	// Reopen the file: recovery leaves none of it.
	db, err := store.Open(ctx, dbPath(t, w.db), store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if after := acqSnapshot(t, db); !maps.EqualFunc(before, after, slices.Equal) {
		t.Fatalf("killed writer left rows: before %v after %v", before, after)
	}

	// Replay converges: the work lands once, and a second replay adds nothing.
	l := New(db)
	for i := range 2 {
		m, err := l.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Collector"], MetadataID: crashMetadataID, Kind: "movie", Name: "Crash Movie"})
		if err != nil {
			t.Fatalf("replay %d AddMembership: %v", i, err)
		}
		out := mustRequest(t, l, AcquisitionRequest{ClientID: "qbit", ReleaseKey: crashRelease, Slots: m.SlotIDs})
		wantAttached := 2 * (1 - i) // the first pass attaches both slots, the second none
		if out.Created != (i == 0) || len(out.Attached) != wantAttached {
			t.Fatalf("replay %d outcome %+v, want created=%v with %d slots attached", i, out, i == 0, wantAttached)
		}
	}
	if got := acqCounts(t, db); !maps.Equal(got, want) {
		t.Fatalf("after replay counts = %v, want %v", got, want)
	}
}

// A writer killed after it inserted a hold and an eligible attachment, before it
// committed, leaves neither; replaying the request lands one acquisition and one hold.
func TestKilledWriterLeavesNoPartialRowsWithHold(t *testing.T) {
	w := applyS2Eligible(t)
	ctx := t.Context()
	paused, eligible := w.slotID("tmdb:604", "hd"), w.slotID("tmdb:603", "hd") // unresolved owner pauses; local attaches
	file := FileRecord{SlotID: eligible, RootKey: "movies", RelativePath: "Synthetic Movie A/a.mkv", SizeBytes: 4096}
	if err := w.lib.RecordFile(ctx, file); err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	before := acqSnapshot(t, w.db)
	beforeCounts := acqCounts(t, w.db)

	seen := killChild(t, "hold-acquisition", dbPath(t, w.db), strconv.FormatInt(int64(paused), 10), strconv.FormatInt(int64(eligible), 10))

	// The child really wrote the hold, the acquisition and its download_slot row.
	want := maps.Clone(beforeCounts)
	want["review_hold"]++
	want["acquisition"]++
	want["download_slot"]++
	if !maps.Equal(seen, want) {
		t.Fatalf("child saw %v inside its transaction, want %v", seen, want)
	}

	db, err := store.Open(ctx, dbPath(t, w.db), store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if after := acqSnapshot(t, db); !maps.EqualFunc(before, after, slices.Equal) {
		t.Fatalf("killed writer left rows: before %v after %v", before, after)
	}
	requireFileRows(t, db, []string{fileRow(file)})

	// Replay converges: one acquisition and one hold land, and a second replay adds nothing.
	l := New(db)
	for i := range 2 {
		out := mustRequest(t, l, AcquisitionRequest{ClientID: "qbit", ReleaseKey: crashRelease, Slots: []SlotID{paused, eligible}})
		wantAttached := 1 - i
		if out.Created != (i == 0) || len(out.Attached) != wantAttached || len(out.Paused) != 1 || out.Paused[0].SlotID != paused || out.Paused[0].Reason != ReasonOwnershipUnresolved {
			t.Fatalf("replay %d outcome %+v, want created=%v, %d attached, the unresolved slot paused", i, out, i == 0, wantAttached)
		}
	}
	if got := acqCounts(t, db); !maps.Equal(got, want) {
		t.Fatalf("after replay counts = %v, want %v", got, want)
	}
	requireFileRows(t, db, []string{fileRow(file)})
}

// A writer killed after it changed episode and movie state, with the rollups
// equal to the recompute inside its open transaction, before it committed,
// leaves the base rows and the rollups as they were, and both agree; replaying
// the same changes lands both together.
func TestKilledWriterProjectionMatchesOracle(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	series, movie := w.slotID("tvdb:9001", "hd"), w.slotID("tmdb:603", "hd")
	requireOracle(t, w.db)
	baseRows := []string{`SELECT * FROM slot_episode`, `SELECT id, media_state, size_bytes FROM slot`, rollupDumpQuery}
	snapshot := func(db *store.DB) [][]string {
		var out [][]string
		for _, q := range baseRows {
			out = append(out, dump(t, db, q))
		}
		return out
	}
	before := snapshot(w.db)
	beforeCounts := acqCounts(t, w.db)

	seen := killChild(t, "slot-state", dbPath(t, w.db), strconv.FormatInt(int64(series), 10), strconv.FormatInt(int64(movie), 10))
	if !maps.Equal(seen, beforeCounts) {
		t.Fatalf("child saw %v inside its transaction, want the acquisition tables unchanged %v", seen, beforeCounts)
	}

	// Reopen the file: recovery leaves none of it, and rollups equal the recompute.
	db, err := store.Open(ctx, dbPath(t, w.db), store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	after := snapshot(db)
	for i, q := range baseRows {
		if !slices.Equal(before[i], after[i]) {
			t.Fatalf("killed writer left rows of %q: before %v after %v", q, before[i], after[i])
		}
	}
	requireOracle(t, db)

	// Replay: the same changes land, base rows and rollups together.
	l := New(db)
	for _, c := range []SlotEpisodeChange{
		{SlotID: series, Season: 1, Number: 2, Monitored: true, State: "live", SizeBytes: 77},
		{SlotID: series, Season: 2, Number: 1, Monitored: true, State: "on_disk", SizeBytes: 55},
	} {
		if err := l.SetSlotEpisode(ctx, c); err != nil {
			t.Fatalf("replay %+v: %v", c, err)
		}
	}
	if err := l.SetMovieSlot(ctx, MovieSlotChange{SlotID: movie, State: "on_disk", SizeBytes: 9}); err != nil {
		t.Fatalf("replay movie: %v", err)
	}
	requireOracle(t, db)
	if got := snapshot(db); slices.Equal(got[2], before[2]) {
		t.Fatal("the replay moved no rollup")
	}
	r, err := l.Rollups(ctx, w.titles["tvdb:9001"])
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(r, func(x Rollup) bool { return x.SlotID == series }); i < 0 || r[i].SizeBytes != 1132 || r[i].OnDisk != 2 || r[i].Spine != "FLMFM" {
		t.Fatalf("rollup after the replay = %+v, want 1132 bytes, 2 on disk, spine FLMFM", r)
	}
}

// A writer killed inside the activation that retires a generation and writes its
// purge intent leaves neither: the retired row keeps its state, the new one is
// not active and no job row exists. A process kill proves application-crash
// recovery only (ADR 0003).
func TestKilledActivationLeavesNoIntent(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	lib := w.lib
	title := w.titles["tmdb:603"]
	// One active generation with an alias, then a staging one over it.
	active, err := lib.StageGeneration(ctx, "dataset-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.SetAliases(ctx, title, &active, []string{"Alpha Alias"}); err != nil {
		t.Fatal(err)
	}
	if err := lib.ActivateGeneration(ctx, active); err != nil {
		t.Fatal(err)
	}
	staging, err := lib.StageGeneration(ctx, "dataset-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.SetAliases(ctx, title, &staging, []string{"Bravo Alias"}); err != nil {
		t.Fatal(err)
	}
	before := dump(t, w.db, `SELECT id, dataset, state FROM alias_generation ORDER BY id`)
	if n := queryInt(t, w.db, `SELECT count(*) FROM job`); n != 0 {
		t.Fatalf("jobs before the activation = %d, want none", n)
	}

	// The child reports only once the retire, the activation and its intent are
	// written inside one open transaction; the kill comes before its commit.
	killChild(t, "activate-generation", dbPath(t, w.db), strconv.FormatInt(int64(staging), 10))

	db, err := store.Open(ctx, dbPath(t, w.db), store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if after := dump(t, db, `SELECT id, dataset, state FROM alias_generation ORDER BY id`); !slices.Equal(before, after) {
		t.Fatalf("the killed activation moved generation state:\n before %v\n after  %v", before, after)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM job`); n != 0 {
		t.Fatalf("the killed activation left %d job rows", n)
	}
	// The same call on the reopened file lands both, once.
	if err := New(db).ActivateGeneration(ctx, staging); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM job`); n != 1 {
		t.Fatalf("jobs after the replay = %d, want one", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM alias_generation WHERE state = 'active' AND id = ?`, int64(staging)); n != 1 {
		t.Fatalf("the replay did not activate the staging generation")
	}
}
