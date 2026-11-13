# Connect, Upgrade, and Uninstall Nodes

Connect a GPU host to a running server. After connecting it, you should see the node online under **Compute Nodes** and verify its accelerators and runtime.

**Use cases**: Connect your own Linux GPU host, an Apple silicon Mac, or a cloud GPU instance. To update an existing node or take it out of service, go directly to [Upgrade](#升级), [Maintain Nodes](#维护节点停止调度与驱逐), or [Uninstall](#卸载).

## Prepare to Connect

- The server is [installed and running](install-server.md), and you have an administrator account for signing in to the console.
- The node and server can reach each other in at least one direction. Choose the connection direction below. SSH is used only for SSH installation; it is not required for server-initiated connections.
- Linux nodes can use Docker; when Docker is unavailable, the agent can use the built-in runveil. For Mac system requirements and runtime limitations, see [Mac Nodes](#macapple-silicon节点).
- The node's installation directory is writable, allows execution, and has enough space for models and runtimes. Nodes without Docker must also allow colons in filenames. If the host already has an agent, first read [Connect the Same Host Again](#重复接入同一台主机) to avoid replacing another node.

## Choose a Connection Method

Cloud hosts can also use the manual command or SSH wizard. When installation starts from a cloud platform, GPUWorks uses the synchronized instance or host information to install the agent. Once the agent connects to the server, the host appears as an online compute node.

| Method | Requirements | Entry point |
|---|---|---|
| Manual command | You can run commands in the node's terminal, and the node can reach the server's download address | **Add node → SSH unavailable** generates an installation command |
| SSH wizard | The server can sign in to the host through SSH | **Add node → SSH supported** installs after a precheck |
| Offline package | Manual installation with a server-initiated connection, including when the node cannot download from the server | **Add node → SSH unavailable → Server-initiated connection** |
| One-click public cloud instance installation | The public cloud account has been added and the target instance synchronized | Expand the account under **External Platforms** and click **Install Agent** in its instance list; see [Public Cloud Instance Installation](rent-cloud-gpus.md#安装-agent让实例出现在计算节点) |
| One-click ZStack Cloud host installation | The ZStack Cloud environment has been added and the target host synchronized | Expand the environment under **External Platforms** and click **Install Agent** for the target host; see [ZStack Cloud Installation](onboard-zstack-cloud.md#安装-agent-并确认接入) |

### agent_dial and server_dial

Under **Add node → Connection direction**, choose according to your network:

| Connection direction | Network requirements | What to enter |
|---|---|---|
| **Agent-initiated connection** (`agent_dial`, default) | The node can reach the server; suitable for nodes behind NAT, with no inbound node ports required | Set **Management node address** to a server address reachable from the node; this corresponds to `--server` |
| **Server-initiated connection** (`server_dial`) | The server can reach the node's listening port; neither node-to-server access nor SSH is required | **Agent listen address** defaults to `0.0.0.0:18800`; set **Compute node address** to an IP, domain name, or mapped address reachable from the server |

For server-initiated connections, the security group or firewall must allow inbound TCP from the server. If the connection address contains only a host, the listening port is used; with port mapping, enter the mapped `host:port`. Existing nodes retain their original listen address regardless of changes to the default. If neither direction works, first [establish network connectivity](#节点与-server-网络不通).

### Connect with a Manual Command

1. Open **Compute Nodes → Add node** and select **SSH unavailable**. This generates manual commands and can be used even if the host supports SSH.
2. Enter an optional **Node name**, select **Agent-initiated connection**, and check **Management node address**. To customize the installation directory or startup method, configure them under **Advanced**.
3. Click **Create node and generate command**, then **Copy command**. The node now shows **Connecting**, and the command contains its unique token.
4. Run the command in the target node's terminal and record the actual installation directory shown in the output. After installation, [verify the connection](#确认接入结果) as described below.

To change parameters such as the driver or state directory, see [Installation Script Parameters](../reference/cli.md#节点安装脚本-installsh). Rerunning installation updates the node's connection configuration; do not use another node's token.

### Connect with the SSH Wizard

1. Open **Compute Nodes → Add node**, select **SSH supported**, and enter the node name, connection direction, and corresponding addresses. You do not need to create a node first to obtain a token.
2. For **SSH target**, enter `user@host[:port]` or paste the SSH command provided by your cloud provider. The wizard reads only the user, host, and port; options such as `-i` and port forwarding have no effect, and jump hosts using `-J` / `ProxyJump` are unsupported.
3. Choose an authentication method. With the default **Platform key**, copy the authorization command from the form and run it on the target host as the SSH user. You can also enter a **Password** or **Private key**. If the target SSH server accepts passwords only through an interactive prompt such as PAM, select **Keyboard-interactive** and use the same password field. It is a different protocol from ordinary SSH password authentication; an explicit selection attempts only that method.
4. Under **Advanced**, check **Installation directory** and **Start via systemd**, then click **Start precheck**. Resolve network, directory, and device permission issues according to the report, then click **Install Agent**.
5. Review the installation results. If prompted to confirm replacement of an existing agent, first check which node is affected. **Installation command executed** does not mean the node is online; continue to [verify the connection](#确认接入结果).

You can also view the platform public key under **SSH Key** on the Settings page. Replace `<platform-public-key>` in the authorization command with the full public key shown on the page:

```sh
mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '<platform-public-key>' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
```

Keyboard-interactive supports only one non-echoing password response, with a bounded number of informational rounds containing no questions. Multiple questions, repeated password prompts, and MFA are unsupported. When authentication methods do not match, the report points to the target SSH configuration or suggests selecting keyboard-interactive; network failures and changed host keys still require their own remedies.

After successful installation using a password, keyboard-interactive, or private key, the server attempts to write the platform public key. If successful, it retains only the platform key; it saves the entered credentials only if writing the public key fails. `SSH authentication` and `Platform key` in the installation report show the authentication and public key write results. The precheck does not write the public key; it only creates and removes temporary files to probe the directory. The node SSH credential editor offers the same authentication options and also attempts to authorize the platform public key when saving. If a password is retained, reopening the editor shows **Password**, which identifies the stored credential type. Subsequent automatic recovery supports both ordinary password and keyboard-interactive authentication without saving the credentials again.

### Offline Package

1. Open **Compute Nodes → Add node**, select **SSH unavailable → Server-initiated connection**, enter the listen address and connection address, then click **Create node and generate command**.
2. Click **Download the offline package gpuworks-agent.run**, transfer the file to the node, and verify that its SHA-256 matches the value shown on the page. The node needs `tail`, `head`, `gzip`, and `mktemp`.
3. In the directory containing the file, run the command generated on the page. The following example uses the default listen address; replace `<node-token>` with the unique token generated on that page:

   ```sh
   sh gpuworks-agent.run --token '<node-token>' --connect-mode server_dial --listen '0.0.0.0:18800'
   ```

4. Wait for the server to connect to the node, then [verify the connection](#确认接入结果).

The offline package contains the Linux / Mac agents embedded in the server and the installation script, and automatically selects the matching platform. It contains no token. The direct download path is `/dl/gpuworks-agent.run` under the node connection address, with the checksum file at `/dl/gpuworks-agent.run.sha256`; it is not downloaded from the gateway address. Download the offline package again after upgrading the server.

### Verify the Connection

Return to **Compute Nodes**, confirm that the node changes from **Connecting** to **Online**, and open its details to check the accelerator model, count, and runtime. If the node is still disconnected, check the installation report, its `agent.log`, and the [network requirements](#节点与-server-网络不通). Do not repeatedly create nodes.

For the networks used by peer sync, multi-node inference, and service traffic, see [Configure Node Networks](configure-networks.md).

### Startup Methods and Subsequent Restarts

| Installation conditions | Startup method | Maintenance considerations |
|---|---|---|
| Linux with systemd, and the installation user is root or has passwordless sudo | Uses `gpuworks-agent.service` when **Start via systemd** is enabled | Starts at boot and restarts automatically after a crash |
| The option above is disabled, or systemd / the required permissions are unavailable | Runs in the background with nohup; select this with `--no-systemd` on the command line | Does not start at boot or restart automatically after exit; use `agent-restart.sh` in the state directory to restart |
| Mac | launchd | Unaffected by the systemd option; see the next section for login and permission requirements |

SSH installation and one-click cloud host installation (including command channels such as Cloud Assistant) record the actual installation directory. Subsequent out-of-band restarts and reinstalls reuse this directory and the node's startup method. Manual script installation does not record this directory, so out-of-band recovery uses `/opt/gpuworks`. Keep this difference in mind when using a custom directory or a second agent on the same host.

The state directory stores the node's state and connection configuration. It is the same as the installation directory unless specified separately. Connection configuration is stored in `gpuworks-agent.conf` within it; the token does not appear in the process command line, systemd unit, or plist. Rerun installation to change the configuration. For files and permissions, see [Installation Layout](../reference/install-layout.md#agent-配置文件).

## Mac (Apple Silicon) Nodes

Connect an Apple silicon Mac (M1–M5, macOS 15 or later) using the manual command, SSH, or offline package workflows above. Intel Macs are unsupported. The server can run on the same Mac or another host.

The SSH wizard uploads the agent and installation script; the Mac does not need to download them from the server. The precheck verifies Apple silicon, the macOS version, and the chip.

Before installation, check the account's permissions to determine whether the agent can run while the user is logged out:

| Permissions and login status | Installation result |
|---|---|
| Administrator privileges are available | After sudo validation succeeds, installs as a LaunchDaemon that starts at boot, but the agent still runs as the installation user. Manual installation can prompt for a password in the terminal; for SSH password login, that password is also used to validate sudo |
| Administrator privileges are not used | Append `--no-sudo` to the manual command; SSH key login without passwordless sudo also uses this method. Installs as a LaunchAgent, which runs only while that user is logged in. Fails if there is no graphical login session |

If no installation directory is specified, the agent is installed in the installing user's `~/gpuworks`, with or without administrator privileges (the same for the manual command and the SSH wizard). If an earlier installation left an agent in `~/gpuworks`, `/opt/gpuworks`, or `~/.gpuworks`, the first directory found in that order is reused. The control plane on the same Mac defaults to `~/gpuworks/server`, so the two do not overwrite each other.

Installation waits for launchd to start the agent reliably. During reinstallation, if the new job is rejected, the original job is restored. Mac also supports **Upgrade Agent** in the console; see the [Upgrade Guide](upgrade-and-compatibility.md#升级-agent) for confirmation and rollback boundaries.

After installation, the node should show the **host** runtime and **one card with unified memory**. Engines run as host processes using Metal environment packages, without Docker Desktop.

Available GPU memory is the portion that Metal allows the GPU to keep resident, typically about 2/3–3/4 of unified memory. It is determined by `iogpu.wired_limit_mb`, which the platform does not change. vLLM calculates its share as `gpu_memory_utilization` × GPU memory. A Mac can run multiple instances concurrently; adjust this value according to memory capacity when doing so.

**Mac does not support engine sleep, multi-node inference, or the KV disk tier.** It does not enter idle sleep while an instance is running, but closing the lid still puts it to sleep.

### Mac Environment and Data Notes

- The agent excludes the installation directory and any separately configured model directory from Time Machine backups. Macs can sync models and environment packages with each other.
- When a TUN proxy is enabled, the agent reports the actual network interface address rather than the proxy's virtual address. Engines listen only on `127.0.0.1`, so they do not trigger application firewall prompts.
- Both node and instance terminals are available. The node terminal uses the agent user's zsh; the instance terminal uses the instance's environment, HOME, and working directory. The instance terminal has no container isolation, and closing its tab cleans up processes started in it.

## Installation Directory Checks

The installation directory stores the agent, model cache, and runtime data. On Linux it defaults to `/opt/gpuworks` (for Mac, see the previous section); change it under **Advanced → Installation directory**. The SSH precheck and installation report show the checks below. One-click cloud host installation also checks the directory under **Advanced** in the installation page. Resolve failures or warnings before installing. If the directory does not yet exist, its nearest existing parent is probed.

| Check | Possible status | Meaning and action |
|---|---|---|
| Install directory | Passed / Warning / Failed | **Failed**: The path is not absolute, exists but is not a directory, is not writable, or is mounted with `noexec`. `noexec` causes SSH installation to fail before upload. The installer falls back to `/tmp/gpuworks` only when the directory is left blank and the default directory is not writable; check the final directory and the risk of losing data after a restart. If an explicitly entered directory is not writable, installation fails without changing directories; on a Mac without administrator privileges, the message suggests leaving the directory empty to install in `~/gpuworks`. **Warning**: The probe did not complete; first check the reason in the report |
| Install directory ownership | Passed / Warning | **Warning**: The filesystem rejects `chown`, so udocker workloads run as the agent's own user. This check is skipped without root or passwordless sudo, and is unnecessary on Mac |
| Install directory path access | Passed / Warning / Failed | A root agent runs runveil workloads as the dedicated `udocker` user, which must be able to enter every directory above the installation directory (`o+x`, or `x` for its group). When one is closed, for example an installation under `/root` or a private 0700 home, every launch fails with `permission denied`. The details name the first blocking directory with its permissions and owner. Run `chmod o+x <directory>` (search only: it grants no listing or writing) or choose another installation directory such as `/opt/gpuworks`. **The result is Failed, and SSH installation and cloud one-click installation stop before uploading the agent, only when the probe can establish all of the following** (the API answers 400 `NODE_INSTALL_DIR_UNTRAVERSABLE`): the SSH user is root (the agent runs as root); the agent will use runveil (the install explicitly selects `udocker`, or selects nothing while neither docker nor nerdctl is usable, on x86_64/arm64); it will use the dedicated user (the filesystem accepts `chown`, and the `udocker` account exists or `useradd` can create it); and the directory really is closed to that user (its permission bits refuse and no ACL could allow it). Otherwise the result is a **Warning** that says which link is uncertain and does not stop the installation: the SSH user is not root (with passwordless sudo the agent runs as root only under systemd, a nohup install runs as that user, and the probe cannot tell which one the host gets), docker or nerdctl is usable on the host (the agent prefers them and switches to runveil only where they cannot give containers the GPUs), the install explicitly selects docker/nerdctl, `chown` could not be tested, there is no `useradd` and no account (the agent then runs workloads as itself), or the directory carries an ACL. A cloud one-click installation names no runtime, so the agent picks one in the order above. When node recovery reinstalls an agent that already runs from the directory, the check never stops it and stays a warning. The installation directory itself is opened by the installer (0711) and is not judged. The check is skipped for a non-root user without passwordless sudo, and when the filesystem rejects `chown` (workloads then run as the agent's own user); if the permissions of the parent directories cannot be read it reports "not checked" and does not stop the installation; it is unnecessary on Mac. After installation, node diagnostics report the same problem as `runtime_state_untraversable` |
| Install directory filesystem | Passed / Warning | **Warning**: A network or userspace filesystem; model loading and engine startup depend on its throughput and latency |
| Install directory free space | Passed / Warning | **Warning**: Less than 50 GiB remains. Reserve space based on unpacked image and model sizes; 50 GiB does not guarantee enough capacity for every deployment |
| Install directory file names | Passed / Warning / Failed | If filenames cannot contain `:`, the result is **Failed** without usable Docker, and you must change directories before deploying. With usable Docker, it is a **Warning**, because Docker stores the images. Skipped if the directory is not writable |
| Install directory persistence | Passed / Warning | **Warning**: A container writable layer, `tmpfs`, or `ramfs` may lose the agent, node identity, and cache after recreation or restart. Prefer a compatible persistent volume when you can choose the directory. Cloud container instances using the container writable layer must reconnect after instance recreation |

Interpret the status together with its details: **Passed** can also appear when ownership or filenames were not checked, or when the filesystem type or free space is unknown. A probe timeout or incomplete output produces only an `Install directory` **Warning** and does not prove a directory problem. The report shows the filesystem type and mount point, without listing all mount options.

### Why udocker (runveil) Nodes Cannot Use a Network Volume for Installation

runveil stores image layers and unpacked files under the installation directory. Network volumes that do not support colons in filenames, such as RunPod `/workspace`, cannot store image files such as `sha256:<hex>` and return `Invalid argument`. Do not place the runtime there simply because the volume is persistent.

Without a compatible persistent volume, use a local container disk with sufficient capacity, such as the default `/opt/gpuworks`. Unpacking images typically requires tens of GiB, plus room for model weights. **Recreating the container loses the agent and cache, requiring reconnection and new downloads.** When usable Docker is available, dockerd stores the images, so these image filename restrictions on the installation directory do not apply.

### Accelerator Device Permissions (Non-root Installation)

When the agent runs as a non-root user on Linux, runveil requires that user to have read and write access to the accelerator devices for the relevant vendor. With systemd disabled, nohup runs as the installation user. Even if that user has passwordless sudo, the agent does not run as root.

If the precheck reports an `Accelerator device access` warning, ask an administrator to run `sudo usermod -aG <groups> <user>` with the device groups and user listed in the report. The change takes effect after you log in again. An agent running as root adds the device's groups to workloads, so this step is unnecessary.

This check does not block installation. Docker opens devices as root and is not subject to these user permissions. However, Docker on NVIDIA nodes may still fall back to runveil if NVIDIA Container Toolkit is missing; see [Troubleshooting](troubleshooting.md).

## Multiple Accelerator Vendors on One Host

A node manages cards from only one vendor at a time. The automatic selection order is Ascend, Hygon, PPU, then NVIDIA. If the host has cards from multiple vendors, node details show an **Accelerator** selector; cards passed through to virtual machines are excluded.

1. Stop or move all instances off the node first. Switching is not allowed while any instance remains unstopped.
2. Open **Accelerator** in node details and select the vendor to use. **Auto** shows the current vendor, with a notice for other unmanaged cards.
3. Wait for the **Switching to …** message to disappear and the node to recover, then check the card model, count, and runtime. Switching restarts the agent; the node no longer manages the previous vendor's cards.

The selection persists through restarts, upgrades, and out-of-band reinstalls. If the current container runtime does not support the new vendor, automatic mode may switch to runveil; follow the on-screen prompts to check.

## Connect the Same Host Again

First determine whether you are updating the original node or adding another node on the same host:

- **Update the original node**: Reuse its token, installation directory, and state directory. Linux systemd installation uses the same `gpuworks-agent` service; using another node's token replaces the original agent and takes the original node offline.
- **Add an independent node**: On Linux, choose a different installation directory and disable **Start via systemd** (use `--no-systemd` on the command line), with a separate state directory and token. Agents in other directories and the existing systemd service continue running. The SSH wizard can record the custom directory to simplify later recovery.

`Existing agent` and `Existing node` in the precheck identify existing agents or nodes at the same address. SSH installation asks for confirmation when replacement is needed; nohup mode asks only about conflicts in the same installation directory. **Manual installation commands and offline packages do not wait for confirmation**: replacing another server's agent only prints a `WARNING`. Check carefully before running them.

After replacement, the node on the other server goes offline. Its out-of-band recovery reports `foreign_agent` and does not automatically reclaim the host. For handling this, see [Troubleshooting](troubleshooting.md).

## No Network Connectivity Between Node and Server

The precheck tests connectivity between the server and node in both directions:

| Check | Direction | What it checks |
|---|---|---|
| Server reachability | Node → server | For agent-initiated connections, requests `/healthz` at `server_url` from the node; failure means the node cannot reach this address |
| Server to agent | Server → node | Attempts a TCP connection to the address the server actually dials; interpret the result below |
| Connectivity | Conclusion | If the selected method fails and the other direction works, suggests switching to it. If neither direction works, warns and points to this section |

**Check the Server to agent address.** A port included in the compute node address takes precedence. Without one, the `--listen` port for server-initiated connections is used, defaulting to `18800`.

Act according to the probe result:

- **Connection succeeded or refused**: Before installation, the agent is not listening yet. “Connection refused” also passes because it means packets reached the node and a response returned. With a transparent proxy on the server side, any TCP connection may “succeed”, so a pass may also be a false positive.
- **Timeout**: Check firewalls, security groups, and routing. For a server-initiated connection, this is an **error**; the details prompt you to allow inbound TCP from the server on port `<port>`.
- **This check appears for an agent-initiated connection**: `Server to agent` appears only when the node cannot reach the server. Its result is a **warning** used to determine whether you can switch to the other direction.

If neither direction works, the agent cannot connect even after installation. This is common for cloud instances without a public IP, such as an ECS instance in an Alibaba Cloud VPC with neither an EIP nor a NAT gateway. Choose a solution based on the server's location:

| Server location | Solution |
|---|---|
| Public internet (or with a public endpoint) | Give the node outbound internet access: associate an EIP with the instance or configure a NAT gateway (SNAT) in the VPC, then use the server's public address as `server_url` |
| Same VPC as the node | Use the server's private address directly as `server_url` |
| Another private network (a data center or another VPC) | Connect the two networks through VPC peering, Cloud Enterprise Network (CEN), or a VPN gateway, then use the address reachable through that connection |
| Anywhere, without changing the network | Place a machine in the node's VPC that can reach both the node and server and use it as a reverse proxy (for example, Nginx; see [Production Deployment](deploy-production.md#反代配置要点控制台) for configuration). Set `server_url` to its private address |

If the node can reach the server but the server cannot reach the node (the node is behind NAT), use an agent-initiated connection; no further action is needed.

A node without outbound internet access still cannot download images or models from the internet after the agent is installed. For import methods, see [Offline Deployment: Nodes Without Internet Access](deploy-offline.md#节点不能出公网).

## Upgrade

Upgrade the server first, then the agent; the node's target version comes from the current server. Upgrading briefly interrupts the node's connection and in-flight requests. Upgrade nodes for multi-node inference or PD disaggregation one at a time, waiting for each node to return online.

- **Upgrade from the console**: Select nodes in the node list, click **Upgrade selected Agents**, preview the selected nodes, and confirm to view progress for each node; **Upgrade Agent** in node details also remains available. Wait for successful formal reconnection, then verify that the node is online and its **Agent version** matches the server. Newer agents can receive the upgrade package over the existing connection; older agents still need access to the server's download address.
- **Rerun the installation script or offline package**: Preserve the old binary first, then install using the original token, connection method, installation directory, and state directory. Afterward, check the online status and version. This rewrites the configuration and does not provide the automatic rollback available with self-upgrade.

For manual environments without a persistent agent, you can also use [`gpuworks-agent upgrade`](../reference/cli.md#gpuworks-agent-upgrade). It runs in the foreground and does not restart the original service. It is unsuitable for server-initiated nodes without a server address.

For rollback boundaries before and after the new process registers, failure handling, and model call verification, see [Upgrades and Version Compatibility](upgrade-and-compatibility.md#升级-agent).

## Maintain Nodes: Cordon and Drain

Before upgrading drivers, replacing cards, or decommissioning a node, use **More → Scheduling and drain** for that node in the node list:

- **Cordon**: Stops new replicas from starting on this node. Existing replicas continue serving. Use this for brief maintenance or observation.
- **Drain**: Cordons the node and moves the replicas on it to other nodes, one at a time per deployment. A replacement replica starts elsewhere first;
  the original replica retires only after the replacement is running, so the number of serving replicas never drops below the count before the drain. Single-node replicas are replaced by the candidate pool. Multi-node and PD replicas move too:
  the replacement goes on a node with the same compute network, the same GPU architecture, and free cards, and the placement is rewritten onto it only once it runs; with no node to move to, the replica stays and waits. The page lists the replicas remaining on the node and how the drain handles each, refreshing every 5 seconds.
While a drain is waiting, the dialog shows timestamped scheduling feedback and the latest failed attempt on another node. Historical feedback does not prove that the next attempt will fail. **Stop drain** stops further moves while keeping the node cordoned; work already started may finish. The platform does not automatically enable scheduling or force replicas off.

- **Evict now**: A single-node deployment pinned to this node is not moved by a drain. To clear the node immediately, click “Evict now”.
  It does not wait for replacements: everything is removed immediately (without counting against retries). Those eligible for other nodes are recreated elsewhere; those that specify this node wait until it becomes schedulable again. You can also first use
  “Edit” in deployment details to change the node requirements, so the pinned replicas move normally.
- **Make schedulable**: Restore scheduling after maintenance. Waiting replicas retry immediately.

When a node goes offline unexpectedly, its replicas move to other nodes automatically once it has been offline longer than the deployment's node-loss rescheduling wait (default 10 minutes), without consuming retries. See [Reschedule Replicas on Node Loss](node-failover.md). Serving capacity drops during the wait. Use drain for planned maintenance.

## Uninstall

Before manual uninstallation, add `--dry-run --json` to the same command to inspect the detailed checklist. Active workloads or critical unknowns must be addressed by default, and checks run again after confirmation. Noninteractive execution requires `--yes`. Use `--keep-resources` only when deliberately removing the agent while retaining workloads; it does not release GPUs or delete models for you. See the [agent uninstall reference](../reference/cli.md#gpuworks-agent-uninstall).

1. Stop or move the deployment instances off the node first.
2. Select **More → Delete** in the node list. Normal removal uninstalls the corresponding agent using saved SSH credentials, then deletes the management record only after success is confirmed. Other nodes on the same host are unaffected.
3. Confirm that the node is removed from the list. Normal removal requires saved SSH credentials. If none are configured, the platform retains the node and prompts you to configure SSH and retry. For manually registered nodes that do not support SSH, uninstall the agent manually on the node, then select **Force removal: only delete management records**. If SSH fails or uninstallation cannot be confirmed, the node and credentials are also retained. Fix SSH and retry, or confirm that manual uninstallation has completed before force removal.

If the node is unreachable, you can explicitly select **Force removal: only delete management records**. This skips remote uninstallation, so the agent may still be running.
Uninstall it manually after you regain access to the node. Force removal still cannot delete a node that has instances. If an older agent on the node cannot stop
nohup processes, normal removal fails. Upgrade the agent or manually uninstall it and confirm the result before forcibly removing the record.

You can also uninstall manually from the node's **actual installation directory**. Replace the placeholder in the Linux command with the `--state` directory from that node's original installation command or startup configuration; it is the same as the installation directory unless set separately. With multiple agents on the same host, check which node each belongs to so you do not stop the wrong one. Omit `sudo` for installations that run as an ordinary user and do not need to manage system services:

```bash
sudo ./gpuworks-agent uninstall --state '<actual-node-state-directory>'
```

On Linux, the command stops the corresponding systemd service or nohup process, then removes the node identity, connection configuration, and PID files after confirming that it has exited. It returns a nonzero status on failure. On Mac, use `./gpuworks-agent uninstall` to remove the launchd job; add `sudo` for a LaunchDaemon. The state directory is read from the job.

**Binaries, models, logs, and the state directory are retained.** After a successful manual uninstall, return to the console and select **Force removal: only delete management records** to finish removing the node. To completely remove the data, first confirm that no instances or models are still in use, then have an administrator delete the corresponding directories.
