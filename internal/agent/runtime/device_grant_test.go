package runtime

import (
	"reflect"
	"testing"
)

// The rendering is what the docker and nerdctl CLIs receive, so its spelling
// and order are a contract: the runtime class first, the toolkit request
// quoted the way dockerd's CSV parser needs, each device node in the order
// it was granted, and the groups last.
func TestDockerDeviceArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec WorkloadSpec
		want []string
	}{
		{"nothing granted", WorkloadSpec{Env: map[string]string{"X": "1"}, Grant: DeviceGrant{Vendor: "nvidia"}}, nil},
		{"runtime class", WorkloadSpec{Grant: DeviceGrant{RuntimeClass: "ascend"}}, []string{"--runtime", "ascend"}},
		{"toolkit devices", WorkloadSpec{Grant: DeviceGrant{ToolkitDevices: []int{2, 4}}}, []string{"--gpus", `"device=2,4"`}},
		{"one toolkit device", WorkloadSpec{Grant: DeviceGrant{ToolkitDevices: []int{0}}}, []string{"--gpus", `"device=0"`}},
		{
			"device nodes keep their order and their access",
			WorkloadSpec{Grant: DeviceGrant{Devices: []Device{{Path: "/dev/b", Access: "rwm"}, {Path: "/dev/a"}}}},
			[]string{"--device", "/dev/b:/dev/b:rwm", "--device", "/dev/a"},
		},
		{
			"everything",
			WorkloadSpec{Grant: DeviceGrant{RuntimeClass: "runc",
				Devices: []Device{{Path: "/dev/xpu1", Access: "rwm"}, {Path: "/dev/xdrdrv"}},
				Groups:  []int{39, 998}},
			},
			[]string{"--runtime", "runc", "--device", "/dev/xpu1:/dev/xpu1:rwm", "--device", "/dev/xdrdrv", "--group-add", "39", "--group-add", "998"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Grant.DockerDeviceArgs(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DockerDeviceArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The workload's own options come first and are not touched: a granted
// device is added after them, never merged into them.
func TestDockerRunArgsKeepsTheWorkloadsOptionsFirstAndUnchanged(t *testing.T) {
	own := []string{"--ipc=host", "--device", "/dev/infiniband"}
	spec := WorkloadSpec{DockerArgs: own, Grant: DeviceGrant{Devices: []Device{{Path: "/dev/card0", Access: "rwm"}}}}
	want := []string{"--ipc=host", "--device", "/dev/infiniband", "--device", "/dev/card0:/dev/card0:rwm"}
	if got := spec.DockerRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DockerRunArgs() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(spec.DockerArgs, []string{"--ipc=host", "--device", "/dev/infiniband"}) || len(own) != 3 {
		t.Fatalf("the workload's own options were changed: %q", spec.DockerArgs)
	}
}

// A granted runtime class or toolkit request replaces the workload's own, in
// either spelling of the option; without one the workload's own stay.
func TestDockerRunArgsReplacesTheWorkloadsOwnSingletons(t *testing.T) {
	own := []string{"--ipc=host", "--gpus", "all", "--gpus=all", "--runtime", "custom", "--privileged"}
	for _, tc := range []struct {
		name  string
		grant DeviceGrant
		want  []string
	}{
		{"nothing granted", DeviceGrant{}, own},
		{"toolkit devices", DeviceGrant{ToolkitDevices: []int{2, 4}}, []string{"--ipc=host", "--runtime", "custom", "--privileged", "--gpus", `"device=2,4"`}},
		{"runtime class", DeviceGrant{RuntimeClass: "ascend"}, []string{"--ipc=host", "--gpus", "all", "--gpus=all", "--privileged", "--runtime", "ascend"}},
		{"device nodes only", DeviceGrant{Devices: []Device{{Path: "/dev/kfd"}}}, append(append([]string(nil), own...), "--device", "/dev/kfd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := WorkloadSpec{DockerArgs: append([]string(nil), own...), Grant: tc.grant}
			if got := spec.DockerRunArgs(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DockerRunArgs() = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(spec.DockerArgs, own) {
				t.Fatalf("the workload's own options were changed: %q", spec.DockerArgs)
			}
		})
	}
}

// What a grant replaces among the workload's own options is left out where
// they are read, in either spelling; a device it does not manage stays, a
// by-path alias with colons in its name is matched whole, and the options
// themselves are not edited.
func TestOwnDockerArgsLeavesOutWhatTheGrantReplaces(t *testing.T) {
	own := []string{
		"--device", "/dev/dri/renderD128",
		"--device=/dev/dri/by-path/pci-0000:43:00.0-render:/dev/dri/renderD128",
		"--device", "/dev/kfd:/dev/kfd:rwm",
		"--device", "/dev/infiniband",
		"--gpus", "all", "--runtime=nvidia", "--ipc=host",
		"--device",
	}
	spec := WorkloadSpec{DockerArgs: append([]string(nil), own...), Grant: DeviceGrant{Replaces: Replaced{
		ToolkitRequest: true, RuntimeClass: true,
		Devices: []string{"/dev/dri", "/dev/dri/by-path/pci-0000:43:00.0-render", "/dev/dri/renderD128", "/dev/kfd"},
	}}}
	want := []string{"--device", "/dev/infiniband", "--ipc=host", "--device"}
	if got := spec.OwnDockerArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnDockerArgs() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(spec.DockerArgs, own) {
		t.Fatalf("the workload's own options were edited: %q", spec.DockerArgs)
	}
	spec.Grant = DeviceGrant{}
	if got := spec.OwnDockerArgs(); !reflect.DeepEqual(got, own) {
		t.Fatalf("with nothing replaced OwnDockerArgs() = %q, want the options as they are", got)
	}
}

func TestParseOwnOptions(t *testing.T) {
	got := ParseOwnOptions([]string{
		"--device", "/dev/infiniband", "--device=/dev/x:/dev/y:rwm",
		"--cap-add", "IPC_LOCK", "--cap-add=SYS_ADMIN",
		"--ulimit", "memlock=-1", "--ulimit=stack=67108864",
		"--ipc=host", "--privileged", "--ulimit memlock=4096", "--device",
	})
	want := OwnOptions{
		Devices:      []string{"/dev/infiniband", "/dev/x:/dev/y:rwm"},
		Capabilities: []string{"IPC_LOCK", "SYS_ADMIN"},
		Ulimits:      []string{"memlock=-1", "stack=67108864"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseOwnOptions = %+v, want %+v", got, want)
	}
}

func TestWithoutDockerDevicesPreservesUnmanagedMappingsAndMalformedOption(t *testing.T) {
	args := []string{
		"--device", "/dev/dri/renderD135:/dev/dri/renderD135:rwm",
		"--device=/dev/dri:/dev/dri:rwm",
		"--device", "/dev/dri/by-path/pci-0000:43:00.0-render:/dev/dri/renderD135:rwm",
		"--device=/dev/dri/by-path:/dev/dri/by-path:rwm",
		"--device", "/dev/infiniband/uverbs0:/dev/infiniband/uverbs0:rwm",
		"--device=/dev/dri-other/renderD135:/dev/dri-other/renderD135:rwm",
		"--device", "--privileged",
		"--mount", "type=bind,src=/host,dst=/data",
	}
	got := withoutDockerDevices(args, []string{
		"/dev/dri", "/dev/dri/renderD135", "/dev/dri/by-path",
		"/dev/dri/by-path/pci-0000:43:00.0-render",
	})
	want := []string{
		"--device", "/dev/infiniband/uverbs0:/dev/infiniband/uverbs0:rwm",
		"--device=/dev/dri-other/renderD135:/dev/dri-other/renderD135:rwm",
		"--device", "--privileged",
		"--mount", "type=bind,src=/host,dst=/data",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withoutDockerDevices = %v, want %v", got, want)
	}
}
