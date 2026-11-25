package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestTargetDesiredRejectsUnsupportedStructuredFeaturesBeforeRuntime(t *testing.T) {
	tests := map[string]proto.InstanceDesiredState{
		"sleepable": {Sleepable: true}, "sleep-order": {DesiredSleep: 1},
		"multinode": {NNodes: 2}, "rank": {NodeRank: 1}, "headless": {Headless: true}, "group": {DistGroup: "other"},
		"partition": {VRAMLimitBytes: 1}, "partition-backend": {PartitionBackend: "nvidia-logical"}, "sm-limit": {SMLimitPct: 50},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
			if err := e.validateTargetDesired(spec); err == nil {
				t.Fatal("unsupported structured contract accepted")
			}

		})
	}
}
func TestTargetDesiredAllowsSupportedWholeCardAllocation(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
	spec := proto.InstanceDesiredState{NNodes: 1, SMLimitPct: 100, GPUIndexes: []int{7, 12}}
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatal(err)
	}
	e.prober = &gpu.FixedProber{VendorName: "hygon"}
	spec.Sleepable = true
	spec.NNodes = 2
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatal("target validation rejected supported Hygon contract:", err)
	}
}

func TestAMDTargetDesiredRejectsEmptyDeviceSelection(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{}, WithHostRuntimeFacts(func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }))
	if err := e.validateTargetDesired(proto.InstanceDesiredState{}); err == nil || !strings.Contains(err.Error(), "nonempty GPU selection") {
		t.Fatalf("empty AMD selection accepted: %v", err)
	}
}

// AMD requires explicit device selection like any other whole-device vendor,
// but also owns a memory-only logical partition backend of its own: a quota
// naming amd-dgpu is accepted once a device is selected, a quota naming a
// different vendor's backend (or none, which defaults to NVIDIA's) is still
// refused as a whole-device contract, and a compute/SM limit is refused
// regardless of backend, because AMD exposes no such control.
func TestAMDTargetDesiredAcceptsItsOwnMemoryPartitionButNotOthers(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
	spec := proto.InstanceDesiredState{GPUIndexes: []int{0}, VRAMLimitBytes: 60000 << 20, PartitionBackend: accelerator.BackendAMDLogical}
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatalf("AMD's own memory partition contract was refused: %v", err)
	}
	foreign := spec
	foreign.PartitionBackend = accelerator.BackendNVIDIALogical
	if err := e.validateTargetDesired(foreign); err == nil || !strings.Contains(err.Error(), "whole-device partition contract") {
		t.Fatalf("a foreign partition backend was accepted: %v", err)
	}
	legacyDefault := spec
	legacyDefault.PartitionBackend = ""
	if err := e.validateTargetDesired(legacyDefault); err == nil || !strings.Contains(err.Error(), "whole-device partition contract") {
		t.Fatalf("a quota with no named backend (implicit NVIDIA default) was accepted: %v", err)
	}
	smLimited := spec
	smLimited.SMLimitPct = 50
	if err := e.validateTargetDesired(smLimited); err == nil || !strings.Contains(err.Error(), "compute/SM limit") {
		t.Fatalf("a compute/SM limit was accepted on AMD: %v", err)
	}
}

func TestTargetDesiredDoesNotParseLaunchArguments(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
	spec := proto.InstanceDesiredState{GPUIndexes: []int{2}, ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"vllm", "serve", "--pipeline-parallel-size", "2"}}}
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatalf("agent reparsed argv instead of trusting the server launch contract: %v", err)
	}
}

func TestTargetDesiredNormalizesDefaultLeaderWireAsSingleNode(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(`{"dist_role":"leader","nnodes":1,"node_rank":0}`), &spec); err != nil {
		t.Fatal(err)
	}
	spec.GPUIndexes = []int{2}
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatalf("single-node leader wire was rejected: %v", err)
	}
}

// AMD runs distributed ranks (RCCL over the NCCL_* socket variables) and
// vLLM sleep mode, so neither the structural multi-node facts nor sleep intent
// of a legacy wire refuse a launch.
func TestTargetDesiredAcceptsAMDDistributedAndSleepWire(t *testing.T) {
	tests := map[string]string{
		"multiple nodes": `{"dist_role":"leader","dist_group":"group-1","dist_attempt":1,"nnodes":2,"node_rank":0}`,
		"worker role":    `{"dist_role":"worker","nnodes":2,"node_rank":1,"headless":true}`,
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			var spec proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(wire), &spec); err != nil {
				t.Fatal(err)
			}
			spec.GPUIndexes = []int{1}
			e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
			if err := e.validateTargetDesired(spec); err != nil {
				t.Fatalf("distributed AMD wire refused: %v", err)
			}
			spec.Sleepable = true
			if err := e.validateTargetDesired(spec); err != nil {
				t.Fatalf("sleepable AMD rank refused: %v", err)
			}
		})
	}
}

