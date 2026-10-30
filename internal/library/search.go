package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Title and alias search. The FTS5 trigram index (schema 60_search.sql) holds
// one row per title name and per alias and changes by trigger in the
// transaction that changes its source row. An alias is manual (no generation)
// or belongs to a generation of a dataset. A search sees manual aliases and
// those of the active generation: activation flips two states and rewrites no
// index row, and a purge deletes retired aliases in bounded batches.

// GenerationID names one alias_generation row.
type GenerationID int64

const (
	stateStaging = "staging"
	stateActive  = "active"
	stateRetired = "retired"

	// A trigram needs 3 characters, so a shorter term can never match.
	minTerm  = 3
	maxTerm  = 128
	maxAlias = 128
)

// qSearch reads through the FTS index: a title is the row -id, an alias its
// own id. UNION removes a title reached twice and sorts by id.
const qSearch = `SELECT -rowid FROM title_search WHERE title_search MATCH ?1 AND rowid < 0
	UNION
	SELECT a.title_id FROM title_search JOIN title_alias a ON a.id = title_search.rowid
		LEFT JOIN alias_generation g ON g.id = a.generation_id
		WHERE title_search MATCH ?1 AND title_search.rowid > 0 AND (a.generation_id IS NULL OR g.state = 'active')
	ORDER BY 1 LIMIT ?2`

// RenameTitle changes the title's name; the index follows in the same transaction.
func (l *Library) RenameTitle(ctx context.Context, id TitleID, name string) error {
	if err := checkText("name", name, maxName); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE title SET name = ? WHERE id = ?`, name, int64(id))
		return needRow(res, err, fmt.Sprintf("title %d", id))
	})
}

// SetAliases replaces the title's aliases of one set: the manual set when gen
// is nil, else the set of a staging generation, which a search does not see
// until the generation is activated. Duplicates collapse.
func (l *Library) SetAliases(ctx context.Context, id TitleID, gen *GenerationID, aliases []string) error {
	seen := make(map[string]bool, len(aliases))
	var uniq []string
	for _, a := range aliases {
		if len(a) < minTerm {
			return invalid("alias", fmt.Sprintf("must be %d-%d bytes", minTerm, maxAlias))
		}
		if err := checkText("alias", a, maxAlias); err != nil {
			return err
		}
		if !seen[a] {
			seen[a] = true
			uniq = append(uniq, a)
		}
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM title WHERE id = ?`, int64(id)).Scan(&one); err != nil {
			return notFound(err, fmt.Sprintf("title %d", id))
		}
		var genArg any // nil binds NULL: the manual set
		if gen != nil {
			state, _, err := generation(ctx, tx, *gen)
			if err != nil {
				return err
			}
			if state != stateStaging {
				return invalid("generation", "is not staging")
			}
			genArg = int64(*gen)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM title_alias WHERE title_id = ? AND generation_id IS ?`, int64(id), genArg); err != nil {
			return err
		}
		for _, a := range uniq {
			if _, err := tx.ExecContext(ctx, `INSERT INTO title_alias (title_id, generation_id, alias) VALUES (?, ?, ?)`, int64(id), genArg, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// generation returns the state and dataset of a generation, or ErrNotFound.
func generation(ctx context.Context, tx *sql.Tx, id GenerationID) (state, dataset string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT state, dataset FROM alias_generation WHERE id = ?`, int64(id)).Scan(&state, &dataset)
	return state, dataset, notFound(err, fmt.Sprintf("alias generation %d", id))
}

