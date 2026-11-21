# ADR 0040: Server Upgrades and Official Catalogs Use a Signed-Index, Multi-Source Distribution Channel; the Last Official Catalog Import Takes Effect

Date: 2026-10-11 · Status: Accepted

## Context

The server needs three things from the publisher: new binary versions, the system image catalog, and the built-in inference template catalog. Previously, each used a different route: `gpuworks upgrade` accessed
`gpuworks.zstack.ai` (whose public DNS resolves to a private IP); "Check for updates" for the image catalog accessed `image-manifest.gpuworks.ai` (an overseas CDN proxying to Hong Kong);
inference templates allowed only a single custom URL. Tests during evening peak hours showed access from mainland China to overseas CDNs at only tens to hundreds of KB/s, with stalls. `.ai` cannot obtain an ICP filing, preventing any CDN from using mainland China nodes.
Default domains for domestic object storage do not require an ICP filing and are one to two orders of magnitude faster (measurements in `artifacts/work/2026-10-11-update-distribution/`).

Also, after either official catalog was updated online, the server reset the database to the version embedded in the binary on every startup, invalidating online updates after a restart.

## Decision

1. **One distribution channel, multiple byte-for-byte identical sources.** The release tree (`<channel>/index.json`, `releases/…`, `catalog/…`) is hosted simultaneously on the default domains of Alibaba Cloud OSS and Huawei Cloud OBS
   and on Cloudflare R2 (`dl.gpuworks.ai`). Default sources are constants in the binary; the CLI can override them with `--source`.
2. **Clients request indexes from all sources concurrently and use the first one that passes signature verification and parsing.** Sources are not tried sequentially, so bad sources do not add delay. Files are downloaded from the winning source first, switching sources on failure and resuming with Range,
   with timeouts determined by "how long no new bytes have arrived." Finally, files are checked against the size and sha256 in the index.
3. **Trust comes from the signature, not the source.** The index is an ed25519 signed envelope (signature and body in one file), with the public key compiled into the binary and files constrained by the sha256 in the index.
   Any source (an overseas CDN, a customer-hosted mirror, or an offline copy) is merely transport; tampered indexes or files are rejected.
4. **Two channels, selected per installation.** `nightly` (rolling previews, built on every master build) and `release` (official releases, `v*` tags). The choice is stored in `server.yaml` as
   `update_channel`, read from the same place by the console and `gpuworks upgrade`. If unset, it follows the build-injected `version.Channel`. Only versions built later than the current build count as updates,
   so switching channels does not prompt a downgrade.
5. **The last official catalog import takes effect.** At startup, the embedded image and inference template catalogs are replayed only when the server build differs from the build at the last embedded catalog replay; otherwise, the database retains
   the result of the last import. Catalog version numbers are not compared. "Update available" is determined by the build time of the release containing the catalog: if the channel's release is older than the current server build (switching from nightly to release, or a build ahead of the channel), its catalog is not considered an update, though it can still be uploaded and applied. Online inference template updates reconcile by upstream source; user profiles are unaffected (ADR 0011).
6. **The server initiates checks, throttled by time.** When the login page opens, the browser asks the server to start a check (no login required, no results returned, and only one check runs at a time). After an administrator logs in, the page header reads the results. The server checks externally at most once per hour for the same channel; the browser does not connect directly to distribution sources.
   Results therefore reflect the network of the server that actually downloads files, signature verification happens only on the server, and customers only need to allow one outbound connection.

## Tradeoffs

- Default sources use provider default domains rather than a custom domain with an ICP filing: they require no filing and are usable the same day, at the cost of dependence on provider policies (Alibaba Cloud has already disabled mainland China default public domains for new OSS users).
  The three sources are independent; if one fails, the others continue to work, and the constants can be updated in a new version.
- Not comparing catalog versions means upgrading to a server version with an older embedded catalog initially reverts to that catalog, requiring the administrator to check for updates again. In return, no version-number ordering rules are needed,
  and there is no irreversible state where "the catalog in the database is newer than the current code can understand."
