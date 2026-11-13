package digitalocean

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestListClustersFindsNVIDIAPoolsOnly(t *testing.T) {
	d, f := newFake(t)
	f.sizes = []map[string]any{gpuSize(), cpuSize()} // the catalog the account may buy from
	f.clusters = []map[string]any{
		clusterJSON("c-gpu", "ml", "nyc2", "running", "https://c-gpu.k8s.ondigitalocean.com",
			nodePoolJSON("np-cpu", "default", "s-4vcpu-8gb", 3),
			nodePoolJSON("np-h100", "h100", "gpu-h100x1-80gb", 2),
			nodePoolJSON("np-ada", "ada", "gpu-6000adax1-48gb", 1), // not in the catalog: from the slug
			nodePoolJSON("np-8x", "eight", "gpu-h100x8-640gb", 1),
			nodePoolJSON("np-amd", "amd", "gpu-mi300x8-1536gb", 1),
		),
		clusterJSON("c-new", "fresh", "ams3", "provisioning", ""),
		clusterJSON("c-cpu", "web", "fra1", "running", "https://c-cpu.k8s.ondigitalocean.com",
			nodePoolJSON("np-web", "web", "s-2vcpu-4gb", 2)),
	}
	got, err := d.ListClusters(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("clusters = %+v", got)
	}
	// Sorted by region, then name.
	var order []string
	for _, c := range got {
		order = append(order, c.ExternalID)
	}
	if !reflect.DeepEqual(order, []string{"c-new", "c-cpu", "c-gpu"}) {
		t.Errorf("order = %v", order)
	}
	gpu := got[2]
	wantPools := []provider.ClusterPool{
		{ID: "np-h100", Name: "h100", GPUModel: "NVIDIA H100", GPUCount: 1, Nodes: 2, Vendor: "nvidia", LabelKey: "doks.digitalocean.com/node-pool-id"},
		{ID: "np-ada", Name: "ada", GPUModel: "NVIDIA RTX 6000 Ada", GPUCount: 1, Nodes: 1, Vendor: "nvidia", LabelKey: "doks.digitalocean.com/node-pool-id"},
		{ID: "np-8x", Name: "eight", GPUModel: "NVIDIA H100", GPUCount: 8, Nodes: 1, Vendor: "nvidia", LabelKey: "doks.digitalocean.com/node-pool-id"},
	}
	if !reflect.DeepEqual(gpu.GPUPools, wantPools) {
		t.Errorf("pools =\n%+v\nwant\n%+v", gpu.GPUPools, wantPools)
	}
	if gpu.Name != "ml" || gpu.Region != "nyc2" || gpu.Version != "1.34.1-do.0" || gpu.State != "running" || !gpu.Running || !gpu.PublicAPI {
		t.Errorf("cluster = %+v", gpu)
	}
	fresh := got[0]
	if fresh.Running || fresh.PublicAPI || fresh.State != "provisioning" || fresh.GPUPools == nil || len(fresh.GPUPools) != 0 {
		t.Errorf("a cluster being created = %+v", fresh)
	}
	if cpu := got[1]; len(cpu.GPUPools) != 0 || !cpu.Running {
		t.Errorf("cpu cluster = %+v", cpu)
	}
	// The catalog is read once, for the pools that need it.
	if n := len(f.calls(http.MethodGet, "/v2/sizes")); n != 1 {
		t.Errorf("GET /v2/sizes sent %d times, want 1", n)
	}
}

func TestListClustersPoolGPUFactsPreferTheCatalog(t *testing.T) {
	d, f := newFake(t)
	// The catalog knows this size and says 2 cards; the slug alone would say 1.
	f.sizes = []map[string]any{sizeJSON("gpu-h100x1-80gb", 4.41, "nvidia_h100", 2, 80)}
	f.clusters = []map[string]any{clusterJSON("c1", "a", "nyc2", "running", "https://x", nodePoolJSON("p", "p", "gpu-h100x1-80gb", 1))}
	got, err := d.ListClusters(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if p := got[0].GPUPools[0]; p.GPUCount != 2 || p.GPUModel != "NVIDIA H100" {
		t.Errorf("pool = %+v", p)
	}
}

func TestListClustersWithoutGPUPoolsNeverReadsTheCatalog(t *testing.T) {
	d, f := newFake(t)
	f.clusters = []map[string]any{clusterJSON("c1", "a", "nyc2", "running", "https://x", nodePoolJSON("p", "p", "s-2vcpu-4gb", 1))}
	if _, err := d.ListClusters(t.Context(), testCreds); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(http.MethodGet, "/v2/sizes")); n != 0 {
		t.Errorf("GET /v2/sizes sent %d times", n)
	}
}

