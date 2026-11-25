package engine

import (
	"context"
	"log/slog"
	"maps"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// An instance container launched through the real start path names the agent
// that launched it and the instance it serves, so an operator can tell apart
// the containers of several agents sharing one docker daemon.
func TestInstanceContainerCarriesOwnerAndInstanceLabels(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{exists: []bool{true}}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{})
	e.SetWorkloadOwner(WorkloadOwner{ServerURL: "https://gw-a.example:8800", NodeUUID: "node-a", StateDir: "/opt/gpuworks-a"})

	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{
		UUID: "inst-1", DeploymentID: 42, Image: "repo:v1",
		APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1,
	})

	if len(driver.runSpecs) != 1 {
		t.Fatalf("run calls = %d, want 1", len(driver.runSpecs))
	}
	want := map[string]string{
		"gpuworks.server":     "https://gw-a.example:8800",
		"gpuworks.node":       "node-a",
		"gpuworks.state-dir":  "/opt/gpuworks-a",
		"gpuworks.instance":   "inst-1",
		"gpuworks.deployment": "42",
	}
	if got := driver.runSpecs[0].Labels; !maps.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

// Facts the agent does not have are left off, never written as "": a
// server_dial agent knows no server address, and before registration there is
// no node UUID.
func TestOwnerLabelsOmitUnknownFacts(t *testing.T) {
	e := newHermeticEngine(nil, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "inst-2", Image: "repo:v1"}

	if got := e.instanceWorkloadSpec(spec, "gpuworks-inst-2").Labels; !maps.Equal(got, map[string]string{"gpuworks.instance": "inst-2"}) {
		t.Fatalf("labels before any owner = %v", got)
	}

	e.SetWorkloadOwner(WorkloadOwner{StateDir: "/opt/gpuworks-b"})
	want := map[string]string{"gpuworks.state-dir": "/opt/gpuworks-b", "gpuworks.instance": "inst-2"}
	if got := e.instanceWorkloadSpec(spec, "gpuworks-inst-2").Labels; !maps.Equal(got, want) {
		t.Fatalf("server_dial labels before registration = %v, want %v", got, want)
	}

	// Registration fills in the node UUID for containers launched afterwards.
	e.SetWorkloadOwner(WorkloadOwner{NodeUUID: "node-b", StateDir: "/opt/gpuworks-b"})
	want["gpuworks.node"] = "node-b"
	if got := e.instanceWorkloadSpec(spec, "gpuworks-inst-2").Labels; !maps.Equal(got, want) {
		t.Fatalf("labels after registration = %v, want %v", got, want)
	}
}

// Containers the engine launches for itself (the per-card runtime check) carry
// the owner too, but no instance: they serve none.
func TestCardSmokeWorkloadCarriesOwnerButNoInstanceLabels(t *testing.T) {
	e := newHermeticEngine(&headlessTestDriver{name: "docker"}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	e.SetWorkloadOwner(WorkloadOwner{ServerURL: "https://gw-a.example", NodeUUID: "node-a", StateDir: "/opt/a"})

	ws, err := e.BuildCardSmokeWorkload("runner:latest", 0, "gpuworks-card-smoke-0-x", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gpuworks.server": "https://gw-a.example", "gpuworks.node": "node-a", "gpuworks.state-dir": "/opt/a"}
	if !maps.Equal(ws.Labels, want) {
		t.Fatalf("card smoke labels = %v, want %v", ws.Labels, want)
	}
}
