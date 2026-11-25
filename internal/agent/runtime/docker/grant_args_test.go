package docker

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The run command carries what the workload was granted after its own
// options: without it a container starts with no accelerator at all.
func TestBuildRunArgsRendersGrantedDevices(t *testing.T) {
	args := buildRunArgs(runtime.WorkloadSpec{
		Name: "gpuworks-test", Image: "fake",
		DockerArgs: []string{"--ipc=host"},
		Grant:      runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/davinci2"}, {Path: "/dev/kfd", Access: "rwm"}}, Groups: []int{39}},
	})
	joined := strings.Join(args, " ")
	want := "--ipc=host --device /dev/davinci2 --device /dev/kfd:/dev/kfd:rwm --group-add 39"
	if !strings.Contains(joined, want) {
		t.Fatalf("docker args = %s, want them to contain %q", joined, want)
	}
}
