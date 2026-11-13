//go:build cloudgpu && partition

package l4

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// giteeFullDeployRentalImage is the Gitee-catalog rental base image
// (image_ident) for this test's compute instance. Distinct from the
// deployment's own application image below: Gitee's Rent() validates
// image_ident server-side against its own curated catalog (third-party
// registries are rejected with "镜像不存在或不可用"), so the RENTAL image
// must come from GET /compute/images. This one was real-verified in the
// CANN9_x86_64_910B2C case of TestGiteeAscendPartitionCapability as a
// working udocker-capable instance (CANN 9.0.0, x86_64, Ascend 910B2C).
const (
	giteeFullDeployRentalImage = "easzlab.io.local:5000/metax/vllm-ascend:v0.21.0rc1-openeuler"
	giteeARM64CANN9RentalImage = "easzlab.io.local:5000/metax/torch-npu:2.10.0-910b-openeuler24.03-py3.11"
)

// giteeFullDeployAppImage is the actual inference workload image, pulled at
// deployment time by the real GPUWorks agent: the built-in 910B vLLM runner
// the catalog's 910B specs use, by its Huawei SWR copy, so the Gitee
// instance only needs connectivity to SWR (no Docker Hub auth needed). The
// manifest is multi-arch (amd64+arm64). The prefetch path downloads all
// layers concurrently before udocker pull runs. Layers are decompressed
// in-place after pull to speed up udocker create; multi-stream gzip layers
// are handled correctly.
func giteeFullDeployAppImage() string {
	if image := strings.TrimSpace(hostEnv("GITEE_FULL_DEPLOY_APP_IMAGE", "")); image != "" {
		return image
	}
	return "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cann9.1-910b-vllm0.26.0rc1-r2"
}

func giteeARM64FullDeployAppImage() string {
	if image := strings.TrimSpace(hostEnv("GITEE_ARM64_FULL_DEPLOY_APP_IMAGE", "")); image != "" {
		return image
	}
	return giteeFullDeployAppImage()
}

type giteeDeployChatProfile struct {
	runSuffix       string
	zone            string
	gpuModel        string
	outerImage      string
	wantArch        string
	wantCANNPrefix  string
	innerAppImage   string
	servedName      string
	evidenceContext string
	startupTimeout  time.Duration
}

// TestGiteeAscendFullServerAgentDeployChatEndToEnd broadens Gitee CI coverage
// beyond the basic compatibility probe in TestGiteeAscendPartitionCapability
// (docs/dev/ledgers/engineering-log.md "Broaden Gitee CI to full server+agent deploy+chat coverage").
// It rents a real Gitee Ascend instance, brings a real GPUWorks agent online
// against it, then drives a real whole-GPU (non-partitioned — this
// deliberately avoids the device-share restriction documented in
// artifacts/archive/2026-08-28-gitee-ascend-verification/device-share-limitation-2026-08-28.md, since
// applyAscendPartition/EnsureDeviceShare are only invoked when a
// vram_limit_gib partition is requested) vLLM deployment entirely through the
// real GPUWorks platform API: POST /api/v1/deployments pulls a real
// Huawei-SWR-hosted vLLM Ascend image, downloads a real small (~0.6B) model
// from ModelScope, waits for the instance to reach running, then performs a
// real POST /v1/chat/completions round trip through the server's OpenAI-
// compatible proxy.
func TestGiteeAscendFullServerAgentDeployChatEndToEnd(t *testing.T) {
	testGiteeAscendDeployChat(t, giteeDeployChatProfile{
		runSuffix: "fulldeploy", zone: "gitee", gpuModel: "Ascend 910B2C",
		outerImage: giteeFullDeployRentalImage, wantArch: "x86_64", wantCANNPrefix: "9.0",
		innerAppImage: giteeFullDeployAppImage(), servedName: "qwen3-06b-gitee-ascend",
		evidenceContext: "x86_64 CANN9 outer and CANN9 application",
		startupTimeout:  45 * time.Minute,
	})
}

// TestGiteeAscendARM64ContainerInContainerDeployChatEndToEnd validates the
// intended container-in-container boundary on Gitee's arm64/CANN9 profile.
// Gitee dropped the suanjia 910B1 offer in 2026-11; 910B2 in the gitee zone
// takes the same arm64 CANN9 outer image, and the arch assertion below fails
// the run if Gitee places it on an x86_64 host instead. Both the outer rental and inner application use
// CANN9: the catalog-advertised CANN8.5 ARM outer image is rejected by Rent,
// while a CANN8.5 application under this PRoot boundary fails inside CANN FE's
// platform_config realpath lookup before inference. Do not claim that known
// incompatibility as passing coverage.
func TestGiteeAscendARM64ContainerInContainerDeployChatEndToEnd(t *testing.T) {
	testGiteeAscendDeployChat(t, giteeDeployChatProfile{
		runSuffix: "arm64-cann9", zone: "gitee", gpuModel: "Ascend 910B2",
		outerImage: giteeARM64CANN9RentalImage, wantArch: "arm64", wantCANNPrefix: "9.0",
		innerAppImage: giteeARM64FullDeployAppImage(), servedName: "qwen3-06b-gitee-arm64-cann9",
		evidenceContext: "arm64 CANN9 outer and CANN9 application",
		startupTimeout:  60 * time.Minute,
	})
}

