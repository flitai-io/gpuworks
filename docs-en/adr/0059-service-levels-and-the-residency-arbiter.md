# ADR 0059: Share GPUs by Service Level: the Gateway Holds Requests While a Wake Happens, and the Residency Arbiter Preempts by One Yield Rule

Date: 2026-10-28 · Status: Accepted

Terminology follows `CONTEXT.md`: **yield policy** (called service level in the original decision of 2026-10-28), **residency arbiter**, **yield**, **held sleep**.
Changes two points of decision 1 in [ADR 0058](0058-scheduling-center-acts-through-operator-orders.md): “no preemption” and “the gateway does not hold requests while waiting for a wake”;
the rest (schedules, idle sleep, waiting wakes, yielding wakes, sleep releasing capacity down to a residue) is unchanged.
This page keeps the original decision of 2026-10-28 and three amendments of 2026-10-29; **service level** and the presets are no longer terms after the second amendment, which states the current behaviour; the third amendment (the last section) adds that a new deployment waits for cards too.

## Background

ADR 0058 only had the platform carry out orders operators gave in advance. In practice the two most common ways of sharing cards are these: real-time chat is used whenever someone shows up while batch work runs whenever the cards are free;
or several models are each rarely used and take turns on a set of cards. The former needs “take the cards back as soon as real-time arrives, and hand them back to batch when it goes idle”, the latter needs “take turns, but do not flap”.
Schedules and manual yielding cannot do this: nobody knows in advance when a request will arrive, and a request to a sleeping deployment only gets a 503.

Measured on an Alibaba Cloud L20 on 2026-10-28 (vLLM 0.17.1, model files in the page cache): swapping a 7B and a 14B model on the same card, from the yielding side going to sleep to the woken side producing its first token took
about 2.5 seconds with L1, and with L2 3.5–5 seconds while the model files stayed in the page cache but 40–110 seconds for 14B when read from disk; an engine's first L1 sleep pins host memory of about 1.9 times the weight size and does not release it after the wake, and the agent does one L1 prewarm right after every sleepable instance starts, so that memory is held from the start;
`/sleep?mode=abort` returns in 0.7 seconds, and in-flight requests end normally with `finish_reason: "abort"` (HTTP 200); the engine does not reject requests that arrive during sleep, but holds them until after the wake.

## Decision

**1. Every deployment has a service level: resident, online, shared, or offline.** Existing deployments are all read as resident, and their behavior is unchanged. Levels other than resident apply only to sleep-capable single-node deployments.

| Level | Yielding cards | Taking cards |
|---|---|---|
| Resident | Never yields | Once put to sleep by the platform, can make shared and offline deployments yield when it has requests |
| Online | Yields only after 5 idle minutes | Can make shared and offline deployments yield at once |
| Shared | To online and resident at any time; takes turns with its peers, each turn lasting at least Q | Can make offline deployments yield; peers go by waiting order |
| Offline | To everyone at any time, and in-flight requests are interrupted at once | Takes only free cards |

Two online deployments cannot share a card: when both are busy neither yields, and each one's requests wait on the other forever. Changing a level is rejected in that case (409 `SERVICE_LEVEL_ONLINE_SHARED`), and placement never puts an online deployment on the cards of another online deployment.

**2. There is exactly one yield rule** (`yieldpolicy.Yields`): a waiting W can make an awake H sleep if and only if H is not resident and at least one of the following holds:

- H is idle (no in-flight and no held requests) for the yield time of its level (5 minutes for online, 1 minute for shared, 10 seconds for offline);
- H can be preempted (shared or offline) and W has a higher level;
- H and W are both shared, H has stayed for Q this turn (Q = max(2 minutes, 10 × the measured switch time)), and W's head request has waited 30 seconds.

