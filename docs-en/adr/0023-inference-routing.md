# ADR 0023: The Multi-Replica Inference Gateway Uses Load Balancing and Retries Connection-Level Failures by Default

Date: 2026-09-23 · Status: Accepted

Multiple running replicas of the same deployment use least-request selection by default. Server-side in-memory snapshots maintain candidate instances and node identities; routing does not query the database on the request hot path. The local in-flight request count is the primary signal, while fresh engine queue counts and KV utilization act only as soft penalties. If a node tunnel or target port fails before an upstream response arrives, the gateway replays the request among candidate replicas, briefly cooling down each instance according to its failure count. Once an upstream response arrives, including a 5xx, it is returned unchanged without further retries.

The request body first enters an in-memory replay body with a global budget, spilling to disk only after the threshold is exceeded. `GetBody` creates an independent reader for each retry. Deployments can choose `least_request`, `prefix_affinity` with deterministic rendezvous hashing, or `failover` to retain the old semantics. The policy is routing configuration rather than an engine launch parameter, so changing it does not restart instances.

This decision does not implement load sharing across servers, precise KV event awareness, sleep draining, or prefill/decode (PD) disaggregation. Periodic refreshes, event triggers, and the slow path for snapshot misses preserve eventual consistency for future HA. Affinity hashing uses FNV, which is stable across processes, rather than hashing with a process-random seed.

<!-- translation-source-sha256: 4c89e0383134a62b3146082ad4464c22e0bfdb0ed368b40ffac805fe8991cf2a # ADR 0023: The Multi-Replica Inference Gateway Uses Load Balancing and Retries Connection-Level Failures by Default -->
