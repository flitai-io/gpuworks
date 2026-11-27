package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// twoCardFixture is one NVIDIA node with two 24 GiB cards and a 4 GiB model,
// so a deployment fits on one card and on both.
func twoCardFixture(t *testing.T) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("two-card", store.HashToken("token"), "two-card"); err != nil {
		t.Fatal(err)
	}
	card := func(index int) proto.GPUInfo {
		return proto.GPUInfo{Index: index, Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	if err := st.UpdateNodeStatus("two-card", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "two-card", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{card(0), card(1)},
	}); err != nil {
		t.Fatal(err)
	}
	// The same value the engines fixture case pins; each request below names
	// its own image, so no other engine default is left set for later cases.
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/two-card", Revision: "main", Name: "two-card", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, st, model.ID
}

func createOnTwoCards(t *testing.T, h *apiHandlers, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

func cloneOf(t *testing.T, h *apiHandlers, id uint) dto.Deployment {
	t.Helper()
	rec := httptest.NewRecorder()
	h.cloneDeployment(rec, withID(httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/deployments/%d/clone", id), strings.NewReader(`{}`)), id))
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

// oneCardManual creates a deployment on card 0 of twoCardFixture whose manual
// command states a one-card world, and answers its clone: the copy an operator
// then edits (GWKS-415).
func oneCardManualClone(t *testing.T, h *apiHandlers, modelID uint, engine, command string) dto.Deployment {
	t.Helper()
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := createOnTwoCards(t, h, fmt.Sprintf(`{"name":"one-card","model_id":%d,"engine":%q,"node_uuid":"two-card","image":"example/engine:test","placement":{"gpus":[0],"tp":1},"quick_params":{},"container_overrides":{"command_mode":"manual","cmd_display":%s}}`, modelID, engine, encoded))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, resp)
	}
	var dep dto.Deployment
	if err := json.Unmarshal([]byte(resp), &dep); err != nil {
		t.Fatal(err)
	}
	return cloneOf(t, h, dep.ID)
}

// editBody is the draft's own wire JSON sent back with replacements applied
// to it, as the deploy form does.
func editBody(t *testing.T, h *apiHandlers, id uint, replacements ...string) string {
	t.Helper()
	_, raw := getDraft(t, h, id)
	var draft struct {
		Deployment json.RawMessage `json:"deployment"`
		Revision   string          `json:"revision"`
	}
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		t.Fatal(err)
	}
	deployment := string(draft.Deployment)
	for i := 0; i+1 < len(replacements); i += 2 {
		if !strings.Contains(deployment, replacements[i]) {
			t.Fatalf("draft lacks %s\n%s", replacements[i], deployment)
		}
		deployment = strings.Replace(deployment, replacements[i], replacements[i+1], 1)
	}
	return fmt.Sprintf(`{"deployment":%s,"base_revision":%q}`, deployment, draft.Revision)
}

func wantJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%v: %s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}

const (
	sglangOneCardScript = "exec python -m sglang.launch_server --model-path /model --tp-size 1 --host 127.0.0.1 --port 30000"
	vllmOneCardScript   = "exec vllm serve /model --tensor-parallel-size 1 --port 8000"
)

