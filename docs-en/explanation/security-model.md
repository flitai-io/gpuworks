# Security Model and Trust Boundaries

## Control Plane and Data Plane

Server-persisted sessions protect management APIs: the web UI uses HttpOnly cookies, and non-browser clients can use session-bound JWTs.
Authentication checks session validity and current user permissions. Changing a password atomically revokes all management sessions; signing out revokes the current credential. Idle and absolute timeouts
are enforced by the server. The frontend only reports real user interactions and handles sign-in again; it does not manage refresh tokens. Background polling does not keep sessions alive.

Platform API Tokens continue to protect management APIs and MCP based on the owning user's permissions, token scopes, and independent expiration. They do not participate in login session renewal or password-change revocation.

API keys protect the /v1 data plane and can be limited to chosen model services (deployments): a model outside the scope returns 403, `/v1/models` lists only the models in scope, and batch requests run under the key's current scope. Every `/v1` request must carry a key by default, even when the database holds none; the only exception is the `inference_open_access` security switch (off by default), which an administrator turns on explicitly so that a request without a key passes anonymously while a request that presents one is still verified. An unreadable switch counts as off, so a failed settings read never opens the gateway. A per-node token only proves the identity of an agent connection;
server_dial uses an HMAC challenge to prevent inbound connections from parties without the token.

preflight, peer sync, and model/image streams use
short-lived, single-use credentials. The server coordinates transfers but does not carry model bytes.

## Reverse Proxy TLS and Forwarded Headers

The server uses HTTP natively, with TLS typically terminated by a reverse proxy. Only sources configured in `trusted_proxies` in `server.yaml`
(or `--trusted-proxies`) may supply X-Forwarded-For as the source
address for node connections, shown in logs and as Source address in node details. Otherwise, the socket peer address is used, preventing arbitrary clients from spoofing their source.

The server no longer stores or infers its own external
address (see [ADR 0025](../adr/0025-server-holds-no-external-address.md)), so `trusted_proxies`
does not affect any address generation. In production, put /install.sh, /dl/, and management APIs behind appropriate access controls.

## Credential Visibility

The master secret unlocks AES-GCM-encrypted provider/AI keys and the platform SSH private key, which the server uses to log in to nodes.
Nodes only need to authorize its public key; node passwords are no longer stored in the database (see [ADR 0035](../adr/0035-platform-ssh-key.md)). Administrators who can read the server data directory or server process memory
remain within the trust boundary.

The agent token must be passed to the agent process. It is stored in `gpuworks-agent.conf` in the state directory with permissions 0600, not on the agent's command line. However, root on the same node or users with permission to inspect process
memory or environments may still obtain it. The platform alone cannot replace node isolation and least privilege.

## Playground HTML Preview

Previews of HTML code blocks in the Playground execute model output and treat it as untrusted code. The iframe uses an opaque-origin sandbox without
`allow-same-origin`, so it cannot access the console's session. By default, CSP also prevents it from making network requests from the viewer's browser.
An administrator can allow network access under Settings → Security, but the sandbox remains unchanged. For residual risks, including self-navigation, WebRTC, and content exfiltration through image URLs,
see [ADR 0037](../adr/0037-playground-html-preview-isolation.md).

## Terminals and Instances

Web terminals are subject to concurrency, idle, and kill-switch policies and generate audit records. The instance contract restricts the OpenAI
paths an instance exposes. Sleeping instances return 503 instead of sending requests to stale ports. Logs and external commands are redacted, but release acceptance still needs to
record digests and summaries of commands and environments so that results can be reproduced without exposing secrets.

<!-- translation-source-sha256: d7535388826549d74f0bd75d834c5983a9cde8236e37fa3f581b3bcb3c61eb9a # Security Model and Trust Boundaries -->
<!-- translation-source-sha256: 6ae8ab57bfefbd36d068f2ff28504484f5c9670a6f4e6473a56c727d1f6e6d73 ## Control Plane and Data Plane -->
<!-- translation-source-sha256: 144b02bd8c8254751d900162703b7cebbf56cb41be5a9533595948097034f367 ## Reverse Proxy TLS and Forwarded Headers -->
<!-- translation-source-sha256: dd90038a724192ba43396884d832e438107f66489bad5afeea31efebc59b54b5 ## Credential Visibility -->
<!-- translation-source-sha256: 50192a02699d5056afb0b33166854160c4041ed457d96dadd9bf58d999079a75 ## Playground HTML Preview -->
<!-- translation-source-sha256: 3ae11131e03dbefe7d089f09a5fd85940b4fad5ea2c862dc1a6f8b28f59e63e9 ## Terminals and Instances -->
