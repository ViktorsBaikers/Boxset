package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const (
	fakeScript = `{ base: "/__boxset_base__" }`
	fakePage   = `<!doctype html><html><body><script>` + fakeScript +
		`</script><script src="/__boxset_base__/_app/immutable/plain.js"></script></body></html>`
)

// fakeDist is a small build with every file shape the loader distinguishes: a file without the
// placeholder that has all three siblings, a placeholder-bearing file without any, and a
// non-immutable file.
func fakeDist(t *testing.T) fstest.MapFS {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("console.log(1);")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		".keep":                      {},
		"200.html":                   {Data: []byte(fakePage)},
		"_app/immutable/plain.js":    {Data: []byte("console.log(1);")},
		"_app/immutable/plain.js.br": {Data: []byte("br-bytes")},
		"_app/immutable/plain.js.zst": {
			Data: []byte("zstd-bytes"),
		},
		"_app/immutable/plain.js.gz": {Data: gz.Bytes()},
		"_app/immutable/based.js":    {Data: []byte("const b=`/__boxset_base__`;")},
		"_app/version.json":          {Data: []byte(`{"version":"1"}`)},
	}
}

func mustServer(t *testing.T, dist fs.FS, base string) http.Handler {
	t.Helper()
	s, err := newServer(dist, base)
	if err != nil {
		t.Fatalf("newServer(%q): %v", base, err)
	}
	return s.http.Handler
}

// realServer serves the embedded web build. An unbuilt tree is a failure, never a skip: every
// runtime consumer runs `bun --cwd=web run build` first.
func realServer(t *testing.T, base string) http.Handler {
	t.Helper()
	s, err := New(base)
	if err != nil {
		t.Fatalf("New(%q): %v (run `bun --cwd=web run build` first: the shell is required)", base, err)
	}
	return s.http.Handler
}

