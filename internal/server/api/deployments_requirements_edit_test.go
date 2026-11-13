package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func putRequirements(t *testing.T, h *apiHandlers, id uint, body string) (int, string) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(id))
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/requirements", id), strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.setDeploymentRequirements(rec, req)
	return rec.Code, rec.Body.String()
}

// editableDeployment is a deployment created through the API pinned to the
// 80 GiB node of automaticPlacementFixture, for its 20 GiB model.
func editableDeployment(t *testing.T, h *apiHandlers, modelID uint) dto.Deployment {
	t.Helper()
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"name":"edited","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","replicas":2,"quick_params":{}}`, modelID)
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

// A deployment pinned to one node is opened to a labelled pool and switched
// to binpack without redeploying: the vendor it was created for is kept, the
// change is announced to the controller, and the response shows each value
// the way the node pages do with how many nodes carry it.
func TestEditRequirementsOpensAPinnedDeploymentToAPool(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	sub := st.Bus.Subscribe(16, "deployment")
	defer sub.Close()

	status, body := putRequirements(t, h, dep.ID, `{"node_requirements":{"pool":["inference"]},"placement_strategy":"binpack"}`)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	for _, want := range []string{
		`"node_requirements":{"gpuworks.ai/vendor":["nvidia"],"pool":["inference"]}`,
		`"node_requirement_labels":[{"key":"gpuworks.ai/vendor","value":"nvidia","display":"NVIDIA","display_code":"common:vendor.nvidia","nodes":3},{"key":"pool","value":"inference","display":"inference","nodes":1}]`,
		`"placement_strategy":"binpack"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response lacks %s\n%s", want, body)
		}
	}
	stored, err := st.DeploymentRequirements(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "pool": {"inference"}}; !reflect.DeepEqual(map[string][]string(stored), want) {
		t.Fatalf("stored = %v, want %v", stored, want)
	}
	if row, _ := st.GetDeployment(dep.ID); row.PlacementStrategy != store.PlacementBinpack || row.Replicas != 2 {
		t.Fatalf("stored strategy=%q replicas=%d", row.PlacementStrategy, row.Replicas)
	}
	select {
	case ev := <-sub.C():
		if ev.Key != fmt.Sprint(dep.ID) || ev.Reason != store.DeploymentChangeRequirements {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("the controller was not told the requirements changed")
	}

	// Unstated strategy keeps the current one; a deleted node in a list is
	// kept and shown as carried by no node.
	status, body = putRequirements(t, h, dep.ID, `{"node_requirements":{"gpuworks.ai/node":["auto-big","gone"]}}`)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	for _, want := range []string{
		`{"key":"gpuworks.ai/node","value":"auto-big","display":"auto-big","nodes":1},{"key":"gpuworks.ai/node","value":"gone","display":"gone","nodes":0}`,
		`"placement_strategy":"binpack"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response lacks %s\n%s", want, body)
		}
	}
}

// Requirements under which no node could ever run a replica, or that cannot
// be stored, are refused and leave the deployment as it was.
func TestEditRequirementsRefusals(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	dep := editableDeployment(t, h, modelID)
	before, err := st.DeploymentRequirements(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"only the 16 GiB card":  {`{"node_requirements":{"gpuworks.ai/gpu-model":["A100"]}}`, http.StatusBadRequest, ErrorDeploymentNeverFits},
		"no node at all":        {`{"node_requirements":{"pool":["nowhere"]}}`, http.StatusBadRequest, `"pool":0`},
		"another vendor":        {`{"node_requirements":{"gpuworks.ai/vendor":["ascend"]}}`, http.StatusBadRequest, ErrorNodeRequirementsInvalid},
		"a misspelt system key": {`{"node_requirements":{"gpuworks.ai/gpu_model":["H100"]}}`, http.StatusBadRequest, ErrorNodeRequirementsInvalid},
		"an unknown strategy":   {`{"node_requirements":{},"placement_strategy":"random"}`, http.StatusBadRequest, ErrorPlacementStrategyInvalid},
		"an unknown field":      {`{"node_requirements":{},"node_labels":{"pool":"a"}}`, http.StatusBadRequest, "unknown field"},
		"not a JSON object":     {`[]`, http.StatusBadRequest, "cannot unmarshal"},
	} {
		status, body := putRequirements(t, h, dep.ID, c.body)
		if status != c.status || !strings.Contains(body, c.code) {
			t.Errorf("%s: %d %s, want %d naming %s", name, status, body, c.status, c.code)
		}
	}
	after, err := st.DeploymentRequirements(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused edit changed the requirements: %v -> %v", before, after)
	}
	if row, _ := st.GetDeployment(dep.ID); row.PlacementStrategy != store.PlacementSpread {
		t.Fatalf("a refused edit changed the strategy to %q", row.PlacementStrategy)
	}

	if status, _ := putRequirements(t, h, 9999, `{"node_requirements":{}}`); status != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", status)
	}
	big, _ := st.NodeByUUID("auto-big")
	multi, err := st.CreateDeployment(&store.Deployment{Name: "multi", ModelID: modelID, NodeID: big.ID, Engine: "vllm", NNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := putRequirements(t, h, multi.ID, `{"node_requirements":{}}`); status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorNodeRequirementsNotEditable+`"`) || !strings.Contains(body, `"shape":"multi_node"`) {
		t.Errorf("multi-node = %d %s", status, body)
	}
	if err := st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if status, body := putRequirements(t, h, dep.ID, `{"node_requirements":{}}`); status != http.StatusConflict || !strings.Contains(body, ErrorDeploymentDeleting) {
		t.Errorf("deleting = %d %s", status, body)
	}
	rec := httptest.NewRecorder()
	Router(Deps{Store: st}).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/requirements", dep.ID), strings.NewReader(`{"node_requirements":{}}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", rec.Code)
	}
}

