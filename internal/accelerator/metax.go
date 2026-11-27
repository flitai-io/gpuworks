package accelerator

import (
	"strconv"
	"strings"
)

// metaxVendor is MetaX MXC. The MXC550 PCI identity was observed as
// 9999:4000, class 0x0380, bound to the metax driver, in November 2026.
var metaxVendor = vendorSpec{
	id:         VendorMetaX,
	brand:      "MetaX",
	frameworks: []string{"maca", "cu-bridge"},
	named: func(v string) bool {
		return v == "cu-bridge" || v == "metax" || strings.HasPrefix(v, "metax ") || v == "maca" || strings.HasPrefix(v, "maca ") || strings.HasPrefix(v, "mxc")
	},
	precedence:   25,
	namePrefixes: []string{"metax"},
	domestic:     true,

	tool:           "mx-smi",
	detectCommand:  "mx-smi",
	controlDevices: []string{"/dev/mxcd"},
	pciID:          0x9999,
	pci:            &pciVendor{classes: []uint32{0x0380}, drivers: []string{"metax"}},
	pciModels:      []PCIModel{{Device: 0x4000, Name: "MXC550", Provenance: "mx-smi 3.100.0 on a MetaX host, November 2026: eight MXC550 cards; lspci reports 9999:4000"}},
	arch:           func(archID, _ string) Arch { return Arch{ID: archID, Family: metaxFamily(archID)} },

	ownCatalogImage: true,

	cardChecks: true,
	checks:     []string{CheckRuntime},
	healthProbe: func(target, _ string) string {
		if target == "" {
			return ""
		}
		return "mx-smi -L -i " + target
	},
	healthGuide: func(card HealthCard, _ string, _ []HealthField) (HealthGuide, bool) {
		return metaxHealthGuide(card.Code, card.Index)
	},
}

func metaxFamily(archID string) string {
	id := strings.ToUpper(strings.NewReplacer("_", "", "-", "", " ", "").Replace(archID))
	if strings.HasPrefix(id, "MXC") {
		return strings.ToLower(id[2:])
	}
	return ""
}

func metaxHealthGuide(code string, index int) (HealthGuide, bool) {
	if index < 0 {
		return HealthGuide{}, false
	}
	switch code {
	case HealthRuntimeUnusable:
		card := strconv.Itoa(index)
		return HealthGuide{Probe: "mx-smi -i " + card + " --show-unavailable-reason", Recheck: "mx-smi -L -i " + card}, true
	case HealthCLIUnanswered:
		return HealthGuide{Recheck: "mx-smi -L"}, true
	}
	return HealthGuide{}, false
}
