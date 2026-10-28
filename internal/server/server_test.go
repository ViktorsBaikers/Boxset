package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPingReportsState(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v (run `bun --cwd=web run build` first: the shell is required)", err)
	}
	ts := httptest.NewServer(s.http.Handler)
	t.Cleanup(ts.Close)

	for _, st := range []State{Starting, Recovering, Ready, Draining} {
		s.SetState(st)
		resp, err := http.Get(ts.URL + "/ping")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Errorf("state %s: %d %q, want 200 ok", st, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Boxset-State"); got != string(st) {
			t.Errorf("state %s: X-Boxset-State = %q", st, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("state %s: Cache-Control = %q, want no-store", st, got)
		}
	}

	// Liveness is read-only and answers HEAD; the shell answers only its listed routes.
	head, err := http.Head(ts.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	_ = head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Errorf("HEAD /ping = %d, want 200", head.StatusCode)
	}
	post, err := http.Post(ts.URL+"/ping", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /ping = %d, want 405", post.StatusCode)
	}
	root, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Body.Close()
	if root.StatusCode != http.StatusOK || root.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("GET / = %d Cache-Control %q, want the shell with no-cache", root.StatusCode, root.Header.Get("Cache-Control"))
	}
	other, err := http.Get(ts.URL + "/unlisted")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("GET /unlisted = %d, want 404: an unlisted path is never the shell", other.StatusCode)
	}
}

// The public cue and the API share one lifecycle vocabulary: every state /ping can report is a
// value of the committed document's Status.state enum, and every enum value is a state.
func TestPingLifecycleCue(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "api", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Status struct {
					Properties struct {
						State struct{ Enum []string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	enum := doc.Components.Schemas.Status.Properties.State.Enum
	states := []State{Starting, Recovering, Ready, Draining}
	if len(enum) != len(states) {
		t.Fatalf("Status.state enum %v, server states %v", enum, states)
	}
	s, err := newServer(fakeDist(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range states {
		s.SetState(st)
		w := serve(s.http.Handler, "GET", "/ping")
		if w.Code != http.StatusOK || w.Body.String() != "ok" || w.Header().Get("Cache-Control") != "no-store" ||
			!slices.Contains(enum, w.Header().Get("X-Boxset-State")) {
			t.Errorf("state %s: %d %q Cache-Control %q X-Boxset-State %q", st, w.Code, w.Body.String(), w.Header().Get("Cache-Control"), w.Header().Get("X-Boxset-State"))
		}
	}
}
