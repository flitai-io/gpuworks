package gpu

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func validKunlunMultiNodeEnv() map[string]string {
	return map[string]string{"BKCL_RDMA_NICS": "bond0,bond0", "GLOO_SOCKET_IFNAME": "bond0"}
}

// Integrate the server's actual network resolution with native device injection.
// The network module owns verbs/limits; Kunlun adds only selected XPUs and XDR.
func TestKunlunConsumesResolvedHostRDMA(t *testing.T) {
	engine, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	for _, finite := range []bool{false, true} {
		params := deploy.Params{Vendor: "kunlunxin", ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, MasterAddr: "192.0.2.1", MasterPort: 29500, ComputeNetwork: deploy.ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_1:1"}}}
		in := deploy.ResolveLaunchInput{Engine: engine, Params: params}
		if finite {
			in.ContainerOverrides.DockerArgs = []string{"--ulimit", "memlock=4096"}
		}
		launch, err := deploy.ResolveLaunch(in)
		if err != nil {
			t.Fatal(err)
		}
		launch.Env["BKCL_RDMA_NICS"] = "bond0,bond0"
		p, _ := kunlunFixture(t)
		if _, err := p.Probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
		req := InjectionRequest{Indexes: []int{1}, DriverName: "Docker", NNodes: 2, DockerArgs: launch.DockerArgs, Env: launch.Env}
		inj := p.RuntimeInjectionForLaunch(t.Context(), req)
		if finite {
			if !strings.Contains(inj.UnsupportedReason, "finite") || len(inj.DockerArgs) != 0 {
				t.Fatalf("finite limit accepted: %+v", inj)
			}
			continue
		}
		if inj.UnsupportedReason != "" {
			t.Fatal(inj.UnsupportedReason)
		}
		combined := append(append([]string(nil), launch.DockerArgs...), inj.DockerArgs...)
		for _, value := range []string{"/dev/infiniband", "memlock=-1", "IPC_LOCK", "/dev/xdrdrv", "/dev/xpu1:/dev/xpu1:rwm"} {
			count := 0
			for _, arg := range combined {
				if arg == value {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%q appears %d times in %v", value, count, combined)
			}
		}
		if launch.Env["NCCL_IB_HCA"] != "mlx5_1:1" {
			t.Fatalf("selected rail changed: %v", launch.Env)
		}
		wantEnv := map[string]string{"BKCL_ENABLE_XDR": "1", "BKCL_RDMA_VERBS": "1", "BKCL_ENABLE_IBGDA": "0", "BKCL_SOCKET_IFNAME": "bond0", "XPU_VISIBLE_DEVICES": "0"}
		if !reflect.DeepEqual(inj.Env, wantEnv) {
			t.Fatalf("injected environment = %v, want %v", inj.Env, wantEnv)
		}
	}
}

func TestKunlunMultiNodeRequiresBKCLNetworkEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "missing RDMA NICs", env: map[string]string{"GLOO_SOCKET_IFNAME": "bond0"}, want: "set BKCL_RDMA_NICS to the verified RDMA netdev list for this node"},
		{name: "blank RDMA NICs", env: map[string]string{"BKCL_RDMA_NICS": " \t ", "GLOO_SOCKET_IFNAME": "bond0"}, want: "set BKCL_RDMA_NICS to the verified RDMA netdev list for this node"},
		{name: "missing compute network", env: map[string]string{"BKCL_RDMA_NICS": "bond0,bond0"}, want: "Kunlun multi-node inference requires a selected compute network"},
		{name: "loopback compute network", env: map[string]string{"BKCL_RDMA_NICS": "bond0,bond0", "GLOO_SOCKET_IFNAME": "lo"}, want: "Kunlun multi-node inference requires a selected compute network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := kunlunFixture(t)
			if _, err := p.Probe(t.Context()); err != nil {
				t.Fatal(err)
			}
			withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
			inj := p.RuntimeInjectionForLaunch(t.Context(), InjectionRequest{
				Indexes: []int{1}, DriverName: "docker", NNodes: 2,
				DockerArgs: []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"},
				Env:        tc.env,
			})
			if inj.UnsupportedReason != tc.want || inj.Failure == nil || len(inj.DockerArgs) != 0 || len(inj.Env) != 0 {
				t.Fatalf("injection = %+v, want reason %q", inj, tc.want)
			}
		})
	}
}

func TestKunlunSingleNodeDoesNotInjectBKCLEnvironment(t *testing.T) {
	p, _ := kunlunFixture(t)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	inj := p.RuntimeInjectionForLaunch(t.Context(), InjectionRequest{Indexes: []int{1}, DriverName: "docker", NNodes: 1, Env: validKunlunMultiNodeEnv()})
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if len(inj.Env) != 1 || inj.Env["XPU_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("single-node environment = %v", inj.Env)
	}
}

func TestKunlunSingleNodeMultiGPURequiresAndInjectsInterface(t *testing.T) {
	p, _ := kunlunFixture(t)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := InjectionRequest{Indexes: []int{0, 1}, DriverName: "docker", NNodes: 1, Env: map[string]string{"GLOO_SOCKET_IFNAME": "br_eno1_31"}}
	inj := p.RuntimeInjectionForLaunch(t.Context(), req)
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if len(inj.Env) != 2 || inj.Env["BKCL_SOCKET_IFNAME"] != "br_eno1_31" || inj.Env["XPU_VISIBLE_DEVICES"] != "0,1" {
		t.Fatalf("single-node multi-GPU environment = %v", inj.Env)
	}
	req.Env["GLOO_SOCKET_IFNAME"] = "lo"
	inj = p.RuntimeInjectionForLaunch(t.Context(), req)
	if inj.UnsupportedReason != "Kunlun multi-GPU inference requires a selected compute network" || inj.Failure == nil {
		t.Fatalf("loopback multi-GPU injection = %+v", inj)
	}
}

func TestKunlunFreshInjectionHonorsCallerCancellation(t *testing.T) {
	p, _ := kunlunFixture(t)
	p.command = func(ctx context.Context, _ ...string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	inj := p.RuntimeInjectionForLaunch(ctx, InjectionRequest{Indexes: []int{1}, DriverName: "docker", NNodes: 1})
	if !strings.Contains(inj.UnsupportedReason, "deadline exceeded") || len(inj.DockerArgs) != 0 {
		t.Fatalf("cancelled injection: %+v", inj)
	}
}

func TestKunlunFreshExecutableRespectsDeadline(t *testing.T) {
	p, _ := kunlunFixture(t)
	exe := t.TempDir() + "/xpu-smi"
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p.command = nil
	p.lookPath = func(string) (string, error) { return exe, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	inj := p.RuntimeInjectionForLaunch(ctx, InjectionRequest{Indexes: []int{0}, DriverName: "docker", NNodes: 1})
	if !strings.Contains(inj.UnsupportedReason, "deadline exceeded") || len(inj.DockerArgs) != 0 {
		t.Fatalf("deadline ignored: %+v", inj)
	}
}
