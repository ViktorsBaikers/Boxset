package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// catalogObject is one application object of sqlite_schema.
type catalogObject struct{ typ, name, table, sql string }

// catalog lists the application objects as the store's own reader does: the
// record tables and what SQLite maintains itself are left out.
func catalog(t *testing.T, db *sql.DB) []catalogObject {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema WHERE tbl_name NOT IN ('boxset_schema_hash', 'goose_db_version', 'sqlite_sequence', 'sqlite_stat1', 'sqlite_stat2', 'sqlite_stat3', 'sqlite_stat4') ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []catalogObject
	for rows.Next() {
		var o catalogObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// versionOneCatalog applies the canonical internal/store/migrations/00001_library.sql
// alone (goose UpTo 1) to a scratch file and returns its application objects.
func versionOneCatalog(t *testing.T) []catalogObject {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v1.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, os.DirFS(filepath.Join("..", "store", "migrations")), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	return catalog(t, db)
}

// The first migration adopts a populated predecessor file in place. These
// proofs build that file from the canonical S2 world (memberships, ownership,
// episode and movie state, holds, indexer restrictions, acquisitions, files,
// rollups and aliases), take it back to the shape and format stamp the temporary
// bootstrap left, adopt it through store.Open, and then hold it to the same
// oracles the fresh-file suites use: identity and projection, search, and the
// writers' own invariants.

// adoptedTables is every table of the library schema.
var adoptedTables = append(slices.Clone(countedTables),
	"acquisition", "download_slot", "file", "review_hold", "library_indexer", "slot_indexer", "title_rollup", "alias_generation", "title_alias")

// populateBeyondS2 adds the state the S2 steps leave out: movie and episode
// state, indexer restrictions (one empty intersection, so one more hold),
// acquisitions with recorded files, and aliases in an active and an inactive generation.
func populateBeyondS2(t *testing.T, w *s2World) {
	t.Helper()
	ctx := t.Context()
	mustMovie(t, w.lib, w.slotID("tmdb:603", "hd"), "on_disk", 4000)
	mustMovie(t, w.lib, w.slotID("tmdb:603", "uhd"), "upgrade", 900)
	for _, c := range []SlotEpisodeChange{
		{SlotID: w.slotID("tvdb:9001", "hd"), Season: 1, Number: 2, Monitored: true, State: "live", SizeBytes: 77},
		{SlotID: w.slotID("tvdb:9001", "hd"), Season: 2, Number: 1, Monitored: true, State: "on_disk", SizeBytes: 55},
		{SlotID: w.slotID("tvdb:9002", "hd"), Season: 1, Number: 1, Monitored: true, State: "unaired"},
	} {
		if err := w.lib.SetSlotEpisode(ctx, c); err != nil {
			t.Fatalf("SetSlotEpisode %+v: %v", c, err)
		}
	}

	series, other := w.slotID("tvdb:9001", "hd"), w.slotID("tvdb:9002", "hd")
	mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: "Series.One.1080p-GRP", Slots: []SlotID{series}})
	if err := w.lib.RecordFile(ctx, FileRecord{SlotID: series, RootKey: "series", RelativePath: "Synthetic Series/s01e01.mkv", SizeBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetLibraryIndexers(ctx, w.libs["Series"], []string{"idx-a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetSlotIndexers(ctx, other, []string{"idx-b"}); err != nil {
		t.Fatal(err)
	}
	if out := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: "Series.Two.1080p-GRP", Slots: []SlotID{other}}); len(out.Paused) != 1 {
		t.Fatalf("fixture: the empty indexer intersection paused %d slots, want 1", len(out.Paused))
	}

	if err := w.lib.SetAliases(ctx, w.titles["tmdb:603"], nil, []string{"Alias Alpha"}); err != nil {
		t.Fatal(err)
	}
	active, err := w.lib.StageGeneration(ctx, "dataset-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetAliases(ctx, w.titles["tmdb:604"], &active, []string{"Generation Bravo"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.ActivateGeneration(ctx, active); err != nil {
		t.Fatal(err)
	}
	staged, err := w.lib.StageGeneration(ctx, "dataset-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetAliases(ctx, w.titles["tmdb:700"], &staged, []string{"Hidden Charlie"}); err != nil {
		t.Fatal(err)
	}
}

// fullSnapshot is every row of every library table.
func fullSnapshot(t *testing.T, db *store.DB) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(adoptedTables))
	for _, name := range adoptedTables {
		out[name] = dump(t, db, "SELECT * FROM "+name)
	}
	return out
}

