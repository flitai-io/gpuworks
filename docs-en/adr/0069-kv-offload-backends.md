# ADR 0069: KV offload backends are compiled-in, engine-scoped descriptor rows assembled at one point

Date: 2026-11-16 · Status: Accepted (amends the parameter classes of [ADR 0021](0021-platform-params-copyable-command.md); keeps the PD roles of [ADR 0028](0028-pd-disaggregated-deployment.md) and the codes of [ADR 0067](0067-runtime-outcomes-as-codes.md))

## Context

vLLM KV offload has one kind today, the platform's native one: the memory tier (`kv_offloading_size`) and the disk tier (`kv_disk_offload`, `shared_kv_dir` and related keys). No server-side type represents "a KV offload backend". About a dozen places each work out tier meaning from raw `LaunchConfig` fields:
- parameter validation and performance warnings;
- argv, `/dev/shm` sizing and the disk mount;
- the PD decode role clearing offload fields;
- the requested-feature snapshot;
- template reverse parsing and unset;
- the deploy form and the instance panel.

External backends are arriving: LMCache, Mooncake Store, and SGLang's HiCache. In the current shape each one adds another round of `== "<id>"` checks across Go and TS. It is also easy to mark a key that reaches the command line as a platform parameter, which breaks ADR 0021 decision 2.

Separately, `engines.CommandFeatures` classifies PD by `kv_role`. The non-PD disk tier also carries `kv_both`, so it is recorded as PD and refused on a vendor without PD support (Kunlun).

A connector name, `kv_connector_module_path`, extra config and environment variables all amount to code execution in the GPU container, so where they come from must be controlled.

## Decision

1. **A backend is a descriptor row compiled into the binary, unique per (engine, id).**
   - Rows live in the leaf package `internal/server/kvbackend`, which depends only on `accelerator`, `engines` and `pdmode`. It has no `init()` registration and no mutable package state; the registry is an immutable value built by `Builtin()`.
   - A row declares data only:
     - its engine;
     - whether it is the engine's default row (an empty selection resolves to it; at most one per engine);
     - a vendor allow-list;
     - whether it is external;
     - the native dictionary keys it excludes (`Replaces`);
     - the PD roles that keep it (`PDRoles`), which always include prefill, the role whose KV a cache tier serves again;
     - the topologies it supports (a node-count cap, PP, DP);
     - a display label.
   - Rendering is not in the row. It is a plain Go builder function in `deploy`, selected by id, with an optional reader.
   - A field is added only when the first row that needs it arrives.
2. **Callers select an id, nothing else.**
   - APIs, templates, manifests and nodes may only name a backend id. They never supply a connector, module path, environment variables or extra config, and values are serialized with `encoding/json`.
   - Rows are never deleted. An unknown id returns a code on evaluate, edit and launch. Stored rows still decode, and running instances are unaffected.
3. **The only new persisted fact is `LaunchConfig.kv_offload_backend`.**
   - An empty value means the engine's default row, which is `native` for vLLM.
   - The registry generates the dictionary key. It is published only when the engine has a selectable (non-default) row. While unpublished, the key is dropped from requests as before, matching older releases.
   - In the parameter table it is one definition carrying the `Selector` marker (its text comes from the parameter locale files); its options are generated per engine from the registry and labelled with each backend's product name.
   - It is a third class of key beyond ADR 0021, a **selector key**:
     - it is not a member of an engine flag column, so it does not join the `--kv-transfer-config` composite group and stored unset sets do not change;
     - it is not a platform parameter either, because it shapes the command line and the engine adapter reads it.
4. **Exclusivity is judged on effective values, and the server refuses rather than silently clearing.**
   - With an external backend selected, any native key in `Replaces` with an effective value returns `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER`. Effective values are:
     - a memory tier above 0;
     - the disk tier on;
     - a non-empty directory;
     - a capacity cap above 0;
     - threads and block size, but only while the disk tier is on.
   - `kv_offload_block_size`, which has no dictionary row, counts whenever it is above 0.
   - Values written by dictionary defaults do not count.
   - With a backend selected, the adapter renders only that row, never the native tiers. Every selectable row's `Replaces` must therefore cover every key of the engine's own offload, which the `kvBackendSetProblems` test in `deploy` enforces.
   - Because of exclusivity, every native field is zero when a backend is selected, so the existing `/dev/shm` and disk-mount readers need no knowledge of backends, and the form and instance panel can show both tiers as replaced for any backend.
