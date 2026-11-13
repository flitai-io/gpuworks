package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// ACK (Container Service for Kubernetes) clusters of the account
// (provider.ClusterProvider), read through the ROA API:
// https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/developer-reference/api-cs-2015-12-15-dir-clusters

// ackClusterTag is the tag ACK puts on the ECS instances of a cluster's
// nodes; its value is the cluster ID.
const ackClusterTag = "ack.aliyun.com"

// ackNodePoolLabel is the label ACK puts on a node pool's nodes.
const ackNodePoolLabel = "alibabacloud.com/nodepool-id"

// ackKubeconfigMinutes is how long an issued kubeconfig is valid: one
// apply or removal needs seconds.
const ackKubeconfigMinutes = 15

// ackPageSize is the DescribeClustersV1 page size.
const ackPageSize = 50

// ackProfilesWithoutNodes are cluster profiles whose pods run on virtual
// nodes, where a DaemonSet with host access cannot run.
var ackProfilesWithoutNodes = map[string]bool{"Serverless": true, "Acs": true}

type ackCluster struct {
	ClusterID      string `json:"cluster_id"`
	Name           string `json:"name"`
	RegionID       string `json:"region_id"`
	State          string `json:"state"`
	ClusterType    string `json:"cluster_type"`
	Profile        string `json:"profile"`
	CurrentVersion string `json:"current_version"`
	// MasterURL is a JSON document in a string:
	// {"api_server_endpoint":"https://…","intranet_api_server_endpoint":"https://…"}.
	MasterURL string `json:"master_url"`
}

func (c ackCluster) publicAPI() bool {
	var m struct {
		APIServerEndpoint string `json:"api_server_endpoint"`
	}
	return json.Unmarshal([]byte(c.MasterURL), &m) == nil && strings.TrimSpace(m.APIServerEndpoint) != ""
}

type ackNodePool struct {
	NodePoolInfo struct {
		NodePoolID string `json:"nodepool_id"`
		Name       string `json:"name"`
	} `json:"nodepool_info"`
	ScalingGroup struct {
		InstanceTypes []string `json:"instance_types"`
	} `json:"scaling_group"`
	Status struct {
		TotalNodes int `json:"total_nodes"`
	} `json:"status"`
}

type ecsInstanceType struct {
	InstanceTypeID string `json:"InstanceTypeId"`
	GPUAmount      int    `json:"GPUAmount"`
	GPUSpec        string `json:"GPUSpec"`
}

