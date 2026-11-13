package hardwarecatalog

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestCatalogUsesTwoLevelNVIDIAClassification(t *testing.T) {
	want := map[string]struct {
		cc       float64
		category string
		arch     string
	}{
		"NVIDIA B300":         {10.3, CategoryDataCenter, "Blackwell"},
		"NVIDIA H100":         {9.0, CategoryDataCenter, "Hopper"},
		"GeForce RTX 4090":    {8.9, CategoryWorkstation, "Ada"},
		"GeForce RTX 2080 Ti": {7.5, CategoryWorkstation, "Turing"},
		"NVIDIA V100":         {7.0, CategoryDataCenter, "Volta"},
	}
	for _, gpu := range List() {
		expected, exists := want[gpu.Name]
		if !exists {
			continue
		}
		if gpu.Vendor != "nvidia" || gpu.CC != expected.cc || gpu.Category != expected.category || gpu.Architecture != expected.arch {
			t.Fatalf("%s = %+v, want %+v", gpu.Name, gpu, expected)
		}
		delete(want, gpu.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing NVIDIA entries: %+v", want)
	}
}

func TestCatalogIncludesLockedDomesticChoices(t *testing.T) {
	want := map[string]string{
		"Ascend 310P": "ascend", "Ascend 910B": "ascend", "Ascend 910C": "ascend", "Ascend 950PR": "ascend", "Ascend 950DT": "ascend",
		"Hygon K100": "hygon", "Hygon K500": "hygon", "Hygon BW1000": "hygon", "Hygon BW1100": "hygon",
		"PPU ZW810E": "ppu", "PPU M890": "ppu",
	}
	for _, gpu := range List() {
		vendor, exists := want[gpu.Name]
		if !exists {
			continue
		}
		if gpu.Vendor != vendor || gpu.Category != CategoryDomestic || gpu.CC != 0 {
			t.Fatalf("%s = %+v", gpu.Name, gpu)
		}
		delete(want, gpu.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing domestic entries: %+v", want)
	}
}

func TestListReturnsCopy(t *testing.T) {
	first := List()
	first[0].Name = "mutated"
	if List()[0].Name == "mutated" {
		t.Fatal("catalog was mutated through List")
	}
}

// The static catalog is only useful for hypothetical planning if the vendor it
// names is the same word a real node reports for the same card.
func TestCatalogVendorsAreKnownAcceleratorVendors(t *testing.T) {
	known := make(map[string]struct{}, len(accelerator.KnownVendors()))
	for _, vendor := range accelerator.KnownVendors() {
		known[vendor] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, gpu := range List() {
		if _, ok := known[gpu.Vendor]; !ok {
			t.Errorf("catalog GPU %q has vendor %q, which is not in accelerator.KnownVendors() %v", gpu.Name, gpu.Vendor, accelerator.KnownVendors())
		}
		if got := accelerator.NormalizeVendor(gpu.Vendor); got != gpu.Vendor {
			t.Errorf("accelerator.NormalizeVendor(%q) = %q; a catalog vendor must be canonical", gpu.Vendor, got)
		}
		seen[gpu.Vendor] = struct{}{}
	}
	if len(seen) == 0 {
		t.Fatal("catalog is empty; the subset assertion would be vacuous")
	}
}

func TestCatalogIncludesAMDWithoutNVIDIAComputeCapability(t *testing.T) {
	want := map[string]string{"AMD Instinct MI300X": CategoryDataCenter, "AMD Radeon 8060S (AMD395)": CategoryWorkstation}
	for _, gpu := range List() {
		category, ok := want[gpu.Name]
		if !ok {
			continue
		}
		if gpu.Vendor != accelerator.VendorAMD || gpu.CC != 0 || gpu.Category != category || gpu.Architecture == "" {
			t.Fatalf("AMD choice = %+v", gpu)
		}
		delete(want, gpu.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing AMD choices: %v", want)
	}
}

func TestKunlunCatalogArchitecture(t *testing.T) {
	for _, gpu := range List() {
		if gpu.Name == "Kunlunxin P800 PCIe" {
			if gpu.Architecture != "KL3" {
				t.Fatalf("architecture = %q", gpu.Architecture)
			}
			return
		}
	}
	t.Fatal("Kunlunxin P800 PCIe missing")
}
