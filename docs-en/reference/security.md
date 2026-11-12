# Security Defaults Checklist

## Management Sessions and API Tokens

The management API uses persistent sessions: the browser stores an opaque HttpOnly cookie, and the Bearer JWT is bound to the same session. Every authentication check queries the session and the user's current permissions. The database stores only the SHA256 hash of the random credential.

Session invalidation is handled as follows:

- Changing a password and revoking all management sessions happen in the same transaction. Signing out revokes the session associated with the credential submitted for that sign-out.
- Credentials issued before the upgrade that are not bound to a session require you to sign in again.
- SSE rechecks the session before forwarding messages. SSE and terminals also close invalidated connections through revocation events and checks every second; terminal data frames do not trigger a database query for every frame.
- After a separate process runs `reset-password`, connections also close on the next check.
- Requests that have already been authenticated and executed are not retroactively rolled back.

The credentials accepted by each entry point and their uses are:

- `/v1` requires a separate API key by default: a request without a key, or with an invalid or revoked one, returns 401, and that holds when the database contains no keys at all. Keyless access exists only when an administrator explicitly turns on the `inference_open_access` security setting (off by default); with it on, a request without a key is served anonymously, a request that presents a key is still verified, and an unreadable setting counts as off. See [OpenAI-Compatible Gateway](openai-gateway.md#authentication).
- A per-node token (a separate token for each node) is used only for that node's connection and bootstrap (initial onboarding). It is not injected into the public `/install.sh`.
- The management API and MCP also accept platform API tokens (`gwp_`): only SHA-256 hashes are stored, and operations use the owning user's current role with the token's scope applied on top (read-only by default). Tokens can have an expiration and can be revoked; no token can manage tokens. MCP is disabled by default and must be enabled by an administrator.

## Administrator Password

Creating or changing an administrator password (install, first start, Change password in the console, `reset-password`) follows one set of rules:

- 8–128 Unicode characters and at most 512 bytes; no mix of case, digits or symbols is required, and Chinese text or a phrase of a few words is accepted.
- Common weak passwords (including variants with digits or symbols around them, such as `Password123`), whole-value repetitions and sequences (such as `12345678` or `qwertyui`) are rejected. The list is short and is not a database of breached passwords.
- The password cannot be the username (ignoring case, punctuation and leading or trailing digits).
- Control characters are not allowed. Existing passwords are not affected and keep working.

Changing and recovering:

- While you can sign in, choose **Change password** in the account menu at the top right of the console. It changes only your own password and asks for the current one; checking the current password shares the login throttle.
- Afterwards all of the user's management sessions end; platform API tokens and API keys keep working. When `initial-credentials` records this user, the server tries to delete it (`reset-password` does the same).
- For a forgotten password, an administrator runs `gpuworks reset-password` on the server; without `--password` it prompts twice in the terminal without echo, which keeps the password out of command history.
- The console of the experience mode cannot change its password; the main console manages it under **Settings › General › Feature experience mode**.

## Credential Encryption and Installation Files

AI service/cloud provider credentials (AI/provider) are encrypted with the server's AES-GCM secretbox. The master secret is read only from `master.key` in the data directory (generated on first startup).

### Agent Installation

- The install script downloads the agent to a temporary file and then replaces it atomically. The systemd unit and state directory are created with minimum permissions. `identity.json` is deleted only during uninstall.
- The agent's startup configuration (including the node token) is stored only in `<state>/gpuworks-agent.conf`, with permissions 0600, and is written from standard input through `gpuworks-agent configure`.
- The systemd unit, nohup's `agent-restart.sh`, and the launchd plist contain only `run --state <directory>`. The token does not appear in `ps`, `systemctl cat`, or the plist. This configuration file is deleted with the agent during uninstall.
- The offline package `/dl/gpuworks-agent.run` is publicly downloadable (like other `/dl/` files) and contains no token; the token is supplied only on the command line that runs it.

### Control Plane Installation and Upgrades

- In Linux systemd mode, `gpuworks install` runs the unit as the dedicated system user `gpuworks` by default. It explicitly falls back to root only when `--service-user root` is specified.
- The initial password is passed only through `<data>/initial-credentials` (`0600` by default; `0660` for the trusted Linux platform group). The unit file is globally readable, so the password is never written to it.
- `gpuworks upgrade` verifies the release package's sha256 before replacing it atomically. If the health check fails, it rolls back to the pre-upgrade snapshot.
- The server neither stores nor infers its own external address. The address in node installation commands is supplied explicitly by the caller (browser, API caller, or `--server`); see [ADR 0025](../adr/0025-server-holds-no-external-address.md).
- Both installation and upgrades write audit records to `<data>/install-history.jsonl` (`0600` by default; `0660` for installations shared with the trusted Linux platform group).

## Directory Permissions for udocker/runveil

When a root agent runs udocker/runveil as the dedicated user (`udocker`), the state directory contains both the patched PRoot and runtime-mounts that this user must execute or access, and the agent's private identity and logs.

Before each use of the runtime, the agent checks whether the dedicated user can “traverse” (x) the state directory, meaning it can pass through the directory to access files for which it has permission. **If the directory is already traversable, it is left unchanged**, and the permission-granting checks below are skipped.

Only when traversal is unavailable and extra permissions are needed does the agent check the state directory's ownership and write permissions. If the directory is not owned by the agent or is writable by the group or others, it reports an error without making changes. If the checks pass, it grants only traversal permission on the state directory itself, without permission to list the directory or write to it:

| State Directory Group Permissions | Agent Action |
|---|---|
| The group has no permissions, or is already the dedicated user's primary group | Change the group to the dedicated user's primary group and add g+x; directories created by the agent change from 0700 to 0710 |
| The group already has its own permissions and is not the dedicated user's primary group | Keep the group unchanged and add only o+x; 0750 becomes 0751 |

Parent directories are normally managed by operations staff; the agent checks them but does not modify them. AutoDL containers have one exception: when a root agent prepares a path for a non-root runtime user, and either the configured path or its resolved symlink target lies under `/root`, the agent may add only the required search bit (g+x or o+x) to `/root`, provided it is root-owned and not writable by group or others. This does not grant directory listing or write access, and other parent directories remain unchanged. For other parent directories that cannot be traversed, udocker initialization logs only a warning: image management and runveil troubleshooting commands remain available, but starting a workload fails. Preparing runtime-mounts, however, reports an error.

Private files retain their own 0600 permissions.

## Network Connections and Terminal Access

- `X-Forwarded-For` is trusted only for explicitly configured IPs/CIDRs and is used to record the source address of agent connections. TLS is usually terminated by a trusted reverse proxy.
- server_dial uses an HMAC challenge derived from the node token. Temporary preflight/peer ports use a one-time token and a TTL (validity period), and close when finished.
- Service network forwarding is enabled when the node has a service network. The agent listens on port `19443` of the service IP:
  - It uses TLS 1.3, with keys derived from the node token through HMAC. The gateway verifies a pinned public key and does not rely on a CA.
  - Every connection must first present `HMAC(node token, instance UUID)` and can forward only to a loopback port of an instance running on that node.
  - Tokens are not stored in the database or displayed in the UI. Inference engine ports remain published only on `127.0.0.1`.
  - Regenerating the node token rotates the TLS keys and all instance tokens together.
- The gateway's Playground entry point returns 404 for paths outside the API contract. Public `/v1` does not perform this check and lets the engine handle them; see [OpenAI-Compatible Gateway](openai-gateway.md#支持的操作). Sleeping or unavailable instances return 503. Logs redact external commands and credentials.
- The web terminal is subject to a kill switch, concurrency limits, idle timeouts, and audit policies.

For security model tradeoffs (reliance on reverse proxy TLS, token visibility in target processes, and trust boundaries), see [Security Model](../explanation/security-model.md). Production deployments should also back up the database and master key, and rotate the admin password/API key.
