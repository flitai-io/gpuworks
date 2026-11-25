package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

var (
	publishedArchitecture = regexp.MustCompile(`(?:^|-)(amd64|arm64)(?:-|$)`)
	publishedRevision     = regexp.MustCompile(`-r[0-9]+$`)
	publishedXRE          = regexp.MustCompile(`^xre-(vllm|sglang)([0-9][0-9A-Za-z.]*)$`)
	dockerTag             = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
)

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
func (item matrixItem) identity() (parsedTag, string, error) {
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

func (item matrixItem) manualVersion(defaultRepositories []string) (manifest.ManifestVersion, parsedTag, error) {
	if item.Revision != "" {
		if item.Tag != "" {
			return manifest.ManifestVersion{}, parsedTag{}, fmt.Errorf("catalog item %q: choose revision or legacy tag", item.ID)
		}
		item.Tag = item.ID + "-" + item.Revision
		if !releaseTagPattern.MatchString(item.Tag) {
			return manifest.ManifestVersion{}, parsedTag{}, fmt.Errorf("catalog item %q: invalid release tag %q", item.ID, item.Tag)
		}
	}

	tag, suffix, err := item.identity()
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
	}
	arch, err := item.arch(tag.Backend)
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
	}
	if tag.Engine == engines.VLLM && slices.Contains(strings.Split(suffix, "-"), "omni") {
		// The tag identifies base vLLM. Only the published package can
		// supply the Omni version, including any vendor build suffix.
		tag.Engine, tag.EngineVersion = engines.VLLMOmni, item.EngineVersion
	} else if item.EngineVersion != "" {
		// Some Ascend releases name the adapter RC in the tag.
		core, _, rc := strings.Cut(tag.EngineVersion, "rc")
		if tag.Backend != "cann" || tag.Engine != engines.VLLM || !rc || core != item.EngineVersion {
			return manifest.ManifestVersion{}, tag, fmt.Errorf("published tag %q: unexpected core version %q", item.Tag, item.EngineVersion)
		}
		tag.EngineVersion = item.EngineVersion
	}
	repositories := item.Repositories
	if repositories == nil {
		repositories = defaultRepositories
	}
	refs, err := repositoryRefs(item.Tag, repositories)
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
	}
	return manifest.ManifestVersion{
		Name: item.Tag, ImageRef: refs[0], RegistrySources: refs[1:],
		EngineVersion: tag.EngineVersion, FrameworkVersion: tag.FrameworkVersion,
		Arch: arch, ManualOnly: true, RollingTag: community.IsRollingTag(item.Tag), Notes: item.Note,
	}, tag, nil
}

// The first verified location is the catalog identity. Other locations carry
// the same tagged content and can be tried according to node pull policy.
func repositoryRefs(tag string, repositories []string) ([]string, error) {
	if len(repositories) == 0 {
		return nil, fmt.Errorf("tag %q has no verified repository", tag)
	}
	refs := make([]string, 0, len(repositories))
	seen := make(map[string]bool)
	for _, repo := range repositories {
		path := repo[strings.LastIndex(repo, "/")+1:]
		if path == "" || strings.Contains(path, ":") || strings.ContainsAny(repo, "@?#") ||
			strings.HasPrefix(repo, "/") || strings.Contains(repo, "//") || strings.IndexFunc(repo, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("tag %q: invalid repository %q", tag, repo)
		}
		key := pullref.NormalizeRef(repo)
		if seen[key] {
			return nil, fmt.Errorf("tag %q: duplicate repository %q", tag, repo)
		}
		seen[key] = true
		refs = append(refs, repo+":"+tag)
	}
	return refs, nil
}

func (item matrixItem) arch(backend string) (string, error) {
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
