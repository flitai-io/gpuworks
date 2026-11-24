package network

import (
	"maps"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A port is taken when an instance that may still run holds it or the agent
// saw something listen on it; a stopped instance's ports are free again, and
// a row whose ports do not decode holds none.
func TestTakenEnginePorts(t *testing.T) {
	instances := []store.Instance{
		{State: proto.InstRunning, Ports: "[30000,30001]"},
		{State: proto.InstStarting, Ports: "[30002]"},
		{State: proto.InstStopped, Ports: "[30003]"},
		{State: proto.InstRunning, Ports: ""},
		{State: proto.InstRunning, Ports: "not json"},
	}
	inv := &proto.NetInventory{EnginePortListeners: []int{30004}}
	for _, tc := range []struct {
		name string
		inv  *proto.NetInventory
		want []int
	}{
		{"instances and listeners", inv, []int{30000, 30001, 30002, 30004}},
		{"no inventory reported", nil, []int{30000, 30001, 30002}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(TakenEnginePorts(tc.inv, instances)))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("taken = %v, want %v", got, tc.want)
			}
		})
	}
}
