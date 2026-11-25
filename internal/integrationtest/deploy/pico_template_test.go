package deploy

import (
	"bytes"
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
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// picoLikeTemplateYAML is a multi-node recipe shaped like pico-dgx-ds4: a
// callable engine whose ranks each read their own pre-split weights
// (rank-scoped subfolder), keep state in a managed directory, build their
// fleet configuration from the peers file and need a long first start.
const picoLikeTemplateYAML = `models:
- model_id: gpuworks/pico-fake-tp2
  specs:
  - name: tp2-gb10
    engine: callable
    image: gpuworks/fake-engine:test
    weights:
      quantization: fp8
      sources:
      - modelscope:
          repo_id: gpuworks/pico-fake-tp2
          revision: main
          subfolder: rank${GPUWORKS_NODE_RANK}
    weight_bytes: 1048576
    gpus: {vendor: nvidia}
    start_timeout_sec: 5400
    dirs:
    - {name: pico-state, mount_path: /pico, limit_bytes: 1073741824}
    entrypoint: [/opt/pico/start.sh]
    args:
    - --rank
    - ${GPUWORKS_NODE_RANK}
    - --nnodes
    - ${GPUWORKS_NNODES}
    - --weights
    - ${GPUWORKS_MODEL_PATH}/rank${GPUWORKS_NODE_RANK}
    - --fleet
    - ${GPUWORKS_PEERS_FILE}
    - --state
    - /pico/rank${GPUWORKS_NODE_RANK}
    - --host
    - ${GPUWORKS_API_HOST}
    - --port
    - ${GPUWORKS_API_PORT}
    api:
      health_endpoint: /health
      operations:
      - {type: openai.chat, endpoint: /v1/chat/completions, mode: stream}
`

// TestPicoLikeTemplateResolvesEachRanksLaunch imports a template that uses
// all four template extensions together - rank-scoped weights, managed dirs,
// the peers file and a template start timeout - through the HTTP API, deploys
// it on two fake agents and checks every rank's resolved launch: its own
// weights, its own writable state directory, the group's peers file, its rank
// in the arguments and the template's start timeout. It then exports the
// deployment as a template and checks the YAML keeps all four.
//
//	go test ./internal/integrationtest/deploy/ -run TestPicoLikeTemplateResolvesEachRanksLaunch
func TestPicoLikeTemplateResolvesEachRanksLaunch(t *testing.T) {
	env := testenv.New(t)
	addresses := []string{"10.44.0.10", "10.44.0.11"}
	var nodes []*testenv.Node
	runs := []chan runtime.WorkloadSpec{make(chan runtime.WorkloadSpec, 1), make(chan runtime.WorkloadSpec, 1)}
	for rank, address := range addresses {
		node := env.AddNode(fmt.Sprintf("pico-gb10-%d", rank),
			testenv.WithGPUs(1, testenv.GPUSpec{Model: "GB10", VRAMMB: 24576}),
			testenv.WithDriverName("docker"), testenv.WithManagementAddress(address))
		captured := runs[rank]
		node.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(captured, spec) })
		nodes = append(nodes, node)
	}
	for _, node := range nodes {
		env.WaitNodeOnline(t, node.Name)
	}

	// Each rank's part is already on its own node, as a recipe's operator
	// would have staged it: rank r's node holds rank r's subfolder only.
	const repo = "gpuworks/pico-fake-tp2"
	var models []*store.Model
	var weightDirs []string
	var rows []*store.Node
	for rank, node := range nodes {
		model, err := env.Store().UpsertModel(&store.Model{
			Source: "modelscope", SourceID: repo, Revision: "main", ArtifactSubfolder: fmt.Sprintf("rank%d", rank),
			Name: repo, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
			Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		dir := testenv.ModelDir(t, fmt.Sprintf("pico-fake-rank%d", rank))
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: dir}); err != nil {
			t.Fatal(err)
		}
		models, weightDirs, rows = append(models, model), append(weightDirs, dir), append(rows, row)
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
	call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", []byte(picoLikeTemplateYAML), http.StatusOK)
	template, err := env.Store().InferenceModelByModelID(repo)
	if err != nil {
		t.Fatal(err)
	}

	const masterPort = 29511
	payload, _ := json.Marshal(map[string]any{
		"template_id": template.ID, "template_spec_name": "tp2-gb10", "node_uuid": nodes[0].UUID,
		"node_uuids": []string{nodes[0].UUID, nodes[1].UUID}, "served_name": "pico-fake",
		"nnodes": 2, "master_addr": addresses[0], "master_port": masterPort, "quick_params": map[string]any{},
	})
	var created dto.Deployment
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/deployments", "application/json", payload, http.StatusCreated), &created); err != nil {
		t.Fatal(err)
	}
	dep, err := env.Store().GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ModelID != models[0].ID {
		t.Fatalf("deployment model = %d, want rank 0's artifact %d", dep.ModelID, models[0].ID)
	}
	group := proto.ManagedDirGroupKey(dep.ID, dep.CreatedAt.Unix())

	peers := []proto.PeerRank{
		{NodeRank: 0, NodeName: nodes[0].Name, NodeUUID: nodes[0].UUID, NodeAddr: addresses[0]},
		{NodeRank: 1, NodeName: nodes[1].Name, NodeUUID: nodes[1].UUID, NodeAddr: addresses[1]},
	}
	for rank := range 2 {
		spec := waitCapturedWorkload(t, runs[rank], fmt.Sprintf("rank %d", rank))
		// The command: this rank's number, its own weights under the model
		// mount, the peers file and its own part of the state directory.
		command := strings.Join(spec.Cmd, " ")
		for _, want := range []string{
			fmt.Sprintf("--rank %d --nnodes 2", rank),
			fmt.Sprintf("--weights /model/rank%d", rank),
			"--fleet " + proto.PeersFileMountPath,
			fmt.Sprintf("--state /pico/rank%d", rank),
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("rank %d command %q lacks %q", rank, command, want)
			}
		}
		assertPeersFileMounted(t, spec, rank, addresses[0], masterPort, peers)

		var model, state *runtime.Mount
		for index := range spec.Mounts {
			switch spec.Mounts[index].Target {
			case "/model":
				model = &spec.Mounts[index]
			case "/pico":
				state = &spec.Mounts[index]
			}
		}
		if model == nil || model.Source != weightDirs[rank] || !model.ReadOnly {
			t.Fatalf("rank %d model mount %+v, want its own %s read-only", rank, model, weightDirs[rank])
		}
		// The managed directory: writable, prepared by the node's agent under
		// the deployment's group, and there on the host.
		if state == nil || state.ReadOnly || !strings.HasSuffix(state.Source, "/managed-dir/"+group+"/pico-state") {
			t.Fatalf("rank %d state mount %+v, want managed-dir/%s/pico-state read-write", rank, state, group)
		}
		if info, err := os.Stat(state.Source); err != nil || !info.IsDir() {
			t.Fatalf("rank %d state directory %s: %v", rank, state.Source, err)
		}
	}
	// Neither node was handed the other rank's part to download.
	for rank, row := range rows {
		if _, err := env.Store().ModelFileAt(store.FileRef{ModelID: models[1-rank].ID, NodeID: row.ID}); err == nil {
			t.Fatalf("rank %d's node was given rank %d's artifact", rank, 1-rank)
		}
	}

	// Every rank waits as long as the template says, and says why.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var detail struct {
			Instances []struct {
				NodeRank        int    `json:"node_rank"`
				StartTimeoutSec int    `json:"start_timeout_sec"`
				Source          string `json:"start_timeout_source"`
			} `json:"instances"`
		}
		if err := json.Unmarshal(call(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", created.ID), "", nil, http.StatusOK), &detail); err != nil {
			t.Fatal(err)
		}
		if len(detail.Instances) == 2 {
			for _, instance := range detail.Instances {
				if instance.StartTimeoutSec != 5400 || instance.Source != "template" {
					t.Fatalf("rank %d start timeout %d from %q, want 5400 from the template", instance.NodeRank, instance.StartTimeoutSec, instance.Source)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instances = %+v", detail.Instances)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The template editor's YAML, written from the stored spec, keeps all
	// four declarations as they were imported (the retention written back).
	exported := call(http.MethodGet, "/api/v1/inference-templates/yaml?model_id="+repo, "", nil, http.StatusOK)
	for _, want := range []string{"subfolder: rank${GPUWORKS_NODE_RANK}", "start_timeout_sec: 5400", "name: pico-state", "mount_path: /pico", "limit_bytes: 1073741824", "retain: deployment", "${GPUWORKS_PEERS_FILE}"} {
		if !bytes.Contains(exported, []byte(want)) {
			t.Fatalf("template YAML lacks %q:\n%s", want, exported)
		}
	}
}
