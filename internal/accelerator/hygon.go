package accelerator

import "strings"

type hygonLogicalBackend struct{}

func (hygonLogicalBackend) ID() string { return BackendHygonLogical }

// Only explicitly supported models may run the per-card quota probe. Unknown
// Hygon models remain whole-card devices.
func (hygonLogicalBackend) Matches(vendor, arch string) bool {
	if NormalizeVendor(vendor) != VendorHygon {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(arch)) {
	case "Z100", "K100AI", "K100_AI", "K100 AI", "BW":
		return true
	default:
		return false
	}
}
func (hygonLogicalBackend) Maturity() Maturity { return MaturityExperimental }
func (hygonLogicalBackend) RequiredAssets() []string {
	return []string{"libamdhip64.so", "libhydmi.so", "libhydmi_mig.so", "librocm_smi64.so", "gpuworks-hygon-vdev-helper"}
}
func (hygonLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorHygon, DisplayName: "Hygon dGPU", Compatibility: "Z100 / K100AI / BW × x86_64 · Docker / udocker · memory-proportional CUs (experimental)"}
}
func (hygonLogicalBackend) NeedsInstanceTeardown() bool { return true }

// Native configuration is injected through structured mounts shared by Docker
// and udocker/runveil. Other runtime paths require their own validation.
// SupportedRuntimes: Hygon dGPU needs Docker or udocker (native vdev is
// unverified on Kubernetes).
func (hygonLogicalBackend) SupportedRuntimes() []string { return []string{"docker", "udocker"} }

var hygonProfiles = []PartitionProfile{
	{Backend: BackendHygonLogical, Vendor: VendorHygon, Framework: "dtk", VersionPrefix: "26.04", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "Z100 / K100AI / BW × DTK 26.04.x × x86_64", ChipFamilies: []string{"Z100", "K100AI", "BW"}},
}

func (hygonLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), hygonProfiles...)
}

// hygonVendor is Hygon DCU. PCI identity measured 2026-10-15: K100
// "KONGMING" 1d94:6211 class 0x0b40 (192.0.2.133).
var hygonVendor = vendorSpec{
	id:           VendorHygon,
	brand:        "Hygon",
	frameworks:   []string{"dtk"},
	named:        contains("hygon", "dcu", "haiguang"),
	precedence:   40,
	namePrefixes: []string{"hygon"},

	tool:          "hy-smi",
	detectCommand: "hy-smi",
	pciID:         0x1d94,
	pci: &pciVendor{
		classes:     []uint32{pciClassCoProcessor},
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"hydcu", "hycu"},
	},
	arch: func(archID, _ string) Arch { return Arch{ID: archID, Family: hygonFamily(archID)} },

	runtimes: []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube},
	supports: Features,
	// Only some models can be divided; see hygonLogicalBackend.Matches.
	partition:             hygonLogicalBackend{},
	ownCatalogImage:       true,
	sleepPoolOnHostMemory: true,

	cardChecks: true,
	checks:     []string{CheckRuntime},
	healthProbe: func(_, bdf string) string {
		if bdf == "" {
			return ""
		}
		ras := "/sys/bus/pci/devices/" + bdf + "/ras/"
		return "grep . " + ras + "*_err_count; cat " + ras + "gpu_vram_bad_pages"
	},
	runtimeGuide: func(card HealthCard) (HealthGuide, bool) {
		if strings.TrimSpace(card.BDF) == "" {
			return HealthGuide{}, false
		}
		return HealthGuide{Probe: HygonRuntimeSmokeCommand(card.BDF), Recheck: "hy-smi --healthcheck"}, true
	},
	healthGuide: func(card HealthCard, _ string, _ []HealthField) (HealthGuide, bool) {
		return hygonHealthGuide(card.Code, card.BDF)
	},
}
