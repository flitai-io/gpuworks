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
  `vllm-dtk/`, `sglang-dtk/`, `vllm-hggc/`, `sglang-hggc/`, and `vllm-rocm/`; the System One decision model image is in `systemone-cuda/`.
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

**Images**: After copying the archive to the node, open the corresponding image series and version on the Images page, then open “Node Distribution.” In the target node's “Acquisition Method,” select “Import from node file…”. Enter the absolute path of the tar file on that node, start the import, and wait for the status to become “Present.” The original archive is kept. Directories, paths on the management server, and archives unsupported by the node's runtime fail with an error. Both the server and the target agent must support this feature.

For Kubernetes nodes, enter the file path on the worker host, such as `/data/runner.tar`. Both the file check and the load run in the host environment; the file does not need to be placed inside the Agent Pod or the shared state directory. The image is loaded into containerd's `k8s.io` namespace used by kubelet.

An administrator can expand a present node in Node Distribution, select **Replace from node file**, enter the new archive’s absolute path, and confirm. You can enter a new target tag when all reported tags resolve to the same image ID. If the archive contains different images, choose an existing tag. A transfer already active for that image on the node continues.

For a new image, open **Custom → Add Custom Image** with **Load from a compute node file** selected by default. Select the online node that holds the archive and enter its absolute path. Click **Resolve** next to the path to read the tagged image reference from the archive on that node. The form also fills in the inference engine, engine version, framework, framework version, and host architecture when the image configuration declares them. Fill in any missing fields. Select a reference if there are several, or enter one manually if there are no tags. For a Docker archive containing a single untagged image, the node tags the image ID returned by this load with the reference you entered. Choosing **Replace from node file** moves the existing tag to the newly loaded image. The node verifies the ID before reporting **Present**. For an uncompressed tar, the node skips the image layers to reach the metadata. A gzip tar is read sequentially. If the one-minute preview times out, enter the reference manually. Confirm the reference, engine, framework, and framework version, then submit. The console registers the image and opens its **Node distribution** tab. If the catalog already records the image as present on that node, the console tells you the node already has it. Choose **Replace from node file** to overwrite it. Otherwise, follow the load progress in the tab. The path applies only to this import. If loading fails, you can retry from the registered version’s node distribution.

**Register an image already on a node.** In **Custom → Add Custom Image**, choose **Import an existing image from a compute node** and select an online node. Pick a tagged image from the runtime inventory, review its engine, framework, versions and architecture, complete any required fields, and click **Add Image**. Docker registers the selected local image directly. With nerdctl, select the source namespace. Standalone nodes deploy from `gpuworks` and Kubernetes nodes deploy from kubelet’s `k8s.io`. An identical image in the target namespace can be registered immediately. When a copy is needed, the platform registers the version and the node copies its platform image in the background. Open **Node distribution** to see bytes copied or cancel the transfer. Closing the page leaves the task running. The node reports **Present** once it verifies the image. Select the node image again to retry after failure or cancellation. If the same tag already identifies a different image in the deployment namespace, the import reports a conflict and preserves the existing image.

While a transfer for an image version and target node is active, concurrent node-file imports, URL imports, automatic pulls, or peer copies do not start duplicate transfers. You can submit a new request after cancellation; a late failure from an older attempt cannot overwrite the new one.

Alternatively, log in to the node and import the archive into its container runtime:

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

2. Open the image on the Images page. In the node table row for the target node, select “Import from an HTTP directory…” under “Acquisition Method” and enter
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

