import { Notice } from "../ui-kit/Notice";
import { Typography } from "antd";
import { Progress } from "../pages/DeploymentControls";
import { useTranslation } from "react-i18next";
import { api, usePolling, type MetricSeries } from "../api";
import { formatTimeSec } from "../i18n/datetime";
import { resolveRange, stepString, type TimeRangeSelection } from "../timeRange";
import { colors } from "../theme/tokens";

const defaultWindow: TimeRangeSelection = { kind: "quick", key: "15m" };
const GIB = 1024 ** 3;

const METRICS = {
  utilization: "gpuworks_gpu_utilization",
  memoryUsed: "gpuworks_gpu_memory_used_bytes",
  memoryTotal: "gpuworks_gpu_memory_total_bytes",
  temperature: "gpuworks_gpu_temperature_celsius",
  power: "gpuworks_gpu_power_watts",
} as const;

// A dGPU instance's own share of each card, as the HAMi interception counts
// it. The card-wide metrics above include every other slice on the card, so
// for such an instance they are context, not its own load.
const SLICE_METRICS = {
  sliceMemoryUsed: "gpuworks_dgpu_memory_used_bytes",
  sliceSM: "gpuworks_dgpu_sm_util",
} as const;

type MetricKey = keyof typeof METRICS | keyof typeof SLICE_METRICS;
type Snapshot = Partial<Record<MetricKey, number>> & { sampledAt?: number };

export default function InstanceGpuStrip({ nodeUuid, gpuIndexes, instanceUuid, vramLimitBytes, selection = defaultWindow, historical = false }: {
  selection?: TimeRangeSelection;
  historical?: boolean;
  nodeUuid: string;
  gpuIndexes: number[];
  // For a dGPU instance: whose slice to read, and the limit it runs under.
  instanceUuid?: string;
  vramLimitBytes?: number;
}) {
  const sliced = !!instanceUuid && (vramLimitBytes ?? 0) > 0;
  const { t } = useTranslation("deployments");
  const indexKey = [...gpuIndexes].sort((a, b) => a - b).join(",");
  const windowKey = selection.kind === "quick" ? selection.key : [selection.startMS, selection.endMS].join("-");
  const queryKey = [nodeUuid, indexKey, instanceUuid, sliced, historical, windowKey].join("/");
  const query = usePolling(async () => {
    const { startMS, endMS, stepMS } = resolveRange(selection, Date.now());
    const allowed = new Set(gpuIndexes);
    const metrics = { ...(!historical ? METRICS : {}), ...(sliced ? SLICE_METRICS : {}) };
    const responses = await Promise.allSettled(Object.entries(metrics).map(async ([name, metric]) => {
      const key = name as MetricKey;
      const slice = key in SLICE_METRICS;
      const result = await api.metricsQuery(metric, {
        node: nodeUuid, ...(slice ? { instance: instanceUuid } : {}),
        start: startMS, end: endMS, step: stepString(stepMS),
      });
      return [key, result.items.filter(item => item.labels.gpu != null && allowed.has(Number(item.labels.gpu)) &&
        (!item.labels.node || item.labels.node === nodeUuid) &&
        (!slice || !item.labels.instance || item.labels.instance === instanceUuid))] as const;
    }));
    const snapshots: Record<number, Snapshot> = {};
    let error: string | undefined;
    for (const response of responses) {
      if (response.status === "rejected") {
        error ??= String(response.reason instanceof Error ? response.reason.message : response.reason);
        continue;
      }
      const [key, samples] = response.value;
      mergeLatest(snapshots, key, samples, allowed);
    }
    return { queryKey, snapshots, error };
  }, resolveRange(selection, Date.now()).pollMs, [queryKey]);
  const result = query.data?.queryKey === queryKey ? query.data : undefined;
  const pending = !result && !query.error;
  const data = result?.snapshots ?? {};
  const utilizationKey = sliced ? "sliceSM" : "utilization";
  const hasAnyMetric = Object.values(data).some((s) =>
    s.utilization != null || s.memoryUsed != null || s.temperature != null || s.power != null || s.sliceMemoryUsed != null || s.sliceSM != null || s.memoryTotal != null,
  );

  const utilValues = gpuIndexes.map((idx) => data[idx]?.[utilizationKey]).filter((v): v is number => v != null);
  const skew = gpuIndexes.length >= 2 ? computeSkew(utilValues) : undefined;

  return (
    <div>
      {result?.error || query.error ? <Notice tone="danger" title={t("instanceGpu.loadFailed")}>{result?.error ?? query.error?.message}</Notice> :
        !pending && !hasAnyMetric && <Notice title={t(historical || selection.kind === "custom" ? "metrics.emptyWindow" : "instanceGpu.waiting")} />}
      {(pending || hasAnyMetric) && <div aria-busy={pending} style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 10 }}>
        {gpuIndexes.map((index) => (
          <GpuTile key={index} index={index} snapshot={data[index] ?? {}} historical={historical} sliceLimit={sliced ? vramLimitBytes : undefined} t={t} />
        ))}
      </div>}
      {hasAnyMetric && gpuIndexes.length >= 2 && (
        <Typography.Text type="secondary" style={{ display: "block", marginTop: 8, fontSize: "var(--font-size-label)" }}>
          {t("instanceGpu.groupSkew", { ratio: skew != null ? `${skew.toFixed(2)}x` : "—" })}
        </Typography.Text>
      )}
    </div>
  );
}

