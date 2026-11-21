# ADR 0055: Store Long-Term Statistics Separately from Raw Observations: One Row per Object, Accumulation During Ingestion, and Incremental Writes

Date: 2026-10-22 · Status: Accepted

## Background

Once the platform sells tokens to external customers, it needs to answer questions such as “How many tokens did an API key use in an arbitrary time range?”, “What was a service's P95 time to first token this month?”, “Which GPUs remain idle over long periods?”, and “How many GPU-hours did each deployment consume?”
Existing observations are unsuitable: raw samples in metrics.db are retained for only 72 hours, with one row per metric. Per-request records in ux.db are retained for only 7 days or up to 2 GB, deleted as whole daily tables, and normal requests are sampled under high traffic.
Gateway token counts are broken down by deployment, not by key. Extending retention increases costs linearly with node, series, and request counts: even after downsampling GPU series for 1,000 nodes × 8 GPUs to hourly data, retaining 400 days
would produce billions of rows.

## Decisions

1. **Long-term statistics are a separate data category, with independently retained hourly and daily tiers** (31 days for hourly data and 400 days for daily data by default): token usage, service quality, GPU utilization, and GPU-hours.
   The monitoring center reads only raw observations; Usage analytics reads only long-term statistics. The two do not mix data sources.
2. **One row per object, with values in typed columns.** A token usage row represents “hour × key × user × deployment × source”; a service quality row represents “hour × deployment”; a GPU utilization row represents one GPU for one hour;
   and a GPU-hours row represents “hour × deployment × GPU model.” This avoids the generic one-row-per-metric structure in metrics.db, reducing row counts by an order of magnitude and eliminating the need to join metrics for queries. Time to first token and end-to-end duration are stored as bucket counts
   (using the same quarter-octave buckets as the request view). These are the only opaque snapshots read and written as a whole (the encoding of `rollup.Histogram`).
3. **Accumulate during ingestion, without rescanning raw data.** When the gateway finishes a request, it passes the request to the accumulator (one map lookup and a few additions per request, with locks sharded by deployment and no queue, so a full queue cannot cause missing accounting records).
   The single metrics writer goroutine also accumulates GPU samples during ingestion. GPU-hours use a once-per-minute query of instances holding GPUs, selecting only the necessary columns. Requests are assigned to hourly buckets by **completion time**, so a long request crossing midnight cannot land in a day that has already been aggregated.
4. **Write increments.** Each write contains the delta since the previous write, which the store adds to the row. Failed deltas are returned to the accumulator for the next write. A restart loses at most the unflushed interval (token usage is flushed every minute; other statistics every 5 minutes).
   A single writer prevents double counting. Daily rows are aggregated from hourly rows in the statistics time zone and can be rebuilt. A day is not recalculated once some of its hours have expired, avoiding replacement of a complete day with only its remaining hours.
5. **Store token usage in the main database and other statistics in stats.db.** Usage is the basis for reconciliation and must be backed up, migrated, and upgraded with the main database. Service quality, GPU utilization, and GPU-hours are monitoring data stored in a separate SQLite file under the data directory.
   They do not contend for locks with main-database transactions or high-frequency metrics.db writes, and the store can be discarded and rebuilt as a whole.
6. **All retention periods, capacity limits, switches, and the statistics time zone belong to one platform setting** (`data_retention`), edited on the Settings page and applied at runtime. The configuration file's `metrics_retention` is used only as the initial value on first startup.
7. **GPU utilization also has aggregate rows by node and GPU model** (`gpu_node_*` has one row per “hour × node × GPU model”; `gpu_model_*` has one row per “hour × GPU model”).
   They are written in the same transaction as per-GPU rows, with the same daily aggregation and retention. Pages showing the entire cluster or a broad selection read these aggregates. Per-GPU rows are read only when the filter selects a small number of GPUs (no more than 256).
   Queries filtered by node locate each “time bucket × node” through the primary key rather than scanning the entire time range.
8. **Backfill history once from ux.db during upgrades** (token usage and service quality), processing days in reverse chronological order. Writes and progress are committed in the same transaction, so an interrupted backfill can resume. GPU utilization and GPU-hours are not backfilled: raw samples cover only 72 hours,
   and scanning every sample at cluster scale costs more than the backfill is worth.

## Trade-offs

- **Accumulation during ingestion versus scheduled rescans of raw data**: rescanning is simplest, but requires reading every GPU sample from 1,000 nodes each hour (about 30 million rows). Accumulation during ingestion spreads the cost across a few additions per sample.
  The accumulator must handle concurrency and restarts, which is why writes use increments rather than replacing whole rows.
- **The main database versus storing everything in stats.db**: storing usage in the main database adds a small transaction every minute (a few thousand row upserts at typical scale), ensuring that reconciliation data is backed up with the platform. Other statistics do not need that guarantee.
- **GPU aggregate rows versus aggregating per-GPU rows at query time**: aggregate rows can be derived entirely from per-GPU rows, so the principle of not storing derivable data would normally rule them out. However, with 1,000 nodes × 8 GPUs,
  one week of hourly data contains 1.28 million per-GPU rows. Grouping by GPU model or node in SQLite takes 2–3 seconds (about 30 seconds for 400 days of daily data), making the page unusable. Aggregate rows reduce the same query to one-eighth of the rows when grouped by node
  and about one two-thousandth when grouped by GPU model. The cost is about 150 MiB more in stats.db (at the same scale, per-GPU rows occupy about 1.3 GiB) and about 1,000 additional node rows per GPU statistics write.
- **Assigning requests by completion time versus received time**: the request view filters by received time, so the two differ slightly for long requests that cross hourly boundaries. Using completion time avoids backfilling daily aggregates.

## Consequences

- At the target scale (1,000 nodes, 500 deployments, and 1,000 active keys, each using 3 deployments), long-term statistics occupy about 1.3 GB (including about 0.5 GB of usage in the main database). Extreme combinations,
  such as every key using every deployment each hour, increase usage write volume. The status endpoint reports the row count and duration of each write, visible on the Settings page.
- Each gateway request adds about 200 ns with no memory allocation; each GPU sample adds one map lookup during metrics ingestion.
- Documentation: [View Usage Analytics](../how-to/analyze-operations.md), [Data Retention Settings](../reference/api.md#analytics用量分析), and [Configuration](../reference/configuration.md).

<!-- translation-source-sha256: 0d620e6f98c1905c034e988cff8ccae84a950594a4d83d17a5035bb4bc96b9e4 # ADR 0055: Store Long-Term Statistics Separately from Raw Observations: One Row per Object, Accumulation During Ingestion, and Incremental Writes -->
<!-- translation-source-sha256: 1e24b1932ae6a8073b9207970275ab4aecf3e4f13bd6ac1620c58ff68660a987 ## Background -->
<!-- translation-source-sha256: ab901e7f80ffeccfca96be77881002c03b9f23345034df6f66f13de0cf5c73b3 ## Decisions -->
<!-- translation-source-sha256: d0366338bf6ee637d7c8918a2c0540c3c6dee05c53b24f2dc3fff23859939cf6 ## Trade-offs -->
<!-- translation-source-sha256: b693add89c3bb489fa94636ac4b243f3028be9714124ba850af87b270f4c2743 ## Consequences -->
