package parse

import (
	"cmp"
	"context"
	"errors"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
)

const (
	// matchTimeout is the fixed per-match regexp2 timeout, set before a
	// compiled pattern is shared. regexp2 checks it approximately (its default
	// clock ticks about every 100 ms), so a running match can overshoot; the
	// shared release budget below is checked before and after every match.
	matchTimeout = 50 * time.Millisecond

	// releaseBudget is the elapsed-work budget of one public parse operation,
	// shared by every nested helper of that operation.
	releaseBudget = 250 * time.Millisecond

	// batchDeadline is the decision safety bound of one EvaluateBatch call,
	// shared by every release of that batch. It is checked wherever the
	// release budget is, ahead of it, so an expired batch is terminal rather
	// than a per-release rejection.
	batchDeadline = 30 * time.Second
)

// compiler compiles the fixed attributed pattern tables of a Parser. The
// first failure aborts the table; the error carries only the numeric index of
// the failing pattern, never the pattern text or the dependency error.
type compiler struct {
	n   int
	err error
}

func (c *compiler) re(src string, opts regexp2.RegexOptions) *regexp2.Regexp {
	c.n++
	if c.err != nil {
		return nil
	}
	re, err := regexp2.Compile(src, opts)
	if err != nil {
		c.err = newReasonError(codeRegexCompileFailed, map[string]int64{"pattern_index": int64(c.n - 1)})
		return nil
	}
	re.MatchTimeout = matchTimeout
	if build.guards != nil {
		if g := newGuard(src, opts); g != nil {
			build.guards[re] = g
		}
	}
	return re
}

// budget is the elapsed-work budget and cancellation state of one public
// operation. All regexp2 work goes through it. A zero decision means the
// operation has no decision deadline beyond its context.
type budget struct {
	ctx      context.Context
	now      func() time.Time
	deadline time.Time
	decision time.Time
	guards   guards  // the guards of the patterns this operation may skip
	subject  subject // the last text a guard read
	rest     subject // the text after a match, read by guards
}

func newBudget(ctx context.Context, now func() time.Time) *budget {
	return &budget{ctx: ctx, now: now, deadline: now().Add(releaseBudget)}
}

// ctxReason maps a cancelled or expired context to its fixed reason; nil when
// the context is live.
func ctxReason(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return newReasonError(codeDeadlineExceeded, nil)
		}
		return newReasonError(codeCancelled, nil)
	}
	return nil
}

// check reports a cancelled or expired context, an expired decision deadline
// or an exhausted budget. It reads the clock once.
func (b *budget) check() error {
	if err := ctxReason(b.ctx); err != nil {
		return err
	}
	t := b.now()
	if !b.decision.IsZero() && !t.Before(b.decision) {
		return decisionError()
	}
	if !t.Before(b.deadline) {
		return newReasonError(codeWorkBudgetExhausted, map[string]int64{"limit_ms": releaseBudget.Milliseconds()})
	}
	return nil
}

func decisionError() error {
	return newReasonError(codeDeadlineExceeded, map[string]int64{"limit_ms": batchDeadline.Milliseconds()})
}

// terminal reports a cancelled or expired context or an expired decision
// deadline: the conditions that end a whole operation, not just one release.
func (b *budget) terminal() error {
	if err := ctxReason(b.ctx); err != nil {
		return err
	}
	if !b.decision.IsZero() && !b.now().Before(b.decision) {
		return decisionError()
	}
	return nil
}

// failed maps a regexp2 failure. A terminal condition observed after the
// failure outranks it, so the failure never surfaces as a release-local reason
// of a batch that is already over.
func (b *budget) failed(err error) error {
	if terr := b.terminal(); terr != nil {
		return terr
	}
	return matchError(err)
}

// matchError maps a regexp2 failure to a fixed reason; the dependency error
// text carries the input and is never propagated.
func matchError(err error) error {
	if errors.Is(err, regexp2.ErrBacktrackingStackLimit) {
		return newReasonError(codeRegexStackLimit, nil)
	}
	return newReasonError(codeMatchTimeout, map[string]int64{"limit_ms": matchTimeout.Milliseconds()})
}

// isMatch is Regex.IsMatch. The budget is checked before and after the match,
// so a late success after expiry is an error, not a pass. A pattern whose guard
// rules the text out is not run, and the budget is still checked twice.
func (b *budget) isMatch(re *regexp2.Regexp, s string) (bool, error) {
	if err := b.check(); err != nil {
		return false, err
	}
	if b.ruledOut(re, s) {
		return false, b.check()
	}
	ok, err := re.MatchString(s)
	if err != nil {
		return false, b.failed(err)
	}
	if err := b.check(); err != nil {
		return false, err
	}
	return ok, nil
}

