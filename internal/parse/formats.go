package parse

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"

	"github.com/dlclark/regexp2/v2"
)

// Outcome is the result of one format for one release name.
type Outcome uint8

const (
	// OutcomeMatched: every required condition holds and, when the format has
	// optional conditions, at least one of them holds.
	OutcomeMatched Outcome = 1
	// OutcomeNotMatched: the format was completely evaluated and does not hold.
	OutcomeNotMatched Outcome = 2
	// OutcomeIndeterminate names a format whose evaluation did not complete.
	// Evaluate and EvaluateBatch never put it in a successful list: an
	// incomplete evaluation is an error (or a Rejected release), so no format
	// outcome of that evaluation is usable.
	OutcomeIndeterminate Outcome = 3
)

// ReleaseState is the state of one EvaluateBatch row.
type ReleaseState uint8

const (
	// ReleaseCompleted: the release was completely evaluated; Formats is the
	// full ordered list and Reason is nil.
	ReleaseCompleted ReleaseState = 1
	// ReleaseRejected: the release failed on its own (its match timeout, stack
	// limit or 250 ms budget); Reason says why and Formats is nil. The batch
	// continued with the next release.
	ReleaseRejected ReleaseState = 2
	// ReleaseUnscheduled: a terminal condition (cancellation or the 30 s
	// decision deadline) stopped the batch before this release completed;
	// Reason is that terminal reason and Formats is nil.
	ReleaseUnscheduled ReleaseState = 3
)

// FormatSpec is one custom format: an ID and its conditions.
type FormatSpec struct {
	ID         string
	Conditions []ConditionSpec
}

// ConditionSpec is one regex condition. Patterns are .NET-style (regexp2) and
// match case-insensitively; use (?-i) to opt out. Negate inverts the match;
// Required conditions must all hold, and of the non-required ones at least
// one must hold when any exist.
type ConditionSpec struct {
	Pattern  string
	Negate   bool
	Required bool
}

// FormatResult is the outcome of one format. Reason is set only for an
// Indeterminate outcome, which this package never returns in a usable list.
type FormatResult struct {
	ID      string
	Outcome Outcome
	Reason  *Reason
}

// ReleaseResult is one row of an EvaluateBatch result. Index is the position
// of the name in the input. When EvaluateBatch also returns an error, every
// row is diagnostic only and must not authorize any effect.
type ReleaseResult struct {
	Index   int
	State   ReleaseState
	Formats []FormatResult
	Reason  *Reason
}

// The admitted maxima bound the batch output backing far below the cap; this
// fails to compile if a limit ever changes so that they no longer do.
const _ = uint(maxBatchOutputBytes - maxBatchReleases*(unsafe.Sizeof(ReleaseResult{})+maxFormats*unsafe.Sizeof(FormatResult{})))

type compiledCondition struct {
	re       *regexp2.Regexp
	negate   bool
	required bool
}

type compiledFormat struct {
	id         string
	conditions []compiledCondition
}

// formatSet is one immutable compiled revision. specs is the retained source
// (owned copies) that a same-revision recompile is compared against. readers
// is guarded by formatState.mu.
type formatSet struct {
	revision uint64
	specs    []FormatSpec
	formats  []compiledFormat
	readers  int
}

// formatState is the Parser-owned compiled payload: one current set, one
// serialized candidate (publish), and retired sets kept alive only by the at
// most maxEvaluations admitted operations. Nothing here escapes the package.
type formatState struct {
	publish  sync.Mutex // serializes CompileFormats; held while compiling, never by readers
	compiles int        // regexp2 compilations so far, guarded by publish

	mu      sync.Mutex // guards the fields below; held only briefly
	current *formatSet
	retired []*formatSet // replaced sets still held by a reader
	readers int          // admitted operations
}

// CompileFormats compiles specs as revision and, only when every format
// compiles, publishes them as the one current profile. Revisions must not
// decrease: the same revision with the same content is a no-op reuse, the same
// revision with different content is format_revision_conflict and a lower
// revision is format_revision_stale. Counts, ID and source sizes and encoding
// are validated for every format before anything is compiled. On any error the
// previous profile stays current. Errors are *ReasonError values whose
// Params carry the 0-based format_index and condition_index where they apply,
// never an ID, a pattern or a dependency error.
func (p *Parser) CompileFormats(ctx context.Context, revision uint64, specs []FormatSpec) error {
	if err := ctxReason(ctx); err != nil {
		return err
	}
	if err := validateFormats(specs); err != nil {
		return err
	}
	f := &p.formats
	f.publish.Lock()
	defer f.publish.Unlock()

	// Only a holder of publish replaces current, so reading it unlocked here
	// cannot race.
	if cur := f.current; cur != nil {
		switch {
		case revision < cur.revision:
			return newReasonError(codeFormatRevisionStale, nil)
		case revision == cur.revision:
			if slices.EqualFunc(cur.specs, specs, equalFormatSpec) {
				return nil
			}
			return newReasonError(codeFormatRevisionConflict, nil)
		}
	}

	set := &formatSet{revision: revision, specs: cloneSpecs(specs)}
	set.formats = make([]compiledFormat, len(set.specs))
	for i, spec := range set.specs {
		set.formats[i] = compiledFormat{id: spec.ID, conditions: make([]compiledCondition, len(spec.Conditions))}
		for j, c := range spec.Conditions {
			if err := ctxReason(ctx); err != nil {
				return err
			}
			re, err := regexp2.Compile(c.Pattern, regexp2.IgnoreCase)
			f.compiles++
			if err != nil {
				return newReasonError(codeRegexCompileFailed, map[string]int64{
					"format_index":    int64(i),
					"condition_index": int64(j),
				})
			}
			re.MatchTimeout = matchTimeout
			set.formats[i].conditions[j] = compiledCondition{re: re, negate: c.Negate, required: c.Required}
		}
	}

	f.mu.Lock()
	if f.current != nil && f.current.readers > 0 {
		f.retired = append(f.retired, f.current)
	}
	f.current = set
	f.mu.Unlock()
	return nil
}

