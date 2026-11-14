package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// pdAutoUCXBody is an automatic 1P1D NIXL body: no unit names a node.
func pdAutoUCXBody(modelID uint) string {
	return pdBody(modelID, `{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{}]},"decode":{"tp":1,"units":[{}]}}}`, `,"placement_mode":"auto"`)
}

func createAuto(h *apiHandlers, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	return rec
}

// Automatic PD placement over Ethernet skips a node that hangs UCX and picks
// another, where the same request naming that node is refused (see
// TestPDOverEthernetIsRefusedOnANodeThatHangsUCX): the platform chooses among
// the nodes that can run the engine rather than failing on one it picked.
func TestAutoPDPlacementSkipsANodeThatHangsUCX(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	body := pdAutoUCXBody(modelID)
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}})

	resp := evaluatePD(t, h, body)
	if !resp.OK || len(resp.Errors) != 0 {
		t.Fatalf("evaluate ok=%t errors=%+v, want the unaffected node picked", resp.OK, resp.Errors)
	}
	rec := createAuto(h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID uint }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	dep, err := st.GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dep.Placement, "pd-a") || !strings.Contains(dep.Placement, `"node_uuid":"pd-b"`) {
		t.Fatalf("stored placement = %s, want every unit on pd-b", dep.Placement)
	}
}

// With every node affected nothing can take the deployment: the answer is the
// ordinary no-candidate refusal, now saying how many nodes were left out for
// the kernel and which.
func TestAutoPDPlacementNamesTheNodesLeftOutForUCX(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	body := pdAutoUCXBody(modelID)
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}, "pd-b": {}})

	rec := createAuto(h, body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+messages.ErrorUnitPlacementNone+`"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, wire.Params, `{"nodes":2,"unmet":0,"cordoned":0,"offline":0,"busy":0,"no_network":0,"no_peer_memory":0,"largest":0,"ucx_netlink_hang":2,"ucx_netlink_nodes":["pd-a","pd-b"]}`)
	if deps, _ := st.ListDeployments(); len(deps) != 0 {
		t.Fatalf("a refused request left %d deployments", len(deps))
	}

	resp := evaluatePD(t, h, body)
	if resp.OK || len(resp.Errors) != 1 || resp.Errors[0].Code != messages.ErrorUnitPlacementNone || resp.Errors[0].Params["ucx_netlink_hang"] != float64(2) {
		t.Fatalf("evaluate ok=%t errors=%+v", resp.OK, resp.Errors)
	}
}

// On a host RDMA network the affected node stays a candidate: UCX is held to
// RDMA transports there, and the fact is only the warning the named placement
// gets.
func TestAutoPDPlacementKeepsANodeThatHangsUCXOverRDMA(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	attachHostRDMA(t, st)
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}, "pd-b": {Kernel: "5.15.0-203-generic"}})

	resp := evaluatePD(t, h, pdAutoUCXBody(modelID))
	if !resp.OK || len(resp.Errors) != 0 || !hasWarning(resp, messages.WarningPDUCXNetlinkHangRDMA) {
		t.Fatalf("evaluate ok=%t errors=%+v warnings=%+v, want the RDMA warning and no refusal", resp.OK, resp.Errors, resp.Warnings)
	}
	if rec := createAuto(h, pdAutoUCXBody(modelID)); rec.Code != http.StatusCreated {
		t.Fatalf("create over RDMA = %d %s", rec.Code, rec.Body.String())
	}
}
