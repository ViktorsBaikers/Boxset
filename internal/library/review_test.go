package library

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

const (
	releaseHold = "Synthetic.Movie.Hold.1080p.WEB-DL-GRP"
	remoteOwner = "instance-1"
)

// setOwner writes a slot's owner directly: the library has no owner-transfer
// API, so this is how a test makes a slot remote-owned.
func (w *s2World) setOwner(slot SlotID, owner string) {
	w.t.Helper()
	err := w.db.Write(w.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(w.t.Context(), `UPDATE slot SET owner = ? WHERE id = ?`, owner, int64(slot))
		return err
	})
	if err != nil {
		w.t.Fatalf("set owner: %v", err)
	}
}

// insertHold stores a hold the way a later resolution workflow could.
func (w *s2World) insertHold(scope string, title TitleID, slot SlotID) {
	w.t.Helper()
	err := w.db.Write(w.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(w.t.Context(), `INSERT INTO review_hold (reason_code, scope_key, title_id, slot_id, params)
			VALUES ('identity_conflict', ?, ?, NULLIF(?, 0), '{}')`, scope, int64(title), int64(slot))
		return err
	})
	if err != nil {
		w.t.Fatalf("insert hold: %v", err)
	}
}

func (w *s2World) request(release string, slots ...SlotID) AcquisitionOutcome {
	w.t.Helper()
	return mustRequest(w.t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: release, Slots: slots})
}

// recordFiles records one file per slot and returns the file rows the table must
// keep holding afterwards, sorted as dump sorts them.
func (w *s2World) recordFiles(slots ...SlotID) []string {
	w.t.Helper()
	var want []string
	for _, s := range slots {
		rec := FileRecord{SlotID: s, RootKey: "movies", RelativePath: fmt.Sprintf("slot-%d/a.mkv", s), SizeBytes: 4096 + int64(s)}
		if err := w.lib.RecordFile(w.t.Context(), rec); err != nil {
			w.t.Fatalf("RecordFile slot %d: %v", s, err)
		}
		want = append(want, fileRow(rec))
	}
	slices.Sort(want)
	return want
}

func (w *s2World) owners() []string { return dump(w.t, w.db, `SELECT id, owner FROM slot`) }

// holdRows is every review_hold row except its id.
func holdRows(t *testing.T, db *store.DB) []string {
	t.Helper()
	return dump(t, db, `SELECT reason_code, scope_key, title_id, slot_id, params FROM review_hold`)
}

// slotHolds is the holds the pause guard wrote for a slot, as `[reason, scope, title, slot]`.
func slotHolds(t *testing.T, db *store.DB) []string {
	t.Helper()
	return dump(t, db, `SELECT reason_code, scope_key, title_id, slot_id FROM review_hold WHERE scope_key LIKE 'slot:%'`)
}

func slotHoldRow(reason ReasonCode, title TitleID, slot SlotID) string {
	return fmt.Sprintf("[%q,\"slot:%d\",%d,%d]", string(reason), slot, title, slot)
}

// wantPause fails unless got is a Pause of slot with reason and these params.
func wantPause(t *testing.T, got Pause, slot SlotID, reason ReasonCode, params map[string]string) {
	t.Helper()
	if got.SlotID != slot || got.Reason != reason {
		t.Fatalf("pause = slot %d %s, want slot %d %s", got.SlotID, got.Reason, slot, reason)
	}
	for k, v := range params {
		if got.Params[k] != v {
			t.Fatalf("pause %s param %q = %q, want %q (params %v)", reason, k, got.Params[k], v, got.Params)
		}
	}
}

// requireNoAcquisition fails when a request created an acquisition or download_slot row.
func requireNoAcquisition(t *testing.T, w *s2World, out AcquisitionOutcome) {
	t.Helper()
	if out.AcquisitionID != 0 || out.Created || len(out.Attached) != 0 {
		t.Fatalf("outcome %+v: want no acquisition", out)
	}
	if got := acqCounts(t, w.db); got["acquisition"] != 0 || got["download_slot"] != 0 {
		t.Fatalf("acquisition rows = %v, want none", got)
	}
}

// validS2 applies the fixture's valid steps only; its conflict steps are left for the test.
func validS2(t *testing.T) *s2World {
	t.Helper()
	w := newS2World(t)
	for _, s := range w.fx.Steps {
		if !s.Conflict {
			w.apply(s)
		}
	}
	return w
}