func TestTargetDesiredLeavesOtherVendorLaunchPolicyToServer(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		driver string
		spec   proto.InstanceDesiredState
	}{
		// A Mac's engines are host processes; its cards are on no other runtime.
		{vendor: "apple", driver: "host", spec: proto.InstanceDesiredState{}},
		{vendor: "nvidia", driver: "docker", spec: proto.InstanceDesiredState{NNodes: 2}},
		{vendor: "ascend", driver: "docker", spec: proto.InstanceDesiredState{SMLimitPct: 50}},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			factsRead := false
			e := NewWithProber(&headlessTestDriver{name: tc.driver}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: tc.vendor}, WithHostRuntimeFacts(func() accelerator.HostRuntime {
				factsRead = true
				return accelerator.HostRuntime{}
			}))
			if err := e.validateTargetDesired(tc.spec); err != nil {
				t.Fatalf("agent changed existing %s launch admission: %v", tc.vendor, err)
			}
			if !factsRead {
				t.Fatalf("non-AMD %s guard did not evaluate host runtime facts", tc.vendor)
			}
		})
	}
}

func TestEngineDoesNotAllocateDefaultHostFactsReader(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: "nvidia"})
	if e.hostRuntimeFacts != nil {
		t.Fatal("engine eagerly allocated a default host runtime facts reader")
	}
}

func TestTargetDesiredRejectsUnsupportedRuntimeBeforeLaunch(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "host"}, nil, nil, t.TempDir(), &gpu.AMDProber{})
	if err := e.validateTargetDesired(proto.InstanceDesiredState{GPUIndexes: []int{0}}); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("host-process target validation error = %v", err)
	}
}

func TestTargetDesiredHostRuntimeFacts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  accelerator.HostRuntime
		valid bool
	}{
		{name: "supported", host: accelerator.HostRuntime{OS: "linux", Arch: "amd64", Driver: "docker"}, valid: true},
		{name: "wrong os", host: accelerator.HostRuntime{OS: "darwin", Arch: "amd64", Driver: "docker"}},
		{name: "wrong arch", host: accelerator.HostRuntime{OS: "linux", Arch: "arm64", Driver: "docker"}},
		{name: "runveil", host: accelerator.HostRuntime{OS: "linux", Arch: "amd64", Driver: "udocker"}, valid: true},
		{name: "wrong runtime", host: accelerator.HostRuntime{OS: "linux", Arch: "amd64", Driver: "host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewWithProber(&headlessTestDriver{name: tc.host.Driver}, nil, nil, t.TempDir(), &gpu.AMDProber{}, WithHostRuntimeFacts(func() accelerator.HostRuntime {
				return tc.host
			}))
			err := e.validateTargetDesired(proto.InstanceDesiredState{GPUIndexes: []int{2}})
			if tc.valid && err != nil {
				t.Fatalf("supported facts rejected: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("unsupported host facts accepted")
			}
		})
	}
}

func TestTargetDesiredRejectsUnsupportedRuntimeBeforePreStart(t *testing.T) {
	driver := &preflightTrackingDriver{headlessTestDriver: &headlessTestDriver{name: "udocker"}}
	e := NewWithProber(driver, nil, nil, t.TempDir(), &gpu.AMDProber{}, WithHostRuntimeFacts(func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }))
	spec := proto.InstanceDesiredState{UUID: "preflight-amd", ResolvedLaunch: proto.ResolvedLaunch{PreStartArgv: []string{"touch", "/tmp/should-not-run"}}}
	if err := e.validateTargetDesired(spec); err == nil {
		t.Fatal("unsupported host runtime accepted")
	}

	if driver.statusCalls != 0 || driver.runCalls != 0 {
		t.Fatalf("runtime touched before target validation: status=%d run=%d", driver.statusCalls, driver.runCalls)
	}
}

type preflightTrackingDriver struct {
	*headlessTestDriver
	statusCalls int
	runCalls    int
}

func (d *preflightTrackingDriver) Status(context.Context, string) (runtime.Status, error) {
	d.statusCalls++
	return runtime.Status{}, nil
}

func (d *preflightTrackingDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.runCalls++
	return "", nil
}