func serve(h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func inlineHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func installedAnnouncerStyle(t *testing.T) (string, string) {
	t.Helper()
	kit := filepath.Join("..", "..", "web", "node_modules", "@sveltejs", "kit")
	pkg, err := os.ReadFile(filepath.Join(kit, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var version struct{ Version string }
	if err := json.Unmarshal(pkg, &version); err != nil {
		t.Fatal(err)
	}
	if version.Version != "3.0.1" {
		t.Fatalf("announcer source: Kit version %q, want 3.0.1", version.Version)
	}
	source, err := os.ReadFile(filepath.Join(kit, "src", "runtime", "components", "root.svelte"))
	if err != nil {
		t.Fatal(err)
	}
	announcer := regexp.MustCompile(`(?s)<div\s+id="svelte-announcer"\s+aria-live="assertive"\s+aria-atomic="true"\s+style="([^"]+)"\s*>`).FindAllSubmatch(source, -1)
	if len(announcer) != 1 || len(announcer[0][1]) > 1024 {
		t.Fatal("installed announcer literal missing, ambiguous or oversized")
	}
	sum := sha256.Sum256(source)
	return string(announcer[0][1]), hex.EncodeToString(sum[:])
}

func TestFixedAnnouncerStylePolicy(t *testing.T) {
	style, _ := installedAnnouncerStyle(t)
	if style != fixedAnnouncerStyle {
		t.Fatal("installed announcer style changed; fixed production allowance must not update automatically")
	}
	if got := inlineHash(style); got != fixedAnnouncerHash {
		t.Fatalf("installed announcer hash = %q, want pinned %q", got, fixedAnnouncerHash)
	}
	for _, denied := range []string{strings.Replace(style, "1px", "2px", 1), "color: red;"} {
		if strings.Contains(contentSecurityPolicy([]byte(fakePage)), inlineHash(denied)) {
			t.Fatalf("production policy permits changed or unrelated attribute %q", denied)
		}
	}
}

func TestBaseNormalization(t *testing.T) {
	good := map[string]string{
		"": "", "/": "", "/media": "/media", "/media/": "/media", "/a/b": "/a/b",
		"/Media_1.x~y-z": "/Media_1.x~y-z",
	}
	for in, want := range good {
		got, err := normalizeBase(in)
		if err != nil || got != want {
			t.Errorf("normalizeBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"//", "///", strings.Repeat("/", maxBaseLen), strings.Repeat("/", maxBaseLen+1),
		"media", "//media", "/media//x", "/a/../b", "/a/./b", "/..", "/media x", "/media%2f", "/m\u00e9dia",
		"/a?b", "/a#b", `/a\b`, "/a;b", "/a:b", "/" + strings.Repeat("a", maxBaseLen),
	} {
		if got, err := normalizeBase(in); !errors.Is(err, ErrBaseInvalid) {
			t.Errorf("normalizeBase(%q) = %q, %v; want base_invalid", in, got, err)
		}
	}
	if _, err := newServer(fakeDist(t), "media"); !errors.Is(err, ErrBaseInvalid) {
		t.Errorf("a server on base %q: %v, want base_invalid", "media", err)
	}
}

func TestShellLoaderRefusals(t *testing.T) {
	keepOnly := fstest.MapFS{".keep": {}}
	if _, err := newServer(keepOnly, ""); !errors.Is(err, ErrShellMissing) {
		t.Errorf(".keep-only tree: %v, want shell_missing", err)
	}
	withSibling := fakeDist(t)
	withSibling["_app/immutable/based.js.gz"] = &fstest.MapFile{Data: []byte("gz")}
	if _, err := newServer(withSibling, ""); !errors.Is(err, ErrShellInconsistent) {
		t.Errorf("placeholder-bearing file with a .gz sibling: %v, want shell_inconsistent", err)
	}
	unbuiltBase := fakeDist(t)
	unbuiltBase["200.html"] = &fstest.MapFile{Data: []byte("<!doctype html><script>{}</script>")}
	if _, err := newServer(unbuiltBase, ""); !errors.Is(err, ErrShellInconsistent) {
		t.Errorf("200.html without the placeholder: %v, want shell_inconsistent", err)
	}
	stray := fakeDist(t)
	stray["_app/immutable/stray.js"] = &fstest.MapFile{Data: []byte(`x="__boxset_base__"`)}
	if _, err := newServer(stray, ""); !errors.Is(err, ErrShellInconsistent) {
		t.Errorf("a placeholder that is not /__boxset_base__: %v, want shell_inconsistent", err)
	}
}

func TestShellRewriteAndHashes(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		h := mustServer(t, fakeDist(t), base)
		page := serve(h, "GET", base+"/")
		body := page.Body.String()
		wantScript := `{ base: "` + base + `" }`
		if page.Code != http.StatusOK || !strings.Contains(body, wantScript) || strings.Contains(body, "__boxset_base__") {
			t.Fatalf("base %q: page %d %q lacks %q or still has the placeholder", base, page.Code, body, wantScript)
		}
		if !strings.Contains(body, `src="`+base+`/_app/immutable/plain.js"`) {
			t.Errorf("base %q: the script URL was not rewritten: %q", base, body)
		}
		csp := page.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, inlineHash(wantScript)) {
			t.Errorf("base %q: CSP %q lacks the hash of the rewritten script", base, csp)
		}
		if base != "" && strings.Contains(csp, inlineHash(fakeScript)) {
			t.Errorf("base %q: CSP holds the hash of the script before the rewrite", base)
		}
		wantPolicy := "default-src 'self'; script-src 'self' " + inlineHash(wantScript) +
			"; style-src 'self'; style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='; img-src 'self' data: blob:; " +
			"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
		if csp != wantPolicy {
			t.Errorf("base %q: CSP %q, want exact policy %q", base, csp, wantPolicy)
		}
		based := serve(h, "GET", base+"/_app/immutable/based.js", "Accept-Encoding", "br, zstd, gzip")
		if got, want := based.Body.String(), "const b=`"+base+"`;"; got != want {
			t.Errorf("base %q: rewritten asset = %q, want %q", base, got, want)
		}
	}
}

// The real build: every placeholder-bearing file is rewritten, no response in any encoding
// carries the placeholder, and the CSP holds the hash of each inline script as served.
func TestShellRewriteOfRealBuild(t *testing.T) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"", "/media"} {
		h := realServer(t, base)
		held, served := 0, 0
		err := fs.WalkDir(dist, "_app", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := path.Ext(p); ext == ".br" || ext == ".zst" || ext == ".gz" {
				return nil
			}
			raw, err := fs.ReadFile(dist, p)
			if err != nil {
				return err
			}
			for _, enc := range []string{"", "gzip", "br", "zstd", "br, zstd, gzip"} {
				w := serve(h, "GET", base+"/"+p, "Accept-Encoding", enc)
				if w.Code != http.StatusOK {
					t.Errorf("base %q %s (%q): %d", base, p, enc, w.Code)
					continue
				}
				if bytes.Contains(w.Body.Bytes(), []byte("__boxset_base__")) {
					t.Errorf("base %q %s (%q): response carries the placeholder", base, p, enc)
				}
			}
			served++
			if bytes.Contains(raw, []byte("/__boxset_base__")) {
				held++
				w := serve(h, "GET", base+"/"+p)
				if want := bytes.ReplaceAll(raw, []byte("/__boxset_base__"), []byte(base)); !bytes.Equal(w.Body.Bytes(), want) {
					t.Errorf("base %q %s: not the rewritten identity bytes", base, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if held == 0 || served < 2 {
			t.Fatalf("base %q: %d of %d _app files hold the placeholder; the build is not the placeholder shell", base, held, served)
		}

		page := serve(h, "GET", base+"/")
		body := page.Body.String()
		if page.Code != http.StatusOK || strings.Contains(body, "__boxset_base__") || !strings.Contains(body, `base: "`+base+`"`) {
			t.Fatalf("base %q: page %d is not the rewritten shell", base, page.Code)
		}
		var scripts []string
		rest := body
		for {
			i := strings.Index(rest, "<script>")
			if i < 0 {
				break
			}
			rest = rest[i+len("<script>"):]
			j := strings.Index(rest, "</script>")
			scripts = append(scripts, rest[:j])
			rest = rest[j:]
		}
		if len(scripts) == 0 {
			t.Fatalf("base %q: the shell has no inline script to hash", base)
		}
		csp := page.Header().Get("Content-Security-Policy")
		for _, s := range scripts {
			if !strings.Contains(csp, inlineHash(s)) {
				t.Errorf("base %q: CSP %q lacks the hash of an inline script as served", base, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "img-src 'self' data: blob:") {
			t.Errorf("base %q: CSP %q", base, csp)
		}
	}
}

func TestShellOnlyListedRoutes(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		h := mustServer(t, fakeDist(t), base)
		listed := []string{base + "/"}
		if base != "" {
			listed = append(listed, base)
		}
		for _, p := range listed {
			w := serve(h, "GET", p)
			if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
				w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Errorf("GET %s = %d %q %q: want the shell", p, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
			}
		}
		if w := serve(h, "HEAD", base+"/"); w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("HEAD %s/ = %d with %d body bytes", base, w.Code, w.Body.Len())
		}
		if w := serve(h, "POST", base+"/"); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s/ = %d, want 405", base, w.Code)
		}
		if w := serve(h, "GET", base+"/ping"); w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Errorf("GET %s/ping = %d %q, want 200 ok", base, w.Code, w.Body.String())
		}
		refused := map[string]int{
			base + "/unlisted":                   http.StatusNotFound,
			base + "/index.html":                 http.StatusNotFound,
			base + "/200.html":                   http.StatusNotFound,
			base + "/_app/":                      http.StatusNotFound,
			base + "/_app/nope.js":               http.StatusNotFound,
			base + "/_app/immutable/plain.js.br": http.StatusNotFound,
			base + "/%2e%2e/":                    http.StatusBadRequest,
			base + "/./":                         http.StatusBadRequest,
			base + "/x%5cy":                      http.StatusBadRequest,
		}
		if base != "" {
			refused["/"] = http.StatusNotFound
			refused["/ping"] = http.StatusNotFound
			refused["/mediax/"] = http.StatusNotFound
			refused["/mediax"] = http.StatusNotFound
			refused["//media/"] = http.StatusNotFound
			refused["/media//"] = http.StatusNotFound
			refused["/media%2F.."] = http.StatusBadRequest
			refused["/media%2f../x"] = http.StatusBadRequest
			refused["/media/../ping"] = http.StatusBadRequest
			refused["/MEDIA/"] = http.StatusNotFound
		} else {
			refused["//"] = http.StatusNotFound
		}
		for p, want := range refused {
			w := serve(h, "GET", p, "Accept", "text/html")
			if w.Code != want || strings.Contains(w.Body.String(), "<!doctype html>") {
				t.Errorf("GET %s = %d, want %d and never the shell (base %q)", p, w.Code, want, base)
			}
		}
	}
}

// shellRoutes must be the SPA's own routes: every +page.svelte below web/src/routes.
func TestShellRoutesMatchSPA(t *testing.T) {
	root := filepath.Join("..", "..", "web", "src", "routes")
	var routes []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "+page.svelte" {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		var segs []string
		for seg := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
			if seg != "." && (!strings.HasPrefix(seg, "(") || !strings.HasSuffix(seg, ")")) {
				segs = append(segs, seg)
			}
		}
		routes = append(routes, "/"+strings.Join(segs, "/"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(routes)
	want := slices.Clone(shellRoutes)
	slices.Sort(want)
	if len(routes) == 0 || !slices.Equal(routes, want) {
		t.Errorf("SPA routes %v, shellRoutes %v: they must match", routes, want)
	}
}

func TestUnknownAPINeverShell(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		h := mustServer(t, fakeDist(t), base)
		for _, p := range []string{
			"/api", "/api/", "/api/v1/system/status", "/api/v1/openapi.json", "/api/v3/series", "/api/v2.0/app/version",
			"/series/api/v3/system/status", "/movie/api/", "/indexer/api/v2.0/indexers", "/api/v1/", "/api/%2e/x",
		} {
			w := serve(h, "GET", base+p, "Accept", "text/html,application/xhtml+xml")
			if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "<!doctype html>") ||
				strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				t.Errorf("base %q GET %s = %d %q: an API path must never be the shell", base, p, w.Code, w.Header().Get("Content-Type"))
			}
		}
	}
}

func TestImmutableAssetHeaders(t *testing.T) {
	h := realServer(t, "/media")
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = fs.WalkDir(dist, "_app/immutable", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) == ".br" || path.Ext(p) == ".zst" || path.Ext(p) == ".gz" {
			return err
		}
		w := serve(h, "GET", "/media/"+p)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
			w.Header().Get("Vary") != "Accept-Encoding" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d Cache-Control %q Vary %q", p, w.Code, w.Header().Get("Cache-Control"), w.Header().Get("Vary"))
		}
		if strings.HasSuffix(p, ".js") && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: Content-Type %q", p, w.Header().Get("Content-Type"))
		}
		if strings.HasSuffix(p, ".css") && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
			t.Errorf("%s: Content-Type %q", p, w.Header().Get("Content-Type"))
		}
		checked++
		return nil
	})
	if err != nil || checked == 0 {
		t.Fatalf("checked %d immutable files: %v", checked, err)
	}
	// Both mounted origins serve the exact admitted font and its original copyright/OFL notice.
	fontPackage := filepath.Join("..", "..", "web", "node_modules", "@fontsource-variable", "archivo")
	for _, base := range []string{"", "/media"} {
		for _, asset := range []struct{ source, suffix string }{
			{filepath.Join("files", "archivo-latin-standard-normal.woff2"), ".woff2"},
			{"LICENSE", ".txt"},
		} {
			want, err := os.ReadFile(filepath.Join(fontPackage, asset.source))
			if err != nil {
				t.Fatal(err)
			}
			matches := 0
			entries, err := fs.ReadDir(dist, "_app/immutable/assets")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), asset.suffix) {
					continue
				}
				matches++
				w := serve(realServer(t, base), "GET", base+"/_app/immutable/assets/"+entry.Name(), "Accept-Encoding", "identity")
				if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
					t.Errorf("base %q font asset %s differs from installed %s", base, entry.Name(), asset.source)
				}
				if asset.source == "LICENSE" {
					sum := sha256.Sum256(want)
					if entry.Name() != fmt.Sprintf("archivo-OFL.%x.txt", sum[:8]) {
						t.Errorf("notice is not content-addressed: %s", entry.Name())
					}
				}
			}
			if matches != 1 {
				t.Errorf("base %q: found %d %s assets, want exactly one", base, matches, asset.suffix)
			}
		}
	}
	// Only hashed files are immutable: the version file revalidates.
	if w := serve(h, "GET", "/media/_app/version.json"); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("version.json: %d Cache-Control %q, want no-cache", w.Code, w.Header().Get("Cache-Control"))
	}
}

