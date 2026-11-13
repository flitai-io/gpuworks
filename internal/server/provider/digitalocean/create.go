package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// tagGPUWorks marks the droplets gpuworks created.
const tagGPUWorks = "gpuworks"

// Base images DigitalOcean ships for GPU droplets: NVIDIA "AI/ML Ready"
// (driver and CUDA installed), the 8-GPU one with NVLink, and the AMD one.
const (
	imageNVIDIASingle = "gpu-h100x1-base"
	imageNVIDIAMulti  = "gpu-h100x8-base"
	imageAMD          = "gpu-amd-base"
)

// defaultImage is the GPU base image for a size, "" when the size is not a
// GPU size the driver can read.
func defaultImage(size string) string {
	parsed, ok := parseSizeSlug(size)
	switch {
	case !ok:
		return ""
	case parsed.Vendor == "amd":
		return imageAMD
	case parsed.Count > 1:
		return imageNVIDIAMulti
	}
	return imageNVIDIASingle
}

// CreateInstance implements provider.Driver. The size comes from
// req.Extra["size"] (a size slug), the image from req.Image or
// req.Extra["image"], else the GPU base image for the size's vendor. The
// platform SSH key is included when AuthorizePublicKey registered it, plus
// the keys named in req.Extra["ssh_keys"] (ids or fingerprints, comma
// separated).
func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	region := strings.TrimSpace(req.Region)
	if region == "" {
		return provider.Instance{}, errors.New("digitalocean: region is required (a region slug such as nyc2)")
	}
	size := strings.TrimSpace(req.Extra["size"])
	if size == "" {
		return provider.Instance{}, errors.New(`digitalocean: the droplet size is required (Extra["size"], a slug such as gpu-h100x1-80gb)`)
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		image = strings.TrimSpace(req.Extra["image"])
	}
	if image == "" {
		if image = defaultImage(size); image == "" {
			return provider.Instance{}, fmt.Errorf("digitalocean: no default image for size %q; give an image", size)
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = tagGPUWorks
	}
	keys, err := d.sshKeysForCreate(ctx, creds, req.Extra["ssh_keys"])
	if err != nil {
		return provider.Instance{}, err
	}
	body := map[string]any{
		"name": name, "region": region, "size": size, "image": image,
		"ssh_keys": keys, "tags": []string{tagGPUWorks},
	}
	raw, err := d.do(ctx, creds, http.MethodPost, "/v2/droplets", nil, body)
	if err != nil {
		return provider.Instance{}, err
	}
	var resp struct {
		Droplet droplet `json:"droplet"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return provider.Instance{}, fmt.Errorf("digitalocean: decode created droplet: %w", err)
	}
	if resp.Droplet.ID == 0 {
		return provider.Instance{}, errors.New("digitalocean: create droplet returned no droplet id")
	}
	return resp.Droplet.toInstance(), nil
}

// sshKeysForCreate is the ssh_keys of a create: the platform key, then the
// operator's. An empty list is a droplet DigitalOcean mails a root password
// for; the operator then gives that password at install.
func (d *Driver) sshKeysForCreate(ctx context.Context, creds provider.Credentials, extra string) ([]any, error) {
	keys := []any{}
	seen := map[string]bool{}
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			keys = append(keys, id) // a key id is a number, a fingerprint a string
			return
		}
		keys = append(keys, ref)
	}
	fingerprint, err := d.platformKeyFingerprint(ctx, creds)
	if err != nil {
		return nil, err
	}
	add(fingerprint)
	for _, ref := range strings.Split(extra, ",") {
		add(ref)
	}
	return keys, nil
}
