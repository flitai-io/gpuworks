package main

import (
	"fmt"
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

const swrRunner = manifest.SWRRunnerRepository

var (
	publishedArchitecture = regexp.MustCompile(`(?:^|-)(amd64|arm64)(?:-|$)`)
	publishedXRE          = regexp.MustCompile(`^xre-(vllm|sglang)([0-9][0-9A-Za-z.]*)$`)
	dockerTag             = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
)

// publishedItem describes an existing SWR artifact. Build and push commands
// consume the matrix's items section and leave this list alone.
type publishedItem struct {
	ID            string   `yaml:"id"`
	Tag           string   `yaml:"tag"`
	Platforms     []string `yaml:"platforms"`
	Alias         bool     `yaml:"alias"`
	EngineVersion string   `yaml:"engine_version"`
	Note          string   `yaml:"note"`
}

func (item publishedItem) version() (manifest.ManifestVersion, parsedTag, error) {
	tag, err := parseTag(item.ID)
	if err != nil {
		// Old Kunlun names identify the engine but carry no XRE release.
		match := publishedXRE.FindStringSubmatch(item.ID)
		if match == nil {
			return manifest.ManifestVersion{}, tag, err
		}
		tag = parsedTag{Backend: "xre", Engine: match[1], EngineVersion: match[2]}
	}
	suffix, matchingID := strings.CutPrefix(item.Tag, item.ID+"-")
	if !dockerTag.MatchString(item.Tag) || (!matchingID && item.Tag != item.ID && !item.Alias) {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("published tag %q does not match matrix id %q", item.Tag, item.ID)
	}
	arch, err := item.arch(tag.Backend)
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
	}
	if tag.Engine == engines.VLLM && strings.Contains(suffix, "-omni-slot") {
		// The legacy tag encodes the base vLLM version only.
		tag.Engine, tag.EngineVersion = engines.VLLMOmni, ""
	}
	if item.EngineVersion != "" {
		// Some Ascend releases name the adapter RC in the tag.
		core, _, rc := strings.Cut(tag.EngineVersion, "rc")
		if tag.Backend != "cann" || tag.Engine != engines.VLLM || !rc || core != item.EngineVersion {
			return manifest.ManifestVersion{}, tag, fmt.Errorf("published tag %q: unexpected core version %q", item.Tag, item.EngineVersion)
		}
		tag.EngineVersion = item.EngineVersion
	}
	return manifest.ManifestVersion{
		Name: item.Tag, ImageRef: swrRunner + ":" + item.Tag,
		EngineVersion: tag.EngineVersion, FrameworkVersion: tag.FrameworkVersion,
		// Inventory records an observed registry tag, without an immutability
		// guarantee. Schema 3 readers already exclude moving tags from auto-pick.
		Arch: arch, RollingTag: true, Notes: item.Note,
	}, tag, nil
}

func (item publishedItem) arch(backend string) (string, error) {
	for _, platform := range item.Platforms {
		if (backend == "metal") != (platform == "darwin/arm64") {
			return "", fmt.Errorf("published tag %q does not support platform %q", item.Tag, platform)
		}
	}
	arch, err := archFromPlatforms(item.Platforms)
	if err != nil {
		return "", fmt.Errorf("published tag %q: %w", item.Tag, err)
	}
	if match := publishedArchitecture.FindStringSubmatch(item.Tag); match != nil && match[1] != arch {
		return "", fmt.Errorf("published tag %q disagrees with architecture %q", item.Tag, arch)
	}
	return arch, nil
}
