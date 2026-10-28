package library

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

// allStates is every media state the schema's CHECK constraints accept.
var allStates = []string{"missing", "on_disk", "upgrade", "live", "unaired"}

func sameSnapshot(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

func TestS2IdentityReuse(t *testing.T) {
	w := applyS2(t)
	fx := w.fx

	ids := map[string]bool{}
	for _, s := range fx.Steps {
		if !s.Conflict && s.MetadataID != "" {
			ids[s.MetadataID] = true
		}
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM title`); got != len(ids) || got != fx.Expect.Titles {
		t.Fatalf("titles = %d, distinct metadata_ids = %d, fixture says %d", got, len(ids), fx.Expect.Titles)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM (SELECT DISTINCT title_id, slot_key FROM slot)`); got != fx.Expect.Slots {
		t.Fatalf("distinct (title, slot_key) = %d, want %d", got, fx.Expect.Slots)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot`); got != fx.Expect.Slots {
		t.Fatalf("slots = %d, want %d", got, fx.Expect.Slots)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM sync_map`); got != fx.Expect.SyncMap {
		t.Fatalf("sync_map = %d, want %d", got, fx.Expect.SyncMap)
	}

	// Membership, persona and import each record their library_title pair, and a
	// mapped source replayed into another library adds only that membership.
	var wantPairs []string
	for _, p := range fx.Expect.LibraryTitles {
		line, err := json.Marshal([]string{p.Library, p.MetadataID})
		if err != nil {
			t.Fatal(err)
		}
		wantPairs = append(wantPairs, string(line))
	}
	slices.Sort(wantPairs)
	if got := memberships(t, w.db); !slices.Equal(got, wantPairs) {
		t.Fatalf("library_title pairs:\n got %v\nwant %v", got, wantPairs)
	}

	for metadataID, want := range fx.Expect.TitleSlots {
		got := w.slots(metadataID)
		if len(got) != len(want) {
			t.Fatalf("%s: %d slots, want %d: %+v", metadataID, len(got), len(want), got)
		}
		for i, g := range got {
			e := want[i]
			have := s2Slot{g.Key, g.Primary, g.Owner, g.Monitored, g.NamingLanguage, g.SearchLanguage, g.Profile}
			if have != e {
				t.Errorf("%s slot %d = %+v, want %+v", metadataID, i, have, e)
			}
			if g.NamingLanguage == g.SearchLanguage {
				t.Errorf("%s/%s: fixture must use unequal naming and search languages, got %q", metadataID, g.Key, g.NamingLanguage)
			}
			if g.Label != g.Key {
				t.Errorf("%s/%s: label %q, want the key until relabelled", metadataID, g.Key, g.Label)
			}
		}
	}
	for _, e := range fx.Expect.SlotEpisode {
		slot := int64(w.slotID(e.MetadataID, e.SlotKey))
		rows := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ?`, slot)
		mon := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND monitored = 1`, slot)
		disk := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND state = 'on_disk'`, slot)
		if rows != e.Rows || mon != e.Monitored || disk != e.OnDisk {
			t.Errorf("%s/%s rows/monitored/on_disk = %d/%d/%d, want %d/%d/%d", e.MetadataID, e.SlotKey, rows, mon, disk, e.Rows, e.Monitored, e.OnDisk)
		}
	}

	// The conflict steps leave exactly their holds: one per conflicting import on
	// tmdb:603/uhd, and one title-wide hold for the membership that requests tmdb:603 as a series.
	title, uhd := w.titles["tmdb:603"], w.slotID("tmdb:603", "uhd")
	wantHolds := []string{
		fmt.Sprintf("[%q,\"import:movie-uhd/movie/42@slot:%d\",%d,%d]", string(ReasonIdentityConflict), uhd, title, uhd),
		fmt.Sprintf("[%q,\"import:movie-uhd/movie/43@slot:%d\",%d,%d]", string(ReasonIdentityConflict), uhd, title, uhd),
		fmt.Sprintf("[%q,%q,%d,null]", string(ReasonIdentityConflict), enrollScope(title), title),
	}
	slices.Sort(wantHolds)
	if got := dump(t, w.db, `SELECT reason_code, scope_key, title_id, slot_id FROM review_hold`); !slices.Equal(got, wantHolds) {
		t.Fatalf("holds after the fixture = %v, want %v", got, wantHolds)
	}
	// The world without the enrollment step has the same canonical rows and only the import holds.
	eligible := applyS2Eligible(t)
	if got, want := snapshot(t, eligible.db), snapshot(t, w.db); !sameSnapshot(got, want) {
		t.Fatalf("eligible world rows differ from the full fixture:\n got %v\nwant %v", got, want)
	}
	importHolds := dump(t, w.db, `SELECT reason_code, scope_key, title_id, slot_id, params FROM review_hold WHERE scope_key LIKE 'import:%'`)
	if got := holdRows(t, eligible.db); len(importHolds) != 2 || !slices.Equal(got, importHolds) {
		t.Fatalf("eligible world holds = %v, want the two import holds %v", got, importHolds)
	}

	// Replaying every step, conflicts included, changes no row and adds no hold.
	before, held := snapshot(t, w.db), holdRows(t, w.db)
	w.applyAll()
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Fatalf("replay changed rows: before %v after %v", before, after)
	}
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("replay changed holds: %v -> %v", held, got)
	}
}

func TestPersonaAndTemplateShareSlotKey(t *testing.T) {
	w := newS2World(t)
	ctx := t.Context()
	var persona, uhdKey string
	for _, p := range w.fx.Personas {
		persona, uhdKey = p.Key, p.SlotKey
	}
	for _, l := range w.fx.Libraries {
		if l.Name == "UHD" && l.Template[0].Key != uhdKey {
			t.Fatalf("UHD template key %q, persona slot key %q", l.Template[0].Key, uhdKey)
		}
	}

	// Template first, persona second.
	a, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["UHD"], MetadataID: "tmdb:1", Kind: "movie", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Movies"], Persona: persona, MetadataID: "tmdb:1", Kind: "movie", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.SlotIDs, b.SlotIDs) || len(a.SlotIDs) != 1 || a.TitleID != b.TitleID || b.Created {
		t.Fatalf("template %+v and persona %+v must share one slot of one title", a, b)
	}
	// The reused slot keeps the UHD policy, both languages included; the Movies defaults are only reported.
	if s := w.slotsOf(a.TitleID)[0]; s.NamingLanguage != "de" || s.SearchLanguage != "nl" || s.Profile != "UHD-2160p" {
		t.Fatalf("reused slot %+v must keep the UHD policy de/nl", s)
	}
	var fields []string
	for _, d := range b.PolicyDiffs {
		fields = append(fields, d.Field)
		if d.Field == "search_language" && (d.Have != "nl" || d.Want != "fr") {
			t.Errorf("search_language diff = %+v, want have nl, want fr", d)
		}
	}
	if want := []string{"naming_language", "search_language", "profile"}; !slices.Equal(fields, want) {
		t.Fatalf("PolicyDiffs fields = %v, want %v", fields, want)
	}

	// Persona first, template second.
	c, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Movies"], Persona: persona, MetadataID: "tmdb:2", Kind: "movie", Name: "B"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["UHD"], MetadataID: "tmdb:2", Kind: "movie", Name: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.SlotIDs, d.SlotIDs) || len(c.SlotIDs) != 1 {
		t.Fatalf("persona %+v and template %+v must share one slot", c, d)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot`); got != 2 {
		t.Fatalf("slots = %d, want 2", got)
	}

	if _, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Movies"], Persona: "nobody", MetadataID: "tmdb:3", Kind: "movie", Name: "C"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown persona: %v, want ErrNotFound", err)
	}
	if err := w.lib.AddPersona(ctx, persona, uhdKey); err != nil {
		t.Fatalf("same mapping again: %v", err)
	}
	if err := w.lib.AddPersona(ctx, persona, "hd"); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("remapping a persona: %v, want ErrIdentityConflict", err)
	}
}

