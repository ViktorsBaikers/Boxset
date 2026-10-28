package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// This file is the only writer of slot_episode rows and movie-slot state,
// including the rows a new slot starts with.

const (
	maxEpisodesPerCall = 20000
	maxSeason          = 9999
	maxNumber          = 99999
)

// Media states of a movie slot or a slot_episode row.
const (
	stateMissing = "missing"
	stateOnDisk  = "on_disk"
	stateUpgrade = "upgrade"
	stateLive    = "live"
	stateUnaired = "unaired"
)

// EpisodeSpec is one episode of a series. AirUTC is set exactly when
// AirPrecision is "instant"; "date" and "unknown" carry no instant.
type EpisodeSpec struct {
	Season       int
	Number       int
	AirUTC       *int64
	AirPrecision string
}

// SlotEpisodeChange sets the state of one slot's row for the episode (Season, Number).
type SlotEpisodeChange struct {
	SlotID    SlotID
	Season    int
	Number    int
	Monitored bool
	State     string
	SizeBytes int64
}

// MovieSlotChange sets the state of a movie slot.
type MovieSlotChange struct {
	SlotID    SlotID
	State     string
	SizeBytes int64
}

// AddEpisodes appends episodes to a series in order and gives every slot of
// the title a missing row for each new one, in the same transaction. An
// episode that already exists by (season, number) is left as it is; its air
// changes through SetEpisodeAir.
func (l *Library) AddEpisodes(ctx context.Context, id TitleID, eps []EpisodeSpec) error {
	if len(eps) == 0 || len(eps) > maxEpisodesPerCall {
		return invalid("episodes", "must be 1-"+strconv.Itoa(maxEpisodesPerCall))
	}
	for _, e := range eps {
		if err := e.validate(); err != nil {
			return err
		}
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM title WHERE id = ?`, int64(id)).Scan(&kind); err != nil {
			return notFound(err, "title "+strconv.FormatInt(int64(id), 10))
		}
		if kind != kindSeries {
			return invalid("title", strconv.FormatInt(int64(id), 10)+" is not a series")
		}
		var lastID, ord int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) FROM episode`).Scan(&lastID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(ord), 0) FROM episode WHERE title_id = ?`, int64(id)).Scan(&ord); err != nil {
			return err
		}
		for _, e := range eps {
			res, err := tx.ExecContext(ctx, `INSERT INTO episode (title_id, ord, season, number, air_utc, air_precision)
				VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (title_id, season, number) DO NOTHING`,
				int64(id), ord+1, e.Season, e.Number, e.AirUTC, e.AirPrecision)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 1 {
				ord++
			}
		}
		return fillEpisodeRows(ctx, tx, int64(id), lastID)
	})
}

// SetEpisodeAir sets the air instant and precision of the existing episode
// (e.Season, e.Number) of a series; e is validated like an AddEpisodes spec.
// The episode row and the slot_episode row and rollup of every slot of the
// title change in one transaction. A call that stores the same air writes
// nothing and moves no revision.
func (l *Library) SetEpisodeAir(ctx context.Context, id TitleID, e EpisodeSpec) error {
	if err := e.validate(); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		var episodeID int64
		var air sql.NullInt64
		var precision string
		err := tx.QueryRowContext(ctx, qEpisodeAir, int64(id), e.Season, e.Number).Scan(&episodeID, &air, &precision)
		if err != nil {
			return notFound(err, fmt.Sprintf("title %d episode S%dE%d", id, e.Season, e.Number))
		}
		if precision == e.AirPrecision && (e.AirUTC == nil || air.Int64 == *e.AirUTC) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, qEpisodeAirSet, e.AirUTC, e.AirPrecision, episodeID); err != nil {
			return err
		}
		slots, err := titleSlots(ctx, tx, int64(id))
		if err != nil {
			return err
		}
		for _, s := range slots {
			if _, err := tx.ExecContext(ctx, qSlotEpisodeAirSet, e.AirUTC, s.id, episodeID); err != nil {
				return err
			}
			if err := applyRollup(ctx, tx, int64(id), s.id, counts{}); err != nil {
				return err
			}
		}
		return nil
	})
}

// The slot-state statements; TestIndexedQueryPlans reads their plans.
const (
	qSlotEpisodeRow = `SELECT se.title_id, se.episode_id, se.monitored, se.state, se.size_bytes
		FROM slot s JOIN episode e ON e.title_id = s.title_id JOIN slot_episode se ON se.slot_id = s.id AND se.episode_id = e.id
		WHERE s.id = ? AND e.season = ? AND e.number = ?`
	qSlotEpisodeSet = `UPDATE slot_episode SET monitored = ?, state = ?, size_bytes = ? WHERE slot_id = ? AND episode_id = ?`
	qMovieRow       = `SELECT s.title_id, s.monitored, s.media_state, COALESCE(s.size_bytes, 0)
		FROM slot s JOIN title t ON t.id = s.title_id WHERE s.id = ? AND t.kind = 'movie'`
	qMovieSet = `UPDATE slot SET media_state = ?, size_bytes = ? WHERE id = ?`
	// The air statements: one episode by its key, its row, and each slot's copy of the air instant.
	qEpisodeAir        = `SELECT id, air_utc, air_precision FROM episode WHERE title_id = ? AND season = ? AND number = ?`
	qEpisodeAirSet     = `UPDATE episode SET air_utc = ?, air_precision = ? WHERE id = ?`
	qSlotEpisodeAirSet = `UPDATE slot_episode SET air_utc = ? WHERE slot_id = ? AND episode_id = ?`
)

// SetSlotEpisode sets monitoring, state and size of one slot's episode row. Other slots are untouched.
// A call that changes nothing writes nothing.
func (l *Library) SetSlotEpisode(ctx context.Context, c SlotEpisodeChange) error {
	if err := checkState(c.State, c.SizeBytes); err != nil {
		return err
	}
	if c.Season < 0 || c.Season > maxSeason || c.Number < 0 || c.Number > maxNumber {
		return invalid("episode", "season or number out of range")
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error { return setSlotEpisode(ctx, tx, c) })
}

// setSlotEpisode is SetSlotEpisode inside the caller's transaction: the row
// and its slot's rollup change together.
func setSlotEpisode(ctx context.Context, tx *sql.Tx, c SlotEpisodeChange) error {
	var titleID, episodeID, monitored, size int64
	var state string
	err := tx.QueryRowContext(ctx, qSlotEpisodeRow, int64(c.SlotID), c.Season, c.Number).
		Scan(&titleID, &episodeID, &monitored, &state, &size)
	if err != nil {
		return notFound(err, fmt.Sprintf("slot %d episode S%dE%d", c.SlotID, c.Season, c.Number))
	}
	if monitored == int64(b2i(c.Monitored)) && state == c.State && size == c.SizeBytes {
		return nil
	}
	if _, err := tx.ExecContext(ctx, qSlotEpisodeSet,
		b2i(c.Monitored), c.State, c.SizeBytes, int64(c.SlotID), episodeID); err != nil {
		return err
	}
	delta := markCounts(int64(b2i(c.Monitored)), c.State, c.SizeBytes).minus(markCounts(monitored, state, size))
	return applyRollup(ctx, tx, titleID, int64(c.SlotID), delta)
}

// SetMovieSlot sets the media state and size of a movie slot. A call that
// changes nothing writes nothing.
func (l *Library) SetMovieSlot(ctx context.Context, c MovieSlotChange) error {
	if err := checkState(c.State, c.SizeBytes); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error { return setMovieSlot(ctx, tx, c) })
}

// setMovieSlot is SetMovieSlot inside the caller's transaction.
func setMovieSlot(ctx context.Context, tx *sql.Tx, c MovieSlotChange) error {
	var titleID, monitored, size int64
	var state sql.NullString
	err := tx.QueryRowContext(ctx, qMovieRow, int64(c.SlotID)).
		Scan(&titleID, &monitored, &state, &size)
	if err != nil {
		return notFound(err, "movie slot "+strconv.FormatInt(int64(c.SlotID), 10))
	}
	if state.String == c.State && size == c.SizeBytes {
		return nil
	}
	if _, err := tx.ExecContext(ctx, qMovieSet, c.State, c.SizeBytes, int64(c.SlotID)); err != nil {
		return err
	}
	delta := markCounts(monitored, c.State, c.SizeBytes)
	if state.Valid { // a slot that never had a state contributes nothing yet
		delta = delta.minus(markCounts(monitored, state.String, size))
	}
	return applyRollup(ctx, tx, titleID, int64(c.SlotID), delta)
}

// initSlotState starts a new slot: a movie slot is missing, a series slot gets
// one row per existing episode. The slot's rollup row already exists (the slot
// trigger made it); this brings it up to the rows written here.
func initSlotState(ctx context.Context, tx *sql.Tx, slotID, titleID int64, kind string) error {
	var monitored int64
	if err := tx.QueryRowContext(ctx, `SELECT monitored FROM slot WHERE id = ?`, slotID).Scan(&monitored); err != nil {
		return err
	}
	if kind == kindMovie {
		if _, err := tx.ExecContext(ctx, `UPDATE slot SET media_state = 'missing', size_bytes = 0 WHERE id = ?`, slotID); err != nil {
			return err
		}
		return applyRollup(ctx, tx, titleID, slotID, markCounts(monitored, stateMissing, 0))
	}
	return fillSlotEpisodes(ctx, tx, titleID, slotID, monitored, 0)
}

// slotMonitor is a slot of a title with its monitoring.
type slotMonitor struct{ id, monitored int64 }

// titleSlots lists the slots of the title.
func titleSlots(ctx context.Context, tx *sql.Tx, titleID int64) ([]slotMonitor, error) {
	var slots []slotMonitor
	rows, err := tx.QueryContext(ctx, `SELECT id, monitored FROM slot WHERE title_id = ?`, titleID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s slotMonitor
		if err := rows.Scan(&s.id, &s.monitored); err != nil {
			_ = rows.Close()
			return nil, err
		}
		slots = append(slots, s)
	}
	return slots, errors.Join(rows.Err(), rows.Close())
}

// fillEpisodeRows gives every slot of the title a missing row for each
// episode with id > afterEpisodeID.
func fillEpisodeRows(ctx context.Context, tx *sql.Tx, titleID, afterEpisodeID int64) error {
	slots, err := titleSlots(ctx, tx, titleID)
	if err != nil {
		return err
	}
	for _, s := range slots {
		if err := fillSlotEpisodes(ctx, tx, titleID, s.id, s.monitored, afterEpisodeID); err != nil {
			return err
		}
	}
	return nil
}

// fillSlotEpisodes inserts a missing, unsized row into slotID for every
// episode of its title with id > afterEpisodeID and adds those rows to the
// slot's rollup. Rows start monitored as their slot is.
func fillSlotEpisodes(ctx context.Context, tx *sql.Tx, titleID, slotID, monitored, afterEpisodeID int64) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO slot_episode (slot_id, episode_id, title_id, ord, monitored, state, size_bytes, air_utc)
		SELECT s.id, e.id, e.title_id, e.ord, s.monitored, 'missing', 0, e.air_utc
		FROM slot s JOIN episode e ON e.title_id = s.title_id
		WHERE s.id = ? AND e.id > ?`, slotID, afterEpisodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return applyRollup(ctx, tx, titleID, slotID, counts{episodes: n, monitored: n * monitored, missing: n})
}

func checkState(state string, size int64) error {
	switch state {
	case stateMissing, stateOnDisk, stateUpgrade, stateLive, stateUnaired:
	default:
		return invalid("state", "must be missing, on_disk, upgrade, live or unaired")
	}
	if size < 0 {
		return invalid("size_bytes", "is negative")
	}
	return nil
}

func (e EpisodeSpec) validate() error {
	if e.Season < 0 || e.Season > maxSeason || e.Number < 0 || e.Number > maxNumber {
		return invalid("episode", "season or number out of range")
	}
	switch e.AirPrecision {
	case "instant":
		if e.AirUTC == nil {
			return invalid("episode air_utc", "is required for instant precision")
		}
	case "date", "unknown":
		if e.AirUTC != nil {
			return invalid("episode air_utc", "is set without instant precision")
		}
	default:
		return invalid("episode air_precision", "must be instant, date or unknown")
	}
	return nil
}
