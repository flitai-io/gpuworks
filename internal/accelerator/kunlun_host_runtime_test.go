package accelerator

import "testing"

func TestKunlunCapabilitiesAndHostRequirements(t *testing.T) {
	for _, feature := range []Feature{FeatureEngineSleep, FeaturePD} {
		if Supports("Kunlunxin", feature) || !Supports(VendorNVIDIA, feature) {
			t.Fatal(feature)
		}
	}
	for _, feature := range []Feature{FeaturePipelineParallel, FeatureDataParallel} {
		if !Supports(VendorKunlunxin, feature) || !Supports(VendorNVIDIA, feature) || !Supports(VendorAscend, feature) {
			t.Fatalf("whole-card parallel mode unexpectedly blocked: %s", feature)
		}
	}
	if !Supports(VendorKunlunxin, FeatureMultiNode) || !RequiresDeviceSelection(VendorKunlunxin) {
		t.Fatal("whole-card multi-node contract lost")
	}
	for _, host := range []HostRuntime{{OS: "linux", Arch: "amd64", Driver: "docker"}, {OS: "Linux", Arch: "x86_64", Driver: "Docker"}, {OS: "linux", Arch: "amd64", Driver: "nerdctl"}, {OS: "linux", Arch: "amd64", Driver: "kube"}} {
		if params := RuntimeMismatchParams(VendorKunlunxin, host); params != nil {
			t.Fatal(params)
		}
	}
	for _, host := range []HostRuntime{{OS: "linux", Arch: "arm64", Driver: "docker"}, {OS: "windows", Arch: "amd64", Driver: "docker"}, {OS: "linux", Arch: "amd64", Driver: "udocker"}, {}} {
		params := RuntimeMismatchParams(VendorKunlunxin, host)
		if params == nil || params["required_os"] != "linux" || params["required_arch"] != "amd64" || params["required_runtime"] != "docker/nerdctl/kube" || params["os"] != host.OS || params["arch"] != host.Arch || params["runtime"] != host.Driver {
			t.Fatal(params)
		}
		if RuntimeMismatchParams(VendorNVIDIA, host) != nil {
			t.Fatal("Kunlun host requirements leaked to NVIDIA")
		}
	}
	if !IsDomestic(VendorKunlunxin) || !RequiresVendorCatalogImage(VendorKunlunxin) {
		t.Fatal("catalog domestic mismatch")
	}
}

func TestKunlunIdentityDoesNotUseIncidentalModelSubstrings(t *testing.T) {
	for _, name := range []string{"NVIDIA P8000", "third-party kl3-compatible board"} {
		if InferVendor("", name, "", "") == VendorKunlunxin {
			t.Fatal(name)
		}
	}
	if InferVendor("", "P800 PCIe", "KL3", "10.2") != VendorKunlunxin {
		t.Fatal("native architecture lost to CUDA facade")
	}
	// Both with the vendor's module bound and without it: see
	// TestACardIsFoundByItsClassWithItsVendorsDriverUnbound.
	for _, driver := range []string{"kunlun", ""} {
		if VendorForPCI(0x2057, 0x3686, 0x078000, driver) != VendorKunlunxin {
			t.Fatalf("captured PCI identity missing on driver %q", driver)
		}
	}
}

func TestKunlunIntegrationAppleDeniesFutureFeatures(t *testing.T) {
	for _, f := range append(append([]Feature(nil), Features...), Feature("future_feature")) {
		if Supports(VendorApple, f) {
			t.Fatalf("Apple unexpectedly supports %q", f)
		}
	}
}

func TestKunlunRuntimeFactsMissingVersusUnsupported(t *testing.T) {
	for _, tc := range []struct {
		host    HostRuntime
		missing bool
	}{
		{HostRuntime{}, true},
		{HostRuntime{OS: "linux", Arch: "amd64"}, true},
		{HostRuntime{OS: "linux", Arch: "amd64", Driver: "udocker"}, false},
		{HostRuntime{OS: "windows"}, false},
	} {
		p := RuntimeMismatchParams(VendorKunlunxin, tc.host)
		if p == nil || p["missing_facts"] != tc.missing {
			t.Fatalf("%+v: %+v", tc.host, p)
		}
	}
}
