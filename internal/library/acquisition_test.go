package library

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// acqTables are the tables an acquisition, a membership, a removal or a pause may touch.
var acqTables = []string{"title", "slot", "library_title", "acquisition", "download_slot", "file", "review_hold"}

// countTables counts the rows of each table inside tx.
func countTables(tx *sql.Tx, tables []string) (map[string]int, error) {
	out := make(map[string]int, len(tables))
	for _, name := range tables {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM " + name).Scan(&n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, nil
}

func acqCounts(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	var out map[string]int
	err := db.Read(t.Context(), func(tx *sql.Tx) (err error) {
		out, err = countTables(tx, acqTables)
		return err
	})
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return out
}

// acqSnapshot returns the full contents of every acqTables table.
func acqSnapshot(t *testing.T, db *store.DB) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(acqTables))
	for _, name := range acqTables {
		out[name] = dump(t, db, "SELECT * FROM "+name)
	}
	return out
}

func mustRequest(t *testing.T, l *Library, req AcquisitionRequest) AcquisitionOutcome {
	t.Helper()
	out, err := l.RequestAcquisition(t.Context(), req)
	if err != nil {
		t.Fatalf("RequestAcquisition %s/%s: %v", req.ClientID, req.ReleaseKey, err)
	}
	return out
}

const (
	releaseA = "Synthetic.Movie.A.2160p.WEB-DL-GRP"
	releaseB = "Synthetic.Movie.A.1080p.BluRay-OTHER"
)

// fileRow is the dump line of the file table row rec persists as.
func fileRow(rec FileRecord) string {
	return fmt.Sprintf("[%d,%q,%q,%d]", rec.SlotID, rec.RootKey, rec.RelativePath, rec.SizeBytes)
}

// requireFileRows fails unless the file table holds exactly want.
func requireFileRows(t *testing.T, db *store.DB, want []string) {
	t.Helper()
	if got := dump(t, db, `SELECT slot_id, root_key, relative_path, size_bytes FROM file`); !slices.Equal(got, want) {
		t.Fatalf("file rows = %v, want %v", got, want)
	}
}

func TestRacingEquivalentRequestsOneAcquisition(t *testing.T) {
	w := newS2World(t)
	ctx := t.Context()
	const n = 64
	type result struct {
		out AcquisitionOutcome
		err error
	}
	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			var m MembershipResult
			var err error
			switch i % 4 { // memberships and a persona that each select part of the intended slot set
			case 0:
				m, err = w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
			case 1:
				m, err = w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Movies"], Persona: "4k", MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
			case 2:
				m, err = w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Collector"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
			default:
				m, err = w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["UHD"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"})
			}
			if err != nil {
				results[i].err = err
				return
			}
			results[i].out, results[i].err = w.lib.RequestAcquisition(ctx, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: m.SlotIDs})
		})
	}
	close(start)
	wg.Wait()

	created, attached := 0, map[SlotID]int{}
	var id int64
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d: %v", i, r.err)
		}
		if id == 0 {
			id = r.out.AcquisitionID
		}
		if r.out.AcquisitionID != id {
			t.Fatalf("request %d converged on acquisition %d, want %d", i, r.out.AcquisitionID, id)
		}
		if r.out.Created {
			created++
		}
		for _, s := range r.out.Attached {
			attached[s]++
		}
	}
	if created != 1 {
		t.Fatalf("%d requests created the acquisition, want 1", created)
	}

	// Only tmdb:603 exists, and its intended slots are the union of what the
	// memberships and the persona select: hd, uhd and dc.
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot`); got != 3 {
		t.Fatalf("slot rows = %d, want 3 (hd, uhd, dc)", got)
	}
	if len(attached) != 3 {
		t.Fatalf("slots attached = %v, want exactly the three intended slots", attached)
	}
	for s, k := range attached {
		if k != 1 {
			t.Fatalf("slot %d attached %d times across outcomes, want 1", s, k)
		}
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); got != 1 {
		t.Fatalf("acquisition rows = %d, want 1", got)
	}
	got := dump(t, w.db, `SELECT acquisition_id, slot_id FROM download_slot`)
	want := dump(t, w.db, `SELECT ?, id FROM slot WHERE slot_key IN ('hd', 'uhd', 'dc')`, id)
	if len(want) != 3 || !slices.Equal(got, want) {
		t.Fatalf("download_slot rows = %v, want one per intended slot %v", got, want)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM title WHERE metadata_id = 'tmdb:603'`); got != 1 {
		t.Fatalf("title rows = %d, want 1", got)
	}
}

