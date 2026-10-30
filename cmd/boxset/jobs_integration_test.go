//go:build integration && unix

package main

import (
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The built binary's durable purge: a process that is killed, or drained by
// SIGTERM, mid-attempt leaves the accepted intent and the key it owns behind,
// and the next start reconciles it and finishes the deletion. The alias set is
// large enough that the purge cannot finish before the signal arrives.

// purgeAliases is the retired generation's alias count: enough batches that the
// attempt is still running when the test signals the process.
const purgeAliases = 120_000

// seedRetiredPurge leaves a data directory whose alias generation was retired
// with purgeAliases aliases, so the required purge it enqueued in the same Write
// is the only work the binary has to do. It returns the retired generation id.
func seedRetiredPurge(t *testing.T, dir string) int64 {
	t.Helper()
	ctx := t.Context()
	layout := install.Layout{Dir: dir}
	if err := install.CheckMount(dir); err != nil {
		t.Fatalf("mount of the data directory: %v", err)
	}
	db, err := store.Open(ctx, layout.DB(), store.Options{})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	lib := library.New(db)
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO title (id, metadata_id, kind, name) VALUES (1, 'tmdb:603', 'movie', 'Synthetic Movie')`)
		return err
	}); err != nil {
		t.Fatalf("seed title: %v", err)
	}
	aliases := make([]string, 0, purgeAliases)
	for i := range purgeAliases {
		aliases = append(aliases, fmt.Sprintf("alias %06d", i))
	}
	retired, err := lib.StageGeneration(ctx, "dataset-a")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := lib.SetAliases(ctx, 1, &retired, aliases); err != nil {
		t.Fatalf("seed aliases: %v", err)
	}
	if err := lib.ActivateGeneration(ctx, retired); err != nil {
		t.Fatalf("first activation: %v", err)
	}
	staging, err := lib.StageGeneration(ctx, "dataset-a")
	if err != nil {
		t.Fatalf("stage replacement: %v", err)
	}
	if err := lib.SetAliases(ctx, 1, &staging, []string{"bravo alias"}); err != nil {
		t.Fatalf("seed replacement alias: %v", err)
	}
	if err := lib.ActivateGeneration(ctx, staging); err != nil {
		t.Fatalf("second activation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	return int64(retired)
}

// readOnly opens the installation's database for a live observation. The
// server holds the file; a read-only connection sees its committed work.
func readOnly(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(install.Layout{Dir: dir}.DB())+"?mode=ro")
	if err != nil {
		t.Fatalf("open the database read-only: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// purgeRow is the one alias purge's recorded state.
type purgeRow struct {
	state    string
	attempts int
	reason   string
}

func jobRow(t *testing.T, dir string) purgeRow {
	t.Helper()
	var row purgeRow
	err := readOnly(t, dir).QueryRowContext(t.Context(),
		`SELECT state, attempts, coalesce(reason_code, '') FROM job ORDER BY id LIMIT 1`).
		Scan(&row.state, &row.attempts, &row.reason)
	if err != nil {
		t.Fatalf("read the job row: %v", err)
	}
	return row
}

func aliasCount(t *testing.T, dir string, generation int64) int64 {
	t.Helper()
	var n int64
	if err := readOnly(t, dir).QueryRowContext(t.Context(),
		`SELECT count(*) FROM title_alias WHERE generation_id = ?`, generation).Scan(&n); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	return n
}

// scheduleRow is the built-in maintenance schedule's recorded state.
type scheduleRow struct {
	zone      string
	localTime string
	nextDueAt int64
	revision  int64
}

// readScheduleRow reads the jobs.prune schedule of one data directory.
func readScheduleRow(t *testing.T, dir string) scheduleRow {
	t.Helper()
	var row scheduleRow
	err := readOnly(t, dir).QueryRowContext(t.Context(),
		`SELECT zone, local_time, next_due_at, revision FROM schedule WHERE name = 'jobs.prune'`).
		Scan(&row.zone, &row.localTime, &row.nextDueAt, &row.revision)
	if err != nil {
		t.Fatalf("read the jobs.prune schedule: %v", err)
	}
	return row
}

// scheduleRows is how many schedule rows one data directory holds.
func scheduleRows(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	if err := readOnly(t, dir).QueryRowContext(t.Context(), `SELECT count(*) FROM schedule`).Scan(&n); err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	return n
}

// drain stops the installation the way its SIGTERM handling does, and requires
// the clean exit that shutdown finishes with.
func drain(t *testing.T, s *proc) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("exit after SIGTERM = %d, want the drain to finish: %s", code, s.stderr.String())
	}
}

// awaitRunning waits until the purge attempt is running, so the signal lands
// while the attempt can still mutate.
func awaitRunning(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		row := jobRow(t, dir)
		switch row.state {
		case "running":
			return
		case "succeeded", "failed", "review", "expired":
			t.Fatalf("the purge reached %s before the signal arrived", row.state)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the purge did not start within 30 s: %+v", jobRow(t, dir))
}

// awaitPurged waits until the retired generation's aliases are gone and the row
// succeeded, and returns the row it settled on.
func awaitPurged(t *testing.T, dir string, generation int64) purgeRow {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if left := aliasCount(t, dir, generation); left == 0 {
			row := jobRow(t, dir)
			if row.state == "succeeded" {
				return row
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the purge did not finish: %+v, aliases left %d", jobRow(t, dir), aliasCount(t, dir, generation))
	return purgeRow{}
}

// TestPurgeResumesAfterKill: a SIGKILL during the purge corrupts nothing, and
// the next start of the built binary reconciles the row it left running and
// finishes the deletion.
func TestPurgeResumesAfterKill(t *testing.T) {
	dir := t.TempDir()
	generation := seedRetiredPurge(t, dir)

	first := start(t, dir)
	first.waitReady(t)
	awaitRunning(t, dir)
	if err := first.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := first.exited(t); code != -1 {
		t.Fatalf("exit after SIGKILL = %d, want the signal", code)
	}
	killed := jobRow(t, dir)
	left := aliasCount(t, dir, generation)
	if left == 0 {
		t.Fatalf("the purge had finished before the kill, so the resume proves nothing")
	}
	if killed.state != "running" {
		t.Fatalf("the killed process left the row %+v, want it running with its key held", killed)
	}

	second := start(t, dir)
	second.waitReady(t)
	done := awaitPurged(t, dir, generation)
	if done.attempts < 2 {
		t.Fatalf("the resumed purge recorded %d attempts, want the killed attempt and this one", done.attempts)
	}
	t.Logf("PURGE-RESUMED-AFTER-KILL killed_state=%s aliases_left=%d attempts=%d state=%s", killed.state, left, done.attempts, done.state)
}

// TestDrainLeavesRecoverablePurge: SIGTERM while the purge runs drains the
// installation (exit 0), cancels the attempt rather than releasing its key, and
// the next start finishes the same intent.
func TestDrainLeavesRecoverablePurge(t *testing.T) {
	dir := t.TempDir()
	generation := seedRetiredPurge(t, dir)

	first := start(t, dir)
	first.waitReady(t)
	awaitRunning(t, dir)
	if err := first.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := first.exited(t); code != 0 {
		t.Fatalf("exit after SIGTERM = %d, want the drain to finish: %s", code, first.stderr.String())
	}
	drained := jobRow(t, dir)
	left := aliasCount(t, dir, generation)
	if drained.state != "running" {
		t.Fatalf("the drained process left the row %+v, want it running for the next start's reconciliation", drained)
	}
	if left == 0 {
		t.Fatalf("the purge had finished before the drain, so the recovery proves nothing")
	}

	second := start(t, dir)
	second.waitReady(t)
	done := awaitPurged(t, dir, generation)
	if done.attempts < 2 {
		t.Fatalf("the recovered purge recorded %d attempts, want the drained attempt and this one", done.attempts)
	}
	t.Logf("DRAIN-LEFT-RECOVERABLE-PURGE drained_state=%s aliases_left=%d attempts=%d state=%s", drained.state, left, done.attempts, done.state)
}

// TestScheduleSurvivesRestart: the built binary seeds jobs.prune once, and a
// restart keeps the stored instant instead of recomputing it, so downtime is
// measured from where the schedule actually was.
func TestScheduleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := start(t, dir)
	first.waitReady(t)
	seeded := readScheduleRow(t, dir)
	if seeded.zone != "UTC" || seeded.localTime != "04:00" {
		t.Fatalf("the seeded schedule = zone %q at %q, want UTC at 04:00", seeded.zone, seeded.localTime)
	}
	if got := time.Unix(seeded.nextDueAt, 0).UTC(); got.Hour() != 4 || got.Minute() != 0 || !got.After(time.Now()) {
		t.Fatalf("the seeded instant = %s, want the coming 04:00 UTC", got.Format(time.RFC3339))
	}
	drain(t, first)

	// A restart that reseeded would overwrite this instant: it is not the coming
	// 04:00, so its survival is the observation.
	stored := time.Date(2030, 1, 2, 4, 0, 0, 0, time.UTC)
	db, err := store.Open(t.Context(), install.Layout{Dir: dir}.DB(), store.Options{})
	if err != nil {
		t.Fatalf("open the stopped store: %v", err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE schedule SET next_due_at = ? WHERE name = 'jobs.prune'`, stored.Unix())
		return err
	}); err != nil {
		t.Fatalf("move the schedule instant: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the stopped store: %v", err)
	}

	second := start(t, dir)
	second.waitReady(t)
	kept := readScheduleRow(t, dir)
	if kept.nextDueAt != stored.Unix() || kept.revision != seeded.revision {
		t.Fatalf("the restart rewrote the schedule to %d revision %d, want %d revision %d",
			kept.nextDueAt, kept.revision, stored.Unix(), seeded.revision)
	}
	if got := scheduleRows(t, dir); got != 1 {
		t.Fatalf("%d schedule rows after the restart, want the one seeded row", got)
	}
	drain(t, second)
	t.Logf("SCHEDULE-SURVIVED-RESTART seeded=%d stored=%d kept=%d revision=%d",
		seeded.nextDueAt, stored.Unix(), kept.nextDueAt, kept.revision)
}

