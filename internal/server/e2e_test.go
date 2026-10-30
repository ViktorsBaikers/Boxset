//go:build e2e && unix

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/redact"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
	"github.com/ViktorsBaikers/Boxset/internal/store"
	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

const fixtureEnvironment = "BOXSET_SESSION_FIXTURE_CONTRACT"
const fixtureTransitionHeader = "X-Boxset-Test-Role-Transition"
const fixtureBeforeHeader = "X-Boxset-Test-Revision-Before"
const fixtureAfterHeader = "X-Boxset-Test-Revision-After"
const fixtureRevisionMax = int64(9007199254740991)

type sessionFixtureDescriptor struct {
	Version          int      `json:"version"`
	TransitionHeader string   `json:"transitionHeader"`
	BeforeHeader     string   `json:"beforeHeader"`
	AfterHeader      string   `json:"afterHeader"`
	Operations       []string `json:"operations"`
}

func sessionFixtureContract() sessionFixtureDescriptor {
	return sessionFixtureDescriptor{1, fixtureTransitionHeader, fixtureBeforeHeader, fixtureAfterHeader, []string{"restrict", "restore"}}
}

func sessionFixtureEncoding() string {
	raw, err := json.Marshal(sessionFixtureContract())
	if err != nil {
		panic("fixture descriptor encoding")
	}
	return string(raw)
}

func validateSessionFixtureEnvironment(env []string) error {
	var raw string
	count := 0
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, fixtureEnvironment+"="); ok {
			raw = value
			count++
		}
	}
	if count != 1 || len(raw) > 512 || !utf8.ValidString(raw) {
		return errors.New("fixture descriptor invalid")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("fixture descriptor invalid")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return errors.New("fixture descriptor invalid")
		}
		switch name {
		case "version", "transitionHeader", "beforeHeader", "afterHeader", "operations":
		default:
			return errors.New("fixture descriptor invalid")
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return errors.New("fixture descriptor invalid")
		}
	}
	if _, err := d.Token(); err != nil {
		return errors.New("fixture descriptor invalid")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("fixture descriptor invalid")
	}
	strict := json.NewDecoder(strings.NewReader(raw))
	strict.DisallowUnknownFields()
	var descriptor sessionFixtureDescriptor
	if strict.Decode(&descriptor) != nil || !reflect.DeepEqual(descriptor, sessionFixtureContract()) || len(seen) != 5 {
		return errors.New("fixture descriptor invalid")
	}
	return nil
}

type sessionRoleFixture struct {
	next      http.Handler
	base      string
	user      int64
	write     func(context.Context, func(*sql.Tx) error) error
	admission chan struct{}
	phase     int
}

type fixtureLostDelivery struct {
	header http.Header
	writes int
}

func (w *fixtureLostDelivery) Header() http.Header       { return w.header }
func (w *fixtureLostDelivery) WriteHeader(int)           {}
func (w *fixtureLostDelivery) Write([]byte) (int, error) { w.writes++; return 0, io.ErrClosedPipe }

func newSessionRoleFixture(next http.Handler, db *store.DB, base string, user int64) *sessionRoleFixture {
	return &sessionRoleFixture{next: next, base: base, user: user, write: db.Write, admission: make(chan struct{}, 1)}
}

