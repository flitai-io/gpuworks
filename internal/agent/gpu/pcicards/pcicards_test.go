package pcicards

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// pciFunction is one entry of a fixture /sys/bus/pci/devices.
type pciFunction struct {
	bdf      string
	vendor   string // sysfs "vendor", e.g. "0x10de"
	device   string
	class    string // sysfs "class", e.g. "0x030000"
	driver   string // bound module, "" for unbound
	numaNode string
	physfn   bool // an SR-IOV virtual function
}

func hostTree(t *testing.T, functions []pciFunction, pciIDs string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range functions {
		device := filepath.Join(root, "sys/bus/pci/devices", f.bdf)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		write := func(name, value string) {
			if value == "" {
				return
			}
			if err := os.WriteFile(filepath.Join(device, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("vendor", f.vendor)
		write("device", f.device)
		write("class", f.class)
		write("numa_node", f.numaNode)
		if f.driver != "" {
			if err := os.Symlink("../../../../bus/pci/drivers/"+f.driver, filepath.Join(device, "driver")); err != nil {
				t.Fatal(err)
			}
		}
		if f.physfn {
			if err := os.Symlink("../0000:00:00.0", filepath.Join(device, "physfn")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if pciIDs != "" {
		path := filepath.Join(root, "usr/share/hwdata/pci.ids")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(pciIDs), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The PCI functions zstone-164 reports, as sysfs shows them: eight 3090s
// (two passed through, one unbound, five bound to nvidia), each with an
// audio function, plus a BMC display adapter and an SR-IOV slice.
func TestListReportsEveryAcceleratorAndWhatHoldsIt(t *testing.T) {
	root := hostTree(t, []pciFunction{
		{bdf: "0000:1a:00.0", vendor: "0x10de", device: "0x2204", class: "0x030000", numaNode: "0"},
		{bdf: "0000:1a:00.1", vendor: "0x10de", device: "0x1aef", class: "0x040300", driver: "snd_hda_intel", numaNode: "0"},
		{bdf: "0000:1b:00.0", vendor: "0x10de", device: "0x2204", class: "0x030000", driver: "vfio-pci", numaNode: "0"},
		{bdf: "0000:1c:00.0", vendor: "0x10de", device: "0x2204", class: "0x030000", driver: "nvidia", numaNode: "0"},
		// A datacenter card reports the 3D class rather than VGA.
		{bdf: "0000:3f:00.0", vendor: "0x10de", device: "0x20b2", class: "0x030200", driver: "nvidia", numaNode: "1"},
		// Not accelerators: a USB-C controller on the card, the BMC's
		// display adapter, and a virtual function of an SR-IOV card.
		{bdf: "0000:1a:00.2", vendor: "0x10de", device: "0x1aeb", class: "0x0c0330", driver: "xhci_hcd"},
		{bdf: "0000:05:00.0", vendor: "0x1a03", device: "0x2000", class: "0x030000", driver: "ast"},
		{bdf: "0000:41:00.4", vendor: "0x10de", device: "0x2204", class: "0x030000", driver: "nvidia", physfn: true},
	}, "")

	got, _ := Scanner{Root: root}.Scan()
	want := []Card{
		{BDF: "0000:1a:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x2204, Driver: DriverNone, NUMANode: 0},
		{BDF: "0000:1b:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x2204, Driver: DriverVFIO, NUMANode: 0},
		{BDF: "0000:1c:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x2204, Driver: "nvidia", NUMANode: 0},
		{BDF: "0000:3f:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x20b2, Driver: "nvidia", NUMANode: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cards =\n%+v\nwant\n%+v", got, want)
	}
}

func TestListIsEmptyWithoutAReadableSysfs(t *testing.T) {
	if got, _ := (Scanner{Root: t.TempDir()}).Scan(); got != nil {
		t.Fatalf("a host without PCI sysfs = %+v, want none", got)
	}
}

// A card with no NUMA information, and one whose attributes cannot be read
// at all, must not be guessed at.
func TestListSkipsFunctionsItCannotIdentify(t *testing.T) {
	root := hostTree(t, []pciFunction{
		{bdf: "0000:1c:00.0", vendor: "0x10de", device: "0x2204", class: "0x030000", driver: "nvidia"},
		{bdf: "0000:1d:00.0", class: "0x030000", driver: "nvidia"},                   // no vendor id
		{bdf: "0000:1e:00.0", vendor: "0x10de", device: "0x2204", driver: "nvidia"},  // no class
		{bdf: "0000:1f:00.0", vendor: "0xdead", device: "0x0001", class: "0x030000"}, // unknown vendor
	}, "")
	got, _ := Scanner{Root: root}.Scan()
	if len(got) != 1 || got[0].BDF != "0000:1c:00.0" || got[0].NUMANode != -1 {
		t.Fatalf("cards = %+v, want only the identified one with an unknown NUMA node", got)
	}
}

// A Kunpeng host presents its SoC compression engines at the same PCI class
// as the NPU. They are neither cards nor news: 192.0.2.129 has eight ZIP
// engines beside its fourteen 910C dies.
func TestListSkipsTheSoCCompressionEngines(t *testing.T) {
	root := hostTree(t, []pciFunction{
		{bdf: "0000:85:00.0", vendor: "0x19e5", device: "0xd803", class: "0x120000", driver: "devdrv_device_driver"},
		{bdf: "0000:31:00.0", vendor: "0x19e5", device: "0xa250", class: "0x120000", driver: "hisi_zip"},
		{bdf: "0000:33:00.0", vendor: "0x19e5", device: "0xa250", class: "0x120000"},
	}, "")
	got, unknown := Scanner{Root: root}.Scan()
	if len(got) != 1 || got[0].BDF != "0000:85:00.0" {
		t.Fatalf("cards = %+v, want only the NPU die", got)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown = %+v, want the ZIP engines reported as neither cards nor unknown", unknown)
	}
}

const pciIDsFixture = `# Comment lines and blank lines are skipped.

1002  Advanced Micro Devices, Inc. [AMD/ATI]
	1234  Some AMD part
10de  NVIDIA Corporation
	1aef  GA102 High Definition Audio Controller
	2204  GA102 [GeForce RTX 3090]
		10de 1454  GeForce RTX 3090 Founders Edition
	20b2  GA100 [A100 SXM4 80GB]
1a03  ASPEED Technology, Inc.
	2000  ASPEED Graphics Family
`

func TestModelNamesReadsThePCIDatabaseForTheCardsAsked(t *testing.T) {
	root := hostTree(t, nil, pciIDsFixture)
	cards := []Card{
		{BDF: "0000:1c:00.0", VendorID: 0x10de, DeviceID: 0x2204},
		{BDF: "0000:3f:00.0", VendorID: 0x10de, DeviceID: 0x20b2},
		{BDF: "0000:40:00.0", VendorID: 0x10de, DeviceID: 0x9999}, // not in the database
	}
	got := Scanner{Root: root}.ModelNames(cards)
	want := map[string]string{
		// The vendor brand word is dropped, as it is for a driver's name.
		"0000:1c:00.0": "GA102 [GeForce RTX 3090]",
		"0000:3f:00.0": "GA100 [A100 SXM4 80GB]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %+v, want %+v", got, want)
	}
}

// A subsystem line names a board variant, not the chip; the device line is
// the name to use.
func TestModelNamesPrefersTheDeviceLineOverItsSubsystems(t *testing.T) {
	root := hostTree(t, nil, pciIDsFixture)
	got := Scanner{Root: root}.ModelNames([]Card{{BDF: "a", VendorID: 0x10de, DeviceID: 0x2204}})
	if got["a"] != "GA102 [GeForce RTX 3090]" {
		t.Fatalf("name = %q", got["a"])
	}
}

// A card neither this agent nor the host has heard of is left for the
// caller to name by its ids ("vendor:device").
func TestModelNamesWithoutADatabaseNamesNothingItHasNotHeardOf(t *testing.T) {
	root := hostTree(t, nil, "")
	if got := (Scanner{Root: root}).ModelNames([]Card{{BDF: "a", VendorID: 0x10de, DeviceID: 0xffff}}); len(got) != 0 {
		t.Fatalf("names = %+v, want none so the caller falls back to the ids", got)
	}
	if got := (Scanner{Root: root}).ModelNames(nil); got != nil {
		t.Fatalf("names for no cards = %+v", got)
	}
}

func TestPCIDeviceLabelNamesACardWithNoKnownName(t *testing.T) {
	if got := accelerator.PCIDeviceLabel(0x10de, 0x2204); got != "10de:2204" {
		t.Fatalf("label = %q", got)
	}
	if got := accelerator.PCIDeviceLabel(0x1d, 0x2); got != "001d:0002" {
		t.Fatalf("short ids = %q, want them padded as lspci prints", got)
	}
}

// The domestic accelerators upstream pci.ids lacks are named by the agent
// itself, whatever the host's database says (or whether it has one). A host
// that cannot run its vendor CLI has no other way to say what a card is,
// and the hosts that cannot are exactly the ones with a wedged driver:
// 198.51.100.31 showed eight "19e5:d802" before this table existed.
func TestADomesticCardUpstreamLacksIsNamedWithoutAnyHostDatabase(t *testing.T) {
	cards := []Card{
		{BDF: "0000:41:00.0", VendorID: 0x19e5, DeviceID: 0xd802},
		{BDF: "0000:81:00.0", VendorID: 0x19e5, DeviceID: 0xd803},
		{BDF: "0000:71:00.0", VendorID: 0x19e5, DeviceID: 0xd806},
		{BDF: "0000:08:00.0", VendorID: 0x1ded, DeviceID: 0x6001},
	}
	// Each is the name this platform shows when the CLI does answer, so one
	// card never reads two ways.
	want := map[string]string{
		"0000:41:00.0": "910B4",
		"0000:81:00.0": "910C",
		"0000:71:00.0": "950PR",
		"0000:08:00.0": "PPU-ZW810E",
	}
	for name, root := range map[string]string{
		"no database":             t.TempDir(),
		"a database without them": hostTree(t, nil, pciIDsFixture),
	} {
		if got := (Scanner{Root: root}).ModelNames(cards); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: names = %+v, want %+v", name, got, want)
		}
	}
}

// The domestic table is consulted before the host's file, so a stale or
// wrong host entry cannot rename a card the agent has read off hardware.
func TestTheDomesticTableWinsOverTheHostDatabase(t *testing.T) {
	root := hostTree(t, nil, "19e5  Huawei Technologies Co., Ltd.\n\td802  Some Stale Name\n")
	got := Scanner{Root: root}.ModelNames([]Card{{BDF: "a", VendorID: 0x19e5, DeviceID: 0xd802}})
	if got["a"] != "910B4" {
		t.Fatalf("name = %q, want the domestic table's", got["a"])
	}
}

// A vendor upstream covers is named from the host's database alone: the
// agent carries no NVIDIA table.
func TestNVIDIACardsAreNamedFromTheHostDatabaseOnly(t *testing.T) {
	cards := []Card{{BDF: "0000:1c:00.0", VendorID: 0x10de, DeviceID: 0x2204}}
	if got := (Scanner{Root: t.TempDir()}).ModelNames(cards); len(got) != 0 {
		t.Fatalf("names = %+v with no host database, want none (the caller shows 10de:2204)", got)
	}
	got := Scanner{Root: hostTree(t, nil, pciIDsFixture)}.ModelNames(cards)
	if got["0000:1c:00.0"] != "GA102 [GeForce RTX 3090]" {
		t.Fatalf("names = %+v, want the host database's name", got)
	}
}

// An entry exists only for a vendor the platform identifies, carries the
// provenance that justifies it.
func TestEveryDomesticModelIsVerifiedAndBelongsToASupportedVendor(t *testing.T) {
	seen := map[[2]uint32]bool{}
	for _, m := range accelerator.PCIModels() {
		id := [2]uint32{m.Vendor, m.Device}
		if seen[id] {
			t.Errorf("%04x:%04x listed twice", m.Vendor, m.Device)
		}
		seen[id] = true
		if m.Name == "" || m.Provenance == "" {
			t.Errorf("%04x:%04x needs a name and the hardware it was read off", m.Vendor, m.Device)
		}
		if accelerator.VendorForPCI(m.Vendor, m.Device, 0x120000, "") == "" &&
			accelerator.VendorForPCI(m.Vendor, m.Device, 0x030200, "") == "" {
			t.Errorf("%04x is not a vendor any rule identifies as an accelerator vendor", m.Vendor)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no vendor's definition carries a model name")
	}
}

// A container sees the whole host's PCI bus, so the bus is not ours to
// describe from inside one: an agent in a rented pod given one GPU of an
// eight-GPU machine would report the other seven as this node's cards
// whose driver failed. The vendor CLI, scoped to what the container was
// given, stays the only source there.
func TestAContainerDoesNotDescribeTheHostsBus(t *testing.T) {
	root := hostTree(t, []pciFunction{
		{bdf: "0000:1c:00.0", vendor: "0x10de", device: "0x2204", class: "0x030000", driver: "nvidia"},
	}, "")
	// A fixture root is the caller's own bus whatever markers exist.
	if cards, _ := (Scanner{Root: root}).Scan(); len(cards) != 1 {
		t.Fatalf("fixture root = %+v, want the card it describes", cards)
	}
	// With a marker present and no fixture root, nothing is claimed.
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	restore := containerMarkers
	containerMarkers = []string{marker}
	defer func() { containerMarkers = restore }()
	if cards, unknown := (Scanner{}).Scan(); cards != nil || unknown != nil {
		t.Fatalf("inside a container: cards=%+v unknown=%+v, want nothing claimed", cards, unknown)
	}
}

// Kubernetes and plain containerd leave no marker file at the root, and a
// pod sees the host's whole bus just the same. Each of what they do leave
// is enough on its own.
func TestAPodWithoutAMarkerFileIsStillAContainer(t *testing.T) {
	restoreMarkers, restoreCgroup := containerMarkers, procOneCgroup
	containerMarkers = nil
	defer func() { containerMarkers, procOneCgroup = restoreMarkers, restoreCgroup }()
	for _, name := range containerEnv {
		t.Setenv(name, "")
	}
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	procOneCgroup = cgroup
	writeCgroup := func(text string) {
		t.Helper()
		if err := os.WriteFile(cgroup, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeCgroup("0::/init.scope\n")
	if !(Scanner{}).hostOwnsThisBus() {
		t.Fatal("a host's own PID 1 was taken for a container")
	}
	writeCgroup("12:memory:/kubepods/burstable/pod1234/abcd\n0::/\n")
	if (Scanner{}).hostOwnsThisBus() {
		t.Fatal("a pod's cgroup path was not recognized")
	}
	writeCgroup("0::/init.scope\n")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	if (Scanner{}).hostOwnsThisBus() {
		t.Fatal("a pod's environment was not recognized")
	}
}
