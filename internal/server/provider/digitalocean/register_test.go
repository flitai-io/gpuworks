package digitalocean

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestDriverIsRegisteredWithItsCapabilities(t *testing.T) {
	drv, ok := provider.Get("digitalocean")
	if !ok {
		t.Fatal("digitalocean is not registered")
	}
	want := provider.Capabilities{CanListInstances: true, CanPowerControl: true, CanInstallAgent: true, CanCreate: true, CanDelete: true, HasPricing: true}
	if got := drv.Capabilities(); got != want {
		t.Errorf("capabilities = %+v, want %+v", got, want)
	}
	var info *provider.KindInfo
	for _, k := range provider.KindInfos() {
		if k.Kind == "digitalocean" {
			info = &k
		}
	}
	if info == nil {
		t.Fatal("no kind info")
	}
	if info.Label["en"] != "DigitalOcean" || info.Label["zh"] != "DigitalOcean" || info.Homepage != "https://www.digitalocean.com" || !info.CanListClusters {
		t.Errorf("kind info = %+v", info)
	}
	var names []string
	for _, f := range info.CredentialFields {
		names = append(names, f.Name)
		if f.Name == "api_key" && (!f.Secret || !f.Required) {
			t.Errorf("api_key field = %+v", f)
		}
		if f.Name == "endpoint" && (f.Required || f.DefaultValue != "https://api.digitalocean.com") {
			t.Errorf("endpoint field = %+v", f)
		}
	}
	if len(names) != 2 || names[0] != "api_key" || names[1] != "endpoint" {
		t.Errorf("credential fields = %v", names)
	}
}

func TestInstallAgentRefusals(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{dropletJSON(1201, "booting", "new", gpuSize(), "nyc2", "", "")}
	read := func(string) ([]byte, error) { return []byte("agent"), nil }

	// A worker of a DOKS cluster joins through the cluster.
	_, err := d.InstallAgentWithReport(t.Context(), testCreds, provider.InstallRequest{
		Instance: provider.Instance{ExternalID: "1", PublicIP: "203.0.113.1", Cluster: "c-gpu"}, ServerURL: "https://s", Token: "t", ReadAgentBinary: read,
	})
	if !errors.Is(err, provider.ErrInstanceInCluster) {
		t.Errorf("cluster node: %v", err)
	}
	// No address yet, and the API has none either.
	_, err = d.InstallAgentWithReport(t.Context(), testCreds, provider.InstallRequest{
		Instance: provider.Instance{ExternalID: "1201"}, ServerURL: "https://s", Token: "t", ReadAgentBinary: read,
	})
	if err == nil {
		t.Error("an install on a droplet without an address went ahead")
	}
	if n := len(f.requests); n != 1 {
		t.Errorf("requests = %d, want the one lookup of the droplet", n)
	}
	for name, req := range map[string]provider.InstallRequest{
		"no token":  {Instance: provider.Instance{PublicIP: "203.0.113.1"}, ServerURL: "https://s", ReadAgentBinary: read},
		"no server": {Instance: provider.Instance{PublicIP: "203.0.113.1"}, Token: "t", ReadAgentBinary: read},
		"no reader": {Instance: provider.Instance{PublicIP: "203.0.113.1"}, ServerURL: "https://s", Token: "t"},
	} {
		if err := d.InstallAgent(t.Context(), testCreds, req); err == nil {
			t.Errorf("%s: install went ahead", name)
		}
	}
}
