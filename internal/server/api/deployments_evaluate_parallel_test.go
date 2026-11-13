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

// A multi-node draft that states no parallelism gets the default from its
// nodes: a pipeline stage per node, TP over the fewest GPUs. The form shows
// it through form_values and the parallelism summary.
func TestEvaluateDefaultsMultiNodeParallelism(t *testing.T) {
	evaluate, modelID := newParallelEvaluate(t)
	out := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-b"]}`, modelID))
	if !out.OK || len(out.Errors) != 0 || out.Acquisition == nil {
		t.Fatalf("default parallelism = ok %v errors %+v", out.OK, out.Errors)
	}
	if out.FormValues["tp"] != float64(2) || out.FormValues["pp"] != float64(2) {
		t.Fatalf("form_values = %v", out.FormValues)
	}
	if want := (parallelSummary{TP: 2, PP: 2, Nodes: 2, GPUsPerNode: 2}); out.Parallelism == nil || *out.Parallelism != want {
		t.Fatalf("parallelism = %+v, want %+v", out.Parallelism, want)
	}
	if !strings.Contains(strings.Join(out.Argv, " "), "--tensor-parallel-size 2 --pipeline-parallel-size 2") {
		t.Fatalf("argv = %v", out.Argv)
	}
	// A node-list error still stops the preview: acquisition needs the nodes.
	out = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["par-a","par-a"],"placement":{"tp":2}}`, modelID))
	if out.OK || len(out.Errors) != 1 || out.Errors[0].Field != "node_uuids" || out.Errors[0].Code != ErrorDistributedDuplicateNode {
		t.Fatalf("duplicate node errors = %+v", out.Errors)
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
