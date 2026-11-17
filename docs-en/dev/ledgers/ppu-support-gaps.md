# PPU Support Gaps and Evidence Ledger

This file records entries marked `REAL_PASS` in the merged ledger under `model-inference-validation/vendors/ppu` for which GPUWorks does not yet have a deployable contract.
Passing research validation does not mean GPUWorks has a deployable contract. Entries whose image, entrypoint, or native runtime cannot be reconstructed from public or repository evidence are marked as gaps.

Here, a "deployable contract" means an explicit specification that GPUWorks can use to deploy a model, including which image to use, how to start it, which runtime assets it requires,
which API it provides, and which topologies it supports. The entrypoint is the startup entry point, and the catalog is the inference catalog. A spec is an executable configuration;
profile retains the term used in historical records. See [`CONTEXT.md`](../../../CONTEXT.md) for the current terminology definitions.

The table below is a snapshot from around 2026-09-08, when the ledger contained 65 unique models marked `REAL_PASS`. It is retained for comparison with historical conclusions.
Current figures follow `pipeline/ledger/current/ppu-full-real-validation.json` (generated on 2026-09-14: `real_pass_unique_models: 108`, `large_tp_or_pp_real_pass_unique_models: 5`).
The number of snapshot rows in this file no longer represents coverage.

## Image Source Definitions

The same model may run on different PPU runtimes in different runs, and those runtimes use different images:

| Research engine label | Actual image | Description |
|---|---|---|
| `asllm-2.0.0-vllm-0.23.0` | `https://containerimages.zstack.ai/asllm_2.0.0-pytorch2.10.0-ubuntu24.04-sail2.1.0-cuda13.0-sglang0.5.13-vllm0.23.0-py312.tar` | Vendor ASLLM image with vLLM 0.23.0 / SGLang 0.5.13 bundled; packaged as a tar archive, with no registry digest |
| `asllm-1.10.4-vllm-0.23.0` | `https://containerimages.zstack.ai/asllm_1.10.4-pytorch2.10.0-ubuntu24.04-sail2.1.0-cuda13.0-sglang0.5.12-vllm0.23.0-py312.tar` | Previous ASLLM version; bundles SGLang 0.5.12 |
| `legacy-vllm-0.23.0` / `vllm-0.23.0` | `swr.cn-south-1.myhuaweicloud.com/gpustack/runner:hggc13.0-vllm0.23.0` | GPUStack runner, the vLLM fallback path for the research worker |
| `sglang-0.5.12` | `swr.cn-south-1.myhuaweicloud.com/gpustack/runner:hggc13.0-sglang0.5.12` | SGLang path in the GPUStack runner |

The `image` of each PPU spec in the catalog must be the image used in its own passing run; it cannot be inferred from the model. For example, `int4-vllm-ppu-tp2-a6977d07` (Laguna-S-2.1) passes with TP2 on `asllm-1.10.4`, but only records `COMPAT_FAIL` on `asllm-2.0.0`.

`internal/server/inferencecatalog/seed/ppu_profiles_test.go` pins the engine / image / tested revision / weight_bytes for each profile together. Changing any of them causes the test to fail.


Hugging Face and ModelScope availability is based on the `source_availability` records for the corresponding tested revision in `pipeline/ledger/current/ppu-full-real-combined-progress.json`. Templates include only providers verified as available. When live HTTP requests to external APIs are unavailable in the current workspace, do not infer that a repository does not exist from a network timeout.

In the table below, tested revision is the model revision used in that test. `template-generated` means a template has been generated; `gap` means deployment information is still missing.
In the source columns, `available`, `unavailable (404)`, and `not recorded` mean recorded as available, returned 404, and not recorded, respectively.
These columns describe template readiness and model source availability. Neither alone can substitute for GPUWorks deployment acceptance results.

