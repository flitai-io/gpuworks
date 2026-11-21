# ADR 0049: The Server Trusts Forwarded Headers from the Same Host While Public Access Is Enabled

Date: 2026-10-17 · Status: Accepted (supplements [ADR 0045](0045-expose-connector-service.md))

## Background

The connector (cloudflared) for `gpuworks expose` terminates HTTPS and forwards plain HTTP to the server, including the browser's Host and `X-Forwarded-Proto: https`. The server accepts forwarded headers only from peers in `trusted_proxies`; otherwise it calculates its own origin as `http://<host>`. The Origin of browser write requests is `https://<host>`. The mismatch causes login to return `MANAGEMENT_ORIGIN_REJECTED` immediately.

The default `127.0.0.1/32, ::1/128` written by `install` covers only one case: the connector targets loopback, and the server was installed with install. Both cases below produce an entry point where “the page opens, but login fails” (observed on gpu3:8816 on 2026-10-17):

- A server started directly with `gpuworks serve` has an empty `trusted_proxies` by default;
- `expose up --origin` points to a non-loopback address on the same host. The connector's peer address is then the host's network interface address, outside the loopback subnet.

## Decisions

1. **Determine whether the connection comes from the same host, rather than checking an address list**: whenever public access for this data directory is `desired: enabled`, the server additionally trusts connections from the same host. The peer must be loopback, or its address must equal the local address on which this connection arrived. The connector and server share a data directory and therefore must run on the same machine; this condition holds regardless of which local address is used as the origin. External hosts cannot spoof such a source address: the kernel drops incoming packets with a local source address as martian packets, replies cannot reach the sender, and the TCP handshake cannot complete.
2. **Apply this trust only while public access is enabled**: the server watches `<data>/expose/config.json` and rereads it only when the file changes. Trust starts immediately after `expose up` and stops immediately after `expose down` / `rm`, without a server restart. Deployments that do not use expose behave exactly as before.
3. **Use one trust check**: the console's Origin and Secure Cookie checks and the agent hub's `X-Forwarded-For` source check share the same `auth.ProxyTrust`.

## Trade-offs

- **While public access is enabled, forwarded headers from any local process are accepted**, including `ssh -L` forwarding and other local reverse proxies. This is equivalent to install trusting loopback by default. Direct clients can already set their own Origin. Forged forwarded headers neither bypass the browser's same-origin protection nor grant additional privileges; at most, they affect the recorded source IP.
- **Do not switch to a dedicated unix socket channel**: the forwarding target of a named tunnel is configured in the user's Cloudflare route, which we cannot change. We can only check the peer address.
- **Do not make loopback trusted by default for `serve`**: that would change the security defaults of every deployment that does not use expose, and still would not cover `--origin` pointing to a local network interface address.

<!-- translation-source-sha256: cbb192316ac16cf0c928adfd13616ec9846e373ff141ad0e1e487dfb591bd398 # ADR 0049: The Server Trusts Forwarded Headers from the Same Host While Public Access Is Enabled -->
<!-- translation-source-sha256: 03a25d88d3689e53ba01fa382b515b480d96c95c462df41f1733c6ada9665c9b ## Background -->
<!-- translation-source-sha256: 7b3020843d9477c100237401ee598325939b93c4dc303f52ec2e933b428a769b ## Decisions -->
<!-- translation-source-sha256: 72e85fe875bd520392f49ce79dcc67835df77e528be56f98e7b07cec13b00899 ## Trade-offs -->
