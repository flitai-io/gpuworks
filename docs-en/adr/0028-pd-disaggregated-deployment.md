# ADR 0028: PD Disaggregation Uses One Deployment With Instance Roles and Two Stages Orchestrated by the Existing Gateway

Date: 2026-09-29 · Status: Accepted

Based on: [PD Disaggregation Plan](../../artifacts/archive/2026-09-28-pd-disaggregation/plan.md) (decisions D1–D16),
[Phase 0 Hardware Verification](../../artifacts/archive/2026-09-28-pd-disaggregation/evidence/phase0-verification.md), and
[Platform Hardware Acceptance](../../artifacts/archive/2026-09-28-pd-disaggregation/evidence/platform-acceptance.md).

## Decision

**One deployment, with roles on instances.** A PD deployment remains one row in `deployments`; `instances` gains a string column, `role` (empty for regular deployments).
The instance count, placement, and TP for each role go in the `pd` object of the existing `Deployment.Placement` JSON:

```json
{"pd": {
  "mode": "vllm-nixl",
  "roles": {
    "prefill": {"tp": 1, "units": [{"node_uuid": "n1", "gpus": [0]}, {"node_uuid": "n2"}]},
    "decode":  {"tp": 2, "units": [{"node_uuid": "n3", "gpus": [0, 1]}]}
  },
  "kv_load_failure": "fail",
  "fallback": "decode"
}}
```

- One `unit` is one instance of that role; if `gpus` is omitted, scheduling selects cards on that node. The instance-to-unit mapping is not persisted. It is
  derived from the role, node, cards, and launch record (`planning.MatchUnits`, shared by all deployment forms; see ADR 0029): running instances match first, then instances in creation order. Unmatched units
  are missing instances; active instances not claimed by a unit are draining. The controller and gateway use the same derivation.
- A role can include `pp`; a unit can include `followers: [{node_uuid, gpus?}]`, making the instance a multi-node group: the leader (rank 0) runs on the unit's
  node and serves requests, while each follower is a headless rank on another node. TP × PP is divided evenly across ranks. Multi-node units
  must specify the role's TP; the ranks of one unit cannot run on the same node.
- `Deployment.Replicas` does not apply to PD deployments; `NodeID` points to the first decode unit's node, and `NodeIDs` is the deduplicated list of all unit nodes
  (including follower nodes), reused for model and image readiness checks. The deployment row's `NNodes` is always 1: role instances span nodes,
  not the deployment itself.
- `kv_load_failure` is the engine-level KV load failure policy (`fail` by default; `recompute` is optional); `fallback` is the gateway's
  behavior when P is unavailable (`decode` by default: fall back to local completion on D; `fail`: return a retryable 503).

**The PD mode catalog is the sole source of rules.** The pure leaf package `internal/server/pdmode` declares each mode: engine, role names, each role's KV connector
role value, wire protocol, TP direction rule, ports required per instance, whether the same vendor is required, and the role from which KV transfer metrics are read. Platform code hardcodes
no rules beyond "prefill/decode". There are currently four modes:

| Mode | Wire Protocol | TP Rule | Transport | Hardware Verification |
|---|---|---|---|---|
| `vllm-nixl` | Sequential, two-stage | decode ≥ prefill | UCX | Verified (same node, cross-node Ethernet, and multiple nodes within a role) |
| `sglang-nixl` | Concurrent dispatch to both (`bootstrap_host/port/room`) | Equal | UCX | Not verified |
| `vllm-ascend-mooncake` | Sequential, two-stage | prefill ≥ decode | HCCL (RoCE built into the NPU; mounts `/etc/hccn.conf`) | Verified (cross-node; P and D NPUs must reach each other on the NPU network) |
| `vllm-mooncake` | Sequential, two-stage; the gateway generates the handoff parameters (prefill returns no `kv_transfer_params`; the gateway reads the engine id from its bootstrap server) | Equal | Mooncake transfer engine (Hygon DTK build, RDMA or TCP) | Verified (K100, two cards in one node, TP 1) |

Violating a TP rule returns the corresponding error code. Engine presets expose `pd_modes` through `pdmode.ForEngine`, and the frontend uses them to offer available modes.

**Validation has one implementation.** One function in `planning` parses and validates the `pd` object (all roles present, nonempty units, no overlapping cards, TP rules, a limit
of 32 per role, and rank partitioning). Evaluation, creation, scaling, and the controller share it.

**The engine adapter injects launch arguments by role.** The vLLM adapter generates `--kv-transfer-config` at a single assembly point: when a PD connector and KV offload
(CPU tier or disk tier) coexist, they are combined into a `MultiConnector`. Phase 0 measurements show that when `--kv-offloading-size` and NIXL's
`--kv-transfer-config` are both provided, the engine silently drops NIXL, so the CPU tier must also use this composition under PD. Environment variables:
`VLLM_NIXL_SIDE_CHANNEL_HOST` is the node's address on the compute network, and `VLLM_NIXL_SIDE_CHANNEL_PORT` is the allocated port.
`UCX_NET_DEVICES` uses the first HCA on an RDMA network, or the compute network interface on an Ethernet network, with
`UCX_TCP_BRIDGE_ENABLE=y` also set (UCX filters bridges by default, so specifying a bridge causes NIXL backend creation to fail). Container arguments injected for an RDMA network match those for multi-node
inference, and `UCX_TLS` is restricted to RDMA and CUDA transports so misconfiguration causes startup to fail. `vllm-nixl` instances also get
`VLLM_SSM_CONV_STATE_LAYOUT=DS`: NixlConnector transfers the conv state of Mamba-style layers (gated delta net, KDA, short conv) only in the dim-first
layout and refuses to start otherwise, while the variable has no effect on models without such layers; a value the template or deployment sets wins. All PD instances use host networking.

