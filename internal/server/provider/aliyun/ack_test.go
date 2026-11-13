package aliyun

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// ackClusterJSON is shaped like a DescribeClustersV1 item (2026-10-22, fields
// the driver does not read trimmed).
func ackClusterJSON(id, name, region, state, profile, publicAPI string) map[string]any {
	return map[string]any{
		"cluster_id": id, "name": name, "region_id": region, "state": state, "cluster_type": "ManagedKubernetes", "profile": profile,
		"cluster_spec": "ack.pro.small", "init_version": "1.35.7-aliyun.1", "current_version": "1.35.7-aliyun.1", "size": 2,
		"master_url": `{"api_server_endpoint":"` + publicAPI + `","intranet_api_server_endpoint":"https://10.16.114.162:6443"}`,
		"tags":       []map[string]string{{"key": "ack.aliyun.com", "value": id}},
	}
}

// ackNodePoolJSON is shaped like a DescribeClusterNodePools item.
func ackNodePoolJSON(id, name string, types []string, nodes int) map[string]any {
	var instanceTypes any
	if types != nil {
		instanceTypes = types
	}
	return map[string]any{
		"nodepool_info":     map[string]any{"nodepool_id": id, "name": name, "type": "ess", "is_default": false},
		"scaling_group":     map[string]any{"scaling_group_id": "asg-1", "instance_types": instanceTypes, "system_disk_size": 120},
		"status":            map[string]any{"total_nodes": nodes, "healthy_nodes": nodes, "state": "active"},
		"kubernetes_config": map[string]any{"labels": []any{}},
	}
}

func ackFake(t *testing.T, clusters [][]map[string]any, pools map[string][]map[string]any) (*Driver, *fakeAliyun) {
	t.Helper()
	total := 0
	for _, page := range clusters {
		total += len(page)
	}
	return newFake(t, func(c fakeCall) (int, any) {
		switch {
		case c.action == "DescribeClustersV1" && c.method == http.MethodGet && c.path == "/api/v1/clusters" && c.host == "cs.aliyuncs.com":
			page := 1
			if c.query["page_number"] == "2" {
				page = 2
			}
			if page > len(clusters) {
				return 200, map[string]any{"clusters": []any{}, "page_info": map[string]any{"page_number": page, "page_size": 50, "total_count": total}}
			}
			return 200, map[string]any{"clusters": clusters[page-1], "page_info": map[string]any{"page_number": page, "page_size": 50, "total_count": total}}
		case c.action == "DescribeClusterNodePools" && c.method == http.MethodGet && strings.HasPrefix(c.path, "/clusters/"):
			id := strings.TrimSuffix(strings.TrimPrefix(c.path, "/clusters/"), "/nodepools")
			p, ok := pools[id]
			if !ok {
				return 404, map[string]any{"code": "ErrorClusterNotFound", "message": "cluster (" + id + ") not found in our records", "requestId": "req-np", "status": 404}
			}
			return 200, map[string]any{"nodepools": p}
		case c.action == "DescribeInstanceTypes":
			var out []map[string]any
			for k, v := range c.query {
				if !strings.HasPrefix(k, "InstanceTypes.") {
					continue
				}
				switch v {
				case "ecs.gn8is.2xlarge":
					out = append(out, map[string]any{"InstanceTypeId": v, "GPUAmount": 1, "GPUSpec": "NVIDIA L20", "CpuCoreCount": 8})
				case "ecs.gn8is.4xlarge":
					out = append(out, map[string]any{"InstanceTypeId": v, "GPUAmount": 1, "GPUSpec": "NVIDIA L20", "CpuCoreCount": 16})
				case "ecs.gn7i-c8g1.2xlarge":
					out = append(out, map[string]any{"InstanceTypeId": v, "GPUAmount": 1, "GPUSpec": "NVIDIA A10", "CpuCoreCount": 8})
				case "ecs.gp7-1x.12xlarge":
					out = append(out, map[string]any{"InstanceTypeId": v, "GPUAmount": 1, "GPUSpec": "PPU-ZW810E", "CpuCoreCount": 12})
				case "ecs.g7.xlarge":
					out = append(out, map[string]any{"InstanceTypeId": v, "GPUAmount": 0, "GPUSpec": "", "CpuCoreCount": 4})
				}
			}
			return 200, map[string]any{"InstanceTypes": map[string]any{"InstanceType": out}, "RequestId": "req-types"}
		case c.action == "DescribeClusterUserKubeconfig" && c.method == http.MethodGet:
			if c.path != "/k8s/c-gpu/user_config" {
				return 404, map[string]any{"code": "ErrorClusterNotFound", "message": "cluster not found", "requestId": "req-kc", "status": 404}
			}
			if c.query["PrivateIpAddress"] != "false" || c.query["TemporaryDurationMinutes"] != "15" {
				return 400, map[string]any{"code": "BadRequest", "message": "unexpected parameters", "requestId": "req-kc", "status": 400}
			}
			return 200, map[string]any{"config": "apiVersion: v1\nkind: Config\n", "expiration": "2026-10-22T13:00:00Z"}
		}
		t.Errorf("unexpected call %+v", c)
		return 400, map[string]any{"Code": "Unexpected", "Message": c.action}
	})
}

