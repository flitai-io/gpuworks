package hub

import "testing"

// A node's UCX hang is what its latest report said: a report without it (the
// sysctl was fixed, or an older agent) clears it, and an offline node has
// none. Nothing is stored.
func TestUCXNetlinkHangIsWhatTheLatestReportSaid(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID, closed: make(chan struct{})}
	if h.UCXNetlinkHang(uuid) != nil {
		t.Fatal("an offline node has a hang")
	}
	h.register(sess)
	sess.handleStatusNode([]byte(`{"ucx_netlink_hang":{"kernel":"5.15.0-203-generic"}}`))
	if got := h.UCXNetlinkHang(uuid); got == nil || got.Kernel != "5.15.0-203-generic" {
		t.Fatalf("after an affected report: %+v", got)
	}
	sess.handleStatusNode([]byte(`{}`))
	if got := h.UCXNetlinkHang(uuid); got != nil {
		t.Fatalf("a report without the field left %+v", got)
	}
	sess.handleStatusNode([]byte(`{"ucx_netlink_hang":{}}`))
	if h.UCXNetlinkHang(uuid) == nil {
		t.Fatal("an affected report without a kernel (unreadable release) must still count")
	}
	h.deregister(sess)
	if h.UCXNetlinkHang(uuid) != nil {
		t.Fatal("a node that disconnected keeps its hang")
	}
}
