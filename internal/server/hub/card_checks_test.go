package hub

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Every status report's cards reach the card check sink as they arrive,
// including reports the queue would coalesce, and the end of a session tells
// it the node is gone.
func TestEveryReportReachesTheCardCheckSink(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		uuid   string
		nodeID uint
		checks int
	}
	var reports []seen
	var gone []string
	h.SetCardCheckSinks(func(nodeUUID string, nodeID uint, _ time.Time, gpus []proto.GPUInfo) {
		reports = append(reports, seen{nodeUUID, nodeID, len(gpus[0].Checks)})
	}, func(nodeUUID string) { gone = append(gone, nodeUUID) })

	sess := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID, closed: make(chan struct{})}
	h.register(sess)
	for i := 0; i < 3; i++ {
		raw, _ := json.Marshal(proto.NodeStatus{GPUs: []proto.GPUInfo{{UUID: "GPU-1", Checks: []proto.CardCheck{{ID: "runtime", Status: "pass"}}}}})
		sess.handleStatusNode(raw)
	}
	if len(reports) != 3 || reports[0] != (seen{uuid, node.ID, 1}) {
		t.Fatalf("reports = %+v, want each of the three", reports)
	}
	h.deregister(sess)
	if len(gone) != 1 || gone[0] != uuid {
		t.Fatalf("gone = %v", gone)
	}
}

// A status report from an agent that predates check statuses, exactly as it
// sends it, reaches the sink, and the store, in today's words.
func TestAnOlderAgentsCardChecksArriveInTodaysWords(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	var got []proto.GPUInfo
	h.SetCardCheckSinks(func(_ string, _ uint, _ time.Time, gpus []proto.GPUInfo) { got = gpus }, func(string) {})
	sess := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID, closed: make(chan struct{})}
	h.register(sess)
	defer h.deregister(sess)
	sess.handleStatusNode(json.RawMessage(`{"gpus":[{"index":0,"uuid":"GPU-1","name":"A100","memory_total_bytes":1,"compute_capability":"8.0","numa_node":0,"healthy":false,` +
		`"health_status":"Error","health_code":"memory_uncorrectable_errors",` +
		`"checks":[{"id":"vendor_health","s":"fail","r":"memory_uncorrectable_errors"},{"id":"runtime","s":"pending"},{"id":"occupancy","s":"skip","r":"probe_failed"}]}]}`))
	want := []proto.CardCheck{
		{ID: "vendor_health", Status: proto.CheckFail, Code: "MEMORY_UNCORRECTABLE_ERRORS"},
		{ID: "runtime", Status: proto.CheckSkip, Code: proto.CardCheckNotRunYet},
		{ID: "occupancy", Status: proto.CheckSkip, Code: proto.CardCheckSkipProbeFailed},
	}
	if len(got) != 1 || got[0].HealthCode != "MEMORY_UNCORRECTABLE_ERRORS" || !reflect.DeepEqual(got[0].Checks, want) {
		t.Fatalf("sink got %+v", got)
	}
}
