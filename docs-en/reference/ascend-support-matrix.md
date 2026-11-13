# Ascend Support Matrix

Updated: 2026-11-02. This document distinguishes three things that are easy to confuse:

- **Outer CANN**: the toolkit provided by the node or rented cloud marketplace image. Self-owned bare-metal machines may have only a driver, recorded as `driver-only`; this does not mean that no container runtime is available.
- **Inner CANN**: the CANN userspace application binary interface (userspace ABI) in the workload image launched by GPUWorks. It affects whether the workload image can run, but does not participate in `libvruntime.so` asset selection.
- **Runtime (container runtime)**: Docker, nerdctl, or runveil (`udocker` driver). Gitee rental images provide only the outer environment, not the workload image; the `udocker` scenarios in the table use PRoot to run workload containers inside the outer container (container-in-container). Results marked `udocker` below were verified with the old udocker CLI launcher; runveil results are listed separately.

## Current Tested Matrix

| Environment | Architecture / NPU | Outer CANN | Inner CANN / Runtime | Highest Level Passed | Conclusion |
|---|---|---:|---|---|---|
| Gitee x86_64 | x86_64 / 910B2C | 9.0 | 9.0 / udocker | Platform full-GPU deployment + `/v1/chat/completions` | Supported; test explicitly asserts `runtime_driver=udocker` |
| Gitee x86_64 | x86_64 / 910B2C | 8.5.1 | 8.5 / udocker | Platform deployment reaches healthy; dGPU capability probe correctly reports cloud-side denial | Outer 8.5 + inner 8.5 can start; successful chat evidence reproducible in CI is still missing |
| Gitee ARM | arm64 / 910B1 | 9.0 | 9.0 / udocker | Platform full-GPU deployment + `/v1/chat/completions` | Supported; actually prefetches arm64 manifest/layers |
| Gitee ARM | arm64 / 910B1 | 9.0 | 8.5 / udocker | Container starts as far as CANN FE | Failed with the old PRoot because `platform_config` realpath resolution failed. ARM64 `faccessat2` is now fixed; this cloud combination has not yet been retested |
| Gitee ARM catalog entry | arm64 / 910B1 | 8.5 | Inner environment not reached | Rental prerequisite validation | The cloud marketplace actually returns “image and host do not match,” so this cannot serve as a CI fixture; this does not establish incompatibility between outer 8.5 and inner 8.5 |
| Self-owned pool Docker member | arm64 / 910B4 | 9.0/driver | 9.0 / Docker | Platform API dGPU with two replicas on one card, real inference/chat, overcommit rejection, full-GPU chat | Supported; complete primary acceptance for platform dGPU |
| Self-owned 910C / Docker | arm64 / 910C | 9.0/driver | A3 images / Docker | Rows marked Docker below completed platform deployment, health/model discovery and real chat | Supports only the individually listed model, topology and runtime combinations; results do not extend to runveil or untested specs. Phy-ID 0 on `910c_1` was excluded by per-die checks |
| Self-owned pool nerdctl member | arm64 / 910B4 | driver-only, 26.1.1 | Embedded probe v2 / nerdctl | Production Engine + Driver maps physical device 1 to container logical device 0 and returns exactly 4 GiB; disabling the hook returns 32 GiB and fails the probe | Node-level dGPU probe passes; this check loads no workload image, model, or inference engine |
| Temporary Docker daemon on self-owned bare metal | arm64 / 910B4 | driver-only, 26.1.1 | Embedded probe v2 / Docker 27.5.1 + Ascend runtime | Production Engine + Driver maps physical device 1 to container logical device 0; the 4 GiB quota and unhooked 32 GiB negative control both behave as expected | Verifies v2 container-local DSMI mapping; platform inference was not rerun in this check |
| Passthrough VM / runveil | arm64 / 910B4 | driver-only, 25.5.1 | Embedded probe v2 / runveil, without the vcann-rt hook | Physical device 7 maps to logical device 1, matching production inference injection; HBM queries report the actual 32 GiB card and fail a 4 GiB expectation | Device mapping only; device-share remains False and the driver cannot enable it in the VM, so this is not a dGPU pass |
| Self-owned bare-metal runveil (physical device IDs 2–7) | arm64 / 910B4 | driver-only, 26.0.rc1 | Embedded probe v2 / runveil | Production Engine + Runner reports 4 GiB on physical device 7; disabling the DSMI hook reports the full 32 GiB and fails the probe | Node-level dGPU probe passes; old v1 reproduces `rc=87 total_kb=0` |
| Same self-hosted bare-metal runveil node | arm64 / 910B4 | driver-only, 26.0.rc1 | 8.5.1 / vLLM 0.18.0 / runveil | Default agent path: platform API two replicas sharing one card, gateway chat, per-instance over-quota rejection, then whole-GPU deployment and chat | Passed; each quota is 8 GiB and both 9 GiB allocations are denied. Image: `gpuworks/gpuworks-runner:cann8.5-910b-vllm0.18.0-r2`; automatic compatibility applies only to the qualified ARM64 TSD library described below |
| Same self-owned bare-metal runveil | arm64 / 910B4 | driver-only, 26.0.rc1 | 9.0 / vLLM 0.20.2 / runveil | Production Engine + runveil Driver selects the device automatically; Qwen3-0.6B with an 8 GiB quota returns HTTP 200 for real chat | Single-instance dGPU inference passes. The container reports 8589934592 bytes, a 9 GiB allocation is rejected, and subsequent chat returns `Hello! 😊`. This test did not use the platform API, scheduler, or gateway |

