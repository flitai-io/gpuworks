# OpenAI-Compatible Gateway

The server's `/v1/*` endpoints require API key authentication by default; see [Authentication](#authentication) below. The `model` field in the request body specifies the model name exposed for inference (the served model). The server uses it to find the deployment, instance, and node, then forwards the request to the local port through the agent tunnel. Management APIs use JWTs; a management JWT cannot replace a gateway key.

## Authentication

`/v1` requires an API key by default: a request without `Authorization: Bearer <key>`, or with a key that does not exist or was revoked, returns 401.

The only exception is an explicit administrator switch: the `inference_open_access` security setting (default `false`), changed in the **Security** card under **Settings → Security** or with `PATCH /api/v1/security/settings`; see [Complete REST API Routes](api.md#settings--network-presets).

- Off (default): every request needs a valid key. That includes a server with no keys at all: a fresh installation no longer creates a `default` token, so `/v1` refuses every request until an operator creates the first key.
- On: a request without a key is accepted anonymously and can reach every deployed model, while a request that does present a key is still verified and an invalid key still returns 401. Anyone who can reach this listener can then call the deployed models and consume their GPUs.
- An unreadable setting counts as off: a failed read never lets a request without a key through; the request is treated as one that must carry a key.
- Revoking a key does not change the decision: revoking the last key does not reopen the gateway, and `/v1` refuses every request until another key is created or an administrator turns the switch on.

The rule covers every `/v1` route, including `/v1/models`, `/v1/files`, `/v1/batches`, `/v1/tokenize`, and `/v1/detokenize`. The 401 code depends on the entry point: gateway routes return `INVALID_API_KEY` (`type` `authentication_error`; see [Errors](#errors)), while Files and Batches return the OpenAI-format `invalid_api_key`.

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
| System One | /v1/systemone | JSON; a `state` and a set of typed questions (`noul` yes/no, `choice`, `score`; the model sets the per-request limits on questions and options, such as 16 questions for Intern-Decision and 64 for Laya), answered with a probability distribution per question and no generated text. The wire format is TypeSafe's System One API; usage records only `usage.input_tokens` |
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
- When the server stops gracefully, a request waiting for a sleeping deployment to wake returns 503 `SERVER_RESTARTING` with `Retry-After: 10`; send it again.

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

## Request Priority

The gateway decides a request's priority: from the priority class its API key is bound to, and the class the client names with the request header `X-GPUWorks-Priority-Class: <name>` within what the key allows; batch requests run at the class their batch was created with, and a batch created through `/v1/batches` always at the `batch` class. A name that matches no class returns 400. The response header `X-GPUWorks-Priority-Class` names the class the request ran at.

For a deployment with request priority on, the gateway writes the class value into the body's `priority` in the engine's direction; otherwise it sets an existing `priority` to `0`. A client's own `priority` field and `X-Vllm-Priority` header never reach the engine, whether the model is named in the body, in `?model=` or in `X-GPUWorks-Model`, and whatever Content-Type the body declares; a body declared as JSON or text must be UTF-8, or the request gets a 400. See [Request Priority](../explanation/request-priority.md).

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

## Errors

Errors the gateway gives itself (refusals before forwarding, failed connections to the engine) use the OpenAI shape, with a stable UPPER_SNAKE `code`:

```json
{"error": {"message": "no deployment serves model qwen", "type": "not_found_error", "code": "MODEL_NOT_FOUND", "param": "model", "params": {"model": "qwen"}}}
```

| Field | Purpose |
|---|---|
| `message` | An English description; do not build handling branches by matching it |
| `type` | The OpenAI error type: `invalid_request_error`, `authentication_error`, `permission_error`, `not_found_error`, `server_error` |
| `code` | The stable error code that tells causes apart |
| `param` | The request field the error is about, `null` for none |
| `params` | The values filled into `message`, for clients that localize it; omitted when there are none |

Every `/v1` response, including a request refused before forwarding, carries `X-Request-ID`. The engine's own error responses are forwarded unchanged when that is safe, in the engine's shape; Files and Batches answer in OpenAI's format with its lower-case codes, see [REST API Reference](api.md#batches).

| `code` | Status | `type` | `param` | Meaning |
|---|---|---|---|---|
| `INVALID_API_KEY` | 401 | `authentication_error` | — | The API key is missing or invalid |
| `MODEL_ACCESS_DENIED` | 403 | `permission_error` | `model` | This API key may not call the model (`params.model`) |
| `MODEL_REQUIRED` | 400 | `invalid_request_error` | `model` | The request names no `model` |
| `REQUEST_BODY_INVALID` | 400 | `invalid_request_error` | — | The request body cannot be read (multipart without a boundary, not a JSON object); `params.detail` says why |
| `REQUEST_TOO_LARGE` | 413 | `invalid_request_error` | — | The request body exceeds the limit (`params.max_bytes`) |
| `METHOD_NOT_ALLOWED` | 405 | `invalid_request_error` | — | The route only accepts `params.method` (such as `GET /v1/tokenize`); the response carries `Allow` |
| `CHAT_INPUT_UNREADABLE` | 400 | `invalid_request_error` | `messages` | The chat input's media types cannot be checked |
| `MODEL_INPUT_MODALITY_UNSUPPORTED` | 400 | `invalid_request_error` | `messages` | The model does not take this input (`params.modality`, `params.supported`) |
| `MODEL_NOT_FOUND` | 404 | `not_found_error` | `model` | No deployment serves the model name |
| `MODEL_ROUTE_NOT_EXPOSED` | 404 | `not_found_error` | — | The deployment's contract does not offer the route (Playground) |
| `TOKENIZE_DISABLED` | 404 | `not_found_error` | — | The deployment turned tokenize forwarding off |
| `ROUTE_NOT_FOUND` | 404 | `not_found_error` | — | This listening address does not serve `/v1` (only the console or agent function) |
| `INSTANCE_NOT_FOUND` | 404 | `not_found_error` | — | The pinned instance does not exist |
| `INSTANCE_MODEL_MISMATCH` | 404 | `not_found_error` | `model` | The pinned instance does not serve the model |
| `MODEL_ASLEEP` | 503 | `server_error` | — | The deployment sleeps; wake deployment `params.deployment_id`, or wait for its schedule or a waiting wake; may carry `Retry-After` |
| `INSTANCE_ASLEEP` | 503 | `server_error` | — | The pinned instance's deployment sleeps |
| `INSTANCE_NOT_RUNNING` | 503 | `server_error` | — | The pinned instance is not running |
| `INSTANCE_OFFLINE` | 503 | `server_error` | — | The pinned instance's node is offline |
| `MODEL_NOT_READY` | 503 | `server_error` | — | No running instance is reachable; check deployment status and node connectivity. This alone does not mean the engine is starting |
| `MODEL_NODE_OFFLINE` | 503 | `server_error` | — | An instance is running and awake, but its node's agent is not connected now; check the node connection. The engine itself needs no restart |
| `DEPLOYMENT_START_FAILED` | 503 | `server_error` | — | The deployment stopped retrying its start; see the deployment list |
| `DISTRIBUTED_NOT_READY` | 503 | `server_error` | — | No replica of the multi-node deployment has every rank running on an online node |
| `DISTRIBUTED_LEADER_OFFLINE` | 503 | `server_error` | — | The node of the multi-node deployment's rank 0 is offline |
| `PD_PLACEMENT_UNREADABLE` | 503 | `server_error` | — | The PD deployment's placement cannot be read |
| `PD_PREFILL_UNAVAILABLE` | 503 | `server_error` | — | No prefill instance can take the request (`params.reason`); carries `Retry-After: 2` |
| `WAKE_REQUEST_ENDED` | 503 | `server_error` | — | The client left while the model woke |
| `SERVER_RESTARTING` | 503 | `server_error` | — | The server is stopping; carries `Retry-After: 10`; send again |
| `UPSTREAM_UNAVAILABLE` | 502 | `server_error` | — | The tunnel or connection to the engine failed (`params.detail`) |
| `DATA_LANE_UNAVAILABLE` | 503 | `server_error` | — | The node's data lane disconnected and did not reconnect within 5 seconds; carries `Retry-After: 2`; retryable |
| `INSTANCE_UNAVAILABLE` | 503 | `server_error` | — | The instance stopped or restarted under the request; carries `Retry-After: 2`; retryable |
| `UPSTREAM_REJECTED` | The engine's | By status | — | When the engine's error body is too large or may echo input, the gateway rewrites it under this code; `params.detail` is the bounded engine text |
| `EXTERNAL_*` | 400/404/500/502 | By status | — | Playground external model errors: `EXTERNAL_ROUTE_UNSUPPORTED`, `EXTERNAL_MODEL_INVALID`, `EXTERNAL_MODEL_NOT_FOUND`, `EXTERNAL_ENDPOINT_INVALID`, `EXTERNAL_KEY_UNREADABLE`, `EXTERNAL_REQUEST_INVALID`, `EXTERNAL_UPSTREAM_UNAVAILABLE` |
| `INTERNAL_ERROR` | 500 | `server_error` | — | An internal gateway error |

## Playground

The model list shows only deployed models that declare inference API operations. External models in global settings are used by the AI assistant and are not automatically added to Playground.

You can call deployed models through `/api/v1/playground/v1/*`, which is protected by a management JWT.

External model names use the form `external:<config-id>:<model>` and allow only chat completions. External endpoint keys are encrypted on the server and decrypted there for forwarding; they are not written to the browser or instance environment.

## Differences and Troubleshooting

The gateway does not emulate all OpenAI product APIs. For the exact capabilities, refer to the spec contract and `GET /api/v1/playground/openapi.json`.

| Status code | What to check |
|---|---|
| 401 `INVALID_API_KEY` | Check `Authorization: Bearer <key>` first. When no key is sent at all, confirm whether anonymous access is really wanted (see [Authentication](#authentication)); `/v1` refuses every request without a key by default |
| 404 `MODEL_NOT_FOUND` / `MODEL_ROUTE_NOT_EXPOSED` | Check the `model` name; for Playground, also check the contract |
| 503 | Look up the `code` in [Errors](#errors): asleep, starting, node offline, or data lane reconnecting |
| 502 `UPSTREAM_UNAVAILABLE` | Check tunnel and engine logs |

Streaming SSE and binary responses (such as WAV) are forwarded with their original content type without truncation.

<!-- translation-source-sha256: 146ba1db59f6915d93d5c0497a63492d4efae6d2c61d64e6ba4f34e24120be14 # OpenAI-Compatible Gateway -->
<!-- translation-source-sha256: 3419573a4fd81544ccdec8647a87d11ee1ec1ccf5113453070fb944603190c79 ## Authentication -->
<!-- translation-source-sha256: 389d4b5522a5bd3216a5041c7dac412010d158cc57bd4bb5ad5788145d664111 ## Supported Operations -->
<!-- translation-source-sha256: 7bf01655b1c8196029d9b3ac65802f9f07b54314ee15dfcd83cb02319629c518 ## Replica Selection and Retries -->
<!-- translation-source-sha256: 6acb770e47b044aaa2f5ae020be8de5399e18f1c1a2ca8766b6335625d6cff3b ## Batches (Files and Batches) -->
<!-- translation-source-sha256: 475124ec8de50b9072b50ee962f92870520b5df325b80187af159d220745f2a7 ## Tokenization (tokenize) -->
<!-- translation-source-sha256: b04cb3cd49891b8cf9a25bb166f063ce5af28e7721601eb94fa2fff50353569d ## Request Priority -->
<!-- translation-source-sha256: 91d6198ea9bcfb40d784f10fa77320acb27b0b45bcb8fdb958cf4d46554ea647 ## Trace Context -->
<!-- translation-source-sha256: 2b9c4f0c8955d7d09cb9fc312559725988923ab947e97e8e4f21fbe931ef9cdf ### Trace Identifiers and Request IDs -->
<!-- translation-source-sha256: 19589bf0b20be8387b262c114cb369861640ef81042c8ac4039bd2b51d39dafe ### Connect to the Caller's Trace -->
<!-- translation-source-sha256: 588ff427c9765c06b01b308bea79a08e63ea16d1883e9063327bbbd969207d2e ## Usage -->
<!-- translation-source-sha256: 9c37d4f05b6f03c9e6aff79e566c378a64251aaa8a9fdb9a0f2af24fb3a2745d ### Obtain Usage for Streaming Requests -->
<!-- translation-source-sha256: b1aad95e9d7e77247af93d91aabebaafbbdcd7f28bfb207c6b8fec973a997271 ### Usage for Messages and Responses -->
<!-- translation-source-sha256: 1e99cb203865e1dc47e85e652d668709bf98e039c55d60b02f03d99d54a77696 ### Source of Cache Hit Counts in PD Disaggregation Deployments -->
<!-- translation-source-sha256: e7f31181b01ee976e4b99e6b3d226da30de2d87a01421059370b45457fa8a71e ## Errors -->
<!-- translation-source-sha256: 8ae89817b16c25868b3a33ebd10d60695c312cb82bf517b25aef2b73b85182c8 ## Playground -->
<!-- translation-source-sha256: 9923f454eda93d5f4500f52ff744daba8ddd4870a4f1564e373d004af15c11b7 ## Differences and Troubleshooting -->
