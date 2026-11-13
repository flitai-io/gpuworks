# Inference Catalog model.yaml Syntax Reference

This document provides a field-by-field syntax reference for `models/<model-id>/model.yaml`. A model can have multiple specs (profiles);
each spec describes a deployable configuration, including applicable GPUs, the image, weight sources, and launch arguments. The authoritative definitions are in the code:
structs and basic validation are in `internal/server/inferencecatalog/flat.go`, executable contracts
(assets / placement / api / components) are in `contracts.go`, the operation vocabulary is in
`internal/server/inferenceapi`, the Starlark evaluator is in `internal/server/placement`,
and the estimation subset is in `pkg/modelconfig`. For background on catalog package layout, digests, and `model_config`, see
[Executable Inference Catalog Packages](inference-catalog-package.md); for design rationale, see ADR 0009/0010.
If the two documents conflict, the code takes precedence. When you find an inconsistency, update the documentation first or open an issue.

The parser (`ParseYAML`) follows these rules throughout this document:

- **Unknown keys are rejected outright** (`KnownFields(true)`); retired fields cannot silently survive import.
- A file contains exactly one YAML document.
- Export (`MarshalYAML`) also validates first, so every exported document can be imported again.
- Case-insensitive fields (model_id, source, engine, quantization, vendor, and so on) are always
  normalized to lowercase before storage. model_id and spec names must be unique within their respective scopes.

## Overview

```yaml
models:                       # FlatCatalog: a single top-level models list
- model_id: wan-ai/wan2.1-t2v-1.3b-diffusers   # Required; org/repo format, unique after lowercasing
  source: upstream            # upstream | user; default: upstream
  display_title: wan2.1-t2v-1.3b-diffusers
  parameter_count: 1.3B class # Free text
  architecture: WanPipeline / WanTransformer3DModel / ...   # Free text
  input_modalities: [text]    # text | image | audio | video; deduplicated
  model_config: {kv_cache: false}                # Estimation subset; see package documentation
  readme_zh_cn: README.zh-CN.md                  # Only these two fixed values are allowed
  readme_en: README.en.md
  kv_bytes_per_token: {vllm: 12288, sglang: 12288}  # Measured per engine
  context_length: 40960
  release_date: '2025-03-01'  # YYYY-MM-DD; must be a valid date
  unsupported:                # Model-level unavailability declaration; the UI displays reason
  - gpus: {vendor: nvidia, cc: '<8.0'}
    quantization: bf16        # Optional: restrict to a quantization
    reason: Insufficient compute capability       # Required
  specs:                      # At least one spec per model, except models referenced as components
  - name: bf16-sglang-nvidia-general   # Unique within the model
    display_name_zh_cn: A100 / H100 · BF16   # Optional; see Display Names below
    display_name_en: A100 / H100 · BF16
    scenario: balanced        # latency | balanced | throughput
    engine: sglang            # vllm | sglang | callable
    image: gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2
    image_sources: []         # Optional additional image pull references; see image_sources below
    weights:
      quantization: bf16      # Required
      sources:                # At least one; compact single-key form
      - modelscope:
          repo_id: Wan-AI/Wan2.1-T2V-1.3B-Diffusers
          revision: 9c0c3b1a54efbd0d1674386cb8dd720fa4e6dc3d
    weight_bytes: 28935653617
    shm_bytes: 429496729600   # Optional /dev/shm bytes required by the engine (400 GiB here)
    memlock_unlimited: true   # Optional: unlimited locked memory in the container (--ulimit memlock=-1:-1)
    gpus:
      vendor: nvidia          # Required
      cc: '>=8.0'             # Exact value / >=x / <x / a..b
      name: '(^|[^A-Za-z0-9])(?:A100|H100)([^A-Za-z0-9]|$)'   # Optional case-insensitive regular expression
    env:
      NCCL_NET_PLUGIN: none
    args:
    - --num-gpus
    - ${GPU_COUNT}            # Placement/runtime variable expansion
    entrypoint: [sglang, serve]   # Override the default engine entrypoint; see below
    placement:
      language: starlark      # Currently the only language
      resolver: |
        def resolve(gpu):
            if gpu.min_memory_gib < 32:
                return reject("each GPU requires at least 32 GiB")
            return accept(gpu_memory_mib_per_device=32768,
                          variables={"GPU_COUNT": gpu.count})
    api:
      health_endpoint: /health
      operations:
      - type: openai.video.generate
        endpoint: /v1/videos
        mode: async
        inputs:
        - {name: seconds, kind: number}
        - {name: size, kind: select, options: [640x360, 1280x720]}
    components: [...]         # Multiple weight sets; see below
```

## Model-Level Fields

| Field | Constraints | Description |
|---|---|---|
| `model_id` | Required; matches `^\S+(/\S+)*$` and contains no dot segments or backslashes | Normalized to lowercase and unique across the catalog; also the directory name within the package |
| `source` | `upstream` \| `user`; defaults to `upstream` | Source category |
| `display_title` / `parameter_count` / `architecture` | Free text, trimmed | For display |
| `input_modalities` | Each item ∈ `text/image/audio/video` | Deduplicated while preserving order; drives input modality display in the UI |
| `model_config` | Shape of `modelconfig.Config` | The estimation subset of config.json + catalog-specific `kv_cache: false`; see the package document |
| `readme_zh_cn` / `readme_en` | Only `README.zh-CN.md` / `README.en.md` are allowed | Declares that the package includes the corresponding README |
| `kv_bytes_per_token` | Map: engine name → nonnegative integer | **Measurements from the engine itself**, not derived values; a missing key means no value is available for that engine, and the deployment form shows “No token capacity” instead of an incorrect value |
| `context_length` | Nonnegative | Context length declared by the model |
| `release_date` | `YYYY-MM-DD` and parseable | |
| `unsupported[]` | `gpus` (same selector as below) + required `reason` + optional `quantization` | The UI displays the reason for unavailability when matched |

