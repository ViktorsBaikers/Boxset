// Command redact is the host-side output redactor and native lifecycle
// controller of scripts/dev-run.sh. It is stdlib-only, static and
// runner-private: the redaction modes read raw bytes on standard input, write
// redacted bytes on standard output and take their secret values from the
// environment they inherit, so owner values never enter Docker.
//
// Usage:
//
//	redact -check
//	redact -cap N [-raw S]
//	redact -tail N
//	redact -clock
//	redact -run REPO RUNID TMPD TIMEOUT WRITE CAPTURE -- COMMAND [ARG...]
//
// -clock (the contract driver's private reader) prints one floored
// CLOCK_MONOTONIC reading and nothing else: no stdin, no environment, no file.
// -run is the native lifecycle controller the bootstrap of scripts/dev-run.sh
// execs (controller.go); it reuses the redaction engine below in process, with
// one immutable secret selection for every stream.
//
// Selection: every non-empty value of an environment variable whose
// ASCII-uppercased name contains SECRET, TOKEN, PASSWORD, PASSWD, CREDENTIAL,
// API_KEY, APIKEY or PRIVATE_KEY, as complete bytes (newlines included). More
// than 64 values, a value above 65,536 bytes or more than 1 MiB in total exits
// 2 before any input is read.
//
// Every raw byte inside any occurrence of any value, across read boundaries
// and overlapping values, is covered. Independently of the environment every
// <scheme>:// token on the original bytes has its userinfo and every query or
// fragment parameter value covered; an authority with no delimiter after
// 1,024 bytes is covered whole. Both decisions are made on the original bytes
// and united before any replacement, so a selected value inside a URL never
// hides it; each maximal covered run emits [REDACTED] once and the output is
// never rescanned (single forward cursor, holdback of at most the longest
// value minus one byte plus the authority). NUL and non-UTF-8 bytes are
// counted and passed unless covered.
//
// Exit status: 0 success; 2 usage or unusable selection; 3 raw byte N+1 or
// emitted byte N+1 under -cap (nothing beyond the cap is emitted); 4 write
// error; 5 EOF after a raw byte count other than S under -raw; 1 read error
// (bytes already decided safe are still written, the held-back ones never);
// 6 an unusable clock reading under -clock.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	marker = "[REDACTED]"

	maxValues     = 64
	maxValueBytes = 65536
	maxTotalBytes = 1 << 20
	maxAuthority  = 1024

	exitOK       = 0
	exitReadFail = 1
	exitUsage    = 2
	exitCap      = 3
	exitWrite    = 4
	exitRawCount = 5
	exitClock    = 6
)

var markerNames = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY"}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-run" {
		os.Exit(runController(os.Args, os.Environ()))
	}
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout))
}

// selectValues returns the distinct secret values of environ, or ok=false when
// the selection exceeds a limit.
func selectValues(environ []string) (values [][]byte, ok bool) {
	count, total := 0, 0
	seen := map[string]bool{}
	for _, kv := range environ {
		name, value, found := strings.Cut(kv, "=")
		if !found || value == "" || !sensitive(name) {
			continue
		}
		count++
		total += len(value)
		if count > maxValues || len(value) > maxValueBytes || total > maxTotalBytes {
			return nil, false
		}
		if !seen[value] {
			seen[value] = true
			values = append(values, []byte(value))
		}
	}
	return values, true
}

func sensitive(name string) bool {
	up := make([]byte, len(name))
	for i := range len(name) {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up[i] = c
	}
	for _, m := range markerNames {
		if strings.Contains(string(up), m) {
			return true
		}
	}
	return false
}

func run(args []string, environ []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "validate the secret selection and exit")
	clock := fs.Bool("clock", false, "print one floored CLOCK_MONOTONIC reading")
	capN := fs.Int64("cap", -1, "raw and emitted byte cap")
	raw := fs.Int64("raw", -1, "required raw byte count (with -cap)")
	tail := fs.Int64("tail", -1, "emit only the last N redacted bytes")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	clockSet := false
	fs.Visit(func(f *flag.Flag) { clockSet = clockSet || f.Name == "clock" })
	if clockSet {
		if len(args) != 1 || args[0] != "-clock" || !*clock {
			return exitUsage
		}
		return clockMode(stdout, clockGettime)
	}
	modes := 0
	if *check {
		modes++
	}
	if *capN >= 0 {
		modes++
	}
	if *tail >= 0 {
		modes++
	}
	if modes != 1 || (*raw >= 0 && *capN < 0) {
		return exitUsage
	}
	values, ok := selectValues(environ)
	if !ok {
		return exitUsage
	}
	if *check {
		return exitOK
	}
	return redactStream(newAutomaton(values), streamOpts{capN: *capN, raw: *raw, tail: *tail}, stdin, stdout)
}

