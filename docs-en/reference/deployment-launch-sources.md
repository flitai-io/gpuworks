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
| 5 | Inference template spec | Non-dictionary args, env, entrypoint, preparation scripts, assets, components, API contract, `/dev/shm` minimum (`shm_bytes`), unlimited locked memory (`memlock_unlimited`), and extra docker arguments (`docker_args`) | Overrides engine-injected values; user input can override this layer in turn | `inferencecatalog/flat.go`, `deploy/catalog_selection.go` |
| 6 | Topology | Multi-node and PD role arguments and env; RDMA docker arguments | Platform-owned; neither templates nor user input can override the PD connector | `deploy/pd.go`, `deploy/compute_network.go` |
| 7 | Engine finalization | For example, SGLang logging configuration | **Fills in missing values** | `deploy/sglang_logging.go` |
| 8 | User input | Dictionary fields, `extra_args` | Overrides the template | `deployment/launch_config.go` |
| 9 | Container overrides | env, env_unset, mounts, docker arguments, `/dev/shm` (when unspecified, derived from the engine baseline plus the CPU memory tier; see "`/dev/shm` Size"), model mount path, entrypoint, command, and command mode | Final deployment-level layer | `deploy/assemble.go`, `kv_cpu_tier.go`, `effective_command.go` |
| 10 | Agent injection | Model directory mount, preparation script wrapper, devices, and dGPU partitions | Device and dGPU env **override everything** | `agent/engine/engine.go`, `agent/gpu/*.go`, `agent/engine/partition*.go` |
| 11 | Runtime assembly | Fixed parts of `docker run`: name, `-d`, `--restart no`, network, and `--shm-size` | Platform-owned; specifying these in docker arguments is rejected | `agent/runtime/docker/docker.go`, `nerdctl/nerdctl.go` |

## Engine Baseline

| Engine | Command | Baseline Env |
|---|---|---|
| vLLM | `vllm serve <model-path>`, `--host 127.0.0.1`, `--port`, `--served-model-name` (the deployment alias; the model path `/model` when no alias is set; omitted when headless); `--tensor-parallel-size` is always injected, and `--pipeline-parallel-size` is injected when greater than 1 | `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`, `VLLM_HOST_IP=127.0.0.1`, `GLOO_SOCKET_IFNAME=lo`, `NCCL_SOCKET_IFNAME=lo` |
| SGLang | `python -m sglang.launch_server --model-path … --tp-size --pp-size --host --port --served-model-name` (all in the entrypoint; `--served-model-name` as for vLLM) | `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1` |
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
  - No default: SGLang `max_model_len`, tri-state switches (such as `enable_prefix_caching`), and other `automatic` rows.
- vLLM context defaults to memory fitting: for new requests using the ordinary engine command, an image-catalog version or unambiguous tag indicating vLLM 0.14.0 or later materializes an unspecified `max_model_len` as integer `-1` and emits `--max-model-len auto`. No additional GPU-vendor gate applies. Unknown or older versions, manual commands, and Omni multi-stage launches do not receive this default. Template limits, explicit user values, and `quick_params_unset` take precedence. Rebuilding or editing stored deployments does not introduce a default.
- Explicit vLLM `auto` and `-1` values in fields, template args, or `extra_args` normalize to the same integer `-1` for storage, editing, estimation, and template export. This records memory-fit intent, not an observed token limit. As with other version-dependent parameters, users remain responsible for the compatibility of explicit overrides. SGLang does not accept this sentinel. Estimation still uses the model window as a ceiling without claiming to predict the fitted length; auto mode no longer receives the KV-shortfall warning intended for a fixed full-window requirement. Auto cannot fix oversized weights or multimodal runtime memory exhaustion.
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
| `start_timeout_sec` | The template step of the startup wait limit (source `template`); see "Startup Wait Limit". Saved with the spec snapshot; later template changes do not affect existing deployments |
| `docker_args` | Appended to the front of `docker run`, before the deployment's own `docker_args` and the platform's appended arguments; see "The three-layer order of docker run arguments". Saved with the spec snapshot; later template changes do not affect existing deployments |
| `image` | Image used by the deployment; it need not be registered in the image catalog first |

