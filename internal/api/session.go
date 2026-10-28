package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// Session is the current identity and policy of one request. user is present
// for every credential and absent only for the direct-peer local-bypass grant,
// which holds no identity, expiry or stored row.
type Session struct {
	User               *User     `json:"user,omitempty"`
	Rights             []string  `json:"rights" maxItems:"3" uniqueItems:"true" nullable:"false" enum:"status:read,users:read,users:write"`
	Credential         string    `json:"credential" enum:"cookie,key,local_bypass"`
	RestoreEpoch       int64     `json:"restoreEpoch" minimum:"1"`
	ExpiresAt          time.Time `json:"expiresAt,omitzero"`
	LocalBypassEnabled bool      `json:"localBypassEnabled"`
	CanLogout          bool      `json:"canLogout"`
}

type SessionCreated struct {
	User      User      `json:"user"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type LoginOutput struct {
	SetCookie    string `header:"Set-Cookie"`
	CacheControl string `header:"Cache-Control"`
	Body         SessionCreated
}
type SessionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         Session
}
type LogoutOutput struct {
	SetCookie string `header:"Set-Cookie"`
}

func (h *handlers) login(ctx context.Context, in *SetupInput) (*LoginOutput, error) {
	s, cookie, err := h.svc.Login(ctx, in.Body.Username, in.Body.Password)
	if err == nil {
		return &LoginOutput{SetCookie: cookie, CacheControl: "no-store", Body: SessionCreated{User: userOf(s.User), ExpiresAt: s.ExpiresAt}}, nil
	}
	var problem *Problem
	if errors.As(err, &problem) {
		return nil, err
	}
	if errors.Is(err, auth.ErrCredentialsInvalid) {
		return nil, NewProblem(401, "credentials_invalid")
	}
	if errors.Is(err, auth.ErrHashBusy) {
		return nil, WithRetryAfter(NewProblem(429, "hash_busy"), 2)
	}
	return nil, NewProblem(500, "internal_error")
}
func (h *handlers) session(ctx context.Context, _ *struct{}) (*SessionOutput, error) {
	s, ok := auth.SessionFrom(ctx)
	if !ok {
		return nil, NewProblem(401, "unauthenticated")
	}
	body := Session{Rights: s.Rights, Credential: s.Credential, RestoreEpoch: s.RestoreEpoch, ExpiresAt: s.ExpiresAt, LocalBypassEnabled: s.LocalBypassEnabled, CanLogout: s.CanLogout}
	if s.Credential != auth.CredentialLocalBypass {
		user := userOf(s.User)
		body.User = &user
	}
	return &SessionOutput{CacheControl: "no-store", Body: body}, nil
}
func (h *handlers) logout(ctx context.Context, _ *struct{}) (*LogoutOutput, error) {
	cookie, err := h.svc.Logout(ctx)
	if err == nil {
		return &LogoutOutput{SetCookie: cookie}, nil
	}
	var problem *Problem
	if errors.As(err, &problem) {
		return nil, err
	}
	return nil, NewProblem(http.StatusInternalServerError, "internal_error")
}
