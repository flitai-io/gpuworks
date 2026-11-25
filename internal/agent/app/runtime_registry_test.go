package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The registry lists a driver; this package builds it. Every listed driver
// must be buildable here under the name it is listed by (kube is built from
// the pod's environment instead), and nothing may be built that is not
// listed, or a driver could be selected whose device view no backend can ask
// for.
func TestEveryRegisteredDriverIsBuiltUnderItsOwnName(t *testing.T) {
	builders := driverBuilders(t.TempDir())
	for _, info := range runtime.Drivers() {
		build, ok := builders[info.Name]
		if info.Name == runtime.DriverKube {
			if ok {
				t.Fatal("kube has a plain builder; it is built from the pod's environment")
			}
			continue
		}
		if !ok {
			t.Fatalf("registered driver %q cannot be built", info.Name)
		}
		if got := build().Name(); got != info.Name {
			t.Fatalf("the builder for %q builds a driver named %q", info.Name, got)
		}
	}
	for name := range builders {
		if _, ok := runtime.DriverNamed(name); !ok {
			t.Fatalf("driver %q is built but not registered", name)
		}
	}
}

// The runtimes a vendor can be declared on and the drivers the agent has
// are the same set of names. A driver added to one and not the other would
// be a runtime no vendor can list, or a listed runtime nothing can run.
func TestVendorRuntimesAreTheAgentsDrivers(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range accelerator.Runtimes() {
		declared[name] = true
	}
	for _, info := range runtime.Drivers() {
		if !declared[info.Name] {
			t.Errorf("driver %q cannot be listed by any vendor", info.Name)
		}
		delete(declared, info.Name)
	}
	for name := range declared {
		t.Errorf("vendors can list runtime %q, which the agent has no driver for", name)
	}
}