- Restarting the same server build no longer replays the embedded catalog, so approaches that rely on "replay at every startup" to populate new columns take effect only when the build changes. New columns already arrive with new builds, so this remains unaffected.
- The signing private key exists only in protected CI variables. During rotation, old and new public keys coexist in the binary for one release cycle.

## Consequences

- `gpuworks upgrade` uses the distribution channel by default. `--base-url` remains for internal distribution sites (legacy format, unsigned, with trust provided by TLS), and repeatable `--source` and `--channel` options are added.
- "Check for updates" for both the image catalog and inference templates retrieves files from the distribution channel; the `template_catalog_url` setting is removed.
- The Settings page has a unified "Version and updates" section, where you can select a channel and see current and latest values for all three items (without displaying distribution sources). The page header indicates new versions, and all three items support update uploads in the UI. During a UI upgrade, the server replaces itself and exits, and the service manager starts the new version (so this is available only with a service manager: a systemd unit, or the `serve --supervise` supervising parent process when systemd is unavailable; added on 2026-10-12). The startup guard handles rollback on failure.
- R2 continues to serve the same image catalog at `image-manifest.gpuworks.ai/manifest.json`, leaving older versions unaffected.

## Addendum (2026-10-17): Update Checks Report Installation Scale With the Request

The publisher needs to know the number of installations, their scale, and hardware. Distribution sources are static object storage and cannot receive writes, so reports must be included as query parameters on index requests. The publisher aggregates them from OSS, OBS, and R2 access logs (without creating a collection service).

