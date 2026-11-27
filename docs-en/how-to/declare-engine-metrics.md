# Declare Engine Metrics for an Inference Template

**When to use this guide**: You have written an inference template (especially for a custom or third-party engine with `engine: callable`), but the inference panels in the deployment details page, instance page, and service monitoring page show no data after deployment. Or you want the engine's own TTFT, KV utilization, and speculative acceptance rate to appear in platform panels.

After completing this guide, the template's metric mappings pass validation, and you can see the corresponding data in the platform panels or metrics explorer after an actual inference request.

**Prerequisites**: The engine provides a Prometheus text or JSON metrics endpoint that the node's agent can reach through the instance port. You can sign in to that node to inspect the raw response, and you can import or edit inference templates (see the [Inference Catalog model.yaml Syntax Reference](../reference/inference-catalog-spec.md)).

## Decide Whether a Declaration Is Needed

First use the table below to decide whether you need to modify the template. Every 10 seconds, the agent scrapes the endpoints of running instances and converts the results to [platform metrics](../reference/inference-metrics.md) using mappings. When `api.metrics` is not declared:

| Template engine | Default behavior | Is a declaration needed? |
|---|---|---|
| `vllm` / `sglang` | Uses the platform's built-in vLLM / SGLang mappings | No; to add or change a few mappings, specify only `map` |
| `callable`, with metrics prefixed by `vllm:` or `sglang:` (a modified vLLM / SGLang engine or an adapter that wraps SGLang) | Scrapes `/metrics` and automatically applies the vLLM or SGLang mappings based on the detected prefix | Usually not |
| `callable`, with the engine's own metric names (such as TensorFold's `tensorfold:`) | Scrapes `/metrics`, but no names match, so no engine metrics are produced | Yes |
| The engine's metrics endpoint returns JSON (such as `/metrics` in Strata 0.1.38) | The Prometheus text parser cannot parse any lines | Declare `format: json`; see below |
| The engine owns the API port, has no `/metrics` there, but a launcher in the image can export metrics on another local port | Scrapes `/metrics` on the API port and gets nothing | Declare `port: separate`; see [Metrics on another port](#指标在另一个端口上) below |
| The engine has no metrics endpoint | Scrape failures are skipped | A declaration does not help; the gateway collects latency, token, and similar statistics |

When a `callable` template does not declare `api.metrics`, importing it (or prechecking it with `POST /api/v1/inference-templates/evaluate`) returns the warning `CATALOG_CALLABLE_METRICS_UNDECLARED`. The UI lists this warning after import or save. It does not block import and can be ignored if the engine has no metrics endpoint or uses vLLM / SGLang metric names (see [Pre-import Checks and Warnings](../reference/inference-catalog-spec.md#导入前检查与警告)).

## 1. Inspect the Engine's Raw Metrics

On the node hosting the instance, request the instance port directly (find the port in the instance details; the agent uses this same address for scraping):

```bash
curl --silent --show-error --include http://127.0.0.1:<instance-port>/metrics
```

If the engine's endpoint is not `/metrics`, replace it with the actual path first; for a template that declares `port: separate`, the port is the one after the API port in the `ports` list of the instance detail, not the API port. Confirm that the HTTP status indicates success, then inspect the response headers and body. Do not filter out `#` lines yet; you need `# TYPE` later to determine metric types.

For runveil nodes, also run the request on the host. If curl is available inside the container, you can instead run `gpuworks-agent runveil exec <container-name-prefix> curl --silent --show-error --include http://127.0.0.1:<instance-port>/metrics` (see the [CLI Reference](../reference/cli.md#gpuworks-agent-runveil)).

Check the response's `Content-Type`: `application/json` indicates a JSON endpoint; write its mappings as described under [JSON Endpoints](#json-端点). For Prometheus text, inspect the output to confirm three things: the family name (`tensorfold:requests_running`), the type (the `# TYPE` line; histograms have three sets of lines: `_bucket` / `_sum` / `_count`), and labels (whether a family has multiple lines distinguished by labels such as `tier` or `state`).

## 2. Write the Mappings

Add `metrics` under `api` in the spec. Each key is an engine metric, and its value is the platform metric's **short name**:

```yaml
api:
  operations:
  - type: openai.chat
  metrics:
    endpoint: /metrics       # Optional; defaults to /metrics
    map:
      'tensorfold:requests_running': requests_running
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}
```

Rules:

- **Use a short name for the target** (`ttft_seconds`), not the storage name `gpuworks_infer_ttft_seconds`. The short name must appear in the [vocabulary](../reference/inference-metrics.md).
- **Map histograms by family name**: Use `tensorfold:time_to_first_token_seconds`, not `..._bucket`. The agent converts `_bucket`, `_sum`, and `_count` together. Histograms cannot use label selectors or scaling. Do not separately map lines such as `_count` from the same family.
- **Select a line by label**: A key can include a selector, for example, `'vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}': kv_disk_hits_total`. Only lines with matching labels are selected; label values inside the selector must be enclosed in double quotes.
- **Other labels are not retained**: Stored series carry only platform labels (instance, deployment, engine, and node). Multiple lines in a counter family (for example, separated by `model_name`) are summed. For a gauge family with multiple lines, only the first is used, so use a selector to choose the line you want when a gauge has lines distinguished by labels such as state.
- **Scaling**: The long form `{to: <short-name>, scale: <factor>}` applies to counters and gauges. For example, use `scale: 0.01` to convert a percentage from 0–100 to a ratio from 0–1, or `scale: 1000` to convert counts in thousands of tokens to tokens.
- **Inheritance**: `inherit: vllm` or `inherit: sglang` first applies that engine's complete built-in mappings, then applies `map`. For the same key, `map` takes precedence (the template wins). Templates for the `vllm` / `sglang` engines inherit their own mappings even without `inherit`.
- Once `map` or `inherit` is specified, automatic detection for callable engines no longer runs.

For engines that report only KV pool capacity and free space, map them to `kv_capacity_tokens` and `kv_free_tokens`. The platform derives KV utilization (see [Derived Metrics](../reference/inference-metrics.md#派生指标) in the vocabulary).

## 3. Import and Fix Validation Errors

When you import YAML, upload a catalog package, or save in the template editor, `api.metrics` is validated along with the other fields. Any error causes the entire submission to be rejected. The template editor provides completion for target names.

| Error code | Cause |
|---|---|
| `CATALOG_METRICS_ENDPOINT_INVALID` | `endpoint` is not a path beginning with `/` |
| `CATALOG_METRICS_INHERIT_UNKNOWN` | `inherit` is neither `vllm` nor `sglang` |
| `CATALOG_METRICS_SOURCE_INVALID` | The key is not a valid metric family name, or the selector syntax is invalid (values require double quotes) |
| `CATALOG_METRICS_UNKNOWN_METRIC` | The target short name is absent from the vocabulary (a typo, or a storage name beginning with `gpuworks_infer_`) |
| `CATALOG_METRICS_HISTOGRAM_SELECTOR` | The key for a histogram target includes a label selector |
| `CATALOG_METRICS_HISTOGRAM_SCALE` | Scaling is specified for a histogram target |
| `CATALOG_METRICS_NEGATIVE_SCALE` | The scale factor is negative |
| `CATALOG_METRICS_SHADOWED_HISTOGRAM` | A `_bucket` / `_sum` / `_count` line is mapped from a histogram family that is already mapped |
| `CATALOG_METRICS_DUPLICATE_SOURCE` | The same key appears twice (after trimming leading and trailing whitespace) |
| `CATALOG_METRICS_PORT_INVALID` | `port` is neither `api` nor `separate` (the platform allocates port numbers; a concrete number cannot be written) |
| `CATALOG_METRICS_PORT_UNDECLARED` | A launch field references `${GPUWORKS_METRICS_PORT}` but `port: separate` is not declared |
| `CATALOG_METRICS_PORT_UNREFERENCED` | `port: separate` is declared but no launch field references `${GPUWORKS_METRICS_PORT}` |
| `CATALOG_METRICS_FORMAT_INVALID` | `format` is neither `prometheus` nor `json` |
| `CATALOG_METRICS_JSON_INHERIT` | `inherit` is specified with `format: json` |
| `CATALOG_METRICS_JSON_PATH_INVALID` | A key is not a dot-separated key path with `format: json` |
| `CATALOG_METRICS_JSON_HISTOGRAM` | A mapping targets a histogram with `format: json` (JSON endpoints support only counters and gauges) |
| `CATALOG_METRICS_JSON_COMPARE_COUNTER` | A comparison expression (`path!=value`) maps to a counter with `format: json` |

### Metrics on another port

Some `callable` engines own the API port and serve only `/health`, `/v1/models` and `/v1/chat/completions`, with no `/metrics`. A launcher in the image can read metrics from the engine's admin interface or logs and export them on a second local port (it does not proxy requests). Have the platform reserve that port and scrape it:

```yaml
args: [--port, "${GPUWORKS_API_PORT}", --metrics-port, "${GPUWORKS_METRICS_PORT}"]
api:
  metrics:
    port: separate
    endpoint: /metrics       # Optional; the path on this port
    map:
      'engine:requests_running': requests_running
```

- With `port: separate` declared, the platform reserves one more port per instance and tells the launcher where to listen through `${GPUWORKS_METRICS_PORT}` (it can be referenced in `args`, `env`, `entrypoint` and `pre_start_*`); the agent scrapes `http://127.0.0.1:<that port><endpoint>` instead of the API port. The port number cannot be written in the template.
- The variable and the declaration must appear as a pair; either alone is rejected at import (see `CATALOG_METRICS_PORT_*` in the table above).
- In a multi-node instance only the rank that serves has this port; `${GPUWORKS_METRICS_PORT}` is `0` on a headless rank, so one command expands as usual.
- The port is fixed with the template snapshot when the deployment is created; changing `port` in the template later does not affect existing instances.

For the full semantics, see the [Inference Catalog model.yaml Syntax Reference](../reference/inference-catalog-spec.md#apimetrics引擎指标映射).

## 4. Verify That the Mappings Take Effect

1. After saving the template, send an actual inference request through a deployment that references the tier, ensuring that the metrics you want to observe produce samples.
2. Wait for the next scrape and check the inference panel in the deployment details page or service monitoring page. You can also query your mapped metrics in the metrics explorer, for example,
   `gpuworks_infer_requests_running{deployment="<deployment-id>"}`. Short requests may finish between scrapes, leaving the running request count at 0; also check whether mapped token or completed request counters increase.
3. If no samples appear, compare the raw endpoint, path, and mapping names using the troubleshooting table at the end. Saving the template successfully does not by itself confirm that collection works.

New deployments use the current mappings directly. Running deployments need no restart or redeployment: after you save the template, the mappings are pushed to instances that reference the tier.
Agents use them on the next scrape after receiving them; offline nodes receive them only after reconnecting. Changes to built-in template mappings from a server upgrade work the same way.

Older agents recognize only simple mappings. Entries with scaling, derived KV utilization, and automatic detection for callable engines take effect only after the node's agent is upgraded.

## Examples

Place all the following snippets under the template's `api`. Preserve existing settings such as `api.operations`; do not put `metrics` at the top level of the template.

**TensorFold** (the built-in GLM-5.3-Flash TensorFold template; names come from the actual `/metrics` output of TensorFold 0.6.0):

```yaml
metrics:
  map:
    'tensorfold:requests_running': requests_running
    'tensorfold:requests_waiting': requests_waiting
    'tensorfold_health:requests_total': requests_success_total
    'tensorfold:prompt_tokens_total': prompt_tokens_total
    'tensorfold_health:completion_tokens_total': generation_tokens_total
    'tensorfold_health:cached_tokens_total': prompt_tokens_cache_hit_total
    'tensorfold:mtp_drafted_total': spec_draft_tokens_total
    'tensorfold:mtp_accepted_total': spec_accepted_tokens_total
    'tensorfold:time_to_first_token_seconds': ttft_seconds
    'tensorfold:request_latency_seconds': e2e_seconds
    'tensorfold_health:pool_tokens': kv_capacity_tokens
    'tensorfold_health:pool_free_tokens': kv_free_tokens
```

**An adapter that wraps SGLang but changes metric prefixes**: Inherit SGLang's mappings, then add the renamed metrics.

```yaml
metrics:
  inherit: sglang
  map:
    'adapter:num_running_reqs': requests_running
    'adapter:num_queue_reqs': requests_waiting
```

**A custom engine whose metrics endpoint is not `/metrics` and that reports KV utilization as a percentage**:

```yaml
metrics:
  endpoint: /stats/prometheus
  map:
    'myengine:active_requests': requests_running
    'myengine:pending_requests': requests_waiting
    'myengine:output_tokens_total': generation_tokens_total
    'myengine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}
```

## JSON Endpoints

Some engines return statistics in a JSON document instead of Prometheus text, such as `/metrics` in Strata 0.1.38:

```json
{"live": {"state": "generating", "queued": 0, "prompt_tokens": 50, "generated": 1971},
 "totals": {"requests": 13, "prompt_tokens": 2548, "output_tokens": 3356, "drafts_offered": 2645, "drafts_accepted": 1578}}
```

Declare `format: json` under the template's `api.metrics` and use dot-separated key paths as the keys in `map`. The following snippet also belongs under `api`:

```yaml
metrics:
  format: json
  map:
    'live.state!=idle': requests_running
    'live.queued': requests_waiting
    'totals.requests': requests_success_total
    'totals.prompt_tokens': prompt_tokens_total
    'live.prompt_tokens': prompt_tokens_total
    'totals.output_tokens': generation_tokens_total
    'live.generated': generation_tokens_total
    'totals.drafts_offered': spec_draft_tokens_total
    'totals.drafts_accepted': spec_accepted_tokens_total
```

- Numeric values at a path are collected; booleans are recorded as 1/0. `null` and missing paths produce no metric.
- For strings, write a comparison: `path==value` or `path!=value`. A true comparison is recorded as 1, and false as 0 (`live.state!=idle` represents the number of requests being processed). Comparisons can map only to gauges. If the compared value is not a string, no metric is produced.
- Only counters and gauges can be mapped. JSON documents contain no histograms, so histogram metrics such as TTFT and end-to-end latency are collected by the gateway.
- `inherit` is not allowed, and built-in engine mappings are not applied. The long form `{to, scale}` remains available. Two paths mapped to the same metric are summed.
- Pay attention to what each counter includes. Strata's `totals` increments only when requests finish. Mapping only this field makes generation throughput stay unchanged during long responses, then jump all at once at the end. `live` describes the request currently being processed. When it finishes, its generated amount is merged into `totals`. Mapping `totals.output_tokens` and `live.generated` to the same counter sums them, so throughput grows continuously during generation.
- A counter summed this way drops when a request is canceled (`live` is cleared, but `totals` is not incremented). The platform treats a counter decrease as an engine restart and counts the entire reading as an increase. To handle this, the agent holds JSON endpoint counters steady: if a decreased value is still at least half the previous reading, it holds the previous reading until the counter exceeds it again. Only a drop below half is passed through as an engine restart.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The panel never shows engine data | Whether curl in step 1 returns output; whether `endpoint` matches the engine's actual path; whether keys match the raw output exactly (including prefixes and colons) |
| Only some metrics have data | Whether the missing metrics exist in the raw output; whether labeled families need selectors |
| Values are off by a fixed factor (such as 100) | The engine uses different units from the vocabulary; add `scale` |
| A gauge has the wrong value | The family has multiple lines, and only the first was used; select the required line with a selector |
| The throughput chart stays at 0 and spikes when requests finish | The mapped counter increments only when requests finish (check whether `# HELP` says finished requests). Map a counter that includes in-flight requests instead. For example, use `tensorfold_health:completion_tokens_total` instead of `tensorfold:generation_tokens_total` for TensorFold |

<!-- translation-source-sha256: 7d1bc400bdf2eeb62b7769e71e4db6ec17cd0b7116c4a2f0237ecc8c09cdf25a # Declare Engine Metrics for an Inference Template -->
<!-- translation-source-sha256: a7e12b5a1ed121fe255bed8f6daec1500dfdc0307eaa35374b6668ecafbecb6c ## Decide Whether a Declaration Is Needed -->
<!-- translation-source-sha256: e1748cd0fd6ba7c66be0266dedd804cdd0d18703b1d1a37beeed6f503934aada ## 1. Inspect the Engine's Raw Metrics -->
<!-- translation-source-sha256: 7af0953d32381163564a17cf0c9003a3f6c673198e6f244d9270a9f7a765fa55 ## 2. Write the Mappings -->
<!-- translation-source-sha256: 5aedb05cb55ae87339587e2043e124cef6cf2b5050b515c17b4a76082c000255 ## 3. Import and Fix Validation Errors -->
<!-- translation-source-sha256: 670fb58c8b0107b25f330ec7913ed55f48fc9861c33c20fb54ecc4cb5e85624f ### Metrics on another port -->
<!-- translation-source-sha256: d5214978d2dd05584bfaf225c1460786c88df88955d6634bb892303574ebeba8 ## 4. Verify That the Mappings Take Effect -->
<!-- translation-source-sha256: fa7021c5d35923b824db7ffd56e0ad463154a8f360f389e86087d6d244b01090 ## Examples -->
<!-- translation-source-sha256: 879cc3c6d61b5db426963fda486555fa03e51b79a2d3d45ca9dea4988c9d172a ## JSON Endpoints -->
<!-- translation-source-sha256: 8aa95ab3f0d6388120b259b92aef920cff2d8bb8a13c2eec604304747f7c3297 ## Troubleshooting -->
<!-- translation-localized-code-block: efcf48e7f67274b8efe441b319eef5025b39ede13d8a161e1aec2d1f830e9ef4 61583f8a3fb80812b8068e4e06e936197cd8e6cc8a5b79258c5faa89422224c7 Translate descriptive Chinese placeholders, preserving quotes, command flags, API fields, and actual paths. -->
<!-- translation-localized-code-block: 9c2e5fe6142d1639ab7482aed29d76bdbf48a8066c12241135466638dc4111d2 ae6fd25a2a23dbeeecfc322e8af5ebf2d7fe7cedf6a2210f4dc9deaf5a495452 Translate Chinese code comments, preserving executable statements, parameter values, and paths. -->
<!-- translation-localized-code-block: 615324c25a0f9212a695ccbb296d80021adcc2baaf928315ed299c07e79f9500 a428cec71bf5d549bcf2bb996aa06b011b500991ae42ed928885ea69b8850bfa -->