func TestDistinctEditionIsSeparateSlot(t *testing.T) {
	w := newS2World(t)
	ctx := t.Context()
	first, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:603", Kind: "movie", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Collector"], MetadataID: "tmdb:603", Kind: "movie", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if second.TitleID != first.TitleID || second.Created {
		t.Fatalf("collector membership made a new title: %+v", second)
	}
	// Template order is hd, dc; hd is reused, dc is the distinct edition.
	if len(second.SlotIDs) != 2 || second.SlotIDs[0] != first.SlotIDs[0] || second.SlotIDs[1] == first.SlotIDs[0] {
		t.Fatalf("slot ids %v after %v: hd must be reused and dc distinct", second.SlotIDs, first.SlotIDs)
	}
	var keys []string
	for _, s := range w.slotsOf(first.TitleID) {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"hd", "dc"}) {
		t.Fatalf("keys = %v, want [hd dc]", keys)
	}
}

func (w *s2World) slotsOf(id TitleID) []Slot {
	w.t.Helper()
	slots, err := w.lib.TitleSlots(w.t.Context(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return slots
}

func TestSlotKeyStableAcrossDefaultsAndLabels(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	slotQ := `SELECT id, title_id, slot_key, is_primary, owner, monitored, naming_language, search_language FROM slot`
	epQ := `SELECT slot_id, episode_id, title_id, ord, monitored, state, size_bytes FROM slot_episode`
	slotsBefore, epsBefore := dump(t, w.db, slotQ), dump(t, w.db, epQ)
	titlesBefore := tableCounts(t, w.db)

	// New defaults differ from every existing slot and name different languages.
	for name, id := range w.libs {
		d := LibraryDefaults{Monitored: false, NamingLanguage: "fr", SearchLanguage: "pt", Profile: "Changed-" + name}
		if err := w.lib.SetLibraryDefaults(ctx, id, d); err != nil {
			t.Fatalf("SetLibraryDefaults %s: %v", name, err)
		}
	}
	for metadataID := range w.fx.Expect.TitleSlots {
		for _, s := range w.slots(metadataID) {
			if err := w.lib.RelabelSlot(ctx, s.ID, "Label "+strconv.Itoa(int(s.ID)), "Profile "+strconv.Itoa(int(s.ID))); err != nil {
				t.Fatalf("RelabelSlot %d: %v", s.ID, err)
			}
		}
	}
	// Every library's template keys already exist on tmdb:603: reusing them reports differences and changes nothing.
	for name, id := range w.libs {
		if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: id, MetadataID: "tmdb:603", Kind: "movie", Name: "n"}); err != nil {
			t.Fatalf("AddMembership %s: %v", name, err)
		}
	}

	if got := dump(t, w.db, slotQ); !slices.Equal(got, slotsBefore) {
		t.Fatalf("slot ids, keys or policy changed:\nbefore %v\nafter  %v", slotsBefore, got)
	}
	if got := dump(t, w.db, epQ); !slices.Equal(got, epsBefore) {
		t.Fatal("slot_episode rows changed")
	}
	after := tableCounts(t, w.db)
	for _, tbl := range []string{"title", "slot", "episode", "slot_episode", "sync_map"} {
		if after[tbl] != titlesBefore[tbl] {
			t.Errorf("%s count %d -> %d", tbl, titlesBefore[tbl], after[tbl])
		}
	}
	for _, s := range w.slots("tmdb:603") {
		if want := "Label " + strconv.Itoa(int(s.ID)); s.Label != want || s.Profile != "Profile "+strconv.Itoa(int(s.ID)) {
			t.Errorf("slot %d label/profile = %q/%q, want the relabelled values", s.ID, s.Label, s.Profile)
		}
	}

	// A slot created now takes the changed defaults, naming and search language separately.
	r, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["UHD"], MetadataID: "tmdb:9", Kind: "movie", Name: "New"})
	if err != nil {
		t.Fatal(err)
	}
	s := w.slotsOf(r.TitleID)[0]
	if s.Monitored || s.NamingLanguage != "fr" || s.SearchLanguage != "pt" || s.Profile != "Changed-UHD" {
		t.Fatalf("new slot %+v must take the changed defaults fr/pt", s)
	}
}

