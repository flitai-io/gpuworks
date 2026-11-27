package cloud

import (
	"cmp"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

// checkHostOnboarding checks the Cloud inventory before installation starts SSH.
func checkHostOnboarding(resource inventory.CloudResource) error {
	host := cmp.Or(resource.Name, resource.ExternalID)
	params := map[string]any{"host": host}
	switch {
	case resource.Type != "host":
		params["type"] = resource.Type
		return messages.NewProductError(messages.ErrorZStackAgentHostType, fmt.Sprintf("Cannot install the agent on %s: resource type %q is not a host. Select a ZStack KVM host.", host, resource.Type), params)
	case !strings.EqualFold(resource.Hypervisor, "KVM"):
		params["hypervisor"] = resource.Hypervisor
		return messages.NewProductError(messages.ErrorZStackAgentHostHypervisor, fmt.Sprintf("Cannot install the agent on %s: hypervisor %q is unsupported. Select a ZStack KVM host.", host, resource.Hypervisor), params)
	case strings.TrimSpace(resource.ManagementAddress) == "":
		return messages.NewProductError(messages.ErrorZStackAgentHostAddressMissing, fmt.Sprintf("Cannot install the agent on %s: configure a management IP for this host in ZStack and retry.", host), params)
	case !strings.EqualFold(resource.Status, "Connected"):
		params["address"] = resource.ManagementAddress
		status := cmp.Or(resource.Status, "Unknown")
		params["status"] = status
		return messages.NewProductError(messages.ErrorZStackAgentHostNotConnected, fmt.Sprintf("Cannot install the agent: ZStack host %s (%s) has connection status %q. Reconnect the host in ZStack and retry once its status is Connected.", host, resource.ManagementAddress, status), params)
	}
	return nil
}
