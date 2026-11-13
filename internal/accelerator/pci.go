package accelerator

import (
	"strconv"
	"strings"
)

// Which PCI functions are accelerators this platform can use, and whose.
// This is vendor identity like the rest of this package, so it is the one
// place a PCI id is turned into a vendor: a new backend declares itself
// here or it is not recognized anywhere.
//
// Entries use recorded device/class/driver facts; AMD is recognized only by
// vendor 0x1002 bound to its existing amdgpu driver contract. No display-class
// fallback is declared for AMD here, and this does not claim a 395 hardware
// validation.
//
// A card is recognized two ways, and either is enough:
//
//   - its PCI class is one this vendor's accelerators use, or
//   - it is bound to this vendor's own accelerator driver.
//
// The second exists because the first cannot be kept exhaustive: a vendor
// that ships a new product line at a new class tells nobody, and the card
// would silently vanish from its node. A vendor changing its kernel
// module's name at the same time is the remaining gap, and that needs a
// new prober anyway. What falls through both is reported rather than
// dropped — see UnknownAcceleratorFunction.

// PCI class codes, as sysfs spells them in "class", less the programming
// interface byte. Their granularity is not uniform, because the classes
// themselves are not:
//
//   - 0x12 is wholly accelerators (PCI-SIG defined it for them), so the
//     whole base class counts and a new subclass — 0x1201 "AI Inference
//     Accelerator" already exists — needs no change here.
//   - 0x0b is the processor base class: 0x0b00..0x0b30 are CPUs and only
//     0x0b40, co-processor, is a card.
//   - 0x03 is display, shared by every BMC and integrated graphics chip on
//     the market, so only its two GPU subclasses count, and only for
//     vendors that do not also make display silicon.
//   - 0x07 is simple communication controllers, whose 0x0780 "other" is
//     where a card lands when its vendor claims no class of its own. A
//     server is full of 0x07 functions, so it counts one vendor at a time.
const (
	pciClassVGA                = 0x0300 // consumer cards (an RTX 3090 reports this)
	pciClass3D                 = 0x0302 // datacenter cards, and every PPU
	pciClassOtherCommunication = 0x0780 // Kunlunxin's P800
	pciClassCoProcessor        = 0x0b40 // Hygon's DCU
	pciBaseClassAccelerators   = 0x12   // Ascend's NPU, and where the industry is heading
)

type pciVendor struct {
	name string
	// classes are exact class values (class >> 8) this vendor's
	// accelerators report.
	classes []uint32
	// baseClasses are whole base classes (class >> 16) that belong to
	// accelerators outright.
	baseClasses []uint32
	// drivers are this vendor's own accelerator kernel modules. A function
	// of this vendor bound to one of them is a card whatever its class.
	drivers []string
	// notAccelerators are device ids of this vendor's own fixed-function
	// engines that report an accelerator class but are not accelerators a
	// node can run work on. They are named individually because the class
	// cannot tell them apart from a card, and because a vendor's next
	// accelerator must still be found by class rather than be excluded by
	// an allow list that predates it.
	notAccelerators []uint32
}

