package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

type passwordStageContext struct {
	context.Context
	admitted chan<- struct{}
	once     sync.Once
}

func (c *passwordStageContext) Deadline() (time.Time, bool) {
	c.once.Do(func() { c.admitted <- struct{}{} })
	return c.Context.Deadline()
}

func holdPasswordSQL(t *testing.T, in *installation) func() {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- in.db.ReadPassword(t.Context(), func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow("SELECT 42").Scan(&n); err != nil {
				return err
			}
			if n != 42 {
				return errors.New("invalid SELECT result")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("password SQL entry=%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("password SQL did not enter")
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(finish)
	return finish
}

func fillPasswordStage(t *testing.T, in *installation) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	admitted := make(chan struct{}, 32)
	done := make(chan error, 32)
	for range 32 {
		go func() {
			_, err := in.s.backend.Load().Setup.StateForPassword(&passwordStageContext{Context: ctx, admitted: admitted})
			done <- err
		}()
	}
	for range 32 {
		select {
		case <-admitted:
		case <-time.After(5 * time.Second):
			t.Fatal("stage population never reached 32")
		}
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			for range 32 {
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) && err.Error() != "hash_busy" {
					t.Error(err)
				}
			}
		})
	}
	t.Cleanup(finish)
	return finish
}

type beforePasswordBody struct {
	io.Reader
	before func()
	once   sync.Once
}

func (b *beforePasswordBody) Read(p []byte) (int, error) {
	b.once.Do(b.before)
	return b.Reader.Read(p)
}
func (*beforePasswordBody) Close() error { return nil }

func TestPasswordStageHTTP(t *testing.T) {
	for _, seam := range []string{"setup_guard", "login_guard", "create_after_turn", "login_user"} {
		for _, failure := range []string{"population", "deadline"} {
			t.Run(seam+"/"+failure, func(t *testing.T) {
				var in *installation
				if strings.HasPrefix(seam, "login") {
					in, _ = signedIn(t, "")
				} else {
					in = newInstallation(t, "", "")
				}
				path := "/api/v1/setup"
				if strings.HasPrefix(seam, "login") {
					path = "/api/v1/session"
				}
				releases := []func(){}
				block := func() {
					releases = append(releases, holdPasswordSQL(t, in))
					if failure == "population" {
						releases = append(releases, fillPasswordStage(t, in))
					}
				}
				guard := strings.HasSuffix(seam, "guard")
				bodyReads := 0
				if guard {
					block()
				}
				r := httptest.NewRequest("POST", path, nil)
				r.Host = "127.0.0.1:8484"
				r.RemoteAddr = "192.0.2.10:1234"
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+in.token)
				r.Body = &beforePasswordBody{Reader: strings.NewReader(credentials("owner")), before: func() {
					bodyReads++
					if !guard {
						block()
					}
				}}
				w := httptest.NewRecorder()
				in.h.ServeHTTP(w, r)
				if w.Code != 429 || code(t, w) != "hash_busy" || w.Header().Get("Retry-After") != "2" {
					t.Errorf("password seam %s status=%d body=%s retry=%q", seam, w.Code, w.Body.String(), w.Header().Get("Retry-After"))
				}
				if w.Header().Get("Set-Cookie") != "" {
					t.Error("refused password request issued a cookie")
				}
				if guard && bodyReads != 0 || !guard && bodyReads != 1 {
					t.Errorf("guard=%v body_reads=%d", guard, bodyReads)
				}
				if len(in.s.admission.passwords) != 0 {
					t.Error("HTTP password permit leaked")
				}
				for _, secret := range []string{password, in.token, "password_phc", "token_sha256"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Error("password material escaped in refusal")
					}
				}
				for i := len(releases) - 1; i >= 0; i-- {
					releases[i]()
				}
				if strings.HasPrefix(seam, "login") {
					if got := request(in.h, "POST", path, credentials("owner")); got.Code != 201 {
						t.Fatalf("released login=%d %s", got.Code, got.Body.String())
					}
				} else {
					if in.users(t) != 0 {
						t.Fatal("refused setup inserted an admin")
					}
					if got := request(in.h, "POST", path, credentials("owner"), "Authorization", "Bearer "+in.token); got.Code != 201 {
						t.Fatalf("released setup=%d %s", got.Code, got.Body.String())
					}
				}
			})
		}
	}
}

