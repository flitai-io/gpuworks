package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestFleetSettingsSaveDoesNotWaitForTheFleet saves a setting that reaches
// every node (a registry credential) on a fleet of in-process agents, and
// holds the request to a bound that does not grow with the fleet: the replays
// to the nodes are scheduled on each session's own worker. Before, the save
// replayed node after node inside the request: 0.8 s at 100 nodes, 3.9 s at
// 400 and 11.1 s at 1000.
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 40m ./internal/integrationtest/fleet/ -run TestFleetSettingsSaveDoesNotWaitForTheFleet
//
// sized by GPUWORKS_FLEET_NODES.
func TestFleetSettingsSaveDoesNotWaitForTheFleet(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 1000)
	env := testenv.NewOnDisk(t)
	for i := 0; i < nodeCount; i++ {
		node := env.AddNode(fmt.Sprintf("fanout-%04d", i), testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
		node.Driver.ShareEnginePorts()
	}
	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatalf("log in: %v", err)
	}
	start := time.Now()
	if _, err := WaitFleetOnline(client, nodeCount, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d nodes online in %s", nodeCount, time.Since(start).Round(time.Millisecond))
	time.Sleep(5 * time.Second) // the connect replays settle

	var took []time.Duration
	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(map[string]any{"host": fmt.Sprintf("registry-%d.example.com", i), "username": "u", "secret": "s"})
		at := time.Now()
		if err := client.PostJSON("/api/v1/pull/credentials", body, nil); err != nil {
			t.Fatal(err)
		}
		took = append(took, time.Since(at))
	}
	sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
	median := took[len(took)/2]
	t.Logf("nodes=%d POST /pull/credentials: min %v median %v max %v", nodeCount, took[0].Round(time.Millisecond), median.Round(time.Millisecond), took[len(took)-1].Round(time.Millisecond))
	// Saving reads the node list and asks each session; nothing waits for an
	// agent. Half a second is far under what the replays took at 100 nodes.
	if median > 500*time.Millisecond {
		t.Fatalf("saving a fleet-wide setting took %v (median) at %d nodes: it waited for the nodes", median, nodeCount)
	}
}
