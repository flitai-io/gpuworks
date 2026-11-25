# Qwen3.8-Flash-Next

[Official SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next). Native FP8 weights (~185.5 GB), single-node 4 H200/H20 or 8 MI350X/MI355X GPUs. Uses the dedicated recipe images. NVIDIA enables native MTP; AMD uses AITER.

Existing Apple GGUF and NVIDIA Strata profiles remain separate. The NVIDIA 4× H200 profile was measured on this platform (below); the AMD profile is untested. The Ascend profile uses vLLM-Ascend's experimental W8A8-MTP recipe: immutable revision `c76aa96a…`, eight 64 GiB A3 logical devices, TP8. 910B (A2) has its own single-node 4-card profiles, below. The NVIDIA/AMD FP8 recipe is not a substitute. Hygon/PPU recipes remain unconfirmed.

The 96 GB and 141 GB H20 variants share the 4-GPU profile; admission uses the capacity reported by the node. This describes the SGLang profile; H20 inference with SGLang has not been validated on hardware.

## vLLM profile (Hopper × 8, TP8)

vLLM is the only engine on this platform that supports sleep/wake, so H200, H20 (96/141 GB) and H100 80 GB also have one single-node, 8-GPU, TP8 vLLM profile, `bf16-vllm-nvidia-hopper-tp8`; other GPU counts are rejected. Arguments follow the [vLLM recipe](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next) (`--max-num-seqs 256`, the `qwen3_coder` tool parser, the `qwen3` reasoning parser, `VLLM_ENGINE_READY_TIMEOUT_S=1800`). The image is the platform's own `gpuworks-runner:cuda13.0-vllm0.30.0-r4` (vLLM 0.30.0), not the upstream nightly: it registers `Qwen4ExpForConditionalGeneration`, and the recipe states that stable vLLM >= 0.30.0 is supported.

**The weights are the BF16 `Qwen/Qwen3.8-Flash-Next`, not the FP8 used by the SGLang/AMD/PPU profiles above**: the repository is ~335 GiB (360.0 GB), about 42 GiB per card at TP8 (measured); the 185.5 GB figure above refers to the FP8 repository only. This is the combination that ran on hardware (TP8, no expert parallelism). The upstream vLLM recipe uses FP8 weights and, at TP8, needs `--enable-expert-parallel` (the FP8 128-wide quantization blocks do not split 8 ways); that combination has not been run on this platform, so it is not offered.

`--disable-custom-all-reduce` is required: measured on H20 with sleep mode on, FULL CUDA graph capture fails at `custom_all_reduce.cuh:164 'invalid argument'` without it, and works with it. Sleep off was not tested, so whether the flag is needed without sleep is unknown.

Memory per card (TP8, `gpu_memory_utilization` at the deployment default 0.9; the ~42 GiB of weights per card is measured, the other columns are derived from card capacity at 0.9 and are not measured):

| Card | Capacity | 0.9 budget | Weights | Budget minus weights (activations, CUDA graphs, KV, linear-attention state) | Platform admission requirement |
| --- | --- | --- | --- | --- | --- |
| H200 / H20 141 GB | ~140 GiB | ~126 GiB | ~42 GiB | ~84 GiB | ~47 GiB |
| H20 96 GB | ~95.6 GiB | ~86 GiB | ~42 GiB | ~44 GiB | ~47 GiB |
| H100 80 GB | ~79.6 GiB | ~71.6 GiB | ~42 GiB | ~30 GiB | ~47 GiB |

Context stays at the model's native 256K; `--max-model-len` is not set. The KV cache is small: of 48 layers only 12 use full attention, with just 2 KV heads, and the other 36 are linear attention with a length-independent state, so even 80 GB cards need no shorter context. H100 has the least headroom; if start-up runs out of memory, lower `gpu_memory_utilization` or `--max-num-seqs` first. The upstream recipe also says vLLM offloads the N-gram embedding to host memory by default on H100; this was not verified here, so host-memory use on H100 is unknown, and `--engram-config '{"cpu_offload":true}'` can be added explicitly if needed.

Upstream options that are not enabled: MTP speculative decoding, `--enable-prefix-caching`, FP8 KV and `--attention-config.indexer_kv_dtype fp8` (none verified on 0.30.0). Add them through the deployment form's extra arguments if needed.

## Measured (vLLM, 8× H20 141 GB, TP8, 2026-10-31)

