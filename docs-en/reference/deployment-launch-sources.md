# Deployment Launch Parameter Sources

This document explains where the inference container's final entrypoint, argv, environment variables, mounts, docker arguments, `/dev/shm`, ports, network, and devices
come from, which layer overrides which, and when each takes effect. The entrypoint is the launch entry point; argv is the list of arguments passed to it.
Terminology follows [`CONTEXT.md`](../../CONTEXT.md)
([Resolved Launch], [Container Overrides], [Command Mode], [Instance Desired State]).

## Single Source of Truth and Two Boundaries

The server has a single merge point: `deploy.ResolveLaunch` (`internal/server/deploy/assemble.go`). Both the deployment page preview and the
controller call it through `planning.PlanLaunch`. The result is a `proto.ResolvedLaunch`, which is serialized into the instance desired state
(the `launch` field in `Instance.DesiredStateJSON`) and sent to the agent. `TestPlanLaunchAgreesBetweenTheControllerAndThePreview`
ensures that the preview equals what is sent.

**These are the only differences between the preview and actual execution**; all other fields are identical character for character:

| Difference | Preview | Actual Execution |
|---|---|---|
| API port | Fixed at `30000` | Port allocated by the scheduler |
| Unresolved placement variables | `$TP_SIZE`, `$GPU_COUNT`, and resolver variables remain symbolic | Expanded based on the allocated GPUs |
| Model component mount source | `$MODEL_COMPONENT_<NAME>_PATH` | Actual directory on the node |
| Agent-side injection (see "Agent Injection") | Not shown | Devices, dGPU, and preparation script wrapper |
| Image-built-in ENV/ENTRYPOINT/CMD | Not shown | Inherited normally by the container runtime; inspect them on a node from the image details, or view the observed values under the instance's "Launch facts" |

The image itself is only a runtime artifact: the image catalog provides no launch configuration that modifies a deployment (ADR 0022, ADR 0031).

## Layers at a Glance

The layers are listed in merge order. Later layers generally override earlier ones; entries marked "fill in missing values" or "platform-owned" follow their own rules:
"fill in missing values" takes effect only if the item is not yet set; override restrictions for "platform-owned" items are described in the corresponding row.

| # | Layer | Contribution | Relationship to Other Layers | Code |
|---|---|---|---|---|
| 1 | Engine baseline | Executable, model path, `--host`/`--port`, `--served-model-name`, TP/PP, baseline env, health/metrics paths, and `/dev/shm` baseline (callable supplies only the executable, baseline env, and paths, without appending arguments) | Lowest layer | `deploy/vllm.go`, `sglang.go`, `callable.go` |
| 2 | Parameter dictionary | Declared dictionary parameters (typed fields) → flags | Dictionary flags in template args and user args are first extracted into fields (ADR 0020); conflicts return `LAUNCH_CONFIG_CONFLICT` | `deploy/param_specs.go`, `materialize.go`, `args.go` |
| 3 | GPU/chip dialect | A few vLLM compatibility fixes | **Fills in missing values**: has no effect when fields, templates, or users have already set the value | `deploy/quirks.go` |
| 4 | Scheduler-derived values | Allocated GPU count → TP; placement variables | Estimator values **do not enter** argv | `planning/launch_plan.go` |
| 5 | Inference template spec | Non-dictionary args, env, entrypoint, preparation scripts, assets, components, API contract, `/dev/shm` minimum (`shm_bytes`), and unlimited locked memory (`memlock_unlimited`) | Overrides engine-injected values; user input can override this layer in turn | `inferencecatalog/flat.go`, `deploy/catalog_selection.go` |
| 6 | Topology | Multi-node and PD role arguments and env; RDMA docker arguments | Platform-owned; neither templates nor user input can override the PD connector | `deploy/pd.go`, `deploy/compute_network.go` |
| 7 | Engine finalization | For example, SGLang logging configuration | **Fills in missing values** | `deploy/sglang_logging.go` |
| 8 | User input | Dictionary fields, `extra_args` | Overrides the template | `deployment/launch_config.go` |
| 9 | Container overrides | env, env_unset, mounts, docker arguments, `/dev/shm` (when unspecified, derived from the engine baseline plus the CPU memory tier; see "`/dev/shm` Size"), model mount path, entrypoint, command, and command mode | Final deployment-level layer | `deploy/assemble.go`, `kv_cpu_tier.go`, `effective_command.go` |
| 10 | Agent injection | Model directory mount, preparation script wrapper, devices, and dGPU partitions | Device and dGPU env **override everything** | `agent/engine/engine.go`, `agent/gpu/*.go`, `agent/engine/partition*.go` |
| 11 | Runtime assembly | Fixed parts of `docker run`: name, `-d`, `--restart no`, network, and `--shm-size` | Platform-owned; specifying these in docker arguments is rejected | `agent/runtime/docker/docker.go`, `nerdctl/nerdctl.go` |

