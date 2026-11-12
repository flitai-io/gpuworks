package conn

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/terminal"
	"gpuworks.ai/gpuworks/internal/proto"
)

type capabilityProber struct{ vendor string }

func (p capabilityProber) Vendor() string                               { return p.vendor }
func (capabilityProber) Available() bool                                { return true }
func (capabilityProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (capabilityProber) DriverVersion(context.Context) string           { return "" }
func (capabilityProber) Topology(context.Context) string                { return "" }
func (capabilityProber) Metrics(context.Context) ([]gpu.Metric, error)  { return nil, nil }
func (capabilityProber) RuntimeInjection([]int) gpu.RuntimeInjection    { return gpu.RuntimeInjection{} }

func TestHelloCapabilitiesReflectVendor(t *testing.T) {
	tests := map[string][]string{
		"nvidia":  {"gpu.nvidia-smi", "sysinfo"},
		"ascend":  {"gpu.npu-smi", "sysinfo"},
		"ppu":     {"gpu.ppu-smi", "sysinfo"},
		"amd":     {"gpu.amdgpu-sysfs", "sysinfo"},
		"rocm":    {"gpu.amdgpu-sysfs", "sysinfo"},
		"hygon":   {"gpu.hy-smi", "sysinfo"},
		"dcu":     {"gpu.hy-smi", "sysinfo"},
		"mystery": {"gpu.mystery", "sysinfo"},
		"":        {"sysinfo"},
	}
	for vendor, want := range tests {
		want = append(append([]string(nil), want...), proto.CapConnectionDiagnostics, proto.CapPeerRelayV1)
		if terminal.Supported() {
			want = append(want, proto.CapWebterm)
		}
		want = append(want, "state.update.network", proto.CapBench, proto.CapBenchWarmup, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence, proto.CapBenchRequestTimeout, proto.CapImagePullCandidates, "storage.files", proto.CapCardChecks)
		if upgradeSupported() {
			want = append(want, proto.CapUpgradePush)
		}
		got := helloCapabilities(capabilityProber{vendor: vendor})
		if len(got) != len(want) {
			t.Errorf("vendor %q caps = %v, want %v", vendor, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("vendor %q cap[%d] = %q, want %q", vendor, i, got[i], want[i])
			}
		}
	}
}

func TestHelloCapabilitiesNilProber(t *testing.T) {
	got := helloCapabilities(nil)
	want := []string{"sysinfo", proto.CapConnectionDiagnostics, proto.CapPeerRelayV1}
	if terminal.Supported() {
		want = append(want, proto.CapWebterm)
	}
	want = append(want, "state.update.network", proto.CapBench, proto.CapBenchWarmup, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence, proto.CapBenchRequestTimeout, proto.CapImagePullCandidates, "storage.files", proto.CapCardChecks)
	if upgradeSupported() {
		want = append(want, proto.CapUpgradePush)
	}
	if len(got) != len(want) {
		t.Fatalf("nil prober caps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nil prober cap[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHelloCapabilitiesNeverInfersUdockerContainerAttach(t *testing.T) {
	got := helloCapabilitiesForRuntime(capabilityProber{vendor: "nvidia"}, "udocker")
	for _, cap := range got {
		if cap == proto.CapWebtermContainer || cap == proto.CapWebtermContainerUdocker {
			t.Fatalf("udocker container capability advertised without acceptance probe: %v", got)
		}
	}
}

func TestValidatedContainerCapsAreRuntimeSpecific(t *testing.T) {
	all := []string{proto.CapWebtermContainer, proto.CapWebtermContainerUdocker}
	if got := validatedContainerCaps("docker", all); len(got) != 1 || got[0] != proto.CapWebtermContainer {
		t.Fatalf("docker caps=%v", got)
	}
	if got := validatedContainerCaps("udocker", all); len(got) != 1 || got[0] != proto.CapWebtermContainerUdocker {
		t.Fatalf("udocker caps=%v", got)
	}
}