func TestPasswordAdmissionForeground(t *testing.T) {
	in, _ := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	releaseSQL := holdPasswordSQL(t, in)
	releaseStage := fillPasswordStage(t, in)
	w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", key)
	if w.Code != http.StatusOK {
		t.Fatalf("foreground during password saturation=%d %s", w.Code, w.Body.String())
	}
	for _, state := range []State{Starting, Recovering, Ready, Draining} {
		in.s.SetState(state)
		w := request(in.h, "GET", "/api/v1/users", "")
		if w.Code != 401 {
			t.Fatalf("principal under saturation state=%s status=%d", state, w.Code)
		}
	}
	in.s.SetState(Ready)
	releaseStage()
	releaseSQL()
	if got := request(in.h, "POST", "/api/v1/session", credentials("owner")); got.Code != 201 {
		t.Fatalf("legitimate released login=%d", got.Code)
	}
}

type passwordBody struct {
	entered chan<- bool
	release <-chan struct{}
	once    sync.Once
	io.Reader
}

func (b *passwordBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.entered <- true; <-b.release })
	return b.Reader.Read(p)
}

func (*passwordBody) Close() error { return nil }

func TestPasswordAdmissionBeforeWork(t *testing.T) {
	for _, cohort := range []string{"login", "setup", "mixed"} {
		t.Run(cohort, func(t *testing.T) {
			var in *installation
			foregroundKey := ""
			if cohort == "login" {
				in, _ = signedIn(t, "")
				_, foregroundKey = keyFor(t, in, "admin")
			} else {
				in = newInstallation(t, "", "")
			}
			baseline := runtime.NumGoroutine()
			var before, held, settled runtime.MemStats
			runtime.ReadMemStats(&before)
			entered := make(chan bool, 1000)
			release := make(chan struct{})
			start := make(chan struct{})
			responses := make(chan *httptest.ResponseRecorder, 1000)
			var workers sync.WaitGroup
			for i := range 1000 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					path := "/api/v1/session"
					if cohort == "setup" || cohort == "mixed" && i%2 == 0 {
						path = "/api/v1/setup"
					}
					r := httptest.NewRequest("POST", path, nil)
					r.Host = "127.0.0.1:8484"
					r.RemoteAddr = fmt.Sprintf("192.0.%d.%d:1234", i/250, i%250+1)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Authorization", "Bearer "+in.token)
					r.Body = &passwordBody{entered: entered, release: release, Reader: strings.NewReader("{")}
					w := httptest.NewRecorder()
					in.h.ServeHTTP(w, r)
					responses <- w
					entered <- false
				}()
			}
			close(start)
			bodies, refused := 0, 0
			for range 1000 {
				if <-entered {
					bodies++
				} else {
					refused++
				}
			}
			runtime.ReadMemStats(&held)
			if len(in.s.admission.passwords) != 32 {
				t.Errorf("held HTTP population=%d", len(in.s.admission.passwords))
			}
			if foregroundKey != "" {
				if w := request(in.h, "GET", "/api/v1/users", "", "X-Api-Key", foregroundKey); w.Code != 200 {
					t.Errorf("foreground under held bodies=%d", w.Code)
				}
			}
			close(release)
			workers.Wait()
			runtime.ReadMemStats(&settled)
			if len(in.s.admission.passwords) != 0 {
				t.Error("body-error cohort leaked password population")
			}
			if current := runtime.NumGoroutine(); current > baseline+64 {
				t.Errorf("settled goroutines=%d baseline=%d", current, baseline)
			}
			t.Logf("PASSWORD-POPULATION peers=1000 held=32 settled=0 goroutines=%d baseline=%d heap_before=%d heap_held=%d heap_settled=%d budgets=not_measured", runtime.NumGoroutine(), baseline, before.Alloc, held.Alloc, settled.Alloc)
			close(responses)
			busy := 0
			for w := range responses {
				if w.Code == 429 {
					busy++
					if code(t, w) != "hash_busy" || w.Header().Get("Retry-After") != "2" {
						t.Errorf("overload=%d %s retry=%q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
					}
				}
			}
			if bodies != 32 || refused != 968 || busy != 968 {
				t.Fatalf("PASSWORD-OUTER-CAP bodies=%d refused_before_body=%d hash_busy=%d; want 32/968/968", bodies, refused, busy)
			}
		})
	}
}

