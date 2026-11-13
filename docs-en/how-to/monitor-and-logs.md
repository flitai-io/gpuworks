# View Metrics and Search Logs

**Use cases**: View GPU metrics for clusters and deployments, investigate why an instance fails to start or reports errors, and search historical logs by keyword.

After completing this guide, you can find the relevant replica from an incident time range or request ID, then use metrics, events, and logs to determine what to investigate next.

**Prerequisites**: You can sign in to the console. To view live node metrics, the node must be [connected](connect-nodes.md) and online. To view historical instance logs, the instance must have been created and produced logs. To view gateway requests, calls must have passed through `/v1` or Playground.

## 1. View the Overview and Metrics

1. Open **Monitor Center** in the sidebar to view GPU utilization, GPU memory, temperature, and instance status. Select the time range when the issue occurred.
2. First check “Data as of …” at the top. If no new sample has arrived in the last 30 seconds, “Stale — no fresh samples in the last 30s” appears and the charts show the last sample;
   “No samples yet” means no data has arrived yet. If a node is offline, restore its connection first. Do not use old values to judge its current load.
3. Open the relevant service's monitoring page to view request reconciliation, latency, throughput, and running requests. For network interface, RDMA port, and role traffic definitions, see the [Network Metrics Reference](../reference/network-metrics.md).

After a service or node is deleted, its raw samples remain until they expire (72 hours by default). The Monitor Center service list and VRAM bars show only existing services and nodes; when the metric explorer groups by node or deployment, deleted ones are still shown by the name they had when deleted and marked “deleted”. Opening a deleted service's monitoring page says it has been deleted and links to Analytics, which keeps its usage, service quality, and GPU hours.

