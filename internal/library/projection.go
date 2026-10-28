package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Rollups and spines. A rollup row describes one (title, slot): counts, that
// slot's own bytes, the next airing and the spine window. The slot-state
// writers in slotstate.go change a rollup in the transaction that changes the
// rows it summarizes, by the delta of the mark that changed; nothing here
// reads a rollup to repair it.

// ErrTitleHasFiles is returned by DeleteTitle when a file or download_slot row
// still references a slot of the title. The title and its rollups stay.
var ErrTitleHasFiles = errors.New("library: title has file or download rows")

// Rollup is the stored projection of one slot. For a series slot every
// slot_episode row is one mark; a movie slot is one mark. NextAirUTC is the
// earliest unaired instant of a series slot, nil when there is none. Spine
// holds at most 24 marks (F on disk, U upgrade, M missing, L live, S unaired,
// O unmonitored); SpineFull counts all marks and SpineOmitted those left out.
// Revision moves only when a stored value changes.
type Rollup struct {
	TitleID      TitleID
	SlotID       SlotID
	Episodes     int64
	Monitored    int64
	OnDisk       int64
	Missing      int64
	SizeBytes    int64
	NextAirUTC   *int64
	Spine        string
	SpineFull    int64
	SpineOmitted int64
	Revision     int64
}

const spineMarks = 24

// The projection queries. Each reads through an index and builds no sorter
// (TestIndexedQueryPlans); the literals select the partial indexes.
const (
	qRollupRow = `SELECT r.episodes, r.monitored, r.on_disk, r.missing, r.size_bytes, r.next_air_utc, r.spine,
		r.spine_full, r.spine_omitted, t.kind, s.monitored, s.media_state
		FROM title_rollup r JOIN slot s ON s.id = r.slot_id JOIN title t ON t.id = r.title_id
		WHERE r.title_id = ? AND r.slot_id = ?`
	qRollupSet = `UPDATE title_rollup SET episodes = ?, monitored = ?, on_disk = ?, missing = ?, size_bytes = ?, next_air_utc = ?,
		spine = ?, spine_full = ?, spine_omitted = ?, revision = revision + 1 WHERE title_id = ? AND slot_id = ?`
	qRollups = `SELECT title_id, slot_id, episodes, monitored, on_disk, missing, size_bytes, next_air_utc, spine,
		spine_full, spine_omitted, revision FROM title_rollup WHERE title_id = ? ORDER BY slot_id`
	qLiveAnchor    = `SELECT ord FROM slot_episode WHERE slot_id = ? AND state = 'live' ORDER BY ord LIMIT 1`
	qUnairedAnchor = `SELECT ord FROM slot_episode WHERE slot_id = ? AND state = 'unaired' AND monitored = 1 ORDER BY ord LIMIT 1`
	qNextAir       = `SELECT air_utc FROM slot_episode WHERE slot_id = ? AND state = 'unaired' AND air_utc IS NOT NULL ORDER BY air_utc LIMIT 1`
	qWindowTo      = `SELECT state, monitored FROM slot_episode WHERE slot_id = ? AND ord <= ? ORDER BY ord DESC LIMIT 24`
	qWindowFirst   = `SELECT state, monitored FROM slot_episode WHERE slot_id = ? ORDER BY ord LIMIT 24`
	qTitleKept     = `SELECT EXISTS (SELECT 1 FROM slot s WHERE s.title_id = ? AND
		(EXISTS (SELECT 1 FROM file f WHERE f.slot_id = s.id) OR EXISTS (SELECT 1 FROM download_slot d WHERE d.slot_id = s.id)))`
)

// counts is what marks add to a rollup: a delta when it is applied.
type counts struct{ episodes, monitored, onDisk, missing, size int64 }

// markCounts is the contribution of one mark.
func markCounts(monitored int64, state string, size int64) counts {
	c := counts{episodes: 1, monitored: monitored, size: size}
	switch state {
	case stateOnDisk:
		c.onDisk = 1
	case stateMissing:
		c.missing = 1
	}
	return c
}

func (c counts) plus(d counts) counts {
	return counts{c.episodes + d.episodes, c.monitored + d.monitored, c.onDisk + d.onDisk, c.missing + d.missing, c.size + d.size}
}

func (c counts) minus(d counts) counts {
	return counts{c.episodes - d.episodes, c.monitored - d.monitored, c.onDisk - d.onDisk, c.missing - d.missing, c.size - d.size}
}

// mark is the spine letter of one slot_episode row or movie slot. A row that
// is on disk, wanted for upgrade or live keeps its letter whether or not it is
// monitored; a missing or unaired one reads O when it is not monitored.
func mark(state string, monitored int64) byte {
	switch state {
	case stateOnDisk:
		return 'F'
	case stateUpgrade:
		return 'U'
	case stateLive:
		return 'L'
	}
	if monitored == 0 {
		return 'O'
	}
	if state == stateUnaired {
		return 'S'
	}
	return 'M'
}