// first is Regex.Match: the first match, or nil.
func (b *budget) first(re *regexp2.Regexp, s string) (*regexp2.Match, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	if b.ruledOut(re, s) {
		return nil, b.check()
	}
	m, err := re.FindStringMatch(s)
	if err != nil {
		return nil, b.failed(err)
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	return m, nil
}

// next advances an enumeration of successive non-overlapping matches.
func (b *budget) next(re *regexp2.Regexp, s string, m *regexp2.Match) (*regexp2.Match, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	if g := b.guards[re]; g != nil && b.exhausted(g, s, m) {
		return nil, b.check()
	}
	n, err := re.FindNextMatch(m)
	if err != nil {
		return nil, b.failed(err)
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	return n, nil
}

// exhausted reports whether g shows that no match follows m in s: a start anchor
// matches only at the start, which the first match has already used, and a
// match after m lies wholly in the text after it.
func (b *budget) exhausted(g *guard, s string, m *regexp2.Match) bool {
	if g.anchored {
		return true
	}
	at, n := m.ByteRange()
	if at < 0 || n < 0 || at+n > len(s) {
		return false
	}
	b.rest.reset(s[at+n:])
	return g.rulesAfter(&b.rest)
}

// last is Matches(s).LastOrDefault(): the final successive match, or nil.
func (b *budget) last(re *regexp2.Regexp, s string) (*regexp2.Match, error) {
	m, err := b.first(re, s)
	var last *regexp2.Match
	for m != nil && err == nil {
		last = m
		m, err = b.next(re, s, m)
	}
	return last, err
}

// count is Matches(s).Count.
func (b *budget) count(re *regexp2.Regexp, s string) (int, error) {
	m, err := b.first(re, s)
	n := 0
	for m != nil && err == nil {
		n++
		m, err = b.next(re, s, m)
	}
	return n, err
}

// groupOK is Match.Groups[name].Success: the named group took part in m.
func groupOK(m *regexp2.Match, name string) bool {
	if m == nil {
		return false
	}
	g := m.GroupByName(name)
	return g != nil && len(g.Captures) > 0
}

// ---- string helpers of the source's extension methods ---------------------

// isBlank is string.IsNullOrWhiteSpace.
func isBlank(s string) bool { return strings.TrimFunc(s, unicode.IsSpace) == "" }

// trimWhite is string.Trim().
func trimWhite(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// replaceUnderscores is name.Replace('_', ' ').
func replaceUnderscores(s string) string { return strings.ReplaceAll(s, "_", " ") }

// containsOrdinal is the ordinal, case-sensitive string.Contains.
func containsOrdinal(s, sub string) bool { return strings.Contains(s, sub) }

// containsFold is ContainsIgnoreCase for a lower-case ASCII needle.
func containsFold(s, lowerSub string) bool {
	n := len(lowerSub)
	for i := 0; i+n <= len(s); i++ {
		j := 0
		for ; j < n; j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != lowerSub[j] {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// containsInvalidPathChar is ContainsInvalidPathChars: on Linux only NUL.
func containsInvalidPathChar(s string) bool { return strings.IndexByte(s, 0) >= 0 }

// pathExtension is Path.GetExtension (Linux): the text from the last '.'
// after the last '/', empty when there is none or the dot ends the name.
func pathExtension(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case '.':
			if i == len(s)-1 {
				return ""
			}
			return s[i:]
		case '/':
			return ""
		}
	}
	return ""
}

// pathExtensionAnywhere is the source's GetPathExtension: from the last '.'
// anywhere in the name, empty when there is none or the dot ends the name.
func pathExtensionAnywhere(s string) string {
	i := strings.LastIndexByte(s, '.')
	if i < 0 || i == len(s)-1 {
		return ""
	}
	return s[i:]
}

// applyRevision is QualityParser.ParseQualityModifiers' revision logic,
// identical in both applications apart from the version pattern: an explicit
// version, then proper and repack/rerip raising it, then the REAL count of the
// un-normalized name.
func applyRevision(b *budget, result *Quality, name, normalized string, versionRe, properRe, repackRe, realRe *regexp2.Regexp) (Quality, error) {
	vm, err := b.first(versionRe, normalized)
	if err != nil {
		return Quality{}, err
	}
	version := 0
	if vm != nil {
		g := vm.GroupByName("version")
		// The source parses the digit with Convert.ToInt32, which throws for the
		// non-ASCII decimal digits that \d admits; such a name has no value.
		if g == nil {
			return Quality{}, newReasonError(codeTitleUnrecognized, nil)
		}
		if version, err = strconv.Atoi(g.String()); err != nil {
			return Quality{}, newReasonError(codeTitleUnrecognized, nil)
		}
		result.Revision.Version = version
		result.RevisionDetection = detectionName
	}

	proper, err := b.isMatch(properRe, normalized)
	if err != nil {
		return Quality{}, err
	}
	if proper {
		result.Revision.Version = 2
		if vm != nil {
			result.Revision.Version = version + 1
		}
		result.RevisionDetection = detectionName
	}

	repack, err := b.isMatch(repackRe, normalized)
	if err != nil {
		return Quality{}, err
	}
	if repack {
		result.Revision.Version = 2
		if vm != nil {
			result.Revision.Version = version + 1
		}
		result.Revision.IsRepack = true
		result.RevisionDetection = detectionName
	}

	// The source counts every REAL token, not only those after the
	// season/episode number.
	realCount, err := b.count(realRe, name)
	if err != nil {
		return Quality{}, err
	}
	if realCount > 0 {
		result.Revision.Real = realCount
		result.RevisionDetection = detectionName
	}
	return *result, nil
}

// ---- guards ---------------------------------------------------------------------

// A guard is a cheap necessary condition of one compiled pattern, derived from
// the pattern's own source when it is compiled: a text that fails it cannot match,
// so the budget skips the match and checks the budget exactly as often as the
// match would have. A guard only ever rules out; any doubt (a construct it does
// not model, case folding beyond ASCII) leaves the match to regexp2.
//
// A guard reads three kinds of fact from the source: the fewest characters a
// match has, the literal texts every match holds (with the word boundaries the
// pattern puts around them), and the pairs of characters that sit side by side
// in every match. What a character class matches is asked of regexp2 itself,
// so a guard never has its own reading of one. Only ASCII text is decided:
// the sets hold ASCII bytes, and any other text goes to regexp2.

// build collects the guards of the patterns one NewParser compiles. Every table
// constructor is reached from NewParser, which holds the lock while it runs, so
// the compiler hands its guards over without any table having to carry them.
var build struct {
	sync.Mutex
	guards guards
	sets   map[setKey]cset
}

// setKey names an atom of a pattern and the options it is read under.
type setKey struct {
	src   string
	ic, x bool
}

// cset is a set of ASCII bytes.
type cset struct{ lo, hi uint64 }

func (s cset) has(c byte) bool {
	if c < 64 {
		return s.lo>>c&1 != 0
	}
	return s.hi>>(c-64)&1 != 0
}

func (s *cset) add(c byte) {
	if c < 64 {
		s.lo |= 1 << c
	} else if c < 128 {
		s.hi |= 1 << (c - 64)
	}
}

func (s cset) or(o cset) cset { return cset{s.lo | o.lo, s.hi | o.hi} }

// every is the set of all ASCII bytes.
var every = cset{^uint64(0), ^uint64(0)}

// weight is how likely a release name is to hold a byte of the set, a rough
// measure that only orders guards by how much they rule out.
func (s cset) weight() float64 {
	w := 0.0
	for c := 0; c < 128; c++ {
		if !s.has(byte(c)) {
			continue
		}
		switch {
		case c == '.':
			w += 0.08
		case c == ' ' || c == '-' || (c >= '0' && c <= '9'):
			w += 0.03
		case c >= 'a' && c <= 'z':
			w += 0.025
		case c >= 'A' && c <= 'Z', c == '_', c == '[', c == ']', c == '(', c == ')':
			w += 0.01
		default:
			w += 0.002
		}
	}
	return w
}

// lit is one thing a pattern needs, either a literal text or a pair of
// adjacent characters. fold: the letters of a literal compare ASCII-folded (the
// pattern ignores case). na: it holds a non-ASCII rune, which ASCII text cannot
// contain. lb, rb: a word boundary precedes / follows the literal. bi: a byte of
// a, then a byte of b, side by side.
type lit struct {
	s        string
	fold, na bool
	lb, rb   bool
	bi       bool
	behind   bool // it comes from a lookbehind: the text it names may lie before the match
	a, b     cset
}

// p estimates how likely a release name is to hold l.
func (l *lit) p() float64 {
	switch {
	case l.bi:
		return min(1, 30*l.a.weight()*l.b.weight())
	case l.na:
		return 0
	}
	p := 0.5
	for i := utf8.RuneCountInString(l.s); i > 1; i-- {
		p /= 24
	}
	if l.lb {
		p *= 0.3
	}
	if l.rb {
		p *= 0.3
	}
	return p
}

// clauseP estimates how likely a release name is to hold any literal of a clause.
func clauseP(clause []lit) float64 {
	p := 0.0
	for i := range clause {
		p += clause[i].p()
	}
	return min(p, 1)
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// subject is a text with the facts guards read. For ASCII text: lower is its
// lower case, pres the bytes it holds and next[c] the bytes that directly follow
// a c.
type subject struct {
	s, lower string
	lowered  bool
	ascii    bool
	loaded   bool
	pres     cset
	next     [128]cset
}

func (x *subject) reset(s string) {
	x.s, x.lower, x.lowered, x.ascii, x.loaded = s, "", false, true, true
	x.pres = cset{}
	x.next = [128]cset{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			x.ascii = false
			return
		}
		x.pres.add(c)
		if i > 0 {
			x.next[s[i-1]].add(c)
		}
	}
}

func (x *subject) lowerText() string {
	if !x.lowered {
		x.lower, x.lowered = strings.ToLower(x.s), true
	}
	return x.lower
}

// hasPair reports whether a byte of a is directly followed by a byte of b.
func (x *subject) hasPair(a, b cset) bool {
	lo, hi := a.lo&x.pres.lo, a.hi&x.pres.hi
	for lo|hi != 0 {
		var c int
		if lo != 0 {
			c = bits.TrailingZeros64(lo)
			lo &= lo - 1
		} else {
			c = 64 + bits.TrailingZeros64(hi)
			hi &= hi - 1
		}
		n := &x.next[c]
		if n.lo&b.lo|n.hi&b.hi != 0 {
			return true
		}
	}
	return false
}

// in reports whether l may occur in x: true when it does or when that cannot be
// told.
func (l *lit) in(x *subject) bool {
	switch {
	case l.bi:
		return !x.ascii || x.hasPair(l.a, l.b)
	case l.na:
		return !x.ascii
	}
	hay := x.s
	if l.fold {
		if !x.ascii {
			return true
		}
		hay = x.lowerText()
	}
	if !x.ascii || (!l.lb && !l.rb) {
		return strings.Contains(hay, l.s)
	}
	for off := 0; ; {
		i := strings.Index(hay[off:], l.s)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(l.s)
		if (!l.lb || i == 0 || !isWordByte(hay[i-1])) && (!l.rb || end == len(hay) || !isWordByte(hay[end])) {
			return true
		}
		off = i + 1
	}
}

// leads reports whether x may start with l.
func (l *lit) leads(x *subject) bool {
	switch {
	case l.na:
		return !x.ascii
	case l.fold:
		return !x.ascii || strings.HasPrefix(x.lowerText(), l.s)
	}
	return strings.HasPrefix(x.s, l.s)
}

// maxClauses is how many clauses a guard checks: the ones that rule out most.
const maxClauses = 10

// guard: a matching text passes one of the branches, and the pattern has no match
// after its first when it is anchored: every branch starts with a start anchor.
type guard struct {
	branches []branch
	anchored bool
}

// branch: a matching text has at least min bytes, starts with lead when it is
// set and with a byte of start when hasStart, and for every clause of need holds
// one of the clause's literals.
type branch struct {
	min      int
	lead     *lit
	start    cset
	hasStart bool
	need     [][]lit
	// needAfter is need without the clauses that name text before the match.
	needAfter [][]lit
	bol       bool
}

// rules reports whether the pattern cannot match x.
func (g *guard) rules(x *subject) bool {
	for i := range g.branches {
		if !g.branches[i].rules(x) {
			return false
		}
	}
	return true
}

// rulesAfter reports whether the pattern cannot match in x, the text after an
// earlier match.
func (g *guard) rulesAfter(x *subject) bool {
	for i := range g.branches {
		if !g.branches[i].rulesAfter(x) {
			return false
		}
	}
	return true
}

// rulesAfter reports whether a match in x cannot take this branch. A branch that
// starts with a start anchor matches only at the start of the whole text.
func (g *branch) rulesAfter(x *subject) bool {
	if g.bol || len(x.s) < g.min {
		return true
	}
	for _, clause := range g.needAfter {
		found := false
		for i := range clause {
			if clause[i].in(x) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// rules reports whether a text that matches the pattern cannot take this branch.
func (g *branch) rules(x *subject) bool {
	if len(x.s) < g.min {
		return true
	}
	if g.hasStart && x.s[0] < 0x80 && !g.start.has(x.s[0]) {
		return true
	}
	if g.lead != nil && !g.lead.leads(x) {
		return true
	}
	for _, clause := range g.need {
		found := false
		for i := range clause {
			if clause[i].in(x) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// guards maps compiled patterns to their guards. A pattern replaced after
// construction has none.
type guards map[*regexp2.Regexp]*guard

// ruledOut reports whether re has a guard that rules out s.
func (b *budget) ruledOut(re *regexp2.Regexp, s string) bool {
	g := b.guards[re]
	if g == nil {
		return false
	}
	if !b.subject.loaded || b.subject.s != s {
		b.subject.reset(s)
	}
	return g.rules(&b.subject)
}

// part is a piece of a branch that consumes text: the fewest characters it
// consumes and the bytes a non-empty match of it starts and ends with.
type part struct {
	min         int
	first, last cset
}

// frag is what the analysis knows of a pattern fragment: the fewest runes it
// consumes, clauses every match holds, the literal every match starts with, whether
// it starts with a start anchor, the bytes a non-empty match starts and ends
// with, and exact: the literals of which a match is exactly one.
type frag struct {
	min         int
	need        [][]lit
	lead        *lit
	bol         bool
	first, last cset
	exact       []lit
}

// analyzer reads a pattern source for guards. It models plain patterns and
// stops at anything else (bad).
type analyzer struct {
	src   []rune
	i     int
	ic, x bool
	bad   bool
}

// newGuard derives the guard of src, nil when it models nothing useful. A branch
// of a top-level alternation gets a guard of its own, because a text only has to
// pass one of them.
func newGuard(src string, opts regexp2.RegexOptions) *guard {
	if opts&^(regexp2.IgnoreCase|regexp2.IgnorePatternWhitespace) != 0 {
		return nil
	}
	a := &analyzer{src: []rune(src), ic: opts&regexp2.IgnoreCase != 0, x: opts&regexp2.IgnorePatternWhitespace != 0}
	g := &guard{anchored: true}
	for {
		f := a.seq()
		if a.bad {
			return nil
		}
		br, ok := newBranch(f)
		if !ok {
			return nil
		}
		g.branches = append(g.branches, br)
		g.anchored = g.anchored && f.bol
		if !a.at('|') {
			break
		}
		a.i++
	}
	if a.i != len(a.src) {
		return nil
	}
	return g
}

// newBranch derives the guard of one branch; false when it rules out nothing.
func newBranch(f frag) (branch, bool) {
	b := branch{min: f.min, bol: f.bol}
	if f.bol {
		b.lead = f.lead
		if f.min > 0 && f.first != every {
			b.start, b.hasStart = f.first, true
		}
	}
	var clauses [][]lit
	for _, clause := range f.need {
		if len(clause) <= 128 && clauseP(clause) < 1 {
			clauses = append(clauses, clause)
		}
	}
	slices.SortStableFunc(clauses, func(x, y []lit) int { return cmp.Compare(clauseP(x), clauseP(y)) })
	b.need = clauses[:min(len(clauses), maxClauses)]
	for _, clause := range b.need {
		if !slices.ContainsFunc(clause, func(l lit) bool { return l.behind }) {
			b.needAfter = append(b.needAfter, clause)
		}
	}
	return b, b.min > 0 || b.lead != nil || b.hasStart || len(b.need) > 0
}

// lit makes the literal of runes r as the pattern reads it.
func (a *analyzer) lit(r []rune) lit {
	l := lit{s: string(r)}
	if !a.ic {
		return l
	}
	for _, c := range r {
		switch {
		case c >= 0x80:
			l.na = true
			if foldsToASCII(c) {
				a.bad = true
			}
		case unicode.IsLetter(c):
			l.fold = true
		}
	}
	if l.fold && !l.na {
		l.s = strings.ToLower(l.s)
	}
	return l
}

// litSet is the set of ASCII bytes the literal rune r matches.
func (a *analyzer) litSet(r rune) cset {
	var s cset
	if r >= 0x80 {
		if a.ic && foldsToASCII(r) {
			a.bad = true
		}
		return s
	}
	s.add(byte(r))
	if a.ic {
		s.add(byte(unicode.ToLower(r)))
		s.add(byte(unicode.ToUpper(r)))
	}
	return s
}

// atomSet is the set of ASCII bytes the one-character atom src matches under the
// options in force, as regexp2 itself decides it.
func (a *analyzer) atomSet(src string) cset {
	key := setKey{src, a.ic, a.x}
	if s, ok := build.sets[key]; ok {
		return s
	}
	var opts regexp2.RegexOptions
	if a.ic {
		opts |= regexp2.IgnoreCase
	}
	if a.x {
		opts |= regexp2.IgnorePatternWhitespace
	}
	re, err := regexp2.Compile(`^(?:`+src+`)$`, opts)
	if err != nil {
		a.bad = true
		return cset{}
	}
	var s cset
	for c := 0; c < 128; c++ {
		ok, err := re.MatchString(string(rune(c)))
		if err != nil {
			a.bad = true
			return cset{}
		}
		if ok {
			s.add(byte(c))
		}
	}
	build.sets[key] = s
	return s
}

// foldsToASCII reports whether a non-ASCII rune is case-equivalent to an ASCII
// one (the Kelvin sign, the long s), which would let ASCII text match it.
func foldsToASCII(c rune) bool {
	if unicode.ToLower(c) < 0x80 || unicode.ToUpper(c) < 0x80 {
		return true
	}
	for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
		if f < 0x80 {
			return true
		}
	}
	return false
}

func (a *analyzer) at(c rune) bool { return a.i < len(a.src) && a.src[a.i] == c }

// blank skips what a pattern with IgnorePatternWhitespace ignores.
func (a *analyzer) blank() {
	for a.i < len(a.src) {
		switch {
		case unicode.IsSpace(a.src[a.i]):
			a.i++
		case a.src[a.i] == '#':
			for a.i < len(a.src) && a.src[a.i] != '\n' {
				a.i++
			}
		default:
			return
		}
	}
}

// alt reads branches up to the closing parenthesis or the end. The clause of an
// alternation is the union of the best clause of each branch, and it has none
// when a branch has none.
func (a *analyzer) alt() frag {
	f := a.seq()
	if !a.at('|') {
		return f
	}
	out := frag{min: f.min}
	var union, exacts []lit
	complete, allExact := true, true
	for {
		out.min = min(out.min, f.min)
		out.first, out.last = out.first.or(f.first), out.last.or(f.last)
		if best := bestClause(f.need); best == nil {
			complete = false
		} else {
			for _, l := range best {
				if !slices.Contains(union, l) {
					union = append(union, l)
				}
			}
		}
		if f.exact == nil {
			allExact = false
		} else {
			exacts = append(exacts, f.exact...)
		}
		if !a.at('|') {
			break
		}
		a.i++
		f = a.seq()
	}
	if complete {
		out.need = [][]lit{union}
	}
	if complete && allExact {
		out.exact = exacts
	}
	return out
}

// bestClause is the clause a release name is least likely to hold.
func bestClause(cnf [][]lit) []lit {
	var best []lit
	bp := 2.0
	for _, c := range cnf {
		if p := clauseP(c); p < bp || (p == bp && len(c) < len(best)) {
			best, bp = c, p
		}
	}
	return best
}

// edgeWord reports whether the left (or right) edge of l is a word character, so
// that a word boundary there says something about the text beside it.
func edgeWord(l *lit, left bool) bool {
	if l.na || l.bi || l.s == "" {
		return false
	}
	if left {
		return isWordByte(l.s[0])
	}
	return isWordByte(l.s[len(l.s)-1])
}

// bound puts a word boundary on the left (or right) of every literal of a clause
// that has a word character there.
func bound(clause []lit, left bool) {
	for k := range clause {
		if l := &clause[k]; edgeWord(l, left) {
			if left {
				l.lb = true
			} else {
				l.rb = true
			}
		}
	}
}

// seq reads one branch: atoms with their quantifiers up to | or ).
func (a *analyzer) seq() frag {
	var (
		f        frag
		parts    []part
		run      []rune // the adjacent literal runes read so far
		runLB    bool   // a word boundary came right before the run
		lead     []rune
		leadFrag *lit
		leadOpen = true // only literals have been read
		first    = true
		wb       bool // a word boundary came right before the next atom
		exact    = -1 // the index in f.need of the exact group the last atom was
		toggled  bool // an inline option changed what the rest of the branch reads
		runs     int
		other    bool // something besides literal runs was read
		runLit   lit
	)
	flush := func(rb bool) {
		if len(run) == 0 {
			return
		}
		l := a.lit(run)
		if runLB && edgeWord(&l, true) {
			l.lb = true
		}
		if rb && edgeWord(&l, false) {
			l.rb = true
		}
		f.need = append(f.need, []lit{l})
		parts = append(parts, part{len(run), a.litSet(run[0]), a.litSet(run[len(run)-1])})
		runLit = l
		runs++
		run, runLB = nil, false
	}
	for a.i < len(a.src) && !a.bad {
		if a.x {
			a.blank()
			if a.i >= len(a.src) {
				break
			}
		}
		c := a.src[a.i]
		if c == '|' || c == ')' {
			break
		}
		if c == '\\' && a.i+1 < len(a.src) && a.src[a.i+1] == 'b' {
			a.i += 2
			if a.at('*') || a.at('?') || a.at('+') {
				a.bad = true // a quantified assertion is not modelled
			}
			first = false
			if len(run) > 0 {
				flush(true)
			} else if exact >= 0 {
				bound(f.need[exact], false)
			}
			wb = true
			continue
		}
		var (
			at    frag
			ch    rune
			isLit bool
			zero  bool
		)
		switch c {
		case '^':
			a.i++
			zero = true
			if first {
				f.bol = true
			}
		case '$':
			a.i++
			zero = true
		case '(':
			ic := a.ic
			at, zero = a.group()
			if a.ic != ic {
				toggled = true
				now := a.ic
				a.ic = ic
				flush(false)
				a.ic = now
			}
		case '[':
			start := a.i
			a.class()
			if !a.bad {
				at.min = 1
				at.first = a.atomSet(string(a.src[start:a.i]))
				at.last = at.first
			}
		case '.':
			a.i++
			at.min = 1
			at.first = a.atomSet(".")
			at.last = at.first
		case '\\':
			ch, isLit, zero, at = a.escape()
		default:
			a.i++
			ch, isLit = c, true
		}
		first = false
		if a.bad {
			break
		}
		if zero {
			f.need = append(f.need, at.need...)
			if (c == '(' || c == '^' || c == '$') && (a.at('*') || a.at('?') || a.at('+')) {
				a.bad = true // a quantified lookaround is not modelled
			}
			continue
		}
		n, quantified := a.quant()
		if isLit && !quantified {
			if len(run) == 0 {
				runLB = wb
			}
			run = append(run, ch)
			if leadOpen {
				lead = append(lead, ch)
			}
			wb, exact = false, -1
			continue
		}
		other = true
		flush(false)
		if isLit {
			at.min = 1
			at.first = a.litSet(ch)
			at.last = at.first
			at.need = [][]lit{{a.lit([]rune{ch})}}
		}
		parts = append(parts, part{at.min * n, at.first, at.last})
		if n >= 1 {
			f.need = append(f.need, at.need...)
			if leadOpen && len(lead) == 0 && leadFrag == nil {
				leadFrag = at.lead
			}
		}
		exact = -1
		if at.exact != nil && !quantified && len(at.need) == 1 {
			exact = len(f.need) - 1
			if wb {
				bound(f.need[exact], true)
			}
		}
		wb = false
		leadOpen = false
	}
	flush(false)
	for i, p := range parts {
		f.min += p.min
		if i > 0 && p.min >= 1 && parts[i-1].min >= 1 {
			f.need = append(f.need, []lit{{bi: true, a: parts[i-1].last, b: p.first}})
		}
	}
	for _, p := range parts {
		f.first = f.first.or(p.first)
		if p.min > 0 {
			break
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		f.last = f.last.or(parts[i].last)
		if parts[i].min > 0 {
			break
		}
	}
	switch {
	case toggled:
	case len(lead) > 0:
		l := a.lit(lead)
		f.lead = &l
	default:
		f.lead = leadFrag
	}
	if !other && runs == 1 {
		f.exact = []lit{runLit}
	}
	return f
}

// group reads a parenthesized fragment. zero: it consumes nothing (a lookaround
// or an (?i) option, which sets whether the rest of the enclosing group ignores
// case).
func (a *analyzer) group() (f frag, zero bool) {
	a.i++
	ic := a.ic
	if a.at('?') {
		a.i++
		switch {
		case a.at(':'), a.at('>'):
			a.i++
		case a.at('='), a.at('!'):
			positive := a.at('=')
			a.i++
			return a.look(positive, false), true
		case a.at('<'):
			a.i++
			if a.at('=') || a.at('!') {
				positive := a.at('=')
				a.i++
				return a.look(positive, true), true
			}
			a.name('>')
		case a.at('\''):
			a.i++
			a.name('\'')
		case a.at('i'), a.at('-'):
			on := a.at('i')
			if !on {
				a.i++
			}
			if !a.at('i') {
				a.bad = true
				return f, false
			}
			a.i++
			switch {
			case a.at(')'):
				a.i++
				a.ic = on
				return f, true
			case a.at(':'):
				a.i++
				a.ic = on
			default:
				a.bad = true
				return f, false
			}
		default:
			a.bad = true
			return f, false
		}
	}
	f = a.alt()
	a.ic = ic
	if !a.at(')') {
		a.bad = true
		return f, false
	}
	a.i++
	f.bol = false
	return f, false
}

// look reads the body of a lookaround. The body of a positive one has to match
// where the lookaround stands, so the text holds whatever the body needs; a
// negative one says nothing a guard can use.
func (a *analyzer) look(positive, behind bool) (f frag) {
	if !positive {
		a.skip()
		return f
	}
	ic := a.ic
	body := a.alt()
	a.ic = ic
	if !a.at(')') {
		a.bad = true
		return f
	}
	a.i++
	for _, clause := range body.need {
		for k := range clause {
			clause[k].behind = clause[k].behind || behind
		}
	}
	f.need = body.need
	return f
}

// name reads a group name and its closing delimiter.
func (a *analyzer) name(end rune) {
	start := a.i
	for a.i < len(a.src) && (a.src[a.i] == '_' || unicode.IsLetter(a.src[a.i]) || unicode.IsDigit(a.src[a.i])) {
		a.i++
	}
	if a.i == start || !a.at(end) {
		a.bad = true
		return
	}
	a.i++
}

// skip passes the rest of a group without reading it.
func (a *analyzer) skip() {
	for depth := 1; a.i < len(a.src) && !a.bad; {
		switch c := a.src[a.i]; {
		case c == '\\':
			a.i += 2
		case c == '[':
			a.class()
		case c == '(':
			depth++
			a.i++
		case c == ')':
			depth--
			a.i++
			if depth == 0 {
				return
			}
		case c == '#' && a.x:
			a.blank()
		default:
			a.i++
		}
	}
	a.bad = true
}

// class passes a character class. A class inside a class (a subtraction) is not
// modelled.
func (a *analyzer) class() {
	a.i++
	if a.at('^') {
		a.i++
	}
	if a.at(']') {
		a.i++
	}
	for a.i < len(a.src) {
		switch a.src[a.i] {
		case '\\':
			a.i += 2
		case '[':
			a.bad = true
			return
		case ']':
			a.i++
			return
		default:
			a.i++
		}
	}
	a.bad = true
}

// escape reads a backslash escape: a literal rune, a zero-width assertion, or a
// single-character class.
func (a *analyzer) escape() (ch rune, isLit, zero bool, f frag) {
	if a.i+1 >= len(a.src) {
		a.bad = true
		return
	}
	start := a.i
	e := a.src[a.i+1]
	a.i += 2
	switch {
	case strings.ContainsRune("bBAZzG", e):
		zero = true
	case strings.ContainsRune("dDwWsSnrtfve", e):
	case e == 'p' || e == 'P':
		if !a.at('{') {
			a.bad = true
			return
		}
		for a.i < len(a.src) && a.src[a.i] != '}' {
			a.i++
		}
		a.i++
	case e == 'u':
		a.i += 4
	case e == 'k':
		// A backreference consumes whatever the group held: at least nothing.
		switch {
		case a.at('<'):
			a.i++
			a.name('>')
		case a.at('\''):
			a.i++
			a.name('\'')
		default:
			a.bad = true
		}
		f.first, f.last = every, every
		return
	case unicode.IsLetter(e) || unicode.IsDigit(e):
		a.bad = true
	default:
		ch, isLit = e, true
	}
	if !zero && !isLit && !a.bad {
		if a.i > len(a.src) {
			a.bad = true
			return
		}
		f.min = 1
		f.first = a.atomSet(string(a.src[start:a.i]))
		f.last = f.first
	}
	return
}

// quant reads the quantifier after an atom and returns its minimum repeat.
func (a *analyzer) quant() (n int, quantified bool) {
	if a.x {
		a.blank()
	}
	switch {
	case a.at('*'), a.at('?'):
		a.i++
	case a.at('+'):
		a.i++
		n = 1
	case a.at('{'):
		j, lo := a.i+1, 0
		for j < len(a.src) && a.src[j] >= '0' && a.src[j] <= '9' {
			lo = lo*10 + int(a.src[j]-'0')
			j++
		}
		if j == a.i+1 {
			return 1, false
		}
		if j < len(a.src) && a.src[j] == ',' {
			j++
			for j < len(a.src) && a.src[j] >= '0' && a.src[j] <= '9' {
				j++
			}
		}
		if j >= len(a.src) || a.src[j] != '}' {
			return 1, false
		}
		a.i, n = j+1, lo
	default:
		return 1, false
	}
	if a.at('?') {
		a.i++
	}
	return n, true
}
