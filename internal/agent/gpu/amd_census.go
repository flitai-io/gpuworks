package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// PCI display identity alone is insufficient: Ryzen graphics without a KFD
// compute node must not offer a vendor switch that disables the working GPU.
// KFD may block during driver reset. Do not hold the entire vendor census
// waiting for AMD: omit its unverified candidate after this discovery budget.
const censusAMDReadTimeout = time.Second

func (c *VendorCensus) amdComputeCandidates(ctx context.Context) map[string]bool {
	candidates, err := boundedSysfsRead(ctx, &c.amdRead, func() (map[string]bool, error) {
		read := c.amdReadFile
		if read == nil {
			read = os.ReadFile
		}
		return amdComputePCIFunctions(ctx, c.HostRoot, read, filepath.Glob), nil
	})
	if err != nil {
		return nil
	}
	return candidates
}

func amdComputePCIFunctions(ctx context.Context, root string, read func(string) ([]byte, error), glob func(string) ([]string, error)) map[string]bool {
	// Empty HostRoot means the host filesystem, independently of agent cwd.
	if root == "" {
		root = "/"
	}
	paths, _ := glob(filepath.Join(root, "sys/class/kfd/kfd/topology/nodes/*/properties"))
	counts := map[string]int{}
	devices, _ := glob(filepath.Join(root, "sys/bus/pci/devices/*"))
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil
		}
		data, err := read(path)
		if err != nil {
			continue
		}
		props, err := parseAMDProperties(string(data))
		if err != nil || !hasAMDProperties(props, "vendor_id", "device_id", "domain", "location_id", "drm_render_minor", "gfx_target_version") || props["vendor_id"] != accelerator.AMDPCIVendorID || props["drm_render_minor"] < 128 || amdGFXTarget(props["gfx_target_version"]) == "" {
			continue
		}
		// Match against the PCI function rather than a KFD directory ordinal.
		for _, device := range devices {
			if ctx.Err() != nil {
				return nil
			}
			bdf := filepath.Base(device)
			domain, location, ok := amdBDFLocation(bdf)
			if !ok || domain != props["domain"] || location != props["location_id"] {
				continue
			}
			raw, err := read(filepath.Join(device, "device"))
			if err != nil || parsePCIHex(string(raw)) != props["device_id"] {
				continue
			}
			counts[bdf]++
		}
	}
	out := map[string]bool{}
	for bdf, n := range counts {
		out[bdf] = n == 1
	}
	return out
}

func parsePCIHex(value string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(value), "0x"), 16, 64)
	if err != nil {
		return -1
	}
	return n
}
