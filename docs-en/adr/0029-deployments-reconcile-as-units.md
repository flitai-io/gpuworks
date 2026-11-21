# ADR 0029: Every Deployment Reconciles by Unit: Role → Unit → Rank, With Shared Retry, Group Failure, and Draining Logic

Date: 2026-09-29 · Status: Accepted

Based on: [Deployment Convergence Plan](../../artifacts/archive/2026-09-29-unit-reconcile/plan.md).

## Context

Regular deployments (replicas), regular multi-node deployments, and PD deployments previously followed three reconcile paths, each with its own retry budgets, group resets, and draining.
Fixes have already started landing in only one path: the multi-node startup barrier was added only to PD, while regular multi-node deployments could still miss rendezvous because ranks finished pulling images at different times.

## Decision

**Represent a deployment as a set of units.** `planning.DeploymentUnits` is the sole conversion:

| Deployment Form | Units | Ranks per Unit |
|---|---|---|
| Regular, `replicas = N` | N interchangeable units with an empty role | 1; nodes form a candidate list, using the first available node |
| Regular multi-node, `nnodes = N` | 1 | N, in deployment node order |
| PD | Every unit of every role (ADR 0028) | leader + followers |

**A generation is one launch of a unit.** The leader (rank 0) and followers (headless) share
`(dist_group, dist_attempt)` and are created together in the same round. The pre-start barrier confirms that each rank's node is online and its model and image are ready
(for multi-node deployments, images are pre-pulled to every node). A multi-node leader records its unit's followers in the scheduling record
(`planning.ScheduleRecord.Unit`) to claim the unit.

**Matching has one implementation.** `planning.MatchUnits` derives which instance fills each unit from instance rows: active leaders claim units by role, node, pinned cards, and
recorded followers; followers follow their leader by generation. Unclaimed active leaders are removed after draining; followers that have lost their leader
are removed immediately. Failed leaders are grouped by unit key as attempts consumed by that unit. The controller and gateway use the same derivation;
`planning.GroupsIncomplete` (whether a generation is complete) is likewise shared by deployment status, the gateway, and the controller.

**Failed leader rows are the retry budget.** Rows are retained as evidence, and the budget is counted by unit key (all units in a regular deployment have the same key, so its budget is deployment-wide);
the attempt number = the failure count + 1. `deployments.restart_count` is removed (migration 110). A leader records which unit it starts for
(the unit key) in its scheduling record, and failures are attributed using that record. Failed rows that no longer belong to any unit (the unit is removed from placement or added back with a different shape) are
removed, so adding the same unit back restores its full budget. When the budget is exhausted, the controller records `UNIT_RETRY_EXHAUSTED` on the latest failure (once only);
operators use `POST /deployments/{id}/units/retry` to restore the budget for an individual unit without affecting others.

**Attempts refused before starting because a port was taken do not charge the budget (amended 2026-11-08).** Before pulling an image or
creating a container, the agent checks the host ports the server allocated; when another process already holds one it reports
`ENGINE_PORT_REFUSED` at once, and nothing has started. An attempt whose leader row failed with this code is retried immediately, without the retry backoff;
a unit's first 10 such refusals do not charge its budget (`planning.FreePortRefusals`), and later ones count like any failure,
so a node that keeps refusing still exhausts the budget in the end. The failed rows are kept as usual, and the ports they hold keep the next allocation away from them. When a follower is refused, the leader row records
`rank N on … failed: …` and the leader's container may already have started, so it counts as an ordinary failure. After reporting a refusal the agent sends a node status at once,
so the next allocation sees the current port listeners. An engine that started and found another model answering on its API port (`ENGINE_PORT_IN_USE`) is not this case: that attempt created a container.

**A generation is either complete or failed.** Multi-node collective communication cannot replace an individual process: if any rank fails, its row disappears (stopped), or its node remains offline beyond
the reconnect grace period, the leader is marked failed and its container is removed, and the followers are removed. Removal is pushed before writing the failed status; if the push fails, the next round retries.

**Removal waits for acknowledgment.** For rows being removed on online nodes, removal is pushed again and waits for the agent to acknowledge the stop. A new generation does not start before that acknowledgment (under dGPU, an unacknowledged rank
still occupies its GPU memory share). Offline nodes cannot acknowledge, so their rows are deleted directly; orphan cleanup removes their containers after reconnection.

**Form-specific behavior stays in the form.** PD network and image drift checks (needed only for pairing across units), port ranges, and connector arguments are supplied by the PD form;
a rank's topology has just one constructor, `planning.RankTopology`.

## Consequences

- Regular multi-node deployments gain a startup barrier; the gateway selects leaders based on whether their generation is complete, as with PD.
- A rank that enters stopped in a regular multi-node deployment makes its generation fail (previously this happened only under dGPU).
- Upgrade compatibility: existing multi-node leaders without unit records claim units by matching nodes and rank counts, so running deployments are not rebuilt;
  old deployments whose reset count was exhausted have their budget recalculated from failed leader rows after upgrade.
- Deployment forms differ only in how units are produced and in PD-specific launch arguments; a new form only needs to provide its units.

<!-- translation-source-sha256: 33c0721354d9dda80c6e9771e7bec1df5b75cb2443167cb0e3c5d2cda76509b6 # ADR 0029: Every Deployment Reconciles by Unit: Role → Unit → Rank, With Shared Retry, Group Failure, and Draining Logic -->
<!-- translation-source-sha256: 96f2d104b6c406aa069352d77782cdf242423f7eba23d6ce02988cd340f1daa0 ## Context -->
<!-- translation-source-sha256: c210388285b164a62b2f9b8971cc5c761d6b32c4c8ac8ea25cc4cdf7522fb9bb ## Decision -->
<!-- translation-source-sha256: eee2d40c7aa67fd63b751c5ff06cf6c343556d6a7b68bcceb2bfd6427e80a018 ## Consequences -->
