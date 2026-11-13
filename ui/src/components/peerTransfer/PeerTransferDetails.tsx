import { useTranslation } from "react-i18next";
import { api, usePolling } from "../../api";
import { formatBytes } from "../../format";
import { formatTime } from "../../i18n/datetime";
import { Button } from "../ui/button";
import { HelpTip } from "../../ui-kit/HelpTip";

// Runtime observations supplement the existing target-side progress ledger.
// Their absence never means that the transfer failed or that its bytes are zero.
export function PeerTransferDetails({ transferId, transferErrorCode }: { transferId: string; transferErrorCode?: string }) {
  const { t } = useTranslation("common");
  const { data, loading, error, refetch } = usePolling(
    () => api.peerTransfers(transferId),
    2000,
    [transferId],
  );
  const observation = data?.active.find((entry) => entry.transfer_id === transferId);
  const refresh = <Button type="button" variant="tertiary" size="xs" loading={loading} onClick={() => void refetch()}>{t("peerTransfer.refresh")}</Button>;
  const label = t("peerTransfer.title");
  const identity = <p className="text-label text-fg-muted">{t("peerTransfer.transferId")}: <code className="break-all">{transferId}</code></p>;
  if (loading && !data) {
    return <section aria-label={label} aria-busy="true">{identity}<p role="status">{t("peerTransfer.loading")}</p></section>;
  }
  if (error) {
    return <section aria-label={label}>{identity}<p role="status">{t("peerTransfer.unavailable")}</p>{refresh}</section>;
  }
  if (!data) return null;
  const reason = (value: string) => t(`peerTransfer.reasons.${value}`, { defaultValue: t("peerTransfer.unknownReason") });
  return <section aria-label={label} aria-busy={loading}>
    <div className="flex items-center justify-between gap-2"><h4 className="text-label font-medium">{label}</h4>{refresh}</div>
    {identity}
    {observation ? <dl className="detail-extra">
      <div><dt>{t("peerTransfer.transport")}</dt><dd>{observation.transport === "direct" ? t("peerTransfer.direct") : observation.transport === "relay" ? t("peerTransfer.relay") : t("peerTransfer.unknownTransport")}</dd></div>
      <div><dt>{t("peerTransfer.phase")}</dt><dd>{t(`peerTransfer.phases.${observation.phase}`, { defaultValue: t("peerTransfer.phaseUnknown") })}</dd></div>
      <div><dt>{t("peerTransfer.observedAt")}</dt><dd>{observation.updated_at ? formatTime(observation.updated_at) : "—"}</dd></div>
      {observation.reason && <div><dt>{t("peerTransfer.reason")}</dt><dd>{reason(observation.reason)}</dd></div>}
      {observation.fallback_reason && <div><dt>{t("peerTransfer.fallback")}</dt><dd>{reason(observation.fallback_reason)}</dd></div>}
      {observation.transport === "relay" && <>
        <div><dt>{t("peerTransfer.serverRate", { seconds: observation.rate_window_seconds })}</dt><dd>{observation.rate_window_seconds > 0 ? `${formatBytes(observation.server_rate_bps)}/s` : "—"}</dd></div>
        <div><dt>{t("peerTransfer.forwarded")}</dt><dd>{formatBytes(observation.source_to_target_bytes)}</dd></div>
        <div><dt>{t("peerTransfer.reverse")}</dt><dd>{formatBytes(observation.target_to_source_bytes)}</dd></div>
        <div><dt>{t("peerTransfer.limit")}</dt><dd>{observation.effective_rate_limit_bps > 0 ? `${formatBytes(observation.effective_rate_limit_bps)}/s` : "—"}</dd></div>
        <div><dt>{t("peerTransfer.rateWait")}</dt><dd>{(observation.rate_wait_ms / 1000).toFixed(1)} s</dd></div>
      </>}
    </dl> : <p role="status">{t(transferErrorCode === "PEER_RELAY_BUSY" ? "peerTransfer.phaseWaiting" : "peerTransfer.unknown")}</p>}
    <p className="flex items-center gap-1 text-label text-fg-muted">{t("peerTransfer.serverBudget")}: {formatBytes(data.policy.rate_limit_bps)}/s · {t("peerTransfer.active", { active: data.active_relay_count, max: data.policy.max_active })}<HelpTip content={t("peerTransfer.accountingHelp")} /></p>
    <p className="text-label text-fg-muted">{t("peerTransfer.history")}</p>
  </section>;
}
