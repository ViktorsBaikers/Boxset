package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// committed is the checked-in document, the one contract both the server and
// the SPA's generated types are held to.
func committed(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenAPIDocumentCurrent(t *testing.T) {
	file := committed(t)
	doc, err := Document()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, file) {
		t.Fatal("internal/api/openapi.json is not the document of the registered operations: run `boxset openapi > internal/api/openapi.json` and regenerate the SPA types")
	}
	var head struct{ OpenAPI string }
	if err := json.Unmarshal(file, &head); err != nil || head.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q (err %v), want 3.1.0", head.OpenAPI, err)
	}
	var cursorMax int
	var parameters struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string
					Schema struct {
						MaxLength int `json:"maxLength"`
					}
				}
			}
		}
	}
	if err := json.Unmarshal(file, &parameters); err != nil {
		t.Fatal(err)
	}
	for _, p := range parameters.Paths["/api/v1/users"].Get.Parameters {
		if p.Name == "cursor" {
			cursorMax = p.Schema.MaxLength
		}
	}
	if cursorMax != 1024 {
		t.Fatalf("users cursor maxLength=%d, want 1024", cursorMax)
	}
	// The route serves exactly the committed bytes.
	mux := http.NewServeMux()
	New(mux, nil)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	served, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(served, file) {
		t.Fatalf("GET /api/v1/openapi.json: %d, %d bytes (err %v); want the committed %d bytes", resp.StatusCode, len(served), err, len(file))
	}
	t.Logf("OPENAPI-CURRENT bytes=%d version=%s", len(file), head.OpenAPI)
}

// document is the part of an OpenAPI document these tests read.
type document struct {
	Paths      map[string]map[string]docOperation `json:"paths"`
	Components struct {
		Schemas         map[string]docSchema      `json:"schemas"`
		SecuritySchemes map[string]map[string]any `json:"securitySchemes"`
	} `json:"components"`
}

type docOperation struct {
	Class     string                 `json:"x-boxset-class"`
	Right     string                 `json:"x-boxset-right"`
	Security  []map[string][]string  `json:"security"`
	Responses map[string]docResponse `json:"responses"`
}

type docResponse struct {
	Codes   []string `json:"x-boxset-codes"`
	Content map[string]struct {
		Schema docSchema `json:"schema"`
	} `json:"content"`
}

type docSchema struct {
	Ref        string               `json:"$ref"`
	Type       any                  `json:"type"`
	Properties map[string]docSchema `json:"properties"`
	Items      *docSchema           `json:"items"`
	Enum       []string             `json:"enum"`
	WriteOnly  bool                 `json:"writeOnly"`
	Required   []string             `json:"required"`
}