// formatClock renders one reading as its floored centisecond text, with the
// line feed; a reading outside the 9-digit second range or with a nanosecond
// field outside [0, 10^9) is refused.
func formatClock(sec, nsec int64) (string, bool) {
	if sec < 0 || sec > 999999999 || nsec < 0 || nsec >= 1000000000 {
		return "", false
	}
	return fmt.Sprintf("%d.%02d\n", sec, nsec/10000000), true
}

// clockMode is the private -clock reader: one reading, exactly its text. A
// failed or out-of-range reading exits 6 with no byte written, a failed write 4;
// there is no retry and no fallback.
func clockMode(stdout io.Writer, read func() (int64, int64, error)) int {
	sec, nsec, err := read()
	if err != nil {
		return exitClock
	}
	text, ok := formatClock(sec, nsec)
	if !ok {
		return exitClock
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		return exitWrite
	}
	return exitOK
}

// streamOpts selects one redaction mode; negative fields are unset.
type streamOpts struct{ capN, raw, tail int64 }

// redactStream is the redaction engine: stdin to stdout under one mode and one
// immutable automaton (nil: no selected values), returning the exit status 0-5.
func redactStream(ac *automaton, o streamOpts, stdin io.Reader, stdout io.Writer) int {
	capN, raw, tail := &o.capN, &o.raw, &o.tail
	s := &sink{w: bufio.NewWriterSize(stdout, 32<<10), cap: -1}
	if *capN >= 0 {
		s.cap = *capN
	} else {
		s.tail = newRing(int(*tail))
	}
	f := &filter{out: s, ac: ac}
	f.url.out = s
	f.url.resetScheme()

	buf := make([]byte, 32<<10)
	var rawCount int64
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if *capN >= 0 && rawCount+int64(n) > *capN {
				chunk = chunk[:*capN-rawCount]
				f.feed(chunk)
				s.flush()
				if s.err != nil {
					return exitWrite
				}
				return exitCap
			}
			rawCount += int64(n)
			f.feed(chunk)
			if s.capHit {
				s.flush()
				if s.err != nil {
					return exitWrite
				}
				return exitCap
			}
			if s.err != nil {
				return exitWrite
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// Only bytes already decided and redacted leave: the holdback and an
			// open URL authority stay unwritten, and the tail ring is emitted as is.
			if s.tail != nil {
				s.emitTail()
			}
			s.flush()
			return exitReadFail
		}
	}
	f.finish()
	if s.capHit {
		s.flush()
		if s.err != nil {
			return exitWrite
		}
		return exitCap
	}
	if s.tail != nil {
		s.emitTail()
	}
	s.flush()
	if s.err != nil {
		return exitWrite
	}
	if *raw >= 0 && rawCount != *raw {
		return exitRawCount
	}
	return exitOK
}

// sink counts and writes emitted bytes. Under a cap the byte after the cap is
// refused (capHit) and never written; in tail mode bytes go to a ring. emit
// turns each maximal run of covered bytes into one marker.
type sink struct {
	w       *bufio.Writer
	cap     int64
	emitted int64
	capHit  bool
	err     error
	tail    *ring
	covered bool // a covered run is open
}

// emit passes an uncovered byte or extends the open covered run.
func (s *sink) emit(b byte, cov bool) {
	if cov {
		s.covered = true
		return
	}
	s.endRun()
	s.put(b)
}

// endRun writes the marker of the open covered run, if any.
func (s *sink) endRun() {
	if s.covered {
		s.covered = false
		s.putString(marker)
	}
}

func (s *sink) put(b byte) {
	if s.capHit || s.err != nil {
		return
	}
	s.emitted++
	if s.tail != nil {
		s.tail.push(b)
		return
	}
	if s.cap >= 0 && s.emitted > s.cap {
		s.emitted--
		s.capHit = true
		return
	}
	if err := s.w.WriteByte(b); err != nil {
		s.err = err
	}
}

func (s *sink) putString(v string) {
	for i := range len(v) {
		s.put(v[i])
	}
}

func (s *sink) flush() {
	if s.err == nil {
		s.err = s.w.Flush()
	}
}

func (s *sink) emitTail() {
	omitted := s.emitted - int64(s.tail.len())
	if omitted > 0 {
		_, err := fmt.Fprintf(s.w, "[dev-run: %d earlier bytes omitted]\n", omitted)
		if err != nil {
			s.err = err
			return
		}
	}
	s.tail.writeTo(s)
}

// ring keeps the last n bytes in constant memory.
type ring struct {
	b    []byte
	next int
	full bool
}

func newRing(n int) *ring { return &ring{b: make([]byte, n)} }

func (r *ring) push(c byte) {
	if len(r.b) == 0 {
		return
	}
	r.b[r.next] = c
	r.next++
	if r.next == len(r.b) {
		r.next = 0
		r.full = true
	}
}

