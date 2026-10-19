// Package library owns canonical title and slot identity: one title per
// metadata_id, one slot per (title, slot_key), and the per-slot state that
// hangs off them. Every mutation is one store Write closure.
package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// Slot owners. Any other owner value is a connected instance id.
const (
	OwnerLocal      = "local"
	OwnerUnresolved = "unresolved"
)

const (
	kindMovie  = "movie"
	kindSeries = "series"
)

var (
	// ErrIdentityConflict is returned before any write when an identity would
	// merge or move: a known source pointing elsewhere, a second source for one
	// slot in one instance, or one metadata_id with two kinds.
	ErrIdentityConflict = errors.New("library: identity conflict")
	// ErrInvalidInput is returned before any write for malformed input.
	ErrInvalidInput = errors.New("library: invalid input")
	// ErrNotFound is returned when a named library, persona, slot or episode is absent.
	ErrNotFound = errors.New("library: not found")
)

type (
	TitleID   int64
	LibraryID int64
	SlotID    int64
)

// Library is the identity and slot-state API over one store.
type Library struct{ db *store.DB }

// New returns a Library over db.
func New(db *store.DB) *Library { return &Library{db: db} }

// LibraryDefaults seed a slot when it is created. They never change a slot afterwards.
type LibraryDefaults struct {
	Monitored      bool
	NamingLanguage string
	SearchLanguage string
	Profile        string
}

// TemplateSlot is one slot key a library selects for every title added to it.
type TemplateSlot struct {
	Key     string
	Primary bool
}

// LibrarySpec describes a new library. With no Primary template slot the first one is primary.
type LibrarySpec struct {
	Name     string
	Defaults LibraryDefaults
	Template []TemplateSlot
}

// MembershipRequest adds a title to a library through the library's template.
type MembershipRequest struct {
	LibraryID  LibraryID
	MetadataID string
	Kind       string
	Name       string
}

// PersonaRequest adds a title through a persona's slot key; LibraryID supplies
// the defaults for a slot the persona creates and the membership it records.
type PersonaRequest struct {
	LibraryID  LibraryID
	Persona    string
	MetadataID string
	Kind       string
	Name       string
}

// SourceRecord is one title of a connected instance. It represents the slot SlotKey.
type SourceRecord struct {
	InstanceID string
	SourceID   string
	LibraryID  LibraryID
	MetadataID string
	Kind       string
	Name       string
	SlotKey    string
}

// PolicyDiff reports a reused slot whose explicit policy differs from the
// defaults of the library that selected it. The slot is left as it is.
type PolicyDiff struct {
	SlotID SlotID
	Field  string
	Have   string
	Want   string
}

// MembershipResult lists the slots the request selected (primary key first,
// then template order). Created reports a new title row.
type MembershipResult struct {
	TitleID     TitleID
	SlotIDs     []SlotID
	Created     bool
	PolicyDiffs []PolicyDiff
}

// ImportResult reports the title and the one slot a source record maps to.
type ImportResult struct {
	TitleID      TitleID
	SlotID       SlotID
	TitleCreated bool
	SlotCreated  bool
}

// Slot is one wanted version of a title. MediaState and SizeBytes describe a
// movie slot and are zero on a series slot.
type Slot struct {
	ID             SlotID
	TitleID        TitleID
	Key            string
	Label          string
	Profile        string
	Primary        bool
	Owner          string
	Monitored      bool
	NamingLanguage string
	SearchLanguage string
	MediaState     string
	SizeBytes      int64
}

