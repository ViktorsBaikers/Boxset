package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// jobService serves the jobs read model from a real runtime over a real store:
// the route under test differs from production in nothing but the guard in front
// of it, which the server package proves.
type jobService struct {
	fakeService
	rt *jobs.Runtime
}

func (s jobService) ListJobs(ctx context.Context, q jobs.Query) (jobs.Page, error) {
	return s.rt.List(ctx, q)
}

// jobsFixture is one store, one runtime and the route over them, seeded with rows
// so a page has something to hold.
func jobsFixture(t *testing.T, rows int) (*store.DB, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rt, err := jobs.New(db, jobs.Config{Boot: "boot-api", BackgroundSlots: 1, Log: func(string, ...any) {}}, nil)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for i := range rows {
			key := fmt.Sprintf("api-%d", i)
			if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
				Kind:           "api.work",
				Class:          store.JobOptional,
				Lane:           store.JobBackground,
				DedupeKey:      key,
				ConcurrencyKey: key,
				MaxAttempts:    1,
				DueAt:          time.Unix(1_800_000_000, 0).Unix(),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(mux, jobService{rt: rt})
	return db, mux
}

var statusRead = []string{auth.RightStatusRead}

// jobsRequest serves one GET of the jobs route on a context carrying the rights
// the guard would have attached; nil sends no principal at all.
func jobsRequest(h http.Handler, target string, rights []string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, Prefix+target, nil)
	if rights != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Rights: rights}))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func jobPageFrom(t *testing.T, w *httptest.ResponseRecorder) JobPage {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("jobs page status=%d body=%s", w.Code, w.Body.String())
	}
	var page JobPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("jobs page body %q: %v", w.Body.String(), err)
	}
	return page
}

// TestJobsListPaging proves the recorded page contract: 50 by default, at most
// 500, newest first and continued from the cursor the page returned.
func TestJobsListPaging(t *testing.T) {
	const rows = 60
	_, h := jobsFixture(t, rows)
	first := jobPageFrom(t, jobsRequest(h, "/system/jobs", statusRead))
	if len(first.Items) != 50 || first.NextCursor == "" {
		t.Fatalf("default page items=%d next=%q, want 50 and a cursor", len(first.Items), first.NextCursor)
	}
	if first.Items[0].ID != rows {
		t.Errorf("newest row = %d, want %d", first.Items[0].ID, rows)
	}
	for i := 1; i < len(first.Items); i++ {
		if first.Items[i-1].ID <= first.Items[i].ID {
			t.Fatalf("page %d: ids %d then %d are not newest first", i, first.Items[i-1].ID, first.Items[i].ID)
		}
	}
	second := jobPageFrom(t, jobsRequest(h, "/system/jobs?cursor="+first.NextCursor, statusRead))
	if len(second.Items) != rows-50 || second.NextCursor != "" {
		t.Fatalf("continued page items=%d next=%q, want the remaining %d and no cursor", len(second.Items), second.NextCursor, rows-50)
	}
	seen := map[int64]bool{}
	for _, job := range slices.Concat(first.Items, second.Items) {
		if seen[job.ID] {
			t.Fatalf("job %d was served twice", job.ID)
		}
		seen[job.ID] = true
	}
	if len(seen) != rows {
		t.Fatalf("pages served %d rows, want %d", len(seen), rows)
	}
	all := jobPageFrom(t, jobsRequest(h, "/system/jobs?limit=500", statusRead))
	if len(all.Items) != rows || all.NextCursor != "" {
		t.Fatalf("full page items=%d next=%q, want all %d rows", len(all.Items), all.NextCursor, rows)
	}
}

// TestJobsListFullPage proves a page that exactly fills its bound is a complete
// page: a cursor is offered only when a row was read past the bound.
func TestJobsListFullPage(t *testing.T) {
	_, h := jobsFixture(t, 50)
	page := jobPageFrom(t, jobsRequest(h, "/system/jobs?limit=50", statusRead))
	if len(page.Items) != 50 || page.NextCursor != "" {
		t.Fatalf("exactly full page items=%d next=%q, want 50 rows and no cursor", len(page.Items), page.NextCursor)
	}
}

// TestJobsListBounds proves the page bound is the recorded one: 501 is a
// validation refusal, never a page of 501 rows.
func TestJobsListBounds(t *testing.T) {
	_, h := jobsFixture(t, 3)
	for _, target := range []string{"/system/jobs?limit=501", "/system/jobs?limit=0", "/system/jobs?state=broken"} {
		assertProblem(t, jobsRequest(h, target, statusRead), http.StatusUnprocessableEntity, "validation_failed", "")
	}
}

