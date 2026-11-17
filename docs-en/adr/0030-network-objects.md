# ADR 0030: Node Networks Are Attachable Objects, With Per-Node, Per-Role Resolution Computed on Demand

**Status**: Accepted
**Date**: 2026-09-25

## Context

Three types of inter-node traffic need network interface selection: model/image synchronization between nodes (storage-sync), collective communication for multi-node inference and KV transfer for PD disaggregation (compute), and gateway-to-inference-instance traffic (serving). Previously, only global or per-node `model_sync_cidr/interface` settings existed, and multi-node inference used the node's `direct_address` directly. This approach has three problems:

- Large clusters cannot be configured in bulk by machine type or rack;
- IB/RoCE and Ethernet require entirely different NCCL/UCX configurations;
- RDMA misconfiguration produces no error, silently falling back to TCP and halving throughput. H200 PD measurements show that all KV traffic goes through eth0 when the container does not mount `/dev/infiniband`.

## Decision

1. **A network is a first-class object**: roles (storage-sync/compute/serving, with multiple selections allowed), type (ethernet/rdma), and default selectors (CIDR, interface name wildcards, HCA wildcards). RDMA no longer asks users to distinguish InfiniBand from RoCE (consolidated from three types on 2026-09-26, migration 99): the link layer is detected automatically per node port, with a notice if two link layers appear in the same network. Connectivity is determined by whether nodes share the same network object, not by type: one network object represents one fabric.
2. **Attach to nodes or node tags**: direct node attachments take precedence over tag-based attachments. If multiple networks for the same role match at the same priority, the first attached network takes effect with a notice, not an error. Node attachments can override the interface, IP, HCA, and bootstrap interface.
3. **Do not persist resolved results**: compute them each time from attachments + node tags + the interface inventory reported by the agent (`store.ResolveNetworks`). Pages, configuration delivery, scheduling, and internal tests all share this function. Store only user declarations and agent observations, not derived results, to avoid inconsistency between them.
4. **Preserve existing fallback behavior**: if there is no network or no matching interface, storage-sync uses the original settings, the compute network uses `direct_address`, and serving uses the tunnel. Do not migrate the original global `model_sync_*` settings, because a global scope cannot be expressed through node or tag attachments.
5. **RDMA compute networks enforce IB transport** (`NCCL_NET=IB`, with only one specific HCA passed to UCX) and fail loudly on misconfiguration. Multi-node deployments require all ranks to share one compute network or all to have none.
6. **Changes apply only to newly started instances**, without tracking drift.

## Consequences

- The interface inventory becomes mandatory agent-reported data: proto `NodeStatus.network`; nil indicates an old agent (retain the previous inventory).
- Internal tests produce measurements, the only derived network data that requires persistence (`network_test_runs`, retaining 10 runs per network). The controller executes tests; the API only records requests, honoring the constraint that background tasks do not start from the HTTP layer.
- The first version of compute networks covers only NVIDIA; direct RoCE from Ascend NPUs is managed by `hccn_tool` and requires separate modeling.
- The serving network does not expose engine ports directly (engines have no authentication, so exposing them lets clients in the serving subnet bypass API keys). The agent starts a minimal TLS forwarder on the serving IP: a connection first presents `HMAC(node token, instance UUID)`, then is spliced unchanged to the instance's loopback port without parsing HTTP, naturally passing through streaming and upgraded connections. Each instance has one token, computed independently by the gateway and agent, neither persisted nor displayed; the TLS key is derived from the node token, and the gateway verifies a pinned public key. The HMAC key uses the raw node token instead of the `SHA256(node token)` used by server_dial, preventing server_dial from becoming a token oracle when it signs an agent-supplied nonce. If a direct connection fails, the same request falls back to the tunnel, with a 30-second backoff for that node. The gateway's latest result (direct/tunnel + reason) is written to the node when it changes, for display in the UI.

## Alternatives Considered

- **A set of network settings per node**: unmaintainable at scale, and unable to express that nodes share the same fabric.
- **Determine connectivity only by type**: IB networks on two different fabrics have the same type but cannot communicate, causing scheduling to choose invalid combinations.
- **Persist resolved results**: any change to tags, inventory, or attachments requires invalidation and recomputation, making inconsistency with declarations likely. Resolution costs very little (node count × attachment count).

## Update (2026-09-26)

Network roles v2 revises decision 4 and the serving network fallback:

- **Use the management network as fallback**: remove `direct_address` and `model_sync_*` settings. Every node belongs to the built-in management network (the local address the agent uses to connect to the server). It handles any role without a dedicated network and can be disabled per node. Storage-sync selects a network per node pair, while compute selects one per entire group. If the group does not share a dedicated compute network, it falls back to the management network with a notice.
- **The serving network does not fall back to the tunnel**: before an instance starts, the server probes the node's serving network forwarding port once; if unreachable, it fails the instance. At runtime, requests whose direct connection fails fail immediately. The statement in Consequences above that the same request falls back to the tunnel after a direct connection failure no longer applies.
- Terminology: the Chinese term changes from “附加” (attach) to “加载” (load).

## Update (2026-09-27)

The resolution function moves from `store.ResolveNetworks` to the pure leaf package `netplan.Resolve`. Core flows call it through the `network` service; store only reads and writes network rows. For module boundaries, see [ADR 0026](0026-network-module-boundary.md).

