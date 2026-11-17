package facade

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

const pciDevicePath = "/pci-device/pci-devices"

// PciDeviceType.gpuDeviceTypes, including accelerator classes added after 4.8.
var pciGPUTypes = []string{
	"GPU_Video_Controller", "GPU_3D_Controller", "GPU_Processing_Accelerators", "GPU_Co_Processor",
}

// PciDeviceInventory and PciDeviceType in Cloud describe physical GPUs
// and their auxiliary functions separately; only compute/display devices apply.
type pciInventory struct {
	UUID        string `json:"uuid"`
	HostUUID    string `json:"hostUuid"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PCIAddress  string `json:"pciDeviceAddress"`
	Type        string `json:"type"`
	State       string `json:"state"`
	Status      string `json:"status"`
	VMUUID      string `json:"vmInstanceUuid"`
	ParentUUID  string `json:"parentUuid"`
	VirtStatus  string `json:"virtStatus"`
}

type PCIGPUQuery struct{ client *apiclient.APIClient }

func (q *PCIGPUQuery) List(ctx context.Context) ([]inventory.GPUDevice, error) {
	filter := url.Values{"q": {"type?=" + strings.Join(pciGPUTypes, ",")}}
	devices, err := apiclient.QueryAll[pciInventory](ctx, q.client, pciDevicePath+"?"+filter.Encode())
	if err != nil {
		return nil, err
	}
	out := make([]inventory.GPUDevice, 0)
	for _, device := range devices {
		if isGPU(device) && device.ParentUUID == "" {
			out = append(out, pciGPU(device))
		}
	}
	return out, nil
}

func (q *PCIGPUQuery) Get(ctx context.Context, uuid string) (inventory.GPUDevice, error) {
	device, err := apiclient.QueryResource[pciInventory](ctx, q.client, pciDevicePath, uuid)
	if err != nil {
		return inventory.GPUDevice{}, err
	}
	if !isGPU(device) {
		return inventory.GPUDevice{}, fmt.Errorf("zstack: queried PCI device is not a GPU")
	}
	return pciGPU(device), nil
}

func isGPU(device pciInventory) bool {
	return slices.Contains(pciGPUTypes, device.Type)
}

func pciGPU(device pciInventory) inventory.GPUDevice {
	return inventory.GPUDevice{HostUUID: device.HostUUID, GPU: inventory.CloudGPU{
		ExternalID: device.UUID, PCIAddress: device.PCIAddress,
		Model: strings.TrimSpace(device.Name), Description: device.Description,
		State: device.State, Status: device.Status, VMUUID: device.VMUUID,
		Allocation: pciAllocation(device),
	}}
}

func pciAllocation(device pciInventory) string {
	if device.VMUUID != "" || device.Status == "Attached" || device.Status == "Reserved" {
		return "Allocated"
	}
	if device.ParentUUID != "" {
		return ""
	}
	switch device.VirtStatus {
	case "UNVIRTUALIZABLE", "SRIOV_VIRTUALIZABLE", "VFIO_MDEV_VIRTUALIZABLE":
		if device.Status == "Active" || device.Status == "System" {
			return "Unallocated"
		}
	}
	return ""
}
