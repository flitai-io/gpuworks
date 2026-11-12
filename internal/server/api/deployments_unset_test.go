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

func TestDeployRequestQuickParamsUnsetUsesLiteralWireContract(t *testing.T) {
	const wire = `{"model_id":3,"node_uuid":"node-1","quick_params":{"max_model_len":8192},"quick_params_unset":["kv_offloading_size","kv_disk_offload"],"extra_args":"","placement":{}}`
	var request dto.DeployRequest
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatal(err)
	}
	want := []string{"kv_offloading_size", "kv_disk_offload"}
	if len(request.QuickParamsUnset) != len(want) || request.QuickParamsUnset[0] != want[0] || request.QuickParamsUnset[1] != want[1] {
		t.Fatalf("quick_params_unset = %#v, want %#v", request.QuickParamsUnset, want)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	unset, ok := object["quick_params_unset"].([]any)
	if !ok || len(unset) != 2 || unset[0] != want[0] || unset[1] != want[1] {
		t.Fatalf("round-trip object = %#v", object)
	}
	if _, leaked := object["QuickParamsUnset"]; leaked {
		t.Fatalf("Go field name leaked to wire: %#v", object)
	}
}

func TestLaunchConfigUnsetConflictCodeMatchesItsProducer(t *testing.T) {
	if ErrorLaunchConfigUnsetConflict != deploy.ErrorLaunchConfigUnsetConflict {
		t.Fatalf("api code %q, deploy code %q", ErrorLaunchConfigUnsetConflict, deploy.ErrorLaunchConfigUnsetConflict)
	}
}

func TestEvaluateCreateAndStoredTemplateSnapshotHonorKVTierUnset(t *testing.T) {
	st, err := store.Open("file:launch_config_kv_unset?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	connector := `{"kv_connector":"OffloadingConnector","kv_role":"kv_both","kv_connector_extra_config":{"spec_name":"TieringOffloadingSpec","cpu_bytes_to_use":34359738368,"secondary_tiers":[{"type":"fs","root_dir":"/mnt/template-kv","n_read_threads":8,"n_write_threads":12}]}}`
	args, err := json.Marshal([]string{"--kv-offloading-size", "32", "--kv-transfer-config", connector, "--max-model-len", "8192", "--load-format", "safetensors"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := store.InferenceModel{
		ModelID: "acme/kv-unset", DisplayTitle: "KV unset", Source: "upstream", Architecture: "llama", ContextLength: 8192,
		Specs: []store.InferenceSpecRow{{
			Name: "kv-template", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16", WeightBytes: 4 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/KV-Unset"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=9.0"}`,
			Args:          string(args),
		}},
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/KV-Unset", Revision: "main", Name: "Acme/KV-Unset", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "kv-unset-node")
	handler := &apiHandlers{d: Deps{Store: st}}
	request := dto.DeployRequest{
		Name: "kv-unset", TemplateID: template.ID, TemplateSpecSource: "upstream", TemplateSpecName: "kv-template",
		ModelID: model.ID, NodeUUID: "kv-unset-node", QuickParams: map[string]any{},
		QuickParamsUnset: []string{"kv_offloading_size", "kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads", "kv_disk_limit_gib"},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	evaluation := httptest.NewRecorder()
	handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(body))))
	if evaluation.Code != http.StatusOK {
		t.Fatalf("evaluate = %d: %s", evaluation.Code, evaluation.Body.String())
	}
	var preview dto.EvaluateResponse
	if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.OK {
		t.Fatalf("evaluate response = %s", evaluation.Body.String())
	}
	command := strings.Join(append(append([]string(nil), preview.Entrypoint...), preview.Argv...), " ")
	if strings.Contains(command, "--kv-offloading-size") || strings.Contains(command, "--kv-transfer-config") || strings.Contains(fmt.Sprint(preview.EffectiveParams), "kv_offload") {
		t.Fatalf("disabled KV tier reached preview: argv=%s params=%v", command, preview.EffectiveParams)
	}
	for _, mount := range preview.Mounts {
		if mount.Purpose == proto.MountPurposeKVDisk {
			t.Fatalf("disabled KV tier reached preview mounts: %+v", preview.Mounts)
		}
	}

	created := httptest.NewRecorder()
	handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(body))))
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments = %+v, error = %v", deployments, err)
	}
	stored := deployments[0]
	if strings.Contains(stored.QuickParams, "kv_offload") || strings.Contains(stored.ExtraArgs, "kv-") {
		t.Fatalf("request tombstone leaked into stored config: quick=%s extra=%q", stored.QuickParams, stored.ExtraArgs)
	}
	var snapshot struct {
		ExtraArgs []string `json:"extra_args"`
	}
	if err := json.Unmarshal([]byte(stored.CatalogSelectionSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	joinedSnapshot := strings.Join(snapshot.ExtraArgs, " ")
	if strings.Contains(joinedSnapshot, "--kv-offloading-size") || strings.Contains(joinedSnapshot, "--kv-transfer-config") {
		t.Fatalf("controller template snapshot can revive disabled KV tier: %v", snapshot.ExtraArgs)
	}
	if !strings.Contains(joinedSnapshot, "--load-format") {
		t.Fatalf("unrelated template residual disappeared: %v", snapshot.ExtraArgs)
	}
}