func TestDistinctReleasesStayDistinct(t *testing.T) {
	w := applyS2(t)
	// The fixture's conflicting imports put tmdb:603/uhd under an identity conflict, so dc stands in for it.
	hd, dc := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "dc")

	a := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{dc}})
	b := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB, Slots: []SlotID{hd}})
	c := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "sab", ReleaseKey: releaseA, Slots: []SlotID{dc}})

	ids := map[int64]bool{a.AcquisitionID: true, b.AcquisitionID: true, c.AcquisitionID: true}
	if len(ids) != 3 || !a.Created || !b.Created || !c.Created {
		t.Fatalf("outcomes %+v %+v %+v: want three distinct created acquisitions", a, b, c)
	}
	// The same slot may serve two acquisitions; the pair is the key.
	d := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB, Slots: []SlotID{dc}})
	if d.AcquisitionID != b.AcquisitionID || d.Created || !slices.Equal(d.Attached, []SlotID{dc}) {
		t.Fatalf("outcome %+v: want release B, not created, dc attached", d)
	}
	uhd := w.slotID("tmdb:603", "uhd")
	if p := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB, Slots: []SlotID{uhd}}); p.AcquisitionID != 0 || len(p.Attached) != 0 || len(p.Paused) != 1 || p.Paused[0].Reason != ReasonIdentityConflict {
		t.Fatalf("outcome %+v: want the conflicted uhd paused, not attached", p)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); got != 3 {
		t.Fatalf("acquisition rows = %d, want 3", got)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM download_slot`); got != 4 {
		t.Fatalf("download_slot rows = %d, want 4", got)
	}
}

// Distinct releases stay distinct when their requests race: every identity
// gets one row and exactly its own slot set, though equivalent requests repeat
// and the identities share slots.
func TestDistinctReleasesStayDistinctUnderRace(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	// tmdb:603/uhd is under the fixture's identity conflict, so the series hd slot takes its place.
	hd, dc, shd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "dc"), w.slotID("tvdb:9001", "hd")
	ids := []struct {
		client, release string
		reqs            [][]SlotID // equivalent requests, each selecting part of want
		want            []SlotID
	}{
		{"qbit", releaseA, [][]SlotID{{shd}, {dc}, {shd, dc}, {dc, shd}}, []SlotID{shd, dc}},
		{"qbit", releaseB, [][]SlotID{{hd}, {dc}, {hd, dc}, {dc, hd}}, []SlotID{hd, dc}},
		{"sab", releaseA, [][]SlotID{{shd}, {shd}}, []SlotID{shd}},
	}
	const perIdentity = 16
	type result struct {
		out AcquisitionOutcome
		err error
	}
	results := make([][]result, len(ids))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for k, id := range ids {
		results[k] = make([]result, perIdentity)
		for i := range perIdentity {
			wg.Go(func() {
				<-start
				results[k][i].out, results[k][i].err = w.lib.RequestAcquisition(ctx, AcquisitionRequest{ClientID: id.client, ReleaseKey: id.release, Slots: id.reqs[i%len(id.reqs)]})
			})
		}
	}
	close(start)
	wg.Wait()

	acqIDs := map[int64]bool{}
	for k, id := range ids {
		var acq int64
		created, attached := 0, map[SlotID]int{}
		for i, r := range results[k] {
			if r.err != nil {
				t.Fatalf("%s/%s request %d: %v", id.client, id.release, i, r.err)
			}
			if acq == 0 {
				acq = r.out.AcquisitionID
			}
			if r.out.AcquisitionID != acq {
				t.Fatalf("%s/%s request %d converged on acquisition %d, want %d", id.client, id.release, i, r.out.AcquisitionID, acq)
			}
			if r.out.Created {
				created++
			}
			for _, s := range r.out.Attached {
				attached[s]++
			}
		}
		acqIDs[acq] = true
		if created != 1 {
			t.Fatalf("%s/%s: %d requests created the acquisition, want 1", id.client, id.release, created)
		}
		if len(attached) != len(id.want) {
			t.Fatalf("%s/%s: slots attached = %v, want exactly %v", id.client, id.release, attached, id.want)
		}
		for _, s := range id.want {
			if attached[s] != 1 {
				t.Fatalf("%s/%s: slot %d attached %d times across outcomes, want 1", id.client, id.release, s, attached[s])
			}
		}
		if got := queryInt(t, w.db, `SELECT count(*) FROM acquisition WHERE client_id = ? AND release_key = ?`, id.client, id.release); got != 1 {
			t.Fatalf("%s/%s: %d acquisition rows, want 1", id.client, id.release, got)
		}
		var wantSlots []string
		for _, s := range id.want {
			wantSlots = append(wantSlots, fmt.Sprintf("[%d]", s))
		}
		slices.Sort(wantSlots)
		if got := dump(t, w.db, `SELECT slot_id FROM download_slot WHERE acquisition_id = ?`, acq); !slices.Equal(got, wantSlots) {
			t.Fatalf("%s/%s: download_slot = %v, want %v", id.client, id.release, got, wantSlots)
		}
	}
	if len(acqIDs) != len(ids) {
		t.Fatalf("acquisition ids %v: want %d distinct", acqIDs, len(ids))
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); got != len(ids) {
		t.Fatalf("acquisition rows = %d, want %d", got, len(ids))
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM download_slot`); got != 5 {
		t.Fatalf("download_slot rows = %d, want 5", got)
	}
}

