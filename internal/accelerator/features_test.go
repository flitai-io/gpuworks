package accelerator

import (
	"slices"
	"testing"
)

func TestFeatureSupportByVendor(t *testing.T) {
	all := []Feature{FeatureEngineSleep, FeatureMultiNode, FeaturePD, FeatureKVDiskTier}
	for _, vendor := range []string{VendorNVIDIA, VendorAscend, VendorPPU, VendorHygon, ""} {
		if missing := Unsupported(vendor, all...); len(missing) != 0 {
			t.Errorf("%q lacks %v", vendor, missing)
		}
	}
	if missing := Unsupported(VendorAMD, all...); len(missing) != 0 {
		t.Fatalf("AMD lacks %v, want none of %v", missing, all)
	}
	if missing := Unsupported(VendorAMD, FeatureDataParallel, FeaturePipelineParallel); len(missing) != 0 {
		t.Fatalf("AMD lacks %v, want none", missing)
	}
	if RequiresKnownLaunchFeatures(VendorAMD) {
		t.Fatal("AMD has no partial restriction left, so an unknown launch needs no proof")
	}
	if missing := Unsupported("Apple", all...); !slices.Equal(missing, all) {
		t.Fatalf("apple lacks %v, want all of %v", missing, all)
	}
	if missing := Unsupported(VendorApple); missing != nil {
		t.Fatalf("asking for nothing = %v", missing)
	}
}

// A vendor that lists some features but not all refuses exactly the ones it
// leaves out and requires a known launch command.
func TestPartialSupportRefusesOnlyWhatIsNotListed(t *testing.T) {
	index := -1
	for i, spec := range vendorSpecs {
		if spec.id == VendorHygon {
			index = i
		}
	}
	saved := vendorSpecs[index].supports
	t.Cleanup(func() { vendorSpecs[index].supports = saved })
	var listed []Feature
	for _, feature := range Features {
		if feature != FeatureDataParallel {
			listed = append(listed, feature)
		}
	}
	vendorSpecs[index].supports = listed
	if missing := Unsupported(VendorHygon, FeatureDataParallel, FeatureEngineSleep); !slices.Equal(missing, []Feature{FeatureDataParallel}) {
		t.Fatalf("partial support lacks %v, want data parallelism only", missing)
	}
	if !RequiresKnownLaunchFeatures(VendorHygon) {
		t.Fatal("partial support must require a known launch")
	}
	// Listing nothing is not a partial list: every feature is refused, and
	// plain inference with a command that cannot be read still runs.
	vendorSpecs[index].supports = nil
	if missing := Unsupported(VendorHygon, Features...); len(missing) != len(Features) {
		t.Fatalf("a vendor that lists nothing lacks %v, want every feature", missing)
	}
	if RequiresKnownLaunchFeatures(VendorHygon) {
		t.Fatal("a vendor that supports no feature refused plain inference with an unknown command")
	}
}

func TestRequiresCollectiveInterface(t *testing.T) {
	for _, tc := range []struct {
		vendor  string
		nodes   int
		devices int
		want    bool
	}{
		{vendor: VendorKunlunxin, nodes: 1, devices: 1},
		{vendor: VendorKunlunxin, nodes: 1, devices: 2, want: true},
		{vendor: VendorKunlunxin, nodes: 2, devices: 1, want: true},
		{vendor: VendorNVIDIA, nodes: 2, devices: 2},
	} {
		if got := RequiresCollectiveInterface(tc.vendor, tc.nodes, tc.devices); got != tc.want {
			t.Fatalf("%s nodes=%d devices=%d: got %t, want %t", tc.vendor, tc.nodes, tc.devices, got, tc.want)
		}
	}
}

// Only Hygon was measured keeping host memory behind a sleepable engine's GPU
// memory; a new vendor must be added on evidence, not by default.
func TestSleepPoolOnHostMemoryIsHygonOnly(t *testing.T) {
	for _, vendor := range KnownVendors() {
		if got, want := SleepPoolOnHostMemory(vendor), vendor == VendorHygon; got != want {
			t.Errorf("SleepPoolOnHostMemory(%q) = %v, want %v", vendor, got, want)
		}
	}
	if !SleepPoolOnHostMemory(" Hygon ") || SleepPoolOnHostMemory("") {
		t.Fatal("vendor spelling is normalized and an unknown vendor backs nothing")
	}
}
