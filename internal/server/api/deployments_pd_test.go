package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func pdAPIFixture(t *testing.T) (*apiHandlers, uint, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/pd", Revision: "main", Name: "org/pd", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"pd-a", "pd-b"} {
		launchConfigNode(t, st, uuid)
		if err := st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st}}, model.ID, st
}

func pdBody(modelID uint, pd string, extra string) string {
	return fmt.Sprintf(`{"name":"pd","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096}%s,"placement":{"pd":%s}}`, modelID, extra, pd)
}

const pdOneEach = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-a","gpus":[0]},{"node_uuid":"pd-a","gpus":[1]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-b"}]}}}`

func TestEvaluatePDPreviewsEveryInstance(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.PD == nil || len(resp.Errors) != 0 {
		t.Fatalf("preview = %s", rec.Body.String())
	}
	if resp.PD.Mode != "vllm-nixl" || resp.PD.Fallback != "decode" || resp.PD.KVLoadFailure != "fail" || resp.Replicas != 3 || resp.PlanScope != "per_instance" {
		t.Fatalf("pd summary = %+v replicas=%d scope=%s", resp.PD, resp.Replicas, resp.PlanScope)
	}
	if len(resp.PD.Roles) != 2 || resp.PD.Roles[0].Role != "decode" || resp.PD.Roles[0].TP != 2 || len(resp.PD.Roles[1].Units) != 2 {
		t.Fatalf("roles = %+v", resp.PD.Roles)
	}
	decode := resp.PD.Roles[0].Units[0]
	if !decode.Fits || len(decode.GPUs) != 2 || decode.Address != "10.0.0.2" || decode.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"] != "10.0.0.2" {
		t.Fatalf("decode unit = %+v", decode)
	}
	if v, _ := flagValue(decode.Argv, "--kv-transfer-config"); !strings.Contains(v, `"kv_role":"kv_consumer"`) {
		t.Fatalf("decode argv = %v", decode.Argv)
	}
	if v, _ := flagValue(decode.Argv, "--tensor-parallel-size"); v != "2" {
		t.Fatalf("decode tp = %q", v)
	}
	prefill := resp.PD.Roles[1].Units[1]
	if v, _ := flagValue(prefill.Argv, "--kv-transfer-config"); !strings.Contains(v, `"kv_role":"kv_producer"`) || fmt.Sprint(prefill.GPUs) != "[1]" {
		t.Fatalf("prefill unit = %+v", prefill)
	}
	// Where the image and weights come from is answered for every node the
	// placement names, as for a multi-node draft.
	if resp.Acquisition == nil || len(resp.Acquisition.Nodes) != 2 || resp.Image == "" {
		t.Fatalf("acquisition = %+v image = %q", resp.Acquisition, resp.Image)
	}
	// The top-level command is the first decode instance's.
	if strings.Join(resp.Argv, " ") != strings.Join(decode.Argv, " ") {
		t.Fatal("top-level argv is not the first decode instance")
	}
	// Literal wire keys of the PD preview.
	var wire struct {
		Roles []struct {
			Units []map[string]json.RawMessage `json:"units"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(raw["pd"], &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node_uuid", "node_name", "address", "gpus", "fits", "argv", "env"} {
		if _, ok := wire.Roles[0].Units[0][key]; !ok {
			t.Fatalf("unit wire lacks %q: %s", key, raw["pd"])
		}
	}
	if _, ok := wire.Roles[0].Units[0]["reason"]; ok {
		t.Fatal("a fitting unit carries a reason")
	}
}

