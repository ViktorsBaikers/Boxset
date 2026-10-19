package library

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Helpers.

// epKey names the episode with 1-based ord i of a series built by mustSeries:
// fifty episodes to a season.
func epKey(ord int) (season, number int) { return (ord-1)/50 + 1, (ord-1)%50 + 1 }

// mustSeries enrolls a series through lib and gives it n episodes with no air date.
func mustSeries(t *testing.T, w *s2World, lib, meta string, n int) (TitleID, []SlotID) {
	t.Helper()
	r, err := w.lib.AddMembership(t.Context(), MembershipRequest{LibraryID: w.libs[lib], MetadataID: meta, Kind: "series", Name: meta})
	if err != nil {
		t.Fatalf("AddMembership %s: %v", meta, err)
	}
	w.titles[meta] = r.TitleID
	if n > 0 {
		eps := make([]EpisodeSpec, n)
		for i := range eps {
			eps[i].Season, eps[i].Number = epKey(i + 1)
			eps[i].AirPrecision = "unknown"
		}
		if err := w.lib.AddEpisodes(t.Context(), r.TitleID, eps); err != nil {
			t.Fatalf("AddEpisodes %s: %v", meta, err)
		}
	}
	return r.TitleID, r.SlotIDs
}

func mustSet(t *testing.T, l *Library, slot SlotID, ord int, monitored bool, state string, size int64) {
	t.Helper()
	s, n := epKey(ord)
	if err := l.SetSlotEpisode(t.Context(), SlotEpisodeChange{SlotID: slot, Season: s, Number: n, Monitored: monitored, State: state, SizeBytes: size}); err != nil {
		t.Fatalf("SetSlotEpisode slot %d ord %d: %v", slot, ord, err)
	}
}

func mustMovie(t *testing.T, l *Library, slot SlotID, state string, size int64) {
	t.Helper()
	if err := l.SetMovieSlot(t.Context(), MovieSlotChange{SlotID: slot, State: state, SizeBytes: size}); err != nil {
		t.Fatalf("SetMovieSlot slot %d: %v", slot, err)
	}
}

func rollupOf(t *testing.T, l *Library, title TitleID, slot SlotID) Rollup {
	t.Helper()
	rs, err := l.Rollups(t.Context(), title)
	if err != nil {
		t.Fatalf("Rollups %d: %v", title, err)
	}
	for _, r := range rs {
		if r.SlotID == slot {
			return r
		}
	}
	t.Fatalf("title %d has no rollup for slot %d: %+v", title, slot, rs)
	return Rollup{}
}

const rollupDumpQuery = `SELECT * FROM title_rollup`

// pinWAL turns the writer's automatic checkpoint off, so the log only grows and
// the frames a commit wrote are the growth of the -wal file.
func pinWAL(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("wal_autocheckpoint reads back %d, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("pin WAL: %v", err)
	}
}

