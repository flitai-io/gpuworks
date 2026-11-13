package deployment

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// testManagement is the network inventory a real agent reports: the address
// it reaches the server from, which the management network carries.
func testManagement(ip string) *proto.NetInventory {
	return &proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
	}
}
