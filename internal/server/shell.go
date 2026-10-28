package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The embedded dist is written by `bun --cwd web run build` and only ever inside a runner
// child or a Docker stage. The tracked dist/.keep lets `go build` compile without it, but
// New refuses such a tree: a build without the web producer never serves a headless fallback.
//
//go:embed all:dist
var embedded embed.FS

// The causes New reports for a shell it cannot serve. `serve` names the cause on stderr.
var (
	ErrBaseInvalid       = errors.New("base_invalid")
	ErrShellMissing      = errors.New("shell_missing")
	ErrShellInconsistent = errors.New("shell_inconsistent")
)

// shellRoutes are the only paths (below the base) that answer with the shell. The SPA's
// +page.svelte routes must equal this list (TestShellRoutesMatchSPA).
var shellRoutes = []string{"/", "/setup", "/sign-in"}

// placeholder is the base the web build is written under (web/vite.config.ts); loading
// replaces every occurrence with the runtime base.
const placeholder = "/__boxset_base__"

// encodings are the precompressed siblings the build writes, best first.
var encodings = []struct{ token, ext string }{{"br", ".br"}, {"zstd", ".zst"}, {"gzip", ".gz"}}

const immutableCache = "public, max-age=31536000, immutable"

// Only the attributed Kit 3.0.1 literal is allowed. Dependency drift must fail
// the installed-source assertion, never learn a new production allowance.
const fixedAnnouncerStyle = "position: absolute; left: 0; top: 0; clip: rect(0 0 0 0); clip-path: inset(50%); overflow: hidden; white-space: nowrap; width: 1px; height: 1px"
const fixedAnnouncerHash = "'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='"

// asset is one served /_app/ file: its identity bytes and any precompressed siblings.
type asset struct {
	contentType string
	identity    []byte
	encoded     map[string][]byte // token -> bytes; empty for a placeholder-bearing file
	immutable   bool
}

// shell is the loaded web build for one base: the rewritten fallback page with its CSP and
// the /_app/ files.
type shell struct {
	base   string
	html   []byte
	csp    string
	assets map[string]*asset // keyed by path below the base, e.g. "/_app/version.json"
}

// loadShell reads the built web app from fsys and rewrites it for rawBase. Nothing is
// served from an unusable tree: no 200.html is shell_missing; a build that was not made under
// the placeholder, or a placeholder-bearing file with a compressed sibling (which would carry the
// placeholder in another encoding), is shell_inconsistent.
func loadShell(fsys fs.FS, rawBase, env string) (*shell, error) {
	base, err := normalizeBase(rawBase)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		files[p] = b
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%w: reading the embedded build: %v", ErrShellInconsistent, err)
	}
	page, ok := files["200.html"]
	if !ok {
		return nil, fmt.Errorf("%w: the embedded dist has no 200.html (run `bun --cwd web run build` before building or testing)", ErrShellMissing)
	}

	// Split compressed siblings from the files they accompany.
	siblings := map[string]map[string][]byte{}
	for name := range files {
		for _, enc := range encodings {
			if main, found := strings.CutSuffix(name, enc.ext); found {
				if _, ok := files[main]; ok {
					if siblings[main] == nil {
						siblings[main] = map[string][]byte{}
					}
					siblings[main][enc.token] = files[name]
				}
			}
		}
	}

	rewrite := func(name string, b []byte) ([]byte, bool, error) {
		if !bytes.Contains(b, []byte(placeholder[1:])) {
			return b, false, nil
		}
		if len(siblings[name]) > 0 {
			return nil, true, fmt.Errorf("%w: %s holds the base placeholder and has a compressed sibling", ErrShellInconsistent, name)
		}
		out := bytes.ReplaceAll(b, []byte(placeholder), []byte(base))
		if bytes.Contains(out, []byte(placeholder[1:])) {
			return nil, true, fmt.Errorf("%w: %s holds a base placeholder that is not a rewritable %s", ErrShellInconsistent, name, placeholder)
		}
		return out, true, nil
	}

	html, held, err := rewrite("200.html", page)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, fmt.Errorf("%w: 200.html does not hold %s, so it was not built under the base placeholder", ErrShellInconsistent, placeholder)
	}
	html = envAttr.ReplaceAll(html, []byte(`${1}data-boxset-env="`+env+`"`))
	sh := &shell{base: base, html: html, csp: contentSecurityPolicy(html), assets: map[string]*asset{}}

	for name, b := range files {
		if !strings.HasPrefix(name, "_app/") || isSibling(name, files) {
			continue
		}
		body, _, err := rewrite(name, b)
		if err != nil {
			return nil, err
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		sh.assets["/"+name] = &asset{
			contentType: ct,
			identity:    body,
			encoded:     siblings[name],
			immutable:   strings.HasPrefix(name, "_app/immutable/"),
		}
	}
	return sh, nil
}

