package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

const rankRevisionRepo = "gpuworks/ranked-revision-fake"

// rankRevisionTemplate is a rank-scoped template pinned to one revision.
func rankRevisionTemplate(revision string) []byte {
	return []byte(fmt.Sprintf(`models:
- model_id: %s
  specs:
  - name: tp2-ranked
    engine: callable
    image: gpuworks/fake-engine:test
    weights:
      quantization: fp8
      sources:
      - modelscope:
          repo_id: %s
          revision: %s
          subfolder: rank${GPUWORKS_NODE_RANK}
    weight_bytes: 1048576
    gpus: {vendor: nvidia}
    entrypoint: [/opt/ranked/start.sh]
    args: [--rank, "${GPUWORKS_NODE_RANK}", --host, "${GPUWORKS_API_HOST}", --port, "${GPUWORKS_API_PORT}"]
    api:
      health_endpoint: /health
      operations:
      - {type: openai.chat, endpoint: /v1/chat/completions, mode: stream}
`, rankRevisionRepo, rankRevisionRepo, revision))
}

// TestReimportedRankScopedTemplateDeploysTheRevisionItNowStates imports a
// rank-scoped template at revision A, deploys it, re-imports it at revision B
// and deploys again: every rank of the second deployment binds an artifact at
// B (new rows, to be downloaded), never the rows registered for A. The
// registered models the deploy form offers for the spec are rank 0's artifact
// at the revision the template states now, so one click is a valid choice.
//
//	go test ./internal/integrationtest/deploy/ -run TestReimportedRankScopedTemplateDeploysTheRevisionItNowStates
func TestReimportedRankScopedTemplateDeploysTheRevisionItNowStates(t *testing.T) {
	env := testenv.New(t)
	addresses := []string{"10.45.0.10", "10.45.0.11"}
	var nodes []*testenv.Node
	for rank, address := range addresses {
		nodes = append(nodes, env.AddNode(fmt.Sprintf("ranked-revision-%d", rank),
			testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
			testenv.WithDriverName("docker"), testenv.WithManagementAddress(address)))
	}
	for _, node := range nodes {
		env.WaitNodeOnline(t, node.Name)
	}
	const oldRevision, newRevision = "7cd1d57aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "11e0d612bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Revision A was deployed before: each rank's part is on its node.
	var old []*store.Model
	for rank, node := range nodes {
		model, err := env.Store().UpsertModel(&store.Model{
			Source: "modelscope", SourceID: rankRevisionRepo, Revision: oldRevision, ArtifactSubfolder: fmt.Sprintf("rank%d", rank),
			Name: rankRevisionRepo, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
			Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, fmt.Sprintf("ranked-revision-old%d", rank))}); err != nil {
			t.Fatal(err)
		}
		old = append(old, model)
	}

	api := env.API(t)
	call := func(method, path, contentType string, body []byte, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d body=%s", method, path, resp.StatusCode, raw)
		}
		return raw
	}
	deployOnce := func(port int) (*store.Deployment, deploy.CatalogSelection) {
		t.Helper()
		template, err := env.Store().InferenceModelByModelID(rankRevisionRepo)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{
			"template_id": template.ID, "template_spec_name": "tp2-ranked", "node_uuid": nodes[0].UUID,
			"node_uuids": []string{nodes[0].UUID, nodes[1].UUID}, "served_name": fmt.Sprintf("ranked-revision-%d", port),
			"nnodes": 2, "master_addr": addresses[0], "master_port": port, "quick_params": map[string]any{},
		})
		var created dto.Deployment
		if err := json.Unmarshal(call(http.MethodPost, "/api/v1/deployments", "application/json", payload, http.StatusCreated), &created); err != nil {
			t.Fatal(err)
		}
		dep, err := env.Store().GetDeployment(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
		if err != nil {
			t.Fatal(err)
		}
		return dep, selection
	}
	weightModelsPath := func() string {
		template, err := env.Store().InferenceModelByModelID(rankRevisionRepo)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("/api/v1/inference-templates/weight-models?template_id=%d&spec_source=%s&spec_name=tp2-ranked", template.ID, template.Source)
	}

	call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", rankRevisionTemplate(oldRevision), http.StatusOK)
	first, firstSelection := deployOnce(29601)
	if first.ModelID != old[0].ID || len(firstSelection.RankWeights) != 2 || firstSelection.RankWeights[1].StoreModelID != old[1].ID {
		t.Fatalf("first deployment model=%d ranks=%+v, want the registered rank artifacts %d and %d", first.ModelID, firstSelection.RankWeights, old[0].ID, old[1].ID)
	}

	call(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", "application/yaml", rankRevisionTemplate(newRevision), http.StatusOK)

	// The chooser: the registered model for the spec is rank 0's artifact at
	// the revision now stated; the old revision's rows are no choice, and the
	// other ranks' rows are never offered next to rank 0's.
	var offered struct {
		Models  []struct{ ID uint } `json:"models"`
		Sources []struct {
			Revision     string `json:"revision"`
			ReuseModelID uint   `json:"reuse_model_id"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(call(http.MethodGet, weightModelsPath(), "", nil, http.StatusOK), &offered); err != nil {
		t.Fatal(err)
	}
	if len(offered.Models) != 0 || len(offered.Sources) != 1 || offered.Sources[0].Revision != newRevision || offered.Sources[0].ReuseModelID != 0 {
		t.Fatalf("revision %s has nothing registered, but the form is offered %+v", newRevision, offered)
	}

	second, selection := deployOnce(29602)
	if len(selection.RankWeights) != 2 {
		t.Fatalf("second deployment rank weights = %+v", selection.RankWeights)
	}
	for rank, weight := range selection.RankWeights {
		model, err := env.Store().GetModel(weight.StoreModelID)
		if err != nil {
			t.Fatal(err)
		}
		if model.Revision != newRevision || model.ArtifactSubfolder != fmt.Sprintf("rank%d", rank) || model.ID == old[rank].ID {
			t.Fatalf("rank %d binds model %d (%s@%s subfolder %q), want a new artifact at %s", rank, model.ID, model.SourceID, model.Revision, model.ArtifactSubfolder, newRevision)
		}
	}
	if second.ModelID != selection.RankWeights[0].StoreModelID || selection.WeightRevision != newRevision {
		t.Fatalf("second deployment model=%d weight revision=%q, want rank 0's artifact at %s", second.ModelID, selection.WeightRevision, newRevision)
	}

	// Now rank 0's artifact at the new revision is registered: it is the one
	// choice the form offers, and naming it deploys.
	if err := json.Unmarshal(call(http.MethodGet, weightModelsPath(), "", nil, http.StatusOK), &offered); err != nil {
		t.Fatal(err)
	}
	if len(offered.Models) != 1 || offered.Models[0].ID != second.ModelID || offered.Sources[0].ReuseModelID != second.ModelID {
		t.Fatalf("form offers %+v, want only rank 0's artifact %d", offered, second.ModelID)
	}

	// The old rows are untouched, and naming one of them is refused rather
	// than deployed from a revision the template no longer states.
	for rank, row := range old {
		if row.Revision != oldRevision || row.ArtifactSubfolder != fmt.Sprintf("rank%d", rank) {
			t.Fatalf("old row %d changed: %+v", row.ID, row)
		}
	}
	template, _ := env.Store().InferenceModelByModelID(rankRevisionRepo)
	payload, _ := json.Marshal(map[string]any{
		"template_id": template.ID, "template_spec_name": "tp2-ranked", "node_uuid": nodes[0].UUID, "model_id": old[0].ID,
		"node_uuids": []string{nodes[0].UUID, nodes[1].UUID}, "served_name": "ranked-revision-old",
		"nnodes": 2, "master_addr": addresses[0], "master_port": 29603, "quick_params": map[string]any{},
	})
	refused := call(http.MethodPost, "/api/v1/deployments", "application/json", payload, http.StatusBadRequest)
	if !strings.Contains(string(refused), "TEMPLATE_MODEL_MISMATCH") {
		t.Fatalf("old revision's model accepted or refused for another reason: %s", refused)
	}
}