## Engine Baseline

| Engine | Command | Baseline Env |
|---|---|---|
| vLLM | `vllm serve <model-path>`, `--host 127.0.0.1`, `--port`, `--served-model-name` (omitted when headless); `--tensor-parallel-size` is always injected, and `--pipeline-parallel-size` is injected when greater than 1 | `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`, `VLLM_HOST_IP=127.0.0.1`, `GLOO_SOCKET_IFNAME=lo`, `NCCL_SOCKET_IFNAME=lo` |
| SGLang | `python -m sglang.launch_server --model-path … --tp-size --pp-size --host --port --served-model-name` (all in the entrypoint) | `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1` |
| callable | `gpuworks-callable` (usually replaced by the template entrypoint); argv contains only template and user arguments. The template places platform facts using variables such as `${MODEL_PATH}`, `${API_HOST}`, and `${API_PORT}` (ADR 0036) | Same as above, with template env merged in |

All three have a `/dev/shm` baseline of 16 GiB (`shm_bytes` in the engine Profile, delivered through `GET /api/v1/engines`).
This is only the baseline: the deployment's actual size adds the CPU memory tier on top of it, following the rules in "`/dev/shm` Size".
The default health check path is `/health`; the template API contract's `health_endpoint` can override it. The metrics path, prefix, and mappings come only from the engine Profile.

## Parameter Dictionary

The parameter dictionary (`param_specs` in `GET /api/v1/engines`) lists the engine parameters that the platform recognizes, describing each parameter's flag spelling and value format for each engine.

- **Only new requests write default values**. Existing saved deployments are not backfilled with defaults.
- Writing a default to the configuration **does not mean it appears in argv**:
  - Emits flags: vLLM `gpu_memory_utilization` 0.9 and `enable_cache_report`; SGLang `log_level`, `--mem-fraction-static 0.88`, and `--enable-cache-report`.
  - Depends on the image version: vLLM `enable_request_metrics` (`--enable-per-request-metrics`) receives a default only when the image catalog or image tag indicates vLLM 0.25.0 or later and neither the template nor `extra_args` contains `--disable-log-stats`. SGLang diffusion server receives a default for `enable_cache_report` starting with 0.5.18 and for `enable_metrics` starting with 0.5.21. For images with an unknown or older version, no default is written and no flag is emitted (the form shows "Auto", and you can still explicitly enable it), to prevent the engine from failing to start because it does not recognize a parameter. If the request does not specify an image, the version is determined from the image selected from the image catalog for the target node (or the target vendor if no node is selected).
  - Writes configuration only, without emitting flags: `dtype auto`, `kv_cache_dtype auto`, `performance_mode balanced`, and boolean switches set to false.
  - No default: `max_model_len`, tri-state switches (such as `enable_prefix_caching`), and other `automatic` rows.
- Platform parameters (such as `kv_disk_limit_gib`) never enter argv; they affect only platform behavior (ADR 0021).
- Parameters that the current entrypoint dialect or image version might not accept receive no default and are left to the engine to decide (`engine_decides_param_keys`). They are neither hidden nor rejected: the form shows "Auto", you can still enable or disable them, and the values shown in the form correspond one-to-one to argv.
- An image family's "common parameter/environment variable suggestions" are just buttons: clicking one writes it to your `extra_args` or environment variable rows; otherwise it has no effect.

Precedence: explicit fields > `extra_args` > template args. If the same parameter is given different values in two places, the request is rejected instead of silently choosing one.

## GPU/Chip Dialects

