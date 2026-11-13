# Deploy Models and Understand Scheduling Results

This guide explains how to deploy a model as a callable inference service, check placement results, and adjust the node scope after deployment.

**Prerequisites**:

- At least one online node. See [Connect Nodes](connect-nodes.md).
- A complete model copy that the target node can read (fully downloaded or registered), or a model that can be downloaded or copied to the target node. See [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md).
- The required runtime image can be acquired on the target node. For offline environments, see [Deploy in an Offline Environment](deploy-offline.md).

## Steps

### Select a Model and Runtime Scheme

On the deployment page, select a registered model or search for a specific model in the built-in catalog, then select a runtime scheme. Models and schemes are searched separately; schemes with the same name are distinguished by source and name. If the catalog does not contain the combination you need, use your own image and configuration.

Chip filtering in the template list checks vendor, model name, applicable compute capability and architecture ID, and honors catalog unsupported rules. More specific matching recipes come first. A missing or different architecture ID does not pass because the GPU has the same name. Template detail warnings and the default engine use the same rules; the platform evaluates node selection for command previews.

The deployment page's **Chip compatible** recommendations are scoped to the selected node, or to online nodes of the target vendor when no node is pinned. They indicate matching hardware in that scope; they do not guarantee availability of manually assigned GPUs, memory or other deployment constraints. After changing the node or vendor, wait for the new scope's result. The current draft's deployment evaluation remains authoritative.

If the form displays **This deployment will expose no inference API**, first set the required APIs on the **Models** page. If neither the model nor the selected template recipe declares any APIs, the deployment does not appear in the Playground and cannot have health checks enabled. APIs declared by the template recipe are assigned to models with no APIs, including models whose API list is `[]`, whose task was not identified at registration, or whose APIs were subsequently cleared. Models that already have APIs remain unchanged.

### Select a Deployment Shape and Nodes

