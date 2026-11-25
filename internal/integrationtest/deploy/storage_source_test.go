package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Deployment 267 on gpu3:8800: it saves into a team storage only one node is
// in, and the other node holds the same model in its local storage. Each
// replica reads the copy its node has, and records which. The team storage is
// a second local storage: a shared directory on a plain temp directory is
// refused, because it would be a bare directory on the root disk and not a
// mounted share.
func TestReplicasReadWhicheverCopyTheirNodeHas(t *testing.T) {
	env := testenv.New(t)
	gpu := testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576})
	env.AddNode("team-node", gpu, testenv.WithDriverName("docker"))
	env.AddNode("other-node", gpu, testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "team-node")
	env.WaitNodeOnline(t, "other-node")
	byName := map[string]store.Node{}
	nodes, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		byName[node.Name] = node
	}
	teamNode, otherNode := byName["team-node"], byName["other-node"]
	if teamNode.ID == 0 || otherNode.ID == 0 {
		t.Fatalf("nodes = %+v", nodes)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "team/q05", Revision: "main", Name: "q05",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second local storage that only the team node has, holding the model.
	root := t.TempDir()
	teamCopy := filepath.Join(root, "team", "q05")
	if err := os.MkdirAll(teamCopy, 0o755); err != nil {
		t.Fatal(err)
	}
	// Created through the API, as an operator would, so the node is told.
	created, err := env.API(t).PostJSON("/api/v1/storages", mustJSON(t, map[string]any{
		"name": "team", "type": proto.StorageLocal, "path": root, "all_nodes": false,
		"targets": []map[string]any{{"node_uuid": teamNode.UUID}}, "probe_node": teamNode.UUID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var team struct {
		ID uint `json:"id"`
	}
	createdBody, _ := io.ReadAll(created.Body)
	created.Body.Close()
	if created.StatusCode != http.StatusCreated || json.Unmarshal(createdBody, &team) != nil || team.ID == 0 {
		t.Fatalf("create storage = %d %s", created.StatusCode, createdBody)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: team.ID, NodeID: teamNode.ID, Path: teamCopy, State: proto.MFPresent, SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	ownCopy := testenv.ModelDir(t, "q05-local")
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: otherNode.ID, State: proto.MFPresent, Path: ownCopy}); err != nil {
		t.Fatal(err)
	}
	env.Eventually(t, 20*time.Second, func() bool {
		state, err := env.Store().StorageNodeStateFor(team.ID, teamNode.ID)
		return err == nil && state.State == proto.StorageStateReady
	})

	dep, err := env.Store().CreateDeploymentWithRequirements(&store.Deployment{
		Name: "q05", ModelID: model.ID, NodeID: teamNode.ID, ServedName: "q05", Engine: "vllm", Image: "example/vllm:test", Replicas: 2, State: store.DeployActive,
		PlacementStrategy: store.PlacementSpread, SaveToStorageID: team.ID,
	}, nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]string{teamNode.ID: teamCopy, otherNode.ID: ownCopy}
	var running []store.Instance
	// Both logical nodes share this host's ports: a replica can lose its port
	// to the other node's engine (ENGINE_PORT_REFUSED) and is placed again, so
	// only the running ones count.
	ok := env.EventuallyNoFail(45*time.Second, func() bool {
		rows, err := env.Store().InstancesForDeployment(dep.ID)
		if err != nil {
			return false
		}
		running = running[:0]
		for _, row := range rows {
			if row.State == proto.InstRunning {
				running = append(running, row)
			}
		}
		return len(running) == 2 && running[0].NodeID != running[1].NodeID
	})
	if !ok {
		rows, _ := env.Store().InstancesForDeployment(dep.ID)
		events, _ := env.Store().DeploymentEvents(dep.ID, 30)
		for _, ev := range events {
			t.Logf("event %s %s %s", ev.Code, ev.Message, ev.Data)
		}
		t.Fatalf("instances = %+v", rows)
	}
	ids := []uint{running[0].ID, running[1].ID}
	reads, err := env.Store().InstanceModelFiles(ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range running {
		got := reads[inst.ID]
		if len(got) != 1 || got[0].HostPath != want[inst.NodeID] {
			t.Fatalf("instance on node %d reads %+v, want %s", inst.NodeID, got, want[inst.NodeID])
		}
	}

	// The deployment detail says which file each instance reads, and from
	// which storage.
	resp, err := env.API(t).Get("/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deployment = %d %s", resp.StatusCode, body)
	}
	var detail struct {
		Instances []struct {
			NodeUUID   string `json:"node_uuid"`
			ModelFiles []struct {
				ModelID     uint   `json:"model_id"`
				StorageName string `json:"storage_name"`
				StorageType string `json:"storage_type"`
				Path        string `json:"path"`
			} `json:"model_files"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, inst := range detail.Instances {
		if len(inst.ModelFiles) != 1 || inst.ModelFiles[0].ModelID != model.ID {
			t.Fatalf("instance %s model_files = %+v", inst.NodeUUID, inst.ModelFiles)
		}
		seen[inst.NodeUUID] = inst.ModelFiles[0].StorageName + " " + inst.ModelFiles[0].StorageType + " " + inst.ModelFiles[0].Path
	}
	local, err := env.Store().StorageByID(env.Store().DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	if seen[teamNode.UUID] != "team local "+want[teamNode.ID] || seen[otherNode.UUID] != local.Name+" local "+ownCopy {
		t.Fatalf("model files by node = %v", seen)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
