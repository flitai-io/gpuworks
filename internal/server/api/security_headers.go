package api

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// WithSecurityHeaders also covers responses emitted before the API router,
// such as a request reaching the wrong function's listening address.
func WithSecurityHeaders(proxies *auth.ProxyTrust, next http.Handler) http.Handler {
	a := &apiHandlers{sessionPolicy: auth.SessionPolicy{Proxies: proxies}}
	return a.securityHeaders(next)
}

func (a *apiHandlers) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not set script-src/default-src globally: srcdoc playground previews
		// inherit this policy and intentionally run sandboxed inline/CDN scripts
		// (ADR 0037). Their own CSP controls scripts and network access. Keeping
		// these restrictions explicit still prevents framing and object embeds.
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'self'; object-src 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Audio Playground records from the microphone; restrict it to this origin.
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=(), payment=(), usb=()")
		if a.managementCookieSecure(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// routingError authenticates protected management paths even when chi cannot
// select a handler. Registered routes retain their normal auth/role middleware.
func (a *apiHandlers) routingError(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		protected := r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/")
		switch r.URL.Path {
		case "/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/updates/prefetch", "/api/v1/branding", "/api/v1/agent/connect",
			"/api/v1/auth/sign-in-options", "/api/v1/auth/sign-in/start", "/api/v1/auth/complete":
			protected = false
		}
		if protected {
			if _, err := a.authenticateIdentity(r); err != nil {
				w.Header().Del("Allow")
				a.writeManagementAuthError(w, err)
				return
			}
		}
		if r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/") {
			writeProductError(w, status, messages.GenericErrorCode(status), http.StatusText(status), nil)
			return
		}
		http.Error(w, http.StatusText(status), status)
	}
}
