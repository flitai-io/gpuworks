// Package digitalocean implements the DigitalOcean cloud-provider driver:
// GPU droplets (list, power, create, delete, agent install over SSH) and
// DigitalOcean Kubernetes (DOKS) clusters (provider.ClusterProvider).
//
// The API (https://docs.digitalocean.com/reference/api/) is REST under
// https://api.digitalocean.com, authenticated with a personal access token
// as a bearer token. A droplet is a VM with a public address, so the agent
// is installed over SSH like on any other VM.
package digitalocean

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func init() {
	provider.Register(New())
}

// Driver is the DigitalOcean provider driver.
type Driver struct {
	baseURL string
	client  httpDoer
	// sleep waits between repeated requests; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
	// platformKeys remembers, per token, the account SSH key that holds the
	// platform public key (AuthorizePublicKey), so a droplet created next
	// is given that one even if it was uploaded under another name.
	platformKeys platformKeyCache
}

// New returns a driver pointed at the real API.
func New() *Driver {
	return &Driver{
		baseURL: provider.DigitalOceanEndpoint,
		client:  newHTTPClient(),
		sleep:   sleepContext,
	}
}

// Name implements provider.Driver.
func (d *Driver) Name() string { return "digitalocean" }

// Capabilities implements provider.Driver.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  true,
		CanInstallAgent:  true,
		CanCreate:        true,
		CanDelete:        true,
		HasPricing:       true,
	}
}

// ValidateCredentials implements provider.Driver: the account endpoint is
// the cheapest call that needs only the token.
func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	_, err := d.get(ctx, creds, "/v2/account", nil)
	return err
}

// ResizeInstance implements provider.Driver.
func (d *Driver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

// The driver registers the optional capabilities it has.
var (
	_ provider.Driver              = (*Driver)(nil)
	_ provider.ReportingDriver     = (*Driver)(nil)
	_ provider.InstanceGetter      = (*Driver)(nil)
	_ provider.PublicKeyAuthorizer = (*Driver)(nil)
	_ provider.ClusterProvider     = (*Driver)(nil)
)
