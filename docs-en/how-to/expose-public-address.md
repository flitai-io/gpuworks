# Give the Console a Public Address

Open a temporary public endpoint for a running console on a development machine, GPU node, or internal server behind NAT, and close it when you finish. This guide uses `cloudflare-quick`, which is not intended for a permanent endpoint or node connections.

**Prerequisites**:

- The server is running, and you can access the console locally. See [Install and Upgrade the Control Plane](install-server.md).
- You can log in to a terminal on the server host and have read and write access to this installation's data directory. Run `./gpuworks` below from the actual installation directory; the installer does not add it to `PATH` automatically.
- The server host can access the internet to download cloudflared and connect to Cloudflare. The quick provider does not require a Cloudflare account or domain.
- Use it only as a temporary endpoint: the quick address changes when the connector is recreated (restarting the server does not recreate it), and SSE is not supported. Use the server's own stable address when installing nodes.

Each data directory manages **one endpoint**. The following provider is currently available:

| Provider | Preparation | Address | SSE (streaming chat and live logs) | Node Connections |
|---|---|---|---|---|
| `cloudflare-quick` | No account or domain | Temporary random domain; unchanged after a server restart, changes when the connector is recreated | Not supported | Use the server's own stable address |

`cloudflare-named` is not yet available. For alternatives that provide a fixed endpoint, see [below](#尚未开放named).

## Quickest Path: Quick

1. Run the following in the actual installation directory on the server host:

   ```bash
   ./gpuworks expose up
   ```

   Verify that the data directory and forwarding address in the output belong to the intended installation. The command downloads cloudflared if needed, starts the connector, waits for registration to succeed and the public probe to pass, and then prints the address.

2. Open the printed address in a browser outside your network, log in with an existing account, and confirm that this is the intended console. The endpoint status should be `ready`:

   ```bash
   ./gpuworks expose status
   ```

3. When you finish, close the endpoint as described in [Common Commands](#常用命令). After a host restart, the default background mode requires you to run `expose up` again. On Linux, use `--systemd` if you need startup at boot.

**Quick limitations**: The address changes when the connector is recreated; restarting the server itself does not recreate the connector. [Cloudflare Quick Tunnels](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/) does not support SSE or provide availability guarantees. Do not use it to test streaming chat or live logs, or include its address in long-term configuration or node installation commands. When connecting a Slurm cluster, the form warns if the "Management node address" is this address and suggests a stable one (it does not block it); the connect check also finds the nodes that cannot reach it, see [Connect a Slurm Cluster](connect-slurm-nodes.md).

A command timeout or Ctrl-C only stops the wait; **the connector keeps running**. Use `status` to check progress. To cancel the endpoint, run `down`. If the status is `degraded`, follow [Network and Package Sources](#网络与包源), then check again from outside your network.

## Not Yet Available: Named

You cannot currently enable `cloudflare-named`. If you need a fixed domain or SSE, use the fixed endpoint described in [Production Deployment](deploy-production.md). Nodes should use a stable server address they can reach.

## Common Commands

Choose commands as needed; you do not need to run them all in sequence. Continue to run these commands in the server's actual installation directory:

| Operation | Command and Result |
|---|---|
| Start at boot (Linux systemd only; requires root) | `sudo ./gpuworks expose up --systemd`; if it is already running as a background process, run `down` before switching |
| Check status | `./gpuworks expose status` shows the status, address, origin, and last error |
| Get only an available address | `./gpuworks expose status --url-only` exits with a nonzero code if no address is available |
| View connector logs | `./gpuworks expose logs -f` |
| Close the endpoint and retain configuration | `./gpuworks expose down` stops the connector service; in systemd mode, it also removes the unit and requires sudo |
| Close the endpoint and clear local endpoint configuration | `./gpuworks expose rm` deletes configuration, observations, the isolation directory, and the managed token; systemd mode requires sudo |

After closing the endpoint, run `status` again to confirm that it is disabled and no longer returns an available address.

- `status` returns an address only when **the connector service is running + the revision matches + the status is ready**. A lock file does not indicate that the service is online,
  and a stale ready snapshot is not treated as an available address.
- `down` writes the disabled state before waiting for the connector service to exit. `rm` retains the binary cache and logs for troubleshooting, and does not touch tunnels,
  routes, or DNS in Cloudflare—those are your resources. `rm` also does not delete the file specified by `--token-file`.
- If the configuration has not changed, `up` reuses the current endpoint (quick does not unnecessarily switch to a new address).

## Data-dir and Origin

- The data-dir resolution order is: `--data` > `server.yaml` next to the binary > `server.yaml` in the default installation directory
  (`/opt/gpuworks/server/server.yaml`; `~/gpuworks/server/server.yaml` on macOS) > `./data` in development mode. The CLI always prints which one it uses
  (except with `--url-only`, when stdout contains only the address).
- You usually do not need to specify origin: it uses the **actual listening** address recorded when the server starts (wildcard addresses become loopback addresses, with IPv6 also handled).
  If no address is recorded, it uses the first console address in the installation directory's `server.yaml`. If neither is suitable, use `--origin`. After changing the server's listening
  address, run `expose up` again.
- While the endpoint is enabled, the server trusts `X-Forwarded-*` forwarded by connections from the local host (the connector always runs locally, whether origin
  specifies loopback or a local network interface address). You do not need to change `trusted_proxies` for this. After `expose down`, the server returns to trusting only
  `trusted_proxies`. See [ADR 0049](../adr/0049-expose-trusts-same-host-connector.md).

## Network and Package Sources

cloudflared first uses a compatible version in the process `PATH`, then the managed cache. When a download is needed:

- On Linux, it obtains and verifies a deb or rpm from `pkg.cloudflare.com`, then extracts it to the cache without installing a system package.
- On Mac, it downloads the `.tgz` for the corresponding architecture from the cloudflared release on GitHub. If that address is unreachable, place a compatible cloudflared in the `PATH` used to run the command beforehand.

If a download fails or the status is `degraded`, first run `./gpuworks expose status` and `./gpuworks expose logs -f`. Use the last error to check package sources, DNS, and local access to the server. After resolving the issue, check the status again and verify that you can log in from outside your network.

DNS propagation delays and similar issues can temporarily cause the public probe to fail. The connector may still be online and automatically returns to `ready` when probing recovers. Do not recreate the endpoint solely because it is `degraded`, as doing so changes the temporary address.

The egress connection, package versions, and speed measurements at the time (2026-09-22, China Telecom in Shanghai, including quick throughput
around 320 KB/s) represent only that measurement, not a guarantee for other networks or providers.

## Security Boundaries

The endpoint exposes the entire server: UI/API access still requires a login session, and `/v1` still requires an API key by default (only an administrator explicitly turning on `inference_open_access` in the security settings accepts a request without one, see [Gateway Authentication](../reference/openai-gateway.md#认证)), just as on the internal network. There is **no**
path filtering, and Cloudflare Access is not configured. The server itself rate-limits failed logins; configure any additional endpoint policies separately. When adding nodes to your team,
the UI warns you that a quick address is temporary if you enter one.

## The Connector Service Is Separate from the Server

- **Server restarts and upgrades do not affect it**, so the quick address stays the same. While the server is stopped, the address returns a Cloudflare error page,
  and the endpoint status is `degraded`. It automatically returns to `ready` when the server comes back.
- Each time the server starts, it writes its actual listening address to `<data>/expose/server-origin`. `expose up` writes that address into the configuration
  as the forwarding target. You can also specify it with `--origin http://HOST:PORT`.
- **Manual startup by default**: `./gpuworks expose up` starts the connector service as a background process (in a separate session, outside
  the `gpuworks.service` cgroup). It does **not** automatically recover after a host restart; run `expose up` again.
- **`--systemd` (Linux only)**: `sudo ./gpuworks expose up --systemd` instead installs a separate `gpuworks-expose.service`
  (`Restart=on-failure`, enabled at boot).
- The service runs as the data directory owner (the server service account in an installed setup). Running `expose up` as root does not make
  cloudflared run as root.
- After you close the endpoint (`expose down`), the service stops cloudflared and exits. In `--systemd` mode, `down` also removes the unit.

<!-- translation-source-sha256: fccd4531c7348f10448cdb28db6083837b9013a6f2c3df51d9791afc20b27b01 # Give the Console a Public Address -->
<!-- translation-source-sha256: b7c897e7ed1604981da47e4810e5a68d8df1a57b2908d1341b1c26a71f0df9b2 ## Quickest Path: Quick -->
<!-- translation-source-sha256: 70654d42d5852245ddaa3590583f632e1f075ce30936aacce85e5c551c6f874c ## Not Yet Available: Named -->
<!-- translation-source-sha256: b96bd477ead3df7613978e2b15cf02dd8f3aec66fee0853a18ab08b33672d185 ## Common Commands -->
<!-- translation-source-sha256: beac428517b3a19a37f763b0d4b41fe4fdd203877fdc0d25a7b0135967196078 ## Data-dir and Origin -->
<!-- translation-source-sha256: a62a1c710fec65a6a73eaaca02cadfcc7ef4831718cc7bfdba0d4a2c49de99c0 ## Network and Package Sources -->
<!-- translation-source-sha256: ed42b3421697d67d37cb0b01706bb88981e45589166a638d74353be9ac5c6862 ## Security Boundaries -->
<!-- translation-source-sha256: 8832c8cf65117513bef923ba177d074b75f987046cd391b31bdc76e2c1938755 ## The Connector Service Is Separate from the Server -->
