package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

// The producer tests run in-process on byte streams in Git's exact `ls-tree -r
// -l -z --full-tree` and `cat-file --batch` formats: the work image has no Git,
// and the real Git path belongs to the runner smoke.

var refTestAreas = []refArea{
	{"tests", "src/T/Tests"},
	{"parser", "src/T/Parser"},
	{"qualities", "src/T/Qualities"},
}

// archiveName maps an upstream path to its archive member name ("" outside the listed directories).
func archiveName(upstreamPath string) string {
	for _, a := range refTestAreas {
		if rest, ok := strings.CutPrefix(upstreamPath, a.upstream+"/"); ok {
			return a.name + "/" + rest
		}
	}
	return ""
}

// refFixture is a listing and the matching batch output, in archive order.
type refFixture struct{ listing, objects []byte }

func newRefFixture(files map[string][]byte) refFixture {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	type item struct {
		name, id string
		data     []byte
	}
	var listing bytes.Buffer
	var items []item
	for i, p := range paths {
		id := fmt.Sprintf("%040x", i+1)
		fmt.Fprintf(&listing, "100644 blob %s %7d\t%s\x00", id, len(files[p]), p)
		items = append(items, item{archiveName(p), id, files[p]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	var objects bytes.Buffer
	for _, it := range items {
		fmt.Fprintf(&objects, "%s blob %d\n", it.id, len(it.data))
		objects.Write(it.data)
		objects.WriteByte('\n')
	}
	return refFixture{listing.Bytes(), objects.Bytes()}
}

// spyObjects serves a canned batch stream and records whether it was asked for.
type spyObjects struct {
	stream []byte
	called bool
}

func (s *spyObjects) open([]string) (io.Reader, error) {
	s.called = true
	return bytes.NewReader(s.stream), nil
}

func buildRef(listing, objects []byte) (int, []byte, *spyObjects) {
	spy := &spyObjects{stream: objects}
	var out bytes.Buffer
	status := refArchive(&out, listing, refTestAreas, spy.open)
	return status, out.Bytes(), spy
}

type archived struct {
	hdr  tar.Header
	body []byte
}

func readArchive(t *testing.T, data []byte) []archived {
	t.Helper()
	var all []archived
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			t.Fatalf("archive/tar cannot read the stream: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		all = append(all, archived{*h, body})
	}
}

func TestRefArchiveWritesGenuineUSTARStream(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), (2<<20)/16) // exactly 2 MiB
	files := map[string][]byte{
		"src/T/Tests/Zeta.cs":          []byte("zeta"),
		"src/T/Tests/Sub/Deep/a.cs":    []byte("deep\r\nunconverted\n"),
		"src/T/Tests/Sub/b.cs":         []byte("sub"),
		"src/T/Tests/big.cs":           big,
		"src/T/Parser/Parser.cs":       []byte("parser"),
		"src/T/Qualities/Qualities.cs": []byte(""),
	}
	fx := newRefFixture(files)
	status, out, spy := buildRef(fx.listing, fx.objects)
	if status != refExitOK || !spy.called {
		t.Fatalf("status %d, objects requested %v", status, spy.called)
	}
	want := []struct {
		name string
		body []byte
	}{
		{"parser/", nil}, {"parser/Parser.cs", files["src/T/Parser/Parser.cs"]},
		{"qualities/", nil}, {"qualities/Qualities.cs", []byte("")},
		{"tests/", nil}, {"tests/Sub/", nil}, {"tests/Sub/Deep/", nil},
		{"tests/Sub/Deep/a.cs", files["src/T/Tests/Sub/Deep/a.cs"]}, {"tests/Sub/b.cs", files["src/T/Tests/Sub/b.cs"]},
		{"tests/Zeta.cs", files["src/T/Tests/Zeta.cs"]}, {"tests/big.cs", big},
	}
	got := readArchive(t, out)
	if len(got) != len(want) {
		t.Fatalf("%d members, want %d", len(got), len(want))
	}
	for i, w := range want {
		h := got[i].hdr
		isDir := strings.HasSuffix(w.name, "/")
		mode, typeflag := int64(0o444), byte(tar.TypeReg)
		if isDir {
			mode, typeflag = 0o555, tar.TypeDir
		}
		if h.Name != w.name || h.Typeflag != typeflag || h.Mode != mode || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.ModTime.Unix() != 0 {
			t.Errorf("member %d header = %+v, want %s type %c mode %o owner 0/0 unnamed time 0", i, h, w.name, typeflag, mode)
		}
		if !isDir && !bytes.Equal(got[i].body, w.body) {
			t.Errorf("%s: content differs (%d bytes, want %d)", w.name, len(got[i].body), len(w.body))
		}
	}
	if len(out)%512 != 0 || !bytes.Equal(out[len(out)-1024:], make([]byte, 1024)) {
		t.Error("the stream must end with the closed tar footer")
	}
	again, out2, _ := buildRef(fx.listing, fx.objects)
	if again != refExitOK || !bytes.Equal(out, out2) {
		t.Error("the same listing and objects must give byte-identical archives")
	}
}

func TestRefArchiveLongNamesUseTheUSTARPrefixField(t *testing.T) {
	long := "src/T/Tests/" + strings.Repeat("d", 60) + "/" + strings.Repeat("e", 60) + "/f.cs"
	exact := "src/T/Tests/" + strings.Repeat("a", 91) + ".cs" // archive name of exactly 100 bytes
	for _, p := range []string{long, exact} {
		fx := newRefFixture(map[string][]byte{p: []byte("x"), "src/T/Parser/p.cs": []byte("y"), "src/T/Qualities/q.cs": []byte("z")})
		status, out, _ := buildRef(fx.listing, fx.objects)
		if status != refExitOK {
			t.Fatalf("%d-byte name: status %d", len(archiveName(p)), status)
		}
		found := false
		for _, m := range readArchive(t, out) {
			found = found || m.hdr.Name == archiveName(p)
		}
		if !found {
			t.Errorf("the %d-byte name did not survive the USTAR round trip", len(archiveName(p)))
		}
	}
}

func TestRefArchiveBoundaryControlsAreAdmitted(t *testing.T) {
	two := bytes.Repeat([]byte("x"), 2<<20)
	nested := "src/T/Tests/" + strings.Repeat("d/", 31) + "a.cs" // 32 components
	cases := map[string]map[string][]byte{
		"nesting 32": {nested: []byte("n"), "src/T/Parser/p.cs": []byte("p"), "src/T/Qualities/q.cs": []byte("q")},
		"a side of exactly 16 MiB": {
			"src/T/Tests/a.cs": two, "src/T/Tests/b.cs": two, "src/T/Tests/c.cs": two,
			"src/T/Parser/a.cs": two, "src/T/Parser/b.cs": two, "src/T/Parser/c.cs": two,
			"src/T/Qualities/a.cs": two, "src/T/Qualities/b.cs": two,
		},
		"executable mode": nil,
	}
	files256, dirs256 := map[string][]byte{"src/T/Parser/p.cs": nil, "src/T/Qualities/q.cs": nil}, map[string][]byte{"src/T/Parser/p.cs": nil, "src/T/Qualities/q.cs": nil}
	for i := range 256 {
		files256[fmt.Sprintf("src/T/Tests/f%03d.cs", i)] = []byte("f")
		dirs256[fmt.Sprintf("src/T/Tests/d%03d/a.cs", i)] = []byte("d")
	}
	cases["256 files in one area"], cases["256 directories in one area"] = files256, dirs256
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if files == nil {
				files = map[string][]byte{"src/T/Tests/a.cs": []byte("a"), "src/T/Parser/p.cs": []byte("p"), "src/T/Qualities/q.cs": []byte("q")}
			}
			fx := newRefFixture(files)
			if name == "executable mode" {
				fx.listing = bytes.ReplaceAll(fx.listing, []byte("100644"), []byte("100755"))
			}
			status, out, _ := buildRef(fx.listing, fx.objects)
			if status != refExitOK {
				t.Fatalf("status %d, want an admitted listing", status)
			}
			if members := readArchive(t, out); len(members) < len(files) {
				t.Errorf("%d members for %d files", len(members), len(files))
			}
		})
	}
}

