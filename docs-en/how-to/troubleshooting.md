# Troubleshooting Guide: Find the Cause by Symptom

Use this guide to troubleshoot node connections, GPUs, models and images, instance startup, and inference requests. Find the section that matches the symptom,
confirm the cause through status, events, or logs, then repeat the original operation after taking action to check whether it recovers.

**Prerequisites**: You can view the relevant node or deployment and log in to the corresponding host when you need to run diagnostic commands.
Restarting, resetting, and changing configuration require the corresponding permissions; first confirm the impact on current services.
Replace addresses, node or instance IDs, and `<jwt>` in this guide with your own values; `<jwt>` is a login credential for the management API, not an inference token.
Run node commands on the affected node and server commands on the control plane host; replace example paths with your actual installation directories.

## Where to Look First

1. Record the time of the failure, the node or deployment name, and the exact error text.
2. Check status and evidence at the corresponding entry in the following table, then follow the section for that symptom.
3. After the fix, repeat the original connection, deployment, or request to confirm recovery; save any remaining errors to narrow down the cause further.

| What to Check | Where | What It Tells You |
|---|---|---|
| Node status | Badges on the Nodes page: Online / Offline / Connecting (`connecting`) | **Only whether the agent is connected**; accelerator issues do not change it |
| Accelerator status | The “Vendor tool did not answer” badge on the node **details page** (not shown in the node list), and each card's allocation status (allocatable / unallocatable, etc.), health, ownership, and usage in the accelerator list | Whether the card is usable and whether its readings are stale |
| Node details | “Latest rejection” row, “Out-of-band recovery” card, and “Diagnose” button (`POST /api/v1/nodes/{uuid}/doctor`, the same checks as `gpuworks-agent doctor`) | Why the connection was rejected, how far recovery has progressed, and whether the environment is healthy |
| Instance event timeline | Instance details; API `GET /api/v1/instances/{uuid}/events` (deployment level: `GET /api/v1/deployments/{id}/events`) | The state and phase where it is stuck and the failure reason (`params.detail`) |
| Scheduling explanation | Deployments page; `schedule.explain` in the instance response; `plan` in the `POST /api/v1/evaluate` response | Why this card was selected and why other candidates were rejected |
| Logs page | Search by instance, level, source (`source=instance\|node\|agent\|server`), and keyword; see [Monitoring and Logs](monitor-and-logs.md) | Engine output, errors such as `CUDA out of memory` |
| Server logs | Installed with `gpuworks install`: `/opt/gpuworks/server/logs/gpuworks-server.log` (`~/gpuworks/server/logs/gpuworks-server.log` on macOS); direct `serve`: `<data>/logs/gpuworks-server.log`; under systemd: `journalctl -u gpuworks` | Handshake failures, recovery rounds, and proxy errors |
| Agent logs | `<state>/agent.log` (the default installation state directory is `/opt/gpuworks`, or `~/gpuworks` on macOS; rotates at 10 MB and keeps one previous file, `agent.log.1`); `systemctl status gpuworks-agent` | Connections, GPU probing, pulls, and startup |
| Environment self-check | `gpuworks-agent doctor` on the node | Runtimes, GPUs, drivers, and device permissions |

Instance status values are `pending`, `pulling`, `starting`, `optimizing`, `running`, `removing`, `stopped`, `failed`, and
`pending-removal`. During `pulling`, the phases may proceed through `image_verify`, `image_pull`, and `image_extract`;
`image_pull_retry` means switching to the next image source after one fails. During `starting`, the phases are `container_prepare`, `container_launch`, and
`health_check`.

Management API errors take the form `{"error": "...", "code": "...", "params": {...}}`. The `code` is stable; use `code` as the reference.

## Scope and Evidence of Connectivity Checks

Separate the segment a check covers from the business operation that failed. Record the observing host, actual target, proxy or network channel, observation time, and last completed stage. A check that did not run or return remains unknown.

| Check | What it establishes | What still needs checking |
|---|---|---|
| Node SSH / Agent process | The control plane can log into the node and the target Agent process exists | Node-to-control-plane DNS, TLS, WebSocket and authentication; an existing process is not an established control session |
| Public control-plane health URL | The observing host can access that URL's health endpoint | Other node or user networks, login, WebSocket and streaming inference |
| Engine startup health | The node's local request to that instance's health endpoint | The gateway-to-node channel and the user's gateway entry point |
| Registry ranking probe | Whether the node receives a response from registry `/v2/` through its effective proxy path | Image authentication, tag, manifest, layer download and extraction; HTTP 401 can still mean the endpoint is reachable |
| Storage test | Mount, read and write results on the named node during that test | Other nodes, identities and later mount state |
| Internal network test | TCP, RDMA or card-level results for the selected node pairs and network | Untested pairs, in-container NCCL/HCCL and actual PD KV transfer |

Cached or historical observations have their own timestamps. Opening a page again does not repeat the probe. A difference between the public entry point and a node's saved URL is a reason to verify both, not proof that the old address is invalid or permission to switch automatically.

### Node Address and Session

When a node stays connecting, start with its last report and recovery error. Administrators can collect read-only evidence from Connection Diagnostics in node details: an online capable Agent returns runtime observations over existing ctrl without control-plane SSH access or full GPU doctor. The report identifies source, acquisition time, effective Server URL, current control/data state and actually observed transport stages.

A successful ctrl round trip is not data or business health. Data `attached` is a connection snapshot; after a previously established lane drops, `detached` waits for reconnection without automatic ctrl fallback. Disabled, unnegotiated, initially pending and unknown states are distinct. An online node alone does not establish that logs/downloads work. After an RPC attempt, SSH is not waited for serially. The Agent has its own short budget; cancelling the page wait does not remotely cancel execution.

Offline or old Agents cannot provide a fresh runtime report. The page retains saved addresses, reporting/recovery records and public-entry history; available SSH is an alternative. Missing credentials, tools or a live control session leave checks unperformed, not proof of a broken network. Through the node console, run:

```bash
gpuworks-agent doctor --connectivity --state /opt/gpuworks --json
```

Substitute the actual directory. It uses the real short handshake with a 10-second total budget and no GPU checks. `source=cli` identifies this invocation's shell/flags/env/file, which can differ from the service. `data_lane=unverified` explicitly leaves data untested. The latest valid recorded safe failure keeps its original time, separate from the current result. Legacy, rotated, unrecorded-at-current-log-level or unreadable evidence is unknown/unavailable; raw log lines are not emitted. Existing `smoke --state` remains available but likewise proves neither dual-lane nor long-term health.

Saved URLs, node configuration snapshots, running-process facts and historical public entries are different evidence sources. SSH HTTP uses the SSH environment, possibly a different proxy; flags/env can override files; historical public URLs may no longer work. Only actual callbacks establish DNS, connect, proxy CONNECT or TLS stages. Proxy-side DNS and missing callbacks cannot be filled in as successful.

After confirming a stable URL, explicitly synchronize the platform record and node configuration and restart using the installation's supervisor. Diagnosis does not migrate an enrolled node's URL in place; a new report entry is not a completed repair. Do not share full configuration, process arguments, environment, tokens or proxy credentials.

### Streaming Requests and File Transfers

Inference request details describe the gateway's observations. `finish_reason` is the engine's model termination reason, not evidence that the client received `data: [DONE]`. To inspect individual segments, enable a scoped debug session under **Settings → Diagnostics**, reproduce the request, and compare its `upstream`, `client` and `end` records. Correlate logs using the request ID / Trace ID.

The captured `client` direction contains bytes the gateway wrote to its immediate downstream. When a third-party gateway, load balancer or client SDK follows it, their own receiving evidence is also required. A successful gateway write does not prove delivery of the complete reply to the final client.

After a storage download sends HTTP 200, an interruption can only abort the connection; it cannot return a JSON error. Keep the request ID and use control-plane logs with the node, storage path, copied bytes and observed failure direction to distinguish an interrupted node read from a failed browser write. For peer model copies, use the transfer ID to follow the target receiver, source-to-target connection, data confirmation and final target state. Two online nodes do not establish that their direct data network is reachable.

## Node Stays Connecting or Does Not Come Online

**Symptoms**: The Nodes page stays at “Connecting”, or repeatedly switches to “Offline” after coming online; the agent logs contain `connection ended`,
`reconnecting`, or `server rejected connection`.

**How to confirm and resolve**

1. Check the connection direction in the node details. For **agent-initiated connections**, run the following command on the node; replace the path if the state directory is not `/opt/gpuworks`:

   ```bash
   gpuworks-agent doctor --connectivity --state /opt/gpuworks
   ```

   Exit code `0` means success, `3` means a 10-second timeout, `4` means the server rejected the connection, and `1` means another error.
   The command reads the connection configuration from this state directory; you can also explicitly pass `--server` and `--token`.
   For **server-initiated connections**, check routing, firewalls, and port mappings from the control plane to the node's listening port, and handshake errors in the agent logs; do not substitute the active connection test above.

2. Check the agent logs on the node, and verify the address, token, or protocol version against the causes below:

   ```bash
   tail -n 100 /opt/gpuworks/agent.log
   ```

   The connection configuration is in `gpuworks-agent.conf` in the same directory. It contains a token; do not share it as-is.
   To view recent rejection records, call the following from a terminal that can reach the control plane:

   ```bash
   curl -s -H "Authorization: Bearer <jwt>" \
     "http://<server>:8800/api/v1/agent/rejects"
   ```

3. After correcting the issue, confirm the node returns to **Online** and watch for further disconnections.

- `GET /api/v1/agent/rejects` returns recently rejected connections: `token_prefix` (the first 8 characters of the token), `reason`, `remote`, and `at`;
  only the latest 64 records are kept in memory and are lost on restart. `bad_token` cannot be associated with a node, so it appears only here and in the server logs
  (`agent session failed`); “Latest rejection” in node details records only `proto_mismatch`.
- After a rejection, the agent waits a fixed 5 minutes before reconnecting (ordinary disconnections use exponential backoff from 1 second to 30 seconds). After fixing a node installed with systemd, run `sudo systemctl restart gpuworks-agent` to reconnect immediately; for other installation methods, restart the agent using the corresponding method. See [Connect Nodes](connect-nodes.md) for connection methods.

**Possible causes**

1. The server address is unreachable from the node or incorrect. The `--server` in the installation command is the address the agent always dials. After installation, it is recorded in `SERVER`
   in `<state>/gpuworks-agent.conf` (`systemctl cat gpuworks-agent` shows neither it nor the token; the unit contains only `run --state <directory>`); `install.sh`
   first downloads the agent binary from that address (and fails immediately if it cannot download), but does not verify after installation that the agent can establish a persistent connection. This does not apply to nodes with server-initiated connections:
   they do not connect to the server. Check whether the server can reach the node's listening port instead (security groups, firewalls, and port mappings).
2. The token is incorrect. After a node is deleted or its token is regenerated (`POST /api/v1/nodes/{uuid}/token/regenerate` disconnects the current connection),
   the old token is rejected with reason `bad_token` (detail: `node token was not recognized`; for an empty token:
   `node token is required`).
3. The reverse proxy or listener does not forward the WebSocket upgrade for `/api/v1/agent/connect`, or that address is not included in `listen.agent`
   (features not served by the listener return 404; see [Configuration Reference](../reference/configuration.md)).
4. The protocol versions do not match, with reason `proto_mismatch` (a detail such as `server supports 1-1`); upgrade the agent.
5. `stale_session`: occurs only during a **data channel** handshake and means its bound control session has expired. The agent only logs it at Debug level and retries
   with backoff; it recovers automatically after the control channel reconnects. See [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md). Take action only if it persists.

Clock skew is **not** a reason for rejection: the handshake does not validate timestamps, and the HMAC for `server_dial` signs only a one-time nonce, with no time component.
If the key for a server_dial node does not match, the agent reports `invalid server_dial proof`.

## Automatic Recovery After a Node Disconnects

**Symptoms**: A node changes from “Online” to “Offline”, and an “Out-of-band recovery” card appears in its details.

### Check Recovery Progress

In the node details, check the phase, attempt count, last probe time, and last error under “Out-of-band recovery”, corresponding to
`recovery_phase`, `recovery_attempts`, `last_probe_at`, and `last_recovery_error`.
This card appears only when `recovery_phase` is nonempty.

The platform marks a node offline after receiving no heartbeat for `--stale-after` (90 seconds by default), then tries the following in order:

1. The agent reconnects on its own.
2. The server redials a `server_dial` node.
3. The server performs out-of-band recovery over SSH: it restarts the agent and waits for `--recovery-restart-wait` (1 minute by default); it reinstalls only if the node still does not come online.

SSH recovery normally runs `systemctl restart gpuworks-agent`, or starts it with nohup if there is no unit; units belonging to other servers are handled as described below.
Reinstallation reuses the original token, rewrites `<state>/gpuworks-agent.conf` using the node information saved by the server, and preserves the host's current systemd or nohup execution method.
See [ADR 0006](../adr/0006-out-of-band-recovery-ssh-credentials.md) for the design.

### Reconnect Manually

1. Check the last error and first address the corresponding cause below.
2. On the Nodes page, click **Reconnect** next to the offline node. This is the recovery action in the UI, corresponding to `POST /api/v1/nodes/{uuid}/recovery/probe`.
3. Wait for the node to return to **Online**; if it still fails, check the updated phase and last error.

**How the button behaves with different credentials**:

- SSH credentials saved: attempts recovery immediately, ignoring the waiting window; still skips nodes that are online, not yet connected, or have no credentials. You can save credentials with `PUT /api/v1/nodes/{uuid}/ssh`.
- No SSH credentials saved: the UI displays an agent installation command and does not perform SSH recovery; run the command on the node to reconnect it.
- Phase is `stopped`: the button is disabled; hover over it to see why the cloud instance was destroyed.
- Server has `--recovery-enabled=false`: a manual trigger returns `recovery is disabled`. See [Configuration Reference](../reference/configuration.md) for parameters.

### Resolve the Last Error

**Missing SSH credentials or platform public key**

Automatic recovery skips nodes without credentials; calling the recovery API directly returns `ssh credential is not configured`.
Add credentials, then reconnect. If the credentials contain only an address, the platform logs in using its platform key; if `has not authorized the platform key` appears, authorize the public key using the command provided in the error.

**Still waiting, or automatic recovery has stopped**

- Less than `--recovery-grace-period` (2 minutes by default) has elapsed since the node was last online, or it is still in backoff: wait for the next attempt or reconnect manually. Backoff starts at 30 seconds, doubles each time, and caps at 10 minutes; `server_dial` nodes must also wait for redial backoff to reach its maximum (5 minutes).
- Two consecutive failures in the same phase change the state to `recovery_blocked`: automatic recovery stops; resolve the error, then reconnect manually.
- SSH is unreachable: check the address and network; the platform continues retrying with backoff. SSH host keys are currently not verified, so an old key does not block uninstallation and recovery after a cloud provider reclaims and reuses a public IP.

**The agent is running but has no control session**

