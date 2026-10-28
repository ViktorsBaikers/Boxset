package server

import "net/http"

// withSecurityHeaders is the outermost handler: every response, whoever writes
// it (the base strip, the guard, the mux's own 404 and 405, Huma, the shell),
// carries the referrer and sniffing policy.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
