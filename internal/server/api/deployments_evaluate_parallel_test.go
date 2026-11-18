package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type parallelSummary struct {
	TP          int `json:"tp"`
	PP          int `json:"pp"`
	Nodes       int `json:"nodes"`
	GPUsPerNode int `json:"gpus_per_node"`
}

type parallelEvaluation struct {
	OK     bool `json:"ok"`
	Errors []struct {
		Field  string         `json:"field"`
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	} `json:"errors"`
	Acquisition *struct {
		Nodes []map[string]any `json:"nodes"`

		Components []dto.AcquisitionComponent `json:"components"`
	} `json:"acquisition"`
	FormValues  map[string]any   `json:"form_values"`
	Parallelism *parallelSummary `json:"parallelism"`
	Plan        json.RawMessage  `json:"plan"`
	Warnings    json.RawMessage  `json:"warnings"`
	Argv        []string         `json:"argv"`
}

// newParallelEvaluate is an evaluate handler over two online nodes with two
// GPUs each, the shape the multi-node form starts from.
func newParallelEvaluate(t *testing.T) (func(string) parallelEvaluation, uint) {
	t.Helper()
	evaluate, modelID, _ := newParallelEvaluateWith(t, nil)
	return evaluate, modelID
}

// newParallelEvaluateWith lets a test add dependencies to the handler.
func newParallelEvaluateWith(t *testing.T, extend func(*Deps)) (func(string) parallelEvaluation, uint, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/parallel", Revision: "main", Name: "parallel", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"par-a", "par-b"} {
		launchConfigNode(t, st, uuid)
		if err := st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	deps := Deps{Store: st}
	if extend != nil {
		extend(&deps)
	}
	h := &apiHandlers{d: deps}
	return func(body string) parallelEvaluation {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var out parallelEvaluation
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}, model.ID, st
}

func TestEvaluateDefaultsMultiNodeParallelism(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"]}`, modelID))
	if !out.OK || len(out.Errors) != 0 || out.Acquisition == nil {
		t.Fatalf("default parallelism = ok %v errors %+v", out.OK, out.Errors)
	}
	if out.FormValues["tp"] != nil || out.FormValues["pp"] != nil {
		t.Fatalf("form_values = %v", out.FormValues)
	}
	if want := (parallelSummary{TP: 1, PP: 2, Nodes: 2, GPUsPerNode: 1}); out.Parallelism == nil || *out.Parallelism != want {
		t.Fatalf("parallelism = %+v, want %+v", out.Parallelism, want)
	}
	if !strings.Contains(strings.Join(out.Argv, " "), "--pipeline-parallel-size 2") {
		t.Fatalf("argv = %v", out.Argv)
	}
	// A node-list error still stops the preview: acquisition needs the nodes.
	out = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-a"],"placement":{"tp":2}}`, modelID))
	if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != "node_uuids" || out.Errors[0].Code != ErrorDistributedDuplicateNode {
		t.Fatalf("duplicate node errors = %+v", out.Errors)
	}
}

func TestMultiNodeRecommendationChangesWithModelSize(t *testing.T) {
	evaluate, smallID, st := newParallelEvaluateWith(t, nil)
	large, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/large", Revision: "main", Name: "large", WeightBytes: 170 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    uint
		cards int
	}{{smallID, 1}, {large.ID, 2}} {
		out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"]}`, tc.id))
		if !out.OK || out.Parallelism == nil || out.Parallelism.GPUsPerNode != tc.cards {
			t.Fatalf("model %d: want %d GPUs per node, got %+v errors %v", tc.id, tc.cards, out.Parallelism, out.Errors)
		}
	}
}

// A single-node draft states no TP: the summary follows the schedule.
func TestEvaluateSingleNodeParallelismFollowsTheSchedule(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"par-a"}`, modelID))
	if want := (parallelSummary{TP: 1, PP: 1, Nodes: 1, GPUsPerNode: 1}); !out.OK || out.Parallelism == nil || *out.Parallelism != want {
		t.Fatalf("single node = ok %v errors %+v parallelism %+v", out.OK, out.Errors, out.Parallelism)
	}
	if _, stated := out.FormValues["tp"]; stated {
		t.Fatalf("a single-node draft keeps TP automatic: %v", out.FormValues)
	}
	out = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"par-a","placement":{"tp":1,"pp":2}}`, modelID))
	if want := (parallelSummary{TP: 1, PP: 2, Nodes: 1, GPUsPerNode: 2}); !out.OK || out.Parallelism == nil || *out.Parallelism != want ||
		!strings.Contains(strings.Join(out.Argv, " "), "--tensor-parallel-size 1 --pipeline-parallel-size 2") {
		t.Fatalf("single node PP 2 = ok %v errors %+v parallelism %+v argv %v", out.OK, out.Errors, out.Parallelism, out.Argv)
	}
}

