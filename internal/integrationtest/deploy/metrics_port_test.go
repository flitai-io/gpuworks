package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A callable template that declares api.metrics.port: separate gets a second
// port reserved with the API port, end to end: the evaluation shows the command
// with a placeholder for it, the created deployment launches with the reserved
// port in its command, and the stored desired state, the instance's reserved
// ports and the workload the agent runs all agree on it. A template that
// references the variable without declaring the port never becomes a package.
func TestSeparateMetricsPortReachesPreviewLaunchAndDesiredState(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("metrics-port-node",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}),
		testenv.WithDriverName("docker"),
	)
	runs := make(chan runtime.WorkloadSpec, 1)
	node.Driver.AfterRun(func(workload runtime.WorkloadSpec) {
		select {
		case runs <- workload:
		default:
		}
	})
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}

	const modelID = "gpuworks/metrics-port"
	const revision = "0123456789abcdef0123456789abcdef01234567"
	weight, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: modelID, Revision: revision, Name: modelID, Arch: "FakeForCausalLM", WeightBytes: 1 << 20, Meta: `{"model_type":"fake","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: weight.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "metrics-port")}); err != nil {
		t.Fatal(err)
	}
	spec := func(args []string, metrics *inferenceapi.Metrics) inferencecatalog.InferenceModel {
		return inferencecatalog.InferenceModel{ModelID: modelID, Source: "user", Specs: []inferencecatalog.InferenceSpec{{
			Name: "own-metrics", Engine: "callable", Image: "example/launcher:test",
			Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID, Revision: revision}}},
			WeightBytes: 1 << 20, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
			Entrypoint: []string{"/opt/launcher"}, Args: args,
			API: &inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}, Metrics: metrics},
		}}}
	}
	pack := func(model inferencecatalog.InferenceModel) (*bytes.Buffer, error) {
		packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, nil)
		if err != nil {
			return nil, err
		}
		var archive bytes.Buffer
		return &archive, inferencecatalog.PackPackage(&archive, packageFS)
	}
	importModel := func(model inferencecatalog.InferenceModel) *http.Response {
		t.Helper()
		archive, err := pack(model)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPost, env.URL()+"/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes()))
		request.Header.Set("Content-Type", "application/gzip")
		response, err := env.API(t).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	// Referencing the variable without declaring the port never becomes a package.
	_, err = pack(spec([]string{"--port", "${GPUWORKS_API_PORT}", "--metrics-port", "${GPUWORKS_METRICS_PORT}"}, nil))
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorCatalogMetricsPortUndeclared {
		t.Fatalf("undeclared reference: err=%v, want %s", err, messages.ErrorCatalogMetricsPortUndeclared)
	}

	accepted := importModel(spec([]string{"--port", "${GPUWORKS_API_PORT}", "--metrics-port", "${GPUWORKS_METRICS_PORT}"}, &inferenceapi.Metrics{Port: "separate"}))
	if accepted.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(accepted.Body)
		accepted.Body.Close()
		t.Fatalf("import status/body=%d %s", accepted.StatusCode, body)
	}
	accepted.Body.Close()
	template, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}

	request, _ := json.Marshal(map[string]any{"template_id": template.ID, "template_spec_name": "own-metrics", "node_uuid": nodes[0].UUID, "served_name": "own-metrics"})
	api := env.API(t)
	evaluated, err := api.PostJSON("/api/v1/evaluate", request)
	if err != nil {
		t.Fatal(err)
	}
	var preview dto.EvaluateResponse
	if evaluated.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(evaluated.Body)
		evaluated.Body.Close()
		t.Fatalf("evaluate status/body=%d %s", evaluated.StatusCode, raw)
	}
	if err := json.NewDecoder(evaluated.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	evaluated.Body.Close()
	if flag := flagValue(preview.Argv, "--metrics-port"); flag != strconv.Itoa(planning.PreviewMetricsPort) {
		t.Fatalf("preview --metrics-port=%q argv=%v, want the placeholder %d", flag, preview.Argv, planning.PreviewMetricsPort)
	}

	created, err := api.PostJSON("/api/v1/deployments", request)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create status/body=%d %s", created.StatusCode, raw)
	}
	var deployment struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(created.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()

	var workload runtime.WorkloadSpec
	select {
	case workload = <-runs:
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not receive the workload")
	}
	var instance store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		instances, _ := env.Store().InstancesForDeployment(deployment.ID)
		if len(instances) != 1 {
			return false
		}
		instance = instances[0]
		return instance.DesiredStateJSON != ""
	})
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if desired.APIPort <= 0 || desired.MetricsPort != desired.APIPort+1 || !slices.Equal(desired.Ports, []int{desired.APIPort, desired.MetricsPort}) {
		t.Fatalf("api port %d metrics port %d ports %v, want two consecutive reserved ports", desired.APIPort, desired.MetricsPort, desired.Ports)
	}
	if instance.Ports != "["+strconv.Itoa(desired.APIPort)+","+strconv.Itoa(desired.MetricsPort)+"]" {
		t.Fatalf("instance reserved ports=%s", instance.Ports)
	}
	for name, argv := range map[string][]string{"desired state": desired.Argv, "workload": workload.Cmd} {
		if got := flagValue(argv, "--port"); got != strconv.Itoa(desired.APIPort) {
			t.Errorf("%s --port=%q argv=%v", name, got, argv)
		}
		if got := flagValue(argv, "--metrics-port"); got != strconv.Itoa(desired.MetricsPort) {
			t.Errorf("%s --metrics-port=%q argv=%v", name, got, argv)
		}
	}

	env.Eventually(t, 30*time.Second, func() bool {
		instances, _ := env.Store().InstancesForDeployment(deployment.ID)
		return len(instances) == 1 && instances[0].State == proto.InstRunning
	})
	// The template saved back from the running deployment keeps the declared
	// port, or its arguments would reference a variable it never declared.
	exported, err := api.PostJSON("/api/v1/deployments/"+strconv.FormatUint(uint64(deployment.ID), 10)+"/catalog-template", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(exported.Body)
	exported.Body.Close()
	var document struct {
		YAML     string `json:"yaml"`
		Saveable bool   `json:"saveable"`
	}
	if exported.StatusCode != http.StatusOK || json.Unmarshal(raw, &document) != nil || !strings.Contains(document.YAML, "port: separate") || !strings.Contains(document.YAML, "${GPUWORKS_METRICS_PORT}") {
		t.Fatalf("export status/body=%d %s", exported.StatusCode, raw)
	}
}

func flagValue(argv []string, flag string) string {
	if index := slices.Index(argv, flag); index >= 0 && index+1 < len(argv) {
		return argv[index+1]
	}
	return ""
}