“Outer 8.5 and inner 8.5 have been tested” refers to the Gitee x86_64 environment in the table: there is a record of a real platform deployment reaching healthy, but the current repository does not retain a successful chat response, so the matrix does not mark complete chat as passed. The next step is to reuse the same instance in the existing x86_64/CANN8.5 rental test case to add chat verification, avoiding another rental.

## 910C Catalog Contracts and Tested Boundaries

Checks on 2026-10-22 confirmed that a runner image carries operators for only one chip family: running `cann9.0-910b-*` on a 910C die fails in `AclOpKernelInit`, while the corresponding `cann9.0-a3-*` build succeeds. A template's predicate and pinned image must therefore describe the same family. Generational inheritance ([ADR 0009](../adr/0009-flat-spec-catalog.md), item 10) still applies, but the successor has its own spec and image instead of merely broadening the original predicate.

- The catalog currently contains **88 inherited A3 specs** and **111 specs whose predicates match 910C**. Contract tests check the arguments, weights and image relationship between each 910B original and its A3 successor, and verify that every 910C-claiming spec resolves to an A3 image. These are catalog contract counts, **not per-spec real-hardware pass counts**; hardware results are recorded only for the individual rows below.
- The 950 family does not use this inheritance. The only current 950 spec is the single-card 950PR profile of `qwen/qwen3.8-27b`, using the corresponding 950 image and based on measurements from 2026-10-07. There is no tested 950DT profile.
- MiniMax-H3's SGLang diffusion path still requires CUDA full-loop denoise, so it declares only 910B4 and does not claim 910C. The Gemma-3-4B-IT and Gemma-3-27B-IT A3 SGLang profiles use `--disable-cuda-graph` to avoid an NPU graph-capture hang; the 4B model also has a working A3 vLLM profile. Both sizes have passed text and real-image chat.

### Frontier Model Catalog and Real-Hardware Status

Having a catalog spec means that pinned weights, an image and launch arguments exist; it does not mean that real-hardware acceptance has completed.