func parse(t *testing.T, b []byte) document {
	t.Helper()
	var d document
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// expected is architecture § API contract for the operations this document
// holds: class, right and, per error status, the codes. 500 internal_error is
// Huma's own fallback for an unexpected handler error.
var expected = map[string]struct {
	class, right string
	success      string
	errors       map[string][]string
}{
	"GET /api/v1/users": {ClassPrincipal, auth.RightUsersRead, "200", map[string][]string{
		"400": {"cursor_invalid", "secret_in_url"}, "401": {"unauthenticated", "credential_invalid"}, "403": {"forbidden"}, "409": {"cursor_stale"}, "422": {"validation_failed"}, "500": {"internal_error"},
	}},
	"PATCH /api/v1/users/{id}": {ClassPrincipal, auth.RightUsersWrite, "200", map[string][]string{
		"400": {"body_invalid", "secret_in_url"}, "401": {"unauthenticated", "credential_invalid"}, "403": {"forbidden", "cross_origin_denied"}, "404": {"user_not_found"}, "409": {"revision_conflict", "last_admin"}, "413": {"body_too_large"}, "422": {"validation_failed", "patch_empty"}, "500": {"internal_error"}, "503": {"draining", "recovering"},
	}},
	"POST /api/v1/session": {ClassLogin, "", "201", map[string][]string{
		"400": {"body_invalid", "secret_in_url"}, "401": {"credentials_invalid"}, "403": {"cross_origin_denied"}, "413": {"body_too_large"}, "421": {"host_not_allowed"}, "422": {"validation_failed"}, "429": {"login_throttled", "hash_busy"}, "500": {"internal_error"}, "503": {"starting", "recovering", "draining"},
	}},
	"GET /api/v1/session":    {ClassPrincipal, "", "200", map[string][]string{"400": {"secret_in_url"}, "401": {"unauthenticated", "credential_invalid"}, "500": {"internal_error"}}},
	"DELETE /api/v1/session": {ClassPrincipal, "", "204", map[string][]string{"400": {"secret_in_url"}, "401": {"unauthenticated", "credential_invalid"}, "403": {"cross_origin_denied", "cookie_required"}, "500": {"internal_error"}, "503": {"recovering", "draining"}}},
	"POST /api/v1/setup": {ClassSetup, auth.RightSetupCreate, "201", map[string][]string{
		"400": {"body_invalid", "secret_in_url"},
		"401": {"setup_credential_invalid"},
		"403": {"cross_origin_denied"},
		"409": {"setup_completed"},
		"413": {"body_too_large"},
		"421": {"host_not_allowed"},
		"422": {"validation_failed"},
		"429": {"setup_throttled", "hash_busy"},
		"500": {"internal_error"},
		"503": {"recovering", "draining"},
	}},
	"GET /api/v1/system/status": {ClassPrincipal, auth.RightStatusRead, "200", map[string][]string{
		"400": {"secret_in_url"},
		"401": {"unauthenticated", "credential_invalid"},
		"403": {"forbidden"},
		"500": {"internal_error"},
	}},
}

func TestOpenAPISemantics(t *testing.T) {
	d := parse(t, committed(t))
	seen := 0
	for path, item := range d.Paths {
		for method, op := range item {
			key := strings.ToUpper(method) + " " + path
			want, ok := expected[key]
			if !ok {
				t.Errorf("%s is in the document but not in the API contract", key)
				continue
			}
			seen++
			if op.Class != want.class || op.Right != want.right {
				t.Errorf("%s: class %q right %q, want %q %q", key, op.Class, op.Right, want.class, want.right)
			}
			statuses := slices.Sorted(maps.Keys(op.Responses))
			wantStatuses := append(slices.Sorted(maps.Keys(want.errors)), want.success)
			if key == "GET /api/v1/users" {
				wantStatuses = append(wantStatuses, "304")
			}
			slices.Sort(wantStatuses)
			if !slices.Equal(statuses, wantStatuses) {
				t.Errorf("%s: statuses %v, want %v", key, statuses, wantStatuses)
			}
			for status, codes := range want.errors {
				resp := op.Responses[status]
				if got := slices.Sorted(slices.Values(resp.Codes)); !slices.Equal(got, slices.Sorted(slices.Values(codes))) {
					t.Errorf("%s %s: codes %v, want %v", key, status, resp.Codes, codes)
				}
				if s := resp.Content["application/problem+json"].Schema; s.Ref != "#/components/schemas/Problem" {
					t.Errorf("%s %s: error body %q, want the Problem schema", key, status, s.Ref)
				}
			}
			switch op.Class {
			case ClassLogin:
				if len(op.Security) != 0 {
					t.Errorf("%s: login must be reachable without a principal", key)
				}
			case ClassPrincipal:
				if fmt.Sprint(op.Security) != "[map[cookie:[]] map[X-Api-Key:[]]]" {
					t.Errorf("%s: security %v, want cookie or X-Api-Key", key, op.Security)
				}
			case ClassSetup:
				if len(op.Security) != 1 || op.Security[0]["setup"] == nil {
					t.Errorf("%s: security %v, want the setup bearer", key, op.Security)
				}
			}
		}
	}
	if seen != len(expected) {
		t.Errorf("the document holds %d of the %d contract operations", seen, len(expected))
	}
	for _, name := range []string{"cookie", "X-Api-Key", "setup"} {
		if d.Components.SecuritySchemes[name] == nil {
			t.Errorf("security scheme %s is not declared", name)
		}
	}
	if d.Components.SecuritySchemes["X-Api-Key"]["in"] != "header" || d.Components.SecuritySchemes["cookie"]["in"] != "cookie" {
		t.Errorf("security schemes %v", d.Components.SecuritySchemes)
	}

	schemas := d.Components.Schemas
	for name, schema := range schemas {
		for _, key := range []string{"username_key", "usernameKey"} {
			if _, exists := schema.Properties[key]; exists {
				t.Errorf("%s exposes private username key %s", name, key)
			}
		}
	}
	for _, name := range []string{"Problem", "ProblemError"} {
		if _, ok := schemas[name].Properties["value"]; ok {
			t.Errorf("%s has a value member: a problem must never echo input", name)
		}
	}
	if pw := schemas["Credentials"].Properties["password"]; !pw.WriteOnly {
		t.Error("Credentials.password is not writeOnly")
	}
	if got := schemas["Status"].Properties["state"].Enum; !slices.Equal(got, []string{"starting", "recovering", "ready", "draining"}) {
		t.Errorf("Status.state enum %v", got)
	}
	if rights := schemas["Session"].Properties["rights"]; rights.Items == nil || !slices.Equal(rights.Items.Enum, []string{"status:read", "users:read", "users:write"}) {
		t.Error("Session.rights must enumerate current rights on each array item")
	}
	if got := schemas["Session"].Required; !slices.Equal(got, []string{"rights", "credential", "restoreEpoch", "localBypassEnabled", "canLogout"}) {
		t.Errorf("Session required fields %v", got)
	}
	// user is optional so the direct-peer grant can answer without inventing a
	// user; the warning and logout facts are always present.
	session := schemas["Session"]
	if session.Properties["user"].Ref != "#/components/schemas/User" {
		t.Error("Session.user is not a User")
	}
	if got := session.Properties["credential"].Enum; !slices.Contains(got, "local_bypass") {
		t.Errorf("Session.credential enum %v lacks the direct-peer grant", got)
	}
	for _, name := range []string{"localBypassEnabled", "canLogout"} {
		if got := session.Properties[name].Type; got != "boolean" {
			t.Errorf("Session.%s type %v, want boolean", name, got)
		}
	}
	// No response body, at any depth, has a password, token or verifier member.
	for key, item := range d.Paths {
		for method, op := range item {
			for status, resp := range op.Responses {
				for _, media := range resp.Content {
					if leak := secretMember(schemas, media.Schema, map[string]bool{}); leak != "" {
						t.Errorf("%s %s %s: response member %q", strings.ToUpper(method), key, status, leak)
					}
				}
			}
		}
	}
	t.Logf("OPENAPI-SEMANTICS operations=%d", seen)
}

func secretMember(schemas map[string]docSchema, s docSchema, seen map[string]bool) string {
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if seen[name] {
			return ""
		}
		seen[name] = true
		return secretMember(schemas, schemas[name], seen)
	}
	for prop, p := range s.Properties {
		lower := strings.ToLower(prop)
		for _, w := range []string{"password", "token", "verifier", "phc", "secret"} {
			if strings.Contains(lower, w) {
				return prop
			}
		}
		if leak := secretMember(schemas, p, seen); leak != "" {
			return leak
		}
	}
	if s.Items != nil {
		return secretMember(schemas, *s.Items, seen)
	}
	return ""
}

