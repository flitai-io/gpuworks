# Download, Cache, and Sync Models Between Nodes

This guide explains how to register a model, download it to a selected storage, or adopt files already on a node so that the model is available for deployment.

**Use case**: Obtain models from Hugging Face or ModelScope, register an offline directory, or reuse a model already on another node. For HTTP directory sources, see [Import from an HTTP Directory](import-from-http.md); for copying between nodes, see [Sync Models and Images](peer-sync-models.md).

**Prerequisites**:

- When downloading, registering a node path, or copying between nodes, the nodes involved are online. See [Connect Nodes](connect-nodes.md). Registering only a public model source does not require an existing target node.
- Public downloads: the node can access the selected source (Hugging Face or ModelScope), or you have configured a proxy and site rules (mirror sites or internal proxies) for it under **Settings → Image & model pulls**. See [Configure Image and Model Pulls](configure-pull-settings.md).
- Peer sync: at least one online node holds a complete copy, and both endpoints can connect directly (automatic copying also requires them to be in the same sync group). For all prerequisites, see [Sync Models and Images Between Nodes](peer-sync-models.md).

## Register and Download a Model

1. Open **Models**, click **Register model**, select Hugging Face or ModelScope, and enter the **Source ID**, such as `Qwen/Qwen3-8B`.
2. To specify a version, enter the **Revision** under advanced options. If left blank, the platform default branch is resolved first; when a commit is obtained, the model is pinned to it. ModelScope's `main` is normalized to `master`. Click **Confirm registration** and check the source and resolved revision in the list.
3. Click **Download to…** in the model row and select a writable storage. For local storage, also select online target nodes; each node downloads its own copy. For shared storage, the model is downloaded only once. Select one online node with write access and access to the model source to perform the download. To change the node for an active download, cancel it first, then select a new node and restart the download.
4. Click **Download** and view progress for the target storage or node in the model details. The target copy is usable as a complete downloaded copy only after its status becomes `present`. If a download fails, check the reason before retrying.

When you register a Hugging Face / ModelScope model, a metadata read failure may produce only a notice; registration does not mean the weights have been downloaded. **Download to…** explicitly downloads from the registered source. To use complete files on another node, use **Copy to…**.

In the model details, click **Cancel download** on a storage or node row to stop an active public download. The state becomes `canceled`; downloaded data is kept for a resumable retry. Deleting or stopping an inference service cancels downloads that service started when no other active service needs them. Downloads started separately from the models page continue. If the writer is offline, cancellation is saved and applied on reconnect. Changing a shared download writer requires the previous writer to acknowledge that it has stopped writing.

Downloads support resume, multiple connections, and rate limiting. A copy is reported as `present` only when all files exist, their sizes match, and the directory structure is valid.
There is no task-level download deadline. Each HTTP request lasts at most 15 minutes. After a timeout, the download attempts to resume from the downloaded portion; retry counts and address-switching conditions are described below.

Downloads first try replacement addresses from applicable site rules, then the original sites allowed by those rules. When retrieving the file list, an unreachable address, an HTTP error, or an unparseable list puts the current address into a 30-minute cooldown and triggers a switch to the next candidate.

When downloading individual files, failures are handled by type:

- **Retry at the same address**: Retryable transport errors, such as connection refusal or reset, TLS handshake timeout, and EOF, are retried at the original address first. If more candidates remain, each file gets at most 6 attempts at that address; the final address gets at most 16 attempts. Both counts include the first attempt. Only after attempts are exhausted does the address enter cooldown and the download switch; if no next candidate exists, the download ends.
- **Switch immediately**: Nonretryable source errors, such as HTTP status errors (including 404, 401/403, and 429) or TLS certificate verification failures, are not retried at the original address. That address immediately enters a 30-minute cooldown, and the download switches to the next candidate.
- **Repeated stalls**: If no response headers or new data arrive for 5 consecutive minutes, the request is cut off and the download attempts to resume. If this happens twice consecutively at the same address and more candidates remain, the address enters cooldown and the download switches. At the final address, no further switch occurs, and the limit of 16 total attempts still applies.
- **Resume after switching addresses**: If the revision is pinned to a commit, downloads can resume after switching addresses; branch-based revisions restart from the beginning when the address changes. If the source does not support Range, resumed downloads also restart from the beginning.

Progress shows the current download site and includes the reason when switching addresses.