func TestPrecompressedNegotiation(t *testing.T) {
	dist := fakeDist(t)
	h := mustServer(t, dist, "/media")
	plain := "/media/_app/immutable/plain.js"
	cases := []struct{ accept, enc, body string }{
		{"", "", "console.log(1);"},
		{"identity", "", "console.log(1);"},
		{"gzip", "gzip", string(dist["_app/immutable/plain.js.gz"].Data)},
		{"br", "br", "br-bytes"},
		{"zstd", "zstd", "zstd-bytes"},
		{"gzip, br", "br", "br-bytes"},
		{"gzip, zstd", "zstd", "zstd-bytes"},
		{"br;q=0, gzip", "gzip", string(dist["_app/immutable/plain.js.gz"].Data)},
		{"gzip;q=0.5, zstd;q=0.9", "zstd", "zstd-bytes"},
		{"*", "br", "br-bytes"},
		{"*, br;q=0", "zstd", "zstd-bytes"},
		{"br;q=0, zstd;q=0, gzip;q=0", "", "console.log(1);"},
	}
	for _, c := range cases {
		w := serve(h, "GET", plain, "Accept-Encoding", c.accept)
		if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != c.enc || w.Body.String() != c.body ||
			w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: %d CE %q Vary %q body %q, want CE %q", c.accept, w.Code,
				w.Header().Get("Content-Encoding"), w.Header().Get("Vary"), w.Body.String(), c.enc)
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader([]byte(cases[2].body)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(zr); err != nil || string(got) != "console.log(1);" {
		t.Errorf("the gzip sibling decodes to %q, %v", got, err)
	}

	// A placeholder-bearing file is served only as its rewritten identity bytes.
	for _, accept := range []string{"", "gzip", "br", "zstd", "br, zstd, gzip", "*"} {
		w := serve(h, "GET", "/media/_app/immutable/based.js", "Accept-Encoding", accept)
		if w.Header().Get("Content-Encoding") != "" || w.Body.String() != "const b=`/media`;" || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("based.js under %q: CE %q body %q", accept, w.Header().Get("Content-Encoding"), w.Body.String())
		}
	}
}

