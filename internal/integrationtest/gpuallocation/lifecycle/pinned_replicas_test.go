package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Scaling records desired state, even while its pinned card is occupied. A
// free card outside that selection must neither be claimed as room nor used
// for the extra replica; the controller must explain why it is waiting.
func TestPinnedReplicaScaleReportsBusyAndPreservesDesiredCount(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("pinned-replica", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "pinned-replica")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "pinned-model", Revision: "main", Name: "pinned-model", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "pinned")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"placement_mode":"manual","placement":{"tp":1,"gpus":[0]},"replicas":1,"quick_params":{"max_model_len":256}}`, model.ID, nodes[0].UUID)
	resp, err := api.PostJSON("/api/v1/deployments", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create=%d %s", resp.StatusCode, data)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(data, &dep); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, env, dep.ID)
	path := fmt.Sprintf("/api/v1/deployments/%d", dep.ID)
	resp, err = api.Do(mustRequest(t, http.MethodPut, env.URL()+path+"/replicas", `{"replicas":2}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale=%d %s", resp.StatusCode, data)
	}
	env.Eventually(t, 10*time.Second, func() bool {
		resp, err := api.Get(path)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var current dto.Deployment
		return json.NewDecoder(resp.Body).Decode(&current) == nil && current.Replicas == 2 && current.PlacementStatus != nil && current.PlacementStatus.Unscheduled == 1 && current.PlacementStatus.Reason != nil && current.PlacementStatus.Reason.Code == "pool_no_fit"
	})
	resp, err = api.Get(path + "/candidates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var candidates dto.DeploymentCandidates
	if err := json.NewDecoder(resp.Body).Decode(&candidates); err != nil {
		t.Fatal(err)
	}
	if candidates.Counts["room"] != 0 || candidates.Counts["busy"] != 1 || len(candidates.Items) != 1 || candidates.Items[0].FreeGPUs != 0 {
		t.Fatalf("pinned pool claims unused card as room: %+v", candidates)
	}
	rows, err := env.Store().InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 || rows[0].GPUIndexes != "[0]" {
		t.Fatalf("pinned replica moved or overcommitted: %+v %v", rows, err)
	}
	event, found, err := env.Store().LatestScheduleEvent(dep.ID)
	if err != nil || !found || event.Code != store.EventDeploymentUnschedulable || !strings.Contains(event.Data, `"busy":1`) {
		t.Fatalf("missing refusal event: %+v found=%v err=%v", event, found, err)
	}
}
