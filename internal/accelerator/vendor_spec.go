package accelerator

import (
	"sort"
	"strings"
)

// vendorSpec is a vendor's definition: everything the platform is told
// about it. The functions of this package that answer for a vendor read its
// definition, and nothing else in the package names a vendor. A field left
// out means the vendor does not have the thing, so what a definition does
// not declare is refused, not assumed.
//
// Each vendor's definition is in the file of its name (nvidia.go, ...) and
// is listed once in vendorSpecs.
type vendorSpec struct {
	// Identity.

	// id is the canonical name used on the wire.
	id string
	// brand is the name shown to people.
	brand string
	// frameworks are the runtimes whose images belong to this vendor
	// (FrameworkVendor). An image declares a framework, never a vendor.
	frameworks []string
	// named reports whether a lowercased, trimmed string names this vendor:
	// its id, its frameworks' and products' names, what its tools print.
	named func(string) bool
	// precedence decides which vendor a string naming two of them belongs
	// to; the lower wins.
	precedence int
	// namePrefixes are the brand words its probes put in front of a model
	// name, which say nothing the vendor field does not (NormalizeGPUName).
	namePrefixes []string
	// domestic: the vendor has its own runtime ABI, so its nodes cannot
	// inherit a CUDA/ROCm engine default image (IsDomestic).
	domestic bool

	// Finding its cards on a host.

	// tool is the vendor tool's own overview command (ToolCommand); empty
	// for a vendor read without one.
	tool string
	// detectCommand is the name of its tool when that name is its alone,
	// and controlDevices are device nodes its kernel driver creates. When
	// no vendor's tool answers, a host with either is still reported as
	// this vendor's, so the failure stays attributable (DetectCommand,
	// ControlDevices).
	detectCommand  string
	controlDevices []string
	// pciID and pci say how its cards are recognized on a PCI bus; pci is
	// nil for a vendor whose cards are not on one.
	pciID uint32
	pci   *pciVendor
	// pciModels names the cards upstream pci.ids has never listed, so that
	// one whose tool cannot answer is still shown by its model (PCIModels).
	// An entry is added only when it was read off real hardware and says
	// where; it is removed once upstream has the name.
	pciModels []PCIModel
	// arch derives the architecture identity from what the card reports;
	// nil leaves the reported id as it is (GPUArch).
	arch func(archID, computeCapability string) Arch
	// cardNetwork describes the network ports on the cards themselves.
	cardNetwork *CardNetwork

	// Running workloads on its cards.

	// runtimes are the runtime drivers a workload can be given its cards
	// on. A node on any other driver is refused before
	// scheduling, so a vendor that lists none carries no workload: its
	// cards are found and monitored only.
	runtimes []string
	// hostOS and hostArch are the operating system and architecture its
	// host must have; empty for a vendor that runs on any. A vendor that
	// requires either is refused on a node that has not reported its
	// runtime, where the others wait for the report.
	hostOS, hostArch string
	// explicitDevices: its cards must be selected by index; a workload
	// cannot be left to take whichever it sees (RequiresDeviceSelection).
	explicitDevices bool
	// supports are the platform features offered on its cards beyond plain
	// inference (Supports). What is not listed is refused, on the server
	// before scheduling and on the agent at admission.
	supports []Feature
	// partition is its dGPU backend, which divides a card's memory among
	// workloads; nil for a vendor whose cards are given out whole.
	partition Backend
	// gpuPlugins names, per runtime driver, the plugin that driver needs to
	// give a container this vendor's cards (RuntimeGPUPlugin). gpuFallback:
	// a node may fall back to a runtime that needs none (RuntimeGPUFallback).
	gpuPlugins  map[string]string
	gpuFallback bool
	// collectiveInterface: its collective runtime needs a real host
	// interface once more than one device or node takes part.
	collectiveInterface bool
	// assetBundles: its partition backend ships runtime asset bundles
	// (HasRuntimeAssetBundles).
	assetBundles bool
	// ownCatalogImage: its engine image has to come from the catalog; no
	// engine default applies (RequiresVendorCatalogImage).
	ownCatalogImage bool
	// sleepPoolOnHostMemory and sharedByMemory: see the functions of the
	// same names.
	sleepPoolOnHostMemory bool
	sharedByMemory        bool

	// Checking its cards.

	// cardChecks: its cards are checked at all (CardChecks). checks are the
	// checks only some vendors' cards get, beyond the ones every card gets.
	cardChecks bool
	checks     []string
	// peerMemoryModule is the kernel module that lets an RDMA NIC read its
	// device memory (PeerMemoryModule).
	peerMemoryModule string
	// smokeFramework and smokeScript are the image framework and the script
	// of the per-card runtime check run in a container, for a vendor whose
	// hosts often lack the runtime (ContainerSmokeFramework/Script).
	smokeFramework string
	smokeScript    string
	// healthProbe is the command that shows what the vendor tool's health
	// verdict on a card looks at, "" when the card cannot be addressed;
	// target is HealthTarget's address and bdf the normalized PCI address
	// (CardCheckProbe).
	healthProbe func(target, bdf string) string
	// runtimeGuide says what to run for a card its host runtime could not
	// use; nil for a vendor whose runtime is not asked on the host.
	runtimeGuide func(card HealthCard) (HealthGuide, bool)
	// healthGuide says what to run for a finding on one card; nil for a
	// vendor with no rules of its own (HealthGuideFor).
	healthGuide func(card HealthCard, target string, evidence []HealthField) (HealthGuide, bool)
}