Template specs always use the `engine` command mode; `image-entry` and `manual` can only be explicitly selected for the current deployment.

## Topology

- **Multi-node**: rank 0 is the leader; the others are headless followers. vLLM injects `--distributed-executor-backend mp --nnodes --node-rank --headless --master-addr --master-port` (default port 29500), and sets `VLLM_HOST_IP` to the node's parameter network address. SGLang injects `--nnodes --node-rank --dist-init-addr`.
- **Peers file**: every rank receives a read-only JSON listing every rank of the group and its rails, at the path in `GPUWORKS_PEERS_FILE`; see "Multi-Node Peers File" below.
- **Parameter network**: injects system-layer env such as `NCCL_SOCKET_IFNAME`, `GLOO_SOCKET_IFNAME`, `NCCL_IB_*`, and `UCX_NET_DEVICES`. When RDMA is used, it appends the docker arguments `--device /dev/infiniband --ulimit memlock=-1 --cap-add IPC_LOCK` (ADR 0027). If the deployment's own docker arguments already specify `memlock`, this item is not appended; see "Locked Memory".
- **PD disaggregation**: generates vLLM `--kv-transfer-config` or SGLang `--disaggregation-*` by role, along with NIXL/UCX-related env and host mounts. `vllm-mooncake` also writes `mooncake_protocol` into `kv_connector_extra_config` (`rdma` on a Host RDMA network, otherwise `tcp`), sets `VLLM_MOONCAKE_BOOTSTRAP_PORT` on prefill, and sets `VLLM_HOST_IP` on every instance; for a model with linear attention or Mamba layers (`layer_types` in `config.json`) it also adds `--no-disable-hybrid-kv-cache-manager`, unless the template or user states either polarity. Connector parameters supplied by a template or user are rejected (ADR 0028).

Features such as sleep, LoRA, speculative decoding, and KV offloading inject arguments only when the corresponding dictionary fields are declared. For example, vLLM sleep injects
`--enable-sleep-mode` and `VLLM_SERVER_DEV_MODE=1`.

### Multi-Node Peers File (`GPUWORKS_PEERS_FILE`)

