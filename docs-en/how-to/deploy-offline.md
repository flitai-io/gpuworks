# Deploy in an Offline Environment

**When to use this guide**: Your cluster cannot access the public internet: it cannot pull from registries such as Docker Hub or download models from Hugging Face / ModelScope.

**Prerequisites**: A machine with internet access (called the “online machine” below), a way to copy downloaded files into the private network, such as a USB drive or jump host, and administrator privileges. You do not need to import directories or node copies that already exist again.

## Recommended Workflow

### 1. Install the Control Plane Node

Choose the release package for your control plane server: `gpuworks-<release_id>-linux-amd64.tar.gz` for Linux x86 servers, or
`gpuworks-<release_id>-darwin-arm64.tar.gz` for Apple silicon Macs. Download it from a release source on the online machine, copy it to the server, extract it, and run `./gpuworks install`.
See [Install and Upgrade the Control Plane](install-server.md). The server embeds the system image and inference template catalogs, so you can use them directly offline.

### 2. Add Compute Nodes

Click “Add node” on the Compute Nodes page and follow the wizard to install the agent. Nodes download the agent from the server and do not need internet access. See [Connect Nodes](connect-nodes.md).

By default, nodes use local storage named `default`, with models stored under `/opt/gpuworks/models` on each node. If this disk cannot hold the models, edit
`default` on the Storage page and change it to a directory on a data disk. If individual nodes use different directories, set them separately under “Applicable nodes.” See [Use Storage](storages.md).

### 3. Download Images on the Online Machine

First determine which images you need: under “System Images” on the Images page, find the version for your engine (vLLM, SGLang, and so on) and accelerator, and note its tag, such as
`cuda13.0-sglang0.5.21-r2` (the full reference is `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2`).

Then download the corresponding archive from Aliyun Drive. You do not need Docker, and on poor networks this is more reliable than pulling from Docker Hub:

<https://bj20013.apps.aliyunfile.com/disk/s/pweW8krgZfw?domainId=bj20013>

- Directories are organized by engine and framework: `vllm-cuda/`, `sglang-cuda/`, `vllm-omni-cuda/`, `vllm-cann/`, `sglang-cann/`, `vllm-omni-cann/`,
  `vllm-dtk/`, `sglang-dtk/`, `vllm-hggc/`, `sglang-hggc/`, and `vllm-rocm/`.
- Filenames use `gpuworks-runner_<tag>_linux-<architecture>.tar.gz`. Select `amd64` or `arm64` for the compute node's CPU architecture (most Ascend servers use `arm64`).
  In Ascend filenames, `910b`, `a3`, `310p`, and `950` identify chip models.
