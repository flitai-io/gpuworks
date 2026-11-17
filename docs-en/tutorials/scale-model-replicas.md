# Scale a Model Service from One Replica to Two

**Use case**: A model already runs on a single GPU. You want to add an independent replica to share requests, then scale back to one replica when demand drops.

**Goal**: Create `tutorial-replicas-qwen3`, scale from one replica to two, confirm that both nodes receive successful requests, then scale back to one replica and complete a conversation.

The screenshots in this tutorial show the English UI. The recorded evaluation uses the [original Chinese prompts](../assets/examples/benchmark-prompts.jsonl); the downloadable English instructions preserve the tasks and request settings but change the input token counts. Names, request IDs, and timings are examples; your run may differ.

## Before You Begin

| What you need | Configuration and checks used in this tutorial |
|---|---|
| Console permissions | You can view nodes, create and edit deployments, create evaluations, and use the Playground |
| Two available nodes | `host2` and `host3` are online and allow scheduling. Each has an idle RTX 4090 (48 GiB). Check the instances occupying them under **GPU Devices** |
| Model | `Qwen/Qwen3-1.7B` from ModelScope is registered, and its files are **Available** on both nodes. The example registration name in this tutorial is `dgpu-demo-a-ms` |
| Inference template and image | Profile `bf16-vllm-nvidia-general`, image `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`. Both nodes already have the image |
| Software environment | The English walkthrough uses Server build `79f927fc`, the runveil runtime, and NVIDIA driver `610.57.04`; check your Agent build in the node details |
| Node connectivity and disk space | Both nodes can serve requests from the Server. The runtime filesystem has enough space for unpacked images and runtime logs |
| Practice data | Download [benchmark-prompts.jsonl (English prompts)](../assets/examples/en/benchmark-prompts.jsonl) to the computer running your browser. It contains 8 synthetic prompts, each with `/no_think` and an output limit of `128` tokens. The English instructions retain the original task of producing a short Chinese paragraph |

`host2`, `host3`, and `dgpu-demo-a-ms` are example names in the screenshots. Select your own corresponding nodes and find your registered model by source ID `Qwen/Qwen3-1.7B`; its registration name can differ. If the model is not ready, follow [Download and Sync Models](../how-to/download-and-sync-models.md) to prepare its files on both nodes.

If experimental deployments still occupy these GPUs, click **Stop** in their deployment details and wait for the GPUs to be released before continuing. Note which deployments you stop so you can restore them after the experiment if needed.

This tutorial uses **Single node**, which means each replica performs inference entirely on one node. After scaling to two replicas, each node runs a complete copy of the model.

## 1. Create a Service with One Replica

1. Open **Inference Services → Create Deployment → Quick Deploy**. Under **Basic Configuration**, select your registered `Qwen/Qwen3-1.7B` model (`dgpu-demo-a-ms` in this example) and set **Target accelerator** to **NVIDIA**.
2. Under **Recommended Inference Template**, select the **general · vllm · balanced** profile for **Qwen3-1.7B** and check the **bf16** label. If the **Use a registered model?** page appears, select the **MS** registration for this model and click **Use this model and deploy**.
3. Confirm that the source ID, profile, and image match the prerequisite table. Enter `tutorial-replicas-qwen3` for both **Deployment Name** and **Public Model Name**.

![Basic configuration: the NVIDIA BF16 vLLM profile for dgpu-demo-a-ms, with tutorial-replicas-qwen3 as both deployment name and public model name](../assets/screenshots/en/tutorial-replicas-basics.png)

4. Under **Node**, select **Single node → Automatic**. Select both `host2` and `host3` under **Only these nodes**. Expand **Placement options** and keep **Placement strategy** set to **Spread**.
5. Enter the resource settings below and keep health checks enabled.

| Field | Value |
|---|---|
| **Replicas** | `1` |
| **Tensor Parallelism TP** | `1` |
| **Pipeline Parallelism PP** | `1` |
| **Maximum Context Length** | `2048` |
| **GPU Memory Utilization** | `0.90` |
| **Sleepable** | Off |
| **dGPU Memory** | Disabled |
| **KV Offload** | Off |

