package cloud

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
)

// ApplyResourceMutation implements the first ZStack resource operations. CPU
// and memory are deltas over the current effective ResourceConfig; GPU changes
// use the PCI device state action. Cross-system query/write races remain an
// explicit limitation of this first version.
func (d *Driver) ApplyResourceMutation(ctx context.Context, creds provider.Credentials, mutation resourceclaim.ResourceMutation) error {
	c, err := d.newClient(creds)
	if err != nil {
		return err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	switch strings.ToLower(strings.TrimSpace(mutation.ResourceType)) {
	case "cpu":
		return updateReservation(ctx, c, mutation.ResourceID, "reservedCpu", mutation.Delta, false)
	case "memory":
		return updateReservation(ctx, c, mutation.ResourceID, "reservedMemory", mutation.Delta, true)
	case "gpu":
		state := strings.TrimSpace(mutation.DesiredState)
		if state != "Enabled" && state != "Disabled" {
			return fmt.Errorf("zstack: GPU state must be Enabled or Disabled")
		}
		update := map[string]any{"state": state}
		if mutation.Description != nil {
			update["description"] = *mutation.Description
		}
		return c.PutAction(ctx, "/pci-device/pci-devices/"+pathEscape(mutation.ResourceID)+"/actions", map[string]any{
			"updatePciDevice": update,
		})
	default:
		return fmt.Errorf("zstack: unsupported resource type")
	}
}

// InspectResource reads a resource after an interrupted write. The result is
// deliberately read-only so reconciliation never repeats an uncertain delta.
func (d *Driver) InspectResource(ctx context.Context, creds provider.Credentials, mutation resourceclaim.ResourceMutation) (resourceclaim.ResourceObservation, error) {
	c, err := d.newClient(creds)
	if err != nil {
		return resourceclaim.ResourceObservation{}, err
	}
	ctx, cancel := c.OperationContext(ctx)
	defer cancel()
	switch strings.ToLower(strings.TrimSpace(mutation.ResourceType)) {
	case "cpu", "memory":
		if strings.EqualFold(mutation.ResourceType, "cpu") {
			reservation, err := newAccess(c).cpu.Get(ctx, mutation.ResourceID)
			if err != nil {
				return resourceclaim.ResourceObservation{}, err
			}
			return resourceclaim.ResourceObservation{ResourceType: "cpu", ResourceID: mutation.ResourceID, Amount: reservation.Value}, nil
		}
		name := "reservedCpu"
		memory := false
		if strings.EqualFold(mutation.ResourceType, "memory") {
			name, memory = "reservedMemory", true
		}
		var config struct {
			Value *string `json:"value"`
		}
		if err := c.Get(ctx, "/resource-configurations/"+pathEscape(mutation.ResourceID)+"/kvm/"+name, &config); err != nil {
			return resourceclaim.ResourceObservation{}, err
		}
		if config.Value == nil {
			return resourceclaim.ResourceObservation{ResourceType: strings.ToLower(mutation.ResourceType), ResourceID: mutation.ResourceID, Amount: 0}, nil
		}
		amount := int64(0)
		if memory {
			amount, err = memoryBytes(*config.Value)
		} else {
			amount, err = strconv.ParseInt(strings.TrimSpace(*config.Value), 10, 64)
		}
		if err != nil || amount < 0 {
			return resourceclaim.ResourceObservation{}, fmt.Errorf("zstack: invalid current resource reservation")
		}
		return resourceclaim.ResourceObservation{ResourceType: strings.ToLower(mutation.ResourceType), ResourceID: mutation.ResourceID, Amount: amount}, nil
	case "gpu":
		device, err := newAccess(c).gpus.Get(ctx, mutation.ResourceID)
		if err != nil {
			return resourceclaim.ResourceObservation{}, err
		}
		if device.GPU.ExternalID != mutation.ResourceID {
			return resourceclaim.ResourceObservation{}, fmt.Errorf("zstack: queried GPU identity does not match the target")
		}
		return resourceclaim.ResourceObservation{ResourceType: "gpu", ResourceID: mutation.ResourceID, State: device.GPU.State, Description: device.GPU.Description}, nil
	default:
		return resourceclaim.ResourceObservation{}, fmt.Errorf("zstack: unsupported resource type")
	}
}

func updateReservation(ctx context.Context, c *apiclient.APIClient, hostID, name string, delta int64, memory bool) error {
	if hostID == "" || delta == 0 {
		return fmt.Errorf("zstack: resource host and non-zero delta are required")
	}
	var config struct {
		Value *string `json:"value"`
	}
	if !memory {
		reservation, err := newAccess(c).cpu.Get(ctx, hostID)
		if err != nil {
			return err
		}
		if !reservation.HostOverrideAvailable {
			// A global kvm.reservedCpu value is a shared baseline. Do not write
			// a host-specific lease into it; the GPUWorks lease ledger remains
			// authoritative when this Cloud cannot bind the config to a host.
			return nil
		}
		current := reservation.Value
		if delta > 0 && current > (1<<63-1)-delta || delta < 0 && current+delta < 0 {
			return fmt.Errorf("zstack: resource reservation overflow")
		}
		target := current + delta
		writeErr := c.PutAction(ctx, "/resource-configurations/kvm/reservedCpu/"+pathEscape(hostID)+"/actions", map[string]any{
			"updateResourceConfig": map[string]any{"resourceUuid": hostID, "category": "kvm", "name": "reservedCpu", "value": strconv.FormatInt(target, 10)},
		})
		if facade.IsMissingResourceConfig(writeErr) {
			return nil
		}
		return writeErr
	}
	if err := c.Get(ctx, "/resource-configurations/"+pathEscape(hostID)+"/kvm/"+name, &config); err != nil {
		return err
	}
	current := int64(0)
	if config.Value != nil {
		var parseErr error
		if memory {
			current, parseErr = memoryBytes(*config.Value)
		} else {
			current, parseErr = strconv.ParseInt(strings.TrimSpace(*config.Value), 10, 64)
		}
		if parseErr != nil || current < 0 {
			return fmt.Errorf("zstack: invalid current resource reservation")
		}
	}
	if delta > 0 && current > (1<<63-1)-delta || delta < 0 && current+delta < 0 {
		return fmt.Errorf("zstack: resource reservation overflow")
	}
	target := current + delta
	value := strconv.FormatInt(target, 10)
	if memory {
		value = formatMemory(target)
	}
	writeErr := c.PutAction(ctx, "/resource-configurations/kvm/"+name+"/"+pathEscape(hostID)+"/actions", map[string]any{
		"updateResourceConfig": map[string]any{"resourceUuid": hostID, "category": "kvm", "name": name, "value": value},
	})
	return writeErr
}

func pathEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "%", "%25"), "/", "%2F")
}

func formatMemory(value int64) string {
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	} {
		if value >= unit.factor && value%unit.factor == 0 {
			return strconv.FormatInt(value/unit.factor, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10) + "B"
}