// AddLibrary creates a library with its template.
func (l *Library) AddLibrary(ctx context.Context, spec LibrarySpec) (LibraryID, error) {
	tpl, err := spec.validate()
	if err != nil {
		return 0, err
	}
	var id LibraryID
	err = l.db.Write(ctx, func(tx *sql.Tx) error {
		d := spec.Defaults
		res, err := tx.ExecContext(ctx, `INSERT INTO library
			(name, default_monitored, default_naming_language, default_search_language, default_profile)
			VALUES (?, ?, ?, ?, ?)`, spec.Name, b2i(d.Monitored), d.NamingLanguage, d.SearchLanguage, d.Profile)
		if err != nil {
			return err
		}
		n, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, t := range tpl {
			if _, err := tx.ExecContext(ctx, `INSERT INTO library_template (library_id, slot_key, is_primary) VALUES (?, ?, ?)`,
				n, t.Key, b2i(t.Primary)); err != nil {
				return err
			}
		}
		id = LibraryID(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AddPersona maps a persona to a slot key. The mapping is immutable: the same
// pair is a no-op and a different key for the persona is ErrIdentityConflict.
func (l *Library) AddPersona(ctx context.Context, persona, slotKey string) error {
	if err := checkKey("persona", persona); err != nil {
		return err
	}
	if err := checkKey("slot_key", slotKey); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO persona (key, slot_key) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, persona, slotKey)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			return err
		}
		var have string
		if err := tx.QueryRowContext(ctx, `SELECT slot_key FROM persona WHERE key = ?`, persona).Scan(&have); err != nil {
			return err
		}
		if have != slotKey {
			return fmt.Errorf("%w: persona %q maps to slot key %q, not %q", ErrIdentityConflict, persona, have, slotKey)
		}
		return nil
	})
}

// AddMembership reuses the title with req.MetadataID (or creates it), reuses
// each template slot by (title, slot_key) and creates the missing ones from
// the library defaults with owner local.
func (l *Library) AddMembership(ctx context.Context, req MembershipRequest) (MembershipResult, error) {
	if err := checkTitle(req.MetadataID, req.Kind, req.Name); err != nil {
		return MembershipResult{}, err
	}
	var res MembershipResult
	err := l.db.Write(ctx, func(tx *sql.Tx) error {
		def, err := loadDefaults(ctx, tx, req.LibraryID)
		if err != nil {
			return err
		}
		keys, err := templateKeys(ctx, tx, req.LibraryID)
		if err != nil {
			return err
		}
		res, _, err = enroll(ctx, tx, req.LibraryID, def, req.MetadataID, req.Kind, req.Name, keys, OwnerLocal)
		return err
	})
	if err != nil {
		return MembershipResult{}, err
	}
	return res, nil
}

// ApplyPersona is AddMembership for the one slot key the persona maps to.
func (l *Library) ApplyPersona(ctx context.Context, req PersonaRequest) (MembershipResult, error) {
	if err := checkKey("persona", req.Persona); err != nil {
		return MembershipResult{}, err
	}
	if err := checkTitle(req.MetadataID, req.Kind, req.Name); err != nil {
		return MembershipResult{}, err
	}
	var res MembershipResult
	err := l.db.Write(ctx, func(tx *sql.Tx) error {
		def, err := loadDefaults(ctx, tx, req.LibraryID)
		if err != nil {
			return err
		}
		var key string
		if err := tx.QueryRowContext(ctx, `SELECT slot_key FROM persona WHERE key = ?`, req.Persona).Scan(&key); err != nil {
			return notFound(err, "persona "+strconv.Quote(req.Persona))
		}
		res, _, err = enroll(ctx, tx, req.LibraryID, def, req.MetadataID, req.Kind, req.Name, []string{key}, OwnerLocal)
		return err
	})
	if err != nil {
		return MembershipResult{}, err
	}
	return res, nil
}

