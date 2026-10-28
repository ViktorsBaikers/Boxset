package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

const metadataBatchSize = store.MaxMetadataRecords

// Reserve the full maps (including hash tables and verifier strings) and one
// snapshot before activity is admitted; payload growth cannot borrow from expiry.
const metadataReservedBytes = 2*sessionMemoryEntries*224 + metadataBatchSize*224 + 16384
const metadataByteLimit = 2 * 1024 * 1024

type metadataItem struct {
	key      string
	keyUse   bool
	activity sessionExpiry
	valid    bool
}

func (s *Sessions) activityTime(epoch int64) time.Time {
	elapsed := s.now()
	if elapsed.Before(s.cadence) {
		elapsed = s.cadence
	}
	s.cadence = elapsed
	now := elapsed.UTC()
	if now.Before(s.clock) {
		now = s.clock
	}
	s.clock = now
	if s.started.IsZero() {
		s.started = elapsed
	}
	if s.lastAttempt.IsZero() {
		s.lastAttempt = s.started
	}
	if s.lastFlush.IsZero() {
		s.lastFlush = s.started
	}
	if s.epoch != 0 && s.epoch != epoch {
		clear(s.expiry)
		clear(s.keys)
	}
	s.epoch = epoch
	return now
}

func (s *Sessions) forget(keyUse bool, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keyUse {
		delete(s.keys, key)
	} else {
		delete(s.expiry, key)
	}
}

// StopMetadata prevents further flush admission. The owner also cancels and
// joins its worker before closing the database or releasing the lifetime lock.
func (s *Sessions) StopMetadata() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

// FlushMetadata is the shared direct-call and worker tick; HTTP calls never flush.
func (s *Sessions) FlushMetadata(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	select {
	case s.flush <- struct{}{}:
		defer func() { <-s.flush }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	wall := s.activityTime(s.epoch)
	now := s.cadence
	if s.stopped || now.Sub(s.lastAttempt) < time.Minute || now.Sub(s.lastFlush) < time.Minute {
		s.mu.Unlock()
		return nil
	}
	var batch [metadataBatchSize]metadataItem
	n := 0
	selectItem := func(key string, keyUse bool, item sessionExpiry) {
		candidate := metadataItem{key: key, keyUse: keyUse, activity: item}
		if n < len(batch) {
			batch[n] = candidate
			n++
			return
		}
		last := 0
		for i := 1; i < n; i++ {
			if batch[i].activity.turn > batch[last].activity.turn {
				last = i
			}
		}
		if item.turn < batch[last].activity.turn {
			batch[last] = candidate
		}
	}
	for key, item := range s.expiry {
		if !wall.Before(item.expires) {
			delete(s.expiry, key)
			continue
		}
		if now.Sub(item.persisted) >= time.Hour && item.expires.After(item.persisted.Add(SessionTTL)) {
			selectItem(key, false, item)
		}
	}
	for key, item := range s.keys {
		if wall.Sub(item.expires) >= SessionTTL {
			delete(s.keys, key)
			continue
		}
		if item.expires.After(item.persisted) {
			selectItem(key, true, item)
		}
	}
	if n > 0 {
		s.lastAttempt = now
	}
	s.mu.Unlock()
	if n == 0 {
		return nil
	}
	if err := s.metadataFault("before_write"); err != nil {
		return err
	}
	err := s.db.WriteMetadata(ctx, n, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		inst, err := q.GetInstallation(ctx)
		if err != nil {
			return err
		}
		changed := false
		for i := 0; i < n; i++ {
			item := &batch[i]
			if item.activity.epoch != inst.RestoreEpoch {
				continue
			}
			if item.keyUse {
				row, err := q.ReadAPIKey(ctx, item.key)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if row.Disabled != 0 || row.Epoch != inst.RestoreEpoch {
					continue
				}
				count, err := q.TouchAPIKey(ctx, storedb.TouchAPIKeyParams{TokenSha256: item.key, Epoch: item.activity.epoch, LastUsedAt: item.activity.expires.Unix()})
				if err != nil {
					return err
				}
				changed = changed || count > 0
			} else {
				row, err := q.ReadSession(ctx, item.key)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if row.Disabled != 0 || row.Epoch != inst.RestoreEpoch {
					continue
				}
				if row.ExpiresAt < item.activity.expires.Unix() {
					if err := q.ExtendSession(ctx, storedb.ExtendSessionParams{ExpiresAt: item.activity.expires.Unix(), TokenSha256: item.key}); err != nil {
						return err
					}
					changed = true
				}
			}
			item.valid = true
			if i == 0 {
				if err := s.metadataFault("after_statement"); err != nil {
					return err
				}
			}
		}
		if !changed {
			return errMetadataUnchanged
		}
		return s.metadataFault("before_commit")
	})
	if err != nil && !errors.Is(err, errMetadataUnchanged) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.activityTime(s.epoch)
		s.lastFlush = s.cadence
	}
	for i := 0; i < n; i++ {
		item := batch[i]
		entries := s.expiry
		if item.keyUse {
			entries = s.keys
		}
		current, exists := entries[item.key]
		if !exists || current.generation != item.activity.generation || current.epoch != item.activity.epoch {
			continue
		}
		if !item.valid {
			if !current.expires.After(item.activity.expires) {
				delete(entries, item.key)
			}
			continue
		}
		if item.keyUse && !current.expires.After(item.activity.expires) {
			delete(entries, item.key)
			continue
		}
		current.persisted = s.lastFlush
		if item.keyUse {
			current.persisted = item.activity.expires
		}
		s.sequence++
		current.turn = s.sequence
		entries[item.key] = current
	}
	return nil
}

var errMetadataUnchanged = errors.New("metadata_unchanged")

func (s *Sessions) metadataFault(point string) error {
	if s.batchFault != nil {
		return s.batchFault(point)
	}
	return nil
}