Docker runtime, `gpuworks-runner:cuda13.0-vllm0.30.0-r4`, BF16 weights on NFS (~830 MB/s), sleep mode on, `--disable-custom-all-reduce`. Chat output was correct. With the weights in the page cache, start to ready took ~3.5 minutes; a cold read from NFS takes longer. Sleep/wake (L1, engine-reported): sleep 1.7-1.9 s, wake 2.9-3.7 s. L2 wake reloads the weights and took 489 s, so it is not practical. H100 80 GB, H20 96 GB and H200 were only checked offline (catalog and admission); they have not been run on hardware, and the headroom figures above are derived.

## Measured (4× H200, TP4, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking and tool calls passed; single stream ~272 tok/s over 1,500 output tokens, TTFT 0.71 s through the gateway, NEXTN accept length 1.7–3.95; ~9 minutes from start to ready. No multi-concurrency run yet.

## Measured (8× 910C, TP8, 2026-10-22)

Self-hosted 910C node, Docker runtime, vLLM-Ascend's `qwen3.8-next-a3` image,
pinned here by digest. Automatic placement left out the die whose ACL context
check fails and ran on the other eight. The deployment reached running;
`/health`, `/v1/models` and an OpenAI-compatible chat request all returned
HTTP 200 with a correct answer. No throughput or multi-concurrency run yet.

## Ascend 910B (A2) · 4 cards · TP4

One node with four 910B cards of at least 64 GiB each, TP4 with expert parallelism, on vLLM-Ascend's
official `v0.32.0rc1-openeuler` image (pinned by its multi-arch index digest; the NJU quay mirror and the
public gpuworks copy on Huawei SWR serve the same digest, since quay.io can be very slow from mainland China).
Four profiles:

| Profile | Weights (ModelScope, pinned revision) | Repository size | Speculative decoding |
| --- | --- | --- | --- |
| `int8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w8a8-mtp` `c76aa96a…` | about 245 GB | none |
| `int8-vllm-910b-tp4-mtp` | same | about 245 GB | MTP, 3 draft tokens |
| `w4a8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w4a8` `d75537fd…` | about 194 GB | none |
| `w4a8-vllm-910b-tp4-mtp` | same | about 194 GB | MTP, 3 draft tokens |

Both repositories carry their own BF16 MTP layer, so the MTP profiles download no separate draft. Arguments:
256K context, at most 16 sequences, 8192 tokens per batch, a fixed 20 GiB KV pool per card
(`--kv-cache-memory-bytes`; vLLM then ignores the memory fraction for KV), prefix caching
(`--mamba-cache-mode align`, `--block-size 4096`), up to 32 images per request, a decode-only ACL graph,
async scheduling off, the `qwen3` reasoning parser and the `qwen3_xml` tool parser. The MTP profiles capture
decode graphs at multiples of four (one target plus three draft tokens per sequence, up to 64 = 16 × 4) and
**let the drafter replay graphs too** (`enforce_eager: false`): vLLM-Ascend's 910C recipe runs it eager, and
this profile changed that on measurement, below.

**Host memory**: `--engram-config` keeps the 51B-parameter per-layer embedding table in host memory, one
mapping per rank (`dp_shared_memory=false`), so four ranks need about 96 GiB of free RAM on top of the page
cache for the weight files.

