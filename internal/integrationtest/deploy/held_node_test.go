package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// siteHold holds every node out of scheduling once switched on, the way a
// maintenance window over the whole site would.
type siteHold struct{ on atomic.Bool }

func (*siteHold) Source() store.SchedulingSource { return "maint" }

func (h *siteHold) Hold(store.NodeHoldFacts) store.NodeHold {
	if !h.on.Load() {
		return store.NodeHold{}
	}
	return store.NodeHold{Code: "MAINT_WINDOW", Fallback: "maintenance until 06:00", Params: map[string]any{"until": "06:00"}}
}

// A node a scheduling holder holds is cordoned by its agent's status
// report, keeps what already runs on it (a hold cordons, never drains),
// refuses an operator's uncordon, and a new deployment pinned to it waits
// counting the node as cordoned, which the evaluator explains as
// node_cordoned. On a real server with a fake agent.
func TestAHeldNodeKeepsItsReplicasAndNewWorkWaits(t *testing.T) {
	env, api, depID := lifecycleEnv(t, testenv.NewFakeDriver("docker"))
	running := waitRunning(t, env, depID, 2, nil)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node := nodes[0]

	hold := &siteHold{}
	if err := env.Store().AddSchedulingHolder(hold); err != nil {
		t.Fatal(err)
	}
	// The hold starts while replicas run; the agent's next status report
	// (here: the one it sends on reconnecting) applies it.
	hold.on.Store(true)
	seen := lastSeen(t, env, node.UUID)
	env.RestartNode(t, "lifecycle-node")
	env.WaitNodeOnline(t, "lifecycle-node")
	env.Eventually(t, 20*time.Second, func() bool {
		row, err := env.Store().NodeByUUID(node.UUID)
		return err == nil && lastSeen(t, env, node.UUID) > seen && row.Scheduling == store.NodeCordoned && row.SchedulingBy == "maint"
	})

	// An operator cannot turn it back on; the refusal carries the hold.
	resp, err := api.Do(mustRequest(t, http.MethodPut, env.URL()+"/api/v1/nodes/"+node.UUID+"/scheduling", `{"scheduling":"schedulable"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), `"code":"MAINT_WINDOW"`) {
		t.Fatalf("uncordon of a held node = %d %s", resp.StatusCode, body)
	}

	// A new deployment pinned to the node is accepted and waits.
	model, err := env.Store().GetModel(mustDeployment(t, env, depID).ModelID)
	if err != nil {
		t.Fatal(err)
	}
	create, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "served_name": "held-model",
		"replicas": 1, "quick_params": map[string]any{"max_model_len": 256},
	})
	resp, err = api.PostJSON("/api/v1/deployments", create)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create on a held node = %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == 0 {
		t.Fatalf("create response %s: %v", body, err)
	}
	// It waits for a node to take it, counting the held node as cordoned.
	if !env.EventuallyNoFail(20*time.Second, func() bool {
		event, found, err := env.Store().LatestScheduleEvent(created.ID)
		return err == nil && found && strings.Contains(event.Data, `"code":"pool_empty"`) && strings.Contains(event.Data, `"cordoned":1`)
	}) {
		event, found, err := env.Store().LatestScheduleEvent(created.ID)
		t.Fatalf("the new deployment's wait = %+v found=%v err=%v, want pool_empty with the node cordoned", event, found, err)
	}
	// The evaluator explains the node as cordoned.
	resp, err = api.PostJSON("/api/v1/evaluate", create)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"code":"node_cordoned"`) {
		t.Fatalf("evaluate on a held node = %d %s", resp.StatusCode, body)
	}

	// The hold cordoned the node and never drained it: what ran there
	// keeps running, and the new deployment has placed nothing.
	row, err := env.Store().NodeByUUID(node.UUID)
	if err != nil || row.Scheduling != store.NodeCordoned || row.SchedulingBy != "maint" {
		t.Fatalf("the held node = %+v err=%v", row, err)
	}
	env.Eventually(t, 20*time.Second, func() bool {
		for _, uuid := range running {
			if instanceState(env, uuid) != proto.InstRunning {
				return false
			}
		}
		return true
	})
	if insts, _ := env.Store().InstancesForDeployment(depID); len(insts) != 2 {
		t.Fatalf("the held node's replicas changed: %+v", insts)
	}
	if insts, _ := env.Store().InstancesForDeployment(created.ID); len(insts) != 0 {
		t.Fatalf("placed on a held node: %+v", insts)
	}
}

func mustDeployment(t *testing.T, env *testenv.Env, id uint) *store.Deployment {
	t.Helper()
	dep, err := env.Store().GetDeployment(id)
	if err != nil {
		t.Fatal(fmt.Errorf("deployment %d: %w", id, err))
	}
	return dep
}

// lastSeen is when the node's agent last reported, as a number.
func lastSeen(t *testing.T, env *testenv.Env, uuid string) int64 {
	t.Helper()
	row, err := env.Store().NodeByUUID(uuid)
	if err != nil || row.LastSeenAt == nil {
		return 0
	}
	return row.LastSeenAt.UnixNano()
}
