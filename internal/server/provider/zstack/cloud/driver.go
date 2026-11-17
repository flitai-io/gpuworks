// Package cloud implements the ZStack Cloud provider.
package cloud

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func init() { provider.Register(New()) }

// Driver is the ZStack Cloud provider driver.
type Driver struct {
	store          *store.Store
	installer      provision.Installer
	resourceClaims *resourceclaim.Service
	clientOptions  []apiclient.ClientOption
	diagnostics    *endpointDiagnostics
}

// New returns a ZStack Cloud driver.
func New() *Driver { return &Driver{} }

// NewWithClientOptions configures Cloud communication without adding
// platform-specific settings to the shared provider or controller contracts.
func NewWithClientOptions(options ...apiclient.ClientOption) (*Driver, error) {
	if err := apiclient.ValidateOptions(options...); err != nil {
		return nil, err
	}
	return &Driver{clientOptions: slices.Clone(options)}, nil
}

func (d *Driver) newClient(creds provider.Credentials) (*apiclient.APIClient, error) {
	return apiclient.New(creds, d.clientOptions...)
}

func (d *Driver) Name() string { return "zstack" }

func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{CanListInstances: true, CanInstallAgent: true}
}

func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	if strings.TrimSpace(creds.Endpoint) == "" {
		return fmt.Errorf("zstack: endpoint is required")
	}
	client, err := d.newClient(creds)
	if err != nil {
		return err
	}
	apiCtx, cancel := client.OperationContext(ctx)
	defer cancel()
	_, err = apiclient.QueryAll[hostInventory](apiCtx, client, "/hosts")
	return d.validationResult(ctx, creds.Endpoint, err)
}

func (d *Driver) validationResult(ctx context.Context, endpoint string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var diagnosis *endpointDiagnosis
	if needsEndpointDiagnosis(err) {
		checks := d.diagnostics
		if checks == nil {
			checks = defaultEndpointDiagnostics()
		}
		diagnosis = checks.run(ctx, endpoint)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return credentialError(err, diagnosis)
}

func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	resources, err := d.DiscoverResources(ctx, creds)
	if err != nil {
		return nil, err
	}
	instances := make([]provider.Instance, 0, len(resources))
	for _, resource := range resources {
		instances = append(instances, cloudHostInstance(resource))
	}
	return instances, nil
}

func cloudHostInstance(resource inventory.CloudResource) provider.Instance {
	address := strings.TrimSpace(resource.ManagementAddress)
	user, port := strings.TrimSpace(resource.SSHUser), resource.SSHPort
	if user == "" {
		user = "root"
	}
	if port == 0 {
		port = 22
	}
	connections := []provider.SSHConnection{}
	if address != "" {
		connections = append(connections, provider.SSHConnection{
			ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"),
			Host: address, Port: port, User: user, InstallSupported: true,
		})
	}
	return provider.Instance{
		ExternalID:     resource.ExternalID,
		Name:           resource.Name,
		GPUCount:       len(resource.GPUs),
		GPUModel:       cloudHostGPUModels(resource.GPUs),
		Power:          cloudHostPowerState(resource),
		PrivateIP:      address,
		PublicIP:       address,
		SSHPort:        port,
		SSHUser:        user,
		SSHConnections: connections,
	}
}

func cloudHostPowerState(resource inventory.CloudResource) provider.PowerState {
	values := []string{resource.State, resource.Status}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "disabled", "stopped", "offline", "disconnected", "error":
			return provider.PowerStopped
		case "enabled", "running", "online", "connected", "available", "ready":
			return provider.PowerRunning
		}
	}
	return provider.PowerUnknown
}

func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	return provider.PowerUnknown, provider.ErrUnsupported
}

func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) ResizeInstance(ctx context.Context, creds provider.Credentials, externalID string, req provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

func cloudHostGPUModels(gpus []inventory.CloudGPU) string {
	seen := make(map[string]bool, len(gpus))
	var models []string
	for _, gpu := range gpus {
		if model := strings.TrimSpace(gpu.Model); model != "" && !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	slices.Sort(models)
	return strings.Join(models, ", ")
}
