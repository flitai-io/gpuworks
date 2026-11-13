package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestGetSettingsReturnsJSONValues(t *testing.T) {
	st, err := store.Open("file:api_settings_get_legacy?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("image_catalog_version", `"v3"`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("image_registry_mirrors", `{"default":"quay"}`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%s", err, rec.Body.String())
	}
	var version string
	if err := json.Unmarshal(got["image_catalog_version"], &version); err != nil {
		t.Fatalf("catalog version is not a JSON string: %v; raw=%s", err, got["image_catalog_version"])
	}
	if version != "v3" {
		t.Fatalf("version=%q, want v3", version)
	}
	if string(got["image_registry_mirrors"]) != `{"default":"quay"}` {
		t.Fatalf("valid JSON setting changed: %s", got["image_registry_mirrors"])
	}
	// The mirror presets moved into pull source rules: the settings read no
	// longer injects them.
	if raw, ok := got["image_registry_presets"]; ok {
		t.Fatalf("image_registry_presets is still served: %s", raw)
	}
}

// The registry rewrite, mirror and preset settings were replaced by pull
// source rules; writing them is refused with a pointer to the new route and
// stores nothing.
func TestPutSettingRefusesTheRemovedRegistrySettings(t *testing.T) {
	st, err := store.Open("file:api_settings_removed_registry?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st}}
	for _, key := range []string{"image_registry_rewrites", "image_registry_mirrors", "image_registry_presets"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/"+key, strings.NewReader(`{"default":"quay"}`))
		ctx := chi.NewRouteContext()
		ctx.URLParams.Add("key", key)
		h.putSetting(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx)))
		if rec.Code != http.StatusGone {
			t.Fatalf("%s: status=%d body=%s", key, rec.Code, rec.Body.String())
		}
		var body struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "SETTING_REMOVED" || len(body.Params) != 2 || body.Params["setting"] != key || body.Params["route"] != "/api/v1/pull/source-rules" {
			t.Fatalf("%s: body=%s", key, rec.Body.String())
		}
		if got := st.GetSetting(store.SettingKey(key)); got != "" {
			t.Fatalf("%s: a refused write stored %q", key, got)
		}
	}
}

func TestPutSettingRejectsRemovedDefaultModelSource(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/default_model_source", strings.NewReader(`"huggingface"`))
	ctx := chi.NewRouteContext()
	ctx.URLParams.Add("key", "default_model_source")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx))
	(&apiHandlers{}).putSetting(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SETTING_REMOVED" {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// The network settings moved into the pull settings: proxies are entries
// selected by node requirements and URL rewrites became site rules. Writing
// the old keys, global or per node, is refused with a pointer to the new
// route and stores nothing; so are the per-node override routes.
func TestTheNetworkSettingsAreGone(t *testing.T) {
	rig := newPullRig(t)
	node := rig.node("n1", true, nil)
	for _, key := range []string{"network", store.NodeNetworkSettingKey(node.ID).String()} {
		code, body := rig.do(rig.admin, "PUT", "/api/v1/settings/"+key, `{"http_proxy":"http://proxy.example:3128"}`)
		if code != http.StatusGone || !strings.Contains(body, `"code":"SETTING_REMOVED"`) || !strings.Contains(body, `"route":"/api/v1/pull/proxies"`) {
			t.Fatalf("PUT %s = %d %s", key, code, body)
		}
		if got := rig.st.GetSetting(store.SettingKey(key)); got != "" {
			t.Fatalf("%s: a refused write stored %q", key, got)
		}
	}
	for _, method := range []string{"GET", "PUT"} {
		code, body := rig.do(rig.admin, method, "/api/v1/nodes/n1/network", `{}`)
		if code != http.StatusGone || !strings.Contains(body, `"route":"/api/v1/pull/proxies"`) {
			t.Fatalf("%s /nodes/n1/network = %d %s", method, code, body)
		}
	}
	if code, _ := rig.do(rig.admin, "GET", "/api/v1/network/presets", ""); code != http.StatusNotFound {
		t.Fatalf("the network presets dictionary still answers: %d", code)
	}
	if synced := rig.networkSyncedNodes(); len(synced) != 0 {
		t.Fatalf("a refused write told nodes to re-read: %v", synced)
	}
}

func TestPutSettingOrphanCleanupValidatesBoolAndResyncsOnlineNodes(t *testing.T) {
	st, err := store.Open("file:api_settings_orphan_cleanup?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("orphan-api-node", store.HashToken("token"), "orphan-api-host")
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	h := &apiHandlers{d: Deps{
		Store:    st,
		Online:   func(string) bool { return true },
		NodeSync: nodeSyncCallbacks{request: func(uuid string) { synced = append(synced, uuid) }},
	}}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/orphan_cleanup", strings.NewReader(body))
		ctx := chi.NewRouteContext()
		ctx.URLParams.Add("key", "orphan_cleanup")
		h.putSetting(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx)))
		return rec
	}
	if rec := put(`true`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := st.GetSetting(store.SettingOrphanCleanup); got != "true" {
		t.Fatalf("stored orphan_cleanup=%q", got)
	}
	if len(synced) != 1 || synced[0] != node.UUID {
		t.Fatalf("saving the toggle must resync online nodes, synced=%v", synced)
	}
	if rec := put(`"yes"`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-boolean accepted: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := st.GetSetting(store.SettingOrphanCleanup); got != "true" {
		t.Fatalf("rejected value overwrote the setting: %q", got)
	}
}

// roleTestToken creates a persisted session for a user with the given role,
// so the admin gate of a route can be exercised from both sides.
func roleTestToken(t *testing.T, st *store.Store, role string) (*auth.TokenSigner, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("role-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: role, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return signer, token
}