func TestSecondCopyOfReleaseConverges(t *testing.T) {
	w := applyS2(t)
	// tmdb:603/uhd is under the fixture's identity conflict, so the series hd slot takes its place.
	hd, dc, shd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "dc"), w.slotID("tvdb:9001", "hd")

	first := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{hd, shd}})
	second := mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{dc, shd}})
	if second.AcquisitionID != first.AcquisitionID || second.Created {
		t.Fatalf("second copy %+v did not converge on %+v", second, first)
	}
	if !slices.Equal(second.Attached, []SlotID{dc}) {
		t.Fatalf("second copy attached %v, want only the new slot %d", second.Attached, dc)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM acquisition`); got != 1 {
		t.Fatalf("acquisition rows = %d, want 1", got)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM download_slot WHERE acquisition_id = ?`, first.AcquisitionID); got != 3 {
		t.Fatalf("download_slot rows = %d, want 3", got)
	}
}

func TestMembershipRemovalPreservesShared(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	hd, shd := w.slotID("tmdb:603", "hd"), w.slotID("tvdb:9001", "hd") // tmdb:603/uhd is under the fixture's identity conflict
	mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{hd, shd}})
	if err := w.lib.RecordFile(ctx, FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "Synthetic Movie A/a.mkv", SizeBytes: 4096}); err != nil {
		t.Fatalf("RecordFile: %v", err)
	}

	wantFile := []string{fileRow(FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "Synthetic Movie A/a.mkv", SizeBytes: 4096})}
	requireFileRows(t, w.db, wantFile)
	before := acqSnapshot(t, w.db)
	if err := w.lib.RemoveMembership(ctx, w.libs["Movies"], w.titles["tmdb:603"]); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}
	requireFileRows(t, w.db, wantFile)
	after := acqSnapshot(t, w.db)
	for _, table := range []string{"title", "slot", "file", "acquisition", "download_slot"} {
		if !slices.Equal(before[table], after[table]) {
			t.Fatalf("%s changed on membership removal: before %v after %v", table, before[table], after[table])
		}
	}
	if len(after["library_title"]) != len(before["library_title"])-1 {
		t.Fatalf("library_title rows %d -> %d, want exactly one removed", len(before["library_title"]), len(after["library_title"]))
	}
	if slices.Contains(memberships(t, w.db), `["Movies","tmdb:603"]`) {
		t.Fatal("Movies membership of tmdb:603 still present")
	}
	if !slices.Contains(memberships(t, w.db), `["Kids","tmdb:603"]`) {
		t.Fatal("Kids membership of tmdb:603 was removed")
	}
	if err := w.lib.RemoveMembership(ctx, w.libs["Movies"], w.titles["tmdb:603"]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second RemoveMembership err = %v, want ErrNotFound", err)
	}
}

func TestReplayConverges(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	hd, shd := w.slotID("tmdb:603", "hd"), w.slotID("tvdb:9001", "hd") // tmdb:603/uhd is under the fixture's identity conflict
	req := AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{hd, shd}}
	rec := FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "Synthetic Movie A/a.mkv", SizeBytes: 4096}

	first := mustRequest(t, w.lib, req)
	if !first.Created || len(first.Attached) != 2 {
		t.Fatalf("first outcome %+v: want created with two slots attached", first)
	}
	if err := w.lib.RecordFile(ctx, rec); err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	requireFileRows(t, w.db, []string{fileRow(rec)})
	before := acqSnapshot(t, w.db)

	for i := range 3 {
		got := mustRequest(t, w.lib, req)
		if got.AcquisitionID != first.AcquisitionID || got.Created || len(got.Attached) != 0 {
			t.Fatalf("replay %d outcome %+v: want acquisition %d, nothing created or attached", i, got, first.AcquisitionID)
		}
		if err := w.lib.RecordFile(ctx, rec); err != nil {
			t.Fatalf("replay %d RecordFile: %v", i, err)
		}
		if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"}); err != nil {
			t.Fatalf("replay %d AddMembership: %v", i, err)
		}
	}
	if after := acqSnapshot(t, w.db); !maps.EqualFunc(before, after, slices.Equal) {
		t.Fatalf("replay changed rows: before %v after %v", before, after)
	}
	requireFileRows(t, w.db, []string{fileRow(rec)}) // the persisted row, not only the snapshot
}

