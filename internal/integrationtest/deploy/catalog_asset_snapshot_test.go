package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestCatalogAssetBytesAreTransientWhilePersistedLaunchKeepsReferences(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("asset-reference",
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

	const modelID = "gpuworks/asset-reference"
	const revision = "0123456789abcdef0123456789abcdef01234567"
	weight, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: modelID, Revision: revision, Name: modelID, Arch: "FakeForCausalLM", WeightBytes: 1 << 20, Meta: `{"model_type":"fake","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: weight.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "asset-reference")}); err != nil {
		t.Fatal(err)
	}
	asset := []byte("print('catalog asset')\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(asset))
	model := inferencecatalog.InferenceModel{ModelID: modelID, Source: "user", Specs: []inferencecatalog.InferenceSpec{{
		Name: "asset", Engine: "vllm", Image: "example/vllm:test",
		Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID, Revision: revision}}},
		WeightBytes: 1 << 20, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
		Assets: []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: digest, UnpackTo: "/opt/gpuworks/serve.py"}},
	}}}
	packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, map[string][]byte{"models/" + modelID + "/assets/serve.py": asset})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	request, _ := http.NewRequest(http.MethodPost, env.URL()+"/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes()))
	request.Header.Set("Content-Type", "application/gzip")
	response, err := api.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("import status/body=%d %s", response.StatusCode, body)
	}
	response.Body.Close()
	template, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil || template.PackageDigest == "" {
		t.Fatalf("template=%+v err=%v", template, err)
	}
	body, _ := json.Marshal(map[string]any{"template_id": template.ID, "template_spec_name": "asset", "node_uuid": nodes[0].UUID, "served_name": "asset-reference"})
	created, err := api.PostJSON("/api/v1/deployments", body)
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
		t.Fatal("agent did not receive asset-backed workload")
	}
	storedDeployment, err := env.Store().GetDeployment(deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedDeployment.CatalogSelectionSnapshot, `"content"`) {
		t.Fatalf("deployment snapshot persisted asset bytes: %s", storedDeployment.CatalogSelectionSnapshot)
	}
	instances, err := env.Store().InstancesForDeployment(deployment.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	if strings.Contains(instances[0].DesiredStateJSON, `"content"`) {
		t.Fatalf("instance desired state persisted asset bytes: %s", instances[0].DesiredStateJSON)
	}
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if len(desired.RuntimeAssets) != 1 || desired.RuntimeAssets[0].PackageDigest != template.PackageDigest || desired.RuntimeAssets[0].ModelID != modelID || desired.RuntimeAssets[0].SizeBytes != int64(len(asset)) || len(desired.RuntimeAssets[0].Content) != 0 {
		t.Fatalf("persisted runtime asset=%+v", desired.RuntimeAssets)
	}
	var mounted string
	for _, mount := range workload.Mounts {
		if mount.Target == "/opt/gpuworks/serve.py" && mount.ReadOnly {
			mounted = mount.Source
		}
	}
	contents, err := os.ReadFile(mounted)
	if err != nil || !bytes.Equal(contents, asset) {
		t.Fatalf("transiently delivered mount=%q contents=%q err=%v", mounted, contents, err)
	}
}
