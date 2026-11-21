# Benchmark Information and Reporting Standard

This standard governs information retention, statistical definitions, interpretation, and report export for performance benchmarking, covering load generation,
server/agent load generation, aggregation, persistence, APIs, the CLI, and the UI. Terminology follows
[`CONTEXT.md`](../../CONTEXT.md); for user instructions, see [Run Benchmarks and Interpret Reports](../how-to/benchmark.md).

This is the current development and review contract, not a statement that every capability is implemented, and it does not define a release schedule.
Existing gaps can be addressed individually. Facts that cannot be recovered from historical data must be marked unknown, never filled in by inference.
When changing a stage, check the affected information categories, upstream and downstream definitions, and corresponding acceptance requirements. A single change does not need to close every gap.

For the inventory of existing capabilities and gaps as of 2026-10-20, see the [Initial Inventory](../../artifacts/acceptance/benchmark-information-inventory-2026-10-20.md).
That inventory does not replace this standard or represent the implementation status of later versions.

The following levels trace a benchmark: a **Run** is a complete benchmark execution containing one or more **test points**.
Each test point corresponds to a specific parameter combination, such as concurrency, input length, and output length. In multi-turn scenarios, a **session** contains multiple requests executed in order.
A **logical request** corresponds to one planned turn. With retries, the same logical request has multiple actual attempts.

## Information Categories and Retention Requirements

“Retention” means that facts or verifiable derivation inputs are available for report traceability. It does not require every format to display every field
or require duplicate persisted fields. Reuse existing facts and derive values when reading wherever possible. New data structures,
persistence, and dependencies must still follow the development rules in the repository root.

| Category | Question Answered | Required Information |
|---|---|---|
| Load plan and configuration | How does the benchmark run? | Dataset and generator versions; target/budget/fixed-length semantics; average/first-turn/peak input; expected context; planned sessions and turns; concurrency or arrival rate; seed and per-point derivation rules; load fingerprint; duration- or count-based termination conditions; timeout/retry/warmup/cache policies |
| Request and session structure | Which entity does the result belong to? | Associations among Run, test point, session, turn, logical request, and retry attempt; session start/end and termination turn; causal ordering within a session |
| Actual input and output | How much load actually ran? | Per-request input/output; context length as input plus output; reasoning/body breakdown when the endpoint provides it; means, distributions, peaks, and planned-versus-actual deviations; local estimates and server usage retained separately |
| Execution results and completeness | How much was attempted and completed? | Planned/initiated/succeeded/failed/skipped/canceled counts; complete and interrupted sessions; error stage, error text and status code, finish_reason, retry count, and elapsed time |
| Timing and interaction experience | Where was time spent? | Planned arrival, client waiting, actual send, first generated output, first visible body text, last generated output, stream end, and session end; per-request latency, grouping by turn, and valid sample counts |
| Cache and reuse evidence | What was reused? | Designed prefix reuse rate; per-request cache fields and input tokens; reporting and unknown request counts; weighted ratios by group; cache policy before each point and evidence of actual execution |
| Throughput and load over time | How much work did the system actually handle? | Total tokens, exact measurement windows and throughput formulas; rules for handling successful output and partial failed output; actual active-session and in-flight-request curves; idle/waiting time; session duration and completion rate |
| Environment and deployment snapshot | Under what conditions were results produced? | Endpoint, load-generation location and client version; GPU model/count; TP/EP/DP; model/tokenizer version or digest; weight/activation/KV precision; engine version; image digest; key launch parameters; service limits; distinction between configuration and actual runtime evidence |
| Comparison conditions and conclusions | What can be compared, and why is it recommended? | Load shape, output and cache policies, measurement protocol, repetition count and variation, experience thresholds, reasons for including/excluding candidates, objective function, recommendation scope, and evidence gaps |

## Attributes Common to All Information Categories

- **Source**: Distinguish user configuration, local tokenizer calculations, character-based estimates, server usage, client timing,
  the deployment system, manual entry, logs, and profilers. Do not mix configured values with values actually in effect.