func TestTargetDesiredUnknownSnapshotIsVendorScoped(t *testing.T) {
	for _, vendor := range []string{"amd", "nvidia", "ascend", "apple"} {
		driver := "docker"
		if vendor == "apple" {
			driver = "host" // a Mac's cards are on no other runtime
		}
		e := NewWithProber(&headlessTestDriver{name: driver}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: vendor})
		spec := proto.InstanceDesiredState{GPUIndexes: []int{0}, ResolvedLaunch: proto.ResolvedLaunch{RequestedFeatures: &proto.RequestedFeatures{CommandKnown: false}}}
		// No vendor carries a partial restriction today, so an unknown
		// command needs no proof anywhere.
		err := e.validateTargetDesired(spec)
		if err != nil {
			t.Fatalf("%s unknown contract error=%v", vendor, err)
		}
	}
}

func TestTargetDesiredRejectsContradictorySnapshotButAllowsSleepAPIMetadata(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: "nvidia"})
	spec := proto.InstanceDesiredState{SleepAPI: "/sleep", ResolvedLaunch: proto.ResolvedLaunch{RequestedFeatures: &proto.RequestedFeatures{CommandKnown: true}}}
	if err := e.validateTargetDesired(spec); err != nil {
		t.Fatalf("available API metadata became a sleep request: %v", err)
	}
	spec.NNodes = 2
	if err := e.validateTargetDesired(spec); err == nil {
		t.Fatal("snapshot omitted explicit distributed topology")
	}
}

func TestLegacyTargetDesiredSleepMetadataIsNotARequest(t *testing.T) {
	// Apple is the vendor that refuses engine sleep; its cards are on the
	// host runtime.
	e := NewWithProber(&headlessTestDriver{name: "host"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: "apple"})
	for _, tc := range []struct {
		name   string
		spec   proto.InstanceDesiredState
		refuse bool
	}{
		{name: "available API", spec: proto.InstanceDesiredState{SleepAPI: "/sleep"}},
		{name: "available mode", spec: proto.InstanceDesiredState{SleepMode: "wait"}},
		{name: "declared request", spec: proto.InstanceDesiredState{SleepAPI: "/sleep", Sleepable: true}, refuse: true},
		{name: "active sleep order", spec: proto.InstanceDesiredState{SleepAPI: "/sleep", DesiredSleep: 1}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.GPUIndexes = []int{0}
			err := e.validateTargetDesired(spec)
			if (err != nil) != tc.refuse {
				t.Fatalf("legacy metadata/request error=%v wantRefusal=%v", err, tc.refuse)
			}
		})
	}
}

// The agent re-checks an external KV cache like any other requested feature:
// no vendor row denies it (the server's backend gate decides vendors), Apple
// denies every feature, and a snapshot without it is the one older servers
// send.
func TestTargetDesiredReChecksExternalKVCache(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: "nvidia"})
	external := proto.InstanceDesiredState{NNodes: 1, ResolvedLaunch: proto.ResolvedLaunch{RequestedFeatures: &proto.RequestedFeatures{CommandKnown: true, ExternalKVCache: true}}}
	if err := e.validateTargetDesired(external); err != nil {
		t.Fatalf("external KV cache refused on NVIDIA: %v", err)
	}
	e.prober = &gpu.FixedProber{VendorName: "apple"}
	if err := e.validateTargetDesired(external); err == nil || !strings.Contains(err.Error(), "external_kv_cache") {
		t.Fatalf("external KV cache accepted on Apple: %v", err)
	}
	if names := (&proto.RequestedFeatures{CommandKnown: true}).Names(); len(names) != 0 {
		t.Fatalf("a snapshot without the field names %v", names)
	}
	raw, _ := json.Marshal(proto.RequestedFeatures{CommandKnown: true})
	if string(raw) != `{"command_known":true}` {
		t.Fatalf("wire = %s, want the field omitted", raw)
	}
}

// A launch on a runtime the cards' vendor does not list is refused at
// admission, before an image is prepared: a Mac's card under docker, and a
// Linux vendor's card under the host runtime.
func TestTargetDesiredRefusesARuntimeTheVendorDoesNotList(t *testing.T) {
	for _, tc := range []struct {
		vendor, driver string
		refuse         bool
	}{
		{"apple", "docker", true},
		{"apple", "host", false},
		{"nvidia", "host", true},
		{"nvidia", "udocker", false},
		{"ppu", "kube", false},
	} {
		e := NewWithProber(&headlessTestDriver{name: tc.driver}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: tc.vendor})
		err := e.validateTargetDesired(proto.InstanceDesiredState{GPUIndexes: []int{0}})
		if (err != nil) != tc.refuse {
			t.Fatalf("%s on %s: err = %v, want refusal %v", tc.vendor, tc.driver, err, tc.refuse)
		}
		if err != nil {
			if failure := failureOf(err, "", ""); failure.Code != proto.ErrorAcceleratorRuntimeUnsupported {
				t.Fatalf("%s on %s: failure = %+v", tc.vendor, tc.driver, failure)
			}
		}
	}
}
