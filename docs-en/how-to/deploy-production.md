# Deploy in Production

Configure an installed server as an entry point for your team or production environment, and check HTTPS, long-lived gateway connections, and credential backups. This guide covers native TLS and a reverse proxy in front of the server; choose the approach that suits your deployment environment.

**Prerequisites**:

- Install the server as described in [Install and Upgrade the Control Plane](install-server.md).
- Have a TLS certificate that covers the entry domain, and permission to change the server or reverse proxy configuration. If you use a cloud load balancer, be able to manage its listeners, certificates, and timeouts.
- Schedule a restart window for configuration changes that require one; restarting the server disconnects in-flight requests.
- The host tuning commands below apply only to Linux systemd installations. Changes require root or sudo. You do not need root access to the server host if you are only configuring a cloud load balancer.

## Reverse Proxy and TLS

Production endpoints must use HTTPS. Configure native TLS as described below, or terminate TLS with Nginx, Caddy, or a cloud load balancer, and configure
`server.yaml`'s `trusted_proxies` (or `--trusted-proxies`) only for trusted proxy networks. Only
`X-Forwarded-*` headers from these addresses are trusted: `X-Forwarded-For` records the source address of agent connections;
`X-Forwarded-Host` / `X-Forwarded-Proto` / `X-Forwarded-Port` are used for Origin checks on console write requests and
Secure Cookies. None of them generates any external address. The server neither stores nor infers its own external
address: addresses for users and install scripts come from the browser's current address (prefilled throughout the UI and editable) or
an explicit `server_url` supplied by the caller in the API. With a reverse proxy or domain name, this address is naturally the correct one. See
[ADR 0025](../adr/0025-server-holds-no-external-address.md).

HTTPS responses include HSTS, and login and logout cookies use `Secure`; local HTTP development endpoints do not set these HTTPS-specific flags.
Both deployment types return headers that prevent framing and MIME sniffing and set referrer and permissions policies. The global CSP restricts `frame-ancestors`, `base-uri`, and
`object-src`; Playground sandbox previews retain their own script and network policies so that inherited global script restrictions do not break them.

### Native HTTPS

Before configuring native HTTPS, check the following:

- Prepare both the certificate chain and private key, and make sure the service account can read them. The server refuses to start if either is missing or cannot be read.
- Use `listen.console` and a specific local IP address. Startup is refused if TLS uses a wildcard listen address. Do not override it with `--addr`: it does not create the local maintenance listener required for service self-calls and is also rejected at startup.
- All non-maintenance listeners (including node and inference endpoints) use TLS; plaintext requests from the public network cannot log in. Local maintenance endpoints on `127.0.0.1` / `::1` still use HTTP for upgrades, installation, and service self-calls, and must not be exposed to external users.

1. Edit `server.yaml` in the actual installation directory on the server host, replacing the address and file paths with actual values:

   ```yaml
   listen:
     console: [192.0.2.10:8800] # Replace with an actual IP address on this host
   tls_cert_file: /etc/gpuworks/fullchain.pem
   tls_key_file: /etc/gpuworks/privkey.pem
   ```

2. Restart the server using your existing service management method and check the startup logs. Then access `https://<domain>:8800` through a domain covered by the certificate and log in.
3. If you need to update the agent connection configuration, use the corresponding `https://` / `wss://` address. The server does not automatically change addresses already saved by agents.

Native TLS does not obtain certificates for you. Restart the server after updating a certificate for the change to take effect.

### Reverse Proxy Configuration Essentials (Console)

Browser console write requests (login, logout, and all modifications) carry an `Origin` header. The server compares it with
its observed “scheme + host + port” and rejects mismatches with 403 `MANAGEMENT_ORIGIN_REJECTED`. The reverse proxy must forward the host and port
used by the browser unchanged.