func TestEvaluatePDFieldErrors(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	for _, c := range []struct {
		name, pd, extra, code string
	}{
		{"tp direction", `{"mode":"vllm-nixl","roles":{"prefill":{"tp":2,"units":[{"node_uuid":"pd-a"}]},"decode":{"tp":1,"units":[{"node_uuid":"pd-b"}]}}}`, "", messages.ErrorPDTPDirection},
		{"overlap", `{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"pd-a","gpus":[0]}]},"decode":{"units":[{"node_uuid":"pd-a","gpus":[0]}]}}}`, "", messages.ErrorPDGPUOverlap},
		{"unknown node", `{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"nope"}]},"decode":{"units":[{"node_uuid":"pd-b"}]}}}`, "", messages.ErrorPDNodeUnknown},
		{"too many gpus", `{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"pd-a"},{"node_uuid":"pd-a"},{"node_uuid":"pd-a"}]},"decode":{"units":[{"node_uuid":"pd-b"}]}}}`, "", messages.ErrorPDNodeGPUsInsufficient},
		{"shape", pdOneEach, `,"nnodes":2`, messages.ErrorPDShapeConflict},
		{"connector arg", pdOneEach, `,"extra_args":"--kv-transfer-config {}"`, messages.ErrorPDConnectorArgConflict},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, pdBody(modelID, c.pd, c.extra))
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			errs := fieldErrorsOf(t, raw)
			if len(errs) == 0 || errs[0].Field != "placement.pd" || errs[0].Code != c.code {
				t.Fatalf("errors = %+v", errs)
			}
		})
	}
}

func TestCreatePDDeployment(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PD == nil || out.PD.State != "not_ready" || !out.PD.Short || len(out.PD.Roles) != 2 {
		t.Fatalf("pd = %+v", out.PD)
	}
	if out.PD.Roles[0].Role != "decode" || out.PD.Roles[0].Desired != 1 || out.PD.Roles[1].Desired != 2 || out.PD.Roles[1].Units[0].NodeName == "" {
		t.Fatalf("roles = %+v", out.PD.Roles)
	}
	if out.NodeUUID != "pd-b" || strings.Join(out.NodeUUIDs, ",") != "pd-b,pd-a" || out.NNodes > 1 {
		t.Fatalf("nodes = %s %v nnodes=%d", out.NodeUUID, out.NodeUUIDs, out.NNodes)
	}
	dep, err := st.GetDeployment(out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dep.Placement, `"vllm-nixl"`) || !strings.Contains(dep.Placement, `"units"`) {
		t.Fatalf("stored placement = %s", dep.Placement)
	}
	// Wire keys of the PD section.
	var wire map[string]map[string]json.RawMessage
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	wire = map[string]map[string]json.RawMessage{}
	var pd map[string]json.RawMessage
	_ = json.Unmarshal(envelope["pd"], &pd)
	wire["pd"] = pd
	for _, key := range []string{"mode", "fallback", "kv_load_failure", "state", "short", "roles"} {
		if _, ok := wire["pd"][key]; !ok {
			t.Fatalf("pd wire lacks %q: %s", key, envelope["pd"])
		}
	}
	// An ordinary deployment has no pd section on the wire.
	plain := httptest.NewRecorder()
	h.createDeployment(plain, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"plain","served_name":"plain","model_id":%d,"node_uuid":"pd-a","engine":"vllm","image":"vllm/vllm-openai:v0.25.1"}`, modelID))))
	if plain.Code != http.StatusCreated || strings.Contains(plain.Body.String(), `"pd":`) {
		t.Fatalf("plain create = %d %s", plain.Code, plain.Body.String())
	}
}

func TestCreatePDDeploymentRejects(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	for _, c := range []struct {
		name, body, code string
	}{
		{"engine", strings.Replace(pdBody(modelID, pdOneEach, ""), `"engine":"vllm"`, `"engine":"sglang"`, 1), messages.ErrorPDModeEngineMismatch},
		{"replicas", pdBody(modelID, pdOneEach, `,"replicas":3`), messages.ErrorPDShapeConflict},
		{"connector arg", pdBody(modelID, pdOneEach, `,"extra_args":"--kv-transfer-config {}"`), messages.ErrorPDConnectorArgConflict},
		{"unreadable", fmt.Sprintf(`{"name":"pd","model_id":%d,"engine":"vllm","placement":{"pd":{"mode":"vllm-nixl","roles":{},"ratio":2}}}`, modelID), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(c.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
			}
			if c.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+c.code+`"`) {
				t.Fatalf("body = %s, want code %s", rec.Body.String(), c.code)
			}
		})
	}
}

