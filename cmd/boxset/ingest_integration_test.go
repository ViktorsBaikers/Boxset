//go:build integration && unix

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/ingest"
)

// TestNativeJSONRequestCap requires every native JSON-body operation to admit a
// document of exactly ingest.MaxJSONBytes and to answer 413 body_too_large one
// byte past it, through the built binary. One subtest drives each operation, so a
// transport bound set on only some of them fails here instead of passing on the
// operation the test happens to exercise. The pass line is printed only after
// every operation has been driven both ways.
func TestNativeJSONRequestCap(t *testing.T) {
	if ingest.MaxJSONBytes != 1<<20 {
		t.Fatalf("the native JSON cap is %d, want 1048576", ingest.MaxJSONBytes)
	}
	capacity := int(ingest.MaxJSONBytes)
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	c := client(t)
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("login admits 1048576 bytes and refuses 1048577", func(t *testing.T) {
		// An unknown principal keeps the admitted case independent of setup state.
		document := `{"username":"nobody","password":"` + syntheticPassword + `"}`
		if status, body := post(t, c, s.port, "/api/v1/session", "", paddedDocument(t, document, capacity)); status != http.StatusUnauthorized || problemCode(t, body) != "credentials_invalid" {
			t.Fatalf("login at 1048576 bytes: status=%d code=%q, want 401 credentials_invalid", status, problemCode(t, body))
		}
		if status, body := post(t, c, s.port, "/api/v1/session", "", paddedDocument(t, document, capacity+1)); status != http.StatusRequestEntityTooLarge || problemCode(t, body) != "body_too_large" {
			t.Fatalf("login at 1048577 bytes: status=%d code=%q, want 413 body_too_large", status, problemCode(t, body))
		}
		// A body with no declared length is bounded by the same cap, so the refusal
		// cannot come from a Content-Length the sender chose.
		if status, body := chunkedPost(t, c, s.port, "/api/v1/session", paddedDocument(t, document, capacity+1)); status != http.StatusRequestEntityTooLarge || problemCode(t, body) != "body_too_large" {
			t.Fatalf("chunked login at 1048577 bytes: status=%d code=%q, want 413 body_too_large", status, problemCode(t, body))
		}
	})

	t.Run("setup admits 1048576 bytes and refuses 1048577", func(t *testing.T) {
		document := `{"username":"admin","password":"` + syntheticPassword + `"}`
		// One byte over first: the setup token is still valid, so the cap answers
		// rather than a consumed setup.
		if status, body := post(t, c, s.port, "/api/v1/setup", string(token), paddedDocument(t, document, capacity+1)); status != http.StatusRequestEntityTooLarge || problemCode(t, body) != "body_too_large" {
			t.Fatalf("setup at 1048577 bytes: status=%d code=%q, want 413 body_too_large", status, problemCode(t, body))
		}
		if status, body := post(t, c, s.port, "/api/v1/setup", string(token), paddedDocument(t, document, capacity)); status != http.StatusCreated {
			t.Fatalf("setup at 1048576 bytes: status=%d code=%q, want 201", status, problemCode(t, body))
		}
	})

	t.Run("users patch admits 1048576 bytes and refuses 1048577", func(t *testing.T) {
		session, status := signIn(t, s.port, "admin")
		if status != http.StatusCreated {
			t.Fatalf("sign in: status=%d, want 201", status)
		}
		document := `{"expectedRevision":1,"role":"admin"}`
		patch := func(size int) (int, string) {
			t.Helper()
			request, err := http.NewRequest(http.MethodPatch, "http://127.0.0.1:"+s.port+"/api/v1/users/1",
				strings.NewReader(paddedDocument(t, document, size)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			return do(t, session, request)
		}
		if status, body := patch(capacity); status != http.StatusOK {
			t.Fatalf("users patch at 1048576 bytes: status=%d code=%q, want 200", status, problemCode(t, body))
		}
		if status, body := patch(capacity + 1); status != http.StatusRequestEntityTooLarge || problemCode(t, body) != "body_too_large" {
			t.Fatalf("users patch at 1048577 bytes: status=%d code=%q, want 413 body_too_large", status, problemCode(t, body))
		}
	})

	if t.Failed() {
		return
	}
	// The gate's expectation is the anchored line itself, so it goes to stdout
	// rather than through the test log's prefix.
	fmt.Println("NATIVE-JSON-CAP-PASS ops=3")
}

// paddedDocument returns document padded with insignificant trailing whitespace
// to exactly size bytes. A size below the document's own length cannot be reached
// by padding, so it fails instead of quietly handing back a shorter body.
func paddedDocument(t *testing.T, document string, size int) string {
	t.Helper()
	if size < len(document) {
		t.Fatalf("paddedDocument: %d bytes is below the %d-byte document", size, len(document))
	}
	return document + strings.Repeat(" ", size-len(document))
}

// chunkedPost sends one body with no declared length.
func chunkedPost(t *testing.T, c *http.Client, port, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+path,
		opaqueReader{reader: strings.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	return do(t, c, request)
}

// problemCode returns the problem code of a response body, or a marker when the
// body is not a problem at all.
func problemCode(t *testing.T, body string) string {
	t.Helper()
	var problem struct{ Code string }
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		return "unparsed"
	}
	return problem.Code
}

// opaqueReader hides the body's concrete type, so the transport cannot derive a
// Content-Length from it and streams the request chunked instead.
type opaqueReader struct{ reader io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.reader.Read(p) }
