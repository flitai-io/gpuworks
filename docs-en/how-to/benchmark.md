# Run Evaluations and Interpret Reports

**Use cases**: Quantify throughput, latency, and success rate for managed deployments or external OpenAI-compatible endpoints, and compare performance across concurrency levels, arrival rates, and input / output lengths.

After completing this guide, you have an evaluation report and can determine which test points are comparable based on actual request and session completion, lengths, and cache evidence.

**Prerequisites**: You can log in to the console, and the load origin can reach the endpoint under test. Managed deployments need running service instances that are awake.
For external endpoints, prepare the address, model name, and required API key. See the target table below for specific choices; [wake](sleep-wake.md) sleeping instances first.

## Create and Complete an Evaluation Run

1. On the **Evaluations** page, click **New Evaluation**. In **Choose Question**, select parameter scan, saturation exploration, or SLA search.
   Parameter scan compares a specified set of concurrency levels or arrival rates; the other two progressively search concurrency levels according to the objective.
2. In **Set Target**, select the target under test and the **Load Origin**. The load origin changes the request path, so select it before comparing results:

   | Target and load origin | What It Measures | Requirements |
   |---|---|---|
   | **Managed Instance**: select an inference service and set **Load Origin** to `Server` (default) | End-to-end deployment results through the gateway, including load balancing; PD includes the P–D handoff | At least one rank 0 that can serve requests must be running and awake; PD is also affected by role readiness and the fallback policy |
   | **Managed Instance**: select a node as the **Load Origin** | Direct access to an instance on that node; PD selects decode, so this is not an end-to-end PD result | The node must have a running, awake rank 0 for that deployment that can serve requests, and its agent must support evaluations |
   | **External Endpoint**: enter the address and API key, select the **Load Origin**, click **Probe**, and select a model | Access to the external endpoint from the selected load origin | Load originates from `Server` by default; you can also select an online node that supports evaluations. Network access and authentication must work from the selected location |

   Testing an ordinary private or local loopback external endpoint from `Server` needs no allowlist by default. An allowlist is needed only in strict mode. Platform administrators can authorize the target host under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes to either field apply to the next request after saving, without restarting the server.
   See [Outbound Access Configuration](../reference/configuration.md#server-配置文件-serveryaml).

   The API can select a specific instance through `instance_uuid`, which requires a direct connection from the node hosting that instance. It must likewise be running, awake, and able to serve requests.
   Multi-node followers are not independent service entry points, and a direct connection to a single PD instance does not prove that P and D work together correctly.
3. In **Configure Load**, select a **Dataset** and enter the applicable input / output lengths, concurrency list or arrival rates, and requests per point.
   To import a dataset, first upload JSONL. Multi-turn conversations send requests in session order, and the output length is an upper limit.
   Random token-length and Agentic datasets require a loadable tokenizer. **Automatic** tries, in order: the inference service's own `/v1/tokenize` and `/v1/detokenize`
   (served by vLLM and SGLang and on by default for a deployment; sampling uses the engine's tokenizer, so lengths match the engine's count whatever the tokenizer format),
   the `tokenizer.json` or `tiktoken.model` already on the target machine, then the tokenizer repository explicitly declared by the template; without that declaration, it uses the weight repository.
   A GGUF weight repository may lack independent tokenizer files. For example, the Strata template uses a commit-pinned `api.tokenizer_ref` to name the base model's tokenizer.
   The evaluation saves the resolved reference; a template never overrides a manually selected source. For an external endpoint, **Automatic** uses only the endpoint's own tokenize endpoints.
   You can also use only the **Inference service (de)tokenize endpoints** (if it is unavailable the evaluation stops before inference rather than switching sources), name a model repository (`org/repo`),
   or upload `tokenizer.json` / `tiktoken.model`.
4. Review warmup, request timeout, output policy, experience thresholds, and per-request details in **Advanced Settings**. To view individual timelines in the report, enable **Keep per-request details** and set the capacity.
   If every warmup request fails transiently (network interruption, 408/429, or recoverable 5xx),
   recovery waits up to 6 minutes (or the request timeout, if longer), sending one probe every 5 seconds. Authentication and parameter errors stop immediately.
   Cancellation stops both waiting and in-flight requests. Warmup attempts and retries never count as measured points,
   throughput, or latency. Reports retain attempt counts, HTTP statuses, stages, and diagnostics even before any measured
   point exists; warmup diagnostics do not retain request or reply bodies.

   Random token-length and Agentic workloads fail before warmup or measured inference if their tokenizer is missing or cannot be loaded; they no longer fall back to character estimates. Natural corpus and imported datasets can omit the tokenizer, with estimated local counts. `exact` means sampling from the tokenizer vocabulary and still does not guarantee that server-reported usage matches local counts; the chat template also adds input. Endpoint-reported usage and local estimates are retained separately.
5. Click **Start Evaluation** and view progress, errors, and results for each point in the details. If a warning says that an evaluation is already running on the target or load origin, assess whether resource contention affects the comparison before deciding to wait or continue.
   You can cancel long-running tasks; completed points are retained. A node can take over a task after reconnecting. Successful creation or a finished Run does not mean that every request succeeded.
6. When the run ends, first check counts, failure reasons, and actual lengths in **Request and session evidence**, then review latency, throughput, and qualifying points as described below, and finally export the report.
   If all requests fail or sessions remain incomplete, resolve connection, workload, or endpoint errors first, then rerun under the same conditions.

For command-line load generation, you can also use `gpuworks-agent bench`. Supply both `--url` and `--model`, and confirm endpoint reachability from the machine running the command.
See the [CLI Reference](../reference/cli.md#gpuworks-agent-bench) for all options. This method tests the specified URL directly, with the request path determined by that URL.
The evaluation itself does not pull images or start additional containers.

## Metric Definitions

| Metric | Definition |
|---|---|
| Output throughput / total throughput | Output tokens from successful requests at the point (plus input tokens from successful requests for total throughput) divided by the point's duration. The window runs from the first actual send to the end of the last response / stream, including time spent on failures, waits between turns, and finalization. Partial output from failed requests is listed separately |
| Request throughput | Successful requests divided by the point's duration |
| Per-user output speed (Interactivity) | `1000 / TPOT P50`, in tok/s/user: the tokens received per second for a single request after decoding starts, excluding TTFT. This corresponds to InferenceMAX interactivity |
| TTFT | Time from sending a request to the first generated output (including reasoning), including network time and server-side waits. Time to the first visible response text is listed separately; client-side timing alone cannot separate server-side queuing from prefill |
| TPOT | For successful requests, `(last output generation time − first output generation time) ÷ (output token count − 1)`. Includes only streaming requests with an observable decoding interval, at least two content chunks, and more than 1 output token |
| E2E latency | In evaluations, this metric ends at the last generated output. **Request and session evidence** separately lists stream-completion latency, which includes finalization after output; do not interchange the two |
| Maximum output throughput (10s) | Highest output throughput in any 10-second window within the point, without dilution from startup and finalization phases |

The “Throughput vs. Interactivity Pareto” chart plots per-user output speed on the horizontal axis and output throughput on the vertical axis, with one dot per test point.
The former “Per-user Output Throughput” metric divided output throughput by concurrency, including TTFT and gaps between requests, and is discontinued.
SLA constraints defined with that metric on historical Runs still use the original definition, but are not carried over when you select **Run Again**.

## Advanced Settings

- **Warmup requests** (default: 1): Before the measured points begin, send this many requests using the first point's workload and discard all results.
  This reduces the effect of the first request's cold start on measured points but does not guarantee that later points are fully warmed up. Set this to 0 to disable warmup; if all warmup requests fail, the evaluation fails immediately. If the load-origin node's
  agent is too old, warmup is skipped and the evaluation displays a warning.
- **Request timeout** (default: 600 seconds; 10–7200 seconds): How long each request, warmup included, may take before it counts as failed.
  Raise it when long contexts or high concurrency make requests queue. If the load-origin node's agent is too old for this setting, every request
  is bounded by a fixed 5 minutes and the evaluation displays a warning. Reports state the configured timeout; evaluations created before the setting existed show it as not recorded.
- **Fixed output length (`ignore_eos`)** (random fixed-length and Agentic workloads; enabled by default): Ask the engine to generate the target
  output length. The report marks fixed length as requested; check the actual OSL and finish reason rather than treating the setting as proof that the server complied. Some SaaS endpoints do not recognize this parameter and
  return 400; disable it in that case. Multi-turn conversations and imported datasets always end naturally.

### Choose a Load Mode

Only parameter scans support a choice of closed-loop or open-loop load. Saturation exploration and SLA search use binary search over concurrency and support only closed-loop load.

**Fixed concurrency (closed loop)**: Advance at most N sessions concurrently. Within each session, requests are sent sequentially by turn, so the actual number of in-flight requests may be lower than N.

**Arrival rate (open loop)**: Observe performance under sustained load at a specified arrival rate. First confirm that the load-origin node's agent supports the `bench.arrival` capability; creation is rejected otherwise.

Enter a list of arrival rates (req/s), with one point per arrival rate. Requests per point = arrival rate × point duration (default: 60 seconds), with a minimum of 20 requests.

- **Burstiness**: Request intervals follow a gamma distribution, and this value is its shape parameter. A value of 1 gives Poisson arrivals, values below 1 produce burstier traffic, and values above 1 produce more uniform traffic.
- **Max in-flight requests**: 0 means unlimited. When the limit is reached, new requests queue on the client; timing starts only when they are actually sent.

### Set Experience Thresholds

Advanced settings for parameter scan and SLA modes let you specify experience thresholds, including TTFT, completion latency, and session completion rate (0–1).
A session completion threshold requires the load-origin agent to support `bench.evidence`; older agents are rejected so that constraints are not silently ignored.
An SLA minimum for **Interactivity** requires the `bench.interactivity` capability; creation is also rejected when it is unsupported.

## Request and Session Evidence

For newer points, **Request and session evidence** shows separate counts for planned, actually initiated, successful, failed, canceled, and skipped requests,
along with the number of fully completed sessions. Request success rate uses actually initiated requests as its denominator; session completion rate uses planned sessions.
For example, suppose there are 4 sessions with 13 turns each. If one session fails on its first turn without retrying and all others complete, the counts are 52 planned, 40 initiated,
39 successful, 1 failed, and 12 skipped. Request success rate is 97.5%, and session completion rate is 75%. A finished Run does not mean that its sessions completed in full.

The first turn and subsequent turns each have P50/P99 values and valid sample counts for TTFT, TPOT, last-output latency, and stream-completion latency.
Unobservable metrics for requests such as single-chunk streams or requests with no generated output appear as unknown; they are not counted as zero-duration samples.
The first reasoning output and the first response text are distinct events: TTFT counts from the first reasoning or answer text, **first content** has its own P50/P99 and sample count, and a successful request that produced only reasoning and no answer text counts as "without content".
Reasoning is recognized under the field the engine uses (`reasoning_content`, `reasoning`, `thinking`); when the server usage reports output tokens but the stream carries no recognizable output, the evaluation warns that stream output was not recognized and those requests are left out of the TTFT/TPOT samples.
Intervals between chunks are not intervals between individual tokens.

Length reconciliation retains the output budget / policy, actual distributions, and peak combined input and output for the same request, listing server-reported usage
and local estimates separately. Naturally short outputs and fixed-length diagnostics can both be retained, with finish_reason and errors explaining differences.

Cache report coverage lists the number of requests with reports out of all actually initiated requests, plus the number of requests with unknown cache information. The weighted cache ratio is calculated only for requests that explicitly report
both server-side input tokens and cached tokens. Coverage in older reports uses successful requests as its denominator; exports identify this legacy definition.
Being a first turn does not prove a cold cache, and changing the seed or performing warmup does not prove complete cache isolation.
Caches are not currently cleared automatically, and the actual cache state at the start of a point is marked unknown.

## Export and Compare

The detail page's **Export report** menu offers four formats for reading, sharing, or data analysis:

| Format | Content and features | Uses and limitations |
|---|---|---|
| **Offline HTML** | Saves the preview in the platform language. The single file includes ECharts, the original report tables and request/session evidence. | Offline, switch the time-series metric, toggle a legend entry or select a throughput point. The captured preview and tables also open with JavaScript disabled. |
| **PNG** | Saves the current preview as a single long image, including the displayed timeline and selected request details. | Suitable for direct sharing or inserting into documents. Text and tables are stored as an image. Export fails if the report exceeds the browser's single-image capacity; show fewer columns or choose HTML instead. |
| **Markdown** | Lists evaluation configuration, results for each point, request and session counts, turn groups, metric definitions, and the deployment snapshot as text and tables. Includes request timings, sources, errors, and other details when per-request records are retained. | Suitable for editing, documentation, or version control. Does not preserve page styling or interactive charts; content is independent of the page's selected point and visible columns. |
| **CSV** | Usually one row per point, with scalar columns such as throughput, latency, and success rate, plus versioned grouped evidence in `evidence_json` and sanitized configuration and report context in `report_context_json`. Unavailable numeric values are empty. | Suitable for filtering, calculations, and comparisons in spreadsheets or scripts. Full groups require parsing the JSON columns. Does not include page charts; content is independent of the page's selected point and visible columns. |

HTML and PNG preserve the current language, curve metric, selected point, and visible columns. Advanced metrics and the run snapshot expand automatically,
and all selected table columns are included. Online actions such as back, rerun, and detail downloads are excluded. Active runs retain their live/provisional
notices; exported files do not continue to update.

The **request timeline** is interactive on the online result page. After choosing a point, inspect requests by session, turn, and attempt, from send through first generated output,
first visible content, last output, and stream completion. Click a bar or use the keyboard to select a request and inspect its sources, latency, and errors.
Long records are paginated, with the current page and retained record count stated explicitly. The exported preview retains the timeline page and request selected at export time.

In HTML, turn on **Show full metrics** to see the original columns on the same point rows. Expand the detailed results below the preview for all retained points, percentiles, samples and warnings.

Metrics with the same name use consistent count accounting, grouping, and statistical definitions across formats. Disabled, unavailable, or truncated per-request details are explicitly indicated.
Core session and turn summaries do not depend on detail files; unobserved events remain unknown. CLI and existing report API exports remain compatible.

Enable **Keep per-request details** to download compressed JSONL from a point or the evidence panel. It contains Run, point, session, and turn identities,
finish_reason, error text, HTTP status, timing, raw usage, and local estimates. Bodies may contain sensitive information,
so enable this only when needed and control the detail capacity. Historical records that lack newer identities or grouping show unknown; missing information must not be inferred after the fact.

Among measured points that satisfy the configured thresholds, the “Best measured point in this run” is selected according to the declared objective. If no point qualifies, none is recommended;
unspecified experience thresholds are not filled in automatically. Shorter outputs, sessions ending early, or insufficient cache evidence trigger a notice that comparison conditions have changed.
A single parameter scan does not establish the throughput ceiling, and stability cannot be determined without repeated measurements.

Information that cannot be obtained automatically from external endpoints, such as GPU model, precision, and the actual kernel, appears as unknown. Managed deployments export snapshots of existing GPUs, TP,
images, and startup arguments. Configuration values cannot substitute for logs or profiler evidence of the kernel actually running.
