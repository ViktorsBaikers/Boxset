// Command parentseed fills a stopped installation of the parent binary's schema
// with the synthetic S2 world plus one active and one staging alias generation.
// The REG-ROLLBACK rehearsal builds it inside a captured parent source tree, so
// it uses only the packages that tree exports and is never part of the product
// binary.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ViktorsBaikers/Boxset/internal/library"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "parentseed:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("parentseed", flag.ContinueOnError)
	var database, fixturePath string
	flags.StringVar(&database, "db", "", "the installation's database file")
	flags.StringVar(&fixturePath, "fixture", "", "the S2 fixture JSON")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if database == "" || fixturePath == "" || flags.NArg() != 0 {
		return errors.New("usage: parentseed -db FILE -fixture FILE")
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		return err
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		return fmt.Errorf("decode %s: %w", fixturePath, err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, database, store.Options{})
	if err != nil {
		return fmt.Errorf("open %s: %w", database, err)
	}
	if err := seed(ctx, db, fx); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}

// fixture is the part of testdata/store/s2-fixture.json this seed applies.
type fixture struct {
	Libraries []fixtureLibrary `json:"libraries"`
	Personas  []fixturePersona `json:"personas"`
	Steps     []fixtureStep    `json:"steps"`
	Expect    struct {
		Titles int `json:"titles"`
	} `json:"expect"`
}

type fixtureLibrary struct {
	Name           string `json:"name"`
	Monitored      bool   `json:"monitored"`
	NamingLanguage string `json:"naming_language"`
	SearchLanguage string `json:"search_language"`
	Profile        string `json:"profile"`
	Template       []struct {
		Key     string `json:"key"`
		Primary bool   `json:"primary"`
	} `json:"template"`
}

type fixturePersona struct {
	Key     string `json:"key"`
	SlotKey string `json:"slot_key"`
}

