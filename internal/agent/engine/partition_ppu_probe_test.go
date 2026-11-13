package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/assets"
)

const probeDeviceUUID = "GPU-00000000-0000-0000-0000-000000000008"

type probePPUProber struct {
	gpu.FixedProber
	library string
}

func (p *probePPUProber) RuntimeInjectionForDriver(indexes []int, driver string) gpu.RuntimeInjection {
	if len(indexes) != 1 || indexes[0] != 8 {
		return gpu.RuntimeInjection{UnsupportedReason: "fixture only has device 8"}
	}
	visible := "0"
	if driver == "udocker" {
		visible = "8"
	}
	return gpu.RuntimeInjection{Env: map[string]string{accelerator.EnvCUDAVisibleDevices: visible, accelerator.PPUEnvDeviceUUIDs: probeDeviceUUID}}
}

func (p *probePPUProber) PartitionProbeLibraries(context.Context) ([]gpu.ProbeLibrary, error) {
	return []gpu.ProbeLibrary{{Name: "libhgml.so", Source: p.library}}, nil
}

// Exercise the real workload builder, embedded quota hook and C assertion
// together. Only the device inventory and HGML driver ABI are fixtures.
func TestPPUProbeUsesSeparateUUIDWithNumericVisibility(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("PPU asset is Linux amd64 only")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("C compiler unavailable")
	}
	fixture := t.TempDir()
	probe := filepath.Join(fixture, "probe")
	library := filepath.Join(fixture, "libhgml.so")
	for _, args := range [][]string{
		{"-Wall", "-Wextra", "-Werror", "../../../build/runtime-assets/probe/probe.c", "-ldl", "-o", probe},
		{"-shared", "-fPIC", "-Wall", "-Wextra", "-Werror", "../../assets/testdata/ppu_hgml_fixture.c", "-o", library},
	} {
		if out, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
			t.Fatalf("compile: %v\n%s", err, out)
		}
	}
	for _, driver := range []string{"docker", "nerdctl", "udocker"} {
		t.Run(driver, func(t *testing.T) {
			state := t.TempDir()
			if _, err := assets.InstallDomesticRuntimeAssets(state, "ppu"); err != nil {
				t.Fatal(err)
			}
			p := &probePPUProber{library: library}
			e := &Engine{stateDir: state, prober: p, driver: &headlessTestDriver{name: driver}}
			ws, err := e.BuildPartitionProbeWorkload(t.Context(), accelerator.BackendPPULogical, assets.PartitionProbeImage, 8, 4<<30, "probe-uuid")
			if err != nil {
				t.Fatal(err)
			}
			visible, managementIndex := "0", 0
			if driver == "udocker" {
				visible, managementIndex = "8", 8
			}
			if ws.Env[accelerator.EnvCUDAVisibleDevices] != visible {
				t.Fatalf("unexpected compute selection: %+v", ws.Env)
			}
			// Materialize container mount targets on the host for the C process.
			var mounts []string
			for _, mount := range ws.Mounts {
				mounts = append(mounts, mount.Target, mount.Source)
			}
			replace := strings.NewReplacer(mounts...)
			for _, tc := range []struct {
				name, uuid string
				noHook     bool
				code       int
			}{
				{name: "builder output"},
				{name: "numeric identity rejected", uuid: visible, code: 2},
				{name: "missing identity rejected", uuid: "unset", code: 2},
				{name: "wrong identity rejected", uuid: "GPU-00000000-0000-0000-0000-000000000009", code: 7},
				{name: "unlimited memory rejected", noHook: true, code: 7},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cmd := exec.Command(probe)
					for key, value := range ws.Env {
						switch key {
						case "GPUWORKS_PARTITION_PROBE_UUID":
							if tc.uuid == "unset" {
								continue
							}
							if tc.uuid != "" {
								value = tc.uuid
							}
						case accelerator.EnvLDPreload:
							value = replace.Replace(value)
							if tc.noHook {
								value = ""
							}
						case accelerator.EnvLDLibraryPath:
							value = fixture
						case accelerator.PPUEnvLedgerPath:
							value = filepath.Join(t.TempDir(), "ledger")
						}
						cmd.Env = append(cmd.Env, key+"="+value)
					}
					if driver != "udocker" {
						cmd.Env = append(cmd.Env, "COMPACT=1")
					}
					out, err := cmd.CombinedOutput()
					if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
						t.Fatalf("exit=%v err=%v out=%s; want %d", cmd.ProcessState, err, out, tc.code)
					}
					if tc.code == 0 && (!strings.Contains(string(out), "uuid="+probeDeviceUUID) || !strings.Contains(string(out), fmt.Sprintf("index=%d rc=0", managementIndex)) || !strings.Contains(string(out), "matches=1 total_bytes=4294967296")) {
						t.Fatalf("wrong identity or quota passed: %s", out)
					}
				})
			}
		})
	}
}

// On Kubernetes the probe pod takes the host's network, as instance pods do,
// so it does not depend on the cluster's network plugin; docker keeps its
// default network.
func TestPartitionProbeUsesTheHostNetworkOnlyOnKubernetes(t *testing.T) {
	for driver, want := range map[string]bool{"kube": true, "docker": false, "nerdctl": false} {
		t.Run(driver, func(t *testing.T) {
			state := t.TempDir()
			if _, err := assets.InstallDomesticRuntimeAssets(state, "ppu"); err != nil {
				t.Fatal(err)
			}
			d := &headlessTestDriver{name: driver}
			e := &Engine{stateDir: state, prober: &probePPUProber{library: filepath.Join(t.TempDir(), "libhgml.so")}, driver: d, hostRoot: hostRootFor(d)}
			if driver == "kube" {
				e.hostRoot = t.TempDir() // the host's root as the kube agent sees it
			}
			ws, err := e.BuildPartitionProbeWorkload(t.Context(), accelerator.BackendPPULogical, assets.PartitionProbeImage, 8, 4<<30, "probe-net")
			if err != nil {
				t.Fatal(err)
			}
			if ws.HostNetwork != want {
				t.Fatalf("%s probe HostNetwork = %v, want %v", driver, ws.HostNetwork, want)
			}
		})
	}
}
