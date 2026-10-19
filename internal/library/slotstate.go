package library

import (
	"context"
	"database/sql"
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
// episode that already exists by (season, number) is left as it is.
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
		return fillSlotEpisodes(ctx, tx, int64(id), 0, lastID)
	})
}

// SetSlotEpisode sets monitoring, state and size of one slot's episode row. Other slots are untouched.
func (l *Library) SetSlotEpisode(ctx context.Context, c SlotEpisodeChange) error {
	if err := checkState(c.State, c.SizeBytes); err != nil {
		return err
	}
	if c.Season < 0 || c.Season > maxSeason || c.Number < 0 || c.Number > maxNumber {
		return invalid("episode", "season or number out of range")
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE slot_episode SET monitored = ?, state = ?, size_bytes = ?
			WHERE slot_id = ? AND episode_id = (SELECT e.id FROM episode e JOIN slot s ON s.title_id = e.title_id
				WHERE s.id = ? AND e.season = ? AND e.number = ?)`,
			b2i(c.Monitored), c.State, c.SizeBytes, int64(c.SlotID), int64(c.SlotID), c.Season, c.Number)
		return needRow(res, err, fmt.Sprintf("slot %d episode S%dE%d", c.SlotID, c.Season, c.Number))
	})
}

// SetMovieSlot sets the media state and size of a movie slot.
func (l *Library) SetMovieSlot(ctx context.Context, c MovieSlotChange) error {
	if err := checkState(c.State, c.SizeBytes); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE slot SET media_state = ?, size_bytes = ?
			WHERE id = ? AND title_id IN (SELECT id FROM title WHERE kind = 'movie')`,
			c.State, c.SizeBytes, int64(c.SlotID))
		return needRow(res, err, "movie slot "+strconv.FormatInt(int64(c.SlotID), 10))
	})
}

// initSlotState starts a new slot: a movie slot is missing, a series slot gets
// one row per existing episode.
func initSlotState(ctx context.Context, tx *sql.Tx, slotID, titleID int64, kind string) error {
	if kind == kindMovie {
		_, err := tx.ExecContext(ctx, `UPDATE slot SET media_state = 'missing', size_bytes = 0 WHERE id = ?`, slotID)
		return err
	}
	return fillSlotEpisodes(ctx, tx, titleID, slotID, 0)
}

// fillSlotEpisodes inserts a missing, unsized row for every episode of the
// title with id > afterEpisodeID into slotID, or into every slot of the title
// when slotID is 0. Rows start monitored as their slot is.
func fillSlotEpisodes(ctx context.Context, tx *sql.Tx, titleID, slotID, afterEpisodeID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO slot_episode (slot_id, episode_id, title_id, ord, monitored, state, size_bytes, air_utc)
		SELECT s.id, e.id, e.title_id, e.ord, s.monitored, 'missing', 0, e.air_utc
		FROM slot s JOIN episode e ON e.title_id = s.title_id
		WHERE s.title_id = ? AND (? = 0 OR s.id = ?) AND e.id > ?`,
		titleID, slotID, slotID, afterEpisodeID)
	return err
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
