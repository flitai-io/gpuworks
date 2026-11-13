package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// DigitalOcean Kubernetes (DOKS), read through
// https://docs.digitalocean.com/reference/api/digitalocean/#tag/Kubernetes

// doksPoolLabel is the label DOKS puts on every node of a node pool.
const doksPoolLabel = "doks.digitalocean.com/node-pool-id"

// kubeconfigSeconds is how long an issued kubeconfig is valid: one apply or
// removal needs seconds.
const kubeconfigSeconds = "900"

type doksNodePool struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Count int    `json:"count"`
}

type doksCluster struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	Version  string `json:"version"`
	Endpoint string `json:"endpoint"`
	Status   struct {
		State string `json:"state"`
	} `json:"status"`
	NodePools []doksNodePool `json:"node_pools"`
}

// sizeFacts is what the sizes catalog says about a GPU size.
type sizeFacts struct {
	Model  string // readable, with vendor: "NVIDIA H100"
	Count  int
	Vendor string
}

// sizeCatalog reads /v2/sizes: slug to GPU facts. The endpoint lists only
// the sizes the account may create droplets with, so a node pool's size can
// be missing from it.
func (d *Driver) sizeCatalog(ctx context.Context, creds provider.Credentials) (map[string]sizeFacts, error) {
	out := map[string]sizeFacts{}
	err := d.listAll(ctx, creds, "/v2/sizes", nil, func(body []byte) (bool, int, int, error) {
		var page struct {
			pageLinks
			Sizes []struct {
				Slug    string   `json:"slug"`
				GPUInfo *gpuInfo `json:"gpu_info"`
			} `json:"sizes"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return false, 0, 0, err
		}
		for _, s := range page.Sizes {
			if s.GPUInfo != nil && s.GPUInfo.Count > 0 {
				out[s.Slug] = sizeFacts{Model: readableModel(s.GPUInfo.Model), Count: s.GPUInfo.Count, Vendor: vendorOf(s.GPUInfo.Model)}
			}
		}
		return page.hasNext(), page.Meta.Total, len(page.Sizes), nil
	})
	return out, err
}

// poolFacts is the GPU model, count per node and vendor of a node pool size:
// from the catalog when it lists the size, else from the slug.
func poolFacts(catalog map[string]sizeFacts, size string) (sizeFacts, bool) {
	if f, ok := catalog[size]; ok {
		if f.Vendor == "" {
			if parsed, ok := parseSizeSlug(size); ok {
				f.Vendor = parsed.Vendor
			}
		}
		return f, true
	}
	if parsed, ok := parseSizeSlug(size); ok {
		prefix := "NVIDIA "
		if parsed.Vendor == "amd" {
			prefix = "AMD "
		}
		return sizeFacts{Model: prefix + parsed.Model, Count: parsed.Count, Vendor: parsed.Vendor}, true
	}
	return sizeFacts{}, false
}

// ListClusters implements provider.ClusterProvider: the account's DOKS
// clusters, each with its NVIDIA GPU node pools. AMD pools are left out:
// gpuworks Kubernetes nodes are NVIDIA, Ascend, Hygon or PPU.
func (d *Driver) ListClusters(ctx context.Context, creds provider.Credentials) ([]provider.Cluster, error) {
	var all []doksCluster
	err := d.listAll(ctx, creds, "/v2/kubernetes/clusters", nil, func(body []byte) (bool, int, int, error) {
		var page struct {
			pageLinks
			Clusters []doksCluster `json:"kubernetes_clusters"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return false, 0, 0, err
		}
		all = append(all, page.Clusters...)
		return page.hasNext(), page.Meta.Total, len(page.Clusters), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list DOKS clusters: %w", err)
	}
	var catalog map[string]sizeFacts // read once, and only when a pool needs it
	out := make([]provider.Cluster, 0, len(all))
	for _, c := range all {
		cluster := provider.Cluster{
			ExternalID: c.ID, Name: c.Name, Region: c.Region, Version: c.Version,
			State: c.Status.State, Running: c.Status.State == "running",
			PublicAPI: strings.TrimSpace(c.Endpoint) != "", GPUPools: []provider.ClusterPool{},
		}
		for _, p := range c.NodePools {
			if !isGPUSlug(p.Size) {
				continue
			}
			if catalog == nil {
				var err error
				if catalog, err = d.sizeCatalog(ctx, creds); err != nil {
					return nil, fmt.Errorf("sizes of DOKS node pools: %w", err)
				}
			}
			facts, ok := poolFacts(catalog, p.Size)
			if !ok || facts.Vendor != "nvidia" {
				continue
			}
			cluster.GPUPools = append(cluster.GPUPools, provider.ClusterPool{
				ID: p.ID, Name: p.Name, GPUModel: facts.Model, GPUCount: facts.Count,
				Nodes: p.Count, Vendor: "nvidia", LabelKey: doksPoolLabel,
			})
		}
		out = append(out, cluster)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ClusterKubeconfig implements provider.ClusterProvider: a kubeconfig valid
// for kubeconfigSeconds, with a token the API issues for it.
func (d *Driver) ClusterKubeconfig(ctx context.Context, creds provider.Credentials, clusterID string) ([]byte, error) {
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return nil, fmt.Errorf("digitalocean: cluster id is required")
	}
	body, err := d.get(ctx, creds, "/v2/kubernetes/clusters/"+url.PathEscape(clusterID)+"/kubeconfig", url.Values{"expiry_seconds": {kubeconfigSeconds}})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, fmt.Errorf("digitalocean: DOKS cluster %s returned no kubeconfig", clusterID)
	}
	return body, nil
}