func (r *ring) len() int {
	if r.full {
		return len(r.b)
	}
	return r.next
}

func (r *ring) writeTo(s *sink) {
	var err error
	if r.full {
		_, err = s.w.Write(r.b[r.next:])
	}
	if err == nil {
		_, err = s.w.Write(r.b[:r.next])
	}
	if err != nil {
		s.err = err
	}
}

// automaton is an Aho-Corasick matcher over the secret values. Children are
// linked lists so memory stays proportional to the total value bytes.
type automaton struct {
	nodes  []acNode
	maxLen int
}

type acNode struct {
	first, sibling, fail uint32
	b                    byte
	out                  uint32 // longest value ending here, directly or via fail
}

func newAutomaton(values [][]byte) *automaton {
	if len(values) == 0 {
		return nil
	}
	a := &automaton{nodes: make([]acNode, 1, 1024)}
	for _, v := range values {
		cur := uint32(0)
		for _, c := range v {
			next := a.child(cur, c)
			if next == 0 {
				a.nodes = append(a.nodes, acNode{b: c, sibling: a.nodes[cur].first})
				next = uint32(len(a.nodes) - 1)
				a.nodes[cur].first = next
			}
			cur = next
		}
		if uint32(len(v)) > a.nodes[cur].out {
			a.nodes[cur].out = uint32(len(v))
		}
		if len(v) > a.maxLen {
			a.maxLen = len(v)
		}
	}
	queue := make([]uint32, 0, len(a.nodes))
	for c := a.nodes[0].first; c != 0; c = a.nodes[c].sibling {
		queue = append(queue, c)
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for c := a.nodes[u].first; c != 0; c = a.nodes[c].sibling {
			f := a.nodes[u].fail
			for f != 0 && a.child(f, a.nodes[c].b) == 0 {
				f = a.nodes[f].fail
			}
			if g := a.child(f, a.nodes[c].b); g != 0 && g != c {
				a.nodes[c].fail = g
			}
			if o := a.nodes[a.nodes[c].fail].out; o > a.nodes[c].out {
				a.nodes[c].out = o
			}
			queue = append(queue, c)
		}
	}
	return a
}

func (a *automaton) child(u uint32, c byte) uint32 {
	for n := a.nodes[u].first; n != 0; n = a.nodes[n].sibling {
		if a.nodes[n].b == c {
			return n
		}
	}
	return 0
}

func (a *automaton) step(state uint32, c byte) uint32 {
	for {
		if n := a.child(state, c); n != 0 {
			return n
		}
		if state == 0 {
			return 0
		}
		state = a.nodes[state].fail
	}
}

type interval struct{ lo, hi int64 } // absolute byte indexes, inclusive

// filter is the secret layer: it decides every byte as covered or not once all
// occurrences that could cover it are known, and feeds each original byte with
// that decision to the URL layer.
type filter struct {
	out *sink
	ac  *automaton
	url urlFilter

	state uint32
	pos   int64 // absolute index of the next input byte
	base  int64 // absolute index of buf[start]
	buf   []byte
	start int        // first undecided byte of buf
	ivs   []interval // merged, disjoint, sorted by lo
}

func (f *filter) feed(chunk []byte) {
	if f.ac == nil {
		for _, c := range chunk {
			f.url.write(c, false)
			if f.out.capHit || f.out.err != nil {
				return
			}
		}
		return
	}
	for _, c := range chunk {
		f.buf = append(f.buf, c)
		f.state = f.ac.step(f.state, c)
		if l := int64(f.ac.nodes[f.state].out); l > 0 {
			f.cover(f.pos-l+1, f.pos)
		}
		f.pos++
		f.decide(f.pos - int64(f.ac.maxLen) + 1) // bytes below this can no longer be covered by a later match
		if f.out.capHit || f.out.err != nil {
			return
		}
	}
}

// cover merges [lo, hi]; hi is never below an earlier interval's hi.
func (f *filter) cover(lo, hi int64) {
	for n := len(f.ivs); n > 0 && f.ivs[n-1].hi >= lo-1; n = len(f.ivs) {
		if f.ivs[n-1].lo < lo {
			lo = f.ivs[n-1].lo
		}
		f.ivs = f.ivs[:n-1]
	}
	f.ivs = append(f.ivs, interval{lo, hi})
}

// decide settles every buffered byte below limit and hands it, with its
// coverage, to the URL layer.
func (f *filter) decide(limit int64) {
	for f.start < len(f.buf) && f.base < limit {
		for len(f.ivs) > 0 && f.ivs[0].hi < f.base {
			f.ivs = f.ivs[1:]
		}
		f.url.write(f.buf[f.start], len(f.ivs) > 0 && f.ivs[0].lo <= f.base)
		f.start++
		f.base++
		if f.out.capHit || f.out.err != nil {
			return
		}
	}
	if f.start > 4096 && f.start*2 > len(f.buf) {
		f.buf = append(f.buf[:0], f.buf[f.start:]...)
		f.start = 0
	}
}

