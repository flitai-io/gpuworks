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
    ├── server.lock                 # exclusive lock held while serve runs; holds its pid
    ├── master.key                  # Credential encryption key
    ├── initial-credentials         # Initial password record
    ├── install-history.jsonl       # Installation/upgrade audit log
    ├── plugins/                    # plugin packages: <id>/pkg/<version>/ (read-only), <id>/pkg/current, <id>/control.json, <id>/data/ …
    └── logs.db / metrics.db / ux.db / images/ / catalog-packages/ …
```

`gpuworks plugin` manages `data/plugins/`: a package is unpacked into `.incoming-*.partial/` and verified there, renamed to `<id>/pkg/<version>/` only when every check passes (files 0444, executables and directories 0555), and made current by replacing `<id>/pkg/current` with a rename; the previous version stays for rollback, and the `*.partial` an interruption leaves is removed by the next install. `<id>/data/`, `<id>/tmp/` and `<id>/snapshots/` are the plugin's own directories (0700), kept on uninstall; `--purge` moves the whole `<id>/` to `.trash/<id>-<time>/`. Before an upgrade, with the plugin stopped, the data directory is copied to `<id>/snapshots/<time>-<version>/` (`data/` and `snapshot.json`: the version it came from and the data marker then), written as `.partial` and renamed; the newest 2 are kept, and a purge moves them away with `<id>/`. While an upgrade is in progress `<id>/upgrade.json` (`from`, `to`, `snapshot`, `started`, 0600) exists; it is removed once the upgrade passes its health gate or rolls back, a server that finds it at startup first rolls back to `from` (see [Extension Points](../dev/extensions.md#管理接口)), and an uninstall removes it. `.index/<publisher>/` holds imported package indexes. `maintenance.jsonl` is the offline maintenance journal (0600, owned by the data directory's owner): `gpuworks plugin clock reset` and `pstate clear`, run by root with the server stopped, each append one line, which the server applies and empties at its next start; an entry that fails to apply stays until the start after that (see [CLI Reference](cli.md#维护日志)).

A package that declares `host_setup` also has files outside the data directory, written by root (see [host setup](../dev/extensions.md#主机配置)): `gpuworks plugin host-setup`, `gpuworks install` and `upgrade` write them, and `gpuworks uninstall` removes them:

| Path | Contents |
|---|---|
| `/etc/udev/rules.d/70-gpuworks-plugin-<id>.rules` | One udev rule file per package (0644) matching the USB devices the package declares by vendor and product id, setting their `/dev/bus/usb` node and hidraw nodes to group `gpuworks-usb`, mode 0660; no symlinks |
| `/etc/gpuworks/plugins/<id>.applied` | The applied record (0644, owned by root): the digest of the host steps root applied. The server accepts only a record owned by root and writable by no group or other user, so the service account cannot forge one although it owns the plugins directory |
| System group `gpuworks-usb` | The service's device group: `install` and `upgrade` put it in the service unit's `SupplementaryGroups=`, and `plugin host-setup` adds the service account to it; kept on uninstall |

These paths are host-wide and named by package id only: `install`, `upgrade`, `uninstall` and `--prune` match them to this installation's data directory and remove what another installation on the same host wrote, so only one installation per host applies host setup.

`gpuworks serve` holds an exclusive flock on `data/server.lock` for its whole life and writes its pid into it. A second server on the same data directory waits 10 seconds, then refuses to start and reports the holder's pid; offline commands whose effect a running server's in-memory state would quietly undo, such as `plugin uninstall`, `enable` and `disable`, refuse when they cannot take the lock (see [CLI Reference](cli.md#gpuworks-plugin)). The lock belongs to the process, so the kernel releases it when the process crashes; the file existing does not mean a server is running, and the pid in it may belong to a server that has exited: a pid is reported only when that process is alive, otherwise the message names the lock file. Only servers from this version on hold the lock: during a rolling upgrade, offline commands take it beside a server of an earlier version, so stop that server yourself first.

`server.yaml` resides in the installation directory, rather than the data directory, so restoring the database to another machine does not carry over the old machine's listen address.

`server.yaml` and the installation directory itself provide the inputs for repeated installations and upgrades:

- `upgrade` reads the information it needs from the existing installation, so you do not need to supply the installation options again:
  - **Installation directory**: Uses the directory specified by `--dir`. If unspecified, it uses the installation directory that contains the currently running binary, or the default directory if it cannot determine that directory.
  - **Service mode**: Checks for a systemd unit or launchd plist that points to this binary.
  - **Service user**: Reads `User=` from the systemd unit or `UserName` from the plist.
  - **Current version**: Queries the installed binary.
- Running `install` again preserves the existing `server.yaml` values and overrides them only when you explicitly provide the corresponding command-line options.
  install writes only the listen address, data directory, log file, log rotation, and trusted proxy settings. It preserves keys it does not manage, such as `ssh_proxy` and `metrics_retention`, unchanged.

### Control Plane Installed in the Agent's Shared Root

The default dedicated directory is unchanged. For an existing control plane installed directly in `/opt/gpuworks`, a repair install or an actual upgrade moves the configuration to `/opt/gpuworks/server-config/server.yaml` and retains `/opt/gpuworks/server.yaml` as a controlled relative alias. The service uses the actual configuration path; the binary, PID, upgrade lock, backups, and working directory for relative paths remain associated with the original installation directory.

The shared root remains owned by root and unwritable by the ordinary service. Only the dedicated configuration directory is handed to the control-plane service. Web settings still save through a temporary file and atomic replacement, without granting access to agent binaries, helper libraries, or cache directories. Migration preserves existing comments and unmanaged keys. Arbitrary links, conflicting private files, and dangling aliases are rejected rather than treated as migration inputs.

Older installers handed all of `/opt/gpuworks` to the service account (owner `gpuworks:gpuworks`, mode `2775`). A repair install or an actual upgrade run as root or with `sudo` available first changes that directory itself to `root:root`, `0755` (without setgid), prints one line per change, and adds an `action: layout` entry to the install history (`layout_changes` lists the path and its uid, gid and mode before and after; the configuration move is a separate item). The migration changes only the shared root itself: `server-config/`, the upgrade lock, `tmp/`, `backups/`, and the data and log directories are handed to the service account as usual; the binary in the shared root is owned by root with mode `0755` (root runs it: `sudo gpuworks upgrade` and the hooks that run as root before the service starts) and is not handed to the service account. Before the migration the service account could replace any entry in the directory, so each of the server's entries is checked before the recursive permission changes: symlinks not created by root or the current operator (the data or log directory may be a root-owned link to another disk), entries of the wrong type, and files with more than one hard link are refused, and the error names the path; an arbitrary link or a hard link at `server.yaml` likewise refuses the migration. Other entries that belong neither to the server nor to root are left as they are; each gets a warning and is recorded in the same `action: layout` entry (`foreign_entry_kept`, with owner and mode) for the operator to check where it came from; older installers also handed a colocated agent's files to the service account. The directory must be owned by root, the current operator, or the installation's service account and not be writable by others; the path must not be a symlink, its parents must be trusted, and extra ACLs are refused. Otherwise the run reports an error and changes nothing. Without root and without `sudo`, the run refuses and the error gives the exact command: retry with `sudo /opt/gpuworks/gpuworks upgrade`, or run `sudo chown root:root /opt/gpuworks && sudo chmod 00755 /opt/gpuworks` by hand (GNU `chmod` clears a directory's setgid bit only with the extra leading zero; `chmod 0755` leaves `2755`). `upgrade --check` and upgrades without a version change are read-only and do not migrate. When a failed upgrade rolls back, the previous binary, database and configuration are restored and the directory keeps its migrated shape, in which the previous version runs normally; rollback does not change the directory back to `2775`, which would again give the service account control over files root runs. If the migration fails partway (for example, the owner is already root but the mode is still `2775`), rerun the same command to finish it.

After migration the service account cannot replace the binary in the shared root, so console upgrades show as unavailable (`not_writable`); upgrade from the command line with `sudo /opt/gpuworks/gpuworks upgrade`.

In the shared root a command-line upgrade does not stage the new release in the service account's `tmp/`: the downloaded package and the unpacked binary go to `/opt/gpuworks/.upgrade-staging/` (`0700`), which only the account running the upgrade can enter. Before the move, the upgrade checks that the staging directory is still the private one it created and that the binary still has the content it unpacked (SHA-256); otherwise it refuses the replacement and rolls back. It then makes the binary `root:root`, `0755`, moves it into place, sets the same owner and mode on the installed binary again, and only then starts the service. The staging directory is deleted when the upgrade finishes; if it already exists and is not a private directory of root or the current operator, the upgrade refuses to run. An account other than root cannot create the staging directory in the root-owned shared root, so the command-line upgrade there must run as root.

`upgrade --check` and upgrades without a version change do not migrate files. If the currently installed installer predates this repair, use the binary from a new release package to repair the original installation directory. The configuration archive member remains named `server.yaml`. Ordinary uninstall retains the configuration and compatibility alias; only `--purge` deletes the controlled configuration and alias, without recursively deleting the shared root.

On macOS, if you choose this shared root for the control plane, configuration purge first verifies that the current process can restore files through native filesystem operations. If permissions are insufficient, it refuses the purge and asks you to run the entire uninstall command as root, avoiding an unsafe privileged link fallback.

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
- `Delegate=yes` hands the unit's cgroup to the server, which moves itself into a `server` child cgroup of it and gives each plugin a cgroup with memory, CPU and process limits ([Extension points: trampoline](../dev/extensions.md#跳板gpuworks-plugin-exec)). Command-line `install` and `upgrade` with the core install hooks rewrite the unit and retain this line. Console upgrades leave the unit untouched; without delegation, the RSS watchdog bounds plugin memory.
- The drop-in `/etc/systemd/system/gpuworks.service.d/host-identity.conf` (0644, written only in systemd mode by the command-line `gpuworks install` and `gpuworks upgrade`, removed by `uninstall`; the detached and launchd modes have none): `ExecStartPre=-+/etc/systemd/system/gpuworks.service.d/host-identity-exec host-identity --write /run/gpuworks/host-identity.json --group gpuworks` (without `--group` when the service account is root). `+` runs the step as root, and `-` lets the server start when it fails, only without the two values only root can read. The `/run/gpuworks/` directory it writes is `root:root` with mode 0755 and no extended access or default ACL, and `host-identity.json` is `root:gpuworks` with mode 0440: the service account can read it but can neither change nor replace it. An extended access or default ACL on that directory refuses the write, preventing 0440 from activating inherited third-party read access; `/run` is a memory file system, rewritten at every boot. An install directory with a space or `%` is quoted by the same rules as the unit's `ExecStart`; a path with `$` or a line break is refused. A console upgrade runs as the service account and does not write the drop-in: an installation upgraded only from the console, never through this release's command-line `install` or `upgrade` as root, lacks the step and never reads the system UUID and board serial, with no error (`-` ignores the failure); running `gpuworks install` again as root (a repeat install) or the next command-line `gpuworks upgrade` adds it. So every command-line upgrade rewrites `gpuworks.service` and runs `daemon-reload`, so that a new drop-in takes effect at the restart; a hand edit to the unit file is overwritten, so put your own changes in a drop-in of your own. See [`gpuworks host-identity`](cli.md#gpuworks-host-identity). Root executes the `host-identity-exec` copy beside the drop-in, rather than the service-writable main program. Command-line install/upgrade atomically publishes that copy from the running installer image. The copy and every path ancestor must be root-owned, with no group/other write access, symlinks or extended ACLs. Console upgrades retain the trusted copy without refreshing it from the main program directory; future readers must remain compatible with the existing identity file format. Uninstall removes only this helper and drop-in, preserving other drop-ins.

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
- Each installation and upgrade appends an audit line to `<data>/install-history.jsonl`: action, result, release, operator (taken from `SUDO_USER` under `sudo`), and time. Failures also include `previous_release_id` and the error. The file is created before the installation tree is handed to the service account, so it belongs to that account and a console upgrade, which the service account confirms itself, can append to it. Failing to write the audit line does not fail an installation or upgrade: a console upgrade is still confirmed, and the service log records a `console upgrade confirmed without an install history entry` warning.

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

<!-- translation-source-sha256: 79c5127eb69f9f1057bb34d2ed7802355cab37fbe8b5d7b1bc5cd02aa4bce0d4 # Control Plane Installation Layout -->
<!-- translation-source-sha256: 81aa1d8e680d1990ee0b0ac22653cff0d49bf6d712aa6ed313dd2025b4e5755b ## Defaults -->
<!-- translation-source-sha256: 6c15e754e6324c24d4ef29895e47c2a969ee462b8c645ca90c5d6f6a5e9ad6d7 ## Directory Layout -->
<!-- translation-source-sha256: e76b4ecd2d7749abab3b9cc29338ba4de71d0fcc6c06c0da1bf9b083129a2328 ### Control Plane Installed in the Agent's Shared Root -->
<!-- translation-source-sha256: ac9d8e4cdb7a8e91c0afd9328ddbdacacd2efc87baa439b47f7b203237e3508c ## Permission Model -->
<!-- translation-source-sha256: ff1629e8a3e17cea2e51b5ceb22a0be0ed6d987294e7b9a381e653b892ce2a42 ## Credential Files -->
<!-- translation-source-sha256: 5bec77008dd27bc084c4d4d6c054df69acb034965d65899ae0cad17e979e11cc ## Interactive install Prompts -->
<!-- translation-source-sha256: 469cc8ba96bd861797016a638e7ca401baf6da8d02a864abf90b43cae380acac ## systemd Unit -->
<!-- translation-source-sha256: 9e299898626f679ce3525b0c8b233af42dc19771fbd9992fa6e108fa99ed92e2 ## launchd plist (macOS) -->
<!-- translation-source-sha256: f3336f511d707715008ca8ccb84c36e230415d238ee17e2864c7860656289ea4 ## Health Checks and Backups -->
<!-- translation-source-sha256: c669e1bc87948b3799b28d392546a6c210eb15272e40220e1eed4fa4613c1bf8 ## Logs and Rotation -->
<!-- translation-source-sha256: d8bd9f7b2259ad221d5a309b17db9227de9804409a8f53e4acbe5f514aa5c258 ## Agent Configuration File -->
<!-- translation-localized-code-block: cf0228b7f6dd01a33d1c0533f369be665aca051a5cd6897ffbf9ee07afc42ce0 63316e0dc2128d8d692b7aa4ea5e28a81d5c9405afd4799c5c37297248c76b5a Reviewed localization of the directory annotations, including the plugins/ and server.lock lines; paths unchanged. -->
<!-- translation-localized-code-block: 0a0c78856b3bcc2f70519eaf27daec112ac8a20c1388e0ac5809fef7c3958d06 5126a8bc98f3780a7a6b25daeaa58a61a8cacb4075ece9e6bf89f05f91ec4fd7 Translate explanatory node-token placeholder -->
