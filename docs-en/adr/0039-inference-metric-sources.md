# ADR 0039: Inference Metric Sources Are Layered; the Server Alone Resolves Observation Configuration and Updates It In Place at Runtime

Date: 2026-10-10 · Status: Accepted

## Context

Callable deployments (TensorFold, adapters wrapping SGLang, and modified vLLM versions running as callable) show no inference metrics. The agent scrapes each instance's `/metrics` every 10 seconds,
but mappings are hard-coded in the vLLM / SGLang engine Profiles. Callable falls back only to the `gpuworks_callable:` prefix, which no engine exports, so all scraped content is discarded.
Templates have no fields for declaring metric endpoints or mappings. Platform metric names (`gpuworks_infer_*`) are strings scattered across engine mappings, monitoring, the gateway, and the frontend; no single place defines "which inference metrics the platform has,
and their types and units."

Deployment snapshots freeze launch configuration, whereas metric mappings determine only how the platform observes instances, not how instances run. Requiring users to redeploy to see data after a mapping error or an engine upgrade renames a metric imposes a cost disproportionate to the benefit.

## Decision

1. **Platform inference metrics are defined once, in the leaf package `internal/inferencemetrics`**: short names, types (counter / gauge / histogram), units, mapping rule types and validation,
   built-in vLLM / SGLang mappings, and derived metrics (KV utilization calculated from pool capacity and free capacity). Stored names are `gpuworks_infer_<short-name>`, with all existing names retained.
   The agent and server share this package, which depends only on `internal/engines`. The engine Profile's `MetricMap` becomes a view derived from it rather than a second copy.
2. **Templates declare `metrics` in the `api` contract**: `endpoint`, `inherit` (vllm / sglang), and `map` (engine family → short name, with optional label selectors and conversions in the long form).
   Effective mappings = inherited mappings overlaid entry by entry with the template's `map`, **with template entries taking priority**. Targets use short names, validated against the vocabulary at import with stable error codes. No tables or columns are added: `api` is already read and written as a complete JSON object in the spec
   (`inferenceapi.Contract`), and `metrics` is one of its keys.
3. **Callable automatically detects prefixes when no declaration is provided**: it continues scraping `/metrics`, but the fallback changes from looking for `gpuworks_callable:` to "apply the vLLM mapping if a `vllm:` family is found; otherwise, apply the SGLang mapping if a `sglang:` family is found." If neither exists, no engine metrics are produced.
4. **The server alone resolves observation configuration**: `deploy.ResolveObservation(engine, api.metrics)` produces `proto.InstanceObservation` (endpoint and rules, or candidate rule sets for automatic detection,
   with targets expanded to stored names) and writes it into the instance's desired state. The agent only executes: scrape, select rules, map, convert, derive, and report. It makes no decisions.
   The legacy `MetricPath` / `MetricPrefix` / `MetricMap` fields are also populated as before, allowing older agents to collect using the old logic. Rules with conversions are excluded from legacy fields to prevent older agents from storing incorrect units.
5. **Observation configuration uses the current template and updates in place at runtime**: both launch and refresh read the current `api.metrics` of the profile used by the deployment (template row + spec source + spec name). If the profile has been deleted, they fall back to the `api` contract in the deployment snapshot.
   Catalog writes (import, edit, and built-in catalog reconciliation) publish an `inference_template` event after the transaction commits (with the template row ID or "all"). The controller coalesces events,
   resolves instances of deployments referencing those templates again, rewrites only instances whose observation configuration has changed in a single transaction, and then pushes by node. The server performs one full refresh at startup.
6. **In-place updates use a separate message, not desired-state replacement**: `StateUpdate.Observations []ObservationUpdate` carries only the instance UUID and new observation configuration. The agent replaces the observation fields on instances it already holds,
   without taking over containers again, repeating health probes, or reporting status; unknown UUIDs are ignored. Older agents do not recognize this field and ignore it, receiving the new configuration from stored desired state at the next `state.sync`.

## Tradeoffs

- Reject "push the entire new desired state again and let the agent compare and skip": `ApplyUpdate` takes over containers and probes health again, potentially reporting a running instance as starting briefly or even reclaiming it.
  Fields such as `ImagePullRefs`, populated during replay, also make agent-side detection of "only observation configuration changed" unreliable. A separate message guarantees "replace observation configuration only" at the protocol level.
- Observation configuration is decoupled from deployment snapshots: a deployment may see mappings different from those at creation. This is intentional—mappings affect observation only, not execution; `api.metrics` therefore does not participate in "deviates from template" checks.
- Older agents do not get conversion, derivation, or automatic detection; these take effect with an agent upgrade. Conversion rules are removed from legacy fields because missing data is preferable to incorrect data.
- Built-in mappings move from engine Profiles to a leaf package: engine facts and the vocabulary reside together, and `inherit` and automatic detection reference the same rules. Profiles retain a derived view for existing callers.
- Refresh is triggered only by catalog changes and uses template row IDs for indexed deployment and instance lookups. Instances with unchanged resolved configuration trigger neither database writes nor pushes; the full startup refresh is a one-time cost.

## Consequences

- Engines with changed prefixes, such as TensorFold, integrate with dashboards through template declarations. Engines retaining vLLM / SGLang naming provide data when deployed as callable even without a declaration.
- After a template author changes a mapping, running deployments apply it on the next scrape (≤ 10 seconds), without a restart or redeployment.
- The gateway measures TTFT, duration, and engine-reported usage token counts for every forwarded response (`gpuworks_proxy_*`, with names also registered in this vocabulary). Monitoring selects sources by signal: TTFT, end-to-end duration, input/output tokens, running requests, and prefix cache hit rate use engine values when reported, otherwise gateway values. The single decision point is the signal source catalog in `monitor` (`window_sources.go`). The UI does not label sources; differences in measurement definitions are documented in the monitoring documentation.
- Documentation: [Inference Metric Vocabulary](../reference/inference-metrics.md), [`api.metrics` Syntax](../reference/inference-catalog-spec.md#apimetrics引擎指标映射),
  [Declare Engine Metrics for an Inference Template](../how-to/declare-engine-metrics.md).

<!-- translation-source-sha256: 0fdb9febe543615eaeb859e60ba2b60bbe913d865a32b724202fa8dbbb1c1085 # ADR 0039: Inference Metric Sources Are Layered; the Server Alone Resolves Observation Configuration and Updates It In Place at Runtime -->
<!-- translation-source-sha256: c4cb1836b18e70825542e9bb505031c83305ae6b2caebe6409180b0154555fea ## Context -->
<!-- translation-source-sha256: 3666bcebb0fd924903a4c0df8dd5a1f23f460f7ae00cc251bfba92188843bd9c ## Decision -->
<!-- translation-source-sha256: 1e7e48bbd4723bd6cd83af7ce8dbb27634a95f406b9926014e8e94e7c2e83c93 ## Tradeoffs -->
<!-- translation-source-sha256: a4e7519c4e29c20267ca9391b48a4b59bb928e1433b2d19eb6397e287d70b96e ## Consequences -->