// pciVendors is read-only after init.
//
// Measured 2026-10-15: Ascend 910B4 19e5:d802 class 0x1200 bound to
// devdrv_device_driver (198.51.100.31/.32); PPU ZW810E 1ded:6001 class 0x0302
// bound to alipci (192.0.2.210); Hygon K100 "KONGMING" 1d94:6211 class
// 0x0b40 (192.0.2.133); NVIDIA RTX 2080 Ti 10de:1e04 class 0x0300 and
// Quadro RTX 8000 10de:1e78 class 0x0302, both bound to nvidia (gpu3).
// Measured 2026-10-28: Kunlunxin P800 2057:3686 class 0x0780 bound to kunlun,
// eight cards on each of two hosts (the node11 and node12 captures in
// internal/agent/gpu/testdata).
//
// Huawei deliberately has no 0x03: 19e5:1711, the BMC display controller on
// every one of its servers, reports 0x0300 and would otherwise be listed as
// an NPU. The same care will be needed for Intel if it is ever added, whose
// integrated graphics is 0x0300 and whose QAT and DSA are 0x0b40.
var pciVendors = map[uint32]pciVendor{
	AMDPCIVendorID: {
		name:    VendorAMD,
		drivers: []string{AMDGPUDriver},
	},
	0x2057: {name: VendorKunlunxin, classes: []uint32{pciClassOtherCommunication}, drivers: []string{"kunlun"}},
	0x10de: {
		name:    VendorNVIDIA,
		classes: []uint32{pciClassVGA, pciClass3D},
		drivers: []string{"nvidia"},
	},
	0x19e5: {
		name:        VendorAscend,
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"devdrv_device_driver"},
		// Kunpeng builds compression into the SoC: 19e5:a250, the ZIP
		// engine, reports 0x1200 like the NPU does and binds hisi_zip.
		// A 910C host presents eight of them (192.0.2.129, measured
		// 2026-10-25), and nothing can run inference on one.
		notAccelerators: []uint32{0xa250},
	},
	0x1ded: {
		name:        VendorPPU,
		classes:     []uint32{pciClass3D},
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"alipci"},
	},
	0x1d94: {
		name:        VendorHygon,
		classes:     []uint32{pciClassCoProcessor},
		baseClasses: []uint32{pciBaseClassAccelerators},
		drivers:     []string{"hydcu", "hycu"},
	},
}

// HasPCICards reports whether a vendor's cards are listed on a PCI bus,
// which makes it a vendor a node can be told to manage when its host has
// cards of more than one (Node.AcceleratorPreference). Apple's are not.
func HasPCICards(vendor string) bool {
	for _, v := range pciVendors {
		if v.name == vendor {
			return true
		}
	}
	return false
}

// IsVendorAcceleratorDriver reports whether a kernel module is the one
// this vendor's own stack binds to its accelerators. Anything else
// holding the card — nouveau, pci-stub, a hypervisor's passthrough stub —
// means the vendor's tools cannot reach it, which is a different thing
// from the driver having failed to bring it up.
func IsVendorAcceleratorDriver(vendorID uint32, driver string) bool {
	vendor, known := pciVendors[vendorID]
	if !known || driver == "" {
		return false
	}
	for _, want := range vendor.drivers {
		if driver == want {
			return true
		}
	}
	return false
}

// VendorForPCI names the accelerator vendor of a PCI function, or "" when
// the function is not one of this platform's accelerators. driver is the
// bound kernel module ("" when none).
//
// A card exposes more than its compute function: a 2080 Ti also presents an
// HD Audio function at class 0x0403 and two USB controllers at 0x0c03 and
// 0x0c80 (measured on gpu3). Those are different base classes and are never
// reached here, which is why the classes above can be widened safely.
func VendorForPCI(vendorID, deviceID, class uint32, driver string) string {
	vendor, known := pciVendors[vendorID]
	if !known {
		return ""
	}
	if vendor.accepts(deviceID, class, driver) {
		return vendor.name
	}
	return ""
}

func (v pciVendor) accepts(deviceID, class uint32, driver string) bool {
	if v.isNotAnAccelerator(deviceID) {
		return false
	}
	for _, want := range v.classes {
		if class>>8 == want {
			return true
		}
	}
	for _, want := range v.baseClasses {
		if class>>16 == want {
			return true
		}
	}
	for _, want := range v.drivers {
		if driver == want {
			return true
		}
	}
	return false
}

func (v pciVendor) isNotAnAccelerator(deviceID uint32) bool {
	for _, want := range v.notAccelerators {
		if deviceID == want {
			return true
		}
	}
	return false
}

