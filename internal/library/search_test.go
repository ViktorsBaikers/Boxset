package library

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The search oracle reads title, title_alias and alias_generation with its own
// SQL and decides a hit in Go: a title matches when its lowercased name, or
// the lowercased text of a manual alias or an alias of an active generation,
// contains the lowercased term. It calls nothing from search.go and never
// reads title_search. Fixture terms are ASCII, so strings.ToLower folds as the
// trigram tokenizer does.

type searchSource struct {
	title int64
	text  string
}

// searchSources returns every text a search may match, from the base tables.
func searchSources(ctx context.Context, q oracleQueryer) ([]searchSource, error) {
	var out []searchSource
	scan := func(rows *sql.Rows) error {
		var s searchSource
		if err := rows.Scan(&s.title, &s.text); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	}
	if err := oracleCollect(ctx, q, `SELECT id, name FROM title`, scan); err != nil {
		return nil, err
	}
	err := oracleCollect(ctx, q, `SELECT title_id, alias FROM title_alias
		WHERE generation_id IS NULL OR generation_id IN (SELECT id FROM alias_generation WHERE state = 'active')`, scan)
	return out, err
}

func oracleSearch(sources []searchSource, term string) []TitleID {
	want := strings.ToLower(term)
	var out []TitleID
	for _, s := range sources {
		if strings.Contains(strings.ToLower(s.text), want) && !slices.Contains(out, TitleID(s.title)) {
			out = append(out, TitleID(s.title))
		}
	}
	slices.Sort(out)
	return out
}

type searchWorld struct {
	t    *testing.T
	db   *store.DB
	lib  *Library
	libr LibraryID
	next int
}

func newSearchWorld(t *testing.T) *searchWorld {
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
	w := &searchWorld{t: t, db: db, lib: New(db)}
	w.libr, err = w.lib.AddLibrary(t.Context(), LibrarySpec{Name: "Movies",
		Defaults: LibraryDefaults{NamingLanguage: "en", SearchLanguage: "en", Profile: "p"}, Template: []TemplateSlot{{Key: "hd", Primary: true}}})
	if err != nil {
		t.Fatalf("AddLibrary: %v", err)
	}
	return w
}

func (w *searchWorld) addTitle(name string) TitleID {
	w.t.Helper()
	w.next++
	r, err := w.lib.AddMembership(w.t.Context(), MembershipRequest{LibraryID: w.libr, MetadataID: "tmdb:" + strconv.Itoa(w.next), Kind: "movie", Name: name})
	if err != nil {
		w.t.Fatalf("AddMembership %q: %v", name, err)
	}
	return r.TitleID
}

func (w *searchWorld) aliases(id TitleID, gen *GenerationID, aliases ...string) {
	w.t.Helper()
	if err := w.lib.SetAliases(w.t.Context(), id, gen, aliases); err != nil {
		w.t.Fatalf("SetAliases %d %q: %v", id, aliases, err)
	}
}

func (w *searchWorld) stage(dataset string) GenerationID {
	w.t.Helper()
	g, err := w.lib.StageGeneration(w.t.Context(), dataset)
	if err != nil {
		w.t.Fatalf("StageGeneration: %v", err)
	}
	return g
}

// want fails unless Search(term) is exactly ids and the oracle agrees.
func (w *searchWorld) want(term string, ids ...TitleID) {
	w.t.Helper()
	got, err := w.lib.Search(w.t.Context(), term, 1000)
	if err != nil {
		w.t.Fatalf("Search %q: %v", term, err)
	}
	if !slices.Equal(got, slices.Sorted(slices.Values(ids))) {
		w.t.Fatalf("Search %q = %v, want %v", term, got, ids)
	}
	w.oracle(term)
}