func fixtureProblem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%q}`, code)
}

func (f *sessionRoleFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var values []string
	present := false
	fields := 0
	for name, entries := range r.Header {
		if strings.EqualFold(name, fixtureTransitionHeader) {
			present = true
			fields++
			values = append(values, entries...)
		}
	}
	if !present {
		f.next.ServeHTTP(w, r)
		return
	}
	if fields != 1 || len(values) != 1 || len(values[0]) < 1 || len(values[0]) > 8 || (values[0] != "restrict" && values[0] != "restore") || r.Method != "GET" || r.URL.Path != f.base+"/api/v1/session" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		fixtureProblem(w, 400, "fixture_transition_invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	select {
	case f.admission <- struct{}{}:
		defer func() { <-f.admission }()
	case <-ctx.Done():
		fixtureProblem(w, 500, "fixture_transition_failed")
		return
	}
	operation := values[0]
	if (operation == "restrict" && f.phase != 0) || (operation == "restore" && f.phase != 1) {
		fixtureProblem(w, 409, "fixture_transition_state")
		return
	}
	prior, next := "admin", "viewer"
	if operation == "restore" {
		prior, next = "viewer", "admin"
	}
	var before, after int64
	stateError := errors.New("fixture state")
	err := f.write(ctx, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx, "SELECT role, revision FROM user WHERE id = ?", f.user).Scan(&role, &before); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return stateError
			}
			return err
		}
		if role != prior || before < 1 || before >= fixtureRevisionMax {
			return stateError
		}
		after = before + 1
		result, err := tx.ExecContext(ctx, "UPDATE user SET role = ?, revision = ? WHERE id = ? AND role = ? AND revision = ?", next, after, f.user, prior, before)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return stateError
		}
		var got int64
		if err := tx.QueryRowContext(ctx, "SELECT role, revision FROM user WHERE id = ?", f.user).Scan(&role, &got); err != nil {
			return err
		}
		if role != next || got != after {
			return stateError
		}
		return nil
	})
	if err != nil {
		// A joined rollback failure is not a known state refusal.
		if err == stateError {
			fixtureProblem(w, 409, "fixture_transition_state")
		} else {
			fixtureProblem(w, 500, "fixture_transition_failed")
		}
		return
	}
	f.phase++
	w.Header().Set(fixtureBeforeHeader, strconv.FormatInt(before, 10))
	w.Header().Set(fixtureAfterHeader, strconv.FormatInt(after, 10))
	clean := r.Clone(ctx)
	for name := range clean.Header {
		if strings.EqualFold(name, fixtureTransitionHeader) {
			delete(clean.Header, name)
		}
	}
	f.next.ServeHTTP(w, clean)
}

type browserOutput struct {
	mu       sync.Mutex
	limit    int
	bytes    []byte
	overflow bool
}

func (o *browserOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	limit := o.limit
	if limit == 0 {
		limit = 64 << 10
	}
	remaining := limit - len(o.bytes)
	if len(p) > remaining {
		o.overflow = true
		p = p[:remaining]
	}
	o.bytes = append(o.bytes, p...)
	return n, nil
}

func (o *browserOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return redact.String(string(o.bytes))
}

func browserEnv(extra ...string) []string {
	env := []string{"CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local"}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "PLAYWRIGHT_BROWSERS_PATH", "BOXSET_CAPTURE_DIR", "BOXSET_CAPTURE_CHALLENGE"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, extra...)
}

func browserCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func sessionBrowser(t *testing.T, mode string, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cmd := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "shell.spec.ts", "session.spec.ts")
	cmd.Dir = filepath.Join("..", "..", "web")
	cmd.Env = sessionBrowserEnvironment(t, mode, env)
	want := 45 // 41 preserved shell cases and four session cases
	switch mode {
	case "two-installations":
		want = 43 // the same shell cases and two proxy modes
	case "local-access":
		want = 43 // the same shell cases and the two grant bases
	}
	runSessionBrowser(t, cmd, want, nil)
	t.Logf("BROWSER-SESSION mode=%s passed=%d csp=production", mode, want)
}

func runSessionBrowser(t *testing.T, cmd *exec.Cmd, want int, cohort *fixtureCohort) {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "current.json")
	cmd.Env = append(cmd.Env, "PLAYWRIGHT_JSON_OUTPUT_FILE="+reportPath)
	started := time.Now()
	runID := rand.Text()
	output := &browserOutput{}
	if cohort != nil {
		// A cohort run keeps the transcript budget the two-case canonical cohort
		// always had, per case: more cases are not a looser bound.
		output.limit = (64 << 10) * len(cohort.cases) / 2
	}
	cmd.Stdout, cmd.Stderr = output, output
	runErr := cmd.Run()
	if cmd.Process != nil && cmd.ProcessState != nil {
		t.Logf("SESSION-BROWSER-PROCESS run=%s pid=%d waited=true exit=%d", runID, cmd.Process.Pid, cmd.ProcessState.ExitCode())
	}
	const reportLimit = 8 << 20
	var raw []byte
	reportFile, reportErr := os.Open(reportPath)
	if reportErr == nil {
		raw, reportErr = io.ReadAll(io.LimitReader(reportFile, reportLimit+1))
		_ = reportFile.Close()
	}
	if cohort != nil {
		cases, err := sessionFixtureReport(raw, started, *cohort)
		source, sourceErr := os.ReadFile(filepath.Join(cmd.Dir, "e2e", "session.spec.ts"))
		declared := map[string]int{}
		for _, expected := range cohort.cases {
			line := 0
			for index, text := range strings.Split(string(source), "\n") {
				if !strings.Contains(text, expected.declaration) {
					continue
				}
				if line != 0 {
					sourceErr = errors.New("fixture source declaration ambiguous")
				}
				line = index + 1
			}
			if line == 0 {
				sourceErr = errors.New("fixture source declaration absent")
			}
			declared[expected.title] = line
		}
		for _, item := range cases {
			if item.Line != declared[item.Name] {
				sourceErr = errors.New("fixture source declaration mismatch")
			}
		}
		exit := 0
		if runErr != nil {
			exit = -1
			var child *exec.ExitError
			if errors.As(runErr, &child) {
				exit = child.ExitCode()
			}
		}
		projection, encodeErr := json.Marshal(struct {
			Run       string                  `json:"run"`
			Source    string                  `json:"source"`
			Report    string                  `json:"report"`
			ChildExit int                     `json:"childExit"`
			Cases     []fixtureCaseProjection `json:"cases"`
			Valid     bool                    `json:"valid"`
		}{runID, fmt.Sprintf("%x", sha256.Sum256(source)), fmt.Sprintf("%x", sha256.Sum256(raw)), exit, cases, err == nil && sourceErr == nil && reportErr == nil && !output.overflow})
		if encodeErr != nil || len(projection) > 16*1024 {
			t.Fatal("fixture projection overflow")
		}
		t.Logf("SESSION-FIXTURE-REPORT %s", projection)
		if err != nil || sourceErr != nil || reportErr != nil || output.overflow {
			t.Fatal("current fixture report rejected")
		}
		if runErr != nil {
			t.Fatalf("session browser child failed exit=%d\n%s", exit, output.String())
		}
	}
	if reportErr != nil || len(raw) > reportLimit || output.overflow {
		t.Fatal("session browser report unavailable or oversized")
	}
	if runErr != nil {
		t.Fatalf("session browser: %v\n%s", runErr, output.String())
	}
	var report struct {
		Stats struct{ Expected, Unexpected, Skipped, Flaky int }
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Stats.Expected != want || report.Stats.Unexpected != 0 || report.Stats.Skipped != 0 || report.Stats.Flaky != 0 {
		t.Fatalf("session browser stats %+v", report.Stats)
	}
}

type fixtureFailureProjection struct {
	ID   string `json:"id"`
	File string `json:"file"`
	Line int    `json:"line"`
}

type fixtureCaseProjection struct {
	Name         string                     `json:"name"`
	File         string                     `json:"file"`
	Line         int                        `json:"line"`
	Status       string                     `json:"status"`
	Failures     []fixtureFailureProjection `json:"failures"`
	Observations []string                   `json:"observations"`
}

// fixtureCase is one expected case of a cohort: the title the browser run must
// report and the exact source declaration it is launched from. Each mode judges
// only its own cases, so a well-formed report is never compared with another
// mode's titles.
type fixtureCase struct {
	title       string
	declaration string
}

// fixtureCohort is the exact cohort one browser run declares, and whether a
// passed case must carry the four credential observations of the sign-in matrix.
type fixtureCohort struct {
	cases        []fixtureCase
	observations bool
}

const signInDeclaration = "test(`${base}: real sign-in, current session, revocation and sign-out`"
const recoveryDeclaration = "test(`${base}: cold recovery navigation serves the public state and denies protected data (${recovery})`"

var (
	// signInCohort is the canonical sign-in, revocation and sign-out matrix.
	signInCohort = fixtureCohort{observations: true, cases: []fixtureCase{
		{title: "root: real sign-in, current session, revocation and sign-out", declaration: signInDeclaration},
		{title: "media: real sign-in, current session, revocation and sign-out", declaration: signInDeclaration},
	}}
	// recoveryCohort is cold navigation into a real recovering installation: both
	// recovery reasons at both supported bases, each under its own title so the
	// report names the state it judged.
	recoveryCohort = fixtureCohort{cases: []fixtureCase{
		{title: recoveryTitle("root", "key_missing"), declaration: recoveryDeclaration},
		{title: recoveryTitle("root", "restore_in_progress"), declaration: recoveryDeclaration},
		{title: recoveryTitle("media", "key_missing"), declaration: recoveryDeclaration},
		{title: recoveryTitle("media", "restore_in_progress"), declaration: recoveryDeclaration},
	}}
)

func recoveryTitle(base, state string) string {
	return fmt.Sprintf("%s: cold recovery navigation serves the public state and denies protected data (%s)", base, state)
}

func sessionFixtureReport(raw []byte, started time.Time, cohort fixtureCohort) ([]fixtureCaseProjection, error) {
	refuse := errors.New("fixture report invalid")
	if len(raw) == 0 || len(raw) > 8<<20 {
		return nil, refuse
	}
	type result struct {
		Status        string
		Retry         int
		ErrorLocation struct {
			File string
			Line int
		}
		Errors []struct {
			Message  string
			Location struct {
				File string
				Line int
			}
		}
		Stdout []struct{ Text, Buffer string }
	}
	type spec struct {
		Title, File string
		Line        int
		Tests       []struct {
			ExpectedStatus string
			Results        []result
		}
	}
	type suite struct {
		Specs  []spec
		Suites []json.RawMessage
	}
	var report struct {
		Errors []json.RawMessage
		Stats  struct {
			Expected, Unexpected, Skipped, Flaky int
			StartTime                            time.Time
		}
		Suites []json.RawMessage
	}
	if json.Unmarshal(raw, &report) != nil || report.Stats.StartTime.Before(started.Add(-time.Second)) || report.Stats.StartTime.After(time.Now().Add(time.Second)) || report.Stats.Skipped != 0 || report.Stats.Flaky != 0 || report.Stats.Expected+report.Stats.Unexpected != len(cohort.cases) {
		return nil, refuse
	}
	wanted := map[string]bool{}
	for _, expected := range cohort.cases {
		wanted[expected.title] = false
	}
	var cases []fixtureCaseProjection
	var visit func([]json.RawMessage) error
	failureID := regexp.MustCompile(`FRESHNESS_[A-Z_]+`)
	visit = func(nodes []json.RawMessage) error {
		for _, node := range nodes {
			var current suite
			if json.Unmarshal(node, &current) != nil {
				return refuse
			}
			for _, item := range current.Specs {
				seen, known := wanted[item.Title]
				if !known || seen || (item.File != "session.spec.ts" && item.File != "e2e/session.spec.ts") || item.Line < 1 || len(item.Tests) != 1 || item.Tests[0].ExpectedStatus != "passed" || len(item.Tests[0].Results) != 1 {
					return refuse
				}
				wanted[item.Title] = true
				r := item.Tests[0].Results[0]
				if r.Status != "passed" && r.Status != "failed" && r.Status != "timedOut" {
					return refuse
				}
				if r.Retry != 0 || (r.Status == "passed" && len(r.Errors) != 0) || (r.Status == "failed" && len(r.Errors) == 0) {
					return refuse
				}
				c := fixtureCaseProjection{Name: item.Title, File: item.File, Line: item.Line, Status: r.Status, Failures: []fixtureFailureProjection{}, Observations: []string{}}
				for _, problem := range r.Errors {
					id := failureID.FindString(problem.Message)
					if id == "" {
						id = "other"
					}
					if len(c.Failures) >= 8 {
						return refuse
					}
					line := problem.Location.Line
					file := filepath.Base(problem.Location.File)
					if line == 0 {
						line = r.ErrorLocation.Line
						file = filepath.Base(r.ErrorLocation.File)
					}
					if file != "session.spec.ts" {
						file, id = "other", "other"
					}
					c.Failures = append(c.Failures, fixtureFailureProjection{id, file, line})
				}
				var observed strings.Builder
				for _, out := range r.Stdout {
					text := out.Text
					if out.Buffer != "" {
						if len(out.Buffer) > 2048 || text != "" {
							return refuse
						}
						decoded, err := base64.StdEncoding.DecodeString(out.Buffer)
						if err != nil || !utf8.Valid(decoded) {
							return refuse
						}
						text = string(decoded)
					}
					if observed.Len()+len(text) > 16*1024 {
						return refuse
					}
					observed.WriteString(text)
				}
				for _, text := range strings.Split(observed.String(), "\n") {
					if strings.HasPrefix(text, "SESSION-FRESHNESS ") {
						if len(text) > 1024 || len(c.Observations) >= 12 {
							return refuse
						}
						var fact struct {
							Phase      string `json:"phase"`
							Fetched    bool   `json:"fetched"`
							Status     int    `json:"status"`
							NoStore    bool   `json:"noStore"`
							Restricted bool   `json:"restricted"`
						}
						decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(text, "SESSION-FRESHNESS ")))
						decoder.DisallowUnknownFields()
						if decoder.Decode(&fact) != nil || (fact.Phase != "initial" && fact.Phase != "restrict" && fact.Phase != "restore" && fact.Phase != "revoke") || !fact.Fetched || !fact.NoStore || (fact.Status != 200 && fact.Status != 401) || fact.Restricted != (fact.Phase == "restrict") || (fact.Status == 401) != (fact.Phase == "revoke") {
							return refuse
						}
						if _, err := decoder.Token(); err != io.EOF {
							return refuse
						}
						canonical, err := json.Marshal(fact)
						if err != nil {
							return refuse
						}
						c.Observations = append(c.Observations, string(canonical))
					}
				}
				if r.Status == "passed" && cohort.observations {
					if len(c.Observations) != 4 {
						return refuse
					}
					for index, phase := range []string{"initial", "restrict", "restore", "revoke"} {
						if !strings.Contains(c.Observations[index], `"phase":"`+phase+`"`) {
							return refuse
						}
					}
				}
				cases = append(cases, c)
			}
			if err := visit(current.Suites); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(report.Suites); err != nil {
		return cases, err
	}
	if len(cases) != len(cohort.cases) || len(report.Errors) != 0 {
		return cases, refuse
	}
	passed := 0
	for _, c := range cases {
		if c.Status == "passed" {
			passed++
		}
	}
	if passed != report.Stats.Expected {
		return cases, refuse
	}
	return cases, nil
}

func sessionBrowserEnvironment(t *testing.T, mode string, env []string) []string {
	t.Helper()
	if mode == "states" {
		if err := validateSessionFixtureEnvironment(env); err != nil {
			t.Fatal(err)
		}
	}
	style, sourceHash := installedAnnouncerStyle(t)
	if style != fixedAnnouncerStyle || inlineHash(style) != fixedAnnouncerHash {
		t.Fatal("installed announcer style differs from fixed production permission")
	}
	return append(env, "BOXSET_SESSION_MODE="+mode, "BOXSET_SHELL_HOST=production", "BOXSET_CSP_SOURCE_STYLE="+style, "BOXSET_CSP_SOURCE_SHA256="+sourceHash)
}

func TestSessionBrowserEnvironment(t *testing.T) {
	style, sourceHash := installedAnnouncerStyle(t)
	for _, mode := range []string{"states", "two-installations", "local-access"} {
		base := browserEnv()
		if mode == "states" {
			base = append(base, fixtureEnvironment+"="+sessionFixtureEncoding())
		}
		env := sessionBrowserEnvironment(t, mode, base)
		for _, want := range []string{"BOXSET_SESSION_MODE=" + mode, "BOXSET_SHELL_HOST=production", "BOXSET_CSP_SOURCE_STYLE=" + style, "BOXSET_CSP_SOURCE_SHA256=" + sourceHash} {
			found := 0
			for _, entry := range env {
				if entry == want {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("%s: installed production prerequisite missing or duplicated", mode)
			}
		}
	}
}

func TestLifecycleResponseCompletion(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	s.SetState(Ready)
	host := httptest.NewServer(s.http.Handler)
	t.Cleanup(host.Close)
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(host.URL + "/ping")
	if err != nil {
		t.Fatal("public ping request failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 3))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "ok" || response.StatusCode != http.StatusOK {
		t.Fatal("public ping did not close with its finite two-byte body")
	}
	t.Log("LIFECYCLE-HTTP-CLOSURE-PASS bytes=2")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := browserCommand(ctx, "bun", "-e", `
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
const browser = await chromium.launch({ headless: true });
let unfinished = 0;
try {
  for (const intercepted of [false, true]) {
    const context = await browser.newContext();
    try {
      const page = await context.newPage();
      let terminal = 0;
      for (const event of ['requestfinished', 'requestfailed']) page.on(event, (request) => {
        if (new URL(request.url()).pathname === '/ping') terminal++;
      });
      await page.addInitScript(() => {
        Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
        const original = window.fetch.bind(window);
        window.fetch = async (...args) => {
          const response = await original(...args);
          if (new URL(response.url).pathname === '/ping') window.__boxsetLifecycleResponse = response;
          return response;
        };
      });
      let release;
      const held = new Promise((resolve) => { release = resolve; });
      if (intercepted) await page.route('**/ping', async (route) => {
        await held;
        await route.continue();
      });
      const session = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/v1/session');
      await page.goto(process.env.S5_DEV_URL);
      release();
      assert.equal((await session).status(), 401);
      await page.waitForFunction(() => document.querySelector('[role="status"]')?.textContent.includes('Sign in to check this Boxset.'));
      let idle = true;
      try { await page.waitForLoadState('networkidle', { timeout: 1000 }); }
      catch { idle = false; }
      const bodyUsed = await page.evaluate(() => window.__boxsetLifecycleResponse.bodyUsed);
      if (!bodyUsed) {
        const bytes = await page.evaluate(async () => {
          const response = window.__boxsetLifecycleResponse;
          const body = await response.text();
          return body === 'ok' ? body.length : -1;
        });
        assert.equal(bytes, 2);
      }
      await page.waitForLoadState('networkidle', { timeout: 5000 });
      assert.equal(terminal, 1);
      console.log('LIFECYCLE-BODY-CONTROL ' + JSON.stringify({ intercepted, bodyUsed, initialIdle: idle, consumedIdle: true, terminal }));
      if (!idle || !bodyUsed) unfinished++;
    } finally { await context.close(); }
  }
} catch (error) {
  const kind = error?.name === 'TimeoutError' ? 'timeout' : error?.name === 'AssertionError' ? 'assertion' : 'unexpected';
  console.log('LIFECYCLE-BODY-FAIL kind=' + kind);
  process.exitCode = 1;
} finally { await browser.close(); }
if (unfinished !== 0) {
  console.log('LIFECYCLE-BODY-FAIL external_disposal=' + unfinished);
  process.exitCode = 1;
}
`)
	cmd.Dir = filepath.Join("..", "..", "web")
	cmd.Env = browserEnv("S5_DEV_URL=" + host.URL + "/")
	output := &browserOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
	t.Log(output.String())
	if err != nil || output.overflow {
		t.Fatalf("lifecycle body completion: %v overflow=%t", err, output.overflow)
	}
}

func TestLogoutAcrossStarting(t *testing.T) {
	for _, base := range []string{"", "/media"} {
		t.Run("base"+base, func(t *testing.T) {
			in, _ := signedIn(t, base)
			backend := *in.s.backend.Load()
			backend.SchemaVersion, backend.RestoreEpoch, backend.Version = 4, 1, "test"
			original, err := New(base)
			if err != nil {
				t.Fatal(err)
			}
			original.Attach(backend)
			original.SetState(Ready)
			restarted, err := New(base)
			if err != nil {
				t.Fatal(err)
			}
			var active atomic.Pointer[Server]
			active.Store(original)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				active.Load().http.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(host.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := browserCommand(ctx, "bun", "-e", `
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { expect } from '@playwright/test';
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage();
  await page.addInitScript(() => Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true }));
  const origin = process.env.S5_DEV_URL;
  await page.goto(new URL('sign-in', origin).href);
  await page.getByLabel('Username').fill('owner');
  await page.getByLabel('Password', { exact: true }).fill('correct horse battery staple');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
  console.log('LOGOUT-RESTART-STARTING');
  await expect.poll(async () => (await page.request.get(new URL('ping', origin).href)).headers()['x-boxset-state']).toBe('starting');
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByRole('status')).toHaveText('Sign-out could not be confirmed. Retry to end this session.');
  await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
  assert.equal(new URL(page.url()).pathname, new URL(origin).pathname);
  await expect(page.getByRole('link', { name: 'Sign in', exact: true })).toHaveAttribute('href', new URL('sign-in', origin).pathname);
  console.log('LOGOUT-RESTART-READY');
  await expect.poll(async () => (await page.request.get(new URL('ping', origin).href)).headers()['x-boxset-state']).toBe('ready');
  assert.equal((await page.request.get(new URL('api/v1/session', origin).href)).status(), 200);
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  assert.equal((await page.request.get(new URL('api/v1/system/status', origin).href)).status(), 401);
  console.log('LOGOUT-RESTART-PASS persisted=true starting=unconfirmed ready_retry=revoked private_paint=false');
} catch (error) {
  console.log('LOGOUT-RESTART-FAIL kind=' + (error?.name === 'TimeoutError' ? 'timeout' : 'assertion'));
  process.exitCode = 1;
} finally { await browser.close(); }
`)
			cmd.Dir = filepath.Join("..", "..", "web")
			cmd.Env = browserEnv("S5_DEV_URL=" + host.URL + base + "/")
			output := &browserOutput{}
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			done := make(chan error, 1)
			go func() {
				defer func() { _ = reader.Close() }()
				scanner := bufio.NewScanner(reader)
				scanner.Buffer(make([]byte, 128), 128)
				for scanner.Scan() {
					switch scanner.Text() {
					case "LOGOUT-RESTART-STARTING":
						active.Store(restarted)
					case "LOGOUT-RESTART-READY":
						backend.Sessions = nil
						restarted.Attach(backend)
						restarted.SetState(Ready)
					}
				}
				done <- scanner.Err()
			}()
			cmd.Stdout, cmd.Stderr = io.MultiWriter(output, writer), output
			err = cmd.Run()
			_ = writer.Close()
			scanErr := <-done
			t.Log(output.String())
			if err != nil || scanErr != nil || output.overflow {
				t.Fatalf("restart logout regression: process=%v protocol=%v overflow=%t", err, scanErr, output.overflow)
			}
		})
	}
}

func TestBrowserSession(t *testing.T) {
	sessionBrowser(t, "states", sessionStateEnvironment(t))
}

func TestBrowserLocalAccess(t *testing.T) {
	sessionBrowser(t, "local-access", sessionLocalAccessEnvironment(t))
}

func TestSessionFixtureContract(t *testing.T) {
	bases, controls := 0, 0
	const expected = `{"version":1,"transitionHeader":"X-Boxset-Test-Role-Transition","beforeHeader":"X-Boxset-Test-Revision-Before","afterHeader":"X-Boxset-Test-Revision-After","operations":["restrict","restore"]}`
	env := sessionStateEnvironment(t)
	if err := validateSessionFixtureEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if sessionFixtureEncoding() != expected {
		t.Fatal("typed descriptor serialization differs")
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, fixtureEnvironment+"=") && entry != fixtureEnvironment+"="+expected {
			t.Fatal("descriptor consumer mismatch")
		}
	}
	invalid := []string{"", `{}`, `null`, expected + " " + expected, strings.Repeat(" ", 513) + expected, strings.Replace(expected, `"version":1`, `"Version":1`, 1), strings.Replace(expected, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(expected, `"version":1`, `"version":1,"vers\u0069on":1`, 1), strings.Replace(expected, `"version":1`, `"version":2`, 1), strings.Replace(expected, `"version":1`, `"version":"1"`, 1), strings.Replace(expected, `"version":1`, `"version":1.0`, 1), strings.Replace(expected, `"version":1,`, ``, 1), strings.Replace(expected, `"version":1`, `"extra":0,"version":1`, 1), strings.Replace(expected, `["restrict","restore"]`, `["restore","restrict"]`, 1), strings.Replace(expected, `"beforeHeader":"X-Boxset-Test-Revision-Before"`, `"beforeHeader":null`, 1)}
	for i, raw := range invalid {
		t.Run(fmt.Sprintf("descriptor-refusal-%d", i), func(t *testing.T) {
			if validateSessionFixtureEnvironment([]string{fixtureEnvironment + "=" + raw}) == nil {
				t.Fatal("descriptor accepted")
			}
		})
	}
	if validateSessionFixtureEnvironment(nil) == nil || validateSessionFixtureEnvironment([]string{fixtureEnvironment + "=" + expected, fixtureEnvironment + "=" + expected}) == nil {
		t.Fatal("missing/duplicate environment accepted")
	}
	t.Run("report-retention-refusals", func(t *testing.T) {
		start := time.Now()
		makeReport := func() map[string]any {
			var specs []any
			for _, name := range []string{"root", "media"} {
				var out []any
				for _, phase := range []string{"initial", "restrict", "restore", "revoke"} {
					status := 200
					if phase == "revoke" {
						status = 401
					}
					fact, _ := json.Marshal(map[string]any{"phase": phase, "fetched": true, "status": status, "noStore": true, "restricted": phase == "restrict"})
					out = append(out, map[string]any{"buffer": base64.StdEncoding.EncodeToString([]byte("SESSION-FRESHNESS " + string(fact) + "\n"))})
				}
				specs = append(specs, map[string]any{"title": name + ": real sign-in, current session, revocation and sign-out", "file": "session.spec.ts", "line": 1, "tests": []any{map[string]any{"expectedStatus": "passed", "results": []any{map[string]any{"status": "passed", "stdout": out}}}}})
			}
			return map[string]any{"stats": map[string]any{"startTime": start.Format(time.RFC3339Nano), "expected": 2, "unexpected": 0, "skipped": 0, "flaky": 0}, "suites": []any{map[string]any{"specs": specs}}}
		}
		getSpecs := func(report map[string]any) []any {
			return report["suites"].([]any)[0].(map[string]any)["specs"].([]any)
		}
		getResult := func(item any) map[string]any {
			return item.(map[string]any)["tests"].([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)
		}
		encode := func(report map[string]any) []byte {
			t.Helper()
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal("report fixture encoding")
			}
			return raw
		}
		if cases, err := sessionFixtureReport(encode(makeReport()), start, signInCohort); err != nil || len(cases) != 2 || len(cases[0].Observations) != 4 {
			t.Fatal("positive current report rejected")
		}
		failed := makeReport()
		failed["stats"].(map[string]any)["expected"] = 0
		failed["stats"].(map[string]any)["unexpected"] = 2
		for _, item := range getSpecs(failed) {
			result := getResult(item)
			result["status"] = "failed"
			result["errors"] = []any{map[string]any{"message": "FRESHNESS_SESSION_REQUEST", "location": map[string]any{"file": "session.spec.ts", "line": 123}}}
			result["stdout"] = result["stdout"].([]any)[:1]
		}
		if cases, err := sessionFixtureReport(encode(failed), start, signInCohort); err != nil || len(cases[0].Failures) != 1 || cases[0].Failures[0].ID != "FRESHNESS_SESSION_REQUEST" || cases[0].Failures[0].Line != 123 {
			t.Fatal("nonzero failure attribution lost")
		}
		for _, label := range []string{"stale", "future", "wrong-name", "duplicate-case", "missing-case", "wrong-file", "missing-observation", "duplicate-observation", "bad-buffer", "skipped", "flaky", "retry", "duplicate-result", "contradictory-status"} {
			t.Run(label, func(t *testing.T) {
				r := makeReport()
				specs := getSpecs(r)
				result := getResult(specs[0])
				stats := r["stats"].(map[string]any)
				switch label {
				case "stale":
					stats["startTime"] = start.Add(-time.Minute).Format(time.RFC3339Nano)
				case "future":
					stats["startTime"] = start.Add(time.Minute).Format(time.RFC3339Nano)
				case "wrong-name":
					specs[0].(map[string]any)["title"] = "wrong identity"
				case "duplicate-case":
					specs[1].(map[string]any)["title"] = specs[0].(map[string]any)["title"]
				case "missing-case":
					r["suites"].([]any)[0].(map[string]any)["specs"] = specs[:1]
				case "wrong-file":
					specs[0].(map[string]any)["file"] = "other.spec.ts"
				case "missing-observation":
					result["stdout"] = []any{}
				case "duplicate-observation":
					out := result["stdout"].([]any)
					out[1] = out[0]
				case "bad-buffer":
					result["stdout"] = []any{map[string]any{"buffer": "!invalid"}}
				case "skipped":
					stats["skipped"] = 1
				case "flaky":
					stats["flaky"] = 1
				case "retry":
					result["retry"] = 1
				case "duplicate-result":
					specs[0].(map[string]any)["tests"].([]any)[0].(map[string]any)["results"] = []any{result, result}
				case "contradictory-status":
					result["status"] = "failed"
				}
				if _, err := sessionFixtureReport(encode(r), start, signInCohort); err == nil {
					t.Fatal("invalid report accepted")
				}
			})
		}
		for _, raw := range [][]byte{nil, []byte("not json"), bytes.Repeat([]byte("x"), (8<<20)+1)} {
			if _, err := sessionFixtureReport(raw, start, signInCohort); err == nil {
				t.Fatal("missing/malformed/overflow report accepted")
			}
		}
	})
	for _, base := range []string{"", "/media"} {
		t.Run("transition"+base, func(t *testing.T) {
			in, cookie := signedIn(t, base)
			var clean bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clean = true
				for name := range r.Header {
					if strings.EqualFold(name, fixtureTransitionHeader) {
						clean = false
					}
				}
				in.h.ServeHTTP(w, r)
			})
			fixture := newSessionRoleFixture(next, in.db, base, 1)
			call := func(operation string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", base+"/api/v1/session", nil)
				r.AddCookie(cookie)
				if operation != "" {
					r.Header[strings.ToLower(fixtureTransitionHeader)] = []string{operation}
				}
				w := httptest.NewRecorder()
				fixture.ServeHTTP(w, r)
				return w
			}
			ordinary := call("")
			if ordinary.Code != 200 || ordinary.Header().Get(fixtureBeforeHeader) != "" || fixture.phase != 0 {
				t.Fatal("no-header delegation changed")
			}
			for i, operation := range []string{"restrict", "restore"} {
				w := call(operation)
				var body struct {
					User struct {
						ID       int64
						Role     string
						Revision int64
					}
					Rights       []string
					Credential   string
					RestoreEpoch int64
				}
				if json.Unmarshal(w.Body.Bytes(), &body) != nil {
					t.Fatal("session body malformed")
				}
				role, rights := "viewer", []string{"status:read"}
				if i == 1 {
					role, rights = "admin", []string{"status:read", "users:read", "users:write"}
				}
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(fixtureBeforeHeader) != strconv.Itoa(i+1) || w.Header().Get(fixtureAfterHeader) != strconv.Itoa(i+2) || body.User.ID != 1 || body.User.Role != role || body.User.Revision != int64(i+2) || !reflect.DeepEqual(body.Rights, rights) || body.Credential != "cookie" || body.RestoreEpoch != 1 || !clean {
					t.Fatal("committed genuine session contract mismatch")
				}
				if repeated := call(operation); repeated.Code != 409 {
					t.Fatal("repeated operation accepted")
				}
			}
			if w := call(""); w.Code != 200 || w.Header().Get(fixtureAfterHeader) != "" {
				t.Fatal("restored ordinary delegation failed")
			}
			bases++
		})
	}
	for _, label := range []string{"empty", "padded", "combined", "oversized", "unknown", "duplicate", "case-alias", "nil-alias", "method", "base", "path", "query", "empty-query", "escaped", "restore-first", "missing-user", "wrong-role", "overflow", "zero-rows", "readback-mismatch", "state-and-failure", "callback-failure", "commit-failure", "unknown-commit", "delegated-denial", "delegated-failure", "lost-delivery", "cancelled-admission"} {
		t.Run(label, func(t *testing.T) {
			in, cookie := signedIn(t, "/media")
			fixture := newSessionRoleFixture(in.h, in.db, "/media", 1)
			r := httptest.NewRequest("GET", "/media/api/v1/session", nil)
			r.AddCookie(cookie)
			r.Header.Set(fixtureTransitionHeader, "restrict")
			status, code := 400, "fixture_transition_invalid"
			writeSQL := func(statement string) {
				t.Helper()
				if err := in.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), statement); return err }); err != nil {
					t.Fatal("fixture preparation failed")
				}
			}
			switch label {
			case "empty":
				r.Header.Set(fixtureTransitionHeader, "")
			case "padded":
				r.Header.Set(fixtureTransitionHeader, " restrict")
			case "combined":
				r.Header.Set(fixtureTransitionHeader, "restrict,restore")
			case "oversized":
				r.Header.Set(fixtureTransitionHeader, "restrictx")
			case "unknown":
				r.Header.Set(fixtureTransitionHeader, "invalid")
			case "duplicate":
				r.Header.Add(fixtureTransitionHeader, "restrict")
			case "case-alias":
				r.Header[strings.ToLower(fixtureTransitionHeader)] = []string{"restrict"}
			case "nil-alias":
				r.Header[strings.ToLower(fixtureTransitionHeader)] = nil
			case "method":
				r.Method = "POST"
			case "base":
				r.URL.Path = "/api/v1/session"
			case "path":
				r.URL.Path = "/media/api/v1/unexpected/api/v1/session"
			case "query":
				r.URL.RawQuery = "ignored=1"
			case "empty-query":
				r.URL.ForceQuery = true
			case "escaped":
				r.URL.RawPath = "/media/api/v1/%73ession"
			case "restore-first":
				r.Header.Set(fixtureTransitionHeader, "restore")
				status, code = 409, "fixture_transition_state"
			case "missing-user":
				fixture.user = 999
				status, code = 409, "fixture_transition_state"
			case "wrong-role":
				writeSQL("UPDATE user SET role='member' WHERE id=1")
				status, code = 409, "fixture_transition_state"
			case "overflow":
				writeSQL("UPDATE user SET revision=9007199254740991 WHERE id=1")
				status, code = 409, "fixture_transition_state"
			case "zero-rows":
				writeSQL("CREATE TRIGGER fixture_ignore BEFORE UPDATE ON user BEGIN SELECT RAISE(IGNORE); END")
				status, code = 409, "fixture_transition_state"
			case "readback-mismatch":
				writeSQL("CREATE TRIGGER fixture_revision AFTER UPDATE ON user WHEN NEW.revision=2 BEGIN UPDATE user SET revision=NEW.revision+1 WHERE id=NEW.id; END")
				status, code = 409, "fixture_transition_state"
			case "state-and-failure":
				writeSQL("UPDATE user SET role='member' WHERE id=1")
				status, code = 500, "fixture_transition_failed"
				fixture.write = func(ctx context.Context, fn func(*sql.Tx) error) error {
					return in.db.Write(ctx, func(tx *sql.Tx) error {
						return errors.Join(fn(tx), errors.New("synthetic-private-rollback-failure"))
					})
				}
			case "callback-failure", "commit-failure", "unknown-commit":
				status, code = 500, "fixture_transition_failed"
				fixture.write = func(ctx context.Context, fn func(*sql.Tx) error) error {
					err := in.db.Write(ctx, func(tx *sql.Tx) error {
						if err := fn(tx); err != nil {
							return err
						}
						if label == "callback-failure" {
							return errors.New("synthetic-private-failure")
						}
						if label == "commit-failure" {
							return tx.Rollback()
						}
						return nil
					})
					if label == "unknown-commit" && err == nil {
						return errors.New("synthetic-private-unknown")
					}
					return err
				}
			case "delegated-denial":
				r.Header.Del("Cookie")
				status, code = 401, ""
			case "delegated-failure":
				fixture.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
				status, code = 503, ""
			case "lost-delivery":
				status, code = 0, ""
			case "cancelled-admission":
				fixture.admission <- struct{}{}
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
				status, code = 500, "fixture_transition_failed"
			}
			var beforeRole string
			var beforeRevision int64
			writes, delegates := 0, 0
			write := fixture.write
			fixture.write = func(ctx context.Context, fn func(*sql.Tx) error) error { writes++; return write(ctx, fn) }
			next := fixture.next
			fixture.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delegates++; next.ServeHTTP(w, r) })
			read := func(role *string, revision *int64) {
				t.Helper()
				if err := in.db.Read(t.Context(), func(tx *sql.Tx) error {
					return tx.QueryRowContext(t.Context(), "SELECT role,revision FROM user WHERE id=1").Scan(role, revision)
				}); err != nil {
					t.Fatal("fixture state unavailable")
				}
			}
			read(&beforeRole, &beforeRevision)
			w := httptest.NewRecorder()
			if label == "lost-delivery" {
				lost := &fixtureLostDelivery{header: make(http.Header)}
				func() {
					defer func() {
						problem, ok := recover().(error)
						if !ok || !errors.Is(problem, io.ErrClosedPipe) {
							t.Fatal("unexpected production delivery failure")
						}
					}()
					fixture.ServeHTTP(lost, r)
				}()
				if lost.writes == 0 {
					t.Fatal("lost-delivery control never attempted the genuine body")
				}
			} else {
				fixture.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("fixture status=%d want=%d", w.Code, status)
				}
			}
			if code != "" && (w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" || w.Body.String() != fmt.Sprintf(`{"code":%q}`, code)) {
				t.Fatal("fixture refusal contract or privacy mismatch")
			}
			committed := strings.HasPrefix(label, "delegated-") || label == "lost-delivery" || label == "unknown-commit"
			var role string
			var revision int64
			read(&role, &revision)
			if committed {
				if role != "viewer" || revision != beforeRevision+1 {
					t.Fatal("committed effect was lost")
				}
				if label != "unknown-commit" && fixture.phase != 1 {
					t.Fatal("committed phase not retained")
				}
			} else if role != beforeRole || revision != beforeRevision || fixture.phase != 0 {
				t.Fatal("refusal/rollback changed state")
			}
			if code != "" && (w.Header().Get(fixtureBeforeHeader) != "" || w.Header().Get(fixtureAfterHeader) != "") {
				t.Fatal("failure emitted success acknowledgement")
			}
			if (status == 400 || label == "restore-first" || label == "cancelled-admission") && writes != 0 {
				t.Fatal("invalid request entered transition transaction")
			}
			if !committed && delegates != 0 {
				t.Fatal("refused request delegated")
			}
			if committed && label != "unknown-commit" && delegates != 1 {
				t.Fatal("committed request did not delegate once")
			}
			if label == "callback-failure" || label == "commit-failure" {
				if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
					var healthyRole string
					var healthyRevision int64
					if err := tx.QueryRowContext(t.Context(), "SELECT role,revision FROM user WHERE id=1").Scan(&healthyRole, &healthyRevision); err != nil {
						return err
					}
					if healthyRole != beforeRole || healthyRevision != beforeRevision {
						return errors.New("rollback state mismatch")
					}
					return nil
				}); err != nil {
					t.Fatal("writer not healthy after known rollback")
				}
			}
			if label == "unknown-commit" {
				if fixture.phase != 0 {
					t.Fatal("unknown commit advanced phase")
				}
				fixture.write = in.db.Write
				again := httptest.NewRecorder()
				fixture.ServeHTTP(again, r)
				if again.Code != 409 {
					t.Fatal("unknown transition replayed")
				}
			}
			controls++
		})
	}
	t.Run("revision-upper-bound", func(t *testing.T) {
		in, cookie := signedIn(t, "")
		if err := in.db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "UPDATE user SET revision=? WHERE id=1", fixtureRevisionMax-1)
			return err
		}); err != nil {
			t.Fatal("revision boundary preparation failed")
		}
		fixture := newSessionRoleFixture(in.h, in.db, "", 1)
		r := httptest.NewRequest("GET", "/api/v1/session", nil)
		r.AddCookie(cookie)
		r.Header.Set(fixtureTransitionHeader, "restrict")
		w := httptest.NewRecorder()
		fixture.ServeHTTP(w, r)
		var body struct {
			User struct {
				Revision int64
			}
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != 200 || body.User.Revision != fixtureRevisionMax || w.Header().Get(fixtureBeforeHeader) != "9007199254740990" || w.Header().Get(fixtureAfterHeader) != "9007199254740991" {
			t.Fatal("largest safe revision transition rejected")
		}
		r.Header.Set(fixtureTransitionHeader, "restore")
		refused := httptest.NewRecorder()
		fixture.ServeHTTP(refused, r)
		if refused.Code != 409 || fixture.phase != 1 || refused.Header().Get(fixtureAfterHeader) != "" {
			t.Fatal("revision overflow did not fail closed")
		}
	})
	t.Logf("SESSION-FIXTURE-CONTRACT descriptor=closed bases=%d controls=%d", bases, controls)
}

// Exercise the recovery fixtures without the capture and layout matrix.
func TestSessionLogoutFixtures(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "session.spec.ts", "--grep", "real sign-in, current session, revocation and sign-out")
	cmd.Dir = filepath.Join("..", "..", "web")
	cmd.Env = sessionBrowserEnvironment(t, "states", sessionStateEnvironment(t))
	runSessionBrowser(t, cmd, 2, &signInCohort)
	t.Log("SESSION-LOGOUT-FIXTURES passed=2 bases=root,media")
}

func sessionStateEnvironment(t *testing.T) []string {
	t.Helper()
	env := browserEnv(fixtureEnvironment + "=" + sessionFixtureEncoding())
	for name, base := range map[string]string{"S5_DEV_URL": "", "S5_PREVIEW_URL": "/media"} {
		dir := t.TempDir()
		db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		setup := auth.NewSetup(db, auth.NewHasher(), dir)
		if _, err := setup.Arm(t.Context(), "synthetic-browser-session-setup"); err != nil {
			t.Fatal(err)
		}
		user, err := setup.Create(t.Context(), auth.Verifier("synthetic-browser-session-setup"), "owner", password)
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(base)
		if err != nil {
			t.Fatal(err)
		}
		s.Attach(Backend{Setup: setup, SchemaVersion: 4, RestoreEpoch: 1, Version: "test"})
		s.SetState(Ready)
		s.http.Handler = newSessionRoleFixture(s.http.Handler, db, base, user.ID)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		})
		env = append(env, name+"=http://"+ln.Addr().String()+base+"/")
	}
	return env
}

// sessionLocalAccessEnvironment boots two installations that configure the
// direct-peer grant, so the browser suite proves the warning, the bypass-only
// access identity and cookie logout through the real server.
func sessionLocalAccessEnvironment(t *testing.T) []string {
	t.Helper()
	env := browserEnv()
	for name, base := range map[string]string{"S5_DEV_URL": "", "S5_PREVIEW_URL": "/media"} {
		dir := t.TempDir()
		db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		setup := auth.NewSetup(db, auth.NewHasher(), dir)
		if _, err := setup.Arm(t.Context(), "synthetic-browser-local-access-setup"); err != nil {
			t.Fatal(err)
		}
		if _, err := setup.Create(t.Context(), auth.Verifier("synthetic-browser-local-access-setup"), "owner", password); err != nil {
			t.Fatal(err)
		}
		s, err := New(base)
		if err != nil {
			t.Fatal(err)
		}
		s.Attach(Backend{Setup: setup, SchemaVersion: 4, RestoreEpoch: 1, Version: "test", LocalBypass: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
		s.SetState(Ready)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		})
		env = append(env, name+"=http://"+ln.Addr().String()+base+"/")
	}
	return env
}

func TestTwoInstallations(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "boxset")
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	build := browserCommand(ctx, "go", "build", "-o", binary, "./cmd/boxset")
	build.Dir = filepath.Join("..", "..")
	build.Env = browserEnv()
	output := &browserOutput{}
	build.Stdout, build.Stderr = output, output
	if err := build.Run(); err != nil {
		t.Fatalf("session CLI build: %v\n%s", err, output.String())
	}
	if output.overflow {
		t.Fatal("CLI build output overflow")
	}
	targets := map[string]*url.URL{}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, base := range []string{"/a", "/b"} {
		data := filepath.Join(dir, base[1:])
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
		_ = ln.Close()
		cmd := browserCommand(ctx, binary, "serve")
		cmd.Env = browserEnv("BOXSET_DATA_DIR="+data, "BOXSET_PORT="+port, "BOXSET_URL_BASE="+base, "BOXSET_TRUSTED_PROXIES=127.0.0.0/8,::1/128", "BOXSET_LOCAL_BYPASS=")
		logs := &browserOutput{}
		cmd.Stdout, cmd.Stderr = logs, logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			select {
			case <-done:
			default:
				_ = cmd.Cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("owned session CLI did not terminate")
				}
			}
			if logs.overflow {
				t.Error("session CLI output overflow")
			}
		})
		target, _ := url.Parse("http://127.0.0.1:" + port)
		targets[base] = target
		deadline := time.Now().Add(30 * time.Second)
		for {
			select {
			case err := <-done:
				done <- err
				t.Fatalf("session CLI exited before ready: %v\n%s", err, logs.String())
			default:
			}
			resp, err := client.Get(target.String() + base + "/ping")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 && resp.Header.Get("X-Boxset-State") == "ready" {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("session CLI readiness timeout\n%s", logs.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		token, err := os.ReadFile(filepath.Join(data, "setup-token"))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", target.String()+base+"/api/v1/setup", bytes.NewBufferString(credentials("owner")))
		if err != nil {
			t.Fatal("setup request invalid")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+string(token))
		clear(token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("session CLI setup transport failed")
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("session CLI setup status=%d", resp.StatusCode)
		}
	}
	proxy := func(w http.ResponseWriter, r *http.Request) {
		base := "/" + strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		target := targets[base]
		if target == nil {
			http.NotFound(w, r)
			return
		}
		p := &httputil.ReverseProxy{}
		p.Rewrite = func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			if request.In.TLS != nil {
				request.Out.Header.Set("X-Forwarded-Proto", "https")
			} else {
				request.Out.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "proxy unavailable", http.StatusBadGateway)
		}
		p.ServeHTTP(w, r)
	}
	plain := httptest.NewServer(http.HandlerFunc(proxy))
	t.Cleanup(plain.Close)
	tls := httptest.NewTLSServer(http.HandlerFunc(proxy))
	t.Cleanup(tls.Close)
	sessionBrowser(t, "two-installations", browserEnv("BOXSET_SESSION_HTTP="+plain.URL, "BOXSET_SESSION_TLS="+tls.URL, "S5_DEV_URL="+plain.URL+"/a/", "S5_PREVIEW_URL="+plain.URL+"/b/"))
}

func TestBrowserSetup(t *testing.T) {
	if os.Getenv("BOXSET_CAPTURE_DIR") == "" || os.Getenv("BOXSET_CAPTURE_CHALLENGE") == "" {
		t.Fatal("run with scripts/dev-run.sh --capture DIR")
	}
	env := append(os.Environ(), "BOXSET_SHELL_HOST=production")
	style, sourceHash := installedAnnouncerStyle(t)
	if style != fixedAnnouncerStyle || inlineHash(style) != fixedAnnouncerHash {
		t.Fatal("installed announcer style differs from fixed production permission")
	}
	env = append(env, "BOXSET_CSP_SOURCE_STYLE="+style, "BOXSET_CSP_SOURCE_SHA256="+sourceHash)
	for name, base := range map[string]string{"S5_DEV_URL": "", "S5_PREVIEW_URL": "/media"} {
		dir := t.TempDir()
		db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		setup := auth.NewSetup(db, auth.NewHasher(), dir)
		if _, err := setup.Arm(t.Context(), "synthetic-browser-setup-credential"); err != nil {
			t.Fatal(err)
		}
		s, err := New(base)
		if err != nil {
			t.Fatal(err)
		}
		s.Attach(Backend{Setup: setup})
		s.SetState(Ready)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		})
		env = append(env, name+"=http://"+ln.Addr().String()+base+"/")
	}
	web := filepath.Join("..", "..", "web")
	report := filepath.Join(web, "test-results", "report.json")
	if err := os.RemoveAll(report); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "shell.spec.ts", "setup.spec.ts")
	cmd.Dir, cmd.Env = web, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("playwright: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Stats struct{ Expected, Unexpected, Skipped, Flaky int }
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Stats.Expected < 7 || got.Stats.Unexpected != 0 || got.Stats.Skipped != 0 || got.Stats.Flaky != 0 {
		t.Fatalf("playwright stats %+v", got.Stats)
	}
	t.Logf("BROWSER-SETUP passed=%d bases=root,/media csp=production", got.Stats.Expected)
}

// TestBrowserShell runs the Playwright shell spec (web/e2e/shell.spec.ts) in the real
// chromium-headless-shell against the production shell served by this package at the root base
// (the spec's "dev" origin) and under /media (its "preview" origin), each with the production
// Content-Security-Policy: the spec asserts zero securitypolicyviolation events, axe-clean public
// states, the offline Retry flow and writes the four capture files under BOXSET_CAPTURE_DIR. Run
// after `bun --cwd=web run build` in the same runner child:
//
//	scripts/dev-run.sh --capture "$CAP" -- sh -euc 'bun --cwd=web run build; exec env CGO_ENABLED=0 go test -tags=e2e ./internal/server -run TestBrowserShell'
func TestBrowserShell(t *testing.T) {
	if os.Getenv("BOXSET_CAPTURE_DIR") == "" || os.Getenv("BOXSET_CAPTURE_CHALLENGE") == "" {
		t.Fatal("BOXSET_CAPTURE_DIR and BOXSET_CAPTURE_CHALLENGE come from `scripts/dev-run.sh --capture DIR`")
	}
	env := append(os.Environ(), "BOXSET_SHELL_HOST=production")
	style, sourceHash := installedAnnouncerStyle(t)
	if style != fixedAnnouncerStyle || inlineHash(style) != fixedAnnouncerHash {
		t.Fatal("installed announcer style differs from fixed production permission")
	}
	env = append(env, "BOXSET_CSP_SOURCE_STYLE="+style, "BOXSET_CSP_SOURCE_SHA256="+sourceHash)
	attribution := os.Getenv("BOXSET_CSP_ATTRIBUTION") == "1"
	// Historical opt-in acquisition only; the fixed policy cannot establish pre-adoption evidence.
	for name, base := range map[string]string{"S5_DEV_URL": "", "S5_PREVIEW_URL": "/media"} {
		s, err := New(base)
		if err != nil {
			t.Fatalf("New(%q): %v (run `bun --cwd=web run build` first)", base, err)
		}
		s.SetState(Ready)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		})
		env = append(env, name+"=http://"+ln.Addr().String()+base+"/")
	}

	web := filepath.Join("..", "..", "web")
	report := filepath.Join(web, "test-results", "report.json")
	if err := os.RemoveAll(report); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "shell.spec.ts")
	cmd.Dir = web
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if attribution {
		records, err := cspAttributionRecords(out)
		for _, record := range records {
			t.Log(record)
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			t.Fatal("CSP-ATTRIBUTION unproven: no diagnostic records from playwright")
		}
	}
	if runErr != nil {
		if attribution {
			lines := strings.Split(string(out), "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "CSP-ATTRIBUTION ") {
					lines[i] = ""
				}
			}
			t.Fatalf("playwright diagnostic: %v (records relayed above; no acceptance)\n%s", runErr, strings.Join(lines, "\n"))
		}
		t.Fatalf("playwright: %v\n%s", runErr, out)
	}

	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Stats struct{ Expected, Unexpected, Skipped, Flaky int }
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Stats.Expected < 5 || got.Stats.Unexpected != 0 || got.Stats.Skipped != 0 || got.Stats.Flaky != 0 {
		t.Fatalf("playwright stats %+v: want at least 5 passed and none failed, skipped or flaky", got.Stats)
	}
	if !attribution {
		t.Logf("BROWSER-SHELL passed=%d bases=\"\",/media csp=production", got.Stats.Expected)
	}
}

// This control uses fresh production documents without capture, axe or label instrumentation.
func TestBrowserShellStylePolicy(t *testing.T) {
	style, _ := installedAnnouncerStyle(t)
	if style != fixedAnnouncerStyle || inlineHash(style) != fixedAnnouncerHash {
		t.Fatal("installed announcer style differs from fixed production permission")
	}
	env := append(os.Environ(), "BOXSET_CSP_SOURCE_STYLE="+style)
	for name, base := range map[string]string{"S5_DEV_URL": "", "S5_PREVIEW_URL": "/media"} {
		s, err := New(base)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		})
		env = append(env, name+"=http://"+ln.Addr().String()+base+"/")
	}
	cmd := exec.Command("bun", "--eval", `