**Only these nodes** limits replica placement to these two nodes. **Spread** prefers candidate nodes with fewer replicas of this deployment. The scheduler selects GPUs automatically. Check the preview on the right to confirm that one replica uses one GPU.

![Placement configuration: automatic selection limited to host2 and host3 with the Spread strategy; one replica, currently previewed on host2 GPU 0](../assets/screenshots/en/tutorial-replicas-placement.png)

If you just stopped another deployment and released its GPU but the creation form still reports no available GPUs, reopen **Inference Services → Create Deployment → Quick Deploy**, enter these settings again, and check the candidate nodes.

6. Wait for **Compatibility check passed** on the right, then click **Deploy**.
7. Open `tutorial-replicas-qwen3` in **Inference Services**. Wait until **Replicas** shows **1 of 1 running** and the health check is normal. Record the first instance's node and GPU.
8. Open **Playground → Chat**, click **New conversation**, and select `tutorial-replicas-qwen3`. Send “Explain a model replica in one sentence.” and confirm that you receive a normal answer.

The first replica can run on either candidate node; it does not have to be `host2`. If the replica is waiting for a node, check the wait reason in the deployment details and open **View candidate nodes**. If startup fails, open the instance's **Logs**, resolve any GPU memory, model file, or disk issues, then retry.

## 2. Scale to Two Replicas

1. Return to **Inference Services → tutorial-replicas-qwen3**.
2. In the number field to the right of **Replicas**, change `1` to `2` and click **Apply**.
3. Wait until **Replicas** shows **2 of 2 running**. Expand both instances and confirm that one runs on `host2` and the other on `host3`, each uses one GPU, and both show **Running**.
4. Check the **Routing Policy** that now appears and keep it set to **Load balance**.

![Scaling result: tutorial-replicas-qwen3 shows 2 of 2 running, a replica count of 2, and Load balance as the routing policy](../assets/screenshots/en/tutorial-replicas-two-ready.png)

Clicking **Apply** only updates the desired replica count. The new instance still needs to start and pass its health check. A desired value of `2` does not mean scaling is complete. Wait until both instances are running before verifying request distribution.

If the status remains **1 of 2 running**, first check that the second node is online, allows scheduling, and has a free GPU. Also check the new instance's logs. Wait for model or image preparation to finish before judging the result.

## 3. Send Concurrent Requests and Check Both Replicas

1. Open **Inference → Evaluations**, click **New Evaluation**, and select **Parameter Sweep** under **Choose Question**.
2. Under **Set Target**, enter:

| Field | Value |
|---|---|
| **Evaluation Name** | `tutorial-replicas-distribution-en` |
| **Target** | **Managed Instance** |
| **Inference Service** | `tutorial-replicas-qwen3` |
| **Load Origin** | **Server** |

**Server** sends requests through the service's gateway and load balancing. If you select a compute node instead, the evaluation measures the instance on that node and cannot verify how requests are distributed across both replicas.

3. Click **Next**. Under **Dataset**, select **Imported Production JSONL**, click **Import JSONL**, and choose the `benchmark-prompts.jsonl` file you downloaded. Confirm that the import summary shows **8 corpus samples**. Although the option name includes “Production,” this tutorial imports synthetic practice data.
4. Set **Samples per Channel** to `4` and **Concurrency List** to `2`. Expand **Advanced Settings**, select **Fixed concurrency**, set **Warmup requests** to `1`, select **Keep per-request details**, and set **Tokenizer Source** to **Estimate**.

![Evaluation workload: 8 imported JSONL samples, 4 samples per channel, concurrency 2, fixed concurrency, and 1 warmup request](../assets/screenshots/en/tutorial-replicas-load.png)

Imported samples are sent exactly as written in the file; no tokenizer is needed to generate fixed-length inputs. Leave **Input Length (ISL)** and **Output Length (OSL)** blank and do not enable fixed output length. Each sample's `output_tokens: 128` is an upper limit, and an answer can finish earlier. The samples use `/no_think` to produce final answers, so the evaluation does not depend on requests that produce only reasoning output and lack valid timing data.

