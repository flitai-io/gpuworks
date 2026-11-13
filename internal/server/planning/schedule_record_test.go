package planning

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestScheduleRecordWireNamesTheNetworkAndPDEndpoints(t *testing.T) {
	plan := LaunchPlan{
		Source: LaunchSource{Image: "vllm:0.25.1"},
		Topology: DistTopology{
			NNodes: 2, NodeAddress: "10.20.1.11", NetworkID: 3, NetworkName: "ib-a",
			Network: deploy.ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1"}, LinkLayer: "infiniband"},
		},
	}
	pd := &PDRecord{Role: "decode", Address: "10.20.1.11", Port: 30001, MasterPort: 30003}
	raw := NewScheduleRecord(plan, true, pd, &UnitRecord{Followers: "n2[]"}).Encode()

	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	wantNetwork := map[string]any{"compute": map[string]any{"name": "ib-a", "management": false, "type": "host_rdma", "interface": "bond0", "ip": "10.20.1.11", "link_layer": "infiniband"}}
	if !reflect.DeepEqual(doc["network"], wantNetwork) {
		t.Fatalf("network = %v, want %v", doc["network"], wantNetwork)
	}
	wantPD := map[string]any{"role": "decode", "address": "10.20.1.11", "port": float64(30001), "master_port": float64(30003)}
	if !reflect.DeepEqual(doc["pd"], wantPD) {
		t.Fatalf("pd = %v, want %v", doc["pd"], wantPD)
	}
	if !reflect.DeepEqual(doc["unit"], map[string]any{"followers": "n2[]"}) {
		t.Fatalf("unit = %v", doc["unit"])
	}
	if !reflect.DeepEqual(doc["image"], map[string]any{"ref": "vllm:0.25.1"}) {
		t.Fatalf("image = %v", doc["image"])
	}
	for _, key := range []string{"explain", "breakdown"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("record has no %q: %s", key, raw)
		}
	}

	back := ReadScheduleRecord(raw)
	if got := back.PDRecord(); got != *pd {
		t.Fatalf("PD record round trip = %+v, want %+v", got, *pd)
	}
	if got := back.UnitRecord().Followers; got != "n2[]" {
		t.Fatalf("unit record round trip = %q", got)
	}
	network, ok := back.ComputeNetwork()
	if !ok || network.Name != "ib-a" || network.Type != "host_rdma" || network.Management {
		t.Fatalf("compute network round trip = %+v %v", network, ok)
	}
}

func TestScheduleRecordLeavesOutWhatTheLaunchDidNotHave(t *testing.T) {
	raw := NewScheduleRecord(LaunchPlan{Topology: DistTopology{NNodes: 1, NetworkName: "mgmt"}}, false, nil, nil).Encode()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pd", "unit", "network"} {
		if _, ok := doc[key]; ok {
			t.Fatalf("ordinary single-node record carries %q: %s", key, raw)
		}
	}
	back := ReadScheduleRecord(raw)
	if back.PDRecord() != (PDRecord{}) {
		t.Fatalf("PD record = %+v", back.PDRecord())
	}
	if _, ok := back.ComputeNetwork(); ok {
		t.Fatal("ordinary record reports a compute network")
	}
	for _, bad := range []string{"", "not json", `{"pd":7}`} {
		if got := ReadScheduleRecord(bad).PDRecord(); got != (PDRecord{}) {
			t.Fatalf("ReadScheduleRecord(%q) = %+v, want zero", bad, got)
		}
	}
}

// Migration 118 (store.renameComputeNetwork) rewrites records written before
// the role was renamed; the record it leaves behind is what this reader
// takes. The legacy key is not read: the migration is the only path from it.
func TestScheduleRecordReadsTheRecordTheComputeRenameMigrationWrote(t *testing.T) {
	migrated := `{"explain":{},"image":{"ref":"vllm:1"},"network":{"compute":{"name":"ib","management":false,"type":"host_rdma","interface":"ib0","ip":"10.20.1.15","link_layer":"infiniband"}},"pd":{"role":"prefill","address":"10.20.1.15","port":8998}}`
	back := ReadScheduleRecord(migrated)
	network, ok := back.ComputeNetwork()
	want := ComputeNetworkRecord{Name: "ib", Type: "host_rdma", Interface: "ib0", IP: "10.20.1.15", LinkLayer: "infiniband"}
	if !ok || network != want {
		t.Fatalf("compute network = %+v %v, want %+v", network, ok, want)
	}
	if got := back.PDRecord(); got.Role != "prefill" || got.Port != 8998 {
		t.Fatalf("PD record = %+v", got)
	}
	if _, ok := ReadScheduleRecord(`{"network":{"parameter":{"name":"ib","type":"host_rdma"}}}`).ComputeNetwork(); ok {
		t.Fatal("a record the migration did not rewrite reads as a compute network")
	}
}