1. Select the chip vendor and replica shape: single-node, multi-node, or PD disaggregation. For the configuration details of multi-node and PD, see [Multi-Node Inference](multi-node-inference.md) and [Deploy with PD Disaggregation](pd-disaggregation.md).
2. Select a placement mode; all three shapes use the same control:

   | Mode | How nodes are selected |
   |---|---|
   | **Automatic** | Single-node: each time a replica starts, select a candidate node with enough resources; you can further restrict the scope under **Only these nodes**. Multi-node and PD: at creation the platform picks nodes from free cards for the ranks or units the request leaves out (each replica, and each PD deployment, is picked inside one compute network and one GPU architecture), the picked nodes are written into the deployment, and when a unit starts it is planned again against the current free cards and only moves if it does not fit. The page shows the candidate count and the current estimated node and GPU selection for the first replica |
   | **Manual** | Single-node: pin the deployment to one node, with optional GPU indexes. Multi-node and PD: you specify the node of every rank and every P / D unit (and for PD optionally cards) and they are pinned as written; only a lost node is still handled by [node-loss rescheduling](node-failover.md) |

   Existing multi-node and PD deployments are all manual. For the order in which automatic placement picks nodes, the re-pick when a unit starts, and its limits, see [Multi-Node Inference](multi-node-inference.md#自动放置与手动放置) and [Deploy with PD Disaggregation](pd-disaggregation.md#自动放置与手动放置).

3. Set node labels and **Hardware and network** requirements as needed, including GPU model, architecture, compute network, storage sync network, and serving network. Multiple selections within a category require a match to any one option; different requirements must all be satisfied. Manual selection cannot bypass these requirements. Set parallelism, the dGPU memory limit, and the replica count as needed.

**Placement options** decide who comes first among otherwise equal candidates: single-node defaults to Spread, preferring nodes with fewer replicas of this deployment, and you can also select Binpack to fill nodes with fewer remaining resources first. See [Scheduling and GPU Memory Estimation](../explanation/scheduling-and-estimation.md#单机副本落在哪台节点). Multi-node and PD deployments default to Binpack when no strategy is given (automatic or manual): they take a whole group of cards at once, and packing leaves whole empty nodes for the next deployment. For an automatically placed PD deployment under Binpack, if one node can take all its units they all go on that node; with Spread, prefill and decode avoid sharing a node where they can. Node requirements are rechecked before each replica starts. Changes to labels, GPUs, or networks do not automatically move running instances.

### Confirm the Launch Configuration, Image, and Weights

1. Enter common parameters in **Quick Deploy**, or switch to **Expert Deploy** for all parameters. Both views share the same draft; switching does not reset environment variables, mounts, or manual commands. Unchanged controls use defaults. Enter flags outside the dictionary as custom arguments. For the difference between **Unset** and **Clear**, and how conflicts are handled, see [Deployment Launch Configuration Semantics](../explanation/launch-config.md#未设置与清除).
2. Confirm the runtime image. You can enter a private or specialized image address directly without first registering it in the image catalog. Changing the image preserves the current parameters and launch mode. By default, the engine generates the command; you can also explicitly select the image entrypoint or a manual command. For the differences, see [Command Modes](../explanation/launch-config.md#命令模式的三档). A notice appears when image compatibility is unknown; known resource and placement conflicts still need to be resolved.
3. Confirm the weight provider (Hugging Face or ModelScope) and the **Save to** storage. [Pull Settings](configure-pull-settings.md) determine the actual download addresses and peer copying for models and images. If automatic scheduling has not selected a node, the page indicates that acquisition happens after scheduling. Once nodes are specified, review the acquisition estimate for each node. For storage restrictions, see [Select Storage at Deployment](storages.md#部署时选存储).
4. Set **Health check** and the timeout threshold as needed. Health checks are enabled by default for new deployments that support chat, text completion, or embedding APIs. The default threshold is 60 seconds, configurable from 1–600 seconds.

Health checks send a minimal request through the gateway every minute: chat or completion input of “1” with only 1 generated token, or embedding input of “1”. Checks do not restart or remove replicas and are excluded from the gateway's request, token, and latency statistics. The engine's own metrics may still include these requests.

Check results mean the following:

- **Healthy**: Responded within the threshold.
- **Timed out**: Exceeded the threshold.
- **Failed**: Returned an error or could not connect to the replica.
- **No replica**: No available awake replica; no request is sent.

The three types of information displayed side by side in the Inference Services list and details serve different purposes:

- **Status** describes how the deployment is running.
- **Health** is the result of the latest minimal request.
- **Replicas** shows **running / desired** counts. A single health result cannot represent the readiness of all replicas.

### Evaluate and Deploy

1. Wait for evaluation of the current draft to finish. Resolve errors beside fields or at the page level, and review the GPU memory breakdown, token capacity, and scheduling explanation.
2. Check the command, configured environment, preparation scripts, mounts, and container arguments. Multi-node commands are displayed and copied by rank; do not concatenate scripts for multiple ranks into one command. After changing the configuration, wait for the new evaluation result before submitting.

   The configured environment in the command preview contains only variables produced by this configuration, not the container's final process environment. The image catalog does not store launch configuration; deployments start from the template and the current inputs. For the relationship between the configured environment and the image's built-in environment, see [Configured Environment and Image Environment](../explanation/launch-config.md#配置环境与镜像环境).

3. Click deploy, then open the details to observe model preparation, image acquisition, and instance startup status. When waiting for nodes, review the reason and closest matching nodes, or click **View candidate nodes** to inspect each node's constraints.
4. Once the instance is ready, send a request using the call example in the details or the Playground, and confirm that you receive the expected response. If startup fails, use the retained instance and logs to check the image, model files, ports, and runtime, then adjust parameters and retry. See [View Metrics and Search Logs](monitor-and-logs.md).

For parameter sources and SGLang's automatically generated logging environment and preparation scripts, see [Deployment Launch Parameter Sources](../reference/deployment-launch-sources.md) and [SGLang Logging](../explanation/sglang-logging.md), respectively.

**Expose tokenize** is enabled by default in the form. The gateway forwards `/v1/tokenize` and `/v1/detokenize` to the deployment's engine, excluding them from request statistics. You can toggle this in the details at any time without restarting. See [Tokenization API](../reference/openai-gateway.md#分词tokenize).

## Adjust After Deployment

In the service overview, change the desired replica count of a single-node deployment and click **Apply**. When health checks are enabled, enter a threshold from 1 to 600 seconds and click **Save**. You can cancel a pending edit.

- **Edit a deployment**: Click **Edit** in the deployment details. The deploy page opens with the current configuration (at `/deploy?edit=<id>`); the form, VRAM evaluation, and command preview are the same as on create, and the evaluation does not count the GPU memory held by the deployment's own instances. The model, template, engine, vendor, name, and served model name cannot be edited; to change them, click **Clone and modify** to create a new deployment from the current configuration. After you click **Review changes**, the confirmation lists each field's old value, new value, and how it takes effect:
  - Replica count, routing, health check, tokenize, start timeout, node requirements, and similar settings **take effect at once** without restarting instances.
  - Launch parameters, extra arguments, image, container parameters, parallelism, and similar settings **rebuild replicas**. Choose rolling replacement (the default with several replicas and at least one serving), rebuilding all at once (the default with one replica, or while starting or failed; service is interrupted), or save only and apply at the next restart. A stopped deployment is only saved, with an option to start after saving; a sleeping deployment can be woken and rebuilt with the new configuration. Turning dGPU on or off, or changing the partition backend, can only rebuild everything at once; the confirmation warns that service will be interrupted.
  - For a deployment that failed to start, click **Modify parameters and retry** beside the failure message, correct the parameters, and rebuild everything; retry counts start over.

  An edit starts from the configuration the deployment stores: parameters you do not change keep their values, and template changes made after the deployment are not brought in. If someone else changed the deployment after you opened it, saving asks you to reload. See [Edit a Deployment](../reference/api.md#编辑部署).
- **Node scope for single-node deployments**: Click **Edit** beside **Node requirements** in the details to change nodes, labels, or the Spread/Binpack strategy. Saving affects only replicas started afterward; restart an existing replica to place it again. If no node can accommodate the deployment under the new requirements, the change is rejected and the original requirements remain. The vendor determined by the image cannot be changed here. For multi-node and PD deployments, use **Edit replicas** to change rank nodes; an automatically placed one can also switch its placement mode there, see below.
- **Choose nodes when adding single-node replicas**: In **Edit replicas**, select **First placement nodes**, or add `place_on` to `PUT /api/v1/deployments/{id}/replicas`. It decides only where the new replicas start the first time; afterward they behave like any other replica. See [Reschedule Replicas on Node Loss](node-failover.md#单机扩容时指定首次放置节点).
- **Switch the placement mode of multi-node and PD**: `PUT /api/v1/deployments/{id}/placement-mode` with `{"mode": "auto"}` or `{"mode": "manual"}` switches between automatic and manual without restarting instances; units keep their current nodes, and the new mode decides what happens the next time a unit starts. Switching to automatic also drops the cards PD units were pinned to (the scheduler picks them at launch). A single-node deployment returns 400 `PLACEMENT_MODE_SHAPE`, because its placement is in its node requirements. `placement_mode` in the deployment detail shows the current mode.
- **Node-loss rescheduling wait**: Once a node has been offline longer than this (default 10 minutes, 0 for never), its replicas move to other nodes automatically. See [Reschedule Replicas on Node Loss](node-failover.md).
- **Schedules and yield policies**: A sleep-capable deployment can sleep, wake, or change its single-node replica count on chosen weekdays and times, and can share cards with other deployments by yield policy (never yield automatically, yield when idle, yield on a turn, yield anytime). See [Use Scheduling Analysis](scheduling-center.md).
- **Health checks**: Enable, disable, or change the threshold in the details without restarting. The API is `PUT /api/v1/deployments/{id}/health-check`. Deployments that exist before an upgrade have health checks disabled by default.
- **Routing across replicas**: The default is `least_request`. Use `PUT /api/v1/deployments/{id}/routing` with request `{"routing_policy": "..."}` to change it to `prefix_affinity` or `failover`. The routing policy is not an engine launch parameter; changing it does not restart instances.

## Multiple Deployments for the Same Model Name

All replicas in a deployment have the same shape: tensor parallelism, GPU count per replica, and launch parameters are the same for every replica ([editing the deployment](#部署后调整) moves them all to the new configuration together). When scaling out, each node is evaluated against the same configuration. A node with larger GPUs does not get a lower TP value, and a single-node replica is not split across two machines. To run the same model with different configurations on different hardware (for example, tp=2 on RTX 2080 Ti and tp=1 on RTX 8000, or serving on both NVIDIA and Ascend), create a separate deployment for each hardware configuration.

The public model name (`served_name`) is unique across deployments. If an existing deployment (running or stopped) uses a name, creating or cloning with that name returns 409 `SERVED_NAME_IN_USE`, with the deployment using it identified in the parameters. A deployment being deleted no longer reserves its name, so you can immediately recreate it with the same name. The deployment form prefills the public model name with the model name, `-`, and four random lowercase letters or digits (such as `Qwen3-0.6B-x7k2`). You can manually change it. If you leave it empty, the server uses the model name and returns 409 if that name is already in use. A clone also appends a random suffix in the same format to the original name.

Deployments created before this rule that already share a name remain unchanged. The gateway does not distribute traffic among them: requests go to the **most recently created deployment that can currently respond**. Older deployments take over only when all newer deployments cannot respond (all replicas are sleeping, failed, or still starting). Request distribution among replicas within a deployment still follows that deployment's routing policy.

To let several hardware configurations share the traffic for one model name, give each deployment a different model name and place a router in front of the gateway that aggregates multiple backends under one model name, such as [LiteLLM](https://docs.litellm.ai/docs/routing):

```yaml
model_list:
  - model_name: qwen3-8b            # Model name exposed to clients
    litellm_params:
      model: openai/qwen3-8b-2080   # served_name of the GPUWorks deployment
      api_base: http://<server>/v1
      api_key: <gpuworks API key>
  - model_name: qwen3-8b
    litellm_params:
      model: openai/qwen3-8b-rtx8000
      api_base: http://<server>/v1
      api_key: <gpuworks API key>
```

With this arrangement, each deployment still has separate metering, sleep behavior, and scaling. Refer to the router's documentation for distribution policies, weights, and retries.

## Common Issues

If submission fails, the error appears on the page with one of the following stable error codes. For more troubleshooting, see [Troubleshooting](troubleshooting.md).

| Error code | Meaning and action |
|---|---|
| `UNIT_PLACEMENT_NONE` | No group of nodes can take an automatically placed multi-node or PD request now (HTTP 409). The parameters give the total node count and how many nodes each check left out (`unmet` node requirements not met, `cordoned`, `offline`, `busy` not enough free cards, `no_network` no usable compute network); `largest` is how many available nodes the largest group in one compute network and architecture has (fewer than the rank count, so it cannot form a replica). Satisfy the item with the largest count, or place manually |
| `AUTO_PLACEMENT_TP_REQUIRED` | An automatic multi-node request has no `placement.tp`, so the platform does not know how many cards each node needs; fill in TP (and PP) |
| `AUTO_PLACEMENT_PD_MULTI_NODE` | An automatically placed PD unit must fit one node; a role's unit with `followers` (spanning nodes) must name its nodes, or do not use automatic placement |
| `AUTOMATIC_PLACEMENT_STALE` | Resources were taken after evaluation (for example, another deployment filled GPU memory), or a selected node no longer satisfies the node requirements, invalidating automatic placement. The server does not silently choose another node. Return to the deployment page, evaluate again, and resubmit |
| `ACQUISITION_CANDIDATE_STALE` | The storage specified in the request no longer exists, or the selected weight provider (Hugging Face / ModelScope) is no longer in the template. Evaluate again and resubmit; the server does not substitute another choice |
| `LAUNCH_CONFIG_CONFLICT` | A form field and extra arguments (`extra_args`) specify different values for the same parameter. The error lists both values; keep only one of them |
| `LAUNCH_CONFIG_UNSET_CONFLICT` | A parameter is disabled (cleared) in the form but is still specified in extra arguments. Remove that value or enable the parameter again |
| `NODE_REQUIREMENTS_NOT_EDITABLE` | Multi-node and PD deployments list nodes by rank and have no editable node requirements. Use “Edit replicas” to change their nodes |
| `NODE_REQUIREMENTS_UNMET` | The selected node does not satisfy the deployment's node requirements (labels, vendor, and so on). Select a matching node or adjust the requirements |