import assert from 'node:assert/strict';
import { chromium, expect } from '@playwright/test';
const browser = await chromium.launch({ headless: true });
try {
	for (const origin of [process.env.S5_DEV_URL, process.env.S5_PREVIEW_URL]) {
		const context = await browser.newContext();
		try {
			const page = await context.newPage();
			await page.addInitScript(() => {
				window.violations = [];
				document.addEventListener('securitypolicyviolation', (event) => {
					window.violations.push([event.effectiveDirective, event.blockedURI, event.disposition]);
				});
			});
			const response = await page.goto(origin);
			assert.equal(response.status(), 200);
			const header = response.headers()['content-security-policy'];
			assert.match(header, /; style-src 'self'; style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='; img-src/);
			assert.equal(response.headers()['content-security-policy-report-only'], undefined);
			const announcer = page.locator('#svelte-announcer');
			await expect(announcer).toHaveAttribute('aria-live', 'assertive');
			await expect(announcer).toHaveAttribute('aria-atomic', 'true');
			await expect(announcer).toHaveAttribute('style', process.env.BOXSET_CSP_SOURCE_STYLE);
			await page.evaluate((origin) => {
				window.clientDocument = true;
				const link = document.createElement('a');
				link.href = origin + '?announcer=1';
				link.textContent = 'Announce route';
				document.body.append(link);
			}, origin);
			await page.getByRole('link', { name: 'Announce route' }).click();
			await page.waitForURL(url => url.search === '?announcer=1');
			assert.equal(await page.evaluate(() => window.clientDocument), true, 'same-document client navigation');
			await page.getByRole('link', { name: 'Announce route' }).evaluate(el => el.remove());
			await expect(announcer).toHaveText('Boxset');
			assert.equal(await announcer.evaluate(el => getComputedStyle(el).position), 'absolute');
			assert.equal(await announcer.evaluate(el => getComputedStyle(el).width), '1px');
			assert.deepEqual(await page.evaluate(() => window.violations), []);
			const source = process.env.BOXSET_CSP_SOURCE_STYLE;
			await announcer.evaluate(el => el.removeAttribute('style'));
			assert.equal(await announcer.evaluate(el => getComputedStyle(el).position), 'static');
			await announcer.evaluate((el, style) => el.setAttribute('style', style.replace('1px', '2px')), source);
			await page.waitForFunction(() => window.violations.length === 1);
			assert.notEqual(await announcer.evaluate(el => getComputedStyle(el).position), 'absolute');
			assert.notEqual(await announcer.evaluate(el => getComputedStyle(el).width), '2px');
			await announcer.evaluate((el, style) => el.setAttribute('style', style), source);
			assert.equal(await announcer.evaluate(el => getComputedStyle(el).position), 'absolute');
			assert.equal(await announcer.evaluate(el => getComputedStyle(el).width), '1px');
			await page.evaluate(() => {
				const unrelated = document.createElement('div');
				unrelated.id = 'unrelated-style-control';
				document.body.append(unrelated);
				unrelated.setAttribute('style', 'position: fixed;');
			});
			await page.waitForFunction(() => window.violations.length === 2);
			assert.equal(await page.locator('#unrelated-style-control').evaluate(el => getComputedStyle(el).position), 'static');
			assert.deepEqual(await page.evaluate(() => window.violations), [
				['style-src-attr', 'inline', 'enforce'],
				['style-src-attr', 'inline', 'enforce']
			]);
			await page.locator('#unrelated-style-control').evaluate(el => el.remove());
			await expect(announcer).toHaveAttribute('style', source);
			await expect(announcer).toHaveText('Boxset');
			console.log('SHELL-STYLE-PASS base=' + new URL(origin).pathname + ' fixed=allowed changed=blocked unrelated=blocked announcer=accessible');
		} finally {
			await context.close();
		}
	}
} finally {
	await browser.close();
}
`)
	cmd.Dir = filepath.Join("..", "..", "web")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("production style controls: %v\n%s", err, out)
	}
	if strings.Count(string(out), "SHELL-STYLE-PASS base=") != 2 {
		t.Fatalf("want both production bases exercised:\n%s", out)
	}
	t.Log(string(out))
}

func cspAttributionRecords(out []byte) ([]string, error) {
	const prefix = "CSP-ATTRIBUTION "
	const limit = 16 * 1024
	var records []string
	total := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		record := strings.TrimSuffix(line, "\r")
		total += len(record) + 1
		if total > limit {
			return records, fmt.Errorf("CSP-ATTRIBUTION unproven: diagnostic record overflow (limit=%d)", limit)
		}
		if !json.Valid([]byte(strings.TrimPrefix(record, prefix))) {
			return records, fmt.Errorf("CSP-ATTRIBUTION unproven: malformed diagnostic record")
		}
		records = append(records, record)
	}
	return records, nil
}

func TestCSPAttributionRelay(t *testing.T) {
	for _, status := range []string{"passed", "failed"} {
		t.Run(status, func(t *testing.T) {
			out := []byte("worker output\nCSP-ATTRIBUTION {\"outcome\":\"unproven\"}\n" + status + "\n")
			records, err := cspAttributionRecords(out)
			if err != nil || len(records) != 1 || records[0] != `CSP-ATTRIBUTION {"outcome":"unproven"}` {
				t.Fatalf("records=%q err=%v", records, err)
			}
		})
	}
	t.Run("evidence overflow", func(t *testing.T) {
		out := []byte("CSP-ATTRIBUTION {}\nCSP-ATTRIBUTION " + strings.Repeat("x", 16*1024))
		records, err := cspAttributionRecords(out)
		if err == nil || !strings.Contains(err.Error(), "overflow") || len(records) != 1 {
			t.Fatalf("records=%d err=%v: want bounded prefix and explicit overflow", len(records), err)
		}
	})
	t.Run("reporter error text is not evidence", func(t *testing.T) {
		records, err := cspAttributionRecords([]byte("Error: CSP-ATTRIBUTION not a record\n"))
		if err != nil || len(records) != 0 {
			t.Fatalf("records=%q err=%v", records, err)
		}
	})
	t.Run("malformed record", func(t *testing.T) {
		if _, err := cspAttributionRecords([]byte("CSP-ATTRIBUTION not-json\n")); err == nil {
			t.Fatal("want malformed record refusal")
		}
	})
}

// recoveryServer is one `boxset serve` process a recovery case drives: the real
// binary, a real data directory and the production shell, so the browser sees
// the same bytes a real start would serve.
type recoveryServer struct {
	cmd    *exec.Cmd
	port   string
	base   string
	out    *browserOutput
	exited chan struct{}
}

// startRecoveryServe runs the built binary with an explicit environment: the
// production settings this test needs, and nothing else.
func startRecoveryServe(t *testing.T, bin, dir, base string, extra ...string) *recoveryServer {
	t.Helper()
	s := &recoveryServer{port: freePort(t), base: base, out: &browserOutput{}, exited: make(chan struct{})}
	s.cmd = exec.Command(bin, "serve")
	s.cmd.Env = append([]string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=" + s.port, "BOXSET_URL_BASE=" + base}, extra...)
	s.cmd.Stdout, s.cmd.Stderr = s.out, s.out
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.cmd.Wait(); close(s.exited) }()
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		select {
		case <-s.exited:
		case <-time.After(30 * time.Second):
			t.Error("the server did not exit within 30 s")
		}
	})
	return s
}

func (s *recoveryServer) origin() string { return "http://127.0.0.1:" + s.port + s.base }

// waitState waits for /ping to report state.
func (s *recoveryServer) waitState(t *testing.T, state string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.exited:
			t.Fatalf("the server exited before it was %s: %s", state, s.out.String())
		default:
		}
		resp, err := client.Get(s.origin() + "/ping")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Boxset-State") == state {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the server was not %s within 30 s: %s", state, s.out.String())
}

// terminate stops the server with SIGTERM and waits for it.
func (s *recoveryServer) terminate(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not stop within 30 s")
	}
}

// postJSON sends one JSON body and returns the response.
func postJSON(t *testing.T, method, target, body string, header ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// recoverySession creates the admin of dir over the real API and returns the
// session cookie it signs in with, so the browser presents a current credential.
func recoverySession(t *testing.T, srv *recoveryServer, token string) string {
	t.Helper()
	resp := postJSON(t, "POST", srv.origin()+"/api/v1/setup", credentials("owner"), "Authorization", "Bearer "+token)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup over the real binary = %d", resp.StatusCode)
	}
	resp = postJSON(t, "POST", srv.origin()+"/api/v1/session", credentials("owner"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("login over the real binary = %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want one", len(cookies))
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

const recoverySetupToken = "synthetic-recovery-setup-token"

// missingKeyRecovery leaves dir holding one encrypted record whose key file is
// gone, which a start reports as recovering/key_missing, and returns the origin a
// browser navigates to and the session cookie of the admin created before the
// loss.
func missingKeyRecovery(t *testing.T, bin, base string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	srv := startRecoveryServe(t, bin, dir, base, "BOXSET_SETUP_TOKEN="+recoverySetupToken)
	srv.waitState(t, "ready")
	session := recoverySession(t, srv, recoverySetupToken)
	srv.terminate(t)

	db, err := store.Open(t.Context(), install.Layout{Dir: dir}.DB(), store.Options{
		BeforeMigrate: func(context.Context, store.Pending) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := secret.New(db, dir)
	keyID, key, err := keys.MintKey()
	if err != nil {
		t.Fatal(err)
	}
	nonce, sealed, err := secret.Seal(key, 1, 1, "indexer", []byte("synthetic-recovery-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		q := storedb.New(tx)
		if err := q.InsertSecretVersion(t.Context(), storedb.InsertSecretVersionParams{
			ID: 1, Version: 1, Purpose: "indexer", KeyID: keyID, Nonce: nonce, Ciphertext: sealed, IsCurrent: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		return q.SetInstallationKey(t.Context(), keyID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keys.KeyPath(keyID)); err != nil {
		t.Fatal(err)
	}

	srv = startRecoveryServe(t, bin, dir, base)
	srv.waitState(t, "recovering")
	return srv.origin(), session
}

// interruptedRestore leaves dir holding a restore journal whose declared recovery
// copy is not there: the start reports recovering/restore_in_progress and serves
// no credential at all, not even the one created before the interruption.
func interruptedRestore(t *testing.T, bin, base string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	srv := startRecoveryServe(t, bin, dir, base, "BOXSET_SETUP_TOKEN="+recoverySetupToken)
	srv.waitState(t, "ready")
	session := recoverySession(t, srv, recoverySetupToken)
	srv.terminate(t)

	journal, err := json.Marshal(install.Recovery{
		Phase:        install.PhaseReconcile,
		RecoveryPath: filepath.Join(dir, "recovery-not-written.db"),
		Epoch:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(install.Layout{Dir: dir}.Journal(), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	srv = startRecoveryServe(t, bin, dir, base)
	srv.waitState(t, "recovering")
	return srv.origin(), session
}

// TestBrowserRecovery requires cold navigation at both supported bases in a real
// missing-key and a real interrupted-restore installation: the data-free shell and
// its hashed assets arrive under the production CSP, the public cue names the
// recovery state, protected data is denied, and the status snapshot is served only
// to a current credential. The browser, its capture root and the production CSP
// come from `scripts/dev-run.sh --capture DIR`; the binary is built here so the
// case drives the real command.
func TestBrowserRecovery(t *testing.T) {
	if os.Getenv("BOXSET_CAPTURE_DIR") == "" || os.Getenv("BOXSET_CAPTURE_CHALLENGE") == "" {
		t.Fatal("BOXSET_CAPTURE_DIR and BOXSET_CAPTURE_CHALLENGE come from `scripts/dev-run.sh --capture DIR`")
	}
	bin := filepath.Join(t.TempDir(), "boxset")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/boxset")
	build.Dir = filepath.Join("..", "..")
	build.Env = browserEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// Every recovery reason at both supported bases, each installation named by
	// the variable it feeds: two states under one name would let os.Getenv pick
	// only one of them, and which one would be map order.
	origins := map[string]string{}
	env := browserEnv()
	states := []string{"key_missing", "restore_in_progress"}
	for _, installation := range []struct{ name, base string }{{"S5_DEV", ""}, {"S5_PREVIEW", "/media"}} {
		for _, state := range states {
			prepare := missingKeyRecovery
			if state == "restore_in_progress" {
				prepare = interruptedRestore
			}
			origin, session := prepare(t, bin, installation.base)
			key := installation.name + "_" + strings.ToUpper(state)
			env = append(env, key+"_URL="+origin+"/", key+"_SESSION="+session)
			origins[installation.name+"/"+state] = origin
		}
	}
	// The names the browser reads are exactly these, once each: a later duplicate
	// would silently shadow the value before it.
	seen := map[string]bool{}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "S5_") {
			continue
		}
		if seen[name] {
			t.Fatalf("%s appears twice in the browser environment", name)
		}
		seen[name] = true
	}
	for _, installation := range []struct{ name, base string }{{"S5_DEV", ""}, {"S5_PREVIEW", "/media"}} {
		for _, state := range states {
			if origins[installation.name+"/"+state] == "" {
				t.Fatalf("%s at %s did not reach the browser", state, installation.base)
			}
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	sessions := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "session.spec.ts")
	sessions.Dir = filepath.Join("..", "..", "web")
	sessions.Env = sessionBrowserEnvironment(t, "recovery", env)
	runSessionBrowser(t, sessions, len(recoveryCohort.cases), &recoveryCohort)

	// The same run publishes the four canonical captures through the shell suite's
	// own validated protocol, in the recovery state the fixture names: the cue
	// assertion is a parameter, so every other shell run keeps its ready default.
	captures := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "shell.spec.ts", "--grep", "loads the public shell, offers Retry offline, passes axe and captures")
	captures.Dir = filepath.Join("..", "..", "web")
	captureEnv := browserEnv(
		"S5_DEV_URL="+origins["S5_DEV/key_missing"]+"/",
		"S5_PREVIEW_URL="+origins["S5_PREVIEW/key_missing"]+"/",
		"BOXSET_SHELL_EXPECT_STATE=recovering",
	)
	captures.Env = sessionBrowserEnvironment(t, "recovery", captureEnv)
	runSessionBrowser(t, captures, 4, nil)
	t.Log("BROWSER-RECOVERY states=key_missing,restore_in_progress bases=/,/media captures=4 cue=recovering csp=production")
}

// freePort reserves one loopback port number for a server a case starts.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// browserStreamSetupToken arms the installation the browser rows sign in to.
const browserStreamSetupToken = "synthetic-browser-stream-setup"

// The row's own reads and actions, answered outside every base:
//
//	GET  /__stream-probe          the event route's counters
//	POST /__stream-control?action=restart|expire|system|job
const browserStreamProbePath = "/__stream-probe"
const browserStreamControlPath = "/__stream-control"

// browserStreamResync is one resync frame the probe read off the wire.
type browserStreamResync struct {
	Watermark int64  `json:"watermark"`
	Reason    string `json:"reason"`
}

// browserStreamProbe is what one installation's event route served: the requests it
// admitted, whether they carried the session cookie, the cursor each request named,
// the frames the streams delivered (hello's own id and any resync), and how many
// requests ended.
type browserStreamProbe struct {
	mu       sync.Mutex
	requests int
	ended    int
	cookie   bool
	hello    bool
	cursors  []string
	lastID   string
	helloID  string
	resync   *browserStreamResync
	resyncID string
}

func (p *browserStreamProbe) begin(cookie bool, cursor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests++
	p.cookie = p.cookie || cookie
	if p.cursors == nil {
		p.cursors = []string{}
	}
	p.cursors = append(p.cursors, cursor)
}

func (p *browserStreamProbe) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended++
}

// observe reads the frames a stream has written so far in this response: the last
// applied id is what a reconnect must carry, and hello's watermark plus a resync are
// the delivered kinds the restart row asserts.
func (p *browserStreamProbe) observe(seen []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, block := range bytes.Split(seen, []byte("\n\n")) {
		var id, event, data string
		for _, line := range bytes.Split(block, []byte("\n")) {
			switch {
			case bytes.HasPrefix(line, []byte("id: ")):
				id = string(line[len("id: "):])
			case bytes.HasPrefix(line, []byte("event: ")):
				event = string(line[len("event: "):])
			case bytes.HasPrefix(line, []byte("data: ")):
				data = string(line[len("data: "):])
			}
		}
		if event == "" || data == "" {
			continue
		}
		if id != "" {
			p.lastID = id
		}
		switch event {
		case "hello":
			// The hello's own id names the sequence it replayed, so the restart row
			// observes the replay rather than reading the resync's watermark twice.
			p.hello = true
			p.helloID = id
		case "resync":
			var payload browserStreamResync
			if json.Unmarshal([]byte(data), &payload) == nil {
				p.resync = &payload
				p.resyncID = id
			}
		}
	}
}

func (p *browserStreamProbe) snapshot() (requests, ended int, cookie, hello bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, p.ended, p.cookie, p.hello
}

// browserStreamCounters is the probe's JSON view, which the browser rows read.
func (p *browserStreamProbe) counters() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	cursors := p.cursors
	if cursors == nil {
		cursors = []string{}
	}
	body, _ := json.Marshal(struct {
		Requests int                  `json:"requests"`
		Ended    int                  `json:"ended"`
		Cookie   bool                 `json:"cookie"`
		Hello    bool                 `json:"hello"`
		Cursors  []string             `json:"cursors"`
		LastID   string               `json:"last_id"`
		HelloID  string               `json:"hello_id"`
		Resync   *browserStreamResync `json:"resync"`
		ResyncID string               `json:"resync_id"`
	}{p.requests, p.ended, p.cookie, p.hello, cursors, p.lastID, p.helloID, p.resync, p.resyncID})
	return body
}

// browserStreamSniff passes frames through unchanged and remembers what each stream
// wrote, which is what makes hello, a resync and the reconnect cursor observable.
type browserStreamSniff struct {
	http.ResponseWriter
	probe *browserStreamProbe
	seen  []byte
}

func (s *browserStreamSniff) Write(p []byte) (int, error) {
	const frameLimit = 16 << 10
	if len(s.seen) < frameLimit {
		room := frameLimit - len(s.seen)
		if room > len(p) {
			room = len(p)
		}
		s.seen = append(s.seen, p[:room]...)
		if bytes.Contains(s.seen, []byte("\n\n")) {
			s.probe.observe(s.seen)
		}
	}
	return s.ResponseWriter.Write(p)
}

func (s *browserStreamSniff) Flush() {
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *browserStreamSniff) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// browserStreamInstallation is one real installation over its own store, serving the
// product's handler behind the row's probe and control reads.
type browserStreamInstallation struct {
	base     string
	origin   string // the mapped name the first-touch row loads
	loopback string // what a Playwright CLI row reaches it by
	probe    *browserStreamProbe
	handler  *browserStreamHandler
	db       *store.DB
	setup    *auth.Setup
	// effects is replaced by a restart while the control handlers read it, so every
	// access goes through effectsMu.
	effectsMu sync.Mutex
	effects   *Effects
}

func (in *browserStreamInstallation) liveEffects() *Effects {
	in.effectsMu.Lock()
	defer in.effectsMu.Unlock()
	return in.effects
}

func (in *browserStreamInstallation) replaceEffects(effects *Effects) {
	in.effectsMu.Lock()
	in.effects = effects
	in.effectsMu.Unlock()
}

// browserStreamHandler serves one installation's product handler, counts its event
// route, and answers the control reads: a restart swaps in a fresh composition over
// the same store, an expire ends the sessions the tabs hold, and a system or job
// action publishes one frame of that kind through the live composition.
type browserStreamHandler struct {
	base    string
	probe   *browserStreamProbe
	mu      sync.Mutex
	active  http.Handler
	restart func() (http.Handler, error)
	expire  func() error
	system  func() error
	job     func() error
}

func (h *browserStreamHandler) counters() string {
	return string(h.probe.counters())
}

func (h *browserStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == browserStreamProbePath && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, h.counters())
		return
	}
	if r.URL.Path == browserStreamControlPath && r.Method == http.MethodPost {
		switch r.URL.Query().Get("action") {
		case "restart":
			next, err := h.restart()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.mu.Lock()
			h.active = next
			h.mu.Unlock()
		case "expire":
			if err := h.expire(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "system":
			if err := h.system(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "job":
			if err := h.job(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.mu.Lock()
	next := h.active
	h.mu.Unlock()
	if r.URL.Path != h.base+"/api/v1/events" || r.Method != http.MethodGet {
		next.ServeHTTP(w, r)
		return
	}
	h.probe.begin(strings.Contains(r.Header.Get("Cookie"), "boxset-"), r.Header.Get("Last-Event-ID"))
	next.ServeHTTP(&browserStreamSniff{ResponseWriter: w, probe: h.probe}, r)
	h.probe.finish()
}

// newBrowserStreamInstallation opens one installation whose first admin is firstAdmin
// and, for each extra account in order, one user with the given role.
func newBrowserStreamInstallation(t *testing.T, base, firstAdmin string, accounts [][2]string) *browserStreamInstallation {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	setup := auth.NewSetup(db, auth.NewHasher(), dir)
	if _, err := setup.Arm(t.Context(), browserStreamSetupToken); err != nil {
		t.Fatal(err)
	}
	effects, err := NewEffects(db, setup, ServeConfig{Boot: "browser-stream", Version: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	s.Attach(Backend{
		Setup:         setup,
		Sessions:      effects.Sessions,
		Hub:           effects.Hub,
		Jobs:          effects.Jobs,
		DB:            db,
		SchemaVersion: 4,
		RestoreEpoch:  1,
		Version:       "test",
	})
	s.SetState(Ready)
	in := &browserStreamInstallation{base: base, probe: &browserStreamProbe{}, db: db, setup: setup, effects: effects}
	in.handler = &browserStreamHandler{base: base, probe: in.probe, active: s.http.Handler}
	in.handler.expire = func() error {
		return db.Write(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(), "DELETE FROM session")
			return err
		})
	}
	hub := func() (*events.Hub, error) {
		live := in.liveEffects()
		if live == nil || live.Hub == nil {
			return nil, errors.New("no live composition")
		}
		return live.Hub, nil
	}
	in.handler.system = func() error {
		live, err := hub()
		if err != nil {
			return err
		}
		live.Publish(events.Event{Name: events.NameSystem, Payload: api.EventSystem{State: "ready"}})
		return nil
	}
	in.handler.job = func() error {
		live, err := hub()
		if err != nil {
			return err
		}
		at := time.Unix(1_700_000_000, 0).UTC()
		live.Publish(events.Event{Name: events.NameJob, Payload: api.EventJob{Job: api.Job{
			ID: 1, Kind: "test", Class: "optional", Lane: "background", State: "succeeded",
			Attempts: 1, MaxAttempts: 1, DueAt: at, CreatedAt: at, UpdatedAt: at, Revision: 1,
		}}})
		return nil
	}
	in.handler.restart = func() (http.Handler, error) {
		// A restart ends the streams the old process served before the new composition
		// can admit one, exactly as a drain does.
		if previous := in.liveEffects(); previous != nil && previous.Hub != nil {
			previous.Hub.Close()
		}
		restarted, err := NewEffects(db, setup, ServeConfig{Boot: "browser-stream-restart", Version: "test"}, nil)
		if err != nil {
			return nil, err
		}
		next, err := New(base)
		if err != nil {
			return nil, err
		}
		next.Attach(Backend{
			Setup:         setup,
			Sessions:      restarted.Sessions,
			Hub:           restarted.Hub,
			Jobs:          restarted.Jobs,
			DB:            db,
			SchemaVersion: 4,
			RestoreEpoch:  1,
			Version:       "test",
		})
		next.SetState(Ready)
		// One committed frame before any client can reconnect, so the resync watermark
		// the restart announces is this composition's sequence and not a vacuous zero.
		restarted.Hub.Publish(events.Event{Name: events.NameSystem, Payload: api.EventSystem{State: "ready"}})
		in.replaceEffects(restarted)
		return next.http.Handler, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(ln, in.handler) }()
	t.Cleanup(func() { _ = ln.Close() })
	// Setup is open, so that class is Host-guarded: the first admin is created over the
	// loopback literal, before any row reaches the installation by name.
	host := "http://" + ln.Addr().String()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host+base+"/api/v1/setup", strings.NewReader(credentials(firstAdmin)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+browserStreamSetupToken)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d %s", response.StatusCode, body)
	}
	for _, account := range accounts {
		hasher := auth.NewHasher()
		if _, err := auth.CreateUser(t.Context(), db, hasher, account[0], password, account[1]); err != nil {
			t.Fatal(err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	in.loopback = fmt.Sprintf("http://127.0.0.1:%d%s/", port, base)
	in.origin = fmt.Sprintf("http://boxset-lan.test:%d%s/", port, base)
	return in
}

// browserStreamSwitchboard is one origin in front of two installations: it routes every
// request to the one it currently names, so a row can observe what a tab does when the
// installation behind its origin is replaced.
type browserStreamSwitchboard struct {
	mu       sync.Mutex
	second   *browserStreamHandler
	active   *browserStreamHandler
	endFirst func()
}

func newBrowserStreamSwitchboard(t *testing.T, first, second *browserStreamInstallation) (*browserStreamSwitchboard, string) {
	t.Helper()
	board := &browserStreamSwitchboard{second: second.handler, active: first.handler}
	board.endFirst = func() {
		if effects := first.liveEffects(); effects != nil && effects.Hub != nil {
			effects.Hub.Close()
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(ln, board) }()
	t.Cleanup(func() { _ = ln.Close() })
	return board, fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
}

func (s *browserStreamSwitchboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == browserStreamProbePath && r.Method == http.MethodGet {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, active.counters())
		return
	}
	if r.URL.Path == browserStreamControlPath && r.Method == http.MethodPost {
		switch r.URL.Query().Get("action") {
		case "switch":
			// The first installation's streams end before the second one can admit one:
			// the worker reconnects to the installation the origin now names.
			s.mu.Lock()
			s.active = s.second
			s.mu.Unlock()
			s.endFirst()
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	active.ServeHTTP(w, r)
}

// TestBrowserStreamIntegration is the first-touch proof of the browser stream: the
// built worker loads under the production CSP at a mapped, non-secure origin, the
// stream carries the session cookie, hello arrives before the stream ends, two tabs
// share one worker, and the document reports no policy violation. Run after
// `bun --cwd=web run build` in the same runner child:
//
//	scripts/dev-run.sh --capture "$CAP" -- sh -euc 'bun --cwd=web run build; exec env CGO_ENABLED=0 go test -tags=e2e ./internal/server -run TestBrowserStreamIntegration'
func TestBrowserStreamIntegration(t *testing.T) {
	if os.Getenv("BOXSET_CAPTURE_DIR") == "" || os.Getenv("BOXSET_CAPTURE_CHALLENGE") == "" {
		t.Fatal("BOXSET_CAPTURE_DIR and BOXSET_CAPTURE_CHALLENGE come from `scripts/dev-run.sh --capture DIR`")
	}
	installations := []*browserStreamInstallation{
		newBrowserStreamInstallation(t, "", "owner", nil),
		newBrowserStreamInstallation(t, "/media", "owner", nil),
	}
	origins := map[string]string{}
	for _, installation := range installations {
		origins[installation.base] = installation.origin
	}
	raw, err := json.Marshal(origins)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cmd := browserCommand(ctx, "bun", "-e", `
import assert from 'node:assert/strict';
import { chromium, expect } from '@playwright/test';
const origins = JSON.parse(process.env.BOXSET_STREAM_ORIGINS);
const browser = await chromium.launch({ headless: true, args: ['--host-resolver-rules=MAP boxset-lan.test 127.0.0.1'] });

async function counters(page) {
	return page.evaluate(async () => (await fetch('/__stream-probe')).json());
}
async function oneOpenStream(page) {
	let last;
	for (let attempt = 0; attempt < 150; attempt++) {
		last = await counters(page);
		if (last.requests === 1 && last.hello && last.cookie && last.ended === 0) return last;
		await new Promise((resolve) => setTimeout(resolve, 100));
	}
	throw new Error('STREAM_PROBE_TIMEOUT ' + JSON.stringify(last));
}
// settled waits until the probe stops moving, so a sampled count is the installation's
// steady state rather than a moment before the second tab's confirmation arrived.
async function settled(page) {
	let last = await counters(page);
	for (let attempt = 0; attempt < 150; attempt++) {
		await new Promise((resolve) => setTimeout(resolve, 500));
		const next = await counters(page);
		if (next.requests === last.requests && next.ended === last.ended) return next;
		last = next;
	}
	throw new Error('STREAM_PROBE_TIMEOUT ' + JSON.stringify(last));
}
try {
	for (const [base, origin] of Object.entries(origins)) {
		const context = await browser.newContext();
		try {
			await context.addInitScript(() => {
				// The rows drive connectivity through the app's own offline state; a
				// headless profile's onLine never gates them.
				Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
				window.__streamViolations = [];
				document.addEventListener('securitypolicyviolation', (event) => {
					window.__streamViolations.push([event.effectiveDirective, event.blockedURI]);
				});
			});
			const owner = await context.newPage();
			const loaded = await owner.goto(origin);
			assert.equal(loaded.status(), 200);
			const policy = loaded.headers()['content-security-policy'];
			assert.match(policy, /script-src 'self'/);
			assert.match(policy, /connect-src 'self'/);
			await expect(owner.getByRole('status')).toHaveText('Sign in to check this Boxset.');
			assert.equal(await owner.evaluate(() => window.isSecureContext), false, 'the mapped origin is not a secure context');
			assert.equal((await counters(owner)).requests, 0, 'an unconfirmed tab opened a stream');

			await owner.getByRole('link', { name: 'Sign in', exact: true }).click();
			await owner.getByLabel('Username').fill('owner');
			await owner.getByLabel('Password', { exact: true }).fill('correct horse battery staple');
			await owner.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(owner.getByRole('status')).toHaveText('Boxset is ready.');
			const first = await oneOpenStream(owner);

			const second = await context.newPage();
			let secondStatus = 0;
			second.on('request', (candidate) => {
				if (candidate.method() === 'GET' && new URL(candidate.url()).pathname.endsWith('/api/v1/system/status')) secondStatus++;
			});
			await second.goto(origin);
			await expect(second.getByRole('status')).toHaveText('Boxset is ready.');
			const shared = await settled(second);
			assert.equal(shared.requests, 1, 'two tabs opened more than one stream');
			assert.equal(shared.requests - shared.ended, 1, 'both tabs did not share one open stream');
			// The second tab's own replayed hello: after its mount read it refetched the
			// projection once from the worker's cached greeting.
			for (let attempt = 0; attempt < 100 && secondStatus < 2; attempt++) {
				await new Promise((resolve) => setTimeout(resolve, 100));
			}
			await new Promise((resolve) => setTimeout(resolve, 500));
			assert.equal(secondStatus, 2, 'the second tab did not apply its own replayed hello exactly once');
			assert.equal(await second.evaluate(() => window.isSecureContext), false);

			// The owner's document leaves: the surviving tab keeps reading the installation
			// through one open stream, whether the worker kept it or re-established it.
			const ownerViolations = await owner.evaluate(() => window.__streamViolations);
			assert.deepEqual(ownerViolations, [], 'the owner document reported a policy violation');
			await owner.close();
			let survived = await counters(second);
			for (let attempt = 0; attempt < 50 && survived.requests - survived.ended !== 1; attempt++) {
				await new Promise((resolve) => setTimeout(resolve, 100));
				survived = await counters(second);
			}
			assert.equal(survived.requests - survived.ended, 1, 'the owner exit left no single open stream');
			assert.equal(survived.hello, true);
			assert.equal(survived.cookie, true);
			const violations = await second.evaluate(() => window.__streamViolations);
			assert.deepEqual(violations, [], 'the surviving document reported a policy violation');
			console.log('STREAM-INTEGRATION ' + JSON.stringify({ base, requests: survived.requests, hello: survived.hello, cookie: survived.cookie, ended: survived.ended, open: survived.requests - survived.ended, shared: first.requests === 1, secure: false, violations: ownerViolations.length + violations.length }));
		} finally {
			await context.close();
		}
	}
} catch (error) {
	console.log('STREAM-INTEGRATION-FAIL ' + (error?.name ?? 'error') + ': ' + (error?.message ?? ''));
	process.exitCode = 1;
} finally {
	await browser.close();
}
`)
	cmd.Dir = filepath.Join("..", "..", "web")
	cmd.Env = browserEnv("BOXSET_STREAM_ORIGINS=" + string(raw))
	output := &browserOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	runErr := cmd.Run()
	t.Log(output.String())
	if runErr != nil || output.overflow {
		t.Fatalf("browser stream integration: %v overflow=%t", runErr, output.overflow)
	}
	// Every admitted stream carried the cookie and announced hello, and the browser's
	// own exit ended all of them: the surviving tab's single open stream was observed
	// in the document, where the closing context could not remove it first.
	for _, installation := range installations {
		requests, ended, cookie, hello := installation.probe.snapshot()
		if requests < 1 || ended != requests || !cookie || !hello {
			t.Fatalf("%s: requests=%d ended=%d cookie=%t hello=%t", installation.base, requests, ended, cookie, hello)
		}
	}
	// Both documents' policy arrays are the counter this PASS line reports, so a
	// violation can never hide behind a literal zero.
	violations, rows := 0, 0
	for _, line := range strings.Split(output.String(), "\n") {
		const prefix = "STREAM-INTEGRATION "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var measured struct {
			Violations int `json:"violations"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &measured); err != nil {
			t.Fatalf("stream integration measurement: %v", err)
		}
		rows++
		violations += measured.Violations
	}
	if rows != len(installations) {
		t.Fatalf("stream integration rows=%d want=%d", rows, len(installations))
	}
	if violations != 0 {
		t.Fatalf("stream integration policy violations=%d", violations)
	}
	// The row runs with --capture, which publishes the four canonical captures, so it
	// drives the same design-brief state matrix the events row does.
	browserCaptureMatrix(t, installations[0].loopback, installations[1].loopback)
	t.Log(fmt.Sprintf("STREAM-INTEGRATION-PASS bases=/,/media secure=false streams=1 hello=first cookie=session csp=%d documents=%d", violations, rows))
}