| Model | Status | Research engine | tested revision | Hugging Face | ModelScope | Notes |
|---|---|---|---|---|---|---|
| `ByteDance-Seed/Seed-OSS-36B-Instruct` | template-generated | `vllm-0.23.0` | `497f1dca95ebdec98e41d517b9f060ee753c902f` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `FunAudioLLM/Fun-ASR-Nano-2512-hf` | gap | `transformers-native` | `972ee603a3abb40a66c802ffaa9ba0ce88742b60` | available | unavailable (404) | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `HuggingFaceTB/SmolLM-135M-Instruct` | template-generated | `vllm-0.23.0` | `fcc320f490e08fdb4b99d935b2c58d40bf35b0d0` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-1.7B-Instruct` | template-generated | `vllm-0.23.0` | `31b70e2e869a7173562077fd711b654946d38674` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-135M-Instruct` | template-generated | `vllm-0.23.0` | `12fd25f77366fa6b3b4b768ec3050bf629380bac` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-360M-Instruct` | template-generated | `vllm-0.23.0` | `a10cc1512eabd3dde888204e902eca88bddb4951` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-1.2B-Instruct` | template-generated | `vllm-0.23.0` | `0f604ada3f766f9f257460c4c9f0b5d6f69d431b` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-230M` | template-generated | `vllm-0.23.0` | `40cb2ad3b3044d5a41eee083a6103c8b523afa45` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-350M` | template-generated | `vllm-0.23.0` | `9e6c6ccf47cd318696e137d381a7ded8fe4df09f` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-VL-450M` | template-generated | `vllm-0.23.0` | `fc6221ca597f3315e4f82fc2df606783267b34ba` | available | available | PPU vLLM/SGLang digest template |
| `OpenOneRec/OneRec-1.7B` | template-generated | `vllm-0.23.0` | `cc4d0b5b7294ecf75e40be1c77fa6b7d284bb84b` | available | available | PPU vLLM/SGLang digest template |
| `PaddlePaddle/PaddleOCR-VL-1.6` | template-generated | `vllm-0.23.0` | `c5630abae1d940eafe0697512a0325494b02ab42` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen1.5-0.5B-Chat` | template-generated | `vllm-0.23.0` | `4d14e384a4b037942bb3f3016665157c8bcb70ea` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-0.5B` | template-generated | `vllm-0.23.0` | `91d2aff3f957f99e4c74c962f2f408dcc88a18d8` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `c540970f9e29518b1d8f06ab8b24cba66ad77b6d` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-1.5B-Instruct` | template-generated | `vllm-0.23.0` | `ba1cf1846d7df0a0591d6c00649f57e798519da8` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `7ae557604adf67be50417f59c2c2f167def9a775` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-1.5B` | template-generated | `vllm-0.23.0` | `8faed761d45a263340a0528343f099c05c9a4323` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-1.5B-Instruct` | template-generated | `vllm-0.23.0` | `989aa7980e4cf806f80c7fef2b1adb7bc71aa306` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-Coder-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `ea3f2471cf1b1f0db85067f1ef93848e38e88c25` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-0.6B` | template-generated | `vllm-0.23.0` | `c1899de289a04d12100db370d81485cdf75e47ca` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-1.7B` | template-generated | `vllm-0.23.0` | `70d244cc86ccca08cf5af4e1e306ecf908b1ad5e` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-1.7B-Base` | template-generated | `vllm-0.23.0` | `ea980cb0a6c2ae4b936e82123acc929f1cec04c1` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-4B` | template-generated | `vllm-0.23.0` | `1cfa9a7208912126459214e8b04321603b3df60c` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-4B-Instruct-2507` | template-generated | `vllm-0.23.0` | `cdbee75f17c01a7cc42f958dc650907174af0554` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-8B` | template-generated | `vllm-0.23.0` | `b968826d9c46dd6066d109eabc6255188de91218` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-ASR-0.6B-hf` | gap | `transformers-native` | `7f1569a48a89f3e3f4dc3a5c9d28bddd903bc76c` | available | available | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `Qwen/Qwen3-TTS-12Hz-0.6B-Base` | gap | `qwen-tts-native` | `5d83992436eae1d760afd27aff78a71d676296fc` | available | available | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice` | gap | `qwen-tts-native` | `85e237c12c027371202489a0ec509ded67b5e4b5` | available | available | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `Qwen/Qwen3-VL-Embedding-2B` | template-generated | `vllm` | `9f2f7e710d6d81056aa5c0a4f04764fec6bb7bda` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-0.8B` | template-generated | `vllm-0.23.0` | `2fc06364715b967f1860aea9cf38778875588b17` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-122B-A10B-GPTQ-Int4` | template-generated | `vllm-0.23.0` | `30cd92cba9707a9aba09d1e490ed4b66b78e9606` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-2B` | template-generated | `vllm-0.23.0` | `15852e8c16360a2fea060d615a32b45270f8a8fc` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-4B` | template-generated | `vllm-0.23.0` | `851bf6e806efd8d0a36b00ddf55e13ccb7b8cd0a` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-9B` | template-generated | `vllm-0.23.0` | `c202236235762e1c871ad0ccb60c8ee5ba337b9a` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.8-27B` | template-generated | `vllm` | `1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3Guard-Gen-0.6B` | template-generated | `vllm-0.23.0` | `fada3b2f655b89601929198343c94cd2f64d93cc` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3Guard-Gen-4B` | template-generated | `vllm-0.23.0` | `6ec42827da0c1ff11e7a49dc269d2e810d27e108` | available | available | PPU vLLM/SGLang digest template |
| `TinyLlama/TinyLlama-1.1B-Chat-v1.0` | template-generated | `vllm-0.23.0` | `fe8a4ea1ffedaf415f4da2f062534de366a451e6` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `WeiboAI/VibeThinker-3B` | template-generated | `vllm-0.23.0` | `77bd2cced09193c8b9a59a32bd8577bbd1f3e01c` | available | available | PPU vLLM/SGLang digest template |
| `bosonai/higgs-audio-v3-tts-4b` | gap | `asllm-2.0.0-vllm-0.23.0` | `7556c17e05201fccd9c8cc120bc216dcc7b5d561` | not recorded | not recorded | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets; reference audio still lacks an encoder component and offline validation |
| `deepreinforce-ai/Ornith-1.0-9B` | template-generated | `vllm-0.23.0` | `83dc1f5e24ef8527af019a6b3bf66ac0f1c2c999` | available | available | metadata incomplete: current_weight_payload_first_published_at |
| `deepseek-ai/DeepSeek-OCR` | template-generated | `vllm-0.23.0` | `9f30c71f441d010e5429c532364a86705536c53a` | available | available | PPU vLLM/SGLang digest template |
| `deepseek-ai/DeepSeek-OCR-2` | template-generated | `vllm` | `aaa02f3811945a91062062994c5c4a3f4c0af2b0` | available | available | PPU vLLM/SGLang digest template |
| `deepseek-ai/DeepSeek-V4-Flash` | gap | `vllm-0.23.0` | `60d8d70770c6776ff598c94bb586a859a38244f1` | available | available | The research record is tied to PP4; the template placement contract cannot currently fully express the multistage topology |
| `google/embeddinggemma-300m` | template-generated | `vllm-0.23.0` | `57c266a740f537b4dc058e1b0cda161fd15afa75` | available | available | PPU vLLM/SGLang digest template |
| `google/gemma-4-E2B-it-qat-w4a16-ct` | template-generated | `vllm-0.23.0` | `971342c08f607aa7779983f6b5289778b5d271a7` | available | available | PPU vLLM/SGLang digest template |
| `ibm-granite/granite-4.1-3b` | template-generated | `vllm-0.23.0` | `c0650403e44e78ec0262dab1c90914c65b196c4e` | available | available | PPU vLLM/SGLang digest template |
| `ibm-granite/granite-4.2-3b` | template-generated | `vllm` | `b7e947307dd2efb3ad3b853b0e8a7e75f8ad4ac2` | available | available | PPU vLLM/SGLang digest template |
| `inclusionAI/LLaDA2.1-mini` | template-generated | `sglang-0.5.12` | `20e64e2ad21644d0e5248586ed9c942cdd45de0f` | available | available | PPU vLLM/SGLang digest template |
| `jinaai/jina-embeddings-v5-text-small` | template-generated | `vllm` | `dd76d535f5447ca3897a9c893fb1e612ead98192` | available | available | PPU vLLM/SGLang digest template |
| `jinaai/jina-embeddings-v5-text-small-clustering` | template-generated | `vllm-0.23.0` | `4ab9d9d3c5cca288d9ab38c1dc5228124fdf3271` | available | available | PPU vLLM/SGLang digest template |
| `meituan-longcat/LongCat-Image-Edit` | gap | `asllm-2.0.0-vllm-omni` | `7b54ef423aa7854be7861600024be5c56ab7875a` | available | available | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `microsoft/Phi-4-mini-instruct` | template-generated | `vllm-0.23.0` | `cfbefacb99257ffa30c83adab238a50856ac3083` | available | available | PPU vLLM/SGLang digest template |
| `mistralai/Shieldstral-1.0-3B` | template-generated | `vllm` | `003ec7e2b0bab5f0e6307edbaf186fa5822b76f5` | available | available | PPU vLLM/SGLang digest template |
| `mistralai/Voxtral-4B-TTS-2603` | gap | `asllm-2.0.0-vllm-omni` | `b81be46c3777f88621676791b512bb01dc1cb970` | available | available | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `nvidia/Cosmos3-Nano` | gap | `asllm-2.0.0-vllm-omni` | `7a312c868bcce8e40b3eb40861300a9d0ba3fde1` | available | unavailable (404) | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `nvidia/NVIDIA-Nemotron-3-Nano-4B-BF16` | template-generated | `vllm-0.23.0` | `dfaf35de3e30f1867dd8dbc38a7fc9fb52d3914f` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `nvidia/Nemotron-3-Embed-1B-BF16` | template-generated | `vllm` | `c0c9fea93ea424587517f2c59e20db9f1d6bf615` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `nvidia/nemotron-3.5-asr-streaming-0.6b` | gap | `transformers-native` | `1c8deaecc64b91f034d73e08dd8b64625eb3395d` | available | unavailable (404) | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `openai/gpt-oss-20b` | template-generated | `vllm-0.23.0` | `6cee5e81ee83917806bbde320786a8fb61efebee` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `openbmb/MiniCPM5-1B` | template-generated | `vllm-0.23.0` | `87179e5c1f455ef22e6223592d2d61351b525bfc` | available | available | PPU vLLM/SGLang digest template |
| `zai-org/GLM-ASR-Nano-2512` | gap | `transformers-native` | `61ba4e0b3309b6656edea3e93e419f7bd5c61957` | available | unavailable (404) | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |
| `zai-org/GLM-OCR` | template-generated | `vllm` | `ca5d8b3e287e52589e37c28385d9655ee4372f9d` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `zai-org/GLM-TTS` | gap | `asllm-2.0.0-vllm-omni` | `b3feb691fa4cf590eec4418f6275f95220e58844` | not recorded | not recorded | The native/Omni path lacks an executable contract for existing engines; awaiting a callable runtime, entrypoint, and assets |

## Passing Evidence for Each PPU Profile in the Catalog

The table below lists the 31 profiles incorporated into `internal/server/inferencecatalog/seed/catalog/` from PPU run records in this round, along with their corresponding actual passing runs. The catalog also contains other PPU profiles added earlier, which are not included in this table.
The run directories reside on the research repository's PPU node (`/bmcp_lvm_fs/gpuworks_weiw/ppu-model-research/runs/<run-id>`). This repository does not duplicate the original evidence packages.
The engine column contains engine labels from the research ledger, each corresponding to an image in the table above.

| catalog model | profile | Research engine | PASS run id |
|---|---|---|---|
| `baidu/ernie-4.5-21b-a3b-pt` | `bf16-vllm-ppu-87db9548` | `asllm-2.0.0-vllm-0.23.0` | `full-baidu__ernie-4.5-21b-a3b-pt-asllm-2.0.0-vllm-0.23.0-full-real-20260904T211715` |
| `deepseek-ai/deepseek-v4-flash` | `fp8-vllm-ppu-pp4-60d8d707` | `vllm-0.23.0` | `full-big-deepseek-ai__deepseek-v4-flash-tp4-pp4-fallback-vllm023-full-real-20260901T145833` |
| `google/gemma-4-e4b-it-qat-w4a16-ct` | `int4-vllm-ppu-6cd26aaa` | `asllm-2.0.0-vllm-0.23.0` | `catalog-google__gemma-4-e4b-it-qat-w4a16-ct-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T173027` |
| `google/translategemma-4b-it` | `bf16-vllm-ppu-10042cb0` | `asllm-2.0.0-vllm-0.23.0` | `full-google__translategemma-4b-it-asllm-2.0.0-vllm-0.23.0-full-real-20260905T231630` |
| `hugging-quants/meta-llama-3.1-8b-instruct-awq-int4` | `awq-vllm-ppu-db1f81ad` | `asllm-2.0.0-vllm-0.23.0` | `full-hugging-quants__meta-llama-3.1-8b-instruct-awq-int4-asllm-2.0.0-vllm-0.23.0-full-real-20260909T052641` |
| `internlm/intern-s1-mini` | `bf16-vllm-ppu-0f25c8f6` | `asllm-2.0.0-vllm-0.23.0` | `full-internlm__intern-s1-mini-asllm-2.0.0-vllm-0.23.0-full-real-20260909T052642` |
| `liquidai/lfm2.5-1.2b-base` | `bf16-vllm-ppu-7453bca9` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-base-asllm-2.0.0-vllm-0.23.0-full-real-20260906T121415` |
| `liquidai/lfm2.5-1.2b-jp` | `bf16-vllm-ppu-58ef35ca` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-jp-asllm-2.0.0-vllm-0.23.0-full-real-20260906T061422` |
| `liquidai/lfm2.5-1.2b-jp-202606` | `bf16-vllm-ppu-52b8b447` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-jp-202606-asllm-2.0.0-vllm-0.23.0-full-real-20260906T150310` |
| `liquidai/lfm2.5-8b-a1b` | `bf16-vllm-ppu-5dd22602` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-8b-a1b-asllm-2.0.0-vllm-0.23.0-full-real-20260907T121750` |
| `microsoft/phi-4-multimodal-instruct` | `bf16-vllm-ppu-93f923e1` | `asllm-2.0.0-vllm-0.23.0` | `catalog-microsoft__phi-4-multimodal-instruct-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T060523` |
| `mistralai/ministral-3-14b-reasoning-2512` | `bf16-vllm-ppu-51f9210f` | `legacy-vllm-0.23.0` | `catalog-mistralai__ministral-3-14b-reasoning-2512-vision-legacy-vllm-0.23.0-full-real-20260911T000357` |
| `mistralai/ministral-3-3b-reasoning-2512` | `bf16-vllm-ppu-4a36357c` | `asllm-2.0.0-vllm-0.23.0` | `catalog-mistralai__ministral-3-3b-reasoning-2512-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T013012` |
| `mistralai/ministral-3-8b-reasoning-2512` | `bf16-vllm-ppu-81eaece1` | `asllm-2.0.0-vllm-0.23.0` | `catalog-mistralai__ministral-3-8b-reasoning-2512-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260910T012942` |
| `nvidia/nvidia-nemotron-3.5-lightning-30b-a3b-bf16` | `bf16-vllm-ppu-a9904d24` | `asllm-2.0.0-vllm-0.23.0` | `full-nvidia__nvidia-nemotron-3.5-lightning-30b-a3b-bf16-asllm-2.0.0-vllm-0.23.0-full-real-20260912T105437` |
| `opengvlab/internvl3_5-8b` | `bf16-vllm-ppu-9bb6a56a` | `asllm-2.0.0-vllm-0.23.0` | `catalog-opengvlab__internvl3_5-8b-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260904T000817` |
| `poolside/laguna-s-2.1` | `int4-vllm-ppu-tp2-a6977d07` | `asllm-1.10.4-vllm-0.23.0` | `full-big-poolside__laguna-s-2.1-int4-tp2-asllm-1.10.4-vllm-0.23.0-full-real-20260911T215920` |
| `qwen/qwen2.5-coder-1.5b-instruct` | `bf16-vllm-ppu-2e1fd397` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen2_5_coder_1_5b_instruct-asllm-2.0.0-vllm-0.23.0-full-real-20260908T184259` |
| `qwen/qwen2.5-vl-72b-instruct` | `int4-vllm-ppu-vision-c8b87d4b` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen2.5-vl-72b-instruct-awq-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260912T203447` |
| `qwen/qwen2.5-vl-7b-instruct` | `int4-vllm-ppu-vision-536a3579` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen2.5-vl-7b-instruct-awq-fp16-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T221946` |
| `qwen/qwen3-14b` | `fp8-sglang-ppu-no-piecewise-9a283b4a` | `sglang-0.5.12` | `full-qwen__qwen3-14b-fp8-graph-no-piecewise-sglang0512-full-real-20260907T212023` |
| `qwen/qwen3-235b-a22b-instruct-2507` | `bf16-vllm-ppu-tp8-ac9c66cc` | `asllm-1.10.4-vllm-0.23.0` | `full-big-qwen__qwen3-235b-a22b-instruct-2507-tp8-asllm-1.10.4-vllm-0.23.0-full-real-20260911T222202` |
| `qwen/qwen3-30b-a3b` | `bf16-vllm-ppu-ad44e777` | `legacy-vllm-0.23.0` | `full-qwen__qwen3-30b-a3b-legacy-vllm-0.23.0-full-real-20260911T044537` |
| `qwen/qwen3-32b-awq` | `awq-vllm-ppu-0499c3ac` | `asllm-2.0.0-vllm-0.23.0` | `full-qwen__qwen3-32b-awq-text-fp16-asllm-2.0.0-vllm-0.23.0-full-real-20260906T224848` |
| `qwen/qwen3-4b` | `fp8-sglang-ppu-no-piecewise-96b30dc1` | `sglang-0.5.12` | `full-qwen__qwen3-4b-fp8-graph-no-piecewise-sglang0512-full-real-20260907T224109` |
| `qwen/qwen3-vl-32b-instruct` | `fp8-sglang-ppu-4bf2c2f3` | `sglang-0.5.12` | `full-qwen__qwen3-vl-32b-instruct-fp8-fallback-sglang0512-full-real-20260910T053304` |
| `qwen/qwen3.5-27b-gptq-int4` | `int4-vllm-ppu-8f0c09f2` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen3.5-27b-gptq-int4-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T174629` |
| `qwen/qwen3.5-35b-a3b-gptq-int4` | `int4-vllm-ppu-3af5ca29` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen3.5-35b-a3b-gptq-int4-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T132754` |
| `qwen/qwen3.5-397b-a17b` | `int4-vllm-ppu-tp4-df333de3` | `asllm-1.10.4-vllm-0.23.0` | `full-big-qwen__qwen3.5-397b-a17b-gptq-int4-tp4-asllm-1.10.4-vllm-0.23.0-full-real-20260911T203042` |
| `qwen/qwen3guard-gen-8b` | `bf16-vllm-ppu-4505cb1a` | `asllm-2.0.0-vllm-0.23.0` | `full-qwen__qwen3guard-gen-8b-asllm-2.0.0-vllm-0.23.0-full-real-20260907T170425` |
| `zai-org/glm-4.7-flash` | `bf16-vllm-ppu-7dd20894` | `legacy-vllm-0.23.0` | `full-zai-org__glm-4.7-flash-legacy-vllm-0.23.0-full-real-20260911T122405` |