func TestRefArchiveSourceRuleBreaksExitThreeWithoutOutputOrObjectRead(t *testing.T) {
	base := newRefFixture(map[string][]byte{"src/T/Tests/a.cs": []byte("x"), "src/T/Parser/p.cs": []byte("y"), "src/T/Qualities/q.cs": []byte("z")}).listing
	entry := func(mode, typ, size, path string) string {
		return fmt.Sprintf("%s %s %040x %7s\t%s\x00", mode, typ, 9, size, path)
	}
	valid := func(path string) string { return entry("100644", "blob", "1", path) }
	two := fmt.Sprint(2 << 20)
	withSizes := func(n int, size string) string {
		var sb strings.Builder
		for i := range n {
			area := refTestAreas[i%3].upstream
			sb.WriteString(entry("100644", "blob", size, fmt.Sprintf("%s/big%d.cs", area, i)))
		}
		return sb.String()
	}
	manyFiles, manyDirs := new(strings.Builder), new(strings.Builder)
	for i := range 257 {
		manyFiles.WriteString(valid(fmt.Sprintf("src/T/Tests/f%03d.cs", i)))
		manyDirs.WriteString(valid(fmt.Sprintf("src/T/Tests/d%03d/a.cs", i)))
	}
	onlyTwoAreas := newRefFixture(map[string][]byte{"src/T/Tests/a.cs": []byte("x"), "src/T/Parser/p.cs": []byte("y")}).listing
	extras := map[string]string{
		"symlink blob":               entry("120000", "blob", "1", "src/T/Tests/link.cs"),
		"gitlink":                    entry("160000", "commit", "-", "src/T/Parser/mod"),
		"tree entry":                 entry("040000", "tree", "-", "src/T/Parser/dir"),
		"outside every directory":    valid("src/T/Other/x.cs"),
		"empty component":            valid("src/T/Tests/a//b.cs"),
		"dot component":              valid("src/T/Tests/./b.cs"),
		"dotdot component":           valid("src/T/Tests/../b.cs"),
		"non-ASCII name":             valid("src/T/Tests/\u00e9.cs"),
		"control byte in name":       valid("src/T/Tests/a\nb.cs"),
		"component over 100 bytes":   valid("src/T/Tests/" + strings.Repeat("a", 101) + ".cs"),
		"not a .cs file":             valid("src/T/Tests/a.txt"),
		"bare .cs name":              valid("src/T/Tests/.cs"),
		"nesting 33":                 valid("src/T/Tests/" + strings.Repeat("d/", 32) + "a.cs"),
		"size 2 MiB + 1":             entry("100644", "blob", fmt.Sprint(2<<20+1), "src/T/Tests/big.cs"),
		"unlisted size":              entry("100644", "blob", "-", "src/T/Tests/a2.cs"),
		"non-numeric size":           entry("100644", "blob", "1x", "src/T/Tests/a2.cs"),
		"malformed object id":        "100644 blob ZZZ       1\tsrc/T/Tests/a2.cs\x00",
		"duplicate entry":            valid("src/T/Tests/a.cs"),
		"entry without a tab":        fmt.Sprintf("100644 blob %040x       1 src/T/Tests/a2.cs\x00", 9),
		"unterminated entry":         strings.TrimSuffix(valid("src/T/Tests/a2.cs"), "\x00"),
		"listing over 1 MiB":         strings.Repeat("x", 1<<20),
		"missing field":              "100644 blob\tsrc/T/Tests/a2.cs\x00",
		"mode with extra executable": entry("100775", "blob", "1", "src/T/Tests/a2.cs"),
	}
	rows := map[string][]byte{
		"257 files in one area":       append(append([]byte{}, base...), manyFiles.String()...),
		"257 directories in one area": append(append([]byte{}, base...), manyDirs.String()...),
		"an area without files":       onlyTwoAreas,
		"empty listing":               nil,
		"a side over 16 MiB":          []byte(withSizes(9, two)),
		"a side of 16 MiB + 1 byte":   []byte(withSizes(8, two) + entry("100644", "blob", "1", "src/T/Tests/one.cs")),
	}
	for name, extra := range extras {
		rows[name] = append(append([]byte{}, base...), extra...)
	}
	for name, listing := range rows {
		t.Run(name, func(t *testing.T) {
			status, out, spy := buildRef(listing, []byte("must stay unread"))
			if status != refExitRule {
				t.Errorf("status %d, want %d", status, refExitRule)
			}
			if len(out) != 0 {
				t.Errorf("%d output bytes, want none", len(out))
			}
			if spy.called {
				t.Error("the object stream must stay unread when the listing breaks a source rule")
			}
		})
	}
}