// browserCaptureMatrix publishes the design brief's state matrix: the existing shell
// capture row at both supported bases asserts 320/768/1024/1440 CSS px, keyboard and
// touch, 200% text, 400% zoom, light and dark and reduced motion with 0 CSP violations
// and 0 axe findings, and writes the four canonical captures.
func browserCaptureMatrix(t *testing.T, dev, media string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	captures := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "shell.spec.ts", "--grep", "loads the public shell, offers Retry offline, passes axe and captures")
	captures.Dir = filepath.Join("..", "..", "web")
	captures.Env = sessionBrowserEnvironment(t, "events", browserEnv("S5_DEV_URL="+dev, "S5_PREVIEW_URL="+media))
	runSessionBrowser(t, captures, 4, nil)
}

// TestBrowserEvents drives the browser rows of the shared stream: three confirmed tabs
// on one server stream, the owner tab's exit, a restart's resync with its cursor and
// exact refetch, the replayed hello of a late-confirming tab, the existing sign-in and
// no-permission states under sign-out, expiry (then the same identity signing in again
// on one new stream), a disable and a downgrade, an unconfirmed tab, a tab that leaves
// its route or whose sign-out attempt fails, a system frame that keeps the ready surface
// painted, and two installations isolated behind one origin. The same run sweeps the
// ready and sign-in states across the shell matrix (320/768/1024/1440 CSS px, light and
// dark, reduced and full motion, default and expanded text), asserting page fit, a
// keyboard focus ring and no layout shift in every cell, screenshots both states at
// 320 px in light mode, and publishes the design brief's capture matrix
// (web/e2e/shell.spec.ts) at both supported bases. Run after
// `bun --cwd=web run build` in the same runner child:
//
//	scripts/dev-run.sh --capture "$CAP" -- sh -euc 'bun --cwd=web run build; exec env CGO_ENABLED=0 go test -tags=e2e ./internal/server -run TestBrowserEvents'
func TestBrowserEvents(t *testing.T) {
	if os.Getenv("BOXSET_CAPTURE_DIR") == "" || os.Getenv("BOXSET_CAPTURE_CHALLENGE") == "" {
		t.Fatal("BOXSET_CAPTURE_DIR and BOXSET_CAPTURE_CHALLENGE come from `scripts/dev-run.sh --capture DIR`")
	}
	accounts := [][2]string{
		{"editor", auth.RoleAdmin},
		{"second", auth.RoleViewer},
		{"disabled", auth.RoleViewer},
		{"expired", auth.RoleViewer},
		{"downgradee", auth.RoleAdmin},
	}
	root := newBrowserStreamInstallation(t, "", "owner", accounts)
	media := newBrowserStreamInstallation(t, "/media", "owner", accounts)
	// The second installation of the isolation row: its own store, installed-b as its
	// first admin (user id 1 and restore epoch 1 like the owner of the first one, so the
	// two confirmed triples differ only in installId), reached only through the shared
	// origin. Its editor signs in to change a user, which publishes a users frame.
	other := newBrowserStreamInstallation(t, "", "installed-b", [][2]string{{"editor", auth.RoleAdmin}})
	_, switchedOrigin := newBrowserStreamSwitchboard(t, root, other)

	countersPath := filepath.Join(t.TempDir(), "events-counters.txt")
	env := browserEnv(
		"S5_DEV_URL="+root.loopback,
		"S5_PREVIEW_URL="+media.loopback,
		"BOXSET_EVENTS_SWITCH="+switchedOrigin,
		"BOXSET_EVENTS_SECOND="+other.loopback,
		"BOXSET_EVENTS_COUNTERS="+countersPath,
	)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	events := browserCommand(ctx, "bun", filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test", "events.spec.ts")
	events.Dir = filepath.Join("..", "..", "web")
	events.Env = sessionBrowserEnvironment(t, "events", env)
	// Twelve rows at each supported base plus the isolation row.
	runSessionBrowser(t, events, 25, nil)

	// The counters come from the spec's own policy collector and axe scan, one line per
	// row, so the PASS line reports what the run measured.
	measured, err := os.ReadFile(countersPath)
	if err != nil {
		t.Fatalf("events counters: %v", err)
	}
	csp, axe, scans, rows := 0, 0, 0, 0
	for _, line := range strings.Split(string(measured), "\n") {
		var lineCSP, lineAxe, lineScans int
		if _, scanErr := fmt.Sscanf(line, "csp=%d axe=%d scans=%d", &lineCSP, &lineAxe, &lineScans); scanErr != nil {
			continue
		}
		rows++
		csp += lineCSP
		axe += lineAxe
		scans += lineScans
	}
	if rows != 25 || scans != 2 {
		t.Fatalf("events counters rows=%d scans=%d", rows, scans)
	}

	// The capture row reports one line for every shell-matrix cell it measured. The
	// expected set is derived here, so a cell the row skipped fails this run instead of
	// leaving the PASS line to count itself.
	cells := make(map[string]bool)
	for _, line := range strings.Split(string(measured), "\n") {
		if strings.HasPrefix(line, "cell ") {
			cells[line] = true
		}
	}
	expected := 0
	for _, base := range []string{"root", "media"} {
		for _, state := range []string{"sign-in", "ready"} {
			for _, width := range []string{"320", "768", "1024", "1440"} {
				for _, theme := range []string{"light", "dark"} {
					for _, motion := range []string{"reduce", "no-preference"} {
						for _, text := range []string{"base", "expanded"} {
							cell := fmt.Sprintf(
								"cell base=%s state=%s width=%s theme=%s motion=%s text=%s",
								base, state, width, theme, motion, text,
							)
							if !cells[cell] {
								t.Fatalf("events matrix skipped %q", cell)
							}
							expected++
						}
					}
				}
			}
		}
	}
	if len(cells) != expected {
		t.Fatalf("events matrix measured %d cells, expected %d", len(cells), expected)
	}

	// The capture row screenshots the two states it asserts at 320 CSS px in light
	// mode, so the pictures the design brief names are the ones the row painted.
	captureDir := os.Getenv("BOXSET_CAPTURE_DIR")
	for _, base := range []string{"root", "media"} {
		for _, state := range []string{"sign-in", "ready"} {
			name := fmt.Sprintf("events-%s-%s-320-light.png", base, state)
			path := filepath.Join(captureDir, name)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("capture %s: %v", name, err)
			}
			header := make([]byte, 8)
			file, err := os.Open(path)
			if err != nil {
				t.Fatalf("capture %s: %v", name, err)
			}
			_, readErr := io.ReadFull(file, header)
			_ = file.Close()
			if readErr != nil || info.Size() < 20 || !bytes.Equal(header, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
				t.Fatalf("capture %s is not a PNG (size=%d read=%v)", name, info.Size(), readErr)
			}
		}
	}

	browserCaptureMatrix(t, root.loopback, media.loopback)
	t.Log(fmt.Sprintf("BROWSER-EVENTS-PASS bases=/,/media rows=%d capture=4,4 csp=%d axe=%d scans=%d cells=%d states=sign-in,ready", rows, csp, axe, scans, len(cells)))
}
