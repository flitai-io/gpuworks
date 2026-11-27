# Request Priority

Request priority makes the engine queue requests by priority: higher-priority requests are scheduled first, and when GPU memory (KV cache) runs short, lower-priority requests are preempted first. It relies on the priority scheduling built into vLLM and SGLang; the platform turns on the engine's switch and lets only the gateway decide each request's priority.

## The Switch on a Deployment

Supported LLM entrypoints enable **Request priority** by default. Evaluation checks the effective entrypoint and arguments after template and user overrides; create, edit and final launch assembly use the same rule. Unsupported or unverified entrypoints receive no automatic priority injection. An explicit request to enable it returns a reason on `priority_scheduling`. When it is on, the platform adds to the engine:

| Engine | Arguments the platform injects | The engine's direction |
|---|---|---|
| vLLM | `--scheduling-policy priority` | Lower values run first; ties go by arrival |
| SGLang | `--enable-priority-scheduling --default-priority-value 0` | Higher values run first; requests without a priority (health checks, the engine's own warmup) count as 0 |

When the custom arguments already contain an argument of the same name, the platform does not inject it and the user's value stands. Changing the switch rebuilds replicas; the edit confirmation lists `priority_scheduling` as a change that needs a restart. Deployments created before the upgrade stay off; turn it on in an edit when needed.

It cannot be turned on in the following cases; the deployment form gives the reason next to the switch, and the deployment does not distinguish priorities:

| Reason | Details |
|---|---|
| The engine is not vLLM or SGLang | Other engines have no priority scheduling the platform can drive |
| The engine build for the accelerator is unverified | Turned on for NVIDIA (vLLM, SGLang; measured), AMD (vLLM, SGLang) and Hygon (vLLM); the latter two run the same scheduler code as upstream, confirmed by source and not yet measured on a card. Ascend, PPU, Kunlunxin, Hygon SGLang and others stay off for now |
| Unverified entrypoint or pipeline | No automatic injection for SGLang diffusion, `sglang serve` without an explicit LLM mode, vLLM omni/pipelines, custom or unknown image entrypoints, or a fully handwritten command; choose a supported LLM entrypoint or turn this switch off |
| PD disaggregated deployment | Not supported yet |
| Conflicting scheduling arguments | vLLM's `--scheduling-policy` is not `priority`; SGLang's `--schedule-policy` is not `fcfs` or `lof` (for example `lpm`), or `--schedule-low-priority-values-first` is set |

When the conflict comes from the inference template (some templates use `--schedule-policy lpm` for prefix-cache hits), the switch offers one click: it adds `--schedule-policy fcfs` to the custom arguments, overriding the template's value, and evaluation then judges again. Doing so loses the original policy's optimization. When the conflict comes from the user's own arguments, the user changes them.

## Priority Classes

A priority class is a name and a value, **larger is more important**, from -100 to 100. Classes are managed under Settings → Nodes and Deployments → Request priority, by administrators only. Five classes are built in:

| Name | Value | Use |
|---|---|---|
| `critical` | 40 | Critical workloads |
| `high` | 20 | |
| `normal` | 0 | The initial default |
| `low` | -20 | |
| `batch` | -40 | Used by batch requests by default |

A built-in class can only change its display name and description; its name and value stay and it cannot be deleted. Administrators may create their own classes. Between two classes more than 10 apart, SGLang lets the higher preempt a running lower one; 10 or less only changes queue order.

One class is the **default**: it is used by API keys bound to no class, requests without a key (when an administrator turned on anonymous access) and playground requests that choose no class. The default cannot be deleted, nor can a class bound to an active key or used by a batch or evaluation that has not finished.

## Key Binding and Client Choice

On the API Keys page an administrator can bind a key:

- **Class**: the class the key's requests run at by default; unbound, the default class.
- **What clients may ask for** (the policy); clients name a class with the request header `X-GPUWorks-Priority-Class: <name>`:
  - **Within ceiling** (the default): any class up to the **ceiling**; a higher one is lowered to the ceiling. Without a ceiling it is the key's own class; a ceiling cannot be below the key's own class.
  - **Fixed**: requests always run at the key's own class; the class in the header has no effect.
  - **Down only**: only classes up to the key's own; a higher one runs at the key's own class.

Only within ceiling has a ceiling; saving another policy drops it. The name in the header is not case-sensitive; one that matches no class is refused with 400 `PRIORITY_CLASS_UNKNOWN` under all three policies (fixed included) before a deployment is chosen, so nothing is forwarded and no sleeping deployment is woken. Anonymous access without a key does not read the header and always uses the default class. Retries of a request and its replay after preemption run at the class decided first.

## Playground, Batches and Evaluations

Requests from these three do not take their class from a key; an administrator chooses it on the page (anyone else runs at the default):

- **Playground**: the chat parameters panel has "Request priority", offering any class; with none chosen the request runs at the default. The request carries the `X-GPUWorks-Priority-Class` header through the gateway, and each reply shows the class the gateway ran it at. When the deployment does not run priority scheduling, the panel says the class is only recorded and does not change queue order. External models do not go through the gateway and offer no choice, and the header is not forwarded to the outside provider. A class saved with a conversation and deleted since falls back to the default on the next send.
- **Batches**: creating a batch in the console may choose any class, the `batch` class otherwise; the batch detail shows it. Every request of the batch runs at that class, whatever key created it. A batch created through `/v1/batches` always runs at the `batch` class.
- **Evaluations**: only a run of a managed deployment measured from the server goes through the gateway, and only then may it choose a class; with none chosen it sends no header and runs at the default. A run measured from a node, of a single instance or of an external endpoint calls the engine directly and cannot choose one; naming one is refused on create (`BENCHMARK_PRIORITY_CLASS_GATEWAY`). The run records the class chosen, listed as "Requested priority class" in the report; the class the gateway actually ran each request at is in the per-request detail's `priority_class` (only the gateway's answer is kept); the target snapshot records whether the measured instance was launched with priority scheduling ("Priority scheduling" in the report), without which the class does not change queue order.

