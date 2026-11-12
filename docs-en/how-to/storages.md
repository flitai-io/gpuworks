# Use Storage: Local Directories, NFS, and NFS With Local Cache

This guide explains how to add model storage, import an existing model library, select storage for a deployment, and manage local caches.

Prepare the paths before adding storage. Before a trial mount, scan, or deployment, connect an [online node](connect-nodes.md) that can access the storage. Existing model directories must be readable by the agent, and download or copy destinations must be writable. Adding, editing, scanning and importing, and cleaning up storage require administrator permissions. Choose the storage type based on where files reside and how they are read:

Storage paths and cache directories can be any absolute path; they do not need to be within `storage_probe_roots` in `server.yaml`. A path cannot be `/` or contain `.` or `..` segments or control characters, and cannot be under a denied directory. By default, `/etc`, `/proc`, `/sys`, `/dev`, `/boot`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/usr`, `/run`, `/var/run`, `/var/lib/docker`, `/var/lib/containerd`, and their subdirectories are denied. A host administrator replaces the whole denied list with `storage_denied_paths` in `server.yaml`; it takes effect after a server restart and cannot be changed through the management API or the settings page. See the [configuration reference](../reference/configuration.md#server-配置文件-serveryaml). When a node mounts or probes storage, the path must also be a canonical directory that does not lead elsewhere through symbolic links. Creation, editing, or a trial mount that breaks these rules is rejected: a malformed path with `STORAGE_PATH_INVALID`, a path under a denied directory with `STORAGE_PATH_DENIED`, both naming the refused path; when a saved path later falls under a denied directory, edits that leave the path unchanged can still be saved.

| Type | Where model files reside | How nodes read them | Suitable for |
|---|---|---|---|
| Local storage | A separate directory on each node | Direct reads; each node has its own copy | Small node counts or no dependency on network storage |
| NFS | An NFS export mounted by the agent | Direct reads after mounting; all nodes share one copy | Fast links, many models, and small local disks |
| NFS with local cache | An NFS export | Copy to the node's cache directory first, then read from the cache | Slow links, long-running services, and workloads that must tolerate NFS disconnections |
| Mounted shared directory | A directory already mounted on the node (CephFS, Lustre, or an NFS mount you manage) | Direct reads; the agent does not mount it | Non-root agents or shared file systems other than NFS |

A new platform installation includes local storage named `default`, with `/opt/gpuworks/models` as its default directory. You can rename it, change its path, or delete it.

To add other local storage or a **Mounted shared directory**, choose the corresponding type under **Storage → Add storage**, then enter the directory and applicable nodes. You can save local storage directly. For a **Mounted shared directory**, first click **Trial mount**, confirm that the directory is readable and the trial succeeds, and then save. After saving, check availability on the nodes. You maintain mounts for pre-mounted directories; the agent neither mounts nor unmounts them.

Local storage requires a directory. **NFS with local cache** uses `/opt/gpuworks/cache/<storage-name>` when no cache directory is specified. When a non-root agent first connects, the default local storage records a `models` path under its writable state directory for that agent. You can view and change this path in the storage details and edit page; it is not automatically overwritten afterward. If a node reports that its directory is unavailable, set a directory for that node under the storage's **Applies to** settings. You can also override individual nodes when all nodes are selected. Directories do not change with the agent's installation location, and upgrades from older versions retain the locations previously used by each node. See [ADR 0034](../adr/0034-model-files-belong-to-storages.md) for the design.

## Add NFS Storage

For agent-managed NFS mounts, the agent on the node must run as root, have an NFS client installed (`nfs-common` or `nfs-utils`), and be able to reach the export address. For non-root agents or mounts you manage yourself, use a **Mounted shared directory**, or select **Use an existing NFS mount** for the corresponding nodes.

1. Open the **Storage** page, click **Add storage**, and select **NFS** or **NFS with local cache**.
2. Enter the export address (`host:/path`). The default mount point is `/mnt/gpuworks/<storage-name>` and is the same on all nodes. The default mount options are `rw,noatime`.
   The kernel negotiates the remaining options; only allow-listed options are accepted.
3. Choose which nodes use this storage: all nodes, by label (new nodes with the label mount it automatically), or one by one. When selecting nodes one by one, you can set a different mount point for a node,
   or select **Use an existing NFS mount** so the agent neither mounts nor unmounts it. For **NFS with local cache**, also fill in the **Cache configuration**: cache directory, default per-node limit, and node overrides. Then review the confirmation page. The limit is unlimited by default; it is neither a disk reservation nor a cluster-wide quota.
4. Click **Trial mount** and check the read and write results. For agent-managed mounts, the agent temporarily mounts the storage on one node, reads the root directory, tests writing under `.gpuworks/`, and then unmounts it. With **Use an existing NFS mount**, it checks the existing directory directly. If it fails, check the client, network, permissions, or export path according to the reason. A successful trial on one node does not mean all nodes can use the storage.
5. After saving, check **N/M online nodes ready** on the storage card, and inspect and address any nodes with problems. Nodes needed for deployment should be ready.

If the trial mount succeeds but writing fails (for example, because of `root_squash`), the storage is saved as **Read-only (disabled)**:
you can scan and import models and deploy from it, but you cannot download into it or delete from it.

## Import an Existing Model Library

If the NFS already contains models organized as `<organization>/<repository>/`:

1. Click **Scan and import** on the storage card. A node with a working mount traverses the directories (up to 4 levels deep by default), parses common formats and manifests, and lists model entries. After finding a model, it still checks subdirectories to avoid missing other entries within the repository.
2. Check the model format, task type, identity basis, and reasons for any problems. Select only complete, importable model entries; components and adapters cannot serve as base models. Add missing shards, repair corrupted metadata, or reorganize directories that are too deep or contain multiple models, then scan again.
3. Select the entries and click **Import selected**. The platform checks the directories again, matches existing registrations, or creates new records. Importing only registers files; it neither writes files to the storage nor moves directories.
4. On the **Models** page, confirm each model's identity, storage, and availability, then select a matching inference template. Recognition as a model entry only confirms that its directory structure meets the rules; it does not guarantee support by every engine.

Scan again after the library changes. New directories are marked **New**; imported directories that disappear are marked missing and blocked from new starts. Full recognition requires a newer agent. Older agents use a compatible method of reading text metadata.

### Recognized Formats, Task Types, and Import Boundaries

#### Check Formats and File Completeness

- Recognized formats include Transformers safetensors/bin (including indexed shards), Sentence Transformers, Diffusers pipelines, ModelScope standard configurations and weights, native Mistral format, and GGUF.
- ONNX currently requires `config.json` and `model.onnx` in the same directory. SavedModel requires `saved_model.pb` and `variables/variables.index`.
- GGUF distinguishes models from mmproj/controlvector components and groups shards that use standard names. Different quantizations in separate subdirectories are listed separately. Multiple independent models in one directory prompt you to import from separate directories; they are not combined into one model.

  Without an association manifest, runtime dependencies between root-directory components and quantized entries are not inferred automatically.

Independent subdirectories within a repository are registered as local models using their actual relative paths. They are not automatically associated with subdirectory artifacts from remote downloads that retain the repository hierarchy. Pipeline components such as `audio_vae` and encoders are not listed as standalone models.

Composite models are checked against each component's file requirements:

- For Sentence Transformers, Transformer and Dense components require configuration and weight checks; Pooling and LogitScore require only configuration checks; Normalize does not require a separate directory.
- For Diffusers, model components require configuration, weights, and indexed shards; schedulers require configuration; common CLIP/T5 tokenizers require vocabulary files.

#### Check Tasks and API Capabilities

API capabilities for new models come from structured task declarations: `text-ranking` maps to rerank, while `feature-extraction` and `sentence-similarity` map to embeddings. Model cards support a single task or a task list. ModelScope's `configuration.json` and Sentence Transformers modules also provide task evidence.

When determining tasks, note the following precedence and outcomes:

- Declarations take precedence over the underlying architecture. Directory names and descriptive prose are not used as type rules. Source precedence follows AIOS: explicit model card declarations come first; task-specific modules and model card tags take precedence over ModelScope's generic task configuration; the underlying architecture is used only as a fallback.
- Conflicting evidence at the same priority leaves capabilities empty and displays the reason. Unknown types no longer default to chat.
- Local directory registration uses the same parser. Existing models retain their API capabilities; rescanning, reimporting, and refreshing do not automatically rewrite old records. This release does not add a way to manually override recognition rules.

#### Determine Why an Entry Cannot Be Imported

Both single-model registration and scan-and-import reject confirmed component directories, directories containing multiple models, and recognized entries whose files are missing or cannot be verified. Both paths require metadata to identify exactly one complete model entry in the selected directory; parent directories, non-model directories, and unknown layouts are rejected.

Do not treat an entry as verified complete when either of these messages appears:

- **Component file layout could not be verified**: unknown custom components are not marked complete just because their directories exist. Current weight checks cover standard default filenames and shards; layouts that require extra variant parameters are not inferred automatically.
- **Recognition is incomplete**: the read limit was reached. This is not evidence of corrupted files or missing shards. Large weight indexes are parsed in chunks, retaining only referenced shard paths; GGUF supports reading larger tokenizer metadata.

## Select Storage for a Deployment

The **Model weights** section of the deployment form lists storage options. A deployment does not select where to acquire files; nodes decide according to [pull settings](configure-pull-settings.md):

- **Already on storage X**: one option for each storage that already contains this model and is accessible to the target node. The local storage option replaces the previous **Already present locally** option.
  NFS shows **Read directly**; NFS with local cache shows how much must be cached first. If the mount is unavailable on the target node, the option is disabled and the reason is shown.
- **Save to**: the storage where the node saves a model it needs to acquire, whether by copying within a sync group or downloading. The default storage is preselected; copies are saved only to local storage.

The selected storage only determines where files are saved when acquisition is needed; it does not bind the deployment to that storage. Each node's replica subsequently reads the copy it can access:

- If the node already has the files on its own disk, either in local storage or in an existing NFS local cache, it uses them.
- Otherwise, if the files are on NFS mounted by the node, it reads them directly.
- Otherwise, if the files are in NFS with local cache storage, it first copies them to this node's cache.
- Only when none of these are available does it acquire files, first by copying within the sync group, then by downloading if that fails. It saves them to the storage selected during deployment if that storage applies to this node; otherwise, it uses the default storage, then another enabled local storage. When the deployment selected no **Save to** and a shared storage the node can read is being written with this model by a write this deployment started, the node waits for that copy and reads it instead of fetching another; a write someone else started is not waited for.

The platform does not automatically download into NFS. If you select an NFS storage under **Save to** during deployment but it does not contain the model (because a download failed or the model was deleted), the replica stops and explains why.
It does not switch to downloading a separate copy on each node.

The one exception is an inference template's components, such as a text encoder kept apart from the primary model: when the primary model is already on a shared storage and the deployment selects no **Save to**, creating the deployment downloads each component that some target node cannot read into that same shared storage, written by one node. The nodes wait for it and read it there instead of each downloading to its local disk. The deployment itself still records no **Save to**.

This write is visible before and after deploying: **Component weights** in the deployment form lists how each template component is acquired (ready, written into which shared storage, or downloaded by each node), and a component going into a shared storage shows the storage name, the space it takes and how to avoid it. After the create, the deployment timeline records that the component started downloading into the shared storage, naming the writer node. To avoid the write, limit the storage in the node requirements; when the form shows **Save to**, you can also choose a local storage there. Deleting the deployment does not delete this copy; delete it from the storage's file list when needed.

To restrict a deployment to reading from a specific storage, such as NFS only, add "Storage: name" to the deployment's node requirements. Nodes outside that storage's scope are excluded.
Nodes within its scope whose mounts are currently unavailable remain candidates, with the reason shown in the placement details.

After the deployment starts, check the instance details for the storage and host path actually used. **Save to** determines the destination for acquired files; the **storage node requirement** restricts which storage can be read. They serve different purposes.

## Cache Preparation Progress and Recovery

For models on NFS with local cache, instances do not start until the local copy is complete. Check the preparation summary on the Inference Services page. To investigate, open the details and inspect the source-checking, copying, verification, installation, or cache-eviction stage by node, main model and components, and selected storage. Copied bytes update even while an individual weight file is still incomplete. No percentage is shown when the total size is unknown, and estimated time remaining appears only after the rate stabilizes.

Deployment cache filling has no rate limit. For a manual copy from the Models page, you can set a rate limit (MiB/s) for that copy; leaving it empty or setting it to 0 means unlimited. Retrying a failed copy retains its rate limit; subsequent new copies do not inherit it.

Two consecutive minutes without actual progress trigger a no-progress warning. Deliberate waits for rate limiting do not count as blocking. This warning appears separately from "NFS unresponsive" reported by storage health checks, node offline status, and insufficient disk space. If observations have not updated for more than 15 seconds, the node disconnects, or the server restarts, the display returns to waiting for an update instead of retaining a stale rate.

If an older agent lacks complete stage reporting and safe-stop confirmation, follow the [upgrade guide](upgrade-and-compatibility.md) to upgrade the server first, then the agent. Waiting for an update does not confirm an NFS failure.

After a failure, first check capacity, node status, and NFS status, address the problem, and retry in the details. The platform must confirm that the old task has exited before starting a new copy. If the file system remains blocked, repeated clicks do not start a second copy. Partial files resume only when the paths, sizes, and modification times match the source manifest; this does not guarantee full-file checksum verification.

Stopping a deployment withdraws its reference to cache preparation. Copies still needed by other deployments continue. When the last reference is withdrawn, that cache copy is canceled and partial files are retained. Blocked I/O continues to occupy the task slot until it exits. Completed caches remain available for later use.

## Local Cache Capacity

**NFS with local cache** storage has a cache limit calculated per node, with per-node overrides and no limit by default. The platform also reserves some free space on cache disks. Configure it on the **Storage** page; the default is the smaller of 10% of the disk and 50 GiB.

When caching a new model on a node would exceed available space or the limit, the platform evicts caches in order of least recent use. It only evicts caches that no deployment or instance uses, that are not being copied, and that still have authoritative files on NFS. If a single model exceeds the cache limit, it is rejected with a reason; the platform does not silently switch to direct NFS reads.

### Default Policy and Node Overrides

To change cache configuration, open the **Nodes** tab in the storage details and first check the default directory, default limit, and effective configuration of each node. Before lowering a limit, inspect cached, reclaimable, and protected data.

**Before changing a directory**, also confirm the following:

- The change does not move old files. Old files do not count toward the new directory's budget and are not deleted by background cleanup for the new directory.
- If capacity for the new directory has not been reported, the node is offline, or statistics are missing, the capacity impact is unknown.

1. Edit the default policy, or select **Follow default**, **Custom**, or **No limit** for a node. Custom limits support GiB/TiB; unlimited caches remain subject to reserved disk space. The default limit affects only nodes that follow it, and restoring limit inheritance does not reset the directory.

   Node scope and configuration overrides are separate. Setting an override for a node selected by label does not permanently add it to the scope. If the node loses the label, its override remains as **Outside scope** and takes effect again when the node matches. Nodes explicitly selected in the past remain scope members. When changing scope, check existing combinations of labels and individual node selections.

2. For batch changes, filter by name, address, configuration source, or cleanup status, select a fixed set of nodes, and fill in the fields to change. Saving changes only the fields explicitly selected.
3. After saving, check the effective configuration. If a conflict indicates that another administrator has changed the configuration, reopen the latest configuration before editing again.

If refreshing the details fails, existing data remains visible with a warning that it may be out of date; click retry. Polling does not change drafts you are editing.

### Check Cleanup Results and Clear Caches

After saving, check the cleanup status. Cleanup reclaims only copies that are safe to remove. If some data is temporarily protected, it first reclaims the safe portion and shows states such as pending, cleaning, blocked, or failed. Offline nodes wait to recover. For failed cleanup, address the cause and retry.

To reclaim space manually, click **Clear cache** on the storage card to evict all eligible caches. To reclaim only eligible data on specific nodes, select those nodes in the details and clear their caches. This does not guarantee removal of data that is in use, being transferred, or lacks an authoritative copy.

### Switch between NFS and NFS with local cache

**More → Switch to …** in the storage details switches only between these two types; the files on the NFS stay. Switching from **NFS with local cache** to **NFS** first removes the caches on each node, as **Clear cache** does, and changes the type only once they are all gone: after the switch the nodes no longer manage the cache directories, so anything left there could not be removed any more. While any node cache remains the switch is refused, saying how many entries remain, on how many nodes, and how many are still copying or downloading. The usual causes are an offline node, a model in use by an instance, a copy or download still running, or a model kept only in a node cache with no authoritative copy on the NFS; deal with those and switch again. The cache directory and limit settings are kept and apply again if you switch back to **NFS with local cache**.

### Handle Blocked Cleanup and Pending Deletion Confirmation

**Cache is full of models in use**: if the disk has enough space but in-use models fill the cache limit, deployment events state this reason. Raise the limit or stop the deployments that use those models.

**Deletion pending confirmation**: after cache deletion times out or fails, model files remain in this state and cannot be used for deployment or copied again. Retrying cleanup or clearing reuses the original deletion identifier, confirms that deletion first, and then replans using the latest capacity.

Until confirmation, deleting the model, node, or storage, or changing related directories or scope, is rejected. Increasing the cache limit can still be saved. If an older agent does not support deletion confirmation, upgrade the agent first; pending records are retained.

The `.gpuworks-evictions/` directory in the agent's state directory holds deletion receipts. They prevent disconnections, restarts, or delayed retries from deleting models cached later. Retain these receipts with the state directory; do not remove them as ordinary cache. Deletion fails if the file system does not support directory synchronization or the state directory is not writable.

## Share One NFS Across Multiple gpuworks Installations

Each gpuworks installation adds storage and scans and imports models independently. On writable storage, each control plane writes a heartbeat file under `.gpuworks/control-planes/`,
and the storage card shows which other gpuworks installations use it. When deleting model files from shared storage, if another gpuworks installation has been seen within 5 minutes (each refreshes its heartbeat once a minute), the platform displays a warning and requires you to select
**Force delete**. Heartbeats are warnings, not locks. After a forced deletion, other control planes mark the model missing on their next scan, and instances reading directly from NFS fail when restarted.
Agree on one gpuworks installation to write to a library; configure the others as read-only or use **NFS with local cache**.

## Browse Files in Storage

Administrators can browse the entire storage in its details, download any file or directory, and place files in two areas ([ADR 0053](../adr/0053-storage-files.md)):

- **There are only two writable areas**: `files/` (a general-purpose area for offline datasets, tokenizers, prompt collections, and similar files) and `.gpuworks/apps/` (application data directories).
  Uploading, creating directories, renaming, and deleting are allowed only beneath these two directories. The `files` and `.gpuworks/apps` directories themselves cannot be deleted or renamed; if absent, they are created automatically on the first write.
  Model file directories and the rest of `.gpuworks/` are read-only. Model files are still changed only through model management. Disabling storage makes the entire storage read-only, but browsing and downloading remain available.
- **Nodes**: local storage has a separate directory on each node, so select a node before browsing. For shared storage (NFS, NFS with local cache, or shared directories), the platform selects a node that is online,
  ready, and connected through the data channel to perform the operation. Browsing NFS with local cache shows the NFS itself, not the node cache. The agent on the node must be upgraded to a version that supports storage files.
- **Uploads**: each file is sent in one request. The browser streams it as it reads, and the platform does not write it to its own disk. You can select **Unpack after upload** (tar, tar.gz, zip). Absolute paths, `..`,
  symbolic links, hard links, and device files are rejected, and the total extracted size and entry count are limited. Files are first written to a temporary location under `.gpuworks/incoming/`. Only after they are fully received and extracted are they atomically renamed to the destination.
  A failure or cancellation midway does not leave a partial file at the destination. **Resumable chunked uploads are not supported**: after an interruption, upload the entire file again.
- **Large-file uploads through reverse proxies such as Cloudflare are not supported**: these proxies limit request body size (100 MB on Cloudflare Free and Pro plans). Uploads exceeding the limit are rejected directly by the proxy.
  To upload a large file, connect directly to the gpuworks address, or place the file on a node first and copy it into storage from there.
- **Downloads**: files are downloaded as-is. Directories are archived while streaming, using `.zip` by default or `.tar.gz` if selected, with the directory name as the archive's top-level directory. Symbolic links are not followed; skipped links are listed in
  `SKIPPED_SYMLINKS.txt` inside the archive. The total size of a directory download is not known in advance, so the browser shows only the amount downloaded. If an error occurs midway, the browser only shows "Download failed".
- **Deletion** first moves files to the storage's trash directory, then deletes the bytes in the background, just as with model file deletion.

## Disable Storage

You can disable any storage. Disabled means read-only: model files can no longer be downloaded, copied into it, or deleted, including downloads triggered by deployment. Existing model files can still be deployed as usual.
For **NFS with local cache**, disabling protects the NFS model library. Node caches can still be filled, cleared, and trimmed according to their capacity policy. This also applies to manually disabled storage; caches that are in use, being transferred, or lack an NFS source copy remain protected.
Images are not stored in storage; they reside in each node's container runtime. Disabling storage does not affect image synchronization.

## Notes

- **NFS disconnection**: with a hard mount, instances that have finished loading weights can usually continue serving. Restarts and L2 wake-ups must read the weights again and may block or fail.
  Use **NFS with local cache** for deployments that must not depend on NFS.
- **Pre-start check**: before creating a container, the agent checks whether the model directory exists, with a 2-second time limit. If it does not exist, startup fails with `MODEL_PATH_UNAVAILABLE`;
  the engine is not started against an empty directory.
- **The node model root directory** is now part of local storage. To switch disks, change the local storage path or add new local storage; it is no longer configured separately on the node.
