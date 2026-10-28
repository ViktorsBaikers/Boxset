// Package api is Boxset's native HTTP API: thin Huma operations over the
// domain services, the problem model every error uses and the operation table
// the server's route guard classifies requests by. The OpenAPI 3.1 document is
// generated from the registered operations (`boxset openapi`) and committed as
// openapi.json; it is never edited by hand.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// Prefix is the path every native API route starts with.
const Prefix = "/api/v1"

// openAPIPath is where Huma serves the document (".json", "-3.0.json",
// ".yaml" and "-3.0.yaml"); those four routes need a principal of any right.
const openAPIPath = Prefix + "/openapi"

// Credential classes of the route guard (architecture § Route guard order).
const (
	ClassPublicDoc = "public-doc"
	ClassLogin     = "login"
	ClassSetup     = "setup"
	ClassPrincipal = "principal"
)

// Route is one native API route as the guard sees it: its ServeMux pattern,
// credential class and the right a principal needs ("" = any right).
type Route struct {
	Pattern string
	Class   string
	Right   string
}

// Service is the domain behind the operations.
type Service interface {
	ListUsers(context.Context, auth.UserQuery, func(auth.UserCollection) error) error
	UpdateUser(context.Context, auth.Actor, int64, auth.UserPatch) (auth.User, error)
	Login(context.Context, string, string) (auth.Session, string, error)
	Logout(context.Context) (string, error)
	// CreateFirstAdmin creates the first admin for the setup principal p.
	CreateFirstAdmin(ctx context.Context, p auth.Principal, username, password string) (auth.User, error)
	// Status is the current lifecycle snapshot.
	Status(ctx context.Context) (Status, error)
}

// operation is the documented contract of one operation: its guard class and
// right, the error codes each status may carry and its typed registration.
type operation struct {
	op       huma.Operation
	class    string
	right    string
	errors   map[int][]string
	register func(huma.API, huma.Operation)
}

// New registers every operation on mux, over svc, and returns the guard's
// routes for them, the four document routes included.
func New(mux *http.ServeMux, svc Service) (huma.API, []Route) {
	config := huma.DefaultConfig("Boxset", "1")
	config.OpenAPIPath = openAPIPath
	config.DocsPath = ""
	config.SchemasPath = ""
	config.CreateHooks = nil // no $schema links: there is no schemas route
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"cookie":    {Type: "apiKey", In: "cookie", Name: "boxset", Description: "Session cookie boxset-INSTALLATION_ID, or __Host-boxset-INSTALLATION_ID over TLS."},
		"X-Api-Key": {Type: "apiKey", In: "header", Name: "X-Api-Key"},
		"setup":     {Type: "http", Scheme: "bearer", Description: "The one-time setup token from `boxset setup-token` or BOXSET_SETUP_TOKEN."},
	}
	a := humago.New(mux, config)
	h := &handlers{svc: svc}

	routes := []Route{}
	for _, suffix := range []string{".json", "-3.0.json", ".yaml", "-3.0.yaml"} {
		routes = append(routes, Route{Pattern: http.MethodGet + " " + openAPIPath + suffix, Class: ClassPrincipal})
	}
	for _, o := range h.operations() {
		routes = append(routes, register(a, o))
	}
	return a, routes
}

// Document is the OpenAPI document exactly as GET /api/v1/openapi.json serves it.
func Document() ([]byte, error) {
	a, _ := New(http.NewServeMux(), nil)
	return json.Marshal(a.OpenAPI())
}

var principalSecurity = []map[string][]string{{"cookie": {}}, {"X-Api-Key": {}}}

func register(a huma.API, o operation) Route {
	codes := map[int][]string{http.StatusInternalServerError: {"internal_error"}}
	for status, c := range o.errors {
		codes[status] = c
		o.op.Errors = append(o.op.Errors, status)
	}
	o.op.Extensions = map[string]any{"x-boxset-class": o.class}
	if o.right != "" {
		o.op.Extensions["x-boxset-right"] = o.right
	}
	switch o.class {
	case ClassPrincipal:
		o.op.Security = principalSecurity
	case ClassSetup:
		o.op.Security = []map[string][]string{{"setup": {}}}
	}
	o.register(a, o.op)
	item := a.OpenAPI().Paths[o.op.Path]
	doc := item.Get
	if o.op.Method == http.MethodPost {
		doc = item.Post
	}
	if o.op.Method == http.MethodDelete {
		doc = item.Delete
	}
	if o.op.Method == http.MethodPatch {
		doc = item.Patch
	}
	for status, resp := range doc.Responses {
		n, err := strconv.Atoi(status)
		if err != nil || n < 400 {
			continue
		}
		c, ok := codes[n]
		if !ok {
			c = []string{defaultCode(n)}
		}
		resp.Extensions = map[string]any{"x-boxset-codes": c}
	}
	return Route{Pattern: o.op.Method + " " + o.op.Path, Class: o.class, Right: o.right}
}

