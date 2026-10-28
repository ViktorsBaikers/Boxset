//go:build integration && unix

package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/server"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

func TestUsersOverHTTP(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hasher := auth.NewHasher()
	for i := range 12 {
		if _, err := auth.CreateUser(t.Context(), db, hasher, fmt.Sprintf("fixture-%02d", i), "synthetic-valid-password", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}
	s, err := server.New("")
	if err != nil {
		t.Fatal(err)
	}
	s.Attach(server.Backend{Setup: auth.NewSetup(db, hasher, dir), SchemaVersion: 2, RestoreEpoch: 1})
	s.SetState(server.Ready)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != http.ErrServerClosed {
			t.Errorf("serve: %v", err)
		}
	})
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + l.Addr().String() + "/api/v1"
	login, err := client.Post(base+"/session", "application/json", strings.NewReader(`{"username":"fixture-00","password":"synthetic-valid-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, login.Body)
	_ = login.Body.Close()
	if login.StatusCode != 201 || len(login.Cookies()) != 1 {
		t.Fatal("login failed")
	}
	cookie := login.Cookies()[0]
	get := func(validator string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", base+"/users", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("If-None-Match", validator)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Accept-Encoding") {
		t.Fatalf("users status=%d encoding=%q vary=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"), resp.Header.Values("Vary"))
	}
	reader, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var page api.UserPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(body) <= 1024 || len(page.Items) != 12 || page.NextCursor != "" || page.CollectionRevision != 13 {
		t.Fatalf("bytes=%d rows=%d revision=%d", len(body), len(page.Items), page.CollectionRevision)
	}
	if strings.Contains(string(body), "password") || strings.Contains(string(body), "username_key") {
		t.Fatal("private user fields exposed")
	}
	match := get(resp.Header.Get("ETag"))
	defer func() { _ = match.Body.Close() }()
	if match.StatusCode != 304 || match.Header.Get("Content-Encoding") != "" {
		t.Fatalf("matching validator status=%d encoding=%q", match.StatusCode, match.Header.Get("Content-Encoding"))
	}
	patch, err := http.NewRequest("PATCH", base+"/users/2", strings.NewReader(`{"expectedRevision":1,"role":"viewer"}`))
	if err != nil {
		t.Fatal(err)
	}
	patch.AddCookie(cookie)
	patch.Header.Set("Content-Type", "application/json")
	edited, err := client.Do(patch)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, edited.Body)
	_ = edited.Body.Close()
	if edited.StatusCode != 200 {
		t.Fatalf("edit status=%d", edited.StatusCode)
	}
	changed := get(resp.Header.Get("ETag"))
	_, _ = io.Copy(io.Discard, changed.Body)
	_ = changed.Body.Close()
	if changed.StatusCode != 200 || changed.Header.Get("ETag") == resp.Header.Get("ETag") {
		t.Fatal("edit did not invalidate HTTP representation")
	}
	t.Logf("USERS-HTTP rows=12 json_bytes=%d encoding=gzip vary=%s revalidation=304 edit=200 invalidation=200", len(body), strings.Join(resp.Header.Values("Vary"), ","))
}
