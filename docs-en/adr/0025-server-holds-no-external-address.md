# ADR 0025: The Server Does Not Manage Its Own External Address

**Status**: Accepted
**Date**: 2026-09-27

## Context

The server's external address—the address used to access the console in a browser, the address targeted by `--server` in agent installation commands, and the address in inference gateway integration examples—previously had several conflicting sources: "Access endpoints" on the settings page (the persisted `access` setting), `server.yaml`'s `public_url` / `--public-url`, inference from request `Host` / `X-Forwarded-Host` (ADR 0012), and a fallback to the local outbound interface when inference fails. With these four sources combined, behavior is hard to predict depending on whether the reverse proxy rewrites `Host`, whether `TrustedProxies` is configured, and whether `access` overrides `public_url`. An incident has already occurred where a reverse-proxy setup inferred an internal address that nodes could never reach (see the original ADR 0012).

The fundamental problem is that the server has no reliable way to know "how others connect to me." Different people may simultaneously access the same server through a domain name, a reverse proxy, NAT port forwarding, or direct LAN access. These addresses differ, so any single address stored or guessed by the server is inevitably wrong for some access paths.

There is a similar issue on the serving-network side: `serving_source` requires you to explicitly declare the local source address used by the gateway to connect to a node's serving network, although the routing table can already make that decision. Another configuration item is simply another opportunity for misconfiguration.

## Decision

1. **The server no longer stores or infers its own external address**: remove the "Access endpoints" settings card and the `access` setting (`GET/PUT /api/v1/settings/access`), the `server.yaml` key `public_url` and `--public-url` / `GPUWORKS_PUBLIC_URL`, address inference from request `Host`/`X-Forwarded-Host` (`resolvePublicURL` and related logic), the fallback to the local outbound interface when inference fails, and the explicit `serving_source` source-address setting for serving-network connections.
2. **The party that has the address supplies it; the server does not calculate it**:
   - Wherever the UI provides the server address (adding nodes, cloud-provider installation, integration examples on the API Key page, and "View Code" in Playground), prefill it with the browser's current `window.location.origin`. You can change it to a domain name or reverse-proxy address;
   - On the API side (node creation, SSH onboarding, and cloud-provider installation), accept this address as the required `server_url` field, with no implicit source. Return 400 if it is missing or is not an absolute http(s) address without credentials, a query, or a fragment; do not silently fall back;
   - `/install.sh` contains no built-in address and accepts only the explicit `--server` argument in the installation command;
   - Requests initiated by the server itself ("Initiate from Server" in evaluation) use the loopback address `http://127.0.0.1:<console-port>`, because the server is connecting to itself and there is no question of how others connect to it. Return 503 if no console port is bound;
   - Serving-network connections no longer bind a source address; the local routing table selects the outbound interface based on the destination.
3. **Retain "Listen addresses,"** but give it only this meaning: the local `IP:port` endpoints bound by the server (plus the always-present `127.0.0.1`). Which address others use to connect—domain name, reverse proxy, or NAT forwarding—is entirely their choice, independent of and unaffected by the listen-address value.

## Consequences

- API callers (the UI, scripts, and third-party integrations) must now know and supply the correct `server_url`. There is no longer a fallback that "works without configuration." In return, the address always matches the one the caller actually intends to use, without being overridden or misidentified by server guesses.
- Installed agents recognize only the address baked into the installation command. Changing the node-access listen address, reverse proxy, or domain does not automatically update installed nodes; you must maintain the corresponding forwarding or reinstall them.
- If an old `server.yaml` still contains `public_url` / `serving_source`, startup rejects it as containing unknown configuration keys. Remove them manually before restarting.
- The three-level resolution described in ADR 0012 (explicit configuration > request Host inference > local outbound IP fallback) is no longer current behavior. It is marked as superseded by this ADR, with the original text retained for traceability.

## Alternatives Considered

- **Keep three-level resolution and simply merge "Access endpoints" with `public_url`**: this does not solve the underlying problem. With reverse-proxy rewriting, SSH port forwarding, container NAT, and similar setups, the request Host may still be neither "the address the browser sees" nor "the address the agent should connect to." Merging removes one configuration location but leaves the failure modes unchanged.
- **Have the server probe several candidate addresses and choose the best**: no reliable signal identifies which candidate is reachable for the person making the request. If an unreachable address is selected, failure surfaces only after node installation, making diagnosis harder than the current requirement to provide an explicit address.
- **Remove only the "Access endpoints" settings card and retain file-based `public_url` as a fallback**: in reverse-proxy setups, this value is almost always the internal address at configuration time. Changing subnets or migrating requires remembering to edit it manually. This makes the same class of mistake as the local outbound-interface fallback, just through a different configuration entry point.

## Related

- [ADR 0012](0012-public-url-resolution-order.md) (superseded by this ADR): the original decision and subsequent updates for three-level PublicURL resolution
- [ADR 0030](0030-network-objects.md): node network objects and the definition of the serving-network role
- [Configure Node Networks](../how-to/configure-networks.md): operating instructions for listen addresses and "how others connect"
- `artifacts/archive/2026-09-27-server-address-cleanup/plan.md`: the plan and acceptance records for this change

<!-- translation-source-sha256: 26c338bb3c4a11f62eee0123a3fb24d977e0b2a61b6ae0fef8aced91a973c704 # ADR 0025: The Server Does Not Manage Its Own External Address -->
<!-- translation-source-sha256: 1f212d0ff1e6ad0ba795510c119a18019643eb61e76400113043526ac21f467e ## Context -->
<!-- translation-source-sha256: c97a4877836ea64eeeb947e906462fa84bbbf3607568371a18f5d6022f52ee75 ## Decision -->
<!-- translation-source-sha256: d18c33c698f0bc096527a28a0536effeb9ef630527bf4786f8029ecda1623cb6 ## Consequences -->
<!-- translation-source-sha256: 8cca2078babd343e6c5a4aef6a1f54c86109fecf8c3084f35eee971a4bed497f ## Alternatives Considered -->
<!-- translation-source-sha256: 015f203e03d24d00a6790e8eae63984eda06d0f161e424522a9fbc542f0a375b ## Related -->
