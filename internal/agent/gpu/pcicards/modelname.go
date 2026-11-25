package pcicards

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Where a card's model name comes from when the driver cannot be asked.
//
// The order is deliberate and the driver is not in it: this package answers
// only from the vendors' definitions (accelerator.PCIModels) and the host's pci.ids, and a
// caller that can reach the driver overrides the name with what the driver
// says (it is the better name: "NVIDIA GeForce RTX 3090" against pci.ids'
// "GA102 [GeForce RTX 3090]"). Nothing is cached to disk — the name is
// looked up per scan and the driver wins whenever it answers, so there is no
// stored name to go stale.
//
//  1. accelerator.PCIModels: the few accelerators upstream pci.ids does not
//     have, named in their vendors' definitions.
//  2. The host's pci.ids, for everything upstream covers (NVIDIA above all).
//  3. Neither: the caller names the card by its ids (PCIDeviceLabel).

// pciIDsPaths are where distributions keep the PCI id database. A trimmed
// system or a container often has none, and then a card is named by its
// ids until the driver answers.
var pciIDsPaths = []string{
	"usr/share/hwdata/pci.ids",
	"usr/share/misc/pci.ids",
}

// ModelNames resolves the model name of each card from the domestic table
// and the host's pci.ids, keyed by PCI address. A card neither lists is absent from
// the result; the caller names it by its ids instead
// (accelerator.PCIDeviceLabel).
func (s Scanner) ModelNames(cards []Card) map[string]string {
	wanted := map[[2]uint32]bool{}
	for _, card := range cards {
		wanted[[2]uint32{card.VendorID, card.DeviceID}] = true
	}
	if len(wanted) == 0 {
		return nil
	}
	byID := s.lookupPCIIDs(wanted)
	if len(byID) == 0 {
		return nil
	}
	names := make(map[string]string, len(cards))
	for _, card := range cards {
		if name, ok := byID[[2]uint32{card.VendorID, card.DeviceID}]; ok {
			names[card.BDF] = name
		}
	}
	return names
}

// lookupPCIIDs scans pci.ids for the wanted (vendor, device) pairs.
//
// The file is a two-level list: a vendor line at column 0, then its devices
// indented by one tab. It runs to megabytes, so it is read as a stream and
// only inside the vendor sections actually wanted.
func (s Scanner) lookupPCIIDs(wanted map[[2]uint32]bool) map[[2]uint32]string {
	found := map[[2]uint32]string{}
	domestic := map[[2]uint32]string{}
	for _, model := range accelerator.PCIModels() {
		domestic[[2]uint32{model.Vendor, model.Device}] = model.Name
	}
	missing := map[[2]uint32]bool{}
	vendors := map[uint32]bool{}
	for pair := range wanted {
		if name, ok := domestic[pair]; ok {
			found[pair] = name
			continue
		}
		missing[pair] = true
		vendors[pair[0]] = true
	}
	if len(missing) == 0 {
		return found
	}
	// Everything the vendors' definitions do not carry is the host's to name,
	// and a card that postdates both is named by its ids.
	for _, candidate := range pciIDsPaths {
		file, err := os.Open(s.path(candidate))
		if err != nil {
			continue
		}
		fromHost := scanPCIIDs(file, missing, vendors)
		_ = file.Close()
		if len(fromHost) > 0 {
			for pair, name := range fromHost {
				found[pair] = name
			}
			break
		}
	}
	return found
}

func scanPCIIDs(file *os.File, wanted map[[2]uint32]bool, vendors map[uint32]bool) map[[2]uint32]string {
	found := map[[2]uint32]string{}
	var inVendor uint32
	var insideWanted bool
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case !strings.HasPrefix(line, "\t"):
			// A vendor line: "10de  NVIDIA Corporation".
			id, _, ok := splitPCIIDsLine(line)
			if !ok {
				insideWanted = false
				continue
			}
			inVendor, insideWanted = id, vendors[id]
		case insideWanted && !strings.HasPrefix(line, "\t\t"):
			// A device line of a vendor we want: "\t2204  GA102 [...]".
			id, name, ok := splitPCIIDsLine(strings.TrimPrefix(line, "\t"))
			if !ok {
				continue
			}
			if pair := [2]uint32{inVendor, id}; wanted[pair] {
				found[pair] = accelerator.NormalizeGPUName(name)
				if len(found) == len(wanted) {
					return found
				}
			}
		}
		// "\t\t" lines are subsystem entries, which name a board variant
		// rather than the chip; the device name is the one we want.
	}
	return found
}

// splitPCIIDsLine splits "2204  GA102 [GeForce RTX 3090]" into its id and
// name. pci.ids separates them with two spaces.
func splitPCIIDsLine(line string) (id uint32, name string, ok bool) {
	hex, rest, found := strings.Cut(strings.TrimSpace(line), "  ")
	if !found {
		return 0, "", false
	}
	value, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, "", false
	}
	name = strings.TrimSpace(rest)
	if name == "" {
		return 0, "", false
	}
	return uint32(value), name, true
}
