package engine

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The server resolves template/RDMA defaults and the deployment's explicit
// limits before the agent adds transport devices. Injection must preserve that
// decision, including a finite limit, while still supplying missing defaults.
func TestDeviceInjectionPreservesResolvedUlimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"absent", nil, []string{"--ulimit", "memlock=-1"}},
		{"template", []string{"--ulimit", "memlock=-1:-1"}, []string{"--ulimit", "memlock=-1:-1"}},
		{"finite", []string{"--ulimit", "memlock=4096:4096"}, []string{"--ulimit", "memlock=4096:4096"}},
		{"equals", []string{"--ulimit=memlock=4096"}, []string{"--ulimit=memlock=4096"}},
		{"editor row", []string{"--ulimit memlock=4096"}, []string{"--ulimit memlock=4096"}},
		{"other limit", []string{"--ulimit", "stack=67108864"}, []string{"--ulimit", "stack=67108864", "--ulimit", "memlock=-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injected := []string{"--ulimit", "memlock=-1", "--device", "/dev/example-transport"}
			before := append([]string(nil), injected...)
			p := injectionProber{inj: gpu.RuntimeInjection{DockerArgs: injected}}
			e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
			ws := runtime.WorkloadSpec{DockerArgs: append([]string(nil), tc.args...)}
			if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{1}, "docker", 2); err != nil {
				t.Fatal(err)
			}
			want := append(append([]string(nil), tc.want...), "--device", "/dev/example-transport")
			if !reflect.DeepEqual(ws.DockerArgs, want) {
				t.Fatalf("resolved limits changed: got %v, want %v", ws.DockerArgs, want)
			}
			if !reflect.DeepEqual(p.inj.DockerArgs, before) {
				t.Fatal("injection mutated the prober's defaults")
			}
		})
	}
}

func TestDeviceInjectionUlimitDefaultsKeepOtherOptions(t *testing.T) {
	for _, defaults := range [][]string{
		{"--ulimit", "stack=67108864"},
		{"--ulimit=stack=67108864"},
		{"--ulimit stack=67108864"},
	} {
		own := []string{"--ulimit", "stack=4096", "--device", "/dev/one"}
		injected := append(append([]string(nil), defaults...), "--device", "/dev/two")
		want := append(append([]string(nil), own...), "--device", "/dev/two")
		if got := mergeDeviceDockerArgs(own, injected); !reflect.DeepEqual(got, want) {
			t.Fatalf("non-memlock default or repeated device handled incorrectly: got %v, want %v", got, want)
		}
	}
	for _, malformed := range [][]string{{"--ulimit"}, {"--ulimit", "--device=/dev/one"}, {"--ulimit=4096"}, {"--ulimit==4096"}} {
		if got := mergeDeviceDockerArgs(nil, malformed); !reflect.DeepEqual(got, malformed) {
			t.Fatalf("malformed arguments silently changed: got %v, want %v", got, malformed)
		}
	}
}
