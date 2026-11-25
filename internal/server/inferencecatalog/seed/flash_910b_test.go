package seed

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// The 910B (A2) profiles for Qwen3.8-Flash-Next and GLM-5.3-Flash: each takes
// only 64 GiB A2 cards in its own device count, launches that count as vLLM TP,
// and the MTP profiles are the only ones that turn on speculative decoding.
func TestFlash910BProfilesPlaceAndLaunchTheirOwnShape(t *testing.T) {
	for _, recipe := range []struct {
		model, name string
		count       int
		mtp         bool
	}{
		{"qwen/qwen3.8-flash-next", "int8-vllm-910b-tp4", 4, false},
		{"qwen/qwen3.8-flash-next", "int8-vllm-910b-tp4-mtp", 4, true},
		{"qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp4", 4, false},
		{"qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp4-mtp", 4, true},
		{"zai-org/glm-5.3-flash", "w8a8-vllm-910b-graph-tp8", 8, false},
	} {
		t.Run(recipe.name, func(t *testing.T) {
			_, spec := catalogSpec(t, recipe.model, recipe.name)
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
			for name, want := range map[string]bool{
				"Ascend 910B3": true, "Ascend 910B4": true, "Ascend 910B4-1": true, "Ascend 910B2C": true,
				"Ascend 910C": false, "Ascend 310P3": false,
			} {
				if got := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched; got != want {
					t.Fatalf("card %q matched=%t, want %t", name, got, want)
				}
			}
			small, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: recipe.count, Vendor: "ascend", MinMemoryGiB: 32})
			if err != nil {
				t.Fatal(err)
			}
			if small.Reason == "" {
				t.Fatal("32 GiB cards were accepted")
			}
			for _, count := range []int{1, 2, 4, 8, 16} {
				resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: count, Vendor: "ascend", MinMemoryGiB: 64})
				if err != nil {
					t.Fatal(err)
				}
				if (resolved.Reason == "") != (count == recipe.count) {
					t.Fatalf("%d cards: reason=%q", count, resolved.Reason)
				}
				if count != recipe.count {
					continue
				}
				engine, err := deploy.SelectEngine(spec.Engine)
				if err != nil {
					t.Fatal(err)
				}
				launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
					Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, APIContract: spec.API, PlacementVariables: resolved.Variables,
					Params: deploy.Params{
						ModelPath: "/model", ServedName: "flash-910b", APIPort: 31417, NNodes: 1,
						RuntimeDriver: "udocker",
						AssignedGPUs:  count, TensorParallel: count,
						TemplateArgs: spec.Args, TemplateEnv: spec.Env,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				for flag, want := range map[string]string{
					"--tensor-parallel-size": strconv.Itoa(recipe.count), "--port": "31417",
					"--served-model-name": "flash-910b", "--quantization": "ascend",
				} {
					if got, found := deploy.LaunchFlag(launch.Argv, flag); !found || got != want {
						t.Errorf("%s=%q, want %q", flag, got, want)
					}
				}
				speculative, found := deploy.LaunchFlag(launch.Argv, "--speculative-config")
				if found != recipe.mtp {
					t.Fatalf("--speculative-config present=%t, want %t", found, recipe.mtp)
				}
				if recipe.mtp {
					var config struct {
						Method string `json:"method"`
						Tokens int    `json:"num_speculative_tokens"`
					}
					if err := json.Unmarshal([]byte(speculative), &config); err != nil || config.Method != "mtp" || config.Tokens != 3 {
						t.Fatalf("speculative config %q (%v)", speculative, err)
					}
					// Every captured decode graph must hold whole sequences of one
					// target token plus the draft tokens.
					graphs, _ := deploy.LaunchFlag(launch.Argv, "--compilation-config")
					var compilation struct {
						Sizes []int `json:"cudagraph_capture_sizes"`
					}
					if err := json.Unmarshal([]byte(graphs), &compilation); err != nil || len(compilation.Sizes) == 0 {
						t.Fatalf("compilation config %q (%v)", graphs, err)
					}
					for _, size := range compilation.Sizes {
						if size%(config.Tokens+1) != 0 {
							t.Errorf("graph size %d is not a multiple of %d", size, config.Tokens+1)
						}
					}
				}
				if recipe.model == "zai-org/glm-5.3-flash" {
					// serve.py derives --hf-overrides from the mounted checkpoint and
					// refuses a second copy; the entrypoint runs it, not vllm directly.
					if _, found := deploy.LaunchFlag(launch.Argv, "--hf-overrides"); found {
						t.Fatal("the GLM 910B profile must not pass --hf-overrides itself")
					}
					if len(launch.Entrypoint) != 4 || !strings.HasSuffix(launch.Entrypoint[2], `exec python /opt/gpuworks/glm53-910b/serve.py "$@"`) {
						t.Fatalf("entrypoint %q", launch.Entrypoint)
					}
					// serve.py reads the model path from its first argument.
					if len(launch.Argv) == 0 || launch.Argv[0] != "/model" {
						t.Fatalf("argv does not start with the model path: %q", launch.Argv)
					}
				}
			}
		})
	}
}

