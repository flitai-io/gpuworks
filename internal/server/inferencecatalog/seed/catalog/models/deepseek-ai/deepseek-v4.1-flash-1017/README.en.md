# DeepSeek-V4.1-Flash-1017

The mixed FP8/FP4 checkpoint is about 510.3 GB. This template carries recipes for three engines:

- **SGLang**, from the [SGLang cookbook](https://docs.sglang.io/cookbook/autoregressive/DeepSeek/DeepSeek-V4_1). It needs the dedicated `lmsysorg/sglang:dev-dsv41` image (CUDA 13); the V4 image or older runners will not work.
- **vLLM (NVIDIA)**, from the [vLLM recipe](https://github.com/vllm-project/recipes/blob/main/models/deepseek-ai/DeepSeek-V4.1-Flash.yaml). It needs vLLM 0.31.0 or later; Ascend uses the separate vLLM-Ascend image below.
- **DeepInfer**, DeepSeek's open-source inference framework [DeepInfer](https://github.com/deepseek-ai/DeepInfer) at commit `774ce24`. The image `gpuworks/deepinfer:774ce24-r9` adds an OpenAI-compatible HTTP server (DeepSeek's deepseek-recipe protocol layer), a single-node local Engram fetch and a multi-node request relay. It serves text input only; the open-source release has no prefill/decode disaggregation.

## Profiles

| Profile | Hardware | Notes |
| --- | --- | --- |
| `fp4-sglang-nvidia-h200-tp8` | 8× H200/H20 | Upstream H200 low-latency command, 1M context, no speculation |
| `fp4-sglang-nvidia-h200-tp8-throughput` | 8× H200/H20 | Upstream H200 high-throughput command: up to 256 running requests |
| `fp4-sglang-nvidia-h200-tp4-engram-host` | 4× H200/H20 (141 GB) | `SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=1` moves the ~196 GB Engram tables to host memory, leaving ~73 GB of backbone per GPU; needs ~250 GB of host RAM; 1M context |
| `fp4-sglang-nvidia-h200-tp4-engram-host-dspark` | 4× H200/H20 (141 GB) | The above plus DSpark; context capped at 256K (at 1M the decode CUDA-graph capture needs another 12 GiB that does not fit) |
| `fp4-sglang-nvidia-b200-tp4` | 4× B200/B300/GB300 | Upstream Blackwell low-latency command with DSpark |
| `fp4-sglang-nvidia-b200-tp4-throughput` | 4× B200/B300/GB300 | Upstream Blackwell high-throughput command: no DSpark, up to 256 running requests |
| `fp4-vllm-nvidia-h200-tp4-engram-host` | 4× H200/H20 (141 GB) | Upstream H200 command: TP4 with the Engram tables in host memory (`--engram-config {"cpu_offload":true}`) |
| `fp4-vllm-nvidia-b200-tp2` | 2× B200 | Upstream B200 command: TP2, Engram tables in host memory (THP), for low concurrency |
| `fp4-vllm-nvidia-b300-tp2` | 2× B300 | Upstream B300 command: TP2, Engram tables in host memory, up to 256 running requests |
| `fp4-vllm-nvidia-blackwell-dep8` | 8× B200/B300 | Upstream single-node DEP command: data parallel 8 with expert parallel, for throughput |
| `fp4-deepinfer-nvidia-blackwell-ep` | 4 or 8× B200/B300 (one node) | Expert parallel and attention data parallel over every GPU; automatic placement prefers the whole 8-GPU node; no RDMA NIC needed |
| `fp4-deepinfer-nvidia-blackwell-ep-multinode` | B200/B300, 4 or 8 per node, 2+ nodes | Expert parallel across nodes; needs a host-RDMA compute network |
| `int8-vllm-ascend-a3-tp4-dp4` | A3/910C × 16 logical dies | 1017 W8A8, DP4 × TP4, 150K context, ~781.5 GB of weights; Engram tables in host memory |

The two 4× H200 SGLang profiles are platform additions; every switch they use comes from the upstream documentation.

H20 reuses these profiles without changing their spec names. Under the default parameters, the 96 GB variant uses either SGLang 8-GPU profile; the three 4-GPU profiles still fail memory admission, while the 141 GB variant can be evaluated with them. Matching the model name does not bypass GPU-count or reported-memory checks. The H200 measurements below do not validate H20; H20 inference has not been tested on hardware.

## Ascend A3 prerequisites and measurements

GPUworks selects `int8-vllm-ascend-a3-tp4-dp4` by default on 910C. This
profile needs 16 logical 910C NPUs on one node, each with at least 64 GiB.
It declares 512 GiB of `/dev/shm` and `memlock_unlimited`. Keep enough host
RAM free to register and pin both shared Engram tables.

For offline deployment, import
`gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1` for the target
node's CPU architecture. When exporting from the Quay mirror, preserve this
canonical reference in the archive. If the node already has that tag, compare
its image ID with the published runner for the same architecture. If the IDs
differ, replace the cached image. For native Docker, use `docker load -i <verified-runner.tar>` against the
same daemon the GPUworks agent uses. To replace the image through GPUworks
using an archive URL already configured for this image, send
`{"source":"url","archive_url":"<verified archive URL>","force":true}` to
`POST /api/v1/images/{id}/nodes/{uuid}/sync` and wait for the transfer to
finish. Ordinary archive sync skips a node that already reports the image
as present. After either method, inspect the canonical tag again and verify
that its image ID matches the runner for the node's architecture.
Existing deployments retain the image sources saved at creation. Create a deployment
from the updated template to use its corrected sources for future pulls.

Prepare the complete ModelScope snapshot of `Eco-Tech/DeepSeek-V4.1-Flash-1017-w8a8`
at revision `12b85455a58c7df345c1abc43934624a76bac576`. Attach the target node's
snapshot directory to an existing ModelScope record only if its repository ID
and revision both match. If no record matches, register a ModelScope model at
that revision and attach the directory, or register the verified directory as a
local model and set its source ID to the full repository name above.
Select the registered model and the W8A8 profile. Check that deployment
evaluation shows the weights are available on the target node, then send a
chat request once the instance is ready.

The GPUworks Ascend catalog covers this 910C W8A8 profile. Eco-Tech W4A8
requires a separately validated image and profile. The latest upstream 910C
example uses model runner V2 and INT8 Engram.
[vLLM-Ascend deployment guide](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/DeepSeek-V4.1-Flash.html#51-single-node-colocated-deployment)

On 2026-10-23 the profile passed platform health, `/v1/models` and a real chat
request on 16× 910C with native Docker. That result does not extend to runveil.

## DeepInfer prerequisites

- **Single node**: the Engram tables (~197 GB) live in the container's `/dev/shm`; the template declares 450 GiB of shared memory, so the host needs that much free RAM. The template declares `memlock_unlimited`, so the platform starts the container with `--ulimit memlock=-1:-1` to pin that host memory. Each DP rank takes up to 128 concurrent requests, 1024 on 8 GPUs.
- **Multi-node**: every node sits on the same host-RDMA compute network with GPUDirect-capable NICs; the platform adds `/dev/infiniband`, `IPC_LOCK` and an unlimited memlock for that network, so no privileged mode is needed. Only rank 0 serves; the other nodes take requests over `MASTER_PORT+7`.
- Both profiles declare `api.metrics`, so the monitoring center shows running/waiting requests, token counters and TTFT/TPOT distributions.

## Measured

### SGLang (2026-10-20, Vast 8× H200 SXM container node, runveil)

~1.67k input and exactly 512 output tokens per request, against the engine on the node; concurrency 16 after warm-up:

| Profile | Concurrency | Total output | Per stream | Mean TTFT | Mean TPOT | Start to ready |
| --- | --- | --- | --- | --- | --- | --- |
| 8× H200 | 1 | 66 tok/s | 67.5 tok/s | 0.21 s | 14.8 ms | 18 min |
| | 16 | 329 tok/s | 20.6 tok/s | 0.52 s | 47.7 ms | |
| 4× H200 + Engram host table | 1 | 86 tok/s | 89.6 tok/s | 0.22 s | 11.2 ms | 20 min |
| | 16 | 306 tok/s | 19.5 tok/s | 0.54 s | 51.4 ms | |
| 4× H200 + Engram host table + DSpark (256K) | 1 | 112 tok/s | 116.8 tok/s | 0.22 s | 8.6 ms | 22 min |
| | 16 | 502 tok/s | 32.5 tok/s | 0.39 s | 30.8 ms | |

Chat, thinking mode (`chat_template_kwargs.thinking=true`) and tool calls passed; DSpark accept length 2.4–3.0.

DSpark also starts at 1M context if `--max-running-requests` is capped at 64 (the 12 GiB scales with max running requests × context length), but measured 80 tok/s at concurrency 1 and 200 tok/s at 16, slower than without DSpark, so no profile is offered for that combination.

### DeepInfer (2026-10-21/22, Nebius, platform evaluations)

Random data, ~4.2k input tokens (chat template included) and exactly 1024 output tokens (`ignore_eos`), load sent by the node agent, tokenizer loaded from ModelScope:

| Profile | Hardware | Concurrency | Output throughput | TTFT p50 | TPOT p50 |
| --- | --- | --- | --- | --- | --- |
| Single node | 8× B200 | 8 | 937 tok/s | 0.23 s | 2.4 ms |
| | | 256 | 8,337 tok/s | 5.3 s | 7.8 ms |
| | | 1024 | 20,222 tok/s | 12.1 s | 10.6 ms |
| Single node | 8× B300 | 256 | 9,309 tok/s | 3.4 s | 6.4 ms |
| | | 1024 | 17,035 tok/s | 14.6 s | 20.0 ms |
| Multi-node | 2 nodes × 8× B300 (InfiniBand) | 256 | 3,718 tok/s | 17.6 s | 11.5 ms |
| | | 1024 | 12,513 tok/s | 17.9 s | 20.1 ms |
| | | 2048 | 13,495 tok/s | 66.7 s | 16.8 ms |

Every request succeeded. On this mixed prefill/decode load the cross-node prefill of the multi-node profile has a much lower per-GPU throughput than a single node, so the single-node profile is the better fit; the multi-node profile raises the concurrency ceiling (1024 per node).

## Notes

- With a host driver older than 580 the SGLang engine reports `No accelerator`: put the image's `/usr/local/cuda-13.0/compat` first in the container's `LD_LIBRARY_PATH` and set `TRITON_LIBCUDA_PATH` to it (`/usr/local/cuda/compat` is too old).
- On runveil (docker-less container nodes) SGLang sizes its thread pool to the host core count; ~3600 threads at TP8 stall PRoot while loading weights. Limit `OMP_NUM_THREADS` and friends to 4 (the platform does this by default for SGLang on runveil).
- vLLM tunes kernels on its first start, which can take more than 10 minutes; the recipes set `VLLM_ENGINE_READY_TIMEOUT_S` to 3600.

Undeclared AMD, Hygon, PPU and Apple combinations are outside this template's coverage; Ascend is limited to the 910C Docker profile above.