// GWKS-415: a one-card deployment whose manual command states --tp-size 1 is
// cloned and the clone's placement edited to two cards. The engine would
// still start on one card and leave the other assigned and idle, so the edit
// is refused with the count the command states, in a dry run and in a write,
// and nothing is stored.
func TestEditRefusesAManualCommandThatLeavesAssignedGPUsIdle(t *testing.T) {
	for name, test := range map[string]struct{ engine, command, detail string }{
		"sglang script": {"sglang", sglangOneCardScript, "sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"},
		"sglang argv":   {"sglang", `["python","-m","sglang.launch_server","--model-path","/model","--tp-size","1"]`, "sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"},
		"vllm script":   {"vllm", vllmOneCardScript, "vllm topology: tensor parallel 1 × pipeline parallel 1 requires 1 GPUs, but placement assigned 2"},
	} {
		t.Run(name, func(t *testing.T) {
			h, st, modelID := twoCardFixture(t)
			clone := oneCardManualClone(t, h, modelID, test.engine, test.command)
			before, err := st.GetDeployment(clone.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Placement != `{"gpus":[0],"tp":1}` {
				t.Fatalf("the clone's placement = %s, want the original's", before.Placement)
			}
			body := editBody(t, h, clone.ID, `"placement":{"gpus":[0],"tp":1}`, `"placement":{"gpus":[0,1],"tp":2}`)
			want := fmt.Sprintf(`{"code":"BAD_REQUEST","error":"deployment %d resolve launch: resolve launch: %s"}`, clone.ID, test.detail)
			for _, dryRun := range []bool{true, false} {
				status, resp := putEdit(t, h, clone.ID, body, dryRun)
				if status != http.StatusBadRequest {
					t.Fatalf("edit (dry run %v) = %d %s", dryRun, status, resp)
				}
				wantJSON(t, resp, want)
			}
			after, err := st.GetDeployment(clone.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Placement != before.Placement || after.ContainerOverrides != before.ContainerOverrides || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("a refused edit wrote the deployment: %+v", after)
			}
		})
	}
}

// The same edit with the command corrected to the two cards is written, and
// so is one of a deployment whose command the platform assembles.
func TestEditToMoreGPUsKeepsAConsistentCommand(t *testing.T) {
	t.Run("manual command stating both cards", func(t *testing.T) {
		h, st, modelID := twoCardFixture(t)
		clone := oneCardManualClone(t, h, modelID, "sglang", sglangOneCardScript)
		body := editBody(t, h, clone.ID,
			`"placement":{"gpus":[0],"tp":1}`, `"placement":{"gpus":[0,1],"tp":2}`,
			`--tp-size 1`, `--tp-size 2`)
		status, resp := putEdit(t, h, clone.ID, body, false)
		if status != http.StatusOK {
			t.Fatalf("edit = %d %s", status, resp)
		}
		for _, want := range []string{
			`{"field":"placement.gpus","layer":"restart","from":"[0]","to":"[0,1]"}`,
			`{"field":"placement.tp","layer":"restart","from":"1","to":"2"}`,
		} {
			if !strings.Contains(resp, want) {
				t.Fatalf("plan lacks %s\n%s", want, resp)
			}
		}
		row, err := st.GetDeployment(clone.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Placement != `{"gpus":[0,1],"tp":2}` || !strings.Contains(row.ContainerOverrides, `--tp-size 2`) {
			t.Fatalf("stored placement=%s overrides=%s", row.Placement, row.ContainerOverrides)
		}
	})
	t.Run("engine command", func(t *testing.T) {
		h, st, modelID := twoCardFixture(t)
		code, resp := createOnTwoCards(t, h, fmt.Sprintf(`{"name":"engine-mode","model_id":%d,"engine":"sglang","node_uuid":"two-card","image":"example/engine:test","placement":{"gpus":[0],"tp":1},"quick_params":{}}`, modelID))
		if code != http.StatusCreated {
			t.Fatalf("create = %d %s", code, resp)
		}
		var dep dto.Deployment
		if err := json.Unmarshal([]byte(resp), &dep); err != nil {
			t.Fatal(err)
		}
		status, resp := putEdit(t, h, dep.ID, editBody(t, h, dep.ID, `"placement":{"gpus":[0],"tp":1}`, `"placement":{"gpus":[0,1],"tp":2}`), false)
		if status != http.StatusOK {
			t.Fatalf("edit = %d %s", status, resp)
		}
		if row, _ := st.GetDeployment(dep.ID); row.Placement != `{"gpus":[0,1],"tp":2}` {
			t.Fatalf("stored placement=%s", row.Placement)
		}
	})
}

// A deployment an earlier build stored with two cards and a one-card script
// stays editable where the edit touches neither its launch nor its placement;
// an edit that changes the launch is judged whole and names the mismatch.
func TestEditOfAnAlreadyMismatchedDeployment(t *testing.T) {
	h, st, modelID := twoCardFixture(t)
	clone := oneCardManualClone(t, h, modelID, "sglang", sglangOneCardScript)
	read, err := st.GetDeployment(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := st.DeploymentRequirements(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *read
	stale.Placement = `{"gpus":[0,1],"tp":2}`
	if err := st.EditDeployment(read, reqs, store.DeploymentEdit{Next: &stale, Apply: store.EditApplyNone}); err != nil {
		t.Fatal(err)
	}

	status, resp := putEdit(t, h, clone.ID, editBody(t, h, clone.ID, `"routing_policy":"least_request"`, `"routing_policy":"failover"`), false)
	if status != http.StatusOK {
		t.Fatalf("routing edit = %d %s", status, resp)
	}
	if row, _ := st.GetDeployment(clone.ID); row.RoutingPolicy != "failover" {
		t.Fatalf("routing policy = %s", row.RoutingPolicy)
	}

	status, resp = putEdit(t, h, clone.ID, editBody(t, h, clone.ID, `--port 30000`, `--port 30000 --context-length 4096`), false)
	if status != http.StatusBadRequest {
		t.Fatalf("launch edit = %d %s", status, resp)
	}
	wantJSON(t, resp, fmt.Sprintf(`{"code":"BAD_REQUEST","error":"deployment %d resolve launch: resolve launch: sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"}`, clone.ID))
}

// Create and evaluate hold a manual script to the same rule, so the form
// shows the mismatch before anything is stored.
func TestCreateAndEvaluateRefuseAManualScriptThatLeavesAssignedGPUsIdle(t *testing.T) {
	h, st, modelID := twoCardFixture(t)
	request := fmt.Sprintf(`{"name":"two-cards","model_id":%d,"engine":"sglang","node_uuid":"two-card","image":"example/engine:test","placement":{"gpus":[0,1],"tp":2},"quick_params":{},"container_overrides":{"command_mode":"manual","cmd_display":%q}}`, modelID, sglangOneCardScript)
	code, resp := createOnTwoCards(t, h, request)
	if code != http.StatusBadRequest {
		t.Fatalf("create = %d %s", code, resp)
	}
	wantJSON(t, resp, `{"code":"BAD_REQUEST","error":"resolve launch: sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"}`)
	if rows, err := st.ListDeployments(); err != nil || len(rows) != 0 {
		t.Fatalf("a refused create stored %d deployments (%v)", len(rows), err)
	}

	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(request)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	if want := `"ok":false`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("evaluate lacks %s\n%s", want, rec.Body)
	}
	// A preview has no deployment yet, so its error names none (GWKS-522).
	if want := `"errors":[{"field":"","code":"FIELD_INVALID","params":{"detail":"resolve launch: resolve launch: sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"}}]`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("evaluate lacks %s\n%s", want, rec.Body)
	}
}
