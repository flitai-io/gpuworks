import { Notice } from "../ui-kit/Notice";
import { Typography } from "antd";
import { Progress } from "../pages/DeploymentControls";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import type { MetricSeries } from "../api";
import { useInstanceMetricSeries } from "../instanceMetricSeries";
import { useMetricWindow } from "./MetricWindow";
import { ratioSeries } from "./InferenceMetricsPanel";
import { colors } from "../theme/tokens";
import { formatBytes } from "../format";

const METRICS = {
  memoryHits: "gpuworks_infer_kv_memory_hits_total",
  memoryQueries: "gpuworks_infer_kv_memory_queries_total",
  diskHits: "gpuworks_infer_kv_disk_hits_total",
  diskQueries: "gpuworks_infer_kv_disk_queries_total",
  diskWriteFailures: "gpuworks_infer_kv_disk_write_failures_total",
  diskUsed: "gpuworks_kv_disk_used_bytes",
  diskLimit: "gpuworks_kv_disk_limit_bytes",
} as const;

// KVTierPanel says where this instance's KV cache actually lives. The three
// tiers have three different sources of truth: the GPU tier is the VRAM bar
// above, the memory tier is engine-reported only, and the disk tier's size is
// measured by the agent because the engine never reports it. A tier the
// deployment configured but the engine does not report is shown as configured
// and labelled, never as a zero.
export default function KVTierPanel({ params, instanceUuid }: { params?: Record<string, unknown>; instanceUuid: string }) {
  const { t } = useTranslation("deployments");
  const memoryGiB = Number(params?.kv_offloading_size ?? 0);
  const diskEnabled = Boolean(params?.kv_disk_offload);
  const diskDir = String(params?.shared_kv_dir ?? "");

  const [selection] = useMetricWindow();
  const memoryHits = useInstanceMetricSeries(METRICS.memoryHits, instanceUuid, selection);
  const memoryQueries = useInstanceMetricSeries(METRICS.memoryQueries, instanceUuid, selection);
  const diskHits = useInstanceMetricSeries(METRICS.diskHits, instanceUuid, selection);
  const diskQueries = useInstanceMetricSeries(METRICS.diskQueries, instanceUuid, selection);
  const diskWriteFailures = useInstanceMetricSeries(METRICS.diskWriteFailures, instanceUuid, selection);
  const diskUsed = useInstanceMetricSeries(METRICS.diskUsed, instanceUuid, selection);
  const diskLimit = useInstanceMetricSeries(METRICS.diskLimit, instanceUuid, selection);

  const memoryHitRate = useHitRate(memoryHits.data?.items ?? [], memoryQueries.data?.items ?? []);
  const diskHitRate = useHitRate(diskHits.data?.items ?? [], diskQueries.data?.items ?? []);
  const usedBytes = latest(diskUsed.data?.items ?? []);
  const limitBytes = latest(diskLimit.data?.items ?? []);
  const failures = windowDelta(diskWriteFailures.data?.items ?? []);

  const error = [memoryHits, memoryQueries, diskHits, diskQueries, diskUsed, diskLimit, diskWriteFailures].find((query) => query.error)?.error;

  return (
    <div style={{ marginTop: 12 }}>
      <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>{t("kvTier.title")}</Typography.Text>
      {error && <Notice tone="danger" title={t("metrics.loadFailed")}>{error.message}</Notice>}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12 }}>
        <Tier title={t("kvTier.gpu")}>
          <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("kvTier.gpuDetail")}</Typography.Text>
        </Tier>

        <Tier title={t("kvTier.memory")}>
          {memoryGiB > 0 ? (
            <>
              <Typography.Text>{t("kvTier.capacityGiB", { gib: memoryGiB })}</Typography.Text>
              <HitRate rate={memoryHitRate} t={t} />
            </>
          ) : (
            <Typography.Text type="secondary">{t("kvTier.disabled")}</Typography.Text>
          )}
        </Tier>

        <Tier title={t("kvTier.disk")}>
          {diskEnabled ? (
            <>
              <Typography.Text style={{ fontSize: "var(--font-size-label)", wordBreak: "break-all" }}>{diskDir}</Typography.Text>
              {usedBytes != null && limitBytes != null && limitBytes > 0 ? (
                <>
                  <Progress
                    percent={Math.min(100, Math.round((usedBytes / limitBytes) * 100))}
                    size="small"
                    showInfo={false}
                    aria-label={t("kvTier.usage")}
                  />
                  <Typography.Text style={{ fontSize: "var(--font-size-label)" }}>
                    {t("kvTier.usage")}: {formatSize(usedBytes)} / {formatSize(limitBytes)}
                  </Typography.Text>
                </>
              ) : (
                <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("kvTier.awaitingAgent")}</Typography.Text>
              )}
              <HitRate rate={diskHitRate} t={t} />
              {failures > 0 && (
                <Typography.Text style={{ color: colors.danger, fontSize: "var(--font-size-label)" }} role="alert">
                  {t("kvTier.writeFailures", { count: failures })}
                </Typography.Text>
              )}
            </>
          ) : (
            <Typography.Text type="secondary">{t("kvTier.disabled")}</Typography.Text>
          )}
        </Tier>
      </div>
    </div>
  );
}

function Tier({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: "var(--radius-panel)", padding: 12, background: colors.white }}>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: "var(--font-size-label)", marginBottom: 4 }}>{title}</Typography.Text>
      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>{children}</div>
    </div>
  );
}

function HitRate({ rate, t }: { rate?: number; t: (key: string, options?: Record<string, unknown>) => string }) {
  if (rate == null) {
    return <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("kvTier.hitRate")}: {t("kvTier.engineSilent")}</Typography.Text>;
  }
  return <Typography.Text style={{ fontSize: "var(--font-size-label)" }}>{t("kvTier.hitRate")}: {(rate * 100).toFixed(1)}%</Typography.Text>;
}

// useHitRate reuses the window-ratio derivation the inference metrics panel
// already owns, so a hit rate means the same thing wherever it is shown.
function useHitRate(hits: MetricSeries[], queries: MetricSeries[]): number | undefined {
  return useMemo(() => latest(ratioSeries(hits, queries)), [hits, queries]);
}

function latest(series: MetricSeries[]): number | undefined {
  for (let i = series.length - 1; i >= 0; i--) {
    const points = series[i].points;
    if (points?.length) return points[points.length - 1][1];
  }
  return undefined;
}

// windowDelta is how much a cumulative counter advanced over the window. A
// counter that restarted with the engine reads as no advance rather than as a
// large negative one.
function windowDelta(series: MetricSeries[]): number {
  let total = 0;
  for (const item of series) {
    const points = item.points ?? [];
    for (let i = 1; i < points.length; i++) {
      const delta = points[i][1] - points[i - 1][1];
      if (delta > 0) total += delta;
    }
  }
  return total;
}

const formatSize = (value: number) => formatBytes(value, { compact: true });