5. Click **Start Evaluation** and wait for it to finish. Confirm that the run has only one point, with concurrency `2`, `8` measured requests, and a success rate of `100%`. Warmup requests are excluded from the point's results but can appear in the service's request history.
6. Open **Monitor Center → tutorial-replicas-qwen3 → History**. Select a time range covering this evaluation and locate its successful requests.
7. Open requests handled by different replicas and check the **Replica** field. At least one request must be handled by the instance on `host2` and at least one by the instance on `host3`. Both requests must have status `200`.

![Request distribution: the history includes requests on host2 and host3, and the health section above shows running replicas on both nodes](../assets/screenshots/en/tutorial-replicas-distribution.png)

All `8` measured requests in this evaluation succeeded, and both replicas served requests. An additional `1` warmup request was handled by `host2` and is excluded from the table below.

| Node | Successful measured requests | Verified request details |
|---|---:|---|
| `host2` | `4` | `r15225`: status `200`, finish reason `stop`, total time about `4.9 s` |
| `host3` | `4` | `r15226`: status `200`, finish reason `stop`, total time about `4.9 s` |

Your request IDs, distribution, and timings may differ. **Hit the length limit** in the screenshot means an answer reached its configured output limit. Open its details to check the HTTP status; not every request needs a finish reason of `stop`.

Load balancing prefers the replica with the lighter current load. The request counts do not need to be equal. This tutorial verifies that both replicas actually serve requests. To determine whether two replicas improve throughput, benchmark one replica and two replicas with the same workload and other settings, then compare the results. For evaluation and report instructions, see [Run a Performance Evaluation and Read the Report](benchmark-concurrency.md).

If the history shows only one instance, first check that **Server** is selected, both replicas are running and awake, and **Routing Policy** is **Load balance**. Then confirm that the time range covers these requests. Instance startup alone does not prove that request distribution works.

## 4. Scale Back to One Replica and Call It Again

1. Wait for the evaluation to finish, then return to **Inference Services → tutorial-replicas-qwen3**.
2. Change **Replicas** from `2` to `1` and click **Apply**.
3. Wait until **Replicas** shows **1 of 1 running**. Confirm that the retained instance is **Running** and the other instance is no longer running. Then open **GPU Devices** and confirm that the removed instance's GPU is released.
4. Open **Playground → Chat**, click **New conversation**, and select `tutorial-replicas-qwen3` again. Send “Explain in one sentence why model services need to scale up and down.”
5. Wait for a normal answer to finish. In the service's request history, confirm that the retained instance handles this request and its status is `200`.

![Inference after scaling down: tutorial-replicas-qwen3 remains selected and returns a normal answer about the purpose of scaling](../assets/screenshots/en/tutorial-replicas-answer.png)

This example completes the `1 → 2 → 1` scaling and inference checks. The instance on `host2` remains after scaling down, and `host3` GPU `0` is released. The next request, `r15621`, succeeds using the same model name. It is handled by the retained instance with status `200` and finish reason `stop`.

During scale-down, the platform chooses which replica to remove and lets it finish existing requests before stopping. You do not specify which node must remain. To remove a particular replica, use **Remove this replica** in that instance's actions. This also decreases the deployment's replica count.

**Completion criteria**: The service answers with one replica. After scaling up, two instances run on different nodes and both handle successful requests. After scaling down, only one instance remains running, the other GPU is released, and the same public model name still returns normal answers.

## 5. Clean Up Experimental Resources

1. When you no longer need the deployment, open **Inference Services → tutorial-replicas-qwen3**, click **Delete**, and confirm using normal deletion.
2. Wait for the deployment to disappear from the list, then confirm that it no longer occupies either GPU. The registered model, model files, and image remain available for reuse.
3. Find `tutorial-replicas-distribution-en` in **Evaluations**. Export its report first if needed, then click **Delete**. Deletion removes the run's point results and per-request details.
4. If you stopped other experimental deployments before starting, click **Start** in their deployment details. Wait for their original replicas to return and verify inference.

<!-- translation-source-sha256: 93dc9c25f11625af4fb6157da7404538ca0b62699408fa5649f0de063cec15e9 -->
