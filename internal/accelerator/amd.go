package accelerator

import "strings"

// amdLogicalBackend is the AMD logical dGPU backend: a per-card VRAM quota
// enforced in user space by libvrocm.so (the rocm-slicing
// shim), interposed at the HIP runtime layer. ROCm enforces compute (CU)
// quotas in hardware through HSA_CU_MASK, read by ROCr below anything a
// preloaded library in the workload process can see or verify, so this
// backend is memory-only and never exposes a compute/SM limit (see
// docs/explanation/dgpu.md).
//
// Unlike PPU/Ascend/Hygon, whose whole-card containers already carry a host
// driver library the behavioral probe can reuse, AMD's whole-card injection
// (internal/agent/gpu/amd_runtime.go) passes through only /dev/kfd and the
// selected render node — no host ROCm user-space library is required or
// mounted. The lightweight embedded probe image has none either, so there is
// no HIP runtime available to run a behavioral check against without adding
// a new host prerequisite. This backend is therefore probe-exempt, like
// NVIDIA's HAMi-core: readiness rests on the embedded asset's checksum and
// the Docker/nerdctl runtime constraint below, not on a per-GPU probe.
type amdLogicalBackend struct{}

func (amdLogicalBackend) ID() string { return BackendAMDLogical }
func (amdLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorAMD
}
func (amdLogicalBackend) Maturity() Maturity       { return MaturityExperimental }
func (amdLogicalBackend) RequiredAssets() []string { return []string{"libvrocm.so"} }
func (amdLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorAMD, DisplayName: "AMD dGPU", Compatibility: "AMD ROCm GPU × x86_64 · Docker / nerdctl / runveil · memory quota only"}
}
func (amdLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The embedded libvrocm asset is validated at extraction time (sha256 in
// build/runtime-assets/manifest.json), so AMD skips the behavioral probe.
func (amdLogicalBackend) SkipsBehaviorProbe() bool { return true }

// AMD's host runtime contract (internal/accelerator/features.go) admits
// Docker, nerdctl and runveil (udocker); libvrocm.so is an LD_PRELOAD
// interposer that PRoot carries like any other bind-mounted library.
func (amdLogicalBackend) RuntimeConstraint(driver string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "docker", "nerdctl", "udocker":
		return ""
	default:
		return "AMD dGPU currently requires the Docker, nerdctl or udocker runtime"
	}
}

// amdProfiles currently names the one combination proven on real hardware
// (MI300X, gfx942, ROCm 7.2.3). Other AMD chips may well work identically —
// libvrocm.so links no ROCm object and is built once for every ROCm version
// — but are not yet verified.
var amdProfiles = []PartitionProfile{
	{Backend: BackendAMDLogical, Vendor: VendorAMD, Framework: "rocm", VersionPrefix: "7.2", Architectures: []string{"x86_64"}, DisplayCompatibility: "ROCm 7.2.x × gfx942 × x86_64", Verified: true, ChipFamilies: []string{"gfx942"}},
}

func (amdLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), amdProfiles...)
}
