# ADR 0033: Lifecycle Operations Apply to Replicas: Draining State, Rolling Restarts by Default, Multiple Multi-Node Replicas, and Sleep Intent on Instances

Date: 2026-09-30 · Status: Accepted

Based on: [Instance-Level Lifecycle Plan](../../artifacts/archive/2026-09-30-instance-lifecycle/plan.md). Amends several decisions in
[ADR 0028](0028-pd-disaggregated-deployment.md) and [ADR 0029](0029-deployments-reconcile-as-units.md), as described below.
Terminology follows `CONTEXT.md`: a **replica** is the unit in ADR 0029 (still called unit in code and the wire protocol); an **instance** is one rank in one generation of a replica.
The phrase "one unit is one instance of that role" in ADR 0028 should therefore be read as "one replica."

## Context

Sleep, restart, stop, and delete were previously available only on deployments; PD additionally supported per-role scaling, retries for individual replicas, and full-group restarts. A change to one part of a deployment with multiple replicas required taking the whole deployment offline:
restart was available only for PD and did not roll, draining worked only in the gateway's PD path, sleep intent existed only once per deployment, and a regular multi-node deployment could contain only one multi-node replica.

## Decision

**An instance explicitly selected to go offline has a state: `draining`.** The server exclusively owns this state (agent reports do not overwrite it unless they report that the instance has stopped). `MatchUnits`
treats it as inactive (the replica is replaced immediately), while the ledger still counts its resource usage and desired state is still sent to the agent. The controller uses the same drain timer for it and for unclaimed active leaders
(Excess), retiring them to `removing` when time is up. The gateway forwards only to running instances, so draining applies to all deployment forms.
An individual instance restart, targeted scale-down, and rolling restart are all expressed by marking a leader `draining`; an operation on a follower affects its entire generation.
There is no operation to keep an individual instance stopped: stopping a replica means scaling down and selecting that replica.
*Amends ADR 0029*: draining is no longer effective only in the gateway's PD path.

**Restart is available for all forms and rolls by default.** `POST /deployments/{id}/restart {"strategy":"rolling"|"all"}`, defaulting to rolling.
The start time is recorded in `deployments.restart_requested_at` (migration 116): replicas with an active leader created before that time still need replacement.
Only when all replicas are available does the controller drain the oldest remaining old leader, one at a time; it clears this column when all have been replaced. The column is also cleared when the deployment leaves active.
*Amends ADR 0028*: full-group restart is no longer exclusive to PD, nor limited to non-rolling behavior.

**Regular multi-node deployments can have multiple multi-node replicas.** The replica list goes into `units` in the existing `Deployment.Placement` JSON (the same shape as PD units),
without adding a column. Old deployments without `units` are read as one replica from `NodeIDs`. Each generation gets a new process group identifier (using the user-supplied group name as a prefix),
so two replicas with the same attempt number do not collide. Compute networks are resolved per replica; replicas whose leader is not on the first leader node use their own leader's compute network address as master.
Leaders of different replicas cannot share a node (leaders listen on the deployment's master port). Each multi-node replica has its own retry budget.
The gateway selects among leaders with complete generations according to the routing policy. `nnodes` (ranks per replica) cannot change after creation; changing it requires cloning.

**Sleep intent is recorded in each instance's desired state.** Deployment-level sleep writes `desired_sleep` for all active instances in one transaction
and records `deployments.desired_sleep`, whose meaning narrows to "the entire deployment is under a sleep command" and which serves only as the default for new instances.
Commands for an individual replica or PD role write only those instances. Waking any replica cancels the deployment-wide command. If an instance fails to sleep, only its own intent
(and any still-active deployment-wide command) is reverted; other sleeping replicas are not awakened. The controller keeps each instance's last pushed intent in memory and pushes only changes.
For an instance that has never received a push, it pushes once only if the reported level differs from the intent—after a server restart, agents receive the full desired state on reconnection, so push records are not persisted.
The gateway excludes instances individually if they have been commanded to sleep or are still asleep (effective immediately when the command is issued), returning 503 asleep only when none remain.
When a PD mode allows an empty prefill role, sleeping all prefill instances is equivalent to scaling to 0. Roles that the mode does not allow to be empty sleep only with the entire deployment.

## Consequences

- `settleRetiring` still blocks the entire deployment: while one instance remains in `removing`, new generations of other replicas do not start either.
  Restricting the scope to overlapping GPUs on the same node would mainly benefit PD and deployments with multiple multi-node replicas, while almost always blocking regular single-node replicas (whose candidates include all deployment nodes).
  The complexity is not justified; slow teardown (such as udocker deleting rootfs) delays replacement.
- Quota changes (GPU memory/SM limits and partitioning backend) still replace the whole deployment: whether dGPU can surge requires a ledger decision, so these changes do not roll yet.
- Rolling restart affects only one replica at a time, without `max_unavailable`.
- Multi-node replicas can sleep, at both the deployment and individual-replica level: commands go to the leader, and the engine's multi-node executor puts followers to sleep and wakes them together
  (verified on hardware with vLLM 0.25; a two-node PD decode replica uses the same launch method as a regular multi-node replica). The intent of a command for one replica is written to all of its ranks.
  Followers do not report a sleep level; the ledger uses the reported level of their generation's leader to account for residual usage and wake admission. Partial wake operates at replica granularity.
- Creation does not accept a list of multi-node replicas; create one first, then add more.

<!-- translation-source-sha256: 82dd17d7efba24387286f0d4dff957a8f63548a38d70a3ca9aff6e8b792a9c60 -->
