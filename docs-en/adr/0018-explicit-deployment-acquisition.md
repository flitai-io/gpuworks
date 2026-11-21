# ADR 0018: Make Deployment Sources Explicit and Stop the Server From Replacing User Selections

Date: 2026-09-07 · Status: Accepted (supersedes the three-level preference of “already present on node > default source > template declaration” and `source_resolution` introduced by `plan-9-15-bug-02-model-source-reuse.md`; cross-source equivalence validation rules remain)

> 2026-10-16: Explicit acquisition selection during deployment (already present locally / peer sync / public download / network path) is superseded by [ADR 0048](0048-pull-settings.md): deployments no longer select sources; nodes acquire artifacts themselves according to pull settings and sync groups. Deployment requests still specify the storage for saving models and the weight source, which are validated without replacement under this ADR.

## Context

Where to download models, where to pull images, and whether they are present on nodes originally follow three independent sets of implicit rules, and the deployment page shows none of their results:

- The model source is not a deployment property. Direct deployment reads `Model.Source`, while template deployment uses a separate three-level preference and then replaces the user-supplied `model_id`. Selecting a template means selecting a source, while the global `default_model_source` serves only as a tie-breaker on the template path, so users see no effect when changing it in Settings.
- The image mirror chain applies only to refs registered in the image catalog. Most template spec images are absent from that catalog, reducing the candidates to the canonical ref alone.
- Two cache detection mechanisms exist, and the deployment flow reads neither: the wizard, automatic node selection, and scheduler disk estimates all ignore image caches. On the model side, `acquisition=download` immediately disables peer sync. Even if another online node has a complete copy, the target connects directly to the public network and fails on timeout without retrying.

Together, these rules leave you unable to see or change what happens. The path with a high production deployment failure rate is precisely the one where the model could have been copied in seconds from another node in the same cluster.

## Decision

**Acquisition sources are explicit fields in deployment requests.** Requests carry `model_source` and `image_pull`; both use `local_present` / `peer_sync` / `public`, with a provider and network path for public access. All values come from the candidate set returned by evaluate; clients do not construct strings themselves.

**The server validates but does not replace.** If a candidate becomes invalid, return `ACQUISITION_CANDIDATE_STALE` and require reevaluation instead of silently switching to another source. The explicitly selected peer source node is the only candidate; if it goes offline or its copy is no longer complete, fail immediately without switching to another node or a public download. Template and catalog metadata provide prefilled values only.

**Remove `default_model_source`.** Remove it from Settings, seeds, and template-path preference ordering together; `PUT /settings/default_model_source` returns 410.

**Merge `model_endpoint` into network rewrite rules.** Model download URLs and image pull candidates then share a single `network.rewrites` representation. Migration converts a nonempty endpoint to a `https://huggingface.co → <endpoint>` rule. The network path selected for an individual deployment is written to that download task without changing the node's global configuration.

## Trade-Offs

This gives up the convenience of having the system automatically select an available source. If a request omits `model_source`, the server still fills in the recommended value and marks this in the snapshot. Once the user makes a selection, however, the server no longer changes it behind the scenes—even when that change would otherwise be correct (for example, the selected peer happens to be offline while another online node has a complete copy). The benefit is explainability: deployment details can answer where this deployment's weights and image come from, and on failure you can see which path failed instead of encountering a silent stall.

Removal of `default_model_source` is irreversible: operators relying on it for cluster-wide source preferences must subsequently select a source explicitly for each deployment or use network rewrite rules to point public addresses at internal mirrors.

## Impact

2026-09-10 addition: the deployment page constraint optimization plan adds image selection candidates and aligns image acquisition modes with those for model weights:
already present locally, peer sync, and public download. Public download explicitly selects a Registry source declared in the catalog, followed by
a direct connection, node proxy, or Docker Hub mirror. The server normalizes the selection into a single actual pull ref and writes it to the existing deployment
snapshot. `image_pull.candidates` is retained only for older clients, older snapshots, and manual pull/retry from the image catalog. It is no longer displayed as
the “pull address order” for new deployments. The global `image_registry_rewrites` server configuration and its Settings page entry remain available to these
compatibility paths, but new deployments do not read it. This adjustment follows the current plan and replaces the description in this ADR's 2026-09-09
addition that new deployments submit an ordered Registry pull chain.

The engine profile's single default image serves only as a compatibility fallback when the catalog is missing in older environments. It does not participate in
the candidate list when an existing catalog match is available. After selecting a canonical image, run evaluate again, then submit the source and network path returned by the server.

2026-09-08 addition: resolve an empty revision once at the source selection entry point. Find existing identities by provider, repo, and subfolder, prioritizing rows already present on the target node, then ordering by ID; create a row with an empty revision only if no match exists. Exact matching and cross-source equivalence functions do not introduce wildcard semantics. New catalog entries may omit revision, but revisions on existing entries are retained: bulk deletion would reduce reproducibility, and public fallback could cause different nodes to obtain different upstream versions under the same model ID. Any future removal requires a design for version convergence across machines first.

2026-10-27 addition: an explicit provider also reuses a copy at the same provider and repo whose revision is empty, that the target node can already use, and whose size is within 0.1% (at least 1 MiB) of the spec's `weight_bytes`. Such rows mostly come from shared-storage scan imports (for example a directory a provider CLI downloaded, with no commit information); an empty revision means unknown, not a different version. Without the reuse, a second full copy of the weights would be downloaded to `<repo>-<revision>` on the same storage. The reuse never crosses providers and never takes a row whose known revision differs; the deployment snapshot records the row actually mounted.

Source candidates retain the full list declared by the spec; local copies affect only the default recommendation. An explicit public provider constrains the actual download identity, preventing cross-source equivalent reuse from redirecting the request to another provider. Image catalog queries first seek an exact match, then examine declared registry_sources; cache queries independently ask nodes by ref, exposing failures through cache_reason.

- The new `internal/server/acquisition` package only produces candidates; it does not select them. Its input is an explicit facts struct.
- The new `internal/server/imagecatalog/pullref` package consolidates pull candidate ordering, with store and imagecatalog sharing one implementation. The divergent `store.imagePullRefsForImage` implementation is removed. The two candidate chain implementations mentioned in ADR 0017's context now use the same code.
- Pull candidates apply to any tagged ref, without requiring registration in the image catalog; prefix matching respects segment boundaries after Docker Hub normalization.
- Node network overrides gain read/write APIs and a card on the node details page, giving evaluate's “This node's override” label a definitive source.

<!-- translation-source-sha256: da975bfd7f70414928d985b8f08c0fb74344f018e9d830569d027ef0625a323e # ADR 0018: Make Deployment Sources Explicit and Stop the Server From Replacing User Selections -->
<!-- translation-source-sha256: f11359ae6bbc870888cff1af7f7d3762815fc331b852a6c56f400971db07493a ## Context -->
<!-- translation-source-sha256: 953ab162e72ec84a9d6e6701c856b661121860be982b05723d545778e588cc44 ## Decision -->
<!-- translation-source-sha256: ae380195f59a5e7e9aa52af52b3583308c3a948b9bd71b914035aba3e99a0204 ## Trade-Offs -->
<!-- translation-source-sha256: 1151fe0839832e0458b086f0b78f37dd4291813d9f2fa0e60acc3f12d1d9f04d ## Impact -->
