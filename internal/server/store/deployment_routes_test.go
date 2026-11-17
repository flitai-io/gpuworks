package store

import (
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDeploymentRouteReads(t *testing.T) {
	s := newTestStore(t)
	a, err := s.RegisterNode("route-a", HashToken("route-a"), "route-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RegisterNode("route-b", HashToken("route-b"), "route-b")
	if err != nil {
		t.Fatal(err)
	}
	var deps []uint
	for i := 0; i < 3; i++ {
		d, err := s.CreateDeployment(&Deployment{Name: fmt.Sprintf("route-%d", i), ModelID: 1, NodeID: a.ID, State: DeployActive})
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, d.ID)
	}
	// deps[0] has two instances on a (one stopped), deps[1] one on b, deps[2] none.
	for _, inst := range []Instance{
		{UUID: "route-i0", DeploymentID: deps[0], NodeID: a.ID, State: proto.InstRunning},
		{UUID: "route-i1", DeploymentID: deps[0], NodeID: a.ID, State: proto.InstStopped},
		{UUID: "route-i2", DeploymentID: deps[1], NodeID: b.ID, State: proto.InstStarting},
	} {
		if _, err := s.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.DeploymentIDs()
	if err != nil || fmt.Sprint(all) != fmt.Sprint(deps) {
		t.Fatalf("DeploymentIDs = %v, %v; want %v", all, err, deps)
	}

	for _, tc := range []struct {
		uuids   []string
		want    []uint
		unknown bool
	}{
		{nil, nil, false},
		{[]string{"route-a"}, []uint{deps[0]}, false},
		{[]string{"route-a", "route-b", "route-a"}, []uint{deps[0], deps[1]}, false},
		{[]string{"route-b", "gone"}, []uint{deps[1]}, true},
		{[]string{"gone"}, nil, true},
	} {
		got, unknown, err := s.DeploymentIDsOnNodes(tc.uuids)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) || unknown != tc.unknown {
			t.Fatalf("DeploymentIDsOnNodes(%v) = %v, %v, %v; want %v, %v", tc.uuids, got, unknown, err, tc.want, tc.unknown)
		}
	}

	dep, node, err := s.InstanceRefs("route-i2")
	if err != nil || dep != deps[1] || node != b.ID {
		t.Fatalf("InstanceRefs = %d, %d, %v; want %d, %d", dep, node, err, deps[1], b.ID)
	}
	if _, _, err := s.InstanceRefs("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing instance err = %v, want ErrNotFound", err)
	}
}