func (f *filter) finish() {
	if f.ac != nil {
		f.decide(f.pos + 1)
	}
	f.url.finish()
}

// urlFilter is the environment-independent URL layer. It sees every original
// byte once, together with the secret layer's coverage decision for it, and
// forwards the byte with the union of both decisions: URL recognition never
// depends on a replacement, so a selected value inside a URL cannot hide it.
type urlFilter struct {
	out *sink

	mode      int
	run       int  // consecutive scheme characters in normal mode
	sinceAlph int  // scheme characters after the last letter of the run; -1 none
	colon     int  // 0 none, 1 after ':', 2 after ":/"
	valid     bool // a valid scheme precedes the pending "://"
	auth      []cell
}

// cell is one original byte with the coverage decided so far.
type cell struct {
	c   byte
	cov bool
}

const (
	uNormal = iota
	uAuth
	uAuthMask
	uPath
	uKey
	uVal
)

func schemeChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-'
}

func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func tokenEnd(c byte) bool {
	return c <= 0x20 || c == 0x7f || c == '"' || c == '\'' || c == '<' || c == '>' || c == '`'
}

func (u *urlFilter) resetScheme() {
	u.run, u.sinceAlph, u.colon, u.valid = 0, -1, 0, false
}

// write consumes one original byte; cov is its secret-layer coverage.
func (u *urlFilter) write(c byte, cov bool) {
	switch u.mode {
	case uNormal:
		u.normal(c, cov)
	case uAuth:
		switch {
		case tokenEnd(c):
			u.flushAuth()
			u.endToken(c, cov)
		case c == '/':
			u.flushAuth()
			u.out.emit(c, cov)
			u.mode = uPath
		case c == '?' || c == '#':
			u.flushAuth()
			u.out.emit(c, cov)
			u.mode = uKey
		default:
			u.auth = append(u.auth, cell{c, cov})
			if len(u.auth) > maxAuthority {
				for _, a := range u.auth {
					u.out.emit(a.c, true)
				}
				u.auth = u.auth[:0]
				u.mode = uAuthMask
			}
		}
	case uAuthMask:
		switch {
		case tokenEnd(c):
			u.endToken(c, cov)
		case c == '/':
			u.out.emit(c, cov)
			u.mode = uPath
		case c == '?' || c == '#':
			u.out.emit(c, cov)
			u.mode = uKey
		default:
			u.out.emit(c, true)
		}
	case uPath:
		switch {
		case tokenEnd(c):
			u.endToken(c, cov)
		case c == '?' || c == '#':
			u.out.emit(c, cov)
			u.mode = uKey
		default:
			u.out.emit(c, cov)
		}
	case uKey:
		switch {
		case tokenEnd(c):
			u.endToken(c, cov)
		case c == '=':
			u.out.emit(c, cov)
			u.mode = uVal
		default:
			u.out.emit(c, cov)
		}
	case uVal:
		switch {
		case tokenEnd(c):
			u.endToken(c, cov)
		case c == '&' || c == '#':
			u.out.emit(c, cov)
			u.mode = uKey
		default:
			u.out.emit(c, true)
		}
	}
}

func (u *urlFilter) normal(c byte, cov bool) {
	u.out.emit(c, cov)
	switch {
	case u.colon == 2 && c == '/':
		// "://" completed: a token starts when a valid scheme precedes it.
		if u.valid {
			u.mode = uAuth
			u.auth = u.auth[:0]
		}
		u.resetScheme()
	case u.colon == 1 && c == '/':
		u.colon = 2
	case c == ':' && u.run > 0 && u.sinceAlph >= 0 && u.sinceAlph <= 31:
		u.colon, u.valid = 1, true
	case schemeChar(c):
		u.colon = 0
		u.run++
		switch {
		case letter(c):
			u.sinceAlph = 0
		case u.sinceAlph >= 0:
			u.sinceAlph++
		}
	default:
		u.resetScheme()
	}
}

// flushAuth settles the buffered authority: everything before its last '@' is
// userinfo and is covered.
func (u *urlFilter) flushAuth() {
	at := -1
	for i, a := range u.auth {
		if a.c == '@' {
			at = i
		}
	}
	for i, a := range u.auth {
		u.out.emit(a.c, a.cov || i < at)
	}
	u.auth = u.auth[:0]
}

// endToken leaves URL mode on a token-ending byte, which is passed through.
func (u *urlFilter) endToken(c byte, cov bool) {
	u.mode = uNormal
	u.resetScheme()
	u.out.emit(c, cov)
}

func (u *urlFilter) finish() {
	if u.mode == uAuth {
		u.flushAuth()
	}
	u.out.endRun()
}
