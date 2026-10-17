package parse

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

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
// so a late success after expiry is an error, not a pass.
func (b *budget) isMatch(re *regexp2.Regexp, s string) (bool, error) {
	if err := b.check(); err != nil {
		return false, err
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
func (b *budget) next(re *regexp2.Regexp, m *regexp2.Match) (*regexp2.Match, error) {
	if err := b.check(); err != nil {
		return nil, err
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

// last is Matches(s).LastOrDefault(): the final successive match, or nil.
func (b *budget) last(re *regexp2.Regexp, s string) (*regexp2.Match, error) {
	m, err := b.first(re, s)
	var last *regexp2.Match
	for m != nil && err == nil {
		last = m
		m, err = b.next(re, m)
	}
	return last, err
}

// count is Matches(s).Count.
func (b *budget) count(re *regexp2.Regexp, s string) (int, error) {
	m, err := b.first(re, s)
	n := 0
	for m != nil && err == nil {
		n++
		m, err = b.next(re, m)
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
