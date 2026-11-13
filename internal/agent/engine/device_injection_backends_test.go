package engine

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Exercise the real existing backends through the shared extension seam.
// Neither prober needs host I/O for Docker injection, so this also runs on Mac.
func TestExistingBackendInjectionPreservedWithTopology(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prober gpu.Prober
		driver string
		args   []string
		env    map[string]string
	}{
		{"nvidia-docker", &gpu.SMIProber{}, "docker", []string{"--gpus", `"device=3,7"`}, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"}},
		{"nvidia-nerdctl", &gpu.SMIProber{}, "nerdctl", []string{"--gpus", `"device=3,7"`}, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"}},
		{"nvidia-udocker", &gpu.SMIProber{}, "udocker", []string{"--gpus", `"device=3,7"`}, map[string]string{"CUDA_VISIBLE_DEVICES": "3,7"}},
		{"ascend-docker", &gpu.AscendProber{}, "docker", []string{"--runtime", "ascend"}, map[string]string{"ASCEND_VISIBLE_DEVICES": "3,7", "ASCEND_RT_VISIBLE_DEVICES": "0,1", "NPU_VISIBLE_DEVICES": "0,1"}},
	} {
		for _, nodes := range []int{1, 2} {
			t.Run(tc.name, func(t *testing.T) {
				e := NewWithProber(&headlessTestDriver{name: tc.driver}, nil, slog.Default(), t.TempDir(), tc.prober)
				user := []string{"--ulimit=memlock=65536:65536", "--ipc=host"}
				mount := runtime.Mount{Source: "/weights", Target: "/model", ReadOnly: true}
				ws := runtime.WorkloadSpec{Env: map[string]string{"USER_SETTING": "kept"}, DockerArgs: append([]string(nil), user...), Mounts: []runtime.Mount{mount}}
				if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{3, 7}, tc.driver, nodes); err != nil {
					t.Fatal(err)
				}
				wantEnv := map[string]string{"USER_SETTING": "kept"}
				for k, v := range tc.env {
					wantEnv[k] = v
				}
				if !reflect.DeepEqual(ws.DockerArgs, append(user, tc.args...)) || !reflect.DeepEqual(ws.Env, wantEnv) || !reflect.DeepEqual(ws.Mounts, []runtime.Mount{mount}) {
					t.Fatalf("existing backend contract changed: %+v", ws)
				}
				if err := e.applyDeviceInjectionForLaunch(context.Background(), &runtime.WorkloadSpec{}, nil, tc.driver, nodes); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// These backend validators still fail before workload mutation when their CLI
// is absent. The new topology extension must not override their diagnostics.
func TestExistingNativeBackendsKeepTheirValidation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		prober gpu.Prober
		cli    string
	}{
		{&gpu.PPUProber{}, "ppu-smi"}, {&gpu.HygonProber{}, "hy-smi"},
	} {
		e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), tc.prober)
		for _, nodes := range []int{1, 2} {
			ws := runtime.WorkloadSpec{DockerArgs: []string{"--ipc=host"}, Env: map[string]string{"kept": "yes"}}
			err := e.applyDeviceInjectionForLaunch(t.Context(), &ws, []int{0}, "docker", nodes)
			if err == nil || !strings.Contains(err.Error(), tc.cli) || !reflect.DeepEqual(ws.DockerArgs, []string{"--ipc=host"}) || !reflect.DeepEqual(ws.Env, map[string]string{"kept": "yes"}) {
				t.Fatalf("%s: %+v %v", tc.cli, ws, err)
			}
			if err := e.applyDeviceInjectionForLaunch(t.Context(), &ws, nil, "docker", nodes); err != nil {
				t.Fatal(err)
			}
		}
	}
}
