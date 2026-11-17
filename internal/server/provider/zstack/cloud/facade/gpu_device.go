package facade

import (
	"context"
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

const gpuDevicePath = "/gpu-device/gpu-devices"

type GPUDeviceQuery struct{ client *apiclient.APIClient }

func (q *GPUDeviceQuery) List(ctx context.Context) ([]inventory.GPUDevice, error) {
	gpus, err := apiclient.QueryAll[gpuInventory](ctx, q.client, gpuDevicePath)
	if err != nil {
		return nil, err
	}
	out := make([]inventory.GPUDevice, 0, len(gpus))
	for _, g := range gpus {
		if g.ParentUUID != "" {
			continue
		}
		out = append(out, inventory.GPUDevice{HostUUID: g.HostUUID, GPU: cloudGPU(g)})
	}
	return out, nil
}

func (q *GPUDeviceQuery) Get(ctx context.Context, uuid string) (inventory.GPUDevice, error) {
	g, err := apiclient.QueryResource[gpuInventory](ctx, q.client, gpuDevicePath, uuid)
	if err != nil {
		return inventory.GPUDevice{}, err
	}
	return inventory.GPUDevice{HostUUID: g.HostUUID, GPU: cloudGPU(g)}, nil
}

type gpuInventory struct {
	pciInventory

	GPUType string `json:"gpuType"`
	Device  string `json:"device"`
	Mode    string `json:"mode"`

	Allocation gpuAllocation `json:"allocateStatus"`
}

// An absent field permits the PCI interpretation; explicit null/empty does not.
type gpuAllocation struct {
	present bool
	value   string
}

func (a *gpuAllocation) UnmarshalJSON(data []byte) error {
	a.present, a.value = true, ""
	return json.Unmarshal(data, &a.value)
}

func cloudGPU(g gpuInventory) inventory.CloudGPU {
	model := strings.TrimSpace(g.GPUType)
	if model == "" {
		model = strings.TrimSpace(g.Device)
	}
	allocation := g.Allocation.value
	if !g.Allocation.present {
		// GpuDeviceInventory inherits these PCI fields in intermediate releases.
		if isGPU(g.pciInventory) {
			allocation = pciAllocation(g.pciInventory)
		}
		if model == "" {
			model = strings.TrimSpace(g.Name)
		}
	}
	return inventory.CloudGPU{
		Model: model, Description: g.Description, ExternalID: g.UUID,
		PCIAddress: g.PCIAddress, State: g.State, Status: g.Status,
		Allocation: allocation, Mode: g.Mode, VMUUID: g.VMUUID,
	}
}
