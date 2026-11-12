# Qwen3.8-Flash-Next 125B (Strata tiered inference)

[Strata](https://github.com/Niko1221/Strata) keeps the MoE experts tiered across VRAM, RAM and SSD, and this catalog provides four GGUF quantizations of the 125B Qwen3.8-Flash-Next. Each assigned NVIDIA card needs at least 12 GiB of VRAM; the selected quantization also needs sufficient host RAM and disk space (see the table below). The template uses the callable engine: the image entrypoint `/opt/strata/gpuworks-entrypoint.sh` turns the platform's model path, address, port and assigned GPUs into Strata's setup arguments and starts the server.

The catalog carries a hash-checked queue-timing patch. Its launcher applies the patch before entering the original image entrypoint, preserving the base image, model arguments and GPU assignment flow. Only the pinned 0.1.38 source is accepted; other versions fail startup explicitly. `timings.queue_ms` in the same response measures FIFO waiting before initial model loading and first-generation admission, excluding model loading, tool execution and later MCP rounds without enabling body monitoring. Update old deployments to this catalog configuration and recreate their instances to emit the field; running instances are not patched live.

## Built-in Strata profiles

After installing a GPUWorks build containing this catalog, open **Inference Templates → Qwen3.8-Flash-Next 125B (Strata)** and select **Turing+ · <quantization> · Strata r5** matching the weights. The server includes all four profiles at startup; no template import or clone is needed. Choose r5 for new deployments. gw6 is marked as a **legacy image** and retained for existing deployments, with its spec ID and published image sources unchanged.

All four profiles use the published `quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5`, also available as `registry.zstack.ai/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5`. Both registry manifests have digest `sha256:19216f47d43e59068266bb4d6d0150f3dea74f202a97e2e3142eeca8240bde9d`. The template declares these two sources so nodes can pull directly without a preloaded image. The matching SWR tag has not passed public-read verification and is not listed as a fallback.

In **Image Catalog → Community Images → Add Community Image**, open the GPUWorks runner repository, search for `strata0.1.38-amd64-slot31-r5`, and select the tag. The form identifies Callable, CUDA 13.0 engine version 0.1.38 and amd64 architecture. r5 retains the pinned Strata engine and fixes model linking and entrypoint compatibility. The new spec IDs contain no image version; the display names and image references identify the version. Publishing an image does not update an older GPUWorks embedded catalog; install a build containing this catalog.

Select the registered local GGUF quantization directory matching the profile, or obtain the pinned weights through that profile's sources. The r5 IQ2_XS profile uses the same weights as gw6; the other three profiles use their own quantized weights. All four share the tokenizer, GPU assignment, API and queue-timing contract. r5 adds optional `--ple-io direct|mmap|ram` and `--ple-gguf /absolute/path/to/file.gguf` custom arguments. The default stays `direct`; no host-specific cache path, RAM allocation or unlimited memlock is enabled by this profile. A redirected PLE file must be a byte-identical copy of the matching weight revision, mounted read-only. RAM mode requires enough container memory and a suitable memlock limit; verify actual residency and locking before relying on it.

## Weight formats and quantizations

These profiles select four **ISTA-DASLab GSQ-RCO GGUF** variants of the original Qwen3.8-Flash-Next. Choose the profile matching the registered model's quantization directory. The existing gw6 spec ID and default arguments are preserved.

| Quantization | Weight bytes (two shards) | Upstream setup RAM recommendation |
|---|---:|---: |
| IQ2_XS | 68,026,093,024 | 48 GB |
| Q2_0 | 66,423,878,624 | 48 GB |
| IQ3_XXS | 75,839,998,528 | 60 GB |
| IQ3_S | 83,617,662,656 | 62 GB |

Sizes are the sums of the files at the [pinned weight revision](https://huggingface.co/ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF/tree/ed59f92082b1e93c0e96d60a8b11aab089b52f09). The new variants declare Hugging Face and ModelScope revisions with matching published shard sizes and SHA-256 metadata; the original IQ2_XS sources remain unchanged. RAM figures come from the image's setup recommendations, not GPUWorks memory admission or measured peak usage. Leave additional headroom for context, preparation and other processes. Every assigned GPU still needs at least 12 GiB. Host RAM is currently not checked during admission, so a low-memory node can pass the VRAM check and fail during setup. See [known issue T109](../../../../../../../../known-issues.md#t109-strata-的宿主机内存不参与准入) for selecting nodes with sufficient memory.

The non-default profiles append `--model=Q2_0`, `--model=IQ3_XXS` or `--model=IQ3_S`. The equals sign matters: the image consumes `--model PATH` as GPUWorks' model mount; the equals form reaches Strata's setup and overrides its IQ2_XS default. Changing only the weights directory would leave setup looking for the wrong shards. Select the matching profile instead of changing this argument independently of the weight source. Model-source identity checks remain in effect.

**Original BF16/FP8 safetensors are not supported by this image.** The pinned [Strata setup](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/setup.py) prepares GGUF inputs, and the GPUWorks image rejects a directory containing no GGUF files. Upstream's safetensors tools extract only the token embedding, PLE table or MTP draft layer; they do not load the full original checkpoint. The FP8 PLE table is also distinct from an FP8-quantized expert model. Removing template source checks cannot add a checkpoint loader or the required expert execution support. No BF16/FP8 profile or automatic lossy conversion is advertised here. GPUWorks already lists original checkpoints under the separate [Qwen3.8-Flash-Next catalog entry](../../qwen/qwen3.8-flash-next/model.yaml), with hardware-specific vLLM/SGLang profiles and their own resource requirements.

Other repositories and quantizations, including Unsloth's experimental UD-Q4_K_XL, need their own family, preparation and capability settings; the same model name alone does not make them interchangeable. Q2_0/IQ3_XXS/IQ3_S use formats explicitly listed by the pinned engine. Template and image argument checks do not constitute real-weight generation or performance validation for these newly added variants; the previous end-to-end evidence applies to IQ2_XS on r4, not to real-weight inference on r5 or the other quantizations.

## Image input

On by default. The image carries Strata's image encoder `strata-vision` and the vision projector `mmproj-Qwen3.8-Flash-Next-BF16.gguf` at the weights' revision (the file sits at the repository root while the template downloads only its selected quantization subfolder, so the image supplies it; a copy of the same name inside the model directory takes precedence). The entrypoint installs with `--vision gpu`: the encoder shares the engine's card, and the engine shrinks its expert cache by what the encoder holds. Previously recorded IQ2_XS figures (not measurements of this r5 release) on one RTX 4090D at a 128K context: the encoder holds about 1.4 GB of VRAM and decoding runs at about 51–55 tok/s, on par with image input off.

- Requests put OpenAI `image_url` parts in a message, as base64 data URLs or http(s) addresses; the GPU encoder turns one image into at most 1024 image tokens.
- Add `--vision cpu` to the custom arguments to encode on the CPU (no VRAM, at most 300 tokens per image, slower), or `--vision no` to turn image input off and leave all VRAM to the expert cache.
- When you turn it off, also set the model's input modalities to text only on the Models page, so Playground stops offering image upload.

## How many cards

Strata uses the cards the platform assigns to the deployment: one when placement picks, or as many as you select in the deploy form, and it splits the layers across them. More cards hold more experts in VRAM and generate faster. Every card needs at least 12 GiB, or the deploy evaluation refuses it.

Neither the card count nor the indexes appear in the template; that is deliberate. The assigned cards reach Strata like this:

1. The scheduler picks the cards. The template's placement resolver only checks each card's memory and pins no count.
2. When the agent starts the container it puts those cards in `CUDA_VISIBLE_DEVICES`, as for every engine. On a udocker node these are the physical indexes; on a docker node the container gets only the selected cards and the variable holds their in-container numbers 0..n-1. Either way they are the numbers `nvidia-smi` shows inside the container.
3. The image entrypoint reads `CUDA_VISIBLE_DEVICES` and passes `--gpu N` for one card or `--gpus A,B` for several. Strata itself picks cards by `nvidia-smi` numbering.

The indexes are not a template variable because they depend on the node's container runtime, which only the agent knows; template variables are expanded on the server, before that is settled. So do not write `--gpu`/`--gpus` into the custom arguments: a fixed index can point at a card another deployment owns.

To see which cards Strata uses, check the `gpu` field of its install config `/opt/strata/strata-<quant>.json` in the container (`<quant>` is the lowercase quantization name: `iq2_xs`, `q2_0`, `iq3_xxs` or `iq3_s`) (the instance terminal in the deployment details can show it), or run `nvidia-smi` on the node and confirm that only the deployment's cards hold its memory.

## Configurable arguments

A callable profile has no parameter dictionary; you tune it by adding command-line arguments in the deploy form's custom arguments. The entrypoint hands them to Strata's `setup.py` unchanged. A repeated argument takes its last value, so what you add replaces the entrypoint's default.

| Argument | Default | Meaning |
|---|---|---|
| `--context N` | Strata's recommendation for the cards (below) | The context window: the most prompt plus reply tokens of one request |
| `--kv int8\|q4_0\|k8v4` | `int8` | KV precision above an 8K context; `q4_0` halves the KV memory at a small quality cost |
| `--kv-streaming auto\|on\|off` | `auto` | From a 64K context, keep the KV cache in RAM and only the attention window in VRAM |
| `--rope-scaling linear\|yarn`, `--rope-scale F` | yarn past 262144, factor = context / 262144 | RoPE extension past the trained length |
| `--vision gpu\|cpu\|no` | `gpu` | Run the image encoder on the GPU or the CPU, or turn image input off (see Image input) |
| `--vram-reserve-mib N` | engine default | VRAM left free for other programs on the card |
| `--gpu N`, `--gpus A,B` | the cards the platform assigned | Do not change: the entrypoint derives them from `CUDA_VISIBLE_DEVICES` (above), and other cards belong to other deployments |

A request's `max_tokens` caps that reply. Without it Strata generates until the context is full, so one reply is at most the context minus the prompt.

## Context default and limits

- **Default**: without `--context`, Strata recommends by the smallest assigned card: 32K below 14 GB of VRAM, 64K below 20 GB, 128K otherwise. The pinned host-RAM context rule applies only to IQ3_XXS/IQ3_S and never recommends less than 128K, so it does not reduce those defaults. For a context above 128K, setup estimates RAM use for the IQ3 variants and warns when it does not fit. IQ2_XS/Q2_0 are outside that rule but still need sufficient RAM. All four quantizations default to 128K on a 24 GB RTX 4090 / 4090D; an explicit `--context` overrides the recommendation.
- **Maximum**: the model is trained to 262144 (256K). Strata also offers 384K and 512K through yarn extension, marked experimental.
- **Cost**: KV is small, about 13.4 KB per token in int8 (7.3 KB with `q4_0`), so a 256K context needs about 3.5 GB of KV split across the cards by layer. That VRAM is shared with the expert cache, though: every GB more of context is a GB fewer of cached experts, and generation slows down accordingly. On small cards keep the default, and pair long contexts with `--kv q4_0` or `--kv-streaming on`.
- The model's `context_length: 262144` is the trained length, not the context a deployment serves. The served value is in Strata's start log (`context N tokens`) and in `/v1/models` (`meta.n_ctx`).

## Known limits

- `/metrics` is JSON, not Prometheus text; this template supplies the field mapping used by the platform.
- The first start builds the expert index and is slower than a restart.
