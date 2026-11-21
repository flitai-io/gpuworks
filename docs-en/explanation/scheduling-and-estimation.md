# Scheduling and GPU Memory Estimation

Deployment requests first pass feasibility filters: whether the node is online, framework support, image/model availability, dGPU capabilities, and
ability to serve traffic. The GPU planner then generates candidate GPU sets, and the scorer ranks them by topology connection quality, GPU packing compactness (smaller tp is preferred), and
GPU memory fit (capacity closer to the requirement scores higher). Every retained and rejected candidate is included in schedule_explain,
and the deployment page and API show the same explanation.

Accelerators with fault-level health do not participate in scheduling. Accelerators with warning-level health, such as GPU memory rows pending reset or a Warning from vendor tools, are eligible, but the planner first plans using only healthy GPUs.
It includes warning-level GPUs only when healthy GPUs cannot fit the workload. The selected plan then carries the `degraded_gpus_used` reason and lists the GPUs used.
For health classification and evidence collection, see [Troubleshooting](../how-to/troubleshooting.md#加速卡待复位显存故障等健康状态).

## Node Placement for Single-Node Replicas

Each replica in a single-node deployment (non-PD, `nnodes=1`) selects a node from the candidate pool **every time it starts**, rather than being bound to one node at creation.
The candidate pool consists of online nodes that satisfy the deployment's node requirements (`node_requirements`, including vendor; manual placement uses a `gpuworks.ai/node` requirement).
The controller first ranks the entire pool using a few batch reads: all nodes and their GPUs, GPUs occupied by awake instances on each node, and model file distribution.
It then performs full planning trials on only the first 8 nodes at most, using the same preview as automatic placement on the deployment page: estimation, the scheduler's assessment of the node's resource ledger,
and the template's hardware constraints. It selects the first node that fits. Regardless of pool size, each round requires only these few trials: a round takes about 80 ms with 1,000 nodes and 3,000 instances.

Ranking first considers whether a node appears to have free GPUs, estimated from GPUs occupied by awake instances. This determines trial order, not admission. It then considers the placement policy:
`spread` (the default) favors nodes with the fewest replicas of this deployment; `binpack` favors nodes with the fewest remaining free GPUs that can still fit the workload. Next come model locality
(model files already present > downloading > absent), nodes that have previously run this deployment, and finally, under `spread`, the number of free GPUs followed by node ID for deterministic ordering.
Nodes rejected in this round are ranked last for 2 minutes. If the nodes at the front of the pool cannot fit the workload, the next round tries nodes farther down instead of repeatedly trying the same ones.

**Deployments that can never fit are rejected at request time.** When creating a single-node deployment, the server groups nodes in the candidate pool, whether online or not, by hardware specification:
vendor, CPU architecture, each GPU's model/memory/compute capability, and ability to serve traffic. It selects one node per specification for an unloaded trial:
all GPUs are treated as free and present, and no disk space is consumed. If no specification can fit the deployment, the server returns 400 `DEPLOYMENT_NEVER_FITS`, including the candidate node count,
specification count, and rejection reason for the first node. Deployments that cannot fit only *right now* because GPUs are occupied, GPUs are temporarily unhealthy, disks are temporarily full, or nodes are offline are created normally and wait.
If 1,000 nodes have only three specifications, only three trials are needed. Scaling up does not perform a separate check: the deployment shape and candidate pool were already validated at creation.

Changing node requirements, through Edit beside Node requirements in deployment details (`PUT /deployments/{id}/requirements`), performs the same check.
The trial uses the deployment's saved configuration and the same code as the controller's candidate trials (`planning.DeploymentLaunch`). With the new requirements, the controller
clears its rejection history for the old pool and selects nodes from the new pool in the next round. The vendor is fixed by the image and cannot be changed.

If no node fits, the replica waits without being persisted or reserving GPUs. The `DEPLOYMENT_UNSCHEDULABLE` event explains why: an empty pool produces
`pool_empty`, with the total node count, counts of nodes that fail the requirements or are offline, and one specific requirement mismatch. If no node in the pool fits, it produces `pool_no_fit`,
with the candidate count, number of nodes tried, best-fitting node, and that node's full rejection reason. If no node in the pool fits outright but some node would fit once deployments sharing its cards give them up, the reason is `waiting_for_yield`; see [How a New Deployment Gets onto a Shared Card](../how-to/scheduling-center.md#新部署怎样放到共用的卡上).

Replicas waiting for a node retry when nodes change: a node coming online, label changes, and similar events immediately trigger rescheduling for all deployments. Each agent's routine status report,
sent once per minute with free GPUs, disk, images, and GPU health, is coalesced by the controller every 2 seconds. Only deployments that have not settled are rescheduled: those with replicas waiting for nodes, starting,
or stopping, or with multi-node or PD configurations. Deployments with the full replica count running are left alone.
A deployment that has just tried a round without finding any node that fits is not retriggered by routine reports for 20 seconds. Nodes coming online, label changes,
node requirement changes, and changes to its own instances still trigger immediate retries. This prevents large numbers of waiting deployments from occupying the controller with the same repeated rejections.

With 1,000 nodes, these reports arrive about 17 times per second. Fully rescheduling on every report would keep the controller
continuously reordering the entire cluster.

**Stopping scheduling and draining.** Before maintaining a node, such as upgrading its driver, replacing a GPU, or taking it out of service, stop scheduling through Scheduling and drain on the node page. The candidate pool lists it separately as
Cordoned and no longer starts new replicas on it. Manually placed multi-node and PD ranks assigned to it also wait with the reason `node_cordoned`; automatically placed ones move to other nodes when the replica is about to start (see [Multi-Node Inference](../how-to/multi-node-inference.md#自动放置与手动放置)). Existing replicas continue serving normally.

Draining also
moves replicas placed there by the candidate pool. For each deployment, it moves one replica at a time: first it starts a replacement replica on another node, temporarily adding one replica in the pool. Only after the replacement is running does the original replica begin draining,
finish in-flight requests, and retire. The number of serving replicas therefore never falls below the count before draining. If the pool cannot fit a replacement, the original replica stays and continues serving, with the reason displayed as usual.

Multi-node and PD replicas also move replacement-first: the replacement goes on a node with the same compute network, the same GPU architecture, and free cards, and the placement is rewritten onto it only once it runs, after which the old replica drains;
with no node to move to, the replica stays and keeps serving. Single-node deployments pinned to this node are not migrated by draining. You can change their node requirements, or use Evict now to remove them immediately
(without waiting for replacements, recreated wherever they fit, without counting toward retries). Resuming scheduling
immediately retries waiting replicas.

Later changes do not retroactively affect replica placement: node labels, GPU replacements, and network changes only affect replicas started afterward; running replicas do not move.
If a replica fails, it is recreated in the pool using the rules above. If a replica's node stays offline longer than the deployment's node-loss rescheduling wait (default 10 minutes), single-node replicas are replaced by the pool, while multi-node and PD replicas move only the lost rank; neither spends retries (see [Reschedule Replicas on Node Loss](../how-to/node-failover.md)). When scaling down, `spread` first removes
replicas from nodes with the most replicas, while `binpack` first removes replicas from nodes with the fewest. Replicas that are not ready are always removed first.
Fixed GPU indices (`placement.gpus`) take effect only when the deployment is pinned to exactly one node. Otherwise, the scheduler selects GPUs on the destination node.

Admission has a single implementation (`GPU.UsableFor`): the planner uses it to decide which candidates to accept, and
messages explaining why there are no candidates come from the same assessment. When no candidate is available, the reported reason is **the actual reason the least resource-intensive candidate was rejected**: tp does not evenly divide the head count,
insufficient GPUs, placement rule rejection, or insufficient GPU memory. It does not independently derive another "insufficient GPU memory" conclusion, which previously produced contradictory messages such as "49.0
GiB required per GPU, 72.0 GiB available per GPU." GPU memory conclusions appear only when memory is actually the bottleneck, and the numbers come from
the candidate that produced the decision, using the smallest eligible GPU on heterogeneous nodes.

Parameters such as GPU memory utilization no longer depend on an implicit "value to use when unset." At deployment creation, form fields, custom arguments, and catalog spec args
are materialized into one complete launch configuration (ADR 0020). Estimation, admission, and argv assembly all read it. GPU memory utilization outside (0,1]
is an error, not a fallback: `scheduler.Request.Validate` rejects it, and scheduling returns
`invalid_request`, indicating that a caller bypassed the write boundary.

The core GPU memory estimation logic is in pkg/estimate. It selects a KV adapter by model family (standard, MLA, sliding window, no KV, or fallback)
to calculate KV cache bytes, then adds fixed costs from the engine profile: engine overhead, context reserve, and graph reserve.
internal/server/estimate then divides the weight and multimodal encoder bytes evenly across candidate GPUs according to the tensor parallel count.
The weight bytes themselves are derived by modelmeta from the model repository's file list or safetensors index and passed as an argument; pkg/estimate
does not parse weights. Estimation divides memory only by tensor parallelism and does not model pipeline parallelism. For PP values and restrictions in multi-node deployments, see [Multi-Node Inference](../how-to/multi-node-inference.md).

The input shape for estimation comes from pkg/modelconfig. The same architectural facts can be expressed differently across repositories and are normalized when read: layer/head counts
(num_layers, decoder_layers), nesting locations for attention configuration (text_config, llm_config, talker_config,
language_config), hybrid layer types (layer_types, layers_block_type, hybrid_override_pattern,
linear_attn_config), MLA ranks, field names in Mistral's native params.json, and
Python-written Infinity/NaN values in config.json. For documents with multiple stacks, such as Qwen2.5-Omni's thinker/talker, one stack is selected before filling in missing values,
so the talker's head_dim does not leak into the thinker.

For hybrid models such as Kimi-Linear/K3, Nemotron-H, and Qwen3.5,
only attention layers that actually have K/V contribute to bytes per token; linear attention/SSM layers retain only recurrent state. Models declaring
`kv_cache: false`, such as diffusion models and pure encoders, produce a definitive "no KV cache" result, distinct from "configuration unavailable."

Shapes for prebuilt models are distributed with the catalog (see `docs/reference/inference-catalog-package.md`), because template deployments
do not register models and the server cannot access restricted repositories. Models registered by users still use the fetched config.json as the source of truth.

On the server, internal/server/estimate adds three things to the shared result: per-engine CUDA graph reserves
(1 GiB each for vLLM and SGLang, from deploy.Profile.GraphReserveBytes), a hardware dtype/quantization matrix that determines
bf16/fp8/fp4 support by accelerator arch family, and wake estimates (L1/L2 offload seconds, calculated from RAM/disk bandwidth).
Available space on the model disk comes from models path usage reported by the agent, not total root disk capacity. Unknown or invalid data is treated as
unknown/0 to avoid overcommitting.

Runtime disk space is reserved only when an image actually needs to be pulled. If a live inspect by the agent confirms that the target node already caches the image,
the image size is no longer deducted. Unknown cache status, due to an offline agent or a timeout, is conservatively treated as uncached.

When a template declares managed writable directories (`dirs`), the disk holding them (reported by the agent as `managed_dirs`) must hold the sum of the declared ceilings, the KV disk tier's ceiling (the same kind of managed directory) and the disk reserve, and also the weights to download and the image to unpack when it shares the models root's or the runtime image store's disk; otherwise the node is refused with `insufficient_managed_dir_disk` ([Managed Writable Directories](../reference/inference-catalog-spec.md#dirs-managed-writable-directories)).

Parallelism is a runtime fact: ordinary specs derive tp from the number of allocated GPUs; multi-node or special topologies express it explicitly through args. This
avoids duplicating tp/nodes in the catalog, where they could conflict with the actual GPU set, and lets the preview and actual controller share ResolveLaunch.

<!-- translation-source-sha256: b85436aa827ce6f0a9fb89a47bbf49ed50011645add01224bc0dc3cc8afd8a52 # Scheduling and GPU Memory Estimation -->
<!-- translation-source-sha256: e1a6b7e6079fb0b9e7c5d87fd7af907fbb6a8132e4cd1380e1ec230603c8b2c2 ## Node Placement for Single-Node Replicas -->