func testGiteeAscendDeployChat(t *testing.T, profile giteeDeployChatProfile) {
	t.Helper()
	secrets := skipIfNoSecrets(t, "GITEE_API_TOKEN")
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := cloudprovider.NewGitee(secrets["GITEE_API_TOKEN"], profile.zone, profile.outerImage, runID+"-"+profile.runSuffix)
	if err != nil {
		t.Fatal(err)
	}
	env := setupCloudTest(t, provider, cloudprovider.GPUSpec{GPUModel: profile.gpuModel, GPUCount: 1, MinVRAMMB: 32 << 10})
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if accelerator.NormalizeArch(node.Arch) != profile.wantArch || !strings.HasPrefix(node.FrameworkVersion, profile.wantCANNPrefix) {
		t.Fatalf("Gitee full-deploy outer profile arch=%q framework=%q/%q, want %s CANN %s.x", node.Arch, node.Framework, node.FrameworkVersion, profile.wantArch, profile.wantCANNPrefix)
	}
	if node.RuntimeDriver != "udocker" {
		t.Fatalf("Gitee full-deploy runtime=%q runtimes=%q, want explicit inner udocker container", node.RuntimeDriver, node.Runtimes)
	}

	// Qwen3-0.6B: real-machine verified elsewhere (198.51.100.31, see
	// qwen3-0.6b-vllm-ascend-910b4.md research) to load, serve, and answer
	// chat requests correctly under vLLM Ascend. Meta mirrors its published
	// config.json (Qwen3ForCausalLM, 28 layers, GQA 16/8 heads, 40960 native
	// context — this deployment requests a much shorter max_model_len).
	model, err := env.Control.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Revision: "master",
		Name: "Qwen3-0.6B", Arch: "qwen3", WeightBytes: 1<<30 + (1 << 29), Dtype: "bfloat16",
		Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepareCloudModelDownload(t, env, model)

	image := profile.innerAppImage
	body := map[string]any{
		"model_id": model.ID, "node_uuid": env.NodeUUID, "served_name": profile.servedName,
		"engine": "vllm", "image": image,
		"quick_params": map[string]any{"max_model_len": 4096, "gpu_memory_utilization": 0.8},
	}
	// Cold start includes a ~1.5 GiB ModelScope download plus a multi-GB image
	// pulled over the rented instance's own network. The arm64 CANN image is
	// ~7 GiB and real Gitee egress takes about 30 minutes for the first pull.
	// Its larger budget also lets the bounded three-attempt controller finish
	// cached-image retries; it is not an unbounded substitute for readiness.
	startupTimeout := profile.startupTimeout
	if startupTimeout <= 0 {
		startupTimeout = 45 * time.Minute
	}
	d := createCloudDeploymentWithTimeout(t, []*cloudTestEnv{env}, body, 1, startupTimeout)
	d.models(t)

	// A bespoke chat call (rather than the shared (*cloudDeployment).chat
	// helper) because Qwen3 defaults to a <think> reasoning prefix that a
	// small max_tokens budget can truncate before any visible reply — the
	// qwen3-0.6b-vllm-ascend-910b4.md reference record hit exactly this and
	// resolved it by disabling thinking mode explicitly.
	chatReq := `{"model":"` + profile.servedName + `","messages":[{"role":"user","content":"只回答两个字：成功"}],"max_tokens":32,"temperature":0,"chat_template_kwargs":{"enable_thinking":false}}`
	req, reqErr := http.NewRequest(http.MethodPost, env.Control.URL()+"/v1/chat/completions", strings.NewReader(chatReq))
	if reqErr != nil {
		t.Fatal(reqErr)
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Gitee Ascend full deploy chat status=%d body=%s", resp.StatusCode, data)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		t.Fatalf("Gitee Ascend full deploy chat returned no content: %s", data)
	}
	t.Logf("Gitee Ascend full deploy chat succeeded: status=%d content=%q", resp.StatusCode, strings.TrimSpace(decoded.Choices[0].Message.Content))
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt",
		"instance=ledger-owned-and-destroyed\ncontext="+profile.evidenceContext+"\nouter_image="+profile.outerImage+"\nouter_arch="+node.Arch+"\nouter_framework="+node.Framework+" "+node.FrameworkVersion+"\nruntime=udocker\ninner_app_image="+image+"\nmodel=Qwen/Qwen3-0.6B\nchat_response="+string(data)+"\n")
}
