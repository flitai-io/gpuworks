# Deployment Launch Configuration Semantics

Every parameter, environment variable, mount, and command on the deployment page ultimately becomes one **launch configuration**, and instance startup reads only that configuration. This article explains how it is built, what "not set," "clear," and "conflict" mean, the three command modes, and how existing deployments interpret these settings. For procedures, see [Deploy Models and Interpret Scheduling Results](../how-to/deploy-models.md).

## One Parameter Dictionary, One Materialization Step

### What It Is

Launch configuration has a single source of truth: the server's **parameter dictionary**. Each dictionary entry represents a parameter and holds its key, type, control, default value, aliases, and flag for each engine. The server supplies the frontend parameter table; it is not hardcoded.

A request can express the same configuration in three ways: typed fields (form controls), free-text `extra_args`, and template (catalog spec) args. When a request enters the server, a single materialization step merges them into one typed configuration. The system has no second internal representation after this step.

Estimation, scheduling, command previews, and instance startup all read this configuration, so the values you see, the values used for GPU memory estimation, and the values passed to the engine agree (for design motivation, see [ADR 0020](../adr/0020-launch-config-single-source.md)).

### Merge Precedence and Defaults

When a parameter is specified in multiple places, precedence is: **explicit fields > `extra_args` > template args > dictionary defaults**.

The server applies dictionary defaults centrally during materialization. An empty form and a default payload produce the same command line, so unchanged controls no longer send default values. Defaults are only fallbacks during materialization and never override an explicit value from any source. Defaults are not filled in when:

- The parameter is cleared (see below).
- The current launch entry does not accept it: the form hides the control, so its default is not considered user-confirmed input.
- Another parameter it depends on is not enabled (for example, companion parameters for a KV tier are inactive when that tier is disabled).

### Flags Inside and Outside the Dictionary

- Put flags outside the dictionary in "Custom Arguments" (`extra_args`); they pass through to the command line unchanged.
- If a flag in the dictionary appears in `extra_args`, materialization moves it into the corresponding typed field and echoes it as `moved` in the evaluation response. The page shows which keys were moved; only parts unknown to the dictionary remain in the `extra_args` text.

## Not Set and Clear

| State | Meaning | Effect on the Command Line |
|---|---|---|
| Not set | No source specifies the key | Emits no flag; the engine's own default behavior applies |
| Explicitly disabled | A boolean parameter explicitly specifies the disabled value | Emits the disabling flag if the parameter dictionary defines one for that parameter |
| Cleared | The request lists the key in `quick_params_unset` | Removes the key from all layers (including template args); the template value is not filled back in |

Key points:

- **Not set means "the user has made no choice; the engine default applies."** The server does not invent values for the engine. Only the dictionary defaults layer fills in values in new requests, as described above.
- **Explicitly choosing "Not set" is different from disabling a parameter or setting an empty string**: a key that is not set is not emitted to the engine, leaving the engine to determine default behavior. "Unset" in the log-level control has this meaning (see [SGLang Logging Injection](sglang-logging.md)).
- A **clear marker** removes a parameter provided by a template, such as a parser inherited from a template. Once cleared, the template value is not filled back in until you enter a value again. Unknown keys in `quick_params_unset` are rejected.

## How Conflicts Are Handled

When the same key is specified in two places, it is a conflict only if the values differ. Specifying the same value twice is merely duplication and does not cause an error.

| Error Code | When It Occurs | How to Fix It |
|---|---|---|
| `LAUNCH_CONFIG_CONFLICT` | The same key has different values in a field and in `extra_args`. The response includes `key`, `flag`, and both values | Keep only one of the conflicting entries |
| `LAUNCH_CONFIG_UNSET_CONFLICT` | The request both lists a key for clearing (`quick_params_unset`) and supplies it in `quick_params` or `extra_args` | Remove the value or cancel clearing. Rejecting this ambiguity prevents a disabled layer from silently swallowing a manually entered value |

Values explicitly specified in the current request are also rejected if they fall outside the dictionary's bounds. Only values specified in the current request are checked: template args are controlled catalog content, and existing deployments predate these rules, so neither is rejected on this basis.

## Parameters Left to the Engine

