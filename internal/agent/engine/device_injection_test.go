package engine

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestApplyDeviceInjectionEmptySelectionIsNoOp(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), injectionProber{
		inj: gpu.RuntimeInjection{UnsupportedReason: "must not inject an empty selection"},
	})
	ws := runtime.WorkloadSpec{
		Env:        map[string]string{"CUDA_VISIBLE_DEVICES": "all", "KEEP": "value"},
		DockerArgs: []string{"--device", "/dev/example", "--privileged"},
		Mounts:     []runtime.Mount{{Source: "/models", Target: "/models", ReadOnly: true}},
	}
	before := cloneWorkloadSpec(ws)
	if err := e.applyDeviceInjection(&ws, nil); err != nil {
		t.Fatalf("empty selection: %v", err)
	}
	if !reflect.DeepEqual(ws, before) {
		t.Fatalf("empty selection changed workload: got %+v want %+v", ws, before)
	}
}

func TestApplyDeviceInjectionFailureLeavesWorkloadUnchanged(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), injectionProber{inj: gpu.RuntimeInjection{
		ClearEnv:          []string{"CUDA_VISIBLE_DEVICES"},
		Env:               map[string]string{"VISIBLE": "0"},
		Mounts:            []gpu.RuntimeMount{{Source: "/host/driver", Target: "/driver"}},
		UnsupportedReason: "selected render device disappeared",
		Grant:             runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/selected"}}},
	}})
	ws := runtime.WorkloadSpec{
		Env:        map[string]string{"CUDA_VISIBLE_DEVICES": "host", "KEEP": "value"},
		DockerArgs: []string{"--device", "/dev/host", "--ipc=host"},
		Mounts:     []runtime.Mount{{Source: "/models", Target: "/models"}},
	}
	before := cloneWorkloadSpec(ws)
	err := e.applyDeviceInjection(&ws, []int{3})
	if err == nil || !strings.Contains(err.Error(), "selected render device disappeared") {
		t.Fatalf("err = %v, want the backend's own refusal", err)
	}
	if !reflect.DeepEqual(ws, before) {
		t.Fatalf("failed injection changed workload: got %+v want %+v", ws, before)
	}
}

func TestApplyDeviceInjectionAppliesProberManagedSelections(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "nerdctl"}, nil, slog.Default(), t.TempDir(), injectionProber{inj: gpu.RuntimeInjection{
		ClearEnv: []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES"},
		Env:      map[string]string{"AMD_TEST_VISIBLE": "7"},
		Grant: runtime.DeviceGrant{
			Devices: []runtime.Device{{Path: "/dev/kfd", Access: runtime.DeviceAccessAll}, {Path: "/dev/dri/renderD135", Access: runtime.DeviceAccessAll}},
			Replaces: runtime.Replaced{RuntimeClass: true, Devices: []string{
				"/dev/dri", "/dev/dri/by-path", "/dev/dri/by-path/pci-0000:43:00.0-render", "/dev/dri/card7", "/dev/dri/card8", "/dev/dri/renderD135", "/dev/dri/renderD136", "/dev/kfd",
			}},
		},
	}})
	ws := runtime.WorkloadSpec{
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "all", "ROCR_VISIBLE_DEVICES": "0", "KEEP": "value"},
		DockerArgs: []string{
			"--device=/dev/dri/renderD136:/dev/dri/renderD136:rwm",
			"--device=/dev/dri/by-path/pci-0000:43:00.0-render:/dev/dri/renderD135:rwm",
			"--device", "/dev/dri/by-path:/dev/dri/by-path:rwm",
			"--device", "/dev/dri/card8:/dev/dri/card8:rwm",
			"--device", "/dev/dri:/dev/dri:rwm",
			"--device", "/dev/infiniband/uverbs0:/dev/infiniband/uverbs0:rwm",
			"--device", "/dev/dri-other/renderD136:/dev/dri-other/renderD136:rwm",
			"--runtime=nvidia", "--ipc=host",
		},
	}
	if err := e.applyDeviceInjection(&ws, []int{7}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ws.Env["CUDA_VISIBLE_DEVICES"]; ok {
		t.Fatalf("managed CUDA selection survived: %+v", ws.Env)
	}
	if _, ok := ws.Env["ROCR_VISIBLE_DEVICES"]; ok {
		t.Fatalf("managed ROCR selection survived: %+v", ws.Env)
	}
	if ws.Env["KEEP"] != "value" || ws.Env["AMD_TEST_VISIBLE"] != "7" {
		t.Fatalf("unrelated or injected environment lost: %+v", ws.Env)
	}
	wantArgs := []string{
		"--device", "/dev/infiniband/uverbs0:/dev/infiniband/uverbs0:rwm",
		"--device", "/dev/dri-other/renderD136:/dev/dri-other/renderD136:rwm",
		"--ipc=host",
		"--device", "/dev/kfd:/dev/kfd:rwm",
		"--device", "/dev/dri/renderD135:/dev/dri/renderD135:rwm",
	}
	if !reflect.DeepEqual(ws.DockerRunArgs(), wantArgs) {
		t.Fatalf("DockerArgs = %v, want %v", ws.DockerRunArgs(), wantArgs)
	}
}