func TestPasswordStageSQLFailure(t *testing.T) {
	in := newInstallation(t, "", "")
	if err := in.db.Close(); err != nil {
		t.Fatal(err)
	}
	w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token)
	if w.Code != 500 || code(t, w) != "internal_error" || w.Header().Get("Retry-After") != "" {
		t.Fatalf("unrelated SQL error relabelled as admission: %d %s", w.Code, w.Body.String())
	}
	if len(in.s.admission.passwords) != 0 {
		t.Fatal("SQL failure leaked HTTP admission")
	}
}

// admissionConsumer composes the server's own handler chain around a mux that
// also holds one test-only operation. No public producer admits interactive work
// in this milestone, so the operation that does is registered here: the parent
// guard authorizes it exactly like a documented route, which is what makes the
// shared 429 mapping observable over real HTTP with a real principal, runtime and
// store file.
func admissionConsumer(t *testing.T, in *installation, rt *jobs.Runtime) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	a, routes := api.New(mux, in.s)
	huma.Register(a, huma.Operation{
		OperationID:   "testAcceptJob",
		Method:        http.MethodPost,
		Path:          api.Prefix + "/test/accept",
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		// The caller comes only from the authenticated principal: the operation
		// names no user, and Accept grants nothing the request did not have.
		session, ok := auth.SessionFrom(ctx)
		if !ok {
			return nil, api.NewProblem(http.StatusUnauthorized, "unauthenticated")
		}
		if _, err := rt.Accept(ctx, jobs.Caller{UserID: session.User.ID}, jobs.KindJobsPrune); err != nil {
			return nil, api.AdmissionRefusal(err)
		}
		return nil, nil
	})
	table := map[string]api.Route{}
	for _, route := range routes {
		table[route.Pattern] = route
	}
	pattern := http.MethodPost + " " + api.Prefix + "/test/accept"
	table[pattern] = api.Route{Pattern: pattern, Class: api.ClassPrincipal, Right: auth.RightStatusRead}
	return withSecurityHeaders(compress(in.s.guard(mux, table)))
}

// admitFixture is one ready installation over a real store file whose composed
// runtime is attached and started, with an admin API key for the caller. handler
// builds the one runnable kind over that installation's own store, so an admitted
// jobs.prune attempt runs the real load unless a test holds it.
func admitFixture(t *testing.T, handler func(*store.DB) jobs.Handler) (*installation, *jobs.Runtime, http.Handler, string) {
	t.Helper()
	in := newInstallation(t, "", "")
	if w := request(in.h, "POST", "/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+in.token); w.Code != http.StatusCreated {
		t.Fatalf("setup = %d %q", w.Code, w.Body.String())
	}
	_, key := keyFor(t, in, "admin")
	rt, err := jobs.New(in.db, jobs.Config{Boot: "boot-http", BackgroundSlots: 1, Log: func(string, ...any) {}}, []jobs.Handler{handler(in.db)})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	backend := *in.s.backend.Load()
	backend.Jobs = rt
	in.s.Attach(backend)
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("runtime start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.Stop(ctx); err != nil {
			t.Errorf("runtime stop: %v", err)
		}
	})
	return in, rt, admissionConsumer(t, in, rt), key
}

// jobCount is the number of durable rows, the counter a refusal must not move.
func jobCount(t *testing.T, in *installation) int64 {
	t.Helper()
	var n int64
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM job`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitJobState polls one row until it reaches state.
func waitJobState(t *testing.T, in *installation, id int64, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var current string
		if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT state FROM job WHERE id = ?`, id).Scan(&current)
		}); err != nil {
			t.Fatal(err)
		}
		if current == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %d never reached %s", id, state)
}