// StageGeneration opens a new staging generation of dataset.
func (l *Library) StageGeneration(ctx context.Context, dataset string) (GenerationID, error) {
	if err := checkText("dataset", dataset, maxText); err != nil {
		return 0, err
	}
	var id GenerationID
	err := l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO alias_generation (dataset, state) VALUES (?, 'staging')`, dataset)
		if err != nil {
			return err
		}
		n, err := res.LastInsertId()
		id = GenerationID(n)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ActivateGeneration makes a staging generation the active one of its dataset
// and retires the one it replaces, in one transaction: a search sees either
// the old set or the new one. The retire and the required job that deletes the
// retired generation's aliases commit together, so a crash leaves both or
// neither; a dataset with no active generation enqueues nothing.
func (l *Library) ActivateGeneration(ctx context.Context, id GenerationID) error {
	return l.db.Write(ctx, func(tx *sql.Tx) error { return activateGeneration(ctx, tx, id) })
}

// activateGeneration is the activation inside the caller's write transaction, so
// a proof can hold the process at the commit that carries the domain change and
// its intent.
func activateGeneration(ctx context.Context, tx *sql.Tx, id GenerationID) error {
	state, dataset, err := generation(ctx, tx, id)
	if err != nil {
		return err
	}
	if state != stateStaging {
		return invalid("generation", "is not staging")
	}
	// The row this transaction is about to retire is read before the UPDATE
	// that retires it.
	var retiring sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM alias_generation WHERE dataset = ? AND state = 'active'`, dataset).Scan(&retiring); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alias_generation SET state = 'retired' WHERE dataset = ? AND state = 'active'`, dataset); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alias_generation SET state = 'active' WHERE id = ?`, int64(id)); err != nil {
		return err
	}
	if !retiring.Valid {
		return nil
	}
	_, _, err = store.EnqueueJob(ctx, tx, purgeIntent(GenerationID(retiring.Int64), time.Now().Unix()))
	return err
}

// purgeIntent is the required alias.purge job of one retired generation: one
// open job and one live attempt per generation, with five bounded attempts.
func purgeIntent(g GenerationID, now int64) store.JobIntent {
	generation := strconv.FormatInt(int64(g), 10)
	return store.JobIntent{
		Kind:           "alias.purge",
		Class:          store.JobRequired,
		Lane:           store.JobBackground,
		DedupeKey:      "alias-generation:" + generation,
		ConcurrencyKey: "alias-generation:" + generation,
		Params:         map[string]string{"generation": generation},
		MaxAttempts:    5,
		DueAt:          now,
	}
}

// PurgeGeneration deletes up to batch aliases of a generation that is not
// active and returns how many remain, read from the count the schema keeps
// with the aliases, so a batch never scans what is left. A staging generation
// is retired first, so it takes no more aliases. Retired aliases are never
// visible, so every batch leaves the search answer as it was.
func (l *Library) PurgeGeneration(ctx context.Context, id GenerationID, batch int) (remaining int, err error) {
	if batch < 1 {
		return 0, invalid("batch", "must be at least 1")
	}
	err = l.db.Write(ctx, func(tx *sql.Tx) error {
		state, _, err := generation(ctx, tx, id)
		if err != nil {
			return err
		}
		if state == stateActive {
			return invalid("generation", "is active")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE alias_generation SET state = 'retired' WHERE id = ?`, int64(id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM title_alias WHERE id IN
			(SELECT id FROM title_alias WHERE generation_id = ? ORDER BY id LIMIT ?)`, int64(id), batch); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT alias_count FROM alias_generation WHERE id = ?`, int64(id)).Scan(&remaining)
	})
	if err != nil {
		return 0, err
	}
	return remaining, nil
}

// Search returns the ids, ascending and at most limit, of the titles whose
// name or visible alias contains term, ignoring case. The term is one
// quoted FTS5 string, so query syntax in it is matched as text. A term must be
// 3-128 bytes of valid UTF-8 without NUL.
func (l *Library) Search(ctx context.Context, term string, limit int) ([]TitleID, error) {
	if len(term) < minTerm || len(term) > maxTerm {
		return nil, invalid("term", fmt.Sprintf("must be %d-%d bytes", minTerm, maxTerm))
	}
	if !utf8.ValidString(term) || strings.IndexByte(term, 0) >= 0 {
		return nil, invalid("term", "is not valid text")
	}
	if limit < 1 {
		return nil, invalid("limit", "must be at least 1")
	}
	quoted := `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	var out []TitleID
	err := l.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, qSearch, quoted, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id TitleID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}
