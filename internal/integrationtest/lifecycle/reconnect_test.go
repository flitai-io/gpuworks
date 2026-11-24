package lifecycle

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestAgentGoesOfflineAndReconnectsAfterRealServerRestart(t *testing.T) {
	env := testenv.NewOnDisk(t)
	env.AddNode("reconnect-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "reconnect-node")
	before, err := env.Store().ListNodes()
	if err != nil || len(before) != 1 {
		t.Fatalf("nodes before restart=%+v err=%v", before, err)
	}
	uuid := before[0].UUID
	env.StopServer(t)
	env.Eventually(t, 10*time.Second, func() bool {
		nodes, _ := env.Store().ListNodes()
		return len(nodes) == 1 && nodes[0].State == store.NodeStateOffline
	})
	env.RestartServer(t)
	env.Eventually(t, 20*time.Second, func() bool {
		nodes, _ := env.Store().ListNodes()
		return len(nodes) == 1 && nodes[0].UUID == uuid && nodes[0].State == store.NodeStateOnline
	})
}
