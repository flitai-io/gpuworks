# Multi-Node Inference

**Use cases**: The model does not fit on one node, or you want to distribute TP / PP across nodes so that multiple ranks of a deployment run on different machines.

After completing this guide, all ranks of a replica are ready, and you can complete an inference request using the deployment's service name.

**Prerequisites**:

- At least two online nodes, connected as described in [Connect Nodes](connect-nodes.md).
- All nodes must share a compute network. Without a shared compute network, fallback to the management network is possible only if every node allows it to carry compute traffic. For network selection and RDMA settings, see [Configure Node Networks](configure-networks.md).
- NVIDIA uses NCCL/UCX for communication on a host RDMA compute network. This network cannot be used on runveil (udocker driver) nodes.
- Ascend multi-node inference requires an accelerator RDMA or accelerator HCCS network.
- You are familiar with single-node deployment. See [Deploy Models, Select GPUs, and Interpret Scheduling Results](deploy-models.md).

## Deploy a Multi-Node Replica

1. Check each node's address on the selected network and confirm that firewalls allow bidirectional TCP between these addresses.
   On the management network, each address is the local address the node uses to connect to the server. On Ascend nodes, also verify connectivity between the accelerator cards you actually select.
   To verify links, select these nodes and run an [internal network test](configure-networks.md#内部测试);
   TCP reachability does not prove RDMA or accelerator network availability. Select the corresponding test method.
   If a network probe fails, fix routes, firewalls, or ports first. Do not bypass the failure by disabling authentication.
2. Choose the placement mode (see [Automatic and Manual Placement](#自动放置与手动放置)). In manual mode, fix the node order by rank 0…N-1 on the deployments page; in automatic mode you may name only the first ranks' nodes and the platform picks the rest. Then configure parallelism. Tensor parallelism (TP) and pipeline parallelism (PP)
   apply to the entire deployment. Total cards = TP × PP; each node runs TP × PP ÷ node count cards (the division must be exact).
   After you select nodes, TP defaults to the smallest GPU count among selected nodes, and PP defaults to the node count. TP can also span nodes: for example, two nodes with
   1 card each can use TP 2. A node with more GPUs than the per-node card count uses only some of them.
   In the multi-node form, parallelism settings follow nodes and precede the image. TP / PP errors appear on the corresponding fields and do not prevent image and weight acquisition estimates from appearing.
   Automatic placement requires TP: the platform needs it to work out how many free cards each node must have. Without TP the request returns 400 `AUTO_PLACEMENT_TP_REQUIRED`.
3. Select the image and weights, then expand the per-node acquisition estimates, such as “1 of 2 nodes needs to pull the image.”
   Nodes that need resources follow the [pull settings](configure-pull-settings.md); nodes that already have them are skipped. Image pre-pulling before startup uses the same candidates.
4. Review the form's configuration assessment. Correct errors in nodes, shared networks, and TP / PP, and confirm that the master port is available.
   The form precheck validates only configuration and node conditions. It does not measure network connectivity or bandwidth.
   For a separate API probe, call `POST /api/v1/deployments/preflight`. TCP bandwidth is measured only if
   `bandwidth` is enabled in the request. See the [API Reference](../reference/api.md) for fields and results.
5. Check node order and parallelism in the command preview, then submit the deployment. vLLM uses `--tensor-parallel-size` / `--pipeline-parallel-size`,
   and SGLang uses `--tp-size` / `--pp-size`. The corresponding PP argument is injected only when PP is greater than 1. The rank 0 address serves as master, and external traffic enters only rank 0;
   the other ranks work as followers, so you do not need to call their endpoints individually.
6. Wait for all ranks to become ready in deployment details, then send a request through Playground or `/v1` using the deployment's service name.
   Seeing rank 0 created or beginning to load does not mean the entire replica is available. On failure, inspect the affected rank's events and logs.

To write the launch command yourself, click **Edit Command Text** in the command preview. The editor starts from a script the server generates; every rank shares
that one script, and variables such as `${GPUWORKS_NODE_RANK}`, `${GPUWORKS_HEADLESS}`, and `${GPUWORKS_MASTER_ADDR}` are replaced with each rank's values before it starts.
Below the editor, each rank's result after replacement is shown. The command must reference `${GPUWORKS_NODE_RANK}` or `${GPUWORKS_HEADLESS}`; otherwise evaluation and creation return
`MANUAL_COMMAND_RANK_BLIND`. See [Manual Commands for Multi-Node and PD Deployments](../explanation/launch-config.md#多机与-pd-的手工命令).

If the deployment keeps waiting for nodes, check `DEPLOYMENT_UNSCHEDULABLE` in the timeline. It lists the ranks that cannot be placed, their nodes, and the reasons.
All ranks of a replica must simultaneously meet the model, image, port, GPU, and GPU memory requirements before they are created together;
the leader does not reserve GPUs first, and this wait does not consume replica retries. Once you supply the missing resources indicated by the reasons, the platform continues automatically. The same applies to multi-node units in PD deployments.

For all creation fields, see the [API Reference](../reference/api.md).

## Automatic and Manual Placement

A multi-node deployment has the same two placement modes as a single-node one:

| Mode | Who chooses the nodes | How to use it |
|---|---|---|
| **Automatic** | The platform. At creation it picks the ranks the request leaves out, from the nodes' free cards; when a replica starts and its nodes cannot take it, it moves only the ranks that do not fit | `placement_mode: "auto"` with `placement.tp`; `node_uuids` may be omitted or may name only the first ranks (the rest are filled in from the same network and architecture) |
| **Manual** | You. You write every rank's node and the platform does not change it | Omit `placement_mode` (it means manual) and write `nnodes` entries in `node_uuids` |

Multi-node deployments that existed before the upgrade are manual. The deployment's `placement_mode` shows the current mode, and `PUT /api/v1/deployments/{id}/placement-mode` switches between them without restarting instances; see [Deploy Models](deploy-models.md#部署后调整).

### How Nodes Are Chosen at Creation

For each replica the platform picks `nnodes` nodes that meet the conditions below. All nodes of one replica must share one compute network and one GPU architecture:

- Online, schedulable (not cordoned or draining), meeting the deployment's node requirements, and with the free cards one rank needs (TP × PP ÷ node count).

Groups that qualify are compared in this order:

1. RDMA networks (host RDMA or accelerator RDMA) before Ethernet.
2. Groups with more nodes that already hold the model file.
3. Within the group, the placement strategy: multi-node defaults to `binpack`, which takes the node with the fewest free cards that still fit so whole empty nodes stay free for the next deployment; `spread` takes the most free cards. Nodes that already have the model always come first, and ties go to the lower node ID, so the same input gives the same result.

After the pick, the request goes through the ordinary checks as if you had written those nodes yourself, and the deployment stores the nodes that were picked. Which cards are used is decided by scheduling at launch. For an automatic multi-node request, the evaluate endpoint (`POST /api/v1/evaluate`) returns `auto_placed: true` and previews the nodes that would be picked now; creation picks again, so the two can differ.

When no group can take the replica, creation returns 409 `UNIT_PLACEMENT_NONE` with parameters saying how many nodes each check left out (requirements not met, cordoned, offline, not enough free cards, no compute network) and how many nodes the largest group has. Evaluation reports it as an error on the `placement_mode` field.

### Choosing Again When a Replica Starts

The nodes picked are the nodes written in the unit (replica), and matching, network resolution, node-loss rescheduling, and drain all work from them as before. When a replica needs to start and has no instance, the platform plans it again with the current free cards:

- All its nodes are online and can take it: it starts as is.
- Some ranks' nodes were cordoned, no longer meet the node requirements, or would refuse the launch (another deployment holds the cards): **only those ranks move to other nodes** and the other ranks keep their nodes. The candidates are chosen as for a [replacement after a node is lost](node-failover.md#怎样选替换节点) (same compute network, same vendor and architecture, free cards, trial plan fits). If the ranks cannot be moved one at a time, the whole replica is chosen again. The timeline records `UNIT_PLACED` with which replica, the old nodes, the new nodes, and the reason (`reason`, for example `node_cordoned`, `placement_rejected`, or the scheduler's refusal code).
- Nowhere to go: the replica keeps its nodes and keeps waiting, with the scheduler's wait reason in the timeline as usual; the platform looks again after a minute.
- An offline node is [node-loss rescheduling](node-failover.md)'s and follows its wait time; replicas on a draining node follow the drain's start-new-then-switch. These two cases are the same for automatic and manual deployments.
- While the deployment's own instances are still leaving those nodes (a restart or a retired attempt) nothing is chosen again, because their cards are about to be released.

Manual deployments do not do this: a replica whose nodes cannot take it waits, and `DEPLOYMENT_UNSCHEDULABLE` says why. Switch to automatic if you want the platform to move it.

## Add Replicas to an Existing Deployment

**For an automatically placed deployment**, say only how many replicas you want:

1. Open **Edit replicas** in deployment details and enter the replica count. If needed, name the new replicas' nodes in **Nodes for first placement**.
2. The API is `PUT /api/v1/deployments/{id}/replicas` with `{"replicas": 3, "place_on": ["<node UUID>", …], "remove": ["<instance UUID>"]}`:
   - `place_on` names nodes for the new replicas' ranks in order: first the first new replica's ranks 0…N-1, then the next replica's. You may give only some; the platform fills in the rest from the same network. More nodes than the new replicas have ranks returns 400 `REPLICAS_PLACE_ON_TOO_MANY`, and nodes that cannot form a replica that fits return 409 `REPLICAS_PLACE_ON_REFUSED` (`params.reason` is `no_group`).
   - When no group can take a replica, 409 `UNIT_PLACEMENT_NONE`.
   - When you reduce the count, the replicas of the instances listed in `remove` go first; without it the most recently added go.

   A new replica's leader does not share a node with another replica's leader. `place_on` only decides where a new replica is first placed; afterwards it takes part in the re-pick at launch like the others.

**For a manually placed deployment**, submit the full replica list (`units`); the recommendation button saves effort:

1. Open **Edit replicas** in deployment details and click **Add a suggested replica**.
2. Check the populated node list and save after confirming it is correct. If no group is recommended, check the number of excluded nodes by category, satisfy the missing conditions indicated by the reasons, then select nodes again.

Nodes in a recommended group must be online, schedulable, satisfy node requirements, and share the same compute network and GPU architecture. Each node must have enough free cards for one rank.
The platform evaluates placement rank by rank using the deployment configuration, then populates the list. A leader does not share a node with another replica's leader. The recommendation is only a suggestion and is saved after you confirm it; it uses the same selection as automatic placement and differs only in that you confirm the result. `place_on` on a manual deployment returns 400 `REPLICAS_BODY_SHAPE`.

**When a node is lost or drained**, automatic and manual deployments behave the same: the platform replaces nodes automatically, only the affected rank moves, and the replacement is chosen by the same rules (same compute network, same GPU vendor and architecture, free cards, trial plan fits) without spending retries. For the wait, selection rules, and undo, see [Reschedule Replicas on Node Loss](node-failover.md).

## Limits

- Automatic placement chooses nodes only, not cards: how many cards each node uses is fixed by TP × PP ÷ node count, and the scheduler picks which ones at launch. A multi-node replica cannot pin cards.
- The platform judges whether nodes can reach each other from the compute network **declaration**, not from network test results. When the declaration does not match reality (for example a switch that does not route), a group that cannot talk can still be chosen, and the engine reports the failure after launch. For network selection and verification see [Configure Node Networks](configure-networks.md).
- Automatic placement does not start some ranks while it waits for the rest: the ranks of a replica are created together or wait together.