| Model | Current Ascend Catalog | 910C Results and Boundaries |
|---|---|---|
| GLM-5.3 (non-Flash) | **No Ascend spec** | Upstream has an A3 recipe, but this profile lacks real-hardware evidence, so no A3 spec is published |
| GLM-5.3-Flash | A3 TP16/EP16, W8A8, INT8 KV, 1M; standard plus graph-16, graph-8 and DFlash16 custom profiles | Health, `/v1/models` and real chat passed with native Docker; all three custom profiles also reached running and returned HTTP 200 for chat; runveil/PRoot has not passed |
| DeepSeek-V4.1-Flash | A3 DP4 × TP4, W8A8, 150K | Health, model discovery and real chat passed with native Docker |
| DeepSeek-V4.1-Flash-1017 | Independent 1017 W8A8 weights, A3 DP4 × TP4, 150K | Health, model discovery and real chat passed with native Docker; does not share the original version's weight snapshot |
| DeepSeek-V4-Flash-0731 | A3 DP4 × TP4, W8A8 + DSpark, 1M | Health, 1M `/v1/models` and real chat passed with native Docker |
| MiniMax-M3 | A3 DP4 × TP4, 133K | All 16 workers, health and real chat passed with native Docker |
| Qwen3.5-397B-A17B | A3 TP16/EP16, W8A8 + MTP, 133K, eager | Health, `/v1/models` and real chat passed with native Docker; eager mode avoids the currently missing `causal_conv1d_update_npu` graph path |
| Qwen3-235B-A22B-W8A8 | A3 TP16, W8A8, vLLM 0.26, eager | Capturing the default ACL graphs concurrently on 16 ranks raised `EE1023 no notify resource`; `--enforce-eager` then passed health and real chat with `Qwen3-235B-910C-ok` |
| Qwen3.8-Flash-Next | Experimental A3 TP8, W8A8-MTP, 135168 context | Health, `/v1/models`, real chat and automatic placement passed with Docker |
| Qwen3.8-27B | 910B spec; inherited W8A8 A3 spec; measured BF16 A3 spec | W8A8 TP2 text requests passed; BF16 TP2 text and real-image requests both returned HTTP 200. Both used native Docker; this verifies the API and multimodal paths, not output quality |
| Qwen3.6-35B-A3B-W8A8 | A3 TP2 W8A8; vLLM 0.30 eager and SGLang profiles | vLLM 0.26 lacked `linear_attn` during KV-cache initialization; the runner vLLM 0.30 profile with graph capture disabled started successfully and returned the exact chat marker. The SGLang profile also passed |
| QwQ-32B-W8A8 | Original A3 SGLang profile; A3 TP2 vLLM 0.30 eager profile | The original SGLang runner omitted the `quant_linear` keyword; with an idempotent template patch, TP2 startup, graph capture and real chat all passed. The vLLM profile also returned the exact `QwQ32B-VLLM-A3-910C-ok` marker |
| Qwen3-14B-GGUF | A3 TP2 SGLang with an explicit Q4_K_M checkpoint and eager mode | Health, weight loading, KV cache and chat HTTP 200 passed; the fixed-token prompt still filled the 64 to 96 token budget with reasoning text, so this is an engine and API pass only |
| Qwen3-1.7B | Newly measured A3 TP1 BF16 SGLang spec | Weights were downloaded on `.29` and peer-copied to `.30`; evaluation, runner startup and an exact-match chat request passed |
| Qwen3-4B | Newly measured A3 TP1 BF16 SGLang spec | Evaluation, runner startup and an exact-match chat request passed; the catalog previously had only NVIDIA, PPU and Hygon profiles |
| PaddleOCR-VL-1.6 | Measured A3 TP1 BF16 vLLM spec | An OCR request against a real 640×220 text image returned both lines verbatim; the template started and served with runner `cann9.1-a3-vllm0.26.0rc1-r2` |
| Qwen2.5-Coder-0.5B-Instruct | Newly measured A3 TP1 BF16 SGLang spec | Evaluation, runner startup and an exact-match chat request passed; the catalog previously had only NVIDIA and PPU profiles |
| Qwen2.5-Coder-1.5B-Instruct | Newly measured A3 TP1 BF16 SGLang spec | Both the API and the Chrome template page completed evaluation, deployment, running state and an exact-match Playground response; the catalog previously had only a PPU profile |
| SmolLM2-360M-Instruct | Newly measured A3 TP1 BF16 SGLang spec | Evaluation, runner startup and a chat request returning HTTP 200 passed; the tiny model followed the fixed-token prompt poorly |
| GPT-OSS-20B | Newly measured A3 TP2 BF16 vLLM spec | Both the API and the Chrome template page completed evaluation, deployment, running state and exact-match chat; eager mode avoids the NPU graph path, and the template points at the runner's vendored tiktoken vocabulary for offline Harmony tokenization |
| Janus-Pro-7B | A3 TP1 BF16 SGLang | Exact-match text chat passed. Both an API 1×1 placeholder and a real 640×220 PNG in Chrome produced 576 image tokens and returned HTTP 200, but the model did not describe either image meaningfully. This confirms the multimodal transport path, not visual quality |
| Phi-4-multimodal-instruct | A3 TP1 BF16 SGLang | After declaring the image modality, a real 448×448 image request returned HTTP 200 and `White` |
| MiniCPM3-4B | A3 TP1 BF16 SGLang, eager | The original graph path killed its child with SIGQUIT; with `--disable-cuda-graph`, evaluation, runner startup and text chat passed |
| MiniCPM-V-4.5-AWQ | A3 TP1 INT4 SGLang, eager | With AWQ explicitly skipping the unquantized `vpm` and `resampler`, weight loading, KV cache, health, exact-match text chat and a real 448×448 image request all passed |
| Trinity-Mini | A3 TP2 BF16 SGLang with a pinned ModelScope revision | The Hugging Face file listing timed out; after switching to the complete ModelScope snapshot, evaluation, deployment, running state and chat passed, with the model returning `TrinityMini-910C-ok` |
| Qwen3-Omni-30B-A3B-Instruct | A3 TP4 BF16 SGLang, eager | The original graph path stayed in health check; with `--disable-cuda-graph`, the runner started and text chat returned `Qwen3Omni30B-910C-ok` |
| Qwen3-30B-A3B-Instruct-2507 | A3 TP4 BF16 SGLang, eager | The original graph path did not become healthy in the observation window; with `--disable-cuda-graph`, startup and exact-match chat passed |
| Qwen3-TTS-12Hz-1.7B-CustomVoice | A3 TP1 BF16 vLLM-Omni r3 | With an explicit deploy config, it generated a non-silent 24 kHz WAV; Qwen3-ASR loopback returned `GPUWorks 910c Speech Validation.` |
| Qwen3-TTS-12Hz-0.6B-CustomVoice | Newly measured A3 TP1 BF16 vLLM-Omni r3 | It generated a non-silent 24 kHz WAV; Qwen3-ASR recognized the substance of the sentence |
| Ming-omni-tts-0.5B | A3 TP1 BF16 vLLM-Omni r3 | It generated a non-silent 44.1 kHz WAV; Qwen3-ASR recognized the substance of the sentence |
| Gemma-4-31B-IT | A3 TP4 BF16 vLLM | With the checkpoint's own chat template, the runner started and text plus real-image chat returned HTTP 200 |
| Gemma-4-26B-A4B-IT | A3 TP4 BF16 vLLM, eager | With the checkpoint's own chat template and graph capture disabled, weight loading, KV cache and health passed. Text returned `Gemma426B-910C-ok`; a real 448×448 image request returned `White` with 258 image tokens |
| Gemma-3-4B-IT / Gemma-3-27B-IT | A3 SGLang / vLLM for 4B; A3 SGLang for 27B | Both TP2 4B engines and the TP4 27B eager profile completed text and real-image chat; the SGLang templates disable NPU graph capture by default to avoid startup hangs |
| Kimi-K2.6-W4A8 | A3 TP4 × DP4, 16 dies; W4A8 + DFlash15 | All 147 files and 499.1 GiB passed full SHA-256 verification. With TP4 declared explicitly, preflight passed, the deployment reached running, and real chat returned the `Kimi-K2.6-910C-ok` marker |
| Kimi-K3 | None | The upstream minimum recipe uses four A3 nodes; there is no catalog spec |