For the error `agent is running but has not established a control-plane session`, recovery does not restart or reinstall the agent.
Check the agent logs and connection configuration as described in [Node Stays Connecting or Does Not Come Online](#节点一直连接中或不上线).

The platform determines ownership from the server address of the agent referenced by the unit: it reads the configuration file, or the command-line `--server` for older installations.
It treats the agent as belonging to this node only if the address matches, the address cannot be read, or the node uses a server-initiated connection (with no such address).

**The unit belongs to another server**

`foreign_agent:` is followed by the other server's address. All installations share the unit name `gpuworks-agent`, so a later installation overwrites an earlier one; recovery does not restart or reinstall another server's running unit.

After confirming that the other server no longer uses this host:

1. Back up the unit file.
2. Run `systemctl stop gpuworks-agent` on the host.
3. Reconnect manually to reinstall the unit with this node's token.

If the other server's unit is already stopped, recovery reinstalls it directly without restarting it first.

**The cloud instance was destroyed**

The phase is `stopped`, with reason `provider instance no longer exists`. The node enters this phase immediately when release through the API succeeds or synchronization finds that the cloud instance no longer exists; the node may still be online at that time. An agent reconnection clears this state.
The platform keeps the node record; delete it manually after confirming it is no longer needed.

### The Cloud Platform Changed the Connection Address

Before SSH recovery, nodes associated with cloud instances reread the instance endpoints:

- The original SSH endpoint is no longer in the endpoint list: switch to a current endpoint, preserving the user and credentials. For example, changing a Pod configuration in RunPod restarts it and renumbers all ports; an ordinary restart does not change ports.
- For `server_dial` nodes using port mappings, such as Vast and RunPod, the agent's mapped address has changed: update the address and redial immediately, without an SSH login in this round.

The instance list for the same cloud account is read at most once per round and reused within 30 seconds; multiple offline nodes do not each repeat the provider API call.

## GPU Not Detected or Vendor Tool Did Not Answer

**Symptoms**: The node is online but its GPU count is 0, or the node details page shows “Vendor tool did not answer” (not shown in the node list). Hover over the badge to see
when the vendor tool first stopped responding (the first consecutive failure; cleared after a successful probe) and the specific error.

> An accelerator probe failure **does not** take a node offline or change its connection status: a connected node is “Online”, and the terminal, diagnosis, and reconnect actions remain available—
> a node with a stuck driver is precisely one you need to log in to investigate. When probing fails, information known only to the vendor tool (GPU memory, UUID, health) retains the last successful probe results,
> marked by “Vendor tool did not answer”; which cards are in the machine and who owns them come from the PCI bus and are unaffected.

**Act on the check results**

First open **Compute nodes → Node details → Diagnose** and check the evidence and recheck commands under **Action required**, then follow the table below.
For the CLI, run `gpuworks-agent doctor` on the node; its exit code is always `0`. Read `-- action required --` instead of judging health by the exit code alone.

| Observed Result | Next Step |
|---|---|
| GPU count is 0, no vendor tool | On the node, check whether the driver and tool are installed and check the agent user's `PATH`; use `nvidia-smi`, `npu-smi info`, `ppu-smi`, or `hy-smi` according to the hardware |
| **Vendor tool did not answer**, `gpu probe failed` | Run the corresponding tool on the node and check its duration and errors; if it remains stuck, check driver logs. Do not treat stale GPU memory readings as current capacity |
| **Runtime cannot use it**, **Driver init failed** | Check the diagnostic evidence for that card and the results for other cards; restore the driver first, then run diagnosis again |
| **Not readable** | Check whether the vendor tool is executable and returns a result; do not conclude from this alone that the hardware is faulty |
| **Passed through**, **No driver bound**, **Held by another driver** | First confirm the intended ownership of the cards; these cards do not participate in scheduling on this node, so do not immediately treat them as hardware failures |
| Host sees the card but container does not | Check **Runtime** or **Runtime plugin** messages; fix the plugin or the workload user's device permissions, then check whether scheduling resumes |
| **In use outside the platform** | Check the displayed processes and GPU memory usage, and release them only after confirming they can be stopped; do not directly terminate unknown service processes |
| `accelerator_unrecognized` | Save the BDF, `vendor:device`, class, and driver information from the report so device recognition can be added |

After taking action, run diagnosis again and confirm that card health and allocation status match the actual devices; a node being **Online** proves only that its connection is working.

### GPU Count Is 0: Check Tools and Drivers First

If the driver or `nvidia-smi` is not installed or is not in `PATH`, the prober returns 0 GPUs without an error; the node is online but has no GPUs.

**How the platform identifies the vendor**: the agent tries `hy-smi`, `npu-smi`, `ppu-smi`, and `nvidia-smi` in that order, selecting the first one that actually responds.
For NVIDIA, `nvidia-smi --query-gpu=index` must succeed. NVIDIA is last because the PPU SDK includes
a CUDA-compatible `nvidia-smi` (`/usr/local/PPU_SDK/CUDA_SDK/bin`) that responds normally with PPU cards presented as GPUs.

If none responds, the agent checks **vendor device files** (`/dev/davinci_manager`, `/dev/alixpu_ctl`, `/dev/nvidiactl`), then **which vendor's CLI is installed**
(in the order `npu-smi`, `hy-smi`, `ppu-smi`, `nvidia-smi`, with NVIDIA last because hosts from other vendors often retain an outdated `nvidia-smi`);
only if none is found is the node classified as having “no accelerator”, with an empty vendor. While the driver is still loading (after an Ascend host boots, the agent starts first, `npu-smi` has not yet responded,
and the device files have not been created), the node therefore remains an Ascend node, and later probes can add its cards.

**Temporarily 0 during Ascend startup**

An Ascend node has 0 cards, or logs contain `gpu command failed vendor=ascend args=[info] err="npu-smi: no answer within 10s: ..."`:
`npu-smi info` takes more than 10 seconds (which can happen just after the driver loads). Wait for the next probe; if it persists, run `npu-smi info` directly on the node and check its duration.

### Vendor Tool Did Not Answer or Readings Are Stale

Run the corresponding tool manually on the node, and assess its duration and errors together with `agent.log`; do not treat stale GPU memory readings as current capacity.

If `nvidia-smi` exists but fails to execute (driver and kernel library mismatch, NVML error, or exceeding 10 seconds), the agent logs
`gpu command failed` (with `output_tail`) and `gpu probe failed`. When the server receives the probe failure, it keeps the existing GPU list,
marks the readings as stale, and reuses the previous dGPU probe results without rerunning that probe for this round. The node's connection status does not change.

#### Why Readings Stay Unchanged When the Tool Times Out

When the driver responds slowly to the vendor CLI (all calls queue on the driver lock, commonly on hosts where a card fails to initialize), each read-only query used for status reports and metrics
(inventory, metrics, topology, and version) waits at most 10 seconds. If no response arrives, callers use the previous result of the same query while the query continues running in the background,
for up to 180 seconds before it is killed. Its completed result is saved for the next round. Only one such query runs per CLI at a time; callers of the same query share one process.
During this period, metrics use the completion time of that query rather than the current time, so charts are sparse but do not contain fabricated new data points.

The log message `gpu query slow; callers are served its last answer meanwhile` (with `took`) indicates a slow driver. Only when the last successful result
is more than 10 minutes old with no new result is this treated as a probe failure (the node stays online, and card readings are marked stale). A timeout is treated only as slowness,
and does not trigger a switch to a query with fewer fields. The `nvidia-smi -q -x` query used by udocker to resolve device nodes follows the same path.

#### Why Metrics Are Sparse and Diagnosis Skips Checks

NVIDIA queries update at the following intervals; not yet reaching the next interval does not indicate a probe failure:

| Query | Update Timing |
|---|---|
| Metrics | Every 10 seconds |
| PCIe throughput (`nvidia-smi dmon`) | Every 60 seconds; metric samples between updates do not include PCIe values |
| Inventory, external processes | With status reports, every 60 seconds |
| Topology (`topo -m`), MIG list (`-L`) | When card indices, UUIDs, or PCIe addresses change, or every hour; topology queries on healthy hosts can also take more than 10 seconds |
| CUDA version | When the driver version changes |

Node diagnosis (doctor) also uses only these shared queries: the driver version comes from the inventory (or the kernel module's `/sys/module/nvidia/version` when `nvidia-smi` does not respond), without running `nvidia-smi` separately. If the CUDA runtime check fails,
it skips the peer access and NCCL checks, which also require driver initialization (showing `SKIP (the CUDA runtime check did not pass)`).
The server waits for doctor for at most 2 minutes; the agent limits a diagnosis to 100 seconds. Checks still running at the deadline are cut off, and the report is returned as usual (the Ascend per-die check also keeps 10 seconds for the rest of the report, and dies that do not fit are listed as not checked).

#### Logs Say the Process Has Not Exited After Being Killed

Commands that change driver state (such as Ascend `npu-smi set`) and calls requiring immediate results do not use old results: they still have a 10-second timeout. If they do not exit after being killed
(processes stuck in D state inside the driver), the agent waits only 3 seconds before giving up and logging `gpu command did not exit after kill`;
until that process exits (logged as `stuck gpu command exited`), calls of this kind to the same CLI fail immediately (`an earlier call has not exited`) without starting a new process.
Calls of this kind to one CLI run one at a time, so two arriving together do not start a process each; on timeout the command is killed together with the processes it started.

The same rules govern the other commands that enter the driver, so however often they are called, each kind has at most one stuck process:
- **Runtime checks**: the periodic per-card checks (every vendor, on the host or in a container), Diagnose's per-card checks and the NVIDIA CUDA / peer access / NCCL checks share one "runtime check" slot, so only one runs at a time.
  Once one is stuck, no new check starts: the periodic round is skipped and Diagnose shows not checked or `SKIP (an earlier runtime check on this node was killed and has not exited ...)`; neither marks a card as failed.
- **Checks in a container**: a container that cannot be removed (its process is stuck in the driver) is remembered, and no new container is started until it is gone.
- **Ascend `hccn_tool`** (reading the accelerator NICs every minute): one slot per die and 3 seconds per call, so a stuck die affects only that die and never holds up the node's status report.
- **Diagnose**: one at a time per node; a second request returns `a diagnosis is already running on this node`.

If this persists, run `ps -eo pid,stat,etime,cmd | grep -E -- '-smi|hccn_tool|python3'` on the node to check for processes in `D` state, and check `dmesg` for
Xid / NVRM errors.

### A Card Is Unusable by the Runtime or Fails Driver Initialization

**`runtime_unusable`: accelerator runtime context creation failed**

A card's health is “Runtime cannot use it” (`runtime_unusable`): accelerator runtime smoke checks target each card every 15 minutes.
Cards running instances and cards in use by processes outside the platform are skipped, the whole round is skipped if the driver does not respond, and the check does not change the node's connection status.
NVIDIA requires `python3` and `libcuda.so.1` and creates a CUDA context on the selected card. One card failing to initialize often makes `cuInit`
fail for the entire host, so examine findings for the other cards as well. On an Ascend host with CANN installed, the check uses `python3` and
`libascendcl.so` to create an ACL context and allocate 4 KiB. For example, `aclrtSetDevice=507033` marks that die unusable
(a die passed through to a VM can return the same code, so first confirm during manual review that nothing else occupies it;
the smoke check itself only asks about cards owned by this node). A driver-only Ascend node without `libascendcl.so` skips the check rather than reporting a missing tool as a card failure.
On a Hygon host with DTK installed, the check uses `python3` and `libamdhip64.so` (`/opt/dtk/lib` and similar) to find the card by its PCI address, `hipSetDevice` to it and allocate 4 KiB;
for example, `hipMalloc=2` marks that card unusable. HIP not listing the card or `hipInit` failing is a host problem, not a card failure; without DTK the check is skipped.
A node whose host has no vendor runtime (a PPU host usually has only the management library `libhgml.so`; an Ascend host with the driver but without CANN) is checked inside a container instead:
it borrows an inference image of the same framework that is **already** on the node (a tag starting with `hggc` / `cann`, preferring images this node's instances use), injects only this card the way a real instance gets it,
runs the same check inside (about 0.45 s per card on PPU, container start included), and removes the container. No image is ever pulled for the check: without such an image on the node the check is skipped and the vendor tool's verdict stands.
This is done only on docker / nerdctl nodes; on udocker a container means unpacking or copying the whole image into a rootfs (27–61 s and 17 GB of disk each time for a 17 GB image on ext4),
so udocker nodes are not checked this way. A container that does not start, or an image without the runtime or python, is a host problem, not a card failure. Apple has no check.

**Diagnose now runs the same per-card check (per die on Ascend) on NVIDIA, Ascend, Hygon and PPU, on demand.** Node details → Diagnose and `gpuworks-agent doctor` run the runtime check above once for each die this node owns
(it is the same code as the periodic check, not a second implementation), so a die the periodic check has already marked `runtime_unusable` always shows up in the diagnosis.
A failing die is shown as “Runtime cannot use it” in the accelerator table, and **Action required** lists a `runtime_unusable` item per die (level Error, with the same evidence the periodic check records).
When the table already carries a more specific vendor verdict for the die (for example `reset_required`), the table keeps that one and `runtime_unusable` is still listed on its own.
The **Per-card runtime check** entry (`card_runtime`) under Information summarizes how many dies passed, failed and were not checked.
NVIDIA also has whole-node CUDA, P2P and NCCL checks: the whole-node CUDA check sees every card at once and only says whether the driver initialises and the first visible card takes a context; each card's verdict comes from the per-card check.

**The evidence is a command you can paste, not a description of one.** Every finding's evidence is a whole command: copy it onto the node and it
reaches the same verdict. The script the command carries is the very one the check runs (single source in `internal/accelerator`), so the two
cannot drift apart. The Ascend command locates the host CANN installation itself and uses `npu-smi info -m` to resolve the die index into the
**logical ACL device id** that `ASCEND_RT_VISIBLE_DEVICES` takes (the two differ on a host whose ids are remapped by passthrough); the NVIDIA
command names the card by UUID, because an index addresses a different card once the driver drops one. In the UI a command longer than 4 lines is
collapsed and can be expanded, and the copy button always copies the whole command (over plain HTTP, where the clipboard API is unavailable, a
box opens with the text ready to select). To find out why `aclrtSetDevice=507033` happens, prefix the command with
`ASCEND_SLOG_PRINT_TO_STDOUT=1 ASCEND_GLOBAL_LOG_LEVEL=1` and CANN prints the runtime and driver errors straight to the terminal; without it they
land under `~/ascend/log/debug/plog/`. **A die that cannot initialize itself may leave nothing in `dmesg` at all, and `npu-smi info -t health`
still reports OK** — reset that die directly (`npu-smi set -t reset -i <npu> -c <chip>`, which affects the other die on the same card), and
reboot the host if that does not help.

A die that was “not checked” is never omitted and never implied healthy; it is named together with the reason:

- The diagnosis time ran out: dies are checked one after another, the remaining time is examined before each one, a die that does not fit is not started, and all the rest are listed as not checked;
  a die that the time limit cut off mid-check is also listed as not checked, not as a fault. A healthy die answers in about 1 second and a failing one can wait up to 45 seconds;
  the agent limits a diagnosis to 100 seconds and keeps 10 seconds for the rest of the report, so with many failing dies the later ones show as not checked. Run Diagnose again, or run `gpuworks-agent doctor` on the node.
- An instance is running on the die (or it is not yet known which dies are held): like the periodic check, doctor leaves it alone and lists it as not checked.
- The vendor tool gave no device number for the die: it cannot be named, so it is listed as not checked.
- The host lacks `python3` or `libascendcl.so` (a driver-only node), the driver does not respond, or the check never reached the die (a host-level conclusion):
  this is stated once in the information as “not run” or “stopped”, nothing more is tried, and no die is blamed; dies that already have a conclusion keep it.

A die passed through to a virtual machine is not this node's and is not part of this check; the table still shows it as passed through. Run standalone, `gpuworks-agent doctor` has no knowledge of the agent's instances and checks every die.

A card is marked `runtime_unusable` only when the check explicitly attributes failure to that die. Failure to load the library, a non-CANN library,
`aclInit` failure, a process crash, or unreadable output is a host-level conclusion (every die on the host would fail identically):
the round stops without recording a fault on any card, and the “Per-card runtime check” entry in node diagnosis (or `gpuworks-agent doctor`) explains why. Failures while releasing resources after a successful allocation
(`aclrtFree`, `aclrtResetDevice`, and so on) are logged but do not mark a fault, because the die has already answered whether it works.
If `ASCEND_ACL_SMOKE_OK` has already been printed, a non-zero exit afterwards (for example CANN crashing during its exit cleanup) still counts as a pass; the exit status is only written to the agent log.
Retry backoff doubles after consecutive failures (1, 2, 4 rounds, up to 4 rounds, about one hour by default) and resets as soon as the card responds.
To recheck a repaired card right away, click “Diagnose” in node details: the diagnosis runs the same check on each card and records the result as the card's latest answer. A pass clears the fault and the backoff, and the card returns to normal at the next report (about a minute); a failure counts as one failure. A card the diagnosis did not ask, because its own time budget ran out or the runtime was unavailable on the host, records nothing. A check in flight when the agent shuts down is discarded: no failure is recorded and no backoff is started.

When a check hangs (for example stuck in the driver and not returning after 30 seconds), checks on the whole node pause until that process returns:
the agent will not start a second check to queue behind the same driver lock. The stuck card is recorded as failed meanwhile, while the other cards keep their last result
(a card that was never checked has no conclusion and is not treated as faulty); `gpuworks-agent doctor` shows the reason too.

**`driver_init_failed`: the driver did not bring up this card**

A card's health is “Driver init failed” (`driver_init_failed`, NVIDIA only): the card exists on PCI and is still bound to the nvidia driver,
but is missing from the cards listed by `nvidia-smi`, usually because the driver could not bring it up
(`dmesg` contains `RmInitAdapter failed`, `Possible bad register read`, or `GPU may need to be reset`).
These cards still appear in the accelerator list (their physical presence is a fact; the agent reads sysfs directly to enumerate PCI in milliseconds, independently of the driver),
but cannot be scheduled; the model name comes from `pci.ids`. CUDA is often unusable across such a host. Check on the node before deploying, and reset these cards if necessary.

**`vendor_cli_unanswered`: card information is temporarily unreadable**

A card's health is “Not readable” (`vendor_cli_unanswered`): the card is bound to its vendor driver, but the vendor CLI did not respond at all in this round
(timed out or is not installed), so nothing is known beyond its PCI identity. This does not mean the card is faulty. Check the node's GPU probe error and run the CLI manually on the node.

### Host Sees the Card but Container Does Not

The driver works but the container cannot see the GPU: NVIDIA Container Toolkit is missing, or (on runveil nodes) the user does not belong to the group owning the device nodes.

**NVIDIA nodes with automatic runtime selection**

On NVIDIA nodes with automatic runtime selection (no `--driver` specified and preference set to `auto`), if Docker / nerdctl cannot provide accelerators to containers, the agent skips them and uses runveil.
The node can continue scheduling with runveil; the “Runtime” row in node details shows `auto (now runveil)`, with an orange message explaining the skipped runtimes and reasons
(`gpu_plugin_missing` if the toolkit is missing, or `gpu_runtime_unconfigured` if it is installed but Docker is not configured), along with installation or configuration commands.

The agent rechecks every minute: when a skipped runtime becomes usable, it switches back automatically if the node has no instances. If instances are present, it shows “Docker is ready and can be switched to” (`gpu_ready`),
and you can switch manually in the “Runtime” dropdown. Ascend nodes do not use this fallback (runveil cannot run CANN on ARM 910B).

After a runtime switch (manual or automatic), instances are recreated in the new runtime. When the new runtime lacks an image, the agent first checks the node's other runtimes that still answer (docker, nerdctl, runveil) for the same image; if one holds it, the image is exported and loaded into the new runtime locally instead of being downloaded again, shown as “copying the image from the local … runtime”. If the copy fails or no runtime holds it, the image is downloaded as usual. Digest-pinned images (`repo@sha256:…`) are always pulled again: a loaded archive cannot carry the registry digest. runveil can export only images in its native store; images registered by the old udocker CLI are downloaded as usual.

**Docker is installed but did not answer when the agent started**

When the agent starts before dockerd at boot, or dockerd is busy pulling a large image and does not answer within 5 seconds, an agent that picks its own runtime waits up to 2 more minutes at startup;
only if Docker still does not answer does it use runveil, and the orange notice on the node details gives the reason (`daemon_not_ready`). It then rechecks every minute and switches back automatically once Docker answers and the node has no instances;
with instances present it likewise shows “Docker is ready and can be switched to”. The agent's systemd unit starts after `docker.service` and `containerd.service` (ordering only,
so a host without Docker is unaffected). An agent user that may not use Docker (not in the docker group) is a different case and is not waited for.

**Nodes with an explicitly selected runtime, or Ascend nodes**

When an explicitly selected runtime (or the runtime on an Ascend node) cannot provide accelerators to containers (`gpu_plugin_missing` / `gpu_runtime_unconfigured`),
the platform automatically stops scheduling on the node: hover over “Scheduling stopped” in the node list to see the reason, and use the “Runtime plugin” row in node details to identify the missing plugin and copy its installation command.
If you manually resume scheduling before resolving the cause, the UI warns you about it, and the platform stops scheduling again on the next report;

on the first report after the cause is resolved (the plugin is installed, or the agent switches to a runtime that does not need a plugin), the platform automatically resumes scheduling.
The platform records who stopped scheduling and lifts only its own stops: a node an operator stopped by hand (including after the platform stopped it) is never resumed automatically.

**When a card needs a host reboot to recover**

When a card reports `reboot_required` or `power_cycle_required` (for example NVIDIA Xid 79, or the driver's recovery action is a reboot), the platform likewise stops scheduling on the whole node:
nothing short of rebooting the host recovers the card, and the other cards behind the same driver may be affected. The rules are the same as above — hover over “Scheduling stopped” to see why;
an administrator may resume scheduling by hand to try a deployment, and while the cause remains the next report (about a minute later) stops it again;
once the host has rebooted and the card is healthy, the first report resumes scheduling automatically. A fault that only needs a card reset (`reset_required`) stops only that card, not the whole node.

### Card Is Passed Through, Has No Driver Bound, or Is Held by Another Driver

Cards in the accelerator list marked “Passed through”, “No driver bound”, or “Held by another driver” are respectively bound to `vfio-pci`
(for virtual machines), not bound to any driver, or bound to a non-vendor driver such as nouveau/pci-stub.

None of these states is a fault. These cards do not count toward the node's GPU count or capacity and do not participate in scheduling, but they appear in the accelerator list. The card list comes from reading `/sys/bus/pci/devices` directly
(with no dependency on the `lspci` command), for all four vendors. Cards listed by the vendor CLI are always in the list; PCI enumeration only adds
cards the CLI cannot report (passed through, missing a driver, or not brought up by the driver), and never removes any.
Verified example: when `npu-smi` on an Ascend host reports `dcmi module initialize failed`, all 8 910B4 cards are still listed.

**The agent does not read the PCI bus when running in a container**: a container can see the host's entire bus, and treating the host's other cards as belonging to this node
would report nonexistent additional faulty cards, so inside a container it recognizes only cards listed by the vendor CLI.

### Card Health Checks

The upper half of the “Health checks” tab in node details is a table of cards by checks, and the lower half is the node's failure records; the “Health checks” tab of a GPU's details has the same two halves for that card alone.

**The checks**: vendor health (the vendor tool's verdict), vendor tool answering, use outside the platform, PCIe link, PCIe errors, Xid (NVIDIA), NVLink (NVIDIA), GPUDirect RDMA (NVIDIA, PPU) and the runtime check.
The first five, Xid and GPUDirect RDMA run once with every report (about a minute), NVLink every 5 minutes and the runtime check every 15 minutes.
GPUDirect RDMA checks, only on a host with RDMA NICs, whether the vendor's peer memory module (`nvidia_peermem`, `alixpu_peermem`) is loaded: not loaded is a warning,
the card stays schedulable, and only PD modes that move KV over RDMA stay off the node; the evidence gives the command that loads it and loads it at boot, and the Diagnose report lists it too. A cell's state:

| State | Meaning |
|---|---|
| Pass / Warning / Fail | What the check last found |
| Skipped | It did not run last time, with the reason: the card is serving, used outside the platform, the platform does not yet know what the node runs, the host cannot be asked right now (the driver is not answering, the runtime is missing, an earlier check has not returned), or the vendor tool did not answer this round |
| Pending | The check has not run since the agent started |
| Stale | No report for more than two report periods (about 2.5 minutes), or the node is offline: the last known result is shown, not what holds now |

A check that applies to none of the node's cards (no NVLink, say) is not shown as a column.

Click a cell to see what the check last did: when, how long it took, the evidence it read, and a command to recheck it yourself; when the last run was skipped, the last run that did run is shown too,
and when an earlier run found a problem, the last such run. The agent answers these from memory and runs nothing; after the agent restarts, each check has to run again before it has any.
No detail is available while the node is offline. “Recheck this card” in the card row's menu runs the runtime check now and records the answer on the card (as Diagnose does).

**Failure records**: one is kept when a check warns or fails, and a problem that persists stays one record (its count and latest evidence are written once an hour, a change of reason at once).
It ends after 30 minutes of passing, and a failure within them continues the same record; one that recurs 10 or more times in 24 hours is marked flapping. Passing checks are not written, so a healthy fleet writes nothing.
Records are kept for 30 days (ongoing ones are never removed), cards are told apart by identity after a replacement, and on the node page a card no longer there is marked removed.
After a server restart, ongoing records continue; counts from up to an hour before the restart may be missing.

### Stopping or Temporarily Allowing a Card by Hand

Expand a card in the accelerator table of node details; its “Scheduling” entry can:

- **Stop scheduling**: the card takes no new deployments; instances already on it are unaffected. It holds until lifted by hand, and the platform never lifts it. The card shows an orange “Scheduling stopped” tag next to its health tag.
- **Allow for now**: for a card the platform judged unusable (a fault, or processes outside the platform) that you want to check yourself. Once allowed it takes new deployments as usual until the first report at least a minute later judges it again: if the problem remains it is stopped again, if it has recovered it stays usable. This is enough to deploy a model by hand and see whether the card really cannot serve. You are asked to confirm first, and the card shows a blue “Allowed for now” tag next to its health tag.

A card passed through to a virtual machine or without a driver is not this node's to use and cannot be allowed (`CARD_NOT_OWNED`).

### Card Is in Use by Processes Outside the Platform

A card marked “In use outside the platform” has compute processes that the platform did not start (such as a vLLM process started by a user).
The card does not participate in scheduling until these processes exit; hover to see process names and GPU memory usage. After they exit, the card is released only once 3 readings in a row (about 3 minutes) find it free,
and until then it still shows the processes last seen: a job somebody starts and stops every minute would otherwise move the card in and out of scheduling with every report. The agent does not make this determination just after restarting, before it receives
which instances it should run—otherwise its own instances would be treated as external processes. It also skips this check if probing failed in the current round;
if the vendor tool just failed to return the inventory, asking it for processes would only incur another timeout.
The platform recognizes its own instances by each card's hardware identity. An instance started by an older version, whose desired state records only device indexes,
gets its identities filled in by a migration when the server is upgraded, and the agent also maps those indexes to cards through the latest probe, so it still counts as the platform's own.

Such a card is also left out of the periodic runtime check and of the recheck after an instance fails to start: someone else's workload should not be disturbed,
and memory held by someone else says nothing about whether the runtime can use the card. The card keeps its last check result, and checking resumes once the processes exit.

While the runtime check's own process (or container) is on a card, and for 15 seconds after it ends, the card is not judged for use outside the platform and keeps the previous round's answer,
so the check never reports a card as “In use outside the platform”. If the card is given to an instance, or a process outside the platform appears, while it is being checked, a failure of that check is discarded:
it is not recorded on the card and earns no backoff. An instance waits for a check in progress on its cards before starting (at most 10 seconds), so the two do not push each other out on a card
only one process may open (an Ascend die without device-share). Diagnose's checks follow the same rules.

Where each vendor's card processes are read from:

| Vendor | Source | Command to verify |
|---|---|---|
| NVIDIA | `nvidia-smi --query-compute-apps` | `nvidia-smi --query-compute-apps=pid,process_name,gpu_bus_id,used_gpu_memory --format=csv` |
| PPU | `ppu-smi --query-compute-apps` | `ppu-smi --query-compute-apps=pid,process_name,pci.bus_id --format=csv` |
| Ascend | The process table at the end of `npu-smi info` (the same call as the probe), matched to the card's Bus-Id by NPU/Chip | `npu-smi info` |
| Hygon | The KFD driver's sysfs (`/sys/class/kfd/kfd/proc/<pid>/queues/*/gpuid`); only a process that created queues on this card counts as using it | `hy-smi --showpids` |
| Apple | Not determined: a single unified-memory GPU that system processes (such as WindowServer) always use, so per-process occupancy is meaningless | — |

### Diagnosis Finds an Unrecognized Accelerator

Doctor reports “Unrecognized accelerator device” (`accelerator_unrecognized`): the host has
a PCI device from a supported vendor, but it matches none of that vendor's recognition rules—usually a new model we have not seen before. Send us the BDF,
`vendor:device`, class, and bound driver from the report so recognition can be added. These devices do not appear in the accelerator list and do not participate in scheduling.

### Read the Diagnostic Report

“Diagnose” in node details and `gpuworks-agent doctor` run the same checks. The UI shows the overview, runtimes,
accelerators (each with BDF and health status), **Action required**, and facts in the current language; the CLI uses English sections:
`-- runtimes --`, `-- accelerators --`, `-- action required --`, and `-- facts --`.
When no GPU is present, it shows `no accelerator tool found (CPU-only node or driver not installed)`.

**Read “Action required”**

Each item in “Action required” provides **Evidence** (the command or call used to collect evidence and its return value) and a **Recheck** command, which you can run on the node.
An **Action** command, such as a reset command, is provided only when the vendor tool itself gives a recovery method.

Checks include accelerator health (see the next section), disk space below 10% free (root partition, model directory, container data directory, and storage),
an unresponsive selected container runtime, accelerator tool execution failures, NVIDIA CUDA initialization failures and multi-card communication check failures, and per-card (per-die on Ascend) runtime check failures (`runtime_unusable`). Cards in use by processes outside the platform are not checked and are listed as not checked.

There are five additional runtime and filesystem checks (see [Runtime Provisioning Failures and Filesystems on Container Nodes](#运行时初始化失败与容器节点的文件系统)):
`runtime_provision_failed` (runtime provisioning failed), `runtime_state_untraversable` (the dedicated runtime user cannot reach the agent directory), `path_noexec`, `path_read_only`, and `path_ephemeral`.

**Other sections**

- In the overview, each path (root, model directory, container data directory, udocker directory, agent directory, and storage) is followed by its filesystem type and mount flags (network, noexec, read-only, container layer).
- Runtime plugins, device access (runveil), device injection method, driver and Fabric Manager, CUDA / peer access / NCCL results,
  the execution identity of udocker workloads (`runtime_identity`), and network filesystems (`network_filesystem`) appear under “Information”, stating facts only.
- Specific probe errors appear only in `agent.log`, not in node details.
- For vendors, probe commands, and the runtime matrix, see [Support Matrix](../reference/support-matrix.md).

## Accelerator Health States Such as “Reset Pending” and “Memory Fault”

**Symptoms**: The health column in the accelerator list in node details is not “OK”; hover over ⓘ to see the explanation, evidence, and recheck command.

**Behavior**: On every probe round, the agent assigns a health level to each card from the vendor tool's fields and stores the evidence fields unchanged (with the same field names as the vendor tool):

| Level | Scheduling | Meaning |
|---|---|---|
| OK | As usual | No findings, or only resolved historical errors (`memory_error_history`) |
| Warning | As usual, but used only when there are not enough healthy cards (evaluation results include `degraded_gpus_used`) | `memory_repair_pending` (reset pending), `vendor_health_warning` (vendor tool Warning), `pcie_link_degraded` (PCIe link degraded), `pcie_errors` (recovered PCIe errors), `nvlink_down` (NVLink down) |
| Fault | Stopped | `memory_uncorrectable_errors`, `memory_remap_failed`, `reset_required`, `reboot_required`, `power_cycle_required`, `vendor_health_fault`, `runtime_unusable` (the runtime cannot use this card), `driver_init_failed` (the driver did not bring it up), `vendor_cli_unanswered` (the vendor CLI did not respond, so information is unreadable), `pcie_fatal_errors` (fatal PCIe errors) |

Whether a card is usable is a conclusion the server determines once when saving the card (combining health level, card ownership, and usage outside the platform).
The API exposes this as `schedulable`. Both the UI and scheduling read this field instead of deriving it independently. **Do not read `healthy`**:
a card passed through to a VM has `healthy` set to false even though it has no fault, while a card occupied by external processes has `healthy` set to true but cannot be scheduled.
The reasons a card is unavailable are in its own `health_status`/`health_code`/`ownership`/`foreign_processes` fields.

A problem the vendor tool reports (those in the Warning and Fault rows above that come from the vendor tool) clears only after 3 clean readings in a row (about 3 minutes); until then the card keeps its level,
and its evidence carries a `Recovery` field (such as `1 of 3 clean readings`) showing the progress: a card whose counters flip back and forth would otherwise move in and out of scheduling with every report.
To use the card meanwhile, “Allow for now” on it.

Evidence fields for each vendor:

| Vendor | Command | Fields and interpretation |
|---|---|---|
| PPU | `ppu-smi --query-ppu=...` | `remapped_rows.pending` / `retired_pages.pending` is Yes → reset pending; `ecc.errors.uncorrected.volatile.dram` > 0 → GPU memory fault; `remapped_rows.failure` is Yes → GPU memory fault; `xid.correctable.ppu_reset` / `os_reboot` / `cold_reboot` has a value → reset / host reboot / power cycle required; `ecc.errors.uncorrected.aggregate.dram`, `retired_pages.double_bit.count`, and `remapped_rows.uncorrectable` are cumulative values that survive resets and serve only as historical records |
| NVIDIA | `nvidia-smi --query-gpu=...` | `ecc.errors.uncorrected.volatile.total` > 0 → GPU memory fault; `remapped_rows.pending` / `retired_pages.pending` is Yes → reset pending; `remapped_rows.failure` is Yes → GPU memory fault; `ecc.errors.uncorrected.aggregate.total` serves only as a historical record; unsupported fields return `[N/A]` and do not affect the assessment; the recovery the driver asks for, `gpu_recovery_action` (R550 and later): Reset → reset required, Reboot → host reboot required, cleared by the driver once done. Xid events are read from the kernel log `/dev/kmsg` (needs root): 79 (the card fell off the bus) → host reboot required; 61, 62, 95, 119, 120, 140 → reset required, both held until a runtime check passes on the card afterwards (or the host reboots); the application's own errors such as 13, 31, 43, and Xids with a state field such as 48, 63, 64, are only logged. NVLink is read every 5 minutes with `nvidia-smi nvlink -s -e` (hourly on hosts without active NVLink); fewer active links than the card has had, two readings in a row → NVLink down (warning), and the topology is read again at once, so the scheduler stops grouping the card with the cards it was linked to |
| Ascend | `npu-smi info`, then `npu-smi info -t health -i <NPU> -c <Chip>` if the result is not OK | Health is Warning → warning; Alarm / Critical → fault; error codes and messages appear unchanged; consult Huawei documentation for their meanings |
| Hygon | Driver sysfs `/sys/bus/pci/devices/<BDF>/ras/` (the same source as `hy-smi --showrasinfo all` and `--showpagesinfo`, no process started) | `ue` > 0 in `umc_err_count` → memory fault; `ue` > 0 in another block (gfx, sdma, …) → vendor fault; a P (pending retirement) row in `gpu_vram_bad_pages` → reset pending, an F (retirement failed) row → memory fault, only R (retired) rows → history; `ce` (correctable) is evidence only. No verdict when the driver has no RAS files |
| All vendors (PCIe) | sysfs of each device between the card and its root port: `current/max_link_speed`, `current/max_link_width`, `aer_dev_{correctable,nonfatal,fatal}` (no process started; reading a port's type from config space needs root) | A link with fewer lanes than both ends support → PCIe link degraded (warning); a lower speed than both ends support counts too, but only on a card one of this node's instances uses and that is not asleep (idle cards and switch uplinks lower their speed on purpose: a K100's switch uplink idles at 2.5 of 8 GT/s); raised after 3 reports in a row and cleared after 3 good ones; on a virtual machine only the card's own link is judged. AER counters are counted from when the agent started: new non-fatal errors → recovered PCIe errors (warning, cleared after 15 minutes without new ones); new fatal errors → fatal PCIe errors (fault, until a runtime check passes on the card afterwards). On platforms with firmware-first error handling the counters stay at 0 and show nothing |

**How to verify and resolve**: Use the verification command generated for the card's PCI address in the health status description. The following example uses a PPU pending reset;
replace `0000:7e:00.0` with your card's address.

1. Check `Remapped Rows → Pending` on the node, and check whether any compute processes are still running on the card:

   ```bash
   ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0
   ppu-smi --query-compute-apps=pid,process_name,pci.bus_id --format=csv
   ```

2. Confirm that workloads on the card have stopped, then reset it as a user with device management permissions:

   ```bash
   ppu-smi -r -i 0000:7e:00.0
   ```

   If the reset fails and the host requires a reboot, first arrange downtime for the other services on that host.
3. Wait for the next node status report (up to approximately 60 seconds), then check health again. Contact the hardware vendor if row remapping fails or error counts continue to increase after the reset.

## Runtime Initialization Failures and Filesystems on Container Nodes

**Symptoms**: All deployments fail with `runveil runtime unavailable: <reason>`; earlier versions may show
`udocker runtime unavailable` or `prepare udocker ownership`. Node diagnostics may also report
`runtime_provision_failed`, `runtime_state_untraversable`, `path_noexec`, `path_read_only`, or `path_ephemeral`.
If launch logs or instances report `fork/exec …/udocker/bin/proot-…: permission denied`, check `runtime_state_untraversable` first.

### Identify the Affected Path First

1. In **Node details → Diagnosis**, review the full error, evidence, probe process, and verification commands. The CLI `doctor` runs as a separate process and cannot see the running agent's initialization state or paths, so it cannot replace this step.
2. On the node, check the filesystem and directory permissions for the reported path. The following RunPod example uses sample directories; replace them with your actual directories:

   ```bash
   findmnt -T /workspace/gpuworks/udocker
   ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state
   gpuworks-agent doctor
   ```

SSH installation also leaves an `udocker-debug` script in the login user's home directory. It uses the same `UDOCKER_DIR` and PRoot to help reproduce udocker commands.

### Resolve the Specific Error

**Filenames cannot contain colons**

The error is `<directory> does not allow ':' in file names … reinstall the agent with an install directory on a local filesystem`.
During initialization, udocker tries to create a file containing `:`. Image layers such as `sha256:<hex>` and Debian files such as `/var/lib/dpkg/info/<package>:amd64.list` require these filenames, so you cannot bypass this restriction.

Reinstall the agent on a local filesystem that supports these filenames. RunPod's `/workspace` network volume does not meet this requirement. Use the container disk and reserve enough space for unpacked images.
**The container disk is lost when the container is recreated**, so images must be pulled again. Prefer a local persistent volume when a suitable one is available.
See [Why the udocker Node Installation Directory Cannot Be on a Network Volume](connect-nodes.md#为什么-udockerrunveil节点的安装目录不能放在网络卷上).

**Paths are non-executable or read-only**

| Check result | Action |
|---|---|
| `path_noexec` (error) | The mount containing the udocker or agent directory has `noexec`; adjust the mount or use a directory that allows execution |
| `path_read_only` (error) | The mount containing the model, udocker, or agent directory is read-only; adjust the mount or use a writable directory |
| `path_ephemeral` (warning) | The model, udocker, or agent directory is in the container's own overlay layer and is lost on recreation; choose a directory according to the persistence requirements above |

Path checks read `/proc/self/mountinfo`; use `findmnt -T <path>` to verify.
Container-layer checks apply to nodes with `/.dockerenv` or `/run/.containerenv`.
Storage and `/var/lib/docker` are not checked for read-only access: shared model libraries can be read-only, and dockerd manages the Docker data directory.
Network filesystems such as NFS, CIFS, and CephFS are not faults in themselves. Their paths are only listed in `network_filesystem`; assess the specific error.

**Cannot switch to the dedicated user**

By default, an agent running as root runs runveil (udocker) workloads as the dedicated `udocker` user.
If the filesystem refuses to change ownership of new files, such as when NFSv4 on RunPod's `/workspace` returns `Operation not permitted`, the agent runs them as its own user instead.

The log message `dedicated runtime user is unusable; running as the agent's own user` and the `runtime_identity` diagnostic record this fallback and its reason.
This message is not a fault in itself. When the agent runs as root, files written to the model directory also belong to root.

**The dedicated user cannot reach the agent directory**

An agent running as root starts workloads as the dedicated `udocker` user. The agent opens its own state directory (0711 or 0710), but the directories **above** it belong to the operator:
if one of them does not let `udocker` enter (typically an installation under `/root/…` or a private 0700 home), every launch fails with
`fork/exec …/udocker/bin/proot-…: permission denied`, while everything else (image management, `runveil ps`) keeps working, so the cause is hard to see.

`runtime_state_untraversable` (error) gives the first blocking directory, its permissions and owner, the runtime user it was judged for, and the fix. To resolve it:

- Run the `chmod o+x <directory>` shown in the evidence: it grants search only, no listing or writing.
- Or reinstall the agent with an installation directory outside that directory (for example `/opt/gpuworks`).

Before installation, the SSH precheck reports the same condition as a failure or warning in `Install directory path access` (see [Installation Directory Checks](connect-nodes.md#安装目录检查)).
It is a failure when the probe can establish that the SSH user is root, that the agent will use runveil and the dedicated user, and that the directory really is closed to that user. SSH installation and cloud one-click installation then stop before uploading the agent (400 `NODE_INSTALL_DIR_UNTRAVERSABLE`), and the error names the blocked directory and the `chmod o+x` command. Uncertain cases, such as a non-root SSH user, docker or nerdctl on the host, an install that explicitly selects another runtime, or a directory with an ACL, are only warnings and the installation continues.
The node page diagnostics, the agent's runtime log warning, and the CLI `gpuworks-agent doctor --state <directory>` (when that directory already contains a `udocker` directory) use the same verdict.
After the fix, run the diagnosis again; restarting the agent is unnecessary.

**Other initialization failures**

`runtime_provision_failed` preserves the full errors for directory creation, PRoot extraction, chown, filenames, and other operations; chown errors include the command's stderr.
Follow the evidence and verification commands in the error. If image pulling encounters this error, it fails immediately with `udocker runtime unavailable: <reason>`, without trying image sources one by one or reporting `all image sources failed`. Fix the node environment first.

### Confirm Recovery

After fixing the issue, wait for the error cache to expire and repeat the original operation, or restart the agent and retry.
The first failure is cached for 30 seconds; subsequent failures are cached for 1, 2, and 4 minutes, up to a maximum of 5 minutes. During the cache period, calls return the same error without repeating initialization.
Only the next call after the interval retries initialization; success clears the error. Opening diagnostics or running `doctor` does not trigger initialization.

## Nodes Without Docker (runveil)

**Symptoms**: Docker is not installed on the node. The UI displays the runtime as runveil (the driver name `udocker` is retained).

**How to verify and resolve**

1. First confirm the actual workload user in the node details diagnostics. Add that user to device groups only if device access failures are reported.
   The following example applies to a node whose workload user is `udocker`:

   ```bash
   sudo gpuworks-agent grant-device-access --user udocker --create-user
   ```

2. Inspect containers and their logs on the node. Run `exec` if you need to enter a container for further investigation:

   ```bash
   gpuworks-agent runveil ps -a
   gpuworks-agent runveil logs -n 200 '<container-name-prefix>'
   ```

   ```bash
   gpuworks-agent runveil exec '<container-name-prefix>'
   ```

The `runveil` subcommand reads the state directory directly. If state differs from the default (`GPUWORKS_STATE_DIR`, otherwise `/opt/gpuworks`), set it accordingly or pass `--state` before the subcommand
(the installer writes state into `--state` in the systemd unit's `ExecStart`; connection configuration is in `gpuworks-agent.conf` in that directory).
See [CLI](../reference/cli.md).

**Behavior**: When `--driver` is omitted, the agent automatically selects docker → nerdctl → runveil in that order (macOS only supports `host`: the engine runs as a host process,
while Docker Desktop containers run in a VM without Metal). On NVIDIA nodes, docker / nerdctl runtimes that cannot expose accelerators to containers are skipped (see [GPU Detection and Runtime Plugins](#gpu-没被探测到或厂商工具未响应)).
runveil requires only Linux on amd64 / arm64;
the default launcher does not require Python. If none of the three runtimes is usable, the log reports
`no usable container runtime (docker/nerdctl/udocker); deployments disabled on this node`. The node can still come online
and report GPUs, but scheduling rejects it with `runtime_unavailable`, and the dGPU reason is `no usable docker/udocker runtime on the node`.
Use `--driver` or `GPUWORKS_RUNTIME_DRIVER` to force a runtime (`docker|udocker|nerdctl|host|none`; `host` is only available on macOS and `udocker` only on Linux.
If you force a driver unsupported by the local OS or supply an invalid value, the reason is recorded only in the agent log, and the node continues running without a runtime),
or use `PATCH /api/v1/nodes/{uuid}/runtime` to change the preference (`auto/docker/nerdctl/udocker`).

**Possible causes (containers do not start on runveil)**

1. A non-root runtime lacks device permissions: `doctor` shows `device access: FAIL`.
2. The image was registered by the old udocker CLI: runveil cannot use it as a sync source; pull or import it again.
3. The workload uses capabilities unsupported by runveil: RDMA compute networks (`COMPUTE_NETWORK_RUNTIME`) or PPU dGPU (verified only on Docker).
4. When a workload started before the agent restarted exits, its exit code is unavailable and is always reported as 1 (`… (code 1)`), so OOM cannot be identified either.
   Workloads started by the current agent process report their actual exit codes (a signal termination is recorded as 128 + signal number; an OOM kill is 137).
5. Insufficient disk space: before unpacking an image, if free space is less than the combined image layer sizes + 2 GiB, the operation fails immediately (`not enough disk space to unpack the image …`).
   Failure to copy host driver libraries into the container during compatibility fixes also fails immediately (`copy NVIDIA driver library libnvidia-ml.so.1 into the container: … no space left on device`).
   Before the container process starts, it also fails immediately if space available to a regular user in the container or log directory is less than 1 GiB (`not enough disk space to start the container: …`):
   the agent runs as root and can use filesystem space reserved for root, but the engine runs as a regular user and cannot write there.
   Earlier agent versions continue starting after a copy failure, and the engine exits immediately (vLLM reports `Failed to infer device type`). When the disk is full, even container output cannot be written.

When a container exits after starting, the failure message includes the engine's last error line, for example, `container exited (code 2): vllm: error: unrecognized arguments: …`.
If there is no output at all, it says `… with no output`. This usually means the disk is full and even logs cannot be written.

## Model Downloads Stall, Fail, or Leave Incomplete Directories

**Symptoms**: A model remains in `downloading` on a node for a long time, or changes to `failed`, `insufficient_disk_space`, or `download_required`, and deployment creation is rejected.

### Check Completeness First

Check the status and message in the model's node list, and review `source=agent` output for the same period on the logs page.
Statuses include `pending`, `downloading`, `syncing`, `present`, `registered`, `failed`, `insufficient_disk_space`, `download_required`, and `absent`; **there is no `partial` status**.

| Observed status or file | Interpretation |
|---|---|
| `present` | Downloaded and verified as complete; all files and sizes match the manifest in `.gpuworks-model-complete.json` |
| `registered` | A local path is registered and ready to use without waiting for `present`; later checks may update the status, but the acquisition method remains local registration |
| `<file>.part`, `<file>.part.state` | Intermediate download files; retries attempt to resume, so these files do not indicate completeness. When another tool (such as `hf download`) has finished the file of the same name at the size `.part.state` records, the two are leftovers: validation and scans no longer count them as unfinished, and the next platform download removes them |

If a model from a local source is incomplete on the node, deployment creation returns 409 `MODEL_SOURCE_UNAVAILABLE`.
For public sources, creation attempts to read metadata. An unreachable site may leave metadata empty without preventing creation. **Successful creation does not mean weights are available**; acquisition must still finish before startup.

### Choose a Resolution Based on the Error

**Insufficient disk space: `insufficient_disk_space`**

The message is `insufficient disk space at <path>: available=… required=…`.
Check space at that path. Reserve **1 GiB** in addition to the download size, then retry after freeing enough space.

**The site is unreachable, or all sources are forbidden**

- `list files:` indicates a failure to retrieve the file list; HTTP errors include `http <status-code>`. `download <path> from <url>:` indicates a download failure; HTTP errors include `unexpected status <status-code>`. Check the host in the logs and network connectivity from the node to the site.
- For networks in China, configure a proxy or site rule under **Settings → Image & model pulls** to direct `https://huggingface.co` to an available mirror or internal proxy. Use **Test connectivity** in the rule page to verify. See [Configuration Examples](configure-pull-settings.md#常见网络配置示例).
- `weights_fetch_forbidden` / 409 `WEIGHTS_FETCH_FORBIDDEN` means the rules applicable to the node forbid every candidate (fallback to the original source is disabled and no alternative addresses are available). Adjust the rules, register a path on a node that already has the model, or use shared storage.

**A private or gated repository returns 401 / 403**

Add a token for `huggingface.co` under **Settings → Image & model pulls → [Credentials](configure-pull-settings.md#凭据)**.
This method is verified only for Hugging Face; private ModelScope repositories are not yet verified. Without credentials, 401 / 403 puts the current address into cooldown and switches candidates; the status becomes `failed` only after all candidates fail.

**Metadata is missing after registration**

If the response includes `metadata_notice` (`MODEL_METADATA_UNREACHABLE` / `MODEL_METADATA_FORBIDDEN`), check the control plane's outbound access.
Select **Used by the management node for model metadata** on an available site rule, and select **Used for management node outbound access** on the proxy if needed.

**The source node is offline, or peer sync fails**

- The explicitly selected source node is offline or its copy is incomplete: the status remains `download_required`, without automatically switching sources.
- The source node is unreachable during automatic copying within a sync group: the target switches to a public download, and copying between this node pair is not retried for **3 hours**. If there is no public source, the status remains `download_required`.
- Public access is unavailable but another node has a copy: use peer sync. If the nodes have no common network, it reports `NO_STORAGE_PATH`. See [Download and Sync Models](download-and-sync-models.md) and [Import from HTTP](import-from-http.md).

When a model is `failed` or `download_required` on a node, the deployment wait reason is `model_unavailable`, with the model, node, and error specified. Replicas with automatic node selection skip that node and try placement elsewhere.

### Long Downloads: Check Source Switching and Resume Progress

Candidates consist of the alternative addresses in site rules and the site itself. The host in progress and failure messages identifies the source currently being tried and the reason for switching. See [How Nodes Try Sources in Sequence](configure-pull-settings.md#节点怎样依次尝试).

| Failure type | Automatic handling |
|---|---|
| File list retrieval fails | If the address is unreachable, returns an HTTP error, or provides an unparseable list, the current address enters a 30-minute cooldown and the next candidate is tried immediately; rate limiting (429, 503) is handled as in "Source rate limiting" below |
| Source rate limiting | If the file list or a download returns 429 or 503, wait for the reset time in the response's `Retry-After` or `RateLimit` header (at least the backoff interval, at most 5 minutes and 1 second) and retry the same address; progress shows "rate limited by the source". These count toward the attempts in the next row, and only after they are exhausted does the address enter cooldown and switch; in the worst case, one file can wait about 75 minutes at the last candidate |
| Non-retryable source error | If a download returns another HTTP error (including 404 and 401/403), TLS certificate verification fails, or a similar error occurs, the original address is not retried. It enters a 30-minute cooldown and the next candidate is tried; the operation ends if there is no next candidate |
| Retryable transfer error | For connection refusals or resets, TLS handshake timeouts, EOF, and similar download errors, retry the same address first: up to 6 attempts per file, or 16 for the last candidate, including the initial attempt. Backoff starts at 1 second and caps at 15 seconds. Only after all attempts fail does the address enter cooldown and switch |
| Individual request times out | If one HTTP request lasts more than 15 minutes, disconnect and attempt to resume; this counts toward the address's total attempt limit |
| No data for an extended period | If no response headers or new data arrive for 5 consecutive minutes, disconnect and attempt to resume. If this happens twice consecutively at the same address and another candidate is available, enter cooldown and switch. The last candidate does not switch again and remains subject to the 16-attempt total limit |

**If the source does not support Range, every resume attempt restarts the download from the beginning**. Large files on slow links may repeatedly restart until the final address exhausts its attempts. The 15-minute limit applies to an individual HTTP request, not the entire download task.

### Retry After the Fix and Confirm the Result

To retry a public download, call `POST /api/v1/models/{id}/download` with body `{"node_uuid":"<uuid>"}`. The status returns to `pending`.
Failed deployments do not automatically restart downloads. Confirm `present` after completion; for a registered local path, confirming `registered` is sufficient.

Public downloads have no cancellation API. To clean up, call `DELETE /api/v1/models/{id}/nodes/{uuid}`; it returns 409 `MODEL_IN_USE` if active deployments still use the model.

## Image Pull Failures

**Symptoms**: An instance remains in `pulling`, events show `image source failed: <ref>: …`, and eventually `all image sources failed: …`; or a node's status on the images page is `failed`.
Image statuses include `absent`, `pulling`, `present`, `syncing`, and `failed`.

### Identify the Failed Source First

Open the instance event timeline and review each reference's error in **Image source failed** (`image_pull_retry`), then follow the relevant resolution below.

| Error or symptom | Next step |
|---|---|
| The node cannot reach the registry | Check proxies, DNS, and firewalls. Docker proxy settings must be configured in dockerd; see [Proxies and dockerd](configure-pull-settings.md#dockerd) |
| A mirror or Harbor is unavailable, or a tag is missing | Check the failed reference and tag, then update the alternative address in the source rule |
| A private registry requires login | Configure [Credentials](configure-pull-settings.md#凭据) and check the tag |
| `insufficient_runtime_disk` or `insufficient disk space at …` | Check space in the node's container data directory and the archive import destination |
| Archive import or peer transfer reports `this transfer stopped reporting progress` | Check the node and transfer, then retry. This failure occurs after 15 minutes without progress reports in the `syncing` state |

For the last case, if the node actually has the image, the status changes to `present`. Registry pulls are not subject to this 15-minute timeout.
Without public access, point source rules to an internal registry or archive URL, or use peer sync. See [Deploy in Offline Environments](deploy-offline.md) and [Import from HTTP](import-from-http.md).

### Sources Are Forbidden, or Retry After Changing Settings

Change the mirror under **Settings → Image & model pulls → Source rules**. Invalid rules return `PULL_SETTING_INVALID`.
If the rules applicable to the node have no alternative addresses and disable fallback to the original source, no candidates are available:

- Image operations return `IMAGE_SOURCE_FORBIDDEN`.
- The deployment scheduling reason is `image_source_forbidden` (with `node` and `image`); replicas do not start on that node.

After fixing the issue, call `POST /api/v1/images/{id}/nodes/{uuid}/sync/retry`, then confirm that the image status on the node is `present`.
Retries recalculate candidates from the current settings. You cannot specify a registry in the retry API; doing so returns `IMAGE_SOURCE_REMOVED`.

If a rolling tag (such as `latest` or `nightly`) still has old content, click **Pull again** in the node row on the image details page. This calls
`POST /api/v1/images/{id}/nodes/{uuid}/sync` with `"force":true`. After completion, check the new digest in the Image ID column.

### Why Candidates Switch and When Pulls Are Considered Stalled

Startup, replay, replica scale-out, multi-node pre-pulling, and “Pull to node” on the images page use the same candidate order. Deployments do not select sources separately:

1. Alternative addresses in the [source rules](configure-pull-settings.md#来源规则) applicable to the node, in rule order.
2. The template's `image_sources`, only when the deployment uses the template's own image; see [Template Syntax](../reference/inference-catalog-spec.md#image_sources模板声明的镜像来源).
3. `registry_sources` registered in the catalog, such as image copies on Docker Hub, quay.io, or Huawei Cloud SWR.
4. The canonical address.

The node probes `https://<host>/v2/` for the image's own copies and sorts them by reachability and latency. Probes time out after 5 seconds, and results are cached for 30 minutes. The next candidate is tried only after the previous one fails.

On nerdctl, layers larger than 1 GiB report progress in 0.1 GiB increments, so the stall timeout is extended to approximately 17.5 minutes. If `Progress output not understood, stall check off` appears, stall detection is disabled for that attempt. The agent log preserves the original line to help investigate formatting changes between runtime versions.

- Download byte progress has been reported, the download is incomplete, and no progress occurs for 3 consecutive minutes: switch candidates. Docker usually reports per-layer byte progress and is also subject to this check.
- A download that makes progress but stays very slow: if it averages under 64 KiB/s over any 5 consecutive minutes and another candidate remains, the node switches candidates; the last candidate is never abandoned for this. Large nerdctl layers that show progress in coarse steps are not judged this way. On runveil nodes the connection level acts first: a single connection that falls clearly behind is replaced (see [Registry Rate Limits or Refused Connections](#仓库限流或连接被拒)).
- No byte progress is available, unpacking is in progress, or image registration is in progress after all bytes have arrived: the 3-minute check above does not apply. Each candidate has up to 60 minutes from the start of pulling to completion of unpacking.
- Network or transfer failure, or a stall: the registry enters a 30-minute cooldown on that node and is placed last among latency-tested candidates, but is not removed. Missing images and authentication or permission denials also trigger candidate switching, but do not put the entire registry into cooldown. Fix the reference or credentials first; there is no need to wait for cooldown. An unavailable runtime immediately ends the pull; restore the runtime first.

### Registry Rate Limits or Refused Connections

When a runveil (udocker) node pulls an image, the agent log (`<state>/agent.log`) records the decisions of the connection budget. In the cases below, first check whether the registry is limiting connections, rather than assuming the network is slow:

- `download connections backed off` appears repeatedly, with the reason `throttled` (the registry answered 429/503 or refused connections; refused connections are waited on and retried) or `connection resets`: the registry or a proxy is limiting the source address, and the agent has already halved the connections and is waiting. Usually nothing needs to be done. If the pull is noticeably slower but keeps making progress, the budget has dropped to a level the registry accepts.
- The pull fails with an error containing `429` or `503` after several retries: the registry keeps refusing. Check the per-address connection and request-rate limits of the private registry (Harbor, SWR, ACR) or the proxy and raise them if needed, or cap the node with `--image-pull-max-connections 2` (range 0 to 32) and restart the agent. When there are several candidate sources, the next one is tried after a failure.
- `registry ignores Range: reading the layer whole` appears: the registry or proxy does not support Range requests for this layer, so it is read whole over one connection and is limited to the speed of a single connection. Use a source that supports Range or adjust the proxy.
- `download connection recycled` appears (only after the same reason holds in two windows in a row) with the reason `far below its siblings`, `collapsed from its own peak` or `below the absolute floor`: one connection was clearly slower than the others of the same pull and was replaced by a new one that continues from the same position. This is normal self-healing. A slow link as a whole does not trigger replacement; then the slowness is the speed of the link itself, see [Why Candidates Switch and When Pulls Are Considered Stalled](#候选为何切换何时判定卡住).

### Additional Checks for Digest References

When a template pins `repo@sha256:…`, the node starts the container directly from the pulled candidate, including candidates already available locally. Runtimes cannot tag digest references, so the reference is not changed back to the canonical one.
**The platform does not verify that the candidate digest matches the template**. Template authors and deployers must confirm the content at alternative addresses. Failure messages list each candidate's reason by reference.

Manual pulling on the images page may return `digest image refs are not supported` if a candidate contains a digest. This is a limitation of the manual pull path, not a lack of digest support in deployments.

## Deployment Rejected, Insufficient Estimated GPU Memory, or No Suitable Node

**Creation behavior**: At creation, `POST /api/v1/deployments` does not schedule GPU memory based on current occupancy and returns 201 on success (exceptions: single-node creation evaluates the candidate pool as if idle;
if no hardware can accommodate the workload, it returns 400 `DEPLOYMENT_NEVER_FITS`. Single-node creation with `placement_mode: "auto"` also reruns the placement plan from evaluation;
if it no longer fits, it returns 409 `AUTOMATIC_PLACEMENT_STALE`). GPU memory and placement conclusions appear in
the `POST /api/v1/evaluate` response and in `DEPLOYMENT_UNSCHEDULABLE` deployment events after creation (`params.scheduler` contains the scheduler's reason).

| Error code | HTTP | Meaning and action |
|---|---|---|
| `AUTOMATIC_PLACEMENT_STALE` | 409 | Resources have been occupied since evaluation, or nodes no longer meet the node requirements; evaluate again |
| `NODE_REQUIREMENTS_UNMET` | 400 | The selected node does not meet the deployment's node requirements; `params.detail` identifies the key and required values |
| `UNIT_PLACEMENT_NONE` | 409 | No group of nodes can take an automatically placed multi-node or PD request (create, evaluate, or scale) now. `params` gives the node total `nodes` and how many each check left out: `unmet` (node requirements not met), `cordoned`, `offline`, `busy` (not enough free cards), `no_network` (no usable compute network); `largest` is how many nodes the largest group has. Satisfy the item with the largest count, or place manually |
| `AUTO_PLACEMENT_TP_REQUIRED` | 400 | An automatic multi-node request has no `placement.tp`, so the platform does not know how many free cards each node needs; fill in TP (and PP) |
| `AUTO_PLACEMENT_PD_MULTI_NODE` | 400 | An automatically placed PD instance must fit on one node; an instance with `followers` must name its nodes (`params.role` is the role) |
| `PLACEMENT_MODE_SHAPE` | 400 | `PUT /deployments/{id}/placement-mode` was used on a single-node deployment; its placement is in its node requirements, so edit those |
| `NODE_SCHEDULABLE` | 409 | Force drain applies only to cordoned or draining nodes; cordon or drain first, or removed replicas are placed back on the same node |
| `DEPLOYMENT_NEVER_FITS` | 400 | When creating a deployment or editing node requirements, no node in the candidate pool can accommodate one replica even when idle (too few cards, insufficient memory per card, unsupported precision, or inability to carry serving traffic). `params` gives the number of candidate nodes, the number of hardware configurations, and the first node's reason. Adjust parallelism, quantization, or node requirements according to the specific reason; do not always reduce TP. See the `no_fit` guidance below |
| `NODE_REQUIREMENTS_INVALID` | 400 | The node requirements themselves are invalid (a misspelled system label key, two vendors, a conflict with `target_vendor`, or `gpuworks.ai/node` specified for a multi-node/PD deployment) |
| `LAUNCH_CONFIG_CONFLICT` | 400 | The same key has different values in a form field and custom arguments; keep only one (parameters are `key`, `field_value`, `flag`, and `extra_value`) |
| `ACQUISITION_CANDIDATE_STALE` | 400 / 409 | The storage specified in the request no longer exists or no longer contains the model, or the selected weight source is no longer in the template; evaluate again. The server does not substitute another option |
| `MODEL_SOURCE_UNAVAILABLE` | 409 | The local model has no available source on the target node, or source validation failed; check the error details rather than treating it as a public download timeout |
| `TEMPLATE_MODEL_MISMATCH` | 400 | The model a template deployment selects (`params.model_id`) has a repository id that matches none of the template's weight sources (`params.repo_id` and `params.revision` are the source the template resolved); the platform does not switch to another model. Select a model of that repository; for a local model, change its source id to the template's repository id in the model details |
| `DISTRIBUTED_*`, `REPLICAS_*`, `MASTER_PORT_INVALID` | 400 | Invalid multi-node / replica parameters |

**Insufficient GPU memory** (`schedule` reason codes; see the evaluation response and `schedule.explain`):

| Reason code | Meaning |
|---|---|
| `no_fit` | Fixed startup requirement per card > available GPU memory; parameters include `required_gib`, `available_gib`, `tp`, and `gpu_memory_utilization` |
| `insufficient_empty_gpus` / `insufficient_shared_gpus` | Not enough idle whole cards / shareable cards |
| `pinned_gpus_unavailable` | The cards a manually placed PD instance is pinned to are held by other deployments; the parameters give `node`, `gpus` (the pinned card indexes), and `holders` (the deployments holding them). The instance does not switch to other free cards on the node but waits for the pinned cards: stop or move the deployments holding them, or switch this deployment to automatic placement (`PUT /deployments/{id}/placement-mode`) so the platform uses free cards. When a pinned card is held outside the platform, is not healthy, or is free but still does not fit, the corresponding reason code applies |
| `waiting_for_yield` | The replica fits once deployments sharing its cards by yield policy give them up, and waits for them; the parameters give `node`, `holders` (each holding deployment with `at_once`, or `idle_sec` and for yield-on-a-turn `max_serve_sec`: the terms it gives way to this deployment on) and `instances`. It resolves by itself when the holders sleep; see [How a New Deployment Gets onto a Shared Card](scheduling-center.md#新部署怎样放到共用的卡上). A holder with never yield is never asked to give way |
| `reserved_for_sleepers` | The deployment does not fit only because sleeping deployments keep their cards for a request's wake; the parameters give `node`, `gpus` (the cards it would have used) and `sleepers` (each sleeper with `deployment` and its yield policy `policy`). A sleeping deployment that shares cards keeps them only when a request can wake it; a sleep you or a schedule ordered (a held sleep) keeps none. Fix: pick another card, turn on sleep for this deployment with a yield policy that can share cards, or put the deployment holding the cards to sleep by hand; see [How a New Deployment Gets onto a Shared Card](scheduling-center.md#新部署怎样放到共用的卡上) |
| `does_not_wait_for_yield` | Deployment form preview only: it fits once the deployments holding its cards sleep, but this deployment never yields and does not wait for them; the parameters give `node`, `gpus`, `holders` (holders that give way to a deployment sharing cards) and `blockers` (holders that never yield), each with `deployment` and its yield policy `policy`. Fix: turn on sleep for this deployment with a yield policy that can share cards; with `blockers`, also pick another card or give them a yield policy that can share cards |
| `held_by_non_yielding` | Deployment form preview only: it fits once the deployments holding its cards sleep, but those in `blockers` never yield and do not give their cards to this deployment; the parameters give `node`, `gpus`, `blockers` (each with `deployment` and `policy`) and `holders` (holders that do give way, with the terms of `waiting_for_yield` as well). Fix: pick another card, or turn on sleep for the deployments in `blockers` with a yield policy that can share cards |
| `no_free_gpu` | No idle GPUs |
| `no_healthy_gpu` | None of the node's own cards has a health level that permits serving |
| `no_owned_gpu` | The node has no cards available for its own use: it has none, or all are passed through to VMs, unbound from drivers, or held by other drivers |
| `gpus_held_outside_platform` | The cards themselves are healthy, but all usable cards are occupied by processes outside the platform |
| `tp_head_count_not_divisible` | The attention head count is not divisible by TP |
| `sleep_residue_blocks_startup` | Residual GPU memory from sleeping instances blocks startup |
| `engine_ports_exhausted` | The node's engine port range is exhausted |
| `insufficient_disk` / `insufficient_runtime_disk` | Insufficient space on the weights disk / runtime disk |
| `runtime_unavailable` / `serving_unavailable` | The node has no container runtime / no network capable of carrying serving traffic |
| `image_source_forbidden` | The pull source rules applicable to the node leave no source from which to pull the image (parameters `node` and `image`); see [Image Pull Failures](#镜像拉取失败) |
| `partition_unavailable` | A dGPU backend is required, but none is ready on the node; see the dGPU section |

Fixed startup requirement = weights / TP + multimodal encoder / TP + reserve, **excluding KV**; capacity = GPU memory × `gpu_memory_utilization`
(for dGPU, the requirement is the GPU memory limit plus reserve, and capacity is the remaining available memory on the card). For `no_fit`, increase TP, raise `gpu_memory_utilization`, select a node with more GPU memory, or stop / put to sleep
other deployments occupying the cards (evaluation suggests actions such as “Sleeping X can release approximately N GiB”). Reducing `max_model_len` affects only the KV estimate
and **cannot** resolve `no_fit`.

**How to verify**: First check the evaluation result on the deployment page (GPU memory breakdown and candidate reasons), then use `GET /api/v1/deployments/{id}/events` to inspect scheduling events after creation.
For estimation principles, see [Scheduling and GPU Memory Estimation](../explanation/scheduling-and-estimation.md); for card selection and labels, see [Deploy Models](deploy-models.md).

## Instance Startup Failure, Repeated Restarts, or OOM

**Symptoms**: An instance is `failed`, the deployment repeatedly creates instances that fail, or a container is killed.

### Find the Cause of This Failure

1. Check the instance event timeline. The failure reason is in `params.detail`:

   ```bash
   curl -s -H "Authorization: Bearer <jwt>" \
     "http://<server>:8800/api/v1/instances/<uuid>/events?limit=20"
   ```

2. View live logs in the instance details (`GET /api/v1/instances/{uuid}/logs`, SSE), or search for `CUDA out of memory` or `Error` on the logs page.
   If the container still exists, you can also use `docker logs` or `gpuworks-agent runveil logs` on the node. If the container has been cleaned up, use previously collected historical logs and events.
3. Resolve the error as described below, then retry and confirm that the instance enters `running`.

### Insufficient GPU Memory or an Incorrect Startup Command

**`CUDA out of memory`: insufficient GPU memory**

Use `nvidia-smi` to check whether other processes occupy GPU memory. Check `gpu_memory_utilization` and `max_model_len`, reduce them or increase TP as needed, then redeploy.

**`container OOM-killed`: first identify which type of memory is exhausted**

This message alone does not prove that GPU memory is insufficient. First examine the engine logs, container memory limits, host memory, and kernel OOM logs. If you confirm a CUDA GPU memory shortage, follow the instructions above. If host or container memory is insufficient, free memory or adjust the memory limit based on capacity, then retry.

The platform identifies OOM by the runtime OOM flag, or by exit code 137 together with `OutOfMemoryError` / `CUDA out of memory` in recent logs. nerdctl has no OOM flag and uses only the latter criterion. Exit code 137 alone is not evidence of insufficient GPU memory.

**`container exited (code N)` / `run failed: …` / `pre-start preparation failed: …`**

Check the image and command against the engine logs. If you see `container glibc too old`, check the image's glibc version.
In a multi-node deployment, failure of any rank fails the entire generation, with the message `rank N on <node> failed: …`.

### Distinguish the Two Startup Timeouts

**Container creation timeout: `run failed: docker run timed out after 2m0s`**

If Docker or nerdctl `run -d` does not return within 120 seconds, the startup attempt fails. First check whether other workloads have filled the node's root disk. dockerd may be waiting for root filesystem writeback while creating the container.
You can reproduce this on the node with `time docker create <image> true`. This limit is unrelated to the engine health check's startup timeout; increasing that timeout does not resolve this issue.

The agent waits for the timed-out `run` to return and removes the container it creates. It waits up to 10 minutes, then terminates the process and removes the container by name to avoid leaving unclaimed `gpuworks-*` containers.

**Engine readiness timeout: `health check timed out`**

For a multi-node follower, the corresponding message is `headless worker readiness timed out`. If loading a large model, compiling kernels, or capturing CUDA graphs is slow, first check whether the logs show continued progress. If progress is normal, increase the startup health check budget:

- Cluster default: **1800 seconds**. Change it under **Settings → Nodes & deployments → Deployments**.
- Individual deployment: set `start_timeout_sec` under **Runtime environment → Start timeout** in the deployment form, in the range **60–86400 seconds**. Leave it blank for an automatic budget using the cluster default as a floor.

The budget applies while waiting for health checks after instance startup. Its value is determined when the instance is created; changes affect only instances started afterward.
Exceeding the budget fails the startup attempt and triggers recreation according to the rules below.



Instance details show the **startup wait limit** separately from the **rough startup estimate**. For managed vLLM, an explicit final `VLLM_ENGINE_READY_TIMEOUT_S` receives 30 extra seconds for probing under the automatic policy, capped at 86400 seconds. When the variable is neither explicitly set nor removed, the platform coordinates the inner wait budget. Manual commands and replaced entrypoints do not have their internal timeout inferred. An explicit deployment budget takes priority even when shorter than the engine deadline; the page reports the conflict. Probe heartbeats do not establish model-loading progress.

The basic estimate covers single-node, single-model managed vLLM/SGLang starts. It separates checkpoint loading from the other engine initialization costs:

```text
estimated seconds = W / (512 * 1024 * 1024 bytes/second) + I_ref
```

`W` is the selected checkpoint size in bytes. `I_ref` is an aggregate initialization reference allowance selected by engine and recognized execution mode:

| Managed engine/mode | `I_ref` |
|---|---:|
| SGLang | 240 seconds (4 minutes) |
| Ordinary vLLM | 300 seconds (5 minutes) |
| vLLM with an explicit `--omni` flag in its final command | 420 seconds (7 minutes) |

The allowance combines framework startup, tokenizer, compilation, KV profiling and graph capture; it is not a sum of measured stages. The final command is authoritative: stale template arguments or mentions of `--omni` in a model name or JSON do not select that profile. Manual commands, multi-node, PD and external multi-component configurations still receive no numeric estimate.

In GiB, estimated seconds are approximately `weight GiB × 2 + I_ref`:

| Weight size | Ordinary vLLM | SGLang | vLLM `--omni` |
|---|---|---|---|
| 10 GiB | 5 min 20 s | 4 min 20 s | 7 min 20 s |
| 50 GiB | 6 min 40 s | 5 min 40 s | 8 min 40 s |
| 100 GiB | 8 min 20 s | 7 min 20 s | 10 min 20 s |
| 256 GiB | 13 min 32 s | 12 min 32 s | 15 min 32 s |
| 512 GiB | 22 min 4 s | 21 min 4 s | 24 min 4 s |
| 1 TiB (1024 GiB) | 39 min 8 s | 38 min 8 s | 41 min 8 s |
| 2 TiB (2048 GiB) | 1 h 13 min 16 s | 1 h 12 min 16 s | 1 h 15 min 16 s |

The table uses binary size units. For decimal 1 TB (10¹² bytes), ordinary vLLM gives about 36 min 3 s. These estimates assume weights are already on the node and exclude model downloads, image pulls and queuing.

**Evidence and limits**: [Breaking the Ice (MLSys 2026)](https://arxiv.org/abs/2606.07362) models initialization by stage and requires environment-specific coefficients; [ServerlessLLM (OSDI 2024)](https://www.usenix.org/conference/osdi24/presentation/fu) uses the bytes/bandwidth term for loading, which does not describe complete readiness. This implementation adopts that decomposition, while 512 MiB/s and the initialization profiles are rounded reference assumptions based on limited project measurements, **not paper coefficients, current-node benchmarks or a cross-platform calibration**.

The profiles are primarily grounded in NVIDIA development-environment samples and do not improve every backend: DeepSeek and some Ascend SGLang starts may be overestimated. Caches, compilation options, model structure, CPU and storage contention still affect latency. Without separate evidence, no linear speedup is inferred from GPU or CPU-core counts, no guessed stage cost is deducted when graphs are disabled, and no runtime penalty is added. Missing weights, distributed or multi-component starts and manual commands still return unknown/unsupported; existing instances display their recorded parameters. The estimate is advisory, does not change the startup wait limit and is not a completion guarantee.

Loading, compilation and graph capture seconds are displayed only when explicitly logged. Duplicate ranks contribute the maximum single duration, not a sum; stages can overlap or come from different ranks and cannot be added as a complete startup breakdown. Agent reconnection/adoption does not measure a new startup. Elapsed wait starts with the current health-check stage, excluding model download and image pulling. Exceeding the rough estimate only indicates a slower start; probes, process exit and OOM determine actual failures.


### Container Exits After Reaching Running

The agent checks containers of running instances every 15 seconds. Whether caused by `docker stop`, a process crash, OOM, or the engine exiting on its own with code 0, it first cleans up the container, then reports `failed` and enters retry.

| Message | Meaning |
|---|---|
| `container exited while running (code N)` | Exited while running; multi-node follower messages use `headless worker …` |
| `container OOM-killed while running (code 137)` | Killed by OOM while running |
| `container disappeared while running (removed outside the agent)` | Another operation on the node removed the container |
| `cleanup pending: …` | Cleanup failed; status is `removing` |

Containers of sleeping instances are still running and are not treated as exited. If a container check such as `docker inspect` fails, only that check cycle is skipped.

### Retry After Fixing the Cause

The platform recreates instances through the controller. Containers use `--restart no` and do not rely on Docker restart policies.
Each unit allows up to 3 attempts, with each new attempt created at least 15 seconds after the previous one. Each attempt uses a new UUID and retains the previous failure record.
An attempt refused before starting because another process holds one of its host ports (`ENGINE_PORT_REFUSED`; nothing has started) is the exception: it is retried at once on other ports, and a unit's first 10 such refusals do not count toward the 3.
When the budget is exhausted, `UNIT_RETRY_EXHAUSTED` is emitted (`params` contains `attempts` and `reason`), and automatic recreation stops.

After resolving the cause, call `POST /api/v1/deployments/{id}/units/retry` with body `{"role":"","index":0}` (`role` is empty for a regular deployment).

| Returned error | Check |
|---|---|
| 404 `UNIT_UNKNOWN` | Whether role and index identify the intended unit |
| 409 `UNIT_NOT_FAILED` | Whether no failed attempt exists yet, or an instance still exists |
| 409 `UNIT_RETRY_NOT_ACTIVE` | Whether the deployment is running |

After retrying, check events and logs again and confirm that the new instance enters `running`. For the recreation mechanism, see [ADR 0029](../adr/0029-deployments-reconcile-as-units.md).

## Gateway Returns 401, 404, or 503

The gateway error body differs from the management API: `{"error":{"message":"…","type":"gpuworks_proxy"}}`. There is no `code`; use `message` to identify the error.

| Status | message | Cause and action |
|---|---|---|
| 401 | `invalid or missing API key` | `Authorization: Bearer <key>` is missing, or the key was deleted or is invalid. `/v1` requires a key by default and refuses every request even when the database contains **no keys at all**; for genuinely keyless access an administrator turns on "Accept /v1 inference requests without a token" (`inference_open_access`) under **Settings → Security** |
| 400 | `request body must specify a model` | The request body has no `model` |
| 404 | `no deployment serves model <name>` | The model name is not the deployment's served name (list names with `GET /v1/models`), or the deployment has stopped |
| 404 | `deployment <name> does not expose <METHOD> <path>` | Only the playground (`/api/v1/playground/v1/*`) performs contract checks and returns this error: the deployment's API contract does not include this endpoint (for example, calling `/v1/embeddings` on a Chat model). Use `GET /api/v1/playground/openapi.json` to view its declared endpoints. The public `/v1` does not perform this check; it forwards the request unchanged to the engine, which returns its own 404 or other response |
| 404 | (The entire `/v1` path returns 404) | The listener you accessed does not serve the inference API. Check `listen.gateway` |
| 503 | `model is asleep; wake it first (console, or POST /api/v1/deployments/<id>/wake; a schedule or a waiting wake may also wake it)` | No awake replica is available to serve requests. Wake it in the console or call `POST /api/v1/deployments/{id}/wake`; a schedule or a waiting wake also wakes it, see the [Scheduling Analysis](scheduling-center.md). Once sleep intent is issued, selected instances stop accepting new requests |
| 503 | `no running instance for <name> (still starting?)` | No running replica: it is still starting, has failed, has been stopped, or its node is offline. Check instance status |
| 503 | `distributed model <name> is not ready: rank N is unavailable`, `distributed model leader is offline for <name>`, `distributed model leader is not ready for <name>`, `distributed deployment node set is incomplete for <name>` | A rank or the leader in a multi-node deployment is not ready |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … did not reattach within 5s` | The node data lane did not reconnect within 5 seconds of disconnecting. **Retryable**; there is no fallback to the control lane |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … data lane closed under an open stream: …` | The data lane disconnected after the request reached the node but before a response arrived. **Retryable**. If it disconnects after the response starts, the client receives a truncated response |
| 503 + `Retry-After: 2` | `no prefill instance could take the request (…)` | The PD deployment has no available prefill instance |
| 502 | `upstream error: …` | The tunnel or connection to the engine failed. Check node connectivity and engine logs |
| 413 | `request body exceeds N bytes` | Exceeds `--proxy-max-body-bytes` (default: 256 MiB) |

Only the data-lane-unavailable and PD-no-prefill categories of 503 include `Retry-After`. For 503 responses caused by sleeping or starting instances, resolve the instance issue first instead of retrying.
For the data lane mechanism, see [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md). For other differences, see [OpenAI-Compatible Gateway](../reference/openai-gateway.md).

## dGPU Capability Unavailable

**Symptoms**: Evaluating a deployment with a GPU memory limit returns `partition_unavailable`, or creation returns 400 `DISTRIBUTED_PARTITION_BACKEND_UNAVAILABLE`
(message: `partition backend <id> is unavailable: <reason>`).

**Read the status**: Check `partition_capabilities[]` in the `GET /api/v1/nodes` / `GET /api/v1/gpus` response. Each entry's `status` is only
`ready` / `unavailable`, and `reason` is the sole source of the cause. Non-NVIDIA backends also have `probe_status` (`passed` / `skipped` / `failed`).

**Confirm and resolve**: `nvidia-dgpu` needs no probe and checks only the runtime and assets. `ascend-dgpu` / `ppu-dgpu` require a passing probe.
By default, the probe runs once per agent process and caches its result (it reruns when assets, GPUs, or the runtime change). After fixing the environment, restart the agent to probe again. Do not fall back to whole GPUs and treat that as success.
For details, see [Enable and Troubleshoot dGPU](dgpu.md) and the [Support Matrix](../reference/support-matrix.md).

**Possible causes (`reason` text, in evaluation order)**

1. `container inference unverified: no usable docker/udocker runtime on the node`: the node has no usable runtime.
2. `container inference unverified: <reason>`: the backend constrains the runtime (for example, PPU is verified only on Docker).
3. `agent has not reported current dGPU safety capability; upgrade the agent`: the agent is too old.
4. `missing dGPU assets: <file-list>`: dGPU assets are missing.
5. `dGPU probe has not completed: …`: the probe has not returned a result yet. Wait or check the agent logs.
6. `dGPU probe failed: <detail>`: the probe failed. If the probe is `skipped`, the message falls back to `container inference unverified: <probe-detail>` (for example, `no container runtime available on the node`). The detail comes from the probe container, such as
   `the container runtime cannot load the embedded probe image`, `dGPU probe timed out` (15 seconds), or
   `probe container exited with code N`.

When a GPU memory quota is set without specifying `partition_backend`, the platform selects a ready backend based on the candidate node. If none is available, it returns that node's capability reason;
it does not always use the NVIDIA backend. Explicitly specifying a backend restricts selection to that backend. Specifying a backend without a positive `vram_limit_gib`
returns `partition_backend requires a positive vram_limit_gib`; correct the configuration first.

## Sleep / Wake Failure

**Symptoms**: Clicking Sleep or Wake reports an error, or an instance becomes `failed` after sleeping / waking.

**Confirm and resolve**: Check entries with `params.phase` set to `sleep` in the instance timeline, along with `params.detail`.
If the state has been confirmed as rolled back, fix the cause and retry. If the state is uncertain and marked `failed`, check the instance recreation result first instead of repeatedly clicking the button.
See [Sleep / Wake](sleep-wake.md).

| Error code | HTTP | Meaning |
|---|---|---|
| `SLEEP_NOT_ENABLED` | 400 | Sleep was not enabled when the deployment was created |
| `SLEEP_ENGINE_UNSUPPORTED` | 400 | The engine does not support sleep (only vLLM supports it) |
| `PD_SLEEP_RDMA_UNSUPPORTED` | 400 | A sleep-enabled PD deployment cannot send KV traffic over the host RDMA network: disable sleep, or switch the nodes to an Ethernet compute network |
| `ASCEND_SLEEP_WEIGHT_NZ_CONFLICT` | 400 | The extra arguments of a sleep-enabled Ascend deployment set `weight_nz_mode` to a nonzero value (`params`: `flag`, `value`): remove the key; the platform sets it to 0 |
| `ASCEND_SLEEP_EXPANDABLE_SEGMENTS` | 400 | The environment of a sleep-enabled Ascend deployment enables `expandable_segments:True` (`params`: `variable`, `value`): remove it or set it to `False` |
| `ASCEND_SLEEP_ADDITIONAL_CONFIG_INVALID` | 400 | The template's `--additional-config` is not a JSON object, so the platform cannot merge `weight_nz_mode` (`params`: `flag`, `value`): fix the template |
| `SLEEP_INVALID_LEVEL` / `SLEEP_INVALID_MODE` | 400 | `level` must be `1` or `2`; `mode` must be `wait` or `abort` |
| `WAKE_INSUFFICIENT_GPU_MEMORY` | 409 | The GPUs are occupied, so the sleeping instance cannot fit back on them. `params` contains `gpu_indexes`, `shortfall_gib`, and `occupants`; stop or put the occupants to sleep first, or use `?wait=true` to wait for room or `?yield=true` to put the occupants to sleep and then wake |
| `WAKE_GPU_HELD_OUTSIDE` | 409 | A process outside the platform holds the card, so the platform cannot see its usage and does not allow the wake (waiting and yielding do not help either). `params` as above, and `refused[].held_outside` is true. Release that process on the node and retry; see [Card Is in Use by Processes Outside the Platform](#卡被平台外进程占用) |
| `WAKE_YIELD_BLOCKED` | 409 | A yielding wake cannot be done and nothing changed: `params.occupants` includes a deployment that cannot sleep, the last awake role of a PD deployment, or a process outside the platform. Wait for room instead, or deal with the occupants by hand |
| `SLEEP_HOST_MEMORY_INSUFFICIENT` | 409 | An L1 sleep moves the weights into host memory, and the available memory minus the headroom (the larger of 5% of total memory and 4 GiB) cannot hold them. `params` contains `node`, `need_gib`, and `available_gib`; use L2 |
| `SCHEDULE_INVALID` | 400 | A field of the schedule is invalid: `params.field` and `params.reason` say which (weekdays, the `HH:MM` time format, sleep level, replica count 1–32, action) |
| `SCHEDULE_TIMEZONE_UNKNOWN` | 400 | The server does not know the time zone name; use an IANA name (such as `Asia/Shanghai`), or leave it empty for the server's time zone |
| `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` | 400 | A multi-node or PD deployment placed by hand cannot have its replica count scheduled; switch it to automatic placement first with `PUT /deployments/{id}/placement-mode`. For an entry written before the deployment was switched to manual placement, the entry fails when due and records `SCHEDULE_ACTION_FAILED` |
| `YIELD_POLICY_INVALID` | 400 | The yield policy request is invalid: `params.field` is the field (`policy`, `sleep_level`, `unfinished`, `idle_sec`, `max_serve_sec`, `unfinished_wait_sec`, `hold_max_sec`) and `params.reason` is `required` (no `policy` given: the body is `{}`, or has parameters but no `policy`), `invalid_value` (not one of the allowed values), or `out_of_range` (a number out of range, which also carries `minimum` and `maximum`; when the idle time of a yield-on-a-turn deployment exceeds the longest serving time, the `maximum` of `idle_sec` is the longest serving time); see [Yield policy](scheduling-center.md#出让策略) |
| `YIELD_POLICY_SHAPE` | 400 | A multi-node or PD deployment can only use never yield automatically |
| `YIELD_POLICY_NEEDS_SLEEP` | 400 | An edit turns sleep off while the deployment's yield policy (`params.policy`) is not never yield automatically; set the yield policy to never first |
| `NODE_REPLACEMENT_INVALID` | 400 | The node-loss rescheduling wait must be 0 (never) or 60–604800 seconds |
| `REPLICAS_PLACE_ON_TOO_MANY` | 400 | `place_on` names more nodes than the replicas being added |
| `REPLICAS_PLACE_ON_REFUSED` | 409 | A node in `place_on` cannot take a new replica; `params.reason` is `unknown` (no such node), `offline`, `cordoned`, `requirements` (does not satisfy the node requirements), or `no_room` (the trial plan does not fit); an automatically placed multi-node deployment also has `no_group` (the named nodes cannot form a replica that fits: different compute networks or architectures, or not enough cards) |

**Possible causes (agent-side failures)**

1. The engine has no sleep endpoint (a custom image or `VLLM_SERVER_DEV_MODE=1` is not enabled). The event contains
   `sleep level N → M failed: <url>: http 404`.
2. L2 wake fails, or the actual engine state cannot be confirmed after failure (request timeout, state probing / recovery failure). The state is uncertain, so the instance is marked `failed` (phase `sleep`), with message
   `sleep level uncertain after …`. Its full GPU memory allocation is retained and gateway forwarding is blocked. The controller recreates this generation of instances.
3. A failure with a confirmed state: the confirmed actual sleep level is retained. If the desired state still corresponds to the failed command, it is rolled back to that actual level (also applies to failed L1 / L2 transitions). The event is `INSTANCE_SLEEP_RESULT`,
   with `params.result` set to `failed_rollback` (`applied` on success). Failures are not retried automatically.

### Schedules, Yielding, or Waiting Did Not Run as Expected

**Symptom**: a sleep or wake did not happen at the scheduled time, or a deployment was put to sleep unexpectedly.

**How to confirm and handle it**: First look at the entries of type `automation` in the deployment timeline, or at **Recent automatic actions** in the Scheduling Analysis:

| Event | Meaning and handling |
|---|---|
| `SCHEDULE_ACTION_FAILED` | The automatic action was refused. `params.reason` is the cause and `reason_code` the matching error code (see the table above); the failed occurrence is not retried, so fix the cause and wait for the next one, or run it by hand |
| `WAKE_WAITING` | A scheduled wake did not fit and now waits for room; look at the scheduling board to see who holds the cards |
| `WAKE_WAIT_TIMEOUT` | The wait ended after 30 minutes with no room; free the cards and wake again |
| `AUTO_SLEEP` (`trigger` is `preempt`) | The deployment yielded its cards to another one; `reason` is `idle` (idle for the idle time), `rank` (the other ranks higher), or `turn` (it was the other yield-on-a-turn deployment's turn, and it had served the longest serving time), and `for_deployment` is the one it yielded for. Set the yield policy to never yield automatically if the deployment must not be preempted (health checks and other requests the platform makes itself are not user requests and do not reset the idle clock). A deployment does not sleep on its own while nobody waits for its cards; one that used to sleep through “auto sleep when not preempted” should use a schedule instead |

After a server outage, only the newest occurrence within 12 hours is made up. If a schedule's `next_at` does not match your time, check its `timezone` (empty means the server's time zone).

### Replicas Were Not Moved to Another Node After a Node Went Offline

**Symptom**: a node has been offline for a long time, yet its replicas stay where they are.

**How to confirm and handle it**:

1. Check whether the deployment's **node-loss rescheduling wait** is 0 (never) or has not elapsed; the default is 10 minutes, counted from when the server last saw the node.
2. The timeline shows `NODE_REPLACE_NO_CANDIDATE`: no node can take over. A replacement for a multi-node or PD replica must be online, schedulable, satisfy the node requirements, share the compute network of the other ranks, have the same GPU vendor and architecture, and have free cards and pass the trial plan; fill in whatever is missing, and the platform retries every minute.
3. A single-node replica pinned to this node (its node requirements allow only this one) does not move; wait for the node to return, or edit the node requirements.

A node that is online but cannot take the replica (cards taken, cordoned) is not node-loss rescheduling: an automatically placed multi-node or PD deployment moves only the ranks that do not fit when the replica is about to start (timeline `UNIT_PLACED`), and a manually placed one waits; see [Node online but cannot take it](node-failover.md#节点在线但放不下自动放置的重新挑选).

For the rules and the undo, see [Reschedule Replicas on Node Loss](node-failover.md).

## Multi-Node, NCCL, or RDMA Network Connectivity Failure

**Symptoms**: A multi-node deployment stalls during startup, rank handshakes or NCCL initialization time out, or a request is rejected with `DISTRIBUTED_NODE_OFFLINE` / `COMPUTE_NETWORK_*`.

**Confirm and resolve**

- To check node connectivity through the API, call `POST /api/v1/deployments/preflight` (2–16 nodes; set `bandwidth:true` to test bandwidth).
  Failure of an individual node pair still returns 200 with `ok:false`. Check the reasons in `pairs[].error`, `pairs[].bandwidth_error`, or `warnings[]`.
- The **Internal tests** tab on the network details page runs TCP / `ib_write_bw` tests between hosts. The **Monitoring** tab shows NIC and RDMA rates and error counts.
  See [Configure Node Networks](configure-networks.md#内部测试) and [Network Metrics Reference](../reference/network-metrics.md).
  Internal tests do not cover the NCCL paths between ranks inside containers.
- Search for `NCCL` on the logs page, and compare it with the platform-injected `NCCL_SOCKET_IFNAME` and `GLOO_SOCKET_IFNAME` (saved env is available in the instance details).
- For the multi-node workflow, see [Multi-Node Inference](multi-node-inference.md).

**Possible causes**

1. Routing, firewall, or port connectivity between nodes fails (the master port must be in 1024–65535).
2. Nodes have inconsistent compute networks: `COMPUTE_NETWORK_MIXED` (some nodes have a compute network and others do not, and the management network cannot carry the traffic),
   `COMPUTE_NETWORK_MISMATCH` (nodes are not on the same compute network, and the management network cannot carry the traffic), `COMPUTE_NETWORK_NONE` (no network can carry compute traffic),
   or `COMPUTE_NETWORK_RUNTIME`, because runveil nodes cannot use RDMA. When the management network allows compute traffic and only some ranks have a compute network, the whole group falls back to the management network with a
   `COMPUTE_NETWORK_FALLBACK` warning.
3. RDMA requires IB (the platform injects `NCCL_IB_DISABLE=0`, `NCCL_NET=IB`, and `NCCL_IB_HCA`). If the container cannot see verbs devices, startup fails immediately;
   it does **not** silently fall back to TCP.
4. The wrong HCA is selected, or the link has error counts. Check `link_downed`, `symbol_error`, and other values in `gpuworks_node_rdma_events_per_sec`.
5. The node list is invalid or a node is offline: `DISTRIBUTED_NODE_COUNT` (the number of `node_uuids` must equal `nnodes`), `DISTRIBUTED_DUPLICATE_NODE`, or `DISTRIBUTED_NODE_OFFLINE`.

## Reverse Proxy Batches Streaming Output or Truncates Long Requests

**Symptoms**: A `stream: true` response arrives all at once (time to first token equals the total generation time); long generations disconnect after a fixed duration;
or large request bodies are rejected.

**Confirm and resolve**: Compare the same request by connecting directly to the server port. For key Nginx settings for `/v1/` (`proxy_buffering off`,
`proxy_request_buffering off`, `client_max_body_size`, and `proxy_read_timeout`), see [Production Deployment](deploy-production.md).
Use `stream: true` to receive output incrementally. Long non-streaming requests still require sufficient timeouts on the reverse proxy and client.

**Possible causes**

1. The reverse proxy enables response buffering (Nginx defaults to `proxy_buffering on`), batching SSE output.
2. The reverse proxy read timeout (Nginx `proxy_read_timeout`) is shorter than the time needed for a long non-streaming generation. The server itself sets no read or write timeout on HTTP connections;
   long streams are cut off only by timeouts in an intermediate reverse proxy, load balancer, or client SDK.
3. The reverse proxy request body limit is too small (Nginx defaults `client_max_body_size` to 1m). To distinguish the source: reverse proxy errors are HTML error pages,
   whereas server errors are JSON with `request body exceeds N bytes`.
4. You are using a temporary public address from `cloudflare-quick`, which does not support SSE. See [Give the Console a Public Address](expose-public-address.md).
5. A server restart, upgrade, or node agent disconnection interrupts in-flight streams without retrying them.

## An Operation Reports "The Server Host's Disk Is Full"

When an operation that saves data (registering a model, creating a deployment, changing a setting) returns HTTP 507 with error code `SERVER_STORAGE_FULL`, the full disk is the **server host's**, not the target node's. SQLite reports it this way whenever its database file, WAL or a temporary file has no room, and the message names the data directory with the free space and size of its disk.

Not every write fails once the disk fills: SQLite first uses up the space it already holds, so in the same stretch of time some operations succeed and others are refused. Every console operation that changes something (signing in included) writes the database, and any of them can be refused.

1. Find the full disk on the server host. The directories to check are the data directory, the log directory, `backups/` under the installation directory, and SQLite's temporary directory. That is the first writable one of `SQLITE_TMPDIR`, `TMPDIR`, `/var/tmp`, `/usr/tmp` and `/tmp`.

   ```bash
   df -h <data> <data>/logs /var/tmp /tmp
   du -xsh <data>/* | sort -h
   ```

   While the disk is full, the console's "Collect diagnostics" cannot write either, and the list shows the collection as failed (`no space left on device`). Run [`gpuworks support-bundle`](../reference/cli.md#gpuworks-support-bundle) on the server host instead, with `--out` on another disk that has room. `server/disk.json` in it records the size and free space of these directories' filesystems, and the size of each entry in every directory. `disk.json` lists only what these directories hold: when free space is 0 but their contents add up to far less than the size, the space is taken by another directory on the same filesystem (such as `/var/log` on the same root partition); keep looking with `du -xsh /*`.
2. Retry once space is freed; the server does not need a restart. While writes fail, the server log file stops, and records still go to stderr (`journalctl -u gpuworks` under systemd). Within 30 seconds of space coming back the log file resumes, starting with a `log file writes resumed` line that carries the number of records dropped (`dropped`) and when writing stopped (`since`).
3. Do not restart the server before freeing space: with 0 bytes free it cannot start, since SQLite cannot even create its shared-memory file. The exit reason, `msg="server exited"`, then adds to the original error how much the data directory's disk has left (`the disk holding the data directory … has 0 B free of …`). Start it again once there is room.

## Forgotten Admin Password

**Symptoms**: You cannot sign in. (To change a password while you can still sign in, choose **Change password** in the console's account menu at the top right.)

**Resolution**

1. Confirm the actual data directory on the server machine. The following command applies to the default installation directory; replace `--data` for a custom installation or when running `serve` directly.
   Run it as an operator with database access. You do not need to stop the server:

   ```bash
   gpuworks reset-password --data /opt/gpuworks/server/data
   ```

2. Sign in again with the new password and confirm that access is restored. Changing the password invalidates this user's existing management sessions; inference tokens are unaffected.

`--user` defaults to `admin`. Without `--data`, the command reads `data_dir` from the `server.yaml` next to the binary, then from the default installation's `server.yaml`, and uses `./data` only if neither exists (you can specify the database path directly with `--db`). Without `--password`, the command prompts twice in the terminal without echo, which keeps the password out of command history; when stdin is not a terminal, `--password` is required. New or changed administrator passwords must contain 8–128 Unicode characters and at most 512 bytes, and must not be the username. Common guessable values, whole-value repetitions and sequences are rejected; character-class mixtures are not required and passphrases are supported. Existing accounts can still authenticate with their current passwords.
If it reports `database ... does not exist`, use `--data` to specify the server's actual data directory as instructed.
If the user does not exist, the command returns `not found`; first check the username and data directory. For permission errors, check the current user's access to the database.
See [CLI](../reference/cli.md).

## When You Cannot Log In On Site: Debug Collection and Collecting Diagnostics

When you cannot log in to the customer's servers, have an administrator work in **Settings → Diagnostics** and send back the downloaded archive.

**Collect diagnostics**: choose a time range (the last hour by default, at most 7 days) and the related deployments and nodes, then click **Start collecting**; when it finishes, download `gpuworks-diagnostics-<id>.tar.gz` from the list. The archive holds:

| Path | Content |
|---|---|
| `manifest.json` | The collection's scope, the server version, and whether each part was collected, skipped (node offline, agent version without support) or failed |
| `server/disk.json` | The server's data, log and backup directories and SQLite's temporary directory: their filesystems' size and free space, and the size of each entry in every directory (largest first) |
| `server/gpuworks-server.log` | The server log in the time range (info included; the Logs page has warn and above only) |
| `nodes/<uuid>/agent.log`, `doctor.json` | The chosen nodes' (without a choice, the nodes of the chosen deployments' instances) agent logs and diagnosis reports |
| `instances/<uuid>.log` | The chosen deployments' instance logs |
| `debug/<session>/`, `nodes/<uuid>/debug/` | Debug collections that overlap the time range |
| `db/*.json` | All nodes, the chosen deployments and instances, events in the time range, the chosen nodes' image states, the image catalog, storages, settings, and the chosen deployments' request records |

Credentials (tokens, passwords, keys, hashes) keep only their first 8 characters, which match the API key prefixes the console shows. Archives are kept for 7 days under `diagnostics/bundles/` in the server's data directory.
When the server does not start, run [`gpuworks support-bundle`](../reference/cli.md#gpuworks-support-bundle) on the server host; it reads the database and log files directly (the nodes' part cannot be collected, and the manifest says so).

**Debug collection**: when a symptom needs debug logs or the raw requests to be understood (a client that never receives the end of a stream, say), start a debug collection, reproduce the problem once, then collect diagnostics:

1. Choose a duration (10 minutes by default, at most 1 hour), a total size (10 GiB by default, at most 50 GiB), the deployments to record and the nodes to turn debug logs on for.
2. While it runs, the server and the agents of the chosen nodes (and of the nodes the chosen deployments run on) write records at debug and above to separate files, leaving the regular log levels unchanged; every request of the chosen deployments is recorded in full as `requests/<deployment>-<request id>.jsonl`:
   the request (credential headers removed), raw blocks the gateway read from the engine (`upstream`), raw blocks it wrote to its immediate downstream (`client`), each with its milliseconds since the start, and how it ended (the `end` line's `observation_scope: "gateway"`, `finish_reason`, `outcome` and read/write errors). One request records at most 16 MiB, and each node's debug file at most 200 MiB.
3. It ends when its time is up, its size is reached, or it is stopped. Its start and end each write a server warning with the operator, as the audit record.
4. The request records contain prompts and replies; they stay on the server and the nodes only and are deleted after 7 days. Nodes whose agent version is too old do not support it and are marked as skipped.

## Still Unresolved?

When filing an issue or asking for help, attach a [diagnostics archive](#无法登录现场时调试采集与收集诊断信息) first; without one, include the following information to reduce back-and-forth:

1. Versions: `gpuworks version` (server) and `gpuworks-agent version` (the affected node).
2. The affected instance UUID and its event timeline: `GET /api/v1/instances/<uuid>/events` (add
   `GET /api/v1/deployments/<id>/events` if needed), plus the `schedule` field in the instance response.
3. Logs restricted to a few minutes before and after the failure: the server's `gpuworks-server.log`, the node's `agent.log`,
   and the instance's engine output on the logs page.
4. The complete output of `gpuworks-agent doctor` on the node.
5. For connection issues, include the result of `GET /api/v1/agent/rejects`, the exit code of `gpuworks-agent smoke`, the deployment request body, and the network topology (whether a reverse proxy / NAT is involved).
   Remove API keys, node tokens, and other credentials before sharing.