- **Scope**: Identify the Run/test point/session/turn/request and whether the population comprises successful requests, all initiated requests, or another subset.
- **Definition**: Specify units, numerator, denominator, valid sample count, exclusion conditions, and formula. For percentiles, identify the metric and population.
- **Validity**: Distinguish observed, estimated, unknown, and not applicable. Unknown is not 0; a ratio with a zero denominator is neither 0% nor 100%.
- **Association and completeness**: Raw facts must be traceable to requests. Record whether details are enabled, truncated, or missing, and their retention scope.
  Do not include raw credentials in reports. Retaining request bodies requires explicit configuration, redaction, and capacity limits. Identity and statistical definitions must not depend on body content.

## Reconciling Request and Session Counts

A logical request is one planned turn; a retry is an additional attempt for the same logical request. Count the two levels separately.
A request without retries also has only one attempt. Classify terminal logical-request states, such as success and failure, by the final result without counting attempts again.

For a finished test point with complete records, the following must reconcile:

```text
Planned logical requests = Initiated logical requests + Uninitiated logical requests
Initiated logical requests = Successful + Failed + Canceled after initiation
Uninitiated logical requests = Skipped due to an earlier failure + Canceled before initiation + Other uninitiated requests (specify the reason)
Request success rate = Successful logical requests / Initiated logical requests
Session completion rate = Sessions with all required turns successful / Planned sessions
```

During execution, count queued, in-flight, and not-yet-arrived requests separately. Do not force reconciliation using terminal-state formulas when execution is canceled or records are lost.
Explicitly record “no retries” for a no-retry policy. With retries, retain each attempt's result and elapsed time, backoff waiting, and total elapsed time.

A Run's `done` state only means execution has ended; it does not establish request success or successful completion of all session turns.
Progress must not present uninitiated turns as completed requests. If progress means planned items have been handled, display that meaning explicitly.

## Reconciling Lengths and Tokens

- An output budget only specifies an allowed upper bound. For fixed-output targets, record the strategy actually in effect and verify observed values.
  Setting `ignore_eos` does not establish that the endpoint generated a fixed length. Retain workloads with naturally short outputs,
  and label fixed-length diagnostic scenarios separately instead of combining them in comparisons.
- Store and display server usage, local tokenizer counts, and character-based estimates separately, labeling the sources of input and output independently.
  Precisely generated input does not guarantee that endpoint-observed token counts match local counts. Reconcile additional overhead such as chat templates.
- Report input/output distributions, peaks, and deviations. Use finish_reason, errors, and source information to flag short outputs,
  budget overruns, or context risks. A deviation alone does not establish tokenizer, cache, or server errors as the cause.
- Expected context is a planned value. Derive the actual peak context by summing input and output for each request and then taking the maximum.
  Do not add peaks from different requests or substitute average output for the output of the peak-context request.
  Mark unknown service limits as unknown; do not present an estimated required length as the endpoint's actual limit.

## Turn and Cache Definitions

First/subsequent turns and cache hit/miss/unknown are independent dimensions. Turn number cannot replace cache facts.
At minimum, provide TTFT, TPOT, completion latency, valid sample counts, and cache statistics separately for first and subsequent turns,
while retaining the basis for analysis by specific turn and identification of anomalous requests.

For any explicitly declared request population:

```text
Cache reporting coverage = Requests with reported cache fields / Requests in the set
Cache ratio among reporting requests = Σcached_tokens / Σinput_tokens
                                      (Both sums include only requests with cache reporting and an identified input source)
```

Do not directly average per-request percentages. At minimum, list the reporting count, unknown count, and coverage among all actually initiated requests.
If coverage among successful requests is also provided, label it explicitly; do not mix its definition with coverage among all initiated requests.

An explicitly reported `cached_tokens=0` means a miss; an absent report means unknown. A default value for an optional field must not masquerade as an explicit report.
When input tokens from the same source are unavailable, label the cache ratio as uncomputable or estimated as appropriate. Do not silently mix in character-based estimates.

Retain designed reuse opportunities, server cache ratios, and client latency observations separately. Without controlled comparisons,
do not describe correlation as a benefit caused by caching. For each point, specify cleared, warmed, reused, or unknown, distinguishing the planned policy,
actual execution result, and verifiable state. Changing the seed or adding a session prefix does not prove a cold cache or complete isolation.
When comparing points, record their order and the risk of cache carryover. There is no requirement to invent cache-clearing capabilities for external endpoints outside your control.

## Timing, Throughput, and Statistical Interpretation

