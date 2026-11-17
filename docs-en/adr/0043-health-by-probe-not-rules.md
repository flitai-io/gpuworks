# ADR 0043: Probes Determine Service Health; Monitoring No Longer Draws Conclusions from Rules and Thresholds

Date: 2026-10-13 · Status: Accepted (replaces the monitoring center's diagnostic rules and `monitor.rules` threshold settings)

Basis: [Service Monitoring Page Redesign Plan](../../artifacts/archive/2026-10-12-service-monitor-redesign/plan.md), D3–D7.

## Context

The monitoring center previously used 16 diagnostic rules (KV saturation, prefill dominance, high temperature, idle state, and others) and a set of configurable thresholds to infer “whether a service is healthy and why it is slow” from metrics in a 5-minute window. It then condensed the results into a triage sentence on the overview and severity levels on signal cards. In practice:

- Rules provide guesses, and thresholds vary with the model, hardware, and load. Defaults often produce false positives or miss problems, leaving users unsure whether to trust them;
- For the most basic question, “is the service usable?”, rules can only infer indirectly (from error rates and ready replicas), and cannot answer “will a request sent now return a response?”;
- Feeding the rules requires five extra groups of time-series queries per snapshot—GPU temperature, utilization, memory bandwidth, throttling reasons, and host CPU—and tracking idle state and the most recent request time.

## Decision

1. **Health = probes**: for deployments with health checks enabled, the server sends a minimal request through the gateway every minute (chat sends "1" with `max_tokens: 1`; embedding-only services receive the input "1"). It classifies the result as healthy, timed out, or failed against a threshold (60 seconds by default). If no awake replica is running, it sends no request and records no available replicas. The check uses the same routing, retries, PD, and model-name rewriting as clients, but does not enter gateway accounting, the in-flight table, or request records (`internal/server/healthcheck`, `proxy.Proxy.Probe`).
2. **Report only; take no action**: probe results do not restart or remove replicas and do not affect scheduling.
3. **Enable by default for new deployments according to the model; disable by default for existing deployments**: new deployments of chat / embedding models enable health checks by default. Deployments that exist before the upgrade keep them disabled until the user enables them in deployment details. The switch and threshold can be changed at any time without restarting instances.
4. **Monitoring reports facts only**: remove diagnostic rules, threshold settings (`monitor.rules`, with migration 139 deleting stored values), triage, signal severity, and request-path stages. Signal rows show only values, details, and trends; the only highlighted fact is “fewer ready replicas than desired.” The overview counts services by probe status and supports filtering, while the service table replaces the diagnosis column with a health column and sorts by health.
5. **Keep results only in memory**: retain the last 10 results per deployment without persistence; results resume within one minute after the server restarts.

## Tradeoffs

- **Probes vs rules**: probes answer “is it usable?” but not “why is it slow?” The latter is left to facts from latency breakdowns, request views, and distributions for people to interpret. Fewer conclusions are preferable to unreliable ones.
- **Probe cost**: one 1-token request per minute for each enabled deployment, with a concurrency limit of 32. The engine's exported metrics include it; gateway statistics do not.
- **No persistence**: historical health charts are not currently required. If needed, add them as typed tables or tsdb series rather than adding persisted facts in advance.

## Consequences

- Five fewer groups of tsdb queries per monitoring snapshot (37 → 32 calls for a cluster snapshot).
- Remove `GET /monitor/rules`, validation for `PUT /settings/monitor.rules`, and `MONITOR_RULES_INVALID` together.
- Documentation: [Health Checks for Model Deployments](../how-to/deploy-models.md), [Health Checks in the API](../reference/api.md), [View Metrics and Search Logs](../how-to/monitor-and-logs.md).

<!-- translation-source-sha256: 59b7ed7d311312c8398d2a3060a2120841117916d17da1a846ed76bcb380b98f -->
