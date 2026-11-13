package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
)

// Every GET route is either readable by the assistant, and then described
// in docs/reference/api.md (which is what the assistant searches), or denied
// with a reason. A new route stays invisible to the assistant until someone
// decides which.
func TestEveryGETRouteIsClassified(t *testing.T) {
	r, ok := Router(Deps{}).(chi.Routes)
	if !ok {
		t.Fatal("Router is not a chi.Routes")
	}
	denied := aihelper.DeniedRoutes()
	seen := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method != http.MethodGet || !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		seen[route] = true
		if _, isDenied := denied[route]; isDenied {
			return nil
		}
		if aihelper.RouteDoc(route) == "" {
			t.Errorf("GET %s is neither described in docs/reference/api.md nor denied in aihelper.deniedRoutes", route)
		}
		return nil
	})
	for route := range denied {
		if !seen[route] {
			t.Errorf("aihelper.deniedRoutes lists %s, which the router does not serve", route)
		}
	}
}

// The reader serves a read in process as the asking user: their credentials
// reach the auth middleware, an unauthenticated asker gets 401 like anyone,
// and denied or unknown routes are refused before any handler runs.
func TestAIReaderReadsAsTheAskingUser(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	router := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	h := router.(*chi.Mux)
	a := &apiHandlers{mux: h}

	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	reader := a.apiReaderFor(ask)

	status, body, err := reader.Get(context.Background(), "/api/v1/monitor/metrics", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("read = %d %v %s", status, err, body)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Total != 1 {
		t.Fatalf("body = %s err=%v", body, err)
	}

	anonymous := a.apiReaderFor(httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil))
	if status, _, err := anonymous.Get(context.Background(), "/api/v1/monitor/metrics", nil); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("anonymous read = %d %v", status, err)
	}

	for _, path := range []string{"/api/v1/apikeys", "/api/v1/ai/configs", "/api/v1/events", "/api/v1/no/such/route", "/install.sh"} {
		if _, _, err := reader.Get(context.Background(), path, url.Values{}); err == nil {
			t.Fatalf("read of %s was not refused", path)
		}
	}
	if got := reader.Route("/api/v1/nodes/abc"); got != "/api/v1/nodes/{uuid}" {
		t.Fatalf("Route = %q", got)
	}
	routes := reader.Routes()
	if len(routes) < 80 || routes[0] > routes[len(routes)-1] {
		t.Fatalf("routes = %d, sorted %v", len(routes), routes[:3])
	}
}

// Every read the assistant makes is logged with who asked, so an
// administrator can see what an answer was built from.
func TestAIReaderLogsEachReadWithTheAsker(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	router := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}, Log: log})
	a := &apiHandlers{mux: router.(*chi.Mux), d: Deps{Store: st, AuthSigner: signer, Log: log}}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	if _, _, err := a.apiReaderFor(ask).Get(context.Background(), "/api/v1/monitor/metrics", url.Values{"x": {"1"}}); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(logged.Bytes(), &record); err != nil {
		t.Fatalf("log = %s: %v", logged.String(), err)
	}
	claims, _ := signer.Verify(token)
	if record["msg"] != "ai api read" || record["user"] != claims.Username || record["route"] != "/api/v1/monitor/metrics" ||
		record["target"] != "/api/v1/monitor/metrics?x=1" || record["status"] != float64(200) || record["bytes"].(float64) <= 0 {
		t.Fatalf("log record = %v", record)
	}
}
