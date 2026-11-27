# ADR 0034: Model Files Belong to Storage, With One Row for Model Files on Shared Storage

Date: 2026-10-02 · Status: Accepted

Based on the [Storage Abstraction and NFS Plan](../../artifacts/archive/2026-10-02-shared-storage/plan.md). This amends the acquisition candidates in
[ADR 0018](0018-explicit-deployment-acquisition.md) by adding a storage dimension to "already available locally / peer sync / public download."

## Context

Model files originally "belonged to a node": `model_files` had one row per (model, node), `Path` was a directory on that node, the model root directory was a node property,
and "storage" was merely a built-in download switch. For multiple nodes to read the same model on NFS, each node had to register its own row. N rows pointed to the same files without knowing about each other.
Deleting one row deleted the directory shared by everyone; two nodes downloading into the same directory corrupted each other's writes; and at a thousand nodes, status synchronization required checking the same shared directories on every node.

## Decision

1. **Storage is a first-class, typed object.** Types are `local` (a directory on each node), `nfs` (an NFS export mounted by the agent, with one copy shared by all nodes),
   `nfs_cache` (an NFS export plus a local cache on each node, read from the cache at startup), and `shared_dir` (a shared directory already mounted on the node, with the same behavior as `nfs`).
   Nodes do not trust "the directory is readable": an existing `nfs`/`nfs_cache` mount must be an NFS mount point in mountinfo, and `shared_dir` must be a mount point or reside on a network filesystem.
   Otherwise, report `failed/not_mounted`: the empty directory left after a mount is lost resides on the root disk and must not be written to as shared storage.
   The type combines two concerns: "where the authoritative files reside" and "how a node obtains a local path." Adding S3 with a local cache later requires only a new backend; the cache layer depends only on "list files" and "read files."
   Local storage is no longer built in: a fresh installation automatically adds an ordinary default local storage object; upgrades migrate the former built-in row to it, and each node's former model root directory becomes its node override.
2. **Model files are unique by (model, storage, node).** Local storage has one row per node; authoritative files on shared storage have one row with node 0, and `path` is relative to the storage root.
   A node cache for `nfs_cache` is a row with a node under the same storage. Whether a node can read shared model files is derived from `storage_node_states` (reported by nodes and written only on changes),
   without storing a row per node. Rows with node 0 are not sent with desired state, and agents do not validate shared directories.
3. **One entry point for location queries.** Only `store.ModelLocation` answers "which host path does node N use to read model M from storage S?"
   Shared paths are constructed only in `store.SharedHostPath`. Startup, evaluation, template resolution, disk admission, and tokenizer reads for benchmarking all query it instead of reading `ModelFile.Path` directly.