To use system image and inference template catalogs newer than those embedded in the server, download the file at `image_catalog_v4.path` in the signed index (use `<channel>/manifest.json` for a schema 3 server)
and the `catalog/templates/<sha256>.tar.gz` listed in `<channel>/index.json` from any release source (such as `https://dl.gpuworks.ai`) on the online machine. Under “Version and updates” on the Settings page, click “Upload manifest” and “Upload catalog package.”
For the rules, see [Image Catalog](../reference/image-families.md#更新目录). In an offline environment, “Check for updates” returns 502 `UPDATE_SOURCES_UNAVAILABLE` because release sources cannot be reached; the database is unchanged.

## Offline Boundaries

The server does not store image tar archives or accept direct uploads of them. Control plane relay in the console reads an archive from an HTTP source and streams it to the node.
Configuring offline sources does not disconnect the operating system from the network. To verify that no public internet access occurs, also apply network isolation or inspect network traffic.

## Related Guides

- Model directories or image archives already hosted on a private HTTP file server: [Import Models and Images from an HTTP File Server](import-from-http.md)
- Downloads, caching, and how “already present locally” is determined: [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md)

<!-- translation-source-sha256: 7919d685c05ac862a992259f34d8a875a1ec110abb944791eaabf349d4f276c7 # Deploy in an Offline Environment -->
<!-- translation-source-sha256: a239010cf7d775f6e9b9ac961c6b83b0b614e123c86830f48c435e52f676cf53 ## Recommended Workflow -->
<!-- translation-source-sha256: b73a6b1917b36b6ac1f3667bab82f7fb64e922cb53a346e618e78743234a2546 ### 1. Install the Control Plane Node -->
<!-- translation-source-sha256: 479cebd2adf6224f7b3ef765f14f5c36e50e01ec8142e85e196bea49078b19c0 ### 2. Add Compute Nodes -->
<!-- translation-source-sha256: d5a507dc03a999b52b01f6a0f5e67d339550bc4e740e6c33c044e58e0567a7a0 ### 3. Download Images on the Online Machine -->
<!-- translation-source-sha256: f5911ccb32c4812b04dce043ce7a574f4831e5dc771f26458f47a49fbb5dca82 ### 4. Download Models on the Online Machine -->
<!-- translation-source-sha256: fc3e04d244b9b9fe161e824f6780977fd455d35e628d4e08ab84d6229df6b4ba ### 5. Import Images and Models -->
<!-- translation-source-sha256: a99d6cc4a2377156c262eb7133b43c5a6e04ab8e4d1161c8f328a229e6dc240a ### 6. Deploy and Verify -->
<!-- translation-source-sha256: ffff45d50019870668450a1fbbc2a8a11bb2c93e6bef48feb5268da6f34fdfbc ## Special Cases -->
<!-- translation-source-sha256: 24fe2bb1fa6636955140d33d56ff60c68948d9df3f9737800a278ce792b81bec ### Store Models on NFS -->
<!-- translation-source-sha256: 1b90763075bd7d71d68f40040f66f17faada339096386b4d1535817ba22c32d4 ### Export Images with Docker -->
<!-- translation-source-sha256: 71a2348752be498cb92304d8a21b732d304dd933bed416d7e3e7b932bc984a59 ### Import Images Through the Control Plane Node -->
<!-- translation-source-sha256: 01c2500d06faeca3f771e63b3c6d219a45a3970c388302c1c419ceb3798f6497 ### Use a Private Registry -->
<!-- translation-source-sha256: cfc41378f115c732b9e67007f3b56c6af9a83e5d12b84b399e6a59ae976fc753 ### Agent Image for Kubernetes Clusters -->
<!-- translation-source-sha256: 5a0156a93daf2f305c4f30d02801b85d638ef8e72de98fef0c84c9b4cf9b8510 ### Store Models in a Private HTTP Directory -->
<!-- translation-source-sha256: 0cd309cbfe4a33a2dd395170ea964de04c682be0f2a9567d291f3c7cbc706569 ### Nodes Cannot Access the Public Internet -->
<!-- translation-source-sha256: 2b52ee21896a93065c94e0d039ec0f784fb9abc4ba4a9d37c828086bfd5e0aa9 ### Immediately Report Why an Image or Model Is Missing -->
<!-- translation-source-sha256: f1840816e6877d35174f99444fc5d1139580728faad52f2f4b4003b4a56dd314 ### Update the Image Catalog and Inference Templates -->
<!-- translation-source-sha256: a3a925dcbbad8133b709314d36a4cc3ba4a8464bfa8cd5a3e7664bc959ffb4d4 ## Offline Boundaries -->
<!-- translation-source-sha256: 98465786e340902f4386309b8d140a002538e2be6f7cd40ac285d8992d70a38d ## Related Guides -->
<!-- translation-localized-code-block: c717c0459f55443287bf882bfa1df2c29a02f63e677ed105a6b9f0cfa648769b fb1f692b5059c1a518bf7610bc04b414fbac8c14c412eb611037ff1567010f7c Translate Chinese code comments, preserving executable statements, parameter values, and paths. -->
<!-- translation-localized-code-block: 02989a499c64fa46ffdb61c1d644c92435ac76da6d226fe734a210745b3e8a0a 0f5116535f0bd45f3e3eff27d33b8cf064e261a7dab15ef84c529acd212a8ccf Translate Chinese code comments, preserving executable statements, parameter values, and paths. -->
<!-- translation-localized-code-block: e0baf1bbf17d7510b11d7a5aa8e9e820fbea43761101fa63db5b2afc691e9ac2 053400b1c9f5dbf83f869d2d57b5498e3a982ac5ffa9457381f740b0c01dd791 Translate Chinese code comments, preserving executable statements, parameter values, and paths. -->