Select a time range at the upper right. Presets from 15 minutes to 7 days end at the current time and refresh automatically with **Follow latest**. You can also use the date picker to select a custom range of up to 30 days that does not extend into the future, subject to the raw metric sample retention configured under [Data retention](../reference/api.md#analytics用量分析). A custom range stops automatic refresh: charts, request distributions, latency breakdowns, and signals use that range, while the service list, replicas, and health checks still show their current states. A message on the page explains this distinction. The selection is stored in the URL (`?from=…&to=…`), so you can share it directly. For longer-term hourly or daily trends in token usage, service quality, GPU utilization, and GPU hours, open **Usage analytics**; see [View Usage Analytics](analyze-operations.md).

Deployment metrics have two sources. Confirm how they are measured before comparing engines:

- **Engine**: The agent collects each instance's metrics endpoint and maps it to platform metrics according to the template. vLLM / SGLang have built-in mappings. A `callable` engine with no declared mapping is detected automatically if it exports metrics with the `vllm:` / `sglang:` prefix. Other engines must [declare a metrics mapping](declare-engine-metrics.md).
- **Gateway**: The gateway measures request duration and in-flight counts for requests that pass through it, and reads token counts from the response's `usage`. Gateway statistics are available even if the engine provides no metrics endpoint. Requests sent directly to instances are excluded from these statistics.

| Metric Shown | Interpretation or Next Step |
|---|---|
| TTFT, end-to-end latency, TPOT, input / output token throughput, running requests | Engine values take priority; available gateway statistics are used otherwise. The UI does not label the source. If neither source provides data, no value appears. Gateway running requests are in-flight counts that include queued requests, so they are not directly equivalent to requests executing in the engine |
| Queue / Prefill / Decode duration | Without engine metrics, the gateway can only aggregate per-request statistics actually reported in responses. A stage cannot be separated if its field is missing |
| KV usage, queued requests, preemptions | Only the engine can provide these. The service monitoring page indicates when they are missing; do not interpret missing values as 0 |
| Request reconciliation (last 5 minutes) | Compares gateway forwarded counts with engine completed counts. A persistent gap exceeding the in-flight count is highlighted in red. Check failures in the request views below and instance logs; red text alone does not establish a root cause. The card is hidden if the engine does not report completed counts |

In gateway metrics, TTFT runs from handing a request to the instance until the first generated token, including thinking. End-to-end latency runs until the response ends and includes instance queueing and network time. These timings differ from the engine's internal timings.

Gateway TPOT is the time from the first generated token to the last text chunk divided by the output token count minus one, including network jitter. It requires an output count in usage and at least two tokens. Do not treat the streaming chunk count as the token count.

Gateway fallbacks for TPOT and stage durations are generated only for engines whose built-in mappings lack the corresponding metrics. Disabling vLLM / SGLang metrics collection does not generate these additional fallbacks. Restore engine collection if data is missing.

Without engine metrics, the metrics panel in deployment details displays available gateway statistics instead. For the full list of metric names, sources, and units, see the [Inference Metrics Glossary](../reference/inference-metrics.md).

The service page's “KV Cache” chart plots two lines per replica: usage on the left axis and prefix cache hit rate on the right, both from 0–100%. For engines that export hit/query counts (vLLM), the hit rate at each point is hits divided by queries within that point's interval. For engines that report only an instantaneous hit rate (SGLang), that value is used directly. A point with no queries is left blank, rather than plotted as 0.

## 2. View Requests and Request Distributions

These views record requests through `/v1` and Playground. Requests sent directly to an instance port do not appear in the gateway request views.
The service page always shows the latency breakdown, health, phase bar, and swimlane scatter plot at the top, with **Overview**, **In flight**, and **History** below.

### Find Completed Requests

Choose an entry point based on the information you have:

- **You have a request ID or trace id**: Paste the ID into the search box at the top right of Monitor Center. A trace id is a 32-character hexadecimal value that can list requests in the same trace across deployments.
  Gateway-generated trace ids directly identify the time. Other IDs are searched within the last 1 hour by default; you can expand this to the last 24 hours.
  When a request's deployment has been deleted, it is shown by the name it had when deleted and marked “deleted”.
- **You do not yet know the specific request**: Open **History** on the service page and search in reverse chronological order. Select a time range, then filter by result (Completed, Hit the length limit, Client left, Connection cut, Error) and end-to-end time.
  Each page contains 50 rows. Use **Previous / Next** to continue searching. The total request count at the bottom is unaffected by filters or list sampling.

Once you find a request, click its row or focus it and press Enter to check the caller, assigned replica, token counts, finish reason, and phase timeline.
You can copy the request ID and trace id. If a [trace link](export-request-traces.md) is configured, click **Open in the tracing backend**.

Both **History** and **In flight** can be filtered by API key and replica. Each row identifies the replica number and node. Its colored bar shows the proportion of each phase relative to that request's total duration;
hover or use keyboard focus to view phase durations, Prefill / Decode speeds, token counts, and finish reasons.

### View In-Flight Requests and Stalls

Open **In flight** while requests are still running. The list sorts requests by longest wait for the first token, then thinking, then answering. Each row shows the request body size, chunks received, and elapsed time.
It refreshes every 2 seconds and can be paused. It shows 20 rows by default and up to 200 rows.

Before a request ends, the engine has not yet returned queue and Prefill statistics, so the time before the first token is combined as **Waiting for first token**. When you find an abnormal request, record its replica and time, then examine that instance's events and logs.

The **phase bar** updates every 2 seconds with the numbers of requests waiting for the first token, thinking, and answering, as well as the longest wait for the first token and the stall count.
Thinking or answering is marked as stalled if no new chunk arrives for more than 2 seconds.

The **swimlane scatter plot** uses one lane for each of these three phases. Its horizontal axis is elapsed time on a logarithmic scale that extends to at least 60 seconds. A red outline indicates a stall; hover to see the caller, replica, chunk count, and time since the last chunk.
A concentration on the right of the waiting-for-first-token lane means many requests are still waiting. An isolated point with a long duration warrants individual investigation, but its position alone cannot establish the cause of queueing or failure.

The plot shows at most 500 points, prioritizing requests with the longest wait for the first token. This limit does not mean other requests do not exist.

### View Service Trends and Distributions

To assess trends for the entire service, view distributions under **Overview** on the service page. Use the latency breakdown and health information at the top alongside them.

- **Input × output**: Counts requests by input tokens × output tokens, with both axes grouped in powers of 2, to distinguish long-input/short-output traffic from short-input/long-output traffic.
  Monitor Center covers the entire cluster; the service page covers the current deployment. Both use requests that finished within the selected time range.
- **Uncached input × TTFT**: Observe whether longer inputs coincide with slower first tokens. If uncached input is small but the first token is slow, check queueing, preprocessing, and the network. The distribution alone cannot prove the cause.
  The card states how many requests have usage and cache reports.

### View Health and Replica Status

When health checks are enabled, the **Health** card shows the latest result, duration, responding replica, and last 10 checks. Green means healthy, orange means timed out, and red means failed, with older results on the left;
hover to view the time and failure reason of each check. Average duration includes only successful responses within the threshold.

If checks are disabled, open deployment details from the card to enable them. Checks run once per minute. Results remain only in server memory; after a restart, wait for new results.
The card also shows ready / desired replica counts and the status of each replica.

### Interpret End-to-End Latency

The **end-to-end latency breakdown** covers complete responses (normal completion or length truncation) within the selected time range. It shows whether time is spent mainly in the gateway, waiting, Prefill, thinking, answering, or finishing the response.
Hover over or focus a segment to see its share, p50 / p95, and speed. Without engine-reported data, portions that cannot be separated appear as **Waiting for first token**.

Here, end-to-end timing starts when the gateway receives the request and ends when the response finishes, including gateway processing. This start point differs from the gateway metrics in Section 1, which start when the request is handed to the instance.
The first token is whichever arrives earlier: the first thinking token or the first answer token. The last chunk refers only to a chunk carrying generated text, excluding finish or usage chunks.

These generation timestamps are observable only in streamed responses. Non-streamed responses can also be split using engine-reported queue, Prefill, and Decode times if all are provided. Do not treat unobserved first-token or thinking times as 0.

#### What Each Phase Means

The table below uses streamed responses as an example:

| Phase | Meaning and Conditions |
|---|---|
| **Gateway** | Time from receiving the request to handing it to the replica. The Prefill time listed separately for sequential PD handoff is not counted again here |
| **Engine preprocessing** | When the engine reports queue and Prefill times, this is the remaining wait before the first token after subtracting those two values. It includes template processing, tokenization, waiting before scheduling, and the network time for the first token to return. It can increase with high concurrency or long computation steps and is not purely CPU processing time |
| **Engine queue / Prefill / Waiting for first token** | Queue and Prefill use engine-reported values. If only Prefill is reported, Prefill is listed separately and the rest is combined as **Waiting for first token**. If Prefill is not reported, the entire wait is combined as **Waiting for first token**; do not interpret it as pure queueing |
| **Thinking / Answer** | Thinking runs from the first thinking chunk to the first answer chunk (or the last chunk if there is no answer). Answer runs from the first answer chunk to the last chunk |
| **Engine postprocessing** | Time from the last text chunk to the end of the response, including finish chunks, usage, `[DONE]`, and the gateway writing back to the client. Slow client reads also count here, so this time cannot be attributed solely to the engine |

#### PD Modes and Older Records

For sequential handoff in the two vLLM PD modes, an additional **Prefill** segment follows **Gateway**, representing the gateway's wait for the prefill instance (called **PD prefill** in individual requests).
Decode-reported queue time appears as **KV transfer & queue**, including the wait for remote KV. Decode's Prefill appears as **First-token recompute** and is hidden if it accounts for no more than 1% of mean end-to-end latency.

Older records that did not record gateway time separately cannot be compared directly with **Engine preprocessing** in new records.

#### Averages, Percentiles, and Speeds

Each segment's average is calculated across all complete responses, with responses that lack that phase counted as 0, so the segment averages sum to the end-to-end average. This does not mean unreported engine phases actually took 0 time.
The top shows mean end-to-end latency and p50 / p95. Phase percentiles include only requests with that phase and have approximately 10% binning error. Aggregates use all requests and are unaffected by request log sampling.

Speeds are calculated for requests with the corresponding token counts and durations by dividing the sum of tokens by the sum of durations:

- **Prefill**: Uses uncached input tokens and engine Prefill time. Sequential PD uses the P instance's reported time.
- **Thinking**: Uses thinking tokens and the thinking phase duration.
- **Answer**: Uses output tokens minus thinking tokens and the answer phase duration. If usage provides no thinking count, only responses with no thinking phase are included.
- **Decode**: Uses engine-reported duration when available; otherwise, it uses the interval from the first to the last generated token and the output token count minus one.

Speed cannot be calculated without token counts or the corresponding phase. For fields and complete breakdown rules, see [Request Timeline and Phase Breakdown](../reference/request-timeline.md).

### Missing Requests or Data

#### No Token Distribution or Speed

Requests without usage can still have a timeline but are excluded from token distributions. Cache distributions also require the engine to report cache hit counts.

For vLLM / SGLang streamed requests, the gateway requests usage and removes the extra usage chunks obtained for statistics, preserving the client's original response.
Responses from the built-in TensorFold and Strata templates include usage. Other callable engines must provide it themselves.

#### No Queue or Prefill Breakdown

vLLM requires **Return per-request timings** (`--enable-per-request-metrics`). This is enabled by default for new deployments using images version 0.25.0 or later. For older or unknown versions, the flag is omitted by default; you can select it manually, but must confirm that the image supports it.
The built-in TensorFold and Strata templates report Prefill and Decode, but not queue time. SGLang does not yet support this breakdown.

#### Missing Request Records

Records are retained for 7 days. If storage exceeds 2 GB, the oldest day's records are deleted early.

Above 300 requests per second, normal requests are sampled proportionally, while abnormal requests (non-2xx, disconnected, truncated, or first output ≥ 10 seconds) are retained.
One row may represent multiple requests; hover to view its weight. Distributions and counts are unaffected by list sampling.

Records may also be lost if writes cannot keep up. Unsaved requests cannot be recovered from the list or ID search.

For API access, use `GET /api/v1/monitor/deployments/{id}/requests`, `GET /api/v1/monitor/requests/lookup`, and
`GET /api/v1/monitor/distributions`. See the [API Reference](../reference/api.md) for parameters.

## 3. View Instance Events and Live Logs

Open instance details on the deployments page. Events (state changes, scheduling results, and errors) and logs appear in a single timeline. Live logs appear while the instance is running. After the container exits, persisted logs appear; you can also select a historical time range to review. Instances and logs are retained after startup failures. Check errors here before changing parameters and retrying.

## 4. Search on the Logs Page

After identifying a time range and instance ID in instance details, open **Logs** in the sidebar to search historical records. You can also query node, agent, and server logs here.

1. **Select filters** in any combination:

   | Filter | Description |
   |---|---|
   | **Instance** | Prefix matching; an 8-character short ID is sufficient |
   | **Level** | `level=warn` means ≥warn, including warn and higher levels |
   | **Source** | `source=instance\|node\|agent\|server`; the API defaults to `instance` if omitted |
   | **Time range** | The page provides presets from the last 30 minutes to 7 days, all time, and more. Select **Custom** to specify start and end times to the second |
   | **Keyword** | Full-text search, for example, `CUDA out of memory` |

2. **View results**. Leaving the keyword blank returns recent logs (equivalent to tail). Click **Load more** if results span multiple pages. Hit counts are capped at 1 million; above the counting limit, they appear as `N+`, not an exact total.
3. **Expand context**. Click **Context** on a hit to view several lines before and after it (up to 1000 per side). For agent and server logs, restrict the scope by node and source; do not reuse the instance filter.
4. **View the histogram**. Above the search results, log volume is counted by time bucket × level using the current filters (red for error/fatal, blue for other levels, including lines without a level). Click a bar to narrow the time range to that bucket (switching to Custom); the new range is redrawn with finer buckets, so you can keep clicking to drill down.
5. **Return to the original event**. Match the level, time, source, and instance UUID in a hit to the corresponding event in instance details.

To search through the API, use `GET /api/v1/logs/search`. Add `with_total=1` to retrieve hit counts, and use `cursor_ts` / `cursor_id` for pagination.
For context, use `GET /api/v1/logs/context?instance=..&ts=..&id=..&before=..&after=..`. For logs without an instance UUID, such as agent and server logs, use `node` to restrict the scope instead.
For the histogram, use `GET /api/v1/logs/histogram` with the same filters as the search. See the [API Reference](../reference/api.md) for all parameters.

While a node is disconnected from the server (a server restart or upgrade, a network outage), the agent holds instance, node, and agent logs in memory and sends them in their original order after it reconnects, so lines from that period are still searchable. The buffer holds up to 16 MiB. If a disconnection lasts long enough to fill it, the oldest logs are dropped, and a warn-level line beginning `gpuworks: N log lines from … to … were dropped` marks the place (search for `were dropped`). An agent process restart loses what was not yet sent. Metrics from the disconnection are not sent later.

### The server's and agent's own logs

- **What reaches the Logs page**: the server and the agent send only records at warn and above to the log center; info and below go only to their log files (the server's `logs/gpuworks-server.log`, the agent's `<state>/agent.log`).
- **Repeated records are compressed**: at most 10 records of one kind are written per 30 seconds (debug records are suppressed only when identical, at most 50 per 30 seconds); the rest are counted. When the window ends, one
  `suppressed similar log messages` record follows, carrying the suppressed message (`message`), the count (`suppressed`) and the window (`since`, `window`).
  Records are of one kind when their level, message and the logger's own fields match; the server also tells them apart by error code, reason, request method, path and status, the agent by error code and reason. With more than 4096 kinds active at once, a new kind counts by its message alone.
  A client retrying one path with bad credentials, or a vendor tool failing on every probe, thus leaves a few lines and a count.
- **Why a request was refused is in the access log**: the server writes one `http request` line per request; when it returns a product error, the line carries `code` (the error code) and `error_params` (the error's parameters as JSON, such as a scheduling refusal's node, reason and memory figures), still there after the dialog is closed.
- **Image checks**: the agent checks the catalog images every minute and writes info only when a conclusion changes: `image present` (with the `image_id` the runtime returned) or `image no longer present`.
  An absent image is an ordinary answer and is not a warning; a failing check command writes the warning `image presence inspection failed`.
- **Scheduling refusals**: `deployment not schedulable yet` is written with the scheduling event, once per waiting window (10 minutes by default) for the same reason.

### Temporarily Capture Debug Logs and Raw Requests

1. As an administrator, open **Settings → Diagnostics → Debug capture** (`/settings#diagnostics`), select the deployments to investigate, and start capture. Their nodes are included automatically; selecting nodes alone does not capture deployment requests. The defaults are 10 minutes and 10 GiB. You can stop capture manually without restarting the engine.
2. Reproduce the issue during capture and record the client's `X-Request-ID`, time, and status code. Confirm that the relevant nodes are collecting. Offline, unsupported, or failed nodes must not be treated as captured.
3. After stopping, use **Collect diagnostics** on the same page, covering the reproduction time and the relevant deployments, nodes, and debug capture, then download the bundle. The debug session contains `server.log`, node debug logs, and `requests/<deployment ID>-<request ID>.jsonl`.

While a session is active and below its size limit, debug files retain repeated original records, including debug records. Ordinary log files and the log center remain throttled as described above; their individual log lines are not a count of failed requests. Stopping, expiry, or the size limit restores the configured ordinary log level. Raw captures contain request and response content; check for sensitive data before sharing.

A request JSONL's `end` record describes the gateway's observations. An upstream EOF before response headers retains `read_error` (for example, `EOF`) and `outcome=upstream_error`; a normal response must not record body EOF as an error. For streamed responses actually returned within the capture limits, inspect `upstream` / `client` data for `finish_reason` and `data: [DONE]`. The gateway's prepared request snapshot does not prove the engine received it, and a missing `[DONE]` alone cannot locate the disconnection.

## Set the Engine Log Level

SGLang logs do not include levels by default, so the platform's level filters and histogram cannot identify actual errors. In single-GPU SGLang deployments with engine-managed startup, the platform automatically injects a logging configuration with levels where applicable. Logs still go to container stderr.

Select the log level directly in the **Features** section of Quick Deploy:

- The default is INFO. Choose another level or **Not set** as needed. Options and defaults come from the server's parameter dictionary, and the form submits the selected value.
- Switching between quick and expert modes preserves this selection.
- **Not set** omits `--log-level`, so no configuration is injected and the engine determines the default level.

The deployment command preview shows `SGLANG_LOGGING_CONFIG_PATH` and the pre-start script that writes the configuration. You can also view the saved script and env in instance details. `/tmp/gpuworks-sglang-logging.json` is the configuration file, not the log output file.

For startup methods without automatic injection, reasons multi-GPU logs may still lack levels, and how to take over the full script yourself, see [SGLang Logging Injection: Behavior and Boundaries](../explanation/sglang-logging.md).