// TP and PP are the deployment's own degrees; each node runs TP×PP ÷ nodes.
func TestEvaluateGlobalParallelism(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	body := func(placement string) string {
		return fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"],"placement":%s}`, modelID, placement)
	}
	argv := func(out parallelEvaluation) string { return strings.Join(out.Argv, " ") }

	// TP 2 spans the nodes, one GPU each.
	out := evaluate(body(`{"tp":2}`))
	if !out.OK || !strings.Contains(argv(out), "--tensor-parallel-size 2") || strings.Contains(argv(out), "--pipeline-parallel-size") {
		t.Fatalf("TP 2 = ok %v errors %+v argv %q", out.OK, out.Errors, argv(out))
	}
	if want := (parallelSummary{TP: 2, PP: 1, Nodes: 2, GPUsPerNode: 1}); out.Parallelism == nil || *out.Parallelism != want {
		t.Fatalf("TP 2 parallelism = %+v", out.Parallelism)
	}
	// TP 2 × PP 2 fills both GPUs of each node.
	out = evaluate(body(`{"tp":2,"pp":2}`))
	if !out.OK || !strings.Contains(argv(out), "--tensor-parallel-size 2 --pipeline-parallel-size 2") {
		t.Fatalf("TP 2 × PP 2 = ok %v errors %+v argv %q plan %s warnings %s", out.OK, out.Errors, argv(out), out.Plan, out.Warnings)
	}
	// TP 8 needs four GPUs per node.
	out = evaluate(body(`{"tp":8}`))
	if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != "placement.tp" || out.Errors[0].Code != ErrorDistributedGPUCount ||
		out.Errors[0].Params["required"] != float64(4) || out.Errors[0].Params["available"] != float64(2) || out.Acquisition == nil {
		t.Fatalf("TP 8 = %+v acquisition %v", out.Errors, out.Acquisition != nil)
	}
	// The degrees the deployer typed stay on the form while they are wrong.
	out = evaluate(body(`{"tp":8,"pp":2}`))
	if out.OK || out.FormValues["tp"] != float64(8) || out.FormValues["pp"] != float64(2) {
		t.Fatalf("TP 8 × PP 2 form_values = %v errors %+v", out.FormValues, out.Errors)
	}
	// Three ranks cannot be shared by two nodes.
	out = evaluate(body(`{"tp":3}`))
	if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != "placement.pp" || out.Errors[0].Code != ErrorDistributedParallelIndivisible ||
		out.Errors[0].Params["tp"] != float64(3) || out.Errors[0].Params["pp"] != float64(1) || out.Errors[0].Params["nodes"] != float64(2) || out.Acquisition == nil {
		t.Fatalf("TP 3 = %+v", out.Errors)
	}
}