5. **One assembly point per engine.**
   - vLLM's `--kv-transfer-config` is composed only in `composeVLLMKVTransfer`. The PD fragment comes from `pdmode` and the offload fragment comes from the selected backend.
   - When both exist they become a `MultiConnector`: the outer role is the PD role, and the load-failure policy stays at the top level.
   - A backend's renderer receives the whole launch (PD role, vendor, compute network, parallelism) and decides its own inner role, endpoint and environment. It may give keys the outer `MultiConnector` needs, such as a vendor's connector module path (the composer's own keys win), and environment, which `vLLMEngine.Build` merges in one place.
   - Without PD and with only the native memory tier, the `--kv-offloading-size` form is kept.
   - The assembly point takes one PD fragment and one offload fragment, so a combination of two offload fragments cannot even be expressed, and no user-facing code exists for it.
6. **Role scope under PD is declared by the row.** `PDRoleLaunchConfig` clears the selection and the native fields when the role is not in `PDRoles`. The native row is `[prefill]`, as today. The PD evaluate warning `PD_KV_OFFLOAD_PREFILL_ONLY` reads the selected row's `PDRoles` (it warns when decode does not keep offload), and the instance read model's `kv_offload_backend` is scoped to the instance's role the same way.
7. **Vendor and topology are checked once, on the server.**
   - `ValidateTargetFeatures`, which every validating mode reaches, checks `Backend.Vendors` and (node count, PP, DP). It returns `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET` or `KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED`.
   - `ProfileForTarget` uses the same predicate to decide which backends to publish (`kv_backends`), and the UI only renders them.
   - An unknown vendor (evaluate with no node, before automatic placement) is not known yet and is allowed, as `accelerator.Supports` allows it; the assigned target is checked after placement.
   - `FeatureExternalKVCache` is only the identity for the requested feature and the scheduler reason (`kv_backend_unsupported_on_node`). It has no second vendor table.
   - For an external backend, `ResolveLaunch` returns `KV_OFFLOAD_BACKEND_CONFLICTS_ARG` when a template or user argument layer states a flag that would replace its configuration (engine hook `KVConflictFlags`, matched after normalizing `_`/`-`, `--x=y` and quotes).
   - The check runs under PD too. `PD_CONNECTOR_ARG_CONFLICT` matches only the hyphen spelling, and the engine reads the underscore spelling as the same flag, which would push the backend out of the prefill's `MultiConnector`. How `CheckPDArgs` and warning W4 match flags for PD deployments without a backend does not change here.
8. **PD is classified by connector, not by `kv_role`.**
   - A known offload-only connector (today `OffloadingConnector`) is not PD, whatever its role.
   - A `MultiConnector` is classified by its children.
   - Any other connector with a valid role is still PD.
   - This table classifies only configs a user wrote. In a non-PD launch, a `--kv-transfer-config` identical to the one the platform composes carries only the selected offload backend, and `ResolveRequestedFeatures` classifies it as not PD, so adding a backend needs no change to the table in `engines`.
9. **A backend's own configuration reads back as its selection.**
   - The engine hook `RecognizeKVBackend` reads a selectable external backend's non-PD config back into `kv_offload_backend`. A backend with a reader decides and returns the values of its own dictionary keys, which are written back beside the selection; one without a reader is recognized only by its rendering with nothing else stated. `ReverseArgs` and `CanUnsetTemplateFlag` share the hook, so a deployment saved as a template deploys again with the same selection.
   - An object wrapped in a PD `MultiConnector` stays opaque, and unsetting the native keys over it is still a conflict.

## Trade-offs

- **Compiled in vs configurable at runtime.** Adding a backend or following an upstream rename needs a release. This cost is accepted, because a backend descriptor is in effect code executed in the container and must not be supplied by an API or a node.
- **Data rows plus Go builders vs a descriptor DSL.** Each backend's configuration has a different shape, and a DSL would only grow a bag of optional fields. Rows hold only the data used for decisions, and shapes stay in testable Go functions.
- **Landing the seam with only the native row first.** The generic machinery for external backends (exclusivity, roles, gates, reverse parsing, form cards) is driven only by a test row at that point, and every user-facing surface is byte-identical to the previous release. The first external backend then only adds a row, a builder and tests.
- **The PD classification changes behaviour.** A non-PD vLLM disk tier on Kunlun is no longer refused, and that combination has not been verified on hardware. An unknown offload connector a user writes is still treated as PD until it is added to the list, which is the safe direction.

## Consequences

- To add a KV offload backend, add one row in `kvbackend` and one builder in `deploy` (with a reader when needed), with tests. Only vLLM has an offload adapter today; the tests refuse a row of another engine until that engine renders and reads it. Backend id literals must not appear in other Go code or in `ui/src`; this is enforced by the `kv_backend_id_literals` budget in `scripts/arch-constraints.txt`, with a baseline of 0.
- Downgrade: an older server decodes with plain JSON and drops `kv_offload_backend`.
  - A deployment that selected an external backend relaunches there with no offload.
  - An edit or draft round-trip removes the selection.
  - This behaviour is documented and pinned by a test.
- In a mixed-vendor cluster the scheduler does not filter nodes by a backend's vendor allow-list. An unsupported vendor is refused at assigned-target validation.
- Terms: the domain glossary (`CONTEXT.md`).

<!-- translation-source-sha256: 6a3a4ac8ea8427947f93fdd1e3325fafecc8ea6f13348def47f6f3a87684645c -->
