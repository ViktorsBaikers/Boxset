package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ViktorsBaikers/Boxset/internal/jobs"
)

// TestAdmissionRefusal proves the one admission mapping: a refused request is the
// recorded 429 problem carrying the refusal's reason word and its Retry-After,
// and any other error stays the error it was.
func TestAdmissionRefusal(t *testing.T) {
	mux := http.NewServeMux()
	a, _ := New(mux, fakeService{})
	var refuse error
	huma.Register(a, huma.Operation{
		OperationID:   "testAdmissionRefusal",
		Method:        http.MethodPost,
		Path:          Prefix + "/test/admit",
		DefaultStatus: http.StatusAccepted,
	}, func(context.Context, *struct{}) (*struct{}, error) {
		if refuse == nil {
			return nil, nil
		}
		return nil, AdmissionRefusal(refuse)
	})
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, Prefix+"/test/admit", bytes.NewReader(nil))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	if w := post(); w.Code != http.StatusAccepted {
		t.Fatalf("admitted request = %d, want 202", w.Code)
	}
	for _, tc := range []struct {
		name  string
		err   error
		code  string
		retry string
	}{
		{"interactive cap", &jobs.AdmissionError{Reason: "interactive_cap", RetryAfter: 2 * time.Second}, "interactive_cap", "2"},
		{"full optional queue", &jobs.AdmissionError{Reason: "queue_full", RetryAfter: 30 * time.Second}, "queue_full", "30"},
		{"sub-second wait", &jobs.AdmissionError{Reason: "queue_full", RetryAfter: 250 * time.Millisecond}, "queue_full", "1"},
	} {
		refuse = tc.err
		w := post()
		var problem Problem
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: body %q: %v", tc.name, w.Body.String(), err)
		}
		if w.Code != http.StatusTooManyRequests || problem.Code != tc.code {
			t.Errorf("%s = %d %q, want 429 %s", tc.name, w.Code, w.Body.String(), tc.code)
		}
		if got := w.Header().Get("Retry-After"); got != tc.retry {
			t.Errorf("%s: Retry-After = %q, want %q", tc.name, got, tc.retry)
		}
		if !bytes.HasPrefix([]byte(w.Header().Get("Content-Type")), []byte("application/problem+json")) {
			t.Errorf("%s: Content-Type = %q", tc.name, w.Header().Get("Content-Type"))
		}
	}

	// Anything else is not an admission refusal, so it keeps its own outcome: a
	// distinct error must not become a 429 with a reason it never had.
	refuse = errors.New("store refused the write")
	if w := post(); w.Code != http.StatusInternalServerError {
		t.Fatalf("unrelated failure = %d, want 500 internal_error", w.Code)
	}
	if got := AdmissionRefusal(refuse); got != refuse {
		t.Errorf("a non-refusal was rewritten as %v", got)
	}
}
