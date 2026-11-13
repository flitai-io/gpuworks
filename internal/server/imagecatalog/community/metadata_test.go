package community

import (
	"reflect"
	"testing"
)

func TestGPUWorksRunnerRepositoriesRemainMixed(t *testing.T) {
	repos, err := Repos(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if repo.Repo != "quay.io/gpuworks/gpuworks-runner" {
			continue
		}
		if repo.Engine != "" || repo.Framework != "" || repo.Registry != RegistryQuay ||
			!reflect.DeepEqual(repo.Siblings, []string{"registry.zstack.ai/gpuworks/gpuworks-runner"}) {
			t.Fatal("mixed runner repositories must use per-tag metadata and verified sibling repositories")
		}
		return
	}
	t.Fatal("published GPUWorks runner repository is missing from the community browser")
}

func TestCommunityTagMetadata(t *testing.T) {
	for _, tc := range []struct {
		tag, engine, engineVersion, framework, frameworkVersion, arch string
	}{
		{"cuda13.0-vllm0.29.0-arm64-slot01", "vllm", "0.29.0", "cuda", "13.0", "arm64"},
		{"cuda13.0-strata0.1.38-r5", "callable", "0.1.38", "cuda", "13.0", ""},
		{"cuda13.0-strata0.1.38-amd64-slot31-r5", "callable", "0.1.38", "cuda", "13.0", "amd64"},
		{"cuda13.0-stratagem0.1.38-r5", "", "", "cuda", "13.0", ""},
		{"cuda13.0-strata-r5", "", "", "cuda", "13.0", ""},
		{"cuda13.0-strata", "", "", "cuda", "13.0", ""},
		{"cann9.0-sglang0.5.15.post1-amd64-diffusion-slot16", "sglang", "0.5.15.post1", "cann", "9.0", "amd64"},
		{"cuda13.0-vllm-omni0.30.0-r3", "vllm-omni", "0.30.0", "cuda", "13.0", ""},
		{"cann8.5-vllm0.18.0-arm64-omni-slot14", "vllm-omni", "0.18.0", "cann", "8.5", "arm64"},
		{"cann9.1-910b-vllm0.27.1rc1-r2", "vllm", "0.27.1rc1", "cann", "9.1", ""},
		{"cuda12.8-v100-vllm1cat1.5.1-r1", "vllm", "1cat1.5.1", "cuda", "12.8", ""},
		{"dtk26.04-sglang0.5.12", "sglang", "0.5.12", "dtk", "26.04", ""},
		{"hggc1.6-vllm0.9.1-amd64-slot21", "vllm", "0.9.1", "hggc", "1.6", "amd64"},
		{"xre5.0.21-vllm0.25.1-amd64-slot25", "vllm", "0.25.1", "xre", "5.0.21", "amd64"},
		{"registry.example:5443/team/runner:cuda12.9-sglang0.5.19-arm64-slot07", "sglang", "0.5.19", "cuda", "12.9", "arm64"},
		{"0.5.12-ubuntu22.04-dtk2604-py3.10", "", "0.5.12", "dtk", "2604", ""},
		{"v0.11.0", "", "0.11.0", "", "", ""},
		{"cuda13.0-nightly", "", "", "cuda", "13.0", ""},
		{"nightly", "", "", "", "", ""},
		{"qwen38", "", "", "", "", ""},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			got := inferTagMetadata(tc.tag)
			if got.Engine != tc.engine || got.EngineVersion != tc.engineVersion || got.Framework != tc.framework || got.FrameworkVersion != tc.frameworkVersion || got.Arch != tc.arch {
				t.Fatalf("metadata=%+v, want engine=%s/%s framework=%s/%s arch=%s", got, tc.engine, tc.engineVersion, tc.framework, tc.frameworkVersion, tc.arch)
			}
		})
	}
}