## Card-Count Constraints Express Topology, Not Evidence Levels

2026-09-15: Removed the resolver constraints that accepted only one card from 79 PPU specs.

Previously, the resolver for each PPU spec included `if gpu.count != N: reject(...)`, where N was the number of cards used in the passing run.
For the vast majority of models (79), N=1, so this constraint actually meant “we have only tested one card”—
an **evidence statement** had become a **runtime gate**, making these profiles completely unavailable on machines with 2/4/8 cards,
even though their weights are at most 72 GB, fit on a single 96 GiB card, and can also run with a standard TP layout across multiple cards.

Now:

- **Pin the card count only when the topology requires it**: The remaining five (DeepSeek-V4-Flash PP4, Laguna-S-2.1 TP2, Qwen3-235B TP8, Qwen3.5-397B TP4, and Qwen3.5-122B-A10B TP2) all have `-tp`/`-pp` in their names.
- **The other 79 do not pin the count**. The scheduler determines the card count, and TP equals the number of allocated cards—consistent with the NVIDIA specs in the catalog (NVIDIA specs have never had a placement block).
- **Keep the tested card count in the evidence**: In each record in the research ledger `pipeline/ledger/current/ppu-full-real-validation.json`,
  `parallelism.tensor_parallel_size` / `pipeline_parallel_size` records the actual topology of the passing run. A spot check confirms that all 79 above use `tp=1 pp=1`.
