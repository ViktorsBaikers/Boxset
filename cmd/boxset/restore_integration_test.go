//go:build integration && unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

// syntheticPassword is the fixture password every rehearsal account uses.
const syntheticPassword = "synthetic-rehearsal-password"

// direct opens the installation's database as a fixture writer while its server
// is stopped.
func direct(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", install.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// addAdmin creates one enabled admin in a stopped installation.
func addAdmin(t *testing.T, dir, name string) auth.User {
	t.Helper()
	db, err := store.Open(t.Context(), install.Layout{Dir: dir}.DB(), store.Options{
		BeforeMigrate: func(context.Context, store.Pending) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := auth.CreateUser(t.Context(), db, auth.NewHasher(), name, syntheticPassword, auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return user
}

// publishSnapshot publishes the installation's current database as the
// pre-migration snapshot the store's own hook publishes.
func publishFixture(t *testing.T, dir string) string {
	t.Helper()
	layout := install.Layout{Dir: dir}
	err := install.PublishSnapshot(t.Context(), layout, store.Pending{
		From: 1, To: 1,
		SnapshotInto: func(ctx context.Context, dest string) error {
			db, err := sql.Open("sqlite", layout.DB())
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			conn, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			_, err = conn.ExecContext(ctx, "VACUUM INTO ?", dest)
			return err
		},
	})
	if err != nil {
		t.Fatalf("publish a snapshot: %v", err)
	}
	entries, err := os.ReadDir(layout.Backups())
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pre-migration-") {
			if newest == "" || e.Name() > newest {
				newest = e.Name()
			}
		}
	}
	if newest == "" {
		t.Fatal("no published snapshot")
	}
	return filepath.Join(layout.Backups(), newest)
}

// sum is the lowercase hex SHA-256 of a file.
func sum(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// client is an HTTP client with its own cookie jar.
func client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// setupAdmin runs the first-admin flow with the installation's token and returns
// the credential the server issued.
func setupAdmin(t *testing.T, s *proc, name string) *http.Client {
	t.Helper()
	token, err := os.ReadFile(filepath.Join(dataOf(s), auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	status, body := post(t, c, s.port, "/api/v1/setup", string(token), `{"username":"`+name+`","password":"`+syntheticPassword+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("setup: %d %s", status, body)
	}
	return c
}

// signIn logs one account in and returns the client that carries its cookie.
func signIn(t *testing.T, port, name string) (*http.Client, int) {
	t.Helper()
	c := client(t)
	status, body := post(t, c, port, "/api/v1/session", "", `{"username":"`+name+`","password":"`+syntheticPassword+`"}`)
	if status != http.StatusCreated {
		return c, status
	}
	if body == "" {
		t.Fatal("an empty sign-in body")
	}
	return c, status
}

// post sends one body to a base-relative path.
func post(t *testing.T, c *http.Client, port, path, bearer, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(t, c, req)
}

// get sends one authorized GET.
func get(t *testing.T, c *http.Client, port, path, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	return do(t, c, req)
}

func do(t *testing.T, c *http.Client, req *http.Request) (int, string) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// dataOf recovers the data directory of a running server from its environment.
func dataOf(s *proc) string {
	for _, entry := range s.cmd.Env {
		if after, ok := strings.CutPrefix(entry, "BOXSET_DATA_DIR="); ok {
			return after
		}
	}
	return ""
}

// accountIDs reads the account id of one name from a stopped installation.
func accountIDs(t *testing.T, dir string) map[string]struct {
	ID       int64
	Revision int64
} {
	t.Helper()
	raw := direct(t, dir)
	rows, err := raw.QueryContext(t.Context(), "SELECT id, username, revision FROM user")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	ids := map[string]struct {
		ID       int64
		Revision int64
	}{}
	for rows.Next() {
		var id, revision int64
		var name string
		if err := rows.Scan(&id, &name, &revision); err != nil {
			t.Fatal(err)
		}
		ids[name] = struct {
			ID       int64
			Revision int64
		}{id, revision}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// patchUser sends one authorized account edit as the holder of c.
func patchUser(t *testing.T, c *http.Client, port string, id, revision int64, patch string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"expectedRevision":%d%s}`, revision, patch)
	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("http://127.0.0.1:%s/api/v1/users/%d", port, id), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return do(t, c, req)
}

// TestRestoreRehearsal: an operator restores a snapshot into the stopped
// installation; the accounts and revocations the installation holds now survive,
// every credential from before the restore is denied, and the recovery copy of
// the replaced database stays as it was.
func TestRestoreRehearsal(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	setupAdmin(t, s, "user-u")
	// W is created with the server stopped, as an installation with two admins
	// would hold it.
	terminate(t, s)
	addAdmin(t, dir, "user-w")

	s = start(t, dir)
	s.waitReady(t)
	if _, status := signIn(t, s.port, "user-w"); status != http.StatusCreated {
		t.Fatalf("user-w sign-in: %d", status)
	}
	terminate(t, s)

	// Two API keys for W: one is revoked before the snapshot, the other stays.
	created, code := keyCLI(t, dir, "create --user user-w --name rehearsal-1 --scope admin")
	if code != 0 {
		t.Fatalf("create a key: %d %s", code, created)
	}
	firstKey := keyToken(t, created)
	if id := keyIDFrom(t, dir, "rehearsal-1"); id != "" {
		if _, code := keyCLI(t, dir, "revoke --id "+id); code != 0 {
			t.Fatalf("revoke: %d", code)
		}
	}
	second, code := keyCLI(t, dir, "create --user user-w --name rehearsal-2 --scope admin")
	if code != 0 {
		t.Fatalf("create a second key: %d %s", code, second)
	}
	validKey := keyToken(t, second)
	if firstKey == validKey || len(validKey) < 20 {
		t.Fatal("the two keys are not distinct tokens")
	}

	snapshot := publishFixture(t, dir)
	// A later admin disables U and demotes W to member through the API.
	addAdmin(t, dir, "user-v")
	s = start(t, dir)
	s.waitReady(t)
	adminV, status := signIn(t, s.port, "user-v")
	if status != http.StatusCreated {
		t.Fatalf("user-v sign-in: %d", status)
	}
	ids := map[string]struct {
		ID       int64
		Revision int64
	}{}
	status, body := get(t, adminV, s.port, "/api/v1/users", "")
	if status != http.StatusOK {
		t.Fatalf("list users: %d %s", status, body)
	}
	var page struct {
		Items []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Revision int64  `json:"revision"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		ids[item.Username] = struct {
			ID       int64
			Revision int64
		}{item.ID, item.Revision}
	}
	if status, body := patchUser(t, adminV, s.port, ids["user-u"].ID, ids["user-u"].Revision, `,"disabled":true`); status != http.StatusOK {
		t.Fatalf("disable user-u: %d %s", status, body)
	}
	if status, body := patchUser(t, adminV, s.port, ids["user-w"].ID, ids["user-w"].Revision, `,"role":"member"`); status != http.StatusOK {
		t.Fatalf("demote user-w: %d %s", status, body)
	}
	// A session that predates the restore is denied afterwards.
	terminate(t, s)
	s = start(t, dir)
	s.waitReady(t)
	staleW, status := signIn(t, s.port, "user-w")
	if status != http.StatusCreated {
		t.Fatalf("user-w sign-in before the restore: %d", status)
	}
	terminate(t, s)

	// The restore itself.
	out, code := restoreCLI(t, dir, snapshot)
	if code != 0 {
		t.Fatalf("restore: %d %s", code, out)
	}
	match := regexp.MustCompile(`epoch=(\d+) recovery=(\S+)`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("restore output: %s", out)
	}
	epoch, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || epoch < 2 {
		t.Fatalf("restore epoch %q", match[1])
	}
	recovery := match[2]
	recoverySum := sum(t, recovery)

	s = start(t, dir)
	s.waitReady(t)
	// Every pre-restore credential is denied.
	for _, c := range []struct {
		name   string
		client *http.Client
		key    string
	}{
		{"revoked key", client(t), firstKey},
		{"valid key", client(t), validKey},
		{"session before the restore", staleW, ""},
	} {
		if status, _ := get(t, c.client, s.port, "/api/v1/system/status", c.key); status != http.StatusUnauthorized {
			t.Fatalf("%s: %d after the restore", c.name, status)
		}
	}
	// A fresh sign-in is current authority.
	if _, status := signIn(t, s.port, "user-u"); status != http.StatusUnauthorized {
		t.Fatalf("the disabled account signed in: %d", status)
	}
	freshW, status := signIn(t, s.port, "user-w")
	if status != http.StatusCreated {
		t.Fatalf("user-w fresh sign-in: %d", status)
	}
	if status, body := get(t, freshW, s.port, "/api/v1/system/status", ""); status != http.StatusOK {
		t.Fatalf("user-w status: %d %s", status, body)
	}
	if status, _ := get(t, freshW, s.port, "/api/v1/users", ""); status != http.StatusForbidden {
		t.Fatalf("the demoted account reached users: %d", status)
	}
	freshV, status := signIn(t, s.port, "user-v")
	if status != http.StatusCreated {
		t.Fatalf("user-v fresh sign-in: %d", status)
	}
	if status, _ := get(t, freshV, s.port, "/api/v1/users", ""); status != http.StatusOK {
		t.Fatalf("the later admin cannot read users: %d", status)
	}
	if got := sum(t, recovery); got != recoverySum {
		t.Fatal("the recovery copy changed after the restore")
	}
	terminate(t, s)
}

// keyToken is the one issued key token in a command's merged output.
func keyToken(t *testing.T, output []byte) string {
	t.Helper()
	match := regexp.MustCompile(`bxk_[A-Za-z0-9_-]{43}`).Find(output)
	if match == nil {
		t.Fatalf("no key token in %s", output)
	}
	return string(match)
}

// keyIDFrom reads the id of one named API key from a stopped installation.
func keyIDFrom(t *testing.T, dir, name string) string {
	t.Helper()
	raw := direct(t, dir)
	var id int64
	if err := raw.QueryRowContext(t.Context(), "SELECT id FROM api_key WHERE name = ?", name).Scan(&id); err != nil {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// execCommand is the built binary running one subcommand under ctx.
func execCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, binary, args...)
}

// exitCodeOf is a finished command's exit status.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatalf("run the command: %v", err)
	return 0
}

// restoreCLI runs `boxset restore --from FILE` on a stopped installation.
func restoreCLI(t *testing.T, dir, snapshot string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, "restore", "--from", snapshot)
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), exitCodeOf(t, err)
}

// terminate stops a running server and requires a clean exit.
func terminate(t *testing.T, s *proc) {
	t.Helper()
	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("shutdown: %d", code)
	}
}

// TestRestoreCurrentPolicyFromUncheckpointedWAL: a disable, a demotion and a later
// admin that were committed only to the replaced file's log are current policy
// after the restore, because the recovery copy is taken through the log.
func TestRestoreCurrentPolicyFromUncheckpointedWAL(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	setupAdmin(t, s, "user-u")
	terminate(t, s)
	userW := addAdmin(t, dir, "user-w")
	snapshot := publishFixture(t, dir)

	// Everything below is committed without a checkpoint, so only the log holds it.
	raw, err := sql.Open("sqlite", install.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=wal", "PRAGMA wal_autocheckpoint=0"} {
		if _, err := raw.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var userU int64
	if err := raw.QueryRowContext(t.Context(), "SELECT id FROM user WHERE username = 'user-u'").Scan(&userU); err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewHasher().Hash(t.Context(), syntheticPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		statement string
		args      []any
	}{
		{"UPDATE user SET disabled = 1, revision = revision + 1 WHERE id = ?", []any{userU}},
		{"UPDATE user SET role = 'member', revision = revision + 1 WHERE id = ?", []any{userW.ID}},
		{"INSERT INTO user (username, username_key, password_phc, role, created_at) VALUES ('user-v', 'user-v', ?, 'admin', 1)", []any{verifier}},
	} {
		if _, err := raw.ExecContext(t.Context(), step.statement, step.args...); err != nil {
			t.Fatalf("%s: %v", step.statement, err)
		}
	}
	// The restore runs while that writer's committed frames are still only in the log.
	out, code := restoreCLI(t, dir, snapshot)
	if code != 0 {
		t.Fatalf("restore: %d %s", code, out)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s = start(t, dir)
	s.waitReady(t)
	if _, status := signIn(t, s.port, "user-u"); status != http.StatusUnauthorized {
		t.Fatalf("the account disabled only in the log signed in: %d", status)
	}
	freshW, status := signIn(t, s.port, "user-w")
	if status != http.StatusCreated {
		t.Fatalf("user-w sign-in: %d", status)
	}
	if status, _ := get(t, freshW, s.port, "/api/v1/users", ""); status != http.StatusForbidden {
		t.Fatalf("the demotion committed only in the log was lost: %d", status)
	}
	freshV, status := signIn(t, s.port, "user-v")
	if status != http.StatusCreated {
		t.Fatalf("the admin created only in the log was lost: %d", status)
	}
	if status, _ := get(t, freshV, s.port, "/api/v1/users", ""); status != http.StatusOK {
		t.Fatalf("the retained admin cannot read users: %d", status)
	}
	terminate(t, s)
}

// TestRestoreEmptyDestinationDeniesSnapshotLogin: a snapshot holding an admin
// restored into an empty data directory is refused, and the installation that
// starts there never carries that account.
func TestRestoreEmptyDestinationDeniesSnapshotLogin(t *testing.T) {
	source := t.TempDir()
	s := start(t, source)
	s.waitReady(t)
	setupAdmin(t, s, "user-u")
	terminate(t, s)
	snapshot := publishFixture(t, source)

	destination := t.TempDir()
	out, code := restoreCLI(t, destination, snapshot)
	if code != 2 || !strings.Contains(out, "authorization_state_unavailable") {
		t.Fatalf("restore into an empty directory: %d %s", code, out)
	}
	for _, name := range []string{"boxset.db", "restore.journal"} {
		if _, err := os.Lstat(filepath.Join(destination, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was created by a refused restore", name)
		}
	}
	s = start(t, destination)
	s.waitReady(t)
	if _, status := signIn(t, s.port, "user-u"); status != http.StatusUnauthorized {
		t.Fatalf("the snapshot's account signed in: %d", status)
	}
	terminate(t, s)
	raw := direct(t, destination)
	var rows int
	if err := raw.QueryRowContext(t.Context(), "SELECT count(*) FROM user").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("the new installation holds %d accounts", rows)
	}
	var installID string
	if err := raw.QueryRowContext(t.Context(), "SELECT install_id FROM installation WHERE id = 1").Scan(&installID); err != nil {
		t.Fatal(err)
	}
	if installID == "" || installID == installIdentity(t, snapshot) {
		t.Fatalf("the new installation's identity %q", installID)
	}
}

// TestRestorePreSetupReplay: the setup bearer a snapshot never saw is still closed
// after restoring it, setup never reopens, and a snapshot whose identity the
// current installation cannot vouch for is refused.
func TestRestorePreSetupReplay(t *testing.T) {
	const (
		bearer       = "synthetic-setup-bearer-token"
		unusedBearer = "synthetic-unused-bearer-token"
	)
	dir := t.TempDir()
	// The installation before setup, whose snapshot names it.
	s := startIn(t, "", dir, "BOXSET_SETUP_TOKEN="+unusedBearer)
	s.waitReady(t)
	terminate(t, s)
	presetup := publishFixture(t, dir)
	presetupID := installIdentity(t, presetup)

	// The configured bearer creates V.
	s = startIn(t, "", dir, "BOXSET_SETUP_TOKEN="+bearer)
	s.waitReady(t)
	c := client(t)
	if status, body := post(t, c, s.port, "/api/v1/setup", bearer, `{"username":"user-v","password":"`+syntheticPassword+`"}`); status != http.StatusCreated {
		t.Fatalf("setup with the configured bearer: %d %s", status, body)
	}
	tokenPath := filepath.Join(dir, auth.TokenFileName)
	tokenBefore, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	terminate(t, s)
	setupBefore := setupRow(t, dir)

	out, code := restoreCLI(t, dir, presetup)
	if code != 0 {
		t.Fatalf("restore over the populated installation: %d %s", code, out)
	}
	s = startIn(t, "", dir, "BOXSET_SETUP_TOKEN="+bearer)
	s.waitReady(t)
	c = client(t)
	if status, body := post(t, c, s.port, "/api/v1/setup", bearer, `{"username":"user-w","password":"`+syntheticPassword+`"}`); status != http.StatusConflict {
		t.Fatalf("a replay of the consumed bearer: %d %s", status, body)
	}
	if _, status := signIn(t, s.port, "user-v"); status != http.StatusCreated {
		t.Fatalf("the admin created before the restore: %d", status)
	}
	terminate(t, s)
	if setupAfter := setupRow(t, dir); setupAfter != setupBefore {
		t.Fatalf("setup row %q, want %q", setupAfter, setupBefore)
	}
	if tokenAfter, err := os.ReadFile(tokenPath); err != nil || !bytes.Equal(tokenAfter, tokenBefore) {
		t.Fatal("the setup token file was rewritten")
	}
	if ids := accountIDs(t, dir); len(ids) != 1 {
		t.Fatalf("accounts after the restore: %v", ids)
	}

	// The current database is lost: absent, and separately recreated empty.
	for _, c := range []struct {
		name  string
		apply func(string)
	}{
		{"absent", func(dir string) { _ = os.Remove(install.Layout{Dir: dir}.DB()) }},
		{"zero length", func(dir string) {
			if err := os.WriteFile(install.Layout{Dir: dir}.DB(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lost := t.TempDir()
			c.apply(lost)
			out, code := restoreCLI(t, lost, presetup)
			if code != 2 || !strings.Contains(out, "authorization_state_unavailable") {
				t.Fatalf("restore: %d %s", code, out)
			}
			if _, err := os.Lstat(filepath.Join(lost, "restore.journal")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused restore left a journal")
			}
			if c.name == "absent" {
				if _, err := os.Lstat(install.Layout{Dir: lost}.DB()); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("a refused restore created a database")
				}
			} else if info, err := os.Stat(install.Layout{Dir: lost}.DB()); err != nil || info.Size() != 0 {
				t.Fatal("a refused restore changed the empty database")
			}
			// A later start there holds a new identity, never the snapshot's.
			s := start(t, lost)
			s.waitReady(t)
			terminate(t, s)
			if got := installIdentity(t, install.Layout{Dir: lost}.DB()); got == presetupID {
				t.Fatalf("the new installation kept the snapshot's identity %q", got)
			}
		})
	}

	t.Run("another installation", func(t *testing.T) {
		other := t.TempDir()
		raw, err := store.Open(t.Context(), install.Layout{Dir: other}.DB(), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		before := sum(t, install.Layout{Dir: other}.DB())
		out, code := restoreCLI(t, other, presetup)
		if code != 2 || !strings.Contains(out, "installation_mismatch") {
			t.Fatalf("restore over another installation: %d %s", code, out)
		}
		if after := sum(t, install.Layout{Dir: other}.DB()); after != before {
			t.Fatal("a refused restore replaced the other installation")
		}
	})

	t.Run("never set up", func(t *testing.T) {
		control := t.TempDir()
		s := start(t, control)
		s.waitReady(t)
		terminate(t, s)
		own := publishFixture(t, control)
		out, code := restoreCLI(t, control, own)
		if code != 0 {
			t.Fatalf("a never-set-up installation restoring its own snapshot: %d %s", code, out)
		}
		s = start(t, control)
		s.waitReady(t)
		token, err := os.ReadFile(filepath.Join(control, auth.TokenFileName))
		if err != nil {
			t.Fatal(err)
		}
		if status, body := post(t, client(t), s.port, "/api/v1/setup", string(token), `{"username":"user-x","password":"`+syntheticPassword+`"}`); status != http.StatusCreated {
			t.Fatalf("setup after the control restore is closed: %d %s", status, body)
		}
		terminate(t, s)
	})
}

// restoreJournal leaves the journal an interrupted restore wrote in the data
// directory.
func restoreJournal(t *testing.T, dir string, r install.Recovery) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(install.Layout{Dir: dir}.Journal(), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stageSnapshot stages snapshot where a restore keeps it until the replacement.
func stageSnapshot(t *testing.T, dir, snapshot string) {
	t.Helper()
	body, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(install.Layout{Dir: dir}.RestoreStaged(), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// vacuumCopy writes a database's committed state, log included, to dest.
func vacuumCopy(t *testing.T, src, dest string) {
	t.Helper()
	raw, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), "VACUUM INTO ?", dest); err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

// TestRestoreStartResumesJournal: a start over a journal an interrupted restore
// left discards a replacement that was never committed to, completes the one
// that was, and refuses to serve an installation whose reconciliation cannot
// finish.
func TestRestoreStartResumesJournal(t *testing.T) {
	t.Run("before replacement", func(t *testing.T) {
		dir := t.TempDir()
		s := start(t, dir)
		s.waitReady(t)
		setupAdmin(t, s, "user-u")
		terminate(t, s)

		layout := install.Layout{Dir: dir}
		if err := os.MkdirAll(layout.RecoveryDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		restoreJournal(t, dir, install.Recovery{
			Phase:          install.PhaseBeforeReplace,
			SnapshotSHA256: strings.Repeat("0", 64),
			RecoveryPath:   filepath.Join(layout.RecoveryDir(), "restore-abandoned.db"),
			Epoch:          2,
		})
		if err := os.WriteFile(layout.RestoreStaged(), []byte("synthetic abandoned staging"), 0o600); err != nil {
			t.Fatal(err)
		}

		s = start(t, dir)
		s.waitReady(t)
		// The leftovers are gone, so neither a later restore nor a later start
		// trips over a replacement that never happened.
		for _, path := range []string{layout.Journal(), layout.RestoreStaged()} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s survived the start", filepath.Base(path))
			}
		}
		if _, status := signIn(t, s.port, "user-u"); status != http.StatusCreated {
			t.Fatalf("sign-in after a discarded journal: %d", status)
		}
		terminate(t, s)
	})

	t.Run("reconcile", func(t *testing.T) {
		dir := t.TempDir()
		s := start(t, dir)
		s.waitReady(t)
		setupAdmin(t, s, "user-u")
		terminate(t, s)
		addAdmin(t, dir, "user-w")
		snapshot := publishFixture(t, dir)

		// The installation moves on after the snapshot: V is a newer admin and W
		// is gone, so the retained authority and the snapshot disagree both ways.
		layout := install.Layout{Dir: dir}
		addAdmin(t, dir, "user-v")
		raw, err := sql.Open("sqlite", layout.DB())
		if err != nil {
			t.Fatal(err)
		}
		raw.SetMaxOpenConns(1)
		if _, err := raw.ExecContext(t.Context(), "DELETE FROM user WHERE username = 'user-w'"); err != nil {
			t.Fatalf("drop user-w: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		recovery := filepath.Join(layout.RecoveryDir(), "restore-pinned.db")
		if err := os.MkdirAll(layout.RecoveryDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		vacuumCopy(t, layout.DB(), recovery)
		restoreJournal(t, dir, install.Recovery{
			Phase:          install.PhaseReconcile,
			SnapshotSHA256: sum(t, snapshot),
			RecoveryPath:   recovery,
			RecoverySHA256: sum(t, recovery),
			Epoch:          2,
		})
		stageSnapshot(t, dir, snapshot)

		s = start(t, dir)
		s.waitReady(t)
		// The journal is finished: the snapshot's file is in place, W lost its
		// access and the installation's own V survived.
		for _, path := range []string{layout.Journal(), layout.RestoreStaged()} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s survived the resumed reconcile", filepath.Base(path))
			}
		}
		if _, status := signIn(t, s.port, "user-w"); status != http.StatusUnauthorized {
			t.Fatalf("the snapshot-only account signed in: %d", status)
		}
		if _, status := signIn(t, s.port, "user-u"); status != http.StatusCreated {
			t.Fatalf("user-u sign-in: %d", status)
		}
		adminV, status := signIn(t, s.port, "user-v")
		if status != http.StatusCreated {
			t.Fatalf("user-v sign-in: %d", status)
		}
		if status, body := get(t, adminV, s.port, "/api/v1/users", ""); status != http.StatusOK {
			t.Fatalf("the retained admin cannot read users: %d %s", status, body)
		}
		terminate(t, s)
		if epoch := restoreEpoch(t, dir); epoch != 2 {
			t.Fatalf("the resumed reconcile left the restore epoch at %d", epoch)
		}
		accounts := accountIDs(t, dir)
		if _, ok := accounts["user-w"]; !ok {
			t.Fatalf("the replacement never put the snapshot's file in place: %v", accounts)
		}
	})

	t.Run("unfinished reconcile", func(t *testing.T) {
		dir := t.TempDir()
		s := start(t, dir)
		s.waitReady(t)
		setupAdmin(t, s, "user-u")
		terminate(t, s)
		addAdmin(t, dir, "user-w")
		snapshot := publishFixture(t, dir)

		layout := install.Layout{Dir: dir}
		recovery := filepath.Join(layout.RecoveryDir(), "restore-unpinned.db")
		vacuumCopy(t, layout.DB(), recovery)
		// The pinned copy is not the one the journal names, so the resume cannot
		// trust what it would apply and must not touch the installation.
		restoreJournal(t, dir, install.Recovery{
			Phase:          install.PhaseReconcile,
			SnapshotSHA256: sum(t, snapshot),
			RecoveryPath:   recovery,
			RecoverySHA256: strings.Repeat("0", 64),
			Epoch:          2,
		})
		stageSnapshot(t, dir, snapshot)

		s = start(t, dir)
		if state := awaitSettledState(t, s, 30*time.Second); state != "recovering" {
			t.Fatalf("a failing resume started %q", state)
		}
		if status, body := post(t, client(t), s.port, "/api/v1/session", "", `{"username":"user-u","password":"`+syntheticPassword+`"}`); status != http.StatusServiceUnavailable {
			t.Fatalf("login while the resume is unfinished: %d %s", status, body)
		}
		if status, _ := get(t, client(t), s.port, "/api/v1/system/status", ""); status != http.StatusUnauthorized {
			t.Fatalf("a credential was served an unfinished resume: %d", status)
		}
		terminate(t, s)
		for _, path := range []string{layout.Journal(), layout.RestoreStaged()} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("%s was removed by a resume that did not finish", filepath.Base(path))
			}
		}
		if accounts := accountIDs(t, dir); len(accounts) != 2 {
			t.Fatalf("a failing resume replaced the database: %v", accounts)
		}
		// The start parks in a state no credential and no restore command may pass,
		// so the line it logs has to name the cause and not only the reason word.
		logged := ""
		for _, line := range strings.Split(s.stderr.String(), "\n") {
			if strings.Contains(line, "restore did not finish") {
				logged = line
			}
		}
		if !strings.Contains(logged, "reason=restore_in_progress") || !strings.Contains(logged, "error=") {
			t.Fatalf("the start did not log why the resume stopped: %q", logged)
		}
	})
}

// TestServeRefusesPendingJournalWhereRestoreIsUnsupported: a start that finds a
// restore journal on a platform without the namespace barrier exits 3
// restore_unsupported before the store is opened, so it never serves half a
// restore; the journal, the staged copy and the database path stay as they were.
// The platform result is stubbed to what Windows returns, because this platform
// has no way to produce it and the refusal is what this level owns.
func TestServeRefusesPendingJournalWhereRestoreIsUnsupported(t *testing.T) {
	for _, phase := range []install.Phase{install.PhaseBeforeReplace, install.PhaseReconcile} {
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			layout := install.Layout{Dir: dir}
			restoreJournal(t, dir, install.Recovery{
				Phase:          phase,
				SnapshotSHA256: strings.Repeat("0", 64),
				RecoveryPath:   filepath.Join(layout.RecoveryDir(), "restore-pinned.db"),
				RecoverySHA256: strings.Repeat("0", 64),
				Epoch:          2,
			})
			journal, err := os.ReadFile(layout.Journal())
			if err != nil {
				t.Fatal(err)
			}

			real := resumeRestore
			resumeRestore = func(context.Context, install.Layout, install.RestoreOptions) (install.Recovery, error) {
				return install.Recovery{}, install.ErrRestoreUnsupported
			}
			t.Cleanup(func() { resumeRestore = real })

			_, exit, stderr := serveHere(t, dir)
			if code := awaitExit(t, exit); code != 3 {
				t.Fatalf("serve over a %s journal returned %d, want exit 3: %s", phase, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "restore_unsupported") {
				t.Fatalf("serve over a %s journal did not name the reason: %s", phase, stderr.String())
			}
			if got, err := os.ReadFile(layout.Journal()); err != nil || !bytes.Equal(got, journal) {
				t.Fatalf("the %s journal was resumed or discarded: %v", phase, err)
			}
			for _, path := range []string{layout.RestoreStaged(), layout.DB(), filepath.Join(layout.RecoveryDir(), "restore-pinned.db")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s was created by a refused start", filepath.Base(path))
				}
			}
		})
	}
}

// installIdentity reads the install_id of a database file.
func installIdentity(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var id string
	if err := raw.QueryRowContext(t.Context(), "SELECT install_id FROM installation WHERE id = 1").Scan(&id); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return id
}

// setupRow is the stored first-admin credential of a stopped installation.
func setupRow(t *testing.T, dir string) string {
	t.Helper()
	raw := direct(t, dir)
	var token string
	var consumed sql.NullInt64
	if err := raw.QueryRowContext(t.Context(), "SELECT token_sha256, consumed_at FROM setup WHERE id = 1").Scan(&token, &consumed); err != nil {
		t.Fatalf("setup row: %v", err)
	}
	return fmt.Sprintf("%s/%d", token, consumed.Int64)
}

// TestSecretReenterCLI: a record whose key file is lost is re-entered from a file
// while the installation is stopped, after which the installation reaches ready
// with the old version byte-identical, and the supplied plaintext never appears in
// the command line, its output or a log.
func TestSecretReenterCLI(t *testing.T) {
	const sentinel = "synthetic-reentered-credential"
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	setupAdmin(t, s, "user-u")
	terminate(t, s)

	// The installation's key and one encrypted record, as a provider credential
	// would be stored once its slice lands.
	db, err := store.Open(t.Context(), install.Layout{Dir: dir}.DB(), store.Options{
		BeforeMigrate: func(context.Context, store.Pending) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := secret.New(db, dir)
	keyID, key, err := keys.MintKey()
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := secret.Seal(key, 1, 1, "indexer", []byte("synthetic-original-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		q := storedb.New(tx)
		if err := q.InsertSecretVersion(t.Context(), storedb.InsertSecretVersionParams{
			ID: 1, Version: 1, Purpose: "indexer", KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext, IsCurrent: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		return q.SetInstallationKey(t.Context(), keyID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	versionBefore, keyBefore, nonceBefore, ciphertextBefore := secretRow(t, dir)

	// While a server runs, the command is refused by the lifetime lock.
	s = start(t, dir)
	s.waitReady(t)
	if out, code := reenterCLI(t, dir, 1, writeInput(t, t.TempDir(), sentinel)); code != 3 || !strings.Contains(out, "lock_held") {
		t.Fatalf("re-entry beside a running server: %d %s", code, out)
	}
	terminate(t, s)

	// The key file is lost, so the record waits for an explicit re-entry.
	if err := os.Remove(secret.KeyPath(dir, keyID)); err != nil {
		t.Fatal(err)
	}
	s = start(t, dir)
	if state := awaitSettledState(t, s, 30*time.Second); state != "recovering" {
		t.Fatalf("a missing key started %q", state)
	}
	if !strings.Contains(s.stderr.String(), "reason=key_missing") {
		t.Fatalf("the start did not name the reason: %s", s.stderr.String())
	}
	terminate(t, s)

	input := writeInput(t, t.TempDir(), sentinel)
	out, code := reenterCLI(t, dir, 1, input)
	if code != 0 || !strings.Contains(out, "reentered id=1 version=2") {
		t.Fatalf("re-entry: %d %s", code, out)
	}
	for _, read := range []string{out, s.stderr.String()} {
		if strings.Contains(read, sentinel) {
			t.Fatal("the re-entered plaintext reached a command line, an output or a log")
		}
	}

	version, keyIDAfter, _, _ := secretRow(t, dir)
	if version != 2 || keyIDAfter == keyID {
		t.Fatalf("re-entry version %d key %q", version, keyIDAfter)
	}
	retired := oldVersion(t, dir, 1, versionBefore)
	if retired.KeyID != keyBefore || !bytes.Equal(retired.Nonce, nonceBefore) || !bytes.Equal(retired.Ciphertext, ciphertextBefore) {
		t.Fatal("re-entry rewrote the version it retired")
	}
	published := mustKey(t, dir, keyIDAfter)
	current := currentVersion(t, dir, 1)
	plaintext, err := secret.Open(published, 1, current.Version, current.Purpose, current.Nonce, current.Ciphertext)
	if err != nil || string(plaintext) != sentinel {
		t.Fatalf("the re-entered version does not open to the supplied bytes: %v", err)
	}
	// Re-entry touches no credential state.
	if epoch := restoreEpoch(t, dir); epoch != 1 {
		t.Fatalf("re-entry changed the restore epoch to %d", epoch)
	}

	s = start(t, dir)
	s.waitReady(t)
	terminate(t, s)
}

// TestSecretReenterReusesCurrentKey: a record whose current key is still usable is
// re-entered under that same key, and a Write that does not commit leaves neither a
// version nor a moved installation key.
func TestSecretReenterReusesCurrentKey(t *testing.T) {
	const (
		first  = "synthetic-reused-key-credential"
		second = "synthetic-abandoned-credential"
	)
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	setupAdmin(t, s, "user-u")
	terminate(t, s)

	// The installation's key and one encrypted record, as TestSecretReenterCLI
	// plants them; this case keeps the key file it published.
	db, err := store.Open(t.Context(), install.Layout{Dir: dir}.DB(), store.Options{
		BeforeMigrate: func(context.Context, store.Pending) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := secret.New(db, dir)
	keyID, key, err := keys.MintKey()
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := secret.Seal(key, 1, 1, "indexer", []byte("synthetic-original-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		q := storedb.New(tx)
		if err := q.InsertSecretVersion(t.Context(), storedb.InsertSecretVersionParams{
			ID: 1, Version: 1, Purpose: "indexer", KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext, IsCurrent: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		return q.SetInstallationKey(t.Context(), keyID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	versionBefore, keyBefore, nonceBefore, ciphertextBefore := secretRow(t, dir)
	installed := installationKey(t, dir)
	keySum := sum(t, secret.KeyPath(dir, keyID))

	out, code := reenterCLI(t, dir, 1, writeInput(t, t.TempDir(), first))
	if code != 0 || !strings.Contains(out, "reentered id=1 version=2") {
		t.Fatalf("re-entry under the current key: %d %s", code, out)
	}
	// Version 2 is written under the key the installation already pointed at.
	if got := installationKey(t, dir); got != installed || got != keyID {
		t.Fatalf("re-entry moved the installation key from %q to %q", installed, got)
	}
	version, keyIDAfter, _, _ := secretRow(t, dir)
	if version != 2 || keyIDAfter != keyID {
		t.Fatalf("re-entry wrote version %d under key %q, want version 2 under %q", version, keyIDAfter, keyID)
	}
	retired := oldVersion(t, dir, 1, versionBefore)
	if retired.KeyID != keyBefore || !bytes.Equal(retired.Nonce, nonceBefore) || !bytes.Equal(retired.Ciphertext, ciphertextBefore) {
		t.Fatal("re-entry rewrote the version it retired")
	}
	if got := sum(t, secret.KeyPath(dir, keyID)); got != keySum {
		t.Fatal("re-entry rewrote the key it reused")
	}
	current := currentVersion(t, dir, 1)
	plaintext, err := secret.Open(mustKey(t, dir, keyID), 1, current.Version, current.Purpose, current.Nonce, current.Ciphertext)
	if err != nil || string(plaintext) != first {
		t.Fatalf("the re-entered version does not open to the supplied bytes: %v", err)
	}

	// A Write that does not commit leaves the record where it was and points the
	// installation at the same key.
	raw := direct(t, dir)
	if _, err := raw.ExecContext(t.Context(), "CREATE TRIGGER synthetic_reenter_fault BEFORE INSERT ON secret BEGIN SELECT RAISE(ABORT, 'synthetic fault'); END"); err != nil {
		t.Fatalf("fail the next Write: %v", err)
	}
	out, code = reenterCLI(t, dir, 1, writeInput(t, t.TempDir(), second))
	if code != 3 || !strings.Contains(out, "secret_reenter_failed") {
		t.Fatalf("a re-entry that cannot commit: %d %s", code, out)
	}
	if strings.Contains(out, second) {
		t.Fatal("the refused plaintext reached the output")
	}
	if got := installationKey(t, dir); got != installed {
		t.Fatalf("a failed re-entry moved the installation key to %q", got)
	}
	version, keyIDAfter, _, _ = secretRow(t, dir)
	if version != 2 || keyIDAfter != keyID {
		t.Fatalf("a failed re-entry left version %d under key %q", version, keyIDAfter)
	}
	if got := sum(t, secret.KeyPath(dir, keyID)); got != keySum {
		t.Fatal("a failed re-entry rewrote the key")
	}
}

// installationKey reads the key the installation names for its next write.
func installationKey(t *testing.T, dir string) string {
	t.Helper()
	raw := direct(t, dir)
	var id string
	if err := raw.QueryRowContext(t.Context(), "SELECT key_id FROM installation_key WHERE id = 1").Scan(&id); err != nil {
		t.Fatalf("installation key: %v", err)
	}
	return id
}

// awaitState waits for the first lifecycle state a start reports.
func awaitSettledState(t *testing.T, s *proc, bound time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		// A start reports "starting" until the store and its keys decided the
		// state, so only a settled state answers here.
		if state := s.ping(); state != "" && state != "starting" {
			return state
		}
		select {
		case <-s.done:
			t.Fatalf("the server exited: %v: %s", s.err, s.stderr.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no lifecycle state within %s: %s", bound, s.stderr.String())
	return ""
}

// reenterCLI runs `boxset secret reenter --id ID --from FILE`.
func reenterCLI(t *testing.T, dir string, id int64, input string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := execCommand(ctx, "secret", "reenter", "--id", strconv.FormatInt(id, 10), "--from", input)
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), exitCodeOf(t, err)
}

// writeInput writes a re-entry file only its owner can read.
func writeInput(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "credential")
	if err := install.WriteSecretFile(path, []byte(content)); err != nil {
		t.Fatal(err)
	}
	return path
}

// secretRow is the current version of record 1.
func secretRow(t *testing.T, dir string) (version int64, keyID string, nonce, ciphertext []byte) {
	t.Helper()
	current := currentVersion(t, dir, 1)
	return current.Version, current.KeyID, current.Nonce, current.Ciphertext
}

// currentVersion reads record 1's current version.
func currentVersion(t *testing.T, dir string, id int64) storedb.GetCurrentSecretRow {
	t.Helper()
	raw := direct(t, dir)
	var row storedb.GetCurrentSecretRow
	err := raw.QueryRowContext(t.Context(), "SELECT id, version, purpose, key_id, nonce, ciphertext FROM secret WHERE id = ? AND is_current = 1", id).
		Scan(&row.ID, &row.Version, &row.Purpose, &row.KeyID, &row.Nonce, &row.Ciphertext)
	if err != nil {
		t.Fatalf("current version of record %d: %v", id, err)
	}
	return row
}

// oldVersion reads one retired version of record 1.
func oldVersion(t *testing.T, dir string, id, version int64) storedb.GetCurrentSecretRow {
	t.Helper()
	raw := direct(t, dir)
	var row storedb.GetCurrentSecretRow
	err := raw.QueryRowContext(t.Context(), "SELECT id, version, purpose, key_id, nonce, ciphertext FROM secret WHERE id = ? AND version = ?", id, version).
		Scan(&row.ID, &row.Version, &row.Purpose, &row.KeyID, &row.Nonce, &row.Ciphertext)
	if err != nil {
		t.Fatalf("version %d of record %d: %v", version, id, err)
	}
	return row
}

// mustKey reads one published key.
func mustKey(t *testing.T, dir, keyID string) []byte {
	t.Helper()
	path := secret.KeyPath(dir, keyID)
	if err := install.VerifySecretFile(path, keyID); err != nil {
		t.Fatalf("the published key: %v", err)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// restoreEpoch reads the installation's current restore epoch.
func restoreEpoch(t *testing.T, dir string) int64 {
	t.Helper()
	raw := direct(t, dir)
	var epoch int64
	if err := raw.QueryRowContext(t.Context(), "SELECT restore_epoch FROM installation WHERE id = 1").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}