// Compare the actual HTTP representation with a separately requested identity response.
// Bun's native decoders are independent of the precompress producer's encoding calls.
func TestPrecompressedSiblingsOfRealBuild(t *testing.T) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		t.Fatal(err)
	}
	var representations []struct {
		Name, Encoding string
		Body, Identity []byte
	}
	for _, base := range []string{"", "/media"} {
		h := realServer(t, base)
		for token, ext := range map[string]string{"br": ".br", "zstd": ".zst", "gzip": ".gz"} {
			checked := 0
			err := fs.WalkDir(dist, "_app/immutable", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || path.Ext(p) == ".br" || path.Ext(p) == ".zst" || path.Ext(p) == ".gz" {
					return err
				}
				raw, err := fs.ReadFile(dist, p)
				if err != nil {
					return err
				}
				identity := serve(h, "GET", base+"/"+p, "Accept-Encoding", "identity")
				w := serve(h, "GET", base+"/"+p, "Accept-Encoding", token)
				if identity.Code != http.StatusOK || identity.Header().Get("Content-Encoding") != "" ||
					bytes.Contains(identity.Body.Bytes(), []byte("__boxset_base__")) {
					t.Errorf("%s%s: invalid identity response", base, p)
				}
				sibling, err := fs.ReadFile(dist, p+ext)
				if bytes.Contains(raw, []byte("__boxset_base__")) {
					if !errors.Is(err, fs.ErrNotExist) || w.Code != http.StatusOK ||
						w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), identity.Body.Bytes()) ||
						w.Header().Get("Vary") != "Accept-Encoding" {
						t.Errorf("%s%s under %s: placeholder response is not rewritten identity only", base, p, token)
					}
					return nil
				}
				if err != nil {
					return err
				}
				if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != token ||
					!bytes.Equal(w.Body.Bytes(), sibling) || w.Header().Get("Vary") != "Accept-Encoding" {
					t.Errorf("%s%s under %s: status %d CE %q, %d bytes (want %d)", base, p, token,
						w.Code, w.Header().Get("Content-Encoding"), w.Body.Len(), len(sibling))
				}
				representations = append(representations, struct {
					Name, Encoding string
					Body, Identity []byte
				}{base + "/" + p, token, w.Body.Bytes(), identity.Body.Bytes()})
				checked++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if checked == 0 {
				t.Fatalf("base %q: no %s representation decoded", base, token)
			}
		}
	}
	input, err := json.Marshal(representations)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "-e", `
import assert from "node:assert/strict";
import { brotliDecompressSync } from "node:zlib";
const rows = JSON.parse(await Bun.stdin.text());
for (const row of rows) {
	const bytes = Buffer.from(row.Body, "base64");
	const decoded = row.Encoding === "br" ? brotliDecompressSync(bytes)
		: row.Encoding === "zstd" ? Bun.zstdDecompressSync(bytes) : Bun.gunzipSync(bytes);
	assert.deepEqual(Buffer.from(decoded), Buffer.from(row.Identity, "base64"), row.Name + " " + row.Encoding);
}
console.log("SHELL-CODECS-PASS representations=" + rows.length);
`)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("decoding HTTP representations: %v\n%s", err, out)
	}
	t.Log(string(out))
}

// The root element carries the detected environment as an attribute, so the setup page can
// show the command that prints the token; no script and no hash change with it.
func TestShellEnvAttribute(t *testing.T) {
	for _, env := range []string{"container", "native"} {
		dist := fakeDist(t)
		dist["200.html"] = &fstest.MapFile{Data: []byte(strings.Replace(fakePage, "<html>", `<html lang="en" data-boxset-env>`, 1))}
		sh, err := loadShell(dist, "/media", env)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(sh.html, []byte(`<html lang="en" data-boxset-env="`+env+`">`)) {
			t.Errorf("env %s: page %q", env, sh.html)
		}
		if !strings.Contains(sh.csp, inlineHash(`{ base: "/media" }`)) {
			t.Errorf("env %s: the attribute changed the script hashes: %q", env, sh.csp)
		}
	}
	page := serve(realServer(t, ""), "GET", "/")
	if !strings.Contains(page.Body.String(), `data-boxset-env="`+hostEnv()+`"`) {
		t.Errorf("the real shell does not name the environment %q", hostEnv())
	}
}