// ownerID is the first admin's id, the caller every admission is attributed to.
func ownerID(t *testing.T, in *installation) int64 {
	t.Helper()
	var id int64
	if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM user ORDER BY id LIMIT 1`).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// openInteractive is n open interactive rows of the caller's, not yet due so no
// lane claims them: the admission counters, written as the rows an earlier
// request would have left open.
func openInteractive(t *testing.T, in *installation, createdBy int64, n int) {
	t.Helper()
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			key := fmt.Sprintf("open-%d", i)
			due := time.Now().Add(time.Hour).Unix()
			if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
				Kind: "http.open", Class: store.JobRequired, Lane: store.JobInteractive,
				DedupeKey: key, ConcurrencyKey: key, MaxAttempts: 1,
				DueAt: due, CreatedBy: &createdBy,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestJobAdmissionHTTP proves AC-024 at the HTTP boundary: the fifth request of
// one caller and the request into a full optional queue are the recorded 429s
// with their reason word and Retry-After, a refusal leaves the row count where it
// was, and an admitted request completes without its caller.
func TestJobAdmissionHTTP(t *testing.T) {
	accept := func(h http.Handler, key string) *httptest.ResponseRecorder {
		t.Helper()
		return request(h, "POST", "/api/v1/test/accept", "", "X-Api-Key", key)
	}
	t.Run("the fifth request of one caller is refused", func(t *testing.T) {
		in, _, h, key := admitFixture(t, jobs.PruneHandler)
		openInteractive(t, in, ownerID(t, in), 4)
		w := accept(h, key)
		if w.Code != http.StatusTooManyRequests || code(t, w) != "interactive_cap" || w.Header().Get("Retry-After") != "2" {
			t.Fatalf("fifth admission = %d %q Retry-After %q, want 429 interactive_cap 2", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		if rows := jobCount(t, in); rows != 4 {
			t.Fatalf("rows after the refusal = %d, want 4: a refusal creates nothing", rows)
		}
		// One of the open rows finishes, so the caller's share returns and the
		// same request is admitted.
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE job SET state = 'succeeded', revision = revision + 1 WHERE id = (SELECT min(id) FROM job)`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if w := accept(h, key); w.Code != http.StatusAccepted {
			t.Fatalf("admission after a finished row = %d %q, want 202", w.Code, w.Body.String())
		}
		if rows := jobCount(t, in); rows != 5 {
			t.Fatalf("rows after the admission = %d, want 5", rows)
		}
	})

	t.Run("a request into a full optional queue is refused", func(t *testing.T) {
		in, _, h, key := admitFixture(t, jobs.PruneHandler)
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
			for i := range 1024 {
				id := fmt.Sprintf("http-bulk-%d", i)
				// Not yet due, so no lane claims these rows while the test runs;
				// the queue count reads the queued state alone.
				if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
					Kind: "http.bulk", Class: store.JobOptional, Lane: store.JobBackground,
					DedupeKey: id, ConcurrencyKey: id, MaxAttempts: 1,
					DueAt: time.Now().Add(time.Hour).Unix(),
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w := accept(h, key)
		if w.Code != http.StatusTooManyRequests || code(t, w) != "queue_full" || w.Header().Get("Retry-After") != "30" {
			t.Fatalf("full queue = %d %q Retry-After %q, want 429 queue_full 30", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		if rows := jobCount(t, in); rows != 1024 {
			t.Fatalf("rows after the refusal = %d, want 1024", rows)
		}
	})

	t.Run("a caller that disconnects after 202 leaves a completing job", func(t *testing.T) {
		// The attempt is held until the caller has left, so the work that
		// finishes was never the request's to cancel.
		release := make(chan struct{})
		held := func(*store.DB) jobs.Handler {
			return jobs.Handler{
				Kind: jobs.KindJobsPrune, Class: jobs.ClassOptional, Lane: jobs.LaneBackground,
				RunNow: true, MaxAttempts: 1, Deadline: 30 * time.Second,
				Run: func(ctx context.Context, _ *jobs.Attempt) jobs.Outcome {
					select {
					case <-release:
						return jobs.Outcome{Result: jobs.Succeeded, Reason: "done"}
					case <-ctx.Done():
						return jobs.Outcome{}
					}
				},
			}
		}
		in, _, h, key := admitFixture(t, held)
		ts := httptest.NewServer(h)
		defer ts.Close()
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		host := ts.Listener.Addr().String()
		requestLine := "POST /api/v1/test/accept HTTP/1.1\r\nHost: " + host + "\r\nX-Api-Key: " + key + "\r\nContent-Length: 0\r\n\r\n"
		if _, err := conn.Write([]byte(requestLine)); err != nil {
			t.Fatal(err)
		}
		// The status line is the accepted work's answer; the caller then leaves
		// without reading the rest of the response.
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "202") {
			t.Fatalf("accepted response = %q, %v; want 202", line, err)
		}
		_ = conn.Close()
		var id int64
		if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT id FROM job ORDER BY id DESC LIMIT 1`).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		// The attempt is running while no client is waiting on it, and it
		// completes once its own work is allowed to.
		waitJobState(t, in, id, store.JobRunning)
		close(release)
		waitJobState(t, in, id, store.JobSucceeded)
	})
}

// TestJobVisibilityHTTP proves the jobs route serves the job rows behind the
// parent guard under the existing status right, that it pages by keyset, and that
// the documented page bound is enforced before any read.
func TestJobVisibilityHTTP(t *testing.T) {
	in, _, h, _ := admitFixture(t, jobs.PruneHandler)
	_, viewer := keyFor(t, in, "read")
	if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
		for _, key := range []string{"visible-1", "visible-2"} {
			// Not yet due: the states below are the test's, not a lane's.
			if _, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
				Kind: "http.work", Class: store.JobOptional, Lane: store.JobBackground,
				DedupeKey: key, ConcurrencyKey: key, MaxAttempts: 1,
				DueAt: time.Now().Add(time.Hour).Unix(),
			}); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE job SET state = 'succeeded', reason_code = 'done' WHERE dedupe_key = 'visible-1'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Items []struct {
			ID         int64  `json:"id"`
			State      string `json:"state"`
			ReasonCode string `json:"reasonCode"`
		} `json:"items"`
		NextCursor string `json:"nextCursor"`
	}
	w := request(h, "GET", "/api/v1/system/jobs?limit=500", "", "X-Api-Key", viewer)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs page = %d %q", w.Code, w.Body.String())
	}
	// The contract's declared response header: a page of job rows is readable
	// under existing rights and stays out of a shared cache.
	if got := w.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Errorf("jobs page Cache-Control = %q, want private, no-cache", got)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("jobs page body %q: %v", w.Body.String(), err)
	}
	if len(body.Items) != 2 || body.Items[0].State != store.JobQueued || body.Items[1].State != store.JobSucceeded || body.Items[1].ReasonCode != "done" {
		t.Fatalf("jobs page = %+v, want the queued and the succeeded row newest first", body.Items)
	}
	// The same route under the guard's own answers: no credential, and a bound
	// over the maximum, which is a validation refusal before any read.
	if w := request(h, "GET", "/api/v1/system/jobs", ""); w.Code != http.StatusUnauthorized || code(t, w) != "unauthenticated" {
		t.Errorf("without a credential = %d %q", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/v1/system/jobs?limit=501", "", "X-Api-Key", viewer); w.Code != http.StatusUnprocessableEntity || code(t, w) != "validation_failed" {
		t.Errorf("limit=501 = %d %q, want 422 validation_failed", w.Code, w.Body.String())
	}
	// One row per page, continued from the cursor the page returned.
	first := request(h, "GET", "/api/v1/system/jobs?limit=1", "", "X-Api-Key", viewer)
	if first.Code != http.StatusOK {
		t.Fatalf("first page = %d %q", first.Code, first.Body.String())
	}
	var page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("first page = %+v, want one row and a cursor", page)
	}
	second := request(h, "GET", "/api/v1/system/jobs?limit=1&cursor="+page.NextCursor, "", "X-Api-Key", viewer)
	if second.Code != http.StatusOK {
		t.Fatalf("continued page = %d %q", second.Code, second.Body.String())
	}
	var rest struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &rest); err != nil {
		t.Fatal(err)
	}
	if len(rest.Items) != 1 || rest.Items[0].ID == page.Items[0].ID || rest.NextCursor != "" {
		t.Fatalf("continued page = %+v after %+v, want the other row and no cursor", rest, page.Items)
	}
}