// computeSkew is the deterministic max/min ratio across GPUs currently
// reporting utilization. A zero minimum has no meaningful ratio (division by
// zero), so that case reports "not computable" rather than Infinity.
function computeSkew(values: number[]): number | undefined {
  if (values.length < 2) return undefined;
  const max = Math.max(...values);
  const min = Math.min(...values);
  if (min <= 0) return undefined;
  return max / min;
}

function mergeLatest(target: Record<number, Snapshot>, key: MetricKey, series: MetricSeries[], allowed: Set<number>) {
  for (const item of series) {
    const gpu = Number(item.labels?.gpu);
    if (!Number.isFinite(gpu) || !allowed.has(gpu)) continue;
    const point = item.points?.[item.points.length - 1];
    if (!point) continue;
    target[gpu] = { ...(target[gpu] ?? {}), [key]: point[1], sampledAt: Math.max(target[gpu]?.sampledAt ?? 0, point[0]) };
  }
}

function GpuTile({ index, snapshot, sliceLimit, historical, t }: { historical: boolean; index: number; snapshot: Snapshot; sliceLimit?: number; t: (key: string, options?: Record<string, unknown>) => string }) {
  const utilPct = snapshot.utilization != null ? Math.max(0, Math.min(100, snapshot.utilization)) : undefined;
  const memPct =
    snapshot.memoryUsed != null && snapshot.memoryTotal
      ? Math.min(100, (snapshot.memoryUsed / snapshot.memoryTotal) * 100)
      : undefined;
  const timestamp = snapshot.sampledAt != null && <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("instanceGpu.lastSample", { time: formatTimeSec(snapshot.sampledAt) })}</Typography.Text>;
  if (sliceLimit) {
    const slicePct = snapshot.sliceMemoryUsed != null ? Math.min(100, (snapshot.sliceMemoryUsed / sliceLimit) * 100) : undefined;
    const smPct = snapshot.sliceSM != null ? Math.max(0, Math.min(100, snapshot.sliceSM)) : undefined;
    return (
      <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: "var(--radius-panel)", padding: "10px 12px", background: colors.white }}>
        <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>{t("instanceGpu.sliceTitle", { index, limit: (sliceLimit / GIB).toFixed(1) })}</Typography.Text>
        <TileBar
          label={t("instanceGpu.sliceVram")}
          percent={slicePct}
          value={snapshot.sliceMemoryUsed == null ? "—" : `${(snapshot.sliceMemoryUsed / GIB).toFixed(1)} / ${(sliceLimit / GIB).toFixed(1)} GiB`}
        />
        <TileBar label={t("instanceGpu.sliceSM")} percent={smPct} value={smPct == null ? "—" : `${smPct.toFixed(0)}%`} />
        {!historical && <Typography.Text type="secondary" style={{ display: "block", marginTop: 6, fontSize: "var(--font-size-label)" }}>
          {t("instanceGpu.wholeCard", {
            util: utilPct == null ? "—" : `${utilPct.toFixed(0)}%`,
            vram: snapshot.memoryUsed == null ? "—" : `${(snapshot.memoryUsed / GIB).toFixed(1)} / ${snapshot.memoryTotal ? (snapshot.memoryTotal / GIB).toFixed(0) : "—"} GiB`,
          })}
        </Typography.Text>}
        {timestamp}
      </div>
    );
  }
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: "var(--radius-panel)", padding: "10px 12px", background: colors.white }}>
      <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>GPU{index}</Typography.Text>
      <TileBar label={t("instanceGpu.utilization")} percent={utilPct} value={utilPct == null ? "—" : `${utilPct.toFixed(0)}%`} />
      <TileBar
        label={t("instanceGpu.vram")}
        percent={memPct}
        // A used figure with no known total has no percentage to show - a bar
        // pinned at 0% would read as "empty" rather than "unknown", so it is
        // hidden instead of drawn wrong.
        hideBar={snapshot.memoryTotal == null && snapshot.sampledAt != null}
        value={
          snapshot.memoryUsed == null
            ? "—"
            : `${(snapshot.memoryUsed / GIB).toFixed(1)} / ${snapshot.memoryTotal ? (snapshot.memoryTotal / GIB).toFixed(0) : "—"} GiB`
        }
      />
      <div style={{ display: "flex", justifyContent: "space-between", marginTop: 6 }}>
        <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>
          {t("instanceGpu.temperature")} {snapshot.temperature == null ? "—" : `${snapshot.temperature.toFixed(0)}C`}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>
          {t("instanceGpu.power")} {snapshot.power == null ? "—" : `${snapshot.power.toFixed(0)}W`}
        </Typography.Text>
      </div>
      {timestamp}
    </div>
  );
}

function TileBar({ label, percent, value, hideBar }: { label: string; percent?: number; value: string; hideBar?: boolean }) {
  return (
    <div style={{ marginBottom: 6 }}>
      <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 2 }}>
        <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{label}</Typography.Text>
        <Typography.Text style={{ fontSize: "var(--font-size-label)" }}>{value}</Typography.Text>
      </div>
      {!hideBar && (
        <Progress label={label} style={{ visibility: percent == null ? "hidden" : undefined }} percent={percent ?? 0} size="small" showInfo={false} strokeColor={colors.chartPrimary} trailColor={colors.borderSoft} />
      )}
    </div>
  );
}
