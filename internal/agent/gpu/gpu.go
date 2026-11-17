// Package gpu detects accelerators on the local host: NVIDIA via nvidia-smi,
// plus Hygon/Ascend/PPU/Apple (see hygon.go/ascend.go/ppu.go/apple.go), all without NVML cgo
// (per plan). This file holds the vendor-agnostic Prober contract and the
// registry that picks the first backend whose CLI actually answers; the
// NVIDIA implementation (SMIProber and its parse helpers) lives in nvidia.go.
package gpu

import (
	"context"
	"errors"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcicards"
	"gpuworks.ai/gpuworks/internal/proto"
)

// RuntimeInjection describes how a runtime should expose selected devices.
type RuntimeInjection struct {
	// ManagedSelection names stale accelerator settings to replace. It is
	// independent of the atomic copy/apply contract used by every injector.
	ManagedSelection  *RuntimeSelectionCleanup
	Env               map[string]string
	DockerArgs        []string
	Mounts            []RuntimeMount
	UnsupportedReason string
	// Failure augments the legacy reason; old backends need not implement it.
	Failure *proto.RuntimeFailure
}

// RuntimeSelectionCleanup identifies only accelerator-owned workload settings.
type RuntimeSelectionCleanup struct {
	Env           []string
	DockerOptions []string
	DevicePaths   []string
}

// RuntimeMount is a host bind mount required for device visibility (for
// example Ascend's driver/DCMI libraries under nerdctl, which has no
// device-plugin runtime to inject them implicitly). Mirrors the shape of
// runtime.Mount without importing the runtime package from here.
type RuntimeMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// DriverAwareInjector is an optional Prober extension for accelerators whose
// device injection differs by container runtime driver — for example Ascend,
// where Docker has the Ascend-Docker-Runtime plugin (--runtime ascend +
// env-only device selection) but nerdctl/containerd has no equivalent plugin
// and needs raw --device passthrough plus explicit driver/CANN bind mounts
// instead (see docs/dev/ledgers/engineering-log.md "Ascend device injection for the nerdctl runtime
// driver"). Probers that don't implement this use RuntimeInjection for every
// driver.
type DriverAwareInjector interface {
	RuntimeInjectionForDriver(indexes []int, driverName string) RuntimeInjection
}

// TopologyAwareInjector extends the same device-injection seam for backends
// whose distributed transport needs extra devices. Its result includes the
// selected accelerators and transport together; failure exposes neither.
// The input slices are borrowed read-only for this call. The caller owns the
// returned resources; implementations must not retain or modify its inputs.
type TopologyAwareInjector interface {
	RuntimeInjectionForLaunch(ctx context.Context, req InjectionRequest) RuntimeInjection
}

// InjectionRequest is what the caller knows about the workload whose devices
// it is asking for. DriverName is the container runtime driver that will start
// it, NNodes its rank count, and DockerArgs the options already resolved for
// it (the compute network's, among others), which a backend may require but
// must not duplicate. Env and the other request fields are read-only for the
// prober.
type InjectionRequest struct {
	Indexes    []int
	DriverName string
	NNodes     int
	DockerArgs []string
	Env        map[string]string
}

// ResolveInjection asks a prober for its device injection through the most
// specific seam it implements. It is the one place the optional extensions
// above are dispatched, so a launch and the doctor that is supposed to
// predict it ask the same question of the same backend.
//
// Asking through a plainer seam than the backend implements means inventing
// the facts the seam leaves out: a caller that drops DriverName has to assume
// some container runtime, and a backend whose contract depends on it then
// answers for a host that may not be this one.
func ResolveInjection(ctx context.Context, p Prober, req InjectionRequest) RuntimeInjection {
	switch injector := p.(type) {
	case nil:
		return RuntimeInjection{}
	case TopologyAwareInjector:
		return injector.RuntimeInjectionForLaunch(ctx, req)
	case DriverAwareInjector:
		return injector.RuntimeInjectionForDriver(req.Indexes, req.DriverName)
	default:
		return injector.RuntimeInjection(req.Indexes)
	}
}

// AcceleratorPortProber is an optional Prober extension for accelerators
// whose cards carry their own network port (Ascend 910's RoCE port). The
// ports never appear in the host's interfaces, so the network inventory takes
// them from the prober.
type AcceleratorPortProber interface {
	AcceleratorPorts(ctx context.Context) []proto.AcceleratorPort
}