// oracle fails unless Search(term) equals the oracle's answer from the base rows.
func (w *searchWorld) oracle(terms ...string) {
	w.t.Helper()
	ctx := w.t.Context()
	var src []searchSource
	if err := w.db.Read(ctx, func(tx *sql.Tx) (err error) { src, err = searchSources(ctx, tx); return }); err != nil {
		w.t.Fatalf("oracle sources: %v", err)
	}
	for _, term := range terms {
		got, err := w.lib.Search(ctx, term, 1_000_000)
		if err != nil {
			w.t.Fatalf("Search %q: %v", term, err)
		}
		if want := oracleSearch(src, term); !slices.Equal(got, want) {
			w.t.Fatalf("Search %q = %v, oracle = %v", term, got, want)
		}
	}
}

// integrityRank1 asks FTS5 to compare the index with the content rows. Without
// the rank, an external-content table checks only its own structures and
// never reads the content, so a missing posting goes unseen.
const (
	integrityRank1 = `INSERT INTO title_search (title_search, rank) VALUES ('integrity-check', 1)`
	integrityOwn   = `INSERT INTO title_search (title_search) VALUES ('integrity-check')`
)

func (w *searchWorld) integrityErr(stmt string) error {
	ctx := w.t.Context()
	return w.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	})
}

// integrity fails unless the index equals its content rows, a stale or missing
// index row is an error, and every generation's alias_count equals the aliases
// it holds.
func (w *searchWorld) integrity() {
	w.t.Helper()
	if err := w.integrityErr(integrityRank1); err != nil {
		w.t.Fatalf("title_search integrity-check against content: %v", err)
	}
	if n := queryInt(w.t, w.db, `SELECT count(*) FROM alias_generation g
		WHERE g.alias_count <> (SELECT count(*) FROM title_alias a WHERE a.generation_id = g.id)`); n != 0 {
		w.t.Fatalf("%d generations whose alias_count differs from their aliases", n)
	}
}

// TestSearchConsistency: after each rename, alias change, deletion, activation
// and purge batch the answer equals the oracle, with no stale hit; then seeded
// random sequences of the same operations.
func TestSearchConsistency(t *testing.T) {
	t.Run("edits", func(t *testing.T) {
		w := newSearchWorld(t)
		ctx := t.Context()
		a, b := w.addTitle("Alpha Show"), w.addTitle("Bravo Show")
		w.want("alpha", a)
		w.want("show", a, b)
		w.want("ALPHA SHOW", a) // case is ignored
		w.want("lpha sho", a)   // a substring, not a prefix

		if err := w.lib.RenameTitle(ctx, a, "Charlie Film"); err != nil {
			t.Fatal(err)
		}
		w.want("alpha")
		w.want("charlie", a)
		w.want("show", b)

		w.aliases(a, nil, "nickname one", "second name")
		w.want("nickname", a)
		w.aliases(a, nil, "second name") // the first alias is gone
		w.want("nickname")
		w.want("second", a)
		w.aliases(a, nil, "second name", "second name") // duplicates collapse
		if n := queryInt(t, w.db, `SELECT count(*) FROM title_alias WHERE title_id = ?`, int64(a)); n != 1 {
			t.Fatalf("duplicate aliases stored %d rows, want 1", n)
		}
		w.aliases(a, nil) // an empty set clears it
		w.want("second")

		w.aliases(b, nil, "bee alias")
		if err := w.lib.DeleteTitle(ctx, b); err != nil {
			t.Fatal(err)
		}
		w.want("bravo")
		w.want("bee alias")
		w.want("show")
		if n := queryInt(t, w.db, `SELECT count(*) FROM title_alias`); n != 0 {
			t.Fatalf("title_alias rows after deletion = %d, want 0", n)
		}
		w.integrity()

		if err := w.lib.RenameTitle(ctx, 9999, "Unknown Title"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RenameTitle of an unknown title = %v, want ErrNotFound", err)
		}
		if err := w.lib.SetAliases(ctx, 9999, nil, []string{"unknown"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetAliases of an unknown title = %v, want ErrNotFound", err)
		}
		for _, bad := range []string{"", strings.Repeat("n", maxName+1), "bad\nname"} {
			if err := w.lib.RenameTitle(ctx, a, bad); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("RenameTitle %q = %v, want ErrInvalidInput", bad, err)
			}
		}
		for _, bad := range []string{"ab", "", strings.Repeat("a", maxAlias+1), "bad\talias"} {
			if err := w.lib.SetAliases(ctx, a, nil, []string{"good alias", bad}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("SetAliases with %q = %v, want ErrInvalidInput", bad, err)
			}
		}
		w.want("good alias") // a rejected set wrote nothing
		w.want("charlie", a)
	})

	t.Run("limit", func(t *testing.T) {
		w := newSearchWorld(t)
		ids := []TitleID{w.addTitle("Same One"), w.addTitle("Same Two"), w.addTitle("Same Three")}
		w.aliases(ids[0], nil, "same alias") // one title reached by name and alias is one hit
		got, err := w.lib.Search(t.Context(), "same", 2)
		if err != nil || !slices.Equal(got, ids[:2]) {
			t.Fatalf("Search limit 2 = %v, %v; want %v", got, err, ids[:2])
		}
		w.want("same", ids...)
	})

	seeds := []uint64{1, 2, 3}
	if s := os.Getenv(oracleSeedEnv); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: %v", oracleSeedEnv, s, err)
		}
		seeds = []uint64{v}
	}
	ops := 250
	if testing.Short() {
		ops = 80
	}
	for _, seed := range seeds {
		t.Run("random/seed"+strconv.FormatUint(seed, 10), func(t *testing.T) { searchRandomRun(t, seed, ops) })
	}
}

