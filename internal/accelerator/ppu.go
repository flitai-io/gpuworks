package accelerator

type ppuLogicalBackend struct{}

func (ppuLogicalBackend) ID() string { return BackendPPULogical }
func (ppuLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorPPU
}
func (ppuLogicalBackend) Maturity() Maturity { return MaturityExperimental }
func (ppuLogicalBackend) RequiredAssets() []string {
	return []string{"hggc_quota.so", "hgml_dlsym_hook.so"}
}
func (ppuLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorPPU, DisplayName: "PPU dGPU", Compatibility: "HGGC 13.x × x86_64 · Docker / Kubernetes"}
}
func (ppuLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The PPU ledger path has only been exercised under Docker and kube; other
// runtimes stay unavailable until verified. Under Docker the cross-process
// ledger is backed by a file in the instance's partition state directory.
// A Kubernetes agent has a private /dev/shm, so it pre-creates the ledger
// on the host for the workload pod to mount (see partition_ppu.go).
func (ppuLogicalBackend) SupportedRuntimes() []string { return []string{"docker", "kube"} }

// The shim's usage region carries what the slice is charged on each card. Its
// compute controller measures utilization only on a card whose compute is
// capped below the whole card, so an uncapped slice has no compute figure.
func (ppuLogicalBackend) SliceUsage(smLimitPct int) (memory, compute bool) {
	return true, smLimitPct > 0 && smLimitPct < 100
}

var ppuProfiles = []PartitionProfile{
	{Backend: BackendPPULogical, Vendor: VendorPPU, Framework: "hggc", VersionPrefix: "13", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "HGGC 13.x × x86_64", Verified: true, ChipFamilies: []string{"ZW810E", "Beethoven"}},
}

func (ppuLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ppuProfiles...)
}

// ppuVendor is T-Head PPU. PCI identity measured 2026-10-15: ZW810E
// 1ded:6001 class 0x0302 bound to alipci (192.0.2.210).
var ppuVendor = vendorSpec{
	id:         VendorPPU,
	brand:      "T-Head",
	frameworks: []string{"hggc"},
	named:      either(contains("ppu", "thead"), exactly("t-head", "alibaba", "beethoven")),
	precedence: 60,
	domestic:   true,

	tool:           "ppu-smi",
	detectCommand:  "ppu-smi",
	controlDevices: []string{"/dev/alixpu_ctl"},
	pciID:          0x1ded,
	pci: &pciVendor{
		classes:     []uint32{pciClass3D},
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"alipci"},
	},
	pciModels: []PCIModel{
		{Device: 0x6001, Name: "PPU-ZW810E", Provenance: "ppu-smi on 192.0.2.210, 2026-10-15: all twelve cards it serves"},
	},
	arch: func(archID, _ string) Arch { return Arch{ID: archID, Family: ppuFamily(archID)} },

	runtimes:        []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube},
	supports:        Features,
	partition:       ppuLogicalBackend{},
	assetBundles:    true,
	ownCatalogImage: true,

	cardChecks:       true,
	checks:           []string{CheckRuntime, CheckPeerMemory},
	peerMemoryModule: "alixpu_peermem",
	smokeFramework:   "hggc",
	smokeScript:      CUDAGradedSmokeScript,
	healthProbe: func(target, _ string) string {
		if target == "" {
			return ""
		}
		return "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target
	},
	healthGuide: func(card HealthCard, target string, evidence []HealthField) (HealthGuide, bool) {
		return ppuHealthGuide(card.Code, target, evidence)
	},
}
