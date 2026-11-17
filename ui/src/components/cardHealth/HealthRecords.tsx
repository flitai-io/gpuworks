import { Button, Collapse, Empty, List, Pagination, Space, Tag, Typography, theme } from "antd";
import { useTranslation } from "react-i18next";
import type { CardHealthRecord, CardHealthRecords } from "../../api";
import { cardName, checkName, codeText, durationText, localTime, statusName } from "./text";

// HealthRecords lists the stretches in which a check warned or failed: ongoing
// ones first (the server orders them), each with how long it lasted, how many
// runs found it, and what the first and latest of them read.
export default function HealthRecords({
  data,
  page,
  onPage,
  filter,
  onClearFilter,
  showCard = true,
}: {
  data?: CardHealthRecords;
  page: number;
  onPage: (page: number) => void;
  filter?: string;
  onClearFilter?: () => void;
  showCard?: boolean;
}) {
  const { t } = useTranslation("nodes");
  const days = data?.retention_days ?? 30;
  return (
    <div className="card-health-records">
      <Space style={{ width: "100%", justifyContent: "space-between", marginBottom: 8 }} wrap>
        <Typography.Text strong>{t("cardHealth.records.title", { days })}</Typography.Text>
        {filter && (
          <Space size={8}>
            <Typography.Text type="secondary">{filter}</Typography.Text>
            <Button size="small" onClick={onClearFilter}>
              {t("cardHealth.records.clear")}
            </Button>
          </Space>
        )}
      </Space>
      {data && data.records.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("cardHealth.records.empty", { days })} />
      ) : (
        <List
          loading={!data}
          dataSource={data?.records ?? []}
          rowKey={(r) => r.id}
          renderItem={(r) => <Record record={r} days={days} showCard={showCard} />}
        />
      )}
      {data && data.total > data.page_size && (
        <Pagination size="small" current={page} pageSize={data.page_size} total={data.total} onChange={onPage} showSizeChanger={false} style={{ marginTop: 8 }} />
      )}
    </div>
  );
}

function Record({ record: r, days, showCard }: { record: CardHealthRecord; days: number; showCard: boolean }) {
  const { t } = useTranslation("nodes");
  const { token } = theme.useToken();
  const span = days * 86400;
  const end = r.ended_at ? new Date(r.ended_at).getTime() : Date.now();
  const startOffset = Math.max(0, Math.min(1, 1 - (Date.now() - new Date(r.started_at).getTime()) / 1000 / span));
  const width = Math.max(0.005, Math.min(1 - startOffset, (end - new Date(r.started_at).getTime()) / 1000 / span));
  const evidence = [
    { key: "first", label: t("cardHealth.records.first"), fields: r.first_evidence },
    ...(r.latest_evidence.length && JSON.stringify(r.latest_evidence) !== JSON.stringify(r.first_evidence)
      ? [{ key: "latest", label: t("cardHealth.records.latest"), fields: r.latest_evidence }]
      : []),
  ].filter((e) => e.fields.length);
  return (
    <List.Item style={{ display: "block" }}>
      <Space size={8} wrap>
        {r.ongoing && <Tag color="red">{t("cardHealth.records.ongoing")}</Tag>}
        {r.flapping && <Tag color="purple">{t("cardHealth.records.flapping")}</Tag>}
        {showCard && (
          <Typography.Text strong>
            {cardName(t, r.card_index, r.card_ident)}
            {r.removed && <Tag style={{ marginLeft: 6 }}>{t("cardHealth.removed")}</Tag>}
          </Typography.Text>
        )}
        <Typography.Text>{checkName(t, r.check)}</Typography.Text>
        <Tag color={r.status === "fail" ? "red" : "orange"}>{codeText(t, r.status, r.code) || statusName(t, r.status)}</Tag>
        <Typography.Text type="secondary">
          {localTime(r.started_at)}
          {r.ended_at ? ` – ${localTime(r.ended_at)}` : ""}
        </Typography.Text>
        <Typography.Text type="secondary">
          {t(r.ongoing ? "cardHealth.records.lasting" : "cardHealth.records.lasted", { duration: durationText(t, r.duration_seconds) })}
        </Typography.Text>
        <Typography.Text type="secondary">{t("cardHealth.records.count", { count: r.count })}</Typography.Text>
      </Space>
      <div aria-hidden style={{ position: "relative", height: 4, marginTop: 6, borderRadius: 2, background: token.colorFillTertiary }}>
        <div
          style={{
            position: "absolute",
            left: `${startOffset * 100}%`,
            width: `${width * 100}%`,
            height: 4,
            borderRadius: 2,
            background: r.status === "fail" ? token.colorError : token.colorWarning,
          }}
        />
      </div>
      {evidence.length > 0 && (
        <Collapse
          ghost
          size="small"
          items={evidence.map((e) => ({
            key: e.key,
            label: e.label,
            children: e.fields.map((f) => (
              <div key={f.name}>
                <Typography.Text type="secondary">{f.name}</Typography.Text> <Typography.Text>{f.value}</Typography.Text>
              </div>
            )),
          }))}
        />
      )}
    </List.Item>
  );
}
