package api

import (
	"fmt"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Extension routes with AccessPublic: no authentication, a token bucket per
// client address, as public plugin routes have (dynamic_extension_rate.go).

// notePublic records a public route at mount time.
func (a *apiHandlers) notePublic(spec RouteSpec) {
	if a.publicSpecs == nil {
		a.publicSpecs, a.publicGets = map[string]RouteSpec{}, map[string]RouteSpec{}
		a.publicLimits = newPublicLimiter(publicLimiterCapacity)
	}
	a.publicSpecs[spec.Method+" /api/v1"+spec.Pattern] = spec
	if spec.Method == http.MethodGet {
		a.publicGets[spec.Pattern] = spec
	}
}

// publicRouteLimit serves h within the route's rate per client address.
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

// publicModuleRoute reports whether pattern is an extension's public route:
// it is served under any platform limit, like a public plugin route, since it
// is how a browser signs in.
func (a *apiHandlers) publicModuleRoute(method, pattern string) bool {
	_, ok := a.publicSpecs[method+" "+pattern]
	return ok
}

// checkSignIns refuses a sign-in entry whose start is not one of its module's
// public GET routes; Router runs it once the extensions are mounted.
func (a *apiHandlers) checkSignIns() {
	seen := map[string]bool{}
	for _, o := range a.d.Extensions.SignIns {
		if seen[o.Source] {
			panic(fmt.Sprintf("api: identity source %s offers a sign-in twice", o.Source))
		}
		seen[o.Source] = true
		if _, ok := a.publicGets[o.Start]; !ok {
			panic(fmt.Sprintf("api: extension %s: the sign-in entry of %s starts at %s, which is not one of its AccessPublic GET routes", o.Module, o.Source, o.Start))
		}
	}
	if len(a.d.Extensions.SignIns) > 0 && a.d.Extensions.Accounts == nil {
		panic("api: sign-in entries without an account redeemer")
	}
}