type fixtureStep struct {
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

// The alias generations the rehearsal activates: both belong to the one dataset
// an activation replaces, so activating the second retires the first and
// enqueues its purge.
const (
	aliasDataset = "dataset-a"
	aliasTitle   = "tmdb:603"
)

var (
	activeAliases  = []string{"synthetic alpha one", "synthetic alpha two"}
	stagingAliases = []string{"synthetic bravo one"}
)

func seed(ctx context.Context, db *store.DB, fx fixture) error {
	lib := library.New(db)
	libraries := make(map[string]library.LibraryID, len(fx.Libraries))
	for _, l := range fx.Libraries {
		spec := library.LibrarySpec{Name: l.Name, Defaults: library.LibraryDefaults{
			Monitored: l.Monitored, NamingLanguage: l.NamingLanguage, SearchLanguage: l.SearchLanguage, Profile: l.Profile,
		}}
		for _, t := range l.Template {
			spec.Template = append(spec.Template, library.TemplateSlot{Key: t.Key, Primary: t.Primary})
		}
		id, err := lib.AddLibrary(ctx, spec)
		if err != nil {
			return fmt.Errorf("library %s: %w", l.Name, err)
		}
		libraries[l.Name] = id
	}
	for _, p := range fx.Personas {
		if err := lib.AddPersona(ctx, p.Key, p.SlotKey); err != nil {
			return fmt.Errorf("persona %s: %w", p.Key, err)
		}
	}
	titles := make(map[string]library.TitleID, len(fx.Steps))
	conflicts := 0
	for i, step := range fx.Steps {
		if err := apply(ctx, lib, libraries, titles, step); err != nil {
			return fmt.Errorf("step %02d %s %s: %w", i, step.Op, step.MetadataID, err)
		}
		if step.Conflict {
			conflicts++
		}
	}
	count, err := count(ctx, db, "SELECT count(*) FROM title")
	if err != nil {
		return err
	}
	if count != int64(fx.Expect.Titles) {
		return fmt.Errorf("fixture titles = %d, want %d", count, fx.Expect.Titles)
	}
	title, ok := titles[aliasTitle]
	if !ok {
		return fmt.Errorf("the fixture created no title %s", aliasTitle)
	}
	active, activeCount, err := generation(ctx, db, lib, title, activeAliases, true)
	if err != nil {
		return err
	}
	staging, stagingCount, err := generation(ctx, db, lib, title, stagingAliases, false)
	if err != nil {
		return err
	}
	fmt.Printf("PARENTSEED titles=%d steps=%d conflicts=%d\n", count, len(fx.Steps), conflicts)
	fmt.Printf("PARENTSEED generation=%s id=%d state=active aliases=%d\n", aliasDataset, active, activeCount)
	fmt.Printf("PARENTSEED generation=%s id=%d state=staging aliases=%d\n", aliasDataset, staging, stagingCount)
	fmt.Println("PARENTSEED PASS")
	return nil
}

// generation stages one alias generation of title, fills it with aliases and,
// when asked, activates it. It returns the generation and the alias count the
// schema recorded for it.
func generation(ctx context.Context, db *store.DB, lib *library.Library, title library.TitleID, aliases []string, activate bool) (library.GenerationID, int64, error) {
	id, err := lib.StageGeneration(ctx, aliasDataset)
	if err != nil {
		return 0, 0, fmt.Errorf("stage %s: %w", aliasDataset, err)
	}
	if err := lib.SetAliases(ctx, title, &id, aliases); err != nil {
		return 0, 0, fmt.Errorf("aliases of generation %d: %w", id, err)
	}
	if activate {
		if err := lib.ActivateGeneration(ctx, id); err != nil {
			return 0, 0, fmt.Errorf("activate generation %d: %w", id, err)
		}
	}
	count, err := count(ctx, db, "SELECT alias_count FROM alias_generation WHERE id = ?", int64(id))
	if err != nil {
		return 0, 0, err
	}
	if count < 1 {
		return 0, 0, fmt.Errorf("generation %d holds %d aliases", id, count)
	}
	return id, count, nil
}

// apply runs one fixture step. A conflict step must be refused as an identity
// conflict: nothing it names is written.
func apply(ctx context.Context, lib *library.Library, libraries map[string]library.LibraryID, titles map[string]library.TitleID, s fixtureStep) error {
	var id library.TitleID
	var err error
	switch s.Op {
	case "membership":
		var result library.MembershipResult
		result, err = lib.AddMembership(ctx, library.MembershipRequest{LibraryID: libraries[s.Library], MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name})
		id = result.TitleID
	case "persona":
		var result library.MembershipResult
		result, err = lib.ApplyPersona(ctx, library.PersonaRequest{LibraryID: libraries[s.Library], Persona: s.Persona, MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name})
		id = result.TitleID
	case "import":
		var result library.ImportResult
		result, err = lib.ImportSource(ctx, library.SourceRecord{InstanceID: s.Instance, SourceID: s.SourceID, LibraryID: libraries[s.Library],
			MetadataID: s.MetadataID, Kind: s.Kind, Name: s.Name, SlotKey: s.SlotKey})
		id = result.TitleID
	case "episodes":
		var episodes []library.EpisodeSpec
		for _, season := range s.Seasons {
			for number := 1; number <= season.Episodes; number++ {
				episodes = append(episodes, library.EpisodeSpec{Season: season.Season, Number: number, AirPrecision: "unknown"})
			}
		}
		err = lib.AddEpisodes(ctx, titles[s.MetadataID], episodes)
	case "slot_episode":
		var slot library.SlotID
		slot, err = slotID(ctx, lib, titles[s.MetadataID], s.SlotKey)
		if err == nil {
			err = lib.SetSlotEpisode(ctx, library.SlotEpisodeChange{SlotID: slot, Season: s.Season, Number: s.Number,
				Monitored: s.Monitored, State: s.State, SizeBytes: s.SizeBytes})
		}
	default:
		return fmt.Errorf("unknown op %q", s.Op)
	}
	if s.Conflict {
		if !errors.Is(err, library.ErrIdentityConflict) {
			return fmt.Errorf("want ErrIdentityConflict, got %v", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if id != 0 {
		titles[s.MetadataID] = id
	}
	return nil
}

// slotID is the id of one named slot of a title.
func slotID(ctx context.Context, lib *library.Library, title library.TitleID, key string) (library.SlotID, error) {
	slots, err := lib.TitleSlots(ctx, title)
	if err != nil {
		return 0, err
	}
	for _, slot := range slots {
		if slot.Key == key {
			return slot.ID, nil
		}
	}
	return 0, fmt.Errorf("title %d holds no slot %q", title, key)
}

// count reads one integer through the store's read transaction.
func count(ctx context.Context, db *store.DB, query string, args ...any) (int64, error) {
	var n int64
	err := db.Read(ctx, func(tx *sql.Tx) error { return tx.QueryRowContext(ctx, query, args...).Scan(&n) })
	return n, err
}