// ImportSource maps one source title of a connected instance to its slot. A
// new slot starts unresolved; an existing slot keeps its owner. A known source
// that points elsewhere, or a second source for one slot in one instance, is
// ErrIdentityConflict with nothing written. Replaying a mapped record changes nothing.
func (l *Library) ImportSource(ctx context.Context, rec SourceRecord) (ImportResult, error) {
	if err := rec.validate(); err != nil {
		return ImportResult{}, err
	}
	var out ImportResult
	err := l.db.Write(ctx, func(tx *sql.Tx) error {
		def, err := loadDefaults(ctx, tx, rec.LibraryID)
		if err != nil {
			return err
		}
		var titleID, slotID int64
		var metadataID, kind, slotKey string
		err = tx.QueryRowContext(ctx, `SELECT m.title_id, m.slot_id, t.metadata_id, t.kind, s.slot_key
			FROM sync_map m JOIN title t ON t.id = m.title_id JOIN slot s ON s.id = m.slot_id
			WHERE m.instance_id = ? AND m.source_kind = ? AND m.source_id = ?`,
			rec.InstanceID, rec.Kind, rec.SourceID).Scan(&titleID, &slotID, &metadataID, &kind, &slotKey)
		switch {
		case err == nil:
			if metadataID != rec.MetadataID || kind != rec.Kind || slotKey != rec.SlotKey {
				return fmt.Errorf("%w: %s source %s of %s maps to %s/%s, not %s/%s",
					ErrIdentityConflict, rec.Kind, rec.SourceID, rec.InstanceID, metadataID, slotKey, rec.MetadataID, rec.SlotKey)
			}
			if err := addMembership(ctx, tx, rec.LibraryID, titleID); err != nil {
				return err
			}
			out = ImportResult{TitleID: TitleID(titleID), SlotID: SlotID(slotID)}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// New source. The slot it names may already be mapped from another source of this instance.
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sync_map m
			JOIN slot s ON s.id = m.slot_id JOIN title t ON t.id = s.title_id
			WHERE m.instance_id = ? AND t.metadata_id = ? AND s.slot_key = ?)`,
			rec.InstanceID, rec.MetadataID, rec.SlotKey).Scan(&taken); err != nil {
			return err
		}
		if taken == 1 {
			return fmt.Errorf("%w: slot %s/%s already has a source in %s", ErrIdentityConflict, rec.MetadataID, rec.SlotKey, rec.InstanceID)
		}
		res, refs, err := enroll(ctx, tx, rec.LibraryID, def, rec.MetadataID, rec.Kind, rec.Name, []string{rec.SlotKey}, OwnerUnresolved)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_map (instance_id, source_kind, source_id, title_id, slot_id) VALUES (?, ?, ?, ?, ?)`,
			rec.InstanceID, rec.Kind, rec.SourceID, int64(res.TitleID), int64(refs[0].id)); err != nil {
			return err
		}
		out = ImportResult{TitleID: res.TitleID, SlotID: refs[0].id, TitleCreated: res.Created, SlotCreated: refs[0].created}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return out, nil
}

// SetLibraryDefaults changes the defaults of slots created from now on. No existing slot changes.
func (l *Library) SetLibraryDefaults(ctx context.Context, id LibraryID, d LibraryDefaults) error {
	if err := d.validate(); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE library SET default_monitored = ?, default_naming_language = ?,
			default_search_language = ?, default_profile = ? WHERE id = ?`,
			b2i(d.Monitored), d.NamingLanguage, d.SearchLanguage, d.Profile, int64(id))
		return needRow(res, err, "library "+strconv.FormatInt(int64(id), 10))
	})
}

// RelabelSlot changes a slot's mutable label and profile. Its id and key never change.
func (l *Library) RelabelSlot(ctx context.Context, id SlotID, label, profile string) error {
	if err := checkText("label", label, maxText); err != nil {
		return err
	}
	if err := checkText("profile", profile, maxText); err != nil {
		return err
	}
	return l.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE slot SET label = ?, profile = ? WHERE id = ?`, label, profile, int64(id))
		return needRow(res, err, "slot "+strconv.FormatInt(int64(id), 10))
	})
}

