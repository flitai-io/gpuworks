package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPutCommunityCatalogValidatesAndPersists(t *testing.T) {
	st, err := store.Open("file:api_community_catalog_setting?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/image_community_catalog", strings.NewReader(`{"repos":[{"repo":"example/runtime","engine":"vllm","upstream_api":"https://registry.example/tags"}]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"key": "image_community_catalog"}))
	rec := httptest.NewRecorder()
	h.putSetting(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(st.GetSetting("image_community_catalog"), `"schema_version":3`) {
		t.Fatalf("status=%d body=%s setting=%s", rec.Code, rec.Body.String(), st.GetSetting("image_community_catalog"))
	}
	req = httptest.NewRequest(http.MethodPut, "/api/v1/settings/image_community_catalog", strings.NewReader(`{"repos":[{"repo":"example/runtime","engine":"vllm","upstream_api":"file:///etc/passwd"}]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"key": "image_community_catalog"}))
	rec = httptest.NewRecorder()
	h.putSetting(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsafe status=%d body=%s", rec.Code, rec.Body.String())
	}
	// A schema 2 document is still accepted; any other version is refused.
	for body, want := range map[string]int{
		`{"schema_version":2,"repos":[{"repo":"example/runtime","engine":"vllm"}]}`:                                                                         http.StatusOK,
		`{"schema_version":3,"repos":[{"repo":"harbor.example:5443/a/vllm","registry":"oci","engine":"vllm","siblings":["harbor.example:5443/a/sglang"]}]}`: http.StatusOK,
		`{"schema_version":4,"repos":[]}`: http.StatusBadRequest,
		`{"schema_version":3,"repos":[{"repo":"registry.example/team/mixed","registry":"oci"}]}`: http.StatusOK,
	} {
		req = httptest.NewRequest(http.MethodPut, "/api/v1/settings/image_community_catalog", strings.NewReader(body))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"key": "image_community_catalog"}))
		rec = httptest.NewRecorder()
		h.putSetting(rec, req)
		if rec.Code != want {
			t.Fatalf("%s: status=%d body=%s, want %d", body, rec.Code, rec.Body.String(), want)
		}
	}
}
