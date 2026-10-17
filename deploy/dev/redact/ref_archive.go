// The private -ref-archive mode of the host redactor: the runner's only reader
// of the reference repositories. It lists the three listed upstream directories
// at one pinned commit with `git ls-tree`, validates the whole listing, and only
// then reads exactly the listed blobs with one `git cat-file --batch`, writing
// them as a deterministic USTAR stream on standard output. Reference text is
// only ever Git objects at the pin: no checkout, index or working tree is read,
// nothing is spooled to the host and nothing is printed except the archive.
//
//	redact -ref-archive ROOT PIN AREA:UPSTREAM AREA:UPSTREAM AREA:UPSTREAM
//
// ROOT is an absolute repository path, PIN 40 lowercase hex digits and the three
// areas tests, parser and qualities each map one upstream directory. Exit 0 only
// after the complete, closed archive; 3 with no output byte when the listing
// breaks a source rule; 2 for malformed arguments; 1 for every other failure.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	refExitOK    = 0
	refExitFail  = 1
	refExitUsage = 2
	refExitRule  = 3

	refMaxListing = 1 << 20  // bytes of `git ls-tree` output
	refMaxBlob    = 2 << 20  // bytes of one source file
	refMaxSide    = 16 << 20 // listed bytes of one side
	refMaxFiles   = 256      // files per area
	refMaxDirs    = 256      // nested directories per area
	refMaxNesting = 32       // path components below an area
	refMaxStream  = 18 << 20 // archive bytes: the side plus its tar framing
	refMaxHeader  = 4096     // bytes of one `git cat-file --batch` header line
)

// refArea maps one neutral container area to one upstream directory.
type refArea struct {
	name     string // tests, parser or qualities
	upstream string // slash-separated, no leading or trailing slash
}

var errRefStreamCap = errors.New("ref archive: stream cap")

