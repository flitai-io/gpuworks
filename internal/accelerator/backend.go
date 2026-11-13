// Package accelerator defines the shared partition-backend registry used by
// both the control plane and the agent. The registry is deliberately explicit:
// adding a backend must be a visible, reviewable change here.
package accelerator

import "strings"

const (
	BackendNVIDIALogical = "nvidia-dgpu"
	BackendAscendLogical = "ascend-dgpu"
	BackendPPULogical    = "ppu-dgpu"
	BackendHygonLogical  = "hygon-dgpu"
	BackendAMDLogical    = "amd-dgpu"

	EnvLDPreload            = "LD_PRELOAD"
	EnvLDLibraryPath        = "LD_LIBRARY_PATH"
	EnvNVIDIAVisibleDevices = "NVIDIA_VISIBLE_DEVICES"
	EnvCUDAVisibleDevices   = "CUDA_VISIBLE_DEVICES"

	DGPULibraryTarget        = "/gpuworks/libvgpu.so"
	DGPUCacheTarget          = "/gpuworks/cache"
	DGPUEnvMemoryCache       = "CUDA_DEVICE_MEMORY_SHARED_CACHE"
	DGPUEnvMemoryLimitPrefix = "CUDA_DEVICE_MEMORY_LIMIT_"
	DGPUEnvSMLimit           = "CUDA_DEVICE_SM_LIMIT"

	AscendLibraryTarget       = "/opt/gpuworks/partition/libvruntime.so"
	AscendPreloadTarget       = "/etc/ld.so.preload"
	AscendConfigTarget        = "/etc/enpu/vcann-rt/npu_info.config"
	AscendEnvDSMIHook         = "ENPU_DSMI_HOOK"
	AscendEnvVisibleDevices   = "ASCEND_VISIBLE_DEVICES"
	AscendEnvRTVisibleDevices = "ASCEND_RT_VISIBLE_DEVICES"
	AscendEnvNPUVisible       = "NPU_VISIBLE_DEVICES"
	// AscendDriverLibraryPath is where the driver's own libraries live, in
	// the order the dynamic linker should search them. A CANN toolkit links
	// against them (libascend_hal and the DCMI pair), and an installation
	// whose ld.so.conf does not already name them resolves nothing without
	// this on LD_LIBRARY_PATH.
	AscendDriverLibraryPath = "/usr/local/Ascend/driver/lib64/common:/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/driver/lib64"

	PPUQuotaLibraryTarget   = "/opt/gpuworks/partition/hggc_quota.so"
	PPUHookLibraryTarget    = "/opt/gpuworks/partition/hgml_dlsym_hook.so"
	PPUEnvMemoryLimitPrefix = "HGGC_DEVICE_MEMORY_LIMIT_"
	PPUEnvSMLimitPrefix     = "HGGC_DEVICE_SM_LIMIT_"
	PPUEnvLedgerPath        = "HGGC_LEDGER_PATH"
	// PPUEnvDeviceUUIDs preserves selected physical identities in compute order,
	// independently of the numeric selectors consumed by inference engines.
	PPUEnvDeviceUUIDs = "GPUWORKS_PPU_DEVICE_UUIDS"

	// AMDLibraryTarget is libvrocm.so (the rocm-slicing-shim,
	// build/runtime-assets/amd/): one shared object for every ROCm version,
	// interposing the HIP runtime's allocating and capacity-query entry
	// points. It links only libc, so it is LD_PRELOAD-safe into any ROCm
	// inference image regardless of that image's own glibc.
	AMDLibraryTarget = "/opt/gpuworks/partition/libvrocm.so"
	// AMDEnvMemoryLimitPrefix (VROCM_DEVICE_MEMORY_LIMIT_<N>, MiB) names the
	// quota for the card at position N in the container's own visible-device
	// list. There is no compute/SM counterpart: ROCm enforces compute quotas
	// through HSA_CU_MASK, read by ROCr below anything a preloaded library in
	// the workload process can see or verify, so this backend is memory-only.
	AMDEnvMemoryLimitPrefix = "VROCM_DEVICE_MEMORY_LIMIT_"
	// AMDEnvLedgerPath (VROCM_LEDGER_PATH) is the cross-process usage region
	// libvrocm.so charges allocations against, analogous to PPUEnvLedgerPath.
	AMDEnvLedgerPath = "VROCM_LEDGER_PATH"
	// AMDEnvLogLevel (LIBVROCM_LOG_LEVEL): 0 quiet, 1 denials, 2 debug.
	AMDEnvLogLevel = "LIBVROCM_LOG_LEVEL"
)

type Maturity string

const (
	MaturityStable       Maturity = "stable"
	MaturityExperimental Maturity = "experimental"
)

// Metadata contains presentation data; it must not be used for admission.
type Metadata struct {
	Vendor        string
	DisplayName   string
	Compatibility string
}

// Backend is the definition of a partition backend. The agent-side container
// contract (mounts, env) lives in the engine's per-backend injectors — it is
// executable and asserted by tests, so it is not re-declared here. Admission is
// intentionally absent: all current backends share the scheduler's existing
// available-memory rule. Optional capabilities (runtime constraints, probe
// exemption, host requirement notes) are separate interfaces in
// capabilities.go.
type Backend interface {
	ID() string
	Matches(vendor, arch string) bool
	Maturity() Maturity
	RequiredAssets() []string
	Metadata() Metadata
	NeedsInstanceTeardown() bool
	// Profiles returns the backend's build-combination table for advisory
	// compatibility warnings; nil when the backend is built for none.
	Profiles() []PartitionProfile
}

var registry = []Backend{
	nvidiaLogicalBackend{},
	ascendLogicalBackend{},
	ppuLogicalBackend{},
	hygonLogicalBackend{},
	amdLogicalBackend{},
}

func All() []Backend { return append([]Backend(nil), registry...) }

func Lookup(id string) (Backend, bool) {
	id = strings.TrimSpace(id)
	for _, backend := range registry {
		if backend.ID() == id {
			return backend, true
		}
	}
	return nil, false
}