// A sleepable PD deployment whose KV would move over an RDMA network is
// refused when it is evaluated and created, with the reason: its KV cache is
// registered with the NIC and cannot be allocated again after sleep. The same
// request without sleep is accepted.
func TestCreateSleepablePDDeploymentOnRDMAIsRefused(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
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
	sleepable := pdBody(modelID, pdOneEach, `,"sleepable":true`)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(sleepable)))
	if !strings.Contains(rec.Body.String(), `"code":"`+messages.ErrorPDSleepRDMAUnsupported+`"`) {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(sleepable)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+messages.ErrorPDSleepRDMAUnsupported+`"`) || !strings.Contains(rec.Body.String(), `"network":"fabric"`) {
		t.Fatalf("create sleepable on RDMA = %d %s", rec.Code, rec.Body.String())
	}
	if deps, _ := st.ListDeployments(); len(deps) != 0 {
		t.Fatalf("a refused request left %d deployments", len(deps))
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without sleep on RDMA = %d %s", rec.Code, rec.Body.String())
	}
}

// A PD request on a network its mode's transport cannot use is refused when
// it is created, not only held by the controller afterwards.
func TestCreatePDDeploymentChecksItsNetwork(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	network, err := st.CreateNetwork(&store.Network{Name: "npu", Type: netplan.TypeAcceleratorRDMA, Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"pd-a", "pd-b"} {
		node, err := st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+messages.ErrorPDNetworkHostRequired+`"`) || !strings.Contains(rec.Body.String(), `"network":"npu"`) {
		t.Fatalf("create on an accelerator network = %d %s", rec.Code, rec.Body.String())
	}
	if deps, _ := st.ListDeployments(); len(deps) != 0 {
		t.Fatalf("a refused request left %d deployments", len(deps))
	}
	if err := st.DeleteNetwork(network.ID); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create off that network = %d %s", rec.Code, rec.Body.String())
	}
}

// pdDecodeAcrossNodes: a TP 2 decode instance whose leader holds pd-a GPU 1
// and whose follower holds a GPU on pd-b.
const pdDecodeAcrossNodes = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-a","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-a","gpus":[1],"followers":[{"node_uuid":"pd-b"}]}]}}}`

func TestEvaluatePDPreviewsEveryRankOfAMultiNodeInstance(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(pdBody(modelID, pdDecodeAcrossNodes, ""))))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.PD == nil || resp.Replicas != 2 {
		t.Fatalf("preview = %s", rec.Body.String())
	}
	decode := resp.PD.Roles[0].Units[0]
	if len(decode.Followers) != 1 {
		t.Fatalf("decode unit = %+v", decode)
	}
	follower := decode.Followers[0]
	if decode.NodeUUID != "pd-a" || fmt.Sprint(decode.GPUs) != "[1]" || follower.NodeUUID != "pd-b" || !follower.Fits || len(follower.GPUs) != 1 || len(follower.Followers) != 0 {
		t.Fatalf("ranks: leader %+v follower %+v", decode, follower)
	}
	for name, argv := range map[string][]string{"leader": decode.Argv, "follower": follower.Argv} {
		if v, _ := flagValue(argv, "--nnodes"); v != "2" {
			t.Fatalf("%s --nnodes = %q in %v", name, v, argv)
		}
		if v, _ := flagValue(argv, "--master-addr"); v != decode.Address {
			t.Fatalf("%s --master-addr = %q, want the leader's %s", name, v, decode.Address)
		}
		if v, _ := flagValue(argv, "--tensor-parallel-size"); v != "2" {
			t.Fatalf("%s tp = %q", name, v)
		}
	}
	if v, _ := flagValue(follower.Argv, "--node-rank"); v != "1" || follower.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"] != follower.Address {
		t.Fatalf("follower = %v env %v", follower.Argv, follower.Env)
	}
	// Literal wire keys: followers nest under the leader's unit.
	var wire struct {
		Roles []struct {
			Units []struct {
				Followers []map[string]json.RawMessage `json:"followers"`
			} `json:"units"`
		} `json:"roles"`
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if err := json.Unmarshal(raw["pd"], &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node_uuid", "address", "gpus", "fits", "argv", "env"} {
		if _, ok := wire.Roles[0].Units[0].Followers[0][key]; !ok {
			t.Fatalf("follower wire lacks %q: %s", key, raw["pd"])
		}
	}
}

