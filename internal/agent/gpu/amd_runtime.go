package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (p *AMDProber) RuntimeInjection(indexes []int) RuntimeInjection {
	return p.RuntimeInjectionForDriver(indexes, "docker")
}

// The render-device cgroup is the enforceable selection boundary. DRM card
// indexes are not HIP ordinals, and amdgpu unique_id is not a HIP UUID, so this
// injector deliberately does not guess a userspace visibility selector.
// https://rocm.docs.amd.com/projects/install-on-linux/en/latest/how-to/docker.html
type amdDeviceNumber struct{ major, minor uint32 }

func parseAMDDeviceNumber(raw string) (amdDeviceNumber, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return amdDeviceNumber{}, fmt.Errorf("invalid device number %q", raw)
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return amdDeviceNumber{}, fmt.Errorf("invalid device major %q: %w", parts[0], err)
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return amdDeviceNumber{}, fmt.Errorf("invalid device minor %q: %w", parts[1], err)
	}
	return amdDeviceNumber{major: uint32(major), minor: uint32(minor)}, nil
}

func amdLinuxRdev(rdev uint64) amdDeviceNumber {
	return amdDeviceNumber{
		major: uint32((rdev>>8)&0xfff | (rdev>>32)&0xfffff000),
		minor: uint32(rdev&0xff | (rdev>>12)&0xffffff00),
	}
}

func (p *AMDProber) requireCharacterDevice(path string, expected amdDeviceNumber) error {
	info, err := p.inspect(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("not a character device")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect character device number")
	}
	actual := amdLinuxRdev(uint64(stat.Rdev))
	if actual != expected {
		return fmt.Errorf("device number mismatch: got %d:%d, sysfs reports %d:%d", actual.major, actual.minor, expected.major, expected.minor)
	}
	return nil
}

func (p *AMDProber) kfdDeviceError() error {
	raw, err := p.read("/sys/class/kfd/kfd/dev")
	if err != nil {
		return fmt.Errorf("KFD sysfs identity unavailable: %w", err)
	}
	expected, err := parseAMDDeviceNumber(string(raw))
	if err != nil {
		return fmt.Errorf("KFD sysfs identity unavailable: %w", err)
	}
	if err := p.requireCharacterDevice("/dev/kfd", expected); err != nil {
		return fmt.Errorf("/dev/kfd unavailable: %w", err)
	}
	return nil
}

func (p *AMDProber) boundedKFDDeviceError(ctx context.Context) error {
	_, err := boundedSysfsRead(ctx, &p.collectRead, func() (struct{}, error) { return struct{}{}, p.kfdDeviceError() })
	return err
}

func (p *AMDProber) renderDeviceError(devicePath string) error {
	renders, err := p.match(filepath.Join(devicePath, "drm/renderD*"))
	if err != nil {
		return fmt.Errorf("render enumeration: %w", err)
	}
	if len(renders) != 1 {
		return fmt.Errorf("requires exactly one render node, found %d", len(renders))
	}
	name := filepath.Base(renders[0])
	minor, err := strconv.ParseUint(strings.TrimPrefix(name, "renderD"), 10, 32)
	if err != nil || !strings.HasPrefix(name, "renderD") {
		return fmt.Errorf("invalid render node %q", name)
	}
	raw, err := p.read(filepath.Join(renders[0], "dev"))
	if err != nil {
		return fmt.Errorf("render sysfs identity: %w", err)
	}
	dev, err := parseAMDDeviceNumber(string(raw))
	if err != nil || dev.major != 226 || dev.minor != uint32(minor) {
		return fmt.Errorf("render sysfs identity mismatch")
	}
	if err := p.requireCharacterDevice(filepath.Join("/dev/dri", name), dev); err != nil {
		return fmt.Errorf("render device %s: %w", name, err)
	}
	return nil
}

