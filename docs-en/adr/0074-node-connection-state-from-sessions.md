# ADR 0074: Authenticated sessions determine displayed connection state; display does not drive recovery grace

Date: 2026-11-18 · Status: accepted

## Background

A persisted online node survives a control-plane process restart even when the new Hub has no session for it. Displaying that row directly in nodes, GPUs, images, diagnostics and statistics conflicts with actual routing availability. Writing every node as offline or connecting at startup could instead change scheduling facts and trigger unnecessary replacement.

## Decision

1. **Online requires a live authenticated session.** Hub supplies the connection fact. An unverified hello or claimed UUID cannot change another node's state. Data-lane reconnection does not replace control-lane identity or accept invalid tokens.
2. **Display uses a derived projection, not a scheduling-state write.** A live session displays online. Without one, a persisted online node displays connecting until existing disconnect and stale rules settle it. Explicitly offline nodes keep the existing recovery-phase fold; old offline nodes are not all claimed to be reconnecting.
3. **Public read models share one rule.** Node lists, details, filters, counts, associated GPU/image states, diagnostics and online statistics use this projection. Do not fix only one page or inject Hub callbacks into Store to hide network state inside persistence reads. Load required facts in batches rather than querying per node.
4. **Scheduling and failover keep existing recovery grace.** The controller's per-Run watchingSince/sighted rules continue to allow reconnection. Displaying connecting does not order a running instance deleted or moved. Do not create duplicate instances during grace; a node still missing afterward follows the existing loss policy.
5. **No new stored state or credential self-healing.** Reuse states, recovery phases and authentication facts without adding tables or persisted epochs. Actual token invalidation needs separate evidence; accepting or resetting credentials or trusting a UUID must not masquerade as recovery.

## Trade-offs

- Treating persisted online as current availability invents a fact; writing it differently solely to fix the UI affects scheduling. A derived read model expresses waiting for authentication while preserving control rules.
- A page-specific fix leaves lists, counts and associated views inconsistent. A shared connection projection keeps consumers aligned.
- This is not a new global state machine or multi-control-plane lease design. If connection authority changes, replace the fact provider rather than making each read model guess.

## Verification

Real HTTP read models show a persisted-online node with no session in a new Hub as connecting, with consistent filters and counts. Authentication changes it to online; disconnect follows offline/recovery semantics. Cover invalid tokens, forged UUIDs, GPU/image/diagnostic views and bounded batch reads. Retain short disconnect, long downtime, startup and reconnect controller regressions proving no replacement or duplicate container during grace and normal loss handling afterward.

<!-- translation-source-sha256: a565587a7a9a1eb5251844721d13a62b524e68ff77cd27e4dbc843295d799770 # ADR 0074: Authenticated sessions determine displayed connection state; display does not drive recovery grace -->
<!-- translation-source-sha256: d5fca545d832dce19d9775a32e9e78758a1a1ce03cee10ce219648fbf9cd0c1f ## Background -->
<!-- translation-source-sha256: d7c789e9bd666af0010836fda02dac36d1e4bae595afee33526187d542ec5007 ## Decision -->
<!-- translation-source-sha256: 61296fa38fea0612f717ee7a843e716de7e20f6053e8a178d0ce9d475a6e35fc ## Trade-offs -->
<!-- translation-source-sha256: fd0baa481093e90d40f30fdca335ef6439195f6b081253cb66ecedfcb65bf43b ## Verification -->
