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
    start_timeout_sec: 5400   # Optional: instance start timeout (seconds) for a recipe with a long first start
    docker_args: [--cap-add=SYS_PTRACE]   # Optional: extra docker run options the engine needs
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
| `engine` | Required; resolved through the registry at deployment time; valid values are `vllm` / `sglang` / `callable` (`internal/engines` is the sole source) | `callable` runs the Serve Adapter bundled with the catalog, with no log dialect or parameter dictionary; **the template controls the entire command line**. The engine appends no arguments. The template places platform facts such as model paths, ports, and multi-node ranks using [variables](#变量展开), and must reference `${GPUWORKS_API_PORT}` (or the older spelling `${API_PORT}`, ADR 0036) |
| `image` | Required | For image reference syntax and drift, see [Image Families](image-families.md) and todo.md T2 |
| `image_sources[]` | Optional; each item is nonempty after trimming, contains no whitespace or `://`, is ≤ 512 characters, and is unique; entries identical to `image` are dropped (see reference comparison below) | Image sources declared by the template itself, included among the image's own candidate copies when a node starts a replica; see [image_sources](#image_sources模板声明的镜像来源) |
| `weights.quantization` | Required; lowercase | |
| `weights.sources[]` | At least one | See below |
| `weight_bytes` / `runtime_weight_bytes` | Nonnegative; the latter ≤ the former | Inputs to scheduling and estimation |
| `shm_bytes` | Optional integer in bytes; > 0 and ≤ 16 TiB (`1<<44`), otherwise returns the stable error code `CATALOG_SHM_BYTES_INVALID`; an explicit `0` is equivalent to omission | The container `/dev/shm` size required by the engine itself, for example, an engine that stores large lookup tables in shared memory. If the deployment does not specify a shared memory size, the container receives the larger of this value and “engine baseline + CPU memory tier”; an explicit deployment value takes precedence. See [Container `/dev/shm` Size](deployment-launch-sources.md#devshm-大小) |
| `memlock_unlimited` | Optional Boolean, default `false`; non-Boolean values (numbers, strings, and so on) are rejected | When `true`, the deployment's container uses `--ulimit memlock=-1:-1` (removing the locked-memory limit only for this container), for engines that lock large amounts of host memory into RAM (for example, DeepInfer uses `cudaHostRegister` to lock about 197 GB). If the deployment's own `docker_args` already specifies `memlock`, that setting takes precedence and no duplicate is appended; this is merged with the `memlock` injected for host RDMA networking into a single entry. See [Locked Memory](deployment-launch-sources.md#锁定内存memlock) |
| `start_timeout_sec` | Optional integer, seconds; 60–86400 (the same bounds as the deployment's own `start_timeout_sec`), otherwise the stable error code `CATALOG_START_TIMEOUT_INVALID` is reported; an explicit `0` is equivalent to omitting it | Startup health check budget for instances of this spec, for recipes whose first start converts weights or warms caches for tens of minutes. It is the template step of the startup wait limit (source `template`): cluster default → template → raised to the engine deadline → the deployment's `start_timeout_sec`; for the order and rules see [Startup Wait Limit](deployment-launch-sources.md#启动等待上限). Saved with the spec snapshot, so later template changes do not affect existing deployments |
| `docker_args` | Optional string array; each item nonempty after trimming; validated exactly like the deployment's own `container_overrides.docker_args` (`deploy.ValidateDockerArgs`): options that hand the container the host (`--privileged`, `--pid`, `-v /`, and so on) are rejected, as are options the platform already manages (`--name`, `--network`, `--gpus`, `--shm-size`, `-e`, `-v`, `--mount`, `--entrypoint`, and so on); `--ipc=host` together with this same spec's own `shm_bytes` is also rejected | Extra `docker run` options the engine needs (for example `--cap-add=SYS_PTRACE`), the template layer of the deployment's own `docker_args`: the template's own args apply first, then the deployment's own, then the platform's (RDMA, memlock). If the deployment itself does not state `--ipc=host` but the selected template does, the deployment's own `shm_bytes` is rejected rather than silently having no effect, because it would never apply. See [the three-layer order of docker run arguments](deployment-launch-sources.md#docker-run-参数的三层顺序) |
| `dirs[]` | Optional, at most 8 entries; `name` is 1–32 lower-case letters, digits or `-` (not starting or ending with `-`), unique within the spec; `mount_path` is an absolute, clean container path other than `/` that overlaps no other container path of the spec ([one container path space](#模板与平台的分工)); `limit_bytes` is a byte count, > 0 and ≤ 64 TiB (`1<<46`); `retain` may only be `deployment`, written back on import when omitted | Writable host directories the platform keeps for the deployment on every node it runs on; see [dirs: Managed Writable Directories](#dirs-managed-writable-directories) |
| `gpus` | Required | See below |
| `args` | String array | Included in the engine command line; supports `${VAR}` [variable expansion](#变量展开); split using the parameter dictionary at deployment time (see below) |
| `env` | Nonempty keys | Container environment variables; values support `${VAR}` expansion |
| `pd_mode` | Optional PD mode name (a name from `internal/server/pdmode`, such as `sglang-mooncake`); the mode's engine must equal `engine` | Once declared, the spec can only be deployed as PD disaggregation in that mode, and its `args` and `env` are what the model needs under PD. For the modes see [PD Disaggregation Deployment](../how-to/pd-disaggregation.md) |
| `pd_roles` | Optional layout by role; requires `pd_mode`. Each role: `tp` (required, ≥ 1), `pp`, `nodes` (how many nodes one instance spans), `units` (initial instance count); the last three default to 1 | The PD deployment form is pre-filled with the roles and instances from it. See [PD Layout and Manual Command](#pd-版式与手工命令) |
| `manual_command` | Optional string, a shell script (not a JSON argv), ≤ 64 KiB | The manual command a deployment uses by default. A PD spec's script must reference `${GPUWORKS_PD_ROLE}`. See [PD Layout and Manual Command](#pd-版式与手工命令) |
| `pre_start_argv` / `pre_start_script` | **Mutually exclusive**; argv entries are nonempty; script ≤ 64 KiB | Runs **inside the container** (not on the host), on **every container start**, before the engine starts: the agent writes it into `/gpuworks/pre-start.sh` (`/bin/sh`, `set -eu`, ending with `exec` of the engine command) and makes that the entrypoint. It sees the container's full environment: the spec's and the deployment's `env` and the accelerator variables the agent injects; `${GPUWORKS_*}` variables are [expanded](#变量展开) by the server at deploy time. Any failing step (a nonzero exit, an unset variable) makes the container exit before the engine starts, and the instance fails as an exited container with the output as evidence; if the wrapper itself cannot be written, the instance fails with `PRE_START_PREPARE_FAILED` |
| `assets[]` | See below | Small files distributed with the package |
| `entrypoint` | Nonempty entries | See the dedicated section below |
| `placement` | See below | Topology derivation |
| `api` | See below | External HTTP contract |
| `components[]` | See below | Combinations of multiple sets of weights (speculative decoding, and so on) |
| `performance_tradeoffs[]` | See below; each entry's `param` must be a performance-costing setting the spec's launch actually sets | States why the spec keeps settings that cost performance (such as `--enforce-eager`); see [performance_tradeoffs](#performance_tradeoffs-reasons-for-performance-costing-settings) |

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
`NGRAM`/`STANDALONE` (its model hook compares against uppercase values before normalization) and form a closed list, while vLLM uses lowercase method names
that, like the tool call parsers, are only suggestions (including `dflash` and `dspark`): a method outside the list reaches the engine verbatim, and whether
the image's vLLM supports it is the deployer's to check. For KV precision, each engine's list
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

#### Rank-scoped weights

When a multi-node recipe pre-splits its weights into one part per rank, kept in subfolders of one repository, the spec's own `weights.sources` may name the rank variable `${GPUWORKS_NODE_RANK}` in `subfolder` (as in template fields, the older unprefixed `${NODE_RANK}` is its alias):

```yaml
weight_bytes: 137438953472          # one rank's size
weights:
  quantization: fp8
  sources:
  - modelscope:
      repo_id: org/model-tp4
      subfolder: rank${GPUWORKS_NODE_RANK}   # rank 0 reads rank0/, rank 3 reads rank3/
```

Each rank of a multi-node deployment then downloads and mounts only its own part (decision in [ADR 0076](../adr/0076-rank-scoped-weights.md)):

- At create, the subfolder is expanded for every rank into one model artifact per rank (same provider, repository and revision, own subfolder). Rank 0's artifact is the deployment's model; the other ranks' bindings are kept in the deployment snapshot and never resolved from the catalog again.
- Each node's download task, model marker and local model identity are its rank's subfolder. Admission, disk budgeting and preparation progress are per rank too: rank `r`'s node reserves disk only for rank `r`'s artifact, and a node that already holds that subfolder counts no download.
- The mount keeps the repository-relative paths: in rank `r`'s container the weights are under `${GPUWORKS_MODEL_PATH}/rank<r>/…`, so the command writes `${GPUWORKS_MODEL_PATH}/rank${GPUWORKS_NODE_RANK}/…`.
- `weight_bytes` (and `runtime_weight_bytes`) state one rank's size; the memory estimate multiplies it back to the group's weights.
- A subfolder may name only the rank variable; either every source of the spec names it or none does; it cannot be combined with `components` or `pd_mode`, nor deployed as PD (`RANK_WEIGHTS_PD_UNSUPPORTED`). A request that names `model_id` must name rank 0's artifact (`RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`). On a single-node deployment `${GPUWORKS_NODE_RANK}` is `0` (as the variable table states), so it reads rank 0's part only; how many parts exist is the recipe's to decide, and the platform does not check the deployment's node count against it.

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

### PD Layout and Manual Command

A PD recipe is often more than "one set of arguments for each of two roles": each role's TP and how many nodes an instance spans are part of the recipe, and for some recipes the two roles also take different engine arguments.
`pd_roles` and `manual_command` let a spec write these down, so the deployment form opens already filled in.

**`pd_roles`** describes only the starting point; a deployment can still change the instance count and pick nodes:

- One entry per role: `tp`, `pp` (pipeline parallel degree), `nodes` (how many nodes one instance occupies, leader plus followers) and `units` (initial instance count, ≤ 32).
- Validation: every role of the mode must be present and no role the mode lacks may be written; `tp` ≥ 1; `tp × pp` must be divisible by `nodes`; and the mode's TP and PP rules must hold.
  `sglang-mooncake` does not compare the two roles' TP (SGLang pairs by attention TP, which the platform cannot see, the engine handles unequal widths, and a pairing it cannot handle makes the decode instance fail at start),
  and only requires decode PP to equal prefill PP or be 1 (`PD_PP_DECODE_MISMATCH`).
- The inference template list API (`GET /api/v1/inference-templates`) returns `pd_roles` on the spec with defaults filled in: each role carries `tp`, `pp`, `nodes`, `gpus_per_node` and `units`.
- The deployment form switches to PD disaggregation and selects the spec's mode, then fills each role's TP, PP and instance rows from the layout. Automatic placement does not take instances that span nodes, so the rows use manual placement with empty follower slots, and you pick the nodes.

**`manual_command`** lets the spec own the whole launch command:

- When the request's `container_overrides` states neither `cmd_display` nor `command_mode`, the deployment's manual command is this script (`command_mode` is `manual`); if either is stated the request wins,
  so the deployer can still rewrite it, or state `command_mode: engine` to return to the platform-built command. The deployment keeps the copy it was created with; a later catalog change does not reach it.
- The evaluation API returns it as `manual_command_draft`, and the command editor starts from it.
- Import-time validation: it is a shell script, not a JSON argv starting with `[`, ≤ 64 KiB; a PD spec's script must reference `${GPUWORKS_PD_ROLE}`, and if any role spans more than one node it must also reference
  `${GPUWORKS_NODE_RANK}` or `${GPUWORKS_HEADLESS}`. Creating a deployment checks once more against the layout of that request (a request may span more nodes than the spec's layout),
  and returns `MANUAL_COMMAND_RANK_BLIND` when it is not met.
- Variable expansion and the system environment layer follow the same rules as a hand-written manual command; see [Manual Commands for Multi-Node and PD Deployments](../explanation/launch-config.md#多机与-pd-的手工命令):
  only names with the `GPUWORKS_` prefix that this launch states are expanded; ranks still carry the system environment layer (an instance spanning several nodes also gets `NCCL_*`, `GLOO_SOCKET_IFNAME`, and so on, which a single-node instance does not),
  and the script can read them as shell variables, for example `"$NCCL_IB_HCA"`. Names this launch does not state (`${GPUWORKS_PD_SIDE_CHANNEL_PORT}` on a decode instance,
  `${GPUWORKS_MASTER_ADDR}` on a single-node role) are left as written for the shell, so write them only in the branch that can use them.

A sketch follows (the other required fields such as `image`, `weights` and `gpus` are omitted): both roles span 2 nodes, with different TP and different engine arguments.

```yaml
- name: pd-mooncake
  engine: sglang
  pd_mode: sglang-mooncake
  pd_roles:
    prefill: {tp: 8, nodes: 2, units: 1}
    decode:  {tp: 16, nodes: 2, units: 2}
  manual_command: |
    set -eu
    : "${NCCL_IB_HCA:?needs a multi-node instance on a host RDMA network}"   # a command substitution swallows set -u, so check first
    IB_DEVICES=$(printf '%s\n' "$NCCL_IB_HCA" | tr ',' '\n' | sed 's/:.*//' | awk 'NF && !seen[$0]++' | paste -sd, -)
    case "${GPUWORKS_PD_ROLE}" in
      prefill)
        exec python3 -m sglang.launch_server --model-path ${GPUWORKS_MODEL_PATH} \
          --served-model-name ${GPUWORKS_SERVED_MODEL_NAME} \
          --host 0.0.0.0 --port ${GPUWORKS_API_PORT} --tp-size 8 \
          --nnodes ${GPUWORKS_NNODES} --node-rank ${GPUWORKS_NODE_RANK} \
          --dist-init-addr ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} \
          --disaggregation-mode prefill --disaggregation-transfer-backend mooncake \
          --disaggregation-bootstrap-port ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} \
          --disaggregation-ib-device "$IB_DEVICES"
        ;;
      decode)
        exec python3 -m sglang.launch_server --model-path ${GPUWORKS_MODEL_PATH} \
          --served-model-name ${GPUWORKS_SERVED_MODEL_NAME} \
          --host 0.0.0.0 --port ${GPUWORKS_API_PORT} --tp-size 16 \
          --nnodes ${GPUWORKS_NNODES} --node-rank ${GPUWORKS_NODE_RANK} \
          --dist-init-addr ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} \
          --disaggregation-mode decode --disaggregation-transfer-backend mooncake \
          --disaggregation-ib-device "$IB_DEVICES"
        ;;
    esac
```

**Writing the served name**: The served name is free text the user enters, and the platform rewrites the value of `--served-model-name` in a manual command, after expansion, into an escaped single-quoted string.
So put `${GPUWORKS_SERVED_MODEL_NAME}` directly on the command line, unquoted, as in the example. Do not put it together with `--served-model-name` inside one double-quoted string
(for example `bash -c "... --served-model-name ${GPUWORKS_SERVED_MODEL_NAME}"`): the single quotes the platform inserts stay as literal characters in the served name, or misalign the outer quotes.

## What the Template Declares and What the Platform Provides

A template declares only what it needs, and the platform provides it on every launch; the table below lists every in-container fact a template can declare or rely on. Facts outside the table (which card, which port, which network) are the platform's decision and a template cannot declare them.

| Kind | The template writes | The container sees | Host side decided by | Lifetime |
|---|---|---|---|---|
| Launch variables | References to `${GPUWORKS_*}` in `args` / `env` / `entrypoint` / `pre_start_*` (a multi-node rank's `${GPUWORKS_PEERS_FILE}` among them), see [Variable Expansion](#变量展开); a weight `subfolder` may reference only `${GPUWORKS_NODE_RANK}` | This launch's values | The platform | Every launch |
| Metrics port | `api.metrics.port: separate`, and a reference to `${GPUWORKS_METRICS_PORT}` in the launch fields, see [api.metrics](#apimetrics引擎指标映射) | The local port the launcher listens on (`${GPUWORKS_METRICS_PORT}`); the template does not choose the number | The platform (reserved together with the API port) | Fixed when the instance is created, kept with the instance |
| System-layer environment variables | Nothing; a key of the same name in `env` overrides it | The compute network's `NCCL_*` / `GLOO_SOCKET_IFNAME` / `UCX_NET_DEVICES`, see [Environment Variable Layers](deployment-launch-sources.md#环境变量分层); a multi-node rank with a peers file also gets `GPUWORKS_PEERS_FILE` (the same value as the launch variable, for programs the recipe starts to read directly) | The platform | Every launch |
| Primary weights | `weights`; `subfolder` may say `rank${GPUWORKS_NODE_RANK}`, see [Rank-Scoped Weights](#按-rank-拆分的权重) | Read-only at `${GPUWORKS_MODEL_PATH}` (default `/model`, or the primary component's `mount_path`); with rank-scoped weights each rank holds only its own subfolder | The platform (model storage) | With the model |
| Component weights | `components[].mount_path` | Read-only at that path | The platform (model storage) | With the model |
| Assets | `assets[].unpack_to` | Read-only, unpacked to that path | The platform (template package) | With the template version |
| Writable directories | `dirs[]` (`name`, `mount_path`, `limit_bytes`, `retain`); see [dirs](#dirs托管的可写目录) | Read-write at `mount_path`; one per node | The platform (node-managed directory; needs the agent capability `mount.managed_dirs`) | With the deployment; never pruned |
| KV disk tier | Cannot (a deployment parameter) | Read-write, at a path the platform picks | The platform (node-managed directory) | While an instance runs on the node; pruned to its limit |
| Deployment mounts | Cannot (the deployment's `extra_mounts`) | As each row states | The operator | Not managed by the platform |
| Compute network files | Nothing | Read-only host files the accelerator network needs (same path) | The platform | Every launch |
| Platform files | Cannot; `/gpuworks` belongs to the platform | `/gpuworks/pre-start.sh` (with `pre_start_*`); `/gpuworks/peers.json` (a multi-node rank, read-only, `proto.PeersFile` v1, see [Multi-Node Peers File](deployment-launch-sources.md#多机-peers-文件gpuworks_peers_file)) | The platform (the peers file needs the agent capability `launch.peers_file`) | Rewritten on every container start |
| Startup wait limit | `start_timeout_sec` | How long the engine health check is waited for | The platform; see [Startup Wait Limit](deployment-launch-sources.md#启动等待上限) | Fixed when the instance is created |

Rules:

- **One variable spelling**: documentation and examples always write the prefixed `${GPUWORKS_X}`; the older unprefixed spelling still expands as an alias in template fields, while a hand-written manual command recognizes only the prefixed one.
- **One set of units**: sizes in a spec are always bytes (`*_bytes`), durations always seconds (`*_sec`), and container paths always absolute, clean paths (`mount_path`, `unpack_to`).
- **One container path space**: no two mounts in the table (primary weights, components, assets, dirs, KV disk tier, deployment mounts, compute network files) and the platform directory `/gpuworks` may target the same path or nest inside each other, by one rule (`proto.ContainerPathsOverlap`). The template's own paths (the model mount, component `mount_path`s, asset `unpack_to`s, dir `mount_path`s) are checked against each other on import and a conflict is `CATALOG_MOUNT_PATH_CONFLICT` (its parameters name both fields and paths; several assets may unpack into one directory); together with the deployment's own mounts and the platform's they are checked again on evaluation, creation and every launch, where one target is `MOUNT_TARGET_DUPLICATE`, nesting is `MOUNT_TARGET_NESTED` and a mount on `/gpuworks` is `MOUNT_TARGET_RESERVED` (a deployment restarted as stored keeps a mount it was created with, unless it would cover this launch's peers file).
- **Agent capability**: each facility only the node's agent can provide has one capability name; a launch that uses it goes only to nodes that announce the capability, and an older agent's node is refused with the scheduling reason `agent_capability_missing` (parameters `capability`, `node`), so the deployment waits for an upgrade or another node instead of starting a container that lacks what the template declared. Declaring `dirs` needs `mount.managed_dirs`; referencing `${GPUWORKS_PEERS_FILE}` in a launch field needs `launch.peers_file`, while a multi-node launch that does not reference it starts on an older agent as before, only without the peers file and its environment variable.

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
- If a vendor-built image has a different entrypoint, declare it for each spec.
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

Variable references in `args` / `env` / `entrypoint` / `pre_start_*` are expanded on every deployment, regardless of whether placement
is present. The documented spelling carries the `GPUWORKS_` prefix, such as `${GPUWORKS_NODE_RANK}`; the older spelling without the prefix (`${NODE_RANK}`)
remains an alias in these template fields and expands the same way. Available variables = resolver return values + platform facts for this launch (the sole sources are
the vocabulary in `internal/server/placement/expand.go` and `internal/server/deploy/launch_variables.go`). The table lists the prefixed spelling (the environment variables and container files the platform provides are not launch variables; see [What the Template Declares and What the Platform Provides](#模板与平台的分工)):

| Variable | Value | When Available |
|---|---|---|
| Any variable returned by the resolver | Placement result (a resolver returns `SP_DEGREE`; reference it as `${GPUWORKS_SP_DEGREE}`) | When a resolver exists |
| `GPUWORKS_GPU_COUNT` | Number of GPUs allocated on this node (overrides a resolver-returned value with the same name) | Placement is determined |
| `GPUWORKS_TP_SIZE` | Tensor parallelism for the entire deployment; a resolver-returned value takes precedence | Placement is determined |
| `GPUWORKS_PP_SIZE` | **Intentionally has no default**: multi-node pipeline parallelism already has a single owner (the engine injects it based on node count); it can only be returned by the resolver | Only when returned by the resolver |
| `GPUWORKS_MODEL_PATH` | Mount path of the primary weights in the container (including a primary component or deployment override) | Always |
| `GPUWORKS_SERVED_MODEL_NAME` | The deployment's served name, which the gateway and the health check send to the engine as `model` | Always |
| `GPUWORKS_API_HOST` | Address the service binds to: `0.0.0.0` for callable, `127.0.0.1` for other engines | Always |
| `GPUWORKS_API_PORT` | Port the platform reserves for this instance, accessed by the gateway and health checks; `0` for a headless rank | Always |
| `GPUWORKS_METRICS_PORT` | The extra metrics port the platform reserves for this instance: the launcher exports engine metrics there and the agent scrapes it (see [api.metrics](#apimetrics引擎指标映射)); `0` for a headless rank | Only when the template declares `api.metrics.port: separate` |
| `GPUWORKS_NNODES` / `GPUWORKS_NODE_RANK` | Number of nodes and index of this rank; `1` / `0` for a single node | Always |
| `GPUWORKS_HEADLESS` | `1` if this rank does not serve external requests; otherwise `0` | Always |
| `GPUWORKS_NODE_ADDR` | Routable address of the node this rank runs on (not rank 0's) | Multi-node only |
| `GPUWORKS_MASTER_ADDR` / `GPUWORKS_MASTER_PORT` | Address and rendezvous port of rank 0 in a multi-node deployment | Multi-node only |
| `GPUWORKS_PD_ROLE` | This instance's PD role: `prefill` or `decode` | PD instances |
| `GPUWORKS_PD_SIDE_CHANNEL_HOST` | Address this instance's KV side channel listens on | PD instances whose address is known |
| `GPUWORKS_PD_SIDE_CHANNEL_PORT` | First port of this rank's extra port block, the KV side channel port; each rank of a multi-node instance has its own | PD instances whose port is allocated |
| `GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT` | The side channel port of this instance's leader: its own on the leader, the leader's on a follower. Use it where a follower meets the leader's side channel (such as SGLang's bootstrap server); commands the platform composes do not use it | PD instances whose port is allocated |
| `GPUWORKS_PEERS_FILE` | The path of this rank's peers file, `/gpuworks/peers.json`, listing every rank of the group and its rails; see [Multi-Node Peers File](deployment-launch-sources.md#多机-peers-文件gpuworks_peers_file) | Multi-node only; a launch that references it goes only to nodes that announce `launch.peers_file` |

Rules:

- **The resolver cannot return platform facts**: returning `MODEL_PATH`, `SERVED_MODEL_NAME`, `API_*`, `METRICS_PORT`, `NNODES`, `NODE_RANK`, `HEADLESS`, `NODE_ADDR`, `MASTER_*`, `PEERS_FILE`, or `PD_*`
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
- Arguments entered by users in the deployment form and command overrides in `engine` / `image-entry` mode are **never expanded**. A handwritten command in
  `manual` mode expands prefixed names only; an unprefixed `${MODEL_PATH}` and a prefixed name this launch does not state are left unchanged for the shell. See
  [Manual Commands for Multi-Node and PD Deployments](../explanation/launch-config.md#多机与-pd-的手工命令).

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
`openai.realtime`, and `systemone.decide` (`/v1/systemone`, a System One decision: a shared `state` and typed questions in, a probability distribution per question out, nothing generated; the wire format is TypeSafe's System One API). Operation types must not repeat within a contract.

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
    port: api                     # Optional: api (default, the endpoint is on the API port) or separate (the platform gives it a port of its own)
    format: prometheus            # Optional: prometheus (default, Prometheus text) or json
    inherit: sglang               # Optional: vllm or sglang; apply that engine's built-in mapping first
    map:                          # Engine metric family (optional {k="v"} selector) -> vocabulary short name
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}   # Long form: scaling
```

- **Effective mapping** = inherited mappings (from the engine specified by `inherit`, or the built-in mappings for a `vllm` / `sglang` template's own engine) overlaid entry by entry with `map`; `map` takes precedence for the same key.
- **`format: json`**: the endpoint returns a JSON document (such as Strata's `/metrics`). Write `map` keys as dot-separated key paths (`totals.output_tokens`) to extract the number at the path (booleans become 1/0; missing or nonnumeric values produce no metric). For strings, write a comparison as `path==value` / `path!=value` (such as `live.state!=idle`), which yields 1 when true and 0 otherwise and can only map to a gauge. Only counters and gauges can be mapped; `inherit` is not allowed, and no built-in engine mapping is applied. Documents larger than 1 MiB or invalid JSON are not read at all.
- **`port: separate`**: for an engine that owns its API port completely and serves no `/metrics` there (such as a Rust front end with only `/health`, `/v1/models` and `/v1/chat/completions`), a launcher in the image exports the metrics on a second local port, without proxying requests. Once declared, the platform **reserves one more port per instance** (allocated together with the API port, usually right after it) and states it in the launch variable `${GPUWORKS_METRICS_PORT}` (older spelling `${METRICS_PORT}`), which the template uses to tell the launcher where to listen. The agent then scrapes `http://127.0.0.1:<that port><endpoint>` instead of the API port. With `api` or nothing written, behavior is as before: no port is reserved and the variable does not exist.
  - **The port is a fact of the launch**: the reserved port is stored with the instance's desired state (in the `ports` list of the instance detail, after the API port) and is kept across agent restarts and container restarts. It comes from the template snapshot taken when the deployment was created, so changing `port` in the template later does not affect existing instances; changing it means a new generation of instances. `endpoint`, `format` and `map` still use the current declaration, as in [takes effect at runtime](#apimetrics引擎指标映射).
  - **Which ranks have it**: only ranks that serve (a single-node instance, rank 0 / the leader of a multi-node instance, every PD leader). A headless rank has none, like its API port, and `${GPUWORKS_METRICS_PORT}` is `0` there, so one command can expand on every rank. Metrics are scraped only from the rank that has the port.
  - **Preview**: the command in the deployment evaluation uses a placeholder port (as for the API port) and creation replaces it with the port actually reserved.
  - **The two must come as a pair** (checked at import, catalog package upload and editor save): declaring `separate` without referencing `${GPUWORKS_METRICS_PORT}` in `args` / `env` / `entrypoint` / `pre_start_*` / `manual_command` is `CATALOG_METRICS_PORT_UNREFERENCED` (nothing listens on the scraped port); referencing it without declaring is `CATALOG_METRICS_PORT_UNDECLARED` (the platform reserves no such port; a resolver cannot return a variable of that name either). Any other value of `port`, including a concrete port number (the platform allocates the port; a template cannot choose it), is `CATALOG_METRICS_PORT_INVALID`.
  - An older agent does not know the port and still scrapes the API port, so it simply gets no engine metrics; the deployment is unaffected.
- **When omitted**: `vllm` / `sglang` use built-in mappings. `callable` scrapes `/metrics` and automatically applies the corresponding mapping based on the `vllm:` / `sglang:` prefixes found; if neither is present, no engine metrics are produced.
- **Validation** (import, catalog package upload, editor save): targets must be vocabulary short names; histograms must map by family name, without selectors or scaling; the `_bucket` / `_sum` / `_count` lines of an already mapped histogram family cannot be mapped again; scale factors must be nonnegative; `inherit` must be `vllm` / `sglang`; `format` must be `prometheus` / `json`; `port` must be `api` / `separate`. With `json`, keys must be key paths (optionally with `==value` / `!=value` comparisons), targets cannot be histograms, comparisons cannot map to counters, and `inherit` is not allowed. Error codes are `CATALOG_METRICS_*`.
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

## dirs: Managed Writable Directories

When an engine needs a host directory it can write, that survives its instances being recreated, and that is cleaned up when the deployment is deleted (a session store, a spill area, logs, or a compile cache or snapshot it derives at first start and reuses), the spec declares `dirs`:

```yaml
dirs:
- name: pico-state
  mount_path: /pico
  limit_bytes: 429496729600
  retain: deployment
```

How it divides the work with weights, assets, the KV disk tier and deployment mounts is in [What the Template Declares and What the Platform Provides](#模板与平台的分工).

- **One per node**: on every node an instance of the deployment lands on, the agent creates `managed-dir/<deployment>-<creation second>/<name>` under the node's runtime mount root (`GPUWORKS_RUNTIME_MOUNT_ROOT`, by default `<state>/runtime-mounts`, `<state>` on docker nodes), owned by the runtime mount owner, and mounts it read-write at `mount_path`. docker, nerdctl, runveil (udocker), Kubernetes and host processes (paths rewritten to the host directory) treat it as any other mount. Instances of one deployment on one node share the directory; a recipe that needs them apart splits it into subdirectories itself with the existing `${GPUWORKS_NODE_RANK}`. There is no launch variable for the path: the recipe writes `mount_path` directly.
- **One mechanism with the KV disk tier**: the KV disk tier is the same kind of managed directory, switched on by a deployment parameter and kept under `kv-disk/<deployment>`; it is a cache, removed once the last instance leaves the node and pruned past its ceiling by the sweeper. `dirs` is state, kept until the deployment is deleted and never pruned.
- **Kept until the deployment is deleted**: stopping, restarting, failing over or re-forming a multi-node group leaves it alone, and an instance that returns to the same node sees what was written there. Once the deployment is deleted (its instances confirmed torn down, its row gone) the server tells every connected node to remove the deployment's directories; a node offline at the time lists the groups it holds in its next hello, the server's replay names those whose deployment no longer exists, and the node removes them then. A group a desired instance still mounts is kept. The group name carries the deployment's creation time, so after a database change (a reinstalled server, a restored backup) a new deployment with the same id does not get the old directory.
- **Admitted on the declared size**: scheduling requires the disk the node keeps managed directories on (the agent reports its free space as `managed_dirs`) to hold every `limit_bytes`, the KV disk tier's ceiling and the disk reserve (the storage settings' reserve, else a tenth of that disk, at most 50 GiB); when the agent reports that disk is shared with the models root or the current runtime's image store, the weights to download and the image to unpack count there too. Otherwise the node is refused with `insufficient_managed_dir_disk`, and the reason states how much is needed, available, declared and reserved. A node that does not report the disk (an older agent) is not checked here. Admission does not subtract what a directory already holds, because that is already out of the free space.
- **Not pruned or capped**: the directory holds state, not a cache, so the platform deletes nothing in it by size and sets no filesystem quota; staying within `limit_bytes` is the recipe's part.
- **Path conflicts**: `mount_path` shares one container path space with every other mount; the rule is in [What the Template Declares and What the Platform Provides](#模板与平台的分工).
- **Agent capability**: the agent announces `mount.managed_dirs` in its hello; a launch that declares `dirs` goes only to nodes that announce it, and an older agent's node is refused with `agent_capability_missing`. When a directory cannot be prepared the instance fails to start with `MANAGED_DIR_PREPARE_FAILED`.

See [ADR 0077](../adr/0077-template-managed-writable-dirs.md) for the trade-offs.

## performance_tradeoffs: Reasons for Performance-Costing Settings

Some settings let an engine run on a given piece of hardware or a given model at the price of throughput or latency, such as turning off graph capture, turning off prefix caching, or capping concurrency at a single digit. `internal/engines` holds a registry with each engine's spellings (`engines.ScanPerformanceCosts`) that recognizes these settings in a spec's launch and grades them:

| Cost | vLLM spelling | SGLang spelling | Level |
|---|---|---|---|
| `graph_disabled` | `--enforce-eager`, `cudagraph_mode: NONE` in `--compilation-config`, `-O0` or `mode: 0` (both only without `cudagraph_mode`), `enforce_eager: true` in `--speculative-config` | `--disable-cuda-graph` | Must be explained |
| `prefix_cache_disabled` | `--no-enable-prefix-caching` | `--disable-radix-cache` | Must be explained |
| `concurrency_capped` | `--max-num-seqs` | `--max-running-requests` | ≤ 4 must be explained; 5–8 shown only |
| `chunked_prefill_disabled` | `--no-enable-chunked-prefill` | `--chunked-prefill-size -1` | Must be explained |
| `overlap_schedule_disabled` | — | `--disable-overlap-schedule` | Must be explained |
| `prefill_batch_small` | `--max-num-batched-tokens` < 2048 | `--chunked-prefill-size` < 2048 | Shown only |
| `kv_memory_reduced` | `--gpu-memory-utilization` < 0.8 | `--mem-fraction-static` < 0.7 | Shown only |
| `graph_capture_narrow` | `cudagraph_mode: FULL_DECODE_ONLY`, largest `cudagraph_capture_sizes` entry < 16 | `--cuda-graph-max-bs` < 16 | Shown only |
| `kernel_fallback` | `--attention-backend TRITON_ATTN`, environment switches that turn a fast kernel off such as `VLLM_USE_DEEP_GEMM=0` | — | Shown only |

The registry reads `args`, `env`, and the flags in `entrypoint`, `pre_start_argv` and `manual_command` split by shell quoting rules: a quoted JSON value with spaces is one value, and the command inside `sh -c '…'` is read word by word. Each input is read on its own, so a `--compilation-config` written across two inputs is not merged. vLLM's underscore spelling (`--max_num_seqs`) counts as the dashed one, and the reported `param` is the dashed form. `backend: eager` in `--compilation-config` is only the torch.compile backend; it does not stop graph replay and is not `graph_disabled`.

A spec states its reasons in `performance_tradeoffs`, one entry per setting:

```yaml
performance_tradeoffs:
- param: --disable-cuda-graph
  reason: hardware_plugin_requires
  detail_en: "NPU graph capture hangs on this runner; the eager path completes chat."
  detail_zh: "该镜像上 NPU 图捕获会挂起；eager 路径能完成对话。"
- param: --speculative-config.enforce_eager
  reason: upstream_bug
  detail_en: "The drafter's graph capture crashes in this engine version."
  detail_zh: "该引擎版本中草稿模型的图捕获会崩溃。"
  link: https://example.com/upstream-issue
```

| Field | Constraints |
|---|---|
| `param` | Required, unique within the spec; the name the registry reports: a switch as the flag itself (`--enforce-eager`), a key inside a JSON value as `<flag>.<key>` (`--compilation-config.cudagraph_mode`, `--speculative-config.enforce_eager`), an environment variable as `env:<name>`, and `-O0` as `-O`. Import fails when the launch no longer sets the setting: a reason must not outlive its setting |
| `reason` | Required; values in the table below |
| `detail_en` / `detail_zh` | Both required; say at which step and how the faster path fails. This is template content written by the author, like display names |
| `link` | Required when `reason: upstream_bug`, optional otherwise; must be an http(s) URL |

| reason | Meaning |
|---|---|
| `hardware_plugin_requires` | The vendor plugin, runtime or driver does not support the faster path (graph capture, prefix caching, a kernel) |
| `model_architecture_requires` | This engine version does not support the faster path for the model's structure |
| `accuracy` | The faster path produces wrong or degraded output |
| `memory_fit` | Memory or concurrency is given up so the model fits the card or slice |
| `task_type` | A task without a decode phase (embeddings, rerank) |
| `upstream_bug` | A known upstream defect; `link` is required |

A spec whose `api.operations` are all `openai.embeddings` / `openai.rerank` gets `task_type` for `graph_disabled`, `prefix_cache_disabled` and `concurrency_capped` without writing it. Import, saving in the template editor and catalog package loading all validate the field. It is not stored in the database projection, and the platform reads these reasons only from the built-in catalog, so a template exported from the database or saved again from the editor does not carry it; importing a spec that sets the field raises the `CATALOG_PERFORMANCE_TRADEOFFS_NOT_KEPT` warning.

`TestBuiltinPerformanceCostsAreExplained` in `internal/server/inferencecatalog/seed` guards the built-in catalog: every must-be-explained setting in a built-in spec needs a `performance_tradeoffs` entry, the automatic exemption above, or a line in `testdata/perf_tradeoff_budget.txt`; otherwise the test fails and its message gives the YAML to add. The budget file only shrinks: once a setting gains a reason or is removed, the same change must delete its line, or the test fails as well. The line count is pinned to the test constant `perfTradeoffBudgetLines`, and the test fails when the file disagrees: deleting a line means lowering it; a new performance-costing setting should get a `performance_tradeoffs` entry, and a line that really must be added means raising the constant and writing `# raised: <reason>` above the line, so review sees it in the change.

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
| `CATALOG_PERFORMANCE_TRADEOFFS_NOT_KEPT` | The spec sets `performance_tradeoffs` | Validated but not kept: the platform reads these reasons only from the built-in catalog (see [performance_tradeoffs](#performance_tradeoffs-reasons-for-performance-costing-settings)) |
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

<!-- translation-source-sha256: 987d33a952651d5b137a2c3a48aacfbf40b3efecfc00a4738e536304577ec69c # Inference Catalog model.yaml Syntax Reference -->
<!-- translation-source-sha256: 11f2dc6a5c907ebac9e360954770fd0c505b7ac3cf3ff9722b83b7d87dbf411a ## Overview -->
<!-- translation-source-sha256: d1cab9aa487c9a1159b67fd447e19bf5893c615e86468538eb50068dd1c9d3b7 ## Model-Level Fields -->
<!-- translation-source-sha256: e8af1451051c90ef785a9c06e676817f0f56b5a02da71615ee57d1ac83735f95 ## Spec-Level Fields -->
<!-- translation-source-sha256: 5514abf483ab7da4e47e650340bc5fa104c07f131905307d15db4b9eceb665bc ### args: Decode Known Parameters Into Fields and Pass Unknown Tokens Through Verbatim -->
<!-- translation-source-sha256: 8e5e8b20bd4d4c8cc7a64d9993eaebfb6a9139b8d8f7471a62c84c07e140bfec ### weights.sources: Compact Single-Key Form -->
<!-- translation-source-sha256: bee9f386d67fd025872be87fc0d6417ca7ea768faa95f1e839f32af63e0822f4 ### image_sources: Image Sources Declared by the Template -->
<!-- translation-source-sha256: ad7eaab32f35f7cd46870557a94a23d13cc8e4863699c8207732d7291eac22d4 ### GPU Selectors (gpus / unsupported[].gpus) -->
<!-- translation-source-sha256: 10f8e878fe47c8726cfd234b9ccbd6beffa018e6242632cb689e45ea41bff5fe ### Display Names -->
<!-- translation-source-sha256: e3430d7e59546f9f0e233c4f700ce07ec690a95a04bb7273d626354f5d919d1f ### assets[] -->
<!-- translation-source-sha256: 83b305081ab8f015fcc842b0f7d0a0d03a0c7e75846702aff54692db07c1894d ### PD Layout and Manual Command -->
<!-- translation-source-sha256: 976fe75b4bf21fd820f6239ae639396f075b0966d7b988fe601b905a08885f96 ## What the Template Declares and What the Platform Provides -->
<!-- translation-source-sha256: ca8ad9d228ec55ff4037230581f5a1230b2b5fed81b4a0103dfa73df72cd835d ## entrypoint: The Sole Declaration Point for Entrypoint Overrides -->
<!-- translation-source-sha256: a755bee59689f833778747841eb293a47a2b0d0dddafec599365a40a05ee5286 ## placement: Starlark Topology Derivation -->
<!-- translation-source-sha256: 8730a225b2be22fb1a6ce28a3795c8c6ef8697f0af90adfad1c78236c1eb3dd1 ### Variable Expansion -->
<!-- translation-source-sha256: 326983de77aaf4bb52b0516a7cb8fe35122d37bffd501bf241bd514297f95ad7 ### How Thresholds Relate to Scheduling Admission -->
<!-- translation-source-sha256: b01668b34d029e72fa32f03c01631d123be2ab05c7fb5ad8edfcad66faef7c82 ## api: External HTTP Contract -->
<!-- translation-source-sha256: 4070ee0770711185358f42c24fdc671a935929a1849ef47543173cae553e89a3 ### api.tokenizer_ref: Independent Tokenizer Source -->
<!-- translation-source-sha256: 7a250e5a02165b91282a0bc517187d409ca0a3991ff66ccde34262b5bda86067 ### api.metrics: Engine Metric Mapping -->
<!-- translation-source-sha256: c43a7f1c915e00849d832b88aa1423056a03dc492634a4d24793ca3c94807e8d ### api.request_stats: Per-Request Statistics in Responses -->
<!-- translation-source-sha256: f2dcc459ab368caf6601fff2c116fb06cdf6f4c7e4b03395844423906e1a87c6 ## components: Multiple Weight Sets -->
<!-- translation-source-sha256: 5ffa7b32ad221104b1942267515674a5d244169f2e115e30796d882f6ccf52d0 ## dirs: Managed Writable Directories -->
<!-- translation-source-sha256: d8159910bf55c76c4d66346dec46c313fadab1bd4cc0c20d7d069416a37908d4 ## performance_tradeoffs: Reasons for Performance-Costing Settings -->
<!-- translation-source-sha256: 2ebe9bf5d43fdc1e745261f42679fd5503d1e94c1c37aaa7ffffdc4a46edbf40 ## Pre-Import Checks and Warnings -->
<!-- translation-source-sha256: 439b1bcae33f36e507bd1217f4552947335ee6d22d0072b928a34e3ba1fbd9ac ## Validation Entry Points and Tools -->
<!-- translation-localized-code-block: a580200df2b28bc011bb736a64288086932955375e5ae8897762ee97bd667e26 8e0792250b6c0eb3ec0dad5bfca28308dc0b1afeb27e9c33bb24ddb21e20ba07 -->
<!-- translation-localized-code-block: 9a54a7271e193f98f78581cbb5fb446f8d5d1aa46c942d8f4fb3536f5f81b115 22ec1397368cfc809b65ad278feb838b5690c611dd587bc022c6e151b5596d99 Translate YAML comments and illustrative user-facing reason text. -->
<!-- translation-localized-code-block: 4cf9267c119973bda779b8e29a9ad818c097c803df34a09ac14492502ab9dce2 e78acb98a7a082fdad6d2080ea7754cc18152f1bec68f60c312e46e33f3bfd59 -->
<!-- translation-localized-code-block: 06db934de28e923689ee3d7a5291cfb9c5c39413ce9a4c3a87dfc3b0d6dc051d 213afdd99d9dc95fee88560f64a09e012578484b424f46c160ffd92f44b8bd72 Translate YAML comments and illustrative user-facing reason text. -->
<!-- translation-localized-code-block: 0db7a21f5d58dc3f0536ef0b3909ce67ba6415f6c0e158dbee9122f79b1f2209 8da5a4455c5d0ab53369e8e0b4455543b9acd70a2615f7faf4b34b620d9cd204 Translate the inline shell comment. -->
<!-- translation-localized-code-block: a611f2e8254725bdcf88ae19eb23c532ef8c679903131a8881bbd61f09aafdb2 33859cded67f664966175739bc36d429175e7fe4c8f3d71422f40719d24be6e6 Translate YAML comments and illustrative user-facing reason text. -->
<!-- translation-localized-code-block: bee5e6aa5c268b0bca3195996e1394ed7ca27c6938ea75731026b323c425c162 50a7b72308d2093f9b328d73f1c889122beed6408c02fe79a803f0bd63b3246f Translate YAML comments and illustrative user-facing reason text. -->
<!-- translation-localized-code-block: ed93aa7d8dc7ab4de4eb6a57ba14a8d08385ab5c6f68ce1d01d09b1520808e6c 6eea2db5a786a386d47baff90f8f621ce18884b6a8f127c011e81e77c4de00ed -->
<!-- translation-localized-code-block: a7d524edd753182838ebcc0b5d00da0acb23a4e6ceb9481fd31f031c520b6138 7454b07423d39cda5d3f4e571f923044557c02bd167ecf53284bb63e4e601388 Translate YAML comments and illustrative user-facing reason text. -->
<!-- translation-localized-code-block: cb01e807364a6fa35f11fd6e177ffbbcbfe444a9860c0043a088d4693962c102 7c96acf3d55e07b7b1526005845a7f4eaae06818f9de255105ab3e2d2216985f Translate YAML comments and illustrative user-facing reason text. -->
