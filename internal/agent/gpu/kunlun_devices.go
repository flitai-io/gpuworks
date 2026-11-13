package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Runtime injection verifies the native device identities before granting the
// scheduled devices to the container. Probe and telemetry stay in kunlun.go;
// the optional multi-node transport extension stays in kunlun_runtime.go.
// A caller that does not say which container runtime will start the workload
// gets no assumption made for it: Kunlun's contract is driver-specific, so
// naming one here would authorize a host this backend has not been verified
// on. gpu.ResolveInjection reaches the seams below instead.
func (p *KunlunProber) RuntimeInjection(indexes []int) RuntimeInjection {
	return p.RuntimeInjectionForDriver(indexes, "")
}
func (p *KunlunProber) RuntimeInjectionForDriver(indexes []int, driver string) RuntimeInjection {
	return p.runtimeInjectionForDriver(context.Background(), indexes, driver)
}
func (p *KunlunProber) runtimeInjectionForDriver(ctx context.Context, indexes []int, driver string) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	fail := func(s string) RuntimeInjection {
		return rejectedInjection(proto.RuntimeFailure{Code: proto.ErrorAcceleratorDeviceUnavailable, Vendor: p.Vendor()}, s)
	}
	if err := ctx.Err(); err != nil {
		return fail(err.Error())
	}
	actual := accelerator.HostRuntime{OS: runtime.GOOS, Arch: runtime.GOARCH, Driver: driver}
	if p.platform != nil {
		actual = p.platform()
		actual.Driver = driver
	}
	if err := RuntimeRequirementError(p.Vendor(), actual); err != nil {
		return rejectedInjection(err.Failure, err.Error())
	}

	inv, e := p.inventoryWithFreshness(ctx, true)
	if e != nil {
		return rejectedInjection(proto.RuntimeFailure{Code: proto.ErrorAcceleratorInventoryUnavailable, Vendor: p.Vendor()}, "cannot verify Kunlun inventory: "+e.Error())
	}
	cards := map[int]kunlunCard{}
	managedPaths := make([]string, 0, len(inv.Cards)+1)
	for _, c := range inv.Cards {
		if n, err := strconv.Atoi(c.Minor); err == nil && n >= 0 && n < 255 {
			managedPaths = append(managedPaths, "/dev/xpu"+strconv.Itoa(n))
		}
		if c.invalid != "" {
			continue
		}
		n, _ := strconv.Atoi(c.Minor)
		cards[n] = c
	}
	managedPaths = append(managedPaths, "/dev/xpuctrl")
	// Probe publishes a new immutable map. Retain one generation for all
	// selected cards without holding its publication lock during filesystem I/O.
	p.mu.RLock()
	previous := p.lastSeen
	p.mu.RUnlock()
	// Docker and nerdctl name runc so a node whose default runtime injects
	// another vendor's devices cannot add them. A pod has no such option: its
	// --runtime would become a RuntimeClass named "runc", which no cluster
	// defines, so the pod could not be created; the kubelet's default runtime
	// is already the plain one.
	var args []string
	if driver != "kube" {
		args = []string{"--runtime", "runc"}
	}
	logical := make([]string, 0, len(indexes))
	seen := map[int]bool{}
	for _, n := range indexes {
		c, ok := cards[n]
		if !ok || seen[n] {
			return fail("invalid or duplicate Kunlun device selection")
		}
		if !strings.EqualFold(strings.TrimSpace(c.Name), "P800 PCIe") || !strings.EqualFold(strings.TrimSpace(c.Arch), "KL3") {
			return fail("Kunlun device is not a supported P800 PCIe/KL3")
		}
		ecc, eccErr := strconv.ParseUint(strings.TrimSpace(c.ECC), 10, 64)
		if eccErr == nil && ecc > 0 {
			return fail("selected Kunlun card health is not ready")
		}
		seen[n] = true
		logical = append(logical, strconv.Itoa(len(logical)))
		old, had := previous[n]
		if !had {
			return fail("Kunlun injection requires a successful probe baseline")
		}
		if old.UUID != c.UUID || old.PCIeBDF != c.BDF {
			return fail("Kunlun device identity changed since probe")
		}
		node, err := p.verifyKunlunDevice(c.BDF, n)
		if err != nil {
			return fail(err.Error())
		}
		args = append(args, "--device", node+":"+node+":rwm")
	}
	if err := p.verifyKunlunControl(); err != nil {
		return fail(err.Error())
	}
	if err := ctx.Err(); err != nil {
		return fail(err.Error())
	}
	args = append(args, "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm")
	return RuntimeInjection{
		ManagedSelection: &RuntimeSelectionCleanup{Env: []string{"XPU_VISIBLE_DEVICES"}, DevicePaths: managedPaths},
		DockerArgs:       args,
		Env:              map[string]string{"XPU_VISIBLE_DEVICES": strings.Join(logical, ",")},
	}
}

// verifyKunlunDevice binds the inventory identity to the driver mapping and
// the actual character device. No container resources are published here.
func (p *KunlunProber) verifyKunlunDevice(bdf string, n int) (string, error) {
	// Confirm the XML minor is bound to this PCI device in the native driver.
	base := "/sys/bus/pci/devices/" + bdf
	vendor, e := p.read(base + "/vendor")
	if e != nil || !accelerator.SysfsVendorIs(p.Vendor(), string(vendor)) {
		return "", errors.New("Kunlun PCI vendor cannot be verified")
	}
	node := "/dev/xpu" + strconv.Itoa(n)
	dev, e := p.read(filepath.Join(base, "kunlun", filepath.Base(node), "dev"))
	if e != nil {
		return "", errors.New("Kunlun sysfs device mapping unavailable")
	}
	parts := strings.Split(strings.TrimSpace(string(dev)), ":")
	if len(parts) != 2 || parts[1] != strconv.Itoa(n) {
		return "", errors.New("Kunlun sysfs minor mismatch")
	}
	major, majorErr := strconv.Atoi(parts[0])
	if majorErr != nil || major <= 0 {
		return "", errors.New("Kunlun sysfs major invalid")
	}
	st, e := p.inspect(node)
	if e != nil || st.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("Kunlun character device unavailable: " + node)
	}
	if e := verifyKunlunRdev(st, major, n); e != nil {
		return "", e
	}
	return node, nil
}

func (p *KunlunProber) verifyKunlunControl() error {
	st, e := p.inspect("/dev/xpuctrl")
	if e != nil || st.Mode()&os.ModeCharDevice == 0 {
		return errors.New("Kunlun control device unavailable")
	}
	control, err := p.read("/sys/class/kunlun/xpuctrl/dev")
	if err != nil {
		return errors.New("Kunlun control sysfs mapping unavailable")
	}
	parts := strings.Split(strings.TrimSpace(string(control)), ":")
	if len(parts) != 2 || parts[1] != "255" {
		return errors.New("Kunlun control sysfs mapping invalid")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major <= 0 {
		return errors.New("Kunlun control sysfs major invalid")
	}
	if err = verifyKunlunRdev(st, major, 255); err != nil {
		return err
	}
	return nil
}

func verifyKunlunRdev(info os.FileInfo, major, minor int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect Kunlun character device identity")
	}
	dev := uint64(st.Rdev)
	actualMajor := ((dev >> 8) & 0xfff) | ((dev >> 32) & 0xfffff000)
	if actualMajor != uint64(major) || unixMinor(dev) != uint64(minor) {
		return fmt.Errorf("Kunlun character device major/minor mismatch")
	}
	return nil
}