// TitleSlots returns the slots of a title in creation order.
func (l *Library) TitleSlots(ctx context.Context, id TitleID) ([]Slot, error) {
	var out []Slot
	err := l.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, title_id, slot_key, label, profile, is_primary, owner, monitored,
			naming_language, search_language, media_state, size_bytes FROM slot WHERE title_id = ? ORDER BY id`, int64(id))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s Slot
			var state sql.NullString
			var size sql.NullInt64
			if err := rows.Scan(&s.ID, &s.TitleID, &s.Key, &s.Label, &s.Profile, &s.Primary, &s.Owner, &s.Monitored,
				&s.NamingLanguage, &s.SearchLanguage, &state, &size); err != nil {
				return err
			}
			s.MediaState, s.SizeBytes = state.String, size.Int64
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

type slotRef struct {
	id      SlotID
	created bool
}

// enroll upserts the title, reuses or creates one slot per key, and records
// the library membership. Every conflict check precedes the first write.
func enroll(ctx context.Context, tx *sql.Tx, lib LibraryID, def LibraryDefaults, metadataID, kind, name string, keys []string, owner string) (MembershipResult, []slotRef, error) {
	var res MembershipResult
	var tid int64
	var have string
	err := tx.QueryRowContext(ctx, `SELECT id, kind FROM title WHERE metadata_id = ?`, metadataID).Scan(&tid, &have)
	if err == nil {
		if have != kind {
			return res, nil, fmt.Errorf("%w: %s is a %s, not a %s", ErrIdentityConflict, metadataID, have, kind)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		r, err := tx.ExecContext(ctx, `INSERT INTO title (metadata_id, kind, name) VALUES (?, ?, ?)`, metadataID, kind, name)
		if err != nil {
			return res, nil, err
		}
		if tid, err = r.LastInsertId(); err != nil {
			return res, nil, err
		}
		res.Created = true
	} else {
		return res, nil, err
	}
	res.TitleID = TitleID(tid)

	refs := make([]slotRef, 0, len(keys))
	for _, key := range keys {
		ref, diffs, err := ensureSlot(ctx, tx, tid, kind, key, def, owner)
		if err != nil {
			return res, nil, err
		}
		refs = append(refs, ref)
		res.SlotIDs = append(res.SlotIDs, ref.id)
		res.PolicyDiffs = append(res.PolicyDiffs, diffs...)
	}
	if err := addMembership(ctx, tx, lib, tid); err != nil {
		return res, nil, err
	}
	return res, refs, nil
}

// ensureSlot reuses the slot by (title, key) only; defaults apply only to a new slot.
func ensureSlot(ctx context.Context, tx *sql.Tx, titleID int64, kind, key string, def LibraryDefaults, owner string) (slotRef, []PolicyDiff, error) {
	var s Slot
	err := tx.QueryRowContext(ctx, `SELECT id, profile, monitored, naming_language, search_language
		FROM slot WHERE title_id = ? AND slot_key = ?`, titleID, key).
		Scan(&s.ID, &s.Profile, &s.Monitored, &s.NamingLanguage, &s.SearchLanguage)
	if err == nil {
		var diffs []PolicyDiff
		add := func(field, have, want string) {
			if have != want {
				diffs = append(diffs, PolicyDiff{SlotID: s.ID, Field: field, Have: have, Want: want})
			}
		}
		add("monitored", strconv.FormatBool(s.Monitored), strconv.FormatBool(def.Monitored))
		add("naming_language", s.NamingLanguage, def.NamingLanguage)
		add("search_language", s.SearchLanguage, def.SearchLanguage)
		add("profile", s.Profile, def.Profile)
		return slotRef{id: s.ID}, diffs, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return slotRef{}, nil, err
	}
	// The first slot of a title is its primary; later slots never take it over.
	res, err := tx.ExecContext(ctx, `INSERT INTO slot (title_id, slot_key, label, profile, is_primary, owner, monitored, naming_language, search_language)
		VALUES (?, ?, ?, ?, NOT EXISTS (SELECT 1 FROM slot WHERE title_id = ? AND is_primary = 1), ?, ?, ?, ?)`,
		titleID, key, key, def.Profile, titleID, owner, b2i(def.Monitored), def.NamingLanguage, def.SearchLanguage)
	if err != nil {
		return slotRef{}, nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return slotRef{}, nil, err
	}
	if err := initSlotState(ctx, tx, id, titleID, kind); err != nil {
		return slotRef{}, nil, err
	}
	return slotRef{id: SlotID(id), created: true}, nil, nil
}

func addMembership(ctx context.Context, tx *sql.Tx, lib LibraryID, titleID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO library_title (library_id, title_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, int64(lib), titleID)
	return err
}

func loadDefaults(ctx context.Context, tx *sql.Tx, id LibraryID) (LibraryDefaults, error) {
	var d LibraryDefaults
	err := tx.QueryRowContext(ctx, `SELECT default_monitored, default_naming_language, default_search_language, default_profile
		FROM library WHERE id = ?`, int64(id)).Scan(&d.Monitored, &d.NamingLanguage, &d.SearchLanguage, &d.Profile)
	return d, notFound(err, "library "+strconv.FormatInt(int64(id), 10))
}

// templateKeys returns the library's slot keys, primary first, then insertion order.
func templateKeys(ctx context.Context, tx *sql.Tx, id LibraryID) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slot_key FROM library_template WHERE library_id = ? ORDER BY is_primary DESC, rowid`, int64(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

// needRow turns an UPDATE that matched nothing into ErrNotFound.
func needRow(res sql.Result, err error, what string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Input validation: every check runs before the first write.

const (
	maxText     = 64
	maxName     = 256
	maxTemplate = 16
)

func invalid(field, why string) error { return fmt.Errorf("%w: %s %s", ErrInvalidInput, field, why) }

// checkText accepts 1..max bytes of valid UTF-8 without control characters.
func checkText(field, s string, max int) error {
	if len(s) == 0 || len(s) > max {
		return invalid(field, "must be 1-"+strconv.Itoa(max)+" bytes")
	}
	if !utf8.ValidString(s) {
		return invalid(field, "is not valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return invalid(field, "has a control character")
		}
	}
	return nil
}

// checkToken accepts 1..max bytes drawn from ASCII letters and digits plus extra.
func checkToken(field, s string, max int, lower bool, extra string) error {
	if len(s) == 0 || len(s) > max {
		return invalid(field, "must be 1-"+strconv.Itoa(max)+" bytes")
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || !lower && c >= 'A' && c <= 'Z' || strings.IndexByte(extra, c) >= 0) {
			return invalid(field, "has a character outside its alphabet")
		}
	}
	return nil
}

// checkKey is the slot_key and persona key format: 1-32 bytes of [a-z0-9-].
func checkKey(field, s string) error { return checkToken(field, s, 32, true, "-") }

// checkMetadataID accepts "<provider>:<id>", e.g. "tmdb:603".
func checkMetadataID(s string) error {
	provider, id, ok := strings.Cut(s, ":")
	if !ok {
		return invalid("metadata_id", "must be <provider>:<id>")
	}
	if err := checkToken("metadata_id provider", provider, 16, true, ""); err != nil {
		return err
	}
	return checkToken("metadata_id value", id, 48, false, "_-")
}

func checkKind(s string) error {
	if s != kindMovie && s != kindSeries {
		return invalid("kind", "must be movie or series")
	}
	return nil
}

func checkTitle(metadataID, kind, name string) error {
	if err := checkMetadataID(metadataID); err != nil {
		return err
	}
	if err := checkKind(kind); err != nil {
		return err
	}
	return checkText("name", name, maxName)
}

func (d LibraryDefaults) validate() error {
	for _, f := range []struct{ name, v string }{
		{"naming_language", d.NamingLanguage}, {"search_language", d.SearchLanguage}, {"profile", d.Profile},
	} {
		if err := checkText(f.name, f.v, maxText); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the spec and returns the template with a primary marked.
func (s LibrarySpec) validate() ([]TemplateSlot, error) {
	if err := checkText("name", s.Name, maxText); err != nil {
		return nil, err
	}
	if err := s.Defaults.validate(); err != nil {
		return nil, err
	}
	if len(s.Template) == 0 || len(s.Template) > maxTemplate {
		return nil, invalid("template", "must have 1-"+strconv.Itoa(maxTemplate)+" slots")
	}
	tpl := append([]TemplateSlot(nil), s.Template...)
	seen := make(map[string]bool, len(tpl))
	primaries := 0
	for _, t := range tpl {
		if err := checkKey("template slot_key", t.Key); err != nil {
			return nil, err
		}
		if seen[t.Key] {
			return nil, invalid("template", "repeats slot_key "+t.Key)
		}
		seen[t.Key] = true
		if t.Primary {
			primaries++
		}
	}
	switch {
	case primaries > 1:
		return nil, invalid("template", "has more than one primary slot")
	case primaries == 0:
		tpl[0].Primary = true
	}
	return tpl, nil
}

// instance ids share the owner column with "local" and "unresolved".
func (r SourceRecord) validate() error {
	if err := checkToken("instance_id", r.InstanceID, maxText, false, "._-"); err != nil {
		return err
	}
	if r.InstanceID == OwnerLocal || r.InstanceID == OwnerUnresolved {
		return invalid("instance_id", "is a reserved owner")
	}
	if err := checkToken("source_id", r.SourceID, maxText, false, "._-"); err != nil {
		return err
	}
	if err := checkKey("slot_key", r.SlotKey); err != nil {
		return err
	}
	return checkTitle(r.MetadataID, r.Kind, r.Name)
}
