package accelerator

import (
	"strings"
	"testing"
)

// What must hold of any vendor's definition, whichever vendor it is. A
// definition that breaks one of these fails by name.
func TestVendorDefinitionsAreConsistent(t *testing.T) {
	ids, precedences, frameworks, pciIDs, tools := map[string]bool{}, map[int]string{}, map[string]string{}, map[uint32]string{}, map[string]string{}
	for _, spec := range vendorSpecs {
		if spec.id == "" || spec.id != strings.ToLower(strings.TrimSpace(spec.id)) || ids[spec.id] {
			t.Fatalf("vendor id %q is empty, not canonical or listed twice", spec.id)
		}
		ids[spec.id] = true
		if spec.brand == "" {
			t.Errorf("%s has no brand", spec.id)
		}
		if other, taken := precedences[spec.precedence]; taken || spec.precedence <= 0 {
			t.Errorf("%s has precedence %d, which is unset or also %s's", spec.id, spec.precedence, other)
		}
		precedences[spec.precedence] = spec.id
		if len(spec.frameworks) == 0 {
			t.Errorf("%s owns no framework; its images could not be attributed to it", spec.id)
		}
		for _, framework := range spec.frameworks {
			if other, taken := frameworks[framework]; taken {
				t.Errorf("framework %q belongs to both %s and %s", framework, other, spec.id)
			}
			frameworks[framework] = spec.id
		}
		if spec.named == nil || NormalizeVendor(strings.ToUpper(spec.id)) != spec.id {
			t.Errorf("%s is not named by its own id", spec.id)
		}

		if firstNotIn(spec.runtimes, Runtimes()) != "" || duplicate(spec.runtimes) != "" {
			t.Errorf("%s lists a runtime that is not one of the platform's (%q) or lists one twice (%q)", spec.id, firstNotIn(spec.runtimes, Runtimes()), duplicate(spec.runtimes))
		}
		for driver := range spec.gpuPlugins {
			if !spec.runsOn(driver) {
				t.Errorf("%s names a plugin for runtime %q, which it does not list", spec.id, driver)
			}
		}
		seen := map[Feature]bool{}
		for _, feature := range spec.supports {
			known := false
			for _, f := range Features {
				known = known || f == feature
			}
			if !known || seen[feature] {
				t.Errorf("%s lists feature %q, which is unknown or listed twice", spec.id, feature)
			}
			seen[feature] = true
		}
		if len(spec.runtimes) == 0 && (len(spec.supports) > 0 || spec.partition != nil) {
			t.Errorf("%s lists no runtime, so it carries no workload, yet declares features or a dGPU backend", spec.id)
		}
		if spec.partition != nil && spec.partition.Metadata().Vendor != spec.id {
			t.Errorf("%s declares the dGPU backend %s, which is %s's", spec.id, spec.partition.ID(), spec.partition.Metadata().Vendor)
		}
		if limited, ok := spec.partition.(RuntimeConstrainer); ok {
			if extra := firstNotIn(limited.SupportedRuntimes(), spec.runtimes); extra != "" {
				t.Errorf("%s's dGPU backend runs on %q, which the vendor does not list", spec.id, extra)
			}
		}

		if spec.detectCommand != "" {
			if other, taken := tools[spec.detectCommand]; taken {
				t.Errorf("%s and %s are both detected by %q", other, spec.id, spec.detectCommand)
			}
			tools[spec.detectCommand] = spec.id
		}
		if (spec.pci == nil) != (spec.pciID == 0) {
			t.Errorf("%s states a PCI vendor id without how its cards are recognized, or the reverse", spec.id)
		}
		if spec.pci != nil {
			if other, taken := pciIDs[spec.pciID]; taken {
				t.Errorf("%s and %s share PCI vendor id %#x", other, spec.id, spec.pciID)
			}
			pciIDs[spec.pciID] = spec.id
			if len(spec.pci.drivers) == 0 {
				t.Errorf("%s names no kernel driver for its cards", spec.id)
			}
		}

		if !spec.cardChecks && (len(spec.checks) > 0 || spec.healthProbe != nil || spec.runtimeGuide != nil) {
			t.Errorf("%s names checks but its cards are not checked", spec.id)
		}
		for _, check := range spec.checks {
			found := false
			for _, known := range cardCheckCatalog {
				found = found || known.ID == check
			}
			if !found {
				t.Errorf("%s names a check %q that is not in the catalog", spec.id, check)
			}
		}
		if (spec.smokeFramework == "") != (spec.smokeScript == "") {
			t.Errorf("%s has a container check framework without a script, or the reverse", spec.id)
		}
		if spec.smokeFramework != "" && frameworks[spec.smokeFramework] != spec.id {
			t.Errorf("%s runs its container check in images of framework %q, which is not its own", spec.id, spec.smokeFramework)
		}
	}
}

