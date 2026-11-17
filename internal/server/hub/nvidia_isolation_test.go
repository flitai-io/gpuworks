package hub

import (
	"testing"
)

// A node's soft-isolation notice is what its latest report says: a report
// without one (the node went strict, or an older agent) clears it, and an
// offline node has none. Nothing is stored.
func TestNVIDIAIsolationIsWhatTheLatestReportSaid(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID, closed: make(chan struct{})}
	if h.NVIDIAIsolation(uuid) != nil {
		t.Fatal("an offline node has a notice")
	}
	h.register(sess)
	sess.handleStatusNode([]byte(`{"nvidia_isolation":{"level":"soft","reason":"cdi_names_missing","toolkit":"gpu_operator","cards":2}}`))
	got := h.NVIDIAIsolation(uuid)
	if got == nil || got.Level != "soft" || got.Reason != "cdi_names_missing" || got.Toolkit != "gpu_operator" || got.Cards != 2 {
		t.Fatalf("after a soft report: %+v", got)
	}
	sess.handleStatusNode([]byte(`{}`))
	if got := h.NVIDIAIsolation(uuid); got != nil {
		t.Fatalf("a report without the field left %+v", got)
	}
	sess.handleStatusNode([]byte(`{"nvidia_isolation":{"level":"soft","reason":"cdi_names_missing"}}`))
	h.deregister(sess)
	if h.NVIDIAIsolation(uuid) != nil {
		t.Fatal("a node that disconnected keeps its notice")
	}
}
