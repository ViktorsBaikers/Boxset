package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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