type handlers struct{ svc Service }

func (h *handlers) operations() []operation {
	return []operation{
		{
			op:       huma.Operation{OperationID: "listUsers", Method: http.MethodGet, Path: Prefix + "/users", Summary: "List users", Tags: []string{"users"}, Responses: map[string]*huma.Response{"304": {Description: "Representation unchanged"}}},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.listUsers) }, class: ClassPrincipal, right: auth.RightUsersRead,
			errors: map[int][]string{400: {"cursor_invalid", "secret_in_url"}, 401: {"unauthenticated", "credential_invalid"}, 403: {"forbidden"}, 409: {"cursor_stale"}, 422: {"validation_failed"}},
		},
		{
			op:       huma.Operation{OperationID: "updateUser", Method: http.MethodPatch, Path: Prefix + "/users/{id}", Summary: "Edit user", Tags: []string{"users"}},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.patchUser) }, class: ClassPrincipal, right: auth.RightUsersWrite,
			errors: map[int][]string{400: {"body_invalid", "secret_in_url"}, 401: {"unauthenticated", "credential_invalid"}, 403: {"forbidden", "cross_origin_denied"}, 404: {"user_not_found"}, 409: {"revision_conflict", "last_admin"}, 413: {"body_too_large"}, 422: {"validation_failed", "patch_empty"}, 503: {"draining", "recovering"}},
		},
		{
			op:       huma.Operation{OperationID: "login", Method: http.MethodPost, Path: Prefix + "/session", Summary: "Sign in", Tags: []string{"session"}, DefaultStatus: 201},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.login) }, class: ClassLogin,
			errors: map[int][]string{400: {"body_invalid", "secret_in_url"}, 401: {"credentials_invalid"}, 403: {"cross_origin_denied"}, 413: {"body_too_large"}, 421: {"host_not_allowed"}, 422: {"validation_failed"}, 429: {"login_throttled", "hash_busy"}, 503: {"starting", "recovering", "draining"}},
		},
		{
			op:       huma.Operation{OperationID: "getSession", Method: http.MethodGet, Path: Prefix + "/session", Summary: "Read current session", Tags: []string{"session"}},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.session) }, class: ClassPrincipal,
			errors: map[int][]string{400: {"secret_in_url"}, 401: {"unauthenticated", "credential_invalid"}},
		},
		{
			op:       huma.Operation{OperationID: "logout", Method: http.MethodDelete, Path: Prefix + "/session", Summary: "Sign out", Tags: []string{"session"}, DefaultStatus: 204},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.logout) }, class: ClassPrincipal,
			errors: map[int][]string{400: {"secret_in_url"}, 401: {"unauthenticated", "credential_invalid"}, 403: {"cross_origin_denied", "cookie_required"}, 503: {"recovering", "draining"}},
		},
		{
			op: huma.Operation{
				OperationID:   "createFirstAdmin",
				Method:        http.MethodPost,
				Path:          Prefix + "/setup",
				Summary:       "Create the first admin with the setup token",
				Tags:          []string{"setup"},
				DefaultStatus: http.StatusCreated,
			},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.setup) },
			class:    ClassSetup,
			right:    auth.RightSetupCreate,
			errors: map[int][]string{
				http.StatusBadRequest:            {"body_invalid", "secret_in_url"},
				http.StatusUnauthorized:          {"setup_credential_invalid"},
				http.StatusForbidden:             {"cross_origin_denied"},
				http.StatusConflict:              {"setup_completed"},
				http.StatusRequestEntityTooLarge: {"body_too_large"},
				http.StatusMisdirectedRequest:    {"host_not_allowed"},
				http.StatusUnprocessableEntity:   {"validation_failed"},
				http.StatusTooManyRequests:       {"setup_throttled", "hash_busy"},
				http.StatusServiceUnavailable:    {"recovering", "draining"},
			},
		},
		{
			op: huma.Operation{
				OperationID: "getSystemStatus",
				Method:      http.MethodGet,
				Path:        Prefix + "/system/status",
				Summary:     "Lifecycle state, versions and startup timings",
				Tags:        []string{"system"},
			},
			register: func(a huma.API, op huma.Operation) { huma.Register(a, op, h.status) },
			class:    ClassPrincipal,
			right:    auth.RightStatusRead,
			errors: map[int][]string{
				http.StatusBadRequest:   {"secret_in_url"},
				http.StatusUnauthorized: {"unauthenticated", "credential_invalid"},
				http.StatusForbidden:    {"forbidden"},
			},
		},
	}
}