// fakeService answers the setup operation with a fixed outcome.
type fakeService struct{ err error }

func (f fakeService) ListUsers(_ context.Context, _ auth.UserQuery, visit func(auth.UserCollection) error) error {
	if f.err != nil {
		return f.err
	}
	return visit(auth.UserCollection{Items: []auth.User{}, InstallID: "123456abcdef", RestoreEpoch: 1, SchemaVersion: 2, Revision: 1})
}
func (f fakeService) UpdateUser(context.Context, auth.Actor, int64, auth.UserPatch) (auth.User, error) {
	return auth.User{}, f.err
}

func (f fakeService) Login(context.Context, string, string) (auth.Session, string, error) {
	return auth.Session{User: auth.User{ID: 1, Username: "admin", Role: auth.RoleAdmin, Revision: 1}}, "boxset=synthetic; Path=/; HttpOnly; Secure; SameSite=Strict", f.err
}
func (f fakeService) Logout(context.Context) (string, error) { return "", f.err }

func (f fakeService) CreateFirstAdmin(context.Context, auth.Principal, string, string) (auth.User, error) {
	return auth.User{ID: 1, Username: "admin", Role: auth.RoleAdmin, Revision: 1}, f.err
}
func (f fakeService) Status(context.Context) (Status, error) { return Status{}, f.err }

