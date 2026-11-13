package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// droplet is the part of a droplet object the driver reads.
type droplet struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	SizeSlug string   `json:"size_slug"`
	Tags     []string `json:"tags"`
	Size     *struct {
		Slug        string   `json:"slug"`
		PriceHourly float64  `json:"price_hourly"`
		GPUInfo     *gpuInfo `json:"gpu_info"`
	} `json:"size"`
	Region struct {
		Slug string `json:"slug"`
	} `json:"region"`
	Networks struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

// slug is the droplet's size, from the embedded size object or size_slug.
func (dr droplet) slug() string {
	if dr.Size != nil && dr.Size.Slug != "" {
		return dr.Size.Slug
	}
	return dr.SizeSlug
}

// gpu is the droplet's GPU facts: the size object's gpu_info, else what the
// slug says. ok is false for a droplet without GPUs.
func (dr droplet) gpu() (model string, count int, ok bool) {
	if dr.Size != nil && dr.Size.GPUInfo != nil && dr.Size.GPUInfo.Count > 0 {
		return readableModel(dr.Size.GPUInfo.Model), dr.Size.GPUInfo.Count, true
	}
	if isGPUSlug(dr.slug()) {
		if parsed, ok := parseSizeSlug(dr.slug()); ok {
			if parsed.Vendor == "nvidia" {
				return "NVIDIA " + parsed.Model, parsed.Count, true
			}
			return "AMD " + parsed.Model, parsed.Count, true
		}
		return "", 0, true // a GPU size the driver cannot name
	}
	return "", 0, false
}

func (dr droplet) power() provider.PowerState {
	switch dr.Status {
	case "active":
		return provider.PowerRunning
	case "off", "archive":
		return provider.PowerStopped
	}
	return provider.PowerUnknown // "new" and anything the API adds
}

// clusterID is the DOKS cluster a worker droplet belongs to: DOKS tags its
// workers "k8s", "k8s:worker" and "k8s:<cluster uuid>".
func (dr droplet) clusterID() string {
	for _, tag := range dr.Tags {
		if id, ok := strings.CutPrefix(tag, "k8s:"); ok && id != "" && id != "worker" {
			return id
		}
	}
	return ""
}

func (dr droplet) toInstance() provider.Instance {
	model, count, _ := dr.gpu()
	inst := provider.Instance{
		ExternalID: strconv.FormatInt(dr.ID, 10),
		Name:       dr.Name,
		GPUModel:   model,
		GPUCount:   count,
		Power:      dr.power(),
		Region:     dr.Region.Slug,
		SSHPort:    22,
		SSHUser:    "root",
		Kind:       "vm",
		Cluster:    dr.clusterID(),
	}
	if dr.Size != nil {
		inst.PricePerHr = dr.Size.PriceHourly
	}
	for _, n := range dr.Networks.V4 {
		switch n.Type {
		case "public":
			if inst.PublicIP == "" {
				inst.PublicIP = n.IPAddress
			}
		case "private":
			if inst.PrivateIP == "" {
				inst.PrivateIP = n.IPAddress
			}
		}
	}
	if inst.PublicIP != "" {
		inst.SSHConnections = []provider.SSHConnection{{
			ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"),
			Host: inst.PublicIP, Port: 22, User: "root", InstallSupported: true,
		}}
		provider.SetPreferredSSHConnection(&inst)
	}
	return inst
}

// ListInstances implements provider.Driver: the account's GPU droplets (the
// account may also hold plain droplets, which gpuworks has no use for).
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	var out []provider.Instance
	err := d.listAll(ctx, creds, "/v2/droplets", nil, func(body []byte) (bool, int, int, error) {
		var page struct {
			pageLinks
			Droplets []droplet `json:"droplets"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return false, 0, 0, err
		}
		for _, dr := range page.Droplets {
			if _, _, isGPU := dr.gpu(); isGPU {
				out = append(out, dr.toInstance())
			}
		}
		return page.hasNext(), page.Meta.Total, len(page.Droplets), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (d *Driver) getDroplet(ctx context.Context, creds provider.Credentials, externalID string) (droplet, error) {
	id, err := dropletID(externalID)
	if err != nil {
		return droplet{}, err
	}
	var resp struct {
		Droplet droplet `json:"droplet"`
	}
	if err := d.getJSON(ctx, creds, "/v2/droplets/"+id, nil, &resp); err != nil {
		return droplet{}, err
	}
	if resp.Droplet.ID == 0 {
		return droplet{}, fmt.Errorf("digitalocean: droplet %s: response has no droplet", id)
	}
	return resp.Droplet, nil
}

// dropletID checks an external id: droplet ids are decimal numbers, and the
// id becomes part of a URL path.
func dropletID(externalID string) (string, error) {
	id := strings.TrimSpace(externalID)
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
		return "", fmt.Errorf("digitalocean: %q is not a droplet id", externalID)
	}
	return id, nil
}

// GetInstance implements provider.InstanceGetter.
func (d *Driver) GetInstance(ctx context.Context, creds provider.Credentials, externalID string) (provider.Instance, error) {
	dr, err := d.getDroplet(ctx, creds, externalID)
	if err != nil {
		return provider.Instance{}, err
	}
	return dr.toInstance(), nil
}

// PowerState implements provider.Driver.
func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	dr, err := d.getDroplet(ctx, creds, externalID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return dr.power(), nil
}

// PowerOn implements provider.Driver.
func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	return d.dropletAction(ctx, creds, externalID, "power_on")
}

// PowerOff implements provider.Driver. A powered-off droplet is still billed
// by DigitalOcean; DeleteInstance is what ends the charge.
func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	return d.dropletAction(ctx, creds, externalID, "power_off")
}

func (d *Driver) dropletAction(ctx context.Context, creds provider.Credentials, externalID, action string) error {
	id, err := dropletID(externalID)
	if err != nil {
		return err
	}
	_, err = d.do(ctx, creds, http.MethodPost, "/v2/droplets/"+id+"/actions", nil, map[string]string{"type": action})
	return err
}

// DeleteInstance implements provider.Driver. A droplet that is already gone
// is deleted.
func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	id, err := dropletID(externalID)
	if err != nil {
		return err
	}
	if _, err := d.do(ctx, creds, http.MethodDelete, "/v2/droplets/"+id, nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}
