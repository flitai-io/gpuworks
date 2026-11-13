package kube

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

func envMap(vars []kubeapi.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

// volumeByMountPath pairs up pod.Spec.Volumes with the container's
// VolumeMounts by mount path, the way instancePod itself links them (same
// index in both slices).
func volumeByMountPath(pod kubeapi.Pod, mountPath string) (*kubeapi.Volume, *kubeapi.VolumeMount) {
	c := pod.Spec.Containers[0]
	for i, mount := range c.VolumeMounts {
		if mount.MountPath == mountPath {
			return &pod.Spec.Volumes[i], &mount
		}
	}
	return nil, nil
}

// TestInstancePodAMDPartitionShape asserts that an AMD dGPU-partitioned
// instance's WorkloadSpec — exactly what amdPartitionInjector.apply
// (internal/agent/engine/partition_amd.go) builds on top of AMD's whole-card
// device injection (internal/agent/gpu/amd_runtime.go) when the engine's
// hostRoot is set (the kube driver) — becomes a pod with: the libvrocm.so
// shim as a read-only hostPath volume, the host's /dev/shm ledger file as a
// writable hostPath volume mounted at the same path inside the container
// (amdLedgerPath, unchanged from the non-kube env value), device plugin
// resources, and no privileged security context (AMD's whole-card injection
// never asks for --privileged).
func TestInstancePodAMDPartitionShape(t *testing.T) {
	libHost := "/var/lib/gpuworks/dgpu-runtime/amd/linux-amd64/libvrocm.so"
	ledger := "/dev/shm/gpuworks-amd-amd-1"
	spec := runtime.WorkloadSpec{
		Name: "gpuworks-amd-1", Image: "img:rocm",
		DockerArgs: []string{"--device", "/dev/kfd:/dev/kfd:rwm", "--device", "/dev/dri/renderD128:/dev/dri/renderD128:rwm"},
		Mounts: []runtime.Mount{
			{Source: libHost, Target: accelerator.AMDLibraryTarget, ReadOnly: true},
			{Source: ledger, Target: ledger},
		},
		Env: map[string]string{
			accelerator.EnvLDPreload:                  accelerator.AMDLibraryTarget,
			accelerator.AMDEnvMemoryLimitPrefix + "0": "60000",
			accelerator.AMDEnvLedgerPath:              ledger,
		},
	}
	reqs := map[string]string{"gpuworks.ai/accelerator-0": "1", "gpuworks.ai/accelerator-shared": "1"}
	pod, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", DeviceRequests: reqs})
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext != nil {
		t.Fatalf("AMD partitioned instance must not be privileged: %+v", c.SecurityContext)
	}
	if r := c.Resources; r.Limits["gpuworks.ai/accelerator-0"] != "1" || r.Requests["gpuworks.ai/accelerator-shared"] != "1" {
		t.Fatalf("device plugin resources = %+v", r)
	}
	libVol, libMount := volumeByMountPath(pod, accelerator.AMDLibraryTarget)
	if libVol == nil || libVol.HostPath == nil || libVol.HostPath.Path != libHost || !libMount.ReadOnly {
		t.Fatalf("libvrocm.so volume = %+v, mount = %+v", libVol, libMount)
	}
	ledgerVol, ledgerMount := volumeByMountPath(pod, ledger)
	if ledgerVol == nil || ledgerVol.HostPath == nil || ledgerVol.HostPath.Path != ledger {
		t.Fatalf("ledger volume = %+v, want hostPath %s", ledgerVol, ledger)
	}
	if ledgerMount.ReadOnly {
		t.Fatalf("ledger mount must be writable: %+v", ledgerMount)
	}
	env := envMap(c.Env)
	if env[accelerator.EnvLDPreload] != accelerator.AMDLibraryTarget {
		t.Fatalf("LD_PRELOAD = %q", env[accelerator.EnvLDPreload])
	}
	if env[accelerator.AMDEnvLedgerPath] != ledger {
		t.Fatalf("ledger env = %q, want %q (same path as the non-kube drivers)", env[accelerator.AMDEnvLedgerPath], ledger)
	}
}

// TestInstancePodPPUPartitionShape mirrors TestInstancePodAMDPartitionShape
// for ppuPartitionInjector.apply (internal/agent/engine/partition_ppu.go):
// two shim libraries as read-only hostPath volumes, the host's /dev/shm
// ledger file as a writable hostPath volume at the same path
// (ppuLedgerPath, unchanged from the non-kube env value), device plugin
// resources for the PPU device nodes, and no privileged security context.
func TestInstancePodPPUPartitionShape(t *testing.T) {
	quotaHost := "/var/lib/gpuworks/dgpu-runtime/ppu/linux-amd64/hggc-13.0/hggc_quota.so"
	hookHost := "/var/lib/gpuworks/dgpu-runtime/ppu/linux-amd64/hggc-13.0/hgml_dlsym_hook.so"
	ledger := "/dev/shm/gpuworks-ppu-ppu-1"
	spec := runtime.WorkloadSpec{
		Name: "gpuworks-ppu-1", Image: "img:hggc",
		DockerArgs: []string{"--device", "/dev/alixpu:/dev/alixpu:rwm", "--device", "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm"},
		Mounts: []runtime.Mount{
			{Source: quotaHost, Target: accelerator.PPUQuotaLibraryTarget, ReadOnly: true},
			{Source: hookHost, Target: accelerator.PPUHookLibraryTarget, ReadOnly: true},
			{Source: ledger, Target: ledger},
		},
		Env: map[string]string{
			accelerator.EnvLDPreload:                  accelerator.PPUHookLibraryTarget + ":" + accelerator.PPUQuotaLibraryTarget,
			accelerator.PPUEnvMemoryLimitPrefix + "0": "8192",
			accelerator.PPUEnvSMLimitPrefix + "0":     "50",
			accelerator.PPUEnvLedgerPath:              ledger,
		},
	}
	reqs := map[string]string{"gpuworks.ai/accelerator-0": "1", "gpuworks.ai/accelerator-shared": "1"}
	pod, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", DeviceRequests: reqs})
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext != nil {
		t.Fatalf("PPU partitioned instance must not be privileged: %+v", c.SecurityContext)
	}
	if r := c.Resources; r.Limits["gpuworks.ai/accelerator-0"] != "1" || r.Requests["gpuworks.ai/accelerator-shared"] != "1" {
		t.Fatalf("device plugin resources = %+v", r)
	}
	quotaVol, quotaMount := volumeByMountPath(pod, accelerator.PPUQuotaLibraryTarget)
	hookVol, hookMount := volumeByMountPath(pod, accelerator.PPUHookLibraryTarget)
	if quotaVol == nil || quotaVol.HostPath == nil || quotaVol.HostPath.Path != quotaHost || !quotaMount.ReadOnly {
		t.Fatalf("hggc_quota.so volume = %+v, mount = %+v", quotaVol, quotaMount)
	}
	if hookVol == nil || hookVol.HostPath == nil || hookVol.HostPath.Path != hookHost || !hookMount.ReadOnly {
		t.Fatalf("hgml_dlsym_hook.so volume = %+v, mount = %+v", hookVol, hookMount)
	}
	ledgerVol, ledgerMount := volumeByMountPath(pod, ledger)
	if ledgerVol == nil || ledgerVol.HostPath == nil || ledgerVol.HostPath.Path != ledger {
		t.Fatalf("ledger volume = %+v, want hostPath %s", ledgerVol, ledger)
	}
	if ledgerMount.ReadOnly {
		t.Fatalf("ledger mount must be writable: %+v", ledgerMount)
	}
	env := envMap(c.Env)
	if !strings.Contains(env[accelerator.EnvLDPreload], accelerator.PPUQuotaLibraryTarget) {
		t.Fatalf("LD_PRELOAD = %q", env[accelerator.EnvLDPreload])
	}
	if env[accelerator.PPUEnvLedgerPath] != ledger {
		t.Fatalf("ledger env = %q, want %q (same path as the non-kube drivers)", env[accelerator.PPUEnvLedgerPath], ledger)
	}
}
