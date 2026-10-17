// Package parse is the native, bounded port of the series and movie release
// name parsers. A Parser owns its immutable attributed pattern tables, built
// once per instance with the fixed 50 ms regexp2 match timeout, and is safe
// for concurrent use. Every public operation validates the name first and then
// runs under one shared elapsed-work budget, checked before and after every
// match; any limit, budget, cancellation or matching failure is a
// *ReasonError and never a usable value.
//
// Quality parsing ports the GPL-3.0 QualityParser of the series reference
// (kind KindSeries) and of the movie reference (kind KindMovie); see
// quality_series.go and quality_movie.go for the exact source commits.
package parse

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Kind selects the source application's parser.
type Kind uint8

const (
	// KindMovie ports the movie reference parser.
	KindMovie Kind = 1
	// KindSeries ports the series reference parser.
	KindSeries Kind = 2
)

// Reason is the stable, non-secret explanation of a rejected operation. Code
// drives handling; Params holds only numeric limits, observed counts and
// indexes, never names, patterns or dependency errors.
type Reason struct {
	Code   string
	Params map[string]int64
}

// ReasonError carries a Reason and is discoverable through errors.As.
type ReasonError struct {
	Reason Reason
}

func (e *ReasonError) Error() string {
	var sb strings.Builder
	sb.WriteString("parse: ")
	sb.WriteString(e.Reason.Code)
	keys := make([]string, 0, len(e.Reason.Params))
	for k := range e.Reason.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, " %s=%d", k, e.Reason.Params[k])
	}
	return sb.String()
}

func newReasonError(code string, params map[string]int64) *ReasonError {
	return &ReasonError{Reason: Reason{Code: code, Params: params}}
}

// Revision is the dimensionless revision of a release (proper/repack/real).
type Revision struct {
	Version  int
	Real     int
	IsRepack bool
}

// Quality is a parsed quality. Code, Source and Modifier are stable Boxset
// codes from explicit per-application maps (vocab.go); Resolution is the
// source-native pixel height (0 unknown). The Detection fields name where the
// facts came from: unknown, name, extension or mediainfo; empty means the
// source application has no such concept (the series side has no modifier).
type Quality struct {
	Code                string
	Source              string
	Resolution          int
	Modifier            string
	Revision            Revision
	SourceDetection     string
	ResolutionDetection string
	ModifierDetection   string
	RevisionDetection   string
}

// Options configures NewParser. Zero values select the production clock and
// the local calendar.
type Options struct {
	// Now is the clock of the elapsed-work budget and, for title parsing, of
	// relative dates. Nil selects time.Now.
	Now func() time.Time
	// Location is the local calendar of relative dates. Nil selects
	// time.Local.
	Location *time.Location
}

// Parser is the immutable, concurrency-safe release name parser.
type Parser struct {
	now      func() time.Time
	location *time.Location
	series   *seriesTable
	movie    *movieTable
}

// NewParser compiles the fixed attributed pattern tables once. An invalid
// built-in pattern aborts construction with a *ReasonError.
func NewParser(options Options) (*Parser, error) {
	p := &Parser{now: options.Now, location: options.Location}
	if p.now == nil {
		p.now = time.Now
	}
	if p.location == nil {
		p.location = time.Local
	}
	var err error
	if p.series, err = newSeriesTable(); err != nil {
		return nil, err
	}
	if p.movie, err = newMovieTable(); err != nil {
		return nil, err
	}
	return p, nil
}

// ParseQuality parses the quality of name for kind. The name is validated
// (at most 4096 UTF-8 bytes, valid UTF-8) and the kind checked before any
// matching; the whole parse then runs under one 250 ms elapsed-work budget,
// checked once more before the result is returned. On any error the returned
// Quality is the zero value.
func (p *Parser) ParseQuality(ctx context.Context, name string, kind Kind) (Quality, error) {
	if kind != KindMovie && kind != KindSeries {
		return Quality{}, newReasonError(codeKindUnsupported, map[string]int64{"kind": int64(kind)})
	}
	if err := validateName(name); err != nil {
		return Quality{}, err
	}
	b := newBudget(ctx, p.now)
	if err := b.check(); err != nil {
		return Quality{}, err
	}
	var (
		q   Quality
		err error
	)
	if kind == KindMovie {
		q, err = p.movie.parseQuality(b, name)
	} else {
		q, err = p.series.parseQuality(b, name)
	}
	if err != nil {
		return Quality{}, err
	}
	// The budget and context are checked once more before any usable value
	// leaves, so a result that completed after expiry or cancellation (blank
	// and unmatched names included) is an error, never a pass.
	if err := b.check(); err != nil {
		return Quality{}, err
	}
	return q, nil
}
