package api

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
)

// RouteInventory lists every route h serves, sorted, one "METHOD /path" per
// route. An extension route adds its access, action and feature:
// "METHOD /path [access=admin] [action=x.y] [feature=k]", or "[community]"
// in place of the feature, and "[sign-in]" last on a SignIn route. A plugin route, installed now, reads
// "METHOD /api/v1/x/<id><pattern> [access=..] [action=..] [feature=k]
// [x=<id>@<version>]", with "[ungated]" in place of an absent feature; the
// mount serving them adds no line. A route set's inventory is its registration
// contract; a server's tests compare it with a golden list. It fails for an
// extension route that names neither a feature nor Community.
func RouteInventory(h http.Handler) ([]string, error) {
	routes, ok := h.(chi.Routes)
	if !ok {
		return nil, fmt.Errorf("route inventory: %T is not a chi router", h)
	}
	var out []string
	var dynamic *dynamicEndpoint
	err := chi.Walk(routes, func(method, route string, handler http.Handler, _ ...func(http.Handler) http.Handler) error {
		if d, ok := handler.(*dynamicEndpoint); ok {
			// The mount itself is not a route; the plugin routes
			// installed now are, each on its own line.
			dynamic = d
			return nil
		}
		line := method + " " + route
		if ep, ok := handler.(*extensionEndpoint); ok {
			s := ep.spec
			if (s.Feature == "") == !s.Community {
				return fmt.Errorf("route inventory: extension route %s names neither a feature nor Community, or both", line)
			}
			line += " [access=" + s.Access.String() + "]"
			if s.Action != "" {
				line += " [action=" + s.Action + "]"
			}
			if s.Community {
				line += " [community]"
			} else {
				line += " [feature=" + string(s.Feature) + "]"
			}
			if s.SignIn {
				line += " [sign-in]"
			}
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if dynamic != nil {
		out = append(out, dynamic.inventory()...)
	}
	sort.Strings(out)
	return out, nil
}