// Rejected input writes nothing. The name extends TestReplayConverges so the
// proof's -run pattern selects it.
func TestReplayConvergesRejectedInputWritesNothing(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	hd, uhd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd")
	mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{hd}})
	if err := w.lib.RecordFile(ctx, FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "a/b.mkv", SizeBytes: 10}); err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	before := acqSnapshot(t, w.db)

	for _, c := range []struct {
		name string
		req  AcquisitionRequest
		want error
	}{
		{"empty client", AcquisitionRequest{ClientID: "", ReleaseKey: releaseB, Slots: []SlotID{hd}}, ErrInvalidInput},
		{"client with space", AcquisitionRequest{ClientID: "q bit", ReleaseKey: releaseB, Slots: []SlotID{hd}}, ErrInvalidInput},
		{"empty release", AcquisitionRequest{ClientID: "qbit", ReleaseKey: "", Slots: []SlotID{hd}}, ErrInvalidInput},
		{"control char release", AcquisitionRequest{ClientID: "qbit", ReleaseKey: "a\nb", Slots: []SlotID{hd}}, ErrInvalidInput},
		{"no slots", AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB}, ErrInvalidInput},
		{"zero slot", AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB, Slots: []SlotID{0}}, ErrInvalidInput},
		{"unknown slot after a real one", AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseB, Slots: []SlotID{uhd, 999999}}, ErrNotFound},
	} {
		if _, err := w.lib.RequestAcquisition(ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	for _, c := range []struct {
		name string
		rec  FileRecord
		want error
	}{
		{"absolute path", FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "/etc/x", SizeBytes: 1}, ErrInvalidInput},
		{"dotdot path", FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "a/../x", SizeBytes: 1}, ErrInvalidInput},
		{"negative size", FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "x", SizeBytes: -1}, ErrInvalidInput},
		{"unknown slot", FileRecord{SlotID: 999999, RootKey: "movies", RelativePath: "x", SizeBytes: 1}, ErrNotFound},
		{"path held by another slot", FileRecord{SlotID: uhd, RootKey: "movies", RelativePath: "a/b.mkv", SizeBytes: 10}, ErrIdentityConflict},
		{"path held with another size", FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "a/b.mkv", SizeBytes: 11}, ErrIdentityConflict},
	} {
		if err := w.lib.RecordFile(ctx, c.rec); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if after := acqSnapshot(t, w.db); !maps.EqualFunc(before, after, slices.Equal) {
		t.Fatalf("rejected input changed rows: before %v after %v", before, after)
	}
}

// Another spelling of a stored path is rejected for the owner and for another
// slot, and the stored file row stays as recorded. The name extends
// TestReplayConverges so the proof's -run pattern selects it.
func TestReplayConvergesRejectsPathAlias(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	hd, uhd := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd")
	rec := FileRecord{SlotID: hd, RootKey: "movies", RelativePath: "a/b.mkv", SizeBytes: 10}
	if err := w.lib.RecordFile(ctx, rec); err != nil {
		t.Fatalf("RecordFile: %v", err)
	}
	want := []string{fileRow(rec)}
	requireFileRows(t, w.db, want)
	before := acqSnapshot(t, w.db)

	for _, alias := range []string{"a/./b.mkv", "a//b.mkv", "./a/b.mkv", "a/b.mkv/", "a/c/../b.mkv", "./a/./b.mkv", "."} {
		for _, slot := range []SlotID{hd, uhd} {
			err := w.lib.RecordFile(ctx, FileRecord{SlotID: slot, RootKey: "movies", RelativePath: alias, SizeBytes: 10})
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("alias %q for slot %d: err = %v, want ErrInvalidInput", alias, slot, err)
			}
		}
	}
	requireFileRows(t, w.db, want)
	if after := acqSnapshot(t, w.db); !maps.EqualFunc(before, after, slices.Equal) {
		t.Fatalf("path alias changed rows: before %v after %v", before, after)
	}
}
