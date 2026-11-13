# Use the AI Assistant

**When to use this guide**: Find the root cause and fix for an instance startup failure; determine which GPUs in your current cluster can run a model and how many it needs; understand why deployment replicas cannot be scheduled; or check node and accelerator health.

By the end, you can obtain evidence-based analysis for a specific object, then return to its page to verify and perform the necessary action. The assistant does not modify the platform for you.

**Prerequisites**:

- The server is running and you can sign in to the console. See [Install GPUWorks and Deploy Your First Model](../tutorials/getting-started.md).
- Platform checks alone do not require an LLM. For AI interpretation, first save an OpenAI-compatible endpoint that the server can access under **Settings → Integrations → AI service** (base URL, model name, and API key). The server stores the key encrypted.
- AI endpoints on ordinary private networks or local loopback addresses need no allowlist by default. An allowlist is needed only in strict mode. Platform administrators can authorize the target host under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes to either field apply to the next request after saving, without restarting the server. See [Outbound Access Configuration](../reference/configuration.md#server-配置文件-serveryaml).
- Analysis sends the diagnostic data it reads to the configured AI service. Before configuring it, confirm the target host and permitted data use. The settings page displays a notice identifying the external service host; same-origin platform models do not show this notice.

## Entry Points: Start With a Click, No Prompt Required

The assistant stays in a right-hand sidebar that remains open when you switch pages. You can continue using the page while it is open. You do not need to write a prompt: each entry point maps to a task, and clicking it lets the server collect the relevant information automatically.

| Task | Entry point |
|---|---|
| Diagnose instance failure | **AI Diagnosis** on the failed instance panel in deployment details |
| Check model hardware fit | The AI button on each model on the **Models** page |
| Explain why a deployment cannot be scheduled | **Why not placed** beside the “replicas waiting for nodes” notice in deployment details |
| Check node health | **AI Health Check** at the upper right of node details |

You can also click **AI Assistant** in the top bar or press ⌘I / Ctrl+I to open the sidebar:
- When empty, it lists objects on the current page (the deployment, node, or failed instance you are viewing), prioritizing those needing attention and showing the reason. Click a task button to start.
- You can ask freely in the input box below. Questions default to the first object on the page; change or clear the selection under **About**.

Each answer ends with two or three suggested follow-up questions that you can click. You can also continue in the input box. Follow-ups retain the same task and object, along with the previous conversation. **New conversation** clears the current conversation.

## Two Parts: Platform Checks and AI Analysis

After opening a task, read the platform checks first, then the AI explanation of the evidence:

| Part | What to look for | Next step |
|---|---|---|
| Platform checks for a failed instance (no LLM needed) | Known failures, matching log lines, and fixes; see the list below for the specific checks | Compare the exact logs and confirm whether the finding applies to this failure; a rule match is a lead to verify |
| Model hardware fit (no LLM needed) | Accelerator types in the online cluster, available and faulty accelerator counts, minimum TP, precision support, fast interconnect groups, GPU memory required per accelerator, and remaining KV capacity | Use this to select hardware. Whether you can deploy immediately still depends on candidate nodes on the deployment page; the fit table does not imply that GPUs are currently free |
| AI analysis (requires a configured LLM) | Root cause, evidence, and suggested changes such as node or engine parameters | Verify the object, logs, and suggested values, then return to the corresponding page to act |

Platform checks can identify the following failures:

- **Runtime resources**: Insufficient GPU memory, KV cache that does not fit, insufficient `/dev/shm`, full disk, or a port already in use.
- **Communication and file retrieval**: NCCL / HCCL communication failure, image pull failure, missing model files, or repository download failure.
- **Compatibility and parameters**: Unsupported GPU architecture or data type, a need for `--trust-remote-code`, or an engine that does not support the model architecture.

## How AI Analysis Works

1. **Confirm the object and start analysis.** The platform reads context for the current task. Use the following scope to check whether you selected the correct object:

   - **Failure diagnosis**: Failure reason, actual startup command, node hardware, scheduling explanation, GPU memory breakdown, events, and logs.
   - **Scheduling diagnosis**: Replica requirements, node constraints, placement policy, existing instances, and recent rejection reasons, including candidate node counts by state, the closest-fitting node, and what it still lacks.
   - **Node health**: Health level of each accelerator and agent evidence, node connection and scheduling status, instances, and warning logs from the node and agent.

2. **View the answer and query steps.** The answer streams into view. The start and end of tool queries appear in the step list; reasoning-model output is collapsed under **Reasoning**.
   The assistant can continue querying the cluster overview, metrics, historical logs, accelerator interconnects, or healthy replicas of the same deployment. You do not need to collect each API result first.
3. **Check whether the evidence is sufficient.** Logs and API results have length limits, and truncation is indicated. Initial logs for failure diagnosis contain at most the latest 60 lines and 40 earlier error log lines, totaling at most 8 KB.
   API lists default to at most 20 items, long strings are truncated to 300 characters, and a single result is capped at 12000 characters. Reading only part of a list does not mean these are the only objects in the cluster.
   If an answer does not cover the failure period, follow up with a specific time, instance, or metric, or verify directly using the [Metrics and Logs Guide](monitor-and-logs.md). Query times use UTC and relative times (“how long ago”); account for time zones when comparing local logs. You can ask using a node name directly.
4. **Continue, retry, or stop.** Suggested follow-ups and the input box retain the current task, object, and history. To switch objects, click **New conversation** first, then select an object under **About**.
   **Analyze again** reads the new state. Closing and reopening the sidebar preserves the result. Click **Stop** during analysis to stop it; existing platform checks are unaffected.

Common tools in the query steps:

| Tool | What it can read |
|---|---|
| `cluster_overview` | Node counts and capacity by status, GPU health and allocation, deployment counts, and offline, degraded, failed, or unscheduled objects |
| `search_api` / `call_api` | Find read-only platform APIs, then read objects or logs; continue retrieving required fields to avoid relying only on truncated initial results |
| `search_metrics` | Search the metric catalog, then read time-series metrics for the relevant period |
| `get_node_connection_diagnostics` | Administrators read connection diagnostics by node UUID: saved address, recovery records, historical public entry, and available current Agent observations or SSH checks |
| `get_gpu_topology` / `compare_with_healthy_replica` | View accelerator interconnects on a node, or compare a failed instance with running replicas of the same deployment |

For connecting or offline nodes, or questions about connection failures, the assistant is instructed to query connection diagnostics before citing the check location, target and observation time. The tool reuses the node detail page's read-only diagnostic and accepts only a node UUID, not custom probe URLs or SSH parameters; members without permission receive an authorization error. Missing SSH access, an offline Agent or an unperformed check means insufficient evidence, not a confirmed network or firewall failure. A different historical public entry alone does not prove the old address is invalid. If the model does not call the tool or the result is incomplete, do not treat the analysis as a confirmed root cause.

After the answer completes, time to first text, total duration, call counts, and tokens show the cost of this analysis. The server's `ai answer` logs also record these values.
One analysis allows at most 10 rounds and 30 tool calls. If evidence remains insufficient when the query limit is reached, do not treat the conclusion as verified.
Each user can process up to 2 questions simultaneously. If `AI_ASK_BUSY` appears, wait for one to finish, then submit again.

## Boundaries

The assistant reads the platform with the asker's permissions. It does not deploy, delete, change parameters, or run arbitrary node commands. An administrator's connection diagnostic can reuse the existing read-only SSH configuration checks and HTTP health probes; it does not restart, reinstall, recover automatically, or change addresses. An operator must confirm a stable entry and check both the platform record and Agent configuration when changing an address.
The platform estimator produces model-fit numbers; AI explains them. The fit table accounts only for weights plus engine reserve and excludes multi-node deployments and quantized variants.

The assistant can query platform-approved read-only APIs, including the cluster overview, nodes / deployments / instances, metrics, and historical logs.
It cannot read API keys, AI endpoint configuration, node SSH settings, platform SSH keys, terminals or their audit records, node file browsing, or external community image browsing. Live log data is read through the historical search API.

Before data is sent to the model, credential fields and recognized string credentials are replaced with `[redacted]`, including tokens, passwords, API keys, credential environment variables,
credential arguments in commands, URL usernames and passwords, and Bearer tokens. Ordinary numeric values needed for diagnosis (such as `max_tokens=512`) are retained.
Diagnostic data is still sent to the configured AI service.

## Evaluate the Analysis

To judge whether an analysis is useful, verify it against the original problem: does the evidence come from the current object and failure period, do the suggested parameters or actions apply,
and after you act, does the instance become ready, do requests recover, or is the original scheduling constraint removed? Make configuration changes yourself on the corresponding page, then click **Analyze again** to view the new state.
Receiving an answer or passing platform checks does not mean the failure is fixed. When evidence is insufficient, retain the uncertainty and continue checking logs or request records.