// validateFormats rejects an over-limit or mis-encoded profile before any
// pattern is compiled, in format and condition order.
func validateFormats(specs []FormatSpec) error {
	if len(specs) > maxFormats {
		return newReasonError(codeTooManyFormats, map[string]int64{"limit": maxFormats, "observed": int64(len(specs))})
	}
	for i, spec := range specs {
		fi := int64(i)
		if len(spec.ID) > maxFormatIDBytes {
			return newReasonError(codeFormatIDTooLong, map[string]int64{
				"format_index": fi, "limit_bytes": maxFormatIDBytes, "observed_bytes": int64(len(spec.ID)),
			})
		}
		if !utf8.ValidString(spec.ID) {
			return newReasonError(codeFormatIDInvalidUTF8, map[string]int64{
				"format_index": fi, "offset": int64(firstInvalidUTF8(spec.ID)),
			})
		}
		if len(spec.Conditions) > maxConditions {
			return newReasonError(codeTooManyConditions, map[string]int64{
				"format_index": fi, "limit": maxConditions, "observed": int64(len(spec.Conditions)),
			})
		}
		for j, c := range spec.Conditions {
			ci := int64(j)
			if len(c.Pattern) > maxPatternBytes {
				return newReasonError(codeRegexSourceTooLong, map[string]int64{
					"format_index": fi, "condition_index": ci,
					"limit_bytes": maxPatternBytes, "observed_bytes": int64(len(c.Pattern)),
				})
			}
			if !utf8.ValidString(c.Pattern) {
				return newReasonError(codeRegexCompileFailed, map[string]int64{"format_index": fi, "condition_index": ci})
			}
		}
	}
	return nil
}

func equalFormatSpec(a, b FormatSpec) bool {
	return a.ID == b.ID && slices.Equal(a.Conditions, b.Conditions)
}

// cloneSpecs deep-copies specs so the retained source never aliases a caller
// buffer (a short ID or pattern must not pin a larger backing string).
func cloneSpecs(specs []FormatSpec) []FormatSpec {
	out := make([]FormatSpec, len(specs))
	for i, spec := range specs {
		out[i].ID = strings.Clone(spec.ID)
		out[i].Conditions = make([]ConditionSpec, len(spec.Conditions))
		for j, c := range spec.Conditions {
			c.Pattern = strings.Clone(c.Pattern)
			out[i].Conditions[j] = c
		}
	}
	return out
}

// acquire admits one evaluation and pins the current set for it. It never
// waits: with no profile it is format_not_configured and with
// maxEvaluations operations already admitted it is evaluator_busy.
func (f *formatState) acquire() (*formatSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == nil {
		return nil, newReasonError(codeFormatNotConfigured, nil)
	}
	if f.readers >= maxEvaluations {
		return nil, newReasonError(codeEvaluatorBusy, map[string]int64{"limit": maxEvaluations})
	}
	f.readers++
	f.current.readers++
	return f.current, nil
}

// release returns the lease taken by acquire; a retired set is reclaimed with
// its last reader.
func (f *formatState) release(s *formatSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readers--
	s.readers--
	if s != f.current && s.readers == 0 {
		if i := slices.Index(f.retired, s); i >= 0 {
			f.retired = slices.Delete(f.retired, i, i+1)
		}
	}
}

// Evaluate evaluates every format of the current profile against name and
// returns the outcomes in profile order. It pins the profile that is current
// at entry and runs under one 250 ms elapsed-work budget. Any failure -- an
// invalid name, no profile, a busy evaluator, a match timeout or stack limit,
// an exhausted budget, cancellation -- is a *ReasonError and returns no
// results: a partially evaluated profile is never usable, and neither
// negation nor an earlier match can turn an incomplete evaluation into a
// match.
func (p *Parser) Evaluate(ctx context.Context, name string) ([]FormatResult, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	set, err := p.formats.acquire()
	if err != nil {
		return nil, err
	}
	defer p.formats.release(set)
	return set.evaluate(newBudget(ctx, p.now), name, make([]FormatResult, 0, len(set.formats)))
}

