package main

import (
	"encoding/json"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

func TestGeneratePublishedSWR(t *testing.T) {
	const input = `
defaults:
  platforms: [linux/amd64]
items:
  - id: cuda13.0-vllm0.30.0
    revision: r4
published:
  - id: cann9.2-910b-vllm0.32.0rc1
    engine_version: 0.32.0
    tag: cann9.2-910b-vllm0.32.0rc1-r1-arm64
    platforms: [linux/arm64]
  - id: cuda13.0-strata0.1.59
    tag: cuda13.0-strata0.1.59-deepseek41-amd64-r5
    platforms: [linux/amd64]
`
	out, err := generate([]byte(input), options{Date: "2026-11-24"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Series) != 3 {
		t.Fatalf("series count = %d, want 3", len(m.Series))
	}
	for _, s := range m.Series {
		v := s.Versions[0]
		switch s.Name {
		case "vLLM · CUDA":
			if v.RollingTag || len(v.RegistrySources) != 2 || v.ImageRef != DefaultRegistry+":cuda13.0-vllm0.30.0-r4" {
				t.Fatalf("existing mirror selection changed: %+v", v)
			}
		case "vLLM · CANN":
			if v.EngineVersion != "0.32.0" || !v.RollingTag || len(v.RegistrySources) != 0 || v.Arch != "arm64" || v.ImageRef != swrRunner+":cann9.2-910b-vllm0.32.0rc1-r1-arm64" {
				t.Fatalf("published Ascend image = %+v", v)
			}
		case "Strata · CUDA":
			if s.Engine != "callable" || !v.RollingTag || v.ImageRef != swrRunner+":cuda13.0-strata0.1.59-deepseek41-amd64-r5" {
				t.Fatalf("published Strata image = %+v", v)
			}
		default:
			t.Fatalf("unexpected series %s", s.Name)
		}
	}
}

func TestPublishedSWRRejectsWrongIdentity(t *testing.T) {
	for _, item := range []publishedItem{
		{ID: "cann9.2-910b-vllm0.32.0rc1", Tag: "cann9.2-910b-vllm0.32.0rc1", Platforms: []string{"linux/arm64"}, EngineVersion: "0.33.0"},
		{ID: "cuda13.0-vllm0.34.0", Tag: "cuda13.0-vllm0.34.0-r1-arm64", Platforms: []string{"linux/amd64"}},
		{ID: "cuda13.0-vllm0.34.0", Tag: "cuda13.0-vllm0.33.0-r1-amd64", Platforms: []string{"linux/amd64"}},
		{ID: "cuda13.0-vllm0.34.0", Tag: "cuda13.0-vllm0.34.0-amd64", Platforms: []string{"linux/amd64", "linux/arm64"}},
	} {
		if _, _, err := item.version(); err == nil {
			t.Errorf("wrong identity accepted: %+v", item)
		}
	}
}

func TestPublishedSWRPreservesLegacyTags(t *testing.T) {
	cases := []struct {
		item                                    publishedItem
		arch, engine, version, frameworkVersion string
	}{
		{publishedItem{ID: "cuda13.0-vllm0.30.0", Tag: "cuda13.0-vllm0.30.0", Platforms: []string{"linux/amd64", "linux/arm64"}}, "multi", "vllm", "0.30.0", "13.0"},
		{publishedItem{ID: "cann9.1-vllm0.23.0", Tag: "cann9.1-vllm0.23.0-amd64-omni-slot13", Platforms: []string{"linux/amd64"}}, "amd64", "vllm-omni", "", "9.1"},
		{publishedItem{ID: "xre-sglang0.5.5.post3", Tag: "xre-sglang0.5.5.post3-amd64-diffusion-slot27", Platforms: []string{"linux/amd64"}}, "amd64", "sglang", "0.5.5.post3", ""},
		{publishedItem{ID: "metal15.0-llama.cpp-b12631", Tag: "metal15.0-llama.cpp-b12631-r1", Platforms: []string{"darwin/arm64"}}, "arm64", "llama.cpp", "b12631", "15.0"},
		{publishedItem{ID: "cuda12.8-v100-vllm1cat1.5.1", Tag: "zz-pilot-v100-1cat-r1", Platforms: []string{"linux/amd64"}, Alias: true}, "amd64", "vllm", "1cat1.5.1", "12.8"},
	}
	for _, test := range cases {
		t.Run(test.item.Tag, func(t *testing.T) {
			v, parsed, err := test.item.version()
			if err != nil {
				t.Fatal(err)
			}
			if v.ImageRef != swrRunner+":"+test.item.Tag || v.Arch != test.arch || parsed.Engine != test.engine ||
				v.EngineVersion != test.version || v.FrameworkVersion != test.frameworkVersion || !v.RollingTag {
				t.Fatalf("published version = %+v, tag = %+v", v, parsed)
			}
		})
	}
}

func TestGenerateRejectsDuplicatePublishedTag(t *testing.T) {
	const input = `
defaults:
  platforms: [linux/amd64]
items:
  - id: cuda13.0-vllm0.30.0
    revision: r4
published:
  - id: cuda13.0-vllm0.30.0
    tag: cuda13.0-vllm0.30.0-r4
    platforms: [linux/amd64]
`
	if _, err := generate([]byte(input), options{Date: "2026-11-24"}); err == nil {
		t.Fatal("a listed tag was added again through SWR")
	}
}

// Installed schema 3 readers discard unknown version fields before hashing
// the catalog. Adding one to the generated file breaks signed-channel sync.
func TestPublishedSeedFitsExistingSchema3Readers(t *testing.T) {
	data, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Series []struct {
			Versions []map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for _, series := range wire.Series {
		for _, version := range series.Versions {
			for field := range version {
				switch field {
				case "name", "image_ref", "engine_version", "framework_version", "arch", "registry_sources",
					"blob_sha256", "size_bytes", "blob", "deprecated", "deprecated_since", "rolling_tag", "notes":
				default:
					t.Fatalf("schema 3 clients omit %q from the checksum of %s", field, version["image_ref"])
				}
			}
		}
	}
}
