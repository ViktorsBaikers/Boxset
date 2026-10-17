package parse

import (
	"unicode/utf8"
)

// Fixed parser limits. Names and budgets are validated before any pattern is
// matched; a limit breach is a structured reason, never a truncation.
const (
	// maxNameBytes bounds a release name: UTF-8 bytes, 4096 admitted, 4097 rejected.
	maxNameBytes = 4096

	// codeFormat limits are owned by later format work; only the fixed
	// reason vocabulary used by quality parsing is declared here.
	codeReleaseNameTooLong     = "release_name_too_long"
	codeReleaseNameInvalidUTF8 = "release_name_invalid_utf8"
	codeKindUnsupported        = "kind_unsupported"
	codeRegexCompileFailed     = "regex_compile_failed"
	codeMatchTimeout           = "match_timeout"
	codeRegexStackLimit        = "regex_stack_limit"
	codeWorkBudgetExhausted    = "release_work_budget_exhausted"
	codeDeadlineExceeded       = "decision_deadline_exceeded"
	codeCancelled              = "evaluation_cancelled"
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
