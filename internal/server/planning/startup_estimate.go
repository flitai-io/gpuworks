package planning

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// Loading and engine initialization are distinct costs. These offline reference
// allowances aggregate bootstrap, tokenizer, compilation, KV profiling and graph
// capture; they are neither measured stages nor portable paper coefficients.
// See the startup estimate in docs/how-to/troubleshooting.md for evidence and limits.
const (
	startupReferenceMiBPerSec      = 512
	startupVLLMInitializationSec   = 300
	startupSGLangInitializationSec = 240
	startupOmniInitializationSec   = 420
)

func launchStartupEstimate(req LaunchRequest, launch deploy.ResolvedLaunch) *proto.StartupEstimate {
	var weight int64
	if req.Model != nil {
		weight = req.Model.WeightBytes
	}
	if req.Selection.WeightBytes > 0 {
		weight = req.Selection.WeightBytes
	}
	out := &proto.StartupEstimate{WeightBytes: max(weight, 0), Method: "missing_weight"}
	// A single loader formula cannot describe a collective or multi-component
	// critical path. These launches still receive the coordinated health budget.
	if (req.Engine != deploy.EngineVLLM && req.Engine != deploy.EngineSGLang) || req.Topology.NNodes > 1 || req.Topology.Headless || req.PD != nil || len(req.Selection.Components) > 0 || launch.CommandMode != string(deploy.CommandEngine) {
		out.Method = "unsupported"
		return out
	}
	if weight <= 0 {
		return out
	}
	initialization := startupInitializationAllowance(req.Engine, launch)
	seconds := float64(weight)/(startupReferenceMiBPerSec*1024*1024) + float64(initialization)
	out.Method = "weight_reference"
	out.EstimatedSeconds = &seconds
	out.ReferenceMiBPerSec = startupReferenceMiBPerSec
	out.InitializationSeconds = initialization
	return out
}

func startupInitializationAllowance(engine string, launch deploy.ResolvedLaunch) int {
	if engine == deploy.EngineSGLang {
		return startupSGLangInitializationSec
	}
	// Inspect the resolved command, not stale template arguments. A mention of
	// --omni inside a model name, JSON value or opaque script is not the flag.
	args, scriptArgs, ok := deploy.LaunchArgs(launch)
	if ok && len(scriptArgs) == 0 && slices.Contains(args, "--omni") {
		return startupOmniInitializationSec
	}
	return startupVLLMInitializationSec
}