## How the Gateway Writes the Priority

The gateway first decides the request's class, names it in the response header `X-GPUWorks-Priority-Class`, and then:

- when the target instance was actually launched with priority scheduling (judged from its launch command; an instance of a deployment that asks for it but landed on an accelerator that does not allow it does not count) and the request is not an auxiliary route such as `/v1/tokenize` or `/v1/detokenize`, writes the class value into the body's `priority` in the engine's direction: negated for vLLM (lower values run first), as is for SGLang;
- otherwise (auxiliary endpoints such as tokenize included) sets a `priority` the body already has to `0`, and leaves a body without one untouched.

The response header `X-GPUWorks-Priority-Scheduling` says which: `on` for the first, `off` for the second.

A request whose `Content-Type` has a malformed parameter (such as `application/json; x`) is still handled as JSON, so a client cannot slip past this way.

Either way, a client's own `priority` field and `X-Vllm-Priority` header (vLLM uses it to override the body) never reach the engine. Both stages of a PD request are handled the same way.

A class change applies to the next request on the server that made it; with several servers, the others follow within 5 seconds. A key binding change applies from that key's next request.

## Observation

- A gateway in front of GPUWorks can read a key's priority binding with `GET /v1/key`, together with how many running replicas of each model the key may call schedule by priority and whether the model can reject when busy; see [OpenAI-Compatible Gateway](../reference/openai-gateway.md#key-information) and [Connecting an Upstream Gateway](#connecting-an-upstream-gateway).
- Every request record carries `priority_class` (the name of the class the request ran at), shown in the request detail; the request records API can filter by it (the `priority_class` parameter).
- The Service quality page of Operations analytics has a "By request priority" table giving each class's requests, success rate and first-token and end-to-end quantiles. A low class can queue for long while a high one keeps the engine saturated, which the totals hide; this table shows it. Requests from before priority classes count as "Unrecorded", their class unknown.

## Connecting an Upstream Gateway