## Update (2026-09-30): Accelerator RDMA Is the Third Type

This implements the statement in Consequences that direct RoCE from Ascend NPUs requires separate modeling. The types expand from two to three and are renamed to avoid ambiguity when listed together (migration 111): `host_ethernet`, `host_rdma`, and `accelerator_rdma`.

- **Why modeling is necessary**: measurements on two 910B4 machines show that Ascend cross-node collective communication (HCCL) uses only the accelerator cards' built-in RoCE ports. The library has no switch to use a host interface and does not fall back automatically: if the host-side handshake pairs successfully but the card pair is unreachable, the communication domain is still created, and failure occurs only at the first collective operation. Which nodes' accelerator ports are connected is a wiring fact that cannot be derived from addresses (the cards on both machines are in the same /16, but only cards with matching numbers can actually reach each other). Measurements are recorded in `artifacts/archive/2026-09-30-accelerator-parameter-network/evidence/ascend-multinode.md`.
- **Keep the fields; change the rail source**: accelerator RDMA networks reuse the RDMA network shape. CIDR selects only bootstrap; each rail is an accelerator card (`npu3`), sourced from agent-reported `accelerator_ports` (the only new data; the host kernel cannot see these ports, so only vendor tools can read them).
- **Bootstrap must use a host address**: no host process can bind to an accelerator port address, so handshakes (HCCL rootInfo, Gloo, and the master address) use a host interface, defaulting to the management network address. Containers receive `HCCL_IF_IP`, `HCCL_SOCKET_IFNAME`, and a read-only mount of `/etc/hccn.conf`: without them, HCCL can select the wrong interface among multiple host interfaces and time out while creating the communication domain.
- **Only the compute role is supported**: storage-sync and serving are host processes and likewise cannot bind to those addresses, so they are rejected at creation.
- **Required for Ascend multi-node deployments**: reject deployment when a multi-node group of Ascend nodes is not on an accelerator RDMA network, rather than letting it start and fail on the first request. PD disaggregation performs the same check based on the mode's transport.

Rejected approach: infer that traffic uses the accelerator side from the PD mode and bypass host compute network validation. This fixes only PD, not regular multi-node deployments; without a network object, there is also nowhere to record the wiring facts.

### Card-Level Connectivity Tests

Internal tests for accelerator RDMA networks add two modes: ping between matching card numbers and a full card matrix. This is the only way before deployment to know which card pair to select. Results are measurements stored in `network_test_runs`, as with other internal tests. Each card pair occupies one row of the existing per-rail details, without adding an entity.

- **Schedule in rounds**: a card can send only one ping at a time, and unreachable addresses require waiting the full timeout, so the number of rounds determines duration. Node pairs and cards are staggered through cyclic shifts, so each card acts once as source and once as target per round; the upper bound is rounds × per-round timeout, not node count or probe count. See the evidence above for measurements (different cards can run concurrently; a suspended probe can be killed and immediately retried; concurrent probes on the same card are unreliable; output is not streamed).
- **Result read path**: a full card matrix result can reach several MB, so test results are read only by the detail endpoint that displays them, no longer on every network status load.
- **The server derives conclusions when reading**: the server derives the node-pair topology and the overall run conclusion from that run's results before returning them, without storing them separately. They describe the moment of the test, not the network's current state, so they can serve only as guidance, not hard placement constraints.
- **Estimation and execution share a source**: the pre-run estimate (node pairs, rounds, probes, and maximum duration) and actual execution read the same schedule; the client does not calculate it independently.

### Source of Vendor Knowledge

Which vendors' accelerator cards have built-in network ports, which environment variables their collective communication libraries need, and which host files their containers require are declared only once, in `internal/accelerator` (`CardNetworkOf`). The `accelerator_rdma` network type is vendor-neutral; placement validation, launch assembly, and PD modes all obtain this knowledge from the same source, which the agent also uses to convert between card names and numbers.

## Update (2026-10-28): Accelerator HCCS is a fourth type

A3/910C LingQu HCCS/HIXL uses a different transport protocol and reachability
command from the RoCE card ports on 910B/910. Treating every accelerator
network as RoCE would select the wrong Mooncake path, so the vocabulary adds
`accelerator_hccs`. It reuses the agent's `accelerator_ports` inventory and
card-rail resolution, while network tests use `hccn_tool -hccs_ping` and
Mooncake launches select `hccs:device` with the required HCCS initialization
parameters.

- **The fields and automatic resolution stay the same**: HCCS carries only Compute; card addresses, state, speed, and bootstrap are inferred from the existing inventory, while the network object records the LingQu fabric boundary.
- **Launch protocol is explicit**: PD and multi-node launches inject `ASCEND_GLOBAL_RESOURCE_CONFIG`, `ACL_OP_INIT_MODE`, and `ASCEND_ENABLE_USE_FABRIC_MEM` for HCCS, and retain a read-only `/etc/hccn.conf` mount for CANN.
- **Old networks remain compatible**: `accelerator_rdma` keeps its API, card tests, and RoCE command; an older agent treats an omitted protocol as the original `-ping` behavior.

<!-- translation-source-sha256: a1b5693b86a689d692fcfb46bbacff4cf1cdc26c49d1f45057d51966593f2155 -->
