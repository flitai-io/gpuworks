package gpu

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu/pcicards"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The operator's choice picks that vendor's backend even when its CLI does
// not answer, so the probe failure is reported as that vendor's; automatic
// and anything unknown leave the pick to New.
func TestTheChosenVendorPicksItsBackend(t *testing.T) {
	probers := defaultProbers()
	for vendor, want := range map[string]any{
		"nvidia":    &SMIProber{},
		"ascend":    &AscendProber{},
		"hygon":     &HygonProber{},
		"ppu":       &PPUProber{},
		"amd":       &AMDProber{},
		"kunlunxin": &KunlunProber{},
	} {
		got := chosenProber(probers, vendor)
		if reflect.TypeOf(got) != reflect.TypeOf(want) {
			t.Fatalf("chosenProber(%q) = %T, want %T", vendor, got, want)
		}
		if got := NewFor(vendor); got.Vendor() != vendor {
			t.Fatalf("NewFor(%q).Vendor() = %q", vendor, got.Vendor())
		}
	}
	for _, other := range []string{"", proto.AcceleratorPreferenceAuto, "apple", "intel"} {
		if got := chosenProber(probers, other); got != nil {
			t.Fatalf("chosenProber(%q) = %T, want New's pick", other, got)
		}
	}
}

// writeBusCard puts one PCI function into a fake sysfs.
func writeBusCard(t *testing.T, root, bdf, vendor, device, class, driver string) {
	t.Helper()
	dir := filepath.Join(root, "sys/bus/pci/devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"vendor": vendor, "device": device, "class": class, "numa_node": "0"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if driver != "" {
		if err := os.Symlink("../../../../bus/pci/drivers/"+driver, filepath.Join(dir, "driver")); err != nil {
			t.Fatal(err)
		}
	}
}

func writePCIIDs(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "usr/share/hwdata/pci.ids")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const mixedHostPCIIDs = "10de  NVIDIA Corporation\n\t2684  AD102 [GeForce RTX 4090]\n\t26b9  AD102GL [L40S]\n19e5  Huawei Technologies Co., Ltd.\n"

// The host 192.0.2.115 as its bus showed it on 2026-10-17: an RTX 4090 and
// an Atlas 310P3 (19e5:d500, which pci.ids does not name), next to the
// ASPEED BMC display and the Huawei-id BMC display function that are
// nobody's accelerator. Both vendors are listed; a card passed through to a
// virtual machine is not the host's, and a vendor with only such cards is
// left out.
func TestTheCensusListsEveryVendorWhoseCardsTheHostCanUse(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:65:00.0", "0x10de", "0x2684", "0x030000", "nvidia")
	writeBusCard(t, root, "0000:17:00.0", "0x19e5", "0xd500", "0x120000", "devdrv_device_driver")
	writeBusCard(t, root, "0000:03:00.0", "0x1a03", "0x2000", "0x030000", "ast")
	writeBusCard(t, root, "0000:04:00.0", "0x19e5", "0x1711", "0x030000", "")
	writeBusCard(t, root, "0000:b1:00.0", "0x1d94", "0x6211", "0x0b4000", "vfio-pci")
	writePCIIDs(t, root, mixedHostPCIIDs)

	census := &VendorCensus{HostRoot: root}
	got := census.Vendors()
	want := []proto.AcceleratorPresence{
		{Vendor: "ascend", Cards: 1, Model: "310P3"},
		{Vendor: "nvidia", Cards: 1, Model: "AD102 [GeForce RTX 4090]"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vendors = %#v, want %#v", got, want)
	}
}

// A card without its driver loaded is still the host's: choosing its
// vendor is how the operator finds out the driver is missing.
func TestACardWithoutItsDriverIsCounted(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:65:00.0", "0x10de", "0x2684", "0x030000", "")
	writeBusCard(t, root, "0000:66:00.0", "0x10de", "0x2684", "0x030000", "nvidia")
	writeBusCard(t, root, "0000:67:00.0", "0x10de", "0x26b9", "0x030200", "nvidia")
	writePCIIDs(t, root, mixedHostPCIIDs)
	got := (&VendorCensus{HostRoot: root}).Vendors()
	want := []proto.AcceleratorPresence{{Vendor: "nvidia", Cards: 3, Model: "AD102 [GeForce RTX 4090]"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vendors = %#v, want %#v", got, want)
	}
}

// pci.ids is read again only when the cards change: a status report every
// few seconds must not stream megabytes each time.
func TestTheCensusNamesCardsOnlyWhenTheyChange(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:65:00.0", "0x10de", "0x2684", "0x030000", "nvidia")
	ids := writePCIIDs(t, root, mixedHostPCIIDs)
	census := &VendorCensus{HostRoot: root}
	first := census.Vendors()
	if err := os.Remove(ids); err != nil {
		t.Fatal(err)
	}
	if again := census.Vendors(); !reflect.DeepEqual(again, first) {
		t.Fatalf("unchanged cards read again: %#v, first %#v", again, first)
	}
	// The returned slice is the caller's.
	again := census.Vendors()
	again[0].Cards = 99
	if census.Vendors()[0].Cards != 1 {
		t.Fatal("a caller's edit reached the cache")
	}

	writeBusCard(t, root, "0000:17:00.0", "0x19e5", "0xd500", "0x120000", "devdrv_device_driver")
	got := census.Vendors()
	want := []proto.AcceleratorPresence{
		{Vendor: "ascend", Cards: 1, Model: "310P3"},
		// pci.ids is gone now, and the card set changed: named afresh.
		{Vendor: "nvidia", Cards: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vendors after a card appeared = %#v, want %#v", got, want)
	}
}

// A bus that cannot be read, or that has no accelerator, lists nothing.
func TestACensusOfNoBusIsEmpty(t *testing.T) {
	if got := (&VendorCensus{HostRoot: t.TempDir()}).Vendors(); got != nil {
		t.Fatalf("vendors = %#v", got)
	}
	root := t.TempDir()
	writeBusCard(t, root, "0000:03:00.0", "0x1a03", "0x2000", "0x030000", "ast")
	census := &VendorCensus{HostRoot: root}
	for range 2 {
		if got := census.Vendors(); got != nil {
			t.Fatalf("vendors = %#v", got)
		}
	}
}

func TestTheCommonModelIsTheMostFrequentName(t *testing.T) {
	for _, tc := range []struct {
		names map[string]string
		want  string
	}{
		{nil, ""},
		{map[string]string{"a": "L40S", "b": "RTX 4090", "c": "RTX 4090"}, "RTX 4090"},
		{map[string]string{"a": "L40S", "b": "RTX 4090"}, "L40S"},
		{map[string]string{"a": " ", "b": "RTX 4090"}, "RTX 4090"},
	} {
		var cards []pcicards.Card
		for bdf := range tc.names {
			cards = append(cards, pcicards.Card{BDF: bdf})
		}
		if got := commonModel(cards, tc.names); got != tc.want {
			t.Fatalf("commonModel(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}