// AcceleratorPortPinger is an optional extension of AcceleratorPortProber:
// it pings an address from the port of one card, named as in the port
// inventory. A nil error means the address answered;
// ErrAcceleratorPingTimeout means nothing did within the limit;
// ErrAcceleratorNoCard means the node has no such card; any other error is
// the vendor tool's own failure. Pings on one card run in turn.
type AcceleratorPortPinger interface {
	PingAcceleratorPort(ctx context.Context, card, target string, timeout time.Duration) (rttMS float64, err error)
}

// AcceleratorPortProtocolPinger is the protocol-aware extension used by
// accelerator fabrics that have a vendor-specific reachability command. The
// legacy AcceleratorPortPinger remains the RoCE default for older probers.
type AcceleratorPortProtocolPinger interface {
	PingAcceleratorPortProtocol(ctx context.Context, card, target, protocol string, timeout time.Duration) (rttMS float64, err error)
}

var (
	// ErrAcceleratorPingTimeout reports that a ping got no answer in time.
	ErrAcceleratorPingTimeout = errors.New("no answer within the time limit")
	// ErrAcceleratorNoCard reports that the node has no card of that name.
	ErrAcceleratorNoCard = errors.New("the node reports no such card")
)

// ProbeLibrary is a host driver library needed only by the tiny behavioral
// probe image. Production inference images normally carry their user-space
// runtime, while the probe image deliberately contains only glibc and the
// assertion binary.
type ProbeLibrary struct {
	Source string
	Name   string
}

// Metric is one accelerator's normalized gpuworks_gpu_* metric set. Values keys
// are canonical metric names such as gpuworks_gpu_utilization.
type Metric struct {
	Index  int
	UUID   string
	Vendor string
	Values map[string]float64
}

// Prober detects GPUs on the local host.
type Prober interface {
	Vendor() string
	Available() bool
	Probe(ctx context.Context) ([]proto.GPUInfo, error)
	DriverVersion(ctx context.Context) string
	Topology(ctx context.Context) string
	Metrics(ctx context.Context) ([]Metric, error)
	RuntimeInjection(indexes []int) RuntimeInjection
}

// New returns the first accelerator backend whose CLI can actually query the
// driver. Looking up the binary is not sufficient: mixed-purpose hosts often
// retain a stale nvidia-smi while their accelerators use another vendor stack.
// Answering is not sufficient for nvidia-smi either, which is why NVIDIA is
// tried last: the PPU SDK ships a CUDA-compat nvidia-smi that answers with
// the PPU cards, and only the vendor's own CLI reports them as what they are.
// When none answers, fallbackProber names the host's vendor from weaker
// evidence, or reports that the host has no accelerator.
func New() Prober {
	probers := defaultProbers()
	bus, managed := pciBusVendors(busScanRoot())
	if p := firstAvailableWithBus(probers, managed); p != nil {
		return p
	}
	return fallbackProber(probers, deviceExists, commandExists, bus)
}

// busScanRoot is the root pciBusVendors scans the PCI bus from. A container
// cannot be trusted to list its own host's whole bus (pcicards.Scanner's
// hostOwnsThisBus guards against a rented pod with one of eight GPUs
// reporting the other seven as its own), so the zero value ("") makes
// Scanner read its own, container-scoped view and come back empty inside
// any container. That is wrong for this agent's own Kubernetes DaemonSet
// pod: it owns the whole node (hostPID, every vendor CLI already runs on
// the host once RunCLIsOnHost is called), so its PCI evidence should be the
// host's, the same root every other host-or-container lookup in this
// package already switches to (hostexec.go). Without this, a card whose
// driver is not ready yet (no device file, no CLI) is invisible to New()
// on Kubernetes and the node is reported as NoAccelerator instead of as its
// real vendor with a driver that is still loading.
func busScanRoot() string {
	if CLIsOnHost() {
		return hostRoot
	}
	return ""
}

// Prefer physical native evidence over an answering compatibility CLI.
// NewFor remains authoritative when an operator explicitly chooses a backend.
func firstAvailableWithBus(probers []Prober, onBus func(string) bool) Prober {
	for _, p := range probers {
		if p == nil || !p.Available() {
			continue
		}
		// A CUDA-compatible CLI can answer even when a native driver has failed.
		// Without a physical NVIDIA card, retain the earlier native PCI owner so
		// the failure remains attributable. An actual NVIDIA card is still a valid
		// fallback on a mixed host. This does not invent PCI facts in containers.
		if p.Vendor() == accelerator.VendorNVIDIA && !onBus(accelerator.VendorNVIDIA) {
			for _, native := range probers {
				if native != nil && native.Vendor() == p.Vendor() {
					break
				}
				if native != nil && onBus(native.Vendor()) {
					return native
				}
			}
		}
		if p.Vendor() == accelerator.VendorAMD {
			for _, other := range probers {
				if other != nil && other.Vendor() != accelerator.VendorAMD && onBus(other.Vendor()) {
					return other
				}
			}
		}
		return p
	}
	return nil
}

