package gpu

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

type amdMemorySource string

const (
	amdMemoryDedicatedVRAM amdMemorySource = "dedicated_vram"
	amdMemoryUMAPool       amdMemorySource = "uma_pool"
)

type amdMemoryFacts struct {
	totalBytes int64
	usedPath   string
	source     amdMemorySource
	diagnostic string
}

// memoryFacts uses dedicated VRAM by default. UMA capacity is enabled only for
// the AMD 0x1586/gfx1151 platform validated against HIP and the kernel's KFD,
// GTT, and TTM limits. It is deliberately not inferred from host RAM.
func (p *AMDProber) memoryFacts(devicePath, bdf, archID string, vramTotal int64) amdMemoryFacts {
	facts := amdMemoryFacts{totalBytes: vramTotal, usedPath: "mem_info_vram_used", source: amdMemoryDedicatedVRAM}
	if strings.TrimSpace(p.text(filepath.Join(devicePath, "device"))) != accelerator.AMDStrixHaloDeviceHex || archID != accelerator.AMDStrixHaloArch {
		return facts
	}
	if vramTotal <= 0 || bdf == "" {
		facts.diagnostic = "uma_pool_rejected=missing_vram_or_pci_identity"
		return facts
	}
	properties, reason := p.matchingKFDProperties(devicePath, bdf)
	if reason != "" {
		facts.diagnostic = "uma_pool_rejected=" + reason
		return facts
	}
	if properties["gfx_target_version"] != accelerator.AMDStrixHaloGFXTarget || amdGFXTarget(properties["gfx_target_version"]) != archID {
		facts.diagnostic = "uma_pool_rejected=kfd_architecture_mismatch"
		return facts
	}
	if _, ok := properties["local_mem_size"]; !ok {
		facts.diagnostic = "uma_pool_rejected=local_memory_fact_missing"
		return facts
	}
	if properties["local_mem_size"] != 0 {
		facts.diagnostic = "uma_pool_rejected=local_memory_nonzero"
		return facts
	}
	if _, ok := properties["mem_banks_count"]; !ok {
		facts.diagnostic = "uma_pool_rejected=memory_bank_count_missing"
		return facts
	}
	if properties["mem_banks_count"] != 1 {
		facts.diagnostic = "uma_pool_rejected=unexpected_memory_bank_count"
		return facts
	}
	node := properties["__node"]
	bankProps := filepath.Join("/sys/class/kfd/kfd/topology/nodes", strconv.FormatInt(node, 10), "mem_banks", "0", "properties")
	bank, err := parseAMDProperties(p.text(bankProps))
	if err != nil || !hasAMDProperties(bank, "heap_type", "size_in_bytes") || bank["heap_type"] != 1 || bank["size_in_bytes"] <= vramTotal {
		facts.diagnostic = "uma_pool_rejected=invalid_public_bank"
		return facts
	}
	gttTotal, err := p.number(filepath.Join(devicePath, "mem_info_gtt_total"))
	if err != nil || gttTotal <= vramTotal || gttTotal != bank["size_in_bytes"] {
		facts.diagnostic = "uma_pool_rejected=gtt_bank_capacity_mismatch"
		return facts
	}
	facts.totalBytes = gttTotal
	facts.usedPath = "mem_info_gtt_used"
	facts.source = amdMemoryUMAPool
	facts.diagnostic = "uma_pool=validated_kfd_public_gtt_pool"
	return facts
}

// matchingKFDProperties requires a unique KFD node for the exact PCI function
// and DRM render minor. KFD's location_id is pci_dev_id(): bus << 8 | devfn.
func (p *AMDProber) matchingKFDProperties(devicePath, bdf string) (map[string]int64, string) {
	domain, location, ok := amdBDFLocation(bdf)
	if !ok {
		return nil, "invalid_pci_identity"
	}
	renders, err := p.match(filepath.Join(devicePath, "drm/renderD*"))
	if err != nil {
		return nil, "render_mapping_unavailable"
	}
	var renderMinor int64 = -1
	for _, render := range renders {
		name := filepath.Base(render)
		minor, parseErr := strconv.ParseInt(strings.TrimPrefix(name, "renderD"), 10, 64)
		if !strings.HasPrefix(name, "renderD") || parseErr != nil || renderMinor >= 0 {
			return nil, "render_mapping_ambiguous"
		}
		renderMinor = minor
	}
	if renderMinor < 0 {
		return nil, "render_mapping_unavailable"
	}
	paths, err := p.match("/sys/class/kfd/kfd/topology/nodes/*/properties")
	if err != nil {
		return nil, "kfd_topology_unavailable"
	}
	var matched map[string]int64
	for _, path := range paths {
		props, parseErr := parseAMDProperties(p.text(path))
		if parseErr != nil || !hasAMDProperties(props, "vendor_id", "device_id", "domain", "location_id", "drm_render_minor", "gfx_target_version") {
			continue
		}
		if props["vendor_id"] != accelerator.AMDPCIVendorID || props["domain"] != domain || props["location_id"] != location || props["drm_render_minor"] != renderMinor {
			continue
		}
		if matched != nil {
			return nil, "kfd_mapping_ambiguous"
		}
		node, nodeErr := strconv.ParseInt(filepath.Base(filepath.Dir(path)), 10, 64)
		if nodeErr != nil || node < 0 {
			return nil, "kfd_node_id_invalid"
		}
		props["__node"] = node
		matched = props
	}
	if matched == nil {
		return nil, "kfd_mapping_unavailable"
	}
	deviceIDText := strings.TrimSpace(p.text(filepath.Join(devicePath, "device")))
	deviceID, err := strconv.ParseInt(strings.TrimPrefix(deviceIDText, "0x"), 16, 64)
	if err != nil || matched["device_id"] != deviceID {
		return nil, "kfd_device_id_mismatch"
	}
	return matched, ""
}

func hasAMDProperties(properties map[string]int64, keys ...string) bool {
	for _, key := range keys {
		if _, ok := properties[key]; !ok {
			return false
		}
	}
	return true
}

func parseAMDProperties(text string) (map[string]int64, error) {
	props := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Parse only the nonnegative identity/capacity facts we consume. KFD
		// also emits unsigned 64-bit unique_id/debug_prop values (%llu in
		// node_show); an unrelated value above MaxInt64 must not hide a card.
		switch fields[0] {
		case "vendor_id", "device_id", "domain", "location_id", "drm_render_minor", "gfx_target_version",
			"local_mem_size", "mem_banks_count", "heap_type", "size_in_bytes":
		default:
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid KFD property %q", fields[0])
		}
		if _, exists := props[fields[0]]; exists {
			return nil, fmt.Errorf("duplicate KFD property %q", fields[0])
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("invalid KFD property %s=%q", fields[0], fields[1])
		}
		props[fields[0]] = value
	}
	return props, nil
}

func amdBDFLocation(bdf string) (domain, location int64, ok bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(bdf)), ":")
	if len(parts) != 3 {
		return 0, 0, false
	}
	devfn := strings.Split(parts[2], ".")
	if len(devfn) != 2 {
		return 0, 0, false
	}
	d, e1 := strconv.ParseInt(parts[0], 16, 64)
	bus, e2 := strconv.ParseInt(parts[1], 16, 64)
	device, e3 := strconv.ParseInt(devfn[0], 16, 64)
	function, e4 := strconv.ParseInt(devfn[1], 16, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || d < 0 || d > 0xffff || bus < 0 || bus > 255 || device < 0 || device > 31 || function < 0 || function > 7 {
		return 0, 0, false
	}
	return d, bus<<8 | device<<3 | function, true
}