func TestPrimarySlotRequired(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	noOne := `SELECT count(*) FROM title t WHERE (SELECT count(*) FROM slot s WHERE s.title_id = t.id AND s.is_primary = 1) <> 1`
	if got := queryInt(t, w.db, noOne); got != 0 {
		t.Fatalf("%d titles without exactly one primary slot", got)
	}

	// A title first seen through a persona gets its primary there; a later template slot is not primary.
	p, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Movies"], Persona: "4k", MetadataID: "tmdb:800", Kind: "movie", Name: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:800", Kind: "movie", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	slots := w.slotsOf(p.TitleID)
	if len(slots) != 2 || !slots[0].Primary || slots[0].Key != "uhd" || slots[1].Primary {
		t.Fatalf("slots = %+v, want uhd primary then hd", slots)
	}
	if got := queryInt(t, w.db, noOne); got != 0 {
		t.Fatalf("%d titles without exactly one primary slot", got)
	}

	// A template with no primary marks its first slot; zero or two primaries are refused.
	d := LibraryDefaults{NamingLanguage: "en", SearchLanguage: "en", Profile: "p"}
	id, err := w.lib.AddLibrary(ctx, LibrarySpec{Name: "NoPrimary", Defaults: d, Template: []TemplateSlot{{Key: "a"}, {Key: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: id, MetadataID: "tmdb:801", Kind: "movie", Name: "Q"})
	if err != nil {
		t.Fatal(err)
	}
	if s := w.slotsOf(r.TitleID); len(s) != 2 || !s[0].Primary || s[0].Key != "a" || s[1].Primary {
		t.Fatalf("slots = %+v, want a primary then b", s)
	}
	before := tableCounts(t, w.db)
	for name, tpl := range map[string][]TemplateSlot{
		"two primaries": {{Key: "a", Primary: true}, {Key: "b", Primary: true}},
		"empty":         nil,
	} {
		if _, err := w.lib.AddLibrary(ctx, LibrarySpec{Name: name, Defaults: d, Template: tpl}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s template: %v, want ErrInvalidInput", name, err)
		}
	}
	if after := tableCounts(t, w.db); !maps.Equal(before, after) {
		t.Fatalf("rejected templates wrote rows: %v -> %v", before, after)
	}
}

func TestPerSlotEpisodeState(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()

	for _, e := range w.fx.Expect.SlotEpisode {
		slot := w.slotID(e.MetadataID, e.SlotKey)
		rows := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ?`, int64(slot))
		mon := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND monitored = 1`, int64(slot))
		disk := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND state = 'on_disk'`, int64(slot))
		if rows != e.Rows || mon != e.Monitored || disk != e.OnDisk {
			t.Errorf("%s/%s rows/monitored/on_disk = %d/%d/%d, want %d/%d/%d", e.MetadataID, e.SlotKey, rows, mon, disk, e.Rows, e.Monitored, e.OnDisk)
		}
	}

	monitoredRows := func(s Slot) int {
		return queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND monitored = 1`, int64(s.ID))
	}

	// A long series gets a second, unmonitored slot after its episodes exist: every row arrives with the slot.
	r, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Series"], MetadataID: "tvdb:9100", Kind: "series", Name: "Long"})
	if err != nil {
		t.Fatal(err)
	}
	var eps []EpisodeSpec
	for n := 1; n <= 400; n++ {
		eps = append(eps, EpisodeSpec{Season: 1 + n/101, Number: n, AirPrecision: "unknown"})
	}
	if err := w.lib.AddEpisodes(ctx, r.TitleID, eps); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Kids"], Persona: "4k", MetadataID: "tvdb:9100", Kind: "series", Name: "Long"}); err != nil {
		t.Fatal(err)
	}
	slots := w.slotsOf(r.TitleID)
	if len(slots) != 2 || !slots[0].Monitored || slots[1].Monitored {
		t.Fatalf("slots = %+v, want a monitored hd slot and an unmonitored uhd slot", slots)
	}
	if hd, uhd := monitoredRows(slots[0]), monitoredRows(slots[1]); hd != 400 || uhd != 0 {
		t.Fatalf("monitored rows hd/uhd = %d/%d, want 400/0 for a slot added after its episodes", hd, uhd)
	}
	air := int64(1_900_000_000)
	more := []EpisodeSpec{{Season: 9, Number: 1, AirUTC: &air, AirPrecision: "instant"}, {Season: 9, Number: 2, AirPrecision: "date"}}
	if err := w.lib.AddEpisodes(ctx, r.TitleID, append(more, eps[:5]...)); err != nil { // the first five already exist
		t.Fatal(err)
	}
	for _, s := range slots {
		if got := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ?`, int64(s.ID)); got != 402 {
			t.Errorf("slot %s rows = %d, want 402", s.Key, got)
		}
		if got := queryInt(t, w.db, `SELECT count(DISTINCT ord) FROM slot_episode WHERE slot_id = ?`, int64(s.ID)); got != 402 {
			t.Errorf("slot %s distinct ord = %d, want 402", s.Key, got)
		}
	}
	if got := queryInt(t, w.db, `SELECT max(ord) FROM episode WHERE title_id = ?`, int64(r.TitleID)); got != 402 {
		t.Errorf("max ord = %d, want 402", got)
	}
	// The episodes that arrive after both slots exist take monitoring from their own slot.
	if hd, uhd := monitoredRows(slots[0]), monitoredRows(slots[1]); hd != 402 || uhd != 0 {
		t.Errorf("monitored rows hd/uhd = %d/%d, want 402/0 for episodes added after their slots", hd, uhd)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND air_utc = ?`, int64(slots[0].ID), air); got != 1 {
		t.Errorf("instant episode rows for hd = %d, want 1", got)
	}

	// One slot's change leaves the other slot's row alone, in both directions.
	q := `SELECT count(*) FROM slot_episode se JOIN episode e ON e.id = se.episode_id
		WHERE se.slot_id = ? AND e.season = ? AND e.number = ? AND se.monitored = ? AND se.state = ? AND se.size_bytes = ?`
	if err := w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: slots[1].ID, Season: 1, Number: 1, Monitored: true, State: "live", SizeBytes: 5}); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, w.db, q, int64(slots[1].ID), 1, 1, 1, "live", 5) != 1 || queryInt(t, w.db, q, int64(slots[0].ID), 1, 1, 1, "missing", 0) != 1 {
		t.Fatal("SetSlotEpisode must change only its own slot")
	}
	if err := w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: slots[0].ID, Season: 1, Number: 2, Monitored: false, State: "on_disk", SizeBytes: 7}); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, w.db, q, int64(slots[0].ID), 1, 2, 0, "on_disk", 7) != 1 || queryInt(t, w.db, q, int64(slots[1].ID), 1, 2, 0, "missing", 0) != 1 {
		t.Fatal("SetSlotEpisode must change only its own slot")
	}
	if err := w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: slots[0].ID, Season: 77, Number: 1, State: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown episode: %v, want ErrNotFound", err)
	}

	// Every series slot holds exactly one row per episode of its title.
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot s JOIN title t ON t.id = s.title_id WHERE t.kind = 'series'
		AND (SELECT count(*) FROM slot_episode se WHERE se.slot_id = s.id) <> (SELECT count(*) FROM episode e WHERE e.title_id = s.title_id)`); got != 0 {
		t.Fatalf("%d series slots with missing or extra episode rows", got)
	}

	// Movie state is per slot and belongs to movie slots only.
	movie := w.slotID("tmdb:603", "uhd")
	if s := w.slots("tmdb:603")[1]; s.MediaState != "missing" || s.SizeBytes != 0 {
		t.Fatalf("new movie slot = %+v, want missing and 0 bytes", s)
	}
	if err := w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: movie, State: "on_disk", SizeBytes: 9}); err != nil {
		t.Fatal(err)
	}
	if s := w.slots("tmdb:603"); s[1].MediaState != "on_disk" || s[1].SizeBytes != 9 || s[0].MediaState != "missing" {
		t.Fatalf("movie slots = %+v, want only uhd on_disk", s)
	}
	if err := w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: slots[0].ID, State: "on_disk"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("series slot as movie slot: %v, want ErrNotFound", err)
	}
	if s := w.slotsOf(r.TitleID)[0]; s.MediaState != "" {
		t.Fatalf("series slot media_state = %q, want unset", s.MediaState)
	}
}

// Both setters accept every state the schema accepts and persist it to the
// addressed slot only. The name extends TestPerSlotEpisodeState so the slice
// proof's -run pattern selects it.
func TestPerSlotEpisodeStateSettersAcceptEverySchemaState(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()

	for _, table := range []string{"slot", "slot_episode"} {
		ddl := strings.Join(dump(t, w.db, `SELECT sql FROM sqlite_master WHERE name = ?`, table), "")
		for _, st := range allStates {
			if !strings.Contains(ddl, "'"+st+"'") {
				t.Fatalf("%s schema does not accept %q: %s", table, st, ddl)
			}
		}
	}

	movieTitle := w.titles["tmdb:603"]
	target := w.slotID("tmdb:603", "uhd")
	for i, st := range allStates {
		size := int64(100 * (i + 1))
		siblings := func() []string {
			return dump(t, w.db, `SELECT * FROM slot WHERE title_id = ? AND id <> ?`, int64(movieTitle), int64(target))
		}
		before := siblings()
		if len(before) != 2 {
			t.Fatalf("movie siblings = %v, want hd and dc", before)
		}
		if err := w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: target, State: st, SizeBytes: size}); err != nil {
			t.Fatalf("SetMovieSlot %s: %v", st, err)
		}
		want := []string{fmt.Sprintf("[%q,%d]", st, size)}
		if got := dump(t, w.db, `SELECT media_state, size_bytes FROM slot WHERE id = ?`, int64(target)); !slices.Equal(got, want) {
			t.Errorf("movie slot after %s = %v, want %v", st, got, want)
		}
		if after := siblings(); !slices.Equal(after, before) {
			t.Errorf("SetMovieSlot %s changed a sibling slot:\nbefore %v\nafter  %v", st, before, after)
		}
	}

	hd, uhd := w.slotID("tvdb:9001", "hd"), w.slotID("tvdb:9001", "uhd")
	for i, st := range allStates {
		size := int64(100 * (i + 1))
		mon := i%2 == 0
		siblingRows := func() []string {
			return dump(t, w.db, `SELECT * FROM slot_episode WHERE slot_id = ?`, int64(hd))
		}
		before := siblingRows()
		if err := w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: uhd, Season: 2, Number: 1, Monitored: mon, State: st, SizeBytes: size}); err != nil {
			t.Fatalf("SetSlotEpisode %s: %v", st, err)
		}
		want := []string{fmt.Sprintf("[%q,%d,%d]", st, size, b2i(mon))}
		got := dump(t, w.db, `SELECT se.state, se.size_bytes, se.monitored FROM slot_episode se JOIN episode e ON e.id = se.episode_id
			WHERE se.slot_id = ? AND e.season = 2 AND e.number = 1`, int64(uhd))
		if !slices.Equal(got, want) {
			t.Errorf("slot episode after %s = %v, want %v", st, got, want)
		}
		if after := siblingRows(); !slices.Equal(after, before) {
			t.Errorf("SetSlotEpisode %s changed the sibling slot:\nbefore %v\nafter  %v", st, before, after)
		}
	}
}

// writeSpy counts row writes as they are attempted. A trigger calls it before
// the row changes, so a write that the store later rolls back is still counted;
// a snapshot comparison cannot see one.
var writeSpy atomic.Int64

func init() {
	sqlite.MustRegisterScalarFunction("boxset_write_spy", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		return writeSpy.Add(1), nil
	})
}

// watchWrites adds a BEFORE INSERT/UPDATE/DELETE trigger on every table that
// calls writeSpy. Reads run no trigger. A statement matching no row changes
// nothing and is not counted.
func (w *s2World) watchWrites() {
	w.t.Helper()
	ctx := w.t.Context()
	err := w.db.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND sql NOT LIKE 'CREATE VIRTUAL%'`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			tables = append(tables, name)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, table := range tables {
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				ddl := fmt.Sprintf(`CREATE TRIGGER "spy_%s_%s" BEFORE %s ON "%s" BEGIN SELECT boxset_write_spy(); END`, table, op, op, table)
				if _, err := tx.ExecContext(ctx, ddl); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatalf("watch writes: %v", err)
	}
}

