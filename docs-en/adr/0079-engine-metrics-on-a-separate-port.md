# ADR 0079: Engine Metrics May Be on a Second Port Beside the API Port: `api.metrics.port: separate`

Date: 2026-11-20 · Status: Accepted

## Background

Every 10 seconds the agent scrapes `http://127.0.0.1:<API port><path>` (ADR 0039), and a template's `api.metrics` can only change the path and the mapping. Some `callable` engines own the API port completely and serve only `/health`, `/v1/models` and `/v1/chat/completions` there, with no `/metrics` (for example an engine whose Rust front end holds the API port). A launcher in the image can read metrics from the engine's admin socket and logs and export them on a second local port, without proxying requests. What the platform lacked was a way to reserve that port, tell the launcher, and have the agent scrape it.

## Decision

1. **The template declares only the shape, never a port number**: `api.metrics.port` is `api` (default, stored as no value) or `separate`. The port is allocated by the platform like the API port and the PD side channel (`pickPorts`, in the same allocation as the API port and usually right after it), so a template author cannot collide with other instances or host listeners.
2. **One launch variable**: once declared, the platform states `${GPUWORKS_METRICS_PORT}` (older spelling `${METRICS_PORT}`), following the one-spelling rule of launch variables (`placement/expand.go`). It is a fact only the launch knows, so a resolver cannot return it. A headless rank has none, like its API port, and the value is `0`, so one command can expand on every rank.
3. **The variable and the declaration come as a pair**: at import, catalog package upload and editor save, referencing the variable without declaring it is `CATALOG_METRICS_PORT_UNDECLARED`, declaring it without referencing it is `CATALOG_METRICS_PORT_UNREFERENCED`, and any other value of `port` is `CATALOG_METRICS_PORT_INVALID`. Referencing the variable without a declaration fails at launch too, like any other unresolved variable.
4. **The port is a fact of the launch, stored with the desired state**: the reserved port is written to `InstanceDesiredState.metrics_port` (default 0, meaning the API port, so old data needs no migration) and to the instance's `ports` list (so later allocations avoid it; second after the API port; for a PD leader: API port, metrics port, side channel block, collective port). The agent keeps it across agent and container restarts. Whether to reserve it is read from the template snapshot taken when the deployment was created (`CatalogSelection.APIContract`). When the deployment uses the model's own operations, that snapshot is replaced by the model's contract; only `metrics.port` is carried over, while `endpoint`, `format` and `map` still read the spec's current declaration (ADR 0039).
5. **Only ranks with a port are scraped**: as today, only instances that serve are scraped (single-node, multi-node leader, every PD leader). The agent scrapes `http://127.0.0.1:<metrics_port><endpoint>`; with `metrics_port` 0 it still scrapes the API port.
6. **Preview and launch share one source**: the deployment evaluation uses a placeholder port (`planning.PreviewMetricsPort`) and creation replaces it with the reserved port. Exporting a template from a deployment carries the declaration back; otherwise the exported arguments would reference an undeclared variable.

## Trade-offs

- **Not `port: <number>`**: a fixed number inevitably collides with other instances and replicas on the same node; the platform already has a port allocator and a template has no reason to bypass it.
- **Not a proxy**: the platform forwards no requests and does not expose the metrics port to the gateway; only the agent on the same machine scrapes it.
- **Not part of the Observation**: the observation is the part the server may update in place while an instance runs, but the port is reserved at launch, and editing a template cannot give a running instance a port out of nothing. It lives in the desired state and `ports`, with the same lifetime as the API port.
- **No new column**: the port goes into the existing desired-state JSON (the named type `proto.InstanceDesiredState`) and the `ports` column; the default value is the old behavior, so reading old data needs no migration.
- **An older agent does not know the field**: it still scrapes the API port and simply gets no engine metrics, with no effect on the deployment, so no agent capability was added for it.

## Consequences

- Syntax and behavior: [api.metrics in the Inference Catalog model.yaml Syntax Reference](../reference/inference-catalog-spec.md#apimetrics引擎指标映射); how to write it: [Declare Engine Metrics for an Inference Template](../how-to/declare-engine-metrics.md#metrics-on-another-port).
- Protocol: `InstanceDesiredState.metrics_port`.
- New error codes: `CATALOG_METRICS_PORT_INVALID`, `CATALOG_METRICS_PORT_UNDECLARED`, `CATALOG_METRICS_PORT_UNREFERENCED` (en/zh texts changed together).

<!-- translation-source-sha256: 893a102abbe30137eed1180dc499a0ea5cc7401bc6a6b5c24c63fc491baa6567 # ADR 0079: Engine Metrics May Be on a Second Port Beside the API Port: `api.metrics.port: separate` -->
<!-- translation-source-sha256: 75c2a7a5e6d49c4a8dcb2d71733a4d5905d3c367ed824571ed3a3b659dc9197d ## Background -->
<!-- translation-source-sha256: 556c212c56ef0fefc9de50952912069d0636e7a1dd681fe17a9b5227b7f2f181 ## Decision -->
<!-- translation-source-sha256: 86db91b9f1c0b157305e909d083ca1086536380c3354e8569f2ad46259e8658d ## Trade-offs -->
<!-- translation-source-sha256: 94b9e8e13db12426be46e0813cac69d49b84b262fbdc2292df19d1744746cdc1 ## Consequences -->
