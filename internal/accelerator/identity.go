package accelerator

import (
	"strconv"
	"strings"
)

// gpuNameVendorPrefixes are the brand words probes put in front of a model
// name. NVIDIA drivers add "NVIDIA " on some releases and not on others, and
// the Ascend probe has always reported "Ascend <chip>", and macOS names the
// GPU "Apple M4 Pro"; none of these words says anything the vendor field does
// not already say.
var gpuNameVendorPrefixes = []string{"nvidia", "huawei", "ascend", "hygon", "amd", "apple", "kunlunxin"}

// NormalizeGPUName is the model name of a GPU as the server stores it: the
// probe's name with surrounding and repeated whitespace collapsed and a
// leading vendor brand word removed ("NVIDIA GeForce RTX 4090" and
// "GeForce RTX 4090" are both "GeForce RTX 4090"; "Ascend 910B4" is
// "910B4"). Only a brand word followed by whitespace is removed, so a name
// that is nothing but the brand, or glues it to the model ("Ascend910"), is
// kept as reported.
//
// It is the one place a GPU name is normalized: GPUs are stored through it,
// so every reader (catalog name predicates, PD model checks, system labels,
// the GPU list) sees the same value. Raw names stay on the agent.
func NormalizeGPUName(name string) string {
	fields := strings.Fields(name)
	if len(fields) > 1 {
		for _, prefix := range gpuNameVendorPrefixes {
			if strings.EqualFold(fields[0], prefix) {
				fields = fields[1:]
				break
			}
		}
	}
	return strings.Join(fields, " ")
}

// Arch is the architecture identity of one GPU, derived from the facts a node
// reports. ID is the most specific identity (NVIDIA's sm_XY, Ascend's 910B4,
// Hygon's SKU); Family is the lowercase generation within the vendor
// (turing, 910b, k100), empty when the identity is not one this build knows.
type Arch struct {
	ID     string
	Family string
}

// GPUArch derives a GPU's architecture from what is stored for it: NVIDIA
// from its compute capability, every other vendor from its ArchID. It is the
// one place the family and NVIDIA's sm_XY are computed; neither is stored.
// vendor may be any spelling NormalizeVendor accepts; an empty vendor is
// inferred from the other two facts.
func GPUArch(vendor, archID, computeCapability string) Arch {
	archID = strings.TrimSpace(archID)
	computeCapability = strings.TrimSpace(computeCapability)
	switch InferVendor(vendor, "", archID, computeCapability) {
	case VendorNVIDIA:
		major, minor, ok := parseComputeCapability(computeCapability)
		if !ok {
			// A row with no capability but an sm_XY says the same thing.
			major, minor, ok = parseSMArch(archID)
		}
		if !ok {
			return Arch{ID: archID}
		}
		return Arch{ID: "sm_" + strconv.Itoa(major) + strconv.Itoa(minor), Family: nvidiaFamily(major, minor)}
	case VendorAscend:
		return Arch{ID: archID, Family: ascendFamily(archID)}
	case VendorHygon:
		return Arch{ID: archID, Family: hygonFamily(archID)}
	case VendorPPU:
		return Arch{ID: archID, Family: ppuFamily(archID)}
	case VendorKunlunxin:
		if strings.EqualFold(archID, "KL3") {
			return Arch{ID: archID, Family: "kl3"}
		}
		return Arch{ID: archID}
	case VendorApple:
		return Arch{ID: archID, Family: appleFamily(archID)}
	default:
		return Arch{ID: archID}
	}
}

// StoredArchID is the ArchID kept for a reported GPU. NVIDIA's sm_XY is a
// function of the compute capability, so it is not kept when the capability
// is; every other vendor's ArchID is the raw identity its probe found.
func StoredArchID(vendor, archID, computeCapability string) string {
	if NormalizeVendor(vendor) == VendorNVIDIA {
		if _, _, ok := parseComputeCapability(computeCapability); ok {
			return ""
		}
	}
	return strings.TrimSpace(archID)
}

