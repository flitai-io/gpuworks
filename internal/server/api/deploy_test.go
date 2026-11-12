package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeleteDeploymentMarksDeploymentForAsyncCleanup(t *testing.T) {
	st, err := store.Open("file:api_delete_deployment_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "delete-me", NodeID: 1, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(dep.ID), 10))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/"+strconv.FormatUint(uint64(dep.ID), 10), nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.deleteDeployment(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	updated, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "deleting" {
		t.Fatalf("state = %q, want deleting", updated.State)
	}
}

func TestEvaluateKeepsScheduleFailureOutOfWarnings(t *testing.T) {
	st, err := store.Open("file:api_evaluate_warning_dedup?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/too-large", Revision: "main", Name: "Too Large", WeightBytes: 200 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("warning-node", store.HashToken("token"), "warning-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("warning-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "warning-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA GPU", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 16 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"warning-node","engine":"vllm"}`, model.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Warnings []messages.Warning `json:"warnings"`
		Plan     scheduler.Result   `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Plan.OK || response.Plan.Message.Code != scheduler.ReasonNoFit || response.Plan.Message.Params["required_gib"] == nil {
		t.Fatalf("schedule message must use a structured reason: %+v", response.Plan)
	}
	seen := map[string]bool{}
	for _, warning := range response.Warnings {
		encoded, _ := json.Marshal(warning)
		key := string(encoded)
		if seen[key] {
			t.Fatalf("duplicate warning %q in %v", key, response.Warnings)
		}
		seen[key] = true
	}
}

func TestEvaluatePerformanceWarningsUseActualNodeCPUAndPlannedGPUs(t *testing.T) {
	st, err := store.Open("file:api_evaluate_performance_resources?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/resource-warning", Revision: "main", Name: "Resource Warning", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("resource-warning-node", store.HashToken("token"), "resource-warning-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("resource-warning-node", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "resource-warning-node", CPUCores: 2, Arch: "amd64", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{
			{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true},
			{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name        string
		placement   string
		wantWarning bool
	}{
		{name: "two planned GPUs exceed CPU budget", placement: `{"tp":2,"gpu_indexes":[0,1]}`, wantWarning: true},
		{name: "one planned GPU fits CPU budget", placement: `{"tp":1,"gpu_indexes":[1]}`, wantWarning: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"resource-warning-node","engine":"vllm","placement":%s,"quick_params":{"api_server_count":1}}`, model.ID, tt.placement)
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
			var response dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			gotWarning := false
			for _, warning := range response.Warnings {
				if warning.Code == "W10" {
					gotWarning = true
					if warning.Params["cpu_cores"] != float64(2) {
						t.Fatalf("W10 params = %#v", warning.Params)
					}
				}
			}
			if gotWarning != tt.wantWarning {
				t.Fatalf("warnings = %v, want W10=%v", response.Warnings, tt.wantWarning)
			}
		})
	}
}

func TestCloneDeploymentPreservesConfigurationWithNewIdentity(t *testing.T) {
	st, err := store.Open("file:api_clone_deployment_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/clone", Revision: "main", Name: "Clone"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, err := st.RegisterNode("clone-node-a", store.HashToken("token-a"), "clone-node-a")
	if err != nil {
		t.Fatal(err)
	}
	nodeB, err := st.RegisterNode("clone-node-b", store.HashToken("token-b"), "clone-node-b")
	if err != nil {
		t.Fatal(err)
	}
	nodeIDs, err := json.Marshal([]uint{nodeA.ID, nodeB.ID})
	if err != nil {
		t.Fatal(err)
	}
	requirements := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "rack": {"r1", "r2"}}
	original, err := st.CreateDeploymentWithRequirements(&store.Deployment{
		Name:                     "source",
		ServedName:               "source-api",
		ModelID:                  model.ID,
		TemplateID:               42,
		TemplateSpecSource:       "user",
		TemplateSpecName:         "latency-fp8",
		TemplateScenario:         "latency",
		TemplateDeviated:         true,
		CatalogSelectionSnapshot: `{"model_id":"acme/clone","spec":{"name":"latency-fp8"},"extra_args":["--template-only"]}`,
		NodeID:                   nodeA.ID,
		NodeIDs:                  string(nodeIDs),
		Engine:                   "vllm",
		Image:                    "vllm/test:1",
		QuickParams:              `{"max_model_len":8192,"speculative_algorithm":"draft_model","speculative_draft_model":"/models/draft","speculative_tokens":5}`,
		ExtraArgs:                `--foo "two words"`,
		Env:                      `{"HF_HOME":"/cache/hf","TOKENIZER_MODE":"fast"}`,
		ContainerOverrides:       `{"model_mount_path":"/weights","extra_mounts":[{"host_path":"/srv/data","container_path":"/data","read_only":true}]}`,
		Replicas:                 3,
		VRAMLimitBytes:           24 << 30,
		PartitionBackend:         "nvidia-hami",
		SMLimitPct:               67,
		Sleepable:                true,
		DesiredSleep:             2,
		Placement:                `{"tp":2,"gpus":[1,3]}`,
		PlacementStrategy:        store.PlacementBinpack,
		DistGroup:                "clone-dist-group",
		DistRole:                 "leader",
		NNodes:                   2,
		NodeRank:                 0,
		MasterAddr:               "10.20.30.40",
		MasterPort:               29617,
		State:                    store.DeployStopped,
	}, requirements)
	if err != nil {
		t.Fatal(err)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(original.ID), 10))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/1/clone", strings.NewReader(`{"name":"copy"}`)).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).cloneDeployment(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body=%d %s", rec.Code, rec.Body.String())
	}
	var got dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == original.ID || got.Name != "copy" || got.ServedName != "copy" || got.DesiredState != store.DeployActive.String() {
		t.Fatalf("clone=%+v original=%+v", got, original)
	}
	if got.TemplateID != original.TemplateID || got.TemplateSpecSource != original.TemplateSpecSource || got.TemplateSpecName != original.TemplateSpecName ||
		got.TemplateScenario != original.TemplateScenario || got.TemplateDeviated != original.TemplateDeviated || got.ModelID != original.ModelID ||
		got.Engine != original.Engine || got.Image != original.Image || got.NodeUUID != nodeA.UUID ||
		!reflect.DeepEqual(got.NodeUUIDs, []string{nodeA.UUID, nodeB.UUID}) || got.Replicas != original.Replicas || !got.Sleepable ||
		got.PartitionBackend != original.PartitionBackend || got.DistGroup != original.DistGroup || got.DistRole != original.DistRole ||
		got.NNodes != original.NNodes || got.NodeRank != original.NodeRank || got.MasterAddr != original.MasterAddr || got.MasterPort != original.MasterPort ||
		got.ExtraArgs != original.ExtraArgs {
		t.Fatalf("clone response lost projected launch configuration: clone=%+v original=%+v", got, original)
	}
	// The requirements are rows of their own; a clone that copied only the
	// deployment row would run anywhere.
	if !reflect.DeepEqual(got.NodeRequirements, requirements) || got.PlacementStrategy != "binpack" {
		t.Fatalf("clone runs elsewhere: requirements=%v strategy=%q", got.NodeRequirements, got.PlacementStrategy)
	}
	if stored, err := st.DeploymentRequirements(got.ID); err != nil || !reflect.DeepEqual(map[string][]string(stored), requirements) {
		t.Fatalf("stored clone requirements = %v, %v", stored, err)
	}
	var wantQuick map[string]any
	if err := json.Unmarshal([]byte(original.QuickParams), &wantQuick); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.QuickParams, wantQuick) {
		t.Fatalf("quick_params=%#v, want %#v", got.QuickParams, wantQuick)
	}
	var wantSnapshot any
	if err := json.Unmarshal([]byte(original.CatalogSelectionSnapshot), &wantSnapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.CatalogSelectionSnapshot, wantSnapshot) {
		t.Fatalf("catalog_selection_snapshot=%#v, want %#v", got.CatalogSelectionSnapshot, wantSnapshot)
	}

	persisted, err := st.GetDeployment(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := *original
	want.ID = persisted.ID
	want.Name = "copy"
	want.ServedName = "copy"
	want.State = store.DeployActive
	want.DesiredSleep = 0
	want.CreatedAt = persisted.CreatedAt
	want.UpdatedAt = persisted.UpdatedAt
	if !reflect.DeepEqual(*persisted, want) {
		t.Fatalf("persisted clone did not preserve every launch field\n got: %#v\nwant: %#v", *persisted, want)
	}
}

func TestForceDeleteDeploymentRemovesItImmediately(t *testing.T) {
	st, err := store.Open("file:api_force_delete_deployment_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "force-delete-me", NodeID: 1, State: "deleting"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "unreachable-instance", DeploymentID: dep.ID, NodeID: 1}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(dep.ID), 10))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/"+strconv.FormatUint(uint64(dep.ID), 10)+"?force=true", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.deleteDeployment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetDeployment(dep.ID); err != store.ErrNotFound {
		t.Fatalf("GetDeployment error = %v, want ErrNotFound", err)
	}
}

func TestListModelsIncludesPerNodeDownloadState(t *testing.T) {
	st, err := store.Open("file:api_list_models_files_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-1", store.HashToken("t"), "node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "huggingface/Qwen_Qwen3-8B-main",
		State: proto.MFDownloading, Revision: "abc123", SizeBytes: 100, Downloaded: 40,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var models []dto.Model
	if err := unmarshalList(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || len(models[0].Nodes) != 1 {
		t.Fatalf("models = %+v; want one node presence", models)
	}
	presence := models[0].Nodes[0]
	if presence.NodeUUID != node.UUID || presence.State != proto.MFDownloading || presence.Revision != "abc123" || presence.Progress != 0.4 {
		t.Fatalf("presence = %+v", presence)
	}
}

func TestDeleteModelRejectsReferencedModel(t *testing.T) {
	st, err := store.Open("file:api_delete_model?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/api-delete", Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(model.ID), 10))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/models/1", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.deleteModel(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorModelInUse+`"`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestEvaluateReturnsEngineSpecificCommandsForEveryRank(t *testing.T) {
	st, err := store.Open("file:api_distributed_evaluate_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main",
		Name: "Qwen3", Arch: "qwen3", WeightBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	for rank, uuid := range []string{"node-1", "node-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys: proto.SysInfo{Hostname: uuid, GPUVendor: "nvidia"}, Network: testManagement(fmt.Sprintf("10.0.0.%d", rank+1)),
			GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := apiHandlers{d: Deps{Store: st}}
	for _, engine := range []string{"vllm", "sglang"} {
		body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-1","node_uuids":["node-1","node-2"],"engine":%q,"nnodes":2,"placement":{"tp":2}}`, model.ID, engine)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.evaluate(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", engine, rec.Code, rec.Body.String())
		}
		var resp dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.RankCommands) != 2 || len(resp.Command) == 0 {
			t.Fatalf("%s response = %+v", engine, resp)
		}
		leader, follower := resp.RankCommands[0], resp.RankCommands[1]
		if leader.Headless || leader.Role != "leader" || !follower.Headless || follower.Role != "follower" {
			t.Fatalf("%s rank roles = %+v / %+v", engine, leader, follower)
		}
		leaderCmd, followerCmd := strings.Join(leader.Command, " "), strings.Join(follower.Command, " ")
		switch engine {
		case "vllm":
			// TP 2 spans the two one-GPU nodes; no pipeline stage is implied.
			if !strings.Contains(leaderCmd, "--tensor-parallel-size 2") || strings.Contains(leaderCmd, "--pipeline-parallel-size") || !strings.Contains(followerCmd, "--headless") {
				t.Fatalf("vllm commands = %q / %q", leaderCmd, followerCmd)
			}
			if leader.Env["VLLM_HOST_IP"] != "10.0.0.1" || follower.Env["VLLM_HOST_IP"] != "10.0.0.2" {
				t.Fatalf("vllm rank env = %+v / %+v", leader.Env, follower.Env)
			}
		case "sglang":
			if !strings.Contains(leaderCmd, "python -m sglang.launch_server") || !strings.Contains(followerCmd, "--tp-size 2") || !strings.Contains(followerCmd, "--port 0") || !strings.Contains(followerCmd, "--dist-init-addr 10.0.0.1:29500") {
				t.Fatalf("sglang commands = %q / %q", leaderCmd, followerCmd)
			}
		}
	}
}

func TestEvaluateReturnsExplicitRuntimePreviewLayersAndMounts(t *testing.T) {
	st, err := store.Open("file:api_evaluate_runtime_preview?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/preview", Revision: "main", Name: "Preview", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{ImageRef: "community/preview:v1", Source: "community", Engine: "vllm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("preview-node", store.HashToken("token"), "preview-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("preview-node", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "preview", GPUVendor: "nvidia", Framework: "cuda"}, GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	reqBody := fmt.Sprintf(`{"model_id":%d,"node_uuid":"preview-node","engine":"vllm","image":"community/preview:v1","container_overrides":{"env":{"USER_ENV":"yes"},"model_mount_path":"/model-alt","extra_mounts":[{"container_path":"/data","host_path":"/data"}]}}`, model.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var got dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CommandMode != "engine" || got.ModelMount != "/model-alt" || len(got.Mounts) != 1 || got.EnvLayers == nil {
		t.Fatalf("preview = %+v", got)
	}
	if got.EnvLayers.User["USER_ENV"] != "yes" || got.Env["USER_ENV"] != "yes" || len(got.DockerArgs) != 0 {
		t.Fatalf("env layers = %+v env=%+v", got.EnvLayers, got.Env)
	}
	if !strings.Contains(strings.Join(got.Command, " "), "/model-alt") {
		t.Fatalf("command did not follow model mount: %v", got.Command)
	}
	// A community image in the catalog is an artifact reference: the launch
	// runs the engine's command, not an image entrypoint.
	if got.Command[0] != "vllm" {
		t.Fatalf("command = %v, want the engine's", got.Command)
	}

	manualCommand := "# environment\nHF_HUB_OFFLINE=1\n\n# launch\nvllm serve /model --host 127.0.0.1"
	manualBody, err := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": "preview-node", "engine": "vllm", "image": "community/preview:v1",
		"container_overrides": map[string]any{"command_mode": "manual", "cmd_display": manualCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(manualBody))))
	if rec.Code != http.StatusOK {
		t.Fatalf("manual status/body = %d %s", rec.Code, rec.Body.String())
	}
	got = dto.EvaluateResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CommandMode != "manual" || len(got.Command) != 3 || got.Command[0] != "/bin/sh" || got.Command[1] != "-c" || got.Command[2] != "set -a\n"+manualCommand {
		t.Fatalf("manual command preview = %#v mode=%q", got.Command, got.CommandMode)
	}
}

func TestEvaluateFlatSpecWithoutNodeUsesSymbolicTP(t *testing.T) {
	st, err := store.Open("file:api_offline_flat_preview?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","engine":"vllm","node_uuid":"","quick_params":{},"placement":{}}`, template.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var got dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	command := strings.Join(got.Command, " ")
	if got.TemplateSpecSource != "upstream" || got.TemplateSpecName != "fp8-balanced" || !strings.Contains(command, "--tensor-parallel-size $TP_SIZE") {
		t.Fatalf("preview = %+v", got)
	}
}

func TestEvaluateUsesTargetVendorBeforeNodeSelection(t *testing.T) {
	st, err := store.Open("file:api_target_vendor_without_node?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/qwen", Revision: "master", Name: "Qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	const image = "catalog/ascend:v1"
	if _, err := st.UpsertImage(&store.Image{ImageRef: image, Engine: "vllm", Framework: "cann", Source: "official", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const alternateImage = "catalog/ascend:v2"
	if _, err := st.UpsertImage(&store.Image{ImageRef: alternateImage, Engine: "vllm", EngineVersion: "0.2", Framework: "cann", Source: "official", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"target_vendor":"ascend","engine":"vllm"}`, model.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var got dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Image == "" || got.Image == "vllm/vllm-openai:latest" || got.Acquisition == nil || got.Acquisition.Image.CacheReason != "node_unselected" {
		t.Fatalf("image acquisition = %+v, image=%q", got.Acquisition, got.Image)
	}
	if len(got.Acquisition.Image.Options) < 2 || got.Acquisition.Image.Options[0].Image != got.Image || got.Acquisition.Image.Options[0].Reason != "catalog_match" {
		t.Fatalf("canonical image options = %+v", got.Acquisition.Image.Options)
	}
	seenAlternate := false
	for _, option := range got.Acquisition.Image.Options {
		seenAlternate = seenAlternate || option.Image == alternateImage
	}
	if !seenAlternate {
		t.Fatalf("compatible manual image missing from options: %+v", got.Acquisition.Image.Options)
	}
}

func TestEvaluatePreviewCarriesTheSleepPlatformInjection(t *testing.T) {
	// The preview must show the launch the create would assemble: with
	// sleepable on, that includes vllm's --enable-sleep-mode and the dev-mode
	// env its sleep endpoints require; with it off, neither may appear.
	st, err := store.Open("file:api_evaluate_sleep_preview?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/qwen", Revision: "master", Name: "Qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"sleepable requested", `{"model_id":%d,"target_vendor":"nvidia","engine":"vllm","sleepable":true}`, true},
		{"sleepable omitted", `{"model_id":%d,"target_vendor":"nvidia","engine":"vllm"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(tc.body, model.ID))))
			if rec.Code != http.StatusOK {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
			var got dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			hasFlag := false
			for _, arg := range got.Command {
				hasFlag = hasFlag || arg == "--enable-sleep-mode"
			}
			if hasFlag != tc.want {
				t.Fatalf("command=%v, want sleep flag: %v", got.Command, tc.want)
			}
			if got.Env["VLLM_SERVER_DEV_MODE"] == "1" != tc.want {
				t.Fatalf("env=%v, want dev mode: %v", got.Env, tc.want)
			}
		})
	}
}

func TestEvaluateUsesCompatibleCatalogImage(t *testing.T) {
	st, err := store.Open("file:api_catalog_image_evaluate_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "Qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	// The entry must outrank the seeded catalog on the deterministic ordering
	// (driver-compatible framework version, then engine version descending),
	// so the fixture declares a newer engine version than any seed entry.
	if _, err := st.UpsertImage(&store.Image{
		Name: "Catalog CUDA 13.0", Engine: "vllm", Framework: "cuda", FrameworkVersion: "13.0", EngineVersion: "0.30.0",
		Arch: "amd64", ImageRef: "catalog/vllm:cuda-13", Source: "official", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-catalog", store.HashToken("t"), "catalog"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("node-catalog", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "catalog", Arch: "amd64", GPUVendor: "nvidia", GPUDriver: "595.99.02", Framework: "cuda", FrameworkVersion: "13.2"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-catalog","engine":"vllm"}`, model.ID)))
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var response dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Image != "catalog/vllm:cuda-13" {
		t.Fatalf("image = %q, want catalog image; response=%+v", response.Image, response)
	}
}

func TestDeployAcceptsOlderCUDARuntimeOnNewerDriver(t *testing.T) {
	st, err := store.Open("file:api_cuda_driver_backward_compatibility?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2-0.5B-Instruct", Revision: "master", Name: "Qwen2", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	const image = "gpustack/runner:cuda12.8-vllm0.17.1"
	if _, err := st.UpsertImage(&store.Image{
		Name: "vLLM CUDA 12.8", Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.8",
		Arch: "multi", ImageRef: image, Source: "official", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-cuda13", store.HashToken("t"), "cuda13"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("node-cuda13", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "cuda13", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA RTX 2000 Ada Generation", Vendor: "nvidia", ArchID: "sm_89", MemoryTotalBytes: 16 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-cuda13","engine":"vllm","image":%q,"quick_params":{"max_model_len":1024}}`, model.ID, image)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeployAllowsExplicitEngineDefaultCUDAImageOnAscendNodeAfterWarning(t *testing.T) {
	st, err := store.Open("file:api_incompatible_default_image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "Qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-ascend", store.HashToken("t"), "ascend"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("node-ascend", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "ascend", Arch: "arm64", GPUVendor: "ascend", Framework: "cann", FrameworkVersion: "8.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-ascend","engine":"vllm","image":"vllm/vllm-openai:latest"}`, model.ID)
	h := &apiHandlers{d: Deps{Store: st}}

	evaluateRec := httptest.NewRecorder()
	h.evaluate(evaluateRec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if evaluateRec.Code != http.StatusOK {
		t.Fatalf("evaluate status/body = %d %s", evaluateRec.Code, evaluateRec.Body.String())
	}
	var evaluated dto.EvaluateResponse
	if err := json.Unmarshal(evaluateRec.Body.Bytes(), &evaluated); err != nil {
		t.Fatal(err)
	}
	if !evaluated.OK || !hasWarningCode(evaluated.Warnings, messages.WarningImageEngineDefaultIncompatible) {
		t.Fatalf("explicit incompatible image should remain usable with a warning: %+v", evaluated)
	}

	createRec := httptest.NewRecorder()
	h.createDeployment(createRec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status/body = %d %s", createRec.Code, createRec.Body.String())
	}
}

func hasWarningCode(warnings []messages.Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func TestOmniQuickParamsRejectInvalidInputBeforePersistence(t *testing.T) {
	st, err := store.Open("file:api_omni_quick_params_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/omni", Revision: "main", Name: "omni"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-1", store.HashToken("t"), "node-1"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}

	createBody := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-1","engine":"vllm","quick_params":{"limit_mm_per_prompt":"{bad"}}`, model.ID)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(createBody))
	createRec := httptest.NewRecorder()
	h.createDeployment(createRec, createReq)
	if createRec.Code != http.StatusBadRequest || !strings.Contains(createRec.Body.String(), "limit_mm_per_prompt must be valid JSON") {
		t.Fatalf("create status/body = %d %s", createRec.Code, createRec.Body.String())
	}
	deps, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("invalid quick params persisted %d deployments", len(deps))
	}

	evalBody := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-1","engine":"vllm","quick_params":{"allowed_local_media_path":"relative/media"}}`, model.ID)
	evalReq := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(evalBody))
	evalRec := httptest.NewRecorder()
	h.evaluate(evalRec, evalReq)
	// Evaluation keeps the draft: the same judgement create answers with 400
	// arrives as a field error with ok=false.
	if evalRec.Code != http.StatusOK {
		t.Fatalf("evaluate status/body = %d %s", evalRec.Code, evalRec.Body.String())
	}
	var evaluated struct {
		OK     bool             `json:"ok"`
		Errors []dto.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(evalRec.Body.Bytes(), &evaluated); err != nil {
		t.Fatal(err)
	}
	if evaluated.OK || len(evaluated.Errors) != 1 {
		t.Fatalf("evaluate body = %s", evalRec.Body.String())
	}
	fieldErr := evaluated.Errors[0]
	if fieldErr.Field != "allowed_local_media_path" || fieldErr.Code != "QUICK_PARAM_PATH_INVALID" || fieldErr.Params["key"] != "allowed_local_media_path" {
		t.Fatalf("field error = %+v", fieldErr)
	}
}

// TestNodeFSListEndpoint: the agent's fs.list RPC answer ({"path",
// "entries"}, as rpcFSList writes it) reaches the client as the list
// envelope {"path", "items", "total"}; an empty directory is "items": [].
func TestNodeFSListEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, rpc, want string
	}{
		{
			name: "entries",
			rpc:  `{"path":"/models","entries":[{"name":"Qwen","is_dir":true,"size":4096},{"name":"README.md","is_dir":false,"size":12}]}`,
			want: `{"path":"/models","items":[{"name":"Qwen","is_dir":true,"size":4096},{"name":"README.md","is_dir":false,"size":12}],"total":2}`,
		},
		{
			name: "empty directory",
			rpc:  `{"path":"/models","entries":[]}`,
			want: `{"path":"/models","items":[],"total":0}`,
		},
		{
			name: "entries null",
			rpc:  `{"path":"/models","entries":null}`,
			want: `{"path":"/models","items":[],"total":0}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{
				CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
					if uuid != "node-1" || method != proto.RPCFSList {
						t.Fatalf("rpc = %q/%q", uuid, method)
					}
					if p, _ := params.(map[string]string); p["path"] != "/models" {
						t.Fatalf("rpc params = %#v", params)
					}
					return json.RawMessage(tc.rpc), nil
				},
			}}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/node-1/fs?path=/models", nil)
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-1"}))
			rec := httptest.NewRecorder()

			h.nodeFSList(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
				t.Fatalf("fs wire JSON\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRegisterModelPathEndpoint(t *testing.T) {
	st, err := store.Open("file:api_register_model_path_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-1", store.HashToken("t"), "node")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{
		Store: st,
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			if uuid != "node-1" || method != proto.RPCRegisterPath {
				t.Fatalf("rpc = %q/%q", uuid, method)
			}
			return json.RawMessage(`{"path":"/models/Qwen3","size_bytes":30,"metadata":{"entries":[{"path":".","format":"transformers"}]}}`), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/register-path", strings.NewReader(`{"node_uuid":"node-1","path":"/models/Qwen3"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "1"}))
	rec := httptest.NewRecorder()

	h.registerModelPath(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	mf, err := st.ModelFileByModelNode(model.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Path != "/models/Qwen3" || mf.State != proto.MFRegistered || mf.SizeBytes != 30 || mf.Downloaded != 30 {
		t.Fatalf("model file = %+v", mf)
	}
}

func TestRegisterModelPathEndpointRejectsInvalidDirectory(t *testing.T) {
	st, err := store.Open("file:api_register_model_path_invalid_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-1", store.HashToken("t"), "node"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{
		Store: st,
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			return nil, staticErr("model directory must contain config.json and weight files")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/register-path", strings.NewReader(`{"node_uuid":"node-1","path":"/tmp"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "1"}))
	rec := httptest.NewRecorder()

	h.registerModelPath(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPreviewLocalModelParsesMetadata(t *testing.T) {
	st, err := store.Open("file:api_preview_local_model_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-local", store.HashToken("t"), "local"); err != nil {
		t.Fatal(err)
	}
	config := `{"model_type":"qwen2","architectures":["Qwen2ForCausalLM"],"torch_dtype":"bfloat16","max_position_embeddings":32768}`
	h := apiHandlers{d: Deps{
		Store: st,
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			if uuid != "node-local" {
				t.Fatalf("uuid = %q", uuid)
			}
			switch method {
			case proto.RPCRegisterPath:
				return json.RawMessage(`{"path":"/models/Qwen2","size_bytes":123456,"metadata":{"entries":[{"path":".","format":"transformers"}],"config_raw":` + strconv.Quote(config) + `,"readme":"# Qwen2","tasks":["text-generation"]}}`), nil
			case proto.RPCFSReadFile:
				p := params.(map[string]any)
				if strings.HasSuffix(p["path"].(string), "/config.json") {
					return json.RawMessage(`{"path":"/models/Qwen2/config.json","content":` + strconv.Quote(config) + `,"size_bytes":120,"truncated":false}`), nil
				}
				return json.RawMessage(`{"path":"/models/Qwen2/README.md","content":"# Qwen2","size_bytes":600000,"truncated":true}`), nil
			default:
				t.Fatalf("unexpected rpc method %q", method)
				return nil, nil
			}
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/local/preview", strings.NewReader(`{"node_uuid":"node-local","path":"/models/Qwen2"}`))
	rec := httptest.NewRecorder()
	h.previewLocalModel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got localModelPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceID != "Qwen2" || got.Arch != "Qwen2ForCausalLM" || got.Precision != "bfloat16" || got.ContextLength != 32768 || got.WeightBytes != 123456 {
		t.Fatalf("preview = %+v", got)
	}
	if len(got.Operations) != 1 || got.Operations[0].Type != "openai.chat" || len(got.InputModalities) != 1 || got.InputModalities[0] != "text" {
		t.Fatalf("inference config = operations %+v modalities %v", got.Operations, got.InputModalities)
	}
	if got.README != "# Qwen2" || got.READMETruncated {
		t.Fatalf("readme = %q truncated=%v", got.README, got.READMETruncated)
	}
}

func TestPreviewLocalModelRejectsInvalidConfigAndMissingWeights(t *testing.T) {
	tests := []struct {
		name        string
		registerErr bool
		readConfig  string
		want        string
	}{
		{name: "invalid config", readConfig: "{not-json", want: "parse config.json"},
		{name: "missing weights", registerErr: true, want: "model directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open("file:api_preview_local_invalid_"+strings.ReplaceAll(tt.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.RegisterNode("node-local", store.HashToken("t"), "local"); err != nil {
				t.Fatal(err)
			}
			h := apiHandlers{d: Deps{
				Store: st,
				CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
					if method == proto.RPCRegisterPath && tt.registerErr {
						return nil, staticErr("model directory must contain config.json and weight files")
					}
					if method == proto.RPCRegisterPath {
						return nil, staticErr("parse config.json: invalid model metadata")
					}
					return json.RawMessage(`{"path":"/models/Qwen2/config.json","content":` + strconv.Quote(tt.readConfig) + `,"size_bytes":10,"truncated":false}`), nil
				},
			}}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/models/local/preview", strings.NewReader(`{"node_uuid":"node-local","path":"/models/Qwen2"}`))
			rec := httptest.NewRecorder()
			h.previewLocalModel(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
