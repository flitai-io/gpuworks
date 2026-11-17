# ADR 0045: The Exposure Connector Runs in a Separate Connector Service, Independent of Server Starts and Stops

Date: 2026-10-14 · Status: Accepted; changes the tradeoff in the [Exposure Plan](../../artifacts/archive/2026-09-22-public-expose/plan.md) that “the server manages the connector, and both share the same lifetime”

## Context

Previously, `gpuworks expose` had the server manage cloudflared: a helper watched a pipe held by the server, and any server exit (including SIGKILL) also terminated the connector. The systemd control-group likewise killed it when the server restarted. The rationale at the time was that “an exposure pointing at a stopped server is worse than having to recreate the exposure.”

In practice, this cost is too high: quick tunnel addresses are assigned per connector, so every server restart or upgrade produces a new address and invalidates links already shared. When the server is stopped, however, the old address only returns a Cloudflare error page; it does not send traffic to someone else.

## Decision

1. **A separate connector service**: `gpuworks expose run` runs the original supervisor (cloudflared download, network retries, probes, status), which manages cloudflared. The helper's liveness pipe is rebound to this service, so the connector exits when the service exits (including when killed by SIGKILL). The server no longer manages exposure; it only writes its actual listen address to `<data>/expose/server-origin` each time it starts.
2. **Manual startup by default**: `expose up` starts the service as a background process (in a separate session, outside the `gpuworks.service` cgroup), with no automatic recovery after a machine reboot. `expose up --systemd` installs a separate `gpuworks-expose.service` (`Restart=on-failure`, enabled at boot). `gpuworks.service` itself is not modified.
3. **Write the forwarding target into configuration**: `up` determines the origin in the order `--origin` > the server-recorded address > `server.yaml` in the installation directory, and writes it to `config.json`. An origin change counts as a new request.
4. **An absent server is not a failure**: if the local origin probe cannot reach the server, mark exposure as `degraded` and keep the connector. Restore `ready` when the server returns.
5. **The service exits when exposure is disabled**: after exposure is disabled, the service stops the connector and exits with code 0 (hence `Restart=on-failure` in the unit). `expose down` waits for the service to exit; with `--systemd`, it also runs `disable --now` and removes the unit.
6. **Runtime account**: the service runs as the owner of the data directory. When root executes `up`, it drops privileges to that account; the systemd unit specifies `User=`.

## Tradeoffs

- The address still resolves while the server is stopped, but returns an error page. In exchange, the address stays the same after server restarts and upgrades.
- The default background process does not recover after a machine reboot, so you must run `up` again manually. Explicitly choose `--systemd` for automatic startup. This avoids changing the server unit or adding a boot service that the user did not request.
- Each data directory can have only one connector service (using the existing supervisor lock). Restarting the connector service itself recreates the connector and changes the quick address.

<!-- translation-source-sha256: a34b1e2ac1272ec825132353c4d695a4981f833e49368b534e53b4df00dd3173 -->
