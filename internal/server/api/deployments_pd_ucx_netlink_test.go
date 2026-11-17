package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The commands are what an operator pastes: their text is the contract, so a
// change to one is a change to these constants (and to the docs that show them).
const (
	wantUCXFixNow     = `sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1`
	wantUCXFixPersist = `printf 'net.ipv6.conf.all.disable_ipv6=1\nnet.ipv6.conf.default.disable_ipv6=1\n' | sudo tee /etc/sysctl.d/90-gpuworks-ucx.conf && sudo sysctl --system`
)

const wantUCXFixes = `[{"code":"disable_ipv6_now","commands":[` + `"` + wantUCXFixNow + `"` + `]},` +
	`{"code":"disable_ipv6_persist","commands":[` + `"printf 'net.ipv6.conf.all.disable_ipv6=1\\nnet.ipv6.conf.default.disable_ipv6=1\\n' | sudo tee /etc/sysctl.d/90-gpuworks-ucx.conf && sudo sysctl --system"` + `]}]`

func ucxHangOn(h *apiHandlers, hangs map[string]*proto.UCXNetlinkHang) {
	h.d.NodeRuntime = &fakeNodeRuntime{ucxHang: hangs}
}

func evaluatePD(t *testing.T, h *apiHandlers, body string) dto.EvaluateResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func attachHostRDMA(t *testing.T, st *store.Store) {
	t.Helper()
	network, err := st.CreateNetwork(&store.Network{Name: "fabric", Type: netplan.TypeHostRDMA, Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"pd-a", "pd-b"} {
		node, err := st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID, IP: fmt.Sprintf("10.20.0.%d", i+1), HCAs: store.EncodeStringList([]string{"mlx5_0"})}); err != nil {
			t.Fatal(err)
		}
	}
}

// A PD deployment whose KV moves with NIXL over Ethernet, placed by hand on a
// node whose kernel hangs UCX, is the operator's choice: evaluate warns with
// the affected nodes and the literal commands, and create goes ahead. A clean
// cluster is not warned.
func TestPDOverEthernetWarnsOnANamedNodeThatHangsUCX(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	body := pdBody(modelID, pdOneEach, "")
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}})

	resp := evaluatePD(t, h, body)
	warning := findWarning(resp, messages.WarningPDUCXNetlinkHang)
	if !resp.OK || len(resp.Errors) != 0 || warning == nil || hasWarning(resp, messages.WarningPDUCXNetlinkHangRDMA) {
		t.Fatalf("evaluate ok=%t errors=%+v warnings=%+v, want one PD_UCX_NETLINK_HANG warning", resp.OK, resp.Errors, resp.Warnings)
	}
	jsonEqual(t, warning.Params, `{
		"mode":"vllm-nixl","network":"management","names":"pd-a",
		"nodes":[{"node_uuid":"pd-a","node_name":"pd-a","kernel":"5.15.0-203-generic"}],
		"fixes":`+wantUCXFixes+`,
		"fix":"`+wantUCXFixNow+`"}`)

	// Both nodes affected: each is named once, decode (pd-b) first as the placement orders them.
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-198-generic"}, "pd-b": {}})
	resp = evaluatePD(t, h, body)
	warning = findWarning(resp, messages.WarningPDUCXNetlinkHang)
	if !resp.OK || warning == nil {
		t.Fatalf("ok=%t errors=%+v warnings=%+v", resp.OK, resp.Errors, resp.Warnings)
	}
	jsonEqual(t, warning.Params["nodes"], `[{"node_uuid":"pd-b","node_name":"pd-b"},{"node_uuid":"pd-a","node_name":"pd-a","kernel":"5.15.0-198-generic"}]`)
	if warning.Params["names"] != "pd-b, pd-a" {
		t.Fatalf("names = %v", warning.Params["names"])
	}

	// An unaffected node, a node nobody reported on, and a server without a
	// hub to ask all leave the request alone.
	for name, rt := range map[string]*fakeNodeRuntime{"none affected": {}, "other node affected": {ucxHang: map[string]*proto.UCXNetlinkHang{"elsewhere": {Kernel: "k"}}}} {
		h.d.NodeRuntime = rt
		if resp := evaluatePD(t, h, body); !resp.OK || len(resp.Errors) != 0 || hasWarning(resp, messages.WarningPDUCXNetlinkHang) || hasWarning(resp, messages.WarningPDUCXNetlinkHangRDMA) {
			t.Fatalf("%s: evaluate ok=%t errors=%+v warnings=%+v", name, resp.OK, resp.Errors, resp.Warnings)
		}
	}
	h.d.NodeRuntime = nil
	if resp := evaluatePD(t, h, body); !resp.OK || len(resp.Errors) != 0 || hasWarning(resp, messages.WarningPDUCXNetlinkHang) {
		t.Fatalf("without a hub: ok=%t errors=%+v warnings=%+v", resp.OK, resp.Errors, resp.Warnings)
	}

	// The warning does not stop the operator: create goes ahead.
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}})
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create on a named affected node = %d %s", rec.Code, rec.Body.String())
	}
	if deps, _ := st.ListDeployments(); len(deps) != 1 {
		t.Fatalf("deployments = %d, want the one created", len(deps))
	}
}

