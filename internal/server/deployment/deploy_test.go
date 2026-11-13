package deployment

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPrepareDistributedDeployRequestDerivesLeaderAndValidatesTP(t *testing.T) {
	st, err := store.Open("file:api_distributed_prepare_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"node-1", "node-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys: proto.SysInfo{Hostname: uuid}, Network: testManagement("10.0.0." + string(rune('1'+i))),
			GPUs: []proto.GPUInfo{{Index: 0, Name: "GPU"}, {Index: 1, Name: "GPU"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	req := dto.DeployRequest{
		NodeUUIDs: []string{"node-1", "node-2"}, NNodes: 2,
		Placement: map[string]any{"tp": float64(2)},
	}
	h := Service{d: Deps{Store: st}}
	nodes, err := h.PrepareDistributedDeployRequest(&req, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || req.NodeUUID != "node-1" || req.NodeRank != 0 || req.DistRole != "leader" {
		t.Fatalf("normalized request/nodes = %+v / %+v", req, nodes)
	}
	if req.MasterAddr != "10.0.0.1" || req.MasterPort != 29500 {
		t.Fatalf("master = %s:%d", req.MasterAddr, req.MasterPort)
	}
}

func TestPrepareDistributedDeployRequestRequiresANetworkForComputeTraffic(t *testing.T) {
	st, err := store.Open("file:api_distributed_address_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"node-1", "node-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid}, GPUs: []proto.GPUInfo{{Index: 0, Name: "GPU"}}}); err != nil {
			t.Fatal(err)
		}
	}
	req := dto.DeployRequest{NodeUUIDs: []string{"node-1", "node-2"}, NNodes: 2, Placement: map[string]any{"tp": float64(2)}}
	// Neither node reported a management address or is on a compute network.
	_, err = (New(Deps{Store: st})).PrepareDistributedDeployRequest(&req, 1)
	var product interface{ Code() string }
	if !errors.As(err, &product) || product.Code() != messages.ErrorComputeNetworkNone {
		t.Fatalf("error = %v, want %s", err, messages.ErrorComputeNetworkNone)
	}
}

func TestPrepareDistributedDeployRequestRejectsInvalidPortAndCapacity(t *testing.T) {
	st, err := store.Open("file:api_distributed_capacity_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"node-1", "node-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{}, Network: testManagement("10.0.0." + string(rune('1'+i))), GPUs: []proto.GPUInfo{{Index: 0, Name: "GPU"}}}); err != nil {
			t.Fatal(err)
		}
	}
	h := Service{d: Deps{Store: st}}
	req := dto.DeployRequest{NodeUUIDs: []string{"node-1", "node-2"}, NNodes: 2, MasterPort: 80, Placement: map[string]any{"tp": float64(2)}}
	if _, err := h.PrepareDistributedDeployRequest(&req, 1); err == nil || !strings.Contains(err.Error(), "master_port") {
		t.Fatalf("port error = %v", err)
	}
	req.MasterPort = 29500
	// TP 4 over two nodes is two GPUs on each; these nodes have one.
	req.Placement["tp"] = float64(4)
	if _, err := h.PrepareDistributedDeployRequest(&req, 1); err == nil || !strings.Contains(err.Error(), "has 1 GPUs, the parallelism needs 2 per node") {
		t.Fatalf("capacity error = %v", err)
	}
}

func TestPrepareDistributedDeployRequestFallsBackToTheManagementNetwork(t *testing.T) {
	st, err := store.Open("file:api_distributed_compute_fallback?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"node-1", "node-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
			t.Fatal(err)
		}
		inv := testManagement(fmt.Sprintf("172.25.0.%d", i+1))
		inv.Interfaces = append(inv.Interfaces, proto.NetInterface{Name: "ib0", Addrs: []string{fmt.Sprintf("10.96.0.%d/13", i+1)}})
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid}, Network: inv, GPUs: []proto.GPUInfo{{Index: 0, Name: "GPU"}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Only node-1 is on the compute network.
	ib, err := st.CreateNetwork(&store.Network{Name: "ib", Type: netplan.TypeHostEthernet, CIDR: "10.96.0.0/13", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	node1, _ := st.NodeByUUID("node-1")
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: ib.ID, NodeID: node1.ID}); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{NodeUUIDs: []string{"node-1", "node-2"}, NNodes: 2, Placement: map[string]any{"tp": float64(2)}}
	if _, err := h.PrepareDistributedDeployRequest(&req, 1); err != nil || req.MasterAddr != "172.25.0.1" {
		t.Fatalf("fallback = %q, %v", req.MasterAddr, err)
	}
	nodes := []*store.Node{node1}
	node2, _ := st.NodeByUUID("node-2")
	nodes = append(nodes, node2)
	group, err := h.ComputeGroup(nodes)
	if err != nil || len(computeFallbackWarning(group)) != 1 || computeFallbackWarning(group)[0].Code != messages.WarningComputeNetworkFallback {
		t.Fatalf("warning = %+v, %v", computeFallbackWarning(group), err)
	}

	// node-1 does not let the management network carry compute traffic, so
	// the two nodes share nothing.
	if err := st.SetNodeManagementRolesOff("node-1", []string{netplan.RoleCompute}); err != nil {
		t.Fatal(err)
	}
	req = dto.DeployRequest{NodeUUIDs: []string{"node-1", "node-2"}, NNodes: 2, Placement: map[string]any{"tp": float64(2)}}
	_, err = h.PrepareDistributedDeployRequest(&req, 1)
	var product interface{ Code() string }
	if !errors.As(err, &product) || product.Code() != messages.ErrorComputeNetworkMixed {
		t.Fatalf("mixed err = %v", err)
	}
}
