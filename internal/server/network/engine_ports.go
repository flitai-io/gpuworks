package network

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TakenEnginePorts are the host ports of a node an engine cannot bind: the
// ports its instances hold, a stopped one's aside, and the listeners inside
// the engine port range its agent last reported (inv; nil when none). Engines
// run on the host network, so placement allocates around both, and the deploy
// preview reports a port a manual command states among them.
func TakenEnginePorts(inv *proto.NetInventory, instances []store.Instance) map[int]bool {
	taken := map[int]bool{}
	for _, instance := range instances {
		if instance.State == proto.InstStopped {
			continue
		}
		var ports []int
		if err := json.Unmarshal([]byte(instance.Ports), &ports); err != nil {
			continue
		}
		for _, port := range ports {
			taken[port] = true
		}
	}
	if inv != nil {
		for _, port := range inv.EnginePortListeners {
			taken[port] = true
		}
	}
	return taken
}