Every rank of a multi-node deployment (`nnodes > 1`, including multi-node PD roles) receives a read-only JSON document listing where every rank of its group is, so a recipe can write its own rendezvous or rank table (fleet.conf, an HCCL rank table, a Mooncake configuration, and so on) without building discovery of its own. The file is at `/gpuworks/peers.json`: the agent writes it in the instance's own runtime mount directory and mounts it read-only there; it is rewritten on every container start and removed with the instance. The path is both the launch variable `${GPUWORKS_PEERS_FILE}` (expanded in a template's args, env, preparation script and in manual commands) and, with the same name and value, the system-layer env `GPUWORKS_PEERS_FILE` in the container. In a multi-node launch the variable always expands to that path, also before the group's ranks are placed (the deployment form's summary preview); whether the file is mounted is decided by the peers. A single-node launch whose template fields reference `${GPUWORKS_PEERS_FILE}` fails, like a single-node reference to `${GPUWORKS_MASTER_ADDR}`; in a manual command it is left to the shell like any other variable the launch does not state. `/gpuworks` is the directory where the platform keeps its own files; see the platform path rule in [Inference Catalog Syntax](inference-catalog-spec.md#变量展开).

The content is fixed when the group is placed (its Go type is `proto.PeersFile`; for the decision see [ADR 0078](../adr/0078-multi-node-peers-file.md)). The files of the ranks differ only in `node_rank`. Field names follow the launch variables (`node_rank`, `nnodes`, `node_addr`, `master_addr`, `master_port`: the variables in lower case) and the network API's vocabulary (a node's `interface` and `hcas`, each rail's `name`, `netdev` and `ip`). Every value that also exists as a launch variable equals that rank's launch variable on every rank:

```json
{
  "version": 1,
  "node_rank": 1,
  "nnodes": 2,
  "master_addr": "192.168.177.51",
  "master_port": 29500,
  "network": {"type": "host_rdma", "link_layer": "roce"},
  "ranks": [
    {
      "node_rank": 0,
      "node_uuid": "3f0c…",
      "node_name": "gb10-1",
      "hostname": "gb10-1",
      "node_addr": "192.168.177.51",
      "management": {"interface": "eno1", "ip": "10.0.0.51"},
      "interface": "enp1s0f1np1",
      "hcas": [
        {"name": "roceP2p1s0f1:1", "device": "roceP2p1s0f1", "port": 1, "netdev": "enP2p1s0f1np1", "ip": "192.168.178.51"},
        {"name": "rocep1s0f1:1", "device": "rocep1s0f1", "port": 1, "netdev": "enp1s0f1np1", "ip": "192.168.177.51"}
      ]
    },
    {
      "node_rank": 1,
      "node_uuid": "9a41…",
      "node_name": "gb10-2",
      "hostname": "gb10-2",
      "node_addr": "192.168.177.52",
      "management": {"interface": "eno1", "ip": "10.0.0.52"},
      "interface": "enp1s0f1np1",
      "hcas": [
        {"name": "roceP2p1s0f1:1", "device": "roceP2p1s0f1", "port": 1, "netdev": "enP2p1s0f1np1", "ip": "192.168.178.52"},
        {"name": "rocep1s0f1:1", "device": "rocep1s0f1", "port": 1, "netdev": "enp1s0f1np1", "ip": "192.168.177.52"}
      ]
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `version` | Shape version, currently `1`. It changes only when a field's meaning changes, not when fields are added; readers should ignore keys they do not know |
| `node_rank` / `nnodes` | The reader's own rank (equal to `${GPUWORKS_NODE_RANK}`; 0 leads) and the number of ranks in the group (equal to `${GPUWORKS_NNODES}`, and the length of `ranks`) |
| `master_addr` / `master_port` | The rendezvous of the whole group, equal to `${GPUWORKS_MASTER_ADDR}` and `${GPUWORKS_MASTER_PORT}`; omitted when there is none |
| `network` | The compute network the group shares: `type` (`host_rdma`, `host_ethernet`, `accelerator_rdma`, `accelerator_hccs`) and `link_layer` (`infiniband`, `roce`, `hccs`; empty for Ethernet); omitted when the group runs over the management network |
| `ranks[].node_rank` | The rank's number |
| `ranks[].node_uuid` / `node_name` / `hostname` | The node's identity and name in the platform, and the host name the node reports |
| `ranks[].node_addr` | The address the rank advertises to the group, equal to its own `${GPUWORKS_NODE_ADDR}`: its address on the compute network, otherwise its routable management address |
| `ranks[].management` | The node's `interface` and `ip` on the management network; omitted when its agent has not reported one |
| `ranks[].interface` | The host interface carrying the group's bootstrap traffic; omitted when none was resolved |
| `ranks[].hcas[]` | The compute network's rails on that node, in the order `NCCL_IB_HCA` names them: RDMA ports on a host RDMA network, the cards' ports on an accelerator network, empty on Ethernet. `name` is the platform's name for the rail (`device:port` for an RDMA port, such as `rocep1s0f1:1`; `npu3` for a card), `device` and `port` are an RDMA rail's device and port split out (empty for a card), `netdev` the host interface holding the rail's address (for RoCE the interface whose address the port's GID table carries, which can be a bridge or bond above the port), and `ip` the rail's own IPv4 address (for RoCE the address verified against the port's GID table, otherwise the interface's; the IPoIB address; the card port's address); omitted when not reported |

The file states only facts known when the platform placed the group or reported by the agents; values a container probes for itself, such as the RoCE GID index, are not included. The agent writes the peers file, so it follows the one agent capability rule in [What the Template Declares and What the Platform Provides](inference-catalog-spec.md#模板与平台的分工) (capability `launch.peers_file`, which older agents do not advertise): when the template references `${GPUWORKS_PEERS_FILE}` in a launch field, an older agent's node is refused with `agent_capability_missing`; when it does not, a rank on an older agent starts as before, only with neither the file nor the environment variable (the other ranks' files still list it). Single-node deployments have no peers file, and referencing the variable fails at deployment, as referencing `MASTER_ADDR` does.

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
| `extra_mounts` | Host directory, named volume or registered model (`model_id`) → container path; can be read-only. A model row is always read-only, and the deployment depends on that model as on a template component: a node without it acquires it from the model's storage, and the launch mounts the node's actual directory, e.g. a speculative-decoding draft at `/draft` | One source per row; paths must be absolute and non-root; they must not overlap or nest with platform mounts; `model_id` must be a registered model (`MOUNT_MODEL_NOT_FOUND`); a volume name starts with a letter or digit and contains only letters, digits and `_.-` (`MOUNT_VOLUME_INVALID`); a named volume is created and kept by the container runtime, so only docker and nerdctl nodes mount one, and udocker, Kubernetes and host-process nodes refuse the launch (`MOUNT_VOLUME_UNSUPPORTED`) |
| `model_mount_path` | Model mount path (default `/model`); also rewrites the model path in the command | The primary component path cannot be changed when model components are present |
| `shm_bytes` | Upper size limit of the container's `/dev/shm` tmpfs, not a memory reservation. When unspecified, it is the engine baseline of 16 GiB plus the CPU memory tier size (16 GiB if the memory tier is disabled). If the template spec declares a larger `shm_bytes`, that value is used. When docker arguments contain `--ipc=host`, the launch configuration carries no size (and no `--shm-size` is sent); the container shares the host's `/dev/shm` | Rejected when set together with `--ipc=host` (`SHM_SIZE_WITH_HOST_IPC`); rejected when smaller than the CPU memory tier (`SHM_BELOW_KV_CPU_TIER`) |
| `docker_args` | Appended to `docker run` in order, after the template spec's own `docker_args`; the final arguments also include platform-appended RDMA options and `--ulimit memlock=-1:-1` from the template spec's `memlock_unlimited`; see "The three-layer order of docker run arguments" | See the table below |
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

### The three-layer order of docker run arguments

A template spec's own `docker_args` (an inference catalog field; see [Inference Catalog Syntax](inference-catalog-spec.md)) is the template layer of the deployment's `docker_args`: like `shm_bytes` and `memlock_unlimited`, it is saved with the spec snapshot, and later template changes do not affect existing deployments. The final `docker run` arguments always apply in this order:

1. The template spec's own `docker_args`
2. The deployment's own `docker_args` (container override field)
3. Platform-appended arguments (topology RDMA options, the `--ulimit` from `memlock_unlimited`; see "Locked Memory")

Validation is identical to the deployment's own `docker_args` (`deploy.ValidateDockerArgs`, see "Disallowed Docker Arguments" below), and applies to this same spec's own `shm_bytes`: a spec that states both `--ipc=host` and `shm_bytes` is already rejected on import.

If the deployment itself does not state `--ipc=host` but the selected template spec does, the deployment's own `shm_bytes` has no effect; the platform rejects this (`SHM_SIZE_WITH_HOST_IPC`) rather than silently ignoring it, because that size would never apply - see "`/dev/shm` Size".

The evaluation API's `docker_args` (the deployment page's "Runtime specification" preview) and the runtime information in instance details are already the result of merging all three layers, matching what the container actually receives.

### Disallowed Docker Arguments

The platform's own docker arguments (RDMA) are exempt from these restrictions; this validation (`deploy.ValidateDockerArgs`) treats the deployment's own `docker_args` and a template spec's own `docker_args` the same way.

| Syntax | Error Code | Use Instead |
|---|---|---|
| `--name`, `-d`/`--detach`, `-a`, `--rm`, `--restart`, `--network`/`--net`, `-p`/`--publish`, `-P`, `--gpus`, `--runtime` | `DOCKER_ARG_PLATFORM_MANAGED` | Set by the platform; cannot be changed |
| `--shm-size` | `DOCKER_ARG_USE_SHM_FIELD` | `shm_bytes` |
| `-e`, `--env`, `--env-file` | `DOCKER_ARG_USE_ENV_FIELD` | Environment variable rows |
| `-v`, `--volume`, `--mount` | `DOCKER_ARG_USE_MOUNT_FIELD` | Mount rows |
| `--entrypoint` | `DOCKER_ARG_USE_ENTRYPOINT_FIELD` | Entrypoint override |
| `--privileged`, `--pid`, `-v /` mounting the host root directory | Rejected for security (`FIELD_INVALID`) | Unsupported |
| `--ipc=host` together with this same layer's own non-zero `shm_bytes` | `SHM_SIZE_WITH_HOST_IPC` | Clear that layer's `shm_bytes`, or remove `--ipc=host` |

The evaluation API attaches these errors to the docker arguments or shared memory control in the deployment form; the creation API returns 400. The udocker runtime only parses `--device` among these arguments.
A template spec's `docker_args` that fails this validation is rejected on import, so it is never discovered only at deployment time.

## Startup Wait Limit

The longest time an instance waits after it starts for the engine health check to pass (`start_timeout_sec`, seconds, 60–86400) is decided in this order; the step that decided it is recorded as `start_timeout_source` in instance and evaluation responses:

1. `cluster`: the cluster default, setting `deployment.start_timeout_sec` (factory default 1800).
2. `template`: the selected inference template spec's `start_timeout_sec`, when it states one, replaces the previous step.
3. `engine`: when the final environment of a managed vLLM launch states `VLLM_ENGINE_READY_TIMEOUT_S` explicitly and that plus a 30-second probe margin is longer than the previous step, the limit is raised to it (at most 86400).
4. `deployment`: the deployment's own `start_timeout_sec` replaces all of the above and is not raised; when it is shorter than the engine deadline, only a conflict is reported (`start_timeout_conflict`).

The template and the deployment use the same field name, unit and range; the template's value is saved with the spec snapshot, so later template changes do not affect existing deployments. The value is fixed when an instance is created; changes affect only instances started afterward.

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
`GPUWORKS_MASTER_PORT` additionally available for multi-node deployments, `GPUWORKS_PEERS_FILE` when the launch has a peers file, and `GPUWORKS_PD_ROLE`, `GPUWORKS_PD_SIDE_CHANNEL_HOST`, `GPUWORKS_PD_SIDE_CHANNEL_PORT`, and `GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT` for PD instances;
`PP_SIZE` has no default. Template fields still expand the older unprefixed spelling (`${NODE_RANK}`); a handwritten command expands prefixed names only and leaves every other `${...}`
to the shell. For the full vocabulary, availability conditions, and error rules, see [Inference Catalog Syntax](inference-catalog-spec.md#变量展开).

## Agent Injection

The agent does not reconstruct catalog, image, or command semantics. It only makes the following additions related to node facts, in order:

1. Mounts the model directory read-only at `model_mount`.
2. For multi-node vLLM: fills empty `GLOO_SOCKET_IFNAME` and `NCCL_SOCKET_IFNAME` from the network interface containing `VLLM_HOST_IP`.
3. Mounts spec assets read-only.
4. If a preparation script exists, writes `/gpuworks/pre-start.sh` (`set -eu`, script, `exec "$@"`), changes the entrypoint to `/bin/sh /gpuworks/pre-start.sh`, and passes the original command as arguments.
5. For a multi-node rank with a peers file, writes it as a read-only file and mounts it at `/gpuworks/peers.json`; see "Multi-Node Peers File".
6. Sets the KV disk tier mount source and quota.
7. **Device injection** (overrides env with the same name and replaces existing `--gpus`/`--runtime`):
   - NVIDIA: `--gpus "device=…"` + `CUDA_VISIBLE_DEVICES` numbered from 0
   - Hygon: `--device` kfd/DRM, `/opt/hyhal`, `--group-add video`, `HIP/ROCR/HYGON_VISIBLE_DEVICES`
   - Ascend docker: `--runtime ascend` + `ASCEND_VISIBLE_DEVICES`, etc.; Ascend nerdctl: `/dev/davinci*` devices and driver mounts
   - PPU: `/dev/alixpu*` devices, `NVIDIA_VISIBLE_DEVICES=void`, `CUDA_VISIBLE_DEVICES`
   - Kunlunxin: Linux amd64 / Docker whole-card TP. Recheck each selected card’s UUID/BDF/minor and sysfs identity, then inject only its `/dev/xpuN` plus `/dev/xpuctrl`. Occupancy comes from the common compute-process inventory, not memory usage alone. Template options use the common administrator contract, with no vendor-specific Docker option filter. See the [support matrix](support-matrix.md) for native multi-node requirements.
8. **dGPU partitioning**: the server sends only the GPU memory limit, compute percentage, and backend name; all injection happens in the agent:
   - HAMi-core: mounts `libvgpu.so`; sets `LD_PRELOAD`, `CUDA_DEVICE_MEMORY_LIMIT_<i>`, and `CUDA_DEVICE_SM_LIMIT` when the deployment carries a compute percentage (deployment requests currently do not accept a compute percentage; see [dGPU Explained](../explanation/dgpu.md))
   - vcann-rt: mounts `libvruntime.so`, etc.; sets `ENPU_DSMI_HOOK=1` and `LD_LIBRARY_PATH`
   - HGGC: mounts `hggc_quota.so`, etc.; sets `LD_PRELOAD` and `HGGC_DEVICE_MEMORY_LIMIT_<i>`

For details, see [dGPU Explained](../explanation/dgpu.md).

## Runtime Assembly

Docker argument order:

```text
docker run -d --name gpuworks-<first-12-UUID-characters> --restart no
  --network host                 # Linux; on other systems, use -p 127.0.0.1:<port>:<port>
  <docker_args>                  # spec docker_args + deployment docker arguments + platform RDMA arguments + spec memlock_unlimited + device injection
  --label gpuworks.…=…           # ownership labels, see below
  --shm-size <shm_bytes>         # omitted when shm_bytes is 0 (--ipc=host)
  -e …  -v …
  --entrypoint <entrypoint[0]> <image> <entrypoint[1:]> <argv>
```

nerdctl uses the same order but omits `--restart`; udocker does not support network settings, shm settings, or most docker arguments.

docker and nerdctl containers carry ownership labels that tell apart the containers started by several agents on one machine (each with its own state directory and its own control plane). They are for troubleshooting only: the agent never selects or cleans up containers by them. A label whose value is unknown is omitted:

| Label | Value |
| --- | --- |
| `gpuworks.server` | the control-plane address the agent connects to (`--server`); omitted on server_dial nodes, which do not know it |
| `gpuworks.node` | the node UUID the control plane issued |
| `gpuworks.state-dir` | absolute path of the agent's state directory |
| `gpuworks.instance` | instance UUID (instance containers only) |
| `gpuworks.deployment` | deployment ID (instance containers only) |

For example `docker ps -a --filter label=gpuworks.server=https://gpuworks.example:8800`, or `docker inspect -f '{{json .Config.Labels}}' <container>`. Card runtime check and dGPU behavior probe containers carry only the first three labels.

## Related Documentation

- ADR [0009](../adr/0009-flat-spec-catalog.md) (flat specs), [0013](../adr/0013-image-command-mode.md) (command modes),
  [0020](../adr/0020-launch-config-single-source.md) (single source of launch configuration), [0021](../adr/0021-platform-params-copyable-command.md) (platform parameters excluded from argv),
  [0022](../adr/0022-template-owned-deployment-launch.md) (template-owned launch), [0031](../adr/0031-image-catalog-carries-no-launch-config.md) (image catalog carries no launch configuration)
- [Inference Catalog model.yaml Syntax Reference](inference-catalog-spec.md), [Image Catalog and Inference Image Families](image-families.md)
- [Deploy Models](../how-to/deploy-models.md), [Multi-Node Inference](../how-to/multi-node-inference.md), [PD Disaggregation](../how-to/pd-disaggregation.md)

<!-- translation-source-sha256: 80866c720eec3704f4c4c35ffc04a92217177fb0b31d16cecacdc779a5aeff4e # Deployment Launch Parameter Sources -->
<!-- translation-source-sha256: 5e99ba37c02aaf654a4b3ad3f0eca30d3c09def5f30a817d9f402eecc7c97de3 ## Single Source of Truth and Two Boundaries -->
<!-- translation-source-sha256: 95c975c07050d8ba781c9f7cda7799ad97f144a4ff19fdd8a289212d1504994c ## Layers at a Glance -->
<!-- translation-source-sha256: 6a2fcc27cc96f75659122ac47beb16d3203191e8bf1c23d8bb092637a6a6e244 ## Engine Baseline -->
<!-- translation-source-sha256: 9182790a3584e17825946558804ae1cd8b359c45ed587d55eed89ba31d31ccbf ## Parameter Dictionary -->
<!-- translation-source-sha256: 6b487cfe74683ab95af8bc04b27fb4752ce73fe017067ca7c72532f40aa10050 ## GPU/Chip Dialects -->
<!-- translation-source-sha256: 9680430205c7f1f6a48b9522e102381f0670171b97e4894feaaad023b7aa5985 ## Scheduler-Derived Values -->
<!-- translation-source-sha256: e7997618bde109c24525841c0067c10fd193e80564573262cb5a09b4d384e2ea ## Inference Template Specs -->
<!-- translation-source-sha256: b72681d90b73277c964104e81d249d128b93e6178482fbb22e8aac033ac02133 ## Topology -->
<!-- translation-source-sha256: b6b39ba843a7c8fd6f321f4105e02fbcf504073d1f642472825c08faa4db713f ### Multi-Node Peers File (`GPUWORKS_PEERS_FILE`) -->
<!-- translation-source-sha256: 0fedf9b59e4bb3f5a97769e0acbb6cb1bdbf3c687a8935194dce04dd76b173e1 ## User Input and Container Overrides -->
<!-- translation-source-sha256: a7595de5ab55ab4dc0ed406f98c2889ac44ca9472ae285c6f9b3049e14c949b4 ### Parameters -->
<!-- translation-source-sha256: 8b3dd7dcc0a4138d3c57db2875dbef13a9e45cbf8a80a38345f1a8049b8f9458 ### Command Modes -->
<!-- translation-source-sha256: 7ad5feb68f5fb7e97bea822ef9bc92dbbf3196ec6dcadbf32869b668a0561f7b ### Container Override Fields -->
<!-- translation-source-sha256: 7cc2b6f33347b0d9b66ba63c9c4a0fb6af8b5a417fdc665815d0d36051f8a48f ### `/dev/shm` Size -->
<!-- translation-source-sha256: 48a7b71d1828df37d9707175d380a78ceddfc7234253976351bd0422b9d84871 ### Locked Memory (memlock) -->
<!-- translation-source-sha256: 3b65db7c208f4f8d4a7be3cfddcddb58fef55416c35eedf80c20670ccdec8f3c ### The three-layer order of docker run arguments -->
<!-- translation-source-sha256: 799082e339ecc310746057cf4a7160f67f9001369facb4f5533c9ef02be2f413 ### Disallowed Docker Arguments -->
<!-- translation-source-sha256: 99b335171c079ef2ccccf80c3e3dd11b85713797d865528d365392695d2b9565 ## Startup Wait Limit -->
<!-- translation-source-sha256: d3a20fa47676a3d0fcf5336ad13fd98385bf4db45f58abbc09495f8793deed29 ## Environment Variable Layers -->
<!-- translation-source-sha256: 5db8a885d66eb6f5288f23c22744d0c85031fbd017e3af540a33d47061712e2d ## Variable Expansion -->
<!-- translation-source-sha256: d53ef11b886a3bc5b3251a8a1c4ad708fddcad85198e71565d60cc3318f04198 ## Agent Injection -->
<!-- translation-source-sha256: 8726e92ccab82993e82b6a6784882697af08ee5e4c185866e0f9d09e50c13d99 ## Runtime Assembly -->
<!-- translation-source-sha256: 7458cea11e4c8b0e87bcf9b6871c25b82d6f36953dc9b2116d085dc942761d5e ## Related Documentation -->
<!-- translation-localized-code-block: 87810a25cc9d7f6adbb697b3c419cb2727115525f01d20d8ba378215c02d16e7 9f5706e114d2dd907dd8185a9c7d45f2bdd0387155df33a59bb6930888f598f1 Translate explanatory comments and placeholders in runtime assembly illustration. -->
