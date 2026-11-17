# ADR 0054: The User Experience Improvement Program Is Enabled by Default, Daily Reports Go to a Managed Log Service, and Publisher Notices Use the Signed Distribution Channel

Date: 2026-10-22 · Status: Accepted; revises the switch trade-off in the supplement to [0040](0040-signed-multi-source-distribution.md) (2026-10-17)

## Background

The supplement to [ADR 0040](0040-signed-multi-source-distribution.md) adds installation scale (node count, GPU models, and agent versions) to index request query parameters during automatic update checks,
allowing the publisher to aggregate it from object storage access logs. This answers only “how many and how large,” not “what is it used for, what tasks ran, and where did they fail.” Query parameters and read-only static sources cannot accommodate more content.
The publisher also needs a way to deliver security advisories, important releases, and community events to administrators.

## Decisions

1. **Enabled by default, with one switch.** `usage_report` in `server.yaml` (enabled when omitted) controls daily reports and the node counts and GPU models attached to update checks.
   `auto_check_updates` controls automatic checks. Automatic checks always include the installation identifier and server and agent versions (added on 2026-10-22 so that installations that opt out can still be counted in installation and version statistics). To stop sending this information entirely, disable automatic checks as well. Private deployments set `usage_report: false` before the first startup, without a separate build or installation package.
2. **Only the server reports, once per day.** A report is sent immediately on first startup and when rejoining. Subsequent reports cover the previous complete UTC day and are sent the following day at times staggered by installation ID. Only installations with `server.yaml` send reports; `GPUWORKS_USAGE_REPORT=off` forces reporting off (CI and l2 tests). Failures are not retried; in-process counts carry over to the next day. Missed days are not backfilled.
   Facts queryable from the database by time window are queried there (timeline events use the `created_at` index). Facts that exist only when they occur (API error codes, route calls, panics, logins, and agent connection issues)
   use in-process counters, without creating reporting tables. The only new persistence is the latest report day, `usage_report_last_day`. The agent is unchanged.
3. **Content contains only enumerated values, counts, and buckets.** The field list (`docs/reference/usage-report.md`) is a commitment to users, with tests ensuring it matches `usagereport.Report` exactly.
   Model names are reported only when known to be public (built-in templates, or no access credentials configured for that model site); otherwise they are recorded as `custom`. Version numbers retain only their numeric portions.
4. **Use a managed log service for collection, without operating a custom service; test latency before sending and use the faster endpoint.** There are two collectors: an Alibaba Cloud SLS WebTracking Logstore (direct access within mainland China, a default domain requiring no ICP filing, and built-in SQL analysis and dashboards)
   and a Cloudflare Worker (writes to R2, then imports into the same Logstore). Both addresses are binary constants. Before sending, issue a GET that writes no data to each endpoint concurrently, then send to the one that responds first,
   favoring SLS (Cloudflare must be more than 50 ms faster), and try the other on failure. Latency test results are included in the report. Measurements from the office network and gpu3 show 0.05–0.35 s to SLS and 0.7–3.3 s to Cloudflare.
   Ingestion costs about CNY 0.4/GB, or a little over CNY 10 per month for around ten thousand installations.
6. **Collectors record the source public IP.** The report body contains no addresses; collectors record the request's source public IP (the server's egress address) for statistics by country and province and to identify forged reports.
   SLS retains it for 180 days, as with the report; Cloudflare records it in object metadata. The Settings page, bell notifications, and field list disclose this accurately.
5. **Notices use the signed distribution channel, not the collectors.** WebTracking returns only an empty 200 response and cannot distribute content. Notices are stored in `notices.json` at the distribution source root,
   using the same ed25519 signature envelope as the version index, and are fetched along with update checks (under the same throttling, with additional background fetches every 6 hours when `auto_check_updates` is enabled).
   The server determines applicability from local facts (channel, build time, and accelerator vendor), without sending any information to the publisher and independently of participation in the improvement program.
   The console displays notices only through the header bell, without banners; applicable security notices also produce a server log entry.

## Trade-offs

- **Enabled by default** yields representative data. The trade-off is that users must be able to preview the exact content (`GET /api/v1/usage-report/preview`) and opt out with one click (the switch in “Versions and updates,” with hover text explaining what is uploaded and what is not recorded), and documentation must explain how to disable it for private deployments.
- **A Logstore accepting anonymous writes** may receive forged data. Reports are used only to observe trends; aggregation filters by `install_id` format and `schema`, and allows one report per installation per day.
  Signed writes would require either embedding a key in the binary (which could still be extracted) or operating a custom signing service, both conflicting with the requirement to avoid operating a service.
- **Moving installation scale under the new switch** means installations with `usage_report` disabled still check for updates, attaching only the installation identifier and versions. This overturns the choice in the 0040 supplement to use “one switch for both checks and reporting”:
  reporting now covers far more than scale, and customers need to disable reports without disabling updates. The trade-off is that opting out does not stop all reporting: installations that want to send nothing must also disable automatic checks.
- **One report per day rather than real-time reporting**: overhead is negligible (one request and one grouped query), but errors become visible only the next day, and server restarts lose that day's in-process counts.
- **Recording source IPs** provides geographic distribution and a basis for detecting abuse, at the cost of collectors retaining an egress address generally considered personal information. It is stored only in the publisher's Logstore and is not correlated with data outside the report content.
  If needed later, raw data can go into a short-term Logstore, with only country and province retained after processing.
- **Notices cannot trigger actions**: even severe vulnerabilities produce only a notice and a log entry, without automatic upgrades or configuration changes. A leaked signing private key can therefore cause only incorrect notices.

## Consequences

- Add `internal/server/usagereport` (a leaf package covering report structures, trimming, counters, sending, scheduling times, and installation scale attached to update checks). `distribution` receives only ready-made query parameters and still depends on no packages in this repository.
- Add `GET /api/v1/usage-report`, `PUT /api/v1/usage-report/enabled`, `GET /api/v1/usage-report/preview`, `GET /api/v1/notices`, and `POST /api/v1/notices/read`;
  add the `notice_reads` table (migration 166).
- The publisher signs and uploads `build/notices/notices.json` using `scripts/publish_notices.sh`; distribution source cache rules must disable caching for `/notices.json`, just as for `*/index.json`.

<!-- translation-source-sha256: e9c75c3e7e1ea8c2f1333ca0ed44af787a9abf8b424e762754d44be39311b7d5 -->