// An automatic draft may list the nodes its replicas are placed among: the
// evaluation selects only from the list, and the create stores the list as
// the node requirement - an automatic deployment over several nodes, not a
// pin to the one the evaluation picked.
func TestAutomaticPlacementAmongListedNodes(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	const listed = `{"gpuworks.ai/node":["auto-small","auto-labelled"]}`
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia","node_requirements":%s}`, modelID, listed))))
	var evaluated struct {
		SelectedNode *struct {
			UUID string `json:"uuid"`
		} `json:"selected_node"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &evaluated) != nil || evaluated.SelectedNode == nil || evaluated.SelectedNode.UUID != "auto-labelled" {
		t.Fatalf("evaluate = %d %s, want auto-labelled: the only listed node the model fits", rec.Code, rec.Body)
	}

	create := func(node string) (int, string) {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"name":"listed-%[1]s","served_name":"listed-%[1]s","model_id":%[2]d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","target_vendor":"nvidia","node_uuid":%[3]q,"node_requirements":%[4]s,"placement":{"tp":1},"quick_params":{}}`, node, modelID, node, listed)
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	status, body := create("auto-labelled")
	if status != http.StatusCreated || !strings.Contains(body, `"node_requirements":{"gpuworks.ai/node":["auto-labelled","auto-small"],"gpuworks.ai/vendor":["nvidia"]}`) {
		t.Fatalf("create = %d %s", status, body)
	}
	// A list of one node is still automatic: the cards the evaluation chose
	// are rechecked, not kept as a pin every replica would then contend for.
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(`{"name":"one-listed","served_name":"one-listed","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","target_vendor":"nvidia","node_uuid":"auto-labelled","replicas":2,"node_requirements":{"gpuworks.ai/node":["auto-labelled"]},"placement":{"tp":1,"gpus":[0]},"quick_params":{}}`, modelID))))
	var one dto.Deployment
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &one) != nil {
		t.Fatalf("one-node list create = %d %s", rec.Code, rec.Body)
	}
	if stored, _ := st.GetDeployment(one.ID); stored.Placement != `{"tp":1}` {
		t.Fatalf("stored placement = %s, want the evaluated cards dropped", stored.Placement)
	}
	// A selection outside the list is a stale evaluation, not a quiet pin.
	if status, body := create("auto-big"); status != http.StatusConflict || !strings.Contains(body, ErrorAutomaticPlacementStale) {
		t.Fatalf("create outside the list = %d %s", status, body)
	}
}
