package parse

import (
	"unicode/utf8"
)

// Fixed parser limits. Names, format sources and budgets are validated before
// any pattern is compiled or matched; a limit breach is a structured reason,
// never a truncation.
const (
	// maxNameBytes bounds a release name: UTF-8 bytes, 4096 admitted, 4097 rejected.
	maxNameBytes = 4096

	// maxPatternBytes bounds one format condition's regex source in UTF-8 bytes.
	maxPatternBytes = 8192
	// maxFormats bounds the formats of one profile revision.
	maxFormats = 256
	// maxConditions bounds the conditions of one format.
	maxConditions = 32
	// maxFormatIDBytes bounds a format ID in UTF-8 bytes.
	maxFormatIDBytes = 64

	// maxBatchReleases bounds the names of one EvaluateBatch call.
	maxBatchReleases = 1024
	// maxBatchNameBytes bounds the aggregate bytes of one batch's names.
	maxBatchNameBytes = 4 << 20
	// maxBatchOutputBytes caps the output backing of one batch.
	maxBatchOutputBytes = 64 << 20

	// maxEvaluations bounds the admitted Evaluate/EvaluateBatch operations at
	// once; excess is rejected with evaluator_busy, never queued. Every
	// admitted operation pins one revision, so this also bounds the retired
	// compiled payloads that publication can leave alive.
	maxEvaluations = 2

	codeReleaseNameTooLong     = "release_name_too_long"
	codeReleaseNameInvalidUTF8 = "release_name_invalid_utf8"
	codeKindUnsupported        = "kind_unsupported"
	codeRegexSourceTooLong     = "regex_source_too_long"
	codeRegexCompileFailed     = "regex_compile_failed"
	codeTooManyFormats         = "too_many_formats"
	codeTooManyConditions      = "too_many_conditions"
	codeFormatIDTooLong        = "format_id_too_long"
	codeFormatIDInvalidUTF8    = "format_id_invalid_utf8"
	codeFormatRevisionConflict = "format_revision_conflict"
	codeFormatRevisionStale    = "format_revision_stale"
	codeFormatNotConfigured    = "format_not_configured"
	codeTooManyReleases        = "too_many_releases"
	codeBatchNameBytes         = "batch_name_bytes_exceeded"
	codeMatchTimeout           = "match_timeout"
	codeRegexStackLimit        = "regex_stack_limit"
	codeWorkBudgetExhausted    = "release_work_budget_exhausted"
	codeDeadlineExceeded       = "decision_deadline_exceeded"
	codeCancelled              = "evaluation_cancelled"
	codeEvaluatorBusy          = "evaluator_busy"
	codeTitleUnrecognized      = "title_unrecognized"
)

// validateName rejects a release name that is longer than maxNameBytes or is
// not valid UTF-8, before any matching work.
func validateName(name string) error {
	if len(name) > maxNameBytes {
		return newReasonError(codeReleaseNameTooLong, map[string]int64{
			"limit_bytes":    maxNameBytes,
			"observed_bytes": int64(len(name)),
		})
	}
	if !utf8.ValidString(name) {
		return newReasonError(codeReleaseNameInvalidUTF8, map[string]int64{"offset": int64(firstInvalidUTF8(name))})
	}
	return nil
}

func firstInvalidUTF8(s string) int {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			return i
		}
		i += n
	}
	return -1
}
