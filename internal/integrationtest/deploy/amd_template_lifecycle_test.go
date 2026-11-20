//go:build linux && amd64

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestAMD395TemplateChatAndStop exercises the control plane with a reported
// gfx1151 card. The GPU prober and runtime are fakes: this does not exercise
// host sysfs/KFD, ROCm containers, or inference on AMD hardware.
// The assembled agent reports the real host OS/architecture, so this scenario
// runs on the Linux/amd64 host contract supported by the AMD implementation.
func TestAMD395TemplateChatAndStop(t *testing.T) {
	testAMDTemplateChatAndStop(t, "AMD GPU 0x1586 (gfx1151)", "gfx1151", 65536, "bf16-vllm-amd-ryzen-ai-max", false, false)
}

// A synthetic W7900 exercises catalog selection, placement and gateway routing.
func TestW7900TemplateChatAndStop(t *testing.T) {
	t.Run("0.6B", func(t *testing.T) {
		testAMDTemplateChatAndStop(t, "AMD Radeon PRO W7900", "gfx1100", 49152, "bf16-vllm-amd-gfx1100", true, false)
	})
	t.Run("8B", func(t *testing.T) {
		testAMDTemplateChatAndStop(t, "AMD Radeon PRO W7900", "gfx1100", 49152, "bf16-vllm-amd-gfx1100", true, true)
	})
}

