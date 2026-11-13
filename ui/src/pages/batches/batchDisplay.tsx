import { Badge, type BadgeStatus } from "../../components/ui/badge";
import { useTranslation } from "react-i18next";
import type { Batch, BatchStatus } from "../../batchApi";

const STATUS_COLORS: Record<BatchStatus, BadgeStatus> = {
  validating: "neutral",
  in_progress: "info",
  finalizing: "info",
  cancelling: "warning",
  completed: "success",
  failed: "danger",
  expired: "warning",
  cancelled: "neutral",
};

/** BatchStatusTag is a batch's status, coloured by how it ended or that it is still moving. */
export function BatchStatusTag({ status }: { status: string }) {
  const { t } = useTranslation("batches");
  const label = t(`status.${status}`, { defaultValue: status });
  return <Badge status={STATUS_COLORS[status as BatchStatus] ?? "neutral"}>{label}</Badge>;
}

/** The share of a batch's requests that reached a final result, and how many of those failed. */
export function batchProgress(batch: Pick<Batch, "request_counts">) {
  const { total, completed, failed } = batch.request_counts;
  const done = completed + failed;
  const percent = total > 0 ? Math.min(100, (done / total) * 100) : 0;
  const succeeded = total > 0 ? Math.min(100, (completed / total) * 100) : 0;
  return { total, completed, failed, done, percent, succeeded };
}

/**
 * BatchProgress is the done / total text with a bar. Requests the platform is
 * retrying are in neither count, so they stay in the unfilled part of the
 * bar; the failed share is drawn in the warning colour.
 */
export function BatchProgress({ batch }: { batch: Batch }) {
  const { t } = useTranslation("batches");
  const p = batchProgress(batch);
  return (
    <div className="batch-progress">
      <div role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={p.percent}
        aria-label={t("progress.aria", { done: p.done, total: p.total, failed: p.failed })}
        className="flex h-2 overflow-hidden rounded-full bg-emphasis">
        <div className="h-full bg-success-strong" style={{ width: `${p.succeeded}%` }} />
        <div className="h-full bg-warning" style={{ width: `${Math.max(0, p.percent - p.succeeded)}%` }} />
      </div>
      <div>
        <span>{t("progress.summary", { done: p.done, total: p.total })}</span>
        {p.failed > 0 && <span className="batch-progress-failed text-fg-warning">{t("progress.failed", { count: p.failed })}</span>}
      </div>
    </div>
  );
}
