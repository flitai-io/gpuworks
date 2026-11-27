package planning

import (
	"math"
	"slices"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const (
	qwen3DGPUModel    = "qwen/qwen3-0.6b"
	qwen3DGPUImage    = "docker.io/gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r2"
	qwen3DGPUQuotaGiB = 8
	// On a 910B4 running Qwen3-0.6B BF16 with this image and an 8 GiB
	// dGPU quota, Decode graph capture exceeded the quota at 0.90 and
	// completed at 0.88. The graph memory budget is unverified at other quotas.
	qwen3DGPUStaticFractionMax = 0.88
)

func dgpuStartupMemoryWarning(req LaunchRequest, plan LaunchPlan) *messages.Warning {
	p := plan.Params
	if req.Engine != deploy.EngineSGLang || req.PD != nil || p.NNodes > 1 ||
		p.PartitionBackend != accelerator.BackendAscendLogical || p.VRAMLimitBytes != qwen3DGPUQuotaGiB<<30 ||
		p.Vendor != accelerator.VendorAscend || p.ArchID != "910B4" ||
		p.AssignedGPUs != 1 || p.TensorParallel != 1 || p.PipelineParallel > 1 || p.DataParallel > 1 ||
		req.Selection.ModelID != qwen3DGPUModel || pullref.NormalizeRef(req.Source.Image) != qwen3DGPUImage {
		return nil
	}
	// An unpinned preview uses the first card when none can be allocated.
	// Its architecture only describes the node if every owned card agrees.
	if !plan.Output.Fits() && len(req.Placement.GPUIndexes) == 0 {
		for _, gpu := range req.Node.OwnedGPUs() {
			if req.Node.ArchOf(gpu).ID != p.ArchID {
				return nil
			}
		}
	}
	args := deploy.LaunchEngineArgs(req.Engine, plan.Resolved)
	dtype, _ := deploy.LaunchFlag(args, "--dtype")
	if (dtype != "bfloat16" && dtype != "bf16") || slices.Contains(args, "--disable-cuda-graph") {
		return nil
	}
	raw, _ := deploy.LaunchFlag(args, "--mem-fraction-static")
	fraction, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(fraction, 1) || !(fraction > qwen3DGPUStaticFractionMax) {
		return nil
	}
	warning := messages.NewWarning(messages.WarningDGPUStartupMemory, map[string]any{
		"quota_gib": qwen3DGPUQuotaGiB, "utilization": fraction, "max_utilization": qwen3DGPUStaticFractionMax,
	})
	return &warning
}
