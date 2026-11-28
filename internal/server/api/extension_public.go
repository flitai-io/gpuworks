package api

import (
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Extension routes with AccessPublic: no authentication, a token bucket per
// client address, as public plugin routes have (dynamic_extension_rate.go).

// notePublic records a public route at mount time.
func (a *apiHandlers) notePublic(spec RouteSpec) {
	if a.publicRoutes == nil {
		a.publicRoutes = map[string]RouteSpec{}
		a.publicLimits = newPublicLimiter(publicLimiterCapacity)
	}
	a.publicRoutes[spec.Method+" /api/v1"+spec.Pattern] = spec
}

// publicRouteLimit serves h within the default public rate per client
// address.
func (a *apiHandlers) publicRouteLimit(spec RouteSpec, h http.Handler) http.Handler {
	key := "module " + spec.Method + " " + spec.Pattern
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wait := a.publicLimits.takeKey(key, 0, 0, a.loginClientIP(r), time.Now().UTC()); wait > 0 {
			retryAfter(w, int((wait+time.Second-1)/time.Second))
			writeProductError(w, http.StatusTooManyRequests, messages.ErrorTooManyRequests, "too many requests to this route; try again later", nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}
