# Control Plane Installation Layout

Use this page to look up the defaults, directory and file permissions, credential files, systemd units, backup contents, and interactive prompt defaults produced by `gpuworks install`. For installation, upgrade, and uninstallation procedures, see [Install and Upgrade the Control Plane](../how-to/install-server.md). For command-line options, see [CLI and Installation Scripts](cli.md#gpuworks-install). For `server.yaml` keys, see [Configuration Reference](configuration.md).

## Defaults

| Item | Default |
|---|---|
| Installation directory | `/opt/gpuworks/server`; on macOS, `~/gpuworks/server` (the home directory of the user who performs the installation, or of `SUDO_USER` under `sudo`) |
| Listen address | `<default-route-interface-IP>:8800` |
| Data directory | `<dir>/data` |
| Logs | `<dir>/logs/gpuworks-server.log`, 10 MB × 20 rotations |
| Backups | `<dir>/backups`, retaining the latest 3 sets |
| Service | systemd unit `gpuworks.service`, running as system user `gpuworks`; on macOS, no service is registered by default and the server runs as a nohup background process (`serve --supervise`); only `--autostart` writes the launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`, running as the user who performs the installation |
| Trusted proxies | `127.0.0.1/32,::1/128` |

The installation directory is a subdirectory of the agent's default directory (`/opt/gpuworks` on Linux, `~/gpuworks` on macOS). When you install the control plane and agent on the same machine, their binaries, state, and data do not overwrite each other.

A leading `~`, `~/x`, or `~user/x` in `--dir`, `--data`, `--logs`, and in the answer to the `install directory` prompt expands to that user's home directory.

The listen address uses the IP of the local default route's outbound interface and port 8800. If no default route is found, it uses the first IPv4 address on a non-loopback, non-virtual interface. Specifying only `:port` for `--addr` binds to `0.0.0.0`.

## Directory Layout

```
/opt/gpuworks/server/
├── gpuworks                        # Installed binary (install copies itself; upgrade replaces it atomically)
├── server.yaml                     # Configuration: listen addresses, data directory, logs, trusted proxies, etc.
├── .upgrade.lock                   # Upgrade mutex lock
├── gpuworks.pid                    # nohup mode only
├── tmp/                            # Temporary storage for downloaded packages and extracted new binaries during upgrades
├── backups/<release_id>-<UTC_timestamp>.tar.gz
├── logs/gpuworks-server.log[.1..20]
└── data/
    ├── gpuworks.db(+ -wal / -shm)  # Main database
    ├── master.key                  # Credential encryption key
    ├── initial-credentials         # Initial password record
    ├── install-history.jsonl       # Installation/upgrade audit log
    └── logs.db / metrics.db / ux.db / images/ / catalog-packages/ …
```

`server.yaml` resides in the installation directory, rather than the data directory, so restoring the database to another machine does not carry over the old machine's listen address.

`server.yaml` and the installation directory itself provide the inputs for repeated installations and upgrades:

- `upgrade` reads the information it needs from the existing installation, so you do not need to supply the installation options again:
  - **Installation directory**: Uses the directory specified by `--dir`. If unspecified, it uses the installation directory that contains the currently running binary, or the default directory if it cannot determine that directory.
  - **Service mode**: Checks for a systemd unit or launchd plist that points to this binary.
  - **Service user**: Reads `User=` from the systemd unit or `UserName` from the plist.
  - **Current version**: Queries the installed binary.
- Running `install` again preserves the existing `server.yaml` values and overrides them only when you explicitly provide the corresponding command-line options.
  install writes only the listen address, data directory, log file, log rotation, and trusted proxy settings. It preserves keys it does not manage, such as `ssh_proxy` and `metrics_retention`, unchanged.

## Permission Model

The installation tree is a shared workspace for the service account and the operator:

| Object | Permissions |
|---|---|
| Linux systemd directories | `2775` (group-writable + setgid; new files inherit the platform group `gpuworks`) |
| Ordinary configuration files such as `server.yaml` | `0664` for shared Linux installations; macOS removes group write permission |
| Database, `master.key`, `initial-credentials`, and backup archives | `0600` by default; `0660` for an explicitly authorized Linux platform group |
| Binary | `0755` |
| `data/install-history.jsonl` | `0600` standalone, or `0660` with a trusted Linux service group |
| systemd unit / launchd plist | `0644` (the plist is owned by `root:wheel`, as required by launchd) |

- install runs **as the operator**, using separate `sudo` commands for only a few steps: initially creating the `/opt` directory, creating the service account, writing the systemd unit, running `systemctl`, and assigning the tree to the service group at the end. `sudo` prompts for a password on the first privileged command, then does not prompt again while the credentials remain cached (typically 15 minutes, depending on the host's sudoers configuration).
- During the first installation, files in the tree (the binary, configuration, and credentials) cannot yet be written directly. The installer automatically falls back to `sudo` and applies chown to the tree at the end. Subsequent installations and upgrades write directly.
- In systemd mode, the operator is added to the reserved platform group `gpuworks`, so subsequent `reset-password` operations do not require `sudo`, nor do backups and binary replacement during `upgrade`. `upgrade` still uses `sudo` for `systemctl` and the final `chown`/`chmod`. The new group does not take effect in the **current terminal**; run `newgrp gpuworks` or log in again.
- nohup mode has no privileged steps: the process runs as the caller and the caller owns the directory, with the sole exception of initially creating the `/opt` directory. The nohup process belongs to the installer: if root installs it, only root can restart it with `gpuworks restart --dir <install-dir>`.
- Preflight requirements: root or usable `sudo` (otherwise, installation falls back to nohup with a warning; if `sudo` is also unavailable and `--dir` is not writable, installation is refused with a prompt to use a user-level directory); the target port is not occupied by another process (except the current installation); and the filesystem containing the installation directory has at least 2 GiB free.

Sensitive files default to owner-only access (`0600`). A Linux systemd installation explicitly bound to this binary and a non-root service account may use its trusted service group (`0660`); group membership grants access to platform credentials. macOS and standalone servers do not implicitly trust ordinary primary groups. Startup, install and upgrade harden existing credentials, databases and sidecars, and recognized backups; temporary database snapshots are owner-only. Failure to protect a file stops the operation: repair the reported owner/service-group permissions and retry. Generated passwords are not written to service logs.

The authorized group is the reserved platform group `gpuworks`, not an arbitrary primary group of a custom service account. The systemd unit explicitly records `Group=gpuworks`, and actual file GIDs must match it. Legacy default-service installations can retain their existing `gpuworks` operator-group authorization; unknown installations stay owner-only. Rolling back to an unfixed binary may reintroduce its older permissive behavior; upgrade to a fixed release again. If preflight finds legacy files that the operator lacks permission to harden, installation or upgrade is refused before stopping the service; retry as the file owner or root. Privileged writes require a validated trusted directory; if directory validation fails, run the complete command as the owner or root.

On Linux, group authorization first clears POSIX extended ACLs from sensitive files. Shared database directories, directories used for privileged writes, and sensitive directory chains receiving installation group permissions reject extended or default POSIX ACLs, preventing extra access through file replacement or sidecar inheritance. If the error identifies an ACL, review and remove unnecessary ACLs before retrying.

## Credential Files

Unit files are globally readable, so the initial password is **not** written to the unit file. Instead, it is stored in `<data>/initial-credentials` (JSON, `0600` / `0660`, plaintext):

```json
{"user": "admin", "password": "...", "created_at": "2026-09-10T09:00:00Z"}
```

Password precedence during install:

1. `--password`;
2. The `GPUWORKS_ADMIN_PASSWORD` environment variable (displayed for confirmation in interactive mode; if rejected, the installer continues to prompt);
3. An interactive prompt (with echo disabled; pressing Enter generates a random password; no prompt if an admin account already exists);
4. A randomly generated password when `--quiet` is set or stdin is not a terminal.

When the service starts for the first time, password precedence for creating the admin account is `GPUWORKS_ADMIN_PASSWORD` > the `initial-credentials` file > random generation. If you run `gpuworks serve` directly without install, you can set the initial password through the environment variable. The password ultimately used is written back to that file. `GPUWORKS_ADMIN_PASSWORD` is read only when creating the admin account. After the account it records changes its password (in the console or with `reset-password`), the server or the command tries to delete the file, whose password no longer works; a failure is only logged as a warning, so remove the file by hand then. A record for another account is left in place.

If `gpuworks.db` already exists in the data directory, the admin account is considered to exist: password processing is skipped, the database and accounts remain unchanged, and `--password` is ignored with a prompt to use `gpuworks reset-password` instead.

## Interactive install Prompts

When output is a terminal and `--quiet` is not set, install first prints a banner (a block-style version when the terminal locale is UTF-8; otherwise, plain ASCII), then prompts for each item. Press Enter to accept the default in parentheses:

| Prompt | New installation default | Existing installation default | When supplied on the command line |
|---|---|---|---|
| `install directory` | `/opt/gpuworks/server`; `~/gpuworks/server` on macOS | Same as on the left (`--dir` determines the directory) | `--dir` skips the prompt |
| `listen address` | Detected `<IP>:8800` | First console listen address in `server.yaml` | `--addr` skips the prompt |
| `start the service automatically` | `yes`; `no` on macOS | `no` if the original installation uses nohup; otherwise `yes`. On macOS, `yes` if the original installation has a launch daemon; otherwise `no` | `--autostart` or `--no-systemd` skips the prompt |
| `Admin password` (echo disabled, final step) | Random | No prompt if the database already has an admin account | No prompt if `--password` is supplied or `GPUWORKS_ADMIN_PASSWORD` is confirmed |

- Entering only a port number for the listen address preserves the default address and replaces only the port. Other input must use `IP:port`; otherwise, the prompt repeats.
- Answering `yes` to automatic startup is equivalent to `--autostart`, and `no` to `--no-systemd`. Only `yes`/`y`/`no`/`n` are accepted.
- When `--quiet` is set or stdin is not a terminal, no banner or prompts are shown, and all defaults are used. The final summary is still printed, including the administrator credentials.

## systemd Unit

The service unit reads the server configuration through `server.yaml`; it does not contain these configuration values or passwords:

- `ExecStart=<dir>/gpuworks serve --config <dir>/server.yaml`; nohup mode runs the same command line, with identical logging, data, and health check behavior.
- `User=` is the service account specified by `--service-user`; `Restart=always` and `RestartSec=3`; `TimeoutStopSec=30` allows ongoing model and image sync operations time to shut down gracefully.
- `KillMode` retains the default (control-group): the server does not own inference containers, and its child processes should exit with it (the agent unit uses the opposite behavior).

The unit is written only when root or usable `sudo` is available, `systemctl` exists, and `/run/systemd/system` actually exists (a container with systemctl installed but no systemd does not qualify). On macOS, the launchd plist below is written only with `--autostart` (or a `yes` answer to the prompt, or when reinstalling over an installation that has a launch daemon). Otherwise, installation uses a nohup background process with a PID file.

## launchd plist (macOS)

On macOS, the systemd unit's equivalent is the launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist` in the system domain. It likewise points only to `server.yaml` and contains no settings or passwords:

- `ProgramArguments` matches the unit's `ExecStart`. `UserName` is the user who performs the installation (taken from `SUDO_USER` under `sudo`), rather than root, and no dedicated account is created. `--service-user` is ignored on macOS, with a warning if you explicitly specify a different account. The service runs as root only when root itself performs the installation.
- `RunAtLoad` + `KeepAlive`: starts at boot and restarts 3 seconds after exiting (`ThrottleInterval`), corresponding to the unit's `Restart=always`; `ExitTimeOut` of 30 seconds corresponds to `TimeoutStopSec`. `AbandonProcessGroup` is not set, so child processes exit with the server.
- The environment variable `GPUWORKS_LAUNCHD=1` tells the server that launchd manages it, enabling upgrades and restarts from the console (launchd starts it again after it exits).
- `StandardErrorPath` is `<dir>/logs/gpuworks-server.stderr.log`, which captures only output before server logging opens and runtime output during crashes.
  Normal logs go to the rotating `gpuworks-server.log`. If `gpuworks-server.stderr.log` exceeds the rotation size limit, it is cleared at startup.
- The installation tree belongs to the runtime user (`chown -R <user>:<primary-group>`); the user is not added to any group.

## Health Checks and Backups

- After starting the service, install and upgrade poll `/healthz` for up to 60 seconds. The probe uses the loopback address corresponding to the listen address (`127.0.0.1` for a wildcard address).
- In nohup mode, this wait ends early if the process has already exited. This mode has no supervisor: once the process left running at startup disappears, nothing can respond, so waiting for the full timeout only delays rollback (for example, if a new version exits immediately because a dynamic library is missing or it rejects a configuration setting). Both systemd units and launchd plists restart a crashed server, so the full 60 seconds allows retries and does not end early in those modes.
- Backups use `backups/<release_id>-<UTC_timestamp>.tar.gz` and contain `data/gpuworks.db` (including `-wal` / `-shm`), `data/master.key`, `server.yaml`, and `bin/gpuworks`. They exclude other data directory contents such as models and images. Restore first deletes the old database's WAL/SHM files to prevent a write-ahead log produced by the new version from being replayed against the old schema.
- Each installation and upgrade appends an audit line to `<data>/install-history.jsonl`: action, result, release, operator (taken from `SUDO_USER` under `sudo`), and time. Failures also include `previous_release_id` and the error.

## Logs and Rotation

The server writes to both stderr and `<dir>/logs/gpuworks-server.log`. Logs rotate at 10 MB and retain 20 rotations (adjustable through install's `--log-max-size` / `--log-keep`; when running `serve` directly, use `--log-file`, with an empty string to write only to stderr). Rotation is implemented natively and does not depend on logrotate. In systemd deployments, `journalctl -u gpuworks` shows the same output.

In launchd deployments, the file receiving stderr has no rotation mechanism. When the server detects that stderr is a regular file, it writes normal logs only to the rotating log file (see the previous section). If the log directory is not writable, it falls back to stderr without preventing startup.

When a write to the log file fails (for example, on a full disk), the file stops and records still go to stderr. The file is retried every 30 seconds; once that works, a `log file writes resumed` line comes first, carrying the number of records dropped (`dropped`), when writing stopped (`since`) and why (`err`), with no restart needed. The agent's log file behaves the same way.

When the server exits because of an error, such as an occupied listen address, it records the reason in the same log with `msg="server exited"`. In nohup mode, stderr points to `/dev/null`, so this log is the only place to see the reason.

## Agent Configuration File

This section covers the agent on a node, rather than the control plane. The agent's startup configuration is stored in only one place: `gpuworks-agent.conf` in the state directory (default: `/opt/gpuworks`; `~/gpuworks` on macOS). The file has permissions `0600` and contains the node token. Only `gpuworks-agent configure` writes it (called by the installation script; see [CLI](cli.md#gpuworks-agent-configure)), rewriting the entire file on every installation.

```
# gpuworks agent configuration, written by `gpuworks-agent configure`.
SERVER=https://gpuworks.example.com:8800
TOKEN=<node-token>
CONNECT_MODE=agent_dial
LISTEN=
RUNTIME_DRIVER=
MOUNT_ROOT=
UDOCKER_BUNDLE=
```

- Each line uses `KEY=value`. Values are read literally, without quoting, escaping, or variable expansion. Lines starting with `#` and blank lines are skipped; unknown keys are ignored, so files written by a newer installation script do not prevent older agents from starting.
- `SERVER` is populated only for `agent_dial` nodes; it is empty for `server_dial` nodes, which never connect to the server. `LISTEN` applies only to `server_dial` and defaults to `0.0.0.0:18800`.
- Every startup method runs only `gpuworks-agent run --state <state>`:

| Startup method | Command |
|---|---|
| systemd unit `gpuworks-agent.service` | `ExecStart=<dir>/gpuworks-agent run --state <state>`; the rest of the unit (`ExecStartPre`, `KillMode=process`, `Restart=always`, `RestartSec=3`) is independent of the configuration |
| nohup | `nohup <dir>/gpuworks-agent run --state <state>`; `<state>/agent-restart.sh` (`0700`) is a restart script for the same command; the PID is in `<state>/gpuworks-agent.pid` |
| launchd (macOS) | `ProgramArguments` is `<dir>/gpuworks-agent run --state <state>`, with plist permissions 0600 |
| Restart after a manual `gpuworks-agent upgrade` | `run --state <state>`; only older agents without a configuration file include the full set of arguments |

- The token therefore does not appear in `ps`, `systemctl cat gpuworks-agent`, the plist, or `agent-restart.sh`.
- To change the configuration, rerun installation (or `configure`), then restart the agent. For temporary overrides, use command-line flags or environment variables (see [`run`](cli.md#gpuworks-agent-run) for precedence).
- A missing file is not an error: older agent installations do not have it and continue to run from their own command-line arguments. Upgrading the binary does not change their startup method; they migrate to the file only after you rerun the installation script.
- `gpuworks-agent uninstall` also deletes this file.
- When the installation script, SSH wizard preflight checks, or out-of-band recovery need to identify the server or token associated with an agent on a host, they read this file (and read the older agent's command line only if the file is missing). For the design rationale, see [ADR 0051](../adr/0051-agent-launch-configuration-file.md).