// rollupsOf is the stored rollups of every title, by metadata id.
func rollupsOf(t *testing.T, w *s2World) map[string][]Rollup {
	t.Helper()
	out := map[string][]Rollup{}
	for metadataID, id := range w.titles {
		r, err := w.lib.Rollups(t.Context(), id)
		if err != nil {
			t.Fatalf("Rollups %s: %v", metadataID, err)
		}
		out[metadataID] = r
	}
	return out
}

// searchesAgree fails unless Search answers every term as the search oracle does
// from the base tables, and the FTS index equals its content.
func searchesAgree(t *testing.T, w *s2World, terms ...string) {
	t.Helper()
	ctx := t.Context()
	var src []searchSource
	if err := w.db.Read(ctx, func(tx *sql.Tx) (err error) { src, err = searchSources(ctx, tx); return }); err != nil {
		t.Fatalf("search sources: %v", err)
	}
	for _, term := range terms {
		got, err := w.lib.Search(ctx, term, 1_000_000)
		if err != nil {
			t.Fatalf("Search %q: %v", term, err)
		}
		if want := oracleSearch(src, term); !slices.Equal(got, want) {
			t.Fatalf("Search %q = %v, oracle = %v", term, got, want)
		}
	}
	if err := w.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, integrityRank1)
		return err
	}); err != nil {
		t.Fatalf("title_search integrity-check against content: %v", err)
	}
}

