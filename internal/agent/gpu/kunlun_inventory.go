package gpu

import (
	"math"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// A damaged row never erases its siblings. Duplicate identities mark all
// ambiguous rows, rather than trusting whichever one happened to arrive first.
// Missing rows are supplied by the shared PCI Roster; attached_xpus is only a
// count and cannot be used to invent a device index or identity.
func markKunlunCards(inv *kunlunInventory) {
	minors, ids, bdfs := map[string]int{}, map[string]int{}, map[string]int{}
	for i := range inv.Cards {
		c := &inv.Cards[i]
		c.Minor, c.UUID, c.BDF = strings.TrimSpace(c.Minor), strings.TrimSpace(c.UUID), strings.TrimSpace(c.BDF)
		if kunlunBDF.MatchString(c.BDF) {
			c.BDF = accelerator.NormalizeBDF(c.BDF)
		}
		if n, err := strconv.Atoi(c.Minor); err == nil {
			c.Minor = strconv.Itoa(n)
		}
		minors[c.Minor]++
		ids[c.UUID]++
		bdfs[c.BDF]++
	}
	for i := range inv.Cards {
		c := &inv.Cards[i]
		n, err := strconv.Atoi(c.Minor)
		mem, memOK := kunlunValue(c.Total, "MiB")
		var issues []string
		if err != nil || n < 0 || n >= 255 || minors[c.Minor] != 1 {
			issues = append(issues, "minor_number")
			c.Minor = "-1"
		}
		if c.UUID == "" || ids[c.UUID] != 1 {
			issues = append(issues, "uuid")
			c.UUID = ""
		}
		if !kunlunBDF.MatchString(c.BDF) || bdfs[c.BDF] != 1 {
			issues = append(issues, "pci_bus_id")
			c.BDF = ""
		}
		if c.Name == "" || !strings.EqualFold(c.Brand, "KUNLUNXIN") {
			issues = append(issues, "product_identity")
		}
		if !memOK || mem <= 0 || mem > float64(math.MaxInt64/(1<<20)) {
			issues = append(issues, "fb_memory_usage.total")
			c.Total = ""
		}
		c.invalid = strings.Join(issues, ",")
	}
}

func kunlunMemoryBytes(raw string) int64 {
	value, ok := kunlunValue(raw, "MiB")
	if !ok || value > float64(math.MaxInt64/(1<<20)) {
		return 0
	}
	return int64(value * (1 << 20))
}