1. Define the backend in the Nginx `http` configuration and configure the TLS `server` below. If you already have a TLS virtual host, merge the corresponding `location` into the existing block to avoid creating a duplicate entry point with the same name. Replace `192.0.2.10:8800` below with the console listener address that Nginx can access:

```nginx
upstream gpuworks {
    server 192.0.2.10:8800;
}

server {
    listen 443 ssl;
    server_name gpuworks.example.com;             # Replace with the actual domain
    ssl_certificate /etc/gpuworks/fullchain.pem;
    ssl_certificate_key /etc/gpuworks/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    location / {
        proxy_pass http://gpuworks;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;              # Includes the port; $host does not
        proxy_set_header X-Forwarded-Host $http_host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Upgrade $http_upgrade;        # Agent connections and web terminals use WebSocket
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

2. In GPUWorks `server.yaml`, add the source address Nginx uses to connect to the server to `trusted_proxies`. For Nginx on the same host, use the loopback address it actually uses. For remote Nginx, use its source IP or a controlled network range. Do not use user addresses, domain resolution addresses, or `0.0.0.0/0`.
3. Redirect the plaintext port 80 endpoint to HTTPS, and restrict direct external access to the HTTP port behind the reverse proxy. If you need an inference endpoint, complete the `/v1/` configuration in the next section before applying and verifying it.

Keeping `$http_host` forwards the browser's port unchanged, especially for non-default ports such as `https://<address>:8808`. Trusted proxies also use `X-Forwarded-Proto` and `X-Forwarded-Port` to reconstruct the external entry point. Incorrect configuration can cause login or modification operations to return `MANAGEMENT_ORIGIN_REJECTED`.

### Reverse Proxy Configuration Essentials (/v1 Gateway)

When inference requests pass through a reverse proxy, disable response buffering and allow long requests. Place the following `location /v1/` in **the same TLS `server` block from the previous section**, alongside `location /`; it reuses `upstream gpuworks`. If the inference gateway uses a separate listener address, define another upstream pointing to that listener and replace `proxy_pass` here:

```nginx
location /v1/ {
    proxy_pass http://gpuworks;          # The upstream points to the server
    proxy_http_version 1.1;
    proxy_set_header Connection "";      # Reuse persistent connections to the upstream
    proxy_buffering off;                 # Otherwise SSE is buffered, making TTFT equal to the full response time
    proxy_request_buffering off;         # Forward large request bodies directly without writing them to Nginx disk
    client_max_body_size 256m;           # Match --proxy-max-body-bytes (Nginx defaults to 1m)
    proxy_read_timeout 3600s;            # Time between reads; non-streaming requests wait for generation to finish
    proxy_send_timeout 3600s;
}
```

