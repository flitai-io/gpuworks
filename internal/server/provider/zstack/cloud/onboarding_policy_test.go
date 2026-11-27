package cloud

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

func TestEvaluateHostOnboardingSeparatesHostEligibilityFromGPUAllocation(t *testing.T) {
	tests := []struct {
		name     string
		resource inventory.CloudResource
		wantCode string
	}{
		{
			name:     "no GPU still admits host",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected"},
		},
		{
			name: "used gpu still admits host",
			resource: inventory.CloudResource{
				Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected",
				GPUs: []inventory.CloudGPU{{ExternalID: "gpu-used", State: "Enabled", Allocation: "Allocated"}},
			},
		},
		{
			name: "invalid host is rejected",
			resource: inventory.CloudResource{
				Type: "host", Hypervisor: "KVM", ManagementAddress: "", Status: "Connected",
				GPUs: []inventory.CloudGPU{{ExternalID: "gpu-free", State: "Enabled", Allocation: "Unallocated"}},
			},
			wantCode: messages.ErrorZStackAgentHostAddressMissing,
		},
		{
			name:     "disabled and allocated GPU",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected", GPUs: []inventory.CloudGPU{{State: "Disabled", Allocation: "Allocated"}}},
		},
		{
			name:     "no GPU and no address",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", Status: "Connected"},
			wantCode: messages.ErrorZStackAgentHostAddressMissing,
		},
		{
			name:     "no GPU and disconnected",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Disconnected"},
			wantCode: messages.ErrorZStackAgentHostNotConnected,
		},
		{
			name:     "no GPU and non KVM",
			resource: inventory.CloudResource{Type: "host", Hypervisor: "ESX", ManagementAddress: "192.0.2.10", Status: "Connected"},
			wantCode: messages.ErrorZStackAgentHostHypervisor,
		},
		{
			name:     "not a host",
			resource: inventory.CloudResource{Type: "vm", Hypervisor: "KVM", ManagementAddress: "192.0.2.10", Status: "Connected"},
			wantCode: messages.ErrorZStackAgentHostType,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkHostOnboarding(tt.resource)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var reason *messages.ProductError
			if !errors.As(err, &reason) || reason.Code() != tt.wantCode {
				t.Fatalf("error = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}
