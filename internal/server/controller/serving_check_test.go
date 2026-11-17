package controller

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestServingPathIsCheckedBeforeAnInstanceStarts(t *testing.T) {
	st, err := store.Open("file:controller_serving_check?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svcNode, token, err := st.CreateNodePending("svc")
	if err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"172.25.0.9/24"}}, {Name: "eth1", Addrs: []string{"10.40.0.5/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "172.25.0.9"},
	}
	if err := st.UpdateNodeStatus(svcNode.UUID, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	svc, _ := st.CreateNetwork(&store.Network{Name: "svc", Type: netplan.TypeHostEthernet, CIDR: "10.40.0.0/24", Roles: store.EncodeStringList([]string{netplan.RoleServing})})
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: svc.ID, NodeID: svcNode.ID}); err != nil {
		t.Fatal(err)
	}
	// The node's forwarder: TLS with the key its token derives.
	cert, err := proto.ServingForwardCertificate(token)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	c := New(st, nil, nil)
	var dialed string
	c.servingDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
	}
	node, _ := st.NodeByUUID(svcNode.UUID)
	if err := c.checkServingPath(context.Background(), node); err != nil || dialed != "10.40.0.5:19443" {
		t.Fatalf("reachable serving network: err = %v, dialed %q", err, dialed)
	}
	// The forwarder is gone: the instance must not start.
	_ = ln.Close()
	if err := c.checkServingPath(context.Background(), node); err == nil || !strings.Contains(err.Error(), "cannot reach 10.40.0.5") {
		t.Fatalf("unreachable serving network err = %v", err)
	}

	// A node without a serving network serves over the tunnel, unless its
	// management network may not carry serving.
	plain, err := st.RegisterNode("plain", store.HashToken("p"), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.checkServingPath(context.Background(), plain); err != nil {
		t.Fatalf("tunnel node err = %v", err)
	}
	if err := st.SetNodeManagementRolesOff("plain", []string{netplan.RoleServing}); err != nil {
		t.Fatal(err)
	}
	plain, _ = st.NodeByUUID("plain")
	if err := c.checkServingPath(context.Background(), plain); err == nil || !strings.Contains(err.Error(), "no network that may carry serving") {
		t.Fatalf("serving off err = %v", err)
	}
	if c.networks.CanServe(*plain) {
		t.Fatal("the scheduler would still place instances on the node")
	}
	// An instance placed there anyway is failed with the reason, coded.
	inst, err := st.CreateInstance(&store.Instance{UUID: "no-serving-path", NodeID: plain.ID, State: proto.InstPending, DesiredStateJSON: `{"uuid":"no-serving-path","desired_state":"running"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.startScheduledInstance(&store.Deployment{}, plain, inst); err != nil {
		t.Fatal(err)
	}
	failed, _ := st.InstanceByUUID(inst.UUID)
	f := failed.Failure()
	if failed.State != proto.InstFailed || f == nil || f.Code != proto.ErrorServingPathUnavailable || f.Node != plain.DisplayName() ||
		!strings.Contains(f.Detail, "no network that may carry serving") || failed.Message != f.Detail {
		t.Fatalf("instance = %s %q reason %+v", failed.State, failed.Message, f)
	}
}
