package library

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The oracle recomputes every rollup from the base tables with its own SQL and
// Go: it reads slot, title and slot_episode, never title_rollup's writers, and
// calls nothing from projection.go or slotstate.go (not even their state
// names or the mark function). A rollup is read as AD-06 pins it: one slot,
// that slot's own bytes (file rows are not an input), the earliest unaired
// instant, and the spine window described on oracleWindow.

// oracleQueryer is what a *sql.Tx and a *sql.DB offer to the oracle.
type oracleQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type slotKey struct{ title, slot int64 }

// oracleRollup is one rollup, recomputed or stored; revision is not part of it.
type oracleRollup struct {
	episodes, monitored, onDisk, missing, size int64
	next                                       sql.NullInt64
	spine                                      string
	full, omitted                              int64
}

func (r oracleRollup) String() string {
	next := "NULL"
	if r.next.Valid {
		next = fmt.Sprint(r.next.Int64)
	}
	return fmt.Sprintf("episodes=%d monitored=%d on_disk=%d missing=%d size=%d next_air=%s spine=%q full=%d omitted=%d",
		r.episodes, r.monitored, r.onDisk, r.missing, r.size, next, r.spine, r.full, r.omitted)
}

func oracleMark(state string, monitored bool) (byte, error) {
	switch {
	case state == "on_disk":
		return 'F', nil
	case state == "upgrade":
		return 'U', nil
	case state == "live":
		return 'L', nil
	case state == "missing" && monitored:
		return 'M', nil
	case state == "unaired" && monitored:
		return 'S', nil
	case state == "missing" || state == "unaired":
		return 'O', nil
	}
	return 0, fmt.Errorf("unknown state %q", state)
}

// oracleWindow picks the spine from the marks in order. At most 24 marks are
// shown, consecutive. The anchor is the first live mark, else the first
// monitored unaired one. With an anchor the window ends on it (anchor at
// position 23), or starts at the first mark when fewer than 24 precede it;
// with no anchor it is the last 24.
func oracleWindow(marks []byte) string {
	anchor := slices.Index(marks, 'L')
	if anchor < 0 {
		anchor = slices.Index(marks, 'S')
	}
	start := 0
	switch {
	case len(marks) <= 24:
	case anchor < 0:
		start = len(marks) - 24
	case anchor > 23:
		start = anchor - 23
	}
	return string(marks[start:min(len(marks), start+24)])
}

