package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type acceleratorAPI struct {
	t      *testing.T
	st     *store.Store
	router http.Handler
	token  string
	calls  []string
	reply  string
	err    error
}

// newAcceleratorAPI serves a node on the mixed host 192.0.2.115: it
// manages the Ascend card and its bus also has an RTX 4090.
func newAcceleratorAPI(t *testing.T) *acceleratorAPI {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, token := roleTestToken(t, st, "admin")
	a := &acceleratorAPI{t: t, st: st, token: token, reply: `{"accelerator":"nvidia"}`}
	a.router = Router(Deps{Store: st, AuthSigner: signer, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		raw, _ := json.Marshal(params)
		a.calls = append(a.calls, uuid+" "+method+" "+string(raw))
		return json.RawMessage(a.reply), a.err
	}})
	for _, uuid := range []string{"mixed", "plain"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	ok := true
	if err := st.UpdateNodeStatus("mixed", proto.NodeStatus{GPUProbeOK: &ok,
		GPUs: []proto.GPUInfo{{Index: 4, UUID: "ascend-4", Name: "310P3", Vendor: "ascend", PCIeBDF: "0000:17:00.0", MemoryTotalBytes: 21 << 30, Healthy: true}},
		Sys: proto.SysInfo{GPUVendor: "ascend", AcceleratorVendors: []proto.AcceleratorPresence{
			{Vendor: "ascend", Cards: 1, Model: "310P3"}, {Vendor: "nvidia", Cards: 1, Model: "AD102 [GeForce RTX 4090]"},
		}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("plain", proto.NodeStatus{GPUProbeOK: &ok, Sys: proto.SysInfo{GPUVendor: "nvidia"}}); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *acceleratorAPI) do(token, method, path, body string) (int, map[string]any, string) {
	a.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func (a *acceleratorAPI) patch(uuid, body string) (int, map[string]any, string) {
	return a.do(a.token, http.MethodPatch, "/api/v1/nodes/"+uuid+"/accelerator", body)
}

func (a *acceleratorAPI) stored(uuid string) string {
	a.t.Helper()
	node, err := a.st.NodeByUUID(uuid)
	if err != nil {
		a.t.Fatal(err)
	}
	return node.AcceleratorPreference
}

// The node reports every vendor on its host and the choice made; a node
// nobody chose for, on a single-vendor host, carries neither key.
func TestANodeSaysWhichVendorsItsHostHasOnTheWire(t *testing.T) {
	a := newAcceleratorAPI(t)
	_, mixed, raw := a.do(a.token, http.MethodGet, "/api/v1/nodes/mixed", "")
	if !strings.Contains(raw, `"accelerator_vendors":[{"vendor":"ascend","cards":1,"model":"310P3"},{"vendor":"nvidia","cards":1,"model":"AD102 [GeForce RTX 4090]"}]`) {
		t.Fatalf("mixed node: %s", raw)
	}
	assertJSONValues(t, mixed, map[string]any{"gpu_vendor": "ascend"})
	assertJSONKeysAbsent(t, mixed, "accelerator_preference")
	_, plain, _ := a.do(a.token, http.MethodGet, "/api/v1/nodes/plain", "")
	assertJSONKeysAbsent(t, plain, "accelerator_vendors", "accelerator_preference")
}

// An idle node is told to manage the other vendor's cards; the choice is
// stored once the agent accepts and shows on the node at once.
func TestChoosingTheOtherVendorOnAnIdleNode(t *testing.T) {
	a := newAcceleratorAPI(t)
	code, out, raw := a.patch("mixed", `{"accelerator":" NVIDIA "}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	if len(a.calls) != 1 || a.calls[0] != `mixed node.set_accelerator {"accelerator":"nvidia"}` {
		t.Fatalf("calls = %q", a.calls)
	}
	assertJSONValues(t, out, map[string]any{"accelerator_preference": "nvidia", "uuid": "mixed"})
	if got := a.stored("mixed"); got != "nvidia" {
		t.Fatalf("stored %q", got)
	}
	ds, err := a.st.DesiredStateForNode(mustNodeID(t, a.st, "mixed"))
	if err != nil || ds.AcceleratorPreference != "nvidia" {
		t.Fatalf("replayed %q, %v", ds.AcceleratorPreference, err)
	}

	// Back to automatic, which every node may choose.
	a.reply = `{"accelerator":"auto"}`
	if code, _, raw := a.patch("plain", `{"accelerator":"auto"}`); code != http.StatusOK {
		t.Fatalf("auto on a single-vendor node: %d %s", code, raw)
	}
	if got := a.stored("plain"); got != "auto" {
		t.Fatalf("stored %q", got)
	}
}

func TestTheAcceleratorChoiceIsValidated(t *testing.T) {
	a := newAcceleratorAPI(t)
	for _, tc := range []struct {
		uuid, body string
		status     int
		code       string
	}{
		{"mixed", `{`, http.StatusBadRequest, ""},
		{"mixed", `{}`, http.StatusBadRequest, ErrorNodeAcceleratorInvalid},
		{"mixed", `{"accelerator":"apple"}`, http.StatusBadRequest, ErrorNodeAcceleratorInvalid},
		{"mixed", `{"accelerator":"hygon"}`, http.StatusBadRequest, ErrorNodeAcceleratorNotOnHost},
		{"plain", `{"accelerator":"nvidia"}`, http.StatusBadRequest, ErrorNodeAcceleratorNotOnHost},
		{"no-such-node", `{"accelerator":"auto"}`, http.StatusNotFound, ""},
	} {
		status, out, raw := a.patch(tc.uuid, tc.body)
		if status != tc.status {
			t.Fatalf("%s %s: status %d %s", tc.uuid, tc.body, status, raw)
		}
		if tc.code != "" && out["code"] != tc.code {
			t.Fatalf("%s %s: code %v in %s", tc.uuid, tc.body, out["code"], raw)
		}
	}
	if len(a.calls) != 0 || a.stored("mixed") != "" {
		t.Fatalf("a refused request reached the agent (%q) or the store", a.calls)
	}
}

// Anything the node would keep running blocks the switch, and the agent is
// not asked; an instance being removed does not.
func TestASwitchIsRefusedWhileInstancesRun(t *testing.T) {
	a := newAcceleratorAPI(t)
	nodeID := mustNodeID(t, a.st, "mixed")
	model, err := a.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/acc", Name: "acc"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := a.st.CreateDeployment(&store.Deployment{Name: "acc", ServedName: "acc", ModelID: model.ID, NodeID: nodeID, State: "active", Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"uuid":"run-1","image":"img:1","desired_state":"running","gpu_indexes":[4],"api_port":8000}`
	if _, err := a.st.CreateInstance(&store.Instance{UUID: "run-1", DeploymentID: dep.ID, NodeID: nodeID, State: proto.InstRunning, DesiredStateJSON: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateInstance(&store.Instance{UUID: "gone-1", DeploymentID: dep.ID, NodeID: nodeID, State: proto.InstRemoving}); err != nil {
		t.Fatal(err)
	}
	status, out, raw := a.patch("mixed", `{"accelerator":"nvidia"}`)
	if status != http.StatusConflict || out["code"] != ErrorNodeAcceleratorHasInstances {
		t.Fatalf("status %d: %s", status, raw)
	}
	if params, _ := out["params"].(map[string]any); params["count"] != float64(1) {
		t.Fatalf("params = %v", out["params"])
	}
	if len(a.calls) != 0 || a.stored("mixed") != "" {
		t.Fatalf("the agent was asked (%q) or the choice stored", a.calls)
	}
}

// The agent's own refusal — it still runs something the server does not
// know of yet, or it is offline — leaves the choice unstored.
func TestTheAgentsRefusalLeavesTheChoiceUnstored(t *testing.T) {
	a := newAcceleratorAPI(t)
	a.err = errors.New("instances are running on this node; stop them before switching accelerators")
	status, _, raw := a.patch("mixed", `{"accelerator":"nvidia"}`)
	if status != http.StatusConflict || !strings.Contains(raw, "stop them before switching") {
		t.Fatalf("status %d: %s", status, raw)
	}
	if a.stored("mixed") != "" {
		t.Fatal("the refused choice was stored")
	}
	a.err, a.reply = nil, `not json`
	if status, _, raw := a.patch("mixed", `{"accelerator":"nvidia"}`); status != http.StatusBadGateway {
		t.Fatalf("garbled reply: %d %s", status, raw)
	}
	if a.stored("mixed") != "" {
		t.Fatal("a garbled reply stored the choice")
	}
}

func TestTheAcceleratorRouteNeedsASessionAndPatch(t *testing.T) {
	a := newAcceleratorAPI(t)
	if status, _, _ := a.do("", http.MethodPatch, "/api/v1/nodes/mixed/accelerator", `{"accelerator":"nvidia"}`); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost} {
		if status, _, _ := a.do(a.token, method, "/api/v1/nodes/mixed/accelerator", `{"accelerator":"nvidia"}`); status != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, status)
		}
	}
	if len(a.calls) != 0 {
		t.Fatalf("calls = %q", a.calls)
	}
}

func mustNodeID(t *testing.T, st *store.Store, uuid string) uint {
	t.Helper()
	node, err := st.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return node.ID
}

func TestTheAcceleratorChoiceNeedsNodeRPC(t *testing.T) {
	h := apiHandlers{d: Deps{}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/nodes/mixed/accelerator", strings.NewReader(`{"accelerator":"nvidia"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "mixed"}))
	rec := httptest.NewRecorder()
	h.updateNodeAccelerator(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), ErrorNodeRPCUnavailable) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// A Kubernetes node is offered, and may be switched to, only the vendors
// whose cards run on Kubernetes: its host's AMD card is left out.
func TestAKubernetesNodeChoosesOnlyAmongVendorsThatRunThere(t *testing.T) {
	a := newAcceleratorAPI(t)
	if err := a.st.DB().Model(&store.Node{}).Where("uuid = ?", "mixed").Update("kube_cluster_id", 7).Error; err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := a.st.UpdateNodeStatus("mixed", proto.NodeStatus{GPUProbeOK: &ok,
		GPUs: []proto.GPUInfo{{Index: 4, UUID: "ascend-4", Name: "310P3", Vendor: "ascend", PCIeBDF: "0000:17:00.0", MemoryTotalBytes: 21 << 30, Healthy: true}},
		Sys: proto.SysInfo{GPUVendor: "ascend", AcceleratorVendors: []proto.AcceleratorPresence{
			{Vendor: "ascend", Cards: 1, Model: "310P3"}, {Vendor: "amd", Cards: 1, Model: "Instinct MI210"},
		}}}); err != nil {
		t.Fatal(err)
	}
	_, _, raw := a.do(a.token, http.MethodGet, "/api/v1/nodes/mixed", "")
	if !strings.Contains(raw, `"accelerator_vendors":[{"vendor":"ascend","cards":1,"model":"310P3"}]`) {
		t.Fatalf("a Kubernetes node offers a vendor that does not run there: %s", raw)
	}
	code, out, body := a.patch("mixed", `{"accelerator":"amd"}`)
	if code != http.StatusBadRequest || out["code"] != ErrorNodeAcceleratorInvalid || len(a.calls) != 0 {
		t.Fatalf("switch to amd = %d %s (calls %q)", code, body, a.calls)
	}
}