func TestListClustersFindsGPUNodePoolsAndSkipsVirtualNodeClusters(t *testing.T) {
	page1 := []map[string]any{
		ackClusterJSON("c-gpu", "ack-gpu", "cn-shanghai", "running", "Default", "https://47.100.1.1:6443"),
		ackClusterJSON("c-private", "ack-private", "cn-hangzhou", "running", "Default", ""),
	}
	page2 := []map[string]any{
		ackClusterJSON("c-acs", "acs", "cn-hangzhou", "running", "Acs", ""),
		ackClusterJSON("c-new", "ack-new", "cn-beijing", "initial", "Default", ""),
	}
	pools := map[string][]map[string]any{
		"c-gpu": {
			ackNodePoolJSON("np-cpu", "default", []string{"ecs.g7.xlarge"}, 2),
			ackNodePoolJSON("np-l20", "l20", []string{"ecs.gn8is.2xlarge", "ecs.gn8is.4xlarge"}, 1),
			ackNodePoolJSON("np-mixed", "mixed", []string{"ecs.gn8is.2xlarge", "ecs.gn7i-c8g1.2xlarge"}, 0),
			ackNodePoolJSON("np-auto", "auto", nil, 3),
		},
		"c-private": {ackNodePoolJSON("np-ppu", "ppu", []string{"ecs.gp7-1x.12xlarge"}, 1)},
	}
	d, fake := ackFake(t, [][]map[string]any{page1, page2}, pools)
	got, err := d.ListClusters(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.ExternalID)
	}
	// Sorted by region; the ACS (virtual node) cluster is left out.
	if !slices.Equal(names, []string{"c-new", "c-private", "c-gpu"}) {
		t.Fatalf("clusters = %v", names)
	}
	gpu, private, fresh := got[2], got[1], got[0]
	if !gpu.Running || !gpu.PublicAPI || gpu.Version != "1.35.7-aliyun.1" || len(gpu.GPUPools) != 2 {
		t.Fatalf("gpu cluster = %+v", gpu)
	}
	l20 := gpu.GPUPools[0]
	if l20.ID != "np-l20" || l20.GPUModel != "NVIDIA L20" || l20.GPUCount != 1 || l20.Nodes != 1 || l20.Vendor != "nvidia" || l20.LabelKey != "alibabacloud.com/nodepool-id" {
		t.Fatalf("L20 pool = %+v", l20)
	}
	if mixed := gpu.GPUPools[1]; mixed.ID != "np-mixed" || mixed.GPUModel != "" || mixed.Vendor != "nvidia" {
		t.Fatalf("mixed-model pool = %+v", mixed)
	}
	if private.PublicAPI || len(private.GPUPools) != 1 || private.GPUPools[0].Vendor != "ppu" {
		t.Fatalf("private cluster = %+v", private)
	}
	if fresh.Running || fresh.State != "initial" || len(fresh.GPUPools) != 0 {
		t.Fatalf("cluster still being created = %+v", fresh)
	}
	for _, c := range fake.calls {
		if c.action == "DescribeClusterNodePools" && (strings.Contains(c.path, "c-new") || strings.Contains(c.path, "c-acs")) {
			t.Fatalf("node pools read for %s", c.path)
		}
	}
	types := 0
	for _, a := range fake.actions() {
		if a == "DescribeInstanceTypes" {
			types++
		}
	}
	if types != 2 { // one per region with GPU pools
		t.Fatalf("DescribeInstanceTypes calls = %d (%v)", types, fake.actions())
	}
}

