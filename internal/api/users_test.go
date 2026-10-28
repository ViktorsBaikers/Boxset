package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

type userService struct {
	fakeService
	db       *store.DB
	sessions *auth.Sessions
	schema   int64
}

func (s *userService) ListUsers(ctx context.Context, q auth.UserQuery, visit func(auth.UserCollection) error) error {
	return s.sessions.ListUsers(ctx, q, func(c auth.UserCollection) error { c.SchemaVersion = s.schema; return visit(c) })
}

// UpdateUser goes through the trusted fixture seam: this fixture serves the
// operations without the server's guard, so it holds no request actor. The
// use-time actor recheck is proved on real HTTP in the server package.
func (s *userService) UpdateUser(ctx context.Context, _ auth.Actor, id int64, p auth.UserPatch) (auth.User, error) {
	return auth.UpdateUser(ctx, s.db, id, p)
}

func usersFixture(t *testing.T, names ...string) (*userService, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hasher := auth.NewHasher()
	for _, name := range names {
		if _, err := auth.CreateUser(t.Context(), db, hasher, name, "synthetic-valid-password", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}
	svc := &userService{db: db, sessions: auth.NewSetup(db, hasher, dir).Sessions(), schema: 2}
	mux := http.NewServeMux()
	New(mux, svc)
	return svc, mux
}

var userRights = []string{auth.RightStatusRead, auth.RightUsersRead, auth.RightUsersWrite}

func usersRequest(t *testing.T, h http.Handler, method, target, body, validator string, rights []string, credential string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, Prefix+target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-None-Match", validator)
	r.Header.Set("If-Match", `W/"deliberately-ignored"`)
	ctx := auth.WithPrincipal(r.Context(), auth.Principal{Rights: rights})
	ctx = auth.WithSession(ctx, auth.Session{Credential: credential})
	ctx = auth.WithActor(ctx, auth.ActorOf(auth.Session{Credential: credential, User: auth.User{ID: 1}}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	return w
}

func pageFrom(t *testing.T, w *httptest.ResponseRecorder) UserPage {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("page status=%d body=%s", w.Code, w.Body.String())
	}
	var p UserPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code, reason string) {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || p.Code != code || (reason != "" && p.Params["reason"] != reason) {
		t.Fatalf("status=%d body=%s want=%d/%s/%s", w.Code, w.Body.String(), status, code, reason)
	}
}

func TestListLimits(t *testing.T) {
	names := make([]string, 55)
	for i := range names {
		names[i] = fmt.Sprintf("user-%03d", i)
	}
	service, mux := usersFixture(t, names...)
	if err := service.ListUsers(t.Context(), auth.UserQuery{Limit: 50, Sort: "username", Order: "asc"}, func(c auth.UserCollection) error {
		if len(c.Items) != 51 || c.Revision != 56 {
			t.Fatalf("bounded snapshot rows=%d revision=%d", len(c.Items), c.Revision)
		}
		return nil
	}); err != nil {
		t.Fatalf("read users snapshot: %v", err)
	}
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", 200}, {"?limit=500", 200}, {"?limit=501", 422},
	} {
		w := usersRequest(t, mux, "GET", "/users"+tc.query, "", "", userRights, "cookie")
		if w.Code != tc.status {
			t.Errorf("query %q: got %d, want %d: %s", tc.query, w.Code, tc.status, w.Body.String())
		}
		if tc.status == 200 {
			p := pageFrom(t, w)
			want := 50
			if tc.query != "" {
				want = 55
			}
			if len(p.Items) != want {
				t.Fatalf("rows=%d want=%d", len(p.Items), want)
			}
		}
	}
	_, unicodeMux := usersFixture(t, "Émilie", "Straße", "STRASSEN", "Emilie", strings.Repeat("ß", 64), "wild%_card")
	for _, tc := range []struct {
		search string
		names  []string
	}{
		{"E\u0301MILIE", []string{"Émilie"}}, {"éMILIE", []string{"Émilie"}}, {"Emilie", []string{"Emilie"}},
		{"STRASSE", []string{"STRASSEN", "Straße"}}, {"%_", []string{"wild%_card"}}, {"absent", []string{}},
		{strings.Repeat("ß", 64), []string{strings.Repeat("ß", 64)}},
	} {
		p := pageFrom(t, usersRequest(t, unicodeMux, "GET", "/users?q="+url.QueryEscape(tc.search), "", "", userRights, "cookie"))
		names := []string{}
		for _, u := range p.Items {
			names = append(names, u.Username)
		}
		if !reflect.DeepEqual(names, tc.names) {
			t.Fatalf("q=%q got=%v want=%v", tc.search, names, tc.names)
		}
	}
	for _, query := range []string{"?limit=0", "?sort=invalid", "?order=invalid", "?role=invalid", "?q=" + url.QueryEscape("bad\u202ename"), "?q=" + strings.Repeat("a", 65), "?cursor=" + strings.Repeat("a", 1025)} {
		assertProblem(t, usersRequest(t, unicodeMux, "GET", "/users"+query, "", "", userRights, "cookie"), 422, "validation_failed", "")
	}
}

func TestNoUserCreateRoute(t *testing.T) {
	_, h := usersFixture(t)
	w := usersRequest(t, h, "POST", "/users", `{}`, "", userRights, "cookie")
	if w.Code != 405 {
		t.Fatalf("create route status=%d", w.Code)
	}
}

func TestSameRevisionRace(t *testing.T) {
	_, h := usersFixture(t, "first", "second")
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, role := range []string{"member", "viewer"} {
		wg.Go(func() {
			<-start
			results <- usersRequest(t, h, "PATCH", "/users/2", `{"expectedRevision":1,"role":"`+role+`"}`, "", userRights, "cookie")
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winner User
	conflicts := 0
	successes := 0
	for w := range results {
		if w.Code == 200 {
			successes++
			if err := json.Unmarshal(w.Body.Bytes(), &winner); err != nil {
				t.Fatal(err)
			}
			if winner.Revision != 2 {
				t.Fatalf("winner revision: status=%d revision=%d successes=%d conflicts=%d", w.Code, winner.Revision, successes, conflicts)
			}
		} else {
			assertProblem(t, w, 409, "revision_conflict", "")
			var p Problem
			_ = json.Unmarshal(w.Body.Bytes(), &p)
			if p.CurrentRevision != 2 {
				t.Fatal("conflict current revision")
			}
			conflicts++
		}
	}
	if winner.ID != 2 || conflicts != 1 {
		t.Fatalf("expected one winner and one conflict: winner_id=%d successes=%d conflicts=%d", winner.ID, successes, conflicts)
	}
	p := pageFrom(t, usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie"))
	if p.Items[1] != winner {
		t.Fatal("final row differs from winner")
	}
	filtered := pageFrom(t, usersRequest(t, h, "GET", "/users?role="+winner.Role, "", "", userRights, "cookie"))
	if len(filtered.Items) != 1 || filtered.Items[0] != winner {
		t.Fatal("role filter differs from committed winner")
	}
	t.Logf("REVISION-RACE winner_revision=%d conflicts=%d final_matches=true", winner.Revision, conflicts)
}

func TestLastAdminGuard(t *testing.T) {
	s, h := usersFixture(t, "only")
	for _, patch := range []string{`{"expectedRevision":1,"role":"member"}`, `{"expectedRevision":1,"disabled":true}`} {
		assertProblem(t, usersRequest(t, h, "PATCH", "/users/1", patch, "", userRights, "cookie"), 409, "last_admin", "")
	}
	unchanged := pageFrom(t, usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie"))
	if unchanged.CollectionRevision != 2 || unchanged.Items[0].Revision != 1 || unchanged.Items[0].Role != "admin" || unchanged.Items[0].Disabled {
		t.Fatal("rejected last-admin edits changed state")
	}
	assertProblem(t, usersRequest(t, h, "PATCH", "/users/1", `{"expectedRevision":1,"disabled":true}`, "", []string{auth.RightUsersRead}, "cookie"), 403, "forbidden", "")
	for _, tc := range []struct {
		id, patch, code string
		status          int
	}{
		{"1", `{"expectedRevision":1}`, "patch_empty", 422}, {"9", `{"expectedRevision":1,"disabled":true}`, "user_not_found", 404},
		{"1", `{"expectedRevision":0,"role":"admin"}`, "validation_failed", 422}, {"1", `{"expectedRevision":1,"username":"changed"}`, "validation_failed", 422}, {"1", `{"expectedRevision":1,"role":"invalid"}`, "validation_failed", 422},
	} {
		assertProblem(t, usersRequest(t, h, "PATCH", "/users/"+tc.id, tc.patch, "", userRights, "cookie"), tc.status, tc.code, "")
	}
	if _, err := auth.CreateUser(t.Context(), s.db, auth.NewHasher(), "other", "synthetic-valid-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"1", "2"} {
		wg.Go(func() {
			<-start
			results <- usersRequest(t, h, "PATCH", "/users/"+id, `{"expectedRevision":1,"disabled":true}`, "", userRights, "cookie")
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for w := range results {
		if w.Code == 200 {
			success++
		} else {
			assertProblem(t, w, 409, "last_admin", "")
			refused++
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("success=%d refused=%d", success, refused)
	}
	page := pageFrom(t, usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie"))
	active := 0
	for _, user := range page.Items {
		if !user.Disabled {
			active++
			continue
		}
		w := usersRequest(t, h, "PATCH", fmt.Sprintf("/users/%d", user.ID), `{"expectedRevision":2,"disabled":false}`, "", userRights, "cookie")
		if w.Code != 200 {
			t.Fatal("explicit false edit failed: " + w.Body.String())
		}
	}
	if active != 1 {
		t.Fatalf("active admins=%d", active)
	}
}

func TestCursorBinding(t *testing.T) {
	s, h := usersFixture(t, "A", "B", "C", strings.Repeat("ß", 64))
	otherService, other := usersFixture(t, "A", "B", "C", strings.Repeat("ß", 64))
	for _, service := range []*userService{s, otherService} {
		if err := service.db.Write(t.Context(), func(tx *sql.Tx) error {
			for i, createdAt := range []int64{1700000300, 1700000100, 1700000100, 1700000200} {
				if _, err := tx.Exec("UPDATE user SET created_at=? WHERE id=?", createdAt, i+1); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := pageFrom(t, usersRequest(t, h, "GET", "/users?limit=1", "", "", userRights, "cookie"))
	base, err := decodeUserCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	request := func(h http.Handler, cursor string) *httptest.ResponseRecorder {
		return usersRequest(t, h, "GET", "/users?limit=1&cursor="+url.QueryEscape(cursor), "", "", userRights, "cookie")
	}
	for _, invalid := range []string{"!", base64.RawURLEncoding.EncodeToString([]byte("not JSON"))} {
		assertProblem(t, request(h, invalid), 400, "cursor_invalid", "")
	}
	assertProblem(t, request(other, first.NextCursor), 400, "cursor_invalid", "")
	if err := otherService.db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE installation SET restore_epoch=restore_epoch+1"); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE user SET revision=revision+1 WHERE id=1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, request(other, first.NextCursor), 400, "cursor_invalid", "")
	for _, tc := range []struct {
		name         string
		mutate       func(*userCursor)
		status       int
		code, reason string
	}{
		{"installation", func(c *userCursor) { c.InstallID = "other" }, 400, "cursor_invalid", ""},
		{"query", func(c *userCursor) { c.Query = "other" }, 400, "cursor_invalid", ""},
		{"scope", func(c *userCursor) { c.Scope = "other" }, 400, "cursor_invalid", ""},
		{"installation plus epoch revision", func(c *userCursor) { c.InstallID = "other"; c.RestoreEpoch++; c.CollectionRevision++ }, 400, "cursor_invalid", ""},
		{"query plus epoch revision", func(c *userCursor) { c.Query = "other"; c.RestoreEpoch++; c.CollectionRevision++ }, 400, "cursor_invalid", ""},
		{"scope plus epoch revision", func(c *userCursor) { c.Scope = "other"; c.RestoreEpoch++; c.CollectionRevision++ }, 400, "cursor_invalid", ""},
		{"epoch", func(c *userCursor) { c.RestoreEpoch++ }, 409, "cursor_stale", "restore_epoch"},
		{"epoch before revision", func(c *userCursor) { c.RestoreEpoch++; c.CollectionRevision++ }, 409, "cursor_stale", "restore_epoch"},
		{"schema", func(c *userCursor) { c.SchemaVersion++ }, 409, "cursor_stale", "schema_version"},
		{"version", func(c *userCursor) { c.V++ }, 409, "cursor_stale", "cursor_version"},
		{"version before installation", func(c *userCursor) { c.V++; c.InstallID = "other" }, 409, "cursor_stale", "cursor_version"},
		{"version before query", func(c *userCursor) { c.V++; c.Query = "other" }, 409, "cursor_stale", "cursor_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			assertProblem(t, request(h, encodeUserCursor(c)), tc.status, tc.code, tc.reason)
		})
	}
	// A future version's remaining fields are deliberately uninterpretable.
	assertProblem(t, request(h, base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"installId":{}}`))), 409, "cursor_stale", "cursor_version")
	for _, handler := range []http.Handler{h, other} {
		for _, tc := range []struct {
			ordering string
			ids      []int64
		}{
			{"", []int64{1, 2, 3, 4}},
			{"&order=desc", []int64{4, 3, 2, 1}},
			{"&sort=createdAt", []int64{2, 3, 4, 1}},
			{"&sort=createdAt&order=desc", []int64{1, 4, 3, 2}},
		} {
			ordering := tc.ordering
			next := ""
			seen := map[int64]bool{}
			var ids []int64
			var previous *User
			for range 5 {
				p := pageFrom(t, usersRequest(t, handler, "GET", "/users?limit=1"+ordering+"&cursor="+url.QueryEscape(next), "", "", userRights, "cookie"))
				for _, u := range p.Items {
					if previous != nil {
						less := previous.Username < u.Username || (previous.Username == u.Username && previous.ID < u.ID)
						if strings.Contains(ordering, "sort=createdAt") {
							less = previous.CreatedAt.Before(u.CreatedAt) || (previous.CreatedAt.Equal(u.CreatedAt) && previous.ID < u.ID)
						}
						if less == strings.Contains(ordering, "order=desc") {
							t.Fatalf("wrong SQL order: previous=%+v next=%+v order=%s", previous, u, ordering)
						}
					}
					previous = &u
					if seen[u.ID] {
						t.Fatal("duplicate page row")
					}
					seen[u.ID] = true
					ids = append(ids, u.ID)
					if utf8Count := len([]rune(u.Username)); utf8Count > 64 {
						t.Fatal("folded key exposed")
					}
				}
				next = p.NextCursor
				if len(next) > 1024 {
					t.Fatal("oversized cursor")
				}
				if next == "" {
					break
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("pagination order=%q ids=%v want=%v", ordering, ids, tc.ids)
			}
			if len(seen) != 4 || next != "" {
				t.Fatalf("pagination rows=%d cursor=%q", len(seen), next)
			}
		}
	}
	for _, anchor := range []string{strings.Repeat("\x01", 64), strings.Repeat("😀", 64), strings.Repeat("\u2028", 64)} {
		c := base
		c.RestoreEpoch = 9223372036854775807
		c.SchemaVersion = c.RestoreEpoch
		c.CollectionRevision = c.RestoreEpoch
		c.After[0], _ = json.Marshal(anchor)
		c.After[1] = json.RawMessage("9223372036854775807")
		encoded := encodeUserCursor(c)
		decoded, err := decodeUserCursor(encoded)
		if err != nil || !reflect.DeepEqual(decoded, c) || len(encoded) > 1024 {
			t.Fatalf("cursor boundary length=%d err=%v", len(encoded), err)
		}
		t.Logf("CURSOR-BOUNDARY runes=64 encoded=%d max=1024", len(encoded))
	}
	if w := usersRequest(t, h, "PATCH", "/users/2", `{"expectedRevision":1,"role":"member"}`, "", userRights, "cookie"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	assertProblem(t, request(h, first.NextCursor), 409, "cursor_stale", "collection_revision")
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE installation SET restore_epoch=restore_epoch+1")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, request(h, first.NextCursor), 409, "cursor_stale", "restore_epoch")
}

func TestETagBelowMaxChange(t *testing.T) {
	s, h := usersFixture(t, "first", "second")
	for rev := 1; rev < 5; rev++ {
		w := usersRequest(t, h, "PATCH", "/users/1", fmt.Sprintf(`{"expectedRevision":%d,"role":"admin"}`, rev), "", userRights, "cookie")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	before := usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie")
	if w := usersRequest(t, h, "GET", "/users", "", before.Header().Get("ETag"), userRights, "cookie"); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("matching tag: status=%d body=%s", w.Code, w.Body.String())
	}
	w := usersRequest(t, h, "PATCH", "/users/2", `{"expectedRevision":1,"role":"viewer"}`, "", userRights, "cookie")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	after := usersRequest(t, h, "GET", "/users", "", before.Header().Get("ETag"), userRights, "cookie")
	if after.Code != 200 || before.Header().Get("ETag") == after.Header().Get("ETag") {
		t.Fatal("below-max edit retained tag")
	}
	if pageFrom(t, after).CollectionRevision != pageFrom(t, before).CollectionRevision+1 {
		t.Fatal("collection revision did not increment")
	}
	t.Logf("ETAG before=%s after=%s", before.Header().Get("ETag"), after.Header().Get("ETag"))
	// Callback errors roll the read back and cannot publish a successful page.
	err := s.ListUsers(t.Context(), auth.UserQuery{Limit: 1, Sort: "username", Order: "asc"}, func(auth.UserCollection) error { return context.Canceled })
	if err != context.Canceled {
		t.Fatalf("callback error=%v", err)
	}
}

func TestETagScopeSeparation(t *testing.T) {
	s, h := usersFixture(t, "first", "second")
	_, other := usersFixture(t, "first", "second")
	tag := usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie").Header().Get("ETag")
	for _, tc := range []struct {
		h          http.Handler
		rights     []string
		credential string
	}{{other, userRights, "cookie"}, {h, []string{auth.RightUsersRead}, "cookie"}, {h, userRights, "key"}} {
		w := usersRequest(t, tc.h, "GET", "/users", "", tag, tc.rights, tc.credential)
		if w.Code != 200 || w.Header().Get("ETag") == tag {
			t.Fatal("identity/scope revalidated")
		}
	}
	for _, mutation := range []string{"UPDATE installation SET restore_epoch=restore_epoch+1", "UPDATE installation SET install_id='abcdef123456'"} {
		if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec(mutation); return e }); err != nil {
			t.Fatal(err)
		}
		w := usersRequest(t, h, "GET", "/users", "", tag, userRights, "cookie")
		if w.Code != 200 || w.Header().Get("ETag") == tag {
			t.Fatal("installation revalidated")
		}
		tag = w.Header().Get("ETag")
	}
	s.schema++
	if w := usersRequest(t, h, "GET", "/users", "", tag, userRights, "cookie"); w.Code != 200 || w.Header().Get("ETag") == tag {
		t.Fatal("schema revalidated")
	}
}

func TestETagQuerySeparation(t *testing.T) {
	_, h := usersFixture(t, "Émilie", "second")
	tag := usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie").Header().Get("ETag")
	for _, query := range []string{"?limit=1", "?order=desc", "?sort=createdAt", "?q=second", "?role=member"} {
		w := usersRequest(t, h, "GET", "/users"+query, "", tag, userRights, "cookie")
		if w.Code != 200 || w.Header().Get("ETag") == tag {
			t.Fatalf("query %s revalidated", query)
		}
	}
	canonical := usersRequest(t, h, "GET", "/users?q="+url.QueryEscape("ÉMILIE"), "", "", userRights, "cookie")
	w := usersRequest(t, h, "GET", "/users?q="+url.QueryEscape("e\u0301milie"), "", canonical.Header().Get("ETag"), userRights, "cookie")
	if w.Code != 304 {
		t.Fatal("canonically equivalent search did not revalidate")
	}
	for _, validator := range []string{strings.TrimPrefix(tag, "W/"), `"another", ` + tag, "*"} {
		if w := usersRequest(t, h, "GET", "/users", "", validator, userRights, "cookie"); w.Code != 304 || w.Body.Len() != 0 {
			t.Fatalf("weak validator %s status=%d", validator, w.Code)
		}
	}
	first := usersRequest(t, h, "GET", "/users?limit=1", "", "", userRights, "cookie")
	page := pageFrom(t, first)
	second := usersRequest(t, h, "GET", "/users?limit=1&cursor="+url.QueryEscape(page.NextCursor), "", first.Header().Get("ETag"), userRights, "cookie")
	if second.Code != 200 || second.Header().Get("ETag") == first.Header().Get("ETag") || pageFrom(t, second).Items[0].ID == page.Items[0].ID {
		t.Fatal("different page revalidated")
	}
	assertProblem(t, usersRequest(t, h, "GET", "/users", "", "*", []string{auth.RightStatusRead}, "cookie"), 403, "forbidden", "")
}

func TestCollectionSnapshotCoherent(t *testing.T) {
	_, h := usersFixture(t, "only")
	var wg sync.WaitGroup
	wg.Go(func() {
		for revision := 1; revision <= 20; revision++ {
			w := usersRequest(t, h, "PATCH", "/users/1", fmt.Sprintf(`{"expectedRevision":%d,"role":"admin"}`, revision), "", userRights, "cookie")
			if w.Code != 200 {
				t.Errorf("concurrent edit status=%d", w.Code)
				return
			}
		}
	})
	for range 30 {
		page := pageFrom(t, usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie"))
		if len(page.Items) != 1 || page.CollectionRevision != page.Items[0].Revision+1 {
			t.Fatalf("incoherent snapshot: %+v", page)
		}
	}
	wg.Wait()
	page := pageFrom(t, usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie"))
	if page.CollectionRevision != 22 || page.Items[0].Revision != 21 {
		t.Fatalf("final revisions: %+v", page)
	}
}

func TestUserWriteFailureDoesNotPublish(t *testing.T) {
	for _, failure := range []string{"callback", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, h := usersFixture(t, "first", "second")
			before := usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie")
			err := s.db.Write(t.Context(), func(tx *sql.Tx) error {
				if failure == "callback" {
					_, err := tx.Exec("CREATE TRIGGER fixture_user_failure AFTER UPDATE ON user BEGIN SELECT RAISE(ABORT, 'synthetic_write_failure'); END")
					return err
				}
				if _, err := tx.Exec("CREATE TABLE fixture_parent (id INTEGER PRIMARY KEY)"); err != nil {
					return err
				}
				if _, err := tx.Exec("CREATE TABLE fixture_child (id INTEGER REFERENCES fixture_parent(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					return err
				}
				_, err := tx.Exec("CREATE TRIGGER fixture_user_failure AFTER UPDATE ON user BEGIN INSERT INTO fixture_child(id) VALUES (999); END")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			role := auth.RoleViewer
			user, err := auth.UpdateUser(t.Context(), s.db, 2, auth.UserPatch{ExpectedRevision: 1, Role: &role})
			if err == nil || user != (auth.User{}) {
				t.Fatalf("failed %s published user=%+v err=%v", failure, user, err)
			}
			assertProblem(t, usersRequest(t, h, "PATCH", "/users/2", `{"expectedRevision":1,"role":"viewer"}`, "", userRights, "cookie"), 500, "internal_error", "")
			after := usersRequest(t, h, "GET", "/users", "", "", userRights, "cookie")
			if after.Code != 200 || after.Header().Get("ETag") != before.Header().Get("ETag") || !reflect.DeepEqual(pageFrom(t, before), pageFrom(t, after)) {
				t.Fatalf("failed %s changed rows/revision/validator", failure)
			}
			t.Logf("USER-WRITE-FAILURE boundary=%s published=false rows_revision_validator_unchanged=true", failure)
		})
	}
}
