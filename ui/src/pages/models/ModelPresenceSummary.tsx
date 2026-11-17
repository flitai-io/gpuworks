import { Badge } from "../../components/ui/badge";
import { Button, Space, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { ModelPresenceSummary as PresenceCounts } from "../../api";

/**
 * A model's file locations summed into one line: how many nodes and shared
 * storages hold it, how many transfers run and how many need attention. A
 * fleet of a hundred nodes reads the same as one. The server sums it.
 */
export function ModelPresenceSummary({ presence: counts, onOpen }: { onOpen: () => void; presence: PresenceCounts }) {
  const { t } = useTranslation("models");
  if (Object.values(counts).every((value) => value === 0)) return <Typography.Text type="secondary">{t("presence.notDownloaded")}</Typography.Text>;
  // A model only in a shared storage reads as that storage, not "0 nodes".
  const ready = [
    counts.ready_nodes > 0 || counts.ready_storages === 0 ? t("presenceSummary.nodes", { count: counts.ready_nodes }) : "",
    counts.ready_storages > 0 ? t("presenceSummary.storages", { count: counts.ready_storages }) : "",
    counts.cached_nodes > 0 ? t("presenceSummary.cached", { count: counts.cached_nodes }) : "",
  ].filter(Boolean).join(" · ");
  return (
    <Space size={4} className="model-presence-summary">
      <Tooltip title={t("presenceSummary.open")}>
        <Button type="link" size="small" onClick={onOpen}>{ready}</Button>
      </Tooltip>
      {counts.in_flight > 0 && <Badge size="sm" status="info">{t("presenceSummary.inFlight", { count: counts.in_flight, percent: counts.in_flight_percent })}</Badge>}
      {counts.failed > 0 && <Badge size="sm" status="danger">{t("presenceSummary.failed", { count: counts.failed })}</Badge>}
    </Space>
  );
}