func testAMDTemplateChatAndStop(t *testing.T, modelName, arch string, memoryMiB int, specName string, importProfile, largeModel bool) {
	t.Helper()
	env := testenv.New(t)
	healthy := true
	// The fake runtime reports Docker protocol identity for AMD admission.
	// It runs an in-process HTTP listener and never contacts a Docker daemon.
	driver := testenv.NewFakeDriver("docker")
	t.Cleanup(driver.CloseAll)
	runs := make(chan runtime.WorkloadSpec, 1)
	driver.AfterRun(func(spec runtime.WorkloadSpec) {
		if !strings.HasPrefix(spec.Name, "gpuworks-partition-probe-") {
			select {
			case runs <- spec:
			default:
			}
		}
	})
	good := env.AddNode("amd-"+arch, testenv.WithGPUList(testenv.GPUSpec{
		Model: modelName, Vendor: "amd", ArchID: arch, VRAMMB: memoryMiB, Healthy: &healthy,
	}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithManagementAddress("10.42.0.10"), testenv.WithDriver(driver), testenv.WithoutAcceleratorRuntimeInjection())
	bad := env.AddNode("amd-mismatch", testenv.WithGPUList(testenv.GPUSpec{
		Model: modelName, Vendor: "amd", ArchID: "gfx1150", VRAMMB: memoryMiB, Healthy: &healthy,
	}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithDriverName("docker"), testenv.WithoutAcceleratorRuntimeInjection())
	env.WaitNodeOnline(t, good.Name)
	env.WaitNodeOnline(t, bad.Name)

	// The built-in catalog's AMD launch spec, as an operator picks it.
	modelID, repo, revision := "qwen/qwen3-0.6b", "Qwen/Qwen3-0.6B", "c1899de289a04d12100db370d81485cdf75e47ca"
	weightBytes := int64(1503300328)
	if largeModel {
		modelID, repo, revision = "qwen/qwen3-8b", "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
		weightBytes = 16397461266
	}
	const runnerImage = "gpuworks/gpuworks-runner:rocm7.2.3-vllm0.30.0-r1"
	template, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: repo, Revision: revision,
		Name: repo, Arch: "Qwen3ForCausalLM", WeightBytes: weightBytes, Meta: template.ModelConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		wantArch := arch
		if node.UUID == bad.UUID {
			wantArch = "gfx1150"
		}
		if node.GPUVendor != "amd" || len(node.GPUs) != 1 || node.GPUs[0].Vendor != "amd" || node.GPUs[0].ArchID != wantArch {
			t.Fatalf("registered AMD node facts: %+v", node)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "amd")}); err != nil {
			t.Fatal(err)
		}
	}
	api := env.API(t)
	payload := map[string]any{"template_id": template.ID, "served_name": "amd-sim", "quick_params": map[string]any{}, "placement": map[string]any{}}
	create := func(uuid string) (*http.Response, error) {
		payload["node_uuid"] = uuid
		body, _ := json.Marshal(payload)
		return api.PostJSON("/api/v1/deployments", body)
	}
	if importProfile {
		checkCatalogRefusal(t, create, good.UUID, "no matching spec")
		data, err := os.ReadFile("../../../build/inference-templates/amd-gfx1100/qwen3.yaml")
		if err != nil {
			t.Fatal(err)
		}
		checkCatalogImport(t, api, data, http.StatusOK, "")
		if !largeModel {
			checkW7900MultiNodeRefusal(t, env, api, template.ID, model.ID, good.UUID, specName)
			invalid := bytes.ReplaceAll(data, []byte("max_nodes: 1"), []byte("max_nodes: -0.5"))
			checkCatalogImport(t, api, invalid, http.StatusBadRequest, "placement.max_nodes must be an integer")
		}
	}
	checkCatalogRefusal(t, create, bad.UUID, "no matching spec")
	created, err := create(good.UUID)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		t.Fatalf("%s status=%d body=%s", arch, created.StatusCode, body)
	}
	var deployment struct {
		ID               uint   `json:"id"`
		TemplateSpecName string `json:"template_spec_name"`
	}
	if err := json.NewDecoder(created.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ID == 0 || deployment.TemplateSpecName != specName {
		t.Fatalf("chosen template=%+v", deployment)
	}
	var workload runtime.WorkloadSpec
	select {
	case workload = <-runs:
	case <-time.After(20 * time.Second):
		t.Fatal("AMD agent did not receive workload")
	}
	var instance store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		rows, _ := env.Store().InstancesForDeployment(deployment.ID)
		if len(rows) != 1 || rows[0].State != proto.InstRunning || rows[0].APIPort == 0 {
			return false
		}
		instance = rows[0]
		return true
	})
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(desired.GPUIndexes, []int{0}) || !reflect.DeepEqual(workload.GPUIndexes, []int{0}) || workload.Image != runnerImage {
		t.Fatalf("AMD desired/workload mismatch: desired=%+v workload=%+v", desired, workload)
	}
	if importProfile {
		for name, want := range map[string]string{
			"--dtype": "bfloat16", "--max-model-len": "auto", "--performance-mode": "throughput", "--tensor-parallel-size": "1",
		} {
			if !containsArgPair(workload.Cmd, name, want) {
				t.Errorf("model %s missing %s %s in final launch", modelID, name, want)
			}
		}
		for _, flag := range []string{"--enforce-eager", "--no-enable-prefix-caching", "--max-num-seqs", "--max-num-batched-tokens"} {
			if containsString(workload.Cmd, flag) {
				t.Errorf("model %s unexpectedly set %s in final launch", modelID, flag)
			}
		}
	}
	key, err := env.Store().CreateAPIKey("amd-simulation", 0)
	if err != nil {
		t.Fatal(err)
	}
	chatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"amd-sim","messages":[{"role":"user","content":"hello"}]}`)))
	chatReq.Header.Set("Authorization", "Bearer "+key)
	chatReq.Header.Set("Content-Type", "application/json")
	chat, err := (&http.Client{Timeout: 10 * time.Second}).Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatBody, _ := io.ReadAll(chat.Body)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK || !bytes.Contains(chatBody, []byte(`"choices"`)) {
		t.Fatalf("chat status=%d body=%s", chat.StatusCode, chatBody)
	}
	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/stop", deployment.ID), ``, http.StatusOK)
	env.Eventually(t, 30*time.Second, func() bool { rows, _ := env.Store().InstancesForDeployment(deployment.ID); return len(rows) == 0 })
	env.Eventually(t, 10*time.Second, func() bool {
		status, err := driver.Status(context.Background(), instance.ContainerID)
		return err == nil && !status.Running
	})
}

func checkCatalogRefusal(t *testing.T, create func(string) (*http.Response, error), uuid, reason string) {
	t.Helper()
	response, err := create(uuid)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogResponse(t, response, http.StatusBadRequest, reason)
}

func checkCatalogResponse(t *testing.T, response *http.Response, status int, reason string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || !bytes.Contains(body, []byte(reason)) {
		t.Fatalf("status=%d, want %d with %q: %s", response.StatusCode, status, reason, body)
	}
}

func checkCatalogImport(t *testing.T, api *testenv.APIClient, data []byte, status int, reason string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/yaml")
	response, err := api.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogResponse(t, response, status, reason)
}

func checkW7900MultiNodeRefusal(t *testing.T, env *testenv.Env, api *testenv.APIClient, templateID, modelID uint, leader, specName string) {
	t.Helper()
	healthy := true
	follower := env.AddNode("w7900-follower", testenv.WithGPUList(testenv.GPUSpec{
		Model: "AMD Radeon PRO W7900", Vendor: "amd", ArchID: "gfx1100", VRAMMB: 49152, Healthy: &healthy,
	}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithDriverName("docker"),
		testenv.WithManagementAddress("10.42.0.11"), testenv.WithoutAcceleratorRuntimeInjection())
	env.WaitNodeOnline(t, follower.Name)
	request := map[string]any{
		"template_id": templateID, "template_spec_name": specName, "template_spec_source": "user",
		"node_uuid": leader, "node_uuids": []string{leader, follower.UUID}, "nnodes": 2,
		"served_name": "w7900-distributed", "placement": map[string]any{"tp": 1, "pp": 2},
	}
	body, _ := json.Marshal(request)
	for _, route := range []struct {
		path   string
		status int
	}{
		{"/api/v1/evaluate", http.StatusOK},
		{"/api/v1/deployments", http.StatusBadRequest},
	} {
		response, err := api.PostJSON(route.path, body)
		if err != nil {
			t.Fatal(err)
		}
		checkCatalogResponse(t, response, route.status, "TEMPLATE_PLACEMENT_NODE_LIMIT")
	}

	// Named automatic requests should report the same template limit.
	delete(request, "node_uuid")
	delete(request, "node_uuids")
	request["placement_mode"] = "auto"
	request["model_id"] = modelID
	body, _ = json.Marshal(request)
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogResponse(t, response, http.StatusBadRequest, "TEMPLATE_PLACEMENT_NODE_LIMIT")

	// Both W7900s have the model, so the group is considered before gfx942.
	// An unnamed request can continue to the built-in multi-node MI325X spec.
	node, err := env.Store().NodeByUUID(follower.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{
		ModelID: modelID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "w7900-follower"),
	}); err != nil {
		t.Fatal(err)
	}
	var alternatives []string
	for rank := range 2 {
		node := env.AddNode(fmt.Sprintf("mi325x-rank-%d", rank), testenv.WithGPUList(testenv.GPUSpec{
			Model: "AMD Instinct MI325X", Vendor: "amd", ArchID: "gfx942", VRAMMB: 262144, Healthy: &healthy,
		}), testenv.WithAccelerator("amd", "rocm", "7.2.3"), testenv.WithDriverName("docker"),
			testenv.WithManagementAddress(fmt.Sprintf("10.42.0.%d", 12+rank)), testenv.WithoutAcceleratorRuntimeInjection())
		env.WaitNodeOnline(t, node.Name)
		alternatives = append(alternatives, node.UUID)
	}
	delete(request, "template_spec_name")
	delete(request, "template_spec_source")
	body, _ = json.Marshal(request)
	response, err = api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("automatic create status = %d: %s", response.StatusCode, detail)
	}
	var created dto.Deployment
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.TemplateSpecName != "bf16-vllm-amd-mi300" || !reflect.DeepEqual(created.NodeUUIDs, alternatives) {
		t.Fatalf("automatic create picked %q on %v, want the MI325X pair %v", created.TemplateSpecName, created.NodeUUIDs, alternatives)
	}
}
