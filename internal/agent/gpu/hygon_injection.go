package gpu

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

// RuntimeInjectionForDriver implements DriverAwareInjector. The selection
// variables must count cards the way HIP inside the container counts them.
// Docker, nerdctl and a Kubernetes pod (which gets only its cards' nodes
// through the agent's device plugin) pass only the selected cards' DRM
// nodes, so HIP sees a compact 0..N-1 list there: host index 2 is HIP device 0, and naming it "2"
// hides every card ("No HIP GPUs are available"). runveil binds the host's
// whole /dev and /sys, so HIP enumerates every card and the host indexes stay.
func (p *HygonProber) RuntimeInjectionForDriver(indexes []int, driverName string) RuntimeInjection {
	inj := p.RuntimeInjection(indexes)
	if inj.UnsupportedReason != "" || len(inj.Env) == 0 {
		return inj
	}
	switch driverName {
	case "docker", "nerdctl", "kube":
		visible := compactVisibleDevices(len(indexes))
		inj.Env = map[string]string{
			"HYGON_VISIBLE_DEVICES": visible,
			"HIP_VISIBLE_DEVICES":   visible,
		}
	}
	return inj
}

func (p *HygonProber) RuntimeInjection(indexes []int) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	out, err := p.run(context.Background(), hygonInventoryArgs()...)
	if err != nil {
		return RuntimeInjection{UnsupportedReason: fmt.Sprintf("cannot verify Hygon device inventory: hy-smi: %v", err)}
	}
	gpus := parseHygonInventory(out, func(path string) ([]byte, error) {
		return p.read(path)
	})
	byIndex := make(map[int]proto.GPUInfo, len(gpus))
	for _, gpu := range gpus {
		byIndex[gpu.Index] = gpu
	}

	args := make([]string, 0, 14+len(indexes)*4)
	devices := []string{}
	for _, path := range []string{"/dev/kfd", "/dev/mkfd"} {
		if err := p.requireDevice(path); err != nil {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("required Hygon device is unavailable: %s: %v", path, err)}
		}
		args = append(args, "--device", path+":"+path+":rwm")
		devices = append(devices, path)
	}
	if info, err := p.inspect("/opt/hyhal"); err != nil || !info.IsDir() {
		return RuntimeInjection{UnsupportedReason: "required Hygon driver directory is unavailable: /opt/hyhal"}
	}

	seen := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		if seen[index] {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("Hygon device index %d was selected more than once", index)}
		}
		seen[index] = true
		gpu, ok := byIndex[index]
		if !ok || gpu.PCIeBDF == "" {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("Hygon device %d is absent from the hy-smi inventory", index)}
		}
		p.mu.RLock()
		expected, hasExpected := p.lastSeen[index]
		p.mu.RUnlock()
		if hasExpected && expected.bdf != "" && expected.bdf != gpu.PCIeBDF {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("Hygon index %d changed from PCI %s to %s since probe", index, expected.bdf, gpu.PCIeBDF)}
		}
		nodes, err := p.drmNodes(gpu.PCIeBDF)
		if err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
		for _, path := range nodes {
			args = append(args, "--device", path+":"+path+":rwm")
			devices = append(devices, path)
		}
	}
	// Docker resolves named supplementary groups inside the image. Minimal
	// probe images have no group database, so pass host device GIDs as numbers.
	groups := map[uint32]bool{}
	for _, path := range devices {
		info, err := p.inspect(path)
		if err != nil {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("inspect Hygon device %s group: %v", path, err)}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("cannot determine Hygon device %s group", path)}
		}
		groups[stat.Gid] = true
	}
	groupIDs := make([]int, 0, len(groups))
	for group := range groups {
		groupIDs = append(groupIDs, int(group))
	}
	sort.Ints(groupIDs)
	for _, group := range groupIDs {
		args = append(args, "--group-add", strconv.Itoa(group))
	}
	visible := joinDeviceIndexes(indexes)
	// Ray in Hygon vLLM images rejects ROCR_VISIBLE_DEVICES when
	// HIP_VISIBLE_DEVICES is also set. HIP alone restricts the visible cards.
	return RuntimeInjection{
		Env: map[string]string{
			"HYGON_VISIBLE_DEVICES": visible,
			"HIP_VISIBLE_DEVICES":   visible,
		},
		DockerArgs: args,
		// Driver libraries are required by every runtime. PRoot launchers do
		// not interpret Docker volume flags; structured mounts reach them too.
		Mounts: []RuntimeMount{{Source: "/opt/hyhal", Target: "/opt/hyhal", ReadOnly: true}},
	}
}

func joinDeviceIndexes(indexes []int) string {
	values := make([]string, len(indexes))
	for i, index := range indexes {
		values[i] = strconv.Itoa(index)
	}
	return strings.Join(values, ",")
}