## Spec-Level Fields

| Field | Constraints | Description |
|---|---|---|
| `name` | Required; unique within the model | |
| `display_name_zh_cn` / `display_name_en` | Optional; one line, at most 56 / 72 characters respectively; each unique within the model and engine (ignoring case and extra spaces) | The spec's Chinese and English display names. The UI shows the one for its language, falls back to the other, and shows `name` when there is neither. Built-in specs set both. See [Display Names](#显示名) |
| `scenario` | `latency` \| `balanced` \| `throughput` | |
| `engine` | Required; resolved through the registry at deployment time; valid values are `vllm` / `sglang` / `callable` (`internal/engines` is the sole source) | `callable` runs the Serve Adapter bundled with the catalog, with no log dialect or parameter dictionary; **the template controls the entire command line**. The engine appends no arguments. The template places platform facts such as model paths, ports, and multi-node ranks using [variables](#变量展开), and must reference `${API_PORT}` (ADR 0036) |
| `image` | Required | For image reference syntax and drift, see [Image Families](image-families.md) and todo.md T2 |
| `image_sources[]` | Optional; each item is nonempty after trimming, contains no whitespace or `://`, is ≤ 512 characters, and is unique; entries identical to `image` are dropped (see reference comparison below) | Image sources declared by the template itself, included among the image's own candidate copies when a node starts a replica; see [image_sources](#image_sources模板声明的镜像来源) |
| `weights.quantization` | Required; lowercase | |
| `weights.sources[]` | At least one | See below |
| `weight_bytes` / `runtime_weight_bytes` | Nonnegative; the latter ≤ the former | Inputs to scheduling and estimation |
| `shm_bytes` | Optional integer in bytes; > 0 and ≤ 16 TiB (`1<<44`), otherwise returns the stable error code `CATALOG_SHM_BYTES_INVALID`; an explicit `0` is equivalent to omission | The container `/dev/shm` size required by the engine itself, for example, an engine that stores large lookup tables in shared memory. If the deployment does not specify a shared memory size, the container receives the larger of this value and “engine baseline + CPU memory tier”; an explicit deployment value takes precedence. See [Container `/dev/shm` Size](deployment-launch-sources.md#devshm-大小) |
| `memlock_unlimited` | Optional Boolean, default `false`; non-Boolean values (numbers, strings, and so on) are rejected | When `true`, the deployment's container uses `--ulimit memlock=-1:-1` (removing the locked-memory limit only for this container), for engines that lock large amounts of host memory into RAM (for example, DeepInfer uses `cudaHostRegister` to lock about 197 GB). If the deployment's own `docker_args` already specifies `memlock`, that setting takes precedence and no duplicate is appended; this is merged with the `memlock` injected for host RDMA networking into a single entry. See [Locked Memory](deployment-launch-sources.md#锁定内存memlock) |
| `gpus` | Required | See below |
| `args` | String array | Included in the engine command line; supports `${VAR}` [variable expansion](#变量展开); split using the parameter dictionary at deployment time (see below) |
| `env` | Nonempty keys | Container environment variables; values support `${VAR}` expansion |
| `pre_start_argv` / `pre_start_script` | **Mutually exclusive**; argv entries are nonempty; script ≤ 64 KiB | Executed inside the container before the engine starts |
| `assets[]` | See below | Small files distributed with the package |
| `entrypoint` | Nonempty entries | See the dedicated section below |
| `placement` | See below | Topology derivation |
| `api` | See below | External HTTP contract |
| `components[]` | See below | Combinations of multiple sets of weights (speculative decoding, and so on) |

### args: Decode Known Parameters Into Fields and Pass Unknown Tokens Through Verbatim

Fill in the argument list (argv) exactly as provided by the upstream launch recipe. Each element is one command-line token.
At deployment time (ADR 0020), this list is split using the parameter dictionary (`internal/server/deploy/param_specs.go`):

- Flags recognized by the dictionary are decoded into launch configuration fields for estimation, scheduling, and form access.
- Unrecognized tokens retain their original order and enter the engine command line verbatim as the template's remaining arguments.

A flag shared by multiple keys (vLLM's `--speculative-config` and `--kv-transfer-config`) also
stays in the template's remaining arguments; it is not guessed to belong to a particular field. A flag owned by only one key that has a value the dictionary cannot decode
(a type mismatch, a value outside the enumeration, or invalid JSON) does not block the import either: the flag and its value stay in the remaining arguments and reach the engine verbatim,
and the import raises a `CATALOG_ARG_PASSED_THROUGH` warning naming the model, spec, flag and value. When the dictionary lags behind the engines
(a new parser, a new KV precision), the template still works; only the deploy form and the memory estimate do not see the value. Such a warning on the built-in catalog means the dictionary needs refreshing.

Case differences and aliases do not count as decoding failures: `--dtype half`
and `--speculative-algorithm eagle3` are both accepted and normalized to the dictionary's spelling (`float16` and `EAGLE3`) before storage.
The dictionary uses each engine's own spelling: SGLang's speculative algorithms are uppercase `EAGLE`/`EAGLE3`/`NEXTN`/`DSPARK`/
`NGRAM`/`STANDALONE` (its model hook compares against uppercase values before normalization), while vLLM uses lowercase method names. For KV precision, each engine's list
includes all upstream values (vLLM's `CacheDType` and SGLang's `--kv-cache-dtype` choices).

Similar names, different vocabularies: `--quantization` is the engine's quantization method (such as `modelslim`), while `weights.quantization`
is the weight format (such as `int8`). They are not cross-validated.

If a spec depends on a nondefault value for a dictionary parameter (for example, a higher
`--mem-fraction-static` when the placement contract declares GPU memory per device close to its full capacity), include it in `args`: the deployment configuration is determined only by these arguments and the user form, with no
implicit default fallback.

Hard requirements of vendor engine builds must also be specified in `args` for each spec; the platform does not inject them based on vendor. For example, the SGLang
build for Hygon DTK has a flash-attention kernel that accepts only 64-token paged KV blocks. With SGLang's default `--page-size 1`,
loading and graph capture complete and `/health` passes, but the first `forward_extend` reports
`Paged KV cache block size must be 64` and triggers a VMFault, while the container process does not exit. On DCUs,
fa3 is also selected automatically when `--attention-backend` is omitted, so every hygon (or DTK image) sglang spec must contain exactly
one `--page-size 64`; `internal/server/inferencecatalog/seed/hygon_sglang_test.go` validates every spec
in the built-in catalog. An explicit `--page-size` in the deployment form takes precedence over the template, and the flag still appears only once in the launch command.

### weights.sources: Compact Single-Key Form

Each source is a map with exactly one key, where the key is the provider (`huggingface` | `modelscope`)
and the value is a repository ID or details:

```yaml
sources:
- huggingface: org/repo                     # Scalar form
- modelscope:
    repo_id: org/repo                       # Required; no newlines
    revision: <commit>                      # Optional; pin the weight contents
    subfolder: sub/dir                      # Optional; normalized (no leading / or ..)
```

`subfolder` selects one subtree of the repository by directory prefix, and the download keeps the repository-relative paths. When no directory has that name, it can name a single file in the repository and only that file is downloaded (for example, one adapter from a repository that keeps many LoRA files at its root); when a directory and a file share the name, the directory is selected.

Multiple sources in the same spec mean that any one can be pulled. The deployment's `model_source.provider` selects which provider's weights to run; [pull settings](../how-to/configure-pull-settings.md) determine how the node obtains them (already present, copied within a sync group, read from shared storage, or downloaded).

### image_sources: Image Sources Declared by the Template

As with `weights.sources`, a spec can declare multiple sources for its runtime image. `image` remains required: it identifies the template's image
and serves as the comparison baseline for whether the deployment deviates from the template. `image_sources` provides additional references from which the same image can be pulled:

```yaml
image: ghcr.io/org/runner@sha256:22789f0c…        # Template image (upstream)
image_sources:                                    # In preference order
- swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:22789f0c…   # Public copy with identical bytes
- ghcr.nju.edu.cn/org/runner:v0.6.0                                       # ghcr mirror; tag reference only
```

**The platform accepts what the template declares and does not judge on the author's behalf**: sources do not have to pin the same digest as `image`. Even when `image` pins a digest,
a source can use a tag reference, and reachability is not checked. Validation covers syntax only: each item must be nonempty after trimming, contain no whitespace or
`://` (download URLs for image archives belong in the image catalog, not templates), be no longer than 512 characters, and be unique.

Two references are compared using registry parsing rules: `nginx:1` and `docker.io/library/nginx:1` are the same; registry hostnames and repository names
are case-insensitive, but tags are case-sensitive (`:V1` and `:v1` are different images).
Entries identical to `image` are dropped: `image` is already a candidate, listed after the declared sources, so repeating it adds no information. Export also
writes only the normalized list.

**Usage at deployment time** (for the decisions, see [ADR 0038](../adr/0038-template-image-sources.md) and [ADR 0048](../adr/0048-pull-settings.md)):

- A deployment does not select an image source. When a node starts a replica, the first candidates are replacement addresses generated by [source rules](../how-to/configure-pull-settings.md#来源规则) applicable to that node, followed by the image's own copies: the template's `image_sources` (in declared order) → the catalog's
  `registry_sources`, if the image also happens to be registered in the image catalog → `image` itself, with duplicates removed. Image archive addresses registered in the catalog are files to download, not candidates. The node probes `https://<host>/v2/` to rank these copies by reachability and latency, trying the next only if the previous one fails or its download stalls. Using a source declared by the template **does not count as deviating from the template**.
- Source rules apply to all images, including template-declared sources: replacement addresses from rules come first, and rules that disable fallback to origin cause the matching copy itself to be discarded. Otherwise, template-declared sources are used as is, without rewriting.
- These sources are available only when the deployment's image equals the spec's `image`. Overriding the image in the request (a different `image` field) means it is a different image,
  so the template's sources are not used.
- When `image` is a digest reference (`repo@sha256:…`), the runtime cannot tag a digest reference, so the node starts the container directly
  with whichever candidate it pulls, without checking whether its digest matches `image` (see [Troubleshooting](../how-to/troubleshooting.md#镜像拉取失败)).
  The instance records the image copy list at that time (`image_sources`); when it is dispatched, candidates are recalculated using the source rules in effect then.

### GPU Selectors (gpus / unsupported[].gpus)

- `vendor`: Required. Use a vendor key defined in `internal/accelerator`
  (`nvidia` / `amd` / `ascend` / `ppu` / `hygon`), which is matched against the normalized value reported by the node.
  An unknown value does not cause an error, but never matches hardware.
- `cc`: Optional; four valid forms—exact (`8.0`), `>=8.0`, `<9.0`, or a closed interval `8.0..9.0`
  (only `>=` and `<`, with no `>` / `<=`; intervals require lo ≤ hi).
- `name`: Optional regular expression; `(?i)` (case-insensitive) is added automatically when matching. Use
  boundaries such as `(^|[^A-Za-z0-9])...([^A-Za-z0-9]|$)` to avoid matching `A1000` with `A100`.

### Display Names

`display_name_zh_cn` / `display_name_en` are written by hand. They sum up which cards the spec is for and what tells it from its siblings, in the form `cards · quantization [· key limit or shape]`:

- **Cards** match what `gpus` actually matches: for a compute-capability range only, the architecture (`>=8.0` is "Ampere 及以上" / "Ampere+", `>=9.0` is "Hopper 及以上" / "Hopper+", `>=10.0` is "Blackwell"); for named models, the models, such as "H20 / H200", "910C (A3)" or "K100 / BW". No vendor name.
- **Quantization** is always written, in upper case: `BF16`, `FP8`, `NVFP4`, `INT8 W8A8`, `INT4 AWQ` and so on; when one quantization comes from several weight publishers, the publisher goes in parentheses, such as "NVFP4 (NVIDIA)".
- **The rest** only states what tells siblings apart or a hard limit users must know, in this order: a fixed card count ("8 卡" / "8 GPUs"), a memory threshold, the context length, the shape ("投机解码" / "Speculative", Engram offload, multi-node, PD disaggregated, an embedding task), the scenario (only when siblings differ in nothing else, such as "High throughput"), and the image (only when siblings differ in nothing else: the engine or CUDA version, such as "CUDA 12.9").
- No engine name and no model name.

For example, GLM-5.3-Flash's two vLLM Hopper specs are "H20 / H200 141G · FP8 · 8 GPUs · 1M context" and "H100 / H20 96G · FP8 · 8 GPUs · 256K context".

The `display_profile` and `gpus.display_target` keys of earlier templates are no longer used: an import still reads them, ignores them and raises the `CATALOG_DISPLAY_PROFILE_RETIRED` warning (see [Pre-Import Checks and Warnings](#导入前检查与警告)).

### assets[]

| Field | Constraints |
|---|---|
| `file` | Must be under this model's own `assets/` directory (relative, clean, with backslashes normalized to `/`) |
| `sha256` | Required; 64 lowercase hexadecimal digits |
| `size_bytes` | Nonnegative |
| `unpack_to` | Absolute path in the container (clean, not `/`) |

Images, weights, framework libraries, and venvs cannot be included in assets; see the package document for the boundaries and rationale. File-valued
arguments of the `--*-plugin` class must be supplied by the spec itself (assets / components / pre_start /
entrypoint text) or through an absolute path. Otherwise, validation returns the stable error code
`CATALOG_FILE_VALUED_ARG_UNRESOLVED` (see the package document for this rule's boundaries and rationale).

## entrypoint: The Sole Declaration Point for Entrypoint Overrides

**Entrypoint forms come from only two sources**:

1. **The engine builder's default form** (`internal/server/deploy`): vLLM builds
   `vllm serve <model> ...`, and SGLang builds `python -m sglang.launch_server ...`.
2. **The spec's `entrypoint` override**: at deployment time, `WithEntrypoint` **replaces the executable portion
   while preserving the entire argument vector**. All builder-injected arguments, such as the model path, topology, and API address, remain;
   only the initial executable tokens are replaced.

You can additionally apply container-level overrides (editing entrypoint/command in the deployment form or using manual command
mode), which take higher precedence. The agent executes the entrypoint sent by the server and **does not infer an entrypoint**: entrypoint selection happens only
when the server assembles the launch configuration. Runtime detection of the checkpoint layout cannot rewrite it (this approach was considered during review and
rejected on 2026-09-15). Manual deployments (without a catalog spec) do not provide nondefault entrypoints. Models that need a nondefault entrypoint
should be added to the catalog with an `entrypoint` declaration.

Authoring conventions:

- **Only diffusion (diffusers pipeline) specs must declare `entrypoint: [sglang, serve]`**.
  Diffusion dispatch logic exists only in the `sglang serve` CLI (`sglang/cli/serve.py`); the legacy
  module can never reach it. A working example is `wan-ai/wan2.1-t2v-1.3b-diffusers` (verified end to end on A100).
- **Multimodal models such as ASR / VLM do not need an override**: they use built-in extensions of SGLang's main LLM server,
  which are reachable through the legacy entrypoint and accept all main ServerArgs parameters (tests confirm that Qwen3-ASR-1.7B loads
  `Qwen3ASRForConditionalGeneration` successfully through `python -m sglang.launch_server`,
  accepts both `--enable-metrics` and `--served-model-name`, and returns 200 for `/health` and `/metrics`).
  The sglang spec for `qwen3-asr-1.7b` in the catalog correctly omits entrypoint.
- **vLLM does not need an override**: `vllm serve` is the unified entrypoint, and no vLLM
  video/image/audio spec in the built-in catalog declares entrypoint.
- If a vendor-built image has a different entrypoint, declare it for each spec (for example, the hygon build of
  `qwen3-tts-12hz-1.7b-voicedesign`).
- An override changes only the entrypoint, not the arguments. The diffusion argument parser does not recognize LLM arguments such as `--mem-fraction-static`
  and `--context-length`; `--enable-cache-report` requires 0.5.18, and `--enable-metrics`
  requires 0.5.21 or later (versions before 0.5.18 also do not recognize `--served-model-name`). For specs with `--model-type diffusion`
  or without `--model-type`, the platform uses the image version to determine which defaults for these arguments it does not inject (see
  [Launch Configuration](../explanation/launch-config.md); served-model-name is not injected when the deployment alias is empty).
  The spec's own args must also be in the vocabulary accepted by the target server's parser. entrypoint cannot resolve
  argument conflicts.

## placement: Starlark Topology Derivation

```yaml
placement:
  language: starlark        # Only valid language
  resolver: |
    def resolve(gpu):
        if gpu.min_memory_gib < 32:
            return reject("each GPU requires at least 32 GiB")
        return accept(gpu_memory_mib_per_device=32768,
                      variables={"GPU_COUNT": gpu.count, "SP_DEGREE": 1})
```

- You must define `resolve(gpu)`. `gpu` is a struct with `count`, `vendor`, `names`,
  `memories_gib`, `min_memory_gib`, `indexes`, and `automatic`.
- There are only two predeclared built-ins: `accept(gpu_memory_mib_per_device, variables?, prefer?)` and
  `reject(reason)`. `gpu_memory_mib_per_device` is an **admission requirement** (1..16777216 MiB;
  the upper limit only prevents int64 overflow), not an allocation request.
- `prefer` is an optional integer (default 0, range -1000000..1000000; out-of-range or noninteger values cause an immediate error) that expresses
  which GPU count is preferred when multiple counts are acceptable. Automatic placement calls the resolver once for each candidate GPU count (1, 2, 4, 8…),
  compares `prefer` among **accepted** candidates first, and selects the highest value. Only when `prefer` values are equal does the existing score
  (topology, bin packing, and fit) decide. If no resolver passes `prefer` (all values are 0), automatic placement uses
  the smallest GPU count that fits. For example, a resolver accepting 4 or 8 GPUs can use
  `accept(..., prefer=gpu.count)` to select 8 GPUs on a node with 8 idle GPUs, while still selecting 4 when only 4 are idle.
  `prefer` affects only automatic GPU count selection: when the user specifies parallelism or GPU indexes, the candidate count is already fixed, and `prefer`
  does not change it. A nonzero `prefer` appears in the scheduling explanation as a `placement_preference` reason.
  `prefer=gpu.count` is permitted bounded arithmetic (`gpu.count`, `gpu.min_memory_gib`, and literals).
- Evaluation is deterministic and performs no I/O: `load`/import is prohibited, and `print` is a no-op. Limits: source
  ≤ 32 KiB, ≤ 100k steps, ≤ 30s wall-clock time (only a fallback; the deterministic limit is the step count), ≤ 64 result variables /
  ≤ 4096 bytes of total string content.
- Variables returned by the resolver enter the `${VAR}` expansion context; variable names must match
  `^[A-Z][A-Z0-9_]*$`.

A text spec that relies on tensor parallelism alone, with no expert, data or pipeline parallelism, does not pin a card count in its resolver. It states a memory condition instead: card count × (memory per card × the engine's memory share − about 2 GiB of runtime overhead) must be at least the weights plus the KV cache needed at start. vLLM refuses to start unless one maximum-length request's KV cache fits, computed as `--max-model-len` (or the model's `context_length` when absent) × `kv_bytes_per_token`; SGLang only needs a KV pool, counted as 4 GiB. Among the counts that qualify, the scheduler takes the fewest and keeps the tensor-parallel size a divisor of the attention heads. A spec measured to pass only at a particular card count keeps that count pinned and says why.

### Variable Expansion

`${NAME}` in `args` / `env` / `entrypoint` / `pre_start_*` is expanded on every deployment, regardless of whether placement
is present. Available variables = resolver return values + platform facts for this launch (the sole sources are
the vocabulary in `internal/server/placement/expand.go` and `internal/server/deploy/launch_variables.go`):

| Variable | Value | When Available |
|---|---|---|
| Any variable returned by the resolver | Placement result (such as `SP_DEGREE`) | When a resolver exists |
| `GPU_COUNT` | Number of GPUs allocated on this node (overrides a resolver-returned value with the same name) | Placement is determined |
| `TP_SIZE` | Tensor parallelism for the entire deployment; a resolver-returned value takes precedence | Placement is determined |
| `PP_SIZE` | **Intentionally has no default**: multi-node pipeline parallelism already has a single owner (the engine injects it based on node count); it can only be returned by the resolver | Only when returned by the resolver |
| `MODEL_PATH` | Mount path of the primary weights in the container (including a primary component or deployment override) | Always |
| `SERVED_MODEL_NAME` | The deployment's served name, which the gateway and the health check send to the engine as `model` | Always |
| `API_HOST` | Address the service binds to: `0.0.0.0` for callable, `127.0.0.1` for other engines | Always |
| `API_PORT` | Port the platform reserves for this instance, accessed by the gateway and health checks; `0` for a headless rank | Always |
| `NNODES` / `NODE_RANK` | Number of nodes and index of this rank; `1` / `0` for a single node | Always |
| `HEADLESS` | `1` if this rank does not serve external requests; otherwise `0` | Always |
| `NODE_ADDR` | Routable address of the node this rank runs on (not rank 0's) | Multi-node only |
| `MASTER_ADDR` / `MASTER_PORT` | Address and rendezvous port of rank 0 in a multi-node deployment | Multi-node only |

Rules:

- **The resolver cannot return platform facts**: returning `MODEL_PATH`, `SERVED_MODEL_NAME`, `API_*`, `NNODES`, `NODE_RANK`, `HEADLESS`, `NODE_ADDR`, or `MASTER_*`
  in resolver results causes an immediate error—the node, port, and rank are not yet available when the resolver runs.
- **Missing variables cause failure**: unresolved names in args / env / entrypoint / `pre_start_argv` cause deployment errors. A spec without a resolver
  that references a name outside the vocabulary (such as the typo `${NODE_RNAK}` or `${PP_SIZE}` without a resolver) is rejected during import. A single-node deployment
  that references `${MASTER_ADDR}` also fails instead of receiving an empty string.
- **`pre_start_script` is a shell script**: names outside the vocabulary (`${HOME}`, `${PATH}`) are left unchanged for the shell; names in the vocabulary but unavailable
  for the current startup still cause an error, because the shell would silently expand them to empty strings.
- **Served name**: the gateway rewrites the request's `model` to the engine's own name only for `vllm` / `sglang`; a `callable` engine receives the deployment's
  served name as is. An engine that checks the model (such as vLLM running under callable) therefore needs `--served-model-name ${SERVED_MODEL_NAME}`; a literal
  name makes every request and health check return 404. The value is arbitrary user text, so quote it inside shell strings (`bash -c`, `pre_start_script`).
- **Preview**: when placement is not yet determined, `GPU_COUNT`, `TP_SIZE`, and resolver variables remain as `$NAME` symbols. Other platform facts expand normally
  (the port is fixed at `30000` for preview).
- Mount paths for additional components (such as a DFlash draft) are determined by the spec's own `components[].mount_path`. Write the literal path directly;
  no variable is needed. Validation requires the path to appear in the startup fields.
- Arguments and command overrides entered by users in the deployment form are **never expanded**.

Minimal callable spec:

```yaml
engine: callable
entrypoint: [python3, /opt/gpuworks/runtime/gpuworks_serve.py]
args: [--model, ${MODEL_PATH}, --host, ${API_HOST}, --port, ${API_PORT}, --adapter, transformers_adapter]
```

### How Thresholds Relate to Scheduling Admission

The GPU memory threshold in placement and scheduling admission must pass independently. Before declaring `min_memory_gib`, calculate the scheduler's per-GPU
admission requirement = `weight_bytes/tp` + encoder runtime (multimodal towers, `pkg/estimate.MMEncoderRuntimeBytes`)
+ `reserve`; **KV is deliberately excluded from admission** and is displayed for information only. `reserve` is the engine's fixed cost, independent of model size.
Look it up by architecture and CUDA graph mode in `internal/server/estimate/reserve.go` (1330 MiB for Ampere piecewise,
4300 MiB for Hopper full-graph, plus a 768 MiB driver shortfall scaled by utilization;
turing..maxwell always use piecewise). Accordingly, admission for a 1B-class bf16 model requires about 4 GiB.
Rejecting `min_memory_gib < 6` is enough to allow 8G cards; raising the threshold by intuition only excludes hardware that can run the model.

Budget parameters such as `--gpu-memory-utilization` are vLLM's **pool-filling policy** (fill the KV pool to
util × GPU memory), not GPU memory requirements. On smaller cards, this determines warmup headroom—vLLM first fills the budget and then performs
grammar/multimodal warmup, causing OOM if headroom is insufficient. Therefore, express GPU memory tiers through args before splitting specs: a combination of
`--gpu-memory-utilization 0.85` + `--limit-mm-per-prompt '{"image":1}'` allows the same spec
to cover 8G cards through H100 (0.85 only leaves a slightly smaller KV pool on larger cards, with no impact on models that need little KV;
`--limit-mm-per-prompt` and `--max-model-len` both reduce peak multimodal profiling memory during startup—the latter
also limits the number of image tokens in a single prompt). Note that the spec's `--gpu-memory-utilization` and GPU
utilization in the deployment form (GPUMemUtil in planning, default 0.9) are two independent controls and are not synchronized.

## api: External HTTP Contract

Omitting `api` gives conservative defaults: only a `/health` health endpoint and no external operations. **Generative
(image/video/audio) models must explicitly declare operations**—the product category alone does not determine endpoints.

The model row's own interfaces (`operations`, editable on the Models page or through `PATCH /api/v1/models/{id}/inference-config`)
take precedence over the spec: when nonempty, the deployment uses the model row's interfaces, and the spec's `api.operations` does not override them.
When the model row's interfaces are empty (never set, or `[]`—because the task was not recognized at registration or someone cleared them), deploying with
this spec writes the interfaces declared by the spec into the model row. Clearing interfaces does not hide the model from Playground; the next template deployment fills them in again.
If the spec also declares no interfaces, the model's interfaces remain empty.

```yaml
api:
  health_endpoint: /health     # Absolute, clean path; no ? or #
  operations:
  - type: openai.video.generate   # See the vocabulary below
    endpoint: /v1/videos          # Uses the registry default if omitted; must be an allowed route for this operation
    mode: async                   # Defaults to the first supported value
    request_encoding: json        # Defaults to the first supported value
    request_defaults: {...}       # Request body defaults
    inputs:                       # Playground form declaration
    - {name: seconds, kind: number}
    - {name: size, kind: select, options: [640x360, 1280x720]}
    - {name: input_reference, kind: image, encoding: data_url}
    example_input:                # Optional working example for the primary text input (prompt / input / query)
      prompt: "A sunrise over mountains"
```

Operation vocabulary (`internal/server/inferenceapi` is the single source): `openai.chat`,
`openai.completions` (`/v1/completions`, continues the prompt as-is without applying a chat template), `openai.embeddings`, `openai.rerank`, `openai.audio.transcribe`,
`openai.audio.speech`, `openai.image.generate`, `openai.image.edit`, `openai.video.generate`,
and `openai.realtime`. Operation types must not repeat within a contract.

Special cases:
`openai.video.generate` must use `async` on `/v1/videos` and
`sync` on `/v1/videos/sync`. In addition to `/v1/audio/speech`, `openai.audio.speech` can declare `/v1/audio/generate`
(the vllm-omni route for text-to-audio models such as Stable Audio, with the same request and response shapes; their
`/v1/audio/speech` does not recognize clip duration). The `name` in `inputs` must not use the reserved word `model`.

`example_input` provides a ready-to-run value for models that require a specific format (such as the tagged prompt for `pfnet/plamo-2-translate`):
the curl / Python examples under "Usage" on the template page and the initial input in Playground use it instead of a generic example, preserving it exactly (including multiple lines and trailing newlines).
It only affects examples and initial values and never rewrites actual requests. Keys must be the operation's primary text input, and values must be nonempty. Examples also include `request_defaults`.

`max_reference_images` is valid only on `openai.image.edit` and declares how many reference images one edit accepts (a positive count, default 1; the platform sets no upper bound, so state what the engine actually accepts). Each reference image is one `image` part of the multipart request, in upload order; Playground uses it to allow selecting several images, show their order, and cap the count.

When a template creates a deployment, if the deployment's model does not yet have its own operations, the operations declared by the template (along with model-level
`input_modalities`) are written to the model as its initial operations. The deployment page, Playground, and its proxy read only the operations
on the model. Existing nonempty operations are not overwritten by subsequent deployments, except that type-only operations inferred from the repository's tasks at registration count as unconfigured and are replaced by the template's declaration. An empty list, including `[]` explicitly cleared on the model page, is populated from the next deployed template that declares operations.
Preview (evaluate) does not write to the model.

### api.tokenizer_ref: Independent Tokenizer Source

When the weight repository lacks independent tokenizer files, declare a verified tokenizer repository for the evaluation's **Automatic** source:

```yaml
api:
  tokenizer_ref: ms://Qwen/Qwen3.8-Flash-Next@2741eec155d03a8ce151b993ccce1a7b1e398d6b
  operations:
  - type: openai.chat
```

Only `hf://org/repo@<40-hex-commit>` or `ms://org/repo@<40-hex-commit>` is accepted; arbitrary URLs, file paths, and mutable branches are not allowed.
Automatic resolution still tries the inference service's tokenize endpoints and the node's existing vocabulary first. Without a node vocabulary, it uses this declaration, or the weight repository if none is declared. Manually selected sources are unaffected.

New deployments save the declaration in their snapshot, and an existing snapshot's declaration takes precedence. Deployments that have deviated from the template do not automatically use template declarations, including those in the snapshot. If an old snapshot lacks one, a declaration added to the current template applies only when the model, spec and source, engine, image, and the actual weights' provider, repository, revision, and subfolder all match. Managed evaluations from legacy clients that omit the source and leave the reference empty also use this declaration; tokenizer repositories are never guessed from model names or READMEs.

### api.metrics: Engine Metric Mapping

Declare the engine's metrics endpoint and how engine metrics map to the platform's [inference metric vocabulary](inference-metrics.md). For syntax, ways to inspect raw metrics, and examples, see
[Declare Engine Metrics for an Inference Template](../how-to/declare-engine-metrics.md).

```yaml
api:
  metrics:
    endpoint: /metrics            # Optional; default: /metrics; starts with /
    format: prometheus            # Optional: prometheus (default, Prometheus text) or json
    inherit: sglang               # Optional: vllm or sglang; apply that engine's built-in mapping first
    map:                          # Engine metric family (optional {k="v"} selector) -> vocabulary short name
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}   # Long form: scaling
```

- **Effective mapping** = inherited mappings (from the engine specified by `inherit`, or the built-in mappings for a `vllm` / `sglang` template's own engine) overlaid entry by entry with `map`; `map` takes precedence for the same key.
- **`format: json`**: the endpoint returns a JSON document (such as Strata's `/metrics`). Write `map` keys as dot-separated key paths (`totals.output_tokens`) to extract the number at the path (booleans become 1/0; missing or nonnumeric values produce no metric). For strings, write a comparison as `path==value` / `path!=value` (such as `live.state!=idle`), which yields 1 when true and 0 otherwise and can only map to a gauge. Only counters and gauges can be mapped; `inherit` is not allowed, and no built-in engine mapping is applied. Documents larger than 1 MiB or invalid JSON are not read at all.
- **When omitted**: `vllm` / `sglang` use built-in mappings. `callable` scrapes `/metrics` and automatically applies the corresponding mapping based on the `vllm:` / `sglang:` prefixes found; if neither is present, no engine metrics are produced.
- **Validation** (import, catalog package upload, editor save): targets must be vocabulary short names; histograms must map by family name, without selectors or scaling; the `_bucket` / `_sum` / `_count` lines of an already mapped histogram family cannot be mapped again; scale factors must be nonnegative; `inherit` must be `vllm` / `sglang`; `format` must be `prometheus` / `json`. With `json`, keys must be key paths (optionally with `==value` / `!=value` comparisons), targets cannot be histograms, comparisons cannot map to counters, and `inherit` is not allowed. Error codes are `CATALOG_METRICS_*`.
- **Takes effect at runtime**: mappings use the **current** declaration of the spec used by the deployment and are not frozen with the deployment snapshot. After a template is saved or the built-in catalog changes with a server upgrade, running instances referencing that spec update in place without restarting ([ADR 0039](../adr/0039-inference-metric-sources.md)). Deployments whose spec has been deleted continue to use the `api` contract in their snapshot. `api.metrics` is not considered when determining whether a deployment differs from its template.

### api.request_stats: Per-Request Statistics in Responses

Declare the timings that the engine writes into each response for that request, mapping them to the per-request statistics vocabulary ([`queue_seconds`, `prefill_seconds`, `decode_seconds`](inference-metrics.md#逐请求统计)).
When a response ends, the gateway reads these from the end of the response (the last event in a streaming response, or the JSON body in a non-streaming response) and writes them to the request record. The request view in Monitoring uses them to break completed requests into queue, Prefill, and Decode stages.

```yaml
api:
  request_stats:                  # Key: dotted path from the top-level event object; value: short name or {to, scale}
    tensorfold.prefill_s: prefill_seconds
    tensorfold.decode_s: decode_seconds
    timings.prompt_ms: {to: prefill_seconds, scale: 0.001}   # Convert milliseconds to seconds
```

- **Paths**: dot-separated key paths (`timings.prompt_ms`), whose first segment is a top-level key in the last response event (or JSON body). The extracted value must be a nonnegative number; otherwise, this item is omitted for the request.
- **Effective declaration** = the engine's built-in declarations overlaid entry by entry with template declarations; the template takes precedence for the same path. `vllm` has built-in declarations for `metrics.queue_time_ms`, `metrics.time_to_first_token_ms`, and `metrics.generation_time_ms` (`scale: 0.001`) and requires the deployment to include `--enable-per-request-metrics` (the "Return per-request timings" deployment parameter). `sglang` and `callable` have no built-in declarations.
- **Built-in templates**: TensorFold specs declare `tensorfold.prefill_s` / `tensorfold.decode_s`; Strata specs declare `timings.prompt_ms` / `timings.predicted_ms` (`scale: 0.001`).
- **Validation** (import, catalog package upload, editor save): paths must be key paths, targets must be per-request statistic short names, scale factors must be nonnegative, and the same path cannot be declared twice. Error codes are `CATALOG_REQUEST_STATS_*`.
- **When changes take effect**: when the routing table is rebuilt following catalog changes, declarations are resolved from the current spec used by the deployment. They are not frozen with the deployment snapshot, and instances do not need to restart. For tradeoffs, see [ADR 0042](../adr/0042-gateway-request-records.md).

## components: Multiple Weight Sets

Use this for scenarios in which a single spec mounts multiple sets of weights, such as speculative decoding (draft/verifier):

```yaml
components:
- name: target
  role: primary              # Exactly one
  weights: {...}             # Same structure as weights
  weight_bytes: 123          # Required and > 0
  mount_path: /models/target # Absolute container path; unique and non-nesting
- name: dspark
  role: speculative_draft    # Or verifier; both require model_id referencing a catalog model
  model_id: redhatai/glm-5.2-speculator.dspark
  weights: {...}
  weight_bytes: 45
  mount_path: /models/draft
```

`role: auxiliary` is for weights the engine reads in addition to the primary model (such as Wav2Vec2-BERT,
CAMPPlus, and the BigVGAN vocoder for IndexTTS 2.5). This is not a standalone model in the catalog. You can omit `model_id`; when omitted, it resolves
against the model owning the spec, just like primary.

A non-primary component's `mount_path` must be referenced in the startup contract text (args / entrypoint /
pre_start / env), or validation fails—unused mounts and mistyped paths are caught during import.
Models referenced as components can have no spec of their own (draft weights appear in a separate row and cannot be deployed independently).

A model referenced by `model_id` must be in the same document/catalog package or already exist on the server. Import forms handle this as follows:

- **Import YAML, including saving in the template editor**: the server includes existing component models in validation but writes only the document's own models.
  The server generates the template editor's YAML using the same catalog types; it contains only the current model, not the component models it references. Empty fields are omitted, and defaults are not added.
- **Clone or edit a template with documentation and assets**: the generated catalog package also includes referenced component models; these entries contain no specs or documentation.
- **Upload a catalog package**: the package must still include the component models it references.

## Pre-Import Checks and Warnings

`POST /api/v1/inference-templates/evaluate` parses and validates the same request body (flat YAML or a `.gpuworks-catalog.tar.gz` catalog package; administrator access required) as an import would, without writing anything, and returns:

```json
{"valid": true, "errors": [], "warnings": [{"code": "CATALOG_CALLABLE_METRICS_UNDECLARED", "params": {"model_id": "acme/adapter", "spec": "serve"}, "error": "…English text…"}]}
```

If validation fails, `valid` is `false`, and `errors` contains the same error codes an import would return. Parsing errors without a dedicated code use `CATALOG_INVALID` (`params.detail` is the parser's original message). It does not check whether a user spec with the same name already exists (which returns 409 during import), nor does it retrieve weight sizes.

Warnings do not block import. A successful import response includes the same `warnings` (the field is absent if there are no warnings). Warnings are produced only by `inferencecatalog.CatalogWarnings`. Each contains a stable `code`, an English `error`, and `params` (`model_id`, `spec`); the UI renders localized text based on `code`:

| Warning Code | Trigger | Consequence |
|---|---|---|
| `CATALOG_CALLABLE_METRICS_UNDECLARED` | `engine: callable` without `api.metrics` | Monitoring has no engine metrics for this spec unless the engine's `/metrics` happens to use vLLM / SGLang metric names (see [Declare Engine Metrics for an Inference Template](../how-to/declare-engine-metrics.md)) |
| `CATALOG_HEALTH_ENDPOINT_UNDECLARED` | `engine: callable` without `api.health_endpoint` | Readiness probes use the default `/health`; no warning is shown for `vllm` / `sglang`, whose engines provide `/health` natively |
| `CATALOG_ARG_PASSED_THROUGH` | A dictionary parameter in `args` carries a value the dictionary cannot read (`params.flag` / `value` / `key`) | Passed to the engine verbatim; the deploy form and the memory estimate do not see the value |
| `CATALOG_DISPLAY_PROFILE_RETIRED` | The spec still sets `display_profile` or `gpus.display_target` | Both are ignored; the UI shows `display_name_zh_cn` / `display_name_en` (or `name` when there is neither) |
| `CATALOG_CALLABLE_SERVED_NAME_FIXED` | In an `engine: callable` spec, the `--served-model-name` value in `args` / `entrypoint` does not use `${SERVED_MODEL_NAME}` (`params.name` is the literal) | An engine that checks the model accepts only that literal, so requests and health checks return 404 whenever the deployment's served name differs; ignore it if the adapter ignores the model or the name is pinned on purpose |

## Validation Entry Points and Tools

- `ValidateFlatCatalog`: structural validation (per spec) + cross-model validation (component references,
  component-only models). Import, export, and seed loading all use it.
- Contract tests in `internal/server/inferencecatalog/seed` traverse the built-in catalog and assert weight
  revisions, contracts, placement, and offline command assembly. For assertions related to `model_config`, see the package documentation.
- `cmd/gpuworks-catalog-model-config`: backfills `model_config` from weight sources declared by the spec
  (reports only by default; `-write` writes to disk; gated repositories require `HF_TOKEN`, and failed reads fall back to
  `fallback-sources.json`).
- The image catalog (system image manifest) is a separate domain. For the format, see
  [Image Catalog and Inference Image Families](image-families.md#manifest-格式).
