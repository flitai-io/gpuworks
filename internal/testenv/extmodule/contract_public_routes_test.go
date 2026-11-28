package extmodule

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// publicModule mounts routes anyone may call: a read, a write, and a write
// that is part of signing in (a SAML response is a POST).
type publicModule struct{ name string }

func (m *publicModule) Name() string { return m.name }
func (m *publicModule) Init(_ context.Context, h *app.Host) error {
	h.MountAPI(publicRoutes{m.name})
	return nil
}

type publicRoutes struct{ name string }

func (r publicRoutes) Name() string { return r.name }
func (r publicRoutes) Routes(m api.Mounter) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	root := "/x/" + r.name
	m.Handle(api.RouteSpec{Method: http.MethodGet, Pattern: root + "/start", Access: api.AccessPublic, Community: true}, ok)
	m.Handle(api.RouteSpec{Method: http.MethodPost, Pattern: root + "/hook", Access: api.AccessPublic, Community: true}, ok)
	m.Handle(api.RouteSpec{Method: http.MethodPost, Pattern: root + "/acs", Access: api.AccessPublic, SignIn: true, Community: true}, ok)
}

// On a running server, a module's public routes need no sign-in, and under
// a platform limit they follow their method: the read is served, the write
// is refused with the limit's code before any session is asked for, and the
// sign-in write is served under any limit.
func TestAModulesPublicRoutesUnderAPlatformLimit(t *testing.T) {
	limiter := newFakeLimiter(featuregate.Full)
	owner := &fakeModule{name: "fake", source: newFakeSource(featuregate.Full), limiter: limiter}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{owner, &publicModule{name: "pub"}}}, nil)
	call := func(method, path string) (int, string) {
		req, err := http.NewRequest(method, e.URL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(body))
	}
	const locked = `{"code":"PLATFORM_LOCKED","error":"this server is locked; it can be viewed, and changes and inference are refused until it is unlocked"}`
	const readOnly = `{"code":"PLATFORM_READ_ONLY","error":"this server is read-only; what runs keeps running and can be viewed, changes are refused"}`
	for _, c := range []struct {
		access featuregate.Access
		hook   int
		body   string
	}{
		{featuregate.Full, http.StatusNoContent, ""},
		{featuregate.ReadOnly, http.StatusForbidden, readOnly},
		{featuregate.Locked, http.StatusForbidden, locked},
	} {
		limiter.set(api.PlatformLimit{Access: c.access})
		if status, body := call(http.MethodGet, "/api/v1/x/pub/start"); status != http.StatusNoContent {
			t.Fatalf("%s: public read = %d %s", c.access, status, body)
		}
		if status, body := call(http.MethodPost, "/api/v1/x/pub/acs"); status != http.StatusNoContent {
			t.Fatalf("%s: sign-in write = %d %s", c.access, status, body)
		}
		if status, body := call(http.MethodPost, "/api/v1/x/pub/hook"); status != c.hook || body != c.body {
			t.Fatalf("%s: public write = %d %s, want %d %s", c.access, status, body, c.hook, c.body)
		}
	}
}
