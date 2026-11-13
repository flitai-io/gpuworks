package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/deviceplugin"
)

// The device nodes a workload's --device options list reach its pod through
// the agent's own device plugin (deviceplugin): one resource per card,
// gpuworks.ai/accelerator-<index>, giving the nodes only that card has, and
// gpuworks.ai/accelerator-shared giving the nodes every card needs (Ascend's
// davinci_manager, PPU's alixpu_ctl). The split comes from the vendor's own
// injection, card by card, so no vendor is named here. A pod asks for the
// resources whose nodes it needs; a device the plugin does not hand out (an
// InfiniBand node) leaves the pod privileged, as a docker container with it
// would see the host's /dev.

const (
	cardResourcePrefix = "gpuworks.ai/accelerator-"
	sharedResource     = "gpuworks.ai/accelerator-shared"
	// cardUnits is how many pods can hold one card at once: logical slices
	// share a card, and the platform's scheduler, not the kubelet, decides
	// who may.
	cardUnits = 32
)

// devicePlan is what the agent's device plugin serves and which resource
// hands out which host device node.
type devicePlan struct {
	resources []deviceplugin.Resource
	byPath    map[string]string // host device path -> resource
}

func cardResource(index int) string { return cardResourcePrefix + strconv.Itoa(index) }

// parseDevice reads a docker --device value, HOST[:CONTAINER[:PERMISSIONS]].
func parseDevice(v string) (deviceplugin.DeviceSpec, error) {
	parts := strings.Split(v, ":")
	if len(parts) > 3 || !strings.HasPrefix(parts[0], "/dev/") {
		return deviceplugin.DeviceSpec{}, fmt.Errorf("device %q is not HOST[:CONTAINER[:PERMISSIONS]] under /dev", v)
	}
	d := deviceplugin.DeviceSpec{HostPath: parts[0], ContainerPath: parts[0], Permissions: "rwm"}
	if len(parts) >= 2 && parts[1] != "" {
		d.ContainerPath = parts[1]
	}
	if len(parts) == 3 {
		if parts[2] == "" || strings.Trim(parts[2], "rwm") != "" {
			return deviceplugin.DeviceSpec{}, fmt.Errorf("device %q: permissions must be made of r, w and m", v)
		}
		d.Permissions = parts[2]
	}
	return d, nil
}

// planDevices splits each card's --device list into the nodes all cards
// share and the ones only that card has.
func planDevices(perCard map[int][]string) (devicePlan, error) {
	plan := devicePlan{byPath: map[string]string{}}
	if len(perCard) == 0 {
		return plan, nil
	}
	specs := map[int]map[string]deviceplugin.DeviceSpec{}
	count := map[string]int{}
	for index, devices := range perCard {
		specs[index] = map[string]deviceplugin.DeviceSpec{}
		for _, v := range devices {
			d, err := parseDevice(v)
			if err != nil {
				return devicePlan{}, fmt.Errorf("card %d: %w", index, err)
			}
			if _, dup := specs[index][d.HostPath]; !dup {
				count[d.HostPath]++
			}
			specs[index][d.HostPath] = d
		}
	}
	indexes := make([]int, 0, len(perCard))
	for i := range perCard {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	shared := deviceplugin.Resource{Name: sharedResource, Units: cardUnits * len(indexes)}
	for _, index := range indexes {
		own := deviceplugin.Resource{Name: cardResource(index), Units: cardUnits}
		for _, path := range sortedKeys(specs[index]) {
			d := specs[index][path]
			if count[path] == len(indexes) {
				if plan.byPath[path] == "" {
					shared.Devices = append(shared.Devices, d)
					plan.byPath[path] = sharedResource
				}
				continue
			}
			if other := plan.byPath[path]; other != "" && other != own.Name {
				return devicePlan{}, fmt.Errorf("device %s belongs to cards %s and %d but not to all", path, strings.TrimPrefix(other, cardResourcePrefix), index)
			}
			own.Devices = append(own.Devices, d)
			plan.byPath[path] = own.Name
		}
		plan.resources = append(plan.resources, own)
	}
	plan.resources = append(plan.resources, shared)
	return plan, nil
}

func sortedKeys(m map[string]deviceplugin.DeviceSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// requests are the resources a workload's devices need, and whether some
// device is not the plugin's (the pod then runs privileged).
func (p devicePlan) requests(devices []string, cards []int) (map[string]string, bool, error) {
	if len(devices) == 0 {
		return nil, false, nil
	}
	out := map[string]string{}
	uncovered := false
	for _, v := range devices {
		d, err := parseDevice(v)
		if err != nil {
			return nil, false, err
		}
		if r := p.byPath[d.HostPath]; r != "" {
			out[r] = "1"
		} else {
			uncovered = true
		}
	}
	// Every card the workload runs on is accounted for, even one whose own
	// nodes are all shared (a one-card node).
	for _, c := range cards {
		if p.has(cardResource(c)) {
			out[cardResource(c)] = "1"
		}
	}
	return out, uncovered, nil
}

func (p devicePlan) has(name string) bool {
	for _, r := range p.resources {
		if r.Name == name {
			return true
		}
	}
	return false
}

// devicePlan splits the node's card devices between the plugin's resources;
// it is worked out once, from the first answer.
func (d *Driver) devicePlan(ctx context.Context) (devicePlan, error) {
	d.planMu.Lock()
	defer d.planMu.Unlock()
	if d.plan != nil {
		return *d.plan, nil
	}
	if d.o.CardDevices == nil {
		return devicePlan{byPath: map[string]string{}}, nil
	}
	perCard, err := d.o.CardDevices(ctx)
	if err != nil {
		return devicePlan{}, err
	}
	plan, err := planDevices(perCard)
	if err != nil {
		return devicePlan{}, err
	}
	d.plan = &plan
	return plan, nil
}

// servePlugin registers the agent's device plugin when the node's cards
// have device nodes to hand out (NVIDIA's come through its runtime class).
func (d *Driver) servePlugin(ctx context.Context) {
	if d.o.PluginDir == "" {
		return
	}
	plan, err := d.devicePlan(ctx)
	if err != nil {
		d.o.Log.Warn("the cards' device nodes are unknown; instances that need them cannot start", "err", err)
		return
	}
	if len(plan.byPath) == 0 {
		return
	}
	p := &deviceplugin.Plugin{Dir: d.o.PluginDir, Log: d.o.Log}
	go p.Run(ctx, plan.resources)
}
