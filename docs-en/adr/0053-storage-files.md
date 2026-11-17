# ADR 0053: Storage Files—Browse All Storage, Write to Only Two Areas, and Transfer Through the Data Channel for the Agent to Save

Date: 2026-10-21 · Status: Accepted

## Background

Storage ([ADR 0034](0034-model-files-belong-to-storages.md)) previously held only model files, with no platform entry point to upload or download arbitrary files. Offline environments need to place evaluation datasets, tokenizers, and prompt collections on storage, and results produced by applications running on nodes (see the plan at `artifacts/work/2026-10-21-apps/plan.md`) also need to be downloaded and retained. This requirement is independent of any specific application, so it becomes a general capability of storage itself rather than a dedicated upload API for a particular application.

## Decisions

1. **Browse everything, write in two areas**: administrators can browse and download any file in storage. Uploading, creating directories, deleting, and renaming are allowed only within `files/` (the general-purpose area) and `.gpuworks/apps/` (application data); the area directories themselves cannot be deleted or renamed. Model file directories and the rest of `.gpuworks/` are read-only, and model files are still modified only through model management. Area definitions, path normalization, and writability checks have a single implementation: `proto.CleanStorageRel` and `proto.StorageRelWritable`, shared by server prechecks and agent execution.
2. **The agent is the only party that touches the filesystem**: the server selects a node and performs checks that can be made in advance (storage disabled, area, node online, data channel connected, and agent capability `storage.files`), then sends bytes to the agent over the data channel. The agent resolves paths relative to the storage root, resolves symbolic links, and then checks that the path remains within the root, rejecting paths that escape it. Directory listing uses lstat and does not follow symbolic links.
3. **Stage uploads first and report only after success**: uploads are written to `.gpuworks/incoming/<control-plane-id>/upload-<id>`. If needed, extraction occurs in staging (tar / tar.gz / zip, rejecting absolute paths, `..`, links, and device files, and limiting the total extracted size and entry count). On completion, an atomic rename moves the result to the destination, and only then does the agent return one result line (`StorageFilesStatus`). Staging is cleaned up on failure. The server treats this line as the sole confirmation of success.
4. **Stream each upload in one request, without resumable chunks**: the browser sends one `PUT`; the server does not save it to disk but forwards it as it arrives through the `storage.upload` stream. Large file transfers through reverse proxies with request body limits, such as Cloudflare, are unsupported.
5. **Package downloads while streaming**: files are transferred as-is; directories are packaged as a stream in the requested format (zip by default, optionally tar.gz), without following symbolic links. The agent first returns one `StorageDownloadHead` line and sends no bytes if an error occurs. Directory downloads have no known length in advance.
6. **Use ctrl RPC for short operations**: `storage.files.list/mkdir/rename/delete`. Deletion first renames the item into the storage's trash directory, then removes the bytes in the background, just as for model file deletion. Rejection reasons are returned using stable codes from `proto.StorageFilesError`, which the server maps to ProductError.
7. **Add no persistence**: files are simply files on storage; the platform keeps no records of them.

## Trade-offs

- Browsing includes model directories and platform metadata: administrators can already see these files on nodes, so read-only browsing does not expand privileges. Restricting writes to two areas prevents manual changes from damaging directories managed by the platform.
- Single-request uploads are simple to implement. The cost is that interrupted large uploads must restart, and transfers through Cloudflare are subject to request body limits.
- Downloads use direct browser links with cookie authentication: the browser streams directly to disk without holding the content in memory. The trade-off is that when a transfer fails midway, the browser shows only a download failure, without the product error message.
- Symbolic link checks resolve paths before operating, leaving a very short race window between validation and the operation. The threat model covers administrator actions and links that may already exist in storage, rather than an adversary concurrently writing malicious links there, so this is accepted.

<!-- translation-source-sha256: e4f8686b5b2d2bff38ad5d4af11cf5756628070b60c7639009f81543b5b81f42 -->
