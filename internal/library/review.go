package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ReasonCode says why an acquisition for a slot did not proceed.
type ReasonCode string

const (
	// ReasonIdentityConflict: an external identity would merge or move. The
	// hold is scoped to the conflicting source, or to a slot or title.
	ReasonIdentityConflict ReasonCode = "identity_conflict"
	// ReasonRestrictionEmptyIntersection: the indexer allowlists that apply to
	// the slot share no indexer.
	ReasonRestrictionEmptyIntersection ReasonCode = "restriction_empty_intersection"
	// ReasonOwnershipUnresolved: no owner is decided for the slot. Boxset never guesses one.
	ReasonOwnershipUnresolved ReasonCode = "ownership_unresolved"
	// ReasonSlotRemoteOwned: a connected instance owns the slot, so it is never
	// acquired locally. This is a refusal; it has no hold.
	ReasonSlotRemoteOwned ReasonCode = "slot_remote_owned"
)

// Pause is the structured outcome for one slot that was not acquired. SlotID
// is 0 for the identity conflict an import reports: it is about the source;
// its hold names the affected slot or title, so acquiring that slot pauses.
type Pause struct {
	SlotID SlotID
	Reason ReasonCode
	Params map[string]string
}

// Hold is one stored review_hold row. TitleID and SlotID are 0 when the hold
// names neither; a hold with only a TitleID affects every slot of the title.
type Hold struct {
	ID       int64
	Reason   ReasonCode
	ScopeKey string
	TitleID  TitleID
	SlotID   SlotID
	Params   map[string]string
}

const maxIndexers = 64

