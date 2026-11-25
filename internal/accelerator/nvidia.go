package accelerator

type nvidiaLogicalBackend struct{}

func (nvidiaLogicalBackend) ID() string { return BackendNVIDIALogical }
func (nvidiaLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorNVIDIA
}
func (nvidiaLogicalBackend) Maturity() Maturity       { return MaturityStable }
func (nvidiaLogicalBackend) RequiredAssets() []string { return []string{"libvgpu.so"} }
func (nvidiaLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorNVIDIA, DisplayName: "NVIDIA dGPU", Compatibility: "NVIDIA CUDA GPU"}
}
func (nvidiaLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The embedded libvgpu asset is validated at extraction time, so NVIDIA
// skips the behavioral probe.
func (nvidiaLogicalBackend) SkipsBehaviorProbe() bool { return true }

// HAMi-core reads the limit from the per-instance shared cache on every
// allocation check and memory query; the agent rewrites it there.
func (nvidiaLogicalBackend) LiveMemoryLimit() bool { return true }

// HAMi-core's shared cache carries each process's memory and SM utilization.
func (nvidiaLogicalBackend) SliceUsage(int) (memory, compute bool) { return true, true }

func (nvidiaLogicalBackend) Profiles() []PartitionProfile { return nil }

// nvidiaVendor is NVIDIA. PCI identity measured 2026-10-15 on gpu3: RTX 2080
// Ti 10de:1e04 class 0x0300 and Quadro RTX 8000 10de:1e78 class 0x0302, both
// bound to nvidia.
var nvidiaVendor = vendorSpec{
	id:           VendorNVIDIA,
	brand:        "NVIDIA",
	frameworks:   []string{"cuda"},
	named:        contains("nvidia", "cuda"),
	precedence:   20,
	namePrefixes: []string{"nvidia"},

	tool:           "nvidia-smi",
	detectCommand:  "nvidia-smi",
	controlDevices: []string{"/dev/nvidiactl"},
	pciID:          0x10de,
	pci:            &pciVendor{classes: []uint32{pciClassVGA, pciClass3D}, drivers: []string{"nvidia"}},
	arch:           nvidiaArch,

	runtimes:    []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube},
	supports:    Features,
	partition:   nvidiaLogicalBackend{},
	gpuPlugins:  map[string]string{RuntimeDocker: PluginNVIDIAContainerToolkit, RuntimeNerdctl: PluginNVIDIAContainerToolkit},
	gpuFallback: true,

	cardChecks:       true,
	checks:           []string{CheckXid, CheckNVLink, CheckRuntime, CheckPeerMemory},
	peerMemoryModule: "nvidia_peermem",
	healthProbe: func(target, _ string) string {
		if target == "" {
			return ""
		}
		return "nvidia-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target
	},
	runtimeGuide: func(card HealthCard) (HealthGuide, bool) {
		if card.UUID == "" {
			// Without the UUID the command could only name an index, which
			// addresses a different card once the driver drops one — the
			// mistake the check itself avoids by using UUIDs.
			return HealthGuide{}, false
		}
		guide := HealthGuide{Probe: CUDARuntimeSmokeCommand(card.UUID)}
		if target := HealthTarget(card.BDF, card.Index); target != "" {
			guide.Recheck = "nvidia-smi -q -i " + target
		}
		return guide, true
	},
	healthGuide: func(card HealthCard, target string, evidence []HealthField) (HealthGuide, bool) {
		return nvidiaHealthGuide(card.Code, target, evidence)
	},
}
