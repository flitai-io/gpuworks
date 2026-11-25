package gpu

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// FixedProber is an explicit test-only prober selected by the
// GPUWORKS_FAKE_GPU environment variable in the standalone agent. Keeping the
// implementation in the gpu package means L2 tests exercise the same conn and
// collector wiring as a real node while remaining independent of nvidia-smi.
type FixedProber struct {
	GPUs                                        []proto.GPUInfo
	VendorName, FrameworkName, FrameworkVersion string
}

// Synthetic marks this prober as describing test-only devices rather than
// hardware that should be validated through the host CUDA library.
func (*FixedProber) Synthetic() bool { return true }

// IsSynthetic reports whether p describes test-only devices. Questions about
// the host's own integration (its libcuda, its container plugin) say nothing
// about such cards, so callers skip them.
func IsSynthetic(p Prober) bool {
	synthetic, ok := p.(interface{ Synthetic() bool })
	return ok && synthetic.Synthetic()
}

func (p *FixedProber) Vendor() string  { return p.VendorName }
func (p *FixedProber) Available() bool { return true }
func (p *FixedProber) Framework(context.Context) (string, string) {
	return p.FrameworkName, p.FrameworkVersion
}
func (p *FixedProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return append([]proto.GPUInfo(nil), p.GPUs...), nil
}
func (p *FixedProber) DriverVersion(context.Context) string      { return "fake-999.00" }
func (p *FixedProber) Topology(context.Context) string           { return "" }
func (p *FixedProber) Metrics(context.Context) ([]Metric, error) { return nil, nil }
func (p *FixedProber) RuntimeInjection(indexes []int) RuntimeInjection {
	if os.Getenv("GPUWORKS_FAKE_GPU_NO_INJECT") == "1" {
		return RuntimeInjection{}
	}
	visible := make([]string, len(indexes))
	compact := make([]string, len(indexes))
	for i, index := range indexes {
		visible[i], compact[i] = strconv.Itoa(index), strconv.Itoa(i)
	}
	switch strings.ToLower(strings.TrimSpace(p.VendorName)) {
	case "apple":
		// A Mac's engines are host processes; its prober grants nothing.
		return RuntimeInjection{}
	case "ascend", "huawei":
		return RuntimeInjection{Env: map[string]string{"ASCEND_VISIBLE_DEVICES": strings.Join(visible, ","), "ASCEND_RT_VISIBLE_DEVICES": strings.Join(compact, ","), "NPU_VISIBLE_DEVICES": strings.Join(compact, ",")}, Grant: runtime.DeviceGrant{RuntimeClass: "ascend"}}
	case "ppu", "t-head", "thead":
		devices := []runtime.Device{{Path: "/dev/alixpu", Access: runtime.DeviceAccessAll}, {Path: "/dev/alixpu_ctl", Access: runtime.DeviceAccessAll}}
		for _, index := range indexes {
			devices = append(devices, runtime.Device{Path: fmt.Sprintf("/dev/alixpu_ppu%d", index), Access: runtime.DeviceAccessAll})
		}
		return RuntimeInjection{Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void", "CUDA_VISIBLE_DEVICES": strings.Join(compact, ",")}, Grant: runtime.DeviceGrant{Devices: devices}}
	default:
		return (&SMIProber{}).RuntimeInjection(indexes)
	}
}

// FromEnv parses GPUWORKS_FAKE_GPU=<model>x<count>[,<vram_mb>]. For example,
// "A100x4" creates four 80 GiB cards, while "A10x2,24576" creates two 24 GiB
// cards. An empty value returns nil so production behavior is unchanged.
func FromEnv(value string) (*FixedProber, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	base := strings.TrimSpace(parts[0])
	idx := strings.LastIndex(strings.ToLower(base), "x")
	if idx <= 0 || idx == len(base)-1 {
		return nil, fmt.Errorf("GPUWORKS_FAKE_GPU must be MODELxCOUNT, got %q", value)
	}
	model := base[:idx]
	count, err := strconv.Atoi(base[idx+1:])
	if err != nil || count <= 0 || count > 64 {
		return nil, fmt.Errorf("invalid fake GPU count in %q", value)
	}
	vramMB := int64(81920)
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		vramMB, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || vramMB <= 0 {
			return nil, fmt.Errorf("invalid fake GPU VRAM in %q", value)
		}
	}
	vendor := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_GPU_VENDOR"))
	if vendor == "" {
		vendor = accelerator.VendorNVIDIA
	}
	archID := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_GPU_ARCH"))
	// NVIDIA spec predicates match on compute capability, so a fake NVIDIA card
	// must report one or every cc-gated spec would look incompatible. The arch
	// id already carries it for this vendor; an explicit override stays
	// available for cards whose arch id is not a cc.
	computeCapability := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_GPU_CC"))
	if computeCapability == "" && accelerator.NormalizeVendor(vendor) == accelerator.VendorNVIDIA {
		computeCapability = archID
	}
	gpus := make([]proto.GPUInfo, count)
	for i := range gpus {
		gpus[i] = proto.GPUInfo{Index: i, UUID: fmt.Sprintf("GPU-FAKE-%d", i), Name: model, MemoryTotalBytes: vramMB << 20, Vendor: vendor, ArchID: archID, ComputeCapability: computeCapability, Healthy: true}
	}
	return &FixedProber{GPUs: gpus, VendorName: vendor, FrameworkName: strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_GPU_FRAMEWORK")), FrameworkVersion: strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_GPU_FRAMEWORK_VERSION"))}, nil
}
