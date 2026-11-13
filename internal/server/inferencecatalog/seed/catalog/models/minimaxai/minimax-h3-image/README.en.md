# MiniMax-H3-Image

MiniMax-H3-Image generates and edits images on the MiniMax-H3 DiT: six CFG-distilled steps per image, one image per request, and up to nine reference images for an edit. The GPUWorks Playground runs text-to-image and single-image edits directly, and both `/v1/images/generations` and `/v1/images/edits` are served through the platform gateway.

## Weights and components

The template has two components. The platform downloads and mounts both at deploy time:

| Component | Source | Size | Container path |
|---|---|---:|---|
| Primary (DiT, VAE, PDD output heads, tokenizer) | `MiniMaxAI/MiniMax-H3-Image` | 68.7 GB | `/models/MiniMax-H3-Image` |
| Qwen3-VL text encoder | the `text_encoder` folder of `MiniMaxAI/MiniMax-H3` | 66.7 GB | `/models/MiniMax-H3` |

The upstream `model_index.json` loads the MiniMax-H3 text encoder from the Hub by repository name. Before the engine starts, the template points it at the mounted folder, so the engine stays offline (`HF_HUB_OFFLINE=1`). Only the `text_encoder` folder is downloaded; a node that already caches the complete MiniMax-H3 repository at the same revision reuses it.

Hugging Face revisions after 2026-11-10 also carry a diffusers-format copy of the weights (about 204 GB for the whole repository) that vLLM-Omni does not use, so the template pins the revision before it.

## Profiles

The image is the upstream release image `vllm/vllm-omni:minimax-h3-image` (vLLM 0.33.0 with vLLM-Omni), and the arguments follow the [vLLM recipe](https://recipes.vllm.ai/MiniMaxAI/MiniMax-H3-Image). All six profiles were run; the figures are end-to-end medians at 2048×2048 and concurrency 1:

| Profile | GPUs | Fits | Measured on | Peak per GPU | Text-to-image | Edit |
|---|---:|---|---|---:|---:|---:|
| 4 GPUs Ulysses | 4 | Hopper+, ≥ 90 GiB per GPU | H20-3e | 83.0 GiB | 2.55 s | 7.8 s |
| 4 GPUs TP | 4 | Hopper+, ≥ 44 GiB per GPU | H20-3e | 35.5 GiB | 2.63 s | 7.8 s |
| 2 GPUs Ulysses | 2 | Hopper+, ≥ 110 GiB per GPU | H20-3e | 94.3 GiB | 4.70 s | 14.6 s |
| 2 GPUs TP | 2 | Ampere+, ≥ 72 GiB per GPU | A100 80GB | 61.3 GiB | 3.31 s | 9.0 s |
| 1 GPU · text encoder offload | 1 | Ampere+, ≥ 78 GiB | A100 80GB / H20-3e | 69.9 / 71.4 GiB | 7.67 / 9.55 s | 17.6 / 27.3 s |
| 1 GPU · layerwise offload | 1 | Ampere+, ≥ 22 GiB | RTX 4090 / A100 / H20-3e | 11.1–12.7 GiB | 18.3 / 19.7 / 9.53 s | 24.3 / 21.7 / 27.3 s |

In the edit column, H20 used a 2720×1536 reference image and the A100 and RTX 4090 a 2048×2048 one, so the two are not strictly comparable.

Choosing one:

- **Lowest entry point**: 1 GPU with layerwise offload. A single 24 GB card runs it; the DiT and the text encoder sit in about 124 GiB of pinned host memory, so host-to-GPU bandwidth sets the speed (about 9.5 s on a PCIe 5 H20, 18–20 s on a PCIe 4 RTX 4090 or A100).
- **80 GB cards**: 1 GPU with text encoder offload (7.7 s on an A100), or 2 GPUs TP (3.3 s on A100s).
- **141 GB cards (H200, H20-3e)**: 4 GPUs TP and 4 GPUs Ulysses are about as fast (2.63 s and 2.55 s on H20), and TP takes less than half the memory per GPU.
- Consumer cards (RTX 4090) have no NVLink, and multi-GPU layouts are slower there: 4 GPUs TP with text encoder offload took 30.6 s per image and 2 GPUs Ulysses with layerwise offload 110 s, so neither is in the template. To raise throughput on 4090s, run one single-GPU replica per card (each needs about 124 GiB of host memory).

Notes:

- Requests below 2048² are generated on a canvas of at least 2048² and resized back, so 1024² and 2048² take about the same time. Requesting a canvas size directly (2048x2048, 2720x1536, 1536x2720, 2368x1760, 2496x1664, 3136x1344) skips the resize.
- The engine serves one request at a time: concurrency only adds queueing, and throughput does not rise with it.
- The two offload profiles keep weights in pinned host memory (about 62 GiB for the text encoder, plus about 62 GiB for the DiT with layerwise offload); the template lifts the `memlock` limit for them.
- On 80 GB cards the platform counts 90% of the card as usable (72 GiB); the 1 GPU text encoder offload profile claims 71.5 GiB, right under that limit.
- Where one GPU holds everything (B200, B300), full single-GPU residency is the upstream recommendation; the template does not offer it yet.

## Request contract

- `num_inference_steps` must be omitted or 6, `guidance_scale` omitted or 1, `negative_prompt` is not supported, and `n` is 1.
- References are presented to the model as `Picture 1`, `Picture 2`, … in upload order, and prompts can name them. Giving references at the canvas size of the requested aspect ratio and requesting that size aligns edits best.
