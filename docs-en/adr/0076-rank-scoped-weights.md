# ADR 0076: Rank-scoped weights: the rank variable in a subfolder, resolved at create into one model artifact per rank

Date: 2026-11-19 · Status: Accepted

## Context

Some multi-node recipes pre-split their weights into one part per rank. The TP4 DeepSeek-V4.1-Flash recipe on four GB10 nodes, for example, gives each rank its own `rank<r>.dsw` and Engram tables (about 128 GB); the four parts live in one repository under `rank0/` … `rank3/`, 510 GB in all. Until now a deployment had one model artifact, so every node of the group downloaded and mounted all of it: three times the disk and bandwidth, and disk admission counted the whole repository.

Template weight sources already support `subfolder`, and the model artifact identity (`source, source_id, revision, artifact_subfolder`), the agent's download and model marker, and peer copies all tell subfolders apart. What was missing is "different ranks read different subfolders".

## Decision

1. **Shape: the rank variable in the `subfolder` of the spec's own weight sources**, such as `subfolder: rank${GPUWORKS_NODE_RANK}`. The spelling rule is the one of the template's launch fields (`placement.Canonical`): the documentation writes the prefixed form and the unprefixed `${NODE_RANK}` is an alias; a subfolder has no rule of its own. A subfolder may name only this variable; either every source of the spec names it or none does (a mirror that did not would give every rank the whole repository). `weight_bytes` and `runtime_weight_bytes` state one rank's size.
2. **Expanded at create into one model artifact per rank**: for a deployment over `n` nodes, rank 0's subfolder is resolved by the existing rules (source choice, revision, reuse of a registered artifact), and the result is the deployment's `model_id`; ranks 1…n-1 resolve with rank 0's provider, repository and revision plus their own subfolder, so a group never mixes mirrors or commits. The whole repository does not stand in for a rank's subfolder.
3. **The binding is kept in the deployment snapshot**: `deploy.CatalogSelection.rank_weights` is `[{rank, subfolder, store_model_id}]`, and `weight_subfolder` keeps the template's text (an exported template writes it back as it was). Like a component's `store_model_id`, the binding is fixed at create and never resolved from the mutable catalog again.
4. **Read per rank**: `planning.RankModelIDs(dep, r)` is what rank `r` reads (its own artifact, plus the components and mounts every rank shares). On each rank's node the controller makes only these ready (download tasks, copies and cache fills are all for that artifact), and the instance records that read and mounts its path. `planning.DeploymentModelIDs` stays the union of every rank's, for download ownership, cancellation and eviction guards; preparation progress lists for a node only its rank's models.
5. **Admission and estimate**: `planning.Input.WeightModel` is the rank's artifact (the deployment's model under the rank's identity), which disk admission and its insufficient-disk explanation count. The memory estimate multiplies one rank's weights by the number of ranks as the group's weights and splits that over TP×PP as usual. At create, model admission, the save-to storage and shared-storage downloads also pair each rank's artifact with its own node only.
6. **Unsupported combinations**: combined with `components` or `pd_mode` it is refused at import; deployed as PD it is refused (`RANK_WEIGHTS_PD_UNSUPPORTED`); a `model_id` the request names must be rank 0's artifact (`RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`). A single-node deployment reads rank 0 only.

## Trade-offs

- **`components[].rank` rejected**: one component per rank ties the template to a node count and leaves four near-identical entries to keep in step; components are mounted on every rank and the primary component overlaps the deployment's model, so a rank dimension would rewrite all of their semantics. The subfolder variable reuses the existing sources, artifact identity and download path with one expansion added.
- **Rank 0's artifact is the deployment's model**: its name, served name, inference configuration and the config the estimate reads all keep coming from one model row; the platform creates the other ranks' rows only as artifact identities. The cost is that one deployment shows as `n` model rows of the same repository with different subfolders.
- **The mount keeps the repository-relative paths**: consistent with existing subfolder semantics, the weights are under `${GPUWORKS_MODEL_PATH}/rank<r>/…` in the container, and the recipe refers to `${GPUWORKS_MODEL_PATH}/rank${GPUWORKS_NODE_RANK}/…`; no new "mount the subfolder" rule.
- **One more snapshot key**: the rank binding cannot be derived reliably on every reconcile (a model row may be deleted and recreated, a revision may resolve elsewhere), so like the component binding it is recorded as a fact of the create. It belongs to the snapshot's one named type, read and written whole; an old snapshot without the key means "not rank-scoped".
- **Not done**: automatic placement preferring to put rank `r` on a node that already holds `rank<r>/` (today rank order is node order, and a node holding another rank's subfolder downloads its own part).

## Consequences

- The subfolder rules `deploy.RankWeight`, `deploy.ExpandRankSubfolder` and friends; import validation in `inferencecatalog`; per-rank resolution and admission in `deployment`; `planning.RankModelIDs` and `planning.NodeModelIDs`; the controller's per-rank `ensureDeploymentModelsPresent`.
- The agent and the protocol are unchanged: each rank's artifact is an ordinary model with a subfolder.
- New failure codes `RANK_WEIGHTS_MODEL_NOT_RANK_ZERO` and `RANK_WEIGHTS_PD_UNSUPPORTED`; no new table or column.

2026-11-20 addendum: for a rank-scoped source the revision is part of the artifact's identity, not a preference. The ranks are one pre-split set of files of one commit, so a registered artifact of a known revision different from the one the template states now does not match (re-importing the template at a new revision makes a deployment create and download new artifacts for every rank; an empty revision still means unknown, not different). A whole-repository source keeps treating the revision as a preference under ADR 0018. By the same rule the deploy form's "use a registered model" lists only rank 0's artifact at that revision: the other ranks' artifacts are created by the platform, and picking one is refused with `RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`; naming an artifact of another revision returns `TEMPLATE_MODEL_MISMATCH`. No new table, column or failure code.

<!-- translation-source-sha256: f447c915d4667ab5b0b3c9fba95b883182ea2e8271db74f184f85a815745effe # ADR 0076: Rank-scoped weights: the rank variable in a subfolder, resolved at create into one model artifact per rank -->
<!-- translation-source-sha256: 33db0ca7b27da14b872ec59bb55dc04c648d655c8d4604acdb86fabe4abfcf88 ## Context -->
<!-- translation-source-sha256: d7faac9ab1e443e0280c6f9120f55c78f99b86de5df9848c1fb82edc46a802c4 ## Decision -->
<!-- translation-source-sha256: c6f695fe62fd325a8c3ed9f99873a72c733389f8741bfb535ce7d68b899fc49a ## Trade-offs -->
<!-- translation-source-sha256: ee6c4f61c64d3f58516152c2ce331eb13401d545719435bca3c70687c487dd2c ## Consequences -->
