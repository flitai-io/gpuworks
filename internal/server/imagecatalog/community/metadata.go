package community

import (
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
)

var (
	engineTagPattern      = regexp.MustCompile(`(?:^|[-_])(vllm-omni|vllm|sglang|strata)[-_]?v?([0-9][0-9A-Za-z]*(?:\.[0-9A-Za-z]+)+)(?:$|[-_])`)
	frameworkTagPattern   = regexp.MustCompile(`(?:^|[-_])(cuda|cann|rocm|dtk|hggc|metal|xre)[-_]?v?([0-9]+(?:\.[0-9]+)*)(?:$|[-_])`)
	archTagPattern        = regexp.MustCompile(`(?:^|[-_])(amd64|arm64)(?:$|[-_])`)
	omniTagPattern        = regexp.MustCompile(`(?:^|[-_])omni(?:$|[-_])`)
	leadingVersionPattern = regexp.MustCompile(`^v?([0-9]+\.[0-9]+(?:\.[0-9A-Za-z]+)*)(?:$|[-_])`)
	versionPattern        = regexp.MustCompile(`(?:^|[^0-9])v?([0-9]+\.[0-9]+(?:\.[0-9]+)?)(?:[^0-9]|$)`)
)

// inferTagMetadata reads explicit components of a tag. It does not choose a
// repository-wide default or guess a runtime version from an engine release.
// These editable hints describe the tag, not an inspection of image contents.
func inferTagMetadata(name string) Tag {
	value := strings.TrimSpace(name)
	if colon := strings.LastIndex(value, ":"); colon > strings.LastIndex(value, "/") {
		value = value[colon+1:]
	}
	tag := Tag{Tag: name, RollingTag: IsRollingTag(name)}
	if tag.RollingTag || strings.Contains(name, "@") {
		return tag
	}
	if match := engineTagPattern.FindStringSubmatch(value); len(match) == 3 {
		tag.Engine, tag.EngineVersion = match[1], match[2]
		if tag.Engine == "strata" {
			tag.Engine = engines.Callable
		}
		if tag.Engine == engines.VLLM && omniTagPattern.MatchString(value) {
			tag.Engine = engines.VLLMOmni
		}
	}
	if match := frameworkTagPattern.FindStringSubmatch(value); len(match) == 3 {
		tag.Framework, tag.FrameworkVersion = match[1], match[2]
	}
	if match := archTagPattern.FindStringSubmatch(value); len(match) == 2 {
		tag.Arch = match[1]
	}
	// Keep generic release tags usable with an explicitly configured repository.
	// A backend-only tag must never lend its CUDA/CANN version to the engine.
	if tag.EngineVersion == "" {
		if match := leadingVersionPattern.FindStringSubmatch(value); len(match) == 2 {
			tag.EngineVersion = match[1]
		} else if tag.Framework == "" {
			if match := versionPattern.FindStringSubmatch(value); len(match) == 2 {
				tag.EngineVersion = match[1]
			}
		}
	}
	return tag
}

// InferEngineVersion prefers the explicitly named engine over a backend
// prefix, and retains the generic release-tag fallback for existing catalogs.
func InferEngineVersion(tagOrRef string) string {
	return inferTagMetadata(tagOrRef).EngineVersion
}
