package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressionVaryAndSSE(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, accept string
		size                      int
		flush, compressed         bool
	}{
		{"threshold", "application/json", "gzip", 1024, false, false},
		{"large", "application/json", "gzip", 1025, false, true},
		{"identity", "application/json", "", 2048, false, false},
		{"gzip refused", "application/json", "gzip;q=0, *;q=1", 2048, false, false},
		{"wildcard", "application/json", "*", 2048, false, true},
		{"sse", "text/event-stream", "gzip", 2048, true, false},
		{"sse without flush", "text/event-stream", "gzip", 2048, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("x", tc.size)
			h := compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Vary", "Cookie")
				_, _ = w.Write([]byte(body[:512]))
				if tc.flush {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(body[512:]))
			}))
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Accept-Encoding", tc.accept)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			got := w.Body.Bytes()
			if (w.Header().Get("Content-Encoding") == "gzip") != tc.compressed {
				t.Fatalf("encoding=%q", w.Header().Get("Content-Encoding"))
			}
			if tc.compressed {
				if len(got) < 10 || got[8] != 4 {
					t.Fatal("gzip must use level 1")
				}
				reader, err := gzip.NewReader(w.Body)
				if err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				_ = reader.Close()
			}
			if string(got) != body {
				t.Fatalf("body bytes=%d, want %d", len(got), len(body))
			}
			if tc.contentType == "application/json" && !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Accept-Encoding") {
				t.Fatal("missing Vary")
			}
			if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Cookie") {
				t.Fatal("lost existing Vary")
			}
		})
	}
}
