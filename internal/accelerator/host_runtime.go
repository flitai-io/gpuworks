package accelerator

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// HostRuntime is observed host information, never inferred from the GPU SKU.
type HostRuntime struct{ OS, Arch, Driver string }

// The runtime drivers a vendor can list, by the names nodes report them.
const (
	RuntimeDocker  = proto.RuntimeDriverDocker
	RuntimeNerdctl = proto.RuntimeDriverNerdctl
	RuntimeUdocker = proto.RuntimeDriverUdocker
	RuntimeKube    = proto.RuntimeDriverKube
	RuntimeHost    = proto.RuntimeDriverHost
)

// NoRuntime is the runtime a refusal names as required for a vendor that
// lists none. It is the name an agent is given to run without a runtime.
const NoRuntime = "none"

// Runtimes lists every runtime driver a vendor can be declared on.
func Runtimes() []string {
	return []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube, RuntimeHost}
}

func (s vendorSpec) runsOn(driver string) bool {
	for _, own := range s.runtimes {
		if own == driver {
			return true
		}
	}
	return false
}

// RuntimeMismatch describes observed facts that fail a target's host contract.
type RuntimeMismatch struct {
	MissingFacts             bool
	Vendor                   string
	Actual                   HostRuntime
	RequiredOS, RequiredArch string
	RequiredDrivers          []string
}

func (m *RuntimeMismatch) Params() map[string]any {
	return map[string]any{"missing_facts": m.MissingFacts, "vendor": VendorBrand(m.Vendor), "os": m.Actual.OS, "arch": m.Actual.Arch, "runtime": m.Actual.Driver, "required_os": m.RequiredOS, "required_arch": m.RequiredArch, "required_runtime": strings.Join(m.RequiredDrivers, "/")}
}

// RuntimeMismatchFor checks what a node reports against what the vendor's
// definition declares: the runtime driver must be one it lists, and the
// operating system and architecture the ones it requires, if it requires
// any. A name that is no vendor's has no definition to check against.
func RuntimeMismatchFor(vendor string, actual HostRuntime) *RuntimeMismatch {
	spec, known := vendorSpecOf(vendor)
	if !known {
		return nil
	}
	return spec.runtimeMismatch(vendor, actual)
}

func (spec vendorSpec) runtimeMismatch(vendor string, actual HostRuntime) *RuntimeMismatch {
	if len(spec.runtimes) == 0 {
		// Its cards carry no workload, whatever the node reports.
		return &RuntimeMismatch{Vendor: vendor, Actual: actual, RequiredDrivers: []string{NoRuntime}}
	}
	hostOS, hostArch := strings.TrimSpace(actual.OS), strings.TrimSpace(actual.Arch)
	driver := strings.ToLower(strings.TrimSpace(actual.Driver))
	osOK := spec.hostOS == "" || strings.EqualFold(spec.hostOS, hostOS)
	archOK := spec.hostArch == "" || NormalizeArch(spec.hostArch) == NormalizeArch(hostArch)
	// A node that has reported no runtime is refused for it only by a vendor
	// that requires a particular host, which needs every fact to tell.
	driverOK := spec.runsOn(driver) || (driver == "" && spec.hostOS == "" && spec.hostArch == "")
	if osOK && archOK && driverOK {
		return nil
	}
	// A fact the node has not reported is missing, not wrong; the refusal
	// says so only when nothing it did report is wrong.
	wrong := (!osOK && hostOS != "") || (!archOK && hostArch != "") || (!driverOK && driver != "")
	return &RuntimeMismatch{MissingFacts: !wrong, Vendor: vendor, Actual: actual, RequiredOS: spec.hostOS, RequiredArch: spec.hostArch, RequiredDrivers: append([]string(nil), spec.runtimes...)}
}

// Requirement is what the vendor requires of the host as one phrase: the
// operating system and architecture when it restricts them, then the
// runtimes it lists.
func (m *RuntimeMismatch) Requirement() string {
	var parts []string
	if m.RequiredOS != "" {
		parts = append(parts, m.RequiredOS)
	}
	if m.RequiredArch != "" {
		parts = append(parts, m.RequiredArch)
	}
	host := strings.Join(parts, "/")
	runtimes := strings.Join(m.RequiredDrivers, "/")
	if host == "" {
		return runtimes
	}
	return host + " with " + runtimes
}

// RuntimeMismatchParams is the serialization adapter retained for consumers
// that expose the established refusal vocabulary.
func RuntimeMismatchParams(vendor string, actual HostRuntime) map[string]any {
	if m := RuntimeMismatchFor(vendor, actual); m != nil {
		return m.Params()
	}
	return nil
}
