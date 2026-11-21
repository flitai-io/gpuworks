# ADR 0072: App owns resources; each Run stops and drains, and Close releases them

Date: 2026-11-18 · Status: accepted

## Background

An App already supports stopping and running again in the same process, and the test harness relies on that contract. Request history, long-term statistics and the trace exporter are created and bound to fixed readers and sinks in New, but the first Run used to close them permanently. The second run could still serve inference while history and quality queries failed with a closed database. Replacing one field would not update closures capturing the original object.

## Decision

1. **App and an individual Run have different lifetimes.** Stores, fixed adapters and the exporter constructed by New remain valid until final App.Close; each Run owns its listeners, HTTP server and background loops. Ending a run stops admission, drains work and writes final records without destroying objects needed by the next run.
2. **Close is the final, idempotent release operation.** A closed App cannot run again. One coordination state closes admission, stops and waits for an active run, then releases resources in dependency order. Waiting must not hold locks needed by the running code to finish.
3. **Callers distinguish stopping from disposal.** The CLI finally closes the App on exit. The harness preserves it when StopServer returns and closes it only during final cleanup. Construction failure also releases resources already constructed, including observability stores, refresh loops and the exporter.
4. **Drain ordering is a behavioral contract.** Finish requests and agent connections first, wait for the accountant's last enqueue, then finish the metrics ingest drain and flush request and statistics records. The trace sender belongs to App, survives between runs and drains on final Close. Normal exit, forced HTTP shutdown and listener failure share one stop sequence.
5. **Hub owns its connection lifetime.** Waiting includes unauthenticated control/data handshakes, server_dial attempts and their child work, not just registered sessions. Stop first prevents new work and cancels the current connection generation, then waits for it to finish so no handshake or report accesses a closed store.

## Trade-offs

- Reopening and rebinding the whole request pipeline on each run would spread reference replacement across sinks, history, analytics and the exporter. Preserving existing App-level objects confines restart to per-run resources.
- Merely removing a close defer loses final flushing and release. Waiting only for successful sessions misses unauthenticated handshakes. Ownership and connection admission and exit must all be covered.
- No generic resource graph or lifecycle framework is introduced. Coordination state expresses running, final closure and connection-work admission; existing business algorithms and storage layouts remain.

## Verification

Two real inference HTTP requests across two Runs of one App succeed, both records remain queryable through history and quality endpoints, and a controlled collector receives traces. Final records are flushed on stop. Cover final and repeated Close, Run after Close, concurrent shutdown, construction/listener failure and unfinished handshakes, while retaining accountant-to-metrics drain and loop-exit regressions.

<!-- translation-source-sha256: 05e0b593cda7153d3fef60d109f494793b10a7557c2a7d8a19bc56ba921c297d # ADR 0072: App owns resources; each Run stops and drains, and Close releases them -->
<!-- translation-source-sha256: e18a07bfa6407315af842c0d8a36b9af11353e2face84acc03eb434e4e9cb24a ## Background -->
<!-- translation-source-sha256: 8812473d8211082c005683f6673b331d90a09f3170a656f09a98865052044236 ## Decision -->
<!-- translation-source-sha256: 6638d2cca30b8d7d0abf2f68ddb55f77ce8fe5402457b0bf0531d51046f8d2a7 ## Trade-offs -->
<!-- translation-source-sha256: 0abc155503ed29ebdeabab43faeba964c5b51fccbaf0f8e8d8b72846d9a4f146 ## Verification -->
