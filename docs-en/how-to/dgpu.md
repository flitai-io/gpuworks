# Enable and Troubleshoot dGPU

This guide explains how to check a GPU's available dGPU capabilities, create a deployment with a GPU memory limit, and adjust an existing deployment's quota.

**Use case**: Multiple model instances share one physical card, or a deployment with a dGPU memory limit is rejected and you need to investigate why.

**Prerequisites**:

- The node is connected and reports GPUs (see [Connect Nodes](connect-nodes.md)).
- NVIDIA: x86_64, glibc ≥ 2.17, and shared libraries provided by the host driver (`libcuda.so.1`, `libnvidia-ml.so.1`).
- Ascend: host device-share permissions meet vcann-rt requirements, and the selected inference image provides glibc ≥ 2.33. Being able to connect the node and run on a whole card does not mean the host permits dGPU device sharing.
- PPU: quotas combine a GPU memory limit with weighted QoS under contention. A 20/80 configuration specifies relative throughput weights under contention, not hard SM partitions.
- AMD: x86_64, Docker, nerdctl or runveil, rocm-slicing-shim (libvrocm.so) enforces a hard GPU memory limit; there is no compute (SM/CU) limit. Only MI300X (gfx942) has been tested so far.
- Hygon Z100 / K100_AI / BW: experimental, with Docker or runveil/udocker on x86_64 and one GPU per instance. Install the native helper and host DTK/HYHAL/DCGM libraries, and pass the per-GPU probe. CUs follow the memory ratio; there is no independent compute quota. A report containing only the K100 family, or another unsupported model, remains full-GPU only. PRoot provides neither a separate cgroup nor kernel-enforced read-only mounts, so it does not establish security isolation against hostile programs. See [Hygon Z100 / K100_AI / BW dGPU](../reference/support-matrix.md#海光-z100--k100_ai--bw-dgpu).
- For a complete walkthrough first, see [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md); for semantics and boundaries, see [What dGPU Is and Is Not](../explanation/dgpu.md).

For each vendor's supported scope, see the [Support Matrix](../reference/support-matrix.md).

## Check Capabilities and Deploy

1. Open **GPU Devices** and check the **dGPU** capability and reason in the target GPU's details. Only GPUs with a status of `ready` can run newly created instances with quotas.
2. When [deploying a model](deploy-models.md), enter the **dGPU Memory (GiB)** limit, check the candidate nodes and GPU memory estimate, and submit. The platform selects a ready backend based on the node hardware: HAMi-core for NVIDIA, vcann-rt for Ascend, HGGC for PPU, libvrocm for AMD, and native vdev for Hygon Z100 / K100_AI / BW. You do not need to specify one manually.
3. After the instance starts, return to **GPU Devices → dGPU** and confirm the slice's deployment, node, and GPU memory limit. To verify that two models share one card, follow the request checks in [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md).

The `status` and `reason` returned on the page determine capability:

- `ready`: All runtime, dGPU asset, and per-GPU probe requirements for that backend are met, so instances with quotas can be created; NVIDIA and AMD are exempt from behavioral probes.
- `unavailable`: At least one requirement is not met. `reason` explains why, such as an agent version too old to report a probe,
  missing dGPU assets, a runtime that has not verified container inference, or a pending or failed probe.

| Unavailability Reason | Action |
|---|---|
| Agent version is too old | [Upgrade the agent](upgrade-and-compatibility.md) and wait for it to report capabilities again |
| Missing dGPU assets | Check the node installation and agent logs for the assets listed in the reason |
| Probe is pending | Wait for the probe result, then check this GPU's status again |
| Runtime verification or probe failed | Check the reason and agent logs, resolve the issue, and confirm that the status returns to `ready`; alternatively, select a node that is already ready |

Backends that run the behavioral probe (every backend except NVIDIA and AMD, whose readiness rests on the embedded asset alone) also have a separate `probe_status` (`passed`/`skipped`/`failed`) for diagnosing the dGPU probe's own
result. It is not equivalent to `status` above.

Switching to a whole-card deployment when the status is `unavailable` is not evidence that dGPU verification passed.

## Adjust dGPU Quotas

This action changes the quota of the **deployment that owns the slice**. It supports only single-node deployments with an existing dGPU quota. Whole-card, multi-node, and PD deployments cannot be adjusted here.

1. Find the slice under **GPU Devices → dGPU**, confirm its deployment, and click **Change quota**.
2. Enter the new GPU memory limit and save. The API is `PUT /api/v1/deployments/{id}/dgpu`, with an example request of `{"vram_limit_gib": 12}`.
3. Check the counts of instances updated immediately and those awaiting restart. Instances shown on the GPU page as **X GiB after restart** still use the original limit; handle them according to the table below. A successful save alone does not mean all instances have switched.

After saving:

- Instances subsequently started by the deployment start with the new limit.
- Running instances that support safe live adjustment change their limits immediately: scheduling accounting uses the new limit, and HAMi-core enforces it from the next allocation without restarting the instance.
- Whether the engine can use the new limit depends on the engine: vLLM and SGLang size their KV caches at startup based on the limit, so they use the new limit only after you **restart the instances** on the deployment page; engines that allocate GPU memory on demand use it immediately.

The following instances keep their original limits and use the new limit only after restarting (the GPU page shows “X GiB after restart”, and the returned `pending` lists each instance and its reason):

| `reason` | Condition |
|---|---|
| `backend` | The backend requires an instance restart to apply the new limit (Ascend vcann-rt, PPU ppu-slicing-shim, AMD rocm-slicing-shim, Hygon Z100 / K100_AI / BW native vdev) |
| `in_use` | When lowering the limit, the instance already uses more GPU memory than the new limit—lowering it immediately would cause the engine's next allocation to fail and the engine to exit |
| `starting` / `asleep` | When lowering the limit, the instance is starting or asleep and may need more GPU memory later than it does now |
| `no_usage` | When lowering the limit, the instance has not reported GPU memory usage |

Rejections (the original quota remains unchanged):

| Code | Meaning and Action |
|---|---|
| `DEPLOYMENT_NEVER_FITS` | No node can run this deployment under the new limit (for example, it exceeds card capacity); this is the same check used at creation |
| `DGPU_QUOTA_NO_ROOM` | When raising the limit, the card hosting the instance cannot fit the new slice after excluding that instance's own allocation (`params` provides the node, card index, and available and required GiB); lower the limit or free space on this card first |
| `DGPU_QUOTA_NOT_PARTITIONED` | The deployment uses whole cards and has no dGPU quota |
| `DGPU_QUOTA_NOT_EDITABLE` | Multi-node or PD deployments place each rank separately; their quotas cannot be changed here |
| `DGPU_QUOTA_INVALID` | The limit is not a positive number |

The deployment timeline records “dGPU memory limit A → B” with the counts of instances updated immediately and those awaiting restart. When an instance exits because usage reaches its limit, the failure reason is
`dGPU memory limit reached on GPU N: the engine needed X GiB, its limit is Y GiB`.