// evaluate appends the outcome of every format to out (whose capacity is the
// format count). The budget is checked before the first match and after the
// last, and by every match, so work that completed after expiry is an error.
func (s *formatSet) evaluate(b *budget, name string, out []FormatResult) ([]FormatResult, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	for i := range s.formats {
		outcome, err := s.formats[i].evaluate(b, name)
		if err != nil {
			return nil, err
		}
		out = append(out, FormatResult{ID: s.formats[i].id, Outcome: outcome})
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	return out, nil
}

// evaluate evaluates every condition even when the outcome is already
// decided, so an error in any condition fails the format instead of being
// skipped behind an earlier result. A format without conditions matches
// nothing.
func (f *compiledFormat) evaluate(b *budget, name string) (Outcome, error) {
	required, optional, optionalMatched := true, false, false
	for _, c := range f.conditions {
		ok, err := b.isMatch(c.re, name)
		if err != nil {
			return 0, err
		}
		if c.negate {
			ok = !ok
		}
		if c.required {
			required = required && ok
		} else {
			optional = true
			optionalMatched = optionalMatched || ok
		}
	}
	if len(f.conditions) > 0 && required && (!optional || optionalMatched) {
		return OutcomeMatched, nil
	}
	return OutcomeNotMatched, nil
}

// EvaluateBatch evaluates names, each as Evaluate would, against the one
// profile that is current at entry (publication during the batch does not
// affect it). Counts, sizes and encodings of all names are validated, and the
// output is allocated once, before any work; a validation, configuration or
// admission error returns nil results.
//
// Each release gets a fresh 250 ms budget under one 30 s decision deadline. A
// release that fails on its own (match timeout, stack limit, its budget) is
// Rejected and the batch continues. Cancellation or the decision deadline is
// terminal: it returns the rows so far together with the error, the release it
// interrupted and every later one Unscheduled. With a non-nil error every row,
// completed ones included, is diagnostic only and must not authorize any
// effect. The result has exactly one row per name, at the name's index.
func (p *Parser) EvaluateBatch(ctx context.Context, names []string) ([]ReleaseResult, error) {
	if err := validateBatch(names); err != nil {
		return nil, err
	}
	set, err := p.formats.acquire()
	if err != nil {
		return nil, err
	}
	defer p.formats.release(set)

	decision := p.now().Add(batchDeadline)
	n := len(set.formats)
	rows := make([]ReleaseResult, len(names))
	backing := make([]FormatResult, len(names)*n)
	if len(names) == 0 {
		return rows, ctxReason(ctx) // no release to carry it, but a dead context is still terminal
	}
	for i, name := range names {
		b := newBudget(ctx, p.now)
		b.decision = decision
		formats, err := set.evaluate(b, name, backing[i*n:i*n:(i+1)*n])
		if err == nil {
			rows[i] = ReleaseResult{Index: i, State: ReleaseCompleted, Formats: formats}
			continue
		}
		reason := reasonOf(err)
		if !isTerminalCode(reason.Code) {
			// A release failure is local only while the batch is still live.
			if terr := b.terminal(); terr != nil {
				err, reason = terr, reasonOf(terr)
			}
		}
		if !isTerminalCode(reason.Code) {
			rows[i] = ReleaseResult{Index: i, State: ReleaseRejected, Reason: &reason}
			continue
		}
		for j := i; j < len(rows); j++ {
			r := Reason{Code: reason.Code, Params: maps.Clone(reason.Params)}
			rows[j] = ReleaseResult{Index: j, State: ReleaseUnscheduled, Reason: &r}
		}
		return rows, err
	}
	return rows, nil
}

// validateBatch bounds a batch before any allocation or work: the count, the
// aggregate bytes, then each name (its error carries the 0-based index).
func validateBatch(names []string) error {
	if len(names) > maxBatchReleases {
		return newReasonError(codeTooManyReleases, map[string]int64{"limit": maxBatchReleases, "observed": int64(len(names))})
	}
	total := 0
	for _, name := range names {
		total += len(name)
	}
	if total > maxBatchNameBytes {
		return newReasonError(codeBatchNameBytes, map[string]int64{"limit_bytes": maxBatchNameBytes, "observed_bytes": int64(total)})
	}
	for i, name := range names {
		if err := validateName(name); err != nil {
			var re *ReasonError
			if errors.As(err, &re) {
				re.Reason.Params["index"] = int64(i)
			}
			return err
		}
	}
	return nil
}

// reasonOf extracts the Reason of a parse error; an error that is not one is
// treated as terminal cancellation (fail closed).
func reasonOf(err error) Reason {
	var re *ReasonError
	if errors.As(err, &re) {
		return re.Reason
	}
	return Reason{Code: codeCancelled}
}

// isTerminalCode reports the reasons that end a whole batch rather than one
// release.
func isTerminalCode(code string) bool {
	return code == codeDeadlineExceeded || code == codeCancelled
}
