# Reschedule Replicas on Node Loss

This guide explains how the platform reschedules the replicas of an offline node onto other nodes: how single-node, multi-node, and PD replicas are handled, how to adjust the wait, and how to make added single-node replicas start on chosen nodes first.

**Use case**: a node crashes, loses its network, or stays down for a long time, and you want service to recover on other nodes without editing replica lists by hand. For planned maintenance, use [drain](connect-nodes.md#维护节点停止调度与驱逐).

**Prerequisites**:

- The cluster has other online, schedulable nodes that satisfy the deployment's node requirements and have free cards.
- Rescheduling a multi-node or PD replica requires the replacement to share the original node's compute network and to have the same GPU vendor and architecture; see [How a Replacement Node Is Chosen](#怎样选替换节点).

## When a Replica Is Rescheduled

A node counts as lost when it has been offline (counted from when the server last saw its agent) for the deployment's **node-loss rescheduling wait**. The default is 10 minutes, adjustable per deployment, and 0 means never (shown as Off in the console). Nothing moves during the wait: if the node comes back soon, its replicas simply carry on. The server's own downtime does not count: after a restart, offline time is counted from the server's start at the earliest, giving agents the chance to reconnect; the same holds for a multi-node unit's reconnect grace.

Change it in the **Scheduling policy** card in deployment details: click **Edit** and use the **Reschedule on node loss** tab (the card's one-line summary shows the current wait), or use the API:

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/node-replacement" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"after_sec":600}'
```

Success returns 200 with the same `{"after_sec":600}`; the `node_replacement` field in deployment details shows the current value. `after_sec` is 0 or 60 to 604800 seconds (7 days); a value outside that returns 400 `NODE_REPLACEMENT_INVALID` (`params.minimum`, `params.maximum`).

Node-loss rescheduling is on by default (10 minutes for new and existing deployments), does not require the deployment to declare a compute network, and works for deployments that use the management network too.

Why it is on by default: without it, a replica on a lost node can only retry in place until its retries run out, and the service stays degraded. Rescheduling rewrites the placement through the same validation as editing the replica list by hand, so every step on the timeline matches an action you could have taken yourself, which is more predictable than relying on failure retries. Why there is no switch back once the new generation is running: switching back would interrupt the service that has just recovered. The undo record lives only in server memory, so no persistent data is added for these short-lived states; the price is that after a server restart the replacements already made are kept and no longer undone.

## How Each Kind of Replica Is Rescheduled

| Replica | Handling |
|---|---|
| Single-node replica | The instance rows on the lost node wait as `removing` while the candidate pool fills in on other nodes; the removal is pushed and leftover containers are cleaned up only when the node reconnects, and deleting the node removes such rows with it. The replica did not fail, so no retry is spent |
| Multi-node replica | Only the lost rank is replaced and the other ranks stay on their nodes; only when that fails are the nodes chosen again for the whole replica. The newly placed replica gets a full retry budget |
| PD replica | Each P and each D is a replica and is handled like a multi-node one: only the lost rank is replaced, and the role's replica in the placement is rewritten |

Rescheduling rewrites the placement exactly as you would by editing nodes under **Edit replicas** or in a PD role, with the same validation. Every rescheduling writes `NODE_REPLACED` (original node, new node, wait) to the deployment timeline.

When no suitable replacement exists, the replica stays as it is, the timeline records `NODE_REPLACE_NO_CANDIDATE` every 10 minutes, and the platform keeps trying until a node frees up.

**A single-node replica pinned to its node is not replaced**: when the deployment's node requirements allow only that one node, there is nowhere else to go; the replica stays and recovers when the node returns.

## How a Replacement Node Is Chosen

The replacement node for a multi-node or PD replica must satisfy all of these:

- Online, schedulable (not cordoned or draining), and satisfying the deployment's node requirements.
- On the same compute network as the replica's other ranks; when the other ranks use the management network, the replacement uses it too.
- The same GPU vendor and architecture as the lost node.
- Enough free cards for the rank.
- Not the node of another rank of the same replica; the leader of a regular multi-node replica also cannot share a node with another replica's leader.
- A trial plan under the deployment's saved configuration fits (up to the best 3 nodes are tried).

Among the candidates, a node that already holds the model files is preferred. When no candidate has the model, the replacement still happens and the new node downloads or copies it.

Accelerator RDMA networks (Ascend) are always treated as switched: the same network and card numbers suffice, and card-to-card connectivity tests are not checked.

## Node Online but Cannot Take the Replica: Automatic Placement Chooses Again

Node-loss rescheduling handles only **offline** nodes. A node that is online but cannot take the replica (its cards were taken by another deployment, it was cordoned, or it no longer meets the node requirements) is a different case, handled according to the deployment's placement mode:

| State of the replica's node | Automatically placed multi-node and PD deployment | Manually placed multi-node and PD deployment |
|---|---|---|
| Offline | Rescheduled after the rescheduling wait time (above, unchanged) | Same |
| Drained | Start the new replica before switching (below, unchanged) | Same |
| Online but cannot take it | When the replica needs to start, only the ranks that do not fit move to other nodes (`UNIT_PLACED`) and the other ranks stay; with nowhere to go it keeps its nodes and waits, looking again every minute | The replica waits, with the reason in `DEPLOYMENT_UNSCHEDULABLE`; when PD pinned cards are held, the scheduling reason is `pinned_gpus_unavailable`, listing the deployments holding them |

Both use the same candidates and trial plans, and both rewrite the placement the same way. They differ only in the trigger: replacement fires when a node has been offline longer than the wait, and the re-pick fires when a replica needs to start and its nodes cannot take it. There is no wait time and no retry is spent. It happens only when the replica has no instance anyway, so it never interrupts a replica that is serving. Single-node replicas are not covered here: the candidate pool chooses again at every start. For switching the placement mode, the selection rules, and the limits, see [Multi-Node Inference](multi-node-inference.md#自动放置与手动放置) and [PD Disaggregated Deployment](pd-disaggregation.md#自动放置与手动放置).

## When the Original Node Returns

- If the original node comes back online **before the new generation is running**, the placement is changed back to it and the timeline records `NODE_REPLACE_REVERTED`.
- If it returns **after the new generation is running**, the replica is not switched back and stays on the new node; once the original node reconnects, the platform pushes the removal and its leftover containers are cleaned up.
- The undo record lives in server memory and is read back from the `NODE_REPLACED` events after a restart: if the server restarts after a replacement and the original node returns before the new replica runs, the replacement is still undone. A move is read back only when all three hold: no `NODE_REPLACE_REVERTED` for that rank since, the placement still has the rank on the node it moved to, and no instance of the deployment has run on that node since the move. When in doubt it is not read back, and the replacement is kept.

## Drain for Multi-Node and PD Replicas

When a node is drained, multi-node and PD replicas also move away, by the same replacement-first rule as single-node replicas.

1. A replica with a rank on the draining node first gets a replacement on other nodes (nodes chosen by the rules above), one per deployment at a time.
2. Only after the replacement runs is the placement rewritten onto it and the old replica drained, and the timeline records `NODE_DRAIN_MOVED`. The number of serving replicas never drops below the count before the drain.
3. When there is no node to go to, the old replica stays in place and keeps serving, and the platform retries about once a minute.

The leader of a regular multi-node replica holds the deployment's master port, so its replacement is chosen as a whole and led from a node other than the old leader's. A PD replica moves only the ranks on the draining node.

Forced eviction is unchanged: it does not wait for replacements, removes everything on the node at once, and does not count against retries.

## First Placement Nodes When Scaling Up a Single-Node Deployment

When adding single-node replicas, you can name the nodes the new replicas start on first. Choose **First placement nodes** in **Edit replicas**, or add `place_on` to `PUT /api/v1/deployments/{id}/replicas`:

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/replicas" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"replicas":3,"place_on":["<节点 UUID>","<节点 UUID>"]}'
```

- Naming more nodes than replicas added returns 400 `REPLICAS_PLACE_ON_TOO_MANY` (`params.count`, `params.maximum`). The same node may be named twice, meaning two replicas go there.
- Each node must be known, online, schedulable, satisfy the deployment's node requirements, and have room for a replica now (the same trial plan the candidate pool runs); otherwise the response is 409 `REPLICAS_PLACE_ON_REFUSED`, where `params.reason` is `unknown`, `offline`, `cordoned`, `requirements`, or `no_room`, and the last two carry `detail` or `code` / `params` with the specific cause.
- A manually placed multi-node deployment with `place_on` returns 400 `REPLICAS_BODY_SHAPE`; it lists nodes directly with `units`. An automatically placed multi-node deployment also supports `place_on`, but there it names the nodes of the new replica's ranks; see [Multi-Node Inference](multi-node-inference.md#为已有部署增加副本) for the rules.
- This governs only the first placement: afterward these replicas, like any others, take part in candidate-pool refill and automatic replacement. The hint is kept in server memory; after a server restart the candidate pool chooses nodes itself.

## Timeline Events

| code | Meaning | Params |
|---|---|---|
| `NODE_REPLACED` | A replica was moved off a lost node | `from`, `to` (absent for single-node replicas), `after_sec`, `unit`, `rank`, `role`, `instances` |
| `NODE_REPLACE_REVERTED` | The original node returned before the new generation ran, and the placement was changed back | `from`, `to`, `unit`, `rank`, `role` |
| `NODE_REPLACE_NO_CANDIDATE` | No node to move to; recorded at most every 10 minutes | `nodes` |
| `UNIT_PLACED` | Before an automatically placed multi-node or PD replica started, the ranks that were online but could not take it moved to other nodes | `unit`, `role`, `from`, `to` (node names, by rank), `reason` (node cordoned, node requirements not met, or the scheduler's refusal code) |
| `NODE_DRAIN_MOVED` | A multi-node or PD replica switched to its replacement during a drain | `unit`, `role` |

## Limitations

- During the wait, a multi-node replica is still handled by the existing rules: a rank whose node is offline makes the whole group fail and be rebuilt, and those failures are recorded as usual; the replacement happens once the wait has elapsed.
- Card-to-card connectivity tests and Ascend direct wiring are not conditions for choosing a node; the risk of crossing switches or of RDMA not working remains for you to confirm at creation, and manually created mixed-architecture deployments only get a warning, with no stricter check.
- Replacement does not change the model, image, or configuration; the models and images the new node needs are obtained as described in [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md).
- The first-placement hint lives only in server memory and does not survive a restart; the undo record is read back from the events after one (see above).
