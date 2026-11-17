package accelerator

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Each vendor gets the checks that exist for its cards, in the catalog's
// order; Apple's unified GPU has none of them.
func TestCardChecksPerVendor(t *testing.T) {
	ids := func(vendor string) string {
		var out []string
		for _, spec := range CardChecks(vendor) {
			out = append(out, spec.ID)
		}
		return strings.Join(out, ",")
	}
	for vendor, want := range map[string]string{
		VendorNVIDIA: "vendor_health,vendor_cli,occupancy,pcie_link,pcie_errors,xid,nvlink,runtime,peer_memory",
		VendorAscend: "vendor_health,vendor_cli,occupancy,pcie_link,pcie_errors,runtime",
		VendorHygon:  "vendor_health,vendor_cli,occupancy,pcie_link,pcie_errors,runtime",
		VendorPPU:    "vendor_health,vendor_cli,occupancy,pcie_link,pcie_errors,runtime,peer_memory",
		VendorAMD:    "vendor_health,vendor_cli,occupancy,pcie_link,pcie_errors",
		VendorApple:  "",
		"":           "",
	} {
		if got := ids(vendor); got != want {
			t.Errorf("%q: %s, want %s", vendor, got, want)
		}
	}
	if got := strings.Join(CardCheckIDs(), ","); got != ids(VendorNVIDIA) {
		t.Errorf("all ids = %s", got)
	}
}

func TestCheckStatusFollowsTheCodesLevel(t *testing.T) {
	for code, want := range map[string]string{
		"":                        proto.CheckPass,
		HealthMemoryErrorHistory:  proto.CheckPass,
		HealthPCIeLinkDegraded:    proto.CheckWarn,
		HealthRuntimeUnusable:     proto.CheckFail,
		"SOME_CODE_FROM_TOMORROW": proto.CheckFail,
	} {
		if got := CheckStatusFor(code); got != want {
			t.Errorf("%q = %s, want %s", code, got, want)
		}
	}
	if CheckProblem(proto.CheckSkip) || CheckProblem(proto.CheckPass) || !CheckProblem(proto.CheckWarn) || !CheckProblem(proto.CheckFail) {
		t.Error("CheckProblem")
	}
}

// Every check names a command for a card it can address, and none for one it
// cannot.
func TestEveryCheckHasAProbe(t *testing.T) {
	card := HealthCard{Vendor: VendorNVIDIA, BDF: "0000:3F:00.0", Index: 0, UUID: "GPU-3f"}
	for _, id := range CardCheckIDs() {
		if CardCheckProbe(id, card) == "" {
			t.Errorf("%s has no probe for an NVIDIA card", id)
		}
	}
	if got := CardCheckProbe(CheckPCIeLink, card); got != "lspci -vv -s 0000:3f:00.0 | grep -E 'LnkCap:|LnkSta:'" {
		t.Errorf("pcie link probe = %q", got)
	}
	if got := CardCheckProbe(CheckRuntime, HealthCard{Vendor: VendorNVIDIA}); got != "" {
		t.Errorf("runtime probe without a UUID = %q", got)
	}
	if got := CardCheckProbe(CheckVendorHealth, HealthCard{Vendor: VendorHygon, BDF: "0000:41:00.0"}); !strings.Contains(got, "/sys/bus/pci/devices/0000:41:00.0/ras/") {
		t.Errorf("hygon vendor probe = %q", got)
	}
	if got := CardCheckProbe("unknown", card); got != "" {
		t.Errorf("unknown check probe = %q", got)
	}
}

func TestPeerMemoryModuleAndFix(t *testing.T) {
	for vendor, want := range map[string]string{VendorPPU: "alixpu_peermem", VendorNVIDIA: "nvidia_peermem", VendorAscend: "", VendorHygon: ""} {
		if got := PeerMemoryModule(vendor); got != want {
			t.Errorf("%s module = %q, want %q", vendor, got, want)
		}
	}
	if got := PeerMemoryFix(VendorPPU); got != "modprobe alixpu_peermem && echo alixpu_peermem > /etc/modules-load.d/alixpu_peermem.conf" {
		t.Errorf("fix = %q", got)
	}
	if PeerMemoryFix(VendorAscend) != "" {
		t.Error("Ascend has no peer-memory module to load")
	}
	if got := CardCheckProbe(CheckPeerMemory, HealthCard{Vendor: VendorPPU}); got != "lsmod | grep alixpu_peermem" {
		t.Errorf("probe = %q", got)
	}
}