func TestListClustersReportsAPIRefusals(t *testing.T) {
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		return 403, map[string]any{"code": "Forbidden.RAM", "message": "User not authorized to operate on the specified resource, or this API doesn't support RAM.", "requestId": "req-ram", "status": 403}
	})
	_, err := d.ListClusters(context.Background(), userCreds)
	if err == nil || !strings.Contains(err.Error(), "Forbidden.RAM") || !strings.Contains(err.Error(), "list ACK clusters") {
		t.Fatalf("err = %v", err)
	}
	d2, _ := ackFake(t, [][]map[string]any{{ackClusterJSON("c-gone", "gone", "cn-shanghai", "running", "Default", "")}}, map[string][]map[string]any{})
	if _, err := d2.ListClusters(context.Background(), userCreds); err == nil || !strings.Contains(err.Error(), "ErrorClusterNotFound") {
		t.Fatalf("node pools refused: %v", err)
	}
}

func TestClusterKubeconfigAsksForAShortLivedPublicOne(t *testing.T) {
	d, fake := ackFake(t, nil, nil)
	kc, err := d.ClusterKubeconfig(context.Background(), userCreds, "c-gpu")
	if err != nil || string(kc) != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("kubeconfig = %q, %v", kc, err)
	}
	if _, err := d.ClusterKubeconfig(context.Background(), userCreds, "c-other"); err == nil || !strings.Contains(err.Error(), "ErrorClusterNotFound") {
		t.Fatalf("unknown cluster: %v", err)
	}
	if !slices.Equal(fake.actions(), []string{"DescribeClusterUserKubeconfig", "DescribeClusterUserKubeconfig"}) {
		t.Fatalf("calls = %v (a refusal is not retried)", fake.actions())
	}
}

func TestACKNodesAreMarkedAsClusterInstances(t *testing.T) {
	node := instanceJSON("i-node", "cn-shanghai", "Running", "", "10.0.0.5", 1, "NVIDIA L20")
	node["Tags"] = map[string]any{"Tag": []map[string]string{{"TagKey": "ack.aliyun.com", "TagValue": "c-gpu"}, {"TagKey": "acs:autoscaling:scalingGroupId", "TagValue": "asg-1"}}}
	if got := mapInstance(decodeInstance(t, node), false); got.Cluster != "c-gpu" {
		t.Fatalf("ACK node = %+v", got)
	}
	if got := mapInstance(decodeInstance(t, instanceJSON("i-vm", "cn-shanghai", "Running", "", "10.0.0.6", 1, "NVIDIA A10")), false); got.Cluster != "" {
		t.Fatalf("plain instance = %+v", got)
	}
}

func decodeInstance(t *testing.T, raw map[string]any) apiInstance {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var inst apiInstance
	if err := json.Unmarshal(b, &inst); err != nil {
		t.Fatal(err)
	}
	return inst
}