func parseComputeCapability(cc string) (major, minor int, ok bool) {
	majorText, minorText, _ := strings.Cut(strings.TrimSpace(cc), ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major <= 0 {
		return 0, 0, false
	}
	if minorText == "" {
		return major, 0, true
	}
	minor, err = strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
}

// parseSMArch reads NVIDIA's sm_XY (sm_90, sm_120): the last digit is the
// minor version.
func parseSMArch(archID string) (major, minor int, ok bool) {
	digits, found := strings.CutPrefix(strings.ToLower(strings.TrimSpace(archID)), "sm_")
	if !found || len(digits) < 2 {
		return 0, 0, false
	}
	return parseComputeCapability(digits[:len(digits)-1] + "." + digits[len(digits)-1:])
}

// nvidiaFamily maps a compute capability to its generation
// (developer.nvidia.com/cuda/gpus).
func nvidiaFamily(major, minor int) string {
	switch major {
	case 5:
		return "maxwell"
	case 6:
		return "pascal"
	case 7:
		if minor == 5 {
			return "turing"
		}
		return "volta"
	case 8:
		if minor == 9 {
			return "ada"
		}
		return "ampere"
	case 9:
		return "hopper"
	case 10, 11, 12:
		return "blackwell"
	default:
		return ""
	}
}

// ascendFamily maps an Ascend chip identity (910B4, 910C, 310P3) to its
// generation. The first-generation 910 is not one: its chip name, Ascend910,
// is also what 910C reports, so only the probe's card layout tells them apart
// and an identity that still reads Ascend910 is left without a family.
func ascendFamily(archID string) string {
	id := strings.ToUpper(archID)
	switch {
	case strings.Contains(id, "910B"):
		return "910b"
	case strings.Contains(id, "910C"):
		return "910c"
	case strings.Contains(id, "310P"):
		return "310p"
	default:
		return ""
	}
}

// hygonFamily maps a Hygon DCU identity to its series. K100 is also what the
// probe stores when the card does not say which K100 it is.
func hygonFamily(archID string) string {
	id := strings.ToUpper(strings.NewReplacer("_", "", "-", "", " ", "").Replace(archID))
	switch {
	case strings.HasPrefix(id, "K100"):
		return "k100"
	case strings.HasPrefix(id, "Z100"):
		return "z100"
	case id == "BW":
		return "bw"
	case id == "BW1000":
		return "bw1000"
	case id == "BW1100" || id == "BW1101":
		return "bw1100"
	default:
		return ""
	}
}

// ppuFamily maps a PPU identity to the architecture ppu-smi reports for it
// (`ppu-smi --query-ppu=name,architecture` prints "PPU-ZW810E, Beethoven").
func ppuFamily(archID string) string {
	id := strings.ToUpper(archID)
	switch {
	case strings.Contains(id, "ZW810E"), id == "BEETHOVEN":
		return "beethoven"
	default:
		return ""
	}
}

// appleFamily is the chip generation of an Apple silicon SoC ("M4 Pro" is
// m4): every tier of one generation shares the GPU architecture and the
// Metal feature set, while the tier only changes the core count and memory.
func appleFamily(archID string) string {
	fields := strings.Fields(strings.ToLower(archID))
	if len(fields) == 0 {
		return ""
	}
	if gen := fields[0]; len(gen) >= 2 && gen[0] == 'm' && strings.Trim(gen[1:], "0123456789") == "" {
		return gen
	}
	return ""
}

// IsAppleFamily reports whether family is an Apple silicon generation as
// GPUArch spells it (m1, m4).
func IsAppleFamily(family string) bool {
	family = strings.ToLower(strings.TrimSpace(family))
	return family != "" && appleFamily(family) == family
}

// ArchBrand writes an architecture family the way the vendor writes it:
// a generation named after someone is a word (turing -> Turing, beethoven ->
// Beethoven), and a part number is upper case (910b -> 910B, bw1100 ->
// BW1100). It is a spelling rule rather than a table, so a family added to
// the functions above needs nothing here. The result is an English fallback;
// these names are proper nouns and are not translated.
func ArchBrand(family string) string {
	family = strings.TrimSpace(family)
	if family == "" {
		return ""
	}
	if strings.ContainsFunc(family, func(r rune) bool { return r >= '0' && r <= '9' }) {
		return strings.ToUpper(family)
	}
	return strings.ToUpper(family[:1]) + family[1:]
}

// unifiedMemoryModels are the accelerator models whose memory is the host's
// coherent pool rather than dedicated device memory: the GPU addresses the
// same bytes the CPU does, so the host OS and every CPU-side process compete
// for the same ceiling the engine allocates from.
//
// The list is deliberately explicit rather than a pattern. A model name is not
// a reliable signal of memory topology - "GB100" is a discrete card and would
// match any prefix rule written for GB10 - and the two consequences of getting
// it wrong are both bad: a discrete card wrongly called unified is handed the
// host's memory figure as its capacity, and a unified part wrongly called
// discrete is sized as if the host needed nothing.
//
// Apple silicon is listed by generation: the stored name drops the "Apple"
// brand word, so "Apple M4 Pro" is "M4 Pro" and its first token is the
// generation. A Mac's card is already sized net of the OS (the Metal working
// set, not host memory), and the server still keeps the unified share out of
// it on purpose: that working set is the ceiling for every GPU user on the
// Mac - the window server and the owner's own applications - not the engine's
// alone.
var unifiedMemoryModels = []string{"GB10", "M1", "M2", "M3", "M4", "M5", "M6"}

// UnifiedMemory reports whether this GPU model addresses the host's memory
// pool instead of its own device memory. name may be raw, as a probe reports
// it ("NVIDIA GB10"), or normalized, as the server stores it ("GB10"); the
// model token must match whole, so GB100 is not GB10.
//
// It is the one place this is decided. The agent reads it to know that a card
// reporting no memory.total of its own should be sized from host MemTotal, and
// the server reads it to know how much of such a card must stay outside an
// engine's share - two questions about the same fact, which is why neither
// side carries a rule of its own and nothing about it is persisted.
func UnifiedMemory(name string) bool {
	fields := strings.Fields(NormalizeGPUName(name))
	if len(fields) == 0 {
		return false
	}
	for _, model := range unifiedMemoryModels {
		if strings.EqualFold(fields[0], model) {
			return true
		}
	}
	return false
}

// NormalizeBDF is a PCI address as this repository compares it: lower case,
// trimmed, and with the domain written in four digits when it fits, so the
// tools that print four domain digits and those that print eight name the
// same function ("00000000:7E:00.0" and "0000:7e:00.0" both become
// "0000:7e:00.0").
//
// Only leading zeros are dropped. A domain that needs more than four digits
// — VMD and some hypervisors number theirs from 0x10000 — keeps them:
// cutting the address to its last twelve characters gave a card in domain
// 10000 the identity of the card at the same bus position in domain 0.
func NormalizeBDF(bdf string) string {
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	domain, rest, ok := strings.Cut(bdf, ":")
	if !ok || !strings.Contains(rest, ":") {
		return bdf // no domain to normalize ("7e:00.0")
	}
	for len(domain) > 4 && domain[0] == '0' {
		domain = domain[1:]
	}
	return domain + ":" + rest
}

// CardIdent is the hardware identity of one accelerator: what names the same
// physical card across probes, agent restarts and driver renumbering. It is
// what a lasting binding (an instance's cards) records, never the vendor
// CLI's device index, which only numbers the cards the driver could
// initialize and therefore shifts whenever one of them fails, is passed
// through to a VM, or is added.
//
// The PCI address wins over the UUID: on Ascend and Hygon the probe
// synthesizes the UUID from that very index ("ascend-3"), so a UUID-keyed
// binding would drift exactly when a card does. Where neither is reported
// (test fixtures, a prober that knows only indexes) the identity is empty and
// the caller keeps its index-based behavior.
func CardIdent(uuid, pcieBDF string) string {
	if bdf := NormalizeBDF(pcieBDF); bdf != "" {
		return "pci:" + bdf
	}
	if uuid = strings.TrimSpace(uuid); uuid != "" {
		return "uuid:" + uuid
	}
	return ""
}