// SetLibraryIndexers replaces the indexer allowlist of a library. An empty
// list removes the restriction.
func (l *Library) SetLibraryIndexers(ctx context.Context, id LibraryID, indexers []string) error {
	keys, err := checkIndexers(indexers)
	if err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM library WHERE id = ?`, int64(id)).Scan(&one); err != nil {
			return notFound(err, "library "+strconv.FormatInt(int64(id), 10))
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM library_indexer WHERE library_id = ?`, int64(id)); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `INSERT INTO library_indexer (library_id, indexer_key) VALUES (?, ?)`, int64(id), k); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetSlotIndexers replaces the indexer allowlist of a slot. An empty list
// removes the restriction.
func (l *Library) SetSlotIndexers(ctx context.Context, id SlotID, indexers []string) error {
	keys, err := checkIndexers(indexers)
	if err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		if err := needSlot(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM slot_indexer WHERE slot_id = ?`, int64(id)); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `INSERT INTO slot_indexer (slot_id, indexer_key) VALUES (?, ?)`, int64(id), k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Holds returns the holds scoped to a title or to one of its slots, oldest first.
func (l *Library) Holds(ctx context.Context, title TitleID) ([]Hold, error) {
	var out []Hold
	err := l.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, reason_code, scope_key, COALESCE(title_id, 0), COALESCE(slot_id, 0), params
			FROM review_hold WHERE title_id = ? ORDER BY id`, int64(title))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var h Hold
			var params string
			if err := rows.Scan(&h.ID, &h.Reason, &h.ScopeKey, &h.TitleID, &h.SlotID, &params); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(params), &h.Params); err != nil {
				return fmt.Errorf("library: hold %d params: %w", h.ID, err)
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// putHold records a hold once per (reason, scope): a replay inserts nothing.
// title and slot are 0 for a hold scoped to neither.
func putHold(ctx context.Context, tx *sql.Tx, reason ReasonCode, scope string, title TitleID, slot SlotID, params map[string]string) error {
	body, err := json.Marshal(params) // map keys marshal sorted, so the stored text is canonical
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_hold (reason_code, scope_key, title_id, slot_id, params)
		VALUES (?, ?, NULLIF(?, 0), NULLIF(?, 0), ?) ON CONFLICT (reason_code, scope_key) DO NOTHING`,
		string(reason), scope, int64(title), int64(slot), string(body))
	return err
}

// slotRow is what the pause guard needs to know about a requested slot.
type slotRow struct {
	id         SlotID
	title      TitleID
	key        string
	owner      string
	metadataID string
}

// loadSlot returns ErrNotFound when no slot has the id.
func loadSlot(ctx context.Context, tx *sql.Tx, id SlotID) (slotRow, error) {
	s := slotRow{id: id}
	err := tx.QueryRowContext(ctx, `SELECT s.title_id, s.slot_key, s.owner, t.metadata_id
		FROM slot s JOIN title t ON t.id = s.title_id WHERE s.id = ?`, int64(id)).Scan(&s.title, &s.key, &s.owner, &s.metadataID)
	return s, notFound(err, fmt.Sprintf("slot %d", id))
}

// evaluateSlot decides whether a slot may be acquired. It returns nil for an
// eligible slot and otherwise the Pause, in this order: remote owner (a
// refusal, no hold), unresolved owner, an existing identity_conflict hold on
// the slot or its title, an empty indexer intersection. Every hold it writes
// is deduplicated, so a replay adds none. It never changes a title, slot or owner.
func evaluateSlot(ctx context.Context, tx *sql.Tx, s slotRow) (*Pause, error) {
	params := map[string]string{"metadata_id": s.metadataID, "slot_key": s.key}
	scope := "slot:" + strconv.FormatInt(int64(s.id), 10)
	switch s.owner {
	case OwnerLocal:
	case OwnerUnresolved:
		params["owner"] = OwnerUnresolved
		return holdSlot(ctx, tx, s, ReasonOwnershipUnresolved, scope, params)
	default:
		params["owner"] = s.owner
		return &Pause{SlotID: s.id, Reason: ReasonSlotRemoteOwned, Params: params}, nil
	}

	var held string
	err := tx.QueryRowContext(ctx, `SELECT scope_key FROM review_hold WHERE reason_code = ?
		AND (slot_id = ? OR (title_id = ? AND slot_id IS NULL)) ORDER BY id LIMIT 1`,
		string(ReasonIdentityConflict), int64(s.id), int64(s.title)).Scan(&held)
	switch {
	case err == nil:
		params["hold_scope"] = held
		return &Pause{SlotID: s.id, Reason: ReasonIdentityConflict, Params: params}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	restricting, empty, err := indexerIntersection(ctx, tx, s)
	if err != nil {
		return nil, err
	}
	if empty {
		params["restrictions"] = restricting
		return holdSlot(ctx, tx, s, ReasonRestrictionEmptyIntersection, scope, params)
	}
	return nil, nil
}

func holdSlot(ctx context.Context, tx *sql.Tx, s slotRow, reason ReasonCode, scope string, params map[string]string) (*Pause, error) {
	if err := putHold(ctx, tx, reason, scope, s.title, s.id, params); err != nil {
		return nil, err
	}
	return &Pause{SlotID: s.id, Reason: reason, Params: params}, nil
}

// indexerIntersection intersects every restricting allowlist that applies to
// the slot: the allowlist of each library that has the slot's title and
// selects the slot's key in its template, and the slot's own. A library or
// slot without rows does not restrict. restricting names the lists that
// restrict, sorted; empty is true when there is at least one and they share no
// indexer. With no restricting list the slot is unrestricted and empty is false.
func indexerIntersection(ctx context.Context, tx *sql.Tx, s slotRow) (restricting string, empty bool, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT 'library:' || li.library_id, li.indexer_key FROM library_indexer li
		JOIN library_title lt ON lt.library_id = li.library_id AND lt.title_id = ?
		JOIN library_template tpl ON tpl.library_id = li.library_id AND tpl.slot_key = ?
		UNION ALL SELECT 'slot', indexer_key FROM slot_indexer WHERE slot_id = ?`,
		int64(s.title), s.key, int64(s.id))
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	lists := map[string]bool{}
	perIndexer := map[string]int{} // (list, indexer) is unique, so this counts the lists naming the indexer
	for rows.Next() {
		var list, indexer string
		if err := rows.Scan(&list, &indexer); err != nil {
			return "", false, err
		}
		lists[list] = true
		perIndexer[indexer]++
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(lists) == 0 {
		return "", false, nil
	}
	restricting = strings.Join(slices.Sorted(maps.Keys(lists)), ",")
	for _, n := range perIndexer {
		if n == len(lists) {
			return restricting, false, nil
		}
	}
	return restricting, true, nil
}

// checkIndexers validates an allowlist and returns it sorted and deduplicated.
func checkIndexers(indexers []string) ([]string, error) {
	if len(indexers) > maxIndexers {
		return nil, invalid("indexers", fmt.Sprintf("must be at most %d", maxIndexers))
	}
	for _, k := range indexers {
		if err := checkToken("indexer_key", k, maxText, false, "._-"); err != nil {
			return nil, err
		}
	}
	keys := slices.Clone(indexers)
	slices.Sort(keys)
	return slices.Compact(keys), nil
}
