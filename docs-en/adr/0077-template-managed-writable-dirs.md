# ADR 0077: Managed Writable Directories: Templates Declare `dirs`, One Mechanism with the KV Disk Tier

Date: 2026-11-19 · Status: Accepted

## Background

Some recipes need a writable host directory at start: the session store and spill area of multi-node inference, a cache derived at first start and reused afterwards (engine compile caches, snapshot directories, todo T59), logs. The directory must still be there after instances stop, restart or a multi-node group is re-formed, must be cleaned when the deployment is deleted, and its size must count in scheduling. Until now templates could not declare mounts (`docker_args` refuses `-v`), a deployment's `extra_mounts` needs an operator to type host paths, and the container's writable layer is lost when the instance is recreated. The platform already had one managed writable host directory, the KV disk tier (`MountPurposeKVDisk`), but it is a cache serving only vLLM's disk tier.

## Decision

1. **One concept: the managed writable directory**. A directory the agent creates under its runtime mount root and mounts read-write as the runtime mount owner, for which the server states only the container path and a ceiling in bytes (`SizeHint`), never a host path, and which scheduling reserves by its ceiling on one disk. There are two kinds, by lifetime:

   | | A template's `dirs` | The KV disk tier |
   |---|---|---|
   | Declared by | The inference template spec | Deployment parameters (`shared_kv_dir`, `kv_disk_limit_gib`) |
   | Mount purpose | `managed-dir` | `kv-disk` |
   | Host directory | `<root>/managed-dir/<deployment id>-<creation second>/<name>` | `<root>/kv-disk/<deployment id>` |
   | Kept | Until the deployment is deleted (`retain: deployment`) | Until the last instance leaves the node |
   | Content | State, never pruned | Cache, pruned to its ceiling by the sweeper |
   | Ceiling | `limit_bytes` | The stated value, else twice the weights |

   Both share `RuntimeMountManager`'s directory preparation and filling (`setManagedMountSource`), and are admitted together (`deploy.ManagedDiskBytes`).

2. **Spec field `dirs`**: `[{name, mount_path, limit_bytes, retain}]`, at most 8 entries. Sizes are bytes, as the spec's `weight_bytes`, `shm_bytes` and component `weight_bytes` are (GiB appears only in the deployment API's `vram_limit_gib` and `kv_disk_limit_gib`, never in the spec); the container path is `mount_path`, as `components[].mount_path` is. `name` is a short lower-case label, and `retain` is for now only `deployment` (written back on import when omitted). One named type, `deploy.ManagedDir`, runs through the YAML/JSON, the `inference_specs.dirs` column (a JSON snapshot read and written whole) and the deployment's catalog selection snapshot; a snapshot without the key declares none.
3. **Launch contract**: each entry becomes a `MountPurposeManagedDir` mount carrying `ManagedDirRef{group, name}`; the group is `<deployment id>-<deployment creation second>` (`proto.ManagedDirGroupKey`). Every runtime (docker, nerdctl, runveil, Kubernetes, host processes) treats it as an ordinary mount. Instances of one deployment on one node share one copy.
4. **Release is told by the server**: once the deployment row is deleted the server sends `StateUpdate.ReleasedDirDeployments` to every connected agent announcing `mount.managed_dirs`. The agent lists the groups it holds in its hello (`Hello.ManagedDirGroups`), and the server's replay answers with those whose deployment no longer exists or whose same-id deployment was created at another time (`DesiredState.ReleasedDirGroups`). The agent removes only entries named like a group key that no desired instance mounts.
5. **Admission**: the node reports the free space of the disk holding managed directories (`PathUsage` kind `managed_dirs`) and names the other reported paths on the same filesystem (`shares_disk_with`). Scheduling requires free space ≥ the sum of the `dirs` ceilings + the KV disk tier's ceiling + the disk reserve (the storage settings' reserve, else `DefaultReserveFreeBytes`), plus the weights to download and the image to unpack when they share the disk; otherwise it refuses with `insufficient_managed_dir_disk`. A node that does not report the disk is not checked.
6. **T59's cache directory is a `dirs` entry**: an engine compile cache or a TensorFold snapshot directory is written as `dirs: [{name: compile-cache, mount_path: /cache, limit_bytes: …}]`, with the launch arguments or `env` pointing at that path. It therefore survives instance recreation instead of staying in the container's writable layer and going with the container, as T59 first had it.
7. **No new launch variable**: the directory's container path is the declared `mount_path`, which the recipe writes directly; a recipe that needs one subdirectory per rank builds it with the existing `${GPUWORKS_NODE_RANK}`. Should the path ever be exposed (say `${GPUWORKS_DIR_<NAME>}`), it follows the `${GPUWORKS_*}` launch-variable form and expansion rules.

## Division of Responsibility

