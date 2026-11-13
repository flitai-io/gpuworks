# ADR 0058: The Scheduling Center Only Carries Out Orders Operators Gave in Advance, Sleep Releases Capacity Down to a Per-Instance Residue, and Node-Loss Rescheduling Rewrites the Placement the Way an Operator's Edit Would

Date: 2026-10-25 · Status: Accepted

Terminology follows `CONTEXT.md`: **sleep residue**, **lifecycle schedule**, **idle sleep**, **wake waiting**, **yielding wake**, **node-loss rescheduling**, **node drain**.
Supplements [ADR 0033](0033-replica-level-lifecycle.md) (sleep intent on instances) and [ADR 0029](0029-deployments-reconcile-as-units.md) (deployments reconcile as units).

## Background

Sleep, wake, and replica counts were entirely manual: a wake that did not fit returned only 409, and the operator had to find who held the card, put it to sleep, and come back to wake; when several models take turns on the same cards, the same steps repeat every day.
After a node crashed, single-node replicas did not move on their own, multi-node replicas retried on the same set of nodes until they ran out, single-rank PD replicas had no offline detection,
and drain skipped multi-node and PD. Meanwhile sleep charged a fixed 1.5 GiB of residue, far from the real figure (about 0.6–1 GiB for a small model, about 4 GiB for a large multi-card one).

Two directions were possible: build a "clever" scheduler (scale by load, preempt lower-priority deployments, hold requests until a wake finishes, keep a share for the original deployment after sleep),
or only do for operators what they would do anyway. The former needs a whole set of semantics for priority, SLAs, and fairness and is hard to explain when it goes wrong; with the latter every action maps to one API call.

## Decision

**1. Automation only carries out orders operators gave in advance, through the same lifecycle lock and in the same order as the API. No preemption, no load-based scaling.**
Schedules, idle sleep, and waiting wakes are three kinds of order given in advance: the operator writes "sleep / wake / set N replicas at this time on these weekdays", sets "sleep after this long idle",
or chooses "wait" when a wake does not fit. The evaluator (`internal/server/automation`, one tick every 15 seconds and also woken once when capacity frees up)
reads only these three kinds of data and, when due, calls the same domain functions as the API (eligibility check, wake admission, intent writing) under the same lifecycle lock,
so a scheduled wake and a manual wake cannot both pass admission for the same card. Every automatic action is written to the deployment timeline (`AUTO_SLEEP`, `AUTO_WAKE`, `AUTO_SCALE`,
`WAKE_WAITING`, `WAKE_WAIT_TIMEOUT`, `SCHEDULE_ACTION_FAILED`), and failures are not retried. Specific constraints:

- Each occurrence runs once; for occurrences missed while the server was down, only the newest runs after it starts, and not if it is more than 12 hours old. A manual operation is not changed back on the next tick.
- The idle time counts from the latest of the server start, the last wake, and the last user request; health checks and other probes the platform sends itself do not count.
- A waiting wake takes only room that already exists and gives up after 30 minutes; the start of the wait is persisted and survives a server restart.
- A yielding wake (`?yield=true`) is a one-off action an operator names: it first puts sleep-capable occupants to sleep, and if an occupant cannot sleep, the whole request is refused (409 `WAKE_YIELD_BLOCKED`) and nothing changes.
- A scheduled replica count applies only to single-node deployments; there is no preemption, no load-based autoscaling, and no gateway holding of requests until a wake.

Persistence: one row per schedule entry (`lifecycle_schedules`, typed columns); idle sleep and the node-loss rescheduling wait are integer columns on the deployment, and the start of a wait is a time column on the instance.

**2. Sleep releases capacity, keeping only a per-instance residue; the idea that ordinary sleep retains the original allocation claim is not adopted.**
For an instance confirmed asleep, the allocation claim shrinks to its residue, the rest of the card's memory can go to other deployments, and a wake must pass admission again. The residue is recorded per instance:
when the instance is alone on its cards and no process outside the platform is on them, the agent reads the card's used memory about 1 second after a successful sleep and uses it as the measured value; otherwise the server estimates it from the model's parameter count in tiers
(up to 3B 0.75 GiB, up to 30B 1 GiB, up to 200B 1.5 GiB, larger 2.5 GiB) plus 0.5 GiB for the CUDA graph pool (the stored spec cannot tell whether the engine runs eager, so it counts as on),
plus 0.5 GiB for multiple cards (1 GiB from 8 cards), limited to 0.75–4 GiB, and 1.5 GiB when the model size is unknown. Without a measurement the estimate is used, never 0;
the reading is of the whole card and so errs high, which is the safe direction. The parameter count is derived from the weight bytes and the stored precision, so no new persisted fact is added.

Two accompanying refusals keep the released capacity from being oversold: before an L1 sleep, the weights moved into host memory must fit in the available memory minus the headroom (the larger of 5% of total and 4 GiB),
and a sleep that does not fit is refused with a hint to use L2, while a node with no host memory observation is not judged (409 `SLEEP_HOST_MEMORY_INSUFFICIENT`); a wake onto a card held by a process outside the platform is refused
(409 `WAKE_GPU_HELD_OUTSIDE`), and neither waiting nor yielding can get around this.

