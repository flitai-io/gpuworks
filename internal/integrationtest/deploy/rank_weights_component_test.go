package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestRankScopedComponentMountsEachRanksOwnSubfolder creates a two-node
// deployment of the GB10 TensorFold shape through the REST API, the
// controller and two fake agents: a primary pack the same on every rank and
// an auxiliary component whose subfolder is rank${GPUWORKS_NODE_RANK}. Each
// rank mounts the shared pack and its own part of the component, and neither
// node is given the other rank's part to download.
func TestRankScopedComponentMountsEachRanksOwnSubfolder(t *testing.T) {
	env := testenv.New(t)
	leader := env.AddNode("rank-component-0",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.43.1.10"))
	follower := env.AddNode("rank-component-1",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.43.1.11"))
	runs := []chan runtime.WorkloadSpec{make(chan runtime.WorkloadSpec, 1), make(chan runtime.WorkloadSpec, 1)}
	leader.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(runs[0], spec) })
	follower.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(runs[1], spec) })
	env.WaitNodeOnline(t, leader.Name)
	env.WaitNodeOnline(t, follower.Name)

	const pack, engram = "gpuworks/pack-fake", "gpuworks/engram-fake-tp2"
	var rows []*store.Node
	for _, node := range []*testenv.Node{leader, follower} {
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	primary, err := env.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: pack, Revision: "main", Name: pack, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var packDirs, engramDirs []string
	var parts []*store.Model
	for rank, row := range rows {
		dir := testenv.ModelDir(t, fmt.Sprintf("pack-fake-%d", rank))
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: primary.ID, NodeID: row.ID, State: proto.MFPresent, Path: dir}); err != nil {
			t.Fatal(err)
		}
		part, err := env.Store().UpsertModel(&store.Model{
			Source: "modelscope", SourceID: engram, Revision: "e1", ArtifactSubfolder: fmt.Sprintf("rank%d", rank), Name: engram, WeightBytes: 1 << 19,
		})
		if err != nil {
			t.Fatal(err)
		}
		partDir := testenv.ModelDir(t, fmt.Sprintf("engram-fake-rank%d", rank))
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: part.ID, NodeID: row.ID, State: proto.MFPresent, Path: partDir}); err != nil {
			t.Fatal(err)
		}
		packDirs, engramDirs, parts = append(packDirs, dir), append(engramDirs, partDir), append(parts, part)
	}
	primarySource := `{"provider":"modelscope","repo_id":"` + pack + `","revision":"main"}`
	catalog := store.InferenceModel{ModelID: pack, Source: "upstream", Architecture: "Qwen3ForCausalLM", ContextLength: 4096, Specs: []store.InferenceSpecRow{{
		Name: "tp2-split-aux", Engine: "vllm", Image: "gpuworks/fake-engine:test", Quantization: "bf16", WeightBytes: 1 << 20,
		WeightSources: "[" + primarySource + "]",
		GPUs:          `{"vendor":"nvidia"}`, Args: `["--max-model-len","512","--engram-dir","/engram/rank${GPUWORKS_NODE_RANK}"]`,
		Components: `[{"name":"main","role":"primary","weights":{"quantization":"bf16","sources":[` + primarySource + `]},"weight_bytes":1048576,"mount_path":"/model"},` +
			`{"name":"engram","role":"auxiliary","weights":{"quantization":"bf16","sources":[{"provider":"modelscope","repo_id":"` + engram + `","revision":"e1","subfolder":"rank${GPUWORKS_NODE_RANK}"}]},"weight_bytes":524288,"mount_path":"/engram"}]`,
	}}}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(map[string]any{
		"template_id": template.ID, "template_spec_name": "tp2-split-aux", "node_uuid": leader.UUID,
		"node_uuids": []string{leader.UUID, follower.UUID}, "served_name": "split-aux-fake",
		"nnodes": 2, "master_addr": "10.43.1.10", "master_port": 29502,
		"placement": map[string]any{"tp": 1, "pp": 2}, "quick_params": map[string]any{},
	})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	dep, err := env.Store().GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ModelID != primary.ID {
		t.Fatalf("deployment model = %d, want the shared pack %d", dep.ModelID, primary.ID)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	var bound deploy.ComponentBinding
	for _, component := range selection.Components {
		if component.Name == "engram" {
			bound = component
		}
	}
	if len(selection.RankWeights) != 0 || bound.Subfolder != "rank${GPUWORKS_NODE_RANK}" || len(bound.RankWeights) != 2 ||
		bound.RankWeights[0].StoreModelID != parts[0].ID || bound.RankWeights[1].StoreModelID != parts[1].ID {
		t.Fatalf("snapshot rank weights %+v, engram %+v", selection.RankWeights, bound)
	}

	for rank := range 2 {
		spec := waitCapturedWorkload(t, runs[rank], fmt.Sprintf("rank %d", rank))
		mounts := map[string]string{}
		for _, mount := range spec.Mounts {
			mounts[mount.Target] = mount.Source
		}
		if mounts["/model"] != packDirs[rank] || mounts["/engram"] != engramDirs[rank] {
			t.Fatalf("rank %d mounts %+v, want the pack %s and its own engram part %s", rank, spec.Mounts, packDirs[rank], engramDirs[rank])
		}
	}
	for rank, row := range rows {
		other := parts[1-rank]
		if _, err := env.Store().ModelFileAt(store.FileRef{ModelID: other.ID, NodeID: row.ID}); err == nil {
			t.Fatalf("rank %d's node was given rank %d's engram part", rank, 1-rank)
		}
	}
}
