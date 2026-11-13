package app

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	nerdctldrv "gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/proto"
)

// runtimeBoundProber stands for a backend whose device injection differs by
// container runtime and that names the runtimes it has been verified on,
// refusing the rest rather than assuming one. Kunlun is such a backend
// (Linux amd64 with Docker); Ascend is another, with a different answer per
// runtime instead of a refusal.
type runtimeBoundProber struct {
	*gpu.FixedProber
	verified string
}

func (p runtimeBoundProber) RuntimeInjectionForDriver(indexes []int, driverName string) gpu.RuntimeInjection {
	if driverName != p.verified {
		return gpu.RuntimeInjection{UnsupportedReason: "runtime_unsupported: requires " + p.verified + ", not " + driverName}
	}
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/fake0:/dev/fake0:rwm"}}
}

// The doctor's job is to predict what a deployment will do on this host, so
// the injection fact has to be asked about the runtime this node actually
// runs. Answering for some other runtime is worse than saying nothing: the
// server refuses the deployment while the report says the host is fine.
//
// go test ./internal/agent/app -run '^TestDoctorDeviceInjectionFactFollowsTheNodesRuntime$' -count=1
func TestDoctorDeviceInjectionFactFollowsTheNodesRuntime(t *testing.T) {
	// No container runtime on PATH, so the plugin probe invents nothing.
	t.Setenv("PATH", t.TempDir())
	cards := []proto.GPUInfo{{Index: 0, Healthy: true, Vendor: "kunlunxin", Name: "P800 PCIe"}}
	prober := runtimeBoundProber{FixedProber: &gpu.FixedProber{VendorName: "kunlunxin", GPUs: cards}, verified: "docker"}

	for _, tc := range []struct {
		driver, want string
	}{
		{"docker", "explicit"},
		{"nerdctl", "unavailable: runtime_unsupported: requires docker, not nerdctl"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			driver := dockerdrv.New()
			report := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{}}
			if tc.driver == "nerdctl" {
				doctorDeviceFacts(context.Background(), &report, prober, nerdctldrv.New(), cards)
			} else {
				doctorDeviceFacts(context.Background(), &report, prober, driver, cards)
			}
			got, ok := factValue(report, proto.DoctorFactDeviceInjection)
			if !ok || got != tc.want {
				t.Fatalf("on %s the injection fact = %q (present %v), want %q", tc.driver, got, ok, tc.want)
			}
		})
	}
}

// The plain Prober seam leaves out the container runtime, so a backend that
// implements a more specific one is never asked through it: that caller would
// have to invent the runtime, and the answer would be about another host.
//
// go test ./internal/agent/app -run '^TestDoctorNeverAsksARuntimeBoundBackendWithoutTheRuntime$' -count=1
func TestDoctorNeverAsksARuntimeBoundBackendWithoutTheRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cards := []proto.GPUInfo{{Index: 0, Healthy: true, Vendor: "kunlunxin", Name: "P800 PCIe"}}
	prober := refusingBaseSeamProber{runtimeBoundProber{FixedProber: &gpu.FixedProber{VendorName: "kunlunxin", GPUs: cards}, verified: "docker"}}
	report := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{}}
	doctorDeviceFacts(context.Background(), &report, prober, dockerdrv.New(), cards)
	if got, ok := factValue(report, proto.DoctorFactDeviceInjection); !ok || got != "explicit" {
		t.Fatalf("injection fact = %q (present %v), want %q", got, ok, "explicit")
	}
	if _, err := doctorWorkloadSpec(context.Background(), prober, "docker", cards); err != nil {
		t.Fatalf("workload spec: %v", err)
	}
}

// refusingBaseSeamProber fails the test if the plain seam is used at all.
type refusingBaseSeamProber struct{ runtimeBoundProber }

func (p refusingBaseSeamProber) RuntimeInjection([]int) gpu.RuntimeInjection {
	return gpu.RuntimeInjection{UnsupportedReason: "the plain seam was used, which cannot know the container runtime"}
}

// The refusal a runtime-bound backend gives is carried through verbatim, so a
// report names the host fact that has to change.
func TestDoctorInjectionRefusalNamesTheRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cards := []proto.GPUInfo{{Index: 0, Healthy: true, Vendor: "kunlunxin"}}
	prober := runtimeBoundProber{FixedProber: &gpu.FixedProber{VendorName: "kunlunxin", GPUs: cards}, verified: "docker"}
	report := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{}}
	doctorDeviceFacts(context.Background(), &report, prober, nerdctldrv.New(), cards)
	got, _ := factValue(report, proto.DoctorFactDeviceInjection)
	if !strings.Contains(got, "nerdctl") || !strings.Contains(got, "docker") {
		t.Fatalf("the refusal hides the runtimes involved: %q", got)
	}
}
