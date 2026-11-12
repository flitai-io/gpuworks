# PD Disaggregation Deployment

**Use cases**: With long prompts (thousands of tokens) and high concurrency, prefill competes with decode and makes output unstable. Split inference into separate prefill and decode instance groups, optionally across machines.

After completing this guide, you can call the model through a single service name and confirm from handoff events and request details that prefill participated in the request.

**Prerequisites**:

- You are familiar with ordinary deployment. See [Deploy Models, Select GPUs, and Interpret Scheduling Results](deploy-models.md).
- Your hardware and engine meet the requirements of the selected PD mode: `vllm-nixl` targets NVIDIA and AMD, `vllm-ascend-mooncake` targets Ascend, `vllm-mooncake` targets Hygon DCU (it needs the Hygon vLLM 0.21 image; see [Hygon DCU](#海光-dcu)), `sglang-ppu-ipc` targets PPU (P and D must be on the same node), `sglang-ppu-mooncake` and `vllm-ppu-mooncake` target PPU with the KV over RoCE NICs (across nodes too), and `sglang-nixl` is unverified.
- PPU Mooncake modes: the nodes are on a host RDMA compute network and have `alixpu_peermem` loaded (see [PPU KV over RDMA](#ppu-经-rdma-传-kv)).
- In the NVIDIA and Hygon modes, KV travels over the compute network when P and D are on different machines; RDMA or Ethernet can be used. In Ascend mode, cross-machine deployments require a shared Accelerator RDMA or Accelerator HCCS network, and the NPUs hosting prefill and decode must be mutually reachable over that accelerator network. For configuration and validation, see [Configure Node Networks](configure-networks.md).

PD disaggregation splits inference for one model into two instance groups: prefill instances process only the prompt, and decode instances retrieve their KV and then generate tokens one by one. Externally, it remains
one deployment with one service name. Send requests to `/v1` as usual; the gateway coordinates the two instance groups. For the design and tradeoffs, see
[ADR 0028](../adr/0028-pd-disaggregated-deployment.md).

## When to Use It

- Benefits are greatest with long prompts (thousands of tokens) and high concurrency when prefill competes with decode and destabilizes output. For very short prompts, KV transfer overhead may outweigh
  the benefits. The mean prompt length on the service page's PD card can help you decide.
- For cross-machine NVIDIA deployments, compare RDMA and Ethernet handoff overhead. Do not apply the conditions that allow Ethernet to Ascend mode.
- In NVIDIA mode, P and D can use separate cards on the same node to verify functionality, but this does not represent cross-machine performance.

## Deploy

1. On the deployments page, select **PD disaggregation** under **Node Placement**, then select a PD mode. Available modes come from the engine profiles: vLLM offers `vllm-nixl` (NVIDIA, AMD),
   `vllm-ascend-mooncake` (Ascend), `vllm-mooncake` (Hygon), and `vllm-ppu-mooncake` (PPU, RDMA); SGLang offers `sglang-ppu-ipc` (PPU, same node),
   `sglang-ppu-mooncake` (PPU, RDMA), and `sglang-nixl` (unverified).

2. Choose the placement mode (see [Automatic and Manual Placement](#自动放置与手动放置)), set TP separately for prefill and decode, then add instances one by one. In manual mode select a node for each instance and optionally pin cards; in automatic mode an instance may leave its node out and the platform picks it. Each mode has TP rules: `vllm-nixl` requires
   decode TP ≥ prefill TP, `vllm-ascend-mooncake` requires the reverse, and `vllm-mooncake`, `sglang-nixl`, `sglang-ppu-ipc`, and the two PPU Mooncake modes require equal values.
   When creating a deployment, each of the P and D roles needs at least one instance.

3. To span an instance across multiple nodes, select **Add Node** under that instance. The first node is the leader (serving external requests), and the rest are followers. You must specify TP in this case,
   with TP × PP divided evenly among the nodes. The platform waits until the model and image are ready on every node before starting all ranks together.

4. Select the failure handling policy for the mode so that you do not mistake a successful request for a successful PD handoff:

   | Mode | When P Is Unavailable | When Decode Fails to Retrieve KV |
   |---|---|---|
   | `vllm-nixl`, `vllm-ascend-mooncake`, `vllm-mooncake` | Decode completes locally by default (`fallback: decode`); returning 503 (`fail`) is also available | Returns an error by default (`kv_load_failure: fail`); local recomputation (`recompute`) is also available |
   | `sglang-nixl`, `sglang-ppu-ipc`, `sglang-ppu-mooncake` | Only returning 503 is supported; decode cannot perform prefill independently | This setting is unavailable |
   | `vllm-ppu-mooncake` | Only returning 503 is supported (fallback is unverified) | This setting is unavailable |

   Mode, TP, PP, and KV load failure policy cannot be changed after startup; clone the deployment to change them. Confirm these selections before submitting.

5. In Ascend mode, KV uses the selected accelerator network: 910B/910 uses the NPUs' built-in RoCE ports, while A3/910C LingQu uses HCCS. For cross-machine deployment, these nodes must share an Accelerator RDMA or Accelerator HCCS network (see
   [Configure Node Networks](configure-networks.md)); otherwise, the deployment is rejected. The NPUs hosting prefill and decode must also be mutually reachable over that network. Use
   `hccn_tool -i <card-index> -ping -g address <peer-ip>` for RoCE or
   `hccn_tool -i <card-index> -hccs_ping -g address <peer-ip>` for HCCS. Use
   `hccn_tool -i <card-index> -lldp -g` to inspect the peer of a RoCE port.

   If two machines are directly connected by card number without a switch, P and D must select matching card numbers that are actually connected.
   First run **Card reachability · full card matrix** under the network's **Internal test** to check cabling. Deployment validation does not verify card-to-card links for you.

### PPU KV over RDMA

`sglang-ppu-mooncake` and `vllm-ppu-mooncake` use Mooncake to read and write the cards' memory directly through the host's RoCE/IB NICs, so:

- The nodes must be on a **host RDMA** compute network, even for a single-node deployment: only then do the instances get `/dev/infiniband`, unlimited locked memory, and
  `IPC_LOCK`, and the KV uses the HCAs that network declares (SGLang's `--disaggregation-ib-device`, vLLM's `device_name`). Otherwise the deployment is rejected with
  `PD_NETWORK_RDMA_REQUIRED`.
- Every node must load the PPU peer memory module `alixpu_peermem`; without it the NIC fails to register the cards' memory (Mooncake logs
  `Failed to register memory ... Bad address`). Run on the node:

  ```bash
  modprobe alixpu_peermem && echo alixpu_peermem > /etc/modules-load.d/alixpu_peermem.conf
  ```

  The second half loads it at boot. The **GPUDirect RDMA** column in the node's **Health checks** shows each card's state (not loaded is a warning; the card stays schedulable),
  and the **Diagnose** report lists it as well. A manual placement on a node without the module is rejected (`PD_PEER_MEMORY_MISSING`, with the fix command); automatic placement skips those
  nodes, and when nothing fits after skipping them returns `PD_PEER_MEMORY_NONE`, stating how many nodes were left out for it.
- vLLM's MooncakeConnector answers the prefill without handoff parameters, so the gateway composes the decoder's
  `kv_transfer_params` itself from the prefill instance's KV endpoint (`remote_engine_id`, `remote_bootstrap_addr`, and one `transfer_id` per request).
- Neither mode can be sleepable: KV cache memory registered with the NIC cannot be allocated again after waking (`PD_SLEEP_RDMA_UNSUPPORTED`).

6. The preview shows placement, GPU memory, and complete commands for each instance. KV Offload applies only to prefill; decode does not attach a KV cache layer. Do not include
   `--kv-transfer-config` or `--kv-offloading-size` in custom arguments; the platform rejects them.

7. Submit the deployment. In its details, confirm that both P and D roles reach their desired ready counts, then check the handoff self-check result in the prefill instance timeline.
   Send a request through Playground or `/v1/chat/completions`, then check the prefill instance and fallback reason in request details.
   For the vLLM modes, which use sequential handoff, you can also see the “PD prefill” phase. If a fallback reason appears, a successful response does not confirm that disaggregation took effect.

API form: `placement.pd` in `POST /api/v1/deployments`. See the [API Reference](../reference/api.md) for fields.

## Automatic and Manual Placement

A PD deployment has the same two placement modes as a single-node or multi-node one:

| Mode | Who chooses nodes and cards | How to use it |
|---|---|---|
| **Automatic** | The platform picks the nodes (at creation from free cards, and again when a unit starts and no longer fits); the scheduler picks the cards at launch and they are not written into the placement | `placement_mode: "auto"`; instances in `placement.pd` may omit `node_uuid`. Instances that name a node stay where they are and the platform fills in only the ones that do not |
| **Manual** | You. You write each instance's node (and optionally its cards) and they are pinned as written | Omit `placement_mode`, write `node_uuid` on every instance and `gpus` where needed |

PD deployments that existed before the upgrade are manual. The deployment's `placement_mode` shows the current mode, and `PUT /api/v1/deployments/{id}/placement-mode` switches between the two without restarting instances. Switching to automatic drops every instance's pinned cards, which the scheduler then picks at launch.

### How Automatic Placement Chooses Nodes

P and D exchange KV, so all instances of the PD deployment are chosen inside one compute network and one GPU architecture, and every instance without a node is picked in one go:

1. A group is chosen first: RDMA networks before Ethernet, then the group with more nodes that already hold the model file.
2. Inside the group the placement strategy allocates; PD defaults to `binpack`:
   - **Pack (`binpack`)**: when one node has the free cards for all the instances, they all go on that node and no KV crosses the network; otherwise the instances fill the nodes already in use first and use as few nodes as possible.
   - **Spread (`spread`)**: each instance takes the node with the most free cards, and prefill and decode avoid sharing a node where they can.
3. Nodes that already hold the model file come first under both strategies.

The nodes chosen first pass the ordinary PD validation (mode, TP rule, whether the compute network can carry KV). When no group can take the deployment, creation returns 409 `UNIT_PLACEMENT_NONE` with parameters saying how many nodes each check left out.

Each automatically placed instance must fit on one node. An instance with `followers` (spanning nodes) must name its nodes; a request with such an instance and no nodes returns 400 `AUTO_PLACEMENT_PD_MULTI_NODE`.

For an automatic PD request, the evaluate endpoint returns `auto_placed: true` and previews the nodes that would be picked now; creation picks again.

### Choosing Again When an Instance Starts

The nodes picked are written into the placement, and afterwards the instances are matched, their networks resolved, and node-loss rescheduling and drain applied as usual. When an instance needs to start and has no running instance, the platform plans it again with the current free cards:

- Its nodes are online and can take it: it starts as is.
- Its nodes were cordoned, no longer meet the node requirements, or would refuse the launch (another deployment holds the cards): only the ranks of that instance that do not fit move to other nodes, and the deployment's other instances are neither moved nor restarted. The timeline records `UNIT_PLACED` (`role`, `unit`, `from`, `to`, `reason`).
- Nowhere to go: it keeps its nodes, keeps waiting, and the platform looks again every minute.
- An offline node still follows the wait time in [Reschedule Replicas on Node Loss](node-failover.md), and a drain still starts the new instance before switching; both are the same for automatic and manual deployments.

### Cards Pinned by Manual Placement Are Held

When the cards a manual placement pins are held by other deployments, the instance does not use other free cards on the same node; it waits until the pinned cards are free. The scheduling reason in `DEPLOYMENT_UNSCHEDULABLE` is then `pinned_gpus_unavailable`: it gives the node, the pinned card indexes, and the deployments holding them, instead of only saying that there are not enough free whole cards. To resolve it, stop or move the deployments holding the cards, or switch this deployment to automatic placement so the platform uses free cards. When a pinned card is held by a process outside the platform, is not healthy, or is free but still does not fit (memory, sharing), the scheduler's original reason applies.

## Hygon DCU

`vllm-mooncake` uses vLLM's built-in MooncakeConnector, and the KV is moved by the Mooncake transfer engine that Hygon builds for DTK.

- **Image**: It must be the built-in Hygon vLLM 0.21 image `gpuworks/gpuworks-runner:dtk26.04-vllm0.21.0-r4`, which the built-in templates' K100 / BW launch specs use.
  In `-r1`, the connector did not report failed KV pulls: decode still returned 200 with wrong content, `kv_load_failure` did not take effect, and the failed blocks stayed in the prefix cache; r2 fixes this, so do not use r1 for PD deployments.
  `-r2` does not support the hybrid models below; `-r3` runs them but can occasionally zero blocks that are receiving remote KV, so decode answers wrongly; use r4 for hybrid models.
  The Hygon vLLM 0.18.1 image does not include Mooncake and cannot be used for PD.
- **Transfer protocol**: The platform chooses it from the compute network shared by P and D: `rdma` on a host RDMA network, and `tcp` on Ethernet, the management network, or a single host.
- **TP**: Prefill and decode must be equal.
- **GDN hybrid models** (models with linear attention or Mamba layers, such as Qwen3.5, Qwen3.6 and Qwen3.8): They need the r4 image.
  vLLM turns off its hybrid KV cache manager whenever a KV connector is configured, and these models cannot start without it, so the platform adds
  `--no-disable-hybrid-kv-cache-manager` based on the `layer_types` in the model's `config.json`; a deployment that states `--disable-hybrid-kv-cache-manager` or `--no-disable-hybrid-kv-cache-manager` itself keeps its choice.
  The KV moves as whole pages, and prefill computes up to the second-to-last prompt token while decode recomputes the last one, so decode's `local_compute` counts 1 more token per request.
  On r2 both P and D fail to start with these models (`Hybrid KV cache manager is disabled but failed to convert the KV cache specs to one unified type`).
- **Handoff**: The prefill instance starts a bootstrap server (one extra port of that instance), where each TP rank registers its address.
  Before the gateway first uses a prefill instance, it queries that server for the instance's engine id and caches it; for each request it then generates a transfer id
  and writes it into the `kv_transfer_params` sent to prefill and to decode, and decode pulls the KV from prefill accordingly.
  If the query fails or some TP rank has not registered yet, the gateway skips that prefill instance; when no instance is usable, it applies `fallback` and records the reason `prefill_no_bootstrap`.
- **Self-check**: The connector does not export transferred bytes, so the handoff self-check judges only whether decode answers normally, and records the transport as `unmeasured`.
- **Sleep**: As with NIXL, a sleepable deployment cannot use a host RDMA compute network.
- **Verification status**: Verified on Hygon K100 (two cards in one node, TP 1, Docker and runveil); cross-node, the `rdma` protocol and TP > 1 are not verified.
  GDN hybrid models are verified on K100 with Qwen3.5-0.8B and `hygon/Qwen3.8-27B-Channel-INT8-w8a8` (two cards in one node, TP 1, `tcp`): the output is identical to a deployment without PD,
  and on a failed pull `fail` returns 500 while decode keeps serving and `recompute` recomputes the correct result; not verified on BW.

## During Operation

### Status

The deployment shows **Not ready / Partially ready / Running**, derived from each role's desired and ready counts. Prefill intentionally scaled to 0 is not counted as missing.

### Self-Check

After each prefill instance first becomes ready, the platform automatically performs one handoff through the gateway and records
`PD_HANDOFF_VERIFIED` in that instance's timeline (transport is rdma, tcp, local, or unmeasured). If the compute network is RDMA but no RDMA traffic is observed, it records
`PD_HANDOFF_NOT_RDMA`. If the handoff fails, it records `PD_HANDOFF_FAILED`.

### Scale

Open **Scale roles** in deployment details to change the instances for each role (`PUT /api/v1/deployments/{id}/pd`). Removed instances drain before stopping.

- For an automatically placed deployment, new instances may omit their nodes: the platform puts them in the network and architecture the existing instances are in and picks by the placement strategy. Instances that name a node are placed as written and pass the ordinary PD validation together with the others. When no node can take a new instance, 409 `UNIT_PLACEMENT_NONE`.
- `vllm-nixl`, `vllm-ascend-mooncake`, and `vllm-mooncake` support scaling prefill to 0. Requests are then intentionally handled entirely by decode and do not count as fallback.
  This differs from still expecting P instances but falling back or returning 503 according to `fallback` when P fails.
- `sglang-nixl`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`, and `vllm-ppu-mooncake` require instances to remain for both P and D.

### Retry an Instance

Once an instance reaches the consecutive failure limit, the platform stops recreating it. Deployment details show its failure count and latest reason and provide
**Retry**, which clears only that instance's failure record and recreates it without affecting other instances. Removing an instance under **Scale roles** also clears its failure record;
adding it again starts a new count.

### Image and Network Changes

New or recreated instances start with the current image and compute network. Differences from running instances are not blocked (for example, replacing images one at a time for a
canary rollout, or cutting over a network). If the two sides are incompatible or unreachable, handoffs fail, as reflected in self-checks (`PD_HANDOFF_FAILED`) and the fallback rate. Restart the entire group if needed.

### Restart

**Restart** in the deployment details page header (`POST /api/v1/deployments/{id}/restart`, available for all deployment types) recreates all instances using the currently resolved configuration and resets each instance's
retry count. By default, instances are replaced one at a time (other instances continue serving, and the next replacement starts only after the previous instance recovers). Selecting **All at once** stops all instances and recreates them together, interrupting service during the process.

### Monitoring

The service page's PD card provides:

- **PD Effectiveness**: The proportion of decode prompt tokens received through KV transfer. The denominator includes tokens from transfer, local computation, and cache hits.
  A low value alone does not prove handoff failure. Consider the desired P count, sleep state, fallback reasons, and handoff self-check results together.
- **Fallback rate and top reasons**: The ratio of fallback requests to gateway requests, aggregated by reason. In `vllm-nixl`, `vllm-ascend-mooncake`, and `vllm-mooncake`, when P is intentionally scaled to 0 or all P instances are asleep,
  decode handles requests independently and they do not count as fallback.
- **Mean and P95 prefill phase duration** (the gateway's wait for the prefill instance), prefill instance queueing, and mean prompt length;
- **KV transfer**: Mean / P95 transfer time, mean size, throughput, failure count, and expiry count, from the vLLM NIXL connector's `vllm:nixl_*` metrics. These are per-transfer statistics,
  not statistics for individual requests.

  Failures count unsuccessful KV pulls by decode. Expiries count requests whose computed KV the prefill instance had to release because it was not retrieved before the lease expired (30 seconds by default in vLLM 0.30, continuously renewed after decode receives
  the request). This indicates that the request did not reach
  decode after prefill (the client disconnected or handing it to decode failed), or that the decode instance failed.

  The `vllm-ascend-mooncake` and `vllm-mooncake` connectors do not export these metrics, so the card shows “Not provided by engine.” For `vllm-ascend-mooncake`, individual transfer times appear only in decode instance logs
  (`KV cache transfer for request … took … ms`).

### Requests

#### View Details and Colored Bars

The PD section of request details shows the prefill instance, prefill phase, that instance's reported queue and Prefill times, and the fallback reason;
requests still in the prefill phase also appear in the in-flight table. For field definitions, see [Request Timeline and Phase Breakdown](../reference/request-timeline.md#6-pd-分离部署).

The vLLM modes all use sequential handoff. The request view lists a separate “PD prefill” segment after “Gateway”; the same segment is simply called “Prefill” in the end-to-end latency breakdown. The decode instance's reported queue time appears as
“KV transfer & queue” (vLLM, vllm-ascend, and vllm-hcu all include the wait for remote KV in queue time), and its reported Prefill appears as “First-token recompute.” In the latency breakdown,
this segment is hidden if it accounts for 1% or less of mean end-to-end latency.

#### Identify Cache Hits

Cache hits for sequential handoff requests (and `cached_tokens` returned to the client) use the prefill instance's value, because decode also counts retrieved KV as cache hits. See [OpenAI-Compatible Gateway](../reference/openai-gateway.md#用量usage).

#### View Traces

In exported OTel traces for sequential handoff, the prefill phase is a `pd.prefill` child span.
`sglang-nixl`, `sglang-ppu-ipc`, and `sglang-ppu-mooncake` call P and D concurrently, and the gateway does not wait for P's response, so there is no separate `pd.prefill` child span.

### Evaluation

By default, evaluations measure end-to-end performance through the gateway. If you specify an instance, they measure that instance directly, and results represent only one ordinary replica.

## Troubleshooting

- **High fallback rate**: Check the gateway's counted reasons (`prefill_unreachable`, `prefill_5xx`, `prefill_no_kv_params`, `prefill_no_bootstrap`, and others) and prefill instance logs.
  `prefill_no_kv_params` usually means the prefill KV connector did not take effect; `prefill_no_bootstrap` occurs only with `vllm-mooncake` and means the gateway could not query the prefill instance's
  bootstrap server, or some TP rank has not registered there yet (see [Hygon DCU](#海光-dcu)).
- **An instance was ready, but requests later keep failing or falling back**: First check instance events and logs. The agent periodically checks running containers;
  if a container exits or disappears, it is treated as failed and the controller recreates it according to the retry policy. Use **Retry** when the retry limit is reached.
  This check is not continuous HTTP health probing. If the container is still running but the engine does not respond, check logs and request errors, then restart or replace the instance through role adjustment if necessary.
