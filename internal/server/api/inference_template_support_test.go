package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func templateSupportFixture() store.InferenceModel {
	model := flatAPIFixture("acme/hardware-support", "nv-general")
	base := model.Specs[0]
	spec := func(name, engine, quant, gpus string) store.InferenceSpecRow {
		row := base
		row.Name = name
		row.Engine = engine
		row.Quantization = quant
		row.GPUs = gpus
		return row
	}
	model.Specs = []store.InferenceSpecRow{
		spec("nv-general", "vllm", "bf16", `{"vendor":"nvidia","cc":">=8.0"}`),
		spec("nv-specific", "sglang", "bf16", `{"vendor":"nvidia","name":"H100","cc":"9.0"}`),
		spec("nv-denied", "vllm", "fp8", `{"vendor":"nvidia","name":"H100","cc":"9.0"}`),
		spec("ascend", "vllm", "bf16", `{"vendor":"ascend","name":"910B4"}`),
		spec("amd", "vllm", "bf16", `{"vendor":"amd","name":"8060S","arch_id":"gfx1151"}`),
	}
	model.Unsupported = `[{"gpus":{"vendor":"nvidia","name":"H100"},"quantization":"fp8","reason":"known fp8 failure"}]`
	return model
}

func TestTemplateHardwareFilterWirePreservesUnsupportedAndSpecificity(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{templateSupportFixture()}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	tests := []struct {
		name, vendor, gpu, cc, arch string
		want                        []string
	}{
		{"nvidia", "nvidia", "NVIDIA H100", "9.0", "", []string{"nv-specific", "nv-general"}},
		{"nvidia below cc", "nvidia", "NVIDIA H100", "7.0", "", nil},
		{"huawei", "ascend", "Huawei Ascend 910B4", "", "", []string{"ascend"}},
		{"huawei wrong family", "ascend", "Huawei Ascend 910C", "", "", nil},
		{"amd", "amd", "AMD Radeon 8060S", "", "gfx1151", []string{"amd"}},
		{"amd wrong arch", "amd", "AMD Radeon 8060S", "", "gfx1150", nil},
		{"amd missing arch", "amd", "AMD Radeon 8060S", "", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"vendor": {tc.vendor}, "gpu": {tc.gpu}}
			if tc.cc != "" {
				q.Set("cc", tc.cc)
			}
			if tc.arch != "" {
				q.Set("arch_id", tc.arch)
			}
			rec := httptest.NewRecorder()
			h.listInferenceTemplates(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates?"+q.Encode(), nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var wire struct {
				Items []struct {
					Specs []struct {
						Name string `json:"name"`
					} `json:"specs"`
				} `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, m := range wire.Items {
				for _, s := range m.Specs {
					names = append(names, s.Name)
				}
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("specs=%v want=%v", names, tc.want)
			}
		})
	}
}

func TestTemplateDetectedSupportWireUsesServerRules(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{templateSupportFixture()}); err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("support-node", store.HashToken("token"), "support-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "support-h100", Vendor: "nvidia", Name: "NVIDIA H100", ComputeCapability: "9.0"}).Error; err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	route := chi.NewRouteContext()
	route.URLParams.Add("*", "acme/hardware-support")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/model/acme/hardware-support", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	h.getInferenceTemplate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	var support dto.InferenceDetectedSupport
	if err := json.Unmarshal(wire["detected_support"], &support); err != nil {
		t.Fatal(err)
	}
	if support.PreferredEngine != "sglang" || len(support.Warnings) != 1 || support.Warnings[0].Reason != "known fp8 failure" || !support.Warnings[0].AlternativeAvailable {
		t.Fatalf("support=%+v", support)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(wire["detected_support"], &keys)
	if len(keys) != 2 || keys["preferred_engine"] == nil || keys["warnings"] == nil {
		t.Fatalf("support wire=%s", wire["detected_support"])
	}
	var warnings []map[string]json.RawMessage
	_ = json.Unmarshal(keys["warnings"], &warnings)
	if len(warnings[0]) != 3 || warnings[0]["alternative_available"] == nil {
		t.Fatalf("warning wire=%s", keys["warnings"])
	}
	if wire["unsupported"] == nil {
		t.Fatal("raw unsupported catalog facts must remain available")
	}
	data, err := json.Marshal(support)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip dto.InferenceDetectedSupport
	if err := json.Unmarshal(data, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip, support) {
		t.Fatal("support roundtrip changed")
	}
}

func TestTemplateRecommendationScopeWire(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{templateSupportFixture()}); err != nil {
		t.Fatal(err)
	}
	add := func(uuid, state string, gpus ...store.GPU) {
		t.Helper()
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(node).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
		for i, gpu := range gpus {
			gpu.NodeID = node.ID
			gpu.Index = i
			gpu.UUID = uuid + string(rune('a'+i))
			if err := st.DB().Create(&gpu).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	add("wrong-arch", "online", store.GPU{Vendor: "amd", Name: "Radeon 8060S", ArchID: "gfx942"})
	add("other-node", "online", store.GPU{Vendor: "amd", Name: "Radeon 8060S", ArchID: "gfx1151"})
	add("mixed-node", "online", store.GPU{Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0"}, store.GPU{Vendor: "amd", Name: "Radeon 8060S", ArchID: "gfx1151"})
	add("offline-node", "offline", store.GPU{Vendor: "amd", Name: "Radeon 8060S", ArchID: "gfx1151"})
	h := &apiHandlers{d: Deps{Store: st}}
	request := func(query string) (int, []byte) {
		rec := httptest.NewRecorder()
		h.listInferenceTemplateIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/index"+query, nil))
		return rec.Code, rec.Body.Bytes()
	}
	for _, tc := range []struct {
		node string
		want bool
	}{{"wrong-arch", false}, {"mixed-node", true}, {"offline-node", false}, {"", true}} {
		t.Run(tc.node, func(t *testing.T) {
			q := url.Values{"compatibility": {"detected"}, "vendor": {"amd"}}
			if tc.node != "" {
				q.Set("node_uuid", tc.node)
			}
			status, body := request("?" + q.Encode())
			if status != 200 {
				t.Fatalf("status=%d body=%s", status, body)
			}
			var answer struct {
				Items []struct {
					Profiles []struct {
						Name       string `json:"name"`
						Compatible *bool  `json:"detected_compatible"`
					} `json:"profiles"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &answer); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range answer.Items {
				for _, p := range m.Profiles {
					if p.Name == "amd" {
						found = true
						if p.Compatible == nil || *p.Compatible != tc.want {
							t.Fatalf("scope %s compatibility=%v body=%s", tc.node, p.Compatible, body)
						}
					}
				}
			}
			if !found {
				t.Fatal("AMD profile omitted")
			}
		})
	}
	for _, query := range []string{"?compatibility=detected&vendor=unknown", "?compatibility=detected", "?compatibility=bogus&vendor=amd"} {
		status, _ := request(query)
		if status != http.StatusBadRequest {
			t.Fatalf("query=%s status=%d", query, status)
		}
	}
	if status, _ := request("?compatibility=detected&vendor=amd&node_uuid=missing"); status != http.StatusNotFound {
		t.Fatalf("missing node status=%d", status)
	}
	status, raw := request("")
	if status != 200 {
		t.Fatalf("raw index status=%d", status)
	}
	var unscoped map[string]json.RawMessage
	_ = json.Unmarshal(raw, &unscoped)
	if unscoped["hardware"] != nil || strings.Contains(string(raw), "detected_compatible") {
		t.Fatal("raw index must not assert global compatibility")
	}
	if err := st.DB().Model(&store.Node{}).Where("uuid IN ?", []string{"other-node", "mixed-node"}).Update("state", "offline").Error; err != nil {
		t.Fatal(err)
	}
	status, body := request("?compatibility=detected&vendor=amd")
	if status != 200 {
		t.Fatalf("offline pool status=%d", status)
	}
	var answer dto.InferenceTemplateIndex
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	for _, m := range answer.Items {
		for _, p := range m.Profiles {
			if p.Name == "amd" && (p.DetectedCompatible == nil || *p.DetectedCompatible) {
				t.Fatal("offline matching cards must not enable automatic-pool recommendation")
			}
		}
	}
}
