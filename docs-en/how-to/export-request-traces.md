# Export Request Traces to OpenTelemetry

**Use case**: Send per-request data from the platform gateway to your tracing system (Jaeger, Grafana Tempo, Grafana Cloud, Datadog, and others). Put your upstream gateway, the platform gateway, and your own services in the same trace, and navigate between the console and the tracing system by trace ID.

**Prerequisites**:

- You can change console settings as an administrator.
- You have an inference deployment that accepts requests and know its service name (`served_name`), and you have created an inference service token. See [Create an Inference Token and Send Requests](../tutorials/getting-started.md) in the getting-started tutorial.
- Prepare an endpoint that accepts OTLP/HTTP (an OpenTelemetry Collector or a tracing backend that supports the protocol). The server must be able to reach its OTLP/HTTP port (4318 by default). If you export only gateway traces, nodes do not need to reach it. If you do not yet have an endpoint, start a trial instance as described in section 1.
- Endpoints on ordinary private networks or the server's local loopback addresses need no allowlist by default. An allowlist is needed only in strict mode. Platform administrators can authorize the target host under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes to either field apply to the next request after saving, without restarting the server. See [Outbound Access Configuration](../reference/configuration.md#server-配置文件-serveryaml).

**What you get**: For every forwarded `/v1` or Playground request, the gateway sends a span after the request ends. It includes the model, caller, deployment and replica; input, output, cache-hit and reasoning tokens; sampling parameters; time to first token; time spent in the gateway, queue, prefill, reasoning and answer phases; and the result (completed, truncated, client disconnected, or error). If the upstream request includes `traceparent`, this span becomes a child of the upstream span. Prompts and replies are not exported, and the platform itself does not store them. See the [appendix](#附录span-字段) for the fields.

<a id="1-五分钟上手用-jaeger-看到第一条-trace"></a>

## 1. View Your First Trace in Jaeger

The Jaeger 2.x all-in-one image uses OpenTelemetry Collector at its core. A single container both accepts OTLP and provides a viewing interface (data is held in memory and lost on restart, making this suitable for trials).

1. Start Jaeger on a machine that the server can reach:

   ```bash
   docker run -d --name jaeger -p 4318:4318 -p 16686:16686 jaegertracing/jaeger:2.11.0
   ```

2. In the console, go to **Settings** → **Integrations** → **Request trace export (OpenTelemetry)** (requires an administrator):
   - Set **Collector address** to `http://<jaeger-host>:4318`.
   - Set **Trace link** to `http://<jaeger-host>:16686/trace/{trace_id}`.
   - Click **Send a test span**. “The collector accepted the test span” confirms that the address is reachable.
   - Turn on **Export** and save. The change takes effect immediately, with no restart required.

3. Send a request with a trace from a terminal that can access the server. Replace `<served-name>` with the service name `served_name` in the deployment details, rather than the deployment's display name. Use the inference service token created earlier.
   The `traceparent` format is `00-<32-hex-digit-trace-id>-<16-hex-digit-parent-span-id>-01`:

   ```bash
   curl http://<server>:8800/v1/chat/completions \
     -H "Authorization: Bearer <inference-token>" -H "Content-Type: application/json" \
     -H "traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" \
     -d '{"model":"<served-name>","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
   ```

4. Wait for the request to finish and confirm that the exported count on the settings card increases. Then open `http://<jaeger-host>:16686`, select `gpuworks` under Service,
   or search directly for trace ID `4bf92f3577b34da6a3ce929d0e0e4736`. Open the corresponding request's span to view its attributes and timeline events.
   A successful test span confirms only connectivity; seeing the real request's span confirms that the export workflow is complete.
5. Return to **Monitor Center** in the console, paste the same trace ID into the search box at the upper right, and open the request details. **Open in the tracing backend** takes you back to Jaeger.

Requests without `traceparent` are also exported: the gateway starts its own trace, subject to the sample ratio.

## 2. Connect a Production Environment

**We recommend running an OpenTelemetry Collector on the server's machine or in the same data center.** The platform sends only to this collector, which forwards batches to the remote backend.
Continue monitoring exported and dropped counts after deployment: collector capacity, network round-trip time, and request volume all affect whether export can keep up. See [Troubleshooting](#5-排查).

Example collector configuration that receives OTLP/HTTP and forwards to Tempo:

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
processors:
  batch: {}
exporters:
  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo]
```

Replace `tempo:4317` in the example with a backend address that the collector can reach, then save and load the configuration using your existing collector deployment procedure.
Confirm that the startup logs show no configuration errors and that the OTLP/HTTP receiver is listening on 4318, then change the platform's Collector address to that receiver.
Repeat the test span and real inference request from section 1, and confirm that you can find them in the production tracing backend. Saving the configuration file alone does not make the collector use it.

Settings:

| Setting | Description |
|---|---|
| **Collector address** | OTLP/HTTP address, such as `http://otel-collector:4318`. If the path is not `/v1/traces`, it is appended automatically. Only OTLP/HTTP (JSON + gzip) is supported; gRPC port 4317 is not supported. |
| **Headers** | Add headers when the backend requires authentication or tenant identification, such as `Authorization: Bearer …` or `X-Scope-OrgID: team-a`. Values are stored encrypted and are not displayed again after saving. Leave a saved value blank to keep it unchanged. |
| **Sample ratio** | Applies only to new traces started by the gateway. Requests with `traceparent` follow the caller's sampling decision (ParentBased). The default is 100%. The platform's own request view is unaffected. |
| **Trace link** | A tracing system URL containing `{trace_id}`, such as Jaeger's `http://jaeger:16686/trace/{trace_id}`. Once set, you can open the trace ID directly from the request details. |

**Send a test span** uses the values currently entered, without requiring you to save first, to send a span named `gpuworks test span`. It sends only once, without retries, and returns its trace ID in the result. Below the card, the runtime status since the server started shows the exported, dropped, failed and queued counts, along with the last success and last error.

You can apply the same configuration through the API: `GET/PUT /api/v1/telemetry/export` and `POST /api/v1/telemetry/export/test`. See the [API Reference](../reference/api.md).

## 3. Connect the Upstream Gateway and Engine

- **Upstream gateway**: Enable W3C Trace Context propagation (`traceparent` / `tracestate`). The platform gateway's span becomes a child of the upstream span, and you can correlate both sides by trace ID. The platform's request ID (the `X-Request-ID` response header) is assigned by the server; a client-supplied `X-Request-ID` does not replace it. See the rules in [OpenAI-Compatible Gateway](../reference/openai-gateway.md#trace-context).
- **Engine**: Requests forwarded by the gateway to the engine include `traceparent`. To view spans inside the engine, add `--otlp-traces-endpoint <collector>` to the deployment's extra arguments for vLLM. The address and protocol must meet the argument requirements of that image version. The engine container must be able to reach the collector, and its image must contain the OpenTelemetry packages. Gateway spans can be exported without this argument; they include queue, prefill, and decode time fields only when the engine actually reports those times. For PD parent-child relationships, see the appendix below.

## 4. Navigate Between the Console and the Tracing System

- **From the console to the tracing system**: Click a row in the request view (or focus it and press Enter) to open its details. You can copy the trace ID from the details. If a Trace link is configured, click **Open in the tracing backend**.
- **From the tracing system to the console**: Paste the trace ID into the search box at the upper right of Monitor Center to list all requests for that trace on the platform. One agent task may make many model calls. Open a request to view its details and deployment. You can also search by request ID. Trace IDs generated by the gateway contain a timestamp and can be located directly. Other trace IDs are searched within the last hour by default, and you can extend the window to 24 hours.

## 5. Troubleshooting

First check the runtime status below the settings card.

| Symptom | Cause and Action |
|---|---|
| Test span fails: connection refused or timeout | The server cannot reach the collector. Check the address, port (OTLP/HTTP uses 4318, not gRPC's 4317), and firewall. The address must be reachable from the server's machine. |
| Test span fails: 401 / 403 | The backend requires authentication. Add a token in Headers. |
| Test span fails: 404 | The URL path is incorrect. Enter only the address up to the port (`http://host:4318`), or include the full `/v1/traces` path. |
| Status is “Settings unusable” | The saved settings cannot be decrypted, for example after changing the data directory's `master.key`. Enter the headers again and save. |
| The failed count keeps increasing | The collector is rejecting requests or is unreachable. Check Last error for the cause. Responses 429 / 502 / 503 / 504 and connection failures are retried 3 times according to `Retry-After` or backoff before being counted as failures. |
| The dropped count keeps increasing | Export cannot keep up: the collector is slow or the network round-trip time is long, and the queue of 8192 spans is full. Place a collector near the server or reduce the sample ratio. |
| A request is missing from the tracing system | Sampling did not select it (new gateway traces use the ratio; upstream traces follow the upstream decision), or the request was rejected before routing (401, model not found, or no available replicas). These requests do not generate spans. |
| The span has no queue / prefill / decode time | The engine did not report it. vLLM deployments need **Return per-request timings** (`--enable-per-request-metrics`, enabled by default for new deployments using images version 0.25.0 or later). |
| Reasoning tokens and the reasoning phase are missing | The deployment has no reasoning parser configured, so reasoning content is mixed into the answer. |

## 6. Limitations

- Export does not block request forwarding. When the send queue is full, spans are dropped and counted; inference requests are unaffected.
- Only requests that have ended are exported. In-progress requests and requests rejected by the gateway before routing have no spans.
- Only traces are exported. Aggregated metrics for GPUs, engines, and other resources remain in platform monitoring.
- Console searches have a time window of up to 24 hours, and records are retained for 7 days. Under heavy request traffic, normal requests are saved proportionally. Unsaved requests cannot be found in the console.
  Trace export uses independent sampling. Only requests that are selected and successfully exported can be found in the tracing system; this does not guarantee complete retention of all requests.

## Appendix: Span Fields

The span kind is SERVER. Its name is `chat <model-name>`, `text_completion <model-name>`, or `embeddings <model-name>`; for other endpoints, it is the path plus the model name. It starts when the gateway receives the request and ends when the response ends. All content comes from the same record used by the request view ([ADR 0042](../adr/0042-gateway-request-records.md)), so it matches what you see in the console.

For timestamp and phase definitions and the phase breakdown algorithm, see [Request Timeline and Phase Breakdown](../reference/request-timeline.md). Attribute names follow three rules: use the OTel GenAI / HTTP semantic convention name where one exists; if it does not, but vLLM's own span (`llm_request`) has an equivalent, use vLLM's name and unit (seconds), so queries written for vLLM work directly; use `gpuworks.*` only when neither has an equivalent. Unobserved values are omitted.

**OTel semantic conventions**

| Attribute | Meaning | On vLLM Spans |
|---|---|---|
| `gen_ai.operation.name`, `gen_ai.request.model` | Operation (`chat` / `text_completion` / `embeddings`) and the model name requested by the client | None |
| `gen_ai.request.max_tokens`, `gen_ai.request.temperature`, `gen_ai.request.top_p` | Sampling parameters set by the client in the request (`max_tokens` uses `max_completion_tokens`, falling back to `max_tokens` if absent). Unset parameters are omitted; the engine uses its own defaults. | Same names |
| `gen_ai.request.choice.count` | The client-specified `n` | `gen_ai.request.n` (vLLM custom name) |
| `gen_ai.request.stream` | Whether the response is streamed | None |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | Engine-reported input tokens (including cache hits) and output tokens | `gen_ai.usage.prompt_tokens`, `completion_tokens` (older convention names) |
| `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.reasoning.output_tokens` | Cache-hit input tokens and reasoning tokens, when reported by the engine | None |
| `gen_ai.response.finish_reasons` | Finish reasons | None |
| `gen_ai.response.time_to_first_chunk` | For streaming responses, seconds from when the gateway receives the request to the first token, including reasoning | `gen_ai.latency.time_to_first_token`, but measured from arrival at the engine, excluding the gateway and network |
| `url.path`, `http.response.status_code`, `error.type` | Path and status code; on error, the error type (status code, or `upstream_error` / `truncated`) | None |

**Names inherited from vLLM (seconds)**

| Attribute | Meaning |
|---|---|
| `gen_ai.latency.e2e` | End-to-end time, equal to this span's duration, measured from when the gateway receives the request |
| `gen_ai.latency.time_in_queue` | Engine-reported queue time |
| `gen_ai.latency.time_in_model_prefill` | Engine-reported prefill time |
| `gen_ai.latency.time_in_model_decode` | Engine-reported decode time |
| `gen_ai.latency.time_in_model_inference` | Prefill + decode; present only when both are reported |

Queue, prefill and decode times are the original values reported by the engine in its response. vLLM deployments require `--enable-per-request-metrics`; TensorFold and Strata report prefill and decode. These names are used regardless of the engine.

**Custom gpuworks attributes**

| Attribute | Meaning |
|---|---|
| `gpuworks.request.id`, `gpuworks.request.result` | Request ID (the `X-Request-ID` response header); result: `completed`, `length`, `client_closed`, `interrupted`, `error` |
| `gpuworks.deployment.id`, `gpuworks.instance.uuid` | Deployment and the replica that handles the request |
| `gpuworks.api_key.id`, `gpuworks.user.id`, `gpuworks.request.source` | Caller (API key or console user) and entry point (`api` / `playground`) |
| `gpuworks.request.body_bytes`, `gpuworks.request.images` | Request body size and image count |
| `gpuworks.stream.events`, `gpuworks.stream.max_gap_ms` | Number of streamed chunks and the longest pause |
| `gpuworks.pd.prefill_instance`, `gpuworks.pd.fallback` | For PD disaggregation deployments: the instance that handles prefill; when prefill is unavailable and decode handles the request alone, the reason (such as `prefill_5xx`) |
| `gpuworks.phase.<phase>_ms` | Gateway-side portions of the request view's phase breakdown, in milliseconds: `gateway` (the gateway itself), `pd_prefill` (the PD prefill phase), `preprocess` (engine preprocessing), `waiting` (waiting for the first token when the engine does not report queue time), `thinking`, `answering`, and `tail`. Together with the engine's queue and prefill times, these equal the end-to-end time. When engine-reported times exceed the wait observed by the gateway because their timing starts differ, the request view scales them proportionally into the wait; engine values exported here remain the originals. |

### Prefill Child Spans for PD Disaggregation Deployments

**Sequential handoff (`vllm-nixl`, `vllm-ascend-mooncake`, `vllm-mooncake`)**: The request has an additional CLIENT child span, `pd.prefill`, from when the gateway sends the request to the prefill instance until it receives the response.

- The child span's attributes include `gpuworks.pd.prefill_instance`, `gpuworks.pd.fallback`, and the prefill instance's reported `gen_ai.latency.time_in_queue` and `gen_ai.latency.time_in_model_prefill`.
- The `traceparent` sent to the prefill instance uses this child span as its parent, while the one sent to the decode instance uses the gateway span. Spans exported by the two engines attach to these two respective locations.
- The parent span's `gen_ai.latency.*` values are reported by the decode instance: queue time includes waiting for remote KV, and prefill only recomputes the last token. See [Request Timeline](../reference/request-timeline.md#6-pd-分离部署).

**Concurrent handoff (`sglang-nixl`, `sglang-mooncake`)**: The gateway does not wait for prefill and there is no child span; the prefill replica's request uses the gateway span as its parent.

### Timeline Events and Resource Attributes

The timeline consists of span events: `gpuworks.dispatch` (handoff to a replica), `gpuworks.first_byte`, `gpuworks.first_reasoning`, `gpuworks.first_answer`, and `gpuworks.last_chunk`.

Resource attributes are `service.name=gpuworks`, `service.version`, `service.instance.id` (control plane ID), and `gpuworks.control_plane.name`.

### Determine Span Status

Only spans with platform-side failures (`error`, `interrupted`) have Error status. Client-initiated disconnections and length truncation do not count as errors.

Prompts and replies are **not exported**, and the platform itself does not store them.

<!-- translation-source-sha256: 9e89ef83343678079720b4f353fa9f7cc83990d55189ccbf018ef9331542b96f # Export Request Traces to OpenTelemetry -->
<!-- translation-source-sha256: 31d2c80f1e002158bf3f9ab59578aa9d8a6a5b8bd88961993f90e3ad1a74afb0 ## 1. View Your First Trace in Jaeger -->
<!-- translation-source-sha256: 5462a618c3eeb6056252edfc6d41423b0678b563e4f4da54ddb62ebf400ae739 ## 2. Connect a Production Environment -->
<!-- translation-source-sha256: 7df8a36facebb3c8842f2b88d880a283ec1cc1a55b7921f7d2eeff3994eb48dd ## 3. Connect the Upstream Gateway and Engine -->
<!-- translation-source-sha256: 7cbcca3364c3607b69649b3fdad8bab9619c33f5a0b6542dd7259e906fc8e309 ## 4. Navigate Between the Console and the Tracing System -->
<!-- translation-source-sha256: f33863d02d75aac99c75cf34f05b937979919022abe256bdb6170584a4a48cb7 ## 5. Troubleshooting -->
<!-- translation-source-sha256: 6f028599cd3ace8a31e11ed2b08dde5f122cee4f9e2db0729c594daf2f126bfc ## 6. Limitations -->
<!-- translation-source-sha256: cbc9e1da0f4463621b37fbe8a568cacd320ed069b905703ed87d36e122a108d2 ## Appendix: Span Fields -->
<!-- translation-source-sha256: 29bf16f3f83f4957efa798644fee1bfb8660020b5beed881367c47b34e9139c0 ### Prefill Child Spans for PD Disaggregation Deployments -->
<!-- translation-source-sha256: e83baed12d2ff5895247d0bfecde9533c11412329900a26d6e7aa07213941520 ### Timeline Events and Resource Attributes -->
<!-- translation-source-sha256: c9b9b2e8aaff3788ef5c20ec6ed915df8f1921b303dbb840854dc4f465aa3baa ### Determine Span Status -->
<!-- translation-localized-code-block: c87cce8916d515a6b24467acbe5590291890c9618f42b2743c08fb0fe1d463cd 67d464e4acec589ac40309dd150cf2f2695cc935b7474316bc7bd9857ffdf160 Translate inference-token and served-name placeholders and generic sample user message to English; this is an uncaptured example, not tested transcript or screenshot evidence. -->
