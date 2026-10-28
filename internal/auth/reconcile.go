package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// ReconcileRestore applies the current installation's authority to the restored
// file in one Write before access resumes. The current authority is the pinned
// pre-replacement copy, never the snapshot: an account the installation disabled
// or demoted, and a setup it consumed, stay that way, while an account created
// after the snapshot is kept.
//
// Every current row is written back by id with its current name, verifier, role
// and disabled flag; a restored id the current installation does not know is
// disabled; the consumed setup side wins; each touched row's revision and the
// users collection revision advance above both sides; and the installation's
// restore epoch becomes the journal's, so no session or key from before the
// restore authenticates again.
func ReconcileRestore(ctx context.Context, db *store.DB, current install.Authority, r install.Recovery) error {
	restored, err := readAuthority(ctx, db)
	if err != nil {
		return err
	}
	if current.InstallID == "" || current.InstallID != restored.InstallID {
		return fmt.Errorf("%w: the pinned copy and the restored file are not one installation", install.ErrInstallationMismatch)
	}
	// A snapshot's grant is never authority on its own: a restored file that holds
	// accounts or a consumed setup the pinned copy does not know is refused before
	// the write.
	if restored.HasAuthority() && !current.HasAuthority() {
		return fmt.Errorf("%w: the restored file holds access state the pinned copy does not", install.ErrAuthorizationStateUnavailable)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		installation, err := q.GetInstallation(ctx)
		if err != nil {
			return fmt.Errorf("auth: restore reconcile: %w", err)
		}
		if installation.InstallID != current.InstallID {
			return fmt.Errorf("%w: the restored file changed installation under the reconciliation", install.ErrInstallationMismatch)
		}
		inFile, err := q.ListUserAuthority(ctx)
		if err != nil {
			return fmt.Errorf("auth: restore reconcile: %w", err)
		}
		restoredRevision := make(map[int64]int64, len(inFile))
		for _, row := range inFile {
			restoredRevision[row.ID] = row.Revision
		}
		// Every current row is written back by id, so an account the snapshot
		// lacked is re-created and one it had keeps the current name, verifier,
		// role and disabled flag. Usernames are immutable per id, so the upsert
		// cannot collide.
		known := make(map[int64]bool, len(current.Users))
		for _, keep := range current.Users {
			known[keep.ID] = true
			if err := q.UpsertRestoredUser(ctx, storedb.UpsertRestoredUserParams{
				ID:          keep.ID,
				Username:    keep.Username,
				UsernameKey: keep.UsernameKey,
				PasswordPhc: keep.PasswordPHC,
				Role:        keep.Role,
				Disabled:    keep.Disabled,
				Revision:    max(keep.Revision, restoredRevision[keep.ID]) + 1,
				CreatedAt:   keep.CreatedAt,
			}); err != nil {
				return fmt.Errorf("auth: restore reconcile: %w", err)
			}
		}
		// A restored id the current installation does not know loses its access.
		for _, row := range inFile {
			if known[row.ID] {
				continue
			}
			if err := q.DisableRestoredUser(ctx, storedb.DisableRestoredUserParams{
				Revision: max(row.Revision, 1) + 1,
				ID:       row.ID,
			}); err != nil {
				return fmt.Errorf("auth: restore reconcile: %w", err)
			}
		}
		if err := reconcileSetup(ctx, q, current.Setup, restored); err != nil {
			return err
		}
		// The users trigger advances this on every row write; setting it last
		// keeps one final value above both sides and every trigger's bump.
		revision, err := q.UsersCollectionRevision(ctx)
		if err != nil {
			return fmt.Errorf("auth: restore reconcile: %w", err)
		}
		if err := q.SetCollectionRevision(ctx, storedb.SetCollectionRevisionParams{
			Name:     "users",
			Revision: max(revision, restored.CollectionRevision, 1) + 1,
		}); err != nil {
			return fmt.Errorf("auth: restore reconcile: %w", err)
		}
		if err := q.SetRestoreEpoch(ctx, r.Epoch); err != nil {
			return fmt.Errorf("auth: restore reconcile: %w", err)
		}
		return nil
	})
}

// reconcileSetup keeps the consumed side of the one-time first-admin credential.
// Setup never reopens: a consumed current verifier replaces an open restored one,
// and a consumed restored row stays when the current installation never consumed
// it.
func reconcileSetup(ctx context.Context, q *storedb.Queries, current *install.AuthoritySetup, restored restoreAuthority) error {
	if current == nil || current.ConsumedAt == 0 {
		return nil
	}
	if restored.Setup != nil && restored.Setup.ConsumedAt == current.ConsumedAt && restored.Setup.TokenSHA256 == current.TokenSHA256 {
		return nil
	}
	if err := q.UpsertSetup(ctx, storedb.UpsertSetupParams{
		TokenSha256: current.TokenSHA256,
		ConsumedAt:  sql.NullInt64{Int64: current.ConsumedAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("auth: restore reconcile: %w", err)
	}
	return nil
}

// restoreAuthority is the access state one database file holds, as the
// reconciliation reads it.
type restoreAuthority struct {
	InstallID          string
	Epoch              int64
	Users              []storedb.User
	Setup              *install.AuthoritySetup
	CollectionRevision int64
}

// HasAuthority reports whether the file already holds accounts or a consumed
// setup credential.
func (a restoreAuthority) HasAuthority() bool {
	return len(a.Users) > 0 || (a.Setup != nil && a.Setup.ConsumedAt > 0)
}

// readAuthority reads the identity and access rows of the open store.
func readAuthority(ctx context.Context, db *store.DB) (a restoreAuthority, err error) {
	err = db.Read(ctx, func(tx *sql.Tx) error {
		q := storedb.New(tx)
		installation, err := q.GetInstallation(ctx)
		if err != nil {
			return err
		}
		a.InstallID, a.Epoch = installation.InstallID, installation.RestoreEpoch
		if a.Users, err = q.ListUserAuthority(ctx); err != nil {
			return err
		}
		if a.CollectionRevision, err = q.UsersCollectionRevision(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err := q.GetSetup(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		setup := &install.AuthoritySetup{TokenSHA256: row.TokenSha256}
		if row.ConsumedAt.Valid {
			setup.ConsumedAt = row.ConsumedAt.Int64
		}
		a.Setup = setup
		return nil
	})
	if err != nil {
		return a, fmt.Errorf("auth: restore reconcile: %w", err)
	}
	return a, nil
}