For the full rules, see [How Nodes Try Candidates in Order](configure-pull-settings.md#节点怎样依次尝试).

HTTP directory sources download only from their own directories and are not rewritten by site rules, but they use the node proxy.

## Register an Existing Directory on a Node

First choose the action based on whether the model is already registered:

| Situation | Action |
|---|---|
| The platform does not yet have this model | Select **Register model** → **Local path on node**, choose a node, enter the directory, click **Parse preview**, and register after confirming the model information |
| A model source is already registered, and you want to add a local copy | Select **Register an existing path on a node** from that model's actions, choose a node, and enter the directory; do not create a separate, unrelated model |

Enter an absolute directory path on the node host. If there are multiple local storages, select the storage that owns the path. The path can be outside the storage root, and registration neither copies nor moves files. After completion, confirm the node, path, and availability in the model details. A `registered` copy is already usable; you do not need to wait for it to become `present`.

If deployment evaluation reports insufficient model disk space, check the named node, storage, target directory, and required/available space. If those weights already exist on that node, follow the link to the current model's details, select **Register an existing path on a node**, and evaluate again after registration. The platform uses registered copies to decide how much additional space is needed; other unregistered directories do not automatically become usable copies. Register the model the deployment actually uses so a separate model with the same name does not leave the deployment pointing to the original source.

When a model copy runs out of disk space during download or transfer, its details retain the directory and capacity information from the original error. Insufficient container runtime disk space requires attention to the filesystem holding images and containers; registering a model directory does not resolve that shortage.

The node checks that the directory is readable, contains nonempty files, and has no incomplete downloads. Registration fails if the directory is missing, empty, or contains unfinished `.part` files; any existing registration remains unchanged. A `.part` / `.part.state` beside a file that is complete (at the size `.part.state` records) is a leftover and does not block registration. Directories identified as components or collections of multiple models, and model entries with missing or unverifiable files, are also rejected. The selected directory must identify exactly one complete model entry through its metadata; parent directories, non-model directories, and unknown layouts are rejected. “Registered” does not guarantee that the selected engine can load the model. For the recognition scope, see [Import an Existing Model Library](storages.md#导入已有的模型库); for requests and status codes, see the [API Contract](../reference/api.md#登记节点已有路径).

Registering a path also fills in missing model metadata (config, README, and size) from that directory without overwriting existing metadata. If the directory was downloaded with `huggingface-cli download --local-dir`, all `.cache/huggingface/download/*.metadata` files point to the same commit, and this is the model's only copy, the model is pinned to that commit. This is the offline import workflow; see [Import Models Offline](configure-pull-settings.md#离线导入模型).

A model registered from a local or HTTP directory takes its source ID from the directory name by default, which may not match the repository ID an inference template declares (such as `Qwen/Qwen3-0.6B`), so the template does not recognize it. Click edit next to **Source ID** in the model details and set it to the repository ID. The source ID of a Hugging Face / ModelScope model is the repository itself and cannot be changed; register it again if needed. Existing copies keep their paths, and copies made or downloaded later are placed in directories named after the new source ID. The change is refused while a copy, download, or removal of the model is in progress, or when another model already uses that source ID. The API is `PATCH /api/v1/models/{id}`.

## How Deployments Obtain Models and Images

Confirm two settings when deploying:

- **Weight source**—When the template declares multiple sources, choose whether to run Hugging Face or ModelScope weights. The template's recommended provider is preselected.
- **Save to**—Choose which storage the node saves the model to when it needs to fetch it. Storage that already holds the model is shown as already having it and does not download it again. For the difference between shared storage and restricting which storage can be read, see [Select Storage for Deployment](storages.md#部署时选存储).

Each node decides how to obtain the model when a replica starts:

- If a complete copy exists locally or in shared storage accessible to the node, use it directly;
- If an online node in the same [sync group](configure-pull-settings.md#同步组) holds a copy, copy it from that node;
- Otherwise, download from the selected source. Download addresses are affected by [site rules and node proxies](configure-pull-settings.md); when rules forbid all candidates, the deployment's scheduling reason is `weights_fetch_forbidden`, and the download API returns 409 `WEIGHTS_FETCH_FORBIDDEN`.

Before automatic scheduling selects a node, the page shows “Images and models are fetched automatically according to pull settings after scheduling”. After automatic scheduling selects nodes, or when nodes are specified for manual placement or multi-node deployment, the page shows an estimate for each node: already present, which node to copy from, and the order in which to pull images or download models. This estimate reflects the current state; the node evaluates it again at startup.

“Already local” is determined by model identity rather than download source: the same repo and revision, or different revisions across sources with identical weight
bytes, architecture, and context length.

Images follow the same decision process: use a local image directly; copy from another online node in the sync group if it has the image; otherwise, pull candidates in order. Candidates come from equivalent copies of the image
(the template's own `image_sources`, sources registered in the image catalog, and the image's own address) and source rules applicable to the node. See
[Configure Image Pulls](configure-pull-settings.md). For template-declared sources, see
[Template Syntax](../reference/inference-catalog-spec.md#image_sources模板声明的镜像来源); using them does not count as deviating from the template.
Cached images no longer count toward the scheduler's runtime disk estimate.

## Peer Sync

On the Models page, use **Copy to…** to select a complete source copy, target storage, and nodes, then start copying. In the image sync page, first run **Test direct connection**. Neither manual operation is restricted by sync groups, and each can have its own rate limit. For transfers between nodes, the source and target connect directly; the server does not relay files. For copies involving shared storage, the platform selects a node that can read the source and write to the target.

After a public download fails, you can still transfer a complete copy from an online node. For the respective operations, network requirements, and recovery steps, see [Sync Models and Images Between Nodes](peer-sync-models.md).

Failed peer sync operations can be retried, and ongoing sync operations can be canceled. Do not register partial files as a complete copy. Deleting a node-local copy requires administrator privileges. The API is
`DELETE /api/v1/models/{id}/nodes/{uuid}`; a model used by an active deployment returns 409.
