package accelerator

import "strconv"

type ascendLogicalBackend struct{}

func (ascendLogicalBackend) ID() string { return BackendAscendLogical }
func (ascendLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorAscend
}
func (ascendLogicalBackend) Maturity() Maturity       { return MaturityExperimental }
func (ascendLogicalBackend) RequiredAssets() []string { return []string{"libvruntime.so"} }
func (ascendLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorAscend, DisplayName: "Ascend dGPU", Compatibility: "Host versions are advisory only; the container needs glibc ≥ 2.33"}
}
func (ascendLogicalBackend) NeedsInstanceTeardown() bool { return true }

// Ascend partition libraries require a newer glibc than driver-only host
// images typically ship, so the requirement is surfaced as a capability note.
func (ascendLogicalBackend) HostRequirementNotes() []string {
	return []string{"The inference image must provide glibc 2.33 or newer"}
}

// vcann-rt's memory ledger carries what the slice is charged; it publishes no
// compute figure.
func (ascendLogicalBackend) SliceUsage(int) (memory, compute bool) { return true, false }

var ascendProfiles = []PartitionProfile{
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "8.5", Architectures: []string{"x86_64"}, DisplayCompatibility: "CANN 8.5.x × x86_64", Verified: true, ChipFamilies: []string{"910B2", "910B2C"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "8.5", Architectures: []string{"arm64"}, DisplayCompatibility: "CANN 8.5.x × ARM64", ChipFamilies: []string{"910B4"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "9.0", Architectures: []string{"arm64"}, DisplayCompatibility: "CANN 9.0.x × ARM64", Verified: true, ChipFamilies: []string{"910B1", "910B4"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "9.0", Architectures: []string{"x86_64"}, DisplayCompatibility: "CANN 9.0.x × x86_64", ChipFamilies: []string{"910B"}},
}

func (ascendLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ascendProfiles...)
}

// ascendVendor is Huawei Ascend. PCI identity measured 2026-10-15: 910B4
// 19e5:d802 class 0x1200 bound to devdrv_device_driver (198.51.100.31/.32).
//
// Its PCI classes deliberately leave out 0x03: 19e5:1711, the BMC display
// controller on every Huawei server, reports 0x0300 and would otherwise be
// listed as an NPU.
var ascendVendor = vendorSpec{
	id:           VendorAscend,
	brand:        "Ascend",
	frameworks:   []string{"cann"},
	named:        either(contains("ascend", "huawei"), exactly("npu")),
	precedence:   50,
	namePrefixes: []string{"huawei", "ascend"},
	domestic:     true,

	tool:           "npu-smi info",
	detectCommand:  "npu-smi",
	controlDevices: []string{"/dev/davinci_manager"},
	pciID:          0x19e5,
	pci: &pciVendor{
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"devdrv_device_driver"},
		// Kunpeng builds compression into the SoC: 19e5:a250, the ZIP
		// engine, reports 0x1200 like the NPU does and binds hisi_zip.
		// A 910C host presents eight of them (192.0.2.129, measured
		// 2026-10-25), and nothing can run inference on one.
		notAccelerators: []uint32{0xa250},
	},
	// Upstream pci.ids has never held a single Ascend NPU: of the 72 devices
	// under 19e5, not one is a d8xx. npu-smi says "Ascend950PR" and the
	// prober makes that "950PR", so "950PR" is what goes here.
	pciModels: []PCIModel{
		{Device: 0xd500, Name: "310P3", Provenance: "npu-smi 24.1.1.3 on 192.0.2.115, 2026-10-17: NPU 4 at bus-id 0000:17:00.0, reported as 310P3; lspci at that address reads 19e5:d500 with no name"},
		{Device: 0xd802, Name: "910B4", Provenance: "npu-smi on 198.51.100.32, 2026-10-15: NPU 0 and 6, bus-ids 0000:C1:00.0 and 0000:41:00.0"},
		{Device: 0xd803, Name: "910C", Provenance: "npu-smi 26.1.1 on syl-910c, 2026-10-15: twenty d803 at 81:00.0..9f:00.0, reported as multi-chip Ascend910, which is what this platform calls a 910C (ascend_chips.go)"},
		{Device: 0xd806, Name: "950PR", Provenance: "npu-smi 25.7.rc1.10, 2026-10-15: one card at bus-id 0000:71:00.0, 128 GiB HBM, reported as Ascend950PR; lspci at that same address reads 19e5:d806"},
	},
	arch:        func(archID, _ string) Arch { return Arch{ID: archID, Family: ascendFamily(archID)} },
	cardNetwork: &ascendCardNetwork,

	runtimes:        []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube},
	supports:        Features,
	partition:       ascendLogicalBackend{},
	gpuPlugins:      map[string]string{RuntimeDocker: PluginAscendDockerRuntime},
	assetBundles:    true,
	ownCatalogImage: true,

	cardChecks:     true,
	checks:         []string{CheckRuntime},
	smokeFramework: "cann",
	smokeScript:    ascendContainerPreamble + AscendACLSmokeScript,
	healthProbe:    func(_, _ string) string { return "npu-smi info" },
	runtimeGuide: func(card HealthCard) (HealthGuide, bool) {
		if card.Index < 0 {
			return HealthGuide{}, false
		}
		return HealthGuide{
			Probe: AscendRuntimeSmokeCommand(strconv.Itoa(card.Index)),
			// npu-smi addresses the chip, not the die index; the die's own
			// row is what `npu-smi info` prints for it.
			Recheck: "npu-smi info -m; npu-smi info",
		}, true
	},
	healthGuide: func(card HealthCard, _ string, evidence []HealthField) (HealthGuide, bool) {
		return ascendHealthGuide(card.Code, evidence)
	},
}
