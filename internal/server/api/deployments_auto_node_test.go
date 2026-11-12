package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An automatic single-node create may name no node: the server picks the one
// its evaluation would, and the stored deployment keeps only what the request
// stated - no pin to the node the pick happened to land on.
func TestAutomaticCreateWithoutANodePicksTheEvaluatedNode(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st) // 20 GiB of weights: only the 80 GiB cards fit
	create := func(name, extra string) (int, dto.Deployment, string) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","quick_params":{}%s}`, name, name, modelID, extra)
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		var got dto.Deployment
		if rec.Code == http.StatusCreated {
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, got, rec.Body.String()
	}

	status, pooled, body := create("pooled", `,"node_requirements":{"pool":["inference"]}`)
	if status != http.StatusCreated {
		t.Fatalf("automatic create without a node = %d %s", status, body)
	}
	if want := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "pool": {"inference"}}; !reflect.DeepEqual(pooled.NodeRequirements, want) {
		t.Fatalf("requirements = %v, want %v (no pin to the picked node)", pooled.NodeRequirements, want)
	}

	// Many replicas, the shape an API client scaling across a fleet sends.
	if status, many, body := create("many", `,"replicas":64`); status != http.StatusCreated || many.Replicas != 64 {
		t.Fatalf("automatic create of 64 replicas = %d %s", status, body)
	}

	// Naming the evaluated node still works exactly as before.
	if status, _, body := create("named", `,"node_uuid":"auto-big"`); status != http.StatusCreated {
		t.Fatalf("automatic create naming its node = %d %s", status, body)
	}
}

// When no node can take the deployment, the create says so with each
// candidate's reason instead of a field error about a node it never asked for.
func TestAutomaticCreateWithoutANodeReportsWhyNoneFits(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	body := fmt.Sprintf(`{"name":"nofit","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_requirements":{"gpuworks.ai/node":["auto-small"]},"quick_params":{}}`, modelID)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Every node is a candidate: the two outside the requirement say so, and
	// the one inside cannot fit the weights.
	reasons, _ := got.Params["reasons"].([]any)
	if got.Code != ErrorAutomaticPlacementUnavailable || got.Params["candidates"] != float64(3) || len(reasons) != 3 {
		t.Fatalf("body = %s, want %s with three candidates", rec.Body.String(), ErrorAutomaticPlacementUnavailable)
	}
	counts := map[any]int{}
	for _, reason := range reasons {
		counts[reason]++
	}
	if counts["node_requirements_unmet"] != 2 || counts["no_fit"] != 1 {
		t.Fatalf("reasons = %v, want two requirement misses and one no_fit", reasons)
	}
	if deps, err := st.ListDeployments(); err != nil || len(deps) != 0 {
		t.Fatalf("a refused create stored %d deployments (%v)", len(deps), err)
	}
}

// When the cards the server picked are taken before the create re-checks
// them (another create placing replicas a moment earlier), a create that
// named no node is placed again on a node that still fits; a create that
// named its node keeps the stale conflict (covered by
// TestAutomaticPlacementCreateRejectsWhenVRAMWasTaken).
func TestAutomaticCreateWithoutANodePicksAgainWhenThePickIsTaken(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	big, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-big", Revision: "main", Name: "auto-big", WeightBytes: 40 << 30})
	if err != nil {
		t.Fatal(err)
	}
	occupier, err := st.CreateDeployment(&store.Deployment{Name: "occupier", ModelID: big.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	var picks []string
	afterAutomaticPick = func(nodeUUID string) {
		picks = append(picks, nodeUUID)
		if len(picks) > 1 {
			return
		}
		node, err := st.NodeByUUID(nodeUUID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: "taken-" + nodeUUID, DeploymentID: occupier.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", APIPort: 30001}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterAutomaticPick = nil })

	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"again","served_name":"again","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","quick_params":{}}`, big.ID))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want the second pick to succeed", rec.Code, rec.Body.String())
	}
	if len(picks) != 2 || picks[0] == picks[1] {
		t.Fatalf("picks = %v, want a second pick on the other 80 GiB node", picks)
	}
}

// Picking again is bounded: when every pick is taken, the create gives up
// after deployment.AutomaticPickAttempts and stores nothing.
func TestAutomaticCreateWithoutANodeStopsPickingAgain(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	big, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-big", Revision: "main", Name: "auto-big", WeightBytes: 40 << 30})
	if err != nil {
		t.Fatal(err)
	}
	occupier, err := st.CreateDeployment(&store.Deployment{Name: "occupier", ModelID: big.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	picks := 0
	afterAutomaticPick = func(nodeUUID string) {
		picks++
		node, err := st.NodeByUUID(nodeUUID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("taken-%d", picks), DeploymentID: occupier.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", APIPort: 30001 + picks}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterAutomaticPick = nil })

	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"never","served_name":"never","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","quick_params":{}}`, big.ID))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want a conflict once no pick holds", rec.Code, rec.Body.String())
	}
	if picks > deployment.AutomaticPickAttempts {
		t.Fatalf("picked %d times, want at most %d", picks, deployment.AutomaticPickAttempts)
	}
	deps, err := st.ListDeployments()
	if err != nil || len(deps) != 1 {
		t.Fatalf("deployments = %d (%v), want only the occupier", len(deps), err)
	}
}
