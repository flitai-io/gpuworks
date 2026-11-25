package engine

import (
	"log/slog"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

type namedVendorProber struct {
	injectionProber
	vendor string
}

func (p namedVendorProber) Vendor() string { return p.vendor }

// The workload the runtime receives says whose cards it was granted. A
// runtime reads that instead of inferring a vendor from the variables, so it
// has to be there exactly when cards were granted: not for a workload that
// selected none, and not when the grant was refused.
func TestDeviceInjectionNamesTheVendorOnTheWorkload(t *testing.T) {
	grant := gpu.RuntimeInjection{
		Env:   map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
		Grant: runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/card0", Access: runtime.DeviceAccessAll}}},
	}
	for _, tc := range []struct {
		name    string
		inj     gpu.RuntimeInjection
		indexes []int
		want    string
		wantErr bool
	}{
		{"granted", grant, []int{0}, accelerator.VendorPPU, false},
		{"no cards selected", grant, nil, "", false},
		{"refused", gpu.RuntimeInjection{UnsupportedReason: "card is gone"}, []int{0}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := namedVendorProber{injectionProber: injectionProber{inj: tc.inj}, vendor: accelerator.VendorPPU}
			e := NewWithProber(&headlessTestDriver{name: "udocker"}, nil, slog.Default(), t.TempDir(), p)
			ws := runtime.WorkloadSpec{Env: map[string]string{"KEEP": "value"}}
			err := e.applyDeviceInjection(&ws, tc.indexes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if ws.Grant.Vendor != tc.want {
				t.Fatalf("Grant.Vendor = %q, want %q", ws.Grant.Vendor, tc.want)
			}
		})
	}
}

// Granted devices are the workload's typed fields. Its own options are left
// exactly as they were, so nothing downstream can mistake a granted device
// for one the user asked for, or find it twice.
func TestDeviceInjectionLeavesTheWorkloadsOwnOptionsAlone(t *testing.T) {
	grant := gpu.RuntimeInjection{
		Env: map[string]string{"HIP_VISIBLE_DEVICES": "0"},
		Grant: runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/kfd", Access: runtime.DeviceAccessAll}, {Path: "/dev/dri/renderD129", Access: runtime.DeviceAccessAll}},
			Groups: []int{39}},
	}
	p := namedVendorProber{injectionProber: injectionProber{inj: grant}, vendor: accelerator.VendorHygon}
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
	own := []string{"--ipc=host", "--device", "/dev/infiniband"}
	ws := runtime.WorkloadSpec{DockerArgs: append([]string(nil), own...)}
	if err := e.applyDeviceInjection(&ws, []int{1}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ws.DockerArgs, own) {
		t.Fatalf("the workload's own options became %q, want %q", ws.DockerArgs, own)
	}
	wantDevices := []runtime.Device{{Path: "/dev/kfd", Access: "rwm"}, {Path: "/dev/dri/renderD129", Access: "rwm"}}
	if !reflect.DeepEqual(ws.Grant.Devices, wantDevices) || !reflect.DeepEqual(ws.Grant.Groups, []int{39}) {
		t.Fatalf("granted devices = %+v, groups = %v", ws.Grant.Devices, ws.Grant.Groups)
	}
	want := []string{"--ipc=host", "--device", "/dev/infiniband", "--device", "/dev/kfd:/dev/kfd:rwm", "--device", "/dev/dri/renderD129:/dev/dri/renderD129:rwm", "--group-add", "39"}
	if got := ws.DockerRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("docker run arguments = %q, want %q", got, want)
	}
}

type viewlessDriver struct {
	*headlessTestDriver
}

func (d viewlessDriver) DeviceView() runtime.DeviceView { return runtime.ViewUnknown }

// No devices are granted for a runtime that does not say which devices its
// containers see, or for no runtime at all: each backend would otherwise
// number the selection by its own guess. Selecting no cards is not refused.
func TestDeviceInjectionRefusesARuntimeWhoseDeviceViewIsUnknown(t *testing.T) {
	grant := gpu.RuntimeInjection{Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"}, Grant: runtime.DeviceGrant{ToolkitDevices: []int{0}}}
	for _, tc := range []struct {
		name   string
		driver runtime.Driver
	}{
		{"no driver", nil},
		{"an unregistered driver without a view", viewlessDriver{&headlessTestDriver{name: "podman"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewWithProber(tc.driver, nil, slog.Default(), t.TempDir(), namedVendorProber{injectionProber: injectionProber{inj: grant}, vendor: accelerator.VendorNVIDIA})
			ws := runtime.WorkloadSpec{Env: map[string]string{"KEEP": "value"}}
			before := cloneWorkloadSpec(ws)
			if err := e.applyDeviceInjection(&ws, []int{0}); err == nil {
				t.Fatal("devices were granted for a runtime whose device view is unknown")
			}
			if !reflect.DeepEqual(ws, before) {
				t.Fatalf("the refused injection changed the workload: %+v", ws)
			}
			if err := e.applyDeviceInjection(&ws, nil); err != nil {
				t.Fatalf("selecting no cards was refused: %v", err)
			}
		})
	}
}
