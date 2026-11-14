# OpenAI-Compatible Gateway

The server's `/v1/*` endpoints require API key authentication by default; see [Authentication](#authentication) below. The `model` field in the request body specifies the model name exposed for inference (the served model). The server uses it to find the deployment, instance, and node, then forwards the request to the local port through the agent tunnel. Management APIs use JWTs; a management JWT cannot replace a gateway key.

## Authentication

`/v1` requires an API key by default: a request without `Authorization: Bearer <key>`, or with a key that does not exist or was revoked, returns 401.

The only exception is an explicit administrator switch: the `inference_open_access` security setting (default `false`), changed in the **Security** card under **Settings → Security** or with `PATCH /api/v1/security/settings`; see [Complete REST API Routes](api.md#settings--network-presets).

- Off (default): every request needs a valid key. That includes a server with no keys at all: a fresh installation no longer creates a `default` token, so `/v1` refuses every request until an operator creates the first key.
- On: a request without a key is accepted anonymously and can reach every deployed model, while a request that does present a key is still verified and an invalid key still returns 401. Anyone who can reach this listener can then call the deployed models and consume their GPUs.
- An unreadable setting counts as off: a failed read never lets a request without a key through; the request is treated as one that must carry a key.
- Revoking a key does not change the decision: revoking the last key does not reopen the gateway, and `/v1` refuses every request until another key is created or an administrator turns the switch on.

The rule covers every `/v1` route, including `/v1/models`, `/v1/files`, `/v1/batches`, `/v1/tokenize`, and `/v1/detokenize`. The 401 body depends on the entry point: gateway routes return `{"error":{"message":"invalid or missing API key","type":"gpuworks_proxy"}}`, while Files and Batches return the OpenAI-format `invalid_api_key`; see [Differences and Troubleshooting](#differences-and-troubleshooting).

`GET /v1/models` lists the served names the gateway can route right now (read from the gateway's event-driven route table, not the database): active deployments only, each served name once, in name order; a scoped key sees only the served names of deployments it may call. A deployment just created or stopped appears or disappears once the route table rebuilds: at most about 0.5 s after the deployment's event, and every 2 s regardless.

## Supported Operations

| Operation | Default endpoint | Encoding/mode |
|---|---|---|
| chat | /v1/chat/completions | JSON |
| text completion | /v1/completions | JSON; supports streaming. The prompt is passed unchanged to the engine without a chat template, for base models and task models driven by fixed formats |
| embeddings | /v1/embeddings | JSON |
| rerank | /v1/rerank (also supports /rerank) | JSON |
| audio transcription | /v1/audio/transcriptions | multipart |
| audio speech | /v1/audio/speech | JSON |
| image generation | /v1/images/generations | JSON or multipart |
| image editing | /v1/images/edits | multipart; the source image must be uploaded as the `image` file field |
| video generation | /v1/videos | JSON or multipart; supports async |
| realtime | /v1/realtime | WebSocket |
| batch (Files and Batches) | /v1/files, /v1/batches | JSON; file upload is multipart. Asynchronous batches, see [Batches](#批处理files-与-batches) |

Each spec's API contract declares the operations and endpoints available for that deployment. The two entry points apply different checks:

- The console Playground (`/api/v1/playground/v1/*`) enforces the contract and returns 404 for paths outside it.
- The public `/v1` entry point does not check the contract. It selects a replica by `model` and forwards the request. The engine itself responds to unsupported paths, usually with 404.

Asynchronous video tasks also provide `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`, and `DELETE /v1/videos/{id}`.

## Replica Selection and Retries

Deployments with multiple replicas use least-request selection by default, preferring replicas with fewer requests. The gateway selects a replica from its in-memory candidate table based on the number of requests this gateway is currently forwarding to each replica (in-flight requests). It also considers recent valid engine queue counts and KV utilization, applying a soft penalty to lower the selection priority of replicas with longer queues or KV utilization at or above the threshold.

- After a connection-level failure, the gateway retries on another replica only if the request can still be replayed.
- Once an upstream response is received, including a 5xx response, the gateway does not retry.
- If an instance cannot serve because it is sleeping, starting, or has a nonzero rank, the gateway returns 503 and preserves retry semantics.
- When the server stops gracefully, a request waiting for a sleeping deployment to wake returns 503 with `Retry-After: 10` and the message `the server is restarting; send the request again`; send it again.

A deployment can select `least_request`, `prefix_affinity`, or `failover` through the management API `PUT /api/v1/deployments/{id}/routing`. Prefix affinity first reads `X-GPUWorks-Affinity-Key` and also supports extracting a stable key from chat messages, prompt, or input. This header is not forwarded to the engine.

## Batches (Files and Batches)

The gateway supports OpenAI's Batch API: upload a JSONL file with `POST /v1/files` (`purpose=batch`), create a batch with `POST /v1/batches`, follow its progress with `GET /v1/batches/{id}`, and fetch the output and error files from `GET /v1/files/{id}/content`. The supported routes are:

- Files: `POST /v1/files`, `GET /v1/files`, `GET /v1/files/{id}`, `GET /v1/files/{id}/content`, `DELETE /v1/files/{id}`.
- Batches: `POST /v1/batches`, `GET /v1/batches`, `GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel`.

The `chat/completions`, `completions`, and `embeddings` requests in a batch take the same gateway path as ordinary requests by `body.model`, are counted in usage and request records, and cannot stream. Like the other `/v1` routes, these two groups authenticate with an API key and return errors in OpenAI's format. For the procedure and examples see [Run Batches](../how-to/batch-api.md); for objects and error codes see [Complete REST API Routes](api.md#batches批处理任务).

## Tokenization (tokenize)

`POST /v1/tokenize` and `POST /v1/detokenize` select the deployment and replica using `model` in the request body. Like other `/v1` requests, they use API key authentication, model name rewriting, 503 responses for sleeping or starting instances, and connection-level retries. However, they are forwarded to the engine's root paths `/tokenize` and `/detokenize` (vLLM provides only the root paths; SGLang provides both root and `/v1` paths). Requests and responses are forwarded unchanged in the engine's own format. For example, with vLLM:

- `/tokenize` accepts `prompt`, or `messages` (with optional `add_generation_prompt`, which defaults to `true`, and `chat_template_kwargs`), plus `add_special_tokens` and `return_token_strs`. It returns `{"count", "max_model_len", "tokens", "token_strs"}`, with `token_strs` set to `null` when not requested.
- `/detokenize` accepts `tokens` (an integer array) and returns `{"prompt": "..."}`.
- Engine validation errors (400) and model-not-found errors (404, `{"error": {"message", "type": "NotFoundError", "param": "model", "code": 404}}`) are returned unchanged.

Tokenization is not inference traffic: it does not count toward gateway request counts, duration metrics, or in-flight counts; create request records; update the deployment's latest request time; or export traces. Only POST is accepted. For other methods, the gateway returns 405 with `Allow: POST`. The root path `/tokenize` belongs to the console and is not handled by the gateway.

You can disable tokenization forwarding for each deployment: set `tokenize_enabled: false` when creating it, or use `PUT /api/v1/deployments/{id}/tokenize` (see [API Reference](api.md)). It is enabled by default. When disabled, the gateway returns 404 directly (`deployment <model> does not expose POST /v1/tokenize: tokenize is turned off for it`) without contacting the engine. The setting takes effect through the gateway routing table without restarting instances. Engines other than vLLM and SGLang may not provide tokenization endpoints; in that case, requests receive the engine's own error.

## Trace Context

Trace context **correlates records for the same call across different systems**. For example, when a request passes through an upstream gateway, GPUWorks, and an inference engine, you can use it to identify which stage takes time or fails. GPUWorks automatically establishes trace context for each forwarded request; ordinary inference calls do not require you to fill in this information manually.

### Trace Identifiers and Request IDs

| Name | Meaning and purpose |
|---|---|
| trace / trace id | A complete trace and its identifier. The upstream gateway, GPUWorks, and engine must use the same trace id to correlate their records in a tracing system |
| span | An operation within a trace. For example, GPUWorks forwarding a request is one span, and the engine processing it is another. Parent-child relationships indicate which operation calls the next |
| Request ID / `X-Request-ID` | The identifier GPUWorks assigns to this request, returned in a response header and saved with the trace id in the request record. Use it to find requests within the platform; use the trace id for correlation across systems |

A client-supplied `X-Request-ID` does not replace the request ID assigned by the server.

### Connect to the Caller's Trace

Systems propagate trace context through W3C request headers: `traceparent` carries the trace id, the caller's span id, and sampling flags; `tracestate` carries additional information for the tracing system. Sampling flags express whether the trace is selected for recording.

| Caller request | GPUWorks behavior |
|---|---|
| Contains a valid `traceparent` | Reuses the trace id, creates a gateway span with the caller's span as its parent, and preserves the caller's sampling decision and `tracestate` |
| Omits `traceparent` or provides an invalid value | Creates a new trace and gateway span |

Therefore, when an upstream gateway such as Higress, Kong, or LiteLLM enables W3C context propagation, GPUWorks processing records can join the same trace.

When forwarding to the engine, GPUWorks updates `traceparent` to identify the span making the call and continues to pass along `tracestate` from valid upstream context (duplicate headers are merged and leading/trailing whitespace is trimmed; values exceeding 512 bytes are discarded):

- Normal forwarding, retries, and decode requests in PD disaggregation deployments all use the gateway span as their parent.
- A PD disaggregation deployment assigns prompt computation (prefill) and subsequent content generation (decode) to different instances. For sequential handoff, the gateway creates an additional `pd.prefill` child span and uses it as the parent of requests sent to the prefill instance. Prefill requests in concurrent handoff still use the gateway span as their parent.
- The engine must enable OpenTelemetry (OTel, for example vLLM's `--otlp-traces-endpoint`) to generate its own spans and join the trace with these parent-child relationships.

A trace id created by the gateway is 16 bytes: the first 6 bytes store the Unix time in milliseconds when the trace is created, and the last 10 bytes are random. It also sets the Trace Context Level 2 random bit, indicating that the rightmost 7 bytes meet the randomness requirements. For the design rationale, see [ADR 0046](../adr/0046-gateway-request-traces.md); for PD child spans, see [ADR 0047](../adr/0047-pd-request-breakdown.md).

To export to an external tracing system, see [Export Request Traces to OpenTelemetry](../how-to/export-request-traces.md). To find requests by trace id or request ID in the console, see [View Metrics and Search Logs](../how-to/monitor-and-logs.md#2-看请求视图与请求分布).

## Usage

`usage` is the usage statistics the engine provides in its response. For text generation requests, it usually records how many tokens the input and output use. A token is the model's unit for counting text, and is not equivalent to a word or character count.

| Common field | Meaning |
|---|---|
| `prompt_tokens` | Number of tokens in the input prompt |
| `completion_tokens` | Number of tokens in the generated content |
| `total_tokens` | Total number of input and generated tokens |
| `prompt_tokens_details.cached_tokens` | Number of input tokens that hit the cache and do not need to be computed again; these are a subset of input tokens and are not added to the total separately |

The fields returned depend on the engine. The gateway usually forwards `usage` unchanged, with additional handling in the following cases.

### Obtain Usage for Streaming Requests

Streaming responses send generated content in multiple parts, and usage statistics may be returned as a separate event at the end of the stream. **The platform needs to record usage, but the client may not have requested this event.**

The gateway automatically requests usage from the engine only when all of the following conditions are met:

- The engine is vLLM or SGLang.
- The endpoint is `/v1/chat/completions` or `/v1/completions`.
- The request body is JSON that the gateway can read and sets `stream: true`.
- The client has not set `stream_options`.

When these conditions are met, the gateway adds `"stream_options":{"include_usage":true}` to the forwarded request. When the engine returns usage, the gateway records the statistics, then removes the separate usage event added as a result from the response stream, so the client does not receive this additional event.

If you want to receive usage in the response stream, add the following field to the request body:

```text
"stream_options": {"include_usage": true}
```

Whenever the client has already set `stream_options` (including `null` or `include_usage: false`), the gateway does not add or override it. The engine handles whether to return usage according to the client's setting.

### Usage for Messages and Responses

Anthropic Messages (`/v1/messages`) and OpenAI Responses (`/v1/responses`) requests are forwarded unchanged. The gateway adds no fields because these protocols always include usage in streaming responses as well. It only reads the responses to collect the platform's own statistics.

Input is consistently recorded as **total input**:

| Protocol and fields returned by the engine | Total input tokens | Cached input tokens |
|---|---|---|
| Responses | `input_tokens` | `input_tokens_details.cached_tokens` |
| Messages with `cache_read_input_tokens` / `cache_creation_input_tokens` (vLLM) | `input_tokens` counts only uncached input; total input is the sum of all three fields | `cache_read_input_tokens` |
| Messages without these two cache fields (SGLang) | `input_tokens` | Unknown |

For streaming Messages responses, input usage comes from `message_start` and output usage comes from the final `message_delta` (SGLang's final `message_delta` contains only output usage). Only Responses reports reasoning tokens (`output_tokens_details.reasoning_tokens`).

Finish reasons are normalized to the chat convention: `max_tokens`, or a status of `incomplete` with a reason of `max_output_tokens`, means truncation at the length limit; `end_turn` and `completed` mean normal completion. vLLM still emits `response.completed` when output is truncated, so the gateway uses the `status` of the response within that event.

### Source of Cache Hit Counts in PD Disaggregation Deployments

PD disaggregation assigns input prompt computation and subsequent generation to different instances. When vLLM sequential handoff succeeds, the prefill instance computes the prompt, then passes the computed result (KV) to the decode instance to continue content generation. Decode counts all received KV as cache hits, so its reported `cached_tokens` equals the full prompt length and cannot indicate how much cache was actually reused during prefill.

**Sequential handoff modes `vllm-nixl`, `vllm-ascend-mooncake`, and `vllm-mooncake` use the cache hit count reported by the prefill instance.** For example, if the input contains 1,036 tokens and prefill actually hits the cache for 512 tokens, the gateway returns `cached_tokens` of 512 even if decode reports 1,036 hits. This means 512 input tokens directly reuse cached prefill results.

| Situation | Returned `cached_tokens` |
|---|---|
| Sequential handoff succeeds and the prefill response provides a cache hit count | Replaces `prompt_tokens_details.cached_tokens` in the decode response with the prefill value |
| The prefill response does not provide this value | Preserves the decode value |
| The request falls back to decode, which independently performs both input computation and content generation | Preserves the decode value |
| SGLang concurrent handoff | Preserves the decode value |

The replacement affects only this cache hit count; other `usage` fields still come from decode. The gateway rewrites the number in place, padding with spaces if it becomes shorter to preserve `Content-Length`. If the new value requires more digits than the original, it is not rewritten. The client, platform request record, gateway counters, and exported trace all use the same number.

## Playground

The model list shows only deployed models that declare inference API operations. External models in global settings are used by the AI assistant and are not automatically added to Playground.

You can call deployed models through `/api/v1/playground/v1/*`, which is protected by a management JWT.

External model names use the form `external:<config-id>:<model>` and allow only chat completions. External endpoint keys are encrypted on the server and decrypted there for forwarding; they are not written to the browser or instance environment.

## Differences and Troubleshooting

The gateway does not emulate all OpenAI product APIs. For the exact capabilities, refer to the spec contract and `GET /api/v1/playground/openapi.json`.

| Status code | What to check |
|---|---|
| 401 | Check `Authorization: Bearer <key>` first. When no key is sent at all, confirm whether anonymous access is really wanted (see [Authentication](#authentication)); `/v1` refuses every request without a key by default |
| 404 | Check the `model` name; for Playground, also check the contract |
| 503 | Check instance status, sleep/wake, and whether the node is online |
| 502 | Check tunnel and engine logs |

Streaming SSE and binary responses (such as WAV) are forwarded with their original content type without truncation.