func TestIdentityInputRejectedBeforeWrite(t *testing.T) {
	w := applyS2(t)
	w.watchWrites()
	ctx := t.Context()
	movies := w.libs["Movies"]
	movieTitle, seriesTitle := w.titles["tmdb:603"], w.titles["tvdb:9001"]
	movieSlot, seriesSlot := w.slotID("tmdb:603", "hd"), w.slotID("tvdb:9001", "hd")
	air := int64(1)
	goodDefaults := LibraryDefaults{NamingLanguage: "en", SearchLanguage: "en", Profile: "p"}

	type reject struct {
		name string
		run  func() error
	}
	var cases []reject
	add := func(name string, run func() error) { cases = append(cases, reject{name, run}) }
	imp := func(name string, mod func(*SourceRecord)) {
		add("import "+name, func() error {
			r := SourceRecord{InstanceID: "inst", SourceID: "1", LibraryID: movies, MetadataID: "tmdb:5", Kind: "movie", Name: "N", SlotKey: "hd"}
			mod(&r)
			_, err := w.lib.ImportSource(ctx, r)
			return err
		})
	}
	mem := func(name, m, k, n string) {
		add(name, func() error {
			_, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: movies, MetadataID: m, Kind: k, Name: n})
			return err
		})
	}
	lib := func(name string, spec LibrarySpec) {
		add("library "+name, func() error { _, err := w.lib.AddLibrary(ctx, spec); return err })
	}
	eps := func(name string, id TitleID, e ...EpisodeSpec) {
		add("episodes "+name, func() error { return w.lib.AddEpisodes(ctx, id, e) })
	}

	for _, m := range []string{"", "603", "TMDB:603", "tmdb:", ":603", "tmdb:60 3", "tmdb:603\n", strings.Repeat("a", 17) + ":1", "tmdb:" + strings.Repeat("1", 49)} {
		mem("metadata_id "+strconv.Quote(m), m, "movie", "N")
	}
	mem("kind show", "tmdb:5", "show", "N")
	mem("kind empty", "tmdb:5", "", "N")
	mem("name empty", "tmdb:5", "movie", "")
	mem("name NUL", "tmdb:5", "movie", "a\x00b")
	mem("name bad utf8", "tmdb:5", "movie", "\xff")
	mem("name long", "tmdb:5", "movie", strings.Repeat("a", 300))
	mem("name empty on a movie requested as a series", "tmdb:603", "series", "")
	add("persona key upper", func() error {
		_, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: movies, Persona: "4K", MetadataID: "tmdb:5", Kind: "movie", Name: "N"})
		return err
	})
	add("persona key upper on a movie requested as a series", func() error {
		_, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: movies, Persona: "4K", MetadataID: "tmdb:603", Kind: "series", Name: "Synthetic Movie A"})
		return err
	})
	add("persona bad title", func() error {
		_, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: movies, Persona: "4k", MetadataID: "x", Kind: "movie", Name: "N"})
		return err
	})
	add("add persona key", func() error { return w.lib.AddPersona(ctx, "Bad Key", "uhd") })
	add("add persona slot key", func() error { return w.lib.AddPersona(ctx, "ok", strings.Repeat("a", 33)) })
	imp("instance empty", func(r *SourceRecord) { r.InstanceID = "" })
	imp("instance local", func(r *SourceRecord) { r.InstanceID = OwnerLocal })
	imp("instance unresolved", func(r *SourceRecord) { r.InstanceID = OwnerUnresolved })
	imp("instance space", func(r *SourceRecord) { r.InstanceID = "a b" })
	imp("source empty", func(r *SourceRecord) { r.SourceID = "" })
	imp("source slash", func(r *SourceRecord) { r.SourceID = "a/b" })
	imp("slot upper", func(r *SourceRecord) { r.SlotKey = "HD" })
	imp("slot long", func(r *SourceRecord) { r.SlotKey = strings.Repeat("a", 33) })
	imp("kind", func(r *SourceRecord) { r.Kind = "anime" })
	lib("name", LibrarySpec{Name: "", Defaults: goodDefaults, Template: []TemplateSlot{{Key: "a"}}})
	lib("language", LibrarySpec{Name: "x", Defaults: LibraryDefaults{SearchLanguage: "en", Profile: "p"}, Template: []TemplateSlot{{Key: "a"}}})
	lib("template key", LibrarySpec{Name: "x", Defaults: goodDefaults, Template: []TemplateSlot{{Key: "A"}}})
	lib("template repeat", LibrarySpec{Name: "x", Defaults: goodDefaults, Template: []TemplateSlot{{Key: "a"}, {Key: "a"}}})
	lib("template two primaries", LibrarySpec{Name: "x", Defaults: goodDefaults, Template: []TemplateSlot{{Key: "a", Primary: true}, {Key: "b", Primary: true}}})

	// Updates of rows that exist: a write before the check would change them.
	add("defaults naming", func() error {
		return w.lib.SetLibraryDefaults(ctx, movies, LibraryDefaults{Monitored: false, SearchLanguage: "en", Profile: "p"})
	})
	add("defaults search", func() error {
		return w.lib.SetLibraryDefaults(ctx, movies, LibraryDefaults{Monitored: false, NamingLanguage: "en", SearchLanguage: "a\tb", Profile: "p"})
	})
	add("defaults profile", func() error {
		return w.lib.SetLibraryDefaults(ctx, movies, LibraryDefaults{Monitored: false, NamingLanguage: "en", SearchLanguage: "en"})
	})
	add("relabel label", func() error { return w.lib.RelabelSlot(ctx, movieSlot, "", "p") })
	add("relabel profile", func() error { return w.lib.RelabelSlot(ctx, movieSlot, "l", "a\tb") })
	add("relabel long label", func() error { return w.lib.RelabelSlot(ctx, movieSlot, strings.Repeat("l", 65), "p") })
	eps("none", seriesTitle)
	eps("precision", seriesTitle, EpisodeSpec{Season: 1, Number: 1, AirPrecision: "exact"})
	eps("instant without air", seriesTitle, EpisodeSpec{Season: 1, Number: 1, AirPrecision: "instant"})
	eps("date with air", seriesTitle, EpisodeSpec{Season: 1, Number: 1, AirUTC: &air, AirPrecision: "date"})
	eps("negative season", seriesTitle, EpisodeSpec{Season: -1, Number: 1, AirPrecision: "unknown"})
	eps("one bad of many", seriesTitle, EpisodeSpec{Season: 3, Number: 1, AirPrecision: "unknown"}, EpisodeSpec{Season: 3, Number: -1, AirPrecision: "unknown"})
	eps("on a movie", movieTitle, EpisodeSpec{Season: 1, Number: 1, AirPrecision: "unknown"})
	add("slot episode state", func() error {
		return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: 1, Number: 1, Monitored: false, State: "done", SizeBytes: 1})
	})
	add("slot episode size", func() error {
		return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: 1, Number: 1, Monitored: false, State: "live", SizeBytes: -1})
	})
	add("slot episode season", func() error {
		return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: maxSeason + 1, Number: 1, State: "live"})
	})
	add("slot episode number", func() error {
		return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: 1, Number: -1, State: "live"})
	})
	add("movie slot state", func() error {
		return w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: movieSlot, State: "gone", SizeBytes: 1})
	})
	add("movie slot size", func() error {
		return w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: movieSlot, State: "live", SizeBytes: -1})
	})

	// The spy sees a write the store rolls back, which the snapshot comparison
	// below cannot: a call that mutates a row and then returns ErrInvalidInput
	// still fails each case.
	t.Run("spy counts a rolled-back write", func(t *testing.T) {
		before, rows := writeSpy.Load(), snapshot(t, w.db)
		err := w.db.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE slot SET label = label || 'x' WHERE id = ?`, int64(movieSlot)); err != nil {
				return err
			}
			return invalid("probe", "rolled back")
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("error = %v, want ErrInvalidInput", err)
		}
		if writeSpy.Load() == before {
			t.Error("a rolled-back UPDATE was not counted")
		}
		if after := snapshot(t, w.db); !sameSnapshot(rows, after) {
			t.Error("probe was not rolled back")
		}
	})

	// Each rejection is its own case: it must attempt no write and leave every row's contents as it was.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, writes := snapshot(t, w.db), writeSpy.Load()
			err := c.run()
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("error = %v, want ErrInvalidInput", err)
			}
			if n := writeSpy.Load() - writes; n != 0 {
				t.Errorf("rejected input attempted %d write(s) before returning %v", n, err)
			}
			if after := snapshot(t, w.db); !sameSnapshot(before, after) {
				t.Errorf("rejected input changed rows:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// Validation runs before admission: with a context that is already canceled, a
// malformed request is still refused as invalid input, while the same
// well-formed request reaches the store and reports the cancellation. Neither
// call changes a row.
func TestIdentityInputRejectedBeforeWriteCanceledContext(t *testing.T) {
	w := applyS2(t)
	seriesSlot := w.slotID("tvdb:9001", "hd")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := snapshot(t, w.db)

	err := w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: 1, Number: 1, Monitored: false, State: "done", SizeBytes: 1})
	if !errors.Is(err, ErrInvalidInput) || errors.Is(err, context.Canceled) {
		t.Errorf("invalid request on a canceled context: %v, want ErrInvalidInput and not context.Canceled", err)
	}
	err = w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: seriesSlot, Season: 1, Number: 1, Monitored: false, State: "live", SizeBytes: 1})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidInput) {
		t.Errorf("valid request on a canceled context: %v, want context.Canceled and not ErrInvalidInput", err)
	}
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Errorf("canceled-context calls changed rows:\nbefore %v\nafter  %v", before, after)
	}
}

// A slot_episode row, and a sync_map row, belong to one title through
// composite foreign keys: a slot or episode of another title is refused. The
// name extends TestS2IdentityReuse so the slice proof's -run pattern selects it.
func TestS2IdentityReuseCompositeForeignKeysRefuseCrossTitleRows(t *testing.T) {
	w := newS2World(t)
	ctx := t.Context()

	type series struct {
		title TitleID
		slot  int64
		ep    int64
	}
	mk := func(metadataID string) series {
		r, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Series"], MetadataID: metadataID, Kind: "series", Name: metadataID})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.lib.AddEpisodes(ctx, r.TitleID, []EpisodeSpec{{Season: 1, Number: 1, AirPrecision: "unknown"}, {Season: 1, Number: 2, AirPrecision: "unknown"}}); err != nil {
			t.Fatal(err)
		}
		return series{
			title: r.TitleID,
			slot:  int64(r.SlotIDs[0]),
			ep:    int64(queryInt(t, w.db, `SELECT id FROM episode WHERE title_id = ? AND season = 1 AND number = 1`, int64(r.TitleID))),
		}
	}
	a, b := mk("tvdb:1"), mk("tvdb:2")

	exec := func(stmts ...[]any) error {
		return w.db.Write(ctx, func(tx *sql.Tx) error {
			for _, s := range stmts {
				if _, err := tx.ExecContext(ctx, s[0].(string), s[1:]...); err != nil {
					return err
				}
			}
			return nil
		})
	}
	const slotEpisode = `INSERT INTO slot_episode (slot_id, episode_id, title_id, ord, monitored, state, size_bytes) VALUES (?, ?, ?, 1, 1, 'missing', 0)`
	const syncMap = `INSERT INTO sync_map (instance_id, source_kind, source_id, title_id, slot_id) VALUES ('inst', 'series', '1', ?, ?)`

	for _, c := range []struct {
		name string
		stmt []any
	}{
		{"slot_episode slot of another title", []any{slotEpisode, a.slot, b.ep, b.title}},
		{"slot_episode episode of another title", []any{slotEpisode, a.slot, b.ep, a.title}},
		{"sync_map slot of another title", []any{syncMap, b.title, a.slot}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := snapshot(t, w.db)
			err := exec(c.stmt)
			if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Errorf("error = %v, want a FOREIGN KEY failure", err)
			}
			if after := snapshot(t, w.db); !sameSnapshot(before, after) {
				t.Errorf("refused row changed the store:\nbefore %v\nafter  %v", before, after)
			}
		})
	}

	// The same statements with a matching title are accepted, so the refusals above are the key's doing.
	del := []any{`DELETE FROM slot_episode WHERE slot_id = ? AND episode_id = ?`, a.slot, a.ep}
	if err := exec(del, []any{slotEpisode, a.slot, a.ep, a.title}, []any{syncMap, a.title, a.slot}); err != nil {
		t.Fatalf("matching rows refused: %v", err)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ?`, a.slot); got != 2 {
		t.Errorf("slot_episode rows of the first series = %d, want 2", got)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM sync_map`); got != 1 {
		t.Errorf("sync_map rows = %d, want 1", got)
	}
}
