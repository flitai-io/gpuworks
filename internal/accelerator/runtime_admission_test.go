package accelerator

import "testing"

// Which runtimes each vendor's cards are admitted on, written out. The
// lists decide what is refused before scheduling, so dropping a runtime
// from one or adding one is a change of behaviour that has to be made here
// too. The host is one the vendor runs on, so only the runtime decides.
func TestVendorRuntimeAdmission(t *testing.T) {
	linux := HostRuntime{OS: "linux", Arch: "amd64"}
	mac := HostRuntime{OS: "darwin", Arch: "arm64"}
	containers := []string{"docker", "nerdctl", "udocker", "kube"}
	for _, tc := range []struct {
		vendor   string
		host     HostRuntime
		admitted []string
	}{
		{VendorNVIDIA, linux, containers},
		{VendorAMD, linux, containers},
		{VendorAscend, linux, containers},
		{VendorPPU, linux, containers},
		{VendorHygon, linux, containers},
		{VendorKunlunxin, linux, []string{"docker", "nerdctl", "kube"}},
		{VendorApple, mac, []string{"host"}},
	} {
		for _, driver := range Runtimes() {
			host := tc.host
			host.Driver = driver
			want := false
			for _, admitted := range tc.admitted {
				want = want || admitted == driver
			}
			if got := RuntimeMismatchFor(tc.vendor, host) == nil; got != want {
				t.Errorf("%s on %s: admitted = %v, want %v", tc.vendor, driver, got, want)
			}
		}
	}
	if got := len(KnownVendors()); got != 7 {
		t.Errorf("%d vendors are defined; the table above covers 7", got)
	}
}
