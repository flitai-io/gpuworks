package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// twoNodesFixture adds a second node to twoCardFixture's cluster: one card
// whose hardware index is 0, so a placement naming card 1 names a card only
// the first node reports.
func twoNodesFixture(t *testing.T) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "_", "-")+"-nodes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	card := func(index int) proto.GPUInfo {
		return proto.GPUInfo{Index: index, Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	for uuid, cards := range map[string][]proto.GPUInfo{"two-card": {card(0), card(1)}, "one-card": {card(0)}} {
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: cards,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/two-nodes", Revision: "main", Name: "two-nodes", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, st, model.ID
}

// pinnedToCardOne creates a deployment pinned to card 1 of the two-card node.
func pinnedToCardOne(t *testing.T, h *apiHandlers, modelID uint) dto.Deployment {
	t.Helper()
	code, resp := createOnTwoCards(t, h, fmt.Sprintf(`{"name":"moved","model_id":%d,"engine":"vllm","node_uuid":"two-card","image":"example/engine:test","placement":{"gpus":[1],"tp":1},"quick_params":{}}`, modelID))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, resp)
	}
	var dep dto.Deployment
	if err := json.Unmarshal([]byte(resp), &dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

// cardNotOnNode is the code and params of a request naming card 1 on the
// one-card node; refusedBody is the whole 400 body a create or edit answers.
const (
	cardNotOnNode = `"code":"PLACEMENT_GPU_NOT_ON_NODE","params":{"gpu_index":1,"node":"one-card","node_gpus":1}`
	refusedBody   = `{"code":"PLACEMENT_GPU_NOT_ON_NODE","error":"node one-card has no GPU with index 1; usable GPUs on it: 1","params":{"gpu_index":1,"node":"one-card","node_gpus":1}}`
)

// GWKS-522: a deployment pinned to card 1 of a two-card node is edited onto a
// one-card node while the request still carries the card chosen on the old
// node. An index names a card on one machine, so the edit is refused with the
// node, the index and the node's card count, and the stored deployment is
// untouched.
func TestEditOntoAnotherNodeRefusesTheOldNodesCards(t *testing.T) {
	h, st, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	before, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := editBody(t, h, dep.ID, `"node_uuid":"two-card"`, `"node_uuid":"one-card"`)
	status, resp := putEdit(t, h, dep.ID, body, false)
	if status != http.StatusBadRequest || strings.TrimSpace(resp) != refusedBody {
		t.Fatalf("edit = %d %s, want 400 %s", status, resp, refusedBody)
	}
	after, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.NodeID != before.NodeID || after.Placement != before.Placement {
		t.Fatalf("refused edit changed the deployment: node %d→%d placement %s→%s", before.NodeID, after.NodeID, before.Placement, after.Placement)
	}
}

// The same move without a card choice - what the form sends, since a changed
// node clears it - saves, and the stored placement names no card: the new
// node's scheduler picks them.
func TestEditOntoAnotherNodeWithoutCardsSaves(t *testing.T) {
	h, st, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	body := editBody(t, h, dep.ID,
		`"node_uuid":"two-card"`, `"node_uuid":"one-card"`,
		`"placement":{"gpus":[1],"tp":1}`, `"placement":{"tp":1}`)
	status, resp := putEdit(t, h, dep.ID, body, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, resp)
	}
	row, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	oneCard, err := st.NodeWithGPUs("one-card")
	if err != nil {
		t.Fatal(err)
	}
	if row.NodeID != oneCard.ID || row.Placement != `{"tp":1}` {
		t.Fatalf("stored deployment node=%d (want %d) placement=%s", row.NodeID, oneCard.ID, row.Placement)
	}
}

// An edit that leaves the node and the cards alone is not judged on them: a
// pinned card the node stopped reporting does not block saving other fields.
func TestEditKeepingAPinnedCardTheNodeLostSaves(t *testing.T) {
	h, st, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	if err := st.UpdateNodeStatus("two-card", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "two-card", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotalBytes: 24 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	status, resp := putEdit(t, h, dep.ID, editBody(t, h, dep.ID), false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, resp)
	}
}

// The edit previewed (the form sends the deployment it edits): the card the
// old node had is a field error on placement.gpus with the same code, not a
// launch failure naming "deployment 0".
func TestEvaluateOnAnotherNodeRefusesTheOldNodesCards(t *testing.T) {
	h, _, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	body := fmt.Sprintf(`{"name":"moved","model_id":%d,"engine":"vllm","node_uuid":"one-card","image":"example/engine:test","placement":{"gpus":[1],"tp":1},"quick_params":{},"deployment_id":%d}`, modelID, dep.ID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	if want := `{"field":"placement.gpus",` + cardNotOnNode + `}`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("evaluate lacks %s\n%s", want, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) || strings.Contains(rec.Body.String(), "deployment 0") {
		t.Fatalf("evaluate = %s", rec.Body)
	}
}

// A create naming a card its node does not report is refused the same way.
func TestCreateRefusesACardTheNodeDoesNotReport(t *testing.T) {
	h, _, modelID := twoNodesFixture(t)
	code, resp := createOnTwoCards(t, h, fmt.Sprintf(`{"name":"elsewhere","model_id":%d,"engine":"vllm","node_uuid":"one-card","image":"example/engine:test","placement":{"gpus":[1],"tp":1},"quick_params":{}}`, modelID))
	if code != http.StatusBadRequest || strings.TrimSpace(resp) != refusedBody {
		t.Fatalf("create = %d %s, want 400 %s", code, resp, refusedBody)
	}
}
