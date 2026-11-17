package cloud

import (
	"context"
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func (d *Driver) prepareHostForAgent(ctx context.Context, creds provider.Credentials, hostID, nodeToken string) error {
	resources, err := d.DiscoverResources(ctx, creds)
	if err != nil {
		return err
	}
	for _, resource := range resources {
		if resource.ExternalID != hostID {
			continue
		}
		if evaluateHostOnboarding(resource) != hostOnboardingCandidate {
			return fmt.Errorf("Cloud host is not eligible for agent installation")
		}
		node, err := d.store.NodeByTokenHash(store.HashToken(nodeToken))
		if err != nil {
			return err
		}
		instance, err := d.store.ProviderInstanceForNode(node.ID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(resource)
		if err != nil {
			return err
		}
		return d.store.CloudLedger().SaveCloudHostSnapshot(cloudstore.CloudHost{
			ProviderID: instance.ProviderID, ExternalID: resource.ExternalID, NodeUUID: node.UUID,
			Snapshot: string(raw), InitialCPUReserved: resource.CPU.Reserved, InitialMemoryReserved: resource.Memory.Reserved,
		})
	}
	return fmt.Errorf("Cloud host not found")
}