For some parameters, the platform determines that the current launch might not recognize them (the entry program or the engine version in the image does not accept the flag). In that case, the platform **does not fill in a default for you** and leaves the decision to the engine. Because this determination can be wrong, the parameter remains visible and can still be enabled or disabled. Explicitly specified values are emitted as usual and are not rejected.

The form shows these parameters as "Auto": toggles have three states, "Auto / Enabled / Disabled," and sliders add "Auto / Specify." Selecting "Auto" omits the parameter, matching the command exactly. The evaluation response and template details list these keys in `engine_decides_param_keys`.

The engine adapter makes this determination from the final launch arguments and the engine version in the image. The version comes from the image catalog record, falling back to the image tag; if the request does not specify an image, it uses the image selected from the catalog for the target node or vendor:

- SGLang `sglang serve --model-type diffusion` runs the diffusion server, which accepts only a small subset of parameters shared with the LLM server and exits with `unrecognized arguments` for all others. LLM parameters such as `gpu_memory_utilization` (`--mem-fraction-static`) are left to the engine. Defaults apply to `enable_cache_report` starting in 0.5.18 and to `enable_metrics` starting in 0.5.21; unknown versions leave them to the engine. `log_level`, `quantization`, `trust_remote_code`, and `data_parallel_size` work as usual.
- When `sglang serve` omits `--model-type`, SGLang selects the server itself from the model directory (`model_index.json` or the diffusion registry). The platform cannot determine which one, so it applies only defaults accepted by both servers.
- If the entry is not a SGLang program recognized by the platform, `enable_metrics` is left to the engine.
- vLLM's `enable_request_metrics` (`--enable-per-request-metrics`) is enabled by default only when the image indicates vLLM 0.25.0 or later and the arguments do not contain `--disable-log-stats`.
- vLLM launches with `--omni` do not apply the `gpu_memory_utilization` default: vllm-omni deploy configuration assigns GPU memory separately to each stage (for example, Fish S2-Pro uses 0.6 + 0.1), whereas one command-line value would override all stages. Admission still accounts using the dictionary default.

Existing deployments are unaffected by defaults.

For topology validation of vLLM launches with `--omni`, GPU count is calculated as TP × PP × Ulysses (`--usp`) × Ring (`--ring`) × CFG parallelism (`--cfg-parallel-size`).

## The Three Command Modes

Command mode selects **how to interpret the launch command** for a deployment. The default is `engine`; `image-entry` and `manual` must be explicit choices for this deployment. Changing the image or switching form views does not change the selected mode (see [ADR 0022](../adr/0022-template-owned-deployment-launch.md); for the origins of the three modes, see [ADR 0013](../adr/0013-image-command-mode.md)).

| Mode | Who Builds the Command | Suitable For |
|---|---|---|
| `engine` (engine-managed, default) | The platform generates the complete command through the engine adapter and ignores the image entrypoint | System images and configurations in the catalog |
| `image-entry` | Keeps the image entrypoint; the platform only appends engine arguments as cmd | Community images that use entrypoint as a launcher |
| `manual` | Your command text is the source of truth; engine parameters are no longer injected | Dedicated images with fixed commands, or experts who need complete control |

### Manual Is the Only Way to Take Over the Complete Command

`manual` is the only mode that bypasses the parameter dictionary. Its boundaries are:

