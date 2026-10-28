package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// User is a user as the API shows it. It has no password, token or verifier
// member.
type User struct {
	ID        int64     `json:"id" minimum:"1"`
	Username  string    `json:"username" minLength:"1" maxLength:"64"`
	Role      string    `json:"role" enum:"admin,member,viewer"`
	Disabled  bool      `json:"disabled"`
	Revision  int64     `json:"revision" minimum:"1"`
	CreatedAt time.Time `json:"createdAt"`
}

func userOf(u auth.User) User {
	return User{ID: u.ID, Username: u.Username, Role: u.Role, Disabled: u.Disabled, Revision: u.Revision, CreatedAt: u.CreatedAt.UTC()}
}

// Credentials is a username and password. The password is write-only: no
// response ever carries it.
type Credentials struct {
	Username string `json:"username" minLength:"1" maxLength:"64"`
	Password string `json:"password" maxLength:"1024" writeOnly:"true" doc:"12 to 1024 bytes."`
}

// Resolve enforces the rules JSON Schema cannot state: a password length in
// bytes and the username's character rules.
func (c *Credentials) Resolve(_ huma.Context, prefix *huma.PathBuffer) []error {
	var errs []error
	if _, _, err := auth.UsernameIdentity(c.Username); err != nil {
		errs = append(errs, &huma.ErrorDetail{Location: prefix.With("username"), Message: "expected 1 to 64 characters without control characters"})
	}
	if len(c.Password) < auth.MinPasswordBytes || len(c.Password) > auth.MaxPasswordBytes {
		errs = append(errs, &huma.ErrorDetail{Location: prefix.With("password"), Message: "expected 12 to 1024 bytes"})
	}
	return errs
}

// SetupInput is the first-admin request. The setup token travels only in the
// Authorization header, which the route guard verifies.
type SetupInput struct {
	Body Credentials
}

// SetupCreated is the 201 body.
type SetupCreated struct {
	User User `json:"user"`
}

// SetupOutput is the 201 response.
type SetupOutput struct {
	Body SetupCreated
}

func (h *handlers) setup(ctx context.Context, in *SetupInput) (*SetupOutput, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok || !p.Has(auth.RightSetupCreate) {
		return nil, NewProblem(http.StatusUnauthorized, "setup_credential_invalid")
	}
	u, err := h.svc.CreateFirstAdmin(ctx, p, in.Body.Username, in.Body.Password)
	switch {
	case err == nil:
		return &SetupOutput{Body: SetupCreated{User: userOf(u)}}, nil
	case errors.Is(err, auth.ErrSetupCompleted):
		return nil, NewProblem(http.StatusConflict, "setup_completed")
	case errors.Is(err, auth.ErrSetupCredential):
		return nil, NewProblem(http.StatusUnauthorized, "setup_credential_invalid")
	case errors.Is(err, auth.ErrHashBusy):
		return nil, WithRetryAfter(NewProblem(http.StatusTooManyRequests, "hash_busy"), int(auth.HashWait/time.Second))
	case errors.Is(err, auth.ErrUsernameInvalid), errors.Is(err, auth.ErrPasswordLength):
		return nil, NewProblem(http.StatusUnprocessableEntity, "validation_failed")
	}
	return nil, NewProblem(http.StatusInternalServerError, "internal_error")
}
