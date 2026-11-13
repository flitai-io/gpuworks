# Choosing Features: dGPU, Sleep/Wake, Multi-Node, Replicas, and PD

After you get your first model running (see [Install GPUWorks and Deploy Your First Model](../tutorials/getting-started.md)), the next questions are usually:
should several models share one GPU, what if a model does not fit, what should you add when traffic increases, and do you need PD? These features have similar names,
but solve different problems and cannot be combined arbitrarily. This article explains each in terms of "what it solves / costs and limitations / when not to use it."
It links to the relevant how-to guides instead of repeating their procedures.

For vendor-specific conclusions, refer to the [Vendor Support Matrix](../reference/support-matrix.md). For combinations with no validation
result in either the matrix or the code, this article says "no validation result" rather than assuming they work.

## Decision Table

| Your Situation | First Choice | Alternative / Combination | Learn More |
|---|---|---|---|
| One model needs an entire GPU, and you want maximum performance from a single instance | [Exclusive full GPU](#整卡独占默认) (default) | — | [Deploy Models](../how-to/deploy-models.md) |
| Several small models must be **online simultaneously**, each with a fixed GPU memory budget | [dGPU](#dgpu显存配额) | Also enable "Sleep Capable" for each instance; see the [Combination Quick Reference](#组合速查) | [Enable and Troubleshoot dGPU](../how-to/dgpu.md) |
| Several models **take turns**, with only one or two serving at a time | [sleep/wake](#sleepwake多个模型轮流用一张卡) | Combine with dGPU (covered by the NVIDIA tutorial) | [sleep / wake](../how-to/sleep-wake.md) |
| The model does not fit on one GPU, but fits across several GPUs in one machine | [Single-node multi-GPU TP](#单机多卡-tp-与多机推理) | — | [Deploy Models](../how-to/deploy-models.md) |
| The model does not fit even across all GPUs in one machine | [Multi-node inference](#单机多卡-tp-与多机推理) | Check the [compute network](../how-to/configure-networks.md) first | [Multi-Node Inference](../how-to/multi-node-inference.md) |
| A single replica fits, but throughput is insufficient or you need to tolerate a single instance failure | [Multiple replicas](#副本数与网关路由策略) | Choose a routing policy based on traffic patterns | [Deploy Models](../how-to/deploy-models.md) |
| Prompts are long and concurrency is high, so prefill interrupts decode output | [PD disaggregation](#pd-分离) | Configure an RDMA compute network between nodes | [Deploy with PD Disaggregation](../how-to/pd-disaggregation.md) |
| You have no GPU machines or need compute capacity only briefly | [Rent from a cloud provider](#云厂商租机与自有节点) | Mix rented machines with your own nodes | [Rent Cloud GPUs](../how-to/rent-cloud-gpus.md) |
| The cluster cannot access the public internet | [Offline environment](#离线环境) | Internal mirror / HTTP file server | [Deploy in an Offline Environment](../how-to/deploy-offline.md) |

General principles:

- **Start with full GPUs, then consider sharing**. Full GPU allocation is the default and simplest option. dGPU and sleep address a shortage of GPUs and introduce
  additional limitations and vendor boundaries to check.
- **Stay within one machine if it can solve the problem**. Crossing machines adds failure points involving compute networks, firewalls, and rebuilding an entire
  group of ranks.
- **More features are not always better**. The server rejects some feature combinations outright and only issues an "unverified" warning for others.
  See the [Combination Quick Reference](#组合速查).

## Exclusive Full GPU (Default)

**What it solves**: One instance exclusively occupies one physical GPU (or TP GPUs), and the engine uses GPU memory according to `gpu_memory_utilization`.
There is no sharing layer, so behavior is closest to running the engine directly. For each vendor's full GPU deployment requirements, see the "Full GPU Deployment" column in the support matrix (PPU and Hygon depend on the provider / runtime and catalog spec).

**Costs and limitations**:

- For scheduling, a full GPU request requires that the GPU have no other allocations. The only exception is the small residual footprint of sleeping instances, which is deducted from
  available GPU memory. The request is accepted only if the engine startup reservation still fits.
- A GPU serves only one instance at a time. If the model needs less than the GPU's total memory, the remaining memory sits idle.

**When not to use it**: If several small models each need only a few GiB but each occupies an entire GPU, consider dGPU or sleep/wake.

**Learn more**: [Deploy Models, Select GPUs, and Interpret Scheduling Results](../how-to/deploy-models.md),
[Scheduling and GPU Memory Estimation](scheduling-and-estimation.md).

## dGPU (GPU Memory Quotas)

**What it solves**: Divide a physical GPU's memory among multiple instances by quota. All instances are **online simultaneously**, and each is constrained to its own
GPU memory limit, so they cannot consume one another's memory. Set the dGPU memory limit in the deployment request; the platform selects the backend according to
node hardware.

**What it is not**: It does not turn one GPU into two independent PCIe devices with separate failure domains, fixed bandwidth, or fixed compute capacity.
Compute semantics vary by vendor, as shown below (for details, see [dGPU Semantics and Boundaries](dgpu.md)).

**Vendor support boundaries** (based on the [Support Matrix](../reference/support-matrix.md),
[Ascend Support Matrix](../reference/ascend-support-matrix.md), and backends registered in `internal/accelerator`):

| Vendor | dGPU | Maturity | Requirements | Semantics and Known Boundaries |
|---|---|---|---|---|
| NVIDIA | Supported | stable | x86_64, glibc ≥ 2.17; the host driver provides `libcuda.so.1` and `libnvidia-ml.so.1` | GPU memory has a hard limit; no compute limit is provided (HAMi throttling cannot constrain vLLM; see [semantics](dgpu.md)) |
| Ascend | Supported | experimental | Required host device-share permissions; inference image requires glibc ≥ 2.33 | On 910B4 in a self-owned environment, Docker passes platform dGPU inference and over-allocation rejection validation; nerdctl passes only the node-level dGPU behavioral probe. Rented cloud machines (the Gitee environment in the matrix) lack permission to execute device-share, so they prove only detection and full GPU chat, not a dGPU pass. These results do not establish a 20/80 throughput guarantee for `aicore-quota` |
| PPU | Supported | experimental | HGGC 13.x, x86_64, and **Docker runtime only** | A 20/80 configuration behaves as weighted QoS under load (borrowing spare capacity when the other instance is idle), not hard SM partitioning |
| Hygon (DTK) | Supported | experimental | Z100 / K100_AI / BW, x86_64, Docker or runveil/udocker; requires the native helper, host libraries, and a per-GPU behavioral probe | Native vdev, one GPU per instance; CUs follow the memory ratio, with no independent compute quota. Devices reporting only K100, or other unsupported models, remain full-GPU only |
| AMD (ROCm) | Supported | experimental | x86_64, Docker, nerdctl, or runveil; requires libvrocm assets | A hard GPU memory limit in user space, with no compute cap. dGPU has been tested on MI300X (gfx942) under Docker and runveil; other models and combinations remain unverified |

The server derives capability availability as `ready` or `unavailable` (with a `reason`); the agent does not declare it independently. If the requirements are not met,
the node can still deploy with full GPUs, but requests with a dGPU memory limit fail explicitly (for example, `partition_unavailable`).
They **never silently fall back to unlimited GPU memory**.

**Costs and limitations**:

- An instance's scheduling allocation is its GPU memory limit plus the backend reservation outside that limit. On NVIDIA, engine overhead is included in the limit, and scheduling adds 512 MiB for the CUDA context. Ascend, PPU, Hygon, and AMD conservatively add the full engine reservation. The limit must cover weights, KV cache, and engine overhead; if it is too small, startup fails. See [dGPU: Isolation and Limits](dgpu.md).
- Ascend, PPU, and Hygon backends must also pass the behavioral probe run by the agent on that GPU; it reruns when assets, GPUs, or the runtime change. NVIDIA and AMD are exempt from behavioral probes but must still meet asset validation, architecture, and runtime requirements. Check that GPU's `status` and `reason`.
- Combining dGPU with PD produces an "unverified" warning during evaluation (see the [Combination Quick Reference](#组合速查)); multi-node + dGPU has no validation result.

**When not to use it**:

- You need hard compute isolation, independent failure domains, or fixed bandwidth: dGPU guarantees none of these.
- The model already needs an entire GPU.
- That GPU's dGPU capability is `unavailable`, for example because its Hygon model is unsupported, assets are missing, a probe has not passed, or its PPU runtime is not Docker.
- You only want several models to take turns on one GPU, with no need to be online simultaneously: sleep/wake is simpler and avoids fragmentation from fixed quotas.

**Learn more**: [Enable and Troubleshoot dGPU](../how-to/dgpu.md), [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md),
[dGPU Semantics and Boundaries](dgpu.md).

## Sleep/Wake: Models Take Turns on One GPU

**What it solves**: An instance you do not currently need releases GPU memory through the engine's sleep mode, while its container keeps running and its state remains
`running`. Wake it when needed. A sleeping instance retains only its own residue in the scheduling ledger (measured, or estimated from the model when it cannot be read; roughly 0.75–4 GiB); the rest of the GPU memory can be used to deploy other models.

**How it differs from dGPU**:

| | dGPU | sleep/wake |
|---|---|---|
| Online simultaneously | Yes, each with a fixed quota | No; sleeping instances do not serve requests |
| GPU memory usage | Quota remains allocated | Only the residue while sleeping (measured or estimated per instance) |
| Switching cost | None | Waking takes time (see below) |
| Dependency | Each vendor's dGPU backend | vLLM sleep mode |

**Costs and limitations**:

- Wake time: L1 (moves weights to host memory) takes one step and a few seconds, but measured it pins about 1.9 times the weight size in host RAM, and keeps it pinned after the wake. L2 (discards weights)
  reloads them from the model directory on wake: a few seconds while the model files are in page cache, tens of seconds to several minutes when they are read from disk, without consuming host RAM. Both levels discard the KV cache.
- When putting instances to sleep manually, **Wake it when a request arrives** determines what happens next: if selected, the gateway holds requests and wakes the instances automatically; otherwise, the selected instances are held asleep and requests do not wake them. HTTP 503 is returned when all sleeping replicas are held and no awake replica can serve the request. The default follows the yield policy: unselected for `never`, selected for other policies. See [sleep / wake](../how-to/sleep-wake.md#休眠时在途请求怎么办).
- After the platform puts an instance to sleep through its [yield policy](service-levels.md), requests wake it automatically. The policies are never yield automatically (`never`), yield when idle (`idle`), yield on a turn (`timeslice`), and yield anytime (`anytime`); deployments sharing GPUs take turns through sleep/wake, and a wake that does not fit can wait for room. [Scheduling Analysis](../how-to/scheduling-center.md) also supports scheduled sleep and wake.
- Automatic yielding (a policy other than `never`) requires a deployment with **Sleep Capable** enabled and an engine that supports sleep. It applies only to single-node, non-PD deployments. Multi-node deployments can be put to sleep manually but cannot use automatic yielding.
- Waking requires admission first: if a new deployment occupies the GPU, it returns 409 `WAKE_INSUFFICIENT_GPU_MEMORY`. Stop or
  sleep the occupying instance first, or choose to wait for room or to put the occupant to sleep and then wake; a GPU held by a process outside the platform is refused outright (`WAKE_GPU_HELD_OUTSIDE`).
- When putting an instance to sleep, choose "Finish in-flight requests first (recommended)" (default; finishes in-flight requests first) or "Abort in-flight requests now."
- Only **vLLM** is supported. SGLang deployments cannot declare sleep capability (rejected at creation with `SLEEP_ENGINE_UNSUPPORTED`). Enable
  "Sleep Capable" when creating the deployment. Multi-node deployments can sleep: the command goes to the leader, whose engine puts followers to sleep and wakes them together. PD
  deployments that transfer KV over a host RDMA network cannot declare sleep capability (`PD_SLEEP_RDMA_UNSUPPORTED`; see [sleep / wake](../how-to/sleep-wake.md)).
- Custom images are not probed. If an image lacks the sleep endpoint, the sleep attempt reports an error in instance events.

**Vendor boundaries**: Sleep depends on the engine, image, container runtime, and launch configuration. Apple nodes do not support engine sleep ([ADR 0044](../adr/0044-mac-engines-as-host-processes.md)). In addition to NVIDIA, the following single-GPU combinations have passed real-hardware validation, but these results do not generalize to other versions, models, or runtimes:

- **Ascend 910C**: L1 and L2 both pass with vLLM-Ascend 0.26.0rc1. When Sleep Capable is declared, the platform merges `weight_nz_mode` as `0` while preserving other `additional-config` keys. If the template environment's `PYTORCH_NPU_ALLOC_CONF` contains `expandable_segments:True`, the platform changes it to `expandable_segments:False`, keeps the other allocator settings, and evaluation/preview reports the warning `ASCEND_SLEEP_EXPANDABLE_SEGMENTS_DISABLED` (`params`: `variable`, `template_value`, `value`). Turning expandable segments off can fragment NPU memory more: the KV cache can be smaller, and a configuration with little memory headroom (for example a large model with `gpu_memory_utilization` 0.9 or higher) may run out of memory at startup or under load. If it does, lower `gpu_memory_utilization` or `max_model_len`, or turn sleep off. These large-model templates have not been validated on real hardware with sleep enabled. A nonzero NZ value the user states in extra arguments (`ASCEND_SLEEP_WEIGHT_NZ_CONFLICT`), or `expandable_segments:True` set in the deployment's own environment (`ASCEND_SLEEP_EXPANDABLE_SEGMENTS`), is rejected during creation or preview. When the command editor replaces the whole launch command, the platform neither injects nor checks these two settings.
- **PPU ZW810E**: L1 and L2 both pass with the current hggc runner (vLLM 0.23.0), which actually runs V1. The logical `/is_sleeping` state may change before the underlying operation completes, so confirm completion from the POST response, memory readings, and inference after waking.
- **Hygon K100**: L1 and L2 pass with vLLM 0.18.1 under both Docker and nerdctl. The platform injects `VLLM_CUDART_SO_PATH` for Hygon when Sleep Capable is declared; template or user environment values can still override it. The udocker/PRoot path fails the basic Torch kernel check and must not be used as evidence that this combination works.
- **AMD Instinct MI300X**: L1 and L2 pass with the gpuworks-runner ROCm entry (vLLM 0.30.0, ROCm 7.2.3): while asleep another process could allocate 150 GiB of the ~185 GiB free, and after the staged L2 wake (weight buffers, weight reload, then KV cache and a prefix-cache reset) inference is correct. On that VF `amd-smi` does not reflect the freed memory; sysfs `mem_info_vram_used` does. Ryzen AI Max (gfx1151) is not validated.

The Hygon ROCm sleep allocator also requires host-memory backing. On a K100 with 62 GiB RAM and no swap, `gpu_memory_utilization=0.9` caused the OOM killer to terminate EngineCore; lowering it to `0.25` passed. The platform currently has **no dedicated admission check for this backing memory**, so before creation you must evaluate node RAM, other instances, and system reserves yourself. Every result above is tied to the tested image and configuration; custom images are still not probed for capability.

**When not to use it**:

- Multiple models must serve simultaneously with low latency.
- Host memory cannot hold the weight copy required for L1, and you cannot accept L2 wake times.
- You use SGLang, which does not support sleep.
- You use PD disaggregation: declaring sleep capability on PD produces an "unverified" warning during evaluation.

**Learn more**: [Operate Sleep / Wake](../how-to/sleep-wake.md), [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md).

## Single-Node Multi-GPU TP and Multi-Node Inference

**Single-node multi-GPU TP**: Use this when model weights do not fit on one GPU but fit across several GPUs in one machine. The scheduler enumerates TP options
and scores interconnect quality, GPU packing compactness (smaller TP is preferred), and GPU memory fit. TP must evenly divide the number of attention heads.
GPU interconnect bandwidth depends on hardware (such as NVLink), which is the main advantage of staying on one node.

**Multi-node inference**: Distribute a deployment's ranks across multiple nodes when the model does not fit across all GPUs in one machine.

**Costs and limitations**:

- All nodes must share one compute network, or all must allow the management network to handle parameter synchronization. Otherwise, the entire group falls back to the management network (evaluation returns
  `COMPUTE_NETWORK_FALLBACK`), or the deployment is rejected. Allow bidirectional TCP between node addresses on the selected network.
- By default, TP is the smallest GPU count among the selected nodes, and PP is the node count. This keeps tensor parallelism within nodes where possible and pipeline parallelism
  across nodes. TP can also span nodes.
- A multi-node deployment starts with **1 replica** (`replicas > 1` returns `DISTRIBUTED_REPLICA_COUNT`). After creation, use "Edit replicas" in deployment details
  (`PUT /api/v1/deployments/{id}/replicas`), listing the nodes for each replica, to scale to multiple multi-node replicas. Sleep is supported.
- RDMA compute networks enforce IB transport. If the container cannot see verbs devices, startup fails immediately instead of silently falling back to TCP (a TCP fallback
  can halve throughput, which is why this is designed to fail loudly; see [ADR 0030](../adr/0030-network-objects.md)).
- **Vendor boundaries**: Compute networks come in four types: host Ethernet, host RDMA (NVIDIA, NCCL/UCX), accelerator RDMA (Ascend RoCE), and accelerator HCCS (Ascend A3/910C LingQu).
  Ascend multi-node deployments must share one accelerator RDMA or accelerator HCCS network (`COMPUTE_NETWORK_ACCELERATOR_REQUIRED`). Host RDMA compute networks cannot be used on runveil
  (`udocker` driver) nodes (`COMPUTE_NETWORK_RUNTIME`). Multi-node inference on other vendors has no validation result.
- If a node hosting one rank goes offline, the whole group fails and is rebuilt. Spanning more machines adds failure points. Once the node has been offline longer than the deployment's node-loss rescheduling wait (default 10 minutes), the platform replaces only the lost rank without spending retries; see [Reschedule Replicas on Node Loss](../how-to/node-failover.md).

**When not to use multiple nodes**: The model fits on one machine. Cross-machine deployment is appropriate only when GPU memory or compute capacity is genuinely insufficient.

**Learn more**: [Multi-Node Inference](../how-to/multi-node-inference.md), [Configure Node Networks](../how-to/configure-networks.md),
[Typical Deployment Topologies](topologies.md).

## Replica Count and Gateway Routing Policies

**Replicas**: Independently scheduled copies of the same deployment (up to 32), each consisting of one rank or a multi-node group of ranks. The gateway distributes requests among their rank-0 instances. They address insufficient throughput and single-instance
failures. Replicas do not increase the capability of an individual request (context length or per-request latency); each replica occupies its own GPU (or dGPU quota).

Set the **routing policy** on the deployment (`routing_policy` at creation, or `PUT /api/v1/deployments/{id}/routing`).
It is routing configuration, not an engine launch parameter, so **changing the policy does not restart instances** ([ADR 0023](../adr/0023-inference-routing.md)).

| Policy | Behavior | Suitable For |
|---|---|---|
| `least_request` (default) | Chooses the replica with the fewest in-flight requests as counted locally, using fresh engine queue counts and KV utilization as soft penalties; breaks ties randomly | Most scenarios with no clear relationship between requests |
| `prefix_affinity` | Tries to keep the same prompt prefix on one replica (stable hashing), spilling over to others if that replica becomes overloaded. Reads the `X-GPUWorks-Affinity-Key` request header first; it can also extract a stable key from chat messages, prompt, or input. This header is not forwarded to the engine | Many requests share a long system prompt or multi-turn conversation, and you want to reuse the prefix cache on the same replica |
| `failover` | Prefers a stable primary replica and switches only when it cannot accept requests | Using replicas as hot standbys without spreading traffic |

Key gateway behaviors:

- Connection-level failures (before receiving an upstream response) replay the request among candidate replicas; failed replicas briefly cool down. **Once an upstream response arrives
  (including 5xx), it is returned unchanged with no further retries**.
- Sleeping, starting, and non-rank-0 instances are excluded from forwarding. If no replica is available, the gateway returns 503.
- Routing uses candidate snapshots in server memory without querying the database on the request hot path. Load is not shared across servers.

**Limitations and when not to use it**:

- Multi-node deployments can only have 1 replica at creation; add more afterward through "Edit replicas." PD deployments do not use `Replicas`; instance counts for each role are set in `placement.pd`.
- Do not add replicas when GPUs are scarce and traffic is low; they waste GPU memory.
- Replicas do not speed up a single long request. Consider TP or PD instead.

**Learn more**: [Deploy Models](../how-to/deploy-models.md), [OpenAI-Compatible Gateway](../reference/openai-gateway.md),
[Deploy in Production](../how-to/deploy-production.md) (gateway capacity and metrics).

## PD Disaggregation

**What it solves**: Split inference into two groups of instances, prefill and decode. Clients still see one deployment and one service name; the gateway calls
the two groups in sequence. The greatest benefit appears with long prompts (thousands of tokens) and high concurrency, when prefill interrupts decode and makes output uneven.

**Vendor support boundaries** (based on `internal/server/pdmode` and [ADR 0028](../adr/0028-pd-disaggregated-deployment.md)):

| PD Mode | Engine / Vendor | KV Transfer | Validation Status |
|---|---|---|---|
| `vllm-nixl` | vLLM / NVIDIA | UCX (over the compute network) | Validated: same machine, cross-machine Ethernet, and multi-node within a role |
| `vllm-ascend-mooncake` | vLLM / Ascend | NPU's built-in RoCE ports (HCCL) | Validated: across two 910B4 machines; P and D NPUs must be mutually reachable on the NPU network |
| `vllm-mooncake` | vLLM 0.21 / Hygon | Mooncake transfer engine (over the compute network, RDMA or TCP) | Verified: Hygon K100, two cards in one node, TP 1, including GDN hybrid models (r4 image required); cross-node and RDMA not verified |
| `sglang-nixl` | SGLang / NVIDIA | UCX | **Unverified** |
| `sglang-ppu-ipc` | SGLang / PPU | Card-to-card IPC within one host (Mooncake NVLink transport) | Validated: two PPU ZW810E cards on one host, TP 1; P and D must be on the same node |
| `sglang-ppu-mooncake` | SGLang / PPU | Mooncake RDMA (over a host RDMA compute network) | Validated: PPU ZW810E over RoCE NICs, TP 1; nodes need `alixpu_peermem` loaded |
| `vllm-ppu-mooncake` | vLLM / PPU | Mooncake RDMA (over a host RDMA compute network) | Validated: PPU ZW810E over RoCE NICs, TP 1; nodes need `alixpu_peermem` loaded |

`sglang-ppu-ipc` moves the KV only between cards of one host, so a placement across several nodes is rejected (`PD_SAME_NODE_REQUIRED`);
it needs the PPU SGLang image with the PD patches (`hggc13.0-sglang0.5.13-r3` and later) and turns off the overlap scheduler.
`sglang-ppu-mooncake` and `vllm-ppu-mooncake` move the KV over RoCE NICs and can span nodes: the nodes must be on a host RDMA compute network (even a single host;
otherwise `PD_NETWORK_RDMA_REQUIRED`) and load the PPU peer memory module `alixpu_peermem`, which lets the NIC register the cards' memory.
A node without it shows a warning with the fix command in the **GPUDirect RDMA** column of its **Health checks**, and the Diagnose report lists it too;
a manual placement on such a node is rejected (`PD_PEER_MEMORY_MISSING`), and automatic placement skips them, returning `PD_PEER_MEMORY_NONE` when none is left.
On Hygon only the vLLM 0.21 image includes Mooncake; the 0.18.1 image cannot be used for PD.
TP rules differ by mode: `vllm-nixl` requires decode TP ≥ prefill TP, `vllm-ascend-mooncake` requires
prefill TP ≥ decode TP, and `vllm-mooncake`, `sglang-nixl`, `sglang-ppu-ipc`, and the two PPU Mooncake modes require them to be equal.

**Costs and limitations**:

- You need at least two groups of instances, prefill and decode, consuming more GPUs than a single deployment. Mode, TP, PP, and the KV loading failure policy
  cannot change after startup; clone the deployment to change them.
- With very short prompts, KV transfer overhead can outweigh the benefit. Use the average prompt length shown in the PD card on the service page to assess this.
- Across machines, KV uses the compute network: RDMA is fastest; Ethernet works but adds a KV copy for each request. P and D on the same machine transfer KV over local TCP,
  which is suitable for validation but does not represent cross-machine performance. Ascend transfers KV through the NPU's built-in RoCE ports; cross-machine transfers must use an "Accelerator RDMA" network, which declares the connectivity between accelerator ports.
- KV Offload applies only to prefill. You cannot put `--kv-transfer-config` or
  `--kv-offloading-size` in custom arguments.
- Combining PD with sleep or dGPU produces an "unverified" evaluation warning (`PD_UNVERIFIED_COMBINATION`).
- Decode in `sglang-nixl`, `sglang-ppu-ipc`, and `sglang-ppu-mooncake` cannot perform prefill independently, so fallback is unavailable and prefill cannot scale down to 0;
  `vllm-ppu-mooncake` also only returns 503 (fallback is unverified). Other modes default
  to completing the request locally on decode when prefill is unavailable (`fallback: decode`); you can also configure them to return 503 directly.
- Known defect: the platform currently does not detect an engine process exiting after an instance becomes ready (`todo.md` T26). With PD, this appears as persistent gateway fallback.

**When not to use it**: Prompts are very short; you have only one GPU or too few GPUs in one machine to divide into two groups; hardware is not NVIDIA, Ascend, PPU, or Hygon; or you want
to scale through "replica count" (PD ignores `Replicas`).

**How to confirm disaggregation is actually happening**: Once each prefill instance first becomes ready, the platform automatically runs a handoff self-check and records
`PD_HANDOFF_VERIFIED` in the timeline (transport: rdma, tcp, local, or unmeasured). If the compute network is RDMA but there is no RDMA
traffic, it records `PD_HANDOFF_NOT_RDMA`. The service page also shows PD effectiveness and fallback rates.

**Learn more**: [Deploy with PD Disaggregation](../how-to/pd-disaggregation.md), [Configure Node Networks](../how-to/configure-networks.md),
[Typical Deployment Topologies](topologies.md#f-多机推理与-pd-分离).

## Rented Cloud Machines and Your Own Nodes

**Renting from a cloud provider** addresses an immediate need for compute without buying hardware. Configure provider credentials (RunPod, CompShare,
AutoDL, Vast, ZStack) on the "External Platforms" page, list instances, and confirm GPU model/count, region, and price. Then create the instance, run SSH prechecks, and install the agent automatically.
The server stores credentials encrypted; they are not written to installation scripts or logs.

**Your own nodes** suit long-term stable operation and networks you can plan yourself. You control drivers, network adapters, and switches, so you can configure
[node networks](../how-to/configure-networks.md) (storage-sync, compute, and serving) and use operations features such as out-of-band recovery.

**Costs and limitations of renting**:

- The cloud instance must be able to access the `server_url` you enter. If the server is behind NAT, provide a stable domain name or reverse proxy address.
  **Do not use** a [temporary public address](../how-to/expose-public-address.md). If the cloud instance cannot connect outbound to the server, select
  `server_dial` during installation (the server connects to the instance's listening port).
- Available capabilities depend on the provider. For example, the rented Gitee Ascend environment in the matrix lacks device-share permissions, so dGPU is unavailable (Gitee is a test environment, not one of the providers listed above). On container-based instances without
  Docker (or NVIDIA nodes where Docker lacks NVIDIA Container Toolkit support), the agent uses runveil, which supports neither RDMA compute networks nor PPU dGPU.
- Each provider implementation determines API fields, region constraints, and cleanup. Documentation and acceptance cover only "rent machine → SSH → agent
  online → inventory." Multi-node RDMA and dGPU on cloud instances have no systematic acceptance records; use the actual detection results on the node page.
- Rented instances are billed by the provider (confirm the price before creation). After use, confirm they have been released on the External Platforms page or in the provider's console.

**When to use your own nodes**: You need a stable multi-node RDMA cluster, long-term availability, or compliance requires data to remain on the internal network.

**Learn more**: [Rent Cloud GPUs](../how-to/rent-cloud-gpus.md), [Connect, Upgrade, and Uninstall Nodes](../how-to/connect-nodes.md),
[Typical Deployment Topologies](topologies.md#g-云厂商租机接入).

## Offline Environments

**What it solves**: Install the platform, connect nodes, and deploy models even when the cluster cannot access the public internet or registries.

**What works offline and how**:

| Stage | Offline Method |
|---|---|
| Install and upgrade the control plane | Copy the release package to the target machine and run `gpuworks install`; upgrade with `gpuworks upgrade --base-url <internal-mirror-url>` |
| Connect nodes | The agent binary is embedded in the server. Nodes obtain it from the server's `/install.sh` and `/dl/`, with no public internet required |
| Inference images | Use source rules to point to an internal image registry; or run `docker save` in an online environment, place the archive on an internal HTTP file server, register its URL as the image's download location, and download it to a node from the Images page; or let nodes in a sync group copy from a node that already has the image |
| Models | Register a model directory local to a node, use an offline model package, import from an internal HTTP directory, or sync between nodes |
| Image catalog and inference catalog | The system image catalog is embedded in the server and works without internet access. Startup replays the embedded catalog only when the server build differs from the one used for the last replay; restarting the same build preserves the most recent import ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)). To update it, download the manifest JSON on a connected machine, then use "Upload manifest" on the Images page. Import an inference catalog package through the API |
| AI assistant | Point it to an internal OpenAI-compatible endpoint. Without configuration, deterministic estimation and other features remain available |

**Limitations**:

- The server does not receive or store image tar archives. Images must reach nodes through an internal image registry, an HTTP archive URL, or peer sync.
- Features requiring public internet: downloads from Hugging Face / ModelScope, pulls from public registries, renting cloud machines, and `gpuworks expose`.
- An "offline declaration" does not prove OS-level disconnection. To prove there is no public internet access, use separate network isolation or packet capture.

**How to choose**: If you already have an internal registry or mirror, point image [source rules](../how-to/configure-pull-settings.md) in pull settings to the internal address (disable origin fallback to avoid the public internet).
Likewise, point model [site rules](../how-to/configure-pull-settings.md#站点规则模型) to an internal mirror (with origin fallback disabled). If the entire cluster is offline, add the two ["Deny" rules](../how-to/configure-pull-settings.md#完全离线两条禁止规则). For nodes that need a proxy for outbound access, use "Proxy" in pull settings. If you have only an ordinary HTTP file server, follow
[Import from an HTTP File Server](../how-to/import-from-http.md) to register models and image archives.

**Learn more**: [Deploy in an Offline Environment](../how-to/deploy-offline.md), [Image Catalog and Inference Image Families](../reference/image-families.md),
[Download, Cache, and Sync Models Between Nodes](../how-to/download-and-sync-models.md),
[Configuration and Environment Variables](../reference/configuration.md) (pull proxies and outbound access).

## Combination Quick Reference

| Combination | Result | Reference |
|---|---|---|
| dGPU + sleep (same instance) | Supported; wake admission uses the full allocation of "GPU memory limit plus reservation." An NVIDIA tutorial is available | [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md) |
| dGPU + multiple replicas (same GPU) | Supported; each replica occupies its own GPU memory limit. Two replicas on one GPU are validated in the self-owned Ascend pool | [Ascend Support Matrix](../reference/ascend-support-matrix.md) |
| sleep + SGLang | Rejected (`SLEEP_ENGINE_UNSUPPORTED`) | [sleep / wake](../how-to/sleep-wake.md) |
| sleep + multi-node | Supported; the leader's engine puts followers to sleep and wakes them together | Same as above |
| sleep + PD with KV over host RDMA | Rejected (`PD_SLEEP_RDMA_UNSUPPORTED`) | Same as above |
| Multi-node + replica count > 1 | Rejected at creation (`DISTRIBUTED_REPLICA_COUNT`); add replicas afterward through "Edit replicas" | [API Reference](../reference/api.md) |
| Multi-node + RDMA compute network + runveil nodes | Rejected (`COMPUTE_NETWORK_RUNTIME`) | [Configure Node Networks](../how-to/configure-networks.md) |
| Multi-node + vendors other than NVIDIA / Ascend | No validation result (compute networks model only NVIDIA host networks and Ascend accelerator ports) | Same as above |
| PD + sleep / dGPU | Allowed, but evaluation issues an "unverified" warning | [Deploy with PD Disaggregation](../how-to/pd-disaggregation.md) |
| PD + `Replicas` | `Replicas` has no effect; instance counts are in `placement.pd` | [ADR 0028](../adr/0028-pd-disaggregated-deployment.md) |
| dGPU + PPU with a non-Docker runtime | Capability is `unavailable` | [Support Matrix](../reference/support-matrix.md) |

For combinations not listed here, use the errors and warnings returned by evaluation on the deployment page. Evaluation does not reserve resources, so you can try repeatedly.