func TestIdentityConflictPauses(t *testing.T) {
	w := validS2(t)
	ctx := t.Context()
	// Files on the slots the conflicts below name must survive every conflict and pause:
	// the conflicted slot, the slots of the kind-clash title, and an untouched slot.
	hd, uhd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd")
	var clash []SlotID
	for _, s := range w.slots("tvdb:9002") {
		clash = append(clash, s.ID)
	}
	wantFiles := w.recordFiles(append([]SlotID{hd, uhd}, clash...)...)
	requireFileRows(t, w.db, wantFiles)
	uhdScope42 := fmt.Sprintf("import:movie-uhd/movie/42@slot:%d", uhd)
	uhdScope43 := fmt.Sprintf("import:movie-uhd/movie/43@slot:%d", uhd)
	before, owners := snapshot(t, w.db), w.owners()
	if got := holdRows(t, w.db); len(got) != 0 {
		t.Fatalf("holds before any conflict = %v", got)
	}

	cases := []struct {
		name        string
		rec         SourceRecord
		scope       string
		extra       map[string]string
		title, slot string // metadata id and slot key the hold must name; slot is empty for a title-wide hold
	}{
		{"known source points elsewhere", SourceRecord{InstanceID: "movie-uhd", SourceID: "42", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"},
			uhdScope42, map[string]string{"cause": "source_remapped", "mapped_metadata_id": "tmdb:603", "mapped_slot_key": "uhd"}, "tmdb:603", "uhd"},
		{"second source for one slot", SourceRecord{InstanceID: "movie-uhd", SourceID: "43", LibraryID: w.libs["UHD"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A", SlotKey: "uhd"},
			uhdScope43, map[string]string{"cause": "slot_has_source", "mapped_source_id": "42"}, "tmdb:603", "uhd"},
		{"metadata id is the other kind", SourceRecord{InstanceID: "series-hd", SourceID: "44", LibraryID: w.libs["Series"], MetadataID: "tvdb:9002", Kind: "movie", Name: "Synthetic Series B", SlotKey: "hd"},
			fmt.Sprintf("import:series-hd/movie/44@title:%d", w.titles["tvdb:9002"]), map[string]string{"cause": "title_kind_mismatch", "mapped_kind": "series"}, "tvdb:9002", ""},
	}
	for _, c := range cases {
		res, err := w.lib.ImportSource(ctx, c.rec)
		if !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("%s: err = %v, want ErrIdentityConflict", c.name, err)
		}
		if res.Pause == nil || res.TitleID != 0 || res.SlotID != 0 {
			t.Fatalf("%s: result %+v, want a Pause and no title or slot", c.name, res)
		}
		wantPause(t, *res.Pause, 0, ReasonIdentityConflict, c.extra)
		var stored string
		var title, slot int64
		err = w.db.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT params, COALESCE(title_id, 0), COALESCE(slot_id, 0) FROM review_hold
				WHERE reason_code = 'identity_conflict' AND scope_key = ?`, c.scope).Scan(&stored, &title, &slot)
		})
		if err != nil {
			t.Fatalf("%s: no hold %s: %v", c.name, c.scope, err)
		}
		want, _ := json.Marshal(res.Pause.Params)
		if stored != string(want) {
			t.Fatalf("%s: stored params %s, want %s", c.name, stored, want)
		}
		wantSlot := SlotID(0)
		if c.slot != "" {
			wantSlot = w.slotID(c.title, c.slot)
		}
		if TitleID(title) != w.titles[c.title] || SlotID(slot) != wantSlot {
			t.Fatalf("%s: hold names title %d slot %d, want title %d slot %d", c.name, title, slot, w.titles[c.title], wantSlot)
		}
	}
	if n := queryInt(t, w.db, `SELECT count(*) FROM review_hold`); n != len(cases) {
		t.Fatalf("holds = %d, want one per conflicting source (%d)", n, len(cases))
	}
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Fatalf("conflicting imports changed title, slot or sync_map rows:\nbefore %v\nafter  %v", before, after)
	}
	requireFileRows(t, w.db, wantFiles)

	// A replay of each conflicting import adds no second hold.
	held := holdRows(t, w.db)
	for _, c := range cases {
		if _, err := w.lib.ImportSource(ctx, c.rec); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("%s replay: err = %v, want ErrIdentityConflict", c.name, err)
		}
	}
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("replayed imports changed holds: %v -> %v", held, got)
	}

	// Acquiring what an import conflicted with pauses: no download_slot, and no
	// title, slot, mapping or owner changes. The hold came from the imports above.
	acq := acqSnapshot(t, w.db)
	kindScope := fmt.Sprintf("import:series-hd/movie/44@title:%d", w.titles["tvdb:9002"])
	out := w.request(releaseHold, uhd)
	if len(out.Paused) != 1 {
		t.Fatalf("paused = %+v, want the conflicted slot", out.Paused)
	}
	wantPause(t, out.Paused[0], uhd, ReasonIdentityConflict, map[string]string{"hold_scope": uhdScope42, "slot_key": "uhd", "metadata_id": "tmdb:603"})
	requireNoAcquisition(t, w, out)
	requireFileRows(t, w.db, wantFiles) // the first all-paused request left the conflicted slot's file alone
	out = w.request(releaseHold, clash...)
	if len(out.Paused) != len(clash) || len(clash) == 0 {
		t.Fatalf("paused = %+v, want every slot of the title the kind clash named (%v)", out.Paused, clash)
	}
	ids := clash
	for i, p := range out.Paused {
		wantPause(t, p, ids[i], ReasonIdentityConflict, map[string]string{"hold_scope": kindScope})
	}
	requireNoAcquisition(t, w, out)
	if after := acqSnapshot(t, w.db); !maps.EqualFunc(acq, after, slices.Equal) {
		t.Fatalf("paused requests changed rows: before %v after %v", acq, after)
	}
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Fatalf("paused requests changed title, slot or sync_map rows:\nbefore %v\nafter  %v", before, after)
	}
	if got := w.owners(); !slices.Equal(got, owners) {
		t.Fatalf("paused requests changed owners: %v -> %v", owners, got)
	}
	requireFileRows(t, w.db, wantFiles)

	// A hold on a slot or on its title pauses that slot's acquisition and adds no hold.
	w.insertHold("manual:slot", w.titles["tmdb:603"], hd)
	out = w.request(releaseHold, hd)
	if len(out.Paused) != 1 {
		t.Fatalf("paused = %+v, want the held slot", out.Paused)
	}
	wantPause(t, out.Paused[0], hd, ReasonIdentityConflict, map[string]string{"hold_scope": "manual:slot", "slot_key": "hd", "metadata_id": "tmdb:603"})
	requireNoAcquisition(t, w, out)

	series := w.titles["tvdb:9001"]
	w.insertHold("manual:title", series, 0)
	held = holdRows(t, w.db)
	sHD, sUHD := w.slotID("tvdb:9001", "hd"), w.slotID("tvdb:9001", "uhd")
	out = w.request(releaseHold, sHD, sUHD)
	if len(out.Paused) != 2 {
		t.Fatalf("paused = %+v, want both slots of the held title", out.Paused)
	}
	wantPause(t, out.Paused[0], sHD, ReasonIdentityConflict, map[string]string{"hold_scope": "manual:title"})
	wantPause(t, out.Paused[1], sUHD, ReasonIdentityConflict, map[string]string{"hold_scope": "manual:title"})
	requireNoAcquisition(t, w, out)
	requireFileRows(t, w.db, wantFiles)
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("a held slot's request added holds: %v -> %v", held, got)
	}
	holds, err := w.lib.Holds(ctx, series)
	if err != nil || len(holds) != 1 || holds[0].ScopeKey != "manual:title" || holds[0].TitleID != series || holds[0].SlotID != 0 || holds[0].Reason != ReasonIdentityConflict {
		t.Fatalf("Holds(series) = %+v, %v", holds, err)
	}
}

func TestEmptyRestrictionIntersectionPauses(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	movies, kids, collector := w.libs["Movies"], w.libs["Kids"], w.libs["Collector"]
	hd, uhd, dc := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd"), w.slotID("tmdb:603", "dc")
	title := w.titles["tmdb:603"]
	set := func(id LibraryID, keys ...string) {
		t.Helper()
		if err := w.lib.SetLibraryIndexers(ctx, id, keys); err != nil {
			t.Fatalf("SetLibraryIndexers %d: %v", id, err)
		}
	}

	// Movies and Kids both select hd and share no indexer.
	set(movies, "idx-a", "idx-b")
	set(kids, "idx-c")
	out := w.request(releaseHold, hd)
	if len(out.Paused) != 1 {
		t.Fatalf("paused = %+v, want hd", out.Paused)
	}
	restricting := []string{fmt.Sprintf("library:%d", movies), fmt.Sprintf("library:%d", kids)}
	slices.Sort(restricting)
	wantPause(t, out.Paused[0], hd, ReasonRestrictionEmptyIntersection, map[string]string{"restrictions": restricting[0] + "," + restricting[1], "slot_key": "hd"})
	requireNoAcquisition(t, w, out)
	if got := slotHolds(t, w.db); !slices.Equal(got, []string{slotHoldRow(ReasonRestrictionEmptyIntersection, title, hd)}) {
		t.Fatalf("slot holds = %v, want one for hd", got)
	}

	// A shared indexer makes the slot eligible; the stored hold does not block it.
	set(kids, "idx-b", "idx-c")
	out = w.request(releaseHold, hd)
	if len(out.Paused) != 0 || !slices.Equal(out.Attached, []SlotID{hd}) || !out.Created {
		t.Fatalf("outcome %+v, want hd attached", out)
	}

	// Libraries and slots without rows do not restrict, and a library only
	// restricts the keys its template selects: Movies has a title by persona
	// but selects hd only, so the uhd slot the persona made is unrestricted.
	persona, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: movies, Persona: "4k", MetadataID: "tmdb:950", Kind: "movie", Name: "Synthetic Movie P"})
	if err != nil || len(persona.SlotIDs) != 1 {
		t.Fatalf("ApplyPersona = %+v, %v", persona, err)
	}
	// Persona-made slots start local; the fixture's tmdb:603/uhd is under an identity conflict instead.
	out = w.request(releaseHold, persona.SlotIDs...)
	if len(out.Paused) != 0 || !slices.Equal(out.Attached, persona.SlotIDs) {
		t.Fatalf("persona uhd outcome %+v, want attached despite the Movies restriction", out)
	}
	if out = w.request(releaseHold, uhd); len(out.Paused) != 1 || out.Paused[0].Reason != ReasonIdentityConflict || len(out.Attached) != 0 {
		t.Fatalf("uhd outcome %+v, want the fixture's identity conflict to pause it", out)
	}

	// The slot's own allowlist joins the intersection.
	if err := w.lib.SetLibraryIndexers(ctx, collector, []string{"idx-x"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetSlotIndexers(ctx, dc, []string{"idx-y"}); err != nil {
		t.Fatal(err)
	}
	out = w.request(releaseHold, dc)
	if len(out.Paused) != 1 {
		t.Fatalf("dc paused = %+v, want the slot", out.Paused)
	}
	wantPause(t, out.Paused[0], dc, ReasonRestrictionEmptyIntersection, map[string]string{"restrictions": fmt.Sprintf("library:%d,slot", collector)})
	if err := w.lib.SetSlotIndexers(ctx, dc, []string{"idx-x"}); err != nil {
		t.Fatal(err)
	}
	if out = w.request(releaseHold, dc); len(out.Paused) != 0 || !slices.Equal(out.Attached, []SlotID{dc}) {
		t.Fatalf("dc outcome %+v, want attached once the allowlists meet", out)
	}

	// An empty list clears a restriction; bad input and unknown ids write nothing.
	set(movies)
	if n := queryInt(t, w.db, `SELECT count(*) FROM library_indexer WHERE library_id = ?`, int64(movies)); n != 0 {
		t.Fatalf("library_indexer rows after clearing = %d", n)
	}
	before := dump(t, w.db, `SELECT * FROM library_indexer`)
	for _, c := range []struct {
		name string
		err  error
		want error
	}{
		{"bad key", w.lib.SetLibraryIndexers(ctx, kids, []string{"a b"}), ErrInvalidInput},
		{"empty key", w.lib.SetSlotIndexers(ctx, dc, []string{""}), ErrInvalidInput},
		{"unknown library", w.lib.SetLibraryIndexers(ctx, 999999, []string{"a"}), ErrNotFound},
		{"unknown slot", w.lib.SetSlotIndexers(ctx, 999999, []string{"a"}), ErrNotFound},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, c.err, c.want)
		}
	}
	if after := dump(t, w.db, `SELECT * FROM library_indexer`); !slices.Equal(before, after) {
		t.Fatalf("rejected input changed library_indexer: %v -> %v", before, after)
	}
}

func TestUnresolvedOwnershipPauses(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	un := w.slotID("tmdb:604", "hd")
	title := w.titles["tmdb:604"]
	owners := w.owners()

	out := w.request(releaseHold, un)
	if len(out.Paused) != 1 {
		t.Fatalf("paused = %+v, want the slot", out.Paused)
	}
	wantPause(t, out.Paused[0], un, ReasonOwnershipUnresolved, map[string]string{"owner": OwnerUnresolved, "metadata_id": "tmdb:604", "slot_key": "hd"})
	requireNoAcquisition(t, w, out)
	if got := slotHolds(t, w.db); !slices.Equal(got, []string{slotHoldRow(ReasonOwnershipUnresolved, title, un)}) {
		t.Fatalf("slot holds = %v, want one for the unresolved slot", got)
	}
	holds, err := w.lib.Holds(ctx, title)
	if err != nil || len(holds) != 1 || holds[0].Reason != ReasonOwnershipUnresolved || holds[0].SlotID != un || holds[0].Params["owner"] != OwnerUnresolved {
		t.Fatalf("Holds = %+v, %v", holds, err)
	}
	if !slices.Equal(w.owners(), owners) {
		t.Fatalf("owners changed: %v -> %v", owners, w.owners())
	}

	// Unresolved ownership is decided before restrictions: one hold, one reason.
	if err := w.lib.SetSlotIndexers(ctx, un, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetLibraryIndexers(ctx, w.libs["Movies"], []string{"b"}); err != nil {
		t.Fatal(err)
	}
	out = w.request(releaseHold, un)
	wantPause(t, out.Paused[0], un, ReasonOwnershipUnresolved, nil)
	if got := slotHolds(t, w.db); len(got) != 1 {
		t.Fatalf("slot holds = %v, want only the ownership hold", got)
	}

	// A missing slot fails the request before any hold is written.
	held := holdRows(t, w.db)
	if _, err := w.lib.RequestAcquisition(ctx, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseHold, Slots: []SlotID{un, 999999}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("a failed request changed holds: %v -> %v", held, got)
	}
}

func TestRemoteOwnedSlotNoLocalAcquisition(t *testing.T) {
	w := applyS2(t)
	remote := w.slotID("tmdb:604", "hd")
	w.setOwner(remote, remoteOwner)
	held := holdRows(t, w.db)

	// Remote ownership is decided first: no hold, whatever else applies to the slot.
	if err := w.lib.SetSlotIndexers(t.Context(), remote, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetLibraryIndexers(t.Context(), w.libs["Movies"], []string{"b"}); err != nil {
		t.Fatal(err)
	}
	out := w.request(releaseHold, remote)
	if len(out.Paused) != 1 {
		t.Fatalf("paused = %+v, want the slot", out.Paused)
	}
	wantPause(t, out.Paused[0], remote, ReasonSlotRemoteOwned, map[string]string{"owner": remoteOwner, "slot_key": "hd", "metadata_id": "tmdb:604"})
	requireNoAcquisition(t, w, out)
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("a refusal wrote a hold: %v -> %v", held, got)
	}
	if got := w.owners(); !slices.Contains(got, fmt.Sprintf("[%d,%q]", remote, remoteOwner)) {
		t.Fatalf("owner changed: %v", got)
	}
}

func TestNoTextOrProfileNameMerge(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	movies := w.libs["Movies"]
	const name = "Twin Title"
	a, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: movies, MetadataID: "tmdb:910", Kind: "movie", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: movies, MetadataID: "tmdb:911", Kind: "movie", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	imp, err := w.lib.ImportSource(ctx, SourceRecord{InstanceID: "movie-hd", SourceID: "912", LibraryID: movies, MetadataID: "tmdb:912", Kind: "movie", Name: name, SlotKey: "hd"})
	if err != nil {
		t.Fatal(err)
	}
	if a.TitleID == b.TitleID || a.TitleID == imp.TitleID || b.TitleID == imp.TitleID {
		t.Fatalf("same-name titles merged: %d %d %d", a.TitleID, b.TitleID, imp.TitleID)
	}
	if n := queryInt(t, w.db, `SELECT count(*) FROM title WHERE name = ?`, name); n != 3 {
		t.Fatalf("titles named %q = %d, want 3", name, n)
	}

	// Equal labels and profiles never select or merge a slot.
	hd, uhd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd")
	slotCount := queryInt(t, w.db, `SELECT count(*) FROM slot`)
	for _, id := range []SlotID{hd, uhd, a.SlotIDs[0], b.SlotIDs[0]} {
		if err := w.lib.RelabelSlot(ctx, id, "Same Label", "Same-Profile"); err != nil {
			t.Fatal(err)
		}
	}
	again, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: movies, MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
	if err != nil || !slices.Equal(again.SlotIDs, []SlotID{hd}) {
		t.Fatalf("membership after relabel = %+v, %v; want the same hd slot", again, err)
	}
	persona, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: movies, Persona: "4k", MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
	if err != nil || !slices.Equal(persona.SlotIDs, []SlotID{uhd}) {
		t.Fatalf("persona after relabel = %+v, %v; want the same uhd slot", persona, err)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot`); got != slotCount {
		t.Fatalf("slots = %d, want %d: a label merged or created a slot", got, slotCount)
	}
	if n := queryInt(t, w.db, `SELECT count(DISTINCT slot_key) FROM slot WHERE title_id = ?`, int64(w.titles["tmdb:603"])); n != 3 {
		t.Fatalf("tmdb:603 slot keys = %d, want 3", n)
	}
}

func TestImportNeverTransfersOwnership(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	local := w.slotID("tmdb:603", "hd")
	unresolved := w.slotID("tmdb:604", "hd")
	remote := w.slotID("tmdb:700", "uhd")
	w.setOwner(remote, remoteOwner)
	owners := w.owners()

	for i, c := range []struct {
		rec  SourceRecord
		slot SlotID
	}{
		{SourceRecord{InstanceID: "other", SourceID: "301", LibraryID: w.libs["Movies"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A", SlotKey: "hd"}, local},
		{SourceRecord{InstanceID: "other", SourceID: "302", LibraryID: w.libs["Movies"], MetadataID: "tmdb:604", Kind: "movie", Name: "Synthetic Movie C", SlotKey: "hd"}, unresolved},
		{SourceRecord{InstanceID: "other", SourceID: "303", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"}, remote},
		{SourceRecord{InstanceID: "movie-uhd", SourceID: "50", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"}, remote}, // replay of the mapped record
	} {
		res, err := w.lib.ImportSource(ctx, c.rec)
		if err != nil || res.SlotID != c.slot || res.SlotCreated || res.Pause != nil {
			t.Fatalf("import %d = %+v, %v; want the existing slot %d, nothing created", i, res, err, c.slot)
		}
	}
	// Conflicting imports leave every owner alone as well.
	for _, rec := range []SourceRecord{
		{InstanceID: "movie-uhd", SourceID: "42", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"},
		{InstanceID: "other", SourceID: "304", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"},
	} {
		if _, err := w.lib.ImportSource(ctx, rec); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("conflicting import %s: err = %v", rec.SourceID, err)
		}
	}
	if got := w.owners(); !slices.Equal(got, owners) {
		t.Fatalf("an import changed an owner:\nbefore %v\nafter  %v", owners, got)
	}
}

func TestPausedReplayDedupesHold(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	un, dc := w.slotID("tmdb:604", "hd"), w.slotID("tmdb:603", "dc")
	files := w.recordFiles(un, dc, w.slotID("tmdb:603", "uhd"))
	if err := w.lib.SetLibraryIndexers(ctx, w.libs["Collector"], []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetSlotIndexers(ctx, dc, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	conflict := SourceRecord{InstanceID: "movie-uhd", SourceID: "43", LibraryID: w.libs["UHD"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A", SlotKey: "uhd"}

	first := w.request(releaseHold, un, dc)
	requireFileRows(t, w.db, files) // the first all-paused request left the files alone
	if _, err := w.lib.ImportSource(ctx, conflict); !errors.Is(err, ErrIdentityConflict) {
		t.Fatal(err)
	}
	holds, acq := holdRows(t, w.db), acqSnapshot(t, w.db)
	if got := slotHolds(t, w.db); len(got) != 2 {
		t.Fatalf("slot holds after the first request = %v, want 2", got)
	}
	for i := range 3 {
		again := w.request(releaseHold, un, dc)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("replay %d outcome %+v, want %+v", i, again, first)
		}
		if _, err := w.lib.ImportSource(ctx, conflict); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("replay %d import: %v", i, err)
		}
		if got := holdRows(t, w.db); !slices.Equal(got, holds) {
			t.Fatalf("replay %d changed holds: %v -> %v", i, holds, got)
		}
	}
	requireFileRows(t, w.db, files)
	if after := acqSnapshot(t, w.db); !sameSnapshot(acq, after) {
		t.Fatalf("replay changed acquisition rows: %v -> %v", acq, after)
	}
}

func TestMixedEligibilityAttachesOnlyEligibleSlots(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	eligible := w.slotID("tmdb:603", "hd")
	unresolved := w.slotID("tmdb:604", "hd")
	empty := w.slotID("tmdb:603", "dc")
	remote := w.slotID("tmdb:603", "uhd")
	other := w.slotID("tvdb:9001", "hd") // a second eligible slot, on another title
	w.setOwner(remote, remoteOwner)
	if err := w.lib.SetLibraryIndexers(ctx, w.libs["Collector"], []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.lib.SetSlotIndexers(ctx, empty, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	owners := w.owners()
	files := w.recordFiles(eligible, empty, unresolved, remote)

	// The acquisition already exists for another eligible slot: the mixed request reuses it.
	pre := w.request(releaseHold, other)
	if !pre.Created || !slices.Equal(pre.Attached, []SlotID{other}) {
		t.Fatalf("pre outcome %+v", pre)
	}
	requireFileRows(t, w.db, files)

	out := w.request(releaseHold, unresolved, eligible, empty, remote)
	if out.AcquisitionID != pre.AcquisitionID || out.Created || !slices.Equal(out.Attached, []SlotID{eligible}) {
		t.Fatalf("outcome %+v, want acquisition %d reused with only the eligible slot attached", out, pre.AcquisitionID)
	}
	if len(out.Paused) != 3 {
		t.Fatalf("paused = %+v, want one per affected slot", out.Paused)
	}
	wantPause(t, out.Paused[0], unresolved, ReasonOwnershipUnresolved, nil)
	wantPause(t, out.Paused[1], empty, ReasonRestrictionEmptyIntersection, nil)
	wantPause(t, out.Paused[2], remote, ReasonSlotRemoteOwned, nil)
	requireFileRows(t, w.db, files) // the mixed request left the files alone

	wantSlots := []string{fmt.Sprintf("[%d,%d]", pre.AcquisitionID, other), fmt.Sprintf("[%d,%d]", pre.AcquisitionID, eligible)}
	slices.Sort(wantSlots)
	if got := dump(t, w.db, `SELECT acquisition_id, slot_id FROM download_slot`); !slices.Equal(got, wantSlots) {
		t.Fatalf("download_slot = %v, want exactly the eligible slots %v", got, wantSlots)
	}
	wantHolds := []string{
		slotHoldRow(ReasonOwnershipUnresolved, w.titles["tmdb:604"], unresolved),
		slotHoldRow(ReasonRestrictionEmptyIntersection, w.titles["tmdb:603"], empty),
	}
	slices.Sort(wantHolds)
	if got := slotHolds(t, w.db); !slices.Equal(got, wantHolds) {
		t.Fatalf("slot holds = %v, want %v (none for the remote-owned slot)", got, wantHolds)
	}
	if n := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); n != 1 {
		t.Fatalf("acquisitions = %d, want 1", n)
	}

	// Replay: no acquisition, hold or download_slot row is added.
	acq, holds := acqSnapshot(t, w.db), holdRows(t, w.db)
	again := w.request(releaseHold, unresolved, eligible, empty, remote)
	if again.AcquisitionID != pre.AcquisitionID || again.Created || len(again.Attached) != 0 || !reflect.DeepEqual(again.Paused, out.Paused) {
		t.Fatalf("replay outcome %+v, want the same acquisition, nothing attached, the same pauses", again)
	}
	if after := acqSnapshot(t, w.db); !sameSnapshot(acq, after) {
		t.Fatalf("replay changed acquisition rows: %v -> %v", acq, after)
	}
	if got := holdRows(t, w.db); !slices.Equal(got, holds) {
		t.Fatalf("replay changed holds: %v -> %v", holds, got)
	}
	if !slices.Equal(w.owners(), owners) {
		t.Fatalf("owners changed: %v -> %v", owners, w.owners())
	}
	requireFileRows(t, w.db, files)

	// A request whose slots are all affected creates no acquisition.
	fresh := w.request("Synthetic.Movie.AllPaused.720p-GRP", unresolved, empty, remote)
	if fresh.AcquisitionID != 0 || fresh.Created || len(fresh.Paused) != 3 {
		t.Fatalf("all-affected outcome %+v, want three pauses and no acquisition", fresh)
	}
	if n := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); n != 1 {
		t.Fatalf("acquisitions = %d, want still 1", n)
	}
	requireFileRows(t, w.db, files) // the all-paused request left them alone too
}

// requireKindClash imports rec, whose metadata id already exists as the other
// kind, and requires one title-wide hold: every slot of the title pauses on
// acquisition, so no sibling slot attaches, and nothing canonical changes.
func requireKindClash(t *testing.T, w *s2World, rec SourceRecord) {
	t.Helper()
	ctx := t.Context()
	title := w.titles[rec.MetadataID]
	var ids []SlotID
	for _, s := range w.slots(rec.MetadataID) {
		ids = append(ids, s.ID)
	}
	files := w.recordFiles(ids...)
	before, owners := snapshot(t, w.db), w.owners()
	if got := holdRows(t, w.db); len(got) != 0 {
		t.Fatalf("holds before the import = %v", got)
	}

	res, err := w.lib.ImportSource(ctx, rec)
	if !errors.Is(err, ErrIdentityConflict) || res.Pause == nil || res.TitleID != 0 || res.SlotID != 0 {
		t.Fatalf("import = %+v, %v; want ErrIdentityConflict and a Pause", res, err)
	}
	wantPause(t, *res.Pause, 0, ReasonIdentityConflict, map[string]string{"cause": "title_kind_mismatch"})
	scope := fmt.Sprintf("import:%s/%s/%s@title:%d", rec.InstanceID, rec.Kind, rec.SourceID, title)
	holds, err := w.lib.Holds(ctx, title)
	if err != nil || len(holds) != 1 || holds[0].ScopeKey != scope || holds[0].TitleID != title || holds[0].SlotID != 0 || holds[0].Reason != ReasonIdentityConflict {
		t.Fatalf("Holds = %+v, %v; want one title-scoped hold %s", holds, err, scope)
	}
	held := holdRows(t, w.db)
	if len(held) != 1 {
		t.Fatalf("holds = %v, want one", held)
	}
	if _, err := w.lib.ImportSource(ctx, rec); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("replay: err = %v", err)
	}
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("replay changed holds: %v -> %v", held, got)
	}
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Fatalf("import changed title, slot or sync_map rows:\nbefore %v\nafter  %v", before, after)
	}
	if got := w.owners(); !slices.Equal(got, owners) {
		t.Fatalf("import changed owners: %v -> %v", owners, got)
	}

	// Acquiring the sibling alone, then every slot of the title, pauses; nothing attaches.
	out := w.request(releaseHold, ids[len(ids)-1])
	if len(out.Paused) != 1 {
		t.Fatalf("sibling paused = %+v, want the slot", out.Paused)
	}
	requireNoAcquisition(t, w, out)
	out = w.request(releaseHold, ids...)
	if len(out.Paused) != len(ids) || len(ids) < 2 {
		t.Fatalf("paused = %+v, want every slot of the title (%v)", out.Paused, ids)
	}
	for i, p := range out.Paused {
		wantPause(t, p, ids[i], ReasonIdentityConflict, map[string]string{"hold_scope": scope})
	}
	requireNoAcquisition(t, w, out)
	if got := holdRows(t, w.db); !slices.Equal(got, held) {
		t.Fatalf("paused requests changed holds: %v -> %v", held, got)
	}
	requireFileRows(t, w.db, files)
}

func TestSeriesImportedAsMovieHoldsEverySlot(t *testing.T) {
	// The instance already maps tvdb:9001/uhd, so the slot-collision check would
	// otherwise answer first and hold only that slot, leaving hd to attach.
	w := validS2(t)
	requireKindClash(t, w, SourceRecord{InstanceID: "series-uhd", SourceID: "8", LibraryID: w.libs["Series"], MetadataID: "tvdb:9001", Kind: "movie", Name: "Synthetic Series", SlotKey: "uhd"})
}

func TestMovieImportedAsSeriesHoldsEverySlot(t *testing.T) {
	w := validS2(t)
	requireKindClash(t, w, SourceRecord{InstanceID: "movie-hd", SourceID: "9", LibraryID: w.libs["Collector"], MetadataID: "tmdb:603", Kind: "series", Name: "Synthetic Movie A", SlotKey: "dc"})
}

func TestOneSourceTwoTargetsKeepsOneHoldEach(t *testing.T) {
	w := validS2(t)
	ctx := t.Context()
	a, b, free := w.slotID("tmdb:603", "uhd"), w.slotID("tmdb:700", "uhd"), w.slotID("tmdb:603", "hd")
	w.setOwner(b, OwnerLocal) // the import left it unresolved; the conflict must pause it, not the owner
	files := w.recordFiles(a, b)
	before, owners := snapshot(t, w.db), w.owners()
	recs := []SourceRecord{ // one source of movie-uhd, pointed at two titles whose uhd slot another source already maps
		{InstanceID: "movie-uhd", SourceID: "99", LibraryID: w.libs["UHD"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A", SlotKey: "uhd"},
		{InstanceID: "movie-uhd", SourceID: "99", LibraryID: w.libs["UHD"], MetadataID: "tmdb:700", Kind: "movie", Name: "Synthetic Movie B", SlotKey: "uhd"},
	}
	for _, rec := range recs {
		if _, err := w.lib.ImportSource(ctx, rec); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("import %s: err = %v", rec.MetadataID, err)
		}
	}
	want := []string{
		fmt.Sprintf("[%q,\"import:movie-uhd/movie/99@slot:%d\",%d,%d]", string(ReasonIdentityConflict), a, w.titles["tmdb:603"], a),
		fmt.Sprintf("[%q,\"import:movie-uhd/movie/99@slot:%d\",%d,%d]", string(ReasonIdentityConflict), b, w.titles["tmdb:700"], b),
	}
	slices.Sort(want)
	conflictHolds := func() []string {
		return dump(t, w.db, `SELECT reason_code, scope_key, title_id, slot_id FROM review_hold`)
	}
	if got := conflictHolds(); !slices.Equal(got, want) {
		t.Fatalf("holds = %v, want one per target %v", got, want)
	}
	for _, rec := range recs { // an exact replay of the same source and target adds nothing
		if _, err := w.lib.ImportSource(ctx, rec); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("replay %s: err = %v", rec.MetadataID, err)
		}
	}
	if got := conflictHolds(); !slices.Equal(got, want) {
		t.Fatalf("replay changed holds: %v", got)
	}

	// Both targets pause and neither attaches; a slot no conflict names still does.
	out := w.request(releaseHold, a, b)
	if len(out.Paused) != 2 {
		t.Fatalf("paused = %+v, want both targets", out.Paused)
	}
	wantPause(t, out.Paused[0], a, ReasonIdentityConflict, map[string]string{"hold_scope": fmt.Sprintf("import:movie-uhd/movie/99@slot:%d", a)})
	wantPause(t, out.Paused[1], b, ReasonIdentityConflict, map[string]string{"hold_scope": fmt.Sprintf("import:movie-uhd/movie/99@slot:%d", b)})
	requireNoAcquisition(t, w, out)
	out = w.request(releaseHold, a, b, free)
	if len(out.Paused) != 2 || !slices.Equal(out.Attached, []SlotID{free}) {
		t.Fatalf("outcome %+v, want only the untouched slot attached", out)
	}
	if after := snapshot(t, w.db); !sameSnapshot(before, after) {
		t.Fatalf("imports changed title, slot or sync_map rows:\nbefore %v\nafter  %v", before, after)
	}
	if got := w.owners(); !slices.Equal(got, owners) {
		t.Fatalf("imports changed owners: %v -> %v", owners, got)
	}
	requireFileRows(t, w.db, files)
}