If the cards a waiter needs hold several awake replicas, they either all yield or none does: yielding only some would stop service without freeing any room. Waiters queue by level and then by how long the head request has waited, so peers are not starved. Within a level, a waiter that a request waits for (or a replica waits to be placed for) goes before a deployment that only waits to take its cards back from the pending-resume list (amended 2026-11-01: on an H20 node with three time-sliced deployments sharing the cards, a resume queued ahead of a request made the request wait for the resumer's idle time as well, stretching its first token from about 8 s to 67 s).

**3. The gateway holds requests for a deployment the platform put to sleep.** When a request reaches a sleeping deployment whose sleep was made by the platform (idle sleep, an arbiter yield),
the gateway holds the request, notifies the arbiter, and picks an instance again every time the routes are rebuilt; holding has limits on count (256 per deployment, 4096 in total) and on time (2 minutes for resident and online, 10 minutes for shared, 30 minutes for offline),
and on timeout it returns 503 with `Retry-After`. A sleep issued by an operator or a schedule is a **held sleep** (`deployments.sleep_held`): requests do not wake it and still get 503, so that a request cannot overturn an operator's decision.
Although the engine itself holds requests that arrive during sleep, the gateway does not forward requests to a sleeping engine: a wake must pass admission, and when it does not fit, the engine would hang half awake.

**4. The residency arbiter is one step of the automation evaluator** (`internal/server/automation/residency.go`), run on every tick and within 100 milliseconds after the gateway holds a request.
It first tries a normal wake (the same wake admission and waiting as the API); when that does not fit, it judges the occupants by rule 2, and once it decides to yield: offline sleeps at once with `mode=abort`;
a busy shared deployment first stops accepting new requests (the gateway holds new requests as asleep) and waits up to 30 seconds for in-flight requests to finish, aborting any still running then; an idle one sleeps with `mode=wait`.
A sleep from yielding is L1 (L2 when the host's available memory cannot hold the weights) and is not held; preemptible levels (shared, offline) are recorded as “pending resume” and take the cards back by rule 2 when they are free (amended 2026-11-01: a pending resume takes the cards back only once no one holds them, and no longer has any awake deployment yield for it. Before, it had a holder yield when idle or at the end of a turn, so two shared deployments with no requests swapped every idle time - every 18 s or so with a 15 s idle time on an H20 node - and one with steady requests stopped at every turn's end for the other that had none. The one that wants its cards back now waits for a request, and the yield rule then takes turns). Every yield and wake is written to the timeline
(`AUTO_SLEEP` with `trigger: preempt`, `reason`, and `for_deployment`; `AUTO_WAKE` with `trigger: request | resume`).

**5. Placement leaves room for sharing.** While a deployment that shares cards by service level (anything other than resident) is asleep, it is still counted at full usage against levels it does not share with (resident, or another online one when it is online),
so no deployment lands on its cards and leaves it unable to wake; against shared and offline only the residue is counted, and they can be placed there. The level can be declared at creation (`service_level`), and placement and preview compute from it.

**6. Trade-offs on state.** Persistence adds only two columns: `deployments.service_level` (text, one of four values) and `deployments.sleep_held` (boolean).
Held requests, in-flight counts, each deployment's residency start this turn, the pending-resume list, and drain progress live only in memory: after a server restart, a deployment that had yielded its cards wakes only when a request arrives.

## Consequences

- The two sharing patterns, real-time plus batch and several models taking turns, no longer need operators to arrange them by hand; every action still maps one-to-one to a timeline entry.
- When an offline or shared deployment is preempted, its in-flight requests are truncated: a non-streaming request is held by the gateway and replayed once the deployment resumes; a streaming request cannot be replayed, so the client sees HTTP 200 with `finish_reason: "abort"` and must retry on its own, and long jobs are better sent as platform-hosted batch jobs.
- Yielding uses L1: the pinned host memory is already held since the prewarm, so L1 takes no more, switches fastest and reads nothing from disk; the cost is that every sleepable deployment holds about 1.9 times its weight size in host memory from the start, which hosts running several of them must plan for.
- The first version covers only single-node deployments; multi-node and PD stay resident. When a new deployment starts and the cards are held by an awake lower-level deployment, the first version does not preempt for the start, and the deployment waits as one that does not fit, to be placed once the other yields or sleeps (replaced by the amendment at the end of this page).
- The arbiter and the gateway counts live in memory and are read per deployment without scanning all deployments; levels affect only the few deployments that share cards by service level, and the path for resident deployments is unchanged.

## Amendment (2026-10-29): One Yield Policy Replaces the Separate Service Level and Idle Sleep Settings

The text above is the original decision of 2026-10-28 and is kept as it was. In use, the service level and idle sleep turned out to answer two halves of one question, “when nobody uses this deployment, does it sleep, after how long, and to which level”, yet they lived in two places and each hard-coded part of the answer: the idle time before yielding was fixed by the level (5 minutes, 1 minute, 10 seconds) and could not be tuned; what happens to in-flight requests when yielding was fixed by the level (shared waits up to 30 seconds, offline cuts them off at once); a yield always went to L1 first; and the hold time was fixed by the level. The change is one model:

**1. One yield policy plus parameters.** A deployment has a **yield policy** that decides only who may preempt whom: never yield (`never`) > yield when idle (`idle`) > time slice (`timeslice`) > yield anytime (`anytime`). The yield rule (original decision 2) keeps its structure and only swaps “level” for the policy's rank: the holder is not `never`, and either it has been idle for its yield idle time (`idle_sec` for `idle` and `timeslice`; a fixed 30 seconds, not configurable, for `anytime`), or it can be preempted (`timeslice`, `anytime`) and the waiter ranks higher, or both are `timeslice` and the holder has held the cards for its turn and the other has waited 30 seconds. Parameters appear only when they apply:

- `idle_sec` (30–604800 s): the idle time before `idle` and `timeslice` yield, and the idle time before an auto sleep under any policy;
- `auto_sleep`: auto sleep when not preempted, replacing idle sleep. The deployment sleeps once idle for `idle_sec` even when no other deployment waits for its cards. It is a sleep the platform chose, not a held sleep, and a later request wakes it (original decision 3 is unchanged); every sleep-capable deployment may turn it on, including multi-node and PD;
- `sleep_level` (`auto` / `l1` / `l2`): the level of every sleep the platform chooses for the deployment (a yield, an auto sleep). `auto` is the behavior of original decision 4 (L1, falling back to L2 when the host cannot hold the weights); `l1` is strict L1 (refused where it does not fit, like an operator's L1 sleep); `l2` is always L2. Sleeps ordered by operators or schedules are unaffected;
- `min_serve_sec` (0–3600 s): the minimum turn of `timeslice`, still raised by the arbiter to 10 times the measured switch time;
- `unfinished` (`abort` / `wait` / `wait_unbounded`) and `unfinished_wait_sec` (1–600 s): what happens to in-flight requests when `timeslice` and `anytime` yield. The gateway holds new requests for the yielding deployment and puts it to sleep once the in-flight requests finish; `wait` waits at most `unfinished_wait_sec` seconds and then sleeps with `mode=abort` if requests are still running, and `wait_unbounded` has no limit (one very long request can keep the waiter waiting, which the documentation warns about). `idle` yields only when idle, when nothing is running; `never` does not yield;
- `hold_max_sec` (10–1800 s): the longest the gateway holds a request while a deployment wakes, replacing the 2 / 10 / 30 minutes fixed by level.

**2. Service levels are presets, derived from the stored parameters.** Resident = `never`; online = `idle`, idle 300 s, `auto`, hold 120 s; shared = `timeslice`, idle 60 s, minimum serving time 120 s, `wait` 30 s, `auto`, hold 600 s; offline = `anytime`, `abort`, `auto`, hold 1800 s; auto sleep is off in all of them. The deployment does not store a preset: the `preset` in the UI and API is derived by comparing the stored policy and parameters with the four presets item by item, a preset only on an exact match and `custom` otherwise. Creating a deployment offers only the four presets; custom is edited after creation.

**3. The rest of the original decisions stands.** Two “online” deployments cannot share a card, which now reads as two `idle` deployments cannot share a card (409 `YIELD_POLICY_IDLE_SHARED`, and placement avoids it too); policies other than `never` apply only to sleep-capable single-node deployments (`YIELD_POLICY_SHAPE`), while auto sleep stays available to multi-node and PD deployments; held sleep, placement leaving room for sharing, the reasoning for L1, and the in-memory state trade-off are unchanged. The evaluator's tick goes from 15 seconds to 10 seconds.

**4. Persistence and migration.** `deployments.service_level` and the idle sleep columns are dropped and replaced by typed columns: the yield policy, the auto sleep switch, the idle time, the sleep level, the minimum serving time, the unfinished-request handling and its wait time, and the hold limit; the preset is not stored. Migration 189 turns each existing deployment's service level into that preset's policy and parameters; a deployment that had idle sleep on gets auto sleep on, with `idle_sec` set to its old idle time (30 seconds when shorter) and `sleep_level` to its old sleep level (`l1` / `l2`), so it becomes custom, and its idle time from then on also serves as its idle time before yielding.

**5. API.** `PUT /api/v1/deployments/{id}/service-level` and `PUT /api/v1/deployments/{id}/idle-sleep` are removed, along with the deployment fields `service_level` and `idle_sleep` and the create-request field `service_level`; `PUT /api/v1/deployments/{id}/yield-policy` is added (the body is `{"preset":…}` or `policy` plus parameters), deployment details carry `yield_policy`, and the create request takes an optional `yield_policy`. `service_level` in the scheduling overview, the board occupants, and the residency timeline is renamed `preset`. The error codes `SERVICE_LEVEL_INVALID`, `SERVICE_LEVEL_SHAPE`, `SERVICE_LEVEL_ONLINE_SHARED`, and `IDLE_SLEEP_INVALID` give way to `YIELD_POLICY_INVALID`, `YIELD_POLICY_SHAPE`, and `YIELD_POLICY_IDLE_SHARED`. The timeline event codes `AUTO_SLEEP` and `AUTO_WAKE` are unchanged; the `trigger` of `AUTO_SLEEP` is still `schedule` / `idle` / `preempt`, where `idle` now means auto sleep when not preempted, and `level` is the level the platform actually chose (1 or 2).

**Consequences.**

- One settings panel answers “does it yield, when, what happens to its requests when it does, and does it sleep when nobody uses it”, with no need to compare two tabs.
- The idle time before yield anytime (formerly offline) changes from 10 seconds to a fixed 30 seconds; the other three presets behave as the former levels did.
- A deployment that had idle sleep on before the migration becomes custom, and its idle time also decides how long it must be idle before it yields.
- `wait_unbounded` leaves how long the waiter waits to the longest request; use it only when an operator explicitly accepts that.

## Second Amendment (2026-10-29): The Yield Policy Is the Only Choice

After the previous amendment there were two layers in the UI: the yield policy with its parameters, and, on top of them, the four service-level presets (resident, online, shared, offline, with “custom” for any edit). In use, the presets and the policy are the same dimension: resident is `never`, online is `idle`, shared is `timeslice`, and offline is `anytime`; choosing another preset means changing the policy, and choosing another policy turns the preset into custom. Several hard-coded or surplus parts of the design also no longer held up. The change is that the yield policy is the only choice, and where the previous amendment conflicts with this section, this section prevails.

**1. Presets are gone.** The yield policy has four values, from the highest rank to the lowest: never yield automatically (`never`) > yield when idle (`idle`) > yield on a turn (`timeslice`) > yield anytime (`anytime`). Resident, online, shared, offline, and “custom” are no longer concepts; they appear only to say what each policy suits: always-on services, online services, several models sharing in turns, batch and offline work. The `preset` request body and response field of the API are removed, and `preset` in the scheduling board, overview, and residency timeline becomes `yield_policy` (the policy name).

**2. Auto sleep when not preempted is gone.** When nobody waits for its cards, sleeping only leaves GPU memory empty and makes the deployment's own next request pay a wake; yielding is only meaningful when someone waits. So a deployment stays awake while nobody waits for its cards, and `auto_sleep`, the `AUTO_SLEEP` with `trigger: idle` (`TriggerIdle`), and the idle scan in the evaluator are removed. To sleep at fixed times, use a schedule. A multi-node or PD deployment, which can only never yield automatically, therefore has no sleep parameters at all.

**3. The idle time is common to every policy except never yield.** `anytime` used to have a fixed, unconfigurable 30-second idle yield time, while `idle` and `timeslice` each had one, and which parameters applied varied by policy. Now `idle_sec` (15–604800 s) applies to all three policies that yield: once idle (no in-flight or held request) that long, the deployment yields if someone waits for its cards. The defaults are 60 s for `idle`, 30 s for `timeslice`, and 30 s for `anytime`; the minimum drops from 30 to 15 seconds.

**4. A longest serving time replaces the least serving time.** `min_serve_sec` (0–3600 s, further raised by the arbiter to 10 times the measured switch time) becomes `max_serve_sec` (30–86400 s, default 120 s), used by `timeslice` only: when taking turns with another yield-on-a-turn deployment, the holder yields to the waiting one once it has served that long in this wake, no longer requires the other's request to have waited 30 seconds, and no longer stretches with the measured switch time. The reason is that “serve at least this long” answers the holder refusing to yield, whereas what turn-taking must really bound is a holder that never lets go; an operator-given upper limit makes the behaviour predictable. The `idle_sec` of a `timeslice` deployment may not exceed `max_serve_sec`, otherwise the idle time could never be reached.

**5. There is no unbounded wait.** `wait_unbounded` is removed: `unfinished` is `abort` or `wait` (`unfinished_wait_sec` 1–600 s, default 30 s). One very long request holds a waiter for at most 600 seconds. The longest request hold `hold_max_sec` is now 10–3600 s, defaulting to 300 s for `idle`, 600 s for `timeslice`, and 3600 s for `anytime`.

**6. Two `idle` deployments may share a card.** This used to be refused when the policy was set (409 `YIELD_POLICY_IDLE_SHARED`), and placement avoided it. Now it is allowed and the error code is removed: while both are busy neither yields, so one side's requests are held by the gateway until the other is idle, or until they reach their own `hold_max_sec` and get a 503. `idle_neighbours` in the 200 answer of `PUT .../yield-policy` lists the other `idle` deployments on the cards, and the UI shows a warning from it and suggests `timeslice` for taking turns. The refusal became a warning because such sharing is not dangerous, only slow for the side that is busy, and blocking it would leave operators no way to say “they are usually both idle, and now and then one is busy”.

**7. Yields happen when they fall due, not on a tick.** The residency arbiter keeps one timer for the next due moment (an idle time ending, a turn ending, a wait-to-finish limit running out) and looks at that moment instead of waiting for the 10-second evaluator tick, so thresholds of 15 or 30 seconds hold to about a second. For a busy holder that yields only once idle, it looks every 2 seconds whether the holder has become idle. When it looks at a waiter's holders, it does so again even while that waiter's wake is waiting for room.

**8. The server serves the dictionary of policies.** New `GET /api/v1/scheduling/yield-policies`: the four policies in rank order, each with `defaults` and the parameters that apply (`applies`), plus the range of each numeric parameter (`bounds`, where `at_most` marks that `idle_sec` may not exceed `max_serve_sec`), `sleep_levels`, and `unfinished`. The defaults, the ranges, and which parameters apply live only on the server, and the UI keeps no copy.

**9. A waiting wake clears the held sleep.** When an operator's `POST /deployments/{id}/wake?wait=true` does not fit and answers 202 `waiting`, the deployment's held sleep (`sleep_held`) is cleared at once: the deployment is meant to be awake, so a request that arrives afterwards is held by the gateway for that wake, and the residency arbiter may make room for it under the yield policies, instead of getting a 503. Before, the hold stayed until the wake landed.

**10. Persistence and migration.** Migration 190 reshapes the `deployments` columns: `auto_sleep` and `min_serve_sec` are dropped and `max_serve_sec` is added. For a `timeslice` deployment `max_serve_sec` comes from the old least serving time, at least 30 s; an `anytime` deployment's `idle_sec` becomes the 30 s that used to be built in; a `wait_unbounded` `unfinished` becomes `wait` for 600 s; a never-yield deployment that had auto sleep on becomes a plain never-yield deployment (after the upgrade it no longer sleeps by itself when nobody waits for its cards); idle times under 15 s are raised to 15 s, a `timeslice` idle time is kept at most `max_serve_sec`, and hold limits over 3600 s are lowered to 3600 s.

**Consequences.**

- One select answers “does it yield, and when”: there is no “the preset became custom” state, and the UI and the docs no longer speak two vocabularies.
- A deployment that relied on “auto sleep when not preempted” to free GPU memory no longer sleeps by itself after the upgrade; use a schedule to sleep at fixed times.
- The length of a `timeslice` turn is now an upper limit the operator states; a model that is slow to switch needs a larger `max_serve_sec` set by hand.
- Two `idle` deployments sharing a card is no longer refused, and operators need to read the warning.

## Amendment (2026-10-29): A New Deployment as a Waiter for Cards

The original decision, “when a new deployment starts and its cards are held by an awake lower-level deployment, the first version does not preempt for the start, and the deployment waits as one that does not fit” (in Consequences), is replaced by this section. In use, an online deployment created onto a card an awake batch deployment held stayed at “does not fit” until an operator made room by hand, which is the opposite of “when the realtime side comes, take the card back”. Now a new replica is a waiter for cards too.

**1. Placement plans again with the holders that share the cards counted as asleep.** When a deployment whose yield policy is not `never` starts a new replica and it does not fit, placement counts the awake instances of the deployments that share cards with it (whose policy is not `never` either) as asleep and plans again; if it fits, the replica is no longer “does not fit” but waits to be placed, with the scheduling reason code `waiting_for_yield` and the parameters `node`, `holders` (one entry per holding deployment: `deployment`, `at_once`, and when it does not give way at once also `idle_sec`, and for yield-on-a-turn `max_serve_sec`, that is, the terms on which it gives way to this new deployment) and `instances`. The terms come from the yield rule (`yieldpolicy.TermsOf`), shared by placement, the arbiter and the UI wording.

**2. The residency arbiter treats the replica like a request held for a sleeping deployment, with the new deployment's policy.** The holders sleep under the same yield rule: when the new replica ranks higher, yield-on-a-turn and yield-anytime holders give way at once; otherwise a holder gives way once idle for its idle time; between two yield-on-a-turn deployments it gives way when the holder has served its longest serving time this turn. A new deployment with `never` waits for nobody and still needs free cards; a holder with `never` is never asked to give way.

**3. The controller plans again every 5 seconds.** A replica with a node pool takes a node that needs a yield only when no node fits outright. The wait lives in memory only and adds nothing to persistence; after a server restart the controller notes it again on its first pass.

**4. A holder that gave way does not take the cards back.** The deployment that yielded does not take the cards back while the new replica is being placed and started (up to 10 minutes; loading a model can take minutes); the new replica's turn and idle time count from when it begins to serve, otherwise it would be made to give way again right after waking for having “served enough” or “been idle”.

**Consequences.**

- Measured in the process test: an online (`idle`) deployment created onto a card an awake yield-anytime (`anytime`) deployment held started serving about 6 seconds after creation, and the yield-anytime deployment was put to sleep by rank.
- Multi-node and PD deployments must still use `never` and take no part; when their cards are held they wait as “does not fit”, as before.
- A new deployment's replica can thereby make others sleep; the action is still written to the timeline (`AUTO_SLEEP`, `trigger: preempt`, `for_deployment` being the new deployment).

## Amendment (2026-10-30): A Held Sleep Keeps No Card

Until now, when a deployment that shares cards by yield policy was asleep, its whole claim was kept against deployments it does not share with (`never`), whoever had ordered the sleep. In use, after an operator put a sharing deployment to sleep by hand, a new deployment pinned to that card was never placed, which read as "no empty card", although a request could not wake that sleep anyway. This is changed:

**1. Cards are kept only when a request can wake the sleeper.** The reservation applies only to a sleeper whose `Deployment.WakesOnRequest()` is true: the deployment is sleepable and the sleep is not a held sleep (`sleep_held`, a sleep an operator or a schedule ordered). The gateway (hold the request or answer 503), the residency arbiter and placement ask the same question, which has one answer.

**2. A held sleep keeps no card.** A request cannot wake it, so its cards are open to any new deployment; the later wake by the operator or the schedule already has to wait for room (`?wait=true`) or make it (`?yield=true`), and not fitting is a stated semantic, not a defect.

**3. A kept card says whose it is.** When a new deployment does not fit only because sleepers keep their cards, the reason is the new scheduling reason code `reserved_for_sleepers`, with the parameters `node`, `gpus` and `sleepers` (each with `deployment` and `policy`), and the UI gives three ways out: pick another card, turn on sleep for this deployment with a yield policy that can share cards, or put the deployment holding the cards to sleep by hand.

**Consequences.** A deployment put to sleep by hand may have its cards taken before it is woken; this agrees with "the capacity a sleep gives up is really given up". The yield policies and the yield rule are unchanged.

## Amendment (2026-10-30): A Held Sleep Is Recorded per Instance

The original decision recorded the held sleep on the deployment (`deployments.sleep_held`), which caused two defects: waking one instance cleared the hold of the whole deployment, so the other instances the operator still wanted asleep became wakeable by a request; and while a deployment was held, a replica the residency arbiter put to sleep to yield cards (not held) was also treated as not wakeable and was never resumed.

**1. The hold is a fact of the instance.** Migration 191 adds `instances.sleep_held` (boolean), backfills it as held for the instances ordered asleep in a deployment that was held, and then drops `deployments.sleep_held`, so the hold has one source. A held sleep holds only the instances it names (every instance for a deployment-wide or scheduled sleep, that role's for a role sleep, that replica for a replica sleep, the yielded replicas for a yielding wake); a wake clears the hold of only the instances it wakes or leaves waiting; and the platform's sleep does not touch holds. A deployment-wide sleep order is always held: only an operator or a schedule puts a whole deployment to sleep, while the platform names replicas; an instance started under such an order is held too.

**2. Whether a request wakes it is asked per instance.** `Deployment.WakesOnRequest(instance)`: the deployment is sleepable and that instance is not held. Every deployment-level answer is derived from it: the gateway holds a request while the deployment has a sleeper a request can wake and answers 503 at once when every sleeper is held; batch model states, the Playground's `wakes_on_request`, the cards placement keeps, and the residency arbiter's wakeable check and resume all do the same. A wake driven by a request or a resume wakes only the instances that are not held; an operator's or a schedule's wake wakes the instances it names and clears their holds.

**3. The gateway reads nothing more for it.** When the gateway finds a deployment asleep it has already read that deployment's instance rows for the request (the snapshot carries only awake candidates); the hold flag is in those same rows, the verdict is recorded with that refusal, and `holdForWake` reads it directly, with no new query.

**4. The deployment DTO carries only derived values.** `sleep_hold` (`all`/`some`/`none`, omitted with no replica asleep) is derived from the asleep replicas' holds, and `sleep_held` is kept as `sleep_hold == all`; the instance DTO adds `sleep_held`. The UI shows its hint from `sleep_hold` and tags each replica from the instance's `sleep_held`, deciding nothing itself.

## Amendment (2026-11-03): An Operator's Sleep Is Held or Not by Yield Policy

The original decision made every sleep an operator ordered a held sleep. In use, on an H20 with two yield-when-idle deployments sharing cards to run batches, an operator put one of them to sleep by hand (to free the cards or to experiment) and requests could never wake it again: it dropped out of the turn-taking. Waking it by hand did not fit, so the only choice was a yielding wake, which in turn made the other deployment a held sleep, and the two could never switch automatically again. What the operator usually means is "sleep for now", not "overturn this deployment's yield policy". Changed to:

**1. The operator chooses whether the sleep is held, defaulting by yield policy.** The sleep commands for a deployment, a role and a replica accept `wake_on_request` (`true`: a request can wake it; `false`: held sleep). When omitted it takes `planning.OperatorSleepWakesOnRequest`: a request can wake it when the deployment is sleepable and its yield policy is not `never`, otherwise it is held. Reason: choosing a policy that yields means agreeing to give cards on demand and take them back on demand; a deployment that never yields takes no turns with others in the first place, and an operator who puts it to sleep wants it asleep. The deployment DTO gives this default as `sleep_wakes_on_request`, and the UI's sleep dialog uses it as the initial value of "Wake it when a request arrives" without judging anything itself. A scheduled sleep is unchanged and always held.

**2. The deployment-wide command is still only for held sleep.** The deployment-wide sleep command (`deployments.desired_sleep`) makes instances started under it sleep and be held too, so it only expresses a held sleep; when the operator chooses a deployment-wide sleep that requests can wake, the platform orders the awake replicas one by one, the way the residency arbiter does.

**3. A yielding wake applies the same rule to the occupants.** An occupant that gives way with a yield policy other than `never` is not held: a request can wake it and it takes its cards back by the rules; a `never` one is held as before. Besides `yielded`, the response gains `yielded_held`, listing those held; the timeline's `YIELDED_FOR_WAKE` records `wake_on_request`.

**4. An existing hold can be cleared without waking.** Added `POST /api/v1/deployments/{id}/wake-on-request`: it clears the hold of every instance of the deployment (`store.ReleaseSleepHolds`) without changing any instance's sleep intent, and withdraws the deployment-wide sleep command at the same time (it is always held, and leaving it would put replicas started later to sleep and hold them again). The timeline records `SLEEP_HOLD_RELEASED` (with the operator), and the scheduling history counts it as a manual action. Before, the only way around was "wake and wait for room, then cancel the wait".

**Consequences.** A deployment whose yield policy yields, put to sleep by hand, still keeps its cards for a request's wake (the rule for keeping cards is in the previous amendment and looks only at whether a request can wake it); an operator who wants it to really release the cards to a deployment that does not share must explicitly choose held sleep. The batch model states, the playground hint, the gateway and the residency arbiter all judge by the instance's hold and need no change.

## Amendment (2026-11-13): After a Server Restart, Read Back the Scheduling State That Can Be Told

Decision 6 of the original says held requests, each turn's start, the pending-resume list and drain progress live in memory only, and that after a server restart a deployment that yielded its cards is woken only when a request comes. Killing the server with SIGKILL on the mainline showed: the state requests drive comes back once clients retry, but a deployment waiting to resume never takes its cards back once they are free, and a yield-on-a-turn holder serves a whole new turn after the restart. On a graceful stop, held requests were dropped when the 5-second graceful shutdown ended, so clients saw a broken connection instead of a refusal they may retry. It is changed:

**1. No new persistence; only facts already in the events are read back.** On its first look after starting, the arbiter reads the pending-resume list back from the event log: a deployment whose yield policy is preempted (yield on a turn, yield anytime), that has a sleeper a request wakes, and whose newest sleep or wake event (the newest of `AUTO_SLEEP`, `AUTO_WAKE`, `YIELDED_FOR_WAKE`, `SLEEP_ORDERED`, `WAKE_ORDERED`) is `AUTO_SLEEP` with `trigger: preempt`. One an operator touched since is not read back. A yield of the last 10 minutes is read back with its guard (do not take the cards back while the other still wakes); a failed read is tried again on the next look.

**2. A turn starts where the instance events say.** A yield-on-a-turn holder's turn runs from when it last began serving, as its instance state and sleep events (the residency timeline) say, looking back at most one turn; a later wake the arbiter saw itself takes precedence. This also corrects a skew without a restart: a holder woken by an operator or a schedule had its turn counted from the server's start, and now counts it from its wake.

**3. Idle time is not read back.** A request record is written when the request ends, so a request cut off by the restart leaves no record of having run, and the stored data cannot show that a holder was idle before the restart; reading it back would make a deployment that was busy when the server stopped yield at once, against "yield only once idle". Idle time still counts from the process's start, which makes a waiter wait at most one idle time longer.

**4. On a stop, held requests get a 503.** When the server stops gracefully (an upgrade, `restart`, the service manager stopping it), the gateway ends the held requests at once with a 503, `Retry-After: 10` (about one restart) and "the server is restarting; send the request again"; a request arriving afterwards that would be held is refused the same way. Requests already being forwarded are not touched and still drop when the graceful shutdown ends. Nothing can be done on SIGKILL: the client sees a dropped connection.

**5. Drain progress and the held requests themselves stay in memory.** With drain progress gone, the replica takes requests as usual after the restart and is judged again once a waiter appears; held requests go with their connections and return to the queue when the client retries. Node replacement's undo record is likewise read back from the events; see the 2026-11-13 amendment of [ADR 0058](0058-scheduling-center-acts-through-operator-orders.md).

**Consequences.**

- A restart or an upgrade no longer costs a deployment waiting to resume its chance to take its cards back on its own, and taking turns does not hand out an extra turn because of the restart.
- Clients retry on connection errors and on a 503 with `Retry-After`, with a total backoff that covers a restart.
- The process-level case `TestProcessResidencyAcrossAServerStopAndKill` covers a graceful stop, SIGKILL and a resume.
