package library

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

const (
	maxReleaseKey   = 256
	maxPath         = 1024
	maxRequestSlots = 64
)

// AcquisitionRequest asks one download client to fetch one release for Slots.
// Two requests with the same ClientID and ReleaseKey are the same acquisition.
type AcquisitionRequest struct {
	ClientID   string
	ReleaseKey string
	Slots      []SlotID
}

// AcquisitionOutcome names the acquisition a request converged on. Created
// reports a new acquisition row. Attached lists, in request order, the slots
// this call newly attached; a slot that was already attached is not repeated.
type AcquisitionOutcome struct {
	AcquisitionID int64
	Attached      []SlotID
	Created       bool
}

// FileRecord is one file of a slot, named by a storage root and a relative path.
type FileRecord struct {
	SlotID       SlotID
	RootKey      string
	RelativePath string
	SizeBytes    int64
}

// RequestAcquisition converges on the one acquisition of (ClientID, ReleaseKey)
// and attaches every requested slot to it. The UNIQUE constraints decide the
// race, so any number of equivalent requests, replays and second copies of the
// release share one acquisition row and one download_slot row per slot. A
// different release or client is a different acquisition.
func (l *Library) RequestAcquisition(ctx context.Context, req AcquisitionRequest) (AcquisitionOutcome, error) {
	if err := req.validate(); err != nil {
		return AcquisitionOutcome{}, err
	}
	var out AcquisitionOutcome
	err := l.db.Write(ctx, func(tx *sql.Tx) (err error) {
		out, err = requestAcquisition(ctx, tx, req)
		return err
	})
	if err != nil {
		return AcquisitionOutcome{}, err
	}
	return out, nil
}

// requestAcquisition is RequestAcquisition inside the caller's transaction.
// Every slot is checked before the first write.
func requestAcquisition(ctx context.Context, tx *sql.Tx, req AcquisitionRequest) (AcquisitionOutcome, error) {
	var out AcquisitionOutcome
	for _, s := range req.Slots {
		if err := needSlot(ctx, tx, s); err != nil {
			return out, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO acquisition (client_id, release_key) VALUES (?, ?)
		ON CONFLICT (client_id, release_key) DO NOTHING`, req.ClientID, req.ReleaseKey)
	if err != nil {
		return out, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return out, err
	}
	out.Created = n == 1
	if err := tx.QueryRowContext(ctx, `SELECT id FROM acquisition WHERE client_id = ? AND release_key = ?`,
		req.ClientID, req.ReleaseKey).Scan(&out.AcquisitionID); err != nil {
		return out, err
	}
	for _, s := range req.Slots {
		res, err := tx.ExecContext(ctx, `INSERT INTO download_slot (acquisition_id, slot_id) VALUES (?, ?)
			ON CONFLICT (acquisition_id, slot_id) DO NOTHING`, out.AcquisitionID, s)
		if err != nil {
			return out, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return out, err
		} else if n == 1 {
			out.Attached = append(out.Attached, s)
		}
	}
	return out, nil
}

// RecordFile records a file of a slot. Replaying the same record changes
// nothing; a record for a path already held by another slot or with another
// size is ErrIdentityConflict and writes nothing.
func (l *Library) RecordFile(ctx context.Context, rec FileRecord) error {
	if err := rec.validate(); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		if err := needSlot(ctx, tx, rec.SlotID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO file (slot_id, root_key, relative_path, size_bytes) VALUES (?, ?, ?, ?)
			ON CONFLICT (root_key, relative_path) DO NOTHING`, rec.SlotID, rec.RootKey, rec.RelativePath, rec.SizeBytes)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			return err
		}
		var slot SlotID
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT slot_id, size_bytes FROM file WHERE root_key = ? AND relative_path = ?`,
			rec.RootKey, rec.RelativePath).Scan(&slot, &size); err != nil {
			return err
		}
		if slot != rec.SlotID || size != rec.SizeBytes {
			return fmt.Errorf("%w: file %s/%s is held by slot %d with %d bytes", ErrIdentityConflict, rec.RootKey, rec.RelativePath, slot, size)
		}
		return nil
	})
}

// RemoveMembership deletes the library_title row and nothing else: the title,
// its slots, acquisitions and files stay, because other libraries may share them.
func (l *Library) RemoveMembership(ctx context.Context, lib LibraryID, title TitleID) error {
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM library_title WHERE library_id = ? AND title_id = ?`, lib, title)
		return needRow(res, err, fmt.Sprintf("membership of title %d in library %d", title, lib))
	})
}

// needSlot returns ErrNotFound when no slot has the id.
func needSlot(ctx context.Context, tx *sql.Tx, id SlotID) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM slot WHERE id = ?`, id).Scan(&one)
	return notFound(err, fmt.Sprintf("slot %d", id))
}

func (r AcquisitionRequest) validate() error {
	if err := checkToken("client_id", r.ClientID, maxText, false, "._-"); err != nil {
		return err
	}
	if err := checkText("release_key", r.ReleaseKey, maxReleaseKey); err != nil {
		return err
	}
	if len(r.Slots) == 0 || len(r.Slots) > maxRequestSlots {
		return invalid("slots", fmt.Sprintf("must be 1-%d", maxRequestSlots))
	}
	for _, s := range r.Slots {
		if s <= 0 {
			return invalid("slots", "has a non-positive slot id")
		}
	}
	return nil
}

func (r FileRecord) validate() error {
	if r.SlotID <= 0 {
		return invalid("slot_id", "must be positive")
	}
	if err := checkToken("root_key", r.RootKey, maxText, false, "._-"); err != nil {
		return err
	}
	if err := checkText("relative_path", r.RelativePath, maxPath); err != nil {
		return err
	}
	if strings.HasPrefix(r.RelativePath, "/") {
		return invalid("relative_path", "must be relative")
	}
	for _, seg := range strings.Split(r.RelativePath, "/") {
		if seg == ".." {
			return invalid("relative_path", "has a .. segment")
		}
	}
	// The path is the uniqueness key, so only its one canonical spelling may
	// be stored: "a/./b", "a//b", "./a/b" and "a/b/" would alias "a/b".
	if r.RelativePath == "." || path.Clean(r.RelativePath) != r.RelativePath {
		return invalid("relative_path", "is not canonical (has a ., empty or trailing segment)")
	}
	if r.SizeBytes < 0 {
		return invalid("size_bytes", "must not be negative")
	}
	return nil
}
