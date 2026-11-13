# Sync Models and Images Between Nodes

This guide explains how to copy existing models or inference images and check the sync results.

**Use cases**: A complete copy already exists and you want to avoid downloading it again over the public internet, or you need to troubleshoot `NO_STORAGE_PATH`, failed transfer connection tests, or tasks stuck at `download_required`. The manual copying described here is not restricted by sync groups.

**Prerequisites**:

- The source holds a complete copy (a local model is `present` or `registered`, a model on shared storage is `present`, or an image already exists in the runtime). Target model storage is writable and applicable to the execution node.
- **Transfer between nodes**: Both source and target nodes are [online](connect-nodes.md) and share a storage-sync network, or both allow the management network to carry storage-sync traffic. See [Configure Node Networks](configure-networks.md).
- **Copy a model on the same node**: The execution node is online and can read the source storage and write to the target storage. No inter-node storage-sync network is required.
- Automatic copying occurs only between nodes in the same group. For grouping and rate limits, see [Configure Image Pulls](configure-pull-settings.md#同步组).
- For model registration and other acquisition methods, see [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md).

When a replica starts, a model or image that is missing locally but available on an online node in the same group can be **copied automatically**. If copying fails, the replica falls back to pulling or downloading. **Manual copying** below uses the source node you confirm. If that source becomes unavailable, it does not silently switch nodes or use the public internet.

## Sync a Model or Image

### Models

1. Click **Copy to…** in the corresponding row on the **Models** page. Under **Source**, confirm the local storage and node holding a complete copy. Both completed downloads and complete copies registered from existing paths can be used.
2. Select writable target local storage and a target node. Enter a rate limit in MiB/s if needed. Leave it blank or enter 0 for no additional limit; relay remains subject to the platform cap below.
3. Click **Start copy**. This dialog has no separate **Test transfer connection** step; copying tries a direct connection first. View progress for the target node in model details and confirm that its status is `present` when complete.

**Copy to…** also supports shared storage as a source or target. For a shared target, you do not specify a target node; the platform selects an execution node. If one node can read and write both locations, it copies directly on that node. Otherwise, it transfers between nodes. Do not assume all shared-storage copies use a direct connection between two nodes.

Same-node I/O does not traverse peer or server relay; shared filesystems such as NFS may still use a storage network.

### Images

1. Start a sync from the target node row in image details and confirm the source node.
2. Click **Test transfer connection**. A successful direct or relay probe enables sync, and the success message identifies the path. The test verifies only the TLS and token handshake; it does not transfer image files or create a sync task record. The actual sync performs a new handshake.
3. Enter a rate limit if needed, then start the sync. View progress in the target node row and confirm that the image is imported and shown as available.

Each copy has an independent rate limit. The API field `rate_limit_kib_s` uses KiB/s. Each target has its own rate limit. Active rate limits are cleared when tasks end; failed records retain the last limit for retries.

## Which Network Is Used

The server selects the network for each node pair according to the rules in [Configure Node Networks](configure-networks.md):

- A shared storage-sync network takes priority. Otherwise, the management network is used if both nodes allow it to carry storage-sync traffic.
- The target receives only on its address on that network. The source initiates the connection from its own address on the same network and verifies the one-time certificate fingerprint.
- If the nodes have no usable shared network, `NO_STORAGE_PATH` is reported. Automatic copying skips that source and the replica pulls or downloads the resource itself. Manually selecting that source returns the error. For images, use **Test transfer connection** first; for models, rely on the copy task's connection result.
- The selected network is tried directly first. Only when all candidate addresses fail to establish TCP connections may the management server relay the transfer. Both nodes must allow **storage sync on the management network**, support relay, and have an available independent data connection. TLS pinning, token, read/write, or verification failures do not trigger fallback, and transport never switches during a transfer.
- For direct-only operation, disable the management network's **storage sync** permission on either node and configure a shared dedicated storage-sync network. This also denies relay over the management network, preserving network isolation.
- With **agent-initiated connections**, the agent opens another outbound connection to the same server address. With **server-initiated connections**, the server dials the same fixed agent listening port again. Both directions and mixed pairs need no additional relay port mapping; server-initiated agents do not need outbound access to the server. Upgrade both agents and attach independent data lanes. Older or single-lane agents remain direct-only; the control connection never carries the archive.
- Relay does not spool to disk. It permits one active relay per node and two per server process. The default **aggregate relay budget is 64 MiB/s**, shared by concurrent transfers; a lower operator or sync-group limit still applies. This bounds encrypted forwarding inside the server, not physical NIC wire speed.
- During automatic copying, a node pair whose copy ultimately fails enters a 3-hour cooldown in server memory. No further copy attempts are made during that period. Restarting the server clears the cooldown.

## Relay Resource Budgets and Observations

Relay capacity is temporary management-server pressure. Tasks queue for a relay slot in arrival order, waiting up to one minute at a time while the phase shows "Waiting for relay capacity"; a task still not admitted retries with its selected source and rate and keeps its place in the queue. A task whose nodes are busy does not hold back tasks between other nodes. Relay capacity alone does not trigger the three-hour node-pair cooldown or public download. Cancellation stops the current server coordinator and both endpoints; an old attempt cannot restart the transfer or overwrite a later retry.

By default, forwarding stops and releases its allowance after **two minutes without progress**. Time spent waiting for the server's own bandwidth allowance is excluded. The default **five-minute** confirmation budget begins only after the source direction has actually drained through the server. The aggregate rate and both timeouts are machine settings for this management server; see the [server configuration reference](../reference/configuration.md). Restart after changing them.

Expand a model or image node row to see the observed transport, phase, fallback reason, and relay allowance. The server rate uses an explicit recent window and is shown separately from the target's average receive rate measured since its receiver started. Older agents, a server restart, or a completed attempt may have no current runtime observation. Missing observation means unknown, not transfer failure.

Server counters measure inner TLS stream bytes toward the target and source, including handshake and protocol overhead. They are not verified file bytes and do not include all outer yamux, WebSocket, or TCP overhead. Do not count the server receiving and forwarding the same data as two model copies. File-manifest verification or the target runtime's import result still determines resource readiness.

Read this process's effective policy, active relays, bandwidth waiting, and cumulative results through authenticated `GET /api/v1/peer-transfers`; filter by `transfer_id` for one attempt. Lifecycle events enter the log center and correlate by transfer ID; aggregate samples reuse existing monitoring. Runtime observations do not reconstruct history across server restarts, and transfer IDs, paths, and node pairs are not unbounded time-series labels.

## Sync Images Across Runtimes

Docker, nerdctl, and runveil nodes can act as either sources or targets for image sync. runveil exports an archive containing an OCI layout plus `manifest.json` (the same structure as Docker 29 `docker save`). Layers are sent as their original compressed bytes from the registry, so the manifest digest matches at both ends. runveil can send only complete images in its own image store; images registered by the older udocker CLI must be pulled or imported again.

## Retry and Recovery

- If the source node is offline or its copy is incomplete, the task fails or remains at `download_required`. Retry after restoring the original source. To change sources, select a complete copy again and start a new copy task. Retries reuse the source node and last rate limit in the failed record. The backend does not automatically retry ordinary failed tasks with manually selected sources; queueing for temporary relay capacity is the exception.
- You can cancel a model sync from the target node row. The cancellation request stops receiving. If the page reports that stopping is unconfirmed, handle that task first; do not register partial files as a complete copy. Canceling image sync stops receiving and runtime import on the target (`docker load` on Docker). This is not equivalent to resuming a model download.
- When the server or node reconnects during image sync, the original transfer is reused if it has made progress within the last two minutes. Otherwise, the transfer restarts from scratch using the original source node and rate limit.
- After a public-internet download fails, you can still manually sync between nodes if an online node holds a complete copy. The resource does not remain permanently stuck in a failed state.

For inter-node transfers, the target agent provides a one-time TLS receiver. Both direct and relayed transfers retain end-to-end TLS 1.3, pinned certificate fingerprints, one-time tokens, and byte-count/SHA256 acknowledgement. A relaying server forwards encrypted bytes only. Relay has no resume support: interrupted transfers must retry the whole task from the beginning.

The image dialog identifies the path used by its connection probe; actual sync selects the path again. For model transfers, check the server sync log's `transport=direct` or `transport=relay` field. TCP internal tests on network pages always measure real node-to-node direct connectivity. A successful relay does not mean the TCP test now passes. This feature transfers model and image files only; it does not provide NCCL/Gloo, RDMA, or distributed-inference network connectivity.

 After a model transfer completes, the file manifest is verified again. An image sync is complete only after the target runtime finishes importing it and confirms that the image exists.