// applyRollup adds delta to the rollup of (titleID, slotID), recomputes the
// next airing and the spine from the slot's rows, and writes the row, with its
// revision moved, only when a stored value differs. It runs after the base
// rows changed, inside the same transaction.
func applyRollup(ctx context.Context, tx *sql.Tx, titleID, slotID int64, delta counts) error {
	var cur counts
	var next sql.NullInt64
	var spine, kind string
	var full, omitted, slotMonitored int64
	var movieState sql.NullString
	err := tx.QueryRowContext(ctx, qRollupRow, titleID, slotID).Scan(&cur.episodes, &cur.monitored, &cur.onDisk, &cur.missing,
		&cur.size, &next, &spine, &full, &omitted, &kind, &slotMonitored, &movieState)
	if err != nil {
		return notFound(err, fmt.Sprintf("rollup of slot %d", slotID))
	}
	want := cur.plus(delta)
	var wantNext sql.NullInt64
	var wantSpine string
	if kind == kindMovie {
		wantSpine = string(mark(movieState.String, slotMonitored))
	} else if wantNext, wantSpine, err = seriesProjection(ctx, tx, slotID); err != nil {
		return err
	}
	wantOmitted := want.episodes - int64(len(wantSpine))
	if want == cur && wantNext == next && wantSpine == spine && want.episodes == full && wantOmitted == omitted {
		return nil
	}
	_, err = tx.ExecContext(ctx, qRollupSet, want.episodes, want.monitored, want.onDisk, want.missing, want.size, wantNext,
		wantSpine, want.episodes, wantOmitted, titleID, slotID)
	return err
}

// seriesProjection reads a series slot's next airing and spine window: one
// indexed read each for the anchor, the earliest unaired instant and the
// window of at most 24 marks. The anchor is the first live mark, else the
// first monitored unaired one. The window ends on the anchor, so the anchor is
// its last mark, unless fewer than 24 marks precede it, when the window is the
// first 24. With no anchor it is the last 24.
func seriesProjection(ctx context.Context, tx *sql.Tx, slotID int64) (next sql.NullInt64, spine string, err error) {
	if err = scanOne(tx.QueryRowContext(ctx, qNextAir, slotID), &next); err != nil {
		return next, "", err
	}
	anchor := int64(1<<63 - 1) // no anchor: the window ends at the last mark
	for _, q := range []string{qLiveAnchor, qUnairedAnchor} {
		var ord sql.NullInt64
		if err = scanOne(tx.QueryRowContext(ctx, q, slotID), &ord); err != nil {
			return next, "", err
		}
		if ord.Valid {
			anchor = ord.Int64
			break
		}
	}
	marks, err := windowMarks(ctx, tx, qWindowTo, slotID, anchor)
	if err != nil {
		return next, "", err
	}
	if len(marks) < spineMarks {
		// Fewer than 24 marks end at the anchor (or fewer exist): the window is the first 24.
		if marks, err = windowMarks(ctx, tx, qWindowFirst, slotID); err != nil {
			return next, "", err
		}
	} else {
		for i, j := 0, len(marks)-1; i < j; i, j = i+1, j-1 { // read newest first
			marks[i], marks[j] = marks[j], marks[i]
		}
	}
	return next, string(marks), nil
}

// scanOne scans a single-column row that may be absent; absence leaves dest NULL.
func scanOne(row *sql.Row, dest *sql.NullInt64) error {
	err := row.Scan(dest)
	if errors.Is(err, sql.ErrNoRows) {
		*dest = sql.NullInt64{}
		return nil
	}
	return err
}

func windowMarks(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	marks := make([]byte, 0, spineMarks)
	for rows.Next() {
		var state string
		var monitored int64
		if err := rows.Scan(&state, &monitored); err != nil {
			return nil, err
		}
		marks = append(marks, mark(state, monitored))
	}
	return marks, rows.Err()
}

// Rollups returns the stored rollup of every slot of the title, by slot id. It
// only reads: a stored value that disagrees with the slot rows stays as it is.
func (l *Library) Rollups(ctx context.Context, id TitleID) ([]Rollup, error) {
	var out []Rollup
	err := l.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, qRollups, int64(id))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r Rollup
			var next sql.NullInt64
			if err := rows.Scan(&r.TitleID, &r.SlotID, &r.Episodes, &r.Monitored, &r.OnDisk, &r.Missing, &r.SizeBytes, &next,
				&r.Spine, &r.SpineFull, &r.SpineOmitted, &r.Revision); err != nil {
				return err
			}
			if next.Valid {
				r.NextAirUTC = &next.Int64
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteTitle deletes the title with its slots, episodes, rollups, holds and
// memberships. It refuses with ErrTitleHasFiles while a file or download_slot
// row references one of its slots, and with ErrNotFound for an unknown title.
func (l *Library) DeleteTitle(ctx context.Context, id TitleID) error {
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM title WHERE id = ?`, int64(id)).Scan(&one); err != nil {
			return notFound(err, "title "+strconv.FormatInt(int64(id), 10))
		}
		var kept bool
		if err := tx.QueryRowContext(ctx, qTitleKept, int64(id)).Scan(&kept); err != nil {
			return err
		}
		if kept {
			return fmt.Errorf("%w: title %d", ErrTitleHasFiles, id)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM title WHERE id = ?`, int64(id))
		return err
	})
}
