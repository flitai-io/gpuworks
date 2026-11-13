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

// A partial restriction (no vendor carries one today) refuses exactly its
// features and requires a known launch command.
func TestPartialRestrictionRefusesOnlyItsFeatures(t *testing.T) {
	vendor := VendorHygon
	saved, had := targetConstraints[vendor]
	targetConstraints[vendor] = targetConstraint{unsupported: []Feature{FeatureDataParallel}}
	t.Cleanup(func() {
		if had {
			targetConstraints[vendor] = saved
		} else {
			delete(targetConstraints, vendor)
		}
	})
	if missing := Unsupported(vendor, FeatureDataParallel, FeatureEngineSleep); !slices.Equal(missing, []Feature{FeatureDataParallel}) {
		t.Fatalf("partial restriction lacks %v, want data parallelism only", missing)
	}
	if !RequiresKnownLaunchFeatures(vendor) {
		t.Fatal("a partial restriction must require a known launch")
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
