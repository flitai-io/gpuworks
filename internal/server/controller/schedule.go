package controller

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// relModelPath is the node-relative directory for a model's weights.
func relModelPath(m *store.Model) string { return store.ModelArtifactPath(m) }

// pickPort allocates a host port in the engine port range not used by other
// instances on the node nor by the host listeners its agent reports. Engines
// run on the host network, so either would make the launch fail to bind. A
// node with no free port gets the rejection to record instead.
func (c *Controller) pickPort(node *store.Node) (int, *scheduler.Reason) {
	ports, rejected := c.pickPorts(node, 1)
	if rejected != nil {
		return 0, rejected
	}
	return ports[0], nil
}

// pickPorts allocates one block of consecutive host ports per requested size,
// each free as pickPort's port is and no two blocks overlapping, and returns
// the first port of each block. A PD instance takes its API port and its side
// channel block in one call; the caller writes every port into
// Instance.Ports so the next allocation on the node avoids them.
func (c *Controller) pickPorts(node *store.Node, sizes ...int) ([]int, *scheduler.Reason) {
	used := map[int]bool{}
	insts, _ := c.store.InstancesForNode(node.ID)
	for _, in := range insts {
		if in.State == proto.InstStopped {
			continue
		}
		for _, p := range parseInts(in.Ports) {
			used[p] = true
		}
	}
	r := c.EnginePorts()
	if inv := network.Inventory(*node); inv != nil {
		for _, p := range inv.EnginePortListeners {
			used[p] = true
		}
		if own := inv.EnginePortRange; own != nil && own.Valid() {
			r = *own
		}
	}
	out := make([]int, len(sizes))
	next := r.Start
	for i, size := range sizes {
		size = max(size, 1)
		found := false
		for base := next; base+size-1 <= r.End; base++ {
			free := true
			for p := base; p < base+size; p++ {
				if used[p] {
					free = false
					base = p
					break
				}
			}
			if !free {
				continue
			}
			out[i], found = base, true
			for p := base; p < base+size; p++ {
				used[p] = true
			}
			next = base + size
			break
		}
		if !found {
			rejected := scheduler.EnginePortsExhausted(node.DisplayName(), r.Start, r.End)
			return nil, &rejected
		}
	}
	return out, nil
}

func jsonInts(v []int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonStrings encodes an identity list for storage; an empty list is stored
// as "" so a node whose prober reports no card identity reads as "unbound"
// rather than "bound to nothing".
func jsonStrings(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseInts(s string) []int {
	if s == "" {
		return nil
	}
	var v []int
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}