// TestTZDataEmbedded: the binary carries the zone database, so the built-in rule
// keeps 04:00 on the wall clock of a zone the host serves no files for.
func TestTZDataEmbedded(t *testing.T) {
	// The linked package set is the observation: time/tzdata is what ships the
	// database inside the binary, and without the import `main` no longer carries
	// its symbols, so this fails on the source change alone. The runtime claim —
	// that this process resolves Europe/Berlin from the embedded copy with no
	// platform zone database — cannot be exercised here: the test image installs
	// tzdata, so the lookup would answer either way and prove nothing.
	symbols, err := exec.Command("go", "tool", "nm", binary).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm %s: %v: %s", binary, err, symbols)
	}
	if !strings.Contains(string(symbols), "time/tzdata.") {
		t.Fatalf("the built binary links no time/tzdata symbol, so nothing embeds the zone database")
	}

	dir := t.TempDir()
	s := startIn(t, "", dir, "TZ=Europe/Berlin", "ZONEINFO=/nonexistent")
	s.waitReady(t)

	row := readScheduleRow(t, dir)
	if row.zone != "Europe/Berlin" || row.localTime != "04:00" {
		t.Fatalf("the seeded schedule = zone %q at %q, want Europe/Berlin at 04:00", row.zone, row.localTime)
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Europe/Berlin: %v", err)
	}
	at := time.Unix(row.nextDueAt, 0).UTC().In(berlin)
	if at.Hour() != 4 || at.Minute() != 0 {
		t.Fatalf("the seeded instant reads %02d:%02d in Europe/Berlin, want 04:00", at.Hour(), at.Minute())
	}
	if !at.After(time.Now()) {
		t.Fatalf("the seeded instant %s is not the coming 04:00", at.Format(time.RFC3339))
	}
	drain(t, s)
	t.Logf("TZDATA-LINKED the binary carries the time/tzdata package")
	t.Logf("TZDATA-EMBEDDED zone=%s next_due_at=%s local=%s",
		row.zone, at.UTC().Format(time.RFC3339), at.Format(time.RFC3339))
}
