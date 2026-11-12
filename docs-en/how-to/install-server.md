# Install and Upgrade the Control Plane

This guide explains how to install, verify, upgrade, and uninstall the GPUWorks control plane.

**Use cases**: Deploy the control plane for the first time or maintain an existing installation. For a first deployment, start with [Installation](#安装). To update or remove an existing installation, go directly to [Upgrade](#升级) or [Uninstall](#卸载).

## Prerequisites

- The installation host runs linux-amd64 or is an Apple silicon Mac (macOS 15+; see [macOS](#macos) for installation instructions). For a first installation, the file system containing the installation directory must have at least 2 GiB of free space, and no other process may occupy the listening port.
- You need a release package to install or upgrade. If the host cannot download it from the internet, download it on another machine and copy it to the host. For upgrades, see [Offline Upgrade](#离线升级).
- Using systemd on Linux or launchd on Mac requires root or sudo. Without sudo, install to a user directory; see [Systemd and Nohup](#systemd-与-nohup). For container environments, first read the persistence requirements in [Run in a Container](#在容器里运行).

## Replace an Existing Inference Platform

When replacing another inference platform on the same host or reusing its GPU nodes, complete this manual handover before running the installation command. To update an existing GPUWorks installation, use the [Upgrade](#升级) procedure in this guide.

The GPUWorks installer checks whether it can bind the configured listening addresses. This check does not identify other inference platforms, stop their services, or migrate their configuration. An available port does not mean that the selected GPU has been released.

1. **Record and back up the old environment.** Identify the old platform's control plane, node services, inference processes or containers, service management method, listening ports, and installation and data directories. Back up its configuration and data, and record the current application endpoint, models, and settings needed for a rollback.
2. **Stop the services being replaced.** During an agreed cutover window, pause the affected application requests. Use the old platform's management tools to stop its control plane, its node services on the selected nodes, and their inference processes or containers. Also handle their boot startup, container restart policies, or supervisors, and confirm that the services will not restart automatically. Act on each identified service, process, or container individually, and retain the old data.
3. **Confirm port and GPU availability.** Check that the listening addresses and ports selected for GPUWorks (port `8800` by default) are available. Confirm that the old inference processes have exited and that resource usage on the selected GPUs is as expected. Investigate and resolve any remaining occupancy before installing.
4. **Install into separate directories.** Choose GPUWorks installation and data directories separate from the old platform. Do not use the old platform's database or state directory as the GPUWorks data directory. Install the control plane as described below, then [connect the nodes](connect-nodes.md), register models, and create deployments and inference tokens. A fresh server comes with no inference token, and `/v1` requires a key by default, so it refuses every inference request until the first token is created; for genuine anonymous access an administrator must turn on `inference_open_access` under **Settings → Security**, see [Gateway Authentication](../reference/openai-gateway.md#authentication). To reuse old model files, first confirm their format, paths, and read permissions, then follow the model registration procedure. Installation does not automatically import model records, deployments, or tokens from the old platform.
5. **Verify before switching the endpoint.** Complete the health check below, confirm that the nodes are online, and [verify an actual inference request](../tutorials/getting-started.md#6-调用并验证回答). Then update the API address, model names, and tokens used by the application. To roll back, first stop the relevant GPUWorks deployments and conflicting services, then restore the old services and application endpoint from the backups and records. Avoid having both platforms compete for the same port or GPU.

## Installation

### One-Line Install

Run the following in a terminal on the target host (Linux or an Apple silicon Mac):

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh)
```

The script picks the source that responds fastest (in mainland China usually Alibaba Cloud OSS or Huawei Cloud OBS), downloads the release package for the local platform, verifies it against `SHA256SUMS`, and then runs `./gpuworks install`; the interactive prompts that follow are the same as steps 3 and 4 below. If no `release` build exists yet, it installs `nightly` instead.
Arguments after the script are passed to `gpuworks install` unchanged, for example `bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet`. Set `GPUWORKS_CHANNEL=nightly` to choose the channel and `GPUWORKS_SOURCE=<source URL>` to choose the source. The script source is `scripts/get.sh` in the repository.

### Manual Install

To check each step yourself, or when the host can only receive a copied release package, run the following steps in a terminal on the target Linux host.

1. **Download and verify the release package.**

   In mainland China, use Alibaba Cloud OSS (in the example below) or Huawei Cloud OBS at `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`. In other regions, use `https://dl.gpuworks.ai`. All three sources provide the same release packages.

   The example below installs a stable release (the `release` channel; see the [Release Notes](../releases/README.md) for what each version changes). To install the rolling preview instead, replace `release` in the commands with `nightly`. `latest` returns the current release ID for the selected channel.

   ```bash
   src=https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com
   id=$(curl -fsSL $src/release/latest)
   curl -fLO $src/releases/$id/gpuworks-$id-linux-amd64.tar.gz
   curl -fLO $src/releases/$id/SHA256SUMS
   sha256sum -c SHA256SUMS --ignore-missing
   ```

   Confirm that verification returns `OK` for the corresponding archive before continuing.

2. **Extract the package and run the installer in the same terminal.**

   ```bash
   tar -xzf gpuworks-$id-linux-amd64.tar.gz
   cd gpuworks-$id-linux-amd64
   ./gpuworks install
   ```

3. **Enter the installation details when prompted.**

   The installer asks for your sudo password when elevated privileges are needed. Press Enter at subsequent prompts to accept the defaults. The following is an example of the interactive prompts; the listening address depends on what is detected on the host:

   ```text
   Press Enter to take the default in brackets, or type your own value and press Enter.
   install directory (default: /opt/gpuworks/server):
   listen address (default: 192.0.2.10:8800):
   start the service automatically (default: yes)? [yes/no]:
   Admin password (default: random):            ← New installations only
   ```

   - The listening address defaults to the host's egress IP and port `8800`. If you enter only a port number, the default IP is retained.
   - Existing installations retain their current values. If an admin account already exists, the installer does not ask for a password again.
   - Parameters already specified on the command line are not prompted for again. Use `--quiet` for scripted installation; unspecified options use their defaults. See [Interactive Prompts](../reference/install-layout.md#install-的交互提问).

4. **Save the installation summary.**

   Save the access URL, administrator password, actual installation directory, and data directory from the summary, then complete the verification in the next section. The default installation directory is `/opt/gpuworks/server`; for a custom directory, use the one in the summary.

New or changed administrator passwords must contain 8–128 Unicode characters and at most 512 bytes, and must not be the username. Common guessable values, whole-value repetitions and sequences are rejected; character-class mixtures are not required and passphrases are supported. Without an explicit password, a 24-byte random value is encoded as hexadecimal. Existing accounts can still authenticate with their current passwords.

## Verification and First Login

For the password change in this section and subsequent maintenance commands, use the **installed binary** in the actual installation directory. The installer does not add it to `PATH` automatically. Confirm the installation and data directories from the installation summary or this installation's `server.yaml`. The offline upgrade example specifies its execution location separately.

1. In a terminal on the control plane host, check the service. Replace `<listen-address>` with the IP address and port from the installation summary:

   ```bash
   curl -fsS http://<listen-address>/healthz
   ```

   Confirm that the request succeeds, `status` is `ok`, and `version` is the running version. If it fails, check the logs and port as described in [Troubleshooting](#排查) first.

2. Open the access URL from the summary in a browser. Log in with `admin` and the password from the summary, and confirm that you can enter the console.

3. Change the initial password: in the console, open the account menu at the top right and choose **Change password**, then enter the current and the new password. Every session of the account ends, and the console tries to delete `initial-credentials` from the data directory (if that fails, the server log has a warning; delete it by hand). You can also change it from the actual installation directory on the control plane host: for a systemd installation, first run `newgrp gpuworks` as instructed in the summary, or log in again to obtain the new group permissions, then run:

   ```bash
   ./gpuworks reset-password
   ```

   Enter the new password twice at the prompt (not echoed). The command reads the data directory from the `server.yaml` next to the binary; you can also specify it explicitly with `--data`.

4. Log in again with the new password and confirm that login succeeds.
5. Confirm that the `initial-credentials` file in the actual data directory is gone: both a change in the console and `reset-password` try to delete it after succeeding, and warn when they cannot, in which case delete it by hand. This file stores the initial credentials in plain text. Deleting it does not change the account password. See [Control Plane Installation Layout](../reference/install-layout.md#凭证文件) for credential details.

Next, [connect a node and deploy your first model](../tutorials/getting-started.md). Nodes must be able to access the control plane. If you need a domain or a public endpoint, see [Give the Console a Public Address](expose-public-address.md). After deploying, create an inference token on the **API Tokens** page before calling `/v1`: a fresh server ships with no token and `/v1` requires a key by default, so it refuses every inference request until one exists; anonymous access is a separate administrator switch, see [Gateway Authentication](../reference/openai-gateway.md#authentication).

## Systemd and Nohup

If root/sudo is available and systemd is running, the installer configures a systemd service. Otherwise, it uses a nohup background process. Add `--no-systemd` to explicitly choose a background process.

- **Without sudo**: From the extracted release package directory, install to a writable user directory:

  ```bash
  ./gpuworks install --dir ~/gpuworks/server --no-systemd
  ```

- **After a host restart with a nohup installation**: The service does not recover automatically. Run the following in the actual installation directory:

  ```bash
  ./gpuworks restart
  ```

  Wait for the command to finish and confirm that `/healthz` recovers. A nohup process installed by root must be restarted by root.

For service configuration and permissions, see [Installation Layout](../reference/install-layout.md#权限模型). For daemon behavior, see the [CLI Reference](../reference/cli.md#gpuworks-serve).

### macOS

1. On the release site, select `gpuworks-<release_id>-darwin-arm64.tar.gz` and its corresponding `SHA256SUMS`, and download them to your Mac.
2. In a Mac terminal, use `shasum -a 256` to check the archive's SHA-256. Extract it only after confirming that the value matches `SHA256SUMS`.
3. Enter the extracted directory, run `./gpuworks install`, complete the prompts described above, and then finish [Verification and First Login](#验证与首次登录).

On a Mac, the default installation directory is `~/gpuworks/server` (the installing user's home directory), and the server runs as a nohup background process without startup at boot; the automatic-startup prompt defaults to `no`. An agent on the same machine resides in `~/gpuworks` by default, so neither overwrites the other. Run the following as needed from the actual control plane installation directory:

```bash
./gpuworks restart            # Restart the service
./gpuworks stop               # Stop the service; launchd installations start again at the next boot
```

To start at system boot, run `./gpuworks install --autostart` from an account with sudo permission (or answer `yes` to the automatic-startup prompt): this installs a launch daemon, and the service runs as the installing user. Reinstalling over an installation that has a launch daemon keeps the launch daemon by default. For launchd configuration, see [Installation Layout](../reference/install-layout.md#launchd-plistmacos).

### Run in a Container

**Place the installation directory, including its configuration and data, on persistent storage first.** Console upgrades do not update the container image. Recreating the container from the old image without retaining the installation directory reverts to the old version.

Choose a startup method based on the container platform:

- **You can set the startup command**: After installation, set the container startup command to the following foreground daemon, adjusting paths to the actual installation directory.

  ```bash
  /opt/gpuworks/server/gpuworks serve --supervise --config /opt/gpuworks/server/server.yaml
  ```

  This restarts the service when the container restarts and also supports console upgrades. **With this method, do not run `gpuworks restart` or `gpuworks upgrade` inside the container.** Upgrade through the console or restart the container instead. Stopping the container's main process causes the container to exit; this also applies when using `docker run --init`.

- **The platform determines the startup command**: Complete installation inside the container first. After a container restart, use the platform's startup script or run `./gpuworks restart` from the installation directory. The startup script must start the service in the background without blocking. Replace `<dir>` with the actual installation directory:

  ```bash
  setsid nohup <dir>/gpuworks serve --supervise --config <dir>/server.yaml >/dev/null 2>&1 &
  ```

  On AutoDL, you can create `/etc/autodl.sh`; Vast.ai uses `/root/onstart.sh`. On RunPod, you can set the startup command to start sshd first, then `exec` the foreground daemon above. Changing the startup command resets the pod and clears the container disk. Stopping and starting RunPod also clears the container disk; use a `/workspace` network volume to retain the installation.

On Kubernetes, upgrade by replacing the image; do not use console upgrades.

## Run Install Again

To repair the service or switch between systemd and nohup, run `./gpuworks install` again from the release package directory and specify the original installation directory. This replaces the binary, recreates the service, and restarts it while preserving existing configuration, the database, master key, and admin account. Only explicitly specified parameters override their corresponding settings.

`--password` does not change an existing account's password. To change versions, use the upgrade procedure below. Reinstalling does not provide upgrade backups or rollback.

## Upgrade

Review the following effects before choosing a console, command-line, or offline upgrade:

- The console and `/v1` gateway are briefly unavailable. Inference containers and models on nodes are unaffected.
- A backup is created automatically before the upgrade, and failures trigger rollback. Rollback restores the pre-upgrade snapshot, so data changes made during the upgrade may be lost. For backup scope and compatibility, see [Upgrades and Version Compatibility](upgrade-and-compatibility.md).
- This upgrades only the control plane. Agents on existing nodes are not upgraded automatically. For node upgrades, see [Connect, Upgrade, and Uninstall Nodes](connect-nodes.md).

### Upgrade in the Console

The control plane must be managed by systemd, launchd, or `serve --supervise`, must run the binary in the installation directory, and its service account must be able to write to that directory. If these conditions are not met, the page explains why and provides commands to resolve the issue.

1. As an administrator, open **Settings → General → Version and updates** and check the current version of **Server**.
2. Under **Update channel**, select **Release** (stable releases) or **Nightly** (rolling previews), click **Check for updates**, and verify the latest server version.
3. Click **Upgrade** on the **Server** row. Check the target version and interruption notice in the confirmation page, then click **Upgrade**.
4. Wait for the page to recover. Confirm that it reports that the server now runs the target version, and that the **Current** version matches the target. If the new version does not respond within 5 minutes, the page reports an issue. Follow [Troubleshooting](#排查) to check the actual version and logs; do not assume a timeout means the upgrade succeeded or rolled back.

**If the server cannot access the release source**, click **Upload release** in the same row and select a `gpuworks-<release_id>-<os>-<arch>.tar.gz` matching the server's operating system and architecture. **Selecting the file immediately starts upload and upgrade; the online upgrade confirmation page does not appear again.** After completion, verify the result as described in step 4.

If no upgrade action is available on the page:

- If it reports that no service manager is available, run the displayed `restart` command on the host. For containers, use the displayed startup command; see [Run in a Container](#在容器里运行) for specific restrictions.
- If it reports that the server is not installed, is not running the program in the installation directory, or cannot write to that directory, address the corresponding condition first, or [upgrade from the command line](#在命令行升级).
- An uploaded package is rejected before replacement if it does not match, cannot run, is a local build, or targets a version that does not support console upgrades. For older version packages, use [Offline Upgrade](#离线升级).

**Check for new versions automatically** is enabled by default. Checks report the installation size to the publisher. When disabled, use **Check for updates** manually; automatic checks and reporting stop. See the [Configuration Reference](../reference/configuration.md#镜像目录与镜像拉取) for report contents. The update channel applies to both the console and the command line.

### Upgrade from the Command Line

Run these commands in the **actual installation directory** on the control plane host. For a systemd installation, first obtain group permissions as instructed in the summary. If the host has multiple installations, enter the directory of the one you want to upgrade. Older installations with only `install.json` must also use the installed binary in that directory.

1. Check for a new version and verify the target installation and version:

   ```bash
   ./gpuworks upgrade --check
   ```

2. After confirming, run the upgrade and wait for the command to finish:

   ```bash
   ./gpuworks upgrade
   ```

3. Open the console again or call `/healthz` to confirm that the running version matches the target. If the command exits with a nonzero code, follow the error message. A failed health check automatically restores the old version, and the output shows the backup path. If the latest version is already installed, the command exits with `0` without upgrading again.

To use another source, select the appropriate command below to replace steps 1 and 2 above. First append `--check` to verify, then remove it to perform the upgrade:

```bash
./gpuworks upgrade --base-url https://releases.example.internal # Internal release site
```

```bash
./gpuworks upgrade --source https://mirror.example.com/gpuworks # Self-hosted release channel mirror
```

To pin a version, change the number of backups retained, or run unattended, see [CLI Parameters](../reference/cli.md#gpuworks-upgrade). If the channel version is older than the current version, no downgrade occurs by default. Use `--force` only after confirming that you need to downgrade or replace a local build. The same reference page describes directory and verification requirements for a self-hosted mirror.

### Offline Upgrade

1. On a machine with internet access, download a release package matching the target host's operating system and architecture, along with `SHA256SUMS` for the same version. Copy both to the control plane host.
2. Verify from the directory containing the release package. Confirm that the corresponding archive shows `OK`, then check the version inside the package. The example below uses the default Linux installation directory. Replace `<release_id>` with the version identifier in the downloaded package name. For a custom installation or Mac, replace the program path and package name. On Mac, use `shasum -a 256` and compare against `SHA256SUMS`.

   ```bash
   sha256sum -c SHA256SUMS --ignore-missing
   /opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz --check
   ```

3. After confirming, run the upgrade. When it finishes, verify as described in step 3 of [Upgrade from the Command Line](#在命令行升级):

   ```bash
   /opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz
   ```

No extraction is needed. This procedure does not access release sources. A corrupt package, an architecture mismatch, or a local build produces an error before the service stops. Do not substitute `./gpuworks install` from the new package for the upgrade, because that does not provide upgrade backups or rollback.

The new agent is embedded in the server, so you can still click **Upgrade Agent** on the Nodes page without accessing a release source.

## Uninstall

First review check statuses, resource evidence, retained items, and suggested actions. Active workloads or unavailable critical evidence block uninstallation by default; `--yes` does not bypass checks. To retain workloads while removing only the control plane, explicitly use `--keep-resources`; see [CLI uninstall checks](../reference/cli.md#gpuworks-uninstall). Database records do not prove that remote GPUs were released or cloud billing ended.

To preview the full cleanup scope first, run `./gpuworks uninstall --purge --dry-run --json` from the actual installation directory. This preview does not stop services or delete files. Review the list before following the steps below.

1. Run the following in the actual installation directory on the control plane host:

   For a user-directory installation without sudo, run `./gpuworks uninstall` as the original installing user. Also omit `sudo` from the automation examples below.

   ```bash
   sudo ./gpuworks uninstall
   ```

2. Choose whether to clean up data when prompted, review the list, and confirm. systemd, nohup, and macOS launchd use the same command.

   | Choice | Result |
   |---|---|
   | Keep data (default) | Stops the service, disables startup at boot, and removes the program; retains configuration, the database, master key, logs, and backups for reinstallation |
   | Clean up data | Also deletes known configuration, data, backups, and service logs for this installation, including external paths specified in the configuration; always review the list before proceeding |

3. Check the uninstall report. For an actual uninstall, exit code `0` means the selected operation is complete, and `1` means it failed. Check errors and completed items, resolve the cause, and retry. For protected paths, symbolic links, mount points, or paths whose ownership cannot be confirmed, the command reports an error and refuses the corresponding cleanup. Check paths and ownership before proceeding.

For automation, choose one as needed:

```bash
sudo ./gpuworks uninstall --yes                 # Keep data
```

```bash
sudo ./gpuworks uninstall --purge --yes --json   # Remove data and output a report
```

Noninteractive execution requires `--yes` (except for previews); `--quiet` does not skip confirmation. For items retained, such as unknown files, service accounts, and groups, see [CLI Uninstall Details](../reference/cli.md#gpuworks-uninstall).

The expose public endpoint for the same installation is also closed. **Uninstalling the control plane does not remove remote agents, inference services, rented cloud machines, or Cloudflare tunnels / DNS**. Handle them separately; cloud resources may continue to incur charges.

## Troubleshooting

Run `./gpuworks` in the table from the actual installation directory. Confirm `<data-directory>` and `<installation-directory>` from the installation summary or this installation's `server.yaml`.

| Symptom | Checks and Resolution |
|---|---|
| Cannot access the service after installation or upgrade | Check `<installation-directory>/logs/gpuworks-server.log`, resolve errors, and retry; call `/healthz` again and verify the version. If the log location is customized, use the actual configuration |
| `cannot listen on :8800` | Another process occupies the port. First use `systemctl status gpuworks` to check whether it is the old service; do not simply install again |
| `create /opt/gpuworks/server needs root` | Use an account with sudo permission, or use `--dir ~/gpuworks/server --no-systemd` |
| `the service account gpuworks cannot enter /root` | The service account cannot enter a directory above the installation directory, so systemd cannot start the service. Install into the default `/opt/gpuworks/server` or another directory whose every level can be entered; add `--service-user root` only if it must run as root |
| Change the initial password or reset a forgotten password | While you can sign in, choose **Change password** in the console's account menu at the top right; for a forgotten password, run `./gpuworks reset-password` and enter the new password twice at the prompt (not echoed); the data directory is read from the `server.yaml` next to the binary (or specify it with `--data`). Do not leave the actual password in command history or logs. Log in again after the change to verify |
| Permission error when upgrading or resetting a password | The current terminal may not yet have `gpuworks` group permissions. Run `newgrp gpuworks` or log in again, then retry. If the data directory is still not writable, check its permissions |
| The console reports that the channel has no version yet | Switch to a channel with a published release, or wait for a release on this channel and check again |
| Release index signature verification fails | Check the server's outbound proxy, gateway, or login interception. As a temporary alternative, use a matching local upgrade package |
| Cannot connect to the release source | Check the server's outbound connectivity and proxy. Without internet access, upload a release package or perform an offline upgrade |
| Console upgrade times out or fails | Check the service logs and `install-history.jsonl` in the actual data directory, then check the running version through `/healthz`. A page timeout alone does not prove rollback. If the server still cannot start after rollback, follow [Upgrades and Version Compatibility](upgrade-and-compatibility.md) to check and restore the backup |
| All nodes are offline after an upgrade | Check whether **Listen addresses** on the **Settings** page matches the reverse proxy/NAT, then check node versions. See [Configure Node Networks](configure-networks.md#监听地址与别人怎么连过来) |

## Additional Details

For installation directories, permissions, interactive defaults, and backup files, see [Control Plane Installation Layout](../reference/install-layout.md). For parameters, upgrade source selection, and compatibility with older layouts, see the [CLI Reference](../reference/cli.md#gpuworks-upgrade). The background mechanisms below are for troubleshooting and are not steps you need to perform manually.

<details>
<summary>Console upgrade verification, confirmation, and rollback mechanisms</summary>

Online upgrades and uploaded release packages use the same procedure: extract the new binary and test-run `version`, then check support for `console-upgrade-protocol`. Versions that do not support confirming or rolling back their own startup are rejected.

After verification passes, `VACUUM INTO` creates a database snapshot. The snapshot, `master.key`, `server.yaml`, and the current binary are written to `<dir>/backups/<release_id>-<timestamp>.tar.gz`. The process writes `<dir>/tmp/upgrade-pending.json`, replaces the binary, and exits, allowing the service manager to start the new version.

- If the new version responds as healthy within 15 minutes, the upgrade is confirmed, an audit record is written, the marker is deleted, and the 3 most recent backups are retained.
- After 3 consecutive failed starts or persistent unhealthiness, the database, master key, configuration, and old binary are restored on a subsequent startup.
- If the target version in the marker differs from the version actually started (for example, because another upgrade method replaced the binary), the marker is deleted. Startup attempts are not counted, and no rollback is triggered by that marker.

The page waits only 5 minutes, less than the server's confirmation window. You must therefore still check the actual version and logs after a page timeout.

</details>