- For TTFT (time until the first generated output is received), state whether the first output includes reasoning. Retain the time to first visible body text separately.
  Last generated output and stream end are distinct events. Specify which defines request completion time rather than using an ambiguous E2E label for both.
- TPOT is the average time per output token during a request's decode phase. When a report uses
  `(last output generation time − first output generation time) / (output token count − 1)`,
  explain the observability conditions, token source, and exclusion rules and counts for short-output, single-chunk, non-streaming, and failed samples.
  Unobservable values must not enter statistics as zero elapsed time.
- A chunk can contain multiple tokens. Chunk intervals cannot be called per-token intervals, and chunk counts do not establish quantization-kernel or speculative-decoding behavior.
  `1000 / TPOT P50(ms)` represents only median decoding interaction speed. It excludes TTFT and does not equal overall throughput divided by concurrency.
- Speculative-decoding report fields are derived from chunks and must state their denominator. `tokens_per_iteration` is the number of tokens emitted per iteration;
  the `acceptance_rate` derived from it (`1 - 1 / tokens_per_iteration`) has the **emitted tokens** as its denominator,
  meaning "the share of the output that came from drafts". That is not the engine's own "accepted drafts / proposed drafts", so the two must not be cited
  interchangeably, and this field does not establish the draft model's hit rate. For the engine definition, take the engine counters
  (`spec_draft_tokens_total` / `spec_accepted_tokens_total`, see [Inference Metrics](../reference/inference-metrics.md)).
- Retain the wall-clock window for overall throughput: the actual elapsed time from measurement start to end, including first-turn processing, between-turn waiting, and completion within the window. List any steady-state or rolling peak separately,
  with its window and calculation method. Explicitly state whether failed-request elapsed time, partial tokens, and retries are included; do not silently exclude them.
- Record planned session concurrency, actual active sessions, and actual in-flight HTTP requests separately. C denotes configured session concurrency; configured C does not prove that C requests are running throughout.
  Separate client queuing from network/service processing time. Without telemetry, do not infer server-internal durations from TTFT and present them as facts.
- All percentiles must have verifiable valid sample counts. Explain limitations of tail percentiles from small samples and variation across repeated measurements.
  Session duration covers the entire flow. List interrupted sessions separately; do not include them in the completion rate for complete sessions.

## Comparison, Recommendations, and Environment Evidence

Select by the declared objective function among comparable test points that satisfy user-specified thresholds such as time to first token, completion latency, and session completion rate.
Do not invent unset thresholds; explicitly display any default thresholds. Making no recommendation is valid when no point qualifies.
Search decisions, the UI, and exports must reuse the same thresholds and definitions rather than implementing conflicting recommendation rules independently.

Before comparing, check load seeds/derivation strategies and length distributions, output policies, cache policies, measurement protocols, retries and timeouts,
load-generation locations, and deployment snapshots. Comparability concerns controlled load-generation conditions; it does not require every point to reuse exactly the same prefix.
Seed changes used to prevent cache contamination must themselves be reproducible and traceable.

When output becomes substantially shorter, sessions terminate early, or conditions are unknown, indicate “comparison conditions have changed” or “insufficient comparison evidence.”
Do not interpret the highest number directly as increased capacity.

Use the recommendation wording “best among the points tested in this run,” and state the tested range, thresholds, objective, and repeated-measurement status.
Do not claim a throughput ceiling before finding a stable boundary. Do not describe P99 as the typical or average wait time.

Reports must include a readable deployment snapshot. When an external endpoint cannot provide it, allow manual entry and label its source, entry time, and unknown items.
Configured precision and launch parameters do not establish the kernels actually executed; logs or profiler evidence must support the latter.

## Export and Acceptance of Subsequent Changes

Machine-readable details must have stable fields and versions, with explicit time units, association identifiers, error text, token sources, and truncation flags.
When per-request JSONL is enabled, it must support session/turn traceability. Truncation or disabled recording must not appear as “no anomalous requests.”
Counts and groupings needed to explain aggregates must not depend exclusively on optional or truncated body files for recovery.

HTML, Markdown, CSV, API, and CLI outputs must use consistent definitions for metrics with the same name. Compact formats can link to details or explain omitted items.
Offline reports must retain the information needed to interpret their conclusions without relying solely on the original service remaining online.

