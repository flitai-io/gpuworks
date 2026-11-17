package deploy

import "strings"

// vLLM v0.14.0 introduced --max-model-len auto (upstream PR #29431).
// Only a known supporting image gets this default. Explicit configuration
// remains the deployer's choice, as with other version-dependent parameters.
func vllmAcceptsContextAuto(version string) bool {
	// Metadata may say nightly-20261116 rather than name a release. The loose
	// comparator treats its date as a version component, so recognize the
	// release first instead of mistaking an unknown build for a newer one.
	match := imageTagLeadVersion.FindStringSubmatch(strings.ToLower(strings.TrimSpace(version)))
	return match != nil && compareVersion(match[1], "0.14.0") >= 0
}

// The existing integer carries the native -1 sentinel without a second
// configuration field. SGLang and every other numeric parameter retain their
// nonnegative-value contract.
func isVLLMContextAuto(spec engineParam, value float64) bool {
	return spec.Key == "max_model_len" && spec.Flag == "--max-model-len" && value == -1
}