The A3 vLLM profile for Qwen3-VL-8B-Instruct pins `--max-model-len` to
**196608**, the measured safe value for one 64 GiB 910C die. TP2 and larger
deployments have passed with the full 262144-token context, but vLLM's
`max_model_len` is a structured integer launch parameter that a template
cannot vary by device count. Multi-device deployments must set
`max_model_len=262144` in the deployment quick parameters. The template keeps
the TP1-safe default rather than exposing a default that can exhaust one die.

The A3 profiles of DeepSeek-V4.1-Flash and its 1017 variant explicitly declare **512 GiB of `/dev/shm`** and `memlock_unlimited: true` for two approximately 103 GiB local DP shared Engram tables. Do not duplicate a Docker memlock argument in template `args`. Hardware checks confirmed that 128 GiB cannot register the second table and 256 GiB can; the catalog uses a 512 GiB isolated tmpfs as the common specification.

Preflight checks use the final effective command. They reject a launch only when the platform can prove it must fail and the user cannot work around it; combinations the platform merely cannot prove produce a warning and the deployment is still created.

- **Rejected at create (400)**: a compat image cannot be combined with `pre_start_argv`; V1 also rejects an additional `pre_start_script` (V2 may compose scripts); the merged pre-start script cannot exceed the agent's 64 KiB limit. These apply on udocker/runveil nodes (the compat script is injected only there) and are judged with the image resolved for that node. With automatic placement and no node chosen yet, nothing is checked at create; the controller reports a clear reason once it picks a node.
- **Evaluate warnings (`warnings[]`, never blocking)**: the rc2 A3 V1 compatibility rendezvous is single-node only, so a multi-node deployment is advised to set `VLLM_USE_V2_MODEL_RUNNER=1`, use Docker/nerdctl, or deploy on one node; V1 combined with data parallelism above 1 has not been verified on real runveil hardware; a manual shell on a compat image (including `/bin/sh -c` inside JSON argv) cannot be matched to the actual runner; Engram `cpu_offload + dp_shared_memory` on udocker/runveil uses the host's `/dev/shm` and ulimits directly, which the platform cannot see, so it is advised to provide about 512 GiB of shared memory and unlimited memlock or use Docker/nerdctl; an Engram configuration that cannot be established is treated the same way.
- **Passed silently**: manual commands that do not involve Engram (variables, `&&`, pipes, redirects, `["sh","-c",...]`) are neither checked nor warned about, on any vendor. When the command text mentions `engram` and can be parsed statically, shared memory and memlock are derived by the same rules as engine mode; when it mentions `engram` but cannot be parsed, only a warning is shown and nothing is derived.