// ListClusters implements provider.ClusterProvider: every ACK cluster of
// the account with real nodes, with its GPU node pools.
func (d *Driver) ListClusters(ctx context.Context, creds provider.Credentials) ([]provider.Cluster, error) {
	var all []ackCluster
	for page := 1; ; page++ {
		var resp struct {
			Clusters []ackCluster `json:"clusters"`
			PageInfo struct {
				TotalCount int `json:"total_count"`
			} `json:"page_info"`
		}
		q := map[string]string{"page_number": strconv.Itoa(page), "page_size": strconv.Itoa(ackPageSize)}
		if err := d.api.getROA(ctx, creds, csProduct, "DescribeClustersV1", "/api/v1/clusters", q, &resp); err != nil {
			return nil, fmt.Errorf("list ACK clusters: %w", err)
		}
		all = append(all, resp.Clusters...)
		if len(resp.Clusters) == 0 || len(all) >= resp.PageInfo.TotalCount {
			break
		}
	}
	types := map[string]map[string]ecsInstanceType{} // region -> type -> GPU facts
	out := make([]provider.Cluster, 0, len(all))
	for _, c := range all {
		if ackProfilesWithoutNodes[c.Profile] {
			continue
		}
		cluster := provider.Cluster{ExternalID: c.ClusterID, Name: c.Name, Region: c.RegionID, Version: c.CurrentVersion,
			State: c.State, Running: c.State == "running", PublicAPI: c.publicAPI(), GPUPools: []provider.ClusterPool{}}
		if !cluster.Running {
			out = append(out, cluster)
			continue
		}
		var pools struct {
			NodePools []ackNodePool `json:"nodepools"`
		}
		if err := d.api.getROA(ctx, creds, csProduct, "DescribeClusterNodePools", "/clusters/"+c.ClusterID+"/nodepools", nil, &pools); err != nil {
			return nil, fmt.Errorf("node pools of ACK cluster %s: %w", c.ClusterID, err)
		}
		known, err := d.instanceTypes(ctx, creds, c.RegionID, pools.NodePools, types)
		if err != nil {
			return nil, err
		}
		for _, p := range pools.NodePools {
			if pool, ok := gpuPool(p, known); ok {
				cluster.GPUPools = append(cluster.GPUPools, pool)
			}
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

// gpuPool is a node pool whose instance types all carry GPUs of one model.
// A pool without listed instance types (an auto-mode pool) is not one: what
// it will run on is not known.
func gpuPool(p ackNodePool, known map[string]ecsInstanceType) (provider.ClusterPool, bool) {
	if len(p.ScalingGroup.InstanceTypes) == 0 {
		return provider.ClusterPool{}, false
	}
	var pool provider.ClusterPool
	for i, t := range p.ScalingGroup.InstanceTypes {
		facts := known[t]
		if facts.GPUAmount == 0 {
			return provider.ClusterPool{}, false
		}
		spec := strings.TrimSpace(facts.GPUSpec)
		if i == 0 {
			pool.GPUModel, pool.GPUCount = spec, facts.GPUAmount
		} else if spec != pool.GPUModel {
			pool.GPUModel = "" // mixed models: say nothing rather than one of them
		}
	}
	vendor := gpuVendor(pool.GPUModel)
	if vendor == "" && pool.GPUModel != "" {
		return provider.ClusterPool{}, false
	}
	if vendor == "" {
		vendor = "nvidia"
	}
	pool.ID, pool.Name, pool.Nodes, pool.Vendor = p.NodePoolInfo.NodePoolID, p.NodePoolInfo.Name, p.Status.TotalNodes, vendor
	pool.LabelKey = ackNodePoolLabel
	return pool, true
}

// gpuVendor names the accelerator vendor of an ECS GPU spec ("NVIDIA L20",
// "NVIDIA A10", "PPU-ZW810E"); "" for a vendor Kubernetes nodes are not
// supported for.
func gpuVendor(spec string) string {
	s := strings.ToUpper(spec)
	switch {
	case strings.Contains(s, "NVIDIA"):
		return "nvidia"
	case strings.Contains(s, "PPU"):
		return "ppu"
	}
	return ""
}

// instanceTypes looks up the GPU facts of the pools' instance types in a
// region, caching them across clusters of one listing.
func (d *Driver) instanceTypes(ctx context.Context, creds provider.Credentials, region string, pools []ackNodePool, cache map[string]map[string]ecsInstanceType) (map[string]ecsInstanceType, error) {
	known := cache[region]
	if known == nil {
		known = map[string]ecsInstanceType{}
		cache[region] = known
	}
	var missing []string
	seen := map[string]bool{}
	for _, p := range pools {
		for _, t := range p.ScalingGroup.InstanceTypes {
			if _, ok := known[t]; !ok && !seen[t] {
				seen[t] = true
				missing = append(missing, t)
			}
		}
	}
	for len(missing) > 0 {
		batch := missing
		if len(batch) > 10 {
			batch = batch[:10]
		}
		missing = missing[len(batch):]
		params := map[string]string{}
		for i, t := range batch {
			params["InstanceTypes."+strconv.Itoa(i+1)] = t
		}
		var resp struct {
			InstanceTypes struct {
				InstanceType []ecsInstanceType `json:"InstanceType"`
			} `json:"InstanceTypes"`
		}
		if err := d.api.call(ctx, creds, ecsProduct, region, "DescribeInstanceTypes", params, &resp); err != nil {
			return nil, fmt.Errorf("instance types in %s: %w", region, err)
		}
		for _, t := range batch {
			known[t] = ecsInstanceType{InstanceTypeID: t} // listed below if it exists
		}
		for _, t := range resp.InstanceTypes.InstanceType {
			known[t.InstanceTypeID] = t
		}
	}
	return known, nil
}

// ClusterKubeconfig implements provider.ClusterProvider: a kubeconfig for
// the API server's public address, valid for ackKubeconfigMinutes.
func (d *Driver) ClusterKubeconfig(ctx context.Context, creds provider.Credentials, clusterID string) ([]byte, error) {
	var resp struct {
		Config string `json:"config"`
	}
	q := map[string]string{"PrivateIpAddress": "false", "TemporaryDurationMinutes": strconv.Itoa(ackKubeconfigMinutes)}
	if err := d.api.getROA(ctx, creds, csProduct, "DescribeClusterUserKubeconfig", "/k8s/"+clusterID+"/user_config", q, &resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.Config) == "" {
		return nil, fmt.Errorf("aliyun: ACK cluster %s returned no kubeconfig", clusterID)
	}
	return []byte(resp.Config), nil
}

// The driver lists ACK clusters.
var _ provider.ClusterProvider = (*Driver)(nil)
