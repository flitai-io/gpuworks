package gpu

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestFallbackProberFollowsTheDriverDevices(t *testing.T) {
	probers := defaultProbers()
	present := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	none := present()
	// An Ascend host whose npu-smi cannot initialize still reports Ascend.
	if got := fallbackProber(probers, present("/dev/davinci_manager"), none, none).Vendor(); got != accelerator.VendorAscend {
		t.Fatalf("ascend devices -> %s", got)
	}
	if got := fallbackProber(probers, present("/dev/nvidiactl"), none, none).Vendor(); got != accelerator.VendorNVIDIA {
		t.Fatalf("nvidia devices -> %s", got)
	}
	// A PPU host whose ppu-smi cannot answer still reports PPU, even with the
	// PPU SDK's CUDA-compat nvidia-smi installed beside it.
	if got := fallbackProber(probers, present("/dev/alixpu_ctl"), present("nvidia-smi"), none).Vendor(); got != accelerator.VendorPPU {
		t.Fatalf("ppu devices with nvidia-smi -> %s", got)
	}
	if got := fallbackProber(probers, present("/dev/xpuctrl"), present("nvidia-smi"), none).Vendor(); got != accelerator.VendorKunlunxin {
		t.Fatalf("Kunlun control device with compatible NVIDIA CLI -> %s", got)
	}
	if got := fallbackProber(probers, none, present("xpu-smi"), none).Vendor(); got != "" {
		t.Fatalf("ambiguous xpu-smi executable invented a vendor -> %s", got)
	}
	// Devices outrank an installed CLI of another vendor.
	if got := fallbackProber(probers, present("/dev/davinci_manager"), present("nvidia-smi"), none).Vendor(); got != accelerator.VendorAscend {
		t.Fatalf("ascend devices with nvidia-smi -> %s", got)
	}
	// 910c_k8s: the agent started while the Ascend driver was still loading.
	// npu-smi was installed but did not answer, and /dev/davinci_manager did
	// not exist yet; the node must still be an Ascend node, whose later
	// probes find the cards.
	if got := fallbackProber(probers, none, present("npu-smi"), none).Vendor(); got != accelerator.VendorAscend {
		t.Fatalf("npu-smi only -> %s", got)
	}
	// A stale nvidia-smi does not outrank another vendor's CLI.
	if got := fallbackProber(probers, none, present("nvidia-smi", "ppu-smi"), none).Vendor(); got != accelerator.VendorPPU {
		t.Fatalf("ppu-smi and nvidia-smi -> %s", got)
	}
	if got := fallbackProber(probers, none, present("hy-smi"), none).Vendor(); got != accelerator.VendorHygon {
		t.Fatalf("hy-smi only -> %s", got)
	}
	if got := fallbackProber(probers, none, present("nvidia-smi"), none).Vendor(); got != accelerator.VendorNVIDIA {
		t.Fatalf("nvidia-smi only -> %s", got)
	}
	// Cards on the bus and nothing else: no driver installed, or every card
	// passed through to a virtual machine. The vendor still owns the node,
	// and its prober lists the cards and says why none can be used.
	if got := fallbackProber(probers, none, none, present(accelerator.VendorHygon)).Vendor(); got != accelerator.VendorHygon {
		t.Fatalf("hygon cards on the bus only -> %s", got)
	}
	// A driver's own evidence still outranks the bus.
	if got := fallbackProber(probers, present("/dev/nvidiactl"), none, present(accelerator.VendorHygon)).Vendor(); got != accelerator.VendorNVIDIA {
		t.Fatalf("nvidia devices with hygon cards on the bus -> %s", got)
	}
	// No sign of any accelerator: no vendor, never NVIDIA by default.
	got := fallbackProber(probers, none, none, none)
	if _, ok := got.(NoAccelerator); !ok || got.Vendor() != "" {
		t.Fatalf("nothing -> %T vendor %q", got, got.Vendor())
	}
}

func TestNoAcceleratorReportsNothingAndRefusesDevices(t *testing.T) {
	p := NoAccelerator{}
	if p.Available() {
		t.Fatal("NoAccelerator is available")
	}
	gpus, err := p.Probe(t.Context())
	if err != nil || len(gpus) != 0 {
		t.Fatalf("Probe = %v, %v", gpus, err)
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 0 {
		t.Fatalf("Metrics = %v, %v", metrics, err)
	}
	if p.DriverVersion(t.Context()) != "" || p.Topology(t.Context()) != "" {
		t.Fatal("NoAccelerator reports a driver or topology")
	}
	if inj := p.RuntimeInjection(nil); inj.UnsupportedReason != "" || len(inj.Env) != 0 || len(inj.DockerArgs) != 0 {
		t.Fatalf("injection for no devices = %+v", inj)
	}
	if inj := p.RuntimeInjection([]int{0}); inj.UnsupportedReason != noAcceleratorReason || len(inj.DockerArgs) != 0 {
		t.Fatalf("injection for a device = %+v", inj)
	}
}
