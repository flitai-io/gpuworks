package deploy

// vllm-omni takes over `vllm serve` only when the command line carries --omni
// (vllm_omni/entrypoints/cli/main.py); without it the same image runs plain
// vLLM. The adapter reads that switch, never the image name: an omni image
// serves ordinary models too.
func isVLLMOmniLaunch(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--omni" {
			return true
		}
	}
	return false
}

// DefaultApplicable leaves the GPU memory share of an omni launch to
// vllm-omni. A vllm-omni pipeline sizes each stage from its deploy config
// (Fish Speech: 0.6 for the talker, 0.1 for the codec), and a command-line
// --gpu-memory-utilization replaces every stage's share with the one figure,
// so the first stage takes the card and the next starts on whatever is left.
// Admission still charges the dictionary default for the unstated share
// (planning reads DefaultGPUMemUtil), as it does for any other launch that
// states none. The per-request metrics switch gets its default only where the
// release takes its flag (vllmAcceptsRequestMetrics).
func (vLLMEngine) DefaultApplicable(key string, launch LaunchShape) bool {
	arguments := append(append([]string(nil), launch.Entrypoint...), launch.Args...)
	switch key {
	case "max_model_len":
		return vllmAcceptsContextAuto(launch.EngineVersion) && !isVLLMOmniLaunch(arguments)
	case "gpu_memory_utilization":
		return !isVLLMOmniLaunch(arguments)
	case "enable_request_metrics":
		return vllmAcceptsRequestMetrics(launch.EngineVersion, arguments)
	}
	return true
}

// omniParallelFlags are the vllm-omni diffusion degrees that take GPUs of
// their own beside tensor and pipeline parallelism: the world is
// tp × pp × ulysses × ring × cfg (DiffusionParallelConfig). HSDP and VAE patch
// parallelism shard work across those same ranks and take none.
var omniParallelFlags = [][]string{
	{"--usp", "--ulysses-degree"},
	{"--ring", "--ring-degree"},
	{"--cfg-parallel-size"},
}

// omniParallelDegree multiplies the diffusion degrees an omni launch states.
// symbolic reports a degree still waiting for a placement variable, which
// leaves the world for the expanded launch to judge.
func omniParallelDegree(arguments []string) (degree int, symbolic bool, err error) {
	degree = 1
	if !isVLLMOmniLaunch(arguments) {
		return degree, false, nil
	}
	for _, names := range omniParallelFlags {
		value, set, valueSymbolic, err := topologyInteger(arguments, names...)
		if err != nil {
			return 0, false, err
		}
		if valueSymbolic {
			return 0, true, nil
		}
		if set {
			degree *= value
		}
	}
	return degree, false, nil
}
