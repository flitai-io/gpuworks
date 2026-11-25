package accelerator

import "strings"

// appleVendor is Apple silicon. It is read through sysctl and ioreg: there
// is no vendor tool to run, no PCI bus to find its one card on, and no
// per-card check.
var appleVendor = vendorSpec{
	id:           VendorApple,
	brand:        "Apple",
	frameworks:   []string{"metal"},
	named:        either(exactly("apple", "metal"), func(v string) bool { return strings.HasPrefix(v, "apple ") }),
	precedence:   70,
	namePrefixes: []string{"apple"},

	arch: func(archID, _ string) Arch { return Arch{ID: archID, Family: appleFamily(archID)} },

	// Engines run as host processes from macOS environment packages. No
	// feature is listed: only ordinary shared-memory inference runs on a Mac.
	runtimes:        []string{RuntimeHost},
	ownCatalogImage: true,
	sharedByMemory:  true,
}