Measured (4× 910B4 64 GiB, ARM64, driver 25.5.0, 2026-11-19): W4A8 without MTP, with largely these arguments
(launched then from a hand-written command, and with at most 4 sequences). 18.65 GiB of weights per card, a 20 GiB KV pool (about 1.44M tokens, 5.48
concurrent 256K requests), decode graph capture 83 s, about 9.5 minutes from load to ready; chat requests were
answered, at about 25 tokens/s for one stream. The W8A8 profile's arguments were also run on the same card type
(the user's run record).

Measured (910B2 64 GiB × 4, ARM64, 2026-11-20, W4A8): 19.88 GiB of weights per card loaded in 172-196 s,
`torch.compile` 92-106 s. Chat, thinking, tool calls, counting 1 to 40 and
streaming were all correct. Load generated against the instance from the platform's built-in OASST1
real-conversation dataset (3 turns per session, 249-277 input tokens and 787-906 natural output tokens on
average, capped at 1024 per request), after warm-up (throughput in tokens/s):

| Configuration | Concurrency | Total output | Per stream | TPOT median | TTFT median | TTFT p99 |
| --- | --- | --- | --- | --- | --- | --- |
| **this profile** (16 sequences + drafter in graphs) | 1 | 37.23 | 37.23 | 24.9 ms | 0.51 s | 2.69 s |
| **this profile** | 4 | 122.98 | 30.75 | 30.4 ms | 0.65 s | 2.49 s |
| **this profile** | 8 | 170.53 | 21.32 | 40.5 ms | 0.86 s | 1.73 s |
| **this profile** | 16 | **268.78** | 16.80 | 54.1 ms | 0.89 s | 1.89 s |
| drafter in graphs only (4 sequences) | 1 | 37.82 | 37.82 | 24.1 ms | 0.52 s | 3.29 s |
| drafter in graphs only | 4 | 126.17 | 31.54 | 29.4 ms | 0.61 s | 3.34 s |
| 16 sequences only (drafter eager) | 4 | 86.41 | 21.60 | 42.6 ms | 0.70 s | 3.50 s |
| 16 sequences only | 8 | 124.21 | 15.53 | 56.4 ms | 0.95 s | 1.93 s |
| 16 sequences only | 16 | 219.27 | 13.70 | 64.7 ms | 0.97 s | 2.00 s |
| the previous arguments (MTP, drafter eager, 4 sequences) | 1 | 23.17 | 23.17 | 39.4 ms | 0.57 s | 3.33 s |
| the previous arguments | 4 | 89.49 | 22.37 | 41.8 ms | 0.70 s | 3.63 s |
| no MTP (4 sequences) | 1 | 22.97 | 22.97 | 42.0 ms | 0.52 s | 3.42 s |
| no MTP | 4 | 76.78 | 19.19 | 48.4 ms | 0.60 s | 3.95 s |

**The two changes compose**: at concurrency 16, 219.27 becomes 268.78 (+22.6%), at 8, 124.21 becomes 170.53
(+37.3%), and at 4, 86.41 becomes 122.98 (+42.3%). That is 3.0× the 89.49 tokens/s ceiling of the previous
arguments and 3.5× the 76.78 without MTP, and median TPOT at concurrency 16 (54.1 ms) is lower than the
16-sequences-only run's (64.7 ms). The two profiles without MTP are set to 16 sequences as well but were not
re-measured there; they decode one token per sequence, so they put less pressure on the scheduler than the
MTP profiles do.

**Whether the drafter replays graphs is the single largest difference in this profile.** With the three draft
tokens eager, an iteration takes 84.8 ms and yields 2.06 tokens (41.1 ms per token), which is what the profile
without MTP costs anyway (41.5 ms per token): at concurrency 1 MTP buys nothing, and at concurrency 4 only
+17%. With the drafter in graphs an iteration drops to 50.8 ms (concurrency 1) and 59.3 ms (concurrency 4),
single-stream throughput rises 61% and concurrency 4 by 37%, and median TPOT falls from 41.8 ms to 24.9 ms, so
latency and throughput improve together. The cost is one more set of captured graphs at start-up: this
profile's arguments reached ready 16:33 after creation, of which the drafter's own `torch.compile` is
3.1-3.9 s and its warm-up 0.16-0.54 s, with the eight decode graph sizes captured in 1:54.

**16 sequences instead of 4**: 4 already saturated the scheduler (total throughput scaled 3.9× with
concurrency while TPOT barely moved, i.e. decoding is bound by memory bandwidth rather than compute). This
profile reaches 268.78 tokens/s at concurrency 16, at the cost of per-stream decode (30.75 to 16.80 tokens/s)
and median TPOT (30.4 to 54.1 ms), and TTFT p99 improves from 2.49 s to 1.89 s because requests stop
queueing. Iteration time still grows with concurrency (59.3 to 86.9 ms), so 16 is not a hard ceiling, just
the highest measured here. The 20 GiB KV pool holds about 1.44M tokens, so 16 concurrent requests are
unconstrained up to about 90K tokens of context each; only when they all approach the full 256K does the pool
fall back to about five, and then the scheduler queues them itself.

Speculative decoding, from the whole-run deltas of `vllm:spec_decode_num_{draft,accepted}_tokens_total` in
`/metrics`: the three draft tokens are accepted 37.2% of the time (36892 of 99282 drafts), 2.11 tokens per
step on average, and 52.7% of the output comes from drafts. All five configurations above, across
concurrency 1 to 16, sit between 2.01 and 2.12 tokens per step: acceptance moves with neither concurrency,
nor the concurrency cap, nor whether the drafter replays graphs. Highly predictable output such as counting reaches 88% acceptance, so the 37% measured on real
conversations is the figure to expect day to day. The platform's benchmark report gives a
`tokens_per_iteration` within 0.1% of the engine counters, but its "acceptance rate" is defined as the share
of output that came from drafts (`1 - 1/tokens per step`, 51.5-52.9% here), which is not vLLM's draft
acceptance rate; do not mix the two. `int8-vllm-910b-tp4-mtp` differs only in weights and quantization and
was not measured separately.

**The first request is slow.** Some kernels are compiled just in time for the tensor shapes a request actually
uses, so the first request takes markedly longer than the ones after it; the throughput above is the steady
state once that cache is warm. The cache lives inside the container, so a rebuilt container pays it again.

## Ascend 910B (A2) · 8 cards · TP8

`w4a8-vllm-910b-tp8-mtp`: one node with eight 910B cards of at least 32 GiB each, TP8 with expert
parallelism, the W4A8 weights (`Eco-Tech/Qwen3.8-Flash-Next-w4a8` `d75537fd…`) with the checkpoint's own MTP
layer, on the same image as the four-card profiles. The `--additional-config` switches (CPU binding,
shared-expert DP, FlashComm1, shared-expert multistream overlap, NPUGraph EX), async scheduling, KV sized by
`--gpu-memory-utilization 0.90` and the environment come from a published vLLM-Ascend `v0.32.0rc1` recipe
for eight cards; the concurrency ceiling of 16, 8192 batched tokens per step, the decode graph sizes
(multiples of four up to 64) and `OMP_NUM_THREADS=8` follow the measurement below. **Thinking is off by
default**; a request turns it on with `chat_template_kwargs: {"enable_thinking": true}`.

Measured (8× 910B4 32 GiB, ARM64, Docker, 2026-11-23). The baseline ran the four-card profiles' arguments
on the same eight cards (without the fixed 20 GiB KV pool, which does not fit a 32 GiB card). 196 input
tokens with a random prefix per request, thinking off, load sent from the node straight to the engine, one
run per point (12–96 requests), every request successful. Output throughput (tokens/s):

| Concurrency | This profile · 360 out | Baseline · 360 out | This profile · 1024 out | Baseline · 1024 out |
| --- | --- | --- | --- | --- |
| 1 | 30.0 | 34.5 | 30.1 | 34.7 |
| 2 | 55.4 | 62.7 | 61.2 | 64.0 |
| 4 | 102.1 | 106.2 | 113.8 | 115.9 |
| 8 | 166.2 | 176.0 | 189.3 | 198.4 |
| 16 | **219.2** | 192.5 | **283.9** | 233.5 |

At concurrency 16 this profile's output throughput is 14%–22% higher, with median TTFT 1.1–1.6 s (baseline
4.3–6.9 s) and P95 TTFT 1.8–2.8 s (baseline 16.3–42.6 s). A single stream decodes about 31 tokens/s against
the baseline's 36: this profile is about a tenth slower at concurrency 1–2 and 2%–6% slower at 4–8 (single
runs; that margin is too small to conclude from). With the recipe's own ceiling of 8 the profile stayed at
164 / 198 tokens/s at concurrency 16, the extra requests queueing. The floor per card is the
32 GiB this run used (this profile has no fixed 20 GiB KV pool as the four-card profiles do, so it does not
need 64 GiB cards). On the 32 GiB cards each card held 11.2 GiB of weights and the KV cache about 880K tokens (about 3.4 concurrent
256K requests).

Three things do not follow the recipe: the image is pinned by digest (the recipe names the mutable tag
`v0.32.0rc1`); the speculative method is `mtp` (the recipe's `qwen3_5_mtp` is this image's old name for it);
and settings that name one host (`HCCL_IF_IP`, the `*_SOCKET_IFNAME` interface names, the jemalloc preload
path) are left out.

**Runtime**: under udocker (proot) the system calls of eight ranks all pass through the single-threaded
proot, and the engine had not finished starting after 14 minutes; use Docker or nerdctl.
**Host memory**: each rank maps its own copy of the embedding table, measured at 11.9 GiB per rank, about
95 GiB of free memory for eight ranks, plus the page cache for the checkpoint.
