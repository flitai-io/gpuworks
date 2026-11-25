package deploy

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The App wiring must use live sessions as the router's source of truth.
// A historical persisted online row must not claim a live session. A real
// agent reconnects independently while runtime-owned containers survive.
func TestNodeReconnectionDoesNotDuplicateSurvivingContainers(t *testing.T) {
	driver := testenv.NewFakeDriver("docker")
	env, api, deployment := lifecycleEnv(t, driver)
	instances := waitRunning(t, env, deployment, 2, nil)
	before, err := driver.RunningContainers(t.Context())
	if err != nil || len(before) != 2 {
		t.Fatalf("running containers=%v err=%v", before, err)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("actual node count=%d err=%v", len(nodes), err)
	}
	actual, err := api.Get("/api/v1/nodes/" + nodes[0].UUID)
	if err != nil {
		t.Fatal(err)
	}
	var connected dto.Node
	err = json.NewDecoder(actual.Body).Decode(&connected)
	actual.Body.Close()
	if err != nil || connected.State != "online" {
		t.Fatalf("actual node not authenticated: state=%s err=%v", connected.State, err)
	}
	node, _, err := env.Store().CreateNodePending("unreconnected")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store().SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := env.Store().TouchNodesAt(map[string]time.Time{node.UUID: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	response, err := api.Get("/api/v1/nodes/" + node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	var view dto.Node
	err = json.NewDecoder(response.Body).Decode(&view)
	response.Body.Close()
	if err != nil || view.State != "connecting" {
		t.Fatalf("node state=%s err=%v", view.State, err)
	}
	// The disconnected historical row expires independently from the live
	// agent, which reconnects with the same token and adopts its containers.
	if err := env.Store().MarkStaleOffline(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	env.RestartNode(t, "lifecycle-node")
	env.WaitNodeOnline(t, "lifecycle-node")
	afterInstances := waitRunning(t, env, deployment, 2, nil)
	after, err := driver.RunningContainers(t.Context())
	slices.Sort(before)
	slices.Sort(after)
	slices.Sort(instances)
	slices.Sort(afterInstances)
	if err != nil || !slices.Equal(before, after) || !slices.Equal(instances, afterInstances) {
		t.Fatalf("reconnect replaced runtime containers: %v -> %v, replicas %v -> %v, err=%v", before, after, instances, afterInstances, err)
	}
}