func firstNotIn(values, allowed []string) string {
	for _, v := range values {
		found := false
		for _, a := range allowed {
			found = found || a == v
		}
		if !found {
			return v
		}
	}
	return ""
}

func duplicate(values []string) string {
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return ""
}

// A node is admitted for a vendor's cards on the runtimes its definition
// lists and on no other, a test double's name included. A fact a
// node has not reported is told apart from one that is wrong, and an
// unreported runtime is refused only by a vendor that requires a particular
// host.
func TestRuntimeMismatchFollowsTheDeclaredRuntimes(t *testing.T) {
	linux := func(driver string) HostRuntime { return HostRuntime{OS: "linux", Arch: "amd64", Driver: driver} }
	mac := func(driver string) HostRuntime { return HostRuntime{OS: "darwin", Arch: "arm64", Driver: driver} }
	for _, tc := range []struct {
		vendor  string
		host    HostRuntime
		refused bool
		missing bool
	}{
		{VendorNVIDIA, linux("docker"), false, false},
		{VendorNVIDIA, linux(" Docker "), false, false},
		{VendorNVIDIA, linux("udocker"), false, false},
		{VendorNVIDIA, linux("host"), true, false},
		{VendorNVIDIA, linux("recording"), true, false},
		{VendorNVIDIA, linux(""), false, false},
		{VendorNVIDIA, HostRuntime{Driver: "docker"}, false, false},
		{VendorHygon, linux("kube"), false, false},
		{VendorApple, mac("host"), false, false},
		{VendorApple, mac("docker"), true, false},
		{VendorKunlunxin, linux("nerdctl"), false, false},
		{VendorKunlunxin, linux("udocker"), true, false},
		{VendorKunlunxin, linux(""), true, true},
		{VendorKunlunxin, HostRuntime{Driver: "docker"}, true, true},
		{VendorKunlunxin, HostRuntime{OS: "linux", Driver: "udocker"}, true, false},
		{VendorAMD, linux("udocker"), false, false},
		{VendorAMD, HostRuntime{OS: "linux", Arch: "arm64", Driver: "docker"}, true, false},
		{"intel", linux("host"), false, false},
	} {
		mismatch := RuntimeMismatchFor(tc.vendor, tc.host)
		if (mismatch != nil) != tc.refused {
			t.Errorf("%s on %+v: refused = %v, want %v", tc.vendor, tc.host, mismatch != nil, tc.refused)
			continue
		}
		if mismatch != nil && mismatch.MissingFacts != tc.missing {
			t.Errorf("%s on %+v: missing facts = %v, want %v", tc.vendor, tc.host, mismatch.MissingFacts, tc.missing)
		}
	}
	// A vendor that lists no runtime carries no workload, on a node that
	// has reported its runtime or not.
	monitored := vendorSpec{id: "monitored"}
	for _, host := range []HostRuntime{linux("docker"), linux(""), {}} {
		mismatch := monitored.runtimeMismatch(monitored.id, host)
		if mismatch == nil || mismatch.MissingFacts || mismatch.Requirement() != "none" {
			t.Errorf("a vendor without runtimes on %+v: %+v", host, mismatch)
		}
	}
	if got := RuntimeMismatchFor(VendorKunlunxin, linux("udocker")).Params()["required_runtime"]; got != "docker/nerdctl/kube" {
		t.Errorf("required_runtime = %v", got)
	}
	// A vendor that restricts neither the system nor the architecture is
	// described by its runtimes alone.
	if got := RuntimeMismatchFor(VendorNVIDIA, linux("host")).Requirement(); got != "docker/nerdctl/udocker/kube" {
		t.Errorf("NVIDIA's requirement = %q", got)
	}
	if got := RuntimeMismatchFor(VendorKunlunxin, linux("udocker")).Requirement(); got != "linux/amd64 with docker/nerdctl/kube" {
		t.Errorf("Kunlunxin's requirement = %q", got)
	}
}

// A definition that omits how it is named identifies nothing: the
// consistency test then reports it by id, which a panic in the first lookup
// would not.
func TestADefinitionWithoutANameIsNeverTheAnswer(t *testing.T) {
	saved := vendorsByPrecedence
	t.Cleanup(func() { vendorsByPrecedence = saved })
	vendorsByPrecedence = append([]vendorSpec{{id: "nameless"}}, saved...)
	if got := NormalizeVendor("nameless"); got != "" {
		t.Fatalf("NormalizeVendor(nameless) = %q, want no vendor", got)
	}
	if got := NormalizeVendor("NVIDIA"); got != VendorNVIDIA {
		t.Fatalf("NormalizeVendor(NVIDIA) = %q behind a nameless definition", got)
	}
}
