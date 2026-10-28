package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// TestSystemStatusReportsGoroutines requires the served status body to carry a
// live reading of this process's goroutines. The fake service answers with a
// zero snapshot, so the count can only come from the handler's own sample, and
// it must move with the process rather than be a fixed number.
func TestSystemStatusReportsGoroutines(t *testing.T) {
	mux := http.NewServeMux()
	New(mux, fakeService{})
	serve := func() int {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, Prefix+"/system/status", nil)
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Rights: []string{auth.RightStatusRead}}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s/system/status: %d %s", Prefix, w.Code, w.Body.String())
		}
		var body struct {
			Goroutines int `json:"goroutines"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Goroutines < 1 {
			t.Fatalf("goroutines = %d in %s, want the live count of this process", body.Goroutines, w.Body.String())
		}
		return body.Goroutines
	}
	idle := serve()
	const parked = 64
	started, release := make(chan struct{}, parked), make(chan struct{})
	var wg sync.WaitGroup
	for range parked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			<-release
		}()
	}
	for range parked {
		<-started
	}
	busy := serve()
	close(release)
	wg.Wait()
	if busy-idle < parked/2 {
		t.Fatalf("goroutines %d while idle, %d with %d parked: want the count sampled at request time", idle, busy, parked)
	}
}