- `TestPPUCardCountPinsExpressTopologyNotEvidence` enforces this boundary: If a PPU spec pins the card count, the count must be >1 and its name must include a topology marker,
  preventing an “evidence statement” from becoming a gate again.

The tradeoff of removing these constraints must be clear: If you manually select 3/5/6 cards, the selection is also accepted (for example, TP=3).
If the model's attention head count is not divisible by 3, it fails only when the engine starts.
This matches the current NVIDIA specs. Tightening this requires only a card-count gate, but that would encode **the scheduler's power-of-two heuristic** as a model property, which this repository deliberately avoids.


## Deployment Information Still Required

- `Qwen/Qwen3-VL-32B-Instruct-FP8` (PPU profile `fp8-sglang-ppu-4bf2c2f3`): The PASS run only used text probes (sentinel/arithmetic/four factual questions). The research catalog classifies it as `kinds: ["text"]`, so the generic worker has never sent an image request.

  The profile only declares the model's own `input_modalities: [text, image]`, and its name has no `vision` marker. A real image request must be tested on physical PPU hardware (a base64 data URL in `image_url.url`, the same validation method used for the two Qwen2.5-VL profiles) before it can be promoted to a vision contract.
- `Qwen/Qwen2.5-VL-7B-Instruct-AWQ` / `Qwen/Qwen2.5-VL-72B-Instruct-AWQ`: The image requests in the passing runs used base64 data URLs in `image_url.url`, not multipart or a separate image field. Use the same structure when adding sample images.
- `FunAudioLLM/Fun-ASR-Nano-2512-hf`: The research ledger records `transformers-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `Qwen/Qwen3-ASR-0.6B-hf`: The research ledger records `transformers-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `Qwen/Qwen3-TTS-12Hz-0.6B-Base`: The research ledger records `qwen-tts-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice`: The research ledger records `qwen-tts-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `bosonai/higgs-audio-v3-tts-4b`: The research ledger records `asllm-2.0.0-vllm-0.23.0`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec. The PPU profile declares `ref_audio` but has no `audio-tokenizer` auxiliary component; local encoder loading and real reference-audio requests remain unverified. The NVIDIA fix does not establish PPU support.
- `meituan-longcat/LongCat-Image-Edit`: The research ledger records `asllm-2.0.0-vllm-omni`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `mistralai/Voxtral-4B-TTS-2603`: The research ledger records `asllm-2.0.0-vllm-omni`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `nvidia/Cosmos3-Nano`: The research ledger records `asllm-2.0.0-vllm-omni`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `nvidia/nemotron-3.5-asr-streaming-0.6b`: The research ledger records `transformers-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `zai-org/GLM-ASR-Nano-2512`: The research ledger records `transformers-native`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.
- `zai-org/GLM-TTS`: The research ledger records `asllm-2.0.0-vllm-omni`; the callable/native entrypoint, runtime assets, API contract, and image digest must be added from the original run records before it can become an official GPUWorks spec.

<!-- translation-source-sha256: dd334db53ca533a4c76381995a3a97741ce847aed0731c5ab8e6b2f781b83fb3 -->