// TestJobsListCursor proves a cursor is only honoured inside the snapshot that
// produced it: another installation or filter is invalid, another restore epoch
// is a stale cursor, and a version this route does not know is stale too.
func TestJobsListCursor(t *testing.T) {
	db, h := jobsFixture(t, 60)
	first := jobPageFrom(t, jobsRequest(h, "/system/jobs", statusRead))
	if first.NextCursor == "" {
		t.Fatal("first page returned no cursor")
	}
	forged := func(change func(*jobCursor)) string {
		t.Helper()
		cursor, err := decodeJobCursor(first.NextCursor)
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		change(&cursor)
		return encodeJobCursor(cursor)
	}
	for _, tc := range []struct {
		name   string
		cursor string
		status int
		code   string
	}{
		{"malformed", "not-a-cursor", http.StatusBadRequest, "cursor_invalid"},
		{"another installation", forged(func(c *jobCursor) { c.InstallID = "000000000000" }), http.StatusBadRequest, "cursor_invalid"},
		{"another filter", forged(func(c *jobCursor) { c.State = store.JobQueued }), http.StatusBadRequest, "cursor_invalid"},
		{"another version", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)), http.StatusConflict, "cursor_stale"},
	} {
		w := jobsRequest(h, "/system/jobs?cursor="+tc.cursor, statusRead)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s = %d %q, want %d %s", tc.name, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
	// A restore moves the epoch, so a page taken before it is stale: the rows a
	// caller already saw may belong to the state the restore replaced.
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE installation SET restore_epoch = restore_epoch + 1 WHERE id = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, jobsRequest(h, "/system/jobs?cursor="+first.NextCursor, statusRead), http.StatusConflict, "cursor_stale", "restore_epoch")
}

// TestJobsListRights proves the route needs the existing status right and nothing
// else: no principal is unauthenticated, a principal without the right is
// forbidden, and the right is served.
func TestJobsListRights(t *testing.T) {
	_, h := jobsFixture(t, 1)
	assertProblem(t, jobsRequest(h, "/system/jobs", nil), http.StatusUnauthorized, "unauthenticated", "")
	assertProblem(t, jobsRequest(h, "/system/jobs", []string{auth.RightUsersRead}), http.StatusForbidden, "forbidden", "")
	if w := jobsRequest(h, "/system/jobs", statusRead); w.Code != http.StatusOK {
		t.Errorf("with status:read = %d %q", w.Code, w.Body.String())
	}
}

// TestJobsListStatesAndReasons proves the page shows each row's state and the
// reason that put it there, including the reason parameters the store holds.
func TestJobsListStatesAndReasons(t *testing.T) {
	db, h := jobsFixture(t, 7)
	states := []string{store.JobQueued, store.JobRunning, store.JobReconciling, store.JobSucceeded, store.JobFailed, store.JobReview, store.JobExpired}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for i, state := range states {
			if _, err := tx.ExecContext(t.Context(), `UPDATE job SET state = ?, reason_code = 'attempt_overdue', reason_params = '{"deadline_s":"30"}' WHERE id = ?`, state, i+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page := jobPageFrom(t, jobsRequest(h, "/system/jobs?limit=500", statusRead))
	if len(page.Items) != len(states) {
		t.Fatalf("page items=%d, want %d", len(page.Items), len(states))
	}
	shown := map[string]bool{}
	for _, job := range page.Items {
		shown[job.State] = true
		if job.ReasonCode != "attempt_overdue" || job.ReasonParams["deadline_s"] != "30" {
			t.Errorf("job %d reason = %q %v, want attempt_overdue with its deadline", job.ID, job.ReasonCode, job.ReasonParams)
		}
	}
	for _, state := range states {
		if !shown[state] {
			t.Errorf("state %s is not served", state)
		}
	}
	filtered := jobPageFrom(t, jobsRequest(h, "/system/jobs?state="+store.JobFailed, statusRead))
	if len(filtered.Items) != 1 || filtered.Items[0].State != store.JobFailed {
		t.Fatalf("filtered page = %+v, want the one failed row", filtered.Items)
	}
}

// TestJobsContractFromFile proves the served page is the document the SPA's types
// are generated from: every member the route emits is a declared property, every
// required property is present, and every enum member is one the document names.
func TestJobsContractFromFile(t *testing.T) {
	doc := parse(t, committed(t))
	jobSchema, declared := doc.Components.Schemas["Job"]
	if !declared {
		t.Fatal("the document declares no Job schema")
	}
	pageSchema, declared := doc.Components.Schemas["JobPage"]
	if !declared || pageSchema.Properties["items"].Items == nil || pageSchema.Properties["items"].Items.Ref != "#/components/schemas/Job" {
		t.Fatal("the document's JobPage.items is not an array of Job")
	}
	db, h := jobsFixture(t, 7)
	states := []string{store.JobQueued, store.JobRunning, store.JobReconciling, store.JobSucceeded, store.JobFailed, store.JobReview, store.JobExpired}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for i, state := range states {
			if _, err := tx.ExecContext(t.Context(), `UPDATE job SET state = ?, reason_code = 'expired' WHERE id = ?`, state, i+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := jobsRequest(h, "/system/jobs?limit=500", statusRead)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs page = %d %q", w.Code, w.Body.String())
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != len(states) {
		t.Fatalf("served items=%d, want %d", len(body.Items), len(states))
	}
	seen := map[string]bool{}
	for _, item := range body.Items {
		for name := range item {
			if _, ok := jobSchema.Properties[name]; !ok {
				t.Errorf("job member %q is not a property of the document's Job", name)
			}
		}
		for _, name := range jobSchema.Required {
			if _, ok := item[name]; !ok {
				t.Errorf("job is missing the required member %q", name)
			}
		}
		for _, name := range []string{"class", "lane", "state"} {
			value, _ := item[name].(string)
			if enum := jobSchema.Properties[name].Enum; !slices.Contains(enum, value) {
				t.Errorf("job %s = %q, which the document's %v does not name", name, value, enum)
			} else if name == "state" {
				seen[value] = true
			}
		}
	}
	for _, state := range states {
		if !seen[state] {
			t.Errorf("document state %s is never served", state)
		}
	}
	t.Logf("JOBS-CONTRACT operations=%d items=%d", len(doc.Paths), len(body.Items))
}