// UnknownAcceleratorFunction reports a PCI function that belongs to a
// vendor whose accelerators this platform supports, that none of that
// vendor's rules recognize, and that could plausibly be a card — a new
// product line at a class nobody told us about. The caller says so instead
// of dropping it: the report carries exactly the ids needed to add a rule.
//
// Plausibility is not optional politeness. Huawei builds the whole server,
// so 0x19e5 is also the vendor of its PCI bridges, its SAS and SATA
// controllers, its USB host controllers and its NICs; reporting every
// unrecognized function of a supported vendor printed eighteen lines of
// them on 198.51.100.31 and would have buried the one line that matters.
//
// A function bound to vfio-pci is not reported either: a host passes all
// sorts of hardware through to its virtual machines, and the vendor id
// alone does not make it an accelerator.
func UnknownAcceleratorFunction(vendorID, deviceID, class uint32, driver string) bool {
	vendor, known := pciVendors[vendorID]
	if !known || driver == PCIDriverVFIO || vendor.accepts(deviceID, class, driver) {
		return false
	}
	if vendor.isNotAnAccelerator(deviceID) {
		// A known fixed-function engine is not news; reporting it would put
		// a permanent warning on every host that has one.
		return false
	}
	return plausiblyAnAccelerator(class, len(vendor.classes) > 0)
}

// plausiblyAnAccelerator reports whether a base class is one an
// accelerator could be found at. The listed base classes are the ones
// PCI-SIG has assigned to something that is plainly not an accelerator,
// and a base class it has not assigned at all counts as plausible — that
// is the case this exists for, since a new class is exactly what nobody
// will tell us about.
//
// displayCounts is false for a vendor whose accelerators do not use the
// display class, because that vendor's display functions are its BMC: on
// every Huawei server 19e5:1711 sits at 0x0300, and reporting it would put
// a permanent warning on every Ascend node.
func plausiblyAnAccelerator(class uint32, displayCounts bool) bool {
	switch class >> 16 {
	case 0x0b, 0x12: // processor, processing accelerators
		return true
	case 0x03: // display
		return displayCounts
	case 0x00, 0x01, 0x02, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09,
		0x0a, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x13:
		// Storage, network, multimedia, memory, bridges, communication,
		// system peripherals, input, docking, serial bus, wireless,
		// intelligent and satellite controllers, encryption, signal
		// processing, instrumentation. None of these has ever held an
		// accelerator, and a server is full of them.
		return false
	default:
		// A base class PCI-SIG has not assigned, or 0xff unassigned.
		return true
	}
}

// PCIVendorHex is a PCI vendor id as pci.ids and sysfs spell it, for
// looking a model name up and for naming an unidentified card.
func PCIVendorHex(id uint32) string {
	return pad4(strconv.FormatUint(uint64(id), 16))
}

// SysfsVendorIs reports whether a PCI function's sysfs "vendor" attribute,
// as read ("0x2057"), is the vendor whose accelerators this platform calls
// vendor. It answers for a backend that has to confirm, before handing a
// character device to a container, that the device it is about to grant
// really belongs to the card it probed.
//
// It exists so that a backend asks this package rather than carrying the id
// itself: a vendor's next generation shipping under a second id is a change
// to pciVendors, and a backend with its own copy would go on rejecting every
// card while the probe and the scheduler accepted them.
func SysfsVendorIs(vendor, sysfsVendor string) bool {
	vendor = NormalizeVendor(vendor)
	if vendor == "" {
		return false
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(strings.ToLower(sysfsVendor)), "0x"), 16, 32)
	if err != nil {
		return false
	}
	known, ok := pciVendors[uint32(id)]
	return ok && known.name == vendor
}

// PCIDeviceLabel is what a card is called when no name is known for it:
// its ids, the way lspci prints them ("10de:2204").
func PCIDeviceLabel(vendorID, deviceID uint32) string {
	return PCIVendorHex(vendorID) + ":" + PCIVendorHex(deviceID)
}

// PCIClassHex is a sysfs class value as lspci prints it ("0300").
func PCIClassHex(class uint32) string {
	return pad4(strconv.FormatUint(uint64(class>>8), 16))
}

func pad4(hex string) string {
	for len(hex) < 4 {
		hex = "0" + hex
	}
	return hex
}

// PCIDriverVFIO is the kernel module a function passed through to a
// virtual machine is bound to.
const PCIDriverVFIO = "vfio-pci"
