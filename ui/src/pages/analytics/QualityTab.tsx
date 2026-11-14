import { useMemo, useState } from "react";
import { Card, Segmented, Space, Table, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { analyticsApi, type QualityAnalytics, type QualityFigures, type QualityRow } from "../../analyticsApi";
import BackfillNote, { leadingBackfilled } from "./backfill";
import { SERIES_COLORS, lineOption, stackedBarOption } from "./chartOptions";
import type { CsvModel } from "./csv";
import EChart from "./EChart";
import { useExportModel } from "./exportContext";
import { DASH, fmtFixed, fmtInt, fmtPct, fmtSeconds, ratio } from "./format";
import { DeploymentCell } from "./names";
import QueryState from "./QueryState";
import RateBar from "./RateBar";
import StatCards, { type StatItem } from "./StatCards";
import type { TabProps } from "./types";
import { useAnalyticsData } from "./useAnalyticsData";
import { bucketLabel, spansDays } from "./zoned";

/** successRate is the share of requests that were not an error and not cut off by the connection; a client leaving or a length cut is not a failure. */
export function successRate(f: QualityFigures): number | null {
  return ratio(f.requests - f.error - f.interrupted, f.requests);
}

function pair(a: number | null, b: number | null): string {
  if (a == null && b == null) return DASH;
  const sec = (ms: number | null) => (ms == null ? DASH : (ms / 1000).toFixed(2));
  return `${sec(a)} / ${sec(b)} s`;
}

const secOrBlank = (ms: number | null) => (ms == null ? "" : (ms / 1000).toFixed(2));

export default function QualityTab({ from, to, granularity, deployment, tz, reload }: TabProps) {
  const { t } = useTranslation("analytics");
  const [mode, setMode] = useState<"all" | "bad">("all");
  const query = useAnalyticsData(
    (signal) => analyticsApi.quality({ from, to, granularity, deployment }, signal),
    [from, to, granularity, deployment, reload],
  );
  const data = query.data;

  const exportModel = useMemo<CsvModel | null>(() => {
    if (!data) return null;
    const cells = (f: QualityFigures) => [
      f.requests, fmtPct(successRate(f)), f.completed, f.length, f.client_closed, f.interrupted, f.error,
      secOrBlank(f.ttft_p50_ms), secOrBlank(f.ttft_p95_ms), secOrBlank(f.e2e_p50_ms), secOrBlank(f.e2e_p95_ms), f.output_tokens_per_sec == null ? "" : f.output_tokens_per_sec.toFixed(1),
    ];
    return {
      name: "quality",
      head: [
        t("quality.export.deployment"), t("quality.export.model"), t("quality.table.requests"), t("quality.table.successRate"), t("quality.outcome.completed"), t("quality.outcome.length"),
        t("quality.outcome.client_closed"), t("quality.outcome.interrupted"), t("quality.outcome.error"),
        t("quality.export.ttftP50"), t("quality.export.ttftP95"), t("quality.export.e2eP50"), t("quality.export.e2eP95"), t("quality.table.speed"),
      ],
      rows: [
        ...data.rows.map((r) => [r.deployment_name || t("names.deploymentId", { id: r.deployment_id }), r.served_name, ...cells(r)]),
        [t("table.total"), "", ...cells(data.totals)],
      ],
    };
  }, [data, t]);
  useExportModel(exportModel);

  const cards = (d: QualityAnalytics): StatItem[] => {
    const c = d.totals;
    return [
      { key: "requests", title: t("quality.cards.requests"), value: fmtInt(c.requests), sub: t("quality.cards.completed", { count: c.completed }) },
      {
        key: "success", title: t("quality.cards.successRate"), value: fmtPct(successRate(c)), tip: t("quality.cards.successTip"),
        sub: t("quality.cards.failures", { count: c.error + c.interrupted }),
      },
      { key: "ttft", title: t("quality.cards.ttft"), value: pair(c.ttft_p50_ms, c.ttft_p95_ms), tip: t("quality.cards.ttftTip"), sub: t("quality.cards.percentileSub") },
      { key: "e2e", title: t("quality.cards.e2e"), value: pair(c.e2e_p50_ms, c.e2e_p95_ms), sub: t("quality.cards.e2eSub") },
      { key: "errors", title: t("quality.cards.errors"), value: fmtInt(c.error), dot: SERIES_COLORS.err, sub: t("quality.cards.errorShare", { rate: fmtPct(ratio(c.error, c.requests)) }) },
    ];
  };

  const charts = (d: QualityAnalytics) => {
    const labels = d.range.buckets.map((b) => bucketLabel(b, tz, d.range.granularity, spansDays(d.range.buckets, tz)));
    const shade = { shadeLeading: leadingBackfilled(d.range), shadeLabel: t("backfill.label") };
    const sec = (ms: number | null) => (ms == null ? null : Number((ms / 1000).toFixed(3)));
    const ttft = lineOption({
      labels,
      series: [
        { name: t("quality.chart.ttftP50"), color: SERIES_COLORS.line[0], data: d.series.map((s) => sec(s.ttft_p50_ms)) },
        { name: t("quality.chart.ttftP95"), color: SERIES_COLORS.line[1], data: d.series.map((s) => sec(s.ttft_p95_ms)) },
      ],
      valueFormat: (v) => `${v.toFixed(2)} s`,
      axisName: t("quality.chart.seconds"),
      ...shade,
    });
    const outcomes = [
      { key: "completed", color: SERIES_COLORS.ok },
      { key: "length", color: SERIES_COLORS.miss },
      { key: "client_closed", color: SERIES_COLORS.grey },
      { key: "interrupted", color: SERIES_COLORS.interrupted },
      { key: "error", color: SERIES_COLORS.err },
    ] as const;
    const results = stackedBarOption({
      labels,
      series: outcomes.filter((o) => !(mode === "bad" && o.key === "completed")).map((o) => ({ name: t(`quality.outcome.${o.key}`), color: o.color, data: d.series.map((s) => s[o.key]) })),
      valueFormat: fmtInt,
      totalLabel: t("table.total"),
      ...shade,
    });
    return { ttft, results };
  };

  const columns: ColumnsType<QualityRow> = [
    { key: "deployment", title: t("quality.table.deployment"), width: 240, sorter: (a, b) => (a.deployment_name || "").localeCompare(b.deployment_name || ""), render: (_, r) => <DeploymentCell t={t} deployment={{ id: r.deployment_id, name: r.deployment_name, served: r.served_name, deleted: r.deployment_deleted }} /> },
    { key: "requests", title: t("quality.table.requests"), align: "right", defaultSortOrder: "descend", sorter: (a, b) => a.requests - b.requests, render: (_, r) => fmtInt(r.requests) },
    { key: "success", title: t("quality.table.successRate"), align: "right", sorter: (a, b) => (successRate(a) ?? -1) - (successRate(b) ?? -1), render: (_, r) => <RateBar percent={successRate(r)} color={SERIES_COLORS.ok} /> },
    ...(["completed", "length", "client_closed", "interrupted"] as const).map((k) => ({
      key: k, title: t(`quality.outcome.${k}`), align: "right" as const, sorter: (a: QualityRow, b: QualityRow) => a[k] - b[k], render: (_: unknown, r: QualityRow) => fmtInt(r[k]),
    })),
    { key: "error", title: t("quality.outcome.error"), align: "right", sorter: (a, b) => a.error - b.error, render: (_, r) => (r.error ? <span style={{ color: SERIES_COLORS.err }}>{fmtInt(r.error)}</span> : fmtInt(r.error)) },
    ...([["ttft_p50_ms", "ttftP50"], ["ttft_p95_ms", "ttftP95"], ["e2e_p50_ms", "e2eP50"], ["e2e_p95_ms", "e2eP95"]] as const).map(([k, label]) => ({
      key: k, title: t(`quality.table.${label}`), align: "right" as const, sorter: (a: QualityRow, b: QualityRow) => (a[k] ?? -1) - (b[k] ?? -1), render: (_: unknown, r: QualityRow) => fmtSeconds(r[k]),
    })),
    { key: "speed", title: t("quality.table.speed"), align: "right", sorter: (a, b) => (a.output_tokens_per_sec ?? -1) - (b.output_tokens_per_sec ?? -1), render: (_, r) => fmtFixed(r.output_tokens_per_sec, 1) },
  ];

  return (
    <QueryState query={query} isEmpty={(d) => d.totals.requests === 0 && d.rows.length === 0}>
      {(d) => {
        const c = charts(d);
        const total = d.totals;
        return (
          <>
            <StatCards items={cards(d)} />
            <Card style={{ marginBottom: 16 }}>
              <Space wrap align="center" style={{ width: "100%", marginBottom: 8 }}>
                <Typography.Title level={5} style={{ margin: 0 }}>{t("quality.chart.resultsTitle", { granularity: t(`granularityName.${d.range.granularity}`) })}</Typography.Title>
                <Segmented
                  value={mode}
                  onChange={(v) => setMode(v as "all" | "bad")}
                  options={[{ value: "all", label: t("quality.chart.modeAll") }, { value: "bad", label: t("quality.chart.modeBad") }]}
                />
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t(mode === "bad" ? "quality.chart.metaBad" : "quality.chart.metaAll")}</Typography.Text>
              </Space>
              <EChart option={c.results} height={300} label={t("quality.chart.resultsAria")} />
            </Card>
            <Card style={{ marginBottom: 16 }}>
              <Typography.Title level={5} style={{ marginTop: 0 }}>{t("quality.chart.ttftTitle", { granularity: t(`granularityName.${d.range.granularity}`) })}</Typography.Title>
              <EChart option={c.ttft} height={300} label={t("quality.chart.ttftAria")} />
            </Card>
            <Card styles={{ body: { padding: 0 } }} title={<Space align="baseline"><span>{t("quality.table.title")}</span><Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{t("quality.table.hint")}</Typography.Text></Space>}>
              <Table<QualityRow>
                rowKey="deployment_id"
                size="middle"
                columns={columns}
                dataSource={d.rows}
                pagination={d.rows.length > 50 ? { pageSize: 50, showSizeChanger: false } : false}
                scroll={{ x: 1300 }}
                locale={{ emptyText: t("state.noRows") }}
                summary={() => (
                  <Table.Summary fixed>
                    <Table.Summary.Row>
                      <Table.Summary.Cell index={0}><strong>{t("table.total")}</strong></Table.Summary.Cell>
                      <Table.Summary.Cell index={1} align="right">{fmtInt(total.requests)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={2} align="right"><RateBar percent={successRate(total)} color={SERIES_COLORS.ok} /></Table.Summary.Cell>
                      <Table.Summary.Cell index={3} align="right">{fmtInt(total.completed)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={4} align="right">{fmtInt(total.length)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={5} align="right">{fmtInt(total.client_closed)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={6} align="right">{fmtInt(total.interrupted)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={7} align="right">{fmtInt(total.error)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={8} align="right">{fmtSeconds(total.ttft_p50_ms)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={9} align="right">{fmtSeconds(total.ttft_p95_ms)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={10} align="right">{fmtSeconds(total.e2e_p50_ms)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={11} align="right">{fmtSeconds(total.e2e_p95_ms)}</Table.Summary.Cell>
                      <Table.Summary.Cell index={12} align="right">{fmtFixed(total.output_tokens_per_sec, 1)}</Table.Summary.Cell>
                    </Table.Summary.Row>
                  </Table.Summary>
                )}
              />
            </Card>
            <BackfillNote range={d.range} kind="quality" />
            <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>{t("quality.foot")} {t("zoneNote", { zone: d.range.time_zone || tz })}</Typography.Paragraph>
          </>
        );
      }}
    </QueryState>
  );
}
