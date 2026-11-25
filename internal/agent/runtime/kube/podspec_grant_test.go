package kube

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// What a grant asks of the pod: toolkit devices become the NVIDIA runtime
// class and its device list, a granted runtime class is the pod's, device
// nodes are handed to the device plugin in the order granted, and groups
// become supplementary groups.
func TestGrantReachesThePodTranslation(t *testing.T) {
	tr, err := workloadTranslation(runtime.WorkloadSpec{Grant: runtime.DeviceGrant{ToolkitDevices: []int{1, 3}}})
	if err != nil || tr.RuntimeClass != "nvidia" || tr.Env["NVIDIA_VISIBLE_DEVICES"] != "1,3" {
		t.Fatalf("toolkit devices: class %q env %v err %v", tr.RuntimeClass, tr.Env, err)
	}
	tr, err = workloadTranslation(runtime.WorkloadSpec{Grant: runtime.DeviceGrant{
		RuntimeClass: "ascend",
		Devices:      []runtime.Device{{Path: "/dev/davinci2"}, {Path: "/dev/kfd", Access: runtime.DeviceAccessAll}},
		Groups:       []int{39, 998},
	}})
	if err != nil || tr.RuntimeClass != "ascend" {
		t.Fatalf("runtime class %q, %v", tr.RuntimeClass, err)
	}
	if want := []string{"/dev/davinci2", "/dev/kfd:/dev/kfd:rwm"}; !reflect.DeepEqual(tr.Devices, want) {
		t.Fatalf("devices = %v, want %v", tr.Devices, want)
	}
	if want := []int64{39, 998}; !reflect.DeepEqual(tr.Groups, want) {
		t.Fatalf("groups = %v, want %v", tr.Groups, want)
	}
}

// The granted selection replaces a singleton among the workload's own
// options and leaves the rest: an image's `--gpus all` and a user's
// `--runtime` give way, a compute network's device stays.
func TestGrantReplacesTheWorkloadsOwnSingletons(t *testing.T) {
	own := []string{"--gpus", "all", "--runtime=custom", "--device", "/dev/infiniband"}
	tr, err := workloadTranslation(runtime.WorkloadSpec{
		DockerArgs: own,
		Grant:      runtime.DeviceGrant{RuntimeClass: "ascend", ToolkitDevices: []int{2}, Devices: []runtime.Device{{Path: "/dev/card"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.RuntimeClass != "ascend" || tr.Env["NVIDIA_VISIBLE_DEVICES"] != "2" {
		t.Fatalf("class %q env %v; want the granted ones", tr.RuntimeClass, tr.Env)
	}
	if want := []string{"/dev/infiniband", "/dev/card"}; !reflect.DeepEqual(tr.Devices, want) {
		t.Fatalf("devices = %v, want %v", tr.Devices, want)
	}
	// Without a grant the workload's own are the pod's.
	tr, err = workloadTranslation(runtime.WorkloadSpec{DockerArgs: own})
	if err != nil || tr.RuntimeClass != "custom" || tr.Env["NVIDIA_VISIBLE_DEVICES"] != "all" {
		t.Fatalf("ungranted: class %q env %v err %v", tr.RuntimeClass, tr.Env, err)
	}
}

// An option a pod cannot express is refused whether or not devices were
// granted.
func TestWorkloadTranslationRefusesUnknownOptions(t *testing.T) {
	_, err := workloadTranslation(runtime.WorkloadSpec{DockerArgs: []string{"--oom-kill-disable"}, Grant: runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/kfd"}}}})
	if err == nil {
		t.Fatal("an option with no Kubernetes equivalent was accepted")
	}
}