func TestCreatePDDeploymentWithAMultiNodeInstance(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdDecodeAcrossNodes, ""))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	unit := out.PD.Roles[0].Units[0]
	if len(unit.Followers) != 1 || unit.Followers[0].NodeUUID != "pd-b" || unit.Followers[0].NodeName == "" || out.PD.Roles[0].Desired != 1 {
		t.Fatalf("pd = %+v", out.PD)
	}
	dep, _ := st.GetDeployment(out.ID)
	if dep.NNodes != 1 || !strings.Contains(dep.Placement, `"followers":[{"node_uuid":"pd-b"}]`) {
		t.Fatalf("stored: nnodes %d placement %s", dep.NNodes, dep.Placement)
	}
	var ids []uint
	_ = json.Unmarshal([]byte(dep.NodeIDs), &ids)
	if len(ids) != 2 {
		t.Fatalf("node ids = %s: the follower's node must be a member", dep.NodeIDs)
	}
	// Wire keys of a deployment's follower rank.
	var wire struct {
		PD struct {
			Roles []struct {
				Units []struct {
					Followers []map[string]json.RawMessage `json:"followers"`
				} `json:"units"`
			} `json:"roles"`
		} `json:"pd"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	follower := wire.PD.Roles[0].Units[0].Followers[0]
	if string(follower["node_uuid"]) != `"pd-b"` {
		t.Fatalf("follower wire = %v", follower)
	}
	if _, ok := follower["gpus"]; ok {
		t.Fatal("an unpinned follower carries gpus")
	}
	// PP is fixed at launch like TP.
	rec = pdOpsRequest(h.updatePD, http.MethodPut, out.ID, `{"roles":{"decode":{"pp":2,"units":[{"node_uuid":"pd-a","gpus":[1],"followers":[{"node_uuid":"pd-b"}]}]}}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), messages.ErrorPDImmutable) {
		t.Fatalf("pp change = %d %s", rec.Code, rec.Body.String())
	}
	// Dropping the follower makes the instance single-node: allowed, TP 2 on
	// one node.
	rec = pdOpsRequest(h.updatePD, http.MethodPut, out.ID, `{"roles":{"decode":{"units":[{"node_uuid":"pd-b"}]}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("to one node = %d %s", rec.Code, rec.Body.String())
	}
}

// A PD draft that names no image gets the one the platform resolves for its
// nodes, and the acquisition answer the form needs to render (it used to come
// back without either, so the form reported that it could not resolve them).
func TestEvaluatePDResolvesImageAndAcquisitionWithoutAnImage(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	body := strings.Replace(pdBody(modelID, pdOneEach, ""), `"image":"vllm/vllm-openai:v0.25.1",`, "", 1)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Image == "" || resp.Acquisition == nil || resp.Acquisition.Image.Image == "" {
		t.Fatalf("image = %q acquisition = %+v", resp.Image, resp.Acquisition)
	}
	if len(resp.PD.Roles) != 2 || len(resp.PD.Roles[0].Units) != 1 {
		t.Fatalf("pd = %+v", resp.PD)
	}
}