// Huma's own errors and the domain errors map to their documented problems,
// and none echoes the submitted values.
func TestSetupErrorMapping(t *testing.T) {
	t.Run("unknown property names never leave validation", func(t *testing.T) {
		mux := http.NewServeMux()
		New(mux, fakeService{})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(`{"username":"admin","password":"valid-long-password","property-secret-sentinel":true}`))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Rights: []string{auth.RightSetupCreate}}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var problem Problem
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if w.Code != 422 || strings.Contains(w.Body.String(), "property-secret-sentinel") || strings.Contains(w.Body.String(), `"value"`) {
			t.Fatalf("unknown property must produce a value-free 422 without its name")
		}
		if len(problem.Errors) != 1 || problem.Errors[0].Location != "body" || problem.Errors[0].Message != "invalid value" {
			t.Fatalf("unknown property location/message must be fixed generic values: %+v", problem.Errors)
		}
	})
	t.Run("known field locations survive serialization", func(t *testing.T) {
		for _, field := range []string{"username", "password"} {
			body := map[string]string{"username": "admin", "password": "valid-long-password"}
			if field == "username" {
				body[field] = "name\u202eformat"
			} else {
				body[field] = "short"
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			New(mux, fakeService{})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewReader(encoded))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Rights: []string{auth.RightSetupCreate}}))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var problem Problem
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if w.Code != 422 || len(problem.Errors) != 1 || problem.Errors[0].Location != "body."+field || problem.Errors[0].Message != "invalid value" || strings.Contains(w.Body.String(), body[field]) || strings.Contains(w.Body.String(), `"value"`) {
				t.Fatalf("%s: expected exact safe field location and generic message without submitted value: %s", field, w.Body.String())
			}
		}
	})
	const sentinel = "pw-S3NT1NEL-0451"
	call := func(svc Service, path, body string) (*httptest.ResponseRecorder, map[string]any) {
		mux := http.NewServeMux()
		New(mux, svc)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Rights: []string{auth.RightSetupCreate}})))
		})
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		if strings.Contains(w.Body.String(), sentinel) || strings.Contains(w.Body.String(), `"value"`) {
			t.Errorf("body %q echoes input", body)
		}
		return w, m
	}
	valid := `{"username":"admin","password":"` + sentinel + `"}`
	for _, c := range []struct {
		svc    Service
		path   string
		body   string
		status int
		code   string
		retry  string
	}{
		{fakeService{}, "/api/v1/setup", valid, 201, "", ""},
		{fakeService{err: auth.ErrSetupCompleted}, "/api/v1/setup", valid, 409, "setup_completed", ""},
		{fakeService{err: auth.ErrSetupCredential}, "/api/v1/setup", valid, 401, "setup_credential_invalid", ""},
		{fakeService{err: auth.ErrHashBusy}, "/api/v1/setup", valid, 429, "hash_busy", "2"},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"` + sentinel, 400, "body_invalid", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"short"}`, 422, "validation_failed", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"","password":"` + sentinel + `"}`, 422, "validation_failed", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"` + sentinel + `","extra":"` + sentinel + `"}`, 422, "validation_failed", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"` + strings.Repeat("x", 1025) + `"}`, 422, "validation_failed", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"` + strings.Repeat("é", 600) + `"}`, 422, "validation_failed", ""},
		{fakeService{}, "/api/v1/setup", `{"username":"admin","password":"` + strings.Repeat("x", 2<<20) + `"}`, 413, "body_too_large", ""},
		{fakeService{}, "/api/v1/session", valid, 201, "", ""},
		{fakeService{err: auth.ErrCredentialsInvalid}, "/api/v1/session", valid, 401, "credentials_invalid", ""},
		{fakeService{err: auth.ErrHashBusy}, "/api/v1/session", valid, 429, "hash_busy", "2"},
	} {
		w, m := call(c.svc, c.path, c.body)
		if w.Code != c.status || (c.code != "" && m["code"] != c.code) || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("%.60s: %d %v Retry-After %q, want %d %s %q", c.body, w.Code, m["code"], w.Header().Get("Retry-After"), c.status, c.code, c.retry)
		}
		if c.code != "" && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%.60s: Content-Type %q", c.body, w.Header().Get("Content-Type"))
		}
		if c.status == 422 {
			errs, _ := m["errors"].([]any)
			if len(errs) == 0 {
				t.Errorf("%.60s: 422 without errors[]", c.body)
			}
			for _, e := range errs {
				entry, _ := e.(map[string]any)
				if _, ok := entry["value"]; ok || entry["location"] == nil || entry["code"] == nil {
					t.Errorf("%.60s: error entry %v", c.body, entry)
				}
			}
		}
		if c.status == 201 {
			user, _ := m["user"].(map[string]any)
			if user["username"] != "admin" || user["role"] != "admin" || len(user) != 6 {
				t.Errorf("201 body %v", m)
			}
		}
	}
}