The **only** hardware rules at the argv level are three vLLM compatibility fixes. All fill in missing values: they have no effect when a field, template, or user has already supplied the same flag.

| Rule | Condition | Injection |
|---|---|---|
| `turing-no-bf16` | NVIDIA with compute capability < 8.0 (V100, T4, etc.) | `--dtype float16` |
| `deepseek-mla-fp8-kv` | DeepSeek MLA architecture, excluding Ascend | `--kv-cache-dtype fp8` |
| `minimax-block-size` | MiniMax architecture | `--block-size 128` |

These decisions use the vendor, architecture, and compute capability of the first allocated GPU. SGLang has no such rules; Ascend, Hygon, and PPU
have no hardcoded rules at the argv level. Vendor differences come from two places:

- **Spec selection**: inference templates select different specs for each type of GPU using GPU matching conditions (predicates) (`chipmatch`). Each spec carries its own args and env; matching is checked again on every reschedule.
- **Agent device injection**: see "Agent Injection".

The dtype compatibility matrix is used only for admission and warnings; it does not rewrite argv.

## Scheduler-Derived Values

- The allocated GPU set determines `--tensor-parallel-size`/`--tp-size`; the final argv must also pass topology validation.
- A spec's placement resolver can output variables; see [Variable Expansion](#变量展开).
- The model weight size determines the capacity limit (`size_hint`) for the KV disk tier mount.
- Values used by the estimator, such as `max_model_len` and GPU memory utilization, participate only in estimation and **are not written back to argv**.

## Inference Template Specs

For spec field syntax, see the [Inference Catalog model.yaml Syntax Reference](inference-catalog-spec.md). This section only describes where the fields end up:

| Spec Field | Destination |
|---|---|
| `args` | Parameters recognized by the dictionary are extracted into typed fields; the rest pass through unchanged as remaining template arguments, after engine-injected arguments. The platform does not inject flags already supplied by the template; user flags with the same name override the template |
| `env` | `env_layers.template` |
| `entrypoint` | Replaces only the executable portion, retaining the model and topology arguments generated by the engine |
| `pre_start_argv` / `pre_start_script` | Wrapped by the agent to run before the main command (see "Agent Injection") |
| `assets` | Mounted read-only by the agent at `unpack_to` |
| `components` | The primary component becomes the model mount path; the others are mounted read-only |
| `api` | API contract and health check path |
| `shm_bytes` | Minimum size for the container's `/dev/shm`; see "`/dev/shm` Size". Saved with the spec snapshot; later template changes do not affect existing deployments |
| `memlock_unlimited` | When `true`, the container receives `--ulimit memlock=-1:-1`; see "Locked Memory". Saved with the spec snapshot |
| `image` | Image used by the deployment; it need not be registered in the image catalog first |

Template specs always use the `engine` command mode; `image-entry` and `manual` can only be explicitly selected for the current deployment.

## Topology

- **Multi-node**: rank 0 is the leader; the others are headless followers. vLLM injects `--distributed-executor-backend mp --nnodes --node-rank --headless --master-addr --master-port` (default port 29500), and sets `VLLM_HOST_IP` to the node's parameter network address. SGLang injects `--nnodes --node-rank --dist-init-addr`.
- **Parameter network**: injects system-layer env such as `NCCL_SOCKET_IFNAME`, `GLOO_SOCKET_IFNAME`, `NCCL_IB_*`, and `UCX_NET_DEVICES`. When RDMA is used, it appends the docker arguments `--device /dev/infiniband --ulimit memlock=-1 --cap-add IPC_LOCK` (ADR 0027). If the deployment's own docker arguments already specify `memlock`, this item is not appended; see "Locked Memory".
- **PD disaggregation**: generates vLLM `--kv-transfer-config` or SGLang `--disaggregation-*` by role, along with NIXL/UCX-related env and host mounts. `vllm-mooncake` also writes `mooncake_protocol` into `kv_connector_extra_config` (`rdma` on a Host RDMA network, otherwise `tcp`), sets `VLLM_MOONCAKE_BOOTSTRAP_PORT` on prefill, and sets `VLLM_HOST_IP` on every instance; for a model with linear attention or Mamba layers (`layer_types` in `config.json`) it also adds `--no-disable-hybrid-kv-cache-manager`, unless the template or user states either polarity. Connector parameters supplied by a template or user are rejected (ADR 0028).