func oracleCollect(ctx context.Context, q oracleQueryer, query string, each func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// oracleRecompute returns what every slot's rollup must be.
func oracleRecompute(ctx context.Context, q oracleQueryer) (map[slotKey]oracleRollup, error) {
	type slotInfo struct {
		key       slotKey
		movie     bool
		monitored bool
		state     sql.NullString
		size      sql.NullInt64
	}
	slots := map[int64]*slotInfo{}
	err := oracleCollect(ctx, q, `SELECT s.id, s.title_id, t.kind, s.monitored, s.media_state, s.size_bytes
		FROM slot s JOIN title t ON t.id = s.title_id`, func(r *sql.Rows) error {
		var s slotInfo
		var kind string
		var mon int64
		if err := r.Scan(&s.key.slot, &s.key.title, &kind, &mon, &s.state, &s.size); err != nil {
			return err
		}
		s.movie, s.monitored = kind == "movie", mon == 1
		slots[s.key.slot] = &s
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make(map[slotKey]oracleRollup, len(slots))
	marks := map[int64][]byte{}
	acc := map[int64]*oracleRollup{}
	for id, s := range slots {
		acc[id] = &oracleRollup{}
		if s.movie {
			if !s.state.Valid || !s.size.Valid {
				return nil, fmt.Errorf("movie slot %d has no state", id)
			}
			m, err := oracleMark(s.state.String, s.monitored)
			if err != nil {
				return nil, fmt.Errorf("movie slot %d: %w", id, err)
			}
			marks[id] = []byte{m}
			a := acc[id]
			a.episodes, a.size = 1, s.size.Int64
			if s.monitored {
				a.monitored = 1
			}
			switch s.state.String {
			case "on_disk":
				a.onDisk = 1
			case "missing":
				a.missing = 1
			}
		}
	}
	err = oracleCollect(ctx, q, `SELECT slot_id, monitored, state, size_bytes, air_utc FROM slot_episode ORDER BY slot_id, ord`, func(r *sql.Rows) error {
		var id, mon, size int64
		var state string
		var air sql.NullInt64
		if err := r.Scan(&id, &mon, &state, &size, &air); err != nil {
			return err
		}
		s, ok := slots[id]
		if !ok || s.movie {
			return fmt.Errorf("slot_episode row for slot %d, which is not a series slot", id)
		}
		m, err := oracleMark(state, mon == 1)
		if err != nil {
			return fmt.Errorf("slot %d: %w", id, err)
		}
		marks[id] = append(marks[id], m)
		a := acc[id]
		a.episodes++
		a.monitored += mon
		a.size += size
		switch state {
		case "on_disk":
			a.onDisk++
		case "missing":
			a.missing++
		case "unaired":
			if air.Valid && (!a.next.Valid || air.Int64 < a.next.Int64) {
				a.next = air
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id, s := range slots {
		a := acc[id]
		a.spine = oracleWindow(marks[id])
		a.full = int64(len(marks[id]))
		a.omitted = a.full - int64(len(a.spine))
		out[s.key] = *a
	}
	return out, nil
}

// oracleStored returns the stored rollups; it does not repair or add a row.
func oracleStored(ctx context.Context, q oracleQueryer) (map[slotKey]oracleRollup, error) {
	out := map[slotKey]oracleRollup{}
	err := oracleCollect(ctx, q, `SELECT title_id, slot_id, episodes, monitored, on_disk, missing, size_bytes, next_air_utc,
		spine, spine_full, spine_omitted FROM title_rollup`, func(r *sql.Rows) error {
		var k slotKey
		var v oracleRollup
		if err := r.Scan(&k.title, &k.slot, &v.episodes, &v.monitored, &v.onDisk, &v.missing, &v.size, &v.next, &v.spine, &v.full, &v.omitted); err != nil {
			return err
		}
		out[k] = v
		return nil
	})
	return out, err
}

// oracleCheck compares the stored rollups with the recompute: one stored row
// per slot, none more, every value equal. It reports up to eight differences.
func oracleCheck(ctx context.Context, q oracleQueryer) error {
	want, err := oracleRecompute(ctx, q)
	if err != nil {
		return fmt.Errorf("oracle recompute: %w", err)
	}
	got, err := oracleStored(ctx, q)
	if err != nil {
		return fmt.Errorf("oracle read of stored rollups: %w", err)
	}
	var diffs []string
	note := func(format string, args ...any) { diffs = append(diffs, fmt.Sprintf(format, args...)) }
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok:
			note("title %d slot %d has no rollup row; want %v", k.title, k.slot, w)
		case g != w:
			note("title %d slot %d rollup\n\t got %v\n\twant %v", k.title, k.slot, g, w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			note("rollup row for title %d slot %d matches no slot", k.title, k.slot)
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	slices.Sort(diffs)
	total, more := len(diffs), ""
	if total > 8 {
		more, diffs = fmt.Sprintf("\n(+%d more)", total-8), diffs[:8]
	}
	return fmt.Errorf("%d rollups differ from the full recompute:\n%s%s", total, strings.Join(diffs, "\n"), more)
}

// requireOracle fails unless the store's rollups equal the recompute inside one snapshot.
func requireOracle(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.Read(t.Context(), func(tx *sql.Tx) error { return oracleCheck(t.Context(), tx) }); err != nil {
		t.Fatal(err)
	}
}