// pciBusVendors shares one scan between fallback evidence and managed choices.
func pciBusVendors(hostRoot string) (func(string) bool, func(string) bool) {
	var once sync.Once
	vendors, managed := map[string]bool{}, map[string]bool{}
	scan := func() {
		once.Do(func() {
			cards, _ := pcicards.Scanner{Root: hostRoot}.Scan()
			for _, card := range cards {
				vendors[card.Vendor] = true
				if card.Driver != pcicards.DriverVFIO {
					managed[card.Vendor] = true
				}
			}
		})
	}
	return func(vendor string) bool { scan(); return vendors[vendor] }, func(vendor string) bool { scan(); return managed[vendor] }
}

// vendorDeviceFiles are control devices a vendor's kernel driver creates.
// When no backend can query its driver, the one whose devices exist is the
// host's real vendor, and reporting it keeps the failure attributable (for
// example "npu-smi: dcmi module initialize failed" instead of "0 NVIDIA GPUs").
var vendorDeviceFiles = map[string][]string{
	accelerator.VendorKunlunxin: {"/dev/xpuctrl"},
	accelerator.VendorNVIDIA:    {"/dev/nvidiactl"},
	accelerator.VendorAscend:    {"/dev/davinci_manager"},
	accelerator.VendorPPU:       {"/dev/alixpu_ctl"},
}

// xpu-smi is deliberately absent: Intel uses that name too. Kunlun fallback
// needs its control device or PCI identity, not an executable name alone.
// vendorCommands are the CLIs each backend queries its driver with, in the
// order fallbackProber trusts them: NVIDIA's last, because a stale
// nvidia-smi is the one commonly left on hosts of other vendors.
var vendorCommands = []struct{ vendor, command string }{
	{accelerator.VendorAscend, "npu-smi"},
	{accelerator.VendorHygon, "hy-smi"},
	{accelerator.VendorPPU, "ppu-smi"},
	{accelerator.VendorNVIDIA, "nvidia-smi"},
}

// deviceExists checks the host's live /dev when CLIs run there: a
// privileged container's own /dev is a copy made at container start, so a
// device node created afterward (a driver still initializing) would
// otherwise be reported missing.
func deviceExists(path string) bool {
	_, err := statTool(path)
	return err == nil
}

func commandExists(name string) bool {
	_, err := lookCLI(name)
	return err == nil
}

// fallbackProber picks a backend when none could query its driver: the one
// whose device files exist, else the one whose CLI is installed, else the
// one whose cards are on the PCI bus, else none.
// A driver that is still loading (an Ascend host 40 s after boot answers
// npu-smi only after the agent has started, and creates its device files
// later still) is thereby still reported as its vendor, and its later probes
// find the cards. A host with no sign of any accelerator gets NoAccelerator,
// whose vendor is empty, rather than being reported as NVIDIA.
//
// The bus comes last because it is the weakest evidence of a usable stack
// and the strongest that there are cards: a host whose cards have no driver
// installed, or are all passed through to virtual machines, has neither
// device files nor a CLI, and reporting it as having no accelerator hid
// exactly the cards its operator needed to see. Every vendor's prober
// lists the bus's cards when its CLI is missing (Roster).
func fallbackProber(probers []Prober, exists, installed, onBus func(string) bool) Prober {
	for _, p := range probers {
		for _, path := range vendorDeviceFiles[p.Vendor()] {
			if exists(path) {
				return p
			}
		}
	}
	for _, c := range vendorCommands {
		if !installed(c.command) {
			continue
		}
		for _, p := range probers {
			if p.Vendor() == c.vendor {
				return p
			}
		}
	}
	for _, p := range probers {
		if onBus(p.Vendor()) {
			return p
		}
	}
	return NoAccelerator{}
}

func defaultProbers() []Prober {
	// Keep the existing automatic-probe order stable. AMD is an explicit
	// choice on mixed-vendor nodes; inserting it before NVIDIA would silently
	// change which vendor the single-prober agent reports in auto mode.
	return []Prober{&HygonProber{}, &AscendProber{}, &PPUProber{}, &KunlunProber{}, &SMIProber{}, &AppleProber{}, &AMDProber{}}
}

func firstAvailable(probers []Prober) Prober {
	for _, p := range probers {
		if p != nil && p.Available() {
			return p
		}
	}
	return nil
}
