import { Table, Tag, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import type { GpuGroupBy } from "../../analyticsApi";
import { formatBytes } from "../../format";
import { colors } from "../../theme/tokens";
import { DASH, fmtCelsius, fmtPct, fmtWatts } from "./format";
import { NameCell } from "./names";
import { isIdle, type GpuRow } from "./gpuModel";

const cmp = (a: number | null, b: number | null) => (a ?? -1) - (b ?? -1);

// GpuTable lists the cards (or their groups) with the range's figures; rows
// whose utilization stayed under the idle line are tinted and tagged.
export default function GpuTable({ rows, group }: { rows: GpuRow[]; group: GpuGroupBy }) {
  const { t } = useTranslation("analytics");
  const mem = (bytes: number | null, percent: number | null) =>
    bytes == null ? DASH : (
      <div>
        {formatBytes(bytes)}
        <div><Typography.Text type="secondary" style={{ fontSize: 12 }}>{fmtPct(percent)}</Typography.Text></div>
      </div>
    );
  const columns: ColumnsType<GpuRow> = [
    {
      key: "unit",
      title: t(`gpus.table.unit.${group}`),
      width: 240,
      sorter: (a, b) => a.title.localeCompare(b.title),
      render: (_, r) => (
        <NameCell
          title={r.title}
          sub={r.sub}
          tags={<>
            {isIdle(r.utilAvg) && <Tag style={{ marginLeft: 8 }}>{t("gpus.table.idle")}</Tag>}
            {r.nodeDeleted && <Tag color="warning" style={{ marginLeft: 8 }}>{t("gpus.table.nodeDeleted")}</Tag>}
            {r.removed > 0 && !r.nodeDeleted && <Tag style={{ marginLeft: 8 }}>{group === "gpu" ? t("gpus.table.removed") : t("gpus.table.removedCount", { count: r.removed })}</Tag>}
          </>}
        />
      ),
    },
    { key: "util", title: t("gpus.table.utilAvg"), align: "right", defaultSortOrder: "descend", sorter: (a, b) => cmp(a.utilAvg, b.utilAvg), render: (_, r) => fmtPct(r.utilAvg) },
    { key: "utilMax", title: t("gpus.table.utilMax"), align: "right", sorter: (a, b) => cmp(a.utilMax, b.utilMax), render: (_, r) => fmtPct(r.utilMax) },
    { key: "mem", title: t("gpus.table.memAvg"), align: "right", sorter: (a, b) => cmp(a.memAvg, b.memAvg), render: (_, r) => mem(r.memAvg, r.memAvgPercent) },
    { key: "memMax", title: t("gpus.table.memMax"), align: "right", sorter: (a, b) => cmp(a.memMax, b.memMax), render: (_, r) => mem(r.memMax, r.memMaxPercent) },
    { key: "power", title: group === "gpu" ? t("gpus.table.power") : t("gpus.table.powerPerCard"), align: "right", sorter: (a, b) => cmp(a.powerAvg, b.powerAvg), render: (_, r) => fmtWatts(r.powerAvg) },
    { key: "temp", title: t("gpus.table.tempMax"), align: "right", sorter: (a, b) => cmp(a.tempMax, b.tempMax), render: (_, r) => fmtCelsius(r.tempMax) },
    {
      key: "deployments",
      title: t("gpus.table.deployments"),
      width: 220,
      render: (_, r) => (
        <div>
          {r.deployments.map((d) => (
            <div key={d.name}>{d.name}{r.cardCount > 1 && <Typography.Text type="secondary" style={{ fontSize: 12 }}> × {d.cards}</Typography.Text>}</div>
          ))}
          {r.unassigned > 0 && <div><Typography.Text type="secondary">{t("gpus.table.unassigned")}{r.cardCount > 1 ? ` × ${r.unassigned}` : ""}</Typography.Text></div>}
        </div>
      ),
    },
  ];
  return (
    <Table<GpuRow>
      rowKey="id"
      size="middle"
      columns={columns}
      dataSource={rows}
      pagination={rows.length > 50 ? { pageSize: 50, showSizeChanger: false } : false}
      scroll={{ x: 1100 }}
      onRow={(r) => ({ style: isIdle(r.utilAvg) ? { background: colors.canvas } : undefined })}
      locale={{ emptyText: t("state.noRows") }}
    />
  );
}
