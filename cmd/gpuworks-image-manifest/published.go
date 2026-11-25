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
	publishedRevision     = regexp.MustCompile(`-r[0-9]+$`)
	publishedXRE          = regexp.MustCompile(`^xre-(vllm|sglang)([0-9][0-9A-Za-z.]*)$`)
	dockerTag             = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
)

// publishedItem describes an existing SWR artifact. Build and push commands
// consume the matrix's items section and leave this list alone.
type publishedItem struct {
	ID            string   `yaml:"id"`
	Tag           string   `yaml:"tag"`
	Platforms     []string `yaml:"platforms"`
	EngineVersion string   `yaml:"engine_version"`
	Note          string   `yaml:"note"`
	// LegacyReason declares a tag that is outside the release grammar and says
	// why it exists. A tag inside the grammar must not carry one, and a tag
	// outside it must: the number of declarations is a ratcheted budget
	// (published_tags_outside_grammar).
	LegacyReason string `yaml:"legacy_reason"`
	// Alias marks a legacy tag whose name is unrelated to its id; the id then
	// only supplies the catalog metadata.
	Alias bool `yaml:"alias"`
}

// grammarProblem says what keeps a tag outside the release grammar, or "" when
// it is inside.
func grammarProblem(tag string) string {
	if releaseTagPattern.MatchString(tag) {
		return ""
	}
	revision := publishedRevision.FindStringIndex(tag)
	if revision == nil {
		return "it does not end in a -r<number> revision"
	}
	return fmt.Sprintf("%q before the revision is not <backend><version>[-<model>]-<engine><version>", tag[:revision[0]])
}

// identity reads what the entry says about the image. A tag inside the
// grammar speaks for itself and its id must repeat it; only a declared legacy
// tag may take its identity from an id it does not spell out.
func (item publishedItem) identity() (parsedTag, string, error) {
	if !dockerTag.MatchString(item.Tag) {
		return parsedTag{}, "", fmt.Errorf("published tag %q is not a registry tag", item.Tag)
	}
	reason := strings.TrimSpace(item.LegacyReason)
	problem := grammarProblem(item.Tag)
	if problem == "" {
		if reason != "" || item.Alias {
			return parsedTag{}, "", fmt.Errorf("published tag %q follows the tag grammar: remove legacy_reason and alias", item.Tag)
		}
		id := item.Tag[:publishedRevision.FindStringIndex(item.Tag)[0]]
		if item.ID != id {
			return parsedTag{}, "", fmt.Errorf("published tag %q does not match matrix id %q (want id %q)", item.Tag, item.ID, id)
		}
		tag, err := parseTag(id)
		return tag, "", err
	}
	if reason == "" {
		return parsedTag{}, "", fmt.Errorf("published tag %q is outside the tag grammar: %s. New tags follow <id>-r<number>; "+
			"a tag that already exists under this name needs a legacy_reason", item.Tag, problem)
	}
	tag, err := parseTag(item.ID)
	if err != nil {
		// Old Kunlun names identify the engine but carry no XRE release.
		match := publishedXRE.FindStringSubmatch(item.ID)
		if match == nil {
			return parsedTag{}, "", err
		}
		tag = parsedTag{Backend: "xre", Engine: match[1], EngineVersion: match[2]}
	}
	suffix, matchingID := strings.CutPrefix(item.Tag, item.ID+"-")
	if !matchingID && item.Tag != item.ID && !item.Alias {
		return parsedTag{}, "", fmt.Errorf("published tag %q does not match matrix id %q", item.Tag, item.ID)
	}
	return tag, suffix, nil
}

func (item publishedItem) version() (manifest.ManifestVersion, parsedTag, error) {
	tag, suffix, err := item.identity()
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
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
