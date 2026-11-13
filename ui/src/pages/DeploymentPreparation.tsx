import { Button, Progress, Space, Tooltip, Typography, message } from "antd";
import { storageDetailPath } from "./storage/storageLinks";
import { useState } from "react";
import { apiErrorText } from "../apiError";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type ModelNodePresence } from "../api";
import { formatBytes } from "../format";
import { isModelFileReady } from "../storageTypes";
import { rateLabel, transferOrigin } from "./models/modelPresenceDisplay";

// All classification, timing and what may be retried comes from the server's
// preparation view; the server also zeroes a rate that is not meaningful.
// Unknown stages and old agents deliberately have no synthetic percentage.
export function PreparationLabel({ item }: { item: ModelNodePresence }) {
  const { t } = useTranslation("deployments");
  const view = item.preparation;
  const key = view?.reason ? `preparation.reason.${view.reason}` : `preparation.phase.${view?.phase ?? "waiting"}`;
  return <>{t(key, { defaultValue: t("preparation.phase.waiting") })}</>;
}

// A failed copy is an operator decision: nothing retries it on its own, and
// the section says so rather than leaving the deployment looking stuck. The
// wording is shared by the drawer and the list row.
export function RetryHint() {
  const { t } = useTranslation("deployments");
  return <Typography.Text type="secondary">{t("preparation.noAutoRetry")}</Typography.Text>;
}

export function DeploymentPreparation({ rows, onChanged }: { rows?: ModelNodePresence[]; onChanged?: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState<string | null>(null);
  const retry = async (item: ModelNodePresence) => {
    if (!item.model_id || !item.storage_id) return;
    setBusy(`${item.node_uuid}:${item.model_id}:${item.storage_id}`);
    try { await api.retryModelCache(item.model_id, item.node_uuid, item.storage_id); onChanged?.(); }
    catch (error) { message.error(apiErrorText(tCommon, error)); }
    finally { setBusy(null); }
  };
  const pending = rows?.filter((item) => !isModelFileReady(item.state)) ?? [];
  if (pending.length === 0) return null;
  return <section style={{ marginTop: 16 }} aria-label={t("detail.modelPreparation")}>
    <Typography.Title level={5}>{t("detail.modelPreparation")}</Typography.Title>
    {pending.map((item) => {
      const view = item.preparation;
      const measurable = Boolean(view?.total_known) && item.size_bytes > 0;
      const origin = transferOrigin(item);
      // A failure is the operator's to act on; its message is the agent's own
      // words about why, so it is never hidden behind a source line.
      const failed = view?.reason === "MODEL_PREPARATION_FAILED";
      const diskFull = view?.reason === "MODEL_CACHE_DISK_FULL";
      return <div key={`${item.node_uuid}:${item.model_id}:${item.storage_id}`} style={{ marginTop: 12 }}>
        <Space wrap>
          <Typography.Text strong>{item.node_name || item.node_uuid.slice(0, 12)}</Typography.Text>
          {item.model_name && <Typography.Text>{item.model_name}</Typography.Text>}
          <Typography.Text type={view?.reason && view.reason !== "MODEL_PREPARATION_WAITING" ? "warning" : "secondary"}><PreparationLabel item={item} /></Typography.Text>
          {item.storage_id && <Link to={storageDetailPath(item.storage_id)}>{item.storage_name || t("preparation.storage")}</Link>}
        </Space>
        {measurable && <Progress aria-label={t("preparation.progress")} percent={Math.min(100, Math.round(item.progress * 100))} size="small" />}
        <div><Typography.Text type="secondary">
          {origin ? `${"node" in origin ? t("detail.modelFromNode", { node: origin.node }) : t("detail.modelFromHost", { host: origin.host })} · ` : ""}
          {measurable ? `${formatBytes(item.downloaded)} / ${formatBytes(item.size_bytes)}` : `${formatBytes(item.downloaded)} · ${t("preparation.unknownTotal")}`}
          {item.rate_bps > 0 ? ` · ${rateLabel(item.rate_bps)}` : ""}
          {view?.eta_seconds ? ` · ${t("preparation.eta", { seconds: view.eta_seconds })}` : ""}
          {view?.reason === "MODEL_COPY_STALLED" ? ` · ${t("preparation.idle", { seconds: view.idle_seconds })}` : ""}
        </Typography.Text></div>
        {view?.retryable && <Space wrap>
          <Button aria-label={t("preparation.retry")} size="small" loading={busy === `${item.node_uuid}:${item.model_id}:${item.storage_id}`} disabled={busy !== null} onClick={() => void retry(item)}>{t("preparation.retry")}</Button>
          <RetryHint />
        </Space>}
        {failed && !view?.retryable && <Typography.Text type="secondary">{t("preparation.retryUnavailable")}</Typography.Text>}
        {item.message && (!origin || failed || diskFull) && <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{item.message}</Typography.Paragraph>}
        {view?.reason === "MODEL_COPY_STALLED" && <Typography.Paragraph>{t("preparation.waitForIO")}</Typography.Paragraph>}
      </div>;
    })}
  </section>;
}

// ListPreparationAction is the same retry, where the operator reads the failed
// state: the deployment list row. It is offered only on the server's own
// retryable fact and with the row's own storage, and a failure the server does
// not offer to retry says why instead of staying silent. The row is narrow, so
// the wording lives in the tooltip and the drawer says it in full.
export function ListPreparationAction({ item, onChanged }: { item: ModelNodePresence; onChanged?: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const view = item.preparation;
  if (view?.retryable && item.model_id && item.storage_id) {
    const retry = async () => {
      setBusy(true);
      try { await api.retryModelCache(item.model_id!, item.node_uuid, item.storage_id!); onChanged?.(); }
      catch (error) { message.error(apiErrorText(tCommon, error)); }
      finally { setBusy(false); }
    };
    return <Tooltip title={t("preparation.noAutoRetry")}>
      <Button size="small" type="link" loading={busy} aria-label={t("preparation.retry")} onClick={() => void retry()}>{t("preparation.retry")}</Button>
    </Tooltip>;
  }
  if (view?.reason !== "MODEL_PREPARATION_FAILED") return null;
  return <Tooltip title={t("preparation.retryUnavailable")}><Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("preparation.retryUnavailableShort")}</Typography.Text></Tooltip>;
}