4. **Each node reads the copy it can access; deployments are not bound to storage.** At deployment creation, the user still explicitly selects the source, and the server only validates it without substitution (ADR 0018):
   "already available locally" becomes "already available on a storage," and peer sync and public download gain a "Save to" option. Afterward, the copy read on each node follows
   the single rule in `store.ModelSourceOn`; scheduling admission, the controller, and previews all query it:
   use a copy the node can read directly (copies on the node's own disk—local storage and already cached `nfs_cache`—take priority over direct NFS reads);
   if a copy exists on NFS with a local cache, fill the cache first; acquire only if no copy exists, saving to the storage recorded on the deployment (`deployments.weight_storage_id`,
   called `SaveToStorageID` in Go; only storage explicitly selected in the request is recorded, or 0 if none is selected; use it when it applies to this node), then the default storage, then another enabled local storage. Shared storage is never written to automatically: if "Save to" selects a shared storage that does not contain the model,
   reject the request with a reason instead of falling back to downloading a separate copy on every node. To "read only from a specific storage," add the node condition `gpuworks.ai/storage` to the deployment
   (a system label whose value is the storage ID; nodes carry all storage IDs applicable to them. Nodes with temporarily unavailable mounts remain candidates, with the reason reported at admission). The platform never adds it automatically.
   At creation, each instance records the model-file row and host path it reads (`instance_model_files`). These records determine "in use" status for cache eviction, model-file deletion,
   storage deletion or relocation, restart checks, and deployment counts on the Storage page. Peer sync saves only to local storage; use download or copy to write to shared storage.
5. **Disabled means read-only.** Disabled storage no longer accepts downloads or copies, or allows model-file deletion (including downloads triggered by deployments); existing files remain deployable.
   Node caches and heartbeats for `nfs_cache` are unaffected. Images reside in the node's container runtime and are outside storage management, so the storage switch no longer affects image synchronization.
6. **One writer node writes to shared storage, staging first and then renaming.** Files are staged under `<root>/.gpuworks/incoming/<control-plane-id>/`
   and renamed to the final path on completion. If another writer has already occupied the target and it is the same model, adopt it; otherwise, fail without overwriting. Do not use NFS file locks.
7. **When multiple gpuworks installations share storage, write heartbeats and warn on deletion.** Each control plane writes a heartbeat to `<root>/.gpuworks/control-planes/<control-plane-id>.json`.
   Freshness is determined by the NFS server's file modification time. Deleting shared model files requires an explicit force option when another control plane is found to be using them. Heartbeats are warnings, not locks.
8. **Every agent call that touches shared storage has a time limit.** A hung hard NFS mount must not block status reporting: health checks, status snapshots, and RPCs use time-limited calls,
   reporting `unresponsive` on timeout. Before creating a container, run a time-limited `stat` on the model path, so docker never creates an empty directory for a missing mount source.
   A directory that does not exist, with the nearest directory above it not empty, fails the start with `MODEL_PATH_MISSING`, and the platform takes back its record of that copy
   (only a copy on the node's own disk, and only while the node reports that storage ready; a shared storage's record is never taken back on one node's word).
   One that does not answer, or is not a directory, fails it with `MODEL_PATH_UNAVAILABLE`. The empty mount point a dropped mount leaves behind does not count as the files being gone.

## Consequences

- Migrations 122–125 migrate built-in storage, node model root directories, existing model files, and deployments into the new structure, and remove `nodes.models_root` and `storages.builtin`.
- Storage directories do not depend on the agent: local storage must have a directory (defaulting to `/opt/gpuworks/models` for fresh installations), and `nfs_cache` uses
  `/opt/gpuworks/cache/<name>` when no cache directory is specified. Set a separate directory for any node that cannot use it. When an agent reports its actual model directory during the handshake (`Hello.own_models_dir`),
  the platform records it as a node-specific setting if that node has none yet: a one-time, visible, editable setting. Revision on 2026-10-04: reporters expand from "non-root agents" to
  "non-root agents plus agents whose state directory is outside `/opt/gpuworks`." If a root agent with a custom `--state` (installed on a data disk in the cloud, or in a CI container) does not report it,
  the first desired-state frame silently moves the model directory to the global default path. This is exactly the "invisible and unmanageable" behavior this ADR seeks to avoid, and it places files on a disk that should be avoided.
  In a standard root layout, the two paths are already identical, so reporting makes no difference and the existing behavior is retained. Migration 126 replaces empty storage directories with explicit paths and records the directories
  previously used by each node as its node-specific settings. Rationale: the model directory is a user decision about data, whereas the agent state directory varies by installation method
  (`/opt/gpuworks`, a custom installation directory, or the user's configuration directory for non-root installations). Letting it determine where models reside makes that decision invisible and unmanageable.
- Protocol: `DesiredState.storages` (storage tasks), `status.storage` (the node's complete storage status, sent on changes); model-file task
  keys become row IDs (`f<id>`, with the old format still accepted) and carry storage, cache, copy source, and staging directory information. Four RPCs are added: `storage.probe/scan/peers/remove`.
- Cache eviction is a pure function (`internal/server/modelcache`) shared by scheduling admission and controller eviction. It evicts in order of least recent use, only for caches that are unreferenced,
  not being transferred, and backed by a shared copy. A single model exceeding the cache limit is rejected instead of switching to direct reads.
- Migration 127 creates `instance_model_files` and backfills it from paths actually mounted in the desired state of existing instances. An earlier version bound deployments to one storage without runtime substitution,
  causing existing local model files on nodes to be ignored while hiding this restriction from node conditions. This is replaced with the rule above; see the
  [design document](../../artifacts/archive/2026-10-02-shared-storage/design-model-source-per-node.md) for the rationale.
- Out of scope: deletion locks across control planes, periodic scans, kernel FS-Cache, docker's NFS volume driver, and direct reads from object storage.

<!-- translation-source-sha256: a8656648b2fd6b7842c14203810a2474e16979c37cbea48b9410780a99a865f6 # ADR 0034: Model Files Belong to Storage, With One Row for Model Files on Shared Storage -->
<!-- translation-source-sha256: 1c102df9476f932dac85e948a07fc96a4d03431ad8e80f6d76b2dd66e7d7b8a6 ## Context -->
<!-- translation-source-sha256: 1cd7378e75416590e15acf5c0b220c54706a6e21c17968c51baf8127a389807d ## Decision -->
<!-- translation-source-sha256: 8bca6e67c02ab67e84c004c79009ddd8ff197ff9f0b3d89e00ba12f98fe51907 ## Consequences -->
