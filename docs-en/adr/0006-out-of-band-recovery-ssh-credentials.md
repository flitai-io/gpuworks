# ADR 0006: Store Encrypted SSH Credentials for Out-of-Band Recovery

Date: 2026-08-24. Status: Accepted.

After a node first connects successfully, the server stores its SSH endpoint and authentication material. Authentication material is encrypted with AES-GCM using the existing `auth.Secretbox` and server master key, and isolated from public node information in a separate table. When the agent control connection fails, recovery follows a fixed order: agent self-reconnection → server_dial redial → out-of-band SSH probing. If SSH is reachable but the agent is not running, restart it first; reinstall it with the original node token only if it still does not reconnect. This recovers from agent process death and cloud instance nonpersistent disk resets without introducing a separate key management system for SSH.

## Trade-Offs and Consequences

- If the server is compromised, the attack surface expands from control plane data to SSH access on every node with stored credentials. Therefore, the credentials API is write-only: responses expose only the endpoint and credential type marker. Modification and clearing require administrator privileges. Protect the database and `data/master.key` as a single sensitive boundary.
- Credential ciphertext shares the same key baseline as provider `creds_cipher`, reducing operational complexity. However, loss of the master key makes both types of credentials undecryptable; backups and restores must include this file.
- SSH uses persistent TOFU host-key verification. A changed host key is treated as unreachable and triggers continued backoff until an administrator verifies and resolves it. Verification must not be relaxed automatically.
- Out-of-band reinstallation preserves the original node identity and desired state. It does not onboard the node again or proactively recreate inference containers that are still running. If registered local models are lost with the disk, reconciliation marks them absent and blocks scheduling until the user registers them again.

## Rejected Alternatives

- Store only public-key fingerprints and rely on the agent to connect back: this cannot perform recovery when the agent process has already died.
- Store no credentials and only alert: this preserves the original security boundary but cannot provide unattended recovery, especially after nonpersistent disk resets.
- Store plaintext credentials: there is no reason to use a lower encryption baseline than existing cloud provider credentials, and a database leak alone would allow direct login to every node.


## Revision (2026-11-04): Sensitive File Permissions

Sensitive files default to owner-only access (`0600`). A Linux systemd installation explicitly bound to this binary and a non-root service account may use its trusted service group (`0660`); group membership grants access to platform credentials. macOS and standalone servers do not implicitly trust ordinary primary groups. Startup, install and upgrade harden existing credentials, databases and sidecars, and recognized backups; temporary database snapshots are owner-only. Failure to protect a file stops the operation: repair the reported owner/service-group permissions and retry. Generated passwords are not written to service logs.

New or changed administrator passwords must contain 8–128 Unicode characters and at most 512 bytes, and must not be the username. Common guessable values, whole-value repetitions and sequences are rejected; character-class mixtures are not required and passphrases are supported. Without an explicit password, a 24-byte random value is encoded as hexadecimal. Existing accounts can still authenticate with their current passwords.