- Archives already contain canonical image references, so no retagging is needed after import. Aliyun Drive usually contains only the current version of each series. For other versions, see [Export Images with Docker](#用-docker-自己导出镜像).

### 4. Download Models on the Online Machine

Use the official CLI to download the complete directory, including `config.json`, the tokenizer, and all weight shards:

```bash
# Hugging Face (in China, optionally set HF_ENDPOINT=https://hf-mirror.com)
huggingface-cli download Qwen/Qwen3-8B --local-dir Qwen3-8B
# ModelScope
modelscope download --model Qwen/Qwen3-8B --local_dir Qwen3-8B
```

Directories downloaded by `huggingface-cli` contain `.cache/huggingface/`. Keep it unchanged so the imported model can be pinned to the corresponding commit.

### 5. Import Images and Models

Copy the files directly to one compute node if possible:

**Images**: After copying the archive to the node, import it into the node's container runtime:

```bash
# Docker nodes
docker load -i gpuworks-runner_<tag>_linux-amd64.tar.gz
# nerdctl nodes (common on Ascend): decompress first and use the gpuworks namespace so the platform can find the image
gunzip -c gpuworks-runner_<tag>_linux-arm64.tar.gz | nerdctl -n gpuworks load
```

After about one minute, the corresponding node on the Images page shows “Present.” On udocker (runveil) nodes, import with the agent's own command, with `<image-ref>` set to the full image reference shown on the Images page:

```bash
sudo gpuworks-agent runveil load -i gpuworks-runner_<tag>_linux-amd64.tar.gz <image-ref>
```

When logging in to the node is inconvenient, you can also [import through the control plane node](#经管理节点中转导入镜像).

**Models**: Copy the model directory into the node's local storage directory, such as `/opt/gpuworks/models/Qwen/Qwen3-8B`. Then, on the Models page, click “Register model” → select
“Local path on node” as the source → select the node and enter the directory → preview and register. The platform reads the architecture, precision, and context length from `config.json`. Local storage does not support batch scanning;
register each model individually. Registration fails if the directory contains incomplete download files such as `.part` files.

**You do not need to copy files to the other nodes manually.** Use the UI to copy them from a node that already has them:

- Automatic copy at deployment: If a node in the same [sync group](configure-pull-settings.md#同步组) already has the image or model, replicas copy it from that node automatically at startup. A newly installed platform puts all nodes in one group by default.
- Manual copy in advance: On the Models page, use “Copy to…” in the corresponding row. On the image details page, select “Sync from another node…” under “Acquisition Method” in the node row. See [Peer Sync](peer-sync-models.md).

Peer copying requires direct network connectivity between the two nodes. After import or sync completes, verify that the target node's image status is `present` and the model copy is available. Do not rely only on transfer progress reaching 100%.

### 6. Deploy and Verify

[Deploy the model](deploy-models.md) as usual, choosing an image and template that match the model and node architecture. Images and models already on the node are used directly without downloading them again.
Once the instance is ready, use the deployment's call example to send a request through the actual inference API and confirm the result. Accessing only the `/v1` prefix does not verify successful inference.

## Special Cases

### Store Models on NFS

For many models and nodes, place models on NFS using the `<organization>/<repository>/` layout. All nodes share one copy, so there is no need to copy files to each node:

1. On the Storage page, click “Add Storage,” select “NFS,” enter the export address, and click “Trial mount.”
2. Click “Scan and import” on the storage card, select models, then click “Import selected” to register a batch at once.

See [Import an Existing Model Library](storages.md#导入已有的模型库).

### Export Images with Docker

If the version you need is absent from Aliyun Drive, or the online machine can access the registry reliably, use Docker (or nerdctl) on the online machine to pull and export it yourself:

```bash
REF=gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2
docker pull --platform linux/amd64 "$REF"      # Use linux/arm64 for arm64 nodes
docker save "$REF" | gzip > gpuworks-runner_cuda13.0-sglang0.5.21-r2_linux-amd64.tar.gz
```

If access to Docker Hub is slow in China, pull from an image replica such as SWR, then use `docker tag` to restore the canonical reference above before running `docker save`. The platform identifies images by their canonical references.
If the archive contains only the replica address tag, the platform does not recognize the imported image.

### Import Images Through the Control Plane Node

For udocker (runveil) nodes, or when logging in to a node is inconvenient, import through the console. The node must remain connected to the server. Even if the node cannot access the archive source, the server can relay it.

Before starting, confirm that the server can read the archive. Ordinary private sources and the local loopback address below need no allowlist by default. An allowlist is needed only in strict mode. Platform administrators can authorize the source under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes to either field apply to the next request after saving, without restarting the server. See [HTTP Import Prerequisites](import-from-http.md).

1. Place the archive at an HTTP URL accessible to the control plane node. The simplest approach is to copy it to that node and start a temporary file server there:

   ```bash
   cd /data/offline-images && python3 -m http.server 8090
   ```

2. Open the image on the Images page. In the node table row for the target node, select “Import from a URL…” under “Acquisition Method” and enter
   `http://127.0.0.1:8090/<filename>` → click “Resolve” → check the image reference in the archive → select “Relay through the management server” → click “Pull.”

The server streams the download to the node without saving it to disk. Relay transfers cannot resume; an interrupted transfer starts again from the beginning. For images of tens of GB over unreliable links, prefer copying to the node and importing locally.
Docker and udocker nodes can use `.tar.gz` directly. For nerdctl nodes, extract the archive to `.tar` before hosting it. See [Import from an HTTP File Server](import-from-http.md#节点获取方式直连或管理节点中转) for details.

### Use a Private Registry

For many nodes, long-term offline operation, or frequent image updates, sync images into a private registry (such as Harbor). After a one-time configuration, deployments pull automatically just as they do online:

1. Keep the original repository path when syncing images, for example, `registry.corp.example/gpuworks/gpuworks-runner:<tag>`.
2. Under Settings → Image & model pulls → Source rules, add a rule from `gpuworks/` to `registry.corp.example/gpuworks/` and disable “Allow upstream” so nodes no longer pull from the public internet.
   If the registry requires authentication, configure [Credentials](configure-pull-settings.md#凭据). For rule syntax, see [Source Rules](configure-pull-settings.md#来源规则).

### Agent Image for Kubernetes Clusters

For a Kubernetes cluster connected through a DaemonSet, the nodes first need the agent image. The `gpuworks-agent/` folder of the Aliyun Drive share holds
`gpuworks-agent_r1_linux-<arch>.tar.gz` (`r1` is the version of the agent's runtime and does not follow the server's version). Import it either way:

- Push it to the internal registry and enter that address in "Agent image" under "Advanced" of the cluster entry (add an "Image pull secret" when the registry needs a login):

  ```bash
  docker load -i gpuworks-agent_r1_linux-amd64.tar.gz
  docker tag gpuworks/gpuworks-agent:r1 registry.corp.example/gpuworks/gpuworks-agent:r1
  docker push registry.corp.example/gpuworks/gpuworks-agent:r1
  ```

- Or import it into the kubelet's containerd on every node and keep the cluster entry's defaults: the manifest's `gpuworks/gpuworks-agent:r1` is already on the node, so it is not pulled.
  Nodes that join the cluster later need the import first as well.

  ```bash
  gunzip -c gpuworks-agent_r1_linux-amd64.tar.gz | ctr -n k8s.io images import -
  ```

The agent program itself is delivered and upgraded by the server, so ordinary server upgrades need no new import; a new archive is needed only when the runtime version changes (the release notes say so).

### Store Models in a Private HTTP Directory

If the model directory is already on an HTTP file server, register and download it as described in [Register a Model from an HTTP Directory](import-from-http.md#从-http-目录注册模型). The server needs to read the directory to parse it, and the target node must also be able to access it directly. Configure outbound authorization for private network addresses as described in that guide.
Model directory downloads do not support control plane relay. If the node cannot read the directory directly, use local copying, NFS, or peer sync as described above.

### Nodes Cannot Access the Public Internet

This commonly affects cloud instances without a public IP or NAT gateway (such as ECS instances in an Alibaba Cloud VPC): nodes can connect to the server but cannot pull images or download models.
Import files using the workflow above. In the cloud, you can also use services within the same VPC instead of the public internet:

- Images: Use the cloud provider's VPC registry address, such as Alibaba Cloud ACR's `registry-vpc.<region>.aliyuncs.com`, and configure it as described in [Use a Private Registry](#有内网镜像仓).
- Models: Use NAS in the same VPC as [NFS](#模型放在-nfs-上), or use `ossutil` through the internal OSS address `oss-<region>-internal.aliyuncs.com`
  to download models onto the node, then register them (copy the `ossutil` installer to the node first).

### Immediately Report Why an Image or Model Is Missing

If the entire cluster has no internet access, follow [Fully Offline: Two Blocking Rules](configure-pull-settings.md#完全离线两条禁止规则) to add one site rule each for Hugging Face and ModelScope
(no alternative addresses, with origin fallback disabled). HF / ModelScope models whose only acquisition path is downloading and whose downloads are blocked by these two rules immediately report `weights_fetch_forbidden`, without waiting for public downloads to time out.
These site rules do not restrict registries, HTTP directories, or other external access.
For images, the equivalent is a source rule with “Allow upstream” disabled.

### Update the Image Catalog and Inference Templates

To use system image and inference template catalogs newer than those embedded in the server, download `<channel>/manifest.json`
and the `catalog/templates/<sha256>.tar.gz` listed in `<channel>/index.json` from any release source (such as `https://dl.gpuworks.ai`) on the online machine. Under “Version and updates” on the Settings page, click “Upload manifest” and “Upload catalog package.”
For the rules, see [Image Catalog](../reference/image-families.md#更新目录). In an offline environment, “Check for updates” returns 502 `UPDATE_SOURCES_UNAVAILABLE` because release sources cannot be reached; the database is unchanged.

## Offline Boundaries

The server does not store image tar archives or accept direct uploads of them. Control plane relay in the console reads an archive from an HTTP source and streams it to the node.
Configuring offline sources does not disconnect the operating system from the network. To verify that no public internet access occurs, also apply network isolation or inspect network traffic.

## Related Guides

- Model directories or image archives already hosted on a private HTTP file server: [Import Models and Images from an HTTP File Server](import-from-http.md)
- Downloads, caching, and how “already present locally” is determined: [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md)