// isSibling reports whether name is a precompressed copy of another file in files.
func isSibling(name string, files map[string][]byte) bool {
	for _, enc := range encodings {
		if main, found := strings.CutSuffix(name, enc.ext); found {
			if _, ok := files[main]; ok {
				return true
			}
		}
	}
	return false
}

// envAttr is the root element's data-boxset-env attribute, which tells the setup page which
// command fetches the setup token. It is an attribute, not a script, so no hash changes.
var envAttr = regexp.MustCompile(`(<html\b[^>]*?\s)data-boxset-env(?:="[^"]*")?`)

// hostEnv is "container" inside a Docker or Podman container, else "native".
func hostEnv() string {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return "container"
		}
	}
	return "native"
}

var inlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
var scriptSrc = regexp.MustCompile(`(?i)\ssrc\s*=`)

// contentSecurityPolicy is the production policy for the shell. Its script allowance is the
// SHA-256 of every inline script of the page as it is served (after the base rewrite),
// never 'unsafe-inline'.
func contentSecurityPolicy(html []byte) string {
	var hashes []string
	for _, m := range inlineScript.FindAllSubmatch(html, -1) {
		if scriptSrc.Match(m[1]) {
			continue
		}
		sum := sha256.Sum256(m[2])
		h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !slices.Contains(hashes, h) {
			hashes = append(hashes, h)
		}
	}
	script := "'self'"
	if len(hashes) > 0 {
		script += " " + strings.Join(hashes, " ")
	}
	return "default-src 'self'; script-src " + script + "; style-src 'self'; style-src-attr 'unsafe-hashes' " + fixedAnnouncerHash + "; img-src 'self' data: blob:; " +
		"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
}

// register adds the public shell rows to mux, every shellRoutes entry and the /_app/ files,
// and returns their patterns.
func (sh *shell) register(mux *http.ServeMux) []string {
	var patterns []string
	for _, route := range shellRoutes {
		pattern := "GET " + route
		if route == "/" {
			pattern = "GET /{$}"
		}
		mux.HandleFunc(pattern, sh.servePage)
		patterns = append(patterns, pattern)
	}
	mux.HandleFunc("GET /_app/", sh.serveAsset)
	return append(patterns, "GET /_app/")
}

func (sh *shell) servePage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", sh.csp)
	http.ServeContent(w, r, "200.html", time.Time{}, bytes.NewReader(sh.html))
}

func (sh *shell) serveAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := sh.assets[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Vary", "Accept-Encoding")
	if a.immutable {
		h.Set("Cache-Control", immutableCache)
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	body := a.identity
	if token := negotiate(r.Header.Get("Accept-Encoding"), a.encoded); token != "" {
		h.Set("Content-Encoding", token)
		body = a.encoded[token]
	}
	http.ServeContent(w, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(body))
}

// negotiate picks the best precompressed encoding the client accepts (q > 0, or a wildcard
// that is not refused), or "" for identity.
func negotiate(accept string, have map[string][]byte) string {
	if len(have) == 0 || accept == "" {
		return ""
	}
	q := map[string]float64{}
	for part := range strings.SplitSeq(accept, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		weight := 1.0
		if _, v, ok := strings.Cut(strings.ReplaceAll(params, " ", ""), "q="); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			weight = f
		}
		q[strings.ToLower(strings.TrimSpace(token))] = weight
	}
	for _, enc := range encodings {
		if _, ok := have[enc.token]; !ok {
			continue
		}
		w, listed := q[enc.token]
		if !listed {
			w, listed = q["*"]
		}
		if listed && w > 0 {
			return enc.token
		}
	}
	return ""
}