// Problem is every error response (application/problem+json). It never
// carries an input value: validation entries name a location, a code and a
// message only.
type Problem struct {
	Status          int            `json:"status" minimum:"100" maximum:"599"`
	Title           string         `json:"title" maxLength:"200"`
	Code            string         `json:"code" pattern:"^[a-z_]{1,64}$"`
	Detail          string         `json:"detail,omitempty" maxLength:"500"`
	Params          ProblemParams  `json:"params,omitempty"`
	Errors          []ProblemError `json:"errors,omitempty" maxItems:"32" nullable:"false"`
	CurrentRevision int64          `json:"currentRevision,omitempty" minimum:"1"`
}

// ProblemError is one validation failure.
type ProblemError struct {
	Location string `json:"location" maxLength:"128"`
	Code     string `json:"code" pattern:"^[a-z_]{1,64}$"`
	Message  string `json:"message" maxLength:"200"`
}

// ProblemParams are at most 8 non-secret parameters of a reason code, each a
// string of at most 200 characters or an integer.
type ProblemParams map[string]any

// Schema documents ProblemParams.
func (ProblemParams) Schema(huma.Registry) *huma.Schema {
	maxProps, maxLen := 8, 200
	return &huma.Schema{
		Type:          huma.TypeObject,
		MaxProperties: &maxProps,
		AdditionalProperties: &huma.Schema{OneOf: []*huma.Schema{
			{Type: huma.TypeString, MaxLength: &maxLen},
			{Type: huma.TypeInteger, Format: "int64"},
		}},
	}
}

func (p *Problem) Error() string             { return p.Code }
func (p *Problem) GetStatus() int            { return p.Status }
func (p *Problem) ContentType(string) string { return "application/problem+json" }

// NewProblem is the problem for status with code.
func NewProblem(status int, code string) *Problem {
	return &Problem{Status: status, Title: http.StatusText(status), Code: code}
}

// WithRetryAfter returns p carrying a Retry-After header of seconds.
func WithRetryAfter(p *Problem, seconds int) error {
	return huma.ErrorWithHeaders(p, http.Header{"Retry-After": {strconv.Itoa(seconds)}})
}

// WriteProblem writes p outside Huma (the route guard), with any extra headers.
func WriteProblem(w http.ResponseWriter, p *Problem, header http.Header) {
	for k, v := range header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func init() {
	// Every error Huma itself raises (body parsing, validation, limits) becomes
	// a Problem with a stable code and never echoes the offending value (F25).
	huma.NewError = newError
}

func newError(status int, _ string, errs ...error) huma.StatusError {
	p := NewProblem(status, defaultCode(status))
	if status != http.StatusUnprocessableEntity {
		return p
	}
	for _, err := range errs {
		if len(p.Errors) == 32 {
			break
		}
		e := ProblemError{Location: "body", Code: "invalid", Message: "invalid value"}
		var d huma.ErrorDetailer
		if errors.As(err, &d) {
			detail := d.ErrorDetail()
			switch detail.Location {
			case "body.username", "body.password":
				e.Location = detail.Location
			}
		}
		p.Errors = append(p.Errors, e)
	}
	return p
}

// defaultCode is the code of a status that has no more specific reason.
func defaultCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "body_invalid"
	case http.StatusRequestEntityTooLarge:
		return "body_too_large"
	case http.StatusUnprocessableEntity:
		return "validation_failed"
	case http.StatusInternalServerError:
		return "internal_error"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '_'
	}, http.StatusText(status))
}