func (p *AMDProber) dockerDevicePathsUnset(inventory []proto.GPUInfo) []string {
	paths := map[string]bool{"/dev/dri": true, "/dev/dri/by-path": true, "/dev/kfd": true}
	// by-path entries are symlinks to DRM card/render nodes. Removing the
	// directory mount alone does not remove an individual --device mapping to
	// one of those aliases, so include the aliases themselves as managed paths.
	if aliases, err := p.matchDev("/dev/dri/by-path/*"); err == nil {
		for _, alias := range aliases {
			name := filepath.Base(alias)
			if name != "." && name != string(filepath.Separator) {
				paths[filepath.Join("/dev/dri/by-path", name)] = true
			}
		}
	}
	for _, card := range inventory {
		if card.Index < 0 || card.PCIeBDF == "" || accelerator.NormalizeVendor(card.Vendor) != accelerator.VendorAMD {
			continue
		}
		paths[filepath.Join("/dev/dri", fmt.Sprintf("card%d", card.Index))] = true
		renders, err := p.match(filepath.Join("/sys/bus/pci/devices", card.PCIeBDF, "drm/renderD*"))
		if err != nil {
			// Selected render nodes were already verified above. Failure to
			// enumerate an unselected sibling must not block that healthy card.
			continue
		}
		for _, render := range renders {
			name := filepath.Base(render)
			if strings.HasPrefix(name, "renderD") {
				paths[filepath.Join("/dev/dri", name)] = true
			}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func (p *AMDProber) RuntimeInjectionForDriver(indexes []int, driver string) RuntimeInjection {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	injection, err := boundedSysfsRead(ctx, &p.injectRead, func() (RuntimeInjection, error) { return p.runtimeInjectionForDriver(indexes, driver, ctx), nil })
	if err != nil {
		return RuntimeInjection{UnsupportedReason: "AMD: injection verification timed out"}
	}
	return injection
}

func (p *AMDProber) runtimeInjectionForDriver(indexes []int, driver string, ctx context.Context) RuntimeInjection {
	fail := func(detail string) RuntimeInjection { return RuntimeInjection{UnsupportedReason: "AMD: " + detail} }
	if len(indexes) == 0 {
		return fail("explicit nonempty GPU selection required")
	}
	host := p.runtimeFacts()
	host.Driver = driver
	if mismatch := accelerator.RuntimeMismatchParams(accelerator.VendorAMD, host); mismatch != nil {
		return fail(fmt.Sprintf("host runtime is unsupported: %v", mismatch))
	}
	if err := p.boundedKFDDeviceError(ctx); err != nil {
		return fail(err.Error())
	}
	p.mu.RLock()
	expected := p.lastSeen
	p.mu.RUnlock()
	inventory, err := p.inventory(ctx)
	if err != nil {
		return fail("cannot verify inventory: " + err.Error())
	}
	conflicts := markAMDDuplicateIdentities(inventory)
	byIndex := map[int]proto.GPUInfo{}
	for _, g := range inventory {
		byIndex[g.Index] = g
	}
	seen := map[int]bool{}
	renderSeen := map[string]bool{}
	args := []string{"--device", "/dev/kfd:/dev/kfd:rwm"}
	// runveil binds the host's whole /dev and /sys, so HIP enumerates every
	// card; ROCR_VISIBLE_DEVICES pins the selection by ROCm's stable GPU
	// UUID (GPU-<unique_id>) rather than by an ordinal that depends on KFD
	// enumeration order.
	var pinned []string
	for _, index := range indexes {
		if seen[index] {
			return fail(fmt.Sprintf("duplicate device %d", index))
		}
		seen[index] = true
		current, ok := byIndex[index]
		if !ok || current.PCIeBDF == "" || current.UUID == "" {
			return fail(fmt.Sprintf("device %d absent from complete inventory", index))
		}
		if conflicts[index] {
			return fail(fmt.Sprintf("device %d has an ambiguous PCI/UUID identity", index))
		}
		if !current.Healthy {
			return fail(fmt.Sprintf("device %d inventory is degraded", index))
		}
		old, ok := expected[index]
		if !ok {
			return fail(fmt.Sprintf("device %d has no published healthy inventory identity", index))
		}
		if old.PCIeBDF != current.PCIeBDF || old.UUID != current.UUID {
			return fail(fmt.Sprintf("device %d PCI or GPU identity changed", index))
		}
		if driver == "udocker" {
			id, ok := strings.CutPrefix(current.UUID, "amd-")
			if !ok || normalizeAMDUniqueID(id) == "" {
				return fail(fmt.Sprintf("device %d has no unique_id; runveil cannot pin it", index))
			}
			pinned = append(pinned, fmt.Sprintf("GPU-%016s", normalizeAMDUniqueID(id)))
		}
		paths, err := p.match(filepath.Join("/sys/bus/pci/devices", current.PCIeBDF, "drm/renderD*"))
		if err != nil {
			return fail(err.Error())
		}
		if len(paths) != 1 {
			return fail(fmt.Sprintf("PCI %s requires exactly one render node; partitioned/ambiguous devices are unsupported", current.PCIeBDF))
		}
		renderPath := paths[0]
		name := filepath.Base(renderPath)
		minor, err := strconv.ParseUint(strings.TrimPrefix(name, "renderD"), 10, 32)
		if err != nil || !strings.HasPrefix(name, "renderD") {
			return fail("invalid render node " + name)
		}
		rawDev, err := p.read(filepath.Join(renderPath, "dev"))
		if err != nil {
			return fail("cannot read render-node sysfs identity: " + err.Error())
		}
		expectedDev, err := parseAMDDeviceNumber(string(rawDev))
		if err != nil {
			return fail("invalid render-node sysfs identity: " + err.Error())
		}
		if expectedDev.minor != uint32(minor) {
			return fail(fmt.Sprintf("render node %s does not match sysfs device minor %d", name, minor))
		}
		path := filepath.Join("/dev/dri", name)
		if renderSeen[path] {
			return fail("multiple selected devices map to " + path)
		}
		renderSeen[path] = true
		if err := p.requireCharacterDevice(path, expectedDev); err != nil {
			return fail(path + " unavailable: " + err.Error())
		}
		args = append(args, "--device", path+":"+path+":rwm")
	}
	return RuntimeInjection{
		ManagedSelection: &RuntimeSelectionCleanup{
			Env:           []string{"NVIDIA_VISIBLE_DEVICES", "CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "GPU_DEVICE_ORDINAL"},
			DockerOptions: []string{"--gpus", "--runtime"},
			DevicePaths:   p.dockerDevicePathsUnset(inventory),
		},
		DockerArgs: args,
		Env:        amdPinnedEnv(pinned),
	}
}

func amdPinnedEnv(pinned []string) map[string]string {
	if len(pinned) == 0 {
		return nil
	}
	return map[string]string{"ROCR_VISIBLE_DEVICES": strings.Join(pinned, ",")}
}
