package library

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
		os.Stderr.WriteString("unknown crash scenario " + name + "\n")
		return 2
	}
	db, err := store.Open(ctx, os.Getenv(crashEnvDB), store.Options{})
	if err != nil {
		os.Stderr.WriteString("child store.Open: " + err.Error() + "\n")
		return 2
	}
	ready := func(tx *sql.Tx) {
		counts, err := countTables(tx, acqTables)
		if err != nil {
			os.Stderr.WriteString("child count: " + err.Error() + "\n")
			os.Exit(2)
		}
		line, _ := json.Marshal(counts)
		os.Stdout.WriteString(crashReady + string(line) + "\n")
	}
	err = sc(ctx, New(db), strings.Split(os.Getenv(crashEnvArgs), "\n"), ready)
	os.Stderr.WriteString("child scenario ended: " + err.Error() + "\n")
	return 3
}

// killChild runs scenario in a child on the store at path, waits until the
// child reports its transaction open, kills it and returns the row counts the
// child saw inside that transaction.
func killChild(t *testing.T, scenario, path string, args ...string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashEnvScenario+"="+scenario, crashEnvDB+"="+path, crashEnvArgs+"="+strings.Join(args, "\n"))
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
	w := applyS2(t)
	ctx := t.Context()
	// One committed acquisition must survive the kill untouched.
	mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{w.slotID("tmdb:603", "hd")}})
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