func TestAdoptedPredecessorPassesCanonicalOracles(t *testing.T) {
	ctx := t.Context()
	source := applyS2(t)
	populateBeyondS2(t, source)
	pristine := fullSnapshot(t, source.db)
	pristineRollups := rollupsOf(t, source)
	for _, table := range []string{"title", "slot", "library_title", "slot_episode", "review_hold", "acquisition", "download_slot", "file", "title_rollup", "title_alias", "library_indexer", "slot_indexer"} {
		if len(pristine[table]) == 0 {
			t.Fatalf("fixture: table %s is empty; the adoption proof needs it populated", table)
		}
	}

	// Take the file back to what the temporary bootstrap produced: the same
	// schema and rows, no migration records and none of the objects later
	// migrations add, the legacy format stamp. The version-1 catalog comes from
	// the canonical first migration alone, applied to a scratch file.
	path := dbPath(t, source.db)
	if err := source.db.Close(); err != nil {
		t.Fatal(err)
	}
	keep := versionOneCatalog(t)
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetMaxOpenConns(1)
	var drops []string
	for _, o := range catalog(t, legacy) {
		if !slices.Contains(keep, o) && o.typ == "table" {
			drops = append(drops, "DROP TABLE "+strconv.Quote(o.name))
		}
	}
	for _, stmt := range append(drops, "DROP TABLE boxset_schema_hash", "DROP TABLE goose_db_version", "PRAGMA user_version=1") {
		if _, err := legacy.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if got := catalog(t, legacy); !slices.Equal(got, keep) {
		t.Fatalf("the legacy fixture's catalog is not the first migration's: %d objects, want %d", len(got), len(keep))
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Adopt it through the public Open, behind the backup hook.
	var pending []store.Pending
	snapshot := filepath.Join(t.TempDir(), "pre-migration.db")
	db, err := store.Open(ctx, path, store.Options{BeforeMigrate: func(c context.Context, p store.Pending) error {
		pending = append(pending, p)
		return p.SnapshotInto(c, snapshot)
	}})
	if err != nil {
		t.Fatalf("store.Open over the populated predecessor: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if len(pending) != 1 || pending[0].From != 0 || pending[0].To < 1 {
		t.Fatalf("BeforeMigrate calls = %+v, want one from version 0", pending)
	}
	snap, err := sql.Open("sqlite", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapTitles int
	if err := snap.QueryRowContext(ctx, "SELECT count(*) FROM title").Scan(&snapTitles); err != nil || snapTitles != len(pristine["title"]) {
		t.Errorf("the pre-migration snapshot holds %d titles (err %v), want %d", snapTitles, err, len(pristine["title"]))
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}

	adopted := &s2World{t: t, fx: source.fx, db: db, lib: New(db), libs: source.libs, titles: source.titles}

	// Every row of every table, and every stored rollup, is what the fresh world held.
	if got := fullSnapshot(t, db); !sameSnapshot(got, pristine) {
		for _, table := range adoptedTables {
			if !slices.Equal(got[table], pristine[table]) {
				t.Errorf("table %s changed by the adoption:\n got %v\nwant %v", table, got[table], pristine[table])
			}
		}
		t.FailNow()
	}
	for metadataID, want := range pristineRollups {
		got, err := adopted.lib.Rollups(ctx, adopted.titles[metadataID])
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("rollups of %s after the adoption = %+v (err %v), want %+v", metadataID, got, err, want)
		}
	}

	// Canonical identity: the fixture's expected world, read through the library API.
	fx := adopted.fx
	var wantPairs []string
	for _, p := range fx.Expect.LibraryTitles {
		line, err := json.Marshal([]string{p.Library, p.MetadataID})
		if err != nil {
			t.Fatal(err)
		}
		wantPairs = append(wantPairs, string(line))
	}
	slices.Sort(wantPairs)
	if got := memberships(t, db); !slices.Equal(got, wantPairs) {
		t.Errorf("library_title pairs after the adoption:\n got %v\nwant %v", got, wantPairs)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM title`); got != fx.Expect.Titles {
		t.Errorf("titles = %d, want %d", got, fx.Expect.Titles)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM slot`); got != fx.Expect.Slots {
		t.Errorf("slots = %d, want %d", got, fx.Expect.Slots)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM sync_map`); got != fx.Expect.SyncMap {
		t.Errorf("sync_map = %d, want %d", got, fx.Expect.SyncMap)
	}
	for metadataID, want := range fx.Expect.TitleSlots {
		got := adopted.slots(metadataID)
		if len(got) != len(want) {
			t.Fatalf("%s: %d slots, want %d", metadataID, len(got), len(want))
		}
		for i, g := range got {
			if have := (s2Slot{g.Key, g.Primary, g.Owner, g.Monitored, g.NamingLanguage, g.SearchLanguage, g.Profile}); have != want[i] {
				t.Errorf("%s slot %d = %+v, want %+v", metadataID, i, have, want[i])
			}
		}
	}

	// Canonical projection and search oracles on the adopted file.
	requireOracle(t, db)
	searchesAgree(t, adopted, "Synthetic", "Movie", "Series", "Alias Alpha", "Generation Bravo", "Hidden Charlie", "nomatch")
	if got, err := adopted.lib.Search(ctx, "Generation Bravo", 10); err != nil || !slices.Equal(got, []TitleID{adopted.titles["tmdb:604"]}) {
		t.Errorf("Search of an active-generation alias = %v, %v; want tmdb:604", got, err)
	}
	if got, err := adopted.lib.Search(ctx, "Hidden Charlie", 10); err != nil || len(got) != 0 {
		t.Errorf("Search of an alias in a staging generation = %v, %v; want none", got, err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM alias_generation g
		WHERE g.alias_count <> (SELECT count(*) FROM title_alias a WHERE a.generation_id = g.id)`); n != 0 {
		t.Errorf("%d generations whose alias_count differs from their aliases", n)
	}

	// Replaying every fixture step converges: no row and no hold changes.
	adopted.applyAll()
	if got := fullSnapshot(t, db); !sameSnapshot(got, pristine) {
		t.Fatal("replaying the fixture steps on the adopted file changed rows")
	}

	// The triggers and writers still work on the adopted file: new state moves the
	// rollups and the index, and both oracles keep agreeing.
	if err := adopted.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: adopted.slotID("tvdb:9001", "uhd"), Season: 1, Number: 1, Monitored: true, State: "on_disk", SizeBytes: 321}); err != nil {
		t.Fatal(err)
	}
	if err := adopted.lib.RenameTitle(ctx, adopted.titles["tmdb:700"], "Renamed Delta"); err != nil {
		t.Fatal(err)
	}
	requireOracle(t, db)
	searchesAgree(t, adopted, "Renamed Delta", "Synthetic Movie B", "Alias Alpha")
}