// Each node of a multi-node draft reports its own image and weights; the
// node lookup answers per node, as the agent RPC does.
func TestEvaluateReportsAcquisitionPerNode(t *testing.T) {
	var asked []string
	evaluate, modelID, st := newParallelEvaluateWith(t, func(d *Deps) {
		d.ImageExists = func(_ context.Context, ref, nodeUUID string) (bool, error) {
			asked = append(asked, nodeUUID+" "+ref)
			return nodeUUID == "par-a", nil
		}
	})
	a, _ := st.NodeByUUID("par-a")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: modelID, NodeID: a.ID, State: proto.MFPresent, Path: "/models/parallel"}); err != nil {
		t.Fatal(err)
	}
	out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","nnodes":2,"node_uuids":["par-a","par-b"],"placement":{"tp":8}}`, modelID))
	// The parallelism does not fit, and the per-node facts still come back.
	if out.OK || out.Acquisition == nil || len(out.Acquisition.Nodes) != 2 {
		t.Fatalf("acquisition = %+v errors %+v", out.Acquisition, out.Errors)
	}
	jsonEqual(t, out.Acquisition.Nodes, `[
		{"node_uuid":"par-a","node_name":"par-a","image_cached":true,"image_plan":"present","weights_present":true,"weights_plan":"present"},
		{"node_uuid":"par-b","node_name":"par-b","image_cached":false,"image_plan":"pull","image_pull_refs":["example/vllm:test"],"image_pull_ranked":true,
		 "weights_present":false,"weights_plan":"copy","weights_copy_from":"par-a"}
	]`)
	if !slices.Contains(asked, "par-b example/vllm:test") {
		t.Fatalf("image lookups = %v", asked)
	}

	// A single named node (a manual placement) is forecast on its own.
	out = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"par-b"}`, modelID))
	if out.Acquisition == nil {
		t.Fatal("no acquisition for a single node draft")
	}
	jsonEqual(t, out.Acquisition.Nodes, `[
		{"node_uuid":"par-b","node_name":"par-b","image_cached":false,"image_plan":"pull","image_pull_refs":["example/vllm:test"],"image_pull_ranked":true,
		 "weights_present":false,"weights_plan":"copy","weights_copy_from":"par-a"}
	]`)
}

func TestRouterEvaluateTwoNodeDataParallel(t *testing.T) {
	_, modelID, st := newParallelEvaluateWith(t, nil)
	signer, err := auth.NewTokenSigner("parallel-dp-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer})
	for _, tc := range []struct {
		name, suffix string
		ok           bool
	}{
		{"default DP1", "", false},
		{"quick DP2", `,"quick_params":{"data_parallel_size":2}`, true},
		{"explicit DP2", `,"extra_args":"--data-parallel-size 2"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"],"placement":{"tp":1,"pp":1}%s}`, modelID, tc.suffix)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			var out parallelEvaluation
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.OK != tc.ok {
				t.Fatalf("ok = %v, want %v; errors = %+v", out.OK, tc.ok, out.Errors)
			}
			if !tc.ok {
				if len(out.Errors) != 1 || out.Errors[0].Code != ErrorDistributedParallelIndivisible {
					t.Fatalf("DP1 should not split one rank: %+v", out.Errors)
				}
				return
			}
			if len(out.Errors) != 0 || out.Parallelism == nil || out.Parallelism.GPUsPerNode != 1 {
				t.Fatalf("DP2 parallelism = %+v, errors = %+v", out.Parallelism, out.Errors)
			}
			if !strings.Contains(strings.Join(out.Argv, " "), "--data-parallel-size 2") {
				t.Fatalf("DP2 command = %q", out.Argv)
			}
		})
	}
}