func findWarning(resp dto.EvaluateResponse, code string) *messages.Warning {
	for i := range resp.Warnings {
		if resp.Warnings[i].Code == code {
			return &resp.Warnings[i]
		}
	}
	return nil
}

func hasWarning(resp dto.EvaluateResponse, code string) bool {
	for _, w := range resp.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// On a host RDMA network UCX is held to RDMA transports and may not run the
// route check, so the same node fact is a warning that stays visible in
// evaluate, and the deployment can still be created.
func TestPDOverRDMAWarnsOnANodeThatHangsUCX(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	attachHostRDMA(t, st)
	body := pdBody(modelID, pdOneEach, "")
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}})

	resp := evaluatePD(t, h, body)
	if !resp.OK || len(resp.Errors) != 0 {
		t.Fatalf("evaluate ok=%t errors=%+v, want a warning only", resp.OK, resp.Errors)
	}
	var warning *messages.Warning
	for i := range resp.Warnings {
		if resp.Warnings[i].Code == messages.WarningPDUCXNetlinkHangRDMA {
			warning = &resp.Warnings[i]
		}
	}
	if warning == nil {
		t.Fatalf("warnings = %+v, want PD_UCX_NETLINK_HANG_RDMA", resp.Warnings)
	}
	jsonEqual(t, warning.Params, `{
		"mode":"vllm-nixl","network":"fabric","names":"pd-a",
		"nodes":[{"node_uuid":"pd-a","node_name":"pd-a","kernel":"5.15.0-203-generic"}],
		"fixes":`+wantUCXFixes+`,
		"fix":"`+wantUCXFixNow+`"}`)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create over RDMA = %d %s", rec.Code, rec.Body.String())
	}
}

// An ordinary (non-PD) deployment on an affected node is not a NIXL user: it is
// neither refused nor warned.
func TestOrdinaryDeploymentOnANodeThatHangsUCXIsUntouched(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	ucxHangOn(h, map[string]*proto.UCXNetlinkHang{"pd-a": {Kernel: "5.15.0-203-generic"}, "pd-b": {Kernel: "5.15.0-203-generic"}})
	body := fmt.Sprintf(`{"name":"plain","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"node_uuid":"pd-a"}`, modelID)
	resp := evaluatePD(t, h, body)
	if len(resp.Errors) != 0 {
		t.Fatalf("an ordinary deployment was refused: %+v", resp.Errors)
	}
	if hasWarning(resp, messages.WarningPDUCXNetlinkHang) || hasWarning(resp, messages.WarningPDUCXNetlinkHangRDMA) {
		t.Fatalf("an ordinary deployment was warned: %+v", resp.Warnings)
	}
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
}

// The node page carries the fact an online node reported, with the literal
// fixes, as wire JSON; a node that did not report it carries nothing, and the
// list stays lean.
func TestNodeAPICarriesTheUCXNetlinkHangAsWireJSON(t *testing.T) {
	st, err := store.Open("file:node_ucx_netlink_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("node-ucx-netlink", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"hangs", "fine"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &fakeNodeRuntime{ucxHang: map[string]*proto.UCXNetlinkHang{"hangs": {Kernel: "5.15.0-203-generic"}}}
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return true }, NodeRuntime: runtime})
	get := func(path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	var hangs map[string]json.RawMessage
	if err := json.Unmarshal(get("/api/v1/nodes/hangs"), &hangs); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, hangs["ucx_netlink_hang"], `{"kernel":"5.15.0-203-generic","fixes":`+wantUCXFixes+`}`)
	var fine map[string]json.RawMessage
	if err := json.Unmarshal(get("/api/v1/nodes/fine"), &fine); err != nil {
		t.Fatal(err)
	}
	if raw, present := fine["ucx_netlink_hang"]; present {
		t.Fatalf("an unaffected node carries ucx_netlink_hang %s", raw)
	}
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(get("/api/v1/nodes"), &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if raw, present := item["ucx_netlink_hang"]; present {
			t.Fatalf("the node list carries ucx_netlink_hang %s", raw)
		}
	}
}