// The eight-card W4A8 profile takes exactly eight A2 cards of 32 GiB or more, launches
// them as TP8 with a published recipe's switches, and carries the four values
// the 2026-11-23 measurement changed: 16 sequences, 8192 batched tokens,
// decode graphs up to 64, and thinking off by default.
func TestQwen38EightCard910BProfileLaunchesThePublishedRecipe(t *testing.T) {
	_, spec := catalogSpec(t, "qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp8-mtp")
	_, fourCard := catalogSpec(t, "qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp4-mtp")
	if spec.Image != fourCard.Image || spec.WeightBytes != fourCard.WeightBytes || spec.ShmBytes != 256<<30 {
		t.Fatalf("image=%q weight_bytes=%d shm=%d, want the four-card image and weights and 256 GiB of shm", spec.Image, spec.WeightBytes, spec.ShmBytes)
	}
	predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
	for name, want := range map[string]bool{"Ascend 910B4": true, "Ascend 910B2C": true, "Ascend 910C": false} {
		if got := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched; got != want {
			t.Fatalf("card %q matched=%t, want %t", name, got, want)
		}
	}
	for _, shape := range []struct {
		count    int
		memory   float64
		accepted bool
	}{{8, 32, true}, {8, 64, true}, {8, 24, false}, {4, 64, false}, {16, 64, false}} {
		resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: shape.count, Vendor: "ascend", MinMemoryGiB: shape.memory})
		if err != nil {
			t.Fatal(err)
		}
		if (resolved.Reason == "") != shape.accepted {
			t.Fatalf("%d cards of %v GiB: reason=%q, accepted want %t", shape.count, shape.memory, resolved.Reason, shape.accepted)
		}
	}
	resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: 8, Vendor: "ascend", MinMemoryGiB: 32})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, APIContract: spec.API, PlacementVariables: resolved.Variables,
		Params: deploy.Params{
			ModelPath: "/model", ServedName: "flash-910b", APIPort: 31417, NNodes: 1,
			RuntimeDriver: "docker", AssignedGPUs: 8, TensorParallel: 8,
			TemplateArgs: spec.Args, TemplateEnv: spec.Env,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{
		"--tensor-parallel-size": "8", "--data-parallel-size": "1", "--quantization": "ascend",
		"--max-model-len": "262144", "--max-num-seqs": "16", "--max-num-batched-tokens": "8192",
		"--gpu-memory-utilization": "0.90", "--mamba-cache-mode": "align",
		"--tool-call-parser": "qwen3_xml", "--reasoning-parser": "qwen3",
		"--engram-config":                `{"cpu_offload":true,"dp_shared_memory":false}`,
		"--compilation-config":           `{"cudagraph_capture_sizes":[4,8,12,16,24,32,48,64],"cudagraph_mode":"FULL_DECODE_ONLY"}`,
		"--default-chat-template-kwargs": `{"enable_thinking":false}`,
		"--speculative-config":           `{"method":"mtp","num_speculative_tokens":3,"enforce_eager":false}`,
	} {
		if got, found := deploy.LaunchFlag(launch.Argv, flag); !found || got != want {
			t.Errorf("%s=%q found=%t, want %q", flag, got, found, want)
		}
	}
	argv := strings.Join(launch.Argv, " ")
	for _, flag := range []string{"--async-scheduling", "--enable-expert-parallel", "--enable-prefix-caching", "--enable-auto-tool-choice", "--trust-remote-code"} {
		if !strings.Contains(argv, flag) {
			t.Errorf("argv lacks %s", flag)
		}
	}
	for _, flag := range []string{"--no-async-scheduling", "--kv-cache-memory-bytes"} {
		if strings.Contains(argv, flag) {
			t.Errorf("argv carries the four-card profiles' %s", flag)
		}
	}
	additional, _ := deploy.LaunchFlag(launch.Argv, "--additional-config")
	var config struct {
		CPUBinding     bool `json:"enable_cpu_binding"`
		SharedExpertDP bool `json:"enable_shared_expert_dp"`
		FusedMC2       *int `json:"enable_fused_mc2"`
		FlashComm1     bool `json:"enable_flashcomm1"`
		Multistream    bool `json:"multistream_overlap_shared_expert"`
		Compilation    struct {
			FuseNormQuant *bool `json:"fuse_norm_quant"`
			NPUGraphEx    bool  `json:"enable_npugraph_ex"`
			StaticKernel  *bool `json:"enable_static_kernel"`
			SuperKernel   *bool `json:"enable_super_kernel"`
		} `json:"ascend_compilation_config"`
	}
	if err := json.Unmarshal([]byte(additional), &config); err != nil {
		t.Fatalf("additional config %q: %v", additional, err)
	}
	off := func(v *bool) bool { return v != nil && !*v }
	if !config.CPUBinding || !config.SharedExpertDP || !config.FlashComm1 || !config.Multistream ||
		config.FusedMC2 == nil || *config.FusedMC2 != 0 || !config.Compilation.NPUGraphEx ||
		!off(config.Compilation.FuseNormQuant) || !off(config.Compilation.StaticKernel) || !off(config.Compilation.SuperKernel) {
		t.Fatalf("additional config %q does not carry the recipe's switches", additional)
	}
	for name, want := range map[string]string{
		"VLLM_USE_V2_MODEL_RUNNER": "1", "VLLM_ENGINE_READY_TIMEOUT_S": "7200", "VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS": "3000",
		"VLLM_CHAT_TEMPLATE_RENDER_TIMEOUT": "300", "HCCL_BUFFSIZE": "1024", "HCCL_CONNECT_TIMEOUT": "1200",
		"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:True", "OMP_PROC_BIND": "false", "OMP_NUM_THREADS": "8", "TASK_QUEUE_ENABLE": "1",
	} {
		if got := launch.Env[name]; got != want {
			t.Errorf("env %s=%q, want %q", name, got, want)
		}
	}
	// One host's address, interface or library path has no place in a template.
	for _, name := range []string{"HCCL_IF_IP", "GLOO_SOCKET_IFNAME", "TP_SOCKET_IFNAME", "HCCL_SOCKET_IFNAME", "LD_PRELOAD"} {
		if _, found := spec.Env[name]; found {
			t.Errorf("template env carries the host-specific %s", name)
		}
	}
}
