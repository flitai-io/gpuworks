# ADR 0012: PublicURL Resolution Order

**Status**: Superseded by [ADR 0025](0025-server-holds-no-external-address.md)
**Date**: 2026-08-23
**Context**: M21 fixes for the public address and installation flow of cloud node onboarding

---

## Context

When installing an agent, the server's external address (`PublicURL` / `server_url`) must be embedded in the launch command. Its correctness directly determines whether the agent can connect to the server—an incorrect address can leave every node permanently unreachable. Commit `51777f2` introduces `inferLocalPublicURL` to automatically detect the local outbound IP and prefill `cfg.PublicURL` at `app.New` startup, aiming to work out of the box without configuring `--public-url`.

However, prefilling has a side effect: all four `if PublicURL == "" → inferRequestBaseURL(r)` checks in the API layer (`api/provider.go:288`, `api/provision.go:41`, `api/api.go:326`, and `app/install.go:117`) become dead code. Regardless of the address used to access the UI (reverse proxy/domain name), installed agents always receive the private IP. A live test on mirror1 on 2026-08-23 shows this: the user accesses the UI through the `a2.tunnelfrp.com:8800` reverse proxy, but the agent on the RunPod pod actually uses `--server http://<private-ip>:8800` (mirror1's private address, unreachable from the pod), leaving the node permanently in the onboarding state.

The central conflict: the prefill strategy cannot provide both out-of-the-box operation (local IP fallback) and correct reverse proxy behavior (request Host takes precedence).

---

## Decision

Reorder PublicURL resolution into three levels, each strictly overriding the next:

1. **Explicit configuration**: `--public-url` / `GPUWORKS_PUBLIC_URL` / the `server_url` field in the request
2. **Inference from the request Host** (`inferRequestBaseURL`): construct the base URL from the request's `Host` or `X-Forwarded-Host` (trusted only when `TrustedProxies` matches) + `X-Forwarded-Proto`; skip this level when Host is loopback (`127.*` / `::1` / `localhost`)
3. **Local outbound IP fallback** (`inferLocalPublicURL`): use only when the first two levels are unavailable; annotate the response with “Inferred from the local network interface; unreachable across network segments”

Implementation:

- `app.New` no longer prefills `cfg.PublicURL`; pass the local IP to `Deps` in a separate `FallbackPublicURL` field
- Consolidate the four API-layer call sites into one `resolvePublicURL(r *http.Request)` helper implementing the three levels above (including a trust-gated XFH check)
- Apply the same logic to the `/install.sh` handler
- Echo the final `server_url` in the response (with its source) for you to verify

Trust `X-Forwarded-Host` only when `TrustedProxies` matches because the bare `Host` header is an intrinsic HTTP field set by the client, whereas `X-Forwarded-*` headers are injected by proxies and can be poisoned by intermediaries. TCP reverse proxies such as frp/ngrok do not change `Host`, so the bare `Host` already covers common usage. Layer 7 reverse proxies should have `TrustedProxies` configured in any case.

---

## Consequences

### Positive

- **Zero configuration for reverse proxies**: when you access the UI through a reverse proxy, installed agents automatically use its address, without manually setting `--public-url`
- **Explicit configuration always applies**: no inference overrides `--public-url`, keeping behavior predictable
- **Out-of-the-box operation is preserved**: local `curl` / browser access from the same network segment can still generate usable commands through the fallback
- **Clear security boundary**: `X-Forwarded-Host` is trusted only for allowlisted proxies, reducing header injection risks

### Negative

- **A loopback Host provides only a heuristic fallback**: when you access the server locally or through SSH port forwarding (Host is `127.*`, `::1`, or `localhost`), remote agents cannot use the request address itself, so resolution must fall back to the local outbound interface address. This address may be unreachable across network segments, NAT, or reverse proxies, so you may still need to configure `--public-url` explicitly or edit `server_url` in the installation form
- **An extra annotation in the response**: the local IP fallback adds “Inferred from the local network interface; unreachable across network segments,” slightly increasing UI complexity—but this is the lowest-cost way to prevent this class of incident

### Neutral

- **XFH is not trusted when `TrustedProxies` is empty**: the default configuration ignores `X-Forwarded-Host` intentionally (security first); if you use a Layer 7 reverse proxy, you must explicitly configure the allowlist
- **Unit tests need adjustment**: existing unit tests explicitly inject `PublicURL`, inadvertently bypassing this bug; add API-level assertions that inference is correct when accessing through a reverse proxy Host

---

## Alternatives Considered

### A. Remove the Local IP Fallback

Return the error “Cannot determine the external address; configure --public-url” immediately for a loopback Host.

**Reason for rejection**: `make demo`, the documentation quickstart, and generating commands through local curl for machines on the same network segment are all real use cases. An error would hurt the out-of-the-box experience. Annotating the fallback result in the response lets you immediately see whether it applies instead of using it blindly.

### B. Always Prefer the Request Host, With No Fallback

Remove `inferLocalPublicURL`; return empty / an error for loopback or when there is no request context.

**Reason for rejection**: the same as A—it hurts the out-of-the-box experience. In addition, the installation command generation API may have no request context (it may be called by a background task), and the lack of a fallback makes such cases entirely unusable.

### C. Replace Prefilling With Lazy Population on the First Request

`app.New` does not prefill the value, but the API layer writes back to `cfg.PublicURL` on the first call to `resolvePublicURL` and reuses it for subsequent requests.

**Reason for rejection**: the first request may use a private or loopback address, contaminating the global configuration once written back. This also introduces state (which caller is first, concurrent races) and adds complexity. Resolving each request independently is simpler.

---

## Update (2026-09-10): Control Plane Self-Installation Does Not Set PublicURL by Default

With the implementation of `gpuworks install` / `gpuworks upgrade`, this decision becomes the installer's default: the
systemd unit and nohup launch arguments generated by install set `GPUWORKS_PUBLIC_URL` **only when you explicitly pass `--public-url`**.
The reason is the same lesson from this incident: prefilling an address during installation (especially for services under `/opt`, which are almost always installed on private networks)
disables both lower resolution levels. Node installation commands then always point to that address, causing the same
failure when using a reverse proxy, changing network segments, or moving to another machine. Leaving it empty by default lets each machine resolve the address through the three levels based on the request Host, making “no configuration” the safe default.

`install.json` records the PublicURL actually supplied, and upgrade uses it to rebuild the unit, so upgrades do not silently enable or disable
this setting. For usage, see [Install and Upgrade the Control Plane](../how-to/install-server.md).

---

## Update (2026-09-25): Access Endpoints on the Settings Page Belong to the Explicit Configuration Level

The Settings page adds “Access endpoints” (the `access` settings: management endpoint, inference gateway endpoint, and default address for adding compute nodes). These belong to
the first, explicit configuration level alongside `--public-url`, and Settings takes precedence over `public_url` in `server.yaml`: node installation uses
the “Default address for adding compute nodes,” then the “Management endpoint” if the former is empty, and returns to this ADR's three-level resolution only if both are empty. All three are empty by default,
continuing the conclusion above that leaving the address unfilled and unconfigured is the safe default. The response's `server_url_source` remains `explicit`.

## Update (2026-09-26): Explicit Configuration Moves to server.yaml

`GPUWORKS_PUBLIC_URL` and `install.json` no longer exist, and unit files no longer carry any settings. The explicit configuration level
now comes from the `public_url` key in `server.yaml` under the installation directory, or the `serve --public-url` command-line argument. The three-level
resolution order itself remains unchanged.

---

## Related

- [M21 Fix Plan](../../artifacts/archive/2026-08-20-images/plan-m21-cloud-install-fix.md): complete incident analysis and fixes
- [CONTEXT.md § Server External Address](../../CONTEXT.md): terminology definitions and Avoid list
- Commit `51777f2`: the original commit introducing `inferLocalPublicURL` prefilling
- Incident evidence: see the symptoms section of the plan for the mirror1 live test record

<!-- translation-source-sha256: 57d0b8291bd5ff51ba0ba5fe8abda51cdeaef97a3d06332cba383ccc319c6a7a # ADR 0012: PublicURL Resolution Order -->
<!-- translation-source-sha256: 6b0f4139b490e9ec4836135805ceda23e8db80e18abd46faa0c5301e1a62ccd2 ## Context -->
<!-- translation-source-sha256: 44669a396239d6f33e9470bc18f7f2e9435dbcc775f708d4e974fab19717e5aa ## Decision -->
<!-- translation-source-sha256: 5a8eb3d3a549bb23e12acaea29779c8f088c52ee888712b2cecaa8478923a227 ## Consequences -->
<!-- translation-source-sha256: f71bea9e4a0cbb0bf0cb1bfa60ccb2f503f128bc0f8d76ceb2f6efebc70a310f ### Positive -->
<!-- translation-source-sha256: 192d1e2567c85cfbccbf8eea0c7b75b85c32ab6cbbb5e6e10cc4976f9d5cbc10 ### Negative -->
<!-- translation-source-sha256: 66b49545ba73fd1af3dca5fb00b0bc48a19e9178ccc7376c4f9ed56050d15a28 ### Neutral -->
<!-- translation-source-sha256: 491082969e3a44712996912fffbb1d1bfb5712755f6cd73328fc5b44e79dea66 ## Alternatives Considered -->
<!-- translation-source-sha256: 16ad5881f585c8802321f64917828c93471d80b8e3f0d8949ac99bf3bbd4daaa ### A. Remove the Local IP Fallback -->
<!-- translation-source-sha256: 0c8af5b5219bafc2fa8bccfd7d3adcea5478eaba44c54ea876b2e441d0d3fe08 ### B. Always Prefer the Request Host, With No Fallback -->
<!-- translation-source-sha256: 6c918d5df0422fb58720fffd5251fce735fb55125ff1b3a46190e635b1945604 ### C. Replace Prefilling With Lazy Population on the First Request -->
<!-- translation-source-sha256: 2eb98bfdb8365b4fc640a59a2a5755c76de5366f8324efea080a11346f6fdc1e ## Update (2026-09-10): Control Plane Self-Installation Does Not Set PublicURL by Default -->
<!-- translation-source-sha256: e9829ae2b0340372163d62ea746332f31202239e4588cfea95f6d3360cfa8917 ## Update (2026-09-25): Access Endpoints on the Settings Page Belong to the Explicit Configuration Level -->
<!-- translation-source-sha256: 2b9c63d1cc3d16a49894e6d12798795da89fcbdcf47d3779f872dd2e4455f86a ## Update (2026-09-26): Explicit Configuration Moves to server.yaml -->
<!-- translation-source-sha256: 009f09a75becdb25729f030eb66a88897c2c20d0d95cbc5dea0ba473f95b346a ## Related -->