**Allocate all ports once and persist them all.** The controller allocates an instance's API port and side-channel port block together and writes all of them to
`Instance.Ports`; subsequent allocations avoid those ports.

**Ports and launch records.** A multi-node unit's leader receives an additional collective communication port. At startup, each PD instance records the facts its peers need in
`schedule_expl.pd` (`planning.PDRecord`: role, KV endpoint address, and ports; leaders also include the collective communication port). The leader's unit is recorded in `schedule_expl.unit` (ADR 0029).
Followers and the gateway both read from this record; there is no separate copy.

**Reconciliation.** Roles do not fail together: failure of one instance rebuilds only that instance. Additions and rebuilds use the currently resolved compute network and image, without blocking because they differ
from running members: operators may be rolling out an image or cutting over a network. Handoff self-checks and fallback rates indicate whether the two sides can still pair. `POST /deployments/{id}/restart`
manually restarts the entire group (removing failed attempts as well and recalculating the retry budget).
P and D failure retry budgets are counted separately per unit. Within a multi-node unit, the existing multi-node inference semantics apply: if a follower fails or stays offline beyond the grace period, the leader
is marked failed and all followers are removed. The next round rebuilds the unit as a new group without affecting other units. A leader is ready and receives traffic only when all followers are running.
The leader starts only after the model and image are ready on every rank's node (missing images are pre-pulled), avoiding staggered rank startup
that misses the collective communication rendezvous.

**Runtime scaling.** `PUT /deployments/{id}/pd` changes each role's units and `fallback`; the mode, TP, PP, and `kv_load_failure`
are immutable after startup (`PD_IMMUTABLE`). Removed units drain first: the gateway immediately stops dispatching requests to them, and the controller waits until their requests and
KV awaiting retrieval reach zero (at least 3 s and at most 5 min) before deleting them. Prefill can scale to 0 (all requests then complete locally on D and are not counted as fallback); decode cannot.

**Continuous verification.** After each prefill instance first becomes ready, the server sends one internal two-stage request through the gateway, reads D's NIXL counters and RDMA traffic on the
compute network to determine the transport, and records `PD_HANDOFF_VERIFIED{transport: rdma|tcp|local|unmeasured}`,
`PD_HANDOFF_NOT_RDMA`, or `PD_HANDOFF_FAILED` in the instance timeline. Monitoring restricts a PD deployment's engine metrics to decode instances and separately calculates PD effectiveness
(the proportion of D's received prompt tokens obtained through transfer), fallback rate, prefill stage duration, and queueing. The corresponding diagnostic rules are `pd_fallback`,
`pd_ineffective`, `pd_prefill_bound`, and `pd_short_prompts`.

**KV offload applies only to prefill.** The deployment's KV offload configuration applies only to prefill instances (combined with the PD connector in a `MultiConnector`); all KV cache tiers are removed
when decode instances start. Phase 0 measurements show that D-side caching only writes and never hits. User arguments containing KV connector flags are rejected.

**Two gateway stages.** For a PD deployment, `internal/server/proxy` first selects D (using the deployment's routing policy, least_request by default), then P
(using prefix affinity; when affinity has no effect, preferring the same node as D). `/v1/chat/completions` and `/v1/completions` use two stages:

1. Send to P: the original request body, with `max_tokens` (and `max_completion_tokens`) changed to 1, `stream=false`, `stream_options` and
   `min_tokens` removed, and `kv_transfer_params={"do_remote_decode":true}`;
2. Take `kv_transfer_params` from P's response and place it unchanged in the request body sent to D. The gateway does not parse its internal fields (the shape of
   `remote_block_ids` already differs between 0.17 and 0.25).

Other endpoints go directly to D. Client-supplied `kv_transfer_params` is always discarded for all deployments: Phase 0 measurements show that
this parameter with missing fields alone can make vLLM's EngineCore exit.

Fallback (`fallback=decode`): when no P is available, the connection to P fails, P returns 5xx, or P returns 200 without `kv_transfer_params`,
the original request (with KV parameters removed) goes directly to D, counted by reason; a 4xx from P is returned unchanged to the client. If the connection to D fails, the existing retry logic replays
the request with the same KV parameters to other D instances. Internal requests to P are neither billed nor counted as user requests; metering records only D's response.

**Status is not persisted.** The deployment's "not ready / partially ready / running" status and target ratio are derived from each role's desired and ready counts.

## Rationale

- An instance's role cannot be derived from cards, nodes, or ports, making it the only required new column; the unit-to-instance mapping can be derived and therefore is not persisted.
- Placing the two stages in the gateway reuses authentication, metering, retries, body replay, tunnels, and serving network paths, without exposing engine ports to third-party components.
- Phase 0 confirms on vLLM 0.17.1 and 0.25.1 that D completes locally when no KV parameters are supplied (fallback works); when D has a cached prefix, it retrieves only the missing portion
  (prefix affinity on D is beneficial); after P restarts, D continues working over TCP; `PYTHONHASHSEED` does not need to be identical.

## Future Work

- PPU images include only Mooncake, not NIXL; supporting PPU most likely means adding another Mooncake mode without changing the data model.
- Per-role images or presets and auxiliary roles (a coordination process for a shared KV pool) require only extensions to the `pd` object and catalog; `role` is a string with unrestricted values.
