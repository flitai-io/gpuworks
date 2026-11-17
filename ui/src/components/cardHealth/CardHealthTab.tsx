import { Alert, List, Skeleton, Space, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { CardRef } from "../../api";
import { agoText } from "../../format";
import CheckCell from "./CheckCell";
import CheckDetail from "./CheckDetail";
import HealthRecords from "./HealthRecords";
import { HealthBanner } from "./NodeHealthTab";
import { checkName, codeText, periodText, statusName } from "./text";
import { useCardHealthChecks, useCardHealthRecords } from "./useCardHealth";

// CardHealthTab is one card's health checks: each check on a line of its own
// on top, the card's failure records below. Choosing a check shows what it
// last did and narrows the records to it.
export default function CardHealthTab({ nodeUUID, card, cardLabel }: { nodeUUID: string; card: CardRef; cardLabel: string }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const checks = useCardHealthChecks(nodeUUID, card, tCommon);
  const [selected, setSelected] = useState<string>();
  const [page, setPage] = useState(1);
  const records = useCardHealthRecords(nodeUUID, card, selected, page, tCommon);
  const data = checks.data;
  const row = data?.cards[0];
  const periods = Object.fromEntries((data?.checks ?? []).map((c) => [c.id, c.period_seconds]));
  const select = (check?: string) => {
    setSelected(check);
    setPage(1);
  };
  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      {checks.error && <Alert type="error" showIcon message={checks.error} />}
      {!data && !checks.error && <Skeleton active />}
      {data && <HealthBanner data={data} />}
      {row && (
        <List
          size="small"
          dataSource={row.cells}
          rowKey={(cell) => cell.check}
          renderItem={(cell) => {
            const reason = codeText(t, cell.status, cell.code);
            const ago = cell.checked_at ? agoText(t, (Date.now() - new Date(cell.checked_at).getTime()) / 1000) : "";
            return (
              <List.Item style={{ cursor: cell.status === "na" ? "default" : "pointer" }} onClick={() => cell.status !== "na" && select(selected === cell.check ? undefined : cell.check)}>
                <Space size={12} wrap>
                  <CheckCell cell={cell} selected={selected === cell.check} />
                  <Typography.Text strong>{checkName(t, cell.check)}</Typography.Text>
                  <Typography.Text>{statusName(t, cell.stale ? "stale" : cell.status)}</Typography.Text>
                  {reason && <Typography.Text type="secondary">{reason}</Typography.Text>}
                  {ago && <Typography.Text type="secondary">{t("cardHealth.lastChecked", { ago })}</Typography.Text>}
                  {!!cell.duration_ms && <Typography.Text type="secondary">{`${cell.duration_ms} ms`}</Typography.Text>}
                  {periods[cell.check] && <Typography.Text type="secondary">{periodText(t, periods[cell.check])}</Typography.Text>}
                </Space>
              </List.Item>
            );
          }}
        />
      )}
      {row && selected && (
        <CheckDetail
          key={selected}
          nodeUUID={nodeUUID}
          card={card}
          cardLabel={cardLabel}
          check={selected}
          canRecheck={row.can_recheck}
          onRechecked={() => {
            checks.reload();
            records.reload();
          }}
        />
      )}
      {records.error && <Alert type="error" showIcon message={records.error} />}
      <HealthRecords
        data={records.data}
        page={page}
        onPage={setPage}
        showCard={false}
        filter={selected ? checkName(t, selected) : undefined}
        onClearFilter={() => select(undefined)}
      />
    </Space>
  );
}
