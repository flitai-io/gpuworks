package api

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubePortOutside is an instance that holds ports outside an entry's new
// engine port range, as the warning KUBE_ENGINE_PORTS_OUTSIDE lists it.
type kubePortOutside struct {
	InstanceUUID string `json:"instance_uuid"`
	Deployment   string `json:"deployment"`
	Ports        []int  `json:"ports"` // the ports outside the range, ascending
}

// kubeEnginePortsWarnings is the warning an edit of an entry's engine_ports
// carries when running instances of the entry hold ports outside the new
// range: the edit is allowed, the instances keep their ports (only ports
// allocated afterwards come from the new range), and the administrator is
// told which. skip names instances that are on their way out anyway. The
// range is the entry's, already validated; no warning when every held port is
// inside it.
func (a *apiHandlers) kubeEnginePortsWarnings(entry store.KubeCluster, skip map[string]bool) ([]messages.Warning, error) {
	out := []messages.Warning{}
	rng, err := proto.ParsePortRange(entry.EnginePorts)
	if err != nil {
		// kubeValid accepted the range before this runs.
		return nil, fmt.Errorf("engine_ports %q: %w", entry.EnginePorts, err)
	}
	held, err := a.d.Store.KubeClusterInstancePorts(entry.ID)
	if err != nil {
		return nil, err
	}
	var outside []kubePortOutside
	for _, inst := range held {
		if skip[inst.InstanceUUID] {
			continue
		}
		var ports []int
		for _, p := range inst.Ports {
			if !rng.Contains(p) {
				ports = append(ports, p)
			}
		}
		if len(ports) > 0 {
			outside = append(outside, kubePortOutside{InstanceUUID: inst.InstanceUUID, Deployment: inst.Deployment, Ports: ports})
		}
	}
	if len(outside) == 0 {
		return out, nil
	}
	return append(out, messages.NewWarning(messages.WarningKubeEnginePortsOutside, map[string]any{"range": entry.EnginePorts, "instances": outside})), nil
}