// A follower with three occupied GPUs is the limiting node. If its last free
// cards disappear, fixed creation may still queue where a recommendation fails.
func TestDistributedRecommendationAndFixedCreateUnderContention(t *testing.T) {
	evaluate, modelID, st := newParallelEvaluateWith(t, nil)
	for i, uuid := range []string{"par-a", "par-b"} {
		status := nodeStatusTwoGPUs(uuid, i)
		card := status.GPUs[0]
		for index := 2; index < 8; index++ {
			card.Index = index
			status.GPUs = append(status.GPUs, card)
		}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.NodeByUUID("par-b")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := st.CreateDeployment(&store.Deployment{Name: "holder", ModelID: modelID, NodeID: n.ID, Engine: "vllm", State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	occupy := func(id, indexes string) {
		_, err := st.CreateInstance(&store.Instance{UUID: id, DeploymentID: holder.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: indexes, NNodes: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	occupy("first", "[0,1,2]")
	body := func(name, placement string) string {
		return fmt.Sprintf(`{"model_id":%d,"name":%q,"served_name":%q,"engine":"vllm","image":"example/vllm:test","nnodes":2,"node_uuids":["par-a","par-b"]%s}`, modelID, name, name, placement)
	}
	auto := evaluate(body("automatic", ""))
	if !auto.OK || auto.Parallelism == nil || auto.Parallelism.GPUsPerNode != 1 {
		t.Fatalf("automatic: %+v", auto)
	}
	// Direct API creation may omit both fields, without a prior UI submit.
	create := func(body string) *httptest.ResponseRecorder {
		h := apiHandlers{d: Deps{Store: st}}
		w := httptest.NewRecorder()
		h.createDeployment(w, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		return w
	}
	checkCreated := func(w *httptest.ResponseRecorder, tp, pp int) {
		t.Helper()
		var dep struct{ ID uint }
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &dep) != nil {
			t.Fatalf("create %d: %s", w.Code, w.Body.String())
		}
		row, err := st.GetDeployment(dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		var degrees struct{ TP, PP int }
		if err := json.Unmarshal([]byte(row.Placement), &degrees); err != nil || degrees.TP != tp || degrees.PP != pp {
			t.Fatalf("stored placement %s: %v", row.Placement, err)
		}
	}
	checkCreated(create(body("automatic", "")), auto.Parallelism.TP, auto.Parallelism.PP)
	occupy("rest", "[3,4,5,6,7]")
	if out := evaluate(body("no-room", "")); out.OK || out.Parallelism != nil || len(out.Errors) == 0 {
		t.Fatalf("busy follower: %+v", out)
	}
	if w := create(body("no-room", "")); w.Code == http.StatusCreated {
		t.Fatal("unavailable automatic placement created")
	}
	checkCreated(create(body("wait", `,"placement":{"tp":1,"pp":2}`)), 1, 2)
}

func TestDistributedTemplateRecommendationDoesNotRegisterWeights(t *testing.T) {
	evaluate, _, st := newParallelEvaluateWith(t, nil)
	template := imageSourcesFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{template}); err != nil {
		t.Fatal(err)
	}
	row, err := st.InferenceModelByModelID(template.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	result := evaluate(fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","template_spec_source":"upstream","engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"]}`, row.ID))
	if !result.OK || result.Parallelism == nil {
		t.Fatalf("template preview: errors=%v plan=%s", result.Errors, result.Plan)
	}
	models, err := st.ListModels("Acme/Sources-FP8")
	if err != nil || len(models) != 0 {
		t.Fatalf("preview registered artifacts: %v, %v", models, err)
	}
}

func TestDistributedRecommendationMatchesManualCommand(t *testing.T) {
	evaluate, modelID, st := newParallelEvaluateWith(t, nil)
	for _, tc := range []struct {
		name, command string
		tp, pp        int
	}{
		{"explicit PP conflicts with two ranks", "exec vllm serve /model --pipeline-parallel-size 1", 0, 0},
		{"explicit PP and engine default TP", "exec vllm serve /model --pipeline-parallel-size 2", 1, 2},
		{"explicit TP and engine default PP", "exec vllm serve /model --tensor-parallel-size 2", 2, 1},
		{"opaque wrapper", "exec custom-launcher --pp-size 2", 0, 0},
		{"external config", "exec vllm serve /model --config=/models/engine.yaml", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"verify-command","model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"]%s}`, modelID, manualOverrides(t, tc.command+" --node-rank ${GPUWORKS_NODE_RANK}"))
			out := evaluate(body)
			if tc.tp == 0 {
				if out.OK || out.Parallelism != nil || len(out.Errors) == 0 || out.Errors[0].Code != "DISTRIBUTED_PARALLELISM_UNAVAILABLE" {
					t.Fatalf("unverified topology: ok=%v degrees=%+v errors=%+v", out.OK, out.Parallelism, out.Errors)
				}
				h := apiHandlers{d: Deps{Store: st}}
				rec := httptest.NewRecorder()
				h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
				if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusBadRequest || code != "DISTRIBUTED_PARALLELISM_UNAVAILABLE" {
					t.Fatalf("direct create returned %d: %s", rec.Code, rec.Body.String())
				}
			} else if !out.OK || out.Parallelism == nil || out.Parallelism.TP != tc.tp || out.Parallelism.PP != tc.pp {
				t.Fatalf("want TP%d PP%d, got %+v errors=%+v", tc.tp, tc.pp, out.Parallelism, out.Errors)
			}
		})
	}
}

func TestDistributedRecommendationAttributesInvalidDegree(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	for _, tc := range []struct{ placement, field string }{
		{`{"tp":1,"pp":-1}`, "placement.pp"},
		{`{"tp":0,"pp":2}`, "placement.tp"},
		{`{"pp":1.5}`, "placement.pp"},
	} {
		body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"],"placement":%s}`, modelID, tc.placement)
		out := evaluate(body)
		if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != tc.field || out.Errors[0].Code != "DISTRIBUTED_DEGREE_INVALID" {
			t.Errorf("%s: want %s, got %+v", tc.placement, tc.field, out.Errors)
		}
	}
}