// walFrames is the number of frames in the -wal file; valid after pinWAL.
func walFrames(t *testing.T, db *store.DB) int64 {
	t.Helper()
	pageSize := int64(queryInt(t, db, `PRAGMA page_size`))
	fi, err := os.Stat(dbPath(t, db) + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return max(0, (fi.Size()-32)/(pageSize+24)) // 32-byte header, then frames of a 24-byte header and a page
}

// framesOf runs op and returns the WAL frames it wrote.
func framesOf(t *testing.T, db *store.DB, op func()) int64 {
	t.Helper()
	before := walFrames(t, db)
	op()
	return walFrames(t, db) - before
}

// rollupLine is one rollup row as a line, with its title; rollupSnapshot keys them by slot.
type rollupLine struct {
	title int64
	line  string
}

func rollupSnapshot(t *testing.T, db *store.DB) map[int64]rollupLine {
	t.Helper()
	out := map[int64]rollupLine{}
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return oracleCollect(t.Context(), tx, `SELECT slot_id, title_id, episodes, monitored, on_disk, missing, size_bytes, IFNULL(next_air_utc, -1),
			spine, spine_full, spine_omitted, revision FROM title_rollup`, func(r *sql.Rows) error {
			var slot, title, ep, mon, disk, miss, size, next, full, omitted, rev int64
			var spine string
			if err := r.Scan(&slot, &title, &ep, &mon, &disk, &miss, &size, &next, &spine, &full, &omitted, &rev); err != nil {
				return err
			}
			out[slot] = rollupLine{title, fmt.Sprintf("%d %d %d %d %d %d %q %d %d rev %d", ep, mon, disk, miss, size, next, spine, full, omitted, rev)}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("rollup snapshot: %v", err)
	}
	return out
}

// TestSpineWindowBounded pins the window rule on hand-computed spines: at most
// 24 marks, the anchor (first live, else first monitored unaired) last in the
// window or the first 24 when fewer precede it, the last 24 without an anchor,
// and full - shown = omitted.
func TestSpineWindowBounded(t *testing.T) {
	w := newS2World(t)
	title, slots := mustSeries(t, w, "Series", "tvdb:100", 100)
	slot := slots[0]
	m := strings.Repeat
	want := func(step, spine string, full, omitted int64) {
		t.Helper()
		r := rollupOf(t, w.lib, title, slot)
		if r.Spine != spine || r.SpineFull != full || r.SpineOmitted != omitted {
			t.Fatalf("%s: spine %q full %d omitted %d, want %q %d %d", step, r.Spine, r.SpineFull, r.SpineOmitted, spine, full, omitted)
		}
		if len(r.Spine) > 24 || r.SpineFull-int64(len(r.Spine)) != r.SpineOmitted {
			t.Fatalf("%s: %d marks, full %d, omitted %d break the bound", step, len(r.Spine), r.SpineFull, r.SpineOmitted)
		}
		requireOracle(t, w.db)
	}
	set := func(ord int, monitored bool, state string) { mustSet(t, w.lib, slot, ord, monitored, state, 0) }
	missing := func(ords ...int) {
		for _, o := range ords {
			set(o, true, "missing")
		}
	}

	want("no anchor: the last 24", m("M", 24), 100, 76)
	set(60, true, "live")
	want("live anchor ends the window", m("M", 23)+"L", 100, 76)
	set(20, true, "live")
	want("the first live row anchors; fewer than 24 precede it", m("M", 19)+"L"+m("M", 4), 100, 76)
	missing(20, 60)
	set(24, true, "live")
	want("anchor at the 24th mark", m("M", 23)+"L", 100, 76)
	missing(24)
	set(25, true, "live")
	want("anchor at the 25th mark slides the window", m("M", 23)+"L", 100, 76)
	missing(25)
	set(40, true, "unaired")
	set(30, false, "unaired")
	want("first monitored unaired anchors, unmonitored unaired reads O", m("M", 13)+"O"+m("M", 9)+"S", 100, 76)
	set(80, true, "live")
	want("a live row beats an unaired anchor", m("M", 23)+"L", 100, 76)
	for _, o := range []int{75, 76, 77} {
		set(o, true, "on_disk")
	}
	set(78, true, "upgrade")
	want("on disk and upgrade marks", m("M", 18)+"FFFUML", 100, 76)
	missing(80)
	want("losing the live row returns to the unaired anchor", m("M", 13)+"O"+m("M", 9)+"S", 100, 76)
	set(40, false, "unaired")
	want("only unmonitored unaired rows: no anchor", "FU"+m("M", 22), 100, 76)

	for _, c := range []struct {
		meta     string
		n        int
		ord      int // live row, 0 for none
		spine    string
		full, om int64
	}{
		{"tvdb:101", 5, 0, "MMMMM", 5, 0},
		{"tvdb:102", 24, 0, m("M", 24), 24, 0},
		{"tvdb:103", 25, 0, m("M", 24), 25, 1},
		{"tvdb:104", 25, 1, "L" + m("M", 23), 25, 1},
		{"tvdb:105", 25, 25, m("M", 23) + "L", 25, 1},
	} {
		id, ss := mustSeries(t, w, "Series", c.meta, c.n)
		if c.ord != 0 {
			mustSet(t, w.lib, ss[0], c.ord, true, "live", 0)
		}
		r := rollupOf(t, w.lib, id, ss[0])
		if r.Spine != c.spine || r.SpineFull != c.full || r.SpineOmitted != c.om {
			t.Errorf("%s: spine %q full %d omitted %d, want %q %d %d", c.meta, r.Spine, r.SpineFull, r.SpineOmitted, c.spine, c.full, c.om)
		}
	}
	id, ss := mustSeries(t, w, "Series", "tvdb:106", 5)
	mustSet(t, w.lib, ss[0], 3, true, "on_disk", 10)
	if r := rollupOf(t, w.lib, id, ss[0]); r.Spine != "MMFMM" {
		t.Errorf("short series spine = %q, want MMFMM", r.Spine)
	}

	// A movie slot is one mark: its state, or O while it is missing or unaired and unmonitored.
	for _, c := range []struct {
		lib, meta string
		marks     map[string]string
	}{
		{"Movies", "tmdb:900", map[string]string{"missing": "M", "on_disk": "F", "upgrade": "U", "live": "L", "unaired": "S"}},
		{"Kids", "tmdb:901", map[string]string{"missing": "O", "unaired": "O", "on_disk": "F", "live": "L"}},
	} {
		r, err := w.lib.AddMembership(t.Context(), MembershipRequest{LibraryID: w.libs[c.lib], MetadataID: c.meta, Kind: "movie", Name: c.meta})
		if err != nil {
			t.Fatal(err)
		}
		if got := rollupOf(t, w.lib, r.TitleID, r.SlotIDs[0]); got.Spine != c.marks["missing"] || got.SpineFull != 1 || got.SpineOmitted != 0 || got.Episodes != 1 {
			t.Errorf("new %s movie rollup = %+v, want spine %q of 1 mark", c.lib, got, c.marks["missing"])
		}
		for _, st := range []string{"on_disk", "upgrade", "live", "unaired", "missing"} {
			if _, ok := c.marks[st]; !ok {
				continue
			}
			mustMovie(t, w.lib, r.SlotIDs[0], st, 0)
			if got := rollupOf(t, w.lib, r.TitleID, r.SlotIDs[0]); got.Spine != c.marks[st] {
				t.Errorf("%s movie in %s: spine %q, want %q", c.lib, st, got.Spine, c.marks[st])
			}
		}
	}

	// A 5,000-episode series shows 24 marks and counts the rest.
	long, ls := mustSeries(t, w, "Series", "tvdb:5000", 5000)
	if r := rollupOf(t, w.lib, long, ls[0]); len(r.Spine) != 24 || r.SpineFull != 5000 || r.SpineOmitted != 4976 || r.Episodes != 5000 {
		t.Errorf("long series rollup = %+v, want 24 marks of 5000, 4976 omitted", r)
	}
	requireOracle(t, w.db)
}

// TestRollupPerTitleSlot: the S2 world has exactly one rollup row per (title,
// slot); a membership, however many, multiplies nothing; each rollup is that
// slot's own counts and bytes.
func TestRollupPerTitleSlot(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	requireOracle(t, w.db)

	if got, want := queryInt(t, w.db, `SELECT count(*) FROM title_rollup`), w.fx.Expect.Slots; got != want {
		t.Fatalf("rollup rows = %d, want one per slot = %d", got, want)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM (SELECT DISTINCT title_id, slot_id FROM title_rollup)`); got != w.fx.Expect.Slots {
		t.Fatalf("distinct (title, slot) rollups = %d, want %d", got, w.fx.Expect.Slots)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM slot s LEFT JOIN title_rollup r ON r.slot_id = s.id AND r.title_id = s.title_id WHERE r.slot_id IS NULL`); got != 0 {
		t.Fatalf("%d slots without a rollup row", got)
	}
	for meta, slots := range w.fx.Expect.TitleSlots {
		rs, err := w.lib.Rollups(ctx, w.titles[meta])
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != len(slots) {
			t.Errorf("%s: %d rollups, want %d (one per slot, not per membership)", meta, len(rs), len(slots))
		}
	}
	for _, e := range w.fx.Expect.SlotEpisode {
		r := rollupOf(t, w.lib, w.titles[e.MetadataID], w.slotID(e.MetadataID, e.SlotKey))
		if int(r.Episodes) != e.Rows || int(r.Monitored) != e.Monitored || int(r.OnDisk) != e.OnDisk || int(r.SpineFull) != e.Rows {
			t.Errorf("%s/%s rollup episodes/monitored/on_disk = %d/%d/%d, want %d/%d/%d", e.MetadataID, e.SlotKey, r.Episodes, r.Monitored, r.OnDisk, e.Rows, e.Monitored, e.OnDisk)
		}
	}
	if r := rollupOf(t, w.lib, w.titles["tvdb:9001"], w.slotID("tvdb:9001", "hd")); r.SizeBytes != 1000 || r.Missing != 4 || r.Spine != "FMMMM" {
		t.Errorf("tvdb:9001/hd rollup = %+v, want 1000 bytes, 4 missing, spine FMMMM", r)
	}
	if r := rollupOf(t, w.lib, w.titles["tvdb:9001"], w.slotID("tvdb:9001", "uhd")); r.SizeBytes != 0 || r.Monitored != 0 || r.Spine != "OOOOO" {
		t.Errorf("tvdb:9001/uhd rollup = %+v, want an untouched unmonitored slot", r)
	}

	// Membership changes touch no rollup: the same title in more libraries, and a removal and re-add.
	before := rollupSnapshot(t, w.db)
	for _, lib := range []string{"Movies", "Kids", "UHD", "Collector"} {
		if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs[lib], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"}); err != nil {
			t.Fatalf("AddMembership %s: %v", lib, err)
		}
	}
	if err := w.lib.RemoveMembership(ctx, w.libs["Movies"], w.titles["tmdb:603"]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:603", Kind: "movie", Name: "Synthetic Movie A"}); err != nil {
		t.Fatal(err)
	}
	if after := rollupSnapshot(t, w.db); !slices.Equal(sortedLines(after), sortedLines(before)) {
		t.Fatalf("membership changes moved rollups:\nbefore %v\nafter  %v", sortedLines(before), sortedLines(after))
	}

	// A membership that selects one more slot adds exactly one rollup row.
	if _, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Collector"], MetadataID: "tvdb:9001", Kind: "series", Name: "Synthetic Series"}); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM title_rollup`); got != w.fx.Expect.Slots+1 {
		t.Fatalf("rollup rows after one new slot = %d, want %d", got, w.fx.Expect.Slots+1)
	}
	if r := rollupOf(t, w.lib, w.titles["tvdb:9001"], w.slotID("tvdb:9001", "dc")); r.Episodes != 5 || r.Monitored != 5 || r.Missing != 5 || r.Spine != "MMMMM" {
		t.Errorf("new series slot rollup = %+v, want 5 monitored missing marks", r)
	}
	requireOracle(t, w.db)

	// Bytes belong to one slot: two slots of one movie, and a file row, never mix.
	hd, uhd, dc := w.slotID("tmdb:603", "hd"), w.slotID("tmdb:603", "uhd"), w.slotID("tmdb:603", "dc")
	mustMovie(t, w.lib, hd, "on_disk", 700)
	mustMovie(t, w.lib, uhd, "upgrade", 900)
	if err := w.lib.RecordFile(ctx, FileRecord{SlotID: dc, RootKey: "movies", RelativePath: "Synthetic Movie A/dc.mkv", SizeBytes: 12345}); err != nil {
		t.Fatal(err)
	}
	for slot, want := range map[SlotID]int64{hd: 700, uhd: 900, dc: 0} {
		if r := rollupOf(t, w.lib, w.titles["tmdb:603"], slot); r.SizeBytes != want {
			t.Errorf("movie slot %d rollup size = %d, want %d", slot, r.SizeBytes, want)
		}
	}
	if r := rollupOf(t, w.lib, w.titles["tmdb:603"], hd); r.OnDisk != 1 || r.Missing != 0 || r.Spine != "F" {
		t.Errorf("on_disk movie slot rollup = %+v", r)
	}
	requireOracle(t, w.db)
}

