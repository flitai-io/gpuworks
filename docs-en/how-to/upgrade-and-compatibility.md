# Upgrades and Version Compatibility

This guide covers console, command-line, and offline upgrades, along with limitations and rollback methods when versions differ.

**When to use this guide**: You have a deployed GPUWorks cluster and need to update the control plane (server), connected nodes (agents), or handle a version mismatch between them. For a first deployment, see [Install and Upgrade the Control Plane](install-server.md). When upgrading a cluster, **upgrade the server first, then upgrade agents one at a time**.

## Pre-Upgrade Checks

- Schedule a maintenance window. Upgrades briefly interrupt the console, gateway, and connections to the affected nodes. In-flight requests disconnect; inference containers on the nodes are usually retained.
- Where possible, wait for long operations such as model downloads, peer sync, and image sync to finish before upgrading.
- Confirm the load origin and access endpoint for evaluations. A server restart marks unfinished evaluations with server-generated load as failed. This recovery rule does not affect agent-generated load, but interruptions to the gateway, target node, or load-generating node can still affect evaluations.
- When upgrading the server, ensure that the control plane installation directory has enough space for backups. The upgrade automatically backs up the main database, configuration, master key, and binary. Not all other data is included; see [Backup Scope](../reference/install-layout.md#探活与备份).

## Upgrade the Server

Use the console or command line for an online upgrade. If the control plane cannot access the release source, use an offline package. Choose one of the following methods, then continue to [Upgrade the Agent](#升级-agent). If the server already runs the target version, check the agents directly.

### Upgrade Through the Console

Sign in to the console with an administrator account.

1. Open **Settings → General → Version and updates**, and check the current **Server** version.
2. Select an **Update channel**, click **Check for updates**, and verify the target version.
3. Click **Upgrade** in the **Server** row. Check the version and interruption notice in the confirmation page, then click **Upgrade**.
4. Wait for the page to recover. Confirm that the **Current** version matches the target, then open **Compute Nodes** and wait for previously online nodes to come back online.

If the page indicates that console upgrades are unsupported, follow its guidance or use the command-line method below.

### Upgrade Through the Command Line

Run commands in the control plane host's **actual installation directory**, using an account authorized to maintain the installation. For a systemd installation, obtain group permissions as described in the installation summary; stopping and starting the service still requires sudo. If the server is the main process of a container, first read [Container Limitations](install-server.md#在容器里运行); do not run upgrade commands inside the container.

1. Check for updates and verify the target installation and version:

   ```bash
   ./gpuworks upgrade --check
   ```

2. After confirming, run the upgrade and wait for it to complete:

   ```bash
   ./gpuworks upgrade
   ```

3. Access `/healthz` on the control plane and confirm that `version` matches the target. If the command reports an error, check the logs and actual version before deciding whether to retry or [roll back](#回滚).

To change the update channel, use an internal release site, or constrain the target version, set the [CLI Options](../reference/cli.md#gpuworks-upgrade) and use the same options for both checking and upgrading. To downgrade from a release channel or replace a local build, explicitly use `--force`. Before downgrading, check [Version Compatibility](#版本不一致时会怎样) below.

### Upgrade With an Offline Package

1. On a machine with internet access, download a release package matching the control plane OS and architecture, along with `SHA256SUMS` for the same version. Verify the package's SHA-256.
2. Choose an execution method:
   - **Console upload**: Under **Settings → General → Version and updates → Server**, click **Upload release** and select the verified release package. **Selecting the file starts the upload and upgrade immediately, with no second confirmation.**
   - **Command line**: Transfer the release package and checksum file to the control plane host. Verify them again, then enter the installation directory as described above. Replace the placeholder below with the package's absolute path, and first check the version inside the package:

     ```bash
     ./gpuworks upgrade --archive '<absolute-path-to-release-archive>' --check
     ```

     After confirming, run:

     ```bash
     ./gpuworks upgrade --archive '<absolute-path-to-release-archive>'
     ```

3. Wait for service recovery, then verify in the console or at `/healthz` that the running version matches the target.

You do not need to extract the release package. Do not use `install` from the new package in place of an upgrade, because that provides no upgrade backup or rollback. For verification commands, see [Offline Package Verification](install-server.md#离线升级).

## Upgrade the Agent

Upgrading the control plane does not automatically update agents. Upgrades from the nodes page support Linux and macOS, and the installed version comes from the current server.

**Upgrade nodes used for multi-node inference and PD disaggregation one at a time, waiting for each node to come back online.** A multi-node deployment node being offline for more than 2 minutes fails the entire current generation of instances.

### Upgrade Through the Console

Use an administrator account. The node must be online; older agents also need access to the server's download address.

1. Open **Compute Nodes**, select the nodes to update, and click **Upgrade selected Agents**. The target summary shows the build type, build time, source, and matching release record, with a link to Version and updates. Selection is retained across pages; the toolbar shows the selected count and a clear action. The preview includes only selected nodes and shows the target version and each node's eligibility or reason for skipping or manual action.
2. Verify that the target is the current server version and confirm that workloads can tolerate a brief interruption. A different version string may represent a downgrade; unidentifiable target builds such as `dev` cannot be used for bulk upgrades.
3. View total, processed, succeeded, failed, skipped, and unconfirmed counts, and open per-node results. Only one agent upgrades at a time across the platform; **Upgrade Agent** in node details uses the same queue. Use **Recent Agent upgrades** in the toolbar to view history; progress records remain available after refreshing the page. The latest 20 completed runs are retained; runs with queued work or unresolved reservations are never pruned.
4. Success requires a new formal connection from the target agent version and a normal status report. Transfer or reconnection timeouts pause the queue; unconfirmed means the outcome is unknown and does not mean it is safe to retry. Check the node's processes, version, service logs, and connection state. After confirming that the old process has exited and no upgrade remains in progress, an administrator can release the reservation with an audit note, then preview and confirm the remaining nodes. Continuing creates a linked new batch and preserves the original results.
5. **Stop remaining upgrades** cancels only nodes that have not started. The current upgrade remains under observation. Console server upgrades are rejected while an upgrade is running or an unconfirmed reservation remains unresolved, keeping the target stable.

For older agents that download their binary, the batch package remains pinned during the download. Older server-initiated agents that cannot receive an upgrade package over the connection require the manual procedure below. Upgrading does not automatically drain workloads or guarantee uninterrupted requests; verify workload recovery one node at a time for multi-node inference and PD disaggregation.

### Nodes of a Kubernetes Cluster

Nodes connected through a DaemonSet are upgraded in a batch like any other: the agent runs from `agent/gpuworks-agent` in the node's state directory (`/opt/gpuworks` by default), and an upgrade replaces that program in place, without changing the DaemonSet or reaching a registry, so private registries and offline clusters are covered too.

- A node the preview marks "State directory not executable" could not start from its state directory (for example, it is mounted noexec) and runs the program built into the image; the batch upgrade skips it. Check the state directory on the node, then delete the node's agent Pod to restart it.
- The image tag is the runtime version and rarely changes. When a release raises it, its release notes say so: after upgrading the server, apply the manifest once more (`kubectl apply` or "Apply to the cluster") and the DaemonSet moves to the new tag; when the cluster uses a private registry or an image imported offline, push the new tag to the registry or import it on each node first.
- A node that joins the cluster later first runs the image's own program and switches to the server's current agent before it registers; it needs no upgrade.

See [Connect a Kubernetes Cluster](connect-kubernetes.md).

### Rerun the Installation Script on the Node

If you can sign in to the node, you can also rerun `/install.sh` to update the agent. Use this method if the node is offline or the old version does not support self-upgrade.

1. Retain the old binary, and verify the original node identity and connection configuration. **Reinstallation rewrites the configuration using the supplied parameters and has none of self-upgrade's automatic rollback.**
2. Follow the [Connection Guide](connect-nodes.md#升级) to rerun the installation script with the original connection parameters. Nodes using **Server-initiated connections** use an [Offline Package](connect-nodes.md#离线包).
3. Confirm that the node is back online and its version matches the server, then proceed to the next node.

`gpuworks-agent upgrade` is only for environments where you run the agent manually without a persistent agent service. For options, see the [CLI Reference](../reference/cli.md#gpuworks-agent-upgrade). It runs in the foreground and does not restart the original service. Do not start this command alongside an existing persistent agent. Nodes with server-initiated connections cannot use it to download upgrades.

## Verify the Results

- The control plane runs the target version, previously online nodes are back online, and upgraded agents match the server version.
- Call an existing model and confirm that it responds normally. In environments with no deployed models, check only versions and node status.

## What Happens When Versions Differ

The server and agent currently enforce no minimum release version check. **Being able to connect does not mean all features are compatible.** You can handle nodes individually during an upgrade, but all should use the target version when finished.

With a **new server + old agent**, each feature uses the following conditions. If prompted to upgrade, update the relevant node first:

- **Web terminal**: The agent must declare host terminal capability; container terminals also require container terminal capability for the relevant runtime. Without the corresponding capability, the terminal cannot open and prompts that it is unsupported or that you must upgrade the agent.
- **Pull images and load images from URLs**: The agent must support the corresponding operation. Otherwise, pulling or loading fails with a prompt to upgrade the agent.
- **Download models from HTTP directories**: A parseable release version must be at least `1.0.0`. Older versions are rejected with `MODEL_SOURCE_REQUIRES_NEWER_AGENT`. Unparseable versions such as `dev` or bare commits are not rejected by this threshold; the actual download result still determines success.
- **Tokenizer file probing for evaluations**: If the agent does not support file checks, the recorded path is still tried. If loading fails, a tokenizer warning appears, and workloads that permit estimation continue in estimation mode.
- **dGPU**: The agent must report the required safety capabilities or partition probe results in the current format. Without the required report, dGPU is shown as unavailable with a prompt to upgrade the agent. Even after reporting, it must still pass the corresponding backend capability checks.

With an **old server + new agent**, new features may not be available. Avoid keeping this combination long term; upgrade the server first.

The node list shows **Agent version** by default. Users with a saved layout can enable it in column settings. An offline Agent shows its stored version with a **Last known version** badge. If it has never reported a version, it shows only a **Not reported** badge. Reconnecting updates the classification.

On **Compute nodes**, an eligible older Agent shows **Upgrade available**, a newer Agent shows **Pending alignment**, and unordered nightly or custom builds show **Different build**. Online Agents with a blocker such as a missing package appear under **Currently unavailable for upgrade**. Filter this group to see the reason. Select a count in the notice or use the **Agent status** filter in the table toolbar. Select **All** to see the full list. **Dismiss this notice** hides the notice for this account and target version in the current browser.

After a successful console upgrade, including an uploaded offline release, **Version and updates** shows counts below the Server row if any online Agents fall into these groups. Select **View compute nodes** to open the full list and choose which nodes to maintain. If every Agent is offline or already current, the notice stays hidden. It lasts up to 30 minutes, and refreshing preserves the remaining time. Closing it, leaving the page, or confirming an empty or entirely up-to-date fleet ends it. After a CLI upgrade, open Compute nodes to review Agent versions. Use the same page after upgrading from an older console that did not yet offer this notice. The batch preview shows each eligible node’s current and target versions or build IDs and explains skipped offline or up-to-date nodes.

Database migrations run automatically when the server starts; no separate command is needed. **Migrations only go forward; reverse migrations are unsupported.** To downgrade, restore a pre-upgrade snapshot instead of merely replacing the binary with an old one. The observability database `ux.db` is not included in upgrade backups and needs separate handling if incompatible with the old version; see [Rollback](#回滚).

## If Problems Occur

| Symptom | Action |
|---|---|
| The page reports an upgrade timeout | First check the actual running version, online status, and logs. The server page waits 5 minutes; the agent page waits 2 minutes. A page timeout does not mean the upgrade failed or rolled back. For the control plane, follow [Installation Troubleshooting](install-server.md#排查); for a node, check `agent.log` |
| The upgrade reports an error, the service does not recover, or the new version behaves abnormally | Check the error and logs, then follow the rollback instructions below. Do not judge recovery success solely by a “rolled back” message |

### Rollback

Use this only if the service does not recover after a failed upgrade, or the new version has problems. **Restoring a snapshot loses data written after the snapshot.**

<details>
<summary>Restore the control plane to a pre-upgrade snapshot</summary>

Run the following commands in the actual installation directory on the control plane host, using an account authorized to maintain the installation. If the server is the main process of a container, first read [Container Limitations](install-server.md#在容器里运行); do not run the stop or restart commands below inside the container.

1. Run `./gpuworks stop` and confirm that the server has exited. If the binary cannot run or the old version does not support `stop`, stop the service using its original service manager. Do not modify the database until you confirm that the server has exited.
2. Select a backup for the target old version from `backups/` in the installation directory and extract it into a temporary directory. Confirm that the backup belongs to this installation, and use its `server.yaml` to verify the actual data directory.
3. Remove the current `gpuworks.db-wal` and `gpuworks.db-shm` from the actual data directory. Restore the backup's `data/` members to the data directory, restore `bin/gpuworks` as `gpuworks` in the installation directory, and restore `server.yaml` to the installation directory. Preserve executable permissions and ensure that the original service account can read and write the restored files.
4. **Handle the observability database only if the old version does not support the existing `ux.db`.** If the logs show `uxstore: file schema version N is newer than M`, with the service stopped, move `ux.db` and any existing `ux.db-wal` and `ux.db-shm` from the actual data directory to a separate directory for safekeeping. They are not included in upgrade backups, and automatic rollback does not handle them. The old version creates a new empty observability database, which does not display the original request observations. If compatible, leave them in place.
5. Run `./gpuworks restart` and check that `version` at `/healthz` matches the target old version. Sign in to the console and confirm that nodes are online and model responses have recovered.

</details>

<details>
<summary>Agent automatic rollback and manual rollback</summary>

- A failure during download or verification retains the old binary. It attempts to restore the old version if the new process remains unregistered for more than 6 minutes, or when the fourth start begins after the first three starts failed to register. Crash restarts depend on a service manager such as systemd or launchd; temporary server unreachability can also trigger rollback. After recovery, check the node's failure reason, resolve it, and retry.
- Automatic rollback stops once the new version successfully registers. For a manual rollback, stop the agent on that node, restore the retained `<bin>.old` as the original binary, and start it using the original service method. Verify its version, online status, and model responses.

</details>

For options and backup layout, see the [CLI Reference](../reference/cli.md#gpuworks-upgrade) and [Installation Layout](../reference/install-layout.md). For the connection mechanism, see [Architecture](../explanation/architecture.md#连接与流).
