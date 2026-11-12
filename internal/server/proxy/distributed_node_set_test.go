package proxy

import (
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A multi-node deployment routes by its instances' generations, not by its
// stored node list: a deployment that lists several replicas stores more
// nodes than NNodes. Whatever shape the list has, including an unreadable
// one, a deployment with no complete generation is the same 503 with the same
// message.
func TestDistributedSelectionDoesNotReadTheNodeSet(t *testing.T) {
	st, err := store.Open("file:proxy_distributed_node_set?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{}}, nil, 0)
	t.Cleanup(p.Close)
	const notReady = "distributed model qwen-72b is not ready: no replica has every rank running on an online node"
	for _, tc := range []struct {
		nodeIDs string
		message string
	}{
		{nodeIDs: "", message: notReady},
		{nodeIDs: "null", message: notReady},
		{nodeIDs: "[]", message: notReady},
		{nodeIDs: "not json", message: notReady},
		{nodeIDs: "[1]", message: notReady},
		{nodeIDs: "[1,2,3]", message: notReady},
		{nodeIDs: "[1,2]", message: notReady},
	} {
		dep := &store.Deployment{ID: 1, NodeID: 1, NodeIDs: tc.nodeIDs, NNodes: 2}
		tgt, status, message, asleep := p.selectDistributedInstance(dep, nil, "qwen-72b", "")
		if status != http.StatusServiceUnavailable || message != tc.message || asleep || tgt.nodeUUID != "" {
			t.Errorf("node_ids %q: status=%d message=%q asleep=%v target=%+v; want 503 %q",
				tc.nodeIDs, status, message, asleep, tgt, tc.message)
		}
	}
}