func TestNonAMDInjectorsKeepEnvironmentValuesAndArgumentMerge(t *testing.T) {
	for _, vendor := range []string{"nvidia", "ascend"} {
		t.Run(vendor, func(t *testing.T) {
			prober := &gpu.FixedProber{VendorName: vendor}
			inj := prober.RuntimeInjection([]int{7})
			if len(inj.ClearEnv) != 0 || !reflect.DeepEqual(inj.Grant.Replaces, runtime.Replaced{}) {
				t.Fatalf("%s injector unexpectedly requests managed cleanup: %+v", vendor, inj)
			}
			e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), prober)
			originalEnv := map[string]string{"KEEP": "old", "CUDA_VISIBLE_DEVICES": "legacy"}
			ws := runtime.WorkloadSpec{
				Env: originalEnv,
				DockerArgs: []string{
					"--device", "/dev/dri/renderD135:/dev/dri/renderD135:rwm",
					"--gpus", "all",
					"--ipc=host",
				},
			}
			if err := e.applyDeviceInjection(&ws, []int{7}); err != nil {
				t.Fatal(err)
			}
			if ws.Env["KEEP"] != "old" {
				t.Fatalf("unrelated env changed: %v", ws.Env)
			}
			// Docker's --gpus renumbers the selected card to container ordinal 0.
			if vendor == "nvidia" && ws.Env["CUDA_VISIBLE_DEVICES"] != "0" {
				t.Fatalf("NVIDIA container CUDA selector = %q, want 0", ws.Env["CUDA_VISIBLE_DEVICES"])
			}
			if vendor == "ascend" && ws.Env["CUDA_VISIBLE_DEVICES"] != "legacy" {
				t.Fatalf("legacy unrelated CUDA selector = %q, want legacy", ws.Env["CUDA_VISIBLE_DEVICES"])
			}
			// Every injector isolates caller-owned environment storage.
			ws.Env["ALIAS_CHECK"] = "same-map"
			if _, aliased := originalEnv["ALIAS_CHECK"]; aliased {
				t.Fatal("injection mutated the caller environment map")
			}
			devicePreserved := false
			for _, arg := range ws.DockerRunArgs() {
				if arg == "/dev/dri/renderD135:/dev/dri/renderD135:rwm" {
					devicePreserved = true
					break
				}
			}
			if !devicePreserved {
				t.Fatalf("legacy injection scanned/removed an undeclared device path: %v", ws.DockerRunArgs())
			}
			// The injected --gpus replaces the image-level --gpus all.
			wantArgs := []string{"--device", "/dev/dri/renderD135:/dev/dri/renderD135:rwm", "--ipc=host", "--gpus", `"device=7"`}
			if vendor != "nvidia" {
				wantArgs = []string{
					"--device", "/dev/dri/renderD135:/dev/dri/renderD135:rwm",
					"--gpus", "all", "--ipc=host", "--runtime", "ascend",
				}
			}
			if !reflect.DeepEqual(ws.DockerRunArgs(), wantArgs) {
				t.Fatalf("legacy %s DockerArgs = %v, want %v", vendor, ws.DockerRunArgs(), wantArgs)
			}
		})
	}
}

func cloneWorkloadSpec(spec runtime.WorkloadSpec) runtime.WorkloadSpec {
	clone := spec
	clone.DockerArgs = append([]string(nil), spec.DockerArgs...)
	clone.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	if spec.Env != nil {
		clone.Env = make(map[string]string, len(spec.Env))
		for key, value := range spec.Env {
			clone.Env[key] = value
		}
	}
	return clone
}

func TestApplyDeviceInjectionReplacesImageGPUSelectionForDocker(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), injectionProber{
		inj: gpu.RuntimeInjection{
			Env:   map[string]string{"CUDA_VISIBLE_DEVICES": "2,4"},
			Grant: runtime.DeviceGrant{ToolkitDevices: []int{2, 4}},
		},
	})
	ws := runtime.WorkloadSpec{
		Env:        map[string]string{},
		DockerArgs: []string{"--ipc=host", "--gpus", "all", "--gpus=all", "--privileged"},
	}

	if err := e.applyDeviceInjection(&ws, []int{2, 4}); err != nil {
		t.Fatal(err)
	}

	wantArgs := []string{"--ipc=host", "--privileged", "--gpus", `"device=2,4"`}
	if fmt.Sprint(ws.DockerRunArgs()) != fmt.Sprint(wantArgs) {
		t.Fatalf("DockerArgs = %v, want %v", ws.DockerRunArgs(), wantArgs)
	}
	if got := ws.Env["CUDA_VISIBLE_DEVICES"]; got != "0,1" {
		t.Fatalf("container CUDA_VISIBLE_DEVICES = %q, want compact indexes", got)
	}
}

func TestApplyDeviceInjectionKeepsPhysicalCUDAIndexesForUdocker(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "udocker"}, nil, slog.Default(), t.TempDir(), injectionProber{
		inj: gpu.RuntimeInjection{
			Env:   map[string]string{"CUDA_VISIBLE_DEVICES": "2,4"},
			Grant: runtime.DeviceGrant{ToolkitDevices: []int{2, 4}},
		},
	})
	ws := runtime.WorkloadSpec{Env: map[string]string{}}

	if err := e.applyDeviceInjection(&ws, []int{2, 4}); err != nil {
		t.Fatal(err)
	}
	if got := ws.Env["CUDA_VISIBLE_DEVICES"]; got != "2,4" {
		t.Fatalf("udocker CUDA_VISIBLE_DEVICES = %q, want physical indexes", got)
	}
}