Some inherited specs also have individual hardware results: health/chat for GLM-4.6V-Flash, Qwen3.6-35B-A3B, Qwen3.6-27B-W8A8, Qwen3.5-122B-A10B and Qwen3-Next-80B-A3B-Instruct; target-plus-draft startup/chat for Qwen3-8B EAGLE3; and A3 image selection, health and a speech request for Qwen3-TTS. These results cover only the named specs and must not be extended to every inherited spec.

Every new Ascend template must pin a complete immutable weight snapshot. A per-file `Revision` returned by the ModelScope file API is not necessarily a complete repository snapshot; one file's revision must not be presented as the snapshot of the entire model.

### Startup and Runtime Boundaries

The GLM-5.3-Flash Docker result applies only to the pinned rc2 A3 image under native Docker. Under runveil/PRoot, CANN Fill/Transpose tiling registration failures or thread starvation remain even after bypassing the Python operator and group-initialization issues, so this spec does not claim runveil support. Docker does not inject the PRoot compatibility script. The MiniMax-M3 Docker result likewise does not extend to runveil.

Shared Ascend container device nodes (`davinci_manager`, `devmm_svm`, `hisi_hdc`, `ummu` and `uburma`) are passed through only when present on the host; every selected `/dev/davinciN` must be passed to the container. Multi-node specs do not hardcode interface names. The agent discovers the interface from the node's advertised IP and fills the collective-communication variables, while explicit settings retain precedence.

