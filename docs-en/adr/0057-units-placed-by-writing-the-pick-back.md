# ADR 0057: The Platform Places Multi-Node and PD Units by Writing the Pick Back into the Units

Date: 2026-10-28 · Status: Accepted

## Background

Single-node deployments have "automatic / manual" placement: when automatic, no node is pinned and the candidate pool chooses again at every start (in [ADR 0029](0029-deployments-reconcile-as-units.md) a single-node replica's unit has one rank, and its node is a candidate list). Multi-node and PD ([ADR 0028](0028-pd-disaggregated-deployment.md)) had only manual placement: the operator wrote the node of every rank and every P / D unit, and a card choice pinned them. The operator had to work out which nodes share a network, have the same architecture, and have free cards; and when the cards a PD deployment pinned were held by other deployments, the unit did not use free cards on the same node and only said "not enough free whole cards" while it waited for hours.

Letting multi-node and PD place automatically runs into one structural fact: across the system a unit **has nodes written in it**.

- `planning.MatchUnits` claims a unit by role, node, pinned cards, and the recorded followers (ADR 0029);
- network resolution ([ADR 0030](0030-network-objects.md)) works out from each rank's node which network and address each role uses, and the PD KV network and the multi-node master address come from that;
- node-loss rescheduling and drain (start the new one, then switch) both rewrite the nodes in a unit, using the same validation and write-back as an operator editing the replica list by hand;
- the ranks of a multi-node replica must be created together and fail together, and the start barrier and image pre-pull need to know which node each rank is on.

## Decisions

1. **The placement mode is one boolean fact on the deployment.** A new typed column `deployments.units_auto` (default false, migration 178) says whether the platform places a multi-node or PD deployment. It cannot be derived from existing data (a single-node "automatic" means "no node pinned", whereas multi-node and PD units always have nodes written), so it earns a column. Deployments that existed before the upgrade read as false and keep the operator's placement. The API calls it `placement_mode` (`auto` / `manual`): create and evaluate accept it, the detail returns it, and `PUT /deployments/{id}/placement-mode` switches between the two (instances are not restarted; switching to automatic also drops the cards PD units were pinned to).

2. **The nodes the platform picks are written back into the units, rather than leaving the units "unplaced".** At creation (and when a multi-node deployment scales by replica count, or a PD deployment adds units) the server picks nodes for the ranks or units the request leaves out and writes the result where a manual request would put it (the multi-node `node_uuids`, the `placement.pd` units). The request then goes through the ordinary checks and storage as if the operator had written those nodes. A unit therefore always has nodes written in it, so matching, network resolution, node-loss rescheduling, drain, and the start barrier all keep working, and not one of them has to know about an "unplaced unit". The nodes written are also the later "prefer the previous nodes", with nothing extra stored.

3. **Creation picks by capacity; starting a unit corrects it with a trial plan.** The pick at create / evaluate looks only at free cards, node requirements, network and architecture and does no full trial plan, so it is cheap and can be previewed (evaluate returns `auto_placed`). When a unit needs to start and has no instance, the controller plans it again under the real launch conditions with the unit trial shared with the API (`planning.UnitTrial`): if all its nodes are online and fit, it starts; if some ranks' nodes were cordoned, no longer meet the node requirements, or would be refused by scheduling (another deployment holds the cards), **only those ranks move to other nodes** while the other ranks stay, using node replacement's candidates and write, and `UNIT_PLACED` is recorded. With nowhere to go, the unit keeps its nodes and waits, looking again about every minute. This happens only when the unit has no instance anyway, so it never interrupts a serving instance and spends no retry.

   An offline node is still node-loss rescheduling's (the wait time is unchanged) and a draining node is still drain's (start the new one, then switch); automatic and manual deployments behave the same in those two cases. A manual deployment does not re-pick: a unit whose nodes cannot take it waits, with the reason in `DEPLOYMENT_UNSCHEDULABLE`; when PD pinned cards are held by other deployments the reason is `pinned_gpus_unavailable`, which names the deployments holding them.

4. **Selection rules.** Candidates are groups of nodes in one compute network and one GPU architecture, each with the free cards one rank needs (a multi-node request is chosen per replica, a PD request for the whole deployment because the roles exchange KV). Between groups RDMA comes before Ethernet, then the group with more nodes that hold the model; within a group nodes with the model come first, then the placement strategy. Multi-node and PD default to `binpack` when no strategy is given (they take a whole group of cards at once, and packing leaves whole empty nodes for the next deployment), while single-node stays `spread`. For PD under `binpack`, if one node can take every unit they all go on that node (no KV crosses the network), and under `spread` prefill and decode avoid sharing a node where they can. Whether nodes can reach each other is judged from the network **declaration** only, not from network tests or KV verification results.
5. **Cards are not written back.** Automatic placement picks nodes for the operator only: multi-node never allowed pinned cards, and automatic PD units are written back without `gpus`; the scheduler picks which cards at launch.
6. **Limits: an automatically placed PD unit must fit one node.** A unit with followers (spanning nodes) must name its nodes (`AUTO_PLACEMENT_PD_MULTI_NODE`); an automatic multi-node request must carry `placement.tp` (`AUTO_PLACEMENT_TP_REQUIRED`) so that the cards each node needs are known.

## Trade-offs

- **Not writing back, keeping units unplaced and choosing everything again at every start**: it looks purer, but matching, network resolution, node-loss rescheduling, drain, the start barrier, image pre-pull, and the PD KV network check would all have to understand "node undecided", and they all assume a unit has nodes written. The ranks of a multi-node replica must start together, and deciding the nodes at the moment of starting would mean redoing every check that precedes the barrier. Writing back confines the change to one place: who writes the nodes.
- **Choosing again at every start**: it would move a serving role because cards elsewhere changed. Now it acts only when the unit has no instance and its nodes cannot take it, and it moves only the ranks that do not fit.
- **A full trial plan at creation**: create, evaluate, and scaling would plan node by node and group by group, which is expensive at the scale target (1000 nodes), and a trial result can be stale before the unit actually starts. So the controller does it at start and creation does only the cheap capacity check.
- **Using the nodes the operator wrote as "prefer the previous nodes" instead of storing them separately**: no new persisted fact (ADR 0032).

## Consequences

- One new column, `deployments.units_auto`; the stored shape of units does not change, and old data reads as manual.
- Creation, evaluation, scaling, and the controller share one set of candidates and trial plans (`planning/unit_choice.go`, `unit_trial.go`), and node-loss rescheduling now orders its candidates by the deployment's placement strategy too.
- The pick at creation is a capacity judgement, so the evaluate preview and creation can differ; a node that cannot take a unit is discovered only when the unit starts, not by continuous monitoring.
- When the network declaration is wrong (for example a switch that does not route), a group that cannot talk can still be chosen, and the engine or the PD handoff self-check reports the failure after launch.
- The units of an automatically placed PD deployment cannot span nodes; such units are still written by hand.
- What changes is only who writes the nodes: switching the placement mode neither restarts anything nor changes any node.

<!-- translation-source-sha256: bc8bcaca43bdd7ae923fe1b394afb9cc8d89b1fe8a04c99a21a91d18532121bf -->