- **What is reported**: an installation identifier (a one-way value generated with HMAC using master.key, used only to count the same installation's hourly checks once), server version, total and online node counts, agent version distribution, and GPU model distribution; each distribution has at most 16 entries. Reports exclude hostnames, IPs, users, deployments, and models. For the complete parameter list, see the [configuration reference](../reference/configuration.md#镜像目录与镜像拉取).
- **Sent only to built-in sources**: clients using `--source` or self-hosted sites receive ordinary requests; command-line paths such as `gpuworks upgrade` do not report.
- **Switch** (revised on 2026-10-22 by [ADR 0054](0054-usage-report-and-notices.md): reporting is now controlled by `usage_report`, while `auto_check_updates` only controls automatic checks; the following is the decision at the time): `auto_check_updates` in `server.yaml`, with a corresponding switch in "Version and updates" on the Settings page. Enabled by default. When disabled, the server does not check automatically (login-page prefetch becomes a no-op, and page loading shows only the result of the last manual check). Manual "Check for updates" still reads the index but also omits the report. Automatic checking and reporting are deliberately tied to one switch because reports accompany checks, leaving customers only one switch to understand: when disabled, no check includes a report.
- **Tradeoff**: query parameters are written to access logs, and the publisher is responsible for object-storage log retention and access control. R2 does not enable access logging by default, so the publisher must enable it. For servers without an install ID (test fixtures without master.key), `id` in the report is empty and ignored in aggregation.

## Supplement (2026-10-28): Version Tags Order by Version Number, and the Image Catalog's Fixed Address Moves into the Channel

Releases now use calendar versions `vYY.M.PATCH` ([Release Guide](../dev/release.md#version-numbers)), and long-term support release lines will follow. Ordering by build time alone would treat a patch to an older release line that was built later (`v26.11.2` after `v26.12.0`) as an update.

- **Ordering**: when both sides are `vYY.M.PATCH`, they are compared by version number; otherwise (a nightly's short commit hash, a local build with a suffix) they are still compared by build time. Both "only a version built later than the current build counts as an update" in decision 4 and "the build time of the release the catalogs belong to" in decision 5 order by this rule. Nightly and release are still compared by build time, so switching channels still never offers a downgrade.
- **Publishing side**: the `release` channel follows only the newest release line, and `publish_dist.sh` refuses to publish a tag below the channel's current version. Long-term support release lines will get channels of their own rather than going into `release`.
- **The root `manifest.json` is removed**: there are no older versions to stay compatible with before the product's official release, so `image-manifest.gpuworks.ai` and `make image-manifest-publish` are removed as well. Downloading the image catalog by hand for offline use now goes through each channel's own `<channel>/manifest.json`, the same file as the image catalog in that channel's index, switched together with the index; the old root file followed only whichever channel was published last, so a release user could download the nightly catalog.

## Supplement (2026-11-16): Indexes Are Scoped to an Edition

A separately built server (another edition, with the same release IDs) that read the community channel would upgrade itself to the community edition, and a community server reading its channel would do the reverse. The edition therefore becomes part of a channel's identity, on a par with the channel name:

- **The index names its edition**: the signed index gains an optional `edition` field inside the signed payload, so a source cannot change it. An absent edition is community, so every index published so far stays valid for community servers, and community indexes keep omitting the field. A client accepts only indexes of its own edition (`ParseIndexFor`); a mismatch counts as an invalid index, like a bad signature, and the next source is used. `gpuworks-dist assemble -edition` writes it, and `verify -edition` reads the tree back as that edition's clients will.
- **One distribution profile per edition**: `distribution.Profile{Edition, Sources, Keys}` keeps an edition, the sources it is published to and the signing keys it trusts together; the zero value is community (the three built-in sources and the built-in keys). The server's update checks, notices, usage report, catalog updates and console upgrades share the one channel client built from it, and the `gpuworks upgrade` command reads it too. A mirror named with `--source` keeps the edition and keys but is treated as a source the customer chose, without the report. An edition's own sources are treated like the built-in ones: update checks report to them as usual.
- **Ask again before replacing the binary**: besides the channel there are `--archive`, `--base-url` and console uploads, which have no signed index. Before stopping the service, an upgrade runs the `edition` subcommand of both the installed and the new binary (an older release that does not know it counts as community) and refuses a difference, changing nothing. On the command line `--allow-edition-change` allows the switch; the console has no such switch and always refuses with `SERVER_EDITION_MISMATCH`.
- **Release packages**: `SERVER_PKG` and `PACKAGE_PREFIX` of `make release-packages` (defaults `./cmd/gpuworks` and `gpuworks`) let another edition build from its own main package and name its packages `<prefix>-<release>-<os>-<arch>.tar.gz`; the binary inside is still named `gpuworks`, so the install layout and the unpacking rule of upgrades stay the same.

Tradeoff: the same release ID names different binaries in different editions, and the version number itself does not tell editions apart; only the index field and the `edition` subcommand do. The output of `gpuworks version` stays unchanged, because install and upgrade parse the release ID from it. When a switch is allowed, the new edition's own install steps belong to the new binary, which the binary running the upgrade cannot perform, so the upgrade ends with a reminder to run the new binary's `install` once more.

<!-- translation-source-sha256: 5605f8c28ea5595b3d98cedfda1e49c76b36bc3b692a93067f1d715de9839a8f # ADR 0040: Server Upgrades and Official Catalogs Use a Signed-Index, Multi-Source Distribution Channel; the Last Official Catalog Import Takes Effect -->
<!-- translation-source-sha256: 4c311a14c6fc75216b276b75d161202938f8b6d5fc2244f1d8d200f272d6c489 ## Context -->
<!-- translation-source-sha256: e5fdb1e3f43031c942bc4789077ba7d425d1b39c467d2b8f6177e3fa01282ae3 ## Decision -->
<!-- translation-source-sha256: dd2e3386a69bb799385fbeb2366f7d37ae20266755ee6c09209865916b099ba4 ## Tradeoffs -->
<!-- translation-source-sha256: 109c4c50595cb2c42fb23a77e7d94859ae7e7a17abcec73ae125ff5e7216f80f ## Consequences -->
<!-- translation-source-sha256: 72fd7194e6f8d2f8848da6e3eacf8c46c97afc9d18141e1800fe80fc33ed2e78 ## Addendum (2026-10-17): Update Checks Report Installation Scale With the Request -->
<!-- translation-source-sha256: 651d60d8d41636ef266d4e573fb83654e84770a66170a435f922137da492ef1b ## Supplement (2026-10-28): Version Tags Order by Version Number, and the Image Catalog's Fixed Address Moves into the Channel -->
<!-- translation-source-sha256: a143ad4e56fa2d7f342e30776b510792724f665e0feaa5ab1c4ba815613156fb ## Supplement (2026-11-16): Indexes Are Scoped to an Edition -->