After configuring Nginx, run `sudo nginx -t` on its host. Only after this succeeds, run `sudo nginx -s reload`. If you changed trusted proxy or TLS settings in `server.yaml`, also restart GPUWorks using your existing service management method. Then complete [Verify the Entry Points](#入口验证).

The server itself sets no read or write timeout on HTTP connections (agent WebSockets and SSE both use long-lived connections). Long streams can only be interrupted by timeouts in an intermediate
reverse proxy, load balancer, or client SDK. Each token in a streaming request resets `proxy_read_timeout`;
**non-streaming** requests send no bytes until generation ends. For generations of tens of thousands of tokens, set the timeout longer than the longest generation time.
Using `stream: true` for all long generations is preferable.

## Gateway Capacity and Host Tuning

All `/v1` data-plane traffic passes through the server process: authentication, replica selection, and forwarding request bodies and streaming responses through agent tunnels.
Estimate capacity using the following three factors (data from the [400-Instance Load Test](../../artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/README.md),
with a fake engine and same-host loopback; measure again in your actual environment):

| Load characteristic | Server CPU | Reference point |
|---|---|---|
| Streaming output | About 35 µs / token (per SSE delta) | 5.9 cores at 4,000 concurrent streams and 170,000 tokens/s |
| Request body upload | About 24 cores / (GB/s) | 9.3 cores at 2,000 concurrent requests, 2.4 MB per request, and about 380 MB/s |
| In-flight connections | About 0.2 MB of memory / stream | About 850 MB RSS with 4,000 long streams |

Example: 400 instances, 16 concurrent streams per instance, and 50 tokens/s per stream produce about 320,000 tokens/s and need about 11 cores;
add more cores for request body bandwidth. The network interface must carry **twice** the request body traffic (incoming from clients and outgoing to nodes).

First decide whether to tune based on current concurrency and observations; you do not need to copy every setting. Run the following commands on the **Linux server host**. The example systemd unit name is `gpuworks`; replace it if you use a custom name.

### Read the Current Configuration

The service must be running. Read the listen queue setting and the service process's actual file descriptor limits, and confirm that `MainPID` is nonzero:

```bash
sysctl net.core.somaxconn
pid=$(systemctl show gpuworks --property MainPID --value)
cat /proc/$pid/limits
```

### Select Changes Based on Load

**Listen queue overflow**

If burst connection load tests cause listen queue overflow, increase `net.core.somaxconn` to 4096 or higher. When the current value is low (such as 128), thousands of clients connecting at once cause SYN packets to be dropped and retransmitted after 1 second, making a batch of requests 1 second slower. Check with `nstat TcpExtListenOverflows`.

Set `net.core.somaxconn` in the configuration under `/etc/sysctl.d/`, run `sudo sysctl --system`, and read the value again to confirm. The server reads the listen queue length at startup, so restart and retest as described in the next section.

**Insufficient file descriptors**

At startup, the server raises the soft limit to the hard limit (Go runtime behavior). Set the hard limit to at least “concurrent clients × 2 + nodes × 4,” plus headroom. Each `/v1` request uses one client connection, and a large request body also uses one temporary file.

The unit generated by `gpuworks install` does not set `LimitNOFILE` and uses the systemd default. For manual deployments or when the default is too low, run `sudo systemctl edit gpuworks` to configure it, for example:

```ini
[Service]
LimitNOFILE=65536
```

`65536` is only an example; choose a value using the concurrency formula above.

**Insufficient temporary disk space for request bodies**

Request bodies larger than 1 MiB or exceeding the process-wide 256 MiB budget are written to the system temporary directory and deleted when the request ends. Go's `os.TempDir()` determines the directory, corresponding to the `TMPDIR` environment variable and defaulting to `/tmp`.

Concurrent large requests occupy “concurrency × request body size” of disk space. XFS also preallocates space for open files, increasing usage to about 2.5 times in testing. For long-context workloads, point `TMPDIR` to a data disk with enough space and avoid using the system disk.

First prepare a directory with enough capacity that the service account can write to, then configure it with `sudo systemctl edit gpuworks`. Replace the example path with your actual directory:

```ini
[Service]
Environment="TMPDIR=/srv/gpuworks-tmp"
```

### Apply Changes and Retest

1. After making the necessary changes, run `sudo systemctl daemon-reload` and restart the service during the maintenance window.
2. Repeat the read commands above to obtain the new process PID, and check `sysctl` and `/proc/$pid/limits` to confirm that the limits have taken effect. If you changed the temporary directory, confirm that it is writable.
3. Retest the original load and observe listen queue overflows, memory, and disk usage to confirm that the corresponding issues have improved.

API keys: the gateway caches validated keys in memory for 5 minutes. Revoking a key through the management API invalidates it immediately; only changes
made directly to the database, bypassing the server, need to wait for cache expiry. Use a separate key for each caller to simplify revocation and troubleshooting with `last_used`
(at minute granularity).

`/v1` requires an API key by default, and production should keep that default. Keyless access is an exception an administrator turns on explicitly:
"Accept /v1 inference requests without a token" (`inference_open_access`, off by default) under **Settings → Security**. With it on, anyone who can
reach the inference listener can call the deployed models anonymously and consume their GPUs, so turn it on only when you really need it (a fully
controlled intranet trial, say) and review what `listen.gateway` exposes in the same change. Do not add a "pass requests without a key" rule in a
reverse proxy or firewall instead of this switch. For the rule and the 401 shapes see [Gateway Authentication](../reference/openai-gateway.md#authentication).

Long requests and upgrades: a server restart or upgrade disconnects all in-flight streams, and responses that have already started are not retried; a node agent
disconnection interrupts all streams on that node. Schedule upgrades for long-generation workloads during off-peak hours, or implement retries for interrupted streams in the caller first.

Gateway metrics (available in the Monitor Center and `/api/v1/metrics/query`): `gpuworks_proxy_requests_total`,
`gpuworks_proxy_request_duration_ms_*`, `gpuworks_proxy_requests_inflight`, plus replica distribution
`gpuworks_proxy_routed_total{deployment,instance}` and retries `gpuworks_proxy_retries_total`.

## Credentials and Backups

Protect `master.key` and the database in the actual data directory, and keep both in backups. The database alone cannot restore the encrypted credentials it contains. Sensitive files default to owner-only access (`0600`). A Linux systemd installation explicitly bound to this binary and a non-root service account may use its trusted service group (`0660`); group membership grants access to platform credentials. macOS and standalone servers do not implicitly trust ordinary primary groups. Startup, install and upgrade harden existing credentials, databases and sidecars, and recognized backups; temporary database snapshots are owner-only. Failure to protect a file stops the operation: repair the reported owner/service-group permissions and retry. Generated passwords are not written to service logs.

After the first login, change the initial password using the [password change steps in the installation guide](install-server.md#验证与首次登录). Verify the new password, then delete `<data>/initial-credentials`. Create separate inference API keys for each caller, and revoke old keys through the management API or console when they are no longer needed. Revoking the last key does not return the gateway to an unauthenticated state: `/v1` refuses every request until another key is created or an administrator explicitly turns on `inference_open_access`. For upgrade backup coverage and restore limitations, see [Upgrades and Version Compatibility](upgrade-and-compatibility.md).

## Operation and Upgrades

Use a dedicated service user and persistent data and model disks; restrict network access to the management API and `/dl/`. On Linux systemd installations,
`gpuworks install` uses the dedicated system user `gpuworks` and creates `<dir>/data`,
plus log and backup directories. Upgrade with `gpuworks upgrade` (stop service → back up database/master.key/old binary → atomic
replacement → health check, with automatic rollback to the snapshot on failure). For manual deployments or custom artifacts, follow the [Release Guide](../dev/release.md)
for atomic replacement and retain a rollback binary. Database migrations only move forward and have no down migrations, so always have a backup before upgrading: `gpuworks upgrade` backs up automatically; for manual replacement, back up the database and `master.key` as described in the upgrade guide. The manual path also has no automatic health checks or rollback. For steps and rollback procedures, see [Upgrades and Version Compatibility](upgrade-and-compatibility.md).

For full installation, credential, directory layout, and rollback semantics, see [Install and Upgrade the Control Plane](install-server.md).

## Verify the Entry Points

After the configuration takes effect, check from a client that uses the actual entry points:

1. Access the HTTPS console and log in. Confirm that there are no Origin rejections or certificate errors.
2. On the **Compute Nodes** page, confirm that nodes that were previously online are back online. If the node endpoint is proxied, confirm that long-lived connections are not repeatedly disconnected.
3. If you configured an inference endpoint, use the call example for a deployed model that supports streaming output to send a `stream: true` request through the new HTTPS `/v1` endpoint. Confirm that output arrives continuously and finishes normally. Use the deployment's actual model type, model name, and API key; opening the page does not replace inference verification.

If verification fails, first check the server and reverse proxy logs. Check trusted proxy addresses, forwarded headers, upstream listeners, and timeout settings separately, then repeat the corresponding checks after correcting the configuration.