func TestRefArchiveObjectStreamMismatchesFailWithoutTheRuleStatus(t *testing.T) {
	fx := newRefFixture(map[string][]byte{"src/T/Tests/a.cs": []byte("alpha"), "src/T/Parser/p.cs": []byte("pp"), "src/T/Qualities/q.cs": []byte("qqq")})
	id := func(n int) string { return fmt.Sprintf("%040x", n) }
	// Archive order is parser, qualities, tests: ids 1, 2 and 3.
	first := fmt.Sprintf("%s blob 2\npp\n", id(1))
	if !bytes.HasPrefix(fx.objects, []byte(first)) {
		t.Fatalf("fixture order changed: %q", fx.objects[:60])
	}
	mutations := map[string][]byte{
		"another id":          bytes.Replace(fx.objects, []byte(id(1)), []byte(id(7)), 1),
		"another type":        bytes.Replace(fx.objects, []byte(id(1)+" blob"), []byte(id(1)+" tree"), 1),
		"another size":        bytes.Replace(fx.objects, []byte(id(1)+" blob 2"), []byte(id(1)+" blob 3"), 1),
		"missing object":      bytes.Replace(fx.objects, []byte(first), []byte(id(1)+" missing\n"), 1),
		"short blob":          fx.objects[:len(first)-3],
		"short last blob":     fx.objects[:len(fx.objects)-3],
		"no blob terminator":  bytes.Replace(fx.objects, []byte("pp\n"), []byte("ppX"), 1),
		"extra header field":  bytes.Replace(fx.objects, []byte(id(1)+" blob 2"), []byte(id(1)+" blob 2 x"), 1),
		"empty object stream": nil,
	}
	for name, objects := range mutations {
		t.Run(name, func(t *testing.T) {
			status, _, spy := buildRef(fx.listing, objects)
			if status == refExitOK || status == refExitRule || !spy.called {
				t.Errorf("status %d (objects requested %v), want a failure other than %d", status, spy.called, refExitRule)
			}
		})
	}
	// The control stream is accepted.
	if status, _, _ := buildRef(fx.listing, fx.objects); status != refExitOK {
		t.Errorf("control status %d", status)
	}
}