When modifying related implementations, provide behavioral verification for the affected scope and consider at least the following counterexamples. This list defines acceptance requirements;
it does not claim that existing automated tests already cover them:

| Scenario | Behavior to Demonstrate |
|---|---|
| No retries; 4 sessions × 13 turns; one session fails on its first turn and the others complete | 52 planned, 40 initiated, 39 succeeded, 1 failed, 12 skipped; request success rate 97.5%, session completion rate 75%; the failed session can be identified |
| A first-turn cache hit, followed by a later turn with a miss or no report | Turn grouping is independent of cache facts; zero and missing values are distinguished; first turns are not all classified as cold cache |
| Two requests: input 100/cached 100 and input 900/cached 0 | Weighted ratio 10%, not the 50% average of percentages; the coverage denominator and treatment of unknown/failed requests are explicit |
| Configured output 220, actual natural stop at 50 | Budget, actual value, and finish_reason are retained separately; the result is not labeled as completed fixed-length output |
| Missing usage, only one generated chunk, reasoning before body text | Estimates, unobservable TPOT, and time to first body text are labeled correctly, without creating zero-duration samples |
| Non-2xx, network failure, interrupted stream, cancellation, or retry | Failure stage/text/timing and attempt associations are retained; counts reconcile; uninitiated requests are excluded from the success-rate denominator |
| Output becomes shorter and tail latency worsens as C increases | Median interaction speed does not conceal changes in the tail or workload; recommendations are interpreted using thresholds and comparability |
| Old records, disabled/truncated details, incomplete external snapshots | Unknowns and evidence scope are explicit; session identities, cache states, and hardware configurations are not guessed |

Submission notes must list the affected categories, definition or compatibility changes, verification evidence, and remaining uncovered gaps.
Updating only this standard or the inventory does not complete feature acceptance. When adding or extending automated gates, record their actual coverage.

<!-- translation-source-sha256: 8a76e9501934ea00f2cf39296544004fe58dba4bd60612b8f4e50568ca56db3f # Benchmark Information and Reporting Standard -->
<!-- translation-source-sha256: 7e28e648152adfc6f885f1409f22269b89801588761926214f309f24c8282696 ## Information Categories and Retention Requirements -->
<!-- translation-source-sha256: f47a00ea6d52c38c8964b30ca40fe47730b78b496eb242efc303f6754e9de50f ## Attributes Common to All Information Categories -->
<!-- translation-source-sha256: 0c2fe8958311c8f609d9bc558146697411d9ba7f7b7ea301fac676a27612ceca ## Reconciling Request and Session Counts -->
<!-- translation-source-sha256: e96954ac965202079b6ed74ebb58a9cbdc00f666ff0feaa50775fb31b494355c ## Reconciling Lengths and Tokens -->
<!-- translation-source-sha256: 2b1457fa6d8643ee51e7e753719db95f18d6ac54eb86546d87191e7250f8ddd4 ## Turn and Cache Definitions -->
<!-- translation-source-sha256: 4580cde7711c393ee67de6a19d858460a7a28405a8ab23432242d462fc7f36b8 ## Timing, Throughput, and Statistical Interpretation -->
<!-- translation-source-sha256: aef976fd35a32f3cbeaef18c1def291bd256bcf953650040779ad5eedce7e415 ## Comparison, Recommendations, and Environment Evidence -->
<!-- translation-source-sha256: e13e157a5d429d4b6f16f2291ee3219949f22b410d23478a1c8105355bfd7d89 ## Export and Acceptance of Subsequent Changes -->
<!-- translation-localized-code-block: 977144588ed733c0dadfd47e149c044f60bba423c5b93e11b83c5513e1614dff 6b511c4b3f0f41683b442a4e6cb0a145a99b4e27746207c44f3779b6d591bcdc Reviewed localization of visible diagram labels, prose formulas, writing template, or directory annotations; executable examples remain unchanged. -->
<!-- translation-localized-code-block: 4c924402689a27813f3d7761be0409e453871ab4f5fb56bdc20093d442152dbe 7f4166cdce26676d6792f6702d52311a6771e8c4729d36cab34b5144e363572d Reviewed localization of visible diagram labels, prose formulas, writing template, or directory annotations; executable examples remain unchanged. -->
