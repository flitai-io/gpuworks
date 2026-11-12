package accelerator

import "testing"

func TestAMDHostRequirementsPreserveBothDeviceRuntimes(t *testing.T) {

	for _, driver := range []string{"docker", "nerdctl", "Docker", "kube"} {
		for _, arch := range []string{"amd64", "x86_64"} {
			if got := RuntimeMismatchParams("AMD", HostRuntime{OS: "linux", Arch: arch, Driver: driver}); got != nil {
				t.Fatalf("supported host rejected: %v", got)
			}
		}
	}
	for _, host := range []HostRuntime{
		{OS: "darwin", Arch: "amd64", Driver: "docker"},
		{OS: "linux", Arch: "arm64", Driver: "docker"},
		{OS: "linux", Arch: "amd64", Driver: "host"},
		{OS: "linux", Arch: "amd64", Driver: ""},
	} {
		got := RuntimeMismatchParams(VendorAMD, host)
		if got == nil || got["required_runtime"] != "docker/nerdctl/udocker/kube" || got["required_os"] != "linux" || got["runtime"] != host.Driver {
			t.Fatalf("missing host refusal facts: %v", got)
		}
	}
	for _, vendor := range []string{VendorNVIDIA, VendorAscend, VendorApple, ""} {
		if got := RuntimeMismatchParams(vendor, HostRuntime{}); got != nil {
			t.Fatalf("AMD host restriction leaked to %q: %v", vendor, got)
		}
	}
	if !RequiresDeviceSelection(VendorAMD) || RequiresDeviceSelection(VendorNVIDIA) || SharedByMemory(VendorAMD) {
		t.Fatal("whole-card AMD allocation contract changed")
	}
}

func TestAMDRuntimeDistinguishesMissingFactsFromKnownMismatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    HostRuntime
		missing bool
	}{
		{"no facts", HostRuntime{}, true},
		{"missing runtime", HostRuntime{OS: "linux", Arch: "amd64"}, true},
		{"missing OS", HostRuntime{Arch: "amd64", Driver: "docker"}, true},
		{"unsupported runtime", HostRuntime{OS: "linux", Arch: "amd64", Driver: "host"}, false},
		{"known mismatch with missing fact", HostRuntime{OS: "darwin", Arch: "amd64"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := RuntimeMismatchParams(VendorAMD, tc.host)
			if params == nil || params["missing_facts"] != tc.missing {
				t.Fatalf("runtime refusal=%v, want missing=%v", params, tc.missing)
			}
		})
	}
}
