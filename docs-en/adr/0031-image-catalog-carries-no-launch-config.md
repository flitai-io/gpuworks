# ADR 0031: The Image Catalog Carries No Launch Configuration; All Deployments Use the Same Launch Semantics

Date: 2026-10-01 · Status: Accepted (supersedes the parts of ADR 0022 concerning existing deployments retaining image defaults and the `launch_contract` marker, as well as the remaining image-source-based command mode defaults in ADR 0013)

Based on: [Deployment Launch Configuration Consolidation Plan](../../artifacts/archive/2026-10-01-deployment-launch-config/plan.md).

## Context

ADR 0022 stops new deployments from reading the editable defaults layer in the image catalog (Image Defaults: env, Docker arguments, mounts, model mount path,
entrypoint, cmd, and command mode). To preserve the launch semantics of deployments created before the upgrade, however, it retains this layer and a `launch_contract` marker:
existing deployments without the marker still launch using the image catalog's current values.

This leaves parallel semantics used only for existing deployments and continues to mislead:

- For each instance, the deployment details page shows a comparison table of the deployment-time snapshot versus current image defaults. New deployment snapshots contain only the fixed baseline, but the comparison uses the current image catalog
  row as the reference. Any value in that row triggers a "changed" indication and a suggestion that you can synchronize on rebuild—even though rebuilding does not synchronize them.
- The defaults layer for every image version in the built-in seed contains only `HF_HUB_OFFLINE=1`, duplicating exactly what all three engines inject themselves.
- When empty, the `entrypoint_display`, `cmd_display`, `env`, and `extra_mounts` columns are backfilled from node inspect results, mixing image facts and manual configuration
  in the same columns without a way to distinguish their sources.
- The product has not been released, so no existing deployments need protection.

## Decision

1. **The image catalog describes only artifacts**: reference, retrieval address, engine and framework versions, architecture, vendor, enabled and deprecated status, and compatibility facts (ADR 0019).
   Remove the image row's `env`, `docker_args`, `extra_mounts`, `model_mount_path`, `entrypoint_display`, `cmd_display`,
   `command_mode`, `run_command`, and `inspect_synced` columns; remove `run_spec` and version-level `command_mode` from the manifest.
   Ignore these keys when importing old manifests and offline packages.
2. **The image's built-in ENTRYPOINT/CMD/ENV are runtime facts, not separate catalog data.** The container runtime uses them directly. To view them, inspect the node in real time
   or read the instance's launch facts (runtime-observed image and container inspect data). Inspect results are no longer written back to the catalog, and node-reported image status includes only the image ID.
3. **All deployments use the same semantics**: launch configuration comes only from the engine, the selected inference template preset, placement results, and inputs for this deployment. Remove
   the `launch_contract` marker and all branches based on it. Command mode defaults to `engine`; `image-entry` and `manual` require explicit selection in the deployment.
4. **Deployment customization has just one container overrides layer** (`deploy.ContainerOverrides`, "Container Overrides" in CONTEXT.md): setting and deleting environment variables,
   mounts, Docker arguments, `/dev/shm`, model mount path, entrypoint, command, and command mode. `env_unset` removes keys set by any lower layer (engine, compute network,
   or template); device and dGPU variables injected on the node are not part of the launch env and are unaffected.
5. **Instances no longer save image defaults snapshots.** The resolved launch plan in the instance's desired state is its complete launch configuration.

## Consequences

- In databases upgraded to this version, unmarked deployments use the new semantics after rebuilding. Deployments using unofficial images that previously relied on catalog `image-entry`/`manual`
  or env/mount settings must set them explicitly in the deployment. Check and rebuild pre-release demo environments individually.
- To reuse a nonstandard launch setup (a dedicated image, preparation scripts, or specific arguments), put it in a user inference template preset; for one-off settings, use container overrides.
  The image catalog is no longer a third source.
- The manifest `sha256` is calculated from the parsed structure, so removing fields invalidates checksums of old manifests. The built-in seed checksums have been recalculated;
  published online catalogs and offline packages must be regenerated with the new `gpuworks-image-manifest`. Old files without `sha256` import as usual, ignoring removed keys.
- Image details no longer show "Runtime Defaults". To inspect the image entrypoint before choosing `image-entry`, run an inspection from image details against a node holding the image:
  the result is the OCI configuration read at that moment, displayed without saving. For instances that have run, you can also view launch facts. Old catalog values could already differ from the artifact.
- For layer sources and precedence, see [Deployment Launch Parameter Sources](../reference/deployment-launch-sources.md).

<!-- translation-source-sha256: 3f997ea401cb8b78c44f162a9ed0b3b5b52f440ddd832662b021af1aa16ee701 # ADR 0031: The Image Catalog Carries No Launch Configuration; All Deployments Use the Same Launch Semantics -->
<!-- translation-source-sha256: e9561d4004f84f1cead8845efba24f2c0752cbab6de5fd8388a1cc3e1d6fd631 ## Context -->
<!-- translation-source-sha256: ccdf7c7e95b61e70904c77bfe996e979724be1f63809c6d1fde0fb59d0a74136 ## Decision -->
<!-- translation-source-sha256: 1aca455b4a0343d5e4847a0dfbfa9046d97e80007185556d840954e9cb18a5a7 ## Consequences -->