// failAfter fails the write that crosses its byte budget.
type failAfter struct{ budget, written int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.written+len(p) > f.budget {
		n := f.budget - f.written
		f.written = f.budget
		return n, errors.New("device full")
	}
	f.written += len(p)
	return len(p), nil
}

func TestRefArchiveWriterFailuresReturnNonzero(t *testing.T) {
	fx := newRefFixture(map[string][]byte{"src/T/Tests/a.cs": []byte("alpha"), "src/T/Parser/p.cs": []byte("pp"), "src/T/Qualities/q.cs": []byte("qqq")})
	status, full, _ := buildRef(fx.listing, fx.objects)
	if status != refExitOK {
		t.Fatalf("control status %d", status)
	}
	// Archive order is parser/, parser/p.cs, ...: the first 512-byte block is a header, the third the first payload.
	for name, budget := range map[string]int{"first header": 100, "second header": 512 + 100, "first payload": 1024 + 1, "final close": len(full) - 1} {
		t.Run(name, func(t *testing.T) {
			spy := &spyObjects{stream: fx.objects}
			if got := refArchive(&failAfter{budget: budget}, fx.listing, refTestAreas, spy.open); got != refExitFail {
				t.Errorf("status %d, want %d", got, refExitFail)
			}
		})
	}
	spy := &spyObjects{stream: fx.objects}
	if got := refArchive(&failAfter{budget: len(full)}, fx.listing, refTestAreas, spy.open); got != refExitOK {
		t.Errorf("a writer with exactly enough room must succeed, got %d", got)
	}
}

func TestRefArchiveRejectsMalformedArgumentsWithZeroOutput(t *testing.T) {
	const pin = "57816c149d02f95543973fefb4780c4495f8121c"
	good := []string{"/srv/repo", pin, "tests:src/T/Tests", "parser:src/T/Parser", "qualities:src/T/Qualities"}
	if _, _, areas, ok := parseRefArchiveArgs(good); !ok || len(areas) != 3 {
		t.Fatalf("the well-formed control was rejected: %v", ok)
	}
	with := func(i int, v string) []string {
		a := append([]string{}, good...)
		a[i] = v
		return a
	}
	cases := map[string][]string{
		"relative root":          with(0, "repo"),
		"empty root":             with(0, ""),
		"control byte in root":   with(0, "/srv/re\npo"),
		"short pin":              with(1, pin[:39]),
		"long pin":               with(1, pin+"0"),
		"upper-case pin":         with(1, strings.ToUpper(pin)),
		"non-hex pin":            with(1, strings.Repeat("g", 40)),
		"two areas":              good[:4],
		"four areas":             append(append([]string{}, good...), "tests:src/T/More"),
		"no areas":               good[:2],
		"duplicate area":         with(3, "tests:src/T/Parser"),
		"unknown area":           with(3, "titles:src/T/Parser"),
		"area without separator": with(3, "parser"),
		"empty upstream":         with(3, "parser:"),
		"dotdot upstream":        with(3, "parser:src/../Parser"),
		"dot upstream":           with(3, "parser:src/./Parser"),
		"absolute upstream":      with(3, "parser:/src/T/Parser"),
		"trailing slash":         with(3, "parser:src/T/Parser/"),
		"doubled slash":          with(3, "parser:src//Parser"),
		"space in upstream":      with(3, "parser:src/T P/Parser"),
		"duplicate upstream":     with(3, "parser:src/T/Tests"),
		"nested upstream":        with(3, "parser:src/T/Tests/Sub"),
		"containing upstream":    with(3, "parser:src/T"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if status := runRefArchive(args, &out); status != refExitUsage || out.Len() != 0 {
				t.Errorf("status %d with %d output bytes, want usage status %d and none", status, out.Len(), refExitUsage)
			}
		})
	}
}
