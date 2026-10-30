package outbound

import "context"

// Locate decides whether a mutation that may or may not have landed is already
// applied, by reading the peer's own identity list. It decides nothing before the
// walk is complete: one identity seen early may appear again on a later page, and
// a repeated cursor or a page cap reached with more to read is an unfinished walk,
// never an absence.
type Presence int

const (
	// PresenceUnknown is the zero value: the walk did not finish, so nothing is
	// decided and the caller must not resend.
	PresenceUnknown Presence = iota
	// PresenceAbsent: the complete walk held no such identity, so the mutation
	// never landed.
	PresenceAbsent
	// PresencePresent: the complete walk held exactly one such identity, so the
	// mutation landed.
	PresencePresent
	// PresenceAmbiguous: the walk held the identity more than once, so this client
	// cannot tell which effect is the one it is asking about.
	PresenceAmbiguous
)

// Page is one decoded page of the peer's identities: the non-secret item
// identities it held, and the cursor of the page after it. The first fetch is
// called with "", and an empty Next is the last page.
type Page struct {
	Identities []string
	Next       string
}

// Locate walks the peer's pages and decides identity's presence once, after the
// whole bounded walk. fetch performs one page read — normally a safe read through
// Do, so the walk spends that read's own budget — and maxPages bounds how many
// pages this walk may read. A fetch error or a cancelled context leaves the
// presence unknown with the error; a repeated cursor or the page cap reached with
// more to read leaves it unknown with nothing decided.
func Locate(ctx context.Context, fetch func(context.Context, string) (Page, error), identity string, maxPages int) (Presence, error) {
	if maxPages < 1 {
		return PresenceUnknown, nil
	}
	cursor := ""
	seen := make(map[string]struct{}, maxPages)
	found := 0
	for pages := 0; pages < maxPages; pages++ {
		if err := ctx.Err(); err != nil {
			return PresenceUnknown, err
		}
		page, err := fetch(ctx, cursor)
		if err != nil {
			return PresenceUnknown, err
		}
		for _, held := range page.Identities {
			if held == identity {
				found++
			}
		}
		if page.Next == "" {
			switch {
			case found == 0:
				return PresenceAbsent, nil
			case found == 1:
				return PresencePresent, nil
			}
			return PresenceAmbiguous, nil
		}
		// A cursor this walk already followed would read the same page forever:
		// the walk cannot finish, so nothing is decided.
		if _, repeated := seen[page.Next]; repeated {
			return PresenceUnknown, nil
		}
		seen[page.Next] = struct{}{}
		cursor = page.Next
	}
	return PresenceUnknown, nil
}
