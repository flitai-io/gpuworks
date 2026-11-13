# ADR 0035: The Server Holds a Platform SSH Key Pair and Prefers It for Out-of-Band Recovery

Date: 2026-10-03. Status: Accepted. Revises the "store authentication material" portion of [ADR 0006](0006-out-of-band-recovery-ssh-credentials.md).

On first startup, the server generates an ed25519 key pair (the platform key). It encrypts the private key with `auth.Secretbox` and stores it in the single-row `platform_ssh_keys` table, storing the public key and fingerprint in plaintext. SSH onboarding, preflight checks, out-of-band recovery, and one-click cloud-provider installation all try the platform key first:

- Operators only need to authorize the platform public key once on the target machine; the UI provides a one-line authorization command.
- When a user logs in with a password or their own private key instead, the server appends the platform public key to that user's `authorized_keys` after a successful login (deduplicating by key material). The node's SSH credentials then record only the endpoint and **no longer store a password or private key**. Only if writing fails does the server fall back to storing the user-provided credentials.
- Node SSH credentials do not gain an "authentication method" field: empty secrets mean the platform key. Removing the old fallback that reads the server's `~/.ssh` makes this meaning unambiguous.
- Cloud providers: RunPod appends the public key to the account's SSH keys (injected at Pod startup); Vast attaches it to the instance through its API; CompShare and AutoDL provide only passwords, so the server logs in once with the instance password returned by the API and then writes the public key.

All SSH logins construct their authentication order in one place, `provision.DialSSH` (platform key → user private key → user password → instance password; when a method is explicitly specified, only that method is attempted). Installation reports always include two lines: "SSH authentication" (what was tried and what was accepted) and "Platform public key" (already present / written during this operation / write failed with the reason), so troubleshooting does not require guessing which credentials the server used.

**Current authentication protocol addendum (GWKS-205)**: Requests now accept `auth_mode: "keyboard_interactive"`, reusing `password`. It handles one non-echoing password response (with a bounded number of informational rounds containing no questions), not multiple questions, repeated password prompts, or MFA. An explicit mode still attempts only the selected method. Automatic mode preserves the original order above, then tries keyboard-interactive with the request's or stored `password`. Storage semantics remain unchanged, without a new authentication-protocol field: reading credentials with a retained password still returns `auth_mode: "password"`, which identifies only the credential type; recovery in automatic mode supports both password protocols. Installation and credential saving still prefer authorizing the platform public key, clearing user secrets on success and retaining them only on failure.

## Tradeoffs and Consequences

- The security boundary is the same as in ADR 0006: anyone who can read the database and `data/master.key` can obtain the platform private key and log in to all nodes that authorize it. However, the database no longer stores each node's password long-term, and password changes do not affect recovery.
- One key covers all nodes: if it leaks, regenerate it on the Settings page and authorize it again on every node. Regeneration does not automatically replace the old public key on nodes (that requires logging in to each node, with substantial cost and risk); the UI states this impact in the confirmation page.
- RunPod account keys are injected only at Pod startup. For a Pod that is already running, the user must still provide a private key the first time; afterward, it also relies solely on the platform key.
- Writing `authorized_keys` modifies the target machine. This occurs only during installation and credential saving. Preflight checks remain read-only and report only "will be written during installation."

## Rejected Alternatives

- Read `~/.ssh` for the user running the server: the default deployment runs as the `gpuworks` system user, whose `HOME` is the installation directory and has no `.ssh`; when the unit does not specify `User=`, `HOME` is empty. The key read is often not the one the administrator expects, and no UI shows which key it is.
- Generate an independent key for each node: authorization and rotation must be performed node by node, while the resulting isolation does not hold if the server is compromised (all private keys are in the same database).
- Continue storing passwords: passwords expire or change, and a database leak exposes reusable passwords.