Another gateway can sit in front of GPUWorks, with other upstreams (a public cloud, for example) behind the same model name. Such a gateway uses `X-GPUWorks-Priority-Class` to have important requests handled first in GPUWorks, and [busy rejection](../reference/openai-gateway.md#busy-rejection) to divert at once when there is no local room. The design trade-offs are in [ADR 0083](../adr/0083-busy-rejection-for-upstream-gateways.md). On the GPUWorks side there are three steps:

1. **Give the API key the gateway uses the policy "Within ceiling"**, with the ceiling set to the highest class the gateway may use (for example `critical`), and the key's own class no higher than `high`. A gateway usually sends the header only on the requests it raises; its ordinary requests carry none and run at the key's own class. When the key's own class is above `high` (for example `critical`), the gateway's requests marked `high` rank below its ordinary unmarked ones. Without a ceiling the ceiling is the key's own class, and a higher class in the header is silently lowered to it.
2. **Turn on "Request priority" for the deployments that should schedule by priority**; see [The Switch on a Deployment](#the-switch-on-a-deployment). Deployments created before an upgrade have it off.
3. **For deployments that should take part in busy rejection, state the concurrent-sequence limit in the launch arguments**: "Maximum Concurrent Sequences" in the deploy form (parameter `max_num_seqs`, that is vLLM `--max-num-seqs` and SGLang `--max-running-requests`), or the same parameter stated in the inference template or custom arguments. Left empty, the engine derives it from GPU memory, GPUWorks does not know the value, and the deployment never rejects; only some built-in inference templates state it. PD-disaggregated and multi-node deployments do not take part in busy rejection.

Afterwards, call `GET /v1/key` with the gateway's key to check (see [Key Information](../reference/openai-gateway.md#key-information)): `priority.own` and `priority.ceiling` are the classes set in step 1; `models[].priority_scheduling` is `all` when step 2 is in effect for every running replica (changing the switch rebuilds replicas, and until then it is `some` or `none`); `models[].busy_reject` is `true` when step 3 is in effect. The response headers `X-GPUWorks-Priority-Class` and `X-GPUWorks-Priority-Scheduling` of each answer give the class actually used and whether the engine scheduled by it.

## Effects and Limits

- Priority only matters while the engine has a queue. When the engine does not queue, turning it on makes no difference.
- In vLLM, a new high-priority request does not displace running requests; it waits for a slot under `--max-num-seqs`. Only when KV cache runs short is the lowest-priority, latest-arriving request preempted. A preempted streaming request pauses for a few seconds and then continues with complete output.
- In SGLang, a new request whose priority exceeds a running request's by more than 10 (the default `--priority-scheduling-preemption-threshold`, strictly greater) preempts it at once; the preempted request resumes from the prefix cache.
- Both engines use strict priority with no aging by waiting time: under sustained saturation, low-priority requests can wait without bound.
- Priority only takes effect inside one replica; the gateway does not consider it when choosing among replicas. Requests with the busy declaration are the exception: when judging whether a replica that schedules by priority has room the gateway counts only the in-flight requests at this request's priority or above (any other replica runs requests in arrival order, so all of them count), but it still chooses among the replicas with room by the deployment's routing policy.
- Measured on a single NVIDIA card, turning it on changed throughput, time to first token and prefix-cache hit rate for same-priority load by no more than measurement noise.

<!-- translation-source-sha256: d7e74375c5cd59bb288240923a8a8f631dac536b5e4f4511c9f8d60b5ba72683 # Request Priority -->
<!-- translation-source-sha256: 0cbe3098670221077975fb0d59dc1994dd3810d41b38fc6bd0e64f49c655b47b ## The Switch on a Deployment -->
<!-- translation-source-sha256: e68bb5553e31da8b61d16b83d68a2774087b167273eaee5b406fa2db9bda789c ## Priority Classes -->
<!-- translation-source-sha256: 79a42ca4732c0d2c8691e80b541bd1984cd98117902a03aba80f9efe56474e29 ## Key Binding and Client Choice -->
<!-- translation-source-sha256: c3e6ec30b3d17ad88cb0201462ebf5a15443cfeae174275ada4509afa8650ba4 ## Playground, Batches and Evaluations -->
<!-- translation-source-sha256: ad69b3aed66393ea5f9b0dd604ead45756bffa34984279967de5b7bbf42a6151 ## How the Gateway Writes the Priority -->
<!-- translation-source-sha256: 01165cd92505eddcfc41e12f3f1b2759daed743432a39e7cb6e4dbd6fe26919c ## Observation -->
<!-- translation-source-sha256: 6be6137f5187a29d1a1852aba1bce2c31de0f3d6c7e58de3377f051b4f88e2d6 ## Connecting an Upstream Gateway -->
<!-- translation-source-sha256: 245f7e46fb3f64ff70d4f3565e372fe1260ec1aeeb39f40ee88a85361bb15526 ## Effects and Limits -->
