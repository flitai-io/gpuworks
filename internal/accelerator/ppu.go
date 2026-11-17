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
// runtimes stay unavailable until verified. The cross-process ledger stays
// a /dev/shm path on both; on Kubernetes alone the agent's own /dev/shm is
// its pod's private tmpfs rather than the host's, so the agent pre-creates
// the ledger on the host (through its hostRoot) and the instance pod
// reaches it through an explicit hostPath mount at that same path (see
// partition_ppu.go).
func (ppuLogicalBackend) SupportedRuntimes() []string { return []string{"docker", "kube"} }

var ppuProfiles = []PartitionProfile{
	{Backend: BackendPPULogical, Vendor: VendorPPU, Framework: "hggc", VersionPrefix: "13", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "HGGC 13.x × x86_64", Verified: true, ChipFamilies: []string{"ZW810E", "Beethoven"}},
}

func (ppuLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ppuProfiles...)
}