func TestListClustersFollowsEveryPage(t *testing.T) {
	d, f := newFake(t)
	for i := 0; i < 205; i++ {
		f.clusters = append(f.clusters, clusterJSON(fmt.Sprintf("c%03d", i), fmt.Sprintf("n%03d", i), "nyc2", "running", "https://x"))
	}
	got, err := d.ListClusters(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 205 {
		t.Fatalf("clusters = %d", len(got))
	}
	if n := len(f.calls(http.MethodGet, "/v2/kubernetes/clusters")); n != 2 {
		t.Errorf("pages requested = %d", n)
	}
}

func TestListClustersFailsRatherThanReturnPartial(t *testing.T) {
	d, f := newFake(t)
	for i := 0; i < 205; i++ {
		f.clusters = append(f.clusters, clusterJSON(fmt.Sprintf("c%03d", i), "n", "nyc2", "running", "https://x"))
	}
	f.failPage("2", http.StatusForbidden)
	if got, err := d.ListClusters(t.Context(), testCreds); err == nil || got != nil {
		t.Fatalf("got %d clusters, err %v", len(got), err)
	}
	// A catalog that cannot be read leaves the pools unknown: an error too.
	d, f = newFake(t)
	f.clusters = []map[string]any{clusterJSON("c1", "a", "nyc2", "running", "https://x", nodePoolJSON("p", "p", "gpu-h100x1-80gb", 1))}
	f.forced = []forcedResponse{{status: 200, body: pageOf("kubernetes_clusters", f.clusters)}, {status: 403, body: `{"id":"forbidden","message":"You are not authorized to perform this operation"}`}}
	if _, err := d.ListClusters(t.Context(), testCreds); err == nil || !strings.Contains(err.Error(), "sizes") {
		t.Fatalf("error = %v", err)
	}
}

func TestClusterKubeconfig(t *testing.T) {
	d, f := newFake(t)
	cfg := "apiVersion: v1\nkind: Config\nclusters:\n- name: do-nyc2-ml\n"
	f.kubeconfigs["c-gpu"] = cfg
	got, err := d.ClusterKubeconfig(t.Context(), testCreds, "c-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cfg {
		t.Errorf("kubeconfig = %q", got)
	}
	calls := f.calls(http.MethodGet, "/v2/kubernetes/clusters/c-gpu/kubeconfig")
	if len(calls) != 1 || calls[0].Query.Get("expiry_seconds") != "900" || len(calls[0].Query) != 1 {
		t.Errorf("request = %+v", calls)
	}
}

func TestClusterKubeconfigErrors(t *testing.T) {
	d, f := newFake(t)
	_, err := d.ClusterKubeconfig(t.Context(), testCreds, "gone")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.ID != "not_found" {
		t.Errorf("error = %v", err)
	}
	f.kubeconfigs["empty"] = " \n"
	if _, err := d.ClusterKubeconfig(t.Context(), testCreds, "empty"); err == nil {
		t.Error("an empty kubeconfig was returned")
	}
	if _, err := d.ClusterKubeconfig(t.Context(), badCreds, "c-gpu"); err == nil {
		t.Error("a bad token got a kubeconfig")
	}
	before := len(f.requests)
	if _, err := d.ClusterKubeconfig(t.Context(), testCreds, " "); err == nil || len(f.requests) != before {
		t.Error("an empty cluster id was sent")
	}
	// An id is one path segment.
	f.kubeconfigs["account"] = "x"
	if _, err := d.ClusterKubeconfig(t.Context(), testCreds, "a/../account"); err == nil {
		t.Error("a path in the cluster id was followed")
	}
}
