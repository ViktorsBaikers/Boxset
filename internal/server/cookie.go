package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

type requestKey struct{}

func (s *Server) sessionCookie(r *http.Request, b *Backend, id, value string, expires time.Time) *http.Cookie {
	c := &http.Cookie{Name: "boxset-" + id, Value: value, Path: s.base, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires}
	if c.Path == "" {
		c.Path = "/"
	}
	if secureRequest(r, b.TrustedProxies) {
		c.Name = "__Host-" + c.Name
		c.Secure = true
		c.Path = "/"
	}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(auth.SessionTTL / time.Second)
	}
	return c
}

func (s *Server) Login(ctx context.Context, username, password string) (auth.Session, string, error) {
	b := s.backend.Load()
	r := ctx.Value(requestKey{}).(*http.Request)
	peer := peerIP(r.RemoteAddr)
	now := s.loginThrottle.now()
	if wait := s.loginThrottle.blocked(peer, now); wait > 0 {
		return auth.Session{}, "", api.WithRetryAfter(api.NewProblem(429, "login_throttled"), int((wait+time.Second-1)/time.Second))
	}
	identity, token, id, err := b.Sessions.Login(ctx, username, password)
	if err != nil {
		if errors.Is(err, auth.ErrCredentialsInvalid) {
			s.loginThrottle.fail(peer, now)
		}
		if b.Log != nil {
			b.Log.Info("login refused", "reason", "login_failed")
		}
		return auth.Session{}, "", err
	}
	return identity, s.sessionCookie(r, b, id, token, identity.ExpiresAt).String(), nil
}

func (s *Server) Logout(ctx context.Context) (string, error) {
	b := s.backend.Load()
	identity, ok := auth.SessionFrom(ctx)
	if !ok || identity.Credential != "cookie" {
		return "", api.NewProblem(403, "cookie_required")
	}
	if err := b.Sessions.Logout(ctx, identity); err != nil {
		return "", err
	}
	id, err := b.Sessions.Installation(ctx)
	if err != nil {
		return "", err
	}
	r := ctx.Value(requestKey{}).(*http.Request)
	return s.sessionCookie(r, b, id, "", time.Unix(1, 0)).String(), nil
}