// searchWords is the vocabulary of random names, aliases and probe terms. It
// holds FTS syntax, so a hit that treated a term as a query would differ from
// the oracle.
var searchWords = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"ab*cd", `x"yz`, "OR", "-neo", "col:on", "NEAR(a b", "AND", "(x)"}

type genModel struct{ dataset, state string }

func searchRandomRun(t *testing.T, seed uint64, ops int) {
	w := newSearchWorld(t)
	ctx := t.Context()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	t.Logf("seed %d (replay with %s=%d), %d operations", seed, oracleSeedEnv, seed, ops)

	words := func() string {
		parts := make([]string, 1+rng.IntN(3))
		for i := range parts {
			parts[i] = searchWords[rng.IntN(len(searchWords))]
			if rng.IntN(4) == 0 {
				parts[i] = strings.ToUpper(parts[i])
			}
		}
		return strings.Join(parts, " ")
	}
	pad := func(s string) string { // an alias needs 3 bytes
		for len(s) < minTerm {
			s += "_"
		}
		return s
	}
	aliasSet := func() []string {
		set := make([]string, rng.IntN(4))
		for i := range set {
			set[i] = pad(words())
		}
		return set
	}

	var titles []TitleID
	gens := map[GenerationID]*genModel{}
	var genIDs []GenerationID
	for range 4 {
		titles = append(titles, w.addTitle(words()))
	}
	counts := map[string]int{}

	// The probes never change: every word, an upper-cased and a clipped form of
	// it, and the FTS-syntax shapes a query parser would act on.
	probes := []string{"alpha bravo", "alpha OR bravo", `"alpha"`, "alp*", "-alp", "NEAR(alpha bravo)", "alpha AND bravo", `a"b`, "OR OR", "col:on", "x*y"}
	for _, word := range searchWords {
		for _, p := range []string{word, strings.ToUpper(word), word[:min(3, len(word))], word[min(1, len(word)-1):]} {
			if len(p) >= minTerm {
				probes = append(probes, p)
			}
		}
	}
	pick := func() GenerationID { return genIDs[rng.IntN(len(genIDs))] }
	staging := func() *GenerationID { // a generation the model calls staging, else any
		var cand []GenerationID
		for _, g := range genIDs {
			if gens[g].state == "staging" {
				cand = append(cand, g)
			}
		}
		if len(cand) == 0 || rng.IntN(8) == 0 {
			g := pick()
			return &g
		}
		return &cand[rng.IntN(len(cand))]
	}
	rejected := func(err error, valid bool, what string) {
		t.Helper()
		switch {
		case valid && err != nil:
			t.Fatalf("seed %d: %s failed: %v", seed, what, err)
		case !valid && !errors.Is(err, ErrInvalidInput):
			t.Fatalf("seed %d: %s = %v, want ErrInvalidInput", seed, what, err)
		}
	}

	for i := 1; i <= ops; i++ {
		var name string
		switch roll := rng.IntN(100); {
		case roll < 12:
			name = "add title"
			titles = append(titles, w.addTitle(words()))
		case roll < 27 && len(titles) > 0:
			name = "rename"
			if err := w.lib.RenameTitle(ctx, titles[rng.IntN(len(titles))], words()); err != nil {
				t.Fatalf("seed %d op %d rename: %v", seed, i, err)
			}
		case roll < 42 && len(titles) > 0:
			name = "set manual aliases"
			if err := w.lib.SetAliases(ctx, titles[rng.IntN(len(titles))], nil, aliasSet()); err != nil {
				t.Fatalf("seed %d op %d set manual aliases: %v", seed, i, err)
			}
		case roll < 57 && len(titles) > 0 && len(genIDs) > 0:
			name = "set generation aliases"
			g := staging()
			err := w.lib.SetAliases(ctx, titles[rng.IntN(len(titles))], g, aliasSet())
			rejected(err, gens[*g].state == "staging", name)
		case roll < 64 && len(gens) < 12:
			name = "stage generation"
			ds := "ds" + strconv.Itoa(rng.IntN(2))
			g := w.stage(ds)
			gens[g] = &genModel{ds, "staging"}
			genIDs = append(genIDs, g)
		case roll < 76 && len(genIDs) > 0:
			name = "activate generation"
			g := *staging()
			m := gens[g]
			err := w.lib.ActivateGeneration(ctx, g)
			rejected(err, m.state == "staging", name)
			if err == nil {
				for _, o := range gens {
					if o.dataset == m.dataset && o.state == "active" {
						o.state = "retired"
					}
				}
				m.state = "active"
			}
		case roll < 90 && len(genIDs) > 0:
			name = "purge batch"
			g := pick()
			m := gens[g]
			left, err := w.lib.PurgeGeneration(ctx, g, 1+rng.IntN(3))
			rejected(err, m.state != "active", name)
			if err == nil {
				m.state = "retired"
				if n := queryInt(t, w.db, `SELECT count(*) FROM title_alias WHERE generation_id = ?`, int64(g)); n != left {
					t.Fatalf("seed %d op %d: purge reported %d remaining, table has %d", seed, i, left, n)
				}
			}
		case len(titles) > 1:
			name = "delete title"
			j := rng.IntN(len(titles))
			if err := w.lib.DeleteTitle(ctx, titles[j]); err != nil {
				t.Fatalf("seed %d op %d delete title: %v", seed, i, err)
			}
			titles = slices.Delete(titles, j, j+1)
		default:
			name = "add title"
			titles = append(titles, w.addTitle(words()))
		}
		counts[name]++

		// Every commit is followed by the full comparison, plus probes cut from the live texts.
		var live []searchSource
		if err := w.db.Read(ctx, func(tx *sql.Tx) (err error) { live, err = searchSources(ctx, tx); return }); err != nil {
			t.Fatal(err)
		}
		terms := slices.Clone(probes)
		for range 3 {
			if len(live) == 0 {
				break
			}
			s := live[rng.IntN(len(live))].text
			if from := rng.IntN(len(s)); len(s)-from >= minTerm {
				terms = append(terms, s[from:from+minTerm+rng.IntN(len(s)-from-minTerm+1)])
			}
		}
		func() {
			defer func() {
				if t.Failed() {
					t.Logf("seed %d failed after operation %d (%s)", seed, i, name)
				}
			}()
			w.oracle(terms...)
			w.integrity()
		}()
	}
	t.Logf("seed %d: %d operations each followed by the oracle comparison and an FTS integrity check, %d titles left, %d generations, mix %v",
		seed, ops, len(titles), len(gens), counts)
}