`dirs`, the weights, components, assets, the KV disk tier and the deployment's `extra_mounts` each own one kind of in-container fact: the read-only weights and assets live with the model or the template version, the writable `dirs` with the deployment, the KV disk tier with the instances on the node, and `extra_mounts` are not managed by the platform. The one complete table (launch variables, system-layer environment and platform files included) is in [the catalog syntax reference's "What the Template Declares and What the Platform Provides"](../reference/inference-catalog-spec.md#模板与平台的分工); this ADR does not keep a second copy.

- **One container path space**: `dirs` has no path rule of its own. The template's own container paths (the model mount, component `mount_path`s, asset `unpack_to`s, dir `mount_path`s) are checked against each other on import by one overlap rule (`proto.ContainerPathsOverlap`), and a conflict is `CATALOG_MOUNT_PATH_CONFLICT`; at launch resolution every mount (`extra_mounts`, the KV disk tier and compute network files included) is checked again, where one target is `MOUNT_TARGET_DUPLICATE` and nesting is `MOUNT_TARGET_NESTED`.
- **Older agents are refused by capability**: only the node's agent can create a managed directory, so a launch that uses one goes only to nodes that announce `mount.managed_dirs`; planning tells from the resolved launch which agent-side facilities it uses, and an older agent's node is refused with `agent_capability_missing` (parameters `capability`, `node`), the deployment showing why and waiting. It is the one rule every agent-side facility shares, instead of an older agent getting a mount with no host source and failing to start.

## Trade-offs

- **The KV disk tier is not rewritten as a `dirs` entry**: switching it on and sizing it belong to the deployment (operators turn it on per deployment; its ceiling defaults to a function of the weights), not to the template; and it is a cache, pruned and removed once no instance is on the node, while `dirs` is state kept across instances until the deployment is deleted. One spec field for both would need three switches on `dirs` (prune, keep per node, size from weights) and template authors would have to know how the KV cache works. Instead it is one mechanism with two lifetimes, a shared code path, and the differences spelled out in the table. The existing KV disk directory stays where it is, so no data moves on nodes.
- **The KV disk tier's ceiling enters the managed directory disk's admission**: both live under the same runtime mount root, and counting only one would let the other fill the disk. The cost is one more check for deployments with the KV disk tier on nodes that report `managed_dirs`.
- **The node knows which groups it holds; the server does not record it**: "which nodes hold a deployment's directory" cannot be derived from instances, and a table of it would be a new persistent fact to reconcile against the nodes' disks. Instead nodes report in their hello and the server answers, and a deletion is broadcast once to every connected node (the message is small and deleting a deployment is rare). The cost is that a group whose broadcast was missed waits for that node's next connection.
- **The group key carries the creation time**: SQLite's autoincrement keys are not reused within one database, but a reinstalled server or a restored backup brings back the same ids; with the id alone an old deployment's state would be handed to a new one. The creation second tells them apart and comes straight from the deployment row, with no new column.
- **`retain` has only `deployment`**: `instance` (removed with the instance) is the container's writable layer, and `node` (across deployments) has no owner; the field is reserved, not implemented.
- **No quota, no usage measurement**: the KV disk uses the deployment id as the XFS project id, so managed directories using it would share that quota, and a separate id range needs allocation. Measuring each directory means walking it periodically as the KV sweeper does, which is not cheap for hundreds of GiB of state. The ceiling is for admission only for now.
- **Admission does not discount what is already used**: rescheduling onto a node that already has the directory reserves the full amount again; the data written is already out of the free space, so this errs on the conservative side.
- **Same-disk detection is the agent's**: only the agent can compare device numbers; the server reads only `shares_disk_with` and, when the disk is the models root's, charges the whole weights (even if the deployment's storage is elsewhere), preferring to over-count.

## Consequences

- New column `inference_specs.dirs` (migration 231), `dirs` on the template DTO and in the deployment snapshot, and `purpose: managed-dir` among the evaluation preview's mounts.
- Protocol: `MountSpec.managed_dir`, `Hello.managed_dir_groups`, `DesiredState.released_dir_groups`, `StateUpdate.released_dir_deployments`, `PathUsage.shares_disk_with`, agent capability `mount.managed_dirs`, failure code `MANAGED_DIR_PREPARE_FAILED`, scheduling reasons `insufficient_managed_dir_disk` and `agent_capability_missing`, import error code `CATALOG_MOUNT_PATH_CONFLICT`.
- An older agent's node takes no launch that declares `dirs`; it fits there once the node's agent is upgraded.
- Syntax and behavior: [Inference Catalog model.yaml Syntax Reference](../reference/inference-catalog-spec.md#dirs-managed-writable-directories).

<!-- translation-source-sha256: 12c202b0daac2bec7c82f6d21e948cbfb31434ce76a32dc77354284f237d149f # ADR 0077: Managed Writable Directories: Templates Declare `dirs`, One Mechanism with the KV Disk Tier -->
<!-- translation-source-sha256: b797f4dfb83ef6f6d5166c87602ebb216c20a73e097b132b727382b203fbf7cd ## Background -->
<!-- translation-source-sha256: 774a0c72605812b95a32734c6a654901236509d2515ebc2bce799271f83c6cf5 ## Decision -->
<!-- translation-source-sha256: 3b56d96a64b725f33c7a461ab9bfb434fdbe8e7dfda9f91281ac2e137ef33b3a ## Division of Responsibility -->
<!-- translation-source-sha256: 9b4a8d297978bd211c80f2ace60a5e4307df41ad70f41ff937875c82a0f46524 ## Trade-offs -->
<!-- translation-source-sha256: bf5e1644f0b47a6b15fe783b8a1413adf6413df3d046699e2300666dbfd80c60 ## Consequences -->