- Command text is the source of truth; **the parameter table no longer controls the manual command**. Estimation does not apply, and the page states this explicitly. Do not treat capacity calculations in the form as the manual command's actual capacity.
- Resource placement remains editable: the form still determines nodes, GPUs, tensor parallelism, and the dGPU memory limit.
- The platform still rewrites any existing `--port` in the command to the port actually allocated to this deployment. If an engine alias is configured, it also rewrites `--served-model-name`.
- A manual script can choose whether to include preparation steps. See [SGLang Logging Injection · Take Control Yourself](sglang-logging.md#怎么自己接管).

`engine` and `image-entry` are not exceptions: all their engine parameters come from the single launch configuration.

### Ports and Platform Variables in a Manual Command

A manual command writes the values assigned to this launch with platform variables carrying the `GPUWORKS_` prefix; the server replaces them with the actual values before the launch. The ones a single-node command uses most:

| Variable | Value |
|---|---|
| `${GPUWORKS_API_PORT}` | The port the platform reserves for this instance, which the gateway and the health check dial |
| `${GPUWORKS_API_HOST}` | The address the server should bind |
| `${GPUWORKS_MODEL_PATH}` | Where the primary weights are mounted in the container, `/model` by default |
| `${GPUWORKS_TP_SIZE}` | The deployment's tensor parallel size, as the form states it |

For the full vocabulary and when each variable exists, see [Inference Catalog Syntax · Variable Expansion](../reference/inference-catalog-spec.md#变量展开). A single-node vLLM manual command can read:

```sh
exec vllm serve ${GPUWORKS_MODEL_PATH} \
  --host ${GPUWORKS_API_HOST} --port ${GPUWORKS_API_PORT} \
  --tensor-parallel-size ${GPUWORKS_TP_SIZE}
```

- **Always state the port.** Without `--port`, the engine listens on its own default port (8000 for vLLM, 30000 for SGLang) while the health check and the gateway dial the port the platform assigned: the instance stays starting and the gateway answers `MODEL_NOT_READY`. Evaluate warns about such a command with `MANUAL_COMMAND_PORT_UNSTATED`; headless ranks serve no API and are not checked.
- **A hard-coded port number is rewritten.** When the port number is already taken on the node that serves the API (another instance on the node that is not stopped holds it, or the agent reported a listener on it), evaluate warns with `MANUAL_COMMAND_PORT_TAKEN`; only ports inside the engine port range can be seen as taken. Both are warnings, not refusals: the create still succeeds, and the deploy page shows them under the command editor.

### Manual Commands for Multi-Node and PD Deployments

A deployment stores one manual command, and every rank of a multi-node group and every PD instance runs it; platform variables express how the ranks differ:

- Write names **with the `GPUWORKS_` prefix**, such as `${GPUWORKS_NODE_RANK}`, in the manual command. Before each rank starts, the server replaces them with that rank's values and only then hands the script to the shell. For the variable vocabulary, see [Inference Catalog Syntax · Variable Expansion](../reference/inference-catalog-spec.md#变量展开).
- A manual command recognizes prefixed names only. Unprefixed `${MODEL_PATH}` and `${API_PORT}`, as well as the script's own variables, are left unchanged for the shell, so existing single-node manual commands stay exactly as written; a prefixed name this launch does not state (`${GPUWORKS_MASTER_ADDR}` on a single node) is left unchanged too.
- A manual launch of a multi-node rank or a PD instance **keeps the system environment layer** (facts known only per rank and per node, such as `VLLM_HOST_IP`, compute-network interfaces, and the PD side channel), layered as in `engine` mode: system → template → user; values the script `export`s itself take effect last. A single-node, non-PD manual launch still has no system layer.
- For a multi-node or PD deployment, **Edit Command Text** on the deployment page starts the editor from a script draft the server generates: one engine command per class of rank (PD role × headless or not), inside a `case` that branches on `${GPUWORKS_PD_ROLE}/${GPUWORKS_HEADLESS}` (only `${GPUWORKS_HEADLESS}` without PD). The draft contains no preparation script or environment variables; the platform still runs the preparation per rank. When ranks of one class differ in something no variable carries (for example the RDMA device names of two nodes), there is no draft, the evaluation reports a `MANUAL_DRAFT_UNAVAILABLE` warning, and the command cannot be edited from the form.
- Evaluation and creation refuse a manual command that cannot tell ranks apart: a multi-node command must reference `${GPUWORKS_NODE_RANK}` or `${GPUWORKS_HEADLESS}`, and a PD command must reference `${GPUWORKS_PD_ROLE}` (both apply when a PD instance spans several nodes); otherwise they return `MANUAL_COMMAND_RANK_BLIND`. The rule does not apply to deployments already stored, which keep launching as before.
- Only a manual command is expanded per rank in a multi-node or PD deployment. A Cmd override in `engine` or `image-entry` mode would reach every rank as written, so evaluation and creation refuse it with `COMMAND_OVERRIDE_RANK_BLIND`; deployments already stored are not affected either.

## Configured Environment and Image Environment

An image is a runtime artifact. The engine version, framework, architecture, download locations, and actual OCI ENTRYPOINT/CMD/ENV inside the image are all **image facts**. They are distinct from the launch inputs configured for this deployment:

- Until a node inspects the image, its built-in OCI environment, entrypoint, and running process state remain "unknown"; they are not guessed.
- "Configured Environment Variables" in the deployment preview contains only environment variables produced by the current configuration. **It cannot represent the final process environment**, which also includes the image's built-in environment.
- The SGLang adapter code generates the environment and preparation script needed for SGLang logging. The page displays the result resolved by the server.

## The Image Catalog Carries No Launch Configuration

The deployment's launch configuration is determined by materializing and resolving a launch plan from the parameter dictionary, selected template, and current user input. It is written into deployment fields and the catalog selection snapshot. The image catalog records only how to obtain the image (reference, download locations, framework, and version); it provides no default configuration that rewrites the deployment. As a result, changing the image catalog after evaluation does not change the command when the same deployment is rebuilt ([ADR 0022](../adr/0022-template-owned-deployment-launch.md), [ADR 0031](../adr/0031-image-catalog-carries-no-launch-config.md)).

- All deployments follow the same semantics: command mode defaults to `engine`, while `image-entry` and `manual` can only be selected explicitly by the deployment. Residual `launch_contract` markers in early snapshots are ignored.
- The deployment's own container settings (environment variables, removed environment variables, mounts, docker arguments, `/dev/shm` size, entrypoint, and command) all belong to the "Container overrides" layer. Docker arguments that duplicate platform-managed items or dedicated fields (such as `--network`, `--shm-size`, `-e`, `-v`, and `--entrypoint`) are rejected instead of silently overriding them.
- For the source layer of each item and override precedence, see [Deployment Launch Parameter Sources](../reference/deployment-launch-sources.md).

<!-- translation-source-sha256: 7d5d71d88bf724aa1ce6a666e9c3242e5a6a7bf7635c3a8c0ddbaa4da6b94747 # Deployment Launch Configuration Semantics -->
<!-- translation-source-sha256: d5fceeb2d695dc9f6922409cb0d49f4e539270b36b338636acf4c4db8d828079 ## One Parameter Dictionary, One Materialization Step -->
<!-- translation-source-sha256: 03b7b41c2b3bde9d8a12b3f67b8600a2c5c7c396e36ba402b20b09a56ed5a74d ### What It Is -->
<!-- translation-source-sha256: 4bb7df25a0eda5159e47134086aaa472359d6e59817f564370c7ad1d116ec6c3 ### Merge Precedence and Defaults -->
<!-- translation-source-sha256: 4c09562f6659a1bd947098c1edeebfe6a21aff37bd8de17c4aeabd9424bd8265 ### Flags Inside and Outside the Dictionary -->
<!-- translation-source-sha256: e52519412a630c8ad4885bd0ca4be73e5974213a8f0610c30f818ef401f6173b ## Not Set and Clear -->
<!-- translation-source-sha256: d2730fa05cfe55d55629c5f7e19e215296c2e50927523d6e0d00328fba5099c9 ## How Conflicts Are Handled -->
<!-- translation-source-sha256: 23655fc4b8a0b7940d08317bd21f9d003819164b91c3ea431a8c4bea59d172d0 ## Parameters Left to the Engine -->
<!-- translation-source-sha256: fd91912ae3a4712ca5e41d49ca964ff7b7326d8d6a9ad16c3fdbf96b1ab9ed05 ## The Three Command Modes -->
<!-- translation-source-sha256: 374ada004f44f962f0986cc7dd81c7a9627036a26ae654772e7d5e63dc36687d ### Manual Is the Only Way to Take Over the Complete Command -->
<!-- translation-source-sha256: 625d97afe2584ee2fdf816cf302eb94e9b66e165cf71b922467875d115e625d2 ### Ports and Platform Variables in a Manual Command -->
<!-- translation-source-sha256: 71995a467d55f734f2196e6946ed49953054a23909052a712bb62382067ff313 ### Manual Commands for Multi-Node and PD Deployments -->
<!-- translation-source-sha256: e15ec7f5155bb2bbd0be97c1c445d0daee9fe90c5001f80f8573d189fc0670b4 ## Configured Environment and Image Environment -->
<!-- translation-source-sha256: 13bbc4704a862ca8d5fb0060a6509f670cb73adad7d647d7a71376b34627ab92 ## The Image Catalog Carries No Launch Configuration -->