Nodes with host CANN check each die by creating an ACL context and allocating 4 KiB. Only failure explicitly attributable to that die marks it `runtime_unusable`. A driver-only node without `libascendcl.so` skips the check rather than treating a missing tool as a hardware fault. See [Runtime-Unusable Cards](../how-to/troubleshooting.md#某张卡运行时不可用或驱动初始化失败) for failure classification, backoff and diagnosis.

## Coverage Levels

1. **Detection**: architecture, NPU, driver/toolkit, runtime, and the reason for dGPU capability status.
2. **Device injection**: the specified physical NPU is visible inside the inner container.
3. **dGPU behavioral probe**: use the production `Engine` to generate the vcann-rt configuration and start the real runtime; the embedded 2.5 MiB `/probe` calls `dsmi_get_hbm_info` and asserts that the visible total GPU memory is exactly 4 GiB. This level deliberately does not load a workload image or model.
4. **Platform dGPU inference**: create deployments and instances through the GPUWorks API and verify multiple replicas on one card, quotas, and real inference.
5. **Complete full-GPU chat**: deploy through the platform API and receive a real response from `/v1/chat/completions`.

The self-owned pool covers levels 1–5. Because Gitee rental containers cannot run `npu-smi set -t device-share`, they cover only detection and full-GPU container-in-container chat. Cloud-side permission denial must not be presented as a dGPU pass.

## Corrected Misdiagnoses

### Behavioral Probe Always Queried Logical Device 0

The old probe always called `dsmi_get_hbm_info(0)`. PRoot does not provide OCI device isolation, and DSMI numbering depends on the devices visible to the driver: it may preserve host IDs or map physical devices 5 and 7 to logical devices 0 and 1. Always querying 0 may target a missing device or a different card; directly using the physical ID can also be wrong. The container runtime name alone therefore cannot determine the query ID.

The agent now passes the selected physical device ID through `GPUWORKS_PARTITION_PROBE_DEVICE`. The probe first resolves the logical ID in the current runtime environment using `dsmi_get_logicid_from_phyid`, then queries that device's HBM. A missing mapping interface, failed mapping, or memory total that differs from the quota causes failure, with no fallback to device 0. Diagnostics record both physical and logical IDs.

Follow-up checks on 2026-10-24 confirmed that v2 maps physical device 1 to logical device 0 in single-device Docker + Ascend runtime and nerdctl containers, and physical device 7 to logical device 1 under runveil in the passthrough VM. The OCI checks use the production Engine with a 4 GiB partition and an unhooked negative control. The VM check queries whole-card HBM only and does not bypass device-share prerequisites. These results do not establish support for untested drivers or 910C.

The embedded probe image is upgraded to `gpuworks/partition-probe:v4` so that an upgraded agent does not keep using cached v1/v2/v3. This fix covers the node-level behavioral probe; actual inference support remains limited to the tested matrix.

### runveil Inference Device Selection and ARM64 Path Resolution

runveil preserves the host device view and cannot reuse Docker's compact device IDs. The agent now combines physical card/chip addresses from `npu-smi info` with `Chip Logic ID` from `npu-smi info -m` to generate `ASCEND_RT_VISIBLE_DEVICES` and `NPU_VISIBLE_DEVICES`. Missing or conflicting mappings prevent startup, with no fallback to 0. Real-hardware tests verified production injection for physical 7 → logical 7 on bare metal and physical 7 → logical 1 in a passthrough VM.

Previously, the ARM64 PRoot syscall table lacked `faccessat2` (439), causing directory checks from glibc `realpath()` to use host paths and incorrectly report `ENOENT`. CANN consequently could not find platform configuration or operator registration libraries; CANN 9.0 reported `361001` for `ZerosLike` / `aclnnInplaceZero`. The error also reproduced with dGPU disabled, so it did not establish that vcann-rt was incompatible with runveil.

The syscall mapping is now added and the embedded ARM64 PRoot rebuilt. The build gate checks `faccessat2` through guest binds, relative dirfds, missing paths, and `realpath` with symlinks, parent directories, and trailing slashes. The original binary failed; the new binary passed on real ARM64 hardware. With the updated embedded asset and automatic device mapping, CANN 9.0 / vLLM 0.20.2 passed single-instance dGPU inference, over-quota allocation rejection, and chat after rejection. Coverage remains as listed in the matrix above.

### CANN 8.5.1 DCMI detection and TSD device translation

CANN 8.5.1 has two initialization issues under runveil. When the image lacks a container marker and `systemd-detect-virt`, the DCMI detection child can deadlock in CANN dump destructors. Runveil now automatically binds `/.dockerenv` for Ascend workloads to avoid that detection branch.

Separately, after the runtime translates selected logical device 7 to user device 0, TSD queries the hardware version before translating back, leading to `TsdOpen` / `507033`. Runveil now loads an embedded compatibility library only for the qualified affected ARM64 CANN 8.5.1 `libtsdclient.so` (SHA256 `4f6648daad462247e23782946b1a10b3d224d42d99aa2f468bd6b45c2a08676a`). Hardware-version queries from that TSD are translated through `ASCEND_RT_VISIBLE_DEVICES` before calling the real driver. The library neither fabricates success nor rewrites every device-0 query. Unknown or fixed libraries do not receive this translation; a “CANN 8.5” version label alone does not establish compatibility.

Compatibility files belong to the disposable container directory; host drivers and shared images remain unchanged, and existing `LD_PRELOAD` entries are preserved. Launch, interactive terminals and non-interactive `Exec` restore the same workload environment and mounts, including from the persisted spec after an agent restart. To use this fix, upgrade the agent and recreate the workload.

### “No Runtime”

The historical ARM fixture reported “no available docker/udocker runtime on the node.” This was neither a CANN declaration nor a hardware incompatibility. Python in that outer image is at `/root/miniconda3/bin/python3` or `/opt/conda/bin/python3`, outside the default `PATH` of a non-interactive shell, so the old detection logic did not enable embedded udocker/PRoot.

Detection now checks these absolute paths and correctly identifies the runtime as `udocker`. This detection is now relevant only for `GPUWORKS_UDOCKER_LAUNCHER=udocker`: the default runveil launcher does not require Python.

### `libvruntime.so` Is Distinguished Only by CPU Architecture

Historically, artifacts for the same architecture in the CANN 8.5.1 and 9.0.0 directories were byte-for-byte identical: the arm64 SHA256 was `3dbe727a…d3d49` for both, and the amd64 SHA256 was `57a9641a…9d010` for both. They came from the same pinned ubs-virt source and patch, not two separate runtime ABIs.

The repository now retains one copy each of `linux-arm64/libvruntime.so` and `linux-amd64/libvruntime.so` as the sole canonical path for each architecture. At runtime, only these two paths are accepted; neither outer nor inner CANN versions are read, and there is no fallback across CANN versions.

After the agent upgrades and successfully installs assets at the canonical paths, it deletes precisely the old `cann-8.5.1/` and `cann-9.0.0/` state directories. CANN versions remain only as records of tested environments and compatibility warnings.

A passing node behavioral probe proves only that the “node + runtime + this SO (shared library)” dGPU path works; it does not automatically prove that any workload image works. Workload image support must be determined by a dedicated image-specific behavioral probe using that image's userspace environment. Until this probe is completed, the existing version matrix can only be marked warning/unknown and cannot establish support. The PRoot `platform_config` failure on ARM with outer 9 + inner 8.5 demonstrates why these two conclusions must remain separate.

`PartitionProfile.Verified` is a verification flag in the code and cannot replace the real-hardware matrix above:

- **Definition**: the profiles table in `internal/accelerator/ascend.go`, a static Boolean hardcoded by backend/framework/version prefix/architecture.
- **Meaning**: only indicates that the combination is marked in the code as verified in a preset real-hardware environment. When false, `partitioncap` appends “This runtime combination has not yet been verified in a preset real-hardware environment.”
- **Maintenance**: maintained manually with code commits; it does not automatically synchronize with this document's real-hardware matrix. For example, CANN 9.0.x × x86_64 has a complete chat record, but this field is not yet marked Verified.
- **Validation boundary**: does not indicate additional build/provenance validation of the corresponding `libvruntime.so` asset itself; that validation is performed independently by `build/runtime-assets/verify.sh`.

Use the real-hardware matrix above as the basis for support conclusions.

## CI Ownership

- `l4-gitee-ascend-partition-capability`: rents an x86_64/CANN8.5 instance to verify node capability detection and known device-share restrictions, without downloading workload models.
- `l4-gitee-ascend-deploy-chat`: explicitly runs full-GPU deployments and real chat on x86_64/CANN9 and arm64/CANN9 at low frequency.
- `l4-ascend-pool-partition-inference`: treats `.31/.32` as a capability pool rather than fixed nodes; verifies platform API dGPU inference/full-GPU chat, nerdctl device injection, and the embedded behavioral probe.
- The nerdctl helper is an approximately 9 MiB gzipped arm64 asset in the repository. CI and GPU nodes do not run `go build`; when updating it, build only on the internal build machine (see `secrets/test-environments.md`) and then check it in.

For failure debugging, preserve the last environment by default: use `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` for cloud rentals and `GPUWORKS_KEEP_ON_FAILURE=1` for the nerdctl helper. After identifying the cause, clean up the exact instances, containers, and temporary directories from that run.

## Cards and Chips: Identification and Numbering

Each chip is an independent device with its own `/dev/davinciN`, PCIe function, HBM, and health status. GPUWorks reports and schedules by chip.
Card layout is determined from the `npu-smi info` table, consistent with zstack-utility's `resolve_handler`:

- **One chip per card (910B)**: each NPU has one chip row, and the header has no `Phy-ID` column. The device number is the NPU ID.
- **Multiple chips per card (910C)**: the header has a `Phy-ID` column, multiple Chip IDs appear under the same NPU, or the chip name contains `910C`.
  The 910C chip name column shows `Ascend910` (without a suffix), with two chips per NPU (Chip 0/1); GPUWorks records these chips as model `910C`.
  The first-generation 910 is a single-chip card with the same name and is not identified as 910C. The device number is the chip's Phy-ID; chips without a Phy-ID cannot be mapped to a device and are not reported.

The basis for using Phy-ID consistently as the device number is read-only verification on two 910C hosts on 2026-10-02:

| Use | Numbering |
|---|---|
| `/dev/davinciN`, `ASCEND_VISIBLE_DEVICES` (Ascend Docker Runtime), nerdctl `--device` | Phy-ID |
| `npu-smi info -t topo` matrix | `Phy-ID<n>` rows and columns (`SIO` between chips and `HCCS_SW` between cards; both treated as high-speed interconnects) |
| `hccn_tool -i` | Phy-ID (zstack-utility `fedf490a2` also switches to physical IDs) |
| `npu-smi info -t health/board/device-share -i <npu> -c <chip>` | NPU ID + Chip ID, converted from the table by the agent |
| `ASCEND_RT_VISIBLE_DEVICES` inside the container | Compact numbering starting at 0 |

For 910C, Logic ID in `npu-smi info -m` equals Phy-ID and is not compacted when cards are missing (on a host missing NPUs 3–6, NPU 7's two chips remain 14 and 15).
All `/dev/davinci0..15` devices exist and map one-to-one to Phy-ID (`Phy-ID = 2 × NPU ID + Chip ID`).
For 910B, NPU ID equals Phy-ID, so single-chip cards continue to use NPU ID.
