# ADR 0026: Centralize Network Decisions in a Pure Leaf Package and One Service, With the Network Catalog Injected Through an Interface

**Status**: Accepted
**Date**: 2026-09-27

## Context

Decisions about node networks ([ADR 0030](0030-network-objects.md))—resolution, management-network fallback, network selection for compute groups, storage sync paths, and whether a network can carry serving traffic—previously reside in the `store` package. The api, controller, hub, planning, and proxy call them directly through seven methods on `*store.Store`. Role/type vocabularies are also defined in store, validation is scattered across api and store, and store performs resolution itself when assembling agent desired state. As a result:

- There is no single boundary that answers "which network serves this role," so the implementation cannot be replaced;
- store both persists data and makes domain decisions;
- The same rule has multiple implementations (api and store each normalize roles, in different orders; deployment previews, actual startup, and NCCL tests each implement "resolution result → compute-network launch arguments" separately).

The product is also considering making complex networking (custom networks, dedicated serving/storage/RDMA compute networks, and internal tests) an advanced feature, while the standard edition uses only the management network and agent tunnels. Removing all network objects naturally makes existing resolution fall back to the management network; all that is missing is a clean boundary.

## Decision

1. **`internal/server/netplan` is a pure leaf package**, depending only on `proto`: it contains the vocabulary, domain models (decoded Network / Attachment / Node), `Resolve`, management networking, compute groups, storage sync paths, serving eligibility, serving forwarding addresses, and the roles covered by internal tests. Every network decision is implemented exactly once here.
2. **`internal/server/network` is the sole entry point**: it reads node and network rows from store, with `convert.go` as the only conversion from store rows to netplan models. It provides `ComputeGroup`, `StoragePath`, `CanServe`, `ServingIP`, `ServingEndpoints`, `NodeNetworks`, `Endpoints`, and `State` to core flows. Validation and changes for networks, attachments, and management-network roles also live here.
3. **store only persists data**: it saves networks, attachments, test records, and node columns; it does not resolve networks, hold vocabularies, or validate roles. Architecture checks enforce that store does not depend on netplan / network. store no longer writes serving-network addresses into desired state; hub asks the network service when pushing it.
4. **Consumers declare narrow interfaces**: hub, controller, proxy, and planning each declare the methods they need. `app` creates a single `network.Service` and injects it into every component (api receives it through `Deps.Networks`), ensuring that all components read the same catalog.
5. **Inject the network catalog through `network.Catalog`**: `StoreCatalog` contains all networks and attachments in the database. `ManagementOnly` declares no networks, so resolution always falls back to the management network and agent tunnels; network and attachment changes return `NETWORK_DEDICATED_UNAVAILABLE`, while per-node management-network role toggles remain available. The catalog is selected in one place in `app`. It is currently fixed to `StoreCatalog`; whether and how to introduce edition-based switching is a separate decision.

## Consequences

- Making complex networking an advanced feature later requires only selecting a catalog implementation in `app`. To exclude advanced code entirely from the standard-edition binary, build tags can be added to `Catalog` implementations and network-page routes without changing core flows.
- api gains dependencies on `network` and `netplan`, and `nodeaddr` is folded into `planning.PeerHost`, giving `api_fanout` a net increase of 1. `Deps.Networks` increases `api_deps_fields` by 1. The `network` service reading store increases `store_concrete_importers` by 1 (see `scripts/arch-constraints.md`).
- Disabled management-network roles are now stored in role display order (previously alphabetical). Existing stored orders are compared as sets on read, so they are not incorrectly treated as changes.

## Alternatives Considered

- **Hide features by edition only in the API and UI**: this requires the fewest changes, but resolution remains scattered throughout store, core flows have no replaceable boundary, and the complexity cannot be isolated from the standard edition.
- **Split into a separate repository or plugin**: Go's static single-binary model is unsuitable for plugins, splitting repositories is costly, and core flows still need a local management-network implementation.
- **Move network HTTP handling into a separate package with its own routes**: this provides a more thorough edition switch, but DTO assembly for network pages currently shares the api layer with other pages. Keep that consistency for now, and split when routes need to be excluded by edition.

<!-- translation-source-sha256: 6da0fdb80cccdb4aa84ce3ef343b5ec295effab2ad33f57549e8b4270e0bdf5a -->
