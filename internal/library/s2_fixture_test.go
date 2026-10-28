package library

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// s2Fixture is testdata/store/s2-fixture.json: the synthetic S2 source-import
// and 4K-persona world, its operations and the state it must produce.
type s2Fixture struct {
	Libraries []struct {
		Name           string `json:"name"`
		Monitored      bool   `json:"monitored"`
		NamingLanguage string `json:"naming_language"`
		SearchLanguage string `json:"search_language"`
		Profile        string `json:"profile"`
		Template       []struct {
			Key     string `json:"key"`
			Primary bool   `json:"primary"`
		} `json:"template"`
	} `json:"libraries"`
	Personas []struct {
		Key     string `json:"key"`
		SlotKey string `json:"slot_key"`
	} `json:"personas"`
	Steps  []s2Step `json:"steps"`
	Expect struct {
		Titles        int                 `json:"titles"`
		Slots         int                 `json:"slots"`
		SyncMap       int                 `json:"sync_map"`
		TitleSlots    map[string][]s2Slot `json:"title_slots"`
		LibraryTitles []struct {
			Library    string `json:"library"`
			MetadataID string `json:"metadata_id"`
		} `json:"library_titles"`
		SlotEpisode []struct {
			MetadataID string `json:"metadata_id"`
			SlotKey    string `json:"slot_key"`
			Rows       int    `json:"rows"`
			Monitored  int    `json:"monitored"`
			OnDisk     int    `json:"on_disk"`
		} `json:"slot_episodes"`
	} `json:"expect"`
}

type s2Slot struct {
	Key            string `json:"key"`
	Primary        bool   `json:"primary"`
	Owner          string `json:"owner"`
	Monitored      bool   `json:"monitored"`
	NamingLanguage string `json:"naming_language"`
	SearchLanguage string `json:"search_language"`
	Profile        string `json:"profile"`
}

type s2Step struct {
	Op         string `json:"op"`
	Library    string `json:"library"`
	Persona    string `json:"persona"`
	MetadataID string `json:"metadata_id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Instance   string `json:"instance"`
	SourceID   string `json:"source_id"`
	SlotKey    string `json:"slot_key"`
	Seasons    []struct {
		Season   int `json:"season"`
		Episodes int `json:"episodes"`
	} `json:"seasons"`
	Season    int    `json:"season"`
	Number    int    `json:"number"`
	Monitored bool   `json:"monitored"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
	Conflict  bool   `json:"conflict"`
}

func loadS2(t *testing.T) s2Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "s2-fixture.json"))
	if err != nil {
		t.Fatalf("read S2 fixture: %v", err)
	}
	var fx s2Fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode S2 fixture: %v", err)
	}
	return fx
}

// s2World is a fresh store holding the fixture's libraries and personas, plus
// the ids the applied steps produced.
type s2World struct {
	t      *testing.T
	fx     s2Fixture
	db     *store.DB
	lib    *Library
	libs   map[string]LibraryID
	titles map[string]TitleID
}

func newS2World(t *testing.T) *s2World {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	w := &s2World{t: t, fx: loadS2(t), db: db, lib: New(db), libs: map[string]LibraryID{}, titles: map[string]TitleID{}}
	for _, l := range w.fx.Libraries {
		spec := LibrarySpec{Name: l.Name, Defaults: LibraryDefaults{
			Monitored: l.Monitored, NamingLanguage: l.NamingLanguage, SearchLanguage: l.SearchLanguage, Profile: l.Profile,
		}}
		for _, ts := range l.Template {
			spec.Template = append(spec.Template, TemplateSlot{Key: ts.Key, Primary: ts.Primary})
		}
		id, err := w.lib.AddLibrary(t.Context(), spec)
		if err != nil {
			t.Fatalf("AddLibrary %s: %v", l.Name, err)
		}
		w.libs[l.Name] = id
	}
	for _, p := range w.fx.Personas {
		if err := w.lib.AddPersona(t.Context(), p.Key, p.SlotKey); err != nil {
			t.Fatalf("AddPersona %s: %v", p.Key, err)
		}
	}
	return w
}

// applyS2 builds a world and applies every fixture step.
func applyS2(t *testing.T) *s2World {
	t.Helper()
	w := newS2World(t)
	w.applyAll()
	return w
}

// applyS2Eligible builds the S2 world from every fixture step except a
// conflict step whose op is membership or persona. That step title-holds
// tmdb:603, so a test whose premise is an eligible tmdb:603/hd or /dc slot
// builds its world here. The canonical rows equal applyS2's, and tmdb:603/uhd
// keeps the two holds of the conflicting imports.
func applyS2Eligible(t *testing.T) *s2World {
	t.Helper()
	w := newS2World(t)
	for _, s := range w.fx.Steps {
		if s.Conflict && (s.Op == "membership" || s.Op == "persona") {
			continue
		}
		w.apply(s)
	}
	return w
}

