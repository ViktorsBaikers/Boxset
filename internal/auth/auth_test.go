package auth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

func openStore(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// setupRow is the raw setup row and the number of users, read without the API.
func setupRow(t *testing.T, db *store.DB) (row storedb.GetSetupRow, users int, found bool) {
	t.Helper()
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM user").Scan(&users); err != nil {
			return err
		}
		r, err := storedb.New(tx).GetSetup(t.Context())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		row, found = r, true
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return row, users, found
}

func TestHashAdmission429(t *testing.T) {
	if h := NewHasher(); cap(h.slots) != 2 || h.wait != 2*time.Second {
		t.Fatalf("production hasher: %d slots, wait %v; want 2 slots and 2 s", cap(h.slots), h.wait)
	}
	phc := mustHash(t)
	synctest.Test(t, func(t *testing.T) {
		h := NewHasher()
		h.slots <- struct{}{}
		h.slots <- struct{}{}
		for _, verify := range []bool{false, true} {
			result := make(chan error, 1)
			go func() {
				if verify {
					_, err := h.Verify(t.Context(), phc, "correct horse battery")
					result <- err
				} else {
					_, err := h.Hash(t.Context(), "correct horse battery")
					result <- err
				}
			}()
			synctest.Wait()
			select {
			case err := <-result:
				t.Fatalf("full admission did not wait: %v", err)
			default:
			}
			time.Sleep(HashWait)
			if err := <-result; !errors.Is(err, ErrHashBusy) {
				t.Fatalf("full admission expiry: %v, want hash_busy", err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := h.Hash(ctx, "correct horse battery"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admission: %v", err)
		}
		<-h.slots
		h.wait = 30 * time.Second
		acquired := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})
		errs := make(chan error, 4)
		for range 4 {
			go func() {
				if err := h.acquire(t.Context()); err != nil {
					errs <- err
					return
				}
				acquired <- struct{}{}
				<-release
				h.release()
				finished <- struct{}{}
			}()
		}
		for range 4 {
			<-acquired
			synctest.Wait()
			if len(h.slots) != HashSlots {
				t.Fatalf("held admission has %d slots, want %d", len(h.slots), HashSlots)
			}
			select {
			case <-acquired:
				t.Fatal("another waiter entered while every slot was held")
			default:
			}
			release <- struct{}{}
			<-finished
		}
		synctest.Wait()
		if len(h.slots) != 1 || len(errs) != 0 {
			t.Fatalf("admission did not drain: slots=%d errors=%d", len(h.slots), len(errs))
		}
	})
}

func mustHash(t *testing.T) string {
	t.Helper()
	phc, err := NewHasher().Hash(t.Context(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return phc
}

func TestHasherRejectsForeignParams(t *testing.T) {
	h := NewHasher()
	phc := mustHash(t)
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("PHC %q does not carry the fixed parameters", phc[:32])
	}
	if ok, err := h.Verify(t.Context(), phc, "correct horse battery"); !ok || err != nil {
		t.Fatalf("own hash: %v, %v", ok, err)
	}
	if ok, err := h.Verify(t.Context(), phc, "correct horse batterY"); ok || err != nil {
		t.Fatalf("wrong password: %v, %v", ok, err)
	}
	salt, key, _ := strings.Cut(strings.TrimPrefix(phc, phcPrefix), "$")
	h.slots <- struct{}{}
	h.slots <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for name, foreign := range map[string]string{
		"huge memory":       "$argon2id$v=19$m=4194304,t=3,p=4$" + salt + "$" + key,
		"one pass":          "$argon2id$v=19$m=65536,t=1,p=4$" + salt + "$" + key,
		"zero threads":      "$argon2id$v=19$m=65536,t=3,p=0$" + salt + "$" + key,
		"argon2i":           "$argon2i$v=19$m=65536,t=3,p=4$" + salt + "$" + key,
		"old version":       "$argon2id$v=16$m=65536,t=3,p=4$" + salt + "$" + key,
		"short salt":        phcPrefix + salt[:8] + "$" + key,
		"long key":          phcPrefix + salt + "$" + key + "AAAA",
		"extra field":       phc + "$x",
		"padded base64":     phcPrefix + salt + "==$" + key,
		"empty":             "",
		"another algorithm": "$2b$12$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz0123",
	} {
		ok, err := h.Verify(ctx, foreign, "correct horse battery")
		if ok || !errors.Is(err, ErrPHCInvalid) {
			t.Errorf("%s: %v, %v; want password_hash_invalid", name, ok, err)
		}
		if len(h.slots) != HashSlots {
			t.Errorf("%s: foreign parameters changed admission slots", name)
		}
	}
	for _, pw := range []string{"short", strings.Repeat("x", MaxPasswordBytes+1)} {
		if _, err := h.Hash(t.Context(), pw); !errors.Is(err, ErrPasswordLength) {
			t.Errorf("Hash of %d bytes: %v, want password_length_invalid", len(pw), err)
		}
	}
	// Bounded error texts carry no input.
	for _, e := range []error{ErrHashBusy, ErrPHCInvalid, ErrPasswordLength} {
		if strings.Contains(e.Error(), "horse") || len(e.Error()) > 64 {
			t.Errorf("error %q is not a bounded code", e)
		}
	}
}

func TestEnvTokenIgnoredAfterSetup(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	const env = "owner-chosen-setup-token-0123"
	tokenFile := filepath.Join(dir, TokenFileName)

	db := openStore(t, dir)
	s := NewSetup(db, NewHasher(), dir)
	if open, err := s.Arm(ctx, "fifteen-chars!!"); !open || !errors.Is(err, ErrEnvTokenInvalid) {
		t.Fatalf("Arm with a 15-character env token = %v, %v; want refused", open, err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused env token left a token file: %v", err)
	}
	if _, _, found := setupRow(t, db); found {
		t.Fatal("a refused env token stored a verifier")
	}
	if open, err := s.Arm(ctx, env); !open || err != nil {
		t.Fatalf("Arm(env) = %v, %v", open, err)
	}
	if b, err := os.ReadFile(tokenFile); err != nil || string(b) != env {
		t.Fatalf("token file holds %d bytes (err %v), want the env token", len(b), err)
	}
	fi, err := os.Stat(tokenFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v (err %v), want 0600", fi.Mode(), err)
	}
	u, err := s.Create(ctx, Verifier(env), "admin", "correct horse battery")
	if err != nil || u.Role != RoleAdmin || u.ID < 1 || u.Revision != 1 || u.Disabled {
		t.Fatalf("Create = %+v, %v", u, err)
	}
	if _, err := s.Create(ctx, Verifier(env), "second", "correct horse battery"); !errors.Is(err, ErrSetupCompleted) {
		t.Fatalf("the env token after setup: %v, want setup_completed", err)
	}
	if _, err := s.Create(ctx, Verifier("some-other-token-value"), "second", "correct horse battery"); !errors.Is(err, ErrSetupCredential) {
		t.Fatalf("another token after setup: %v, want setup_credential_invalid", err)
	}
	before, users, _ := setupRow(t, db)
	if users != 1 || !before.ConsumedAt.Valid {
		t.Fatalf("after setup: %d users, consumed %v", users, before.ConsumedAt)
	}
	fileBefore, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	statBefore, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart with BOXSET_SETUP_TOKEN still set, to the same and to another value.
	for _, again := range []string{env, "a-different-env-token-value", ""} {
		db = openStore(t, dir)
		s = NewSetup(db, NewHasher(), dir)
		time.Sleep(10 * time.Millisecond) // a rewrite would show in the modification time
		if open, err := s.Arm(ctx, again); open || err != nil {
			t.Fatalf("Arm after setup with env %q = %v, %v; want closed, nothing done", again, open, err)
		}
		after, users, _ := setupRow(t, db)
		if after != before || users != 1 {
			t.Errorf("restart with env %q changed the setup row (%+v -> %+v) or users (%d)", again, before, after, users)
		}
		fileAfter, err := os.ReadFile(tokenFile)
		statAfter, serr := os.Stat(tokenFile)
		if err != nil || serr != nil || !bytes.Equal(fileAfter, fileBefore) || !statAfter.ModTime().Equal(statBefore.ModTime()) || !os.SameFile(statAfter, statBefore) {
			t.Errorf("restart with env %q rewrote the token file", again)
		}
		if _, err := s.Create(ctx, Verifier(again), "late", "correct horse battery"); err == nil {
			t.Errorf("restart with env %q reopened setup", again)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArmGeneratesToken(t *testing.T) {
	dir := t.TempDir()
	db := openStore(t, dir)
	t.Cleanup(func() { _ = db.Close() })
	s := NewSetup(db, NewHasher(), dir)
	if open, err := s.Arm(t.Context(), ""); !open || err != nil {
		t.Fatalf("Arm = %v, %v", open, err)
	}
	first, err := os.ReadFile(filepath.Join(dir, TokenFileName))
	if err != nil || len(first) != 43 || strings.ContainsAny(string(first), "+/=\n") {
		t.Fatalf("generated token is %d bytes (err %v), want 32 random bytes as unpadded base64url", len(first), err)
	}
	st, err := s.State(t.Context())
	if err != nil || !st.Open || !st.Matches(Verifier(string(first))) {
		t.Fatalf("state after Arm: %+v, %v", st, err)
	}
	// A restart while setup is still open replaces the token; the old one stops working.
	if _, err := s.Arm(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, TokenFileName))
	if err != nil || bytes.Equal(first, second) {
		t.Fatalf("second token equals the first (err %v)", err)
	}
	if st, _ := s.State(t.Context()); st.Matches(Verifier(string(first))) || !st.Matches(Verifier(string(second))) {
		t.Fatal("the stored verifier is not the current token's")
	}
	if _, err := s.Create(t.Context(), Verifier(string(first)), "admin", "correct horse battery"); !errors.Is(err, ErrSetupCredential) {
		t.Fatalf("the replaced token: %v, want setup_credential_invalid", err)
	}
	for _, name := range []string{"", strings.Repeat("a", 65), "bad\x00name", "tab\tname", "rtl\u202ename"} {
		if _, err := s.Create(t.Context(), Verifier(string(second)), name, "correct horse battery"); !errors.Is(err, ErrUsernameInvalid) {
			t.Errorf("username %q: %v, want username_invalid", name, err)
		}
	}
	if _, users, _ := setupRow(t, db); users != 0 {
		t.Fatalf("%d users after refused creations", users)
	}
	u, err := s.Create(t.Context(), Verifier(string(second)), strings.Repeat("é", 64), "correct horse battery")
	if err != nil || u.Username != strings.Repeat("é", 64) {
		t.Fatalf("64-character username: %+v, %v", u, err)
	}
}

func TestUsernameIdentity(t *testing.T) {
	create := func(ctx context.Context, db *store.DB, name, role string) (User, error) {
		var u User
		err := db.Write(ctx, func(tx *sql.Tx) error {
			var err error
			u, err = createUser(ctx, tx, name, "synthetic-prehashed-password", role, time.Unix(1, 0))
			return err
		})
		if err != nil {
			return User{}, err
		}
		return u, nil
	}
	for _, tc := range []struct{ name, first, equivalent, display, key string }{
		{"canonical display", "e\u0301", "É", "é", "é"},
		{"accent case", "É", "é", "É", "é"},
		{"sigma", "Σ", "ς", "Σ", "σ"},
		{"expanded fold", "Straße", "STRASSE", "Straße", "strasse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openStore(t, t.TempDir())
			defer func() { _ = db.Close() }()
			u, err := create(t.Context(), db, tc.first, RoleMember)
			if err != nil || u.Username != tc.display || u.ID != 1 {
				t.Fatalf("created display=%q id=%d err=%v, want %q id=1", u.Username, u.ID, err, tc.display)
			}
			if _, err := create(t.Context(), db, tc.equivalent, RoleMember); err == nil {
				t.Fatal("equivalent username created a second user")
			}
			var display, key string
			if err := db.Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(), "SELECT username, username_key FROM user WHERE id=1").Scan(&display, &key)
			}); err != nil || display != tc.display || key != tc.key {
				t.Fatalf("persisted display=%q key=%q err=%v", display, key, err)
			}
			if _, n, _ := setupRow(t, db); n != 1 {
				t.Fatalf("equivalent creates left %d users", n)
			}
		})
	}
	t.Run("bounds and distinctions", func(t *testing.T) {
		db := openStore(t, t.TempDir())
		defer func() { _ = db.Close() }()
		for _, name := range []string{"", string([]byte{0xff}), "bad\x00name", "rtl\u202ename", strings.Repeat("a", 65), strings.Repeat("e\u0301", 33), strings.Repeat("\u0344", 33)} {
			if _, err := create(t.Context(), db, name, RoleMember); !errors.Is(err, ErrUsernameInvalid) {
				t.Errorf("invalid username %q: %v", name, err)
			}
		}
		for i, name := range []string{"resume", "résumé", " owner ", "owner", "K", "Ｋ", strings.Repeat("ß", 64)} {
			u, err := create(t.Context(), db, name, RoleMember)
			if err != nil || u.Username != name || u.ID != int64(i+1) {
				t.Fatalf("distinct/bounded name %q: %+v %v", name, u, err)
			}
		}
		var key string
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), "SELECT username_key FROM user WHERE id=7").Scan(&key)
		}); err != nil || key != strings.Repeat("ss", 64) {
			t.Fatalf("expanded key %q: %v", key, err)
		}
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO user(username, username_key, password_phc, role, created_at) VALUES ('empty-key', '', 'synthetic', 'member', 1)")
			return err
		}); err == nil {
			t.Fatal("database accepted an empty identity key")
		}
		var binaryUnique int
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pragma_index_list('user') i JOIN pragma_index_xinfo(i.name) x WHERE i."unique"=1 AND x.name='username_key' AND x.coll='BINARY' AND x."key"=1`).Scan(&binaryUnique)
		}); err != nil || binaryUnique != 1 {
			t.Fatalf("binary unique identity index count=%d err=%v", binaryUnique, err)
		}
	})
	t.Run("concurrent equivalents", func(t *testing.T) {
		db := openStore(t, t.TempDir())
		defer func() { _ = db.Close() }()
		results := make(chan error, 12)
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Go(func() {
				_, err := create(t.Context(), db, []string{"Straße", "STRASSE"}[i%2], RoleMember)
				results <- err
			})
		}
		wg.Wait()
		close(results)
		created := 0
		for err := range results {
			if err == nil {
				created++
			} else if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				t.Fatalf("unexpected concurrent-create failure: %v", err)
			}
		}
		if _, n, _ := setupRow(t, db); created != 1 || n != 1 {
			t.Fatalf("created=%d users=%d, want one of each", created, n)
		}
	})
	t.Run("rollback and reopen", func(t *testing.T) {
		dir := t.TempDir()
		db := openStore(t, dir)
		defer func() { _ = db.Close() }()
		s := NewSetup(db, NewHasher(), dir)
		if _, err := s.Arm(t.Context(), "synthetic-identity-setup-token"); err != nil {
			t.Fatal(err)
		}
		for _, cancelWrite := range []bool{false, true} {
			ctx, cancel := context.WithCancel(t.Context())
			err := db.Write(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "UPDATE setup SET consumed_at=1 WHERE id=1"); err != nil {
					return err
				}
				role := "invalid-role"
				if cancelWrite {
					role = RoleAdmin
				}
				_, err := createUser(ctx, tx, "e\u0301", "synthetic-prehashed-password", role, time.Unix(1, 0))
				if cancelWrite {
					cancel()
				}
				return err
			})
			cancel()
			if err == nil {
				t.Fatal("failed/cancelled write committed")
			}
			row, n, found := setupRow(t, db)
			if !found || row.ConsumedAt.Valid || n != 0 {
				t.Fatalf("rollback changed setup/users: %+v %d", row, n)
			}
		}
		u, err := s.Create(t.Context(), Verifier("synthetic-identity-setup-token"), "e\u0301", "synthetic-long-password")
		if err != nil || u.Username != "é" || u.ID != 1 {
			t.Fatalf("setup after rollback: %+v %v", u, err)
		}
		before, _, _ := setupRow(t, db)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db = openStore(t, dir)
		if _, err := create(t.Context(), db, "É", RoleMember); err == nil {
			t.Fatal("reopen lost canonical-key uniqueness")
		}
		var display, key string
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), "SELECT username, username_key FROM user WHERE id=1").Scan(&display, &key)
		}); err != nil || display != "é" || key != "é" {
			t.Fatalf("reopen changed the user identity: display=%q key=%q err=%v", display, key, err)
		}
		after, n, found := setupRow(t, db)
		if !found || before != after || !after.ConsumedAt.Valid || n != 1 {
			t.Fatalf("reopen changed consumed setup or users: %+v %+v n=%d", before, after, n)
		}
	})
}