Features such as sleep, LoRA, speculative decoding, and KV offloading inject arguments only when the corresponding dictionary fields are declared. For example, vLLM sleep injects
`--enable-sleep-mode` and `VLLM_SERVER_DEV_MODE=1`.

## User Input and Container Overrides

### Parameters

For dictionary fields and `extra_args`, see "Parameter Dictionary". `quick_params_unset` explicitly disables parameters enabled by the template or by default.

### Command Modes

| Mode | Entrypoint | Argv | Env |
|---|---|---|---|
| `engine` (default) | Generated by the engine; entrypoint override > template entrypoint | Engine arguments + remaining template arguments + remaining user arguments | System → template → user |
| `image-entry` | Empty (uses the image ENTRYPOINT); can be overridden | Command override (if any) + engine argument vector | System → template → user |
| `manual` (handwritten command) | `/bin/sh -c`; you write the entire script | Platform variables with the `GPUWORKS_` prefix expand per rank; `--port` and `--served-model-name` are rewritten to platform-allocated values | Single-node, non-PD: **no system layer**, template → user only; multi-node ranks and PD instances: system → template → user |

When you select "Command already includes preparation" for a handwritten command, the template's preparation script no longer runs, avoiding duplicate execution. A handwritten command for
a multi-node or PD deployment is shared by every rank and must reference the variables that tell ranks apart; otherwise evaluation and creation return `MANUAL_COMMAND_RANK_BLIND`. See
[Manual Commands for Multi-Node and PD Deployments](../explanation/launch-config.md#多机与-pd-的手工命令).

### Container Override Fields

| Field | Effect | Restrictions |
|---|---|---|
| `env` / `env_unset` | Adds or overrides environment variables; removes variables set by the engine or template | Device and dGPU env are injected on the node by the agent and are unaffected |
| `extra_mounts` | Host directory, named volume or registered model (`model_id`) → container path; can be read-only. A model row is always read-only, and the deployment depends on that model as on a template component: a node without it acquires it from the model's storage, and the launch mounts the node's actual directory, e.g. a speculative-decoding draft at `/draft` | One source per row; paths must be absolute and non-root; they must not overlap or nest with platform mounts; `model_id` must be a registered model (`MOUNT_MODEL_NOT_FOUND`) |
| `model_mount_path` | Model mount path (default `/model`); also rewrites the model path in the command | The primary component path cannot be changed when model components are present |
| `shm_bytes` | Upper size limit of the container's `/dev/shm` tmpfs, not a memory reservation. When unspecified, it is the engine baseline of 16 GiB plus the CPU memory tier size (16 GiB if the memory tier is disabled). If the template spec declares a larger `shm_bytes`, that value is used. When docker arguments contain `--ipc=host`, the launch configuration carries no size (and no `--shm-size` is sent); the container shares the host's `/dev/shm` | Rejected when set together with `--ipc=host` (`SHM_SIZE_WITH_HOST_IPC`); rejected when smaller than the CPU memory tier (`SHM_BELOW_KV_CPU_TIER`) |
| `docker_args` | Appended to `docker run` in order; the final arguments also include platform-appended RDMA options and `--ulimit memlock=-1:-1` from the template spec's `memlock_unlimited` | See the table below |
| `entrypoint_display`, `cmd_display`, `command_mode`, `command_includes_preparation` | See command modes | — |

### `/dev/shm` Size

vLLM puts the CPU memory tier (dictionary parameter `kv_offloading_size`, in GiB) in `/dev/shm`. The required space equals the entire memory tier
(the total across all workers, not a separate copy for each tensor-parallel rank; measurements show that a 19 GiB memory tier requires 19450 MiB).
If the container's `/dev/shm` is smaller, the engine exits at startup with `Insufficient space in /dev/shm`. Therefore, `ResolveLaunch`
derives the size using the following rules. `cpu_bytes_to_use` in `--kv-transfer-config` reads the same value used here
(`deploy.KVCPUTierBytes`):

| Situation | `/dev/shm` |
|---|---|
| Docker arguments contain `--ipc=host` | No size is specified; the container uses the host's `/dev/shm` (the memory tier is not validated in this case) |
| The deployment specifies `shm_bytes` | Uses the specified value (overrides the template's `shm_bytes`, even if smaller); if it is smaller than the CPU memory tier, `ResolveLaunch` returns `SHM_BELOW_KV_CPU_TIER` (parameters `shm_gib`, `tier_gib`) |
| Unspecified | The larger of "engine baseline + CPU memory tier" and "template spec's `shm_bytes`"; if the memory tier is disabled and the template declares no value, uses the engine baseline |

- The template spec's `shm_bytes` is the total `/dev/shm` needed by the engine itself (for example, DeepInfer places an approximately 400 GB Engram table in shared memory; the A3 profiles of DeepSeek-V4.1-Flash and its 1017 variant each declare 512 GiB for two approximately 103 GiB local DP shared tables). It therefore takes the maximum of this value and the derived value instead of adding them. It is not a container override, and the image catalog does not contribute it either (ADR 0031 is unaffected). The deployment's own `shm_bytes` always takes precedence; the template value cannot make an explicit value smaller than the memory tier pass validation. If the value declared by the template exceeds the node's physical memory, the container still starts normally (`--shm-size` is only a limit, not a reservation); the engine reports an error when writes exceed the limit.
- The baseline is left for NCCL, torch IPC, and other users of `/dev/shm`: when memory tier allocation fails, the 16 GiB is completely unused, so the tier size is added rather than carved out of the baseline. A specified value only needs to be at least as large as the memory tier; you must add any headroom for these other uses yourself.
- The memory tier counts only when the engine can express the parameter and the platform generates the command. With a handwritten command (`manual`) or a complete `cmd_display` override, you are responsible for your command. The platform neither derives a size from memory tier settings left in the quick parameters nor rejects the request on that basis.
- With PD disaggregation, only the prefill role has a memory tier (offloading configuration is removed from the decode role), so decode instances still use the engine baseline. Deployment-level `shm_bytes` applies equally to both roles, so it must satisfy prefill. The preview shows each instance's actual `shm_bytes` separately.
- The evaluation API attaches this error to the shared memory control (`container_shm_gib`); the creation API returns 400. Both occur before anything starts running.
- Cases without validation: udocker has no `--shm-size`; the container uses the host's `/dev/shm` (half of physical memory by default). `--ipc=host` also uses the host's shared memory. The platform currently does not report the host's `/dev/shm` size. In these two cases, a memory tier exceeding the host's `/dev/shm` still fails when the engine starts.

### Locked Memory (memlock)

Some engines need to lock large amounts of host memory into RAM (for example, DeepInfer uses `cudaHostRegister` to lock an approximately 197 GB Engram table, and the DeepSeek A3 profiles use `aclrtHostRegisterV2` to register shared Engram tables), and the default `memlock` limit prevents them from starting. Setting `memlock_unlimited: true` in the template spec removes the limit only for that deployment's container, without changing the host. The A3 profiles of DeepSeek-V4.1-Flash and its 1017 variant both use this declaration instead of duplicating a Docker memlock argument in template `args`.

| Situation | `memlock` in the Final Docker Arguments |
|---|---|
| The deployment's own `docker_args` specifies `--ulimit memlock=…` (both `--ulimit memlock=…` and `--ulimit=memlock=…` forms count) | Uses the deployment's entry; neither the template nor RDMA appends another |
| Unspecified, with spec `memlock_unlimited: true` | `--ulimit memlock=-1:-1` |
| Unspecified, for a multi-node rank or PD instance on a host RDMA network | `--ulimit memlock=-1` (still only this entry if the spec also sets `memlock_unlimited`) |
| Unspecified, not declared by the spec (omitted or `false`), and not on a host RDMA network | No `memlock` |

- The final arguments contain at most one `memlock` entry, determined centrally by `launchDockerArgs` in `deploy/memlock.go`.
- This entry is generated in `ResolveLaunch`, so the deployment page's "Runtime specification" preview (`docker_args` in the evaluation API) and the runtime information in instance details show the arguments that the container actually receives.
- Removing the limit requires container permission: docker allows `--ulimit memlock=-1` by default; udocker only parses `--device` and does not apply ulimit.
- The spec value is saved with the deployment's spec snapshot; later template changes do not affect existing deployments.

### Disallowed Docker Arguments

The platform's own docker arguments (RDMA) are exempt from these restrictions; only arguments supplied by the deployment are validated here.

| Syntax | Error Code | Use Instead |
|---|---|---|
| `--name`, `-d`/`--detach`, `-a`, `--rm`, `--restart`, `--network`/`--net`, `-p`/`--publish`, `-P`, `--gpus`, `--runtime` | `DOCKER_ARG_PLATFORM_MANAGED` | Set by the platform; cannot be changed |
| `--shm-size` | `DOCKER_ARG_USE_SHM_FIELD` | `shm_bytes` |
| `-e`, `--env`, `--env-file` | `DOCKER_ARG_USE_ENV_FIELD` | Environment variable rows |
| `-v`, `--volume`, `--mount` | `DOCKER_ARG_USE_MOUNT_FIELD` | Mount rows |
| `--entrypoint` | `DOCKER_ARG_USE_ENTRYPOINT_FIELD` | Entrypoint override |
| `--privileged`, `--pid`, `-v /` mounting the host root directory | Rejected for security (`FIELD_INVALID`) | Unsupported |

The evaluation API attaches these errors to the docker arguments or shared memory control in the deployment form; the creation API returns 400. The udocker runtime only parses `--device` among these arguments.

## Environment Variable Layers

`ResolvedLaunch.env_layers` records the source of each layer; `env` is the merged result:

1. `system`: engine baseline env + parameter network env.
2. `template`: spec env.
3. `user`: container override env.

`env_unset` takes effect before `user`: it removes keys set by `system` and `template` from the merged result (for example, removing the engine-injected
`HF_HUB_OFFLINE=1` allows a custom image to download over the network). `env_layers` retains each layer's original values for source tracing; if the same key is written into `env` again, the deployment's value takes precedence.

Two types of additions follow: SGLang logging configuration and the agent's multi-node network interface selection fill in missing values; the agent's device and dGPU env override all layers above.

Image-built-in ENV is inherited by the container runtime; only keys explicitly set by `-e` are replaced (for example, Ascend vcann-rt first reads the image's
`LD_LIBRARY_PATH` and then appends the driver paths).

For non-Chinese GPUs, the udocker runtime (both the runveil and udocker CLI launchers) includes
`LD_LIBRARY_PATH=/opt/nvidia-shim:/usr/local/nvidia/lib64:/usr/local/nvidia/lib:/usr/local/cuda/lib64`: if deployment env
does not set this key, this value is merged before the image's value (deduplicating and discarding empty segments); if deployment env sets it, the deployment's value is used unchanged.

## Variable Expansion

Variables expand at every launch, in template args, env, entrypoint, and preparation scripts, and in the handwritten command of `manual` mode; other user input is **never expanded**.
The syntax is `${GPUWORKS_NAME}`, where the name consists of uppercase letters, digits, and underscores. Available variables are resolver outputs plus the platform-provided `GPUWORKS_GPU_COUNT`,
`GPUWORKS_TP_SIZE`, `GPUWORKS_MODEL_PATH`, `GPUWORKS_SERVED_MODEL_NAME`, `GPUWORKS_API_HOST`, `GPUWORKS_API_PORT`,
`GPUWORKS_NNODES`, `GPUWORKS_NODE_RANK`, and `GPUWORKS_HEADLESS`, with `GPUWORKS_NODE_ADDR`, `GPUWORKS_MASTER_ADDR`, and
`GPUWORKS_MASTER_PORT` additionally available for multi-node deployments, and `GPUWORKS_PD_ROLE`, `GPUWORKS_PD_SIDE_CHANNEL_HOST`, and `GPUWORKS_PD_SIDE_CHANNEL_PORT` for PD instances;
`PP_SIZE` has no default. Template fields still expand the older unprefixed spelling (`${NODE_RANK}`); a handwritten command expands prefixed names only and leaves every other `${...}`
to the shell. For the full vocabulary, availability conditions, and error rules, see [Inference Catalog Syntax](inference-catalog-spec.md#变量展开).

## Agent Injection

The agent does not reconstruct catalog, image, or command semantics. It only makes the following additions related to node facts, in order:

1. Mounts the model directory read-only at `model_mount`.
2. For multi-node vLLM: fills empty `GLOO_SOCKET_IFNAME` and `NCCL_SOCKET_IFNAME` from the network interface containing `VLLM_HOST_IP`.
3. Mounts spec assets read-only.
4. If a preparation script exists, writes `/gpuworks/pre-start.sh` (`set -eu`, script, `exec "$@"`), changes the entrypoint to `/bin/sh /gpuworks/pre-start.sh`, and passes the original command as arguments.
5. Sets the KV disk tier mount source and quota.
6. **Device injection** (overrides env with the same name and replaces existing `--gpus`/`--runtime`):
   - NVIDIA: `--gpus "device=…"` + `CUDA_VISIBLE_DEVICES` numbered from 0
   - Hygon: `--device` kfd/DRM, `/opt/hyhal`, `--group-add video`, `HIP/ROCR/HYGON_VISIBLE_DEVICES`
   - Ascend docker: `--runtime ascend` + `ASCEND_VISIBLE_DEVICES`, etc.; Ascend nerdctl: `/dev/davinci*` devices and driver mounts
   - PPU: `/dev/alixpu*` devices, `NVIDIA_VISIBLE_DEVICES=void`, `CUDA_VISIBLE_DEVICES`
   - Kunlunxin: Linux amd64 / Docker whole-card TP. Recheck each selected card’s UUID/BDF/minor and sysfs identity, then inject only its `/dev/xpuN` plus `/dev/xpuctrl`. Occupancy comes from the common compute-process inventory, not memory usage alone. Template options use the common administrator contract, with no vendor-specific Docker option filter. See the [support matrix](support-matrix.md) for native multi-node requirements.
7. **dGPU partitioning**: the server sends only the GPU memory limit, compute percentage, and backend name; all injection happens in the agent:
   - HAMi-core: mounts `libvgpu.so`; sets `LD_PRELOAD`, `CUDA_DEVICE_MEMORY_LIMIT_<i>`, and `CUDA_DEVICE_SM_LIMIT` when the deployment carries a compute percentage (deployment requests currently do not accept a compute percentage; see [dGPU Explained](../explanation/dgpu.md))
   - vcann-rt: mounts `libvruntime.so`, etc.; sets `ENPU_DSMI_HOOK=1` and `LD_LIBRARY_PATH`
   - HGGC: mounts `hggc_quota.so`, etc.; sets `LD_PRELOAD` and `HGGC_DEVICE_MEMORY_LIMIT_<i>`

For details, see [dGPU Explained](../explanation/dgpu.md).

## Runtime Assembly

Docker argument order:

```text
docker run -d --name gpuworks-<first-12-UUID-characters> --restart no
  --network host                 # Linux; on other systems, use -p 127.0.0.1:<port>:<port>
  <docker_args>                  # deployment docker arguments + platform RDMA arguments + spec memlock_unlimited + device injection
  --shm-size <shm_bytes>         # omitted when shm_bytes is 0 (--ipc=host)
  -e …  -v …
  --entrypoint <entrypoint[0]> <image> <entrypoint[1:]> <argv>
```

nerdctl uses the same order but omits `--restart`; udocker does not support network settings, shm settings, or most docker arguments.

## Related Documentation

- ADR [0009](../adr/0009-flat-spec-catalog.md) (flat specs), [0013](../adr/0013-image-command-mode.md) (command modes),
  [0020](../adr/0020-launch-config-single-source.md) (single source of launch configuration), [0021](../adr/0021-platform-params-copyable-command.md) (platform parameters excluded from argv),
  [0022](../adr/0022-template-owned-deployment-launch.md) (template-owned launch), [0031](../adr/0031-image-catalog-carries-no-launch-config.md) (image catalog carries no launch configuration)
- [Inference Catalog model.yaml Syntax Reference](inference-catalog-spec.md), [Image Catalog and Inference Image Families](image-families.md)
- [Deploy Models](../how-to/deploy-models.md), [Multi-Node Inference](../how-to/multi-node-inference.md), [PD Disaggregation](../how-to/pd-disaggregation.md)