func (w *s2World) applyAll() {
	w.t.Helper()
	for i, s := range w.fx.Steps {
		w.t.Logf("step %02d %s %s %s", i, s.Op, s.MetadataID, s.Library)
		w.apply(s)
	}
}

// apply runs one step. A conflict step must fail with ErrIdentityConflict, write no counted row and leave only its hold.
func (w *s2World) apply(s s2Step) {
	t := w.t
	t.Helper()
	before := tableCounts(t, w.db)
	ctx := t.Context()
	var id TitleID
	var err error
	switch s.Op {
	case "membership":
		var r MembershipResult
		r, err = w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs[s.Library], MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name})
		id = r.TitleID
	case "persona":
		var r MembershipResult
		r, err = w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs[s.Library], Persona: s.Persona, MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name})
		id = r.TitleID
	case "import":
		var r ImportResult
		r, err = w.lib.ImportSource(ctx, SourceRecord{InstanceID: s.Instance, SourceID: s.SourceID, LibraryID: w.libs[s.Library],
			MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name, SlotKey: s.SlotKey})
		id = r.TitleID
	case "episodes":
		var eps []EpisodeSpec
		for _, se := range s.Seasons {
			for n := 1; n <= se.Episodes; n++ {
				eps = append(eps, EpisodeSpec{Season: se.Season, Number: n, AirPrecision: "unknown"})
			}
		}
		err = w.lib.AddEpisodes(ctx, w.titles[s.MetadataID], eps)
	case "slot_episode":
		err = w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: w.slotID(s.MetadataID, s.SlotKey), Season: s.Season, Number: s.Number,
			Monitored: s.Monitored, State: s.State, SizeBytes: s.SizeBytes})
	default:
		t.Fatalf("unknown fixture op %q", s.Op)
	}
	if s.Conflict {
		if !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("%s %s: err = %v, want ErrIdentityConflict", s.Op, s.MetadataID, err)
		}
		if after := tableCounts(t, w.db); !maps.Equal(before, after) {
			t.Fatalf("%s %s: conflict wrote rows: before %v after %v", s.Op, s.MetadataID, before, after)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s %s: %v", s.Op, s.MetadataID, err)
	}
	if id != 0 {
		w.titles[s.MetadataID] = id
	}
}

func (w *s2World) slots(metadataID string) []Slot {
	w.t.Helper()
	slots, err := w.lib.TitleSlots(w.t.Context(), w.titles[metadataID])
	if err != nil {
		w.t.Fatalf("TitleSlots %s: %v", metadataID, err)
	}
	return slots
}

func (w *s2World) slotID(metadataID, key string) SlotID {
	w.t.Helper()
	for _, s := range w.slots(metadataID) {
		if s.Key == key {
			return s.ID
		}
	}
	w.t.Fatalf("no slot %s/%s", metadataID, key)
	return 0
}

var countedTables = []string{"title", "library", "library_title", "slot", "episode", "slot_episode", "sync_map", "library_template", "persona"}

func tableCounts(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	out := make(map[string]int, len(countedTables))
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		for _, name := range countedTables {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM " + name).Scan(&n); err != nil {
				return err
			}
			out[name] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return out
}

// queryInt runs a one-value query.
func queryInt(t *testing.T, db *store.DB, q string, args ...any) int {
	t.Helper()
	var n int
	err := db.Read(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow(q, args...).Scan(&n) })
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// dump returns every row of q as one JSON line, sorted, for before/after comparison.
func dump(t *testing.T, db *store.DB, q string, args ...any) []string {
	t.Helper()
	var out []string
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			line, err := json.Marshal(vals)
			if err != nil {
				return err
			}
			out = append(out, string(line))
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	slices.Sort(out)
	return out
}

// snapshot returns the full contents of every counted table.
func snapshot(t *testing.T, db *store.DB) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(countedTables))
	for _, name := range countedTables {
		out[name] = dump(t, db, "SELECT * FROM "+name)
	}
	return out
}

// memberships lists every (library name, metadata_id) pair of library_title.
func memberships(t *testing.T, db *store.DB) []string {
	t.Helper()
	return dump(t, db, `SELECT l.name, t.metadata_id FROM library_title lt
		JOIN library l ON l.id = lt.library_id JOIN title t ON t.id = lt.title_id`)
}
