# MiniMax H3

MiniMax H3 generates audio and video. GPUWorks can submit an asynchronous video job from the Playground, poll its status, and download the completed result.

## Ascend profile

The current Ascend profile was verified on four 910B4 GPUs with 32 GiB each. The template selects the repository's `FL2VA` directory while preserving its repository-relative layout, then launches SGLang Diffusion with TP4 and SP1. Users do not need to enter these details manually.

Start with a 512-pixel short edge. A verified 896×512 video of about four seconds completed with approximately 29,424 MiB peak memory per GPU.

The current profile does not support 910C/A3. Upstream MiniMax H3's SGLang diffusion full-loop denoise still requires CUDA, and changing the attention backend only moves the failure into CUDA JIT; changing the context to NPU does not remove that dependency. A 910C profile should be added only after upstream provides a working NPU denoise path and it has been verified on real hardware.

## NVIDIA profiles

The NVIDIA profiles follow vLLM-Omni's own MiniMax H3 recipes. Each profile pins its GPU count:

| Profile | GPUs | Cards | Topology | Steps | Image |
|---|---:|---|---|---:|---|
| General | 1 | Ampere and newer | Layerwise offload of the DiT and text encoder (capacity path, slow) | 50 | GPUWorks vLLM-Omni 0.30 runner |
| Blackwell | 4 | Blackwell | Ulysses 4, no offload | 50 | Same |
| RTX 4090 | 2 | RTX 4090 | TP2 with distributed layerwise offload, 12 resident DiT blocks | 50 | Same |
| RTX 5090 | 2 | RTX 5090 | TP2 with distributed layerwise offload, 20 resident DiT blocks | 50 | Same |
| RTX PRO 5000 | 4 | RTX PRO 5000 | TP2 × Ulysses 2, no offload | 50 | Same |
| Turbo 4-step · 2 GPUs | 2 | Ampere and newer, at least 24 GB each (3090, 4090, 5090, A100, L40S, …) | TP2 with distributed layerwise offload: 20 resident DiT blocks on cards of 32 GB and more, 12 on 24 GB cards; LightX2V Turbo LoRA | 4 | `vllm/vllm-omni:minimax-h3-image` |
| Turbo 4-step · 1 GPU | 1 | Ampere and newer, at least 32 GB (5090, A100, L40S, RTX 6000 Ada, …) | Single-GPU distributed layerwise offload, 12 resident DiT blocks, Turbo LoRA | 4 | Same |
| FastH3 Omni NVFP4 · 1 GPU | 1 | Blackwell, at least 32 GB (5090, RTX PRO 6000, B200, DGX Spark, …) | FastVideo's NVFP4 distilled checkpoint, layerwise offload of the DiT and text encoder | 8 | Same |

### Choosing a profile

Each number is the end-to-end time of one request producing H.264 with an audio track:

| Profile | Measured on | 1344×768 · 5 s | 832×480 · 15 s | 1344×768 · 15 s |
|---|---|---:|---:|---:|
| RTX 5090 · 2 GPUs (50 steps) | 2 × RTX 5090 | About 13 minutes (about 15 s per step) | — | Over 30 minutes |
| Turbo 4-step · 2 GPUs | 2 × RTX 5090 | 68 s | — | 274 s |
| Turbo 4-step · 2 GPUs | 2 × RTX 3090 | 184 s | 225 s | Out of memory on 24 GB (768p · 10 s works, 507 s) |
| Turbo 4-step · 1 GPU | 1 × RTX 5090 | 113 s | 108 s | Out of memory on 32 GB |
| FastH3 Omni NVFP4 · 1 GPU | 1 × RTX 5090 | 52 s | 68 s | Out of memory on 32 GB |

- **The cost is the step count.** The 50-step 2-GPU profile takes about 15 s per step, and it still takes 15 s per step with the DiT quantized to FP8 and fully resident: attention compute is the bottleneck, and the layerwise-offload transfers already overlap it. Distilling to 4 or 8 steps is an order-of-magnitude speedup; the output is not identical to 50 steps. Spreading one instance over more consumer GPUs is not faster: TP4 on four GPUs is slower than two.
- **Host memory, not GPU count, limits how many instances a machine runs.** The 2-GPU Turbo profile pins about 190 GiB of host memory, the 1-GPU Turbo profile about 150 GiB, and FastH3 NVFP4 about 65 GiB. On an 8-GPU machine with 512 GiB, starting a second 2-GPU offload instance exhausted memory while it loaded and killed the first one. Run one 2-GPU offload instance on such a machine; on Blackwell cards, give the remaining GPUs to FastH3 NVFP4 single-GPU instances (four of them serving at once took 54–74 s per request).
- **The Turbo profiles** load the FL2VA 4-step v1.3 LoRA from [LightX2V MiniMax-H3 Turbo](https://huggingface.co/lightx2v/Minimax-h3-Turbo). Requests that omit the step count get 4 steps at guidance 1. They accept text alone or a first-frame reference image, and are trained at 768p: the template offers 1344×768, 832×480 and their portrait sizes, and the output follows `size`. Durations must be 4–15 s. The engine requires `aspect_ratio` on a text-only request; the Playground sends the template default `16:9`, and API callers must send it themselves.
- **FastH3 Omni NVFP4** is the Ref2VA checkpoint [FastVideo](https://huggingface.co/FastVideo/FastH3-Omni-NVFP4-QAD) produced with quantization-aware distillation. It runs a fixed 8 steps and **requires a reference image** (no text-only generation), and its NVFP4 kernels exist only on Blackwell. The template also downloads MiniMax-H3's `Ref2VA/video_vae` and `Ref2VA/audio_vae` and the 8-layer NVFP4 video decoder, and builds an offline Hugging Face cache entry in the container before the engine starts.
- The Turbo and FastH3 profiles use `vllm/vllm-omni:minimax-h3-image` (vLLM 0.33.0 with vLLM-Omni), which upstream released with MiniMax-H3-Image. Neither the GPUWorks 0.30 runner nor upstream v0.32.0 accepts the FastH3 Omni checkpoint.
- The offload profiles keep most weights in pinned host memory, and the template lifts the `memlock` limit for them. Container nodes without Docker (udocker/runveil) do not apply that setting and use the host process's existing limit; they also pass large results between processes slowly, so long videos can time out.

## Known boundaries

- This profile accepts exactly four 910B4 GPUs with at least 32 GiB each. Other GPU counts and models are not inferred from a single machine result.
- A 768-pixel short edge may run out of memory during the final VAE decode.
- TP8 caused excessive host-memory pressure before weight sharding and is not published as a supported profile.
- The template downloads only the required `FL2VA` repository directory. If the same revision of the complete repository is already cached on the node, GPUWorks reuses it.
