package main

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

func TestLatestReleasePerPlatform(t *testing.T) {
	versions := []manifest.ManifestVersion{
		{Name: "cuda13.0-sglang0.5.22-r90", Arch: "multi"},
		{Name: "cuda12.0-sglang0.5.13-r2", Arch: "multi"},
		{Name: "cuda13.0-sglang0.5.23rc2-r12", Arch: "multi"},
		{Name: "cuda13.0-sglang0.5.23-r2", Arch: "multi"},
		{Name: "cuda13.0-sglang0.5.23-r10", Arch: "amd64"},
	}
	got, err := latestPlatformVersions(versions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Arch != "multi" || got[1].AutoSelect != "arm64" {
		t.Fatalf("partial winner %+v", got)
	}
	var names []string
	for _, v := range got {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, []string{versions[1].Name, versions[3].Name, versions[4].Name}) {
		t.Fatalf("winners %v", names)
	}
}

func TestStableReleaseBeatsPaddedCandidateAndKeepsTemplatePin(t *testing.T) {
	old := manifest.ManifestVersion{Name: "cuda13.0-strata0.34.0rc1-r99", Arch: "amd64", ImageRef: "gpuworks/gpuworks-runner:cuda13.0-strata0.34.0rc1-r99", RegistrySources: []string{"quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.34.0rc1-r99"}}
	stable := manifest.ManifestVersion{Name: "cuda13.0-strata0.34-r1", Arch: "amd64"}
	got, err := latestPlatformVersions([]manifest.ManifestVersion{old, stable}, map[string]bool{old.Name: true})
	if err != nil || len(got) != 2 || got[0].AutoSelect != "manual" || got[0].ImageRef != old.ImageRef || !slices.Equal(got[0].RegistrySources, old.RegistrySources) || got[1].AutoSelect != "" {
		t.Fatalf("catalog %+v: %v", got, err)
	}
}

func TestPrebuiltLegacyDeclarationDoesNotAdmitTag(t *testing.T) {
	const tag = "cuda13.0-vllm0.34.0-r1-arm64"
	matrix := "items:\n  - id: " + tag + "\n    backend: cuda\n    builder: prebuilt\n    artifact: quay.io/gpuworks/gpuworks-runner:" + tag + "@sha256:" + strings.Repeat("1", 64) + "\n    platforms: [linux/arm64]\nlegacy_tags:\n  - tag: " + tag + "\n    legacy_reason: Historical release.\n"
	if _, err := generate([]byte(matrix), options{Date: "2026-11-26"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("invalid catalog tag: %v", err)
	}
}

func TestManualReleaseDoesNotDisplaceAutomaticWinner(t *testing.T) {
	stable := manifest.ManifestVersion{Name: "cuda13.0-vllm0.34.0-r1", Arch: "multi"}
	manual := manifest.ManifestVersion{Name: "cuda13.0-vllm0.35.0-r1", Arch: "amd64", AutoSelect: "manual"}
	got, err := latestPlatformVersions([]manifest.ManifestVersion{stable, manual}, nil)
	if err != nil || len(got) != 2 || got[0].AutoSelect != "" || got[1].AutoSelect != "manual" {
		t.Fatalf("selection %+v: %v", got, err)
	}
}
