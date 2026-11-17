package cloud

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

// The wire names below follow ZStack's HostInventory. Missing capacities remain unknown.
type hostInventory struct {
	UUID            string `json:"uuid"`
	Name            string `json:"name"`
	ManagementIP    string `json:"managementIp"`
	Username        string `json:"username"`
	SSHPort         int    `json:"sshPort"`
	Hypervisor      string `json:"hypervisorType"`
	State           string `json:"state"`
	Status          string `json:"status"`
	TotalCPU        *int64 `json:"totalCpuCapacity"`
	AvailableCPU    *int64 `json:"availableCpuCapacity"`
	TotalMemory     *int64 `json:"totalMemoryCapacity"`
	AvailableMemory *int64 `json:"availableMemoryCapacity"`
}

func (d *Driver) DiscoverResources(ctx context.Context, creds provider.Credentials) ([]inventory.CloudResource, error) {
	c, err := d.newClient(creds)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	hosts, err := apiclient.QueryAll[hostInventory](ctx, c, "/hosts?q=hypervisorType=KVM")
	if err != nil {
		return nil, err
	}
	access := newAccess(c)
	gpus, err := access.gpus.List(ctx)
	if err != nil {
		return nil, err
	}
	byHost := map[string][]inventory.CloudGPU{}
	for _, g := range gpus {
		if g.GPU.ExternalID == "" || g.HostUUID == "" {
			return nil, fmt.Errorf("zstack: GPU inventory lacks identity")
		}
		byHost[g.HostUUID] = append(byHost[g.HostUUID], g.GPU)
	}
	resources := make([]inventory.CloudResource, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		if h.UUID == "" || seen[h.UUID] {
			return nil, fmt.Errorf("zstack: missing or duplicate host identity")
		}
		seen[h.UUID] = true
		var reserved, reservedCPU *int64
		if h.Hypervisor == "KVM" {
			var config struct {
				Value *string `json:"value"`
			}
			if err := c.Get(ctx, "/resource-configurations/"+url.PathEscape(h.UUID)+"/kvm/reservedMemory", &config); err != nil {
				return nil, err
			}
			if config.Value != nil {
				value, err := memoryBytes(*config.Value)
				if err != nil {
					return nil, err
				}
				reserved = &value
			}
			cpuReservation, err := access.cpu.Get(ctx, h.UUID)
			if err != nil {
				return nil, fmt.Errorf("zstack: host CPU reservation query failed: %w", err)
			}
			reservedCPU = &cpuReservation.Value
		}
		devices := byHost[h.UUID]
		if devices == nil {
			devices = []inventory.CloudGPU{}
		}
		resources = append(resources, inventory.CloudResource{
			Type: "host", ExternalID: h.UUID, Name: h.Name, ManagementAddress: h.ManagementIP,
			SSHUser: h.Username, SSHPort: h.SSHPort,
			Hypervisor: h.Hypervisor, State: h.State, Status: h.Status,
			CPU:    inventory.ResourceCapacity{Total: h.TotalCPU, Available: h.AvailableCPU, Reserved: reservedCPU, Unit: "vcpu"},
			Memory: inventory.ResourceCapacity{Total: h.TotalMemory, Available: h.AvailableMemory, Reserved: reserved, Unit: "bytes"}, GPUs: devices,
		})
	}
	return resources, nil
}

func (d *Driver) QueryHostResources(ctx context.Context, creds provider.Credentials, hostID string, gpuIDs []string) (inventory.CloudResource, error) {
	if strings.TrimSpace(hostID) == "" || len(gpuIDs) == 0 {
		return inventory.CloudResource{}, fmt.Errorf("zstack: target host and GPUs are required")
	}
	seen := make(map[string]bool, len(gpuIDs))
	for _, id := range gpuIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return inventory.CloudResource{}, fmt.Errorf("zstack: target GPU IDs must be non-empty and unique")
		}
		seen[id] = true
	}
	c, err := d.newClient(creds)
	if err != nil {
		return inventory.CloudResource{}, err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	h, err := apiclient.QueryResource[hostInventory](ctx, c, "/hosts", hostID)
	if err != nil {
		return inventory.CloudResource{}, err
	}
	if h.UUID != hostID {
		return inventory.CloudResource{}, fmt.Errorf("zstack: queried host identity does not match the target")
	}
	resource := inventory.CloudResource{
		Type: "host", ExternalID: h.UUID, Name: h.Name, ManagementAddress: h.ManagementIP,
		Hypervisor: h.Hypervisor, State: h.State, Status: h.Status,
		CPU:    inventory.ResourceCapacity{Total: h.TotalCPU, Available: h.AvailableCPU, Unit: "vcpu"},
		Memory: inventory.ResourceCapacity{Total: h.TotalMemory, Available: h.AvailableMemory, Unit: "bytes"},
		GPUs:   make([]inventory.CloudGPU, 0, len(gpuIDs)),
	}
	access := newAccess(c)
	for _, id := range gpuIDs {
		g, err := access.gpus.Get(ctx, id)
		if err != nil {
			return inventory.CloudResource{}, err
		}
		if g.GPU.ExternalID != id || g.HostUUID != hostID {
			return inventory.CloudResource{}, fmt.Errorf("zstack: queried GPU identity or host does not match the target")
		}
		resource.GPUs = append(resource.GPUs, g.GPU)
	}
	return resource, nil
}

func memoryBytes(raw string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	multiplier := int64(1)
	for suffix, factor := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "B": 1} {
		if strings.HasSuffix(s, suffix) {
			multiplier = factor
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("zstack: invalid memory reservation")
	}
	return v * multiplier, nil
}
