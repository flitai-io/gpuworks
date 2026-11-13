package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Run with: go test ./internal/server/api -run '^TestGPUListUsesNodeDisplayNames$' -count=1
func TestGPUListUsesNodeDisplayNames(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cases := []struct {
		uuid, name, hostname, want string
	}{
		{"node-named", "z-display", "a-host", "z-display"},
		{"node-unnamed", "", "b-host", "b-host"},
		{"node-whitespace", " \t", " c-host ", "c-host"},
		{"node-no-hostname", "a-display", "", "a-display"},
		{"uuid-fallback-node", "", "", "uuid-fal"},
	}
	wantByUUID := make(map[string]string, len(cases))
	for _, tc := range cases {
		node, err := st.RegisterNode(tc.uuid, store.HashToken(tc.uuid), tc.hostname)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.RenameNode(node.UUID, tc.name); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: tc.hostname, GPUVendor: "nvidia"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: "gpu-" + tc.uuid, Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		dep, err := st.CreateDeployment(&store.Deployment{Name: "deployment-" + tc.uuid, NodeID: node.ID, State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		spec, err := json.Marshal(proto.InstanceDesiredState{VRAMLimitBytes: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{
			UUID: "instance-" + tc.uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
			GPUIndexes: "[0]", DesiredStateJSON: string(spec),
		}); err != nil {
			t.Fatal(err)
		}
		wantByUUID[tc.uuid] = tc.want
	}
	h := &apiHandlers{d: Deps{Store: st}}

	t.Run("physical_names_sort_and_filter_labels", func(t *testing.T) {
		status, page, body := getGPUPage(t, h, "page=1&page_size=20&sort=node")
		if status != http.StatusOK || page.Total != len(cases) || len(page.Items) != len(cases) {
			t.Fatalf("GPU page status=%d body=%s", status, body)
		}
		var names, facetNames []string
		for _, gpu := range page.Items {
			if want, ok := wantByUUID[gpu.NodeUUID]; !ok || gpu.NodeName != want {
				t.Errorf("node %q: node_name=%q, want %q", gpu.NodeUUID, gpu.NodeName, want)
			}
			if gpu.Name != "H100" || gpu.UUID != "gpu-"+gpu.NodeUUID {
				t.Errorf("GPU identity or model changed: %+v", gpu)
			}
			names = append(names, gpu.NodeName)
		}
		for _, node := range page.Facets.Nodes {
			if want, ok := wantByUUID[node.UUID]; !ok || node.Name != want {
				t.Errorf("node facet %+v, want name %q", node, want)
			}
			facetNames = append(facetNames, node.Name)
		}
		wantOrder := []string{"a-display", "b-host", "c-host", "uuid-fal", "z-display"}
		if !reflect.DeepEqual(names, wantOrder) || !reflect.DeepEqual(facetNames, wantOrder) {
			t.Fatalf("names=%v facets=%v, want %v", names, facetNames, wantOrder)
		}
	})
	for _, tc := range cases {
		t.Run("search/"+tc.uuid, func(t *testing.T) {
			status, page, body := getGPUPage(t, h, "page=1&page_size=20&q="+url.QueryEscape(tc.want))
			if status != http.StatusOK || page.Total != 1 || len(page.Items) != 1 || page.Items[0].NodeUUID != tc.uuid || page.Items[0].NodeName != tc.want {
				t.Fatalf("search %q: status=%d body=%s", tc.want, status, body)
			}
		})
	}
	t.Run("dgpu_names", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.listDGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dgpus", nil))
		var response dto.DGPUListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK || len(response.Items) != len(cases) {
			t.Fatalf("dGPU response=%s status=%d err=%v", rec.Body.String(), rec.Code, err)
		}
		for _, gpu := range response.Items {
			if want, ok := wantByUUID[gpu.NodeUUID]; !ok || gpu.NodeName != want {
				t.Errorf("dGPU node %q: node_name=%q, want %q", gpu.NodeUUID, gpu.NodeName, want)
			}
		}
	})
	t.Run("rename_after_inventory_refresh", func(t *testing.T) {
		if _, err := st.RenameNode("node-named", "renamed-display"); err != nil {
			t.Fatal(err)
		}
		h.gpuCacheAt = time.Now().Add(-gpuInventoryCacheTTL)
		status, page, body := getGPUPage(t, h, "page=1&page_size=20&q=renamed-display")
		if status != http.StatusOK || page.Total != 1 || len(page.Items) != 1 || page.Items[0].NodeName != "renamed-display" || page.Items[0].NodeUUID != "node-named" {
			t.Fatalf("renamed GPU page status=%d body=%s", status, body)
		}
	})
}