func sortedLines(m map[int64]rollupLine) []string {
	out := make([]string, 0, len(m))
	for slot, l := range m {
		out = append(out, fmt.Sprintf("slot %d title %d %s", slot, l.title, l.line))
	}
	slices.Sort(out)
	return out
}

// TestNoRevisionChurn: a mutation that changes nothing writes no WAL frame and
// no revision; a base change that moves no rollup value moves no revision.
func TestNoRevisionChurn(t *testing.T) {
	w := applyS2(t)
	pinWAL(t, w.db)
	ctx := t.Context()
	hd9001, uhd9002 := w.slotID("tvdb:9001", "hd"), w.slotID("tvdb:9002", "uhd")
	movie := w.slotID("tmdb:603", "hd")

	noops := []struct {
		name string
		op   func() error
	}{
		{"SetSlotEpisode to the stored values", func() error {
			return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: hd9001, Season: 1, Number: 1, Monitored: true, State: "on_disk", SizeBytes: 1000})
		}},
		{"SetSlotEpisode on a fresh row", func() error {
			return w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: uhd9002, Season: 2, Number: 2, Monitored: false, State: "missing"})
		}},
		{"SetMovieSlot to the stored state", func() error {
			return w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: movie, State: "missing"})
		}},
		{"AddEpisodes of existing episodes", func() error {
			return w.lib.AddEpisodes(ctx, w.titles["tvdb:9001"], []EpisodeSpec{
				{Season: 1, Number: 1, AirPrecision: "unknown"}, {Season: 2, Number: 2, AirPrecision: "unknown"}})
		}},
		{"SetEpisodeAir to the stored air", func() error {
			return w.lib.SetEpisodeAir(ctx, w.titles["tvdb:9001"], EpisodeSpec{Season: 1, Number: 1, AirPrecision: "unknown"})
		}},
		{"AddMembership replay", func() error {
			_, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Series"], MetadataID: "tvdb:9001", Kind: "series", Name: "Synthetic Series"})
			return err
		}},
	}
	before := rollupSnapshot(t, w.db)
	for _, n := range noops {
		if got := framesOf(t, w.db, func() {
			if err := n.op(); err != nil {
				t.Fatalf("%s: %v", n.name, err)
			}
		}); got != 0 {
			t.Errorf("%s wrote %d WAL frames, want 0", n.name, got)
		}
	}
	if after := rollupSnapshot(t, w.db); !slices.Equal(sortedLines(after), sortedLines(before)) {
		t.Fatalf("no-op mutations moved rollups:\nbefore %v\nafter  %v", sortedLines(before), sortedLines(after))
	}

	// The probe sees a real write: a change writes frames and moves exactly one revision.
	frames := framesOf(t, w.db, func() { mustSet(t, w.lib, hd9001, 2, true, "on_disk", 5) })
	if frames == 0 {
		t.Fatal("a real change wrote no WAL frame: the frame probe is blind")
	}
	changed := rollupSnapshot(t, w.db)
	for slot, l := range changed {
		if (l.line != before[slot].line) != (SlotID(slot) == hd9001) {
			t.Errorf("slot %d rollup %q -> %q after a change to slot %d only", slot, before[slot].line, l.line, hd9001)
		}
	}
	if r := rollupOf(t, w.lib, w.titles["tvdb:9001"], hd9001); r.Revision < 2 || r.OnDisk != 2 || r.SizeBytes != 1005 {
		t.Errorf("rollup after the change = %+v", r)
	}
	// Replaying it is a no-op again.
	if got := framesOf(t, w.db, func() { mustSet(t, w.lib, hd9001, 2, true, "on_disk", 5) }); got != 0 {
		t.Errorf("replayed change wrote %d frames, want 0", got)
	}

	// A base change outside the window that no rollup value reads: the rollup row keeps its revision.
	id, ss := mustSeries(t, w, "Series", "tvdb:200", 60)
	mustSet(t, w.lib, ss[0], 3, false, "upgrade", 0) // missing -> upgrade moves the counts
	settled := rollupOf(t, w.lib, id, ss[0])
	frames = framesOf(t, w.db, func() { mustSet(t, w.lib, ss[0], 3, false, "unaired", 0) }) // upgrade -> unaired, unmonitored, no instant
	after := rollupOf(t, w.lib, id, ss[0])
	if after != settled {
		t.Errorf("a change no rollup value reads moved the rollup: %+v -> %+v", settled, after)
	}
	if frames == 0 || queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND ord = 3 AND state = 'unaired'`, int64(ss[0])) != 1 {
		t.Errorf("the base row did not change (frames %d)", frames)
	}
	requireOracle(t, w.db)
}

// depthAllowance is how many more WAL frames a one-episode change in the long
// series may write than the same change in a 50-episode series. A change
// rewrites its slot_episode leaf, the leaf of any partial index it moves in or
// out of, and its rollup leaf; in the long series each of those B-trees is
// deeper and its full leaves can split, which adds a sibling and a parent per
// tree. Eight frames cover two such trees; the work is not proportional to the
// episode count, which a rewrite of the series would be (hundreds of frames).
const depthAllowance = 8

func TestOneEpisodeChangeBoundedWrites(t *testing.T) {
	measure := func(n int) (frames []int64) {
		w := newS2World(t)
		_, ss := mustSeries(t, w, "Series", "tvdb:300", n)
		pinWAL(t, w.db)
		for _, ord := range []int{1, n / 4, n / 2, 3 * n / 4, n} {
			for _, step := range []struct {
				name  string
				state string
				mon   bool
				size  int64
			}{{"on_disk", "on_disk", true, 123456789}, {"live", "live", true, 0}, {"unaired", "unaired", true, 0}, {"missing", "missing", true, 0}} {
				got := framesOf(t, w.db, func() { mustSet(t, w.lib, ss[0], ord, step.mon, step.state, step.size) })
				t.Logf("series of %d, ord %d -> %s: %d WAL frames", n, ord, step.name, got)
				frames = append(frames, got)
			}
		}
		requireOracle(t, w.db)
		return frames
	}
	short, long := measure(50), measure(5000)
	if slices.Min(short) < 1 {
		t.Fatalf("a change wrote no frame in the 50-episode series: %v", short)
	}
	maxShort, maxLong := slices.Max(short), slices.Max(long)
	t.Logf("max frames per one-episode change: 50 episodes %d, 5000 episodes %d, allowance %d", maxShort, maxLong, depthAllowance)
	if maxLong > maxShort+depthAllowance {
		t.Errorf("a one-episode change wrote up to %d frames in the 5000-episode series, %d in the 50-episode series, allowance %d", maxLong, maxShort, depthAllowance)
	}
}

// planLines returns the EXPLAIN QUERY PLAN detail lines of query.
func planLines(t *testing.T, db *store.DB, query string, args ...any) []string {
	t.Helper()
	var lines []string
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			lines = append(lines, detail)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	return lines
}

func TestIndexedQueryPlans(t *testing.T) {
	w := newS2World(t)
	_, ss := mustSeries(t, w, "Series", "tvdb:400", 5000)
	mustSet(t, w.lib, ss[0], 2500, true, "live", 0)
	title, slot := int64(w.titles["tvdb:400"]), int64(ss[0])
	nullArg := sql.NullInt64{}

	for _, c := range []struct {
		name  string
		query string
		args  []any
		index string // an index (or key) the plan must name
	}{
		{"rollup row", qRollupRow, []any{title, slot}, "sqlite_autoindex_title_rollup_1"},
		{"rollup set", qRollupSet, []any{1, 1, 1, 1, 1, nullArg, "", 1, 1, title, slot}, "sqlite_autoindex_title_rollup_1"},
		{"rollups of a title", qRollups, []any{title}, "sqlite_autoindex_title_rollup_1"},
		{"live anchor", qLiveAnchor, []any{slot}, "slot_episode_live"},
		{"unaired anchor", qUnairedAnchor, []any{slot}, "slot_episode_next_unaired"},
		{"next airing", qNextAir, []any{slot}, "slot_episode_airing"},
		{"window ending at the anchor", qWindowTo, []any{slot, 2500}, "slot_episode_by_ord"},
		{"first window", qWindowFirst, []any{slot}, "slot_episode_by_ord"},
		{"episode row", qSlotEpisodeRow, []any{slot, 1, 1}, "sqlite_autoindex_episode"},
		{"episode set", qSlotEpisodeSet, []any{1, "missing", 0, slot, 1}, "sqlite_autoindex_slot_episode_1"},
		{"episode by key", qEpisodeAir, []any{title, 1, 1}, "sqlite_autoindex_episode"},
		{"episode air set", qEpisodeAirSet, []any{nullArg, "unknown", 1}, "INTEGER PRIMARY KEY"},
		{"slot episode air set", qSlotEpisodeAirSet, []any{nullArg, slot, 1}, "sqlite_autoindex_slot_episode_1"},
		{"movie row", qMovieRow, []any{slot}, "INTEGER PRIMARY KEY"},
		{"movie set", qMovieSet, []any{"missing", 0, slot}, "INTEGER PRIMARY KEY"},
		{"files or downloads of a title", qTitleKept, []any{title}, "file_by_slot"},
		{"downloads of a title", qTitleKept, []any{title}, "download_slot_by_slot"},
	} {
		lines := planLines(t, w.db, c.query, c.args...)
		t.Logf("%s:\n\t%s", c.name, strings.Join(lines, "\n\t"))
		named := false
		for _, l := range lines {
			if strings.Contains(l, "TEMP B-TREE") {
				t.Errorf("%s builds a temp B-tree: %q", c.name, l)
			}
			if strings.HasPrefix(l, "SCAN") && !strings.Contains(l, "CONSTANT ROW") {
				t.Errorf("%s scans: %q", c.name, l)
			}
			named = named || strings.Contains(l, c.index)
		}
		if !named {
			t.Errorf("%s does not use %s: %v", c.name, c.index, lines)
		}
	}
}

// TestReaderSnapshotsConsistent: while the writer mutates, every reader
// transaction sees rollups equal to the recompute over the same snapshot.
func TestReaderSnapshotsConsistent(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	series := []SlotID{w.slotID("tvdb:9001", "hd"), w.slotID("tvdb:9001", "uhd"), w.slotID("tvdb:9002", "hd"), w.slotID("tvdb:9002", "uhd")}
	movie := w.slotID("tmdb:603", "hd")

	var done atomic.Bool
	var reads, bad atomic.Int64
	var firstErr atomic.Pointer[error]
	versions := map[string]struct{}{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(store.ReaderCount(), 4) {
		wg.Go(func() {
			for !done.Load() {
				var version string
				err := w.db.Read(ctx, func(tx *sql.Tx) error {
					if err := tx.QueryRowContext(ctx, `SELECT group_concat(revision) FROM (SELECT revision FROM title_rollup ORDER BY slot_id)`).Scan(&version); err != nil {
						return err
					}
					return oracleCheck(ctx, tx)
				})
				reads.Add(1)
				if err != nil {
					bad.Add(1)
					firstErr.CompareAndSwap(nil, &err)
					return
				}
				mu.Lock()
				versions[version] = struct{}{}
				mu.Unlock()
			}
		})
	}

	rng := rand.New(rand.NewPCG(7, 11))
	states := []string{"missing", "on_disk", "upgrade", "live", "unaired"}
	const writes = 150
	for i := range writes {
		slot := series[rng.IntN(len(series))]
		key := [][2]int{{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 2}}[rng.IntN(5)] // the fixture's episodes
		var err error
		switch i % 10 {
		case 0:
			err = w.lib.SetMovieSlot(ctx, MovieSlotChange{SlotID: movie, State: states[rng.IntN(5)], SizeBytes: rng.Int64N(1000)})
		case 1:
			err = w.lib.AddEpisodes(ctx, w.titles["tvdb:9002"], []EpisodeSpec{{Season: 5, Number: i, AirPrecision: "unknown"}})
		default:
			err = w.lib.SetSlotEpisode(ctx, SlotEpisodeChange{SlotID: slot, Season: key[0], Number: key[1], Monitored: rng.IntN(2) == 0, State: states[rng.IntN(5)], SizeBytes: rng.Int64N(1000)})
		}
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	done.Store(true)
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		t.Fatalf("a reader saw a rollup that differs from its own snapshot (%d of %d reads): %v", bad.Load(), reads.Load(), *e)
	}
	t.Logf("%d concurrent reads over %d writes saw %d distinct rollup versions, all equal to the recompute", reads.Load(), writes, len(versions))
	if reads.Load() == 0 || len(versions) < 2 {
		t.Fatalf("readers did not overlap the writer: %d reads, %d versions", reads.Load(), len(versions))
	}
	requireOracle(t, w.db)
}

// TestOracleDisagreementIsFailure: a rollup that differs from the recompute
// fails the oracle; reading never repairs it and a mutation never rebuilds it.
func TestOracleDisagreementIsFailure(t *testing.T) {
	for _, c := range []struct {
		name    string
		corrupt string // runs with the slot id
		repair  bool   // a mutation of the same slot may legitimately rewrite the stored value
	}{
		{"count", `UPDATE title_rollup SET on_disk = on_disk + 1 WHERE slot_id = ?`, false},
		{"size", `UPDATE title_rollup SET size_bytes = size_bytes + 1 WHERE slot_id = ?`, false},
		{"missing count", `UPDATE title_rollup SET missing = missing - 1 WHERE slot_id = ?`, false},
		{"next airing", `UPDATE title_rollup SET next_air_utc = 5 WHERE slot_id = ?`, true},
		{"spine", `UPDATE title_rollup SET spine = 'MMMMM' WHERE slot_id = ?`, true},
		{"omitted", `UPDATE title_rollup SET spine_omitted = spine_omitted + 1 WHERE slot_id = ?`, true},
		{"missing row", `DELETE FROM title_rollup WHERE slot_id = ?`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := applyS2(t)
			ctx := t.Context()
			requireOracle(t, w.db) // the healthy world agrees
			title, slot := w.titles["tvdb:9001"], w.slotID("tvdb:9001", "hd")
			if err := w.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, c.corrupt, int64(slot)); return err }); err != nil {
				t.Fatalf("corrupt: %v", err)
			}
			check := func(when string) {
				t.Helper()
				err := w.db.Read(ctx, func(tx *sql.Tx) error { return oracleCheck(ctx, tx) })
				if err == nil {
					t.Fatalf("%s: the oracle accepted a corrupted %s", when, c.name)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("slot %d", slot)) {
					t.Fatalf("%s: the oracle failure does not name slot %d: %v", when, slot, err)
				}
			}
			check("after corrupting")
			corrupted := dump(t, w.db, rollupDumpQuery)

			// Every read path leaves the corruption as it is.
			if _, err := w.lib.Rollups(ctx, title); err != nil {
				t.Fatal(err)
			}
			if _, err := w.lib.TitleSlots(ctx, title); err != nil {
				t.Fatal(err)
			}
			if got := dump(t, w.db, rollupDumpQuery); !slices.Equal(got, corrupted) {
				t.Fatalf("a read changed the rollups:\nbefore %v\nafter  %v", corrupted, got)
			}
			check("after reading")

			// A mutation of another title leaves it alone too.
			mustSet(t, w.lib, w.slotID("tvdb:9002", "hd"), 1, true, "on_disk", 9)
			check("after a mutation elsewhere")

			// A mutation of the same slot applies its delta to what is stored; it does not rebuild the count.
			change := SlotEpisodeChange{SlotID: slot, Season: 2, Number: 2, Monitored: true, State: "on_disk", SizeBytes: 77}
			err := w.lib.SetSlotEpisode(ctx, change)
			switch {
			case c.name == "missing row":
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("a mutation of a slot with no rollup row: err = %v, want ErrNotFound and no rebuild", err)
				}
				if got := queryInt(t, w.db, `SELECT count(*) FROM slot_episode WHERE slot_id = ? AND state = 'on_disk'`, int64(slot)); got != 1 {
					t.Fatalf("the failed mutation left %d on_disk rows, want the original 1", got)
				}
				check("after a refused mutation")
			case !c.repair:
				if err != nil {
					t.Fatal(err)
				}
				check("after a mutation of the same slot")
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestDeleteTitleRefusesWithFiles: a title with a file row or an attached
// download is refused and keeps everything; a title without is deleted with
// its rollups; membership removal never deletes a title.
func TestDeleteTitleRefusesWithFiles(t *testing.T) {
	w := applyS2(t)
	ctx := t.Context()
	filed, attached, plain := w.titles["tmdb:603"], w.titles["tvdb:9001"], w.titles["tvdb:9002"]
	if err := w.lib.RecordFile(ctx, FileRecord{SlotID: w.slotID("tmdb:603", "hd"), RootKey: "movies", RelativePath: "Synthetic Movie A/a.mkv", SizeBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	mustRequest(t, w.lib, AcquisitionRequest{ClientID: "qbit", ReleaseKey: releaseA, Slots: []SlotID{w.slotID("tvdb:9001", "hd")}})

	rollups, counts, files := dump(t, w.db, rollupDumpQuery), tableCounts(t, w.db), acqSnapshot(t, w.db)
	for name, id := range map[string]TitleID{"file row": filed, "download_slot row": attached} {
		if err := w.lib.DeleteTitle(ctx, id); !errors.Is(err, ErrTitleHasFiles) {
			t.Fatalf("DeleteTitle with a %s: err = %v, want ErrTitleHasFiles", name, err)
		}
	}
	if got := dump(t, w.db, rollupDumpQuery); !slices.Equal(got, rollups) {
		t.Fatalf("a refused delete moved rollups: %v -> %v", rollups, got)
	}
	if got := tableCounts(t, w.db); fmt.Sprint(got) != fmt.Sprint(counts) {
		t.Fatalf("a refused delete changed rows: %v -> %v", counts, got)
	}
	if got := acqSnapshot(t, w.db); fmt.Sprint(got) != fmt.Sprint(files) {
		t.Fatal("a refused delete changed acquisition or file rows")
	}

	// Removing the memberships does not free it either: files outlive membership.
	for _, lib := range []string{"Movies", "Kids", "UHD", "Collector"} {
		if err := w.lib.RemoveMembership(ctx, w.libs[lib], filed); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.lib.DeleteTitle(ctx, filed); !errors.Is(err, ErrTitleHasFiles) {
		t.Fatalf("DeleteTitle after removing memberships: err = %v, want ErrTitleHasFiles", err)
	}

	// A title without either is deleted with its slots, episodes and rollups; the others stay.
	slotsBefore := queryInt(t, w.db, `SELECT count(*) FROM title_rollup`)
	own := queryInt(t, w.db, `SELECT count(*) FROM title_rollup WHERE title_id = ?`, int64(plain))
	if own == 0 {
		t.Fatal("fixture title has no rollups")
	}
	if err := w.lib.DeleteTitle(ctx, plain); err != nil {
		t.Fatalf("DeleteTitle: %v", err)
	}
	if got := queryInt(t, w.db, `SELECT count(*) FROM title_rollup`); got != slotsBefore-own {
		t.Fatalf("rollup rows after delete = %d, want %d", got, slotsBefore-own)
	}
	for _, q := range []string{`SELECT count(*) FROM slot WHERE title_id = ?`, `SELECT count(*) FROM episode WHERE title_id = ?`,
		`SELECT count(*) FROM slot_episode WHERE title_id = ?`, `SELECT count(*) FROM library_title WHERE title_id = ?`} {
		if got := queryInt(t, w.db, q, int64(plain)); got != 0 {
			t.Errorf("%s = %d after delete", q, got)
		}
	}
	if rs, err := w.lib.Rollups(ctx, plain); err != nil || len(rs) != 0 {
		t.Errorf("Rollups of a deleted title = %v, %v", rs, err)
	}
	if err := w.lib.DeleteTitle(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteTitle of a deleted title: err = %v, want ErrNotFound", err)
	}
	requireOracle(t, w.db)
}

var (
	dateAir    = EpisodeSpec{AirPrecision: "date"}
	unknownAir = EpisodeSpec{AirPrecision: "unknown"}
)

func instantAir(v int64) EpisodeSpec { return EpisodeSpec{AirUTC: &v, AirPrecision: "instant"} }

// twoSlotSeries enrolls a series with two slots and one episode per spec, S1E1 on (epKey).
func twoSlotSeries(t *testing.T, w *s2World, meta string, specs ...EpisodeSpec) (TitleID, SlotID, SlotID) {
	t.Helper()
	ctx := t.Context()
	r, err := w.lib.AddMembership(ctx, MembershipRequest{LibraryID: w.libs["Series"], MetadataID: meta, Kind: "series", Name: meta})
	if err != nil {
		t.Fatalf("AddMembership %s: %v", meta, err)
	}
	w.titles[meta] = r.TitleID
	if _, err := w.lib.ApplyPersona(ctx, PersonaRequest{LibraryID: w.libs["Kids"], Persona: "4k", MetadataID: meta, Kind: "series", Name: meta}); err != nil {
		t.Fatalf("ApplyPersona %s: %v", meta, err)
	}
	eps := slices.Clone(specs)
	for i := range eps {
		eps[i].Season, eps[i].Number = epKey(i + 1)
	}
	if err := w.lib.AddEpisodes(ctx, r.TitleID, eps); err != nil {
		t.Fatalf("AddEpisodes %s: %v", meta, err)
	}
	ss, err := w.lib.TitleSlots(ctx, r.TitleID)
	if err != nil || len(ss) != 2 {
		t.Fatalf("TitleSlots %s = %v, %v; want two slots", meta, ss, err)
	}
	return r.TitleID, ss[0].ID, ss[1].ID
}

func mustAir(t *testing.T, l *Library, title TitleID, ord int, e EpisodeSpec) {
	t.Helper()
	e.Season, e.Number = epKey(ord)
	if err := l.SetEpisodeAir(t.Context(), title, e); err != nil {
		t.Fatalf("SetEpisodeAir title %d ord %d %+v: %v", title, ord, e, err)
	}
}

// wantNext fails unless every slot's public next air (Rollups) is want.
func wantNext(t *testing.T, step string, l *Library, title TitleID, want *int64, slots ...SlotID) {
	t.Helper()
	for _, s := range slots {
		got := rollupOf(t, l, title, s).NextAirUTC
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("%s: slot %d next air = %s, want %s", step, s, fmtAir(got), fmtAir(want))
		}
	}
}

func fmtAir(v *int64) string {
	if v == nil {
		return "nil"
	}
	return strconv.FormatInt(*v, 10)
}

func at(v int64) *int64 { return &v }

// TestRollupPerTitleSlotNextAir reads the next airing through Rollups, the
// public value: the earliest unaired instant of the slot, whatever its
// monitoring; a date-only or unknown air, a movie slot, and no unaired
// instant give nil.
func TestRollupPerTitleSlotNextAir(t *testing.T) {
	w := newS2World(t)
	l := w.lib
	title, a, b := twoSlotSeries(t, w, "tvdb:610", instantAir(5000), instantAir(3000), instantAir(4000), dateAir, unknownAir)

	wantNext(t, "instants on missing rows", l, title, nil, a, b)
	mustSet(t, l, a, 1, true, "unaired", 0)
	mustSet(t, l, a, 3, true, "unaired", 0)
	wantNext(t, "competing unaired instants", l, title, at(4000), a)
	mustSet(t, l, a, 2, false, "unaired", 0)
	wantNext(t, "earlier unmonitored unaired instant", l, title, at(3000), a)
	wantNext(t, "other slot owns its rows", l, title, nil, b)
	mustSet(t, l, a, 4, true, "unaired", 0)
	mustSet(t, l, a, 5, true, "unaired", 0)
	wantNext(t, "date-only and unknown beside instants", l, title, at(3000), a)
	mustSet(t, l, b, 4, true, "unaired", 0)
	mustSet(t, l, b, 5, true, "unaired", 0)
	wantNext(t, "date-only and unknown alone", l, title, nil, b)

	mustSet(t, l, a, 2, false, "live", 0)
	wantNext(t, "earliest leaves unaired", l, title, at(4000), a)
	mustSet(t, l, a, 3, true, "on_disk", 1)
	wantNext(t, "next leaves unaired", l, title, at(5000), a)
	mustSet(t, l, a, 1, false, "unaired", 0)
	wantNext(t, "unmonitoring keeps the instant", l, title, at(5000), a)
	mustSet(t, l, a, 1, true, "missing", 0)
	wantNext(t, "last instant leaves unaired", l, title, nil, a)

	m, err := l.AddMembership(t.Context(), MembershipRequest{LibraryID: w.libs["Movies"], MetadataID: "tmdb:611", Kind: "movie", Name: "Movie"})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "unaired", "on_disk"} {
		mustMovie(t, l, m.SlotIDs[0], state, 0)
		wantNext(t, "movie slot "+state, l, m.TitleID, nil, m.SlotIDs[0])
	}
	requireOracle(t, w.db)
}

// TestRollupPerTitleSlotAirChange: SetEpisodeAir moves the episode row and
// every slot's copy and rollup together; the same air twice writes nothing;
// AddEpisodes never changes an existing air.
func TestRollupPerTitleSlotAirChange(t *testing.T) {
	w := newS2World(t)
	l, ctx := w.lib, t.Context()
	pinWAL(t, w.db)
	title, a, b := twoSlotSeries(t, w, "tvdb:620", instantAir(1000), unknownAir, instantAir(2000))
	mustSet(t, l, a, 1, true, "unaired", 0)
	mustSet(t, l, b, 1, false, "unaired", 0)
	wantNext(t, "start", l, title, at(1000), a, b)

	stored := func(ord int, e EpisodeSpec) {
		t.Helper()
		s, n := epKey(ord)
		if got := queryInt(t, w.db, `SELECT count(*) FROM episode WHERE title_id = ? AND season = ? AND number = ? AND air_precision = ? AND air_utc IS ?`,
			int64(title), s, n, e.AirPrecision, e.AirUTC); got != 1 {
			t.Errorf("episode %d is not stored as %+v", ord, e)
		}
		if got := queryInt(t, w.db, `SELECT count(*) FROM slot_episode se JOIN episode e ON e.id = se.episode_id
			WHERE e.title_id = ? AND e.season = ? AND e.number = ? AND se.air_utc IS ?`, int64(title), s, n, e.AirUTC); got != 2 {
			t.Errorf("%d slot rows of episode %d carry %s, want 2", got, ord, fmtAir(e.AirUTC))
		}
	}
	revisions := func() [2]int64 {
		return [2]int64{rollupOf(t, l, title, a).Revision, rollupOf(t, l, title, b).Revision}
	}

	for _, c := range []struct {
		name string
		air  EpisodeSpec
		next *int64
	}{
		{"earlier instant", instantAir(500), at(500)},
		{"later instant", instantAir(900), at(900)},
		{"instant to date", dateAir, nil},
		{"date to instant", instantAir(700), at(700)},
		{"instant to unknown", unknownAir, nil},
	} {
		before := revisions()
		mustAir(t, l, title, 1, c.air)
		wantNext(t, c.name, l, title, c.next, a, b)
		stored(1, c.air)
		if got := revisions(); got != [2]int64{before[0] + 1, before[1] + 1} {
			t.Errorf("%s: revisions %v -> %v, want each slot to move by one", c.name, before, got)
		}
		if frames := framesOf(t, w.db, func() { mustAir(t, l, title, 1, c.air) }); frames != 0 {
			t.Errorf("%s: the same air again wrote %d WAL frames, want 0", c.name, frames)
		}
		if got := revisions(); got != [2]int64{before[0] + 1, before[1] + 1} {
			t.Errorf("%s: the same air again moved revisions to %v", c.name, got)
		}
	}

	// AddEpisodes stays append-only: a repeated key keeps its air.
	snap := rollupSnapshot(t, w.db)
	if err := l.AddEpisodes(ctx, title, []EpisodeSpec{{Season: 1, Number: 1, AirUTC: at(12345), AirPrecision: "instant"}}); err != nil {
		t.Fatal(err)
	}
	stored(1, unknownAir)
	if after := rollupSnapshot(t, w.db); !slices.Equal(sortedLines(after), sortedLines(snap)) {
		t.Fatalf("AddEpisodes of an existing episode moved rollups:\nbefore %v\nafter  %v", sortedLines(snap), sortedLines(after))
	}

	// Competing instants: the earliest of the unaired ones wins as either moves.
	mustAir(t, l, title, 1, instantAir(3000))
	mustSet(t, l, a, 3, true, "unaired", 0)
	mustSet(t, l, b, 3, true, "unaired", 0)
	wantNext(t, "competing", l, title, at(2000), a, b)
	mustAir(t, l, title, 3, instantAir(4000))
	wantNext(t, "earliest moves later", l, title, at(3000), a, b)
	mustAir(t, l, title, 3, instantAir(100))
	wantNext(t, "later moves earliest", l, title, at(100), a, b)

	// An episode no slot has unaired changes no rollup; the slot that later marks it unaired sees the new air.
	snap = rollupSnapshot(t, w.db)
	mustAir(t, l, title, 2, instantAir(50))
	stored(2, instantAir(50))
	if after := rollupSnapshot(t, w.db); !slices.Equal(sortedLines(after), sortedLines(snap)) {
		t.Fatalf("an air change on rows that are not unaired moved rollups:\nbefore %v\nafter  %v", sortedLines(snap), sortedLines(after))
	}
	mustSet(t, l, a, 2, true, "unaired", 0)
	wantNext(t, "slot marks the changed episode unaired", l, title, at(50), a)
	wantNext(t, "other slot keeps its own rows", l, title, at(100), b)

	for _, c := range []struct {
		name string
		id   TitleID
		e    EpisodeSpec
		want error
	}{
		{"unknown episode", title, EpisodeSpec{Season: 77, Number: 1, AirPrecision: "unknown"}, ErrNotFound},
		{"unknown title", 999999, EpisodeSpec{Season: 1, Number: 1, AirPrecision: "unknown"}, ErrNotFound},
		{"instant without a value", title, EpisodeSpec{Season: 1, Number: 1, AirPrecision: "instant"}, ErrInvalidInput},
		{"date with a value", title, EpisodeSpec{Season: 1, Number: 1, AirUTC: at(1), AirPrecision: "date"}, ErrInvalidInput},
	} {
		if err := l.SetEpisodeAir(ctx, c.id, c.e); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	requireOracle(t, w.db)
}

// TestRollupPerTitleSlotAirChangeRollsBack: a failure inside SetEpisodeAir's own
// write, after the episode row and the first slot are updated and before the
// last slot's rollup lands, leaves the episode air, both slot copies, the
// public next air and the revisions as they were; the same change then commits
// on both slots.
func TestRollupPerTitleSlotAirChangeRollsBack(t *testing.T) {
	w := newS2World(t)
	l, ctx := w.lib, t.Context()
	title, a, b := twoSlotSeries(t, w, "tvdb:621", instantAir(1000), unknownAir, instantAir(2000))
	mustSet(t, l, a, 1, true, "unaired", 0)
	mustSet(t, l, b, 1, true, "unaired", 0)
	wantNext(t, "start", l, title, at(1000), a, b)
	requireOracle(t, w.db)

	state := func() [][]string {
		var out [][]string
		for _, q := range []string{`SELECT * FROM episode`, `SELECT * FROM slot_episode`, rollupDumpQuery} {
			out = append(out, dump(t, w.db, q))
		}
		return out
	}
	before := state()
	revs := func() [2]int64 { return [2]int64{rollupOf(t, l, title, a).Revision, rollupOf(t, l, title, b).Revision} }
	beforeRevs := revs()

	// The second slot rollup of the title fails, whichever slot goes first: by then the
	// episode row, the first slot's copy and the first slot's rollup are in the transaction.
	sum := queryInt(t, w.db, `SELECT sum(revision) FROM title_rollup WHERE title_id = ?`, int64(title))
	trigger := func(ddl string) {
		t.Helper()
		if err := w.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, ddl); return err }); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	trigger(`CREATE TRIGGER air_rollback_probe BEFORE UPDATE ON title_rollup
		WHEN (SELECT sum(revision) FROM title_rollup WHERE title_id = NEW.title_id) > ` + strconv.Itoa(sum) +
		` BEGIN SELECT RAISE(ABORT, 'air rollback probe'); END`)

	err := l.SetEpisodeAir(ctx, title, EpisodeSpec{Season: 1, Number: 1, AirUTC: at(500), AirPrecision: "instant"})
	if err == nil || !strings.Contains(err.Error(), "air rollback probe") {
		t.Fatalf("SetEpisodeAir err = %v, want the injected failure", err)
	}
	after := state()
	for i, q := range []string{"episode", "slot_episode", "title_rollup"} {
		if !slices.Equal(before[i], after[i]) {
			t.Fatalf("failed air change left %s rows: before %v after %v", q, before[i], after[i])
		}
	}
	if got := revs(); got != beforeRevs {
		t.Fatalf("failed air change moved revisions %v -> %v", beforeRevs, got)
	}
	wantNext(t, "after the failed change", l, title, at(1000), a, b)
	requireOracle(t, w.db)

	trigger(`DROP TRIGGER air_rollback_probe`)
	if err := l.SetEpisodeAir(ctx, title, EpisodeSpec{Season: 1, Number: 1, AirUTC: at(500), AirPrecision: "instant"}); err != nil {
		t.Fatalf("SetEpisodeAir retry: %v", err)
	}
	wantNext(t, "after the retry", l, title, at(500), a, b)
	if got := revs(); got != [2]int64{beforeRevs[0] + 1, beforeRevs[1] + 1} {
		t.Fatalf("retry revisions %v -> %v, want each slot to move by one", beforeRevs, got)
	}
	requireOracle(t, w.db)
}

// The randomized run: one 5,000-episode series among short titles, mutated at
// random; every commit leaves the rollups equal to the recompute, and a
// mutation leaves other titles' rollups byte for byte as they were.

const (
	oracleSeedEnv      = "BOXSET_ORACLE_SEED"
	oracleMutationsEnv = "BOXSET_ORACLE_MUTATIONS"
	longSeriesEpisodes = 5000
)

type randTitle struct {
	id    TitleID
	meta  string
	movie bool
	long  bool
	slots []SlotID
	keys  [][2]int // (season, number) of each episode
	files int
}

type randRun struct {
	t      *testing.T
	w      *s2World
	rng    *rand.Rand
	titles []*randTitle
	next   int // counter for fresh ids, seasons and paths
	base   int64
}

func (r *randRun) refreshSlots(tt *randTitle) {
	r.t.Helper()
	ss, err := r.w.lib.TitleSlots(r.t.Context(), tt.id)
	if err != nil {
		r.t.Fatalf("TitleSlots: %v", err)
	}
	tt.slots = tt.slots[:0]
	for _, s := range ss {
		tt.slots = append(tt.slots, s.ID)
	}
}

func (r *randRun) air() (*int64, string) {
	switch r.rng.IntN(3) {
	case 0:
		v := r.base + r.rng.Int64N(100_000_000)
		return &v, "instant"
	case 1:
		return nil, "date"
	}
	return nil, "unknown"
}

func (r *randRun) freshID() int { r.next++; return r.next }

func (r *randRun) series() []*randTitle {
	var out []*randTitle
	for _, tt := range r.titles {
		if !tt.movie {
			out = append(out, tt)
		}
	}
	return out
}

// pickSeries skews toward the long series.
func (r *randRun) pickSeries() *randTitle {
	ss := r.series()
	for _, tt := range ss {
		if tt.long && r.rng.IntN(100) < 55 {
			return tt
		}
	}
	return ss[r.rng.IntN(len(ss))]
}

func (r *randRun) pickKey(tt *randTitle) [2]int {
	if tt.long && r.rng.IntN(2) == 0 { // half of the long series' changes land in its last 30 episodes
		return tt.keys[len(tt.keys)-1-r.rng.IntN(30)]
	}
	return tt.keys[r.rng.IntN(len(tt.keys))]
}

func (r *randRun) pickShort() *randTitle {
	var short []*randTitle
	for _, tt := range r.titles {
		if !tt.long {
			short = append(short, tt)
		}
	}
	if len(short) == 0 {
		return nil
	}
	return short[r.rng.IntN(len(short))]
}

func (r *randRun) remove(tt *randTitle) {
	r.titles = slices.DeleteFunc(r.titles, func(x *randTitle) bool { return x == tt })
}

var randLibs = []string{"Movies", "Kids", "UHD", "Collector", "Series"}
var randStates = []string{"missing", "on_disk", "upgrade", "live", "unaired"}

// enroll adds a new title through a random library; a series gets 1-8 episodes.
func (r *randRun) enroll() *randTitle {
	t, ctx := r.t, r.t.Context()
	movie := r.rng.IntN(2) == 0
	kind, prefix := "series", "tvdb"
	if movie {
		kind, prefix = "movie", "tmdb"
	}
	meta := prefix + ":" + strconv.Itoa(10_000+r.freshID())
	res, err := r.w.lib.AddMembership(ctx, MembershipRequest{LibraryID: r.w.libs[randLibs[r.rng.IntN(len(randLibs))]], MetadataID: meta, Kind: kind, Name: meta})
	if err != nil {
		t.Fatalf("AddMembership %s: %v", meta, err)
	}
	tt := &randTitle{id: res.TitleID, meta: meta, movie: movie, slots: slices.Clone(res.SlotIDs)}
	if !movie {
		var eps []EpisodeSpec
		for n := 1; n <= 1+r.rng.IntN(8); n++ {
			air, prec := r.air()
			eps = append(eps, EpisodeSpec{Season: 1, Number: n, AirUTC: air, AirPrecision: prec})
			tt.keys = append(tt.keys, [2]int{1, n})
		}
		if err := r.w.lib.AddEpisodes(ctx, tt.id, eps); err != nil {
			t.Fatalf("AddEpisodes %s: %v", meta, err)
		}
	}
	r.titles = append(r.titles, tt)
	return tt
}

// step performs one random mutation and names it; touched is the title whose
// rollups may change (0: none).
func (r *randRun) step() (name string, touched TitleID) {
	t, ctx, l := r.t, r.t.Context(), r.w.lib
	switch roll := r.rng.IntN(100); {
	case roll < 48: // state, monitoring and size of one episode
		tt := r.pickSeries()
		slot, key := tt.slots[r.rng.IntN(len(tt.slots))], r.pickKey(tt)
		size := int64(0)
		if r.rng.IntN(2) == 0 {
			size = r.rng.Int64N(1 << 40)
		}
		c := SlotEpisodeChange{SlotID: slot, Season: key[0], Number: key[1], Monitored: r.rng.IntN(3) > 0, State: randStates[r.rng.IntN(5)], SizeBytes: size}
		if err := l.SetSlotEpisode(ctx, c); err != nil {
			t.Fatalf("SetSlotEpisode %+v: %v", c, err)
		}
		return fmt.Sprintf("SetSlotEpisode %+v", c), tt.id
	case roll < 55: // a movie slot's state and size
		var movies []*randTitle
		for _, tt := range r.titles {
			if tt.movie {
				movies = append(movies, tt)
			}
		}
		if len(movies) == 0 {
			return "skip: no movie", 0
		}
		tt := movies[r.rng.IntN(len(movies))]
		c := MovieSlotChange{SlotID: tt.slots[r.rng.IntN(len(tt.slots))], State: randStates[r.rng.IntN(5)], SizeBytes: r.rng.Int64N(1 << 30)}
		if err := l.SetMovieSlot(ctx, c); err != nil {
			t.Fatalf("SetMovieSlot %+v: %v", c, err)
		}
		return fmt.Sprintf("SetMovieSlot %+v", c), tt.id
	case roll < 63: // new episodes, with air instants, dates or nothing; one may repeat an existing key
		tt := r.pickSeries()
		season := 1000 + r.freshID()
		var eps []EpisodeSpec
		for n := 1; n <= 1+r.rng.IntN(5); n++ {
			air, prec := r.air()
			eps = append(eps, EpisodeSpec{Season: season, Number: n, AirUTC: air, AirPrecision: prec})
			tt.keys = append(tt.keys, [2]int{season, n})
		}
		eps = append(eps, EpisodeSpec{Season: tt.keys[0][0], Number: tt.keys[0][1], AirPrecision: "unknown"})
		if err := l.AddEpisodes(ctx, tt.id, eps); err != nil {
			t.Fatalf("AddEpisodes: %v", err)
		}
		return fmt.Sprintf("AddEpisodes %d to %s", len(eps), tt.meta), tt.id
	case roll < 71: // a new title
		tt := r.enroll()
		return "enroll " + tt.meta, tt.id
	case roll < 79: // a slot added to a title that exists
		tt := r.titles[r.rng.IntN(len(r.titles))]
		kind := "series"
		if tt.movie {
			kind = "movie"
		}
		var err error
		if r.rng.IntN(2) == 0 {
			_, err = l.AddMembership(ctx, MembershipRequest{LibraryID: r.w.libs["Collector"], MetadataID: tt.meta, Kind: kind, Name: tt.meta})
		} else {
			_, err = l.ApplyPersona(ctx, PersonaRequest{LibraryID: r.w.libs["Kids"], Persona: "4k", MetadataID: tt.meta, Kind: kind, Name: tt.meta})
		}
		if err != nil {
			t.Fatalf("slot add to %s: %v", tt.meta, err)
		}
		r.refreshSlots(tt)
		return "slot add to " + tt.meta, tt.id
	case roll < 85: // delete a short title; one with a file row is refused
		tt := r.pickShort()
		if tt == nil {
			return "skip: no short title", 0
		}
		err := l.DeleteTitle(ctx, tt.id)
		switch {
		case tt.files > 0 && errors.Is(err, ErrTitleHasFiles):
			return "DeleteTitle refused " + tt.meta, 0
		case tt.files == 0 && err == nil:
			r.remove(tt)
			return "DeleteTitle " + tt.meta, tt.id
		}
		t.Fatalf("DeleteTitle %s with %d files: %v", tt.meta, tt.files, err)
	case roll < 89: // a file row: not a rollup input
		tt := r.titles[r.rng.IntN(len(r.titles))]
		rec := FileRecord{SlotID: tt.slots[r.rng.IntN(len(tt.slots))], RootKey: "lib", RelativePath: fmt.Sprintf("t%d/f%d.mkv", tt.id, r.freshID()), SizeBytes: r.rng.Int64N(1 << 30)}
		if err := l.RecordFile(ctx, rec); err != nil {
			t.Fatalf("RecordFile: %v", err)
		}
		tt.files++
		return fmt.Sprintf("RecordFile %+v", rec), 0
	case roll < 94: // membership removed and added back
		tt := r.titles[r.rng.IntN(len(r.titles))]
		lib := randLibs[r.rng.IntN(len(randLibs))]
		if err := l.RemoveMembership(ctx, r.w.libs[lib], tt.id); err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("RemoveMembership: %v", err)
		}
		if r.rng.IntN(2) == 0 {
			kind := "series"
			if tt.movie {
				kind = "movie"
			}
			if _, err := l.AddMembership(ctx, MembershipRequest{LibraryID: r.w.libs[lib], MetadataID: tt.meta, Kind: kind, Name: tt.meta}); err != nil {
				t.Fatalf("AddMembership: %v", err)
			}
			r.refreshSlots(tt)
			return "membership churn " + lib + " " + tt.meta, tt.id
		}
		return "RemoveMembership " + lib + " " + tt.meta, 0
	case roll < 97: // the air of an episode that exists: an instant, a date or nothing; a repeat is a no-op
		tt := r.pickSeries()
		key := r.pickKey(tt)
		air, prec := r.air()
		e := EpisodeSpec{Season: key[0], Number: key[1], AirUTC: air, AirPrecision: prec}
		for range 1 + r.rng.IntN(2) {
			if err := l.SetEpisodeAir(ctx, tt.id, e); err != nil {
				t.Fatalf("SetEpisodeAir %+v: %v", e, err)
			}
		}
		return fmt.Sprintf("SetEpisodeAir %d.%d to %s of %s", key[0], key[1], prec, tt.meta), tt.id
	default: // the same episode change twice: a no-op
		tt := r.pickSeries()
		c := SlotEpisodeChange{SlotID: tt.slots[0], Season: tt.keys[0][0], Number: tt.keys[0][1], Monitored: true, State: "on_disk", SizeBytes: 42}
		for range 2 {
			if err := l.SetSlotEpisode(ctx, c); err != nil {
				t.Fatalf("SetSlotEpisode %+v: %v", c, err)
			}
		}
		return fmt.Sprintf("SetSlotEpisode twice %+v", c), tt.id
	}
	return "unreachable", 0
}

func TestRandomizedMutationsMatchOracle(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv(oracleSeedEnv); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: %v", oracleSeedEnv, s, err)
		}
		seed = v
	}
	mutations := 300
	if testing.Short() {
		mutations = 80
	}
	if s := os.Getenv(oracleMutationsEnv); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			t.Fatalf("%s=%q: want a positive integer", oracleMutationsEnv, s)
		}
		mutations = v
	}
	t.Logf("seed %d (replay with %s=%d), %d mutations", seed, oracleSeedEnv, seed, mutations)

	w := newS2World(t)
	r := &randRun{t: t, w: w, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), base: 2_000_000_000}

	// The long series has two slots, one monitored and one not; its last 100 episodes carry air instants.
	res, err := w.lib.AddMembership(t.Context(), MembershipRequest{LibraryID: w.libs["Series"], MetadataID: "tvdb:5000", Kind: "series", Name: "Long Series"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.lib.ApplyPersona(t.Context(), PersonaRequest{LibraryID: w.libs["Kids"], Persona: "4k", MetadataID: "tvdb:5000", Kind: "series", Name: "Long Series"}); err != nil {
		t.Fatal(err)
	}
	long := &randTitle{id: res.TitleID, meta: "tvdb:5000", long: true}
	eps := make([]EpisodeSpec, longSeriesEpisodes)
	for i := range eps {
		s, n := epKey(i + 1)
		eps[i] = EpisodeSpec{Season: s, Number: n, AirPrecision: "unknown"}
		switch {
		case i >= longSeriesEpisodes-100:
			v := r.base + int64(i)*86400
			eps[i].AirUTC, eps[i].AirPrecision = &v, "instant"
		case i%11 == 0:
			eps[i].AirPrecision = "date"
		}
		long.keys = append(long.keys, [2]int{s, n})
	}
	if err := w.lib.AddEpisodes(t.Context(), long.id, eps); err != nil {
		t.Fatal(err)
	}
	r.refreshSlots(long)
	r.titles = append(r.titles, long)
	for range 24 {
		r.enroll()
	}
	requireOracle(t, w.db)

	ops := map[string]int{}
	for i := 1; i <= mutations; i++ {
		before := rollupSnapshot(t, w.db)
		name, touched := r.step()
		kind, _, _ := strings.Cut(name, " ")
		ops[kind]++
		after := rollupSnapshot(t, w.db)
		for slot, b := range before {
			if a, ok := after[slot]; b.title != int64(touched) && (!ok || a != b) {
				t.Fatalf("seed %d mutation %d %q changed the rollup of slot %d of untouched title %d: %v -> %v", seed, i, name, slot, b.title, b, a)
			}
		}
		for slot, a := range after {
			if _, ok := before[slot]; !ok && a.title != int64(touched) {
				t.Fatalf("seed %d mutation %d %q added a rollup for slot %d of untouched title %d", seed, i, name, slot, a.title)
			}
		}
		if err := w.db.Read(t.Context(), func(tx *sql.Tx) error { return oracleCheck(t.Context(), tx) }); err != nil {
			t.Fatalf("seed %d mutation %d %q: rollups differ from the full recompute: %v", seed, i, name, err)
		}
	}
	t.Logf("seed %d: %d mutations each followed by a full recompute, %d titles left, ops %v", seed, mutations, len(r.titles), ops)
}
