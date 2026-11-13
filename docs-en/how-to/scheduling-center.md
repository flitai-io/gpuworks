# Use Scheduling Analysis: Schedules, Yield Policies, and Waiting Wakes

This guide explains how to have the platform carry out sleep, wake, and replica-count changes you agreed on in advance, and how to read the scheduling board to see who holds each card, what is waiting, and what is about to happen.

**Use case**: several sleep-capable models take turns on the same GPUs (for example one model by day and another at night), or a real-time model and a batch model share one set of cards.

**Prerequisites**:

- Scheduled sleep, scheduled wake, and yield policies require the deployment to be **sleep capable** (vLLM, enabled at creation); see [Sleep and Wake Operations](sleep-wake.md) for the conditions.
- A scheduled replica count applies to single-node deployments and to multi-node and PD deployments placed automatically (`placement_mode` `auto`; a PD deployment is scheduled one role at a time); multi-node and PD deployments placed by hand are not supported.
- Yield policies other than never yield automatically apply only to sleep-capable single-node deployments; a multi-node or PD deployment must use never yield automatically.
- Scheduling analysis does not scale by load. The platform makes one deployment give up its cards for another only when its yield policy allows it; see [Yield policy](#出让策略).

## What Scheduling Analysis Does

| Feature | What you agree in advance | What the platform does when due |
|---|---|---|
| Schedule | Weekdays, HH:MM, and time zone, plus sleep L1/L2, wake, or a replica count | Runs the same API action once at that time |
| Waiting wake | Choose “wait” when a wake does not fit | Wakes automatically once cards free up; gives up after 30 minutes |
| Yielding wake | Choose “put the occupants to sleep, then wake” when a wake does not fit | Puts the occupants to sleep first, then wakes through waiting |
| On-demand wake | Nothing to set: deployments the platform put to sleep itself (yielding cards) | When a request arrives, the gateway holds it, wakes the deployment, and lets the request through once it is awake |
| Yield policy | Never yield automatically, yield when idle, yield on a turn, or yield anytime, plus the parameters that apply | Deployments sharing a set of cards yield cards by policy and take turns |

They carry out the same actions you would take by hand: the same lifecycle lock, the same eligibility checks and wake admission. An automatic wake and your manual **Wake** cannot both pass admission for the same card. Every automatic action is written to the deployment timeline; see [Timeline Events](#时间线事件).

The **Scheduling policy** card in deployment details keeps these settings in one place. Collapsed, it shows one summary line: the wait for node-loss rescheduling, the number of schedule entries and the next run, and a summary of the yield policy (not shown when the deployment cannot sleep). Only admins see **Edit**; it opens one tab per setting, each saved on its own. For node-loss rescheduling, see [Reschedule Replicas on Node Loss](node-failover.md).

## Scheduling Board

Open **Scheduling Analysis** from the “Overview” group of the left menu (`/scheduling`, right below “Usage analytics”). The upper half shows the whole cluster, the lower half the queues and the history:

- **Summary strip**: the total number of cards in the cluster; how many are serving, asleep, free, held by a process outside the platform, faulty, or on an offline node (six disjoint classes that add up to the total); and contention - wakes waiting for room, requests the gateway holds for a wake right now, yields in the last hour - plus the deployments waiting for placement and the schedule actions in the next 24 hours. These figures are computed by the server for the whole cluster and do not change with the filters below.
- **GPU heatmap**: one small cell (about 12 px) per card, grouped by node; 1000 nodes of 8 cards fit on one or two screens. The colour is the card's state, with the legend at the top right of the card:

  | Colour | Meaning |
  |---|---|
  | Blue, deeper = more claimed | Serving: an instance holds memory on the card; the depth is the share of memory claimed |
  | Purple | Asleep: only the residue of sleepers is on the card |
  | Light green with a green border | Free |
  | Orange | Held by a process outside the platform |
  | Red | Faulty, or unusable because of its ownership or health |
  | Grey, hatched | Its node is offline (as last reported, so possibly stale) |
  | Black inner outline | An instance on the card is starting, going to sleep, waking or draining |

  A card has exactly one state, which the server derives in a fixed order (its node is offline, faulty, held by a process outside the platform, serving, asleep, free: the first that holds); the summary, the heatmap colours and the card states in the node drawer all use that one result, and the page only maps a state to its colour and words.

  Hover or keyboard-focus a cell to see the card's details: node, card index, model, memory, the shares that are serving, asleep and free, and its occupants (deployment, yield policy, serving or asleep). The heatmap itself carries no occupant detail, so even a thousand-card fleet costs one read of about 290 KB (about 25 KB gzipped).
- **Filters and view**: filter by GPU model (several allowed), node label (`key=value`) and node state (has free cards, has sleepers, has cards held outside, offline); type a part of a deployment name in "Find a deployment" and the cards it occupies get a red ring while the rest fade, with a count of the occupied cards. Group by node (the default), by GPU model, or by the value of a node label; sort by name, by free capacity (the unclaimed memory of the cards, not counting offline, faulty or externally held cards) or by contention (sleepers waiting to wake, plus the requests held for deployments asleep on the node, plus the yields of deployments that have cards on it). **Clear filters and view** returns to the default.
- **Node drawer**: click a cell, or a node name anywhere on the page ("Cards that switched most" beside the scheduling activity, the "Node and GPUs" column of the sharing table, the "Node" column of the scheduling history), and a drawer opens that node: a bar per card split into serving usage, the residue memory of sleepers and free memory, the occupants of each card (deployment and its yield policy, serving or asleep, sleep level, claim and residue), cards held by processes outside the platform marked, and below them the node's [residency timeline](#驻留时间线). The drawer reads only that one node, and only when it is open. The open node is in the address (`/scheduling?node=<node UUID>`), so a link to it opens it directly; closing the drawer removes the parameter, and the browser's Back button returns to the node opened before.
- **Scheduling activity**: below the heatmap, a bar chart of scheduling events stacked by reason, with the cards that switched most over the same range beside it; see [Scheduling Activity](#调度活动).
- **Deployments waiting for placement**: deployments the scheduler refused in the last 30 minutes, with reasons written the same way as the placement status in deployment details. A new replica waiting for deployments that share its cards to give them up shows the reason `waiting_for_yield` (see [How a New Deployment Gets onto a Shared Card](#新部署怎样放到共用的卡上) below); when it does not fit only because sleeping deployments keep their cards for a request's wake, the reason is `reserved_for_sleepers` (same section).
- **Waiting wakes**: which deployments are waiting and since when; the wait can be cancelled.
- **Schedule actions in the next 24 hours**: time, deployment, action.
- **Deployments sharing cards**: the stats, over a window of 15 minutes, 1 hour, 6 hours (the default) or 24 hours, of the deployments across the whole cluster whose yield policy is not never yield; see [Residency Timeline](#驻留时间线).
- **Scheduling history**: when, why and what was scheduled; see [Scheduling History](#调度记录).

The heatmap works from the keyboard alone: Tab enters it and the focus rests on one cell; Left and Right move between cards (continuing across nodes), Up and Down move between neighbouring nodes at the same position, Page Up and Page Down jump ten nodes, Home and End go to the first and last card; Enter or Space opens that node's drawer. The page refreshes every 10 seconds and redraws nothing when the cluster has not changed.

The server keeps the state of the whole cluster in a memory cache. Changes to nodes, instances and deployments invalidate it, but it is rebuilt at most every 2 seconds, so the status reports of a large fleet do not make every refresh recompute it. The API is:

```bash
curl --fail-with-body -sS "$API_URL/api/v1/scheduling/overview" -H "Authorization: Bearer $TOKEN"

curl --fail-with-body -sS "$API_URL/api/v1/scheduling/board?node=$NODE_UUID" -H "Authorization: Bearer $TOKEN"
```

The overview's structure is in the [API reference](../reference/api.md#调度总览). `GET /api/v1/scheduling/board` can still be read page by page (`page`, `page_size`, `q`; `q` matches part of a node's name, hostname or UUID, case-insensitively); the main structure of its response is below (`nodes` is a paged object; the other three items cover the whole cluster):

```json
{
  "nodes": {
    "items": [{
      "uuid": "…", "name": "host2", "online": true,
      "gpus": [{
        "index": 0, "name": "RTX 8000", "memory_bytes": 51539607552,
        "awake_bytes": 0, "sleeping_bytes": 1610612736, "free_bytes": 49928994816,
        "state": "asleep", "held_outside": false, "switching": false,
        "occupants": [{
          "deployment_id": 7, "deployment": "qwen", "yield_policy": "timeslice", "instance": "…", "state": "asleep",
          "sleep_level": 2, "claim_bytes": 38654705664, "residue_bytes": 1610612736
        }]
      }]
    }],
    "page": 1, "page_size": 20, "total": 3
  },
  "pending": [], "waiting_wakes": [], "upcoming": []
}
```

The real `free_bytes` and the other sizes are integer bytes, and the values above are only illustrative; `residue_bytes` appears only on sleeping occupants. The `state` of each card takes the same values as the overview (`offline`, `unhealthy`, `held_outside`, `awake`, `asleep`, `free`), and `switching` says an instance on the card is starting, going to sleep, waking or draining. The board no longer carries `recent` (the latest automation events); read the scheduling history below for those.

### Residency Timeline

The **Residency timeline** in the node drawer draws a horizontal time axis for every card of that node: one lane for each deployment that held the card, with blocks coloured by the state it was in during that stretch:

| State | Meaning |
|---|---|
| Serving | The instance was running and could serve requests |
| Asleep | The engine slept and only its residue stayed in memory; L1 or L2 is shown |
| Switching | Between a sleep or wake order and its result |
| Starting | Being created, pulling its image or loading; not serving yet |

The time ticks are drawn to scale. Hover a block, or focus it with the keyboard, to see the deployment, the state and the start and end times. The window is chosen with the platform's time picker: the quick windows of 15 minutes, 1 hour, 6 hours (the default) and 1 day, or a custom start and end (at most 7 days, the end not after now). In both places the windows offered come from the server with the response (`windows`), and the console shows them; any other `window` in a request returns 400 `FIELD_INVALID`, with a message naming the windows offered. A quick window refreshes every minute; a custom range is a fixed stretch, read once and not refreshed.

**Cards that change hands often**: when a card has more than 200 segments in the window (for example two time-sliced deployments swapping every dozen seconds), the timeline draws the newest 200 segment by segment, and the server summarizes the earlier time into at most 50 equal time slices, drawn in each deployment's own lane: every deployment has one lane across the whole window, with its hatched blocks for the slices left of the summarizing line and its segments to the right, and there is no separate summary lane. A block is coloured by the state the server names as the one the deployment spent the most time in during the slice (serving, asleep, switching or starting, in the same colours as the segments; on a tie of the time, the first of serving, switching, starting, asleep wins), which the page does not work out itself; the legend gains one item, "Summarized (earlier)", that explains the hatch. A dashed line marks on every lane where summarizing ends, and a note beside the card title says "Summarized before ...". A deployment that held the card only in the summarized time has its own lane too. Hover a block, or focus it with the keyboard, for the slice's start and end, how many times the card changed hands, and how long this deployment spent serving, asleep, switching and starting (states it spent no time in are left out), for example "09:00 to 09:26 · this card switched 85 times · this deployment serving 12 min · asleep 13 min · switching 1 min". A card "changes hands" when the awake deployment on it becomes another one: a swap counts once, not as a yield plus a wake as in the per-deployment "Switches" of the sharing table below, and a deployment going to sleep and waking again is not a change of hands. Slices in which no deployment held the card are not drawn. A card with 200 segments or fewer is drawn in full, with no hatched blocks or dashed line.

In a custom range, an instance created after the end of the range is not shown; one removed after the end still holds its card until the end; and each instance's state at the start comes from the newest event before it. The time axis picks its ticks from the length of the range: a few minutes for 15 minutes, 10 minutes for an hour, whole hours for several hours up to a day, with the date added beyond a day and one tick a day for a week. The answer for a range that has already ended is cached for about 5 minutes for the same page of nodes, so opening it again does not read the event log again; the answer for a quick window is cached for about 30 seconds, so several people looking at the same page share one read, and the timeline can be up to 30 seconds behind now.

The switches, yielded and yielded-to figures in the "Deployments sharing cards" table come from the scheduling activity, which keeps the last 24 hours only: when a custom range begins earlier than that, those three columns show a dash and a line above the table says so; the other columns (serving time, held requests and so on) are unaffected.

The **Deployments sharing cards** table on the page lists the deployments across the whole cluster that held cards in the window and whose yield policy is not never yield (the drawer shows the same table under its timeline, limited to the deployments that held that node's cards). Deployments deleted since that gave a card up or were given one in the window are listed too, their name followed by "(deleted)" and greyed, with no link and — for the level, as the [scheduling history](#调度记录) shows deleted deployments: they did share cards in the window, and deleting them should not make other deployments' "Yielded to" shrink.

| Column | Meaning |
|---|---|
| Node and GPUs | The nodes and GPUs its replicas are on now, one line per node; a node name is clickable and opens that node's drawer, a node that no longer exists shows as `#id` and is not clickable; — when none is placed |
| Serving time, share | The time at least one replica was serving / the time the deployment existed in the window, and the first as a share of the second. It existed from the later of the window's start and its creation to the earlier of now and its deletion, so a deployment created partway through the window and serving since shows close to 100%, not its share of the whole window |
| Switches | Times it yielded plus times the platform woke it |
| Yielded, yielded to | Times it was put to sleep to give a card to another deployment, and times another deployment gave a card to it |
| Held requests, wait p50 / p95 | Requests the gateway held while it woke, and the median and 95th percentile of their waits |

The endpoint is separate so that it does not slow down the overview, which refreshes every few seconds:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/timeline?node=$NODE_UUID&window=6h" \
  -H "Authorization: Bearer $TOKEN"

curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/timeline?node=$NODE_UUID&since=2026-10-28T06:00:00Z&until=2026-10-28T09:00:00Z" \
  -H "Authorization: Bearer $TOKEN"

curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/timeline?sharing=1&window=1h" \
  -H "Authorization: Bearer $TOKEN"
```

What the figures mean and where they stop:

- **A yield is counted one way**: the overview's "yields in the last hour", the timeline's "Yielded" and "Yielded to", and the shared-cards table count the same kind of event: the residency arbiter putting a holder to sleep (a yield), and an operator's yielding wake (`?yield=true`) putting an occupant to sleep. An operator's yielding wake also records which deployment it was for (`for_deployment`), so it links to that deployment in the [scheduling history](#调度记录) and counts in that deployment's "Yielded to" too.
- The lanes are derived from instance records and the event log. Removing an instance now also records an `INSTANCE_REMOVED` event naming the node and cards it held; instances removed before the upgrade have no such record and do not appear in past segments.
- The figures in the table (serving time, time existed, switches, yielded, yielded to) come from the rollups of the [scheduling activity](#调度活动), not from the event log: they are counted as the events are written, the last 24 hours kept in memory and replayed from the event log once when the server restarts.
- The drawer's table lists only the deployments that appeared on that node's cards, and counts only their replicas on that node: the serving and present time come from its segments in that node's lanes (present is the time at least one of its replicas held cards on the node), and the switches, yields, yields to it and wakes count only the events that concern cards on the node; the ⓘ icon beside the table's title (on hover or keyboard focus) says "These figures count only this node's replicas", shown from the scope the server returns. The page's sharing table still gives each deployment's figures across the cluster (all its replicas), so for a deployment with replicas on several nodes the drawers' figures add up to the sharing table's (except awake and present time where the nodes overlap). The held requests and their waits are what the gateway recorded for the whole deployment and are the same in both; "Node and GPUs" shows where all its replicas are now. "Yielded to" goes by the `for_deployment` the yield recorded, also when the one that gave way has been deleted. The result of `sharing=1` is cached for about 30 seconds.
- A deleted deployment's creation time and yield policy are not kept: the time it existed counts from the earliest its instances appear in the window, up to its deletion.
- A follower of a multi-node replica follows its leader's sleep and wake.
- At most the latest 200 segments of a card are returned; older ones are left out and the console says so.
- The wait quantiles approximate the window: the gateway's quantile of each flush interval, weighted by the requests of that interval, not an exact per-request quantile of the window. The counts start again from zero when the gateway restarts, and both columns stay empty when no request was held in the window. The metric the gateway records is `wake_hold_wait_ms`, see [Inference Metrics](../reference/inference-metrics.md).

## Scheduling Activity

The **Scheduling activity** card below the GPU heatmap on the Scheduling Analysis page shows, as stacked bars, why the platform scheduled over a range:

| Reason | Events counted |
|---|---|
| Idle yield | The residency arbiter put an idle holder to sleep for a deployment waiting for its cards |
| Rank preemption | A higher-ranked deployment preempted a preemptible holder (a preemption that names no reason counts here too) |
| Time-slice turn | Equals took turns and the holder had used its time slice |
| Operator yield | An operator's yielding wake put an occupant to sleep (`YIELDED_FOR_WAKE`) |
| Schedule | A schedule's sleeps, wakes and replica changes |
| Placement & moves | The platform placed a new replica on a node (`UNIT_PLACED`) or moved one off a node an operator drains (`NODE_DRAIN_MOVED`) |
| Failure (rebuilt / replaced / timed out) | Replicas rebuilt, node failover moves (with their reverts and the moves with nowhere to go), retry budgets exhausted, rolling restarts stalled, wake waits that timed out, automatic orders that failed |
| Unschedulable / queued | A deployment that cannot be placed, a wake that starts waiting |

Wakes a request or a resume brought (the other half of a yield, already counted once on the side that gave way, though still a switch of the card) and operator orders count under no reason; they are all in the [scheduling history](#调度记录). A move off a lost node counts as a failure. The reasons and their order come from the server; the console only colours and words them.

- **Range**: the last 24 hours by default; the quick ranges are 15 minutes, 1 hour, 6 hours, 1 day and 1 week (the same as the scheduling history's), or a custom start and end. The server picks how long a bar is for the range: 1 minute for 15 minutes and 1 hour, 5 minutes for 6 hours, 15 minutes for 24 hours, 1 hour for a week, and for a custom range the shortest that keeps it within about 200 bars.
- **Cards that switched most**: the cards that changed hands most over the same range (node, card index, count), as many rows as fit the chart's height; click a node name to open that node's drawer. A yield or a wake on a card is one switch, by the cards the event's instances held at the time.
- **Retention**: the last 24 hours are counted in the server's memory, by the minute; older figures are written to stats.db by the hour, kept 31 days by the hour and 400 days by the day (the [data retention](../reference/api.md#analytics用量分析) setting shared with usage analytics' long-term statistics). When a custom range begins before the daily retention, the card says it is beyond the statistics retention, and that part has no figures.
- **Scale**: neither the chart nor the list reads the event log. Each event adds a few numbers to the minute it fell in as it is written, and each hour is written to the database once; with 1000 deployments each switching 10 times an hour, a day of events and a week of hourly rollups, reading 24 hours or a week takes at most a few hundred milliseconds, and answers are cached for about 30 seconds.

The API is `GET /api/v1/scheduling/activity`; see [the scheduling activity API](../reference/api.md#调度活动):

```bash
curl --fail-with-body -sS "$API_URL/api/v1/scheduling/activity?window=7d" -H "Authorization: Bearer $TOKEN"
```

## Schedules

Each deployment can have any number of entries, each of which is “do an action at a time on certain weekdays”. In the **Scheduling policy** card in deployment details, click **Edit** and add them on the **Schedule** tab:

1. Choose the action: **Sleep** (pick L1 or L2), **Wake**, or **Replica count** (1–32, single-node deployments and automatically placed multi-node and PD ones only; a PD deployment also asks for a **role**, see below).
2. Tick the weekdays and enter the time (HH:MM).
3. The time zone defaults to the server's own; each entry can use a different IANA time zone (such as `Asia/Shanghai`).
4. Save. You can disable, edit, or delete an entry at any time; the list shows the next run time.

Which actions can be added is told by the server for each deployment (`schedule_capabilities` in the deployment details): a deployment whose engine cannot sleep (**Sleep capable** is off, or the engine does not support sleep) offers no **Sleep** or **Wake**, and the deployment details show no sleep or wake controls either. The range of **Replicas** (1 to `max_replicas` for a single-node deployment, per role for a PD deployment, down to 0 for a role its mode allows to be empty) and the reason a replica count cannot be scheduled (manual placement, for example) also come from the server; the page does not work them out itself.

The API lives under `/api/v1/deployments/{id}/schedule`:

```bash
# 每个工作日 22:00（东八区）休眠到 L2
curl --fail-with-body -sS -X POST \
  "$API_URL/api/v1/deployments/7/schedule" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2}'

# 每个工作日 08:30 唤醒
curl --fail-with-body -sS -X POST \
  "$API_URL/api/v1/deployments/7/schedule" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"wake","weekdays":[1,2,3,4,5],"time":"08:30","timezone":"Asia/Shanghai"}'
```

Creating returns 201 and the entry:

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| Route | Description |
|---|---|
| `GET /deployments/{id}/schedule` | List, `{"items":[…],"total":n}` |
| `POST /deployments/{id}/schedule` | Create, 201 |
| `PUT /deployments/{id}/schedule/{sid}` | Replace an entry, 200; the body is the same as for creating |
| `DELETE /deployments/{id}/schedule/{sid}` | Delete, 204 |

Request fields: `action` (`sleep` / `wake` / `replicas`), `weekdays` (0 for Sunday through 6 for Saturday, at least one, no repeats), `time` (`HH:MM`), `timezone` (an empty string uses the server's time zone), `sleep_level` (only for `sleep`, required), `replicas` (only for `replicas`, required), `role` (only for `replicas`: required for a PD deployment, refused for any other), and `enabled` (default true).

### What Happens When an Occurrence Is Due

- **Each occurrence runs once.** The evaluator looks every 10 seconds and runs an entry when its newest occurrence is later than the one it last handled.
- **Catch-up**: for occurrences missed while the server was down, only the newest one runs after it starts, and that one may not be older than 12 hours; older ones are only marked handled, not run.
- **Manual operations are not overridden repeatedly**: an entry acts once at its time, and if you then wake or sleep by hand, the entry does not change it back on the next round.
- **A new or edited entry takes effect from its next occurrence** and does not look back at earlier ones.
- When two entries of one deployment are due together (the server was down across both), the earlier runs first and the newer runs last and stands.
- **An entry in a time zone the server does not know does not run and has no next run time.** A time zone the server does not know is rejected when an entry is created or edited (`SCHEDULE_TIMEZONE_UNKNOWN`), so this only happens when the server later loses that zone's data; the API gives no `next_at` for such an entry and the evaluator does not run it, so the two agree and the entry is never quietly run in the server's zone instead.
- When an action fails (the deployment cannot sleep, it does not fit, and so on), the timeline records `SCHEDULE_ACTION_FAILED` and the occurrence still counts as handled; it is not retried on every tick.
- **A scheduled wake that does not fit**: replicas that fit wake first and the rest go into a waiting wake (timeline `WAKE_WAITING`) rather than failing outright.

### Validation When Creating

| Case | Response |
|---|---|
| Missing fields, weekdays out of range or repeated, a bad time format, `sleep` without a level, `replicas` out of range (1–32; 0–32 for a PD role the mode lets be empty) | 400 `SCHEDULE_INVALID`; `params.field` and `params.reason` say what is wrong |
| A `replicas` entry of a PD deployment with no `role` or a role its mode does not have; a `role` on a deployment that is not PD; a `role` on a `sleep` or `wake` entry | 400 `SCHEDULE_INVALID` with `params.field` `role` (for a PD deployment `params.roles` lists the roles to choose from) |
| The server does not know the time zone | 400 `SCHEDULE_TIMEZONE_UNKNOWN`, `params.timezone` |
| A replica count for a multi-node or PD deployment placed by hand | 400 `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` |
| The deployment is not sleep capable or the engine cannot sleep (`sleep` and `wake` actions) | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |

**A scheduled replica count for a multi-node deployment** takes the same path as the "grow or shrink by count" of `PUT /api/v1/deployments/{id}/replicas`: when growing, the server picks nodes for the new replicas by the placement strategy (the rules of [how nodes are picked at creation](multi-node-inference.md#创建时怎样挑节点)); when shrinking, the replicas added last go. If nothing fits, the `reason_code` of `SCHEDULE_ACTION_FAILED` is `UNIT_PLACEMENT_NONE`. A multi-node deployment placed by hand lists the nodes of every replica, so someone has to name the nodes of an added replica, and it is not supported.

**A scheduled replica count for a PD deployment is set per role**: the entry's `role` is a role of its mode (such as `prefill` or `decode`) and `replicas` is the number of instances of that role; the other role is left alone. When due it takes the same path as `PUT /api/v1/deployments/{id}/pd`: when growing, the server picks nodes for the new units beside the placed ones (as when an automatically placed PD deployment is created); when shrinking, the units added last go. If nothing fits, the `reason_code` of `SCHEDULE_ACTION_FAILED` is `UNIT_PLACEMENT_NONE`; any other failure (say, the role's units span several nodes while a new unit gets one) keeps the PD update's own error code and `reason`. A role's minimum holds as well: a role the mode lets be empty (`prefill` in `vllm-nixl`) may be scheduled to 0, every other role needs at least 1, so a number below the minimum is refused when the entry is created; `decode` at 0 returns 400. A deployment placed by hand is refused likewise (`SCHEDULE_REPLICAS_MANUAL_PLACEMENT`).

If a deployment is switched to manual placement after its schedule was written, the entry fails when it is due: the timeline records `SCHEDULE_ACTION_FAILED` (`reason_code` `SCHEDULE_REPLICAS_MANUAL_PLACEMENT`) and the entry itself stays, so it runs as usual at the next occurrence after the deployment goes back to automatic placement.

## Waiting Wakes and Yielding Wakes

When a wake is refused with `WAKE_INSUFFICIENT_GPU_MEMORY`, the console offers two choices, corresponding to `?wait=true` and `?yield=true`; for usage and responses see [Sleep and Wake Operations](sleep-wake.md#放不下时等位与让位). In brief:

- Waiting takes only room that is already there and makes no one give way; once capacity is released the server usually wakes the deployment within 10 seconds, and gives up after 30 minutes with `WAKE_WAIT_TIMEOUT` in the timeline.
- Yielding puts the occupants to sleep first; if an occupant cannot sleep, the whole request is refused with 409 `WAKE_YIELD_BLOCKED` and nothing changes.
- The time after which a wait gives up comes from the server: it is `wake_wait_limit_sec` (seconds) in the deployment details, and the page's hints use that number.
- While waiting, the deployment details show a waiting banner and the start (`wake_waiting_since`), and you can click **Cancel waiting**, which maps to `DELETE /deployments/{id}/wake-waiting`. The start of the wait is stored in the database, so the wait continues after a server restart.
- When a process outside the platform holds the card, neither waiting nor yielding takes effect: the wake is refused outright with 409 `WAKE_GPU_HELD_OUTSIDE`.

## Yield Policy

How a deployment gets along with the other deployments that share its cards is decided by one **yield policy** plus a few parameters that appear only when they apply. The yield policy is the only thing to choose. For the concepts and the rule see [Yield Policy: How Deployments Share a Set of GPUs](../explanation/service-levels.md), and for the design trade-offs see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md).

While no other deployment waits for its cards, a deployment stays awake and does not put itself to sleep. To sleep at fixed times, use the [schedules](#定时计划) above.

### Policies and Parameters

The four yield policies are listed from the highest rank to the lowest; the rank only decides who may preempt whom:

| Yield policy | Suits | Meaning |
|---|---|---|
| Never yield automatically (`never`, default) | Always-on services | Never gives its cards to another deployment |
| Yield when idle (`idle`) | Online services | Gives up its cards only when another deployment waits for them and it has been idle (no in-flight or held request) for the idle time; never yields while busy |
| Yield on a turn (`timeslice`) | Several models sharing in turns | Yields once idle for the idle time; yields at once to a higher-ranked deployment, busy or not; takes turns with other yield-on-a-turn deployments, each turn lasting at most the longest serving time |
| Yield anytime (`anytime`) | Batch and offline work | Yields once idle for the idle time; yields at once to a higher-ranked deployment, busy or not; yields to another yield-anytime deployment only when idle |

Parameters appear in the UI only when they apply; the API accepts inapplicable parameters, ignores them, and leaves them out of responses. A parameter left out takes the default of the chosen policy:

| Parameter | Values | Applies to | Default (idle / turn / anytime) |
|---|---|---|---|
| `idle_sec` idle time | 15–604800 s; for yield on a turn it may not exceed `max_serve_sec` | The three policies other than never yield | 60 / 30 / 30 |
| `max_serve_sec` longest serving time | 30–86400 s | Yield on a turn | — / 120 / — |
| `unfinished` handling of unfinished requests when yielding | `abort` cuts them off at once; `wait` lets them finish, for at most `unfinished_wait_sec` (1–600 s, default 30), then cuts off what is left | Yield on a turn and yield anytime | — / `wait` / `abort` |
| `sleep_level` sleep level | `auto` (L1, falling back to L2 when host memory cannot hold the weights), `l1` (strict L1, refused where it does not fit), `l2` | The three policies other than never yield | `auto` |
| `hold_max_sec` longest request hold | 10–3600 s | The three policies other than never yield | 300 / 600 / 3600 |

These defaults, which parameters apply to which policy, and the range of each parameter are served by `GET /api/v1/scheduling/yield-policies` (see below); the UI displays them from there and keeps no copy.

- **Idle time**: how long without a user request counts as idle. The platform's own health checks are not user requests. Once idle this long, the deployment yields if another one waits for these cards.
- **Longest serving time**: when two yield-on-a-turn deployments share cards and one waits for the other's, the holder yields once it has served this long since it last woke.
- **Sleep level** applies to every sleep the platform chooses for the deployment (a yield); sleeps an operator orders by hand or a schedule entry specifies use their own level and are unaffected. `auto` has one meaning: L1 first, falling back to L2 when this node's host memory cannot hold the weights. An operator's yielding wake with `yield_level=1` (the default) and the yield policy's `auto` follow the same rule; only the yield policy's `l1` and an operator's direct L1 sleep (`sleep?level=1`) are strict L1 and refuse when it does not fit.
- **`wait` of `unfinished`**: when yielding, the gateway first stops sending new requests to the deployment (they are held), and the deployment sleeps once the in-flight requests finish, waiting at most `unfinished_wait_sec` seconds and cutting off what is left then. There is no “unbounded” option: one very long request holds a waiter for at most 600 seconds.
- **Two yield-when-idle deployments may share a card**: while both are busy neither yields, so one side's requests are held by the gateway until the other is idle, or until they reach their own `hold_max_sec` and get a 503. When you set the policy, `idle_neighbours` in the response lists such neighbours and the UI shows a warning; to make them take turns, use yield on a turn.
- The rule is applied at the moment a yield falls due, not at a periodic evaluation, so thresholds of 15 or 30 seconds hold to about a second.

### Configuring It

In the **Scheduling policy** card in deployment details, click **Edit** and use the yield policy tab:

1. Choose one of the four policies in the **Yield policy** select; the defaults of that policy are filled in below.
2. Below it only the parameters that apply to the current policy are shown.
3. Save.

When creating a deployment (with **Sleep capable** on) you can also choose a yield policy; the other parameters take their defaults, and you edit them after creation. The create form sends the chosen policy to the server as it is, and the server checks it against "sleep capable, single-node, not PD" and refuses it when it does not hold (see the table below); the deployment details' `yield_policy_eligible` and the `yield_policy_eligible` that the evaluate response works out for the draft tell the page this result in advance (`ok`, and when not allowed `reason` is `SLEEP_NOT_ENABLED`, `SLEEP_ENGINE_UNSUPPORTED` or `YIELD_POLICY_SHAPE`), and the page disables the policies that cannot be chosen and shows the reason.

The API has a single entry point, `PUT /api/v1/deployments/{id}/yield-policy`. The request body carries `policy` plus any parameters; a missing parameter takes the default of that policy. The first request below sets yield when idle with all defaults, and the second sets yield on a turn, a turn of 300 s, and waits at most 60 s for unfinished requests when yielding:

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/yield-policy" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"policy":"idle"}'

curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/yield-policy" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"policy":"timeslice","max_serve_sec":300,"unfinished":"wait","unfinished_wait_sec":60}'
```

Success returns 200 with the stored policy, with inapplicable parameters absent. The first request returns:

```json
{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}
```

The response can be sent back as a request body as it is. The `yield_policy` field in deployment details gives the current setting (same shape). When you set yield when idle and the cards already hold another yield-when-idle deployment, the response carries one more field, `idle_neighbours` (the other deployment's name), and is still 200.

| Case | Response |
|---|---|
| The body is `{}`, or carries parameters but no `policy` | 400 `YIELD_POLICY_INVALID`, with `params` of `{"field":"policy","reason":"required"}` |
| A value is not one of the allowed ones, or a number is out of range (including an idle time above the longest serving time) | 400 `YIELD_POLICY_INVALID`, where `params.field` is the field and `params.reason` is `invalid_value` or `out_of_range` (with `minimum` and `maximum` when out of range) |
| The deployment is not sleep capable / the engine does not support sleep, and the policy is not never yield | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |
| A multi-node or PD deployment with a policy other than never yield | 400 `YIELD_POLICY_SHAPE` |

The dictionary of yield policies is a separate endpoint, for clients that edit a policy:

```bash
curl --fail-with-body -sS "$API_URL/api/v1/scheduling/yield-policies" \
  -H "Authorization: Bearer $TOKEN"
```

In the response, `policies` lists the four policies from the highest rank to the lowest, each with `defaults` (the values filled in when it is chosen) and `applies` (the names of the parameters that can apply); `applies_when` says which parameters apply only when another parameter has certain values, currently `{"unfinished_wait_sec":{"unfinished":["wait"]}}`, that is, the wait time means something only when `unfinished` is `wait`, and the editor reads it instead of keeping the rule itself; `bounds` holds the range of each numeric parameter, with `at_most` naming another parameter it may not exceed; `sleep_levels` and `unfinished` are the values of the two enumerated parameters. See the [API reference](../reference/api.md#出让策略).

### Choosing a Yield Policy

The typical use is real-time plus batch: set the real-time model to yield when idle and the batch model to yield anytime, deployed on the same cards. When a request waits for the batch model (a batch job retries what was cut off) and the real-time model has been idle for its idle time, the real-time model yields; when the real-time model gets a request, the batch model yields at once and the request waits at the gateway for the real-time model to wake (an L1 switch takes about 2.5 seconds). How to point a batch job at a yield-anytime deployment is in [Run Batch Jobs](batch-api.md#与随时出让搭配).

- **Several models taking turns on a set of cards**: set all of them to yield on a turn. Whichever has a request wakes; the holder yields to the one waiting once it has served the longest serving time or has been idle for the idle time.
- **A core service that stays resident**: never yield automatically; other deployments cannot take cards from it.
- **Hold cards only while someone uses them**: yield when idle; while it is idle, other deployments may take the cards.

A deployment that is never idle (for example one that is called all the time) never yields because of idleness, so two yield-when-idle deployments on the same cards that both stay busy do not give way to each other; for that case choose yield on a turn.

#### On-Demand Wake

When the platform itself put a deployment to sleep (yielding cards), a request that arrives does not get a 503 from the gateway. The gateway holds the request, wakes the deployment, and lets the request through once it is awake:

- The hold time is the deployment's `hold_max_sec` (2 minutes when it has none); on timeout it returns 503 with `Retry-After`. At most 256 requests are held per deployment, and the rest get a 503 at once.
- When the server stops gracefully (an upgrade, a restart), held requests return 503 at once with `Retry-After: 10` and the message `the server is restarting; send the request again`; sent again to the restarted server, they are held again. On SIGKILL their connections drop. After a restart, a yield-on-a-turn holder continues its turn as its instance events say; idle time counts from the restart.
- A sleep a schedule ordered, and a sleep you ordered by hand without ticking **Wake it when a request arrives**, is a **held sleep**: a request does not wake it and still gets a 503. For a sleep ordered by hand this option defaults to the deployment's yield policy: a deployment that never yields is held by default, while any other yield policy is woken by a request by default and the deployment keeps taking turns with the others by the rules (the deployment's `sleep_wakes_on_request`). On a deployment that is already held you can click **Let requests wake it** in the detail view (`POST /api/v1/deployments/{id}/wake-on-request`) to clear the hold without waking it. The hold is per instance (the instance's `sleep_held`): only the instances the sleep names are held, and a request still wakes other instances of the same deployment that the platform put to sleep to yield cards; when the deployment's `sleep_hold` is `all` (`sleep_held` is `true`) a request wakes none of them, and when it is `some` a request wakes only those not held. A wake clears the hold of the instances it wakes, including a wake that is waiting for room: when you click **Wake** and choose to wait (`?wait=true`, answered 202 `waiting`), the hold is cleared at once. The deployment is meant to be awake, so a request that arrives afterwards is held by the gateway for that wake, and the residency arbiter may make room for it under the yield policies, instead of getting a 503; before, the hold stayed until the wake landed. A held sleep also **keeps no card**: since a request cannot wake it, its cards are open to any new deployment, and the later wake by you or a schedule either waits for room (`?wait=true`) or has the holders sleep (`?yield=true`), see [Sleep and Wake Operations](sleep-wake.md#放不下时等位与让位).
- When it does not fit, the yield policy decides whether the holder yields; if it should not, the request waits until its hold time runs out.
- When a yield-on-a-turn or yield-anytime deployment yields, the requests still running are handled by `unfinished`: `abort` cuts them off at once; `wait` first stops new requests (they are held) and waits for the in-flight requests to finish, cutting off what is still running at `unfinished_wait_sec`. An interrupted **non-streaming** request is held by the gateway and replayed once the deployment is back (up to 3 times), so the client only gets the result of the run that finished; a **streaming** request has already started to produce output and cannot be replayed: the client sees HTTP 200, `finish_reason` of `"abort"`, and truncated content, and must retry. The platform-managed [batch jobs](batch-api.md#与随时出让搭配) retry interrupted requests automatically.
- A deployment that yielded cards sleeps at its `sleep_level` (under `auto` L1 first; the L1 warm-up at instance start already pinned the host memory, and it falls back to L2 when the host cannot hold it); yield-on-a-turn and yield-anytime deployments resume automatically once no one holds their cards; no awake deployment yields for a resume. After a server restart, the deployments waiting to resume are read back from the event log and still resume once their cards are free; one an operator ordered to sleep or wake since is not read back.

### How a New Deployment Gets onto a Shared Card

When you create a new deployment (or add a replica to one), the cards it needs may be held awake by another deployment. When its yield policy is not never yield and the holding deployment's policy is not never yield either, the new replica does not stay at "does not fit": **Deployments waiting for placement** shows `waiting_for_yield` and the holders give up the cards under their yield policies:

> Waiting: batch yields the GPU at once; shared yields the GPU after 30 seconds idle or 2 minutes served this turn. Then the replica is placed on gpu-1.

The reason's parameters are `node`, `holders` (one entry per holding deployment: `deployment`, `at_once`, and when it does not give way at once also `idle_sec`, and for yield-on-a-turn also `max_serve_sec`, that is, the terms on which it gives the cards to this new deployment) and `instances`. Once the holders are asleep, the replica is placed and started automatically; nothing needs to be done by hand. In short:

- The deployment form's preview (`POST /api/v1/evaluate`) says what the controller says: when the replica does not fit but would once the holders yield, the preview's `plan` and the per-node `candidates[].reason` of automatic placement are likewise `waiting_for_yield`; when it does not fit only because sleepers keep the cards, they are `reserved_for_sleepers`, rather than a plain "does not fit".
- The holders sleep under [the same yield rule](../explanation/service-levels.md#让卡规则), using the yield policy of the new deployment: when the new deployment ranks higher, yield-on-a-turn and yield-anytime holders give way at once; otherwise a holder gives way once idle for its idle time; between two yield-on-a-turn deployments it gives way when the holder has served its longest serving time this turn.
- A new deployment with never yield waits for nobody and needs the cards to be free; a holder with never yield is never asked to give way, so the new replica still waits as one that does not fit.
- The controller plans the replica again every 5 seconds. A replica of a deployment with a node pool takes a node that needs a yield only when no node fits outright. A replica already waiting for the holders on one node to yield is looked at on that node first each time: if it still fits once its holders sleep, only that node is planned again; the search goes on to the other nodes of the pool only when it no longer fits even with the holders' yield.
- While a yield lands, the residency arbiter looks every 250 milliseconds, for up to two minutes, whether it has landed; it then looks only at the waiting wakes of the deployment the yield was for and no longer goes through every waiting wake in the cluster, and the waiter's wake still lands within one look of the holder going to sleep.
- A holder that gave way does not take the cards back while the new replica is being placed and started (up to 10 minutes); the new replica's turn and idle time count from when it begins to serve.
- The wait lives in server memory only; after a restart the controller notes it again on its first pass.

**When yield policy keeps a new deployment from waiting for the cards, the preview says who holds them.** In the deployment form's preview, when a node does not fit but would once the deployments holding its cards slept, and yield policy keeps the new deployment from waiting for them, the preview's `plan` and the per-node `candidates[].reason` of automatic placement name the node and cards, the holding deployments with their yield policies, and what to change:

- `does_not_wait_for_yield`: the new deployment never yields and waits for no holder, while some holders share cards (they would give way once the new deployment picks a policy that shares cards). The UI reads: "GPU 0 on gpu3 is held by qwen3-asr (Yield when idle); this deployment never yields, so it does not wait for them to give the cards up. Enable sleep for this deployment and pick a yield policy that shares cards, and it waits for qwen3-asr to give them up by yield policy."
- `held_by_non_yielding`: the new deployment shares cards, but some holders never yield and do not give their cards to it. The UI reads: "GPU 0, 1 on gpu3 is held by shared (Yield on a turn), batch (Never yield automatically); batch never yields and does not give its cards to this deployment. Choose another card, or enable sleep for batch and pick a yield policy that shares cards. The other holders give way: shared gives up its GPUs after 30 s idle or 2 min of serving."

Both have the parameters `node`, `gpus` (the cards it would have used), `holders` and `blockers`, each entry with `deployment` and its yield policy `policy`: `blockers` are the holders that never yield; `holders` are the rest, and in `held_by_non_yielding` they also carry the same terms as `waiting_for_yield` (`at_once`, or `idle_sec` and, for yield-on-a-turn, `max_serve_sec`). When it would not fit even with every holder asleep, the reason stays the scheduler's own; when both the new deployment and the holders never yield, the cards are simply in use and the wording does not change either. These two reasons appear only in the preview: the controller does not run this extra trial when it records a real replica's reason.

**A sleeping deployment keeps its cards for a request's wake.** While a deployment that shares cards by yield policy is asleep, and a request can wake it (the deployment is sleepable and that instance's sleep is not a held sleep as described above), a new deployment it does not share with (never yield) is not placed on its cards, so the deployment cannot be left unable to wake. When a new deployment does not fit only for this reason, **Deployments waiting for placement** shows `reserved_for_sleepers`, with the parameters `node`, `gpus` (the cards it would have used) and `sleepers` (each with `deployment` and `policy`, the sleeper's yield policy). The UI reads:

> GPU 0 of gpu3 is reserved for the sleeping qwen3-asr (yield when idle) so a request can wake it; a deployment that never yields is not placed on these cards. Pick another card, turn on sleep for this deployment with a yield policy that can share cards, or put the deployment holding the cards to sleep by hand (a deployment put to sleep by hand keeps no card).

Follow the hint: pick another card; turn on sleep for the new deployment and choose a yield policy that can share cards (it then becomes a waiter for cards and takes the `waiting_for_yield` path above); or put the deployment holding the cards to sleep by hand and choose held sleep (leave **Wake it when a request arrives** unticked). A request does not wake a held sleep, so it **keeps no card** and the new deployment can be placed on it directly; a manual sleep that a request can wake still keeps its cards.

## 调度记录

The **Scheduling history** at the bottom of the Scheduling Analysis page lists the scheduling events of the whole cluster, newest first:

| Column | Content |
|---|---|
| Time | When the event happened |
| Deployment | The deployment name is clickable and opens that deployment's detail; a deleted deployment is marked "deleted" |
| Node | The node or nodes the event concerns; a node name is clickable and opens that node's drawer on this page. A node that no longer exists, or one an older event kept only by name, shows its name and is not clickable; — when the node cannot be told |
| Action | The action name for the event code, such as automatic sleep, replica rebuilt, replicas changed |
| Reason and details | The same wording as the deployment timeline: from which node to which, which attempt, the schedule that triggered it, the replica count change, and so on |
| Category | **Automatic**: the platform acted on its own settings or schedules; **Failure**: something went wrong and the platform is dealing with it or has given up; **Manual**: an operator's order |
| By | Who gave a manual order; empty for automatic and failure events |

An entry that gave cards to another deployment (a yield) also names that deployment, clickable the same way; it shows “deleted” when that deployment is gone.

Filter by category, deployment (search by name), and time range (the platform’s time picker, the last 24 hours by default; quick ranges of 15 minutes, 1 hour, 6 hours, 1 day and 1 week, or custom start and end times), and page with Older / Newer, 20 entries per page. The API is `GET /api/v1/scheduling/history`; see the [history API](../reference/api.md#调度记录).

What is in the history:

- **Automatic**: the actions of schedules and yielding cards, waiting wakes, and yielding wakes (see [Timeline Events](#时间线事件)), automatic placement of replicas, and replicas moved off a draining node.
- **Failure**: a failed replica rebuilt automatically (`REPLICA_REBUILT`), a unit that spent its retry budget and was given up, a deployment that cannot be placed, a rolling restart stopped on a replica that gave up, and replicas rescheduled after a node was lost.
- **Manual**: changing the replica count, restarting a deployment or a replica, retrying a replica, and ordering sleep and wake (`SLEEP_ORDERED`, `WAKE_ORDERED`). A manual event records the user who gave the order.

The history holds only these scheduling events; instance state changes, configuration edits, and the like are not in it and stay on each deployment's timeline.

The "Node" column finds the nodes an event concerns in this order: the nodes the event's params recorded (`node_uuids`, or an older event's `node` and `nodes`), then the nodes the instances the event names (`instance`, `instances`) are on now, and otherwise none. Instances are removed in time, so events record their nodes in their params when they are written; see [Timeline events](#时间线事件).

## First Placement Nodes

When adding replicas to a single-node deployment, you can choose the nodes the new replicas start on first; see the note in [Deploy Models](deploy-models.md#部署后调整). This works alongside a scheduled replica count: the schedule changes only the number, and the candidate pool decides which node; for a multi-node deployment placed automatically the server picks the nodes.

## Timeline Events

Automatic actions are written to the deployment timeline (event kind `automation`, also shown in the [scheduling history](#调度记录)):

| code | Meaning | Main params |
|---|---|---|
| `AUTO_SLEEP` | A schedule or yielding cards put the deployment to sleep | `trigger` (`schedule` a schedule / `preempt` yielding cards), `schedule_id`, `scheduled_at`, `level` (the level the platform chose, 1 or 2); when yielding cards also `reason` (`idle` idle for the idle time / `rank` the other ranks higher / `turn` it is the other yield-on-a-turn deployment's turn), `for_deployment` (the deployment it yields for), `abort` (whether in-flight requests were interrupted), `instances` |
| `AUTO_WAKE` | A schedule, a waiting wake, a request, or a resume woke instances | `trigger` (`schedule` / `wake_waiting` / `request` a request is waiting / `resume` taking cards back after yielding, once no one holds them), `schedule_id`, `instances` |
| `AUTO_SCALE` | A schedule changed the replica count | `trigger`, `schedule_id`, `replicas`, `role` (PD) |
| `WAKE_WAITING` | A wake did not fit and now waits for room | `trigger` (`schedule` / `operator`), `instances`, `yielded` |
| `WAKE_WAIT_TIMEOUT` | The wait ended after 30 minutes | `trigger`, `instances`, `waited_sec` |
| `SCHEDULE_ACTION_FAILED` | An automatic action did not succeed and is not retried | `trigger`, `action`, `reason`, `reason_code`, `schedule_id`, `level`, `replicas`, `role` (PD) |
| `YIELDED_FOR_WAKE` | Recorded on the deployment put to sleep to make room: for whom, and to which level; the scheduling history links the deployment named by `for_deployment` | `deployment`, `for_deployment`, `instances`, `level` |

An order an operator gives through the API is also written to the deployment timeline (event kind `schedule`) with `actor` (the user name):

| code | Meaning | Main params |
|---|---|---|
| `REPLICAS_CHANGED` | The replica count was changed | `from`, `to`, `role` (PD), `actor` |
| `SLEEP_ORDERED` | Sleep was ordered | `level`, `mode`, `scope`, `role`, `instances`, `actor` |
| `WAKE_ORDERED` | A wake started waking instances | `instances`, `actor` |

When a failed replica is rebuilt automatically the controller records `REPLICA_REBUILT` (params `replica`, `role`, `attempt`, `reason`, `node`, `node_uuids`); see the [history API](../reference/api.md#调度记录).

The events the scheduling history lists also carry `node_uuids` when they are written: the list of node UUIDs the event concerns, which the history's "Node" column uses. An event that names instances (`instance`, `instances`, or an event on an instance, such as `AUTO_SLEEP`, `AUTO_WAKE`, `YIELDED_FOR_WAKE`, `WAKE_WAITING`, `WAKE_WAIT_TIMEOUT`, `SLEEP_ORDERED`, `WAKE_ORDERED`, `REPLICA_RESTARTED`, `DEPLOYMENT_RESTARTED`, `UNIT_RETRY_EXHAUSTED`) records the nodes those instances were on; the controller's node events record the nodes they concern: `REPLICA_REBUILT` the node of the new attempt's leader, `NODE_REPLACED` and `NODE_REPLACE_REVERTED` the nodes moved from and to, `NODE_REPLACE_NO_CANDIDATE` the lost nodes, `UNIT_PLACED` and `NODE_DRAIN_MOVED` every node of the placements before and after, `UNIT_RETRIED` and `DEPLOYMENT_ROLLING_STALLED` the nodes of the failed attempts. `DEPLOYMENT_UNSCHEDULABLE` keeps its existing `node` (a node UUID) instead. Orders for the whole deployment (a scheduled sleep, a replica count, a rolling restart) concern no particular node and do not carry it.

For the events of node-loss rescheduling after a node is lost and of drain moves (`NODE_REPLACED` and the like), see [Reschedule Replicas on Node Loss](node-failover.md#时间线事件).

## Limitations

- Cards are yielded only when the yield policy allows it: a deployment that never yields automatically never sleeps for anyone else; a yielding wake still happens only when an operator names it and the occupant can sleep.
- No load-based scaling: the replica count changes only when a schedule entry is due.
- Yield policies other than never yield automatically apply only to single-node deployments; a multi-node or PD deployment whose cards are held still waits as one that does not fit.
- A scheduled replica count applies only to single-node deployments and automatically placed multi-node and PD ones, not to multi-node or PD deployments placed by hand; a PD entry changes one role, so changing both takes two entries.
- The evaluator runs inside the server process; nothing happens automatically while the server is down, and after it recovers only the newest entry within 12 hours is made up.