The rejected approach is "ordinary sleep keeps the original allocation claim, and only an explicit declaration releases it", that is, a guaranteed wake: it makes sleep pointless for several models taking turns on one card,
and it runs against the current code and how users work. Scenarios that need a guaranteed wake can be revisited later as an optional mode alongside time-sharing groups.

**3. Node-loss rescheduling is validated like an operator's placement edit, never spends retries, is undone only before the moved unit runs, and keeps its state in memory; drain of the units it names moves replacement-first.**
A node counts as lost when it has been offline for the deployment's rescheduling wait (`node_replace_after_sec`, default 600, 0 for never):

- Single-node replicas: the instance rows on the lost node are removed and the candidate pool fills in on other nodes; the replica did not fail, so no retry is spent. A replica pinned to its node has nowhere to go and stays.
- Multi-node and PD replicas: only the lost rank is replaced, on a node of the same compute network (the management network as a fallback), the same vendor and architecture, with free cards and a trial plan per rank under the deployment's configuration that fits,
  preferring a node that already has the model; only when replacing rank by rank fails, and only for an ordinary multi-node replica, are the nodes chosen again for the whole replica. The result is written back through `SetDeploymentReplicaUnits` / `SetDeploymentPlacement`
  with the same validation as an operator editing the replica list or a PD role (`planning.PlanReplicaUnits`, `ApplyPDUpdate`); the newly placed replica gets a full retry budget.
- If the original node returns before the new generation runs, the placement is changed back; once it runs, it is not switched back. This record lives only in memory: after a server restart the replacement made so far is kept and not undone.
- Automatic node choice requires the same vendor and architecture; accelerator RDMA networks are always treated as switched, and card connectivity tests are not a constraint; manually created mixed-architecture deployments keep their warning and are not tightened.
- Each replacement writes `NODE_REPLACED`, an undo writes `NODE_REPLACE_REVERTED`, and no candidate writes `NODE_REPLACE_NO_CANDIDATE` (at most once every 10 minutes).

When draining, multi-node and PD replicas move too, replacement-first: a replacement is started on other nodes first (surge, one per deployment at a time), and only once it runs is the placement rewritten onto it and the old replica drained
(`NODE_DRAIN_MOVED`); with no node to go to the replica stays in place and waits, and the number of serving replicas never drops below the count before the drain. This in-progress state is in memory too:
after a server restart, a replacement no unit claims is drained as an extra replica and the move is chosen again. Forced eviction is unchanged: it does not wait for replacements, removes at once, and does not spend retries.

When a single-node deployment grows, `PUT /deployments/{id}/replicas` can use `place_on` to name the nodes the new replicas are placed on first: it governs only the first placement, the hint is kept in memory,
and afterward the replicas take part in refill and replacement like any other.

## Consequences

- Every automatic behavior corresponds on the timeline to an action an operator could have done by hand, so troubleshooting means troubleshooting that API call; there is no hidden priority or policy.
- Operators must arrange who holds a card themselves: when two deployments share a card in turns, pair sleeping and waking with schedules, or use a yielding wake; the platform does not give way on anyone's behalf.
- After sleep releases capacity, a wake may not fit back. This is an explicit semantic (409, waiting, and yielding offer ways out), not a defect.
- The estimate used when nothing was measured is only an empirical figure and may be high or low; a measurement exists only when the instance is alone on its cards, so sleeping instances sharing a card always use the estimate.
- Node-loss rescheduling, its undo, and the drain surge keep their state in memory, so a server restart may lose the chance to undo or choose the replacement node again, in exchange for adding no persisted facts for these short-lived states.
- The evaluator runs inside the server process: nothing happens automatically while the server is down, and after it recovers only the newest schedule entry within 12 hours is made up.
- The evaluator reads only deployments that have a schedule, an idle setting, or a wait, and never scans them all, in keeping with the scale target; the board is paged by node and reads occupants only for the nodes on the page.

> Note, 2026-10-30: the page this record calls the “scheduling center” is now named “Scheduling analysis” (in the “Overview” menu group, route still `/scheduling`); the decisions are unchanged.

## Amendment (2026-11-13): The Undo Record Is Read Back from the Events After a Restart

Decision 3 says node replacement's undo record lives in memory only, and that after a server restart a replacement already made is kept and not undone. A controller-restart case confirms it: after a replacement and a restart, when the original node returns before the new replica runs, the replica stays on the node it moved to and no `NODE_REPLACE_REVERTED` is written. It is changed:

- No new persistence. `NODE_REPLACED` already names the unit, the rank and both nodes by UUID, and `NODE_REPLACE_REVERTED` the undoing; the first time the controller looks at a deployment after starting, it reads its undo record back from its newest events of these two kinds (at most 100), and reads again on the next pass when the read fails.
- Only what can still be undone is read back: the rank's newest event is the move, not a revert; the placement still has the rank on the node it moved to (one edited since is history); and no instance of the deployment has served on that node since the move (the record in memory is dropped once the unit runs there, and a unit that ran and stopped must not be switched back). The last check is by node, so another replica of the same deployment having run on that node counts too; whatever cannot be told is not read back, which is the original behaviour.
- The drain surge's progress and the first-placement hint stay in memory.

**Consequences.** A restart no longer keeps a replacement that would have been undone; cases that cannot be told fall back to the original "keep the replacement".