// TestGenerationActivation: staged aliases are invisible until activation,
// which switches atomically; a purge leaves every answer as it was; a
// generation that is not staging or not active is refused.
func TestGenerationActivation(t *testing.T) {
	w := newSearchWorld(t)
	ctx := t.Context()
	a, b := w.addTitle("Alpha Show"), w.addTitle("Bravo Show")
	w.aliases(a, nil, "manual alias")

	g1 := w.stage("titles")
	w.aliases(b, &g1, "zulu one", "zulu two")
	w.want("zulu") // staged: invisible
	w.want("manual alias", a)

	if err := w.lib.ActivateGeneration(ctx, g1); err != nil {
		t.Fatal(err)
	}
	w.want("zulu", b)

	g2 := w.stage("titles")
	w.aliases(a, &g2, "yankee one")
	w.aliases(b, &g2, "yankee two", "zulu three")
	w.want("yankee") // g2 staged: still invisible
	w.want("zulu one", b)
	w.want("zulu three")

	// A failure after the old generation is retired rolls the switch back whole.
	probe := func(sqlText string) {
		t.Helper()
		if err := w.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, sqlText); return err }); err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
	}
	probe(`CREATE TRIGGER activation_probe BEFORE UPDATE ON alias_generation WHEN NEW.state = 'active' BEGIN SELECT RAISE(ABORT, 'probe'); END`)
	if err := w.lib.ActivateGeneration(ctx, g2); err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("ActivateGeneration with a failing second step = %v, want the probe failure", err)
	}
	w.want("zulu one", b)
	w.want("yankee")
	if n := queryInt(t, w.db, `SELECT count(*) FROM alias_generation WHERE state = 'active'`); n != 1 {
		t.Fatalf("%d active generations after a failed activation, want 1", n)
	}
	probe(`DROP TRIGGER activation_probe`)

	if err := w.lib.ActivateGeneration(ctx, g2); err != nil {
		t.Fatal(err)
	}
	w.want("yankee", a, b)
	w.want("zulu one") // g1 retired in the same commit
	w.want("zulu three", b)
	w.want("manual alias", a)
	if n := queryInt(t, w.db, `SELECT count(*) FROM alias_generation WHERE dataset = 'titles' AND state = 'active'`); n != 1 {
		t.Fatalf("%d active generations, want 1", n)
	}

	// Purge in batches: g1's two aliases leave one batch at a time, answers unchanged.
	for _, wantLeft := range []int{1, 0, 0} {
		left, err := w.lib.PurgeGeneration(ctx, g1, 1)
		if err != nil || left != wantLeft {
			t.Fatalf("PurgeGeneration g1 = %d, %v; want %d remaining", left, err, wantLeft)
		}
		w.want("zulu three", b)
		w.want("zulu one")
		w.want("yankee", a, b)
		w.integrity()
	}

	// Refused: a generation in the wrong state, an unknown one, a bad batch.
	if err := w.lib.SetAliases(ctx, a, &g2, []string{"late alias"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SetAliases on an active generation = %v, want ErrInvalidInput", err)
	}
	if err := w.lib.SetAliases(ctx, a, &g1, []string{"late alias"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SetAliases on a retired generation = %v, want ErrInvalidInput", err)
	}
	if err := w.lib.ActivateGeneration(ctx, g2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ActivateGeneration of an active generation = %v, want ErrInvalidInput", err)
	}
	if err := w.lib.ActivateGeneration(ctx, g1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ActivateGeneration of a retired generation = %v, want ErrInvalidInput", err)
	}
	if _, err := w.lib.PurgeGeneration(ctx, g2, 5); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PurgeGeneration of an active generation = %v, want ErrInvalidInput", err)
	}
	if _, err := w.lib.PurgeGeneration(ctx, g1, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PurgeGeneration batch 0 = %v, want ErrInvalidInput", err)
	}
	if _, err := w.lib.PurgeGeneration(ctx, 999, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PurgeGeneration of an unknown generation = %v, want ErrNotFound", err)
	}
	if err := w.lib.ActivateGeneration(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActivateGeneration of an unknown generation = %v, want ErrNotFound", err)
	}
	if err := w.lib.SetAliases(ctx, a, new(GenerationID), []string{"late alias"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAliases on an unknown generation = %v, want ErrNotFound", err)
	}
	if _, err := w.lib.StageGeneration(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("StageGeneration of an empty dataset = %v, want ErrInvalidInput", err)
	}

	// An abandoned staging generation is purged and retired; its aliases never show.
	g3 := w.stage("titles")
	w.aliases(a, &g3, "abandoned one")
	if left, err := w.lib.PurgeGeneration(ctx, g3, 10); err != nil || left != 0 {
		t.Fatalf("PurgeGeneration of a staging generation = %d, %v", left, err)
	}
	if err := w.lib.SetAliases(ctx, a, &g3, []string{"after purge"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SetAliases on a purged generation = %v, want ErrInvalidInput", err)
	}
	w.want("abandoned")

	// A second dataset keeps its own active generation.
	h := w.stage("other")
	w.aliases(a, &h, "other alias")
	if err := w.lib.ActivateGeneration(ctx, h); err != nil {
		t.Fatal(err)
	}
	w.want("other alias", a)
	w.want("yankee", a, b)
	if n := queryInt(t, w.db, `SELECT count(*) FROM alias_generation WHERE state = 'active'`); n != 2 {
		t.Fatalf("%d active generations over two datasets, want 2", n)
	}

	// A deleted title takes its generation aliases with it.
	if err := w.lib.DeleteTitle(ctx, b); err != nil {
		t.Fatal(err)
	}
	w.want("zulu three")
	w.want("yankee", a)
	w.integrity()
}

// TestSearchConsistencyIntegrityCheck: with a posting missing from the index,
// the check that compares content fails while the table's own check passes.
func TestSearchConsistencyIntegrityCheck(t *testing.T) {
	w := newSearchWorld(t)
	w.aliases(w.addTitle("Alpha Show"), nil, "manual alias")
	w.integrity()
	if err := w.integrityErr(`INSERT INTO title_search (title_search, rowid, term) SELECT 'delete', id, alias FROM title_alias`); err != nil {
		t.Fatalf("plant a missing posting: %v", err)
	}
	if err := w.integrityErr(integrityOwn); err != nil {
		t.Fatalf("the table's own integrity-check = %v, want it to pass", err)
	}
	if err := w.integrityErr(integrityRank1); err == nil {
		t.Fatal("integrity-check against content passed with a posting missing")
	}
}

// TestGenerationActivationPurgeCount: alias_count follows replacement, purge
// and title deletion, and a purge batch reports it instead of counting the
// aliases that remain.
func TestGenerationActivationPurgeCount(t *testing.T) {
	w := newSearchWorld(t)
	ctx := t.Context()
	const perTitle = 100
	var titles []TitleID
	for i := range 30 {
		titles = append(titles, w.addTitle("Title "+strconv.Itoa(i)))
	}
	big := w.stage("big")
	for i, id := range titles {
		var as []string
		for j := range perTitle {
			as = append(as, "term "+strconv.Itoa(i)+"-"+strconv.Itoa(j))
		}
		w.aliases(id, &big, as...)
	}
	count := func(g GenerationID) int {
		return queryInt(t, w.db, `SELECT alias_count FROM alias_generation WHERE id = ?`, int64(g))
	}
	if n := count(big); n != 30*perTitle {
		t.Fatalf("alias_count after staging = %d, want %d", n, 30*perTitle)
	}
	w.aliases(titles[0], &big, "term 0-0", "term 0-1") // replacement: 100 become 2
	if n := count(big); n != 29*perTitle+2 {
		t.Fatalf("alias_count after a replacement = %d, want %d", n, 29*perTitle+2)
	}
	w.integrity()
	if err := w.lib.ActivateGeneration(ctx, big); err != nil {
		t.Fatal(err)
	}
	w.want("term 5-7", titles[5])

	next := w.stage("big")
	w.aliases(titles[1], &next, "next one")
	if err := w.lib.ActivateGeneration(ctx, next); err != nil { // big is retired
		t.Fatal(err)
	}
	w.want("term 5-7")
	w.want("next one", titles[1])
	if err := w.lib.DeleteTitle(ctx, titles[29]); err != nil { // its 100 aliases go with it
		t.Fatal(err)
	}
	left := 28*perTitle + 2
	if n := count(big); n != left {
		t.Fatalf("alias_count after a title deletion = %d, want %d", n, left)
	}
	w.integrity()

	for left > 0 {
		left = max(left-700, 0)
		got, err := w.lib.PurgeGeneration(ctx, big, 700)
		if err != nil || got != left {
			t.Fatalf("PurgeGeneration big = %d, %v; want %d remaining", got, err, left)
		}
		w.want("term 5-7")
		w.want("next one", titles[1])
		w.integrity()
	}
	if n := queryInt(t, w.db, `SELECT count(*) FROM title_alias WHERE generation_id = ?`, int64(big)); n != 0 {
		t.Fatalf("%d aliases left after the purge", n)
	}

	// The remaining count comes from the maintained value: plant 1000 extra
	// on a small generation and the purge reports them, which a scan of the
	// remaining aliases could not.
	small := w.stage("small")
	w.aliases(titles[2], &small, "small one", "small two", "small three")
	if err := w.integrityErr(`UPDATE alias_generation SET alias_count = alias_count + 1000 WHERE id = ` + strconv.Itoa(int(small))); err != nil {
		t.Fatal(err)
	}
	if got, err := w.lib.PurgeGeneration(ctx, small, 1); err != nil || got != 1002 {
		t.Fatalf("PurgeGeneration small = %d, %v; want 1002 from the maintained count", got, err)
	}
}

// TestGenerationActivationSetAliasesFault: a failure after SetAliases deleted
// the old aliases, inside the same write, leaves the old aliases and their
// hits; with the fault gone the same call replaces them.
func TestGenerationActivationSetAliasesFault(t *testing.T) {
	w := newSearchWorld(t)
	ctx := t.Context()
	a := w.addTitle("Alpha Show")
	w.aliases(a, nil, "old one", "old two")
	if err := w.integrityErr(`CREATE TRIGGER alias_fault BEFORE INSERT ON title_alias WHEN NEW.alias = 'fault now'
		BEGIN SELECT RAISE(ABORT, 'fault'); END`); err != nil {
		t.Fatal(err)
	}
	replacement := []string{"new one", "fault now"} // the first insert succeeds, the second fails
	if err := w.lib.SetAliases(ctx, a, nil, replacement); err == nil || !strings.Contains(err.Error(), "fault") {
		t.Fatalf("SetAliases with a failing insert = %v, want the fault", err)
	}
	w.want("old one", a)
	w.want("old two", a)
	w.want("new one")
	if n := queryInt(t, w.db, `SELECT count(*) FROM title_alias WHERE title_id = ?`, int64(a)); n != 2 {
		t.Fatalf("%d aliases after the failed replacement, want the 2 old ones", n)
	}
	w.integrity()

	if err := w.integrityErr(`DROP TRIGGER alias_fault`); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetAliases(ctx, a, nil, replacement); err != nil {
		t.Fatalf("SetAliases after the fault is removed: %v", err)
	}
	w.want("old one")
	w.want("old two")
	w.want("new one", a)
	w.want("fault now", a)
	w.integrity()
}

// TestSearchTermQuoted: a term holding FTS syntax is matched as text, and a
// term outside 3-128 bytes is rejected.
func TestSearchTermQuoted(t *testing.T) {
	w := newSearchWorld(t)
	ctx := t.Context()
	plain := w.addTitle("plain foo bar")
	or := w.addTitle("foo OR bar")
	quote := w.addTitle(`say "b" now`)
	star := w.addTitle("star* wars")
	near := w.addTitle("NEAR(foo bar) film")
	col := w.addTitle("col:umn one")
	dash := w.addTitle("dash -neo end")
	w.aliases(plain, nil, "foo AND bar")

	w.want("foo OR bar", or)       // not "foo or bar" as a query: plain foo bar is no hit
	w.want("foo AND bar", plain)   // an alias
	w.want(`"b"`, quote)           // the quotes are text
	w.want(`y "b" n`, quote)       // inner quotes doubled, not closing the string
	w.want("star*", star)          // not a prefix query
	w.want("sta*")                 // would match star under prefix syntax
	w.want("NEAR(foo bar)", near)  // not a NEAR query
	w.want("col:umn", col)         // not a column filter
	w.want("-neo", dash)           // not a negation
	w.want("foo bar", plain, near) // case is ignored, terms are phrases
	w.want("(foo", near)
	w.want("^foo")
	w.oracle("foo", "bar", "OR ", `"""`, `x"""`, "*** ", "a*b", "title_search:foo", "term:foo", "foo -bar")

	ok := func(term string) {
		t.Helper()
		if _, err := w.lib.Search(ctx, term, 10); err != nil {
			t.Fatalf("Search %q (%d bytes) = %v, want accepted", term, len(term), err)
		}
	}
	bad := func(term string) {
		t.Helper()
		if got, err := w.lib.Search(ctx, term, 10); !errors.Is(err, ErrInvalidInput) || got != nil {
			t.Fatalf("Search %q (%d bytes) = %v, %v; want ErrInvalidInput", term, len(term), got, err)
		}
	}
	ok("foo")
	ok("\xc3\xa9\xc3\xa9") // 4 bytes, 2 characters: accepted, matches nothing
	edge := strings.Repeat("o", 128)
	w.want(edge, w.addTitle(edge)) // 128 bytes: accepted, finds its title
	for _, term := range []string{"", "f", "fo", "\xc3\xa9", strings.Repeat("o", 129), "foo\x00bar", "\xff\xfe\xfd\xfc"} {
		bad(term)
	}
	if _, err := w.lib.Search(ctx, "foo", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Search limit 0 = %v, want ErrInvalidInput", err)
	}
}

// TestSearchQueryPlan: Search reads through the FTS index and scans neither the
// title nor the alias table.
func TestSearchQueryPlan(t *testing.T) {
	w := newSearchWorld(t)
	w.aliases(w.addTitle("Plan Title"), nil, "plan alias")
	plan := planLines(t, w.db, qSearch, `"plan"`, 10)
	t.Logf("plan:\n%s", strings.Join(plan, "\n"))
	all := strings.Join(plan, "\n")
	if n := strings.Count(all, "VIRTUAL TABLE INDEX"); n != 2 {
		t.Fatalf("plan has %d FTS index reads, want 2 (title rows, alias rows):\n%s", n, all)
	}
	for _, line := range plan {
		if strings.Contains(line, "VIRTUAL TABLE INDEX") && !strings.Contains(line, ":M") {
			t.Fatalf("FTS read without a MATCH constraint: %s", line)
		}
		for _, table := range []string{"title", "title_alias", "alias_generation"} {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "SCAN" && f[1] == table {
				t.Fatalf("plan scans %s: %s", table, line)
			}
		}
	}
	if !strings.Contains(all, "SEARCH a USING INTEGER PRIMARY KEY") {
		t.Fatalf("alias rows are not reached by primary key:\n%s", all)
	}
}