// parseRefArchiveArgs validates the arguments after -ref-archive: an absolute
// root, a 40-digit lowercase hex pin and exactly the three areas, each once,
// each mapped to a safe upstream directory that neither repeats nor contains
// another one.
func parseRefArchiveArgs(args []string) (root, pin string, areas []refArea, ok bool) {
	if len(args) != 5 {
		return "", "", nil, false
	}
	root, pin = args[0], args[1]
	if !strings.HasPrefix(root, "/") || strings.IndexFunc(root, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 || !validObjectID(pin) {
		return "", "", nil, false
	}
	seen := map[string]bool{}
	for _, a := range args[2:] {
		name, upstream, found := strings.Cut(a, ":")
		if !found || (name != "tests" && name != "parser" && name != "qualities") || seen[name] || !safeUpstream(upstream) {
			return "", "", nil, false
		}
		seen[name] = true
		areas = append(areas, refArea{name, upstream})
	}
	for i, a := range areas {
		for _, b := range areas[i+1:] {
			if a.upstream == b.upstream || strings.HasPrefix(a.upstream, b.upstream+"/") || strings.HasPrefix(b.upstream, a.upstream+"/") {
				return "", "", nil, false
			}
		}
	}
	return root, pin, areas, true
}

// safeUpstream accepts non-empty path components of letters, digits, dot,
// underscore and hyphen, none of them . or ...
func safeUpstream(p string) bool {
	if p == "" || len(p) > 200 {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
		for i := range len(c) {
			b := c[i]
			if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '.' && b != '_' && b != '-' {
				return false
			}
		}
	}
	return true
}

func validObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		if b := s[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// printableASCII is the name alphabet: no control byte, no DEL, no byte above 0x7e.
func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ustarName reports whether name fits the USTAR name and prefix fields; a
// directory name carries its trailing slash.
func ustarName(name string) bool {
	if len(name) <= 100 {
		return true
	}
	for i := 1; i < len(name)-1; i++ {
		if name[i] == '/' && i <= 155 && len(name)-i-1 <= 100 {
			return true
		}
	}
	return false
}

// refEntry is one archive member: a file named <area>/<relative path> or a
// directory with a trailing slash.
type refEntry struct {
	name string
	dir  bool
	id   string
	size int64
}

// planRefArchive validates a whole `git ls-tree -r -l -z --full-tree` listing
// against the source rules and returns the sorted archive members. It reads
// nothing but the listing; any rule break (ok=false) happens before any object
// is read or any byte is written.
func planRefArchive(listing []byte, areas []refArea) (entries []refEntry, ok bool) {
	if len(listing) > refMaxListing {
		return nil, false
	}
	files := map[string]bool{}
	dirs := map[string]bool{}
	fileCount := map[string]int{}
	dirCount := map[string]int{}
	var total int64
	for rest := listing; len(rest) > 0; {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, false
		}
		meta, p, found := strings.Cut(string(rest[:end]), "\t")
		rest = rest[end+1:]
		f := strings.Fields(meta)
		if !found || len(f) != 4 || (f[0] != "100644" && f[0] != "100755") || f[1] != "blob" || !validObjectID(f[2]) || !isDigits(f[3]) {
			return nil, false
		}
		size, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil || size > refMaxBlob {
			return nil, false
		}
		var area refArea
		var rel string
		matches := 0
		for _, a := range areas {
			if r, under := strings.CutPrefix(p, a.upstream+"/"); under {
				area, rel = a, r
				matches++
			}
		}
		if matches != 1 {
			return nil, false
		}
		comps := strings.Split(rel, "/")
		if len(comps) > refMaxNesting {
			return nil, false
		}
		for _, c := range comps {
			if c == "" || c == "." || c == ".." || !printableASCII(c) {
				return nil, false
			}
		}
		if last := comps[len(comps)-1]; len(last) <= 3 || !strings.HasSuffix(last, ".cs") {
			return nil, false
		}
		name := area.name + "/" + rel
		if files[name] || !ustarName(name) {
			return nil, false
		}
		files[name] = true
		entries = append(entries, refEntry{name: name, id: f[2], size: size})
		if fileCount[area.name]++; fileCount[area.name] > refMaxFiles {
			return nil, false
		}
		if total += size; total > refMaxSide {
			return nil, false
		}
		for k := 1; k < len(comps); k++ {
			d := area.name + "/" + strings.Join(comps[:k], "/") + "/"
			if dirs[d] {
				continue
			}
			if !ustarName(d) {
				return nil, false
			}
			dirs[d] = true
			entries = append(entries, refEntry{name: d, dir: true})
			if dirCount[area.name]++; dirCount[area.name] > refMaxDirs {
				return nil, false
			}
		}
	}
	for _, a := range areas {
		if fileCount[a.name] == 0 {
			return nil, false
		}
		entries = append(entries, refEntry{name: a.name + "/", dir: true})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, true
}

// capWriter fails rather than pass more than max bytes.
type capWriter struct {
	w      io.Writer
	n, max int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.max {
		return 0, errRefStreamCap
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeRefArchive writes entries as a USTAR stream: numeric owner and group 0
// with empty names, modification time 0, files 0444 and directories 0555. For
// every file the `git cat-file --batch` header read from objects must name the
// listed id, type blob and listed size before exactly that many bytes are
// copied, unconverted. Any mismatch, missing object, short read or write error
// returns refExitFail; success only after the closed archive.
func writeRefArchive(out io.Writer, entries []refEntry, objects io.Reader) int {
	tw := tar.NewWriter(&capWriter{w: out, max: refMaxStream})
	br := bufio.NewReaderSize(objects, 64<<10)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
		if e.dir {
			h.Typeflag, h.Mode = tar.TypeDir, 0o555
		} else {
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o444, e.size
			if !batchHeaderIs(br, e) {
				return refExitFail
			}
		}
		if tw.WriteHeader(h) != nil {
			return refExitFail
		}
		if e.dir {
			continue
		}
		if _, err := io.CopyN(tw, br, e.size); err != nil {
			return refExitFail
		}
		if b, err := br.ReadByte(); err != nil || b != '\n' {
			return refExitFail
		}
	}
	if tw.Close() != nil {
		return refExitFail
	}
	return refExitOK
}

// batchHeaderIs reads one `<id> <type> <size>` header line and reports whether
// it names exactly e.
func batchHeaderIs(br *bufio.Reader, e refEntry) bool {
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > refMaxHeader {
		return false
	}
	f := strings.Fields(string(line[:len(line)-1]))
	return len(f) == 3 && f[0] == e.id && f[1] == "blob" && f[2] == strconv.FormatInt(e.size, 10)
}

// refArchive is the producer core: it validates the listing, then asks objects
// for the batch output of the listed ids (in archive order) and writes the
// archive. objects is not called when the listing breaks a source rule.
func refArchive(out io.Writer, listing []byte, areas []refArea, objects func(ids []string) (io.Reader, error)) int {
	entries, ok := planRefArchive(listing, areas)
	if !ok {
		return refExitRule
	}
	var ids []string
	for _, e := range entries {
		if !e.dir {
			ids = append(ids, e.id)
		}
	}
	r, err := objects(ids)
	if err != nil {
		return refExitFail
	}
	return writeRefArchive(out, entries, r)
}

// gitCommand runs git at root with the minimal explicit environment: discovery
// cannot leave root, no replace refs, no lazy fetch and no system configuration.
// Standard input is empty and diagnostics are discarded.
func gitCommand(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(root),
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	return cmd
}

// readListing runs `git ls-tree` for the listed upstream directories and
// returns at most refMaxListing+1 bytes of its output: more than the cap is
// returned truncated, with its child killed, for planRefArchive to reject.
func readListing(root, pin string, areas []refArea) ([]byte, bool) {
	args := []string{"ls-tree", "-r", "-l", "-z", "--full-tree", pin, "--"}
	for _, a := range areas {
		args = append(args, a.upstream+"/")
	}
	cmd := gitCommand(root, args...)
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return nil, false
	}
	data, readErr := io.ReadAll(io.LimitReader(out, refMaxListing+1))
	if len(data) > refMaxListing {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // killed on purpose: the oversize listing is rejected on its size
		return data, true
	}
	if cmd.Wait() != nil || readErr != nil {
		return nil, false
	}
	return data, true
}

// runRefArchive is the -ref-archive mode: args are those after the flag. The
// archive goes to stdout only after the whole listing validated.
func runRefArchive(args []string, stdout io.Writer) int {
	root, pin, areas, ok := parseRefArchiveArgs(args)
	if !ok {
		return refExitUsage
	}
	listing, ok := readListing(root, pin, areas)
	if !ok {
		return refExitFail
	}
	var batch *exec.Cmd
	objects := func(ids []string) (io.Reader, error) {
		cmd := gitCommand(root, "cat-file", "--batch")
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		batch = cmd
		go func() {
			// A failed feed leaves the batch output short, which the reader rejects.
			w := bufio.NewWriter(in)
			for _, id := range ids {
				_, _ = w.WriteString(id + "\n")
			}
			_ = w.Flush()
			_ = in.Close()
		}()
		return out, nil
	}
	bw := bufio.NewWriterSize(stdout, 64<<10)
	status := refArchive(bw, listing, areas, objects)
	if batch != nil {
		if status != refExitOK {
			_ = batch.Process.Kill()
		}
		if err := batch.Wait(); err != nil && status == refExitOK {
			status = refExitFail
		}
	}
	if status == refExitOK && bw.Flush() != nil {
		status = refExitFail
	}
	return status
}