func contains(names ...string) func(string) bool {
	return func(v string) bool {
		for _, name := range names {
			if strings.Contains(v, name) {
				return true
			}
		}
		return false
	}
}

func either(matchers ...func(string) bool) func(string) bool {
	return func(v string) bool {
		for _, match := range matchers {
			if match(v) {
				return true
			}
		}
		return false
	}
}

func exactly(names ...string) func(string) bool {
	return func(v string) bool {
		for _, name := range names {
			if v == name {
				return true
			}
		}
		return false
	}
}

// vendorSpecs lists every vendor, in the order KnownVendors reports them.
// Adding a vendor's definition here is what makes it one of the platform's.
var vendorSpecs = []vendorSpec{
	nvidiaVendor,
	amdVendor,
	ascendVendor,
	ppuVendor,
	hygonVendor,
	appleVendor,
	kunlunVendor,
}

// vendorsByPrecedence is vendorSpecs in the order names are tried.
var vendorsByPrecedence = func() []vendorSpec {
	ordered := append([]vendorSpec(nil), vendorSpecs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].precedence < ordered[j].precedence })
	return ordered
}()

// vendorSpecOf finds a vendor's entry by any of its names.
func vendorSpecOf(vendor string) (vendorSpec, bool) {
	id := NormalizeVendor(vendor)
	for _, spec := range vendorSpecs {
		if spec.id == id {
			return spec, true
		}
	}
	return vendorSpec{}, false
}

// vendorSpecByID finds a vendor's entry by its canonical name alone.
func vendorSpecByID(id string) (vendorSpec, bool) {
	for _, spec := range vendorSpecs {
		if spec.id == id {
			return spec, true
		}
	}
	return vendorSpec{}, false
}

// ControlDevices are the device nodes the vendor's kernel driver creates.
func ControlDevices(vendor string) []string {
	spec, _ := vendorSpecOf(vendor)
	return append([]string(nil), spec.controlDevices...)
}

// DetectCommand is the vendor's tool when finding it installed says the
// host is this vendor's; empty when its tool's name is not its alone.
func DetectCommand(vendor string) string {
	spec, _ := vendorSpecOf(vendor)
	return spec.detectCommand
}

// PCIModel is the model name of one PCI device id, with the hardware the
// name was read off. The name is written as the platform shows the card
// when its tool does answer, so one card never reads two ways.
type PCIModel struct {
	Vendor, Device uint32
	Name           string
	Provenance     string
}

// PCIModels lists the model names the vendors' definitions carry for cards
// upstream pci.ids does not name.
func PCIModels() []PCIModel {
	var models []PCIModel
	for _, spec := range vendorSpecs {
		for _, model := range spec.pciModels {
			model.Vendor = spec.pciID
			models = append(models, model)
		}
	}
	return models
}
