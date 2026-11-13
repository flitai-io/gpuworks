package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEnginesTargetVendorScopesSleep(t *testing.T) {
	f := newSleepFixture(t)
	router, token := kunlunRouter(t, f.st)
	for _, tc := range []struct {
		vendor string
		status int
		sleep  bool
	}{
		{"kunlunxin", http.StatusOK, false}, {"nvidia", http.StatusOK, true},
		{"", http.StatusOK, true}, {"unsupported", http.StatusBadRequest, false},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/engines?target_vendor="+tc.vendor, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				return
			}
			var profiles []deploy.Profile
			if err := unmarshalList(rec.Body.Bytes(), &profiles); err != nil {
				t.Fatal(err)
			}
			for _, p := range profiles {
				if tc.vendor == "kunlunxin" && len(p.PDModes) != 0 {
					t.Fatalf("unsupported PD advertised by %s: %+v", p.Name, p.PDModes)
				}
				if p.Name == "vllm" && p.Sleep.Supported != tc.sleep {
					t.Fatalf("sleep = %+v", p.Sleep)
				}
			}
		})
	}
}

func TestKunlunEvaluateRejectsSleepWithoutSelectedNode(t *testing.T) {
	f := newSleepFixture(t)
	if err := f.st.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{Sys: proto.SysInfo{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "kunlunxin", Framework: "xre"}, GPUs: []proto.GPUInfo{{Index: 0, UUID: "k0", Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotalBytes: 96 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	model, err := f.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []string{`"sleepable":true`, `"extra_args":"--enable-sleep-mode"`} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","target_vendor":"kunlunxin",%s}`, model.ID, fields)
		router, token := kunlunRouter(t, f.st)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "sleep_unsupported_on_node") {
			t.Fatalf("sleep bypass: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func kunlunRouter(t *testing.T, st *store.Store) (http.Handler, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("kunlun-contract-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return Router(Deps{Store: st, AuthSigner: signer}), token
}
func TestKunlunEngineRouteAuthenticationAndMethod(t *testing.T) {
	f := newSleepFixture(t)
	router, token := kunlunRouter(t, f.st)
	for _, tc := range []struct {
		method, token string
		want          int
	}{{"GET", "", 401}, {"POST", token, 405}, {"GET", token, 200}} {
		req := httptest.NewRequest(tc.method, "/api/v1/engines?target_vendor=kunlunxin", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}
func TestKunlunAutomaticSleepReportsRefusalThroughRouter(t *testing.T) {
	f := newSleepFixture(t)
	if err := f.st.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{Sys: proto.SysInfo{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "kunlunxin", Framework: "xre"}, GPUs: []proto.GPUInfo{{Index: 0, UUID: "k0", Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotalBytes: 96 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/kunlun-auto", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	router, token := kunlunRouter(t, f.st)
	for _, path := range []string{"/api/v1/evaluate"} {
		// No target_vendor, node or selected GPU: this was the automatic-placement gap.
		body := fmt.Sprintf(`{"name":"auto-sleep","model_id":%d,"engine":"vllm","image":"example/kunlun:test","placement_mode":"auto","sleepable":true}`, model.ID)
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code >= 500 || !strings.Contains(rec.Body.String(), "sleep_unsupported_on_node") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
