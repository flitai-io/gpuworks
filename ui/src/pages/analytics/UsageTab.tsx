import { useEffect, useMemo, useState } from "react";
import { Alert, Card, Select, Segmented, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { analyticsApi, type UsageAnalytics, type UsageSource } from "../../analyticsApi";
import BackfillNote, { leadingBackfilled } from "./backfill";
import { SERIES_COLORS, stackedBarOption } from "./chartOptions";
import { type CsvModel } from "./csv";
import EChart from "./EChart";
import { useExportModel } from "./exportContext";
import { fmtCompact, fmtInt, fmtPct, ratio } from "./format";
import { keyTitle } from "./names";
import QueryState from "./QueryState";
import StatCards, { type StatItem } from "./StatCards";
import type { TabProps } from "./types";
import { useAnalyticsData } from "./useAnalyticsData";
import UsageTable from "./UsageTable";
import {
  buildUsageTree, hasKeylessRows, hitRate, keyOptions, uncachedTokens, uncachedUnreported,
  type KeyOption, type UsageGroupBy,
} from "./usageModel";
import { bucketLabel, fullTime, spansDays } from "./zoned";

const GROUPS: UsageGroupBy[] = ["key", "deployment", "model"];

function groupParam(value: string | null): UsageGroupBy {
  return (GROUPS as string[]).includes(value ?? "") ? (value as UsageGroupBy) : "key";
}

export default function UsageTab(props: TabProps) {
  const { from, to, granularity, deployment, tz, reload, search, setSearch } = props;
  const { t, i18n } = useTranslation("analytics");
  const language = i18n.resolvedLanguage ?? "en";
  const key = search.get("key") ?? "";
  const keyPrefix = search.get("keyPrefix") ?? "";
  const source = (["api", "playground", "batch", "evaluation"].includes(search.get("source") ?? "") ? search.get("source") : undefined) as UsageSource | undefined;
  const group = groupParam(search.get("group"));
  const [mode, setMode] = useState<"in" | "out">("in");
  const [options, setOptions] = useState<KeyOption[]>([]);
  const [keyless, setKeyless] = useState(false);

  const query = useAnalyticsData(
    (signal) => analyticsApi.usage({ from, to, granularity, deployment, key: key || undefined, source }, signal),
    [from, to, granularity, deployment, key, source, reload],
  );
  const data = query.data;

  // The key filter lists the keys the unfiltered answer holds: a filtered
  // answer only holds the chosen one, so it adds to the list instead of
  // replacing it.
  useEffect(() => {
    if (!data) return;
    const fresh = keyOptions(data.rows);
    if (!key) {
      setOptions(fresh);
      setKeyless(hasKeylessRows(data.rows));
    } else {
      setOptions((old) => [...old, ...fresh.filter((o) => !old.some((x) => x.id === o.id))]);
    }
  }, [data, key]);

  // A link from the API tokens page names the token by prefix: the filter takes
  // the id, which is in the rows once the unfiltered answer is in.
  const resolved = useMemo(() => (keyPrefix && data && !key ? data.rows.find((r) => r.api_key_prefix === keyPrefix) : undefined), [keyPrefix, data, key]);
  useEffect(() => {
    if (resolved) setSearch({ key: String(resolved.api_key_id), keyPrefix: undefined });
  }, [resolved, setSearch]);
  const prefixMissing = !!keyPrefix && !!data && !key && !resolved;

  const tree = useMemo(() => (data ? buildUsageTree(data.rows, group) : []), [data, group]);

  const exportModel = useMemo<CsvModel | null>(() => {
    if (!data) return null;
    const cs = (c: typeof data.totals) => [c.requests, c.prompt_tokens, c.cached_tokens, fmtPct(hitRate(c)), uncachedTokens(c), c.completion_tokens, c.no_usage];
    return {
      name: "usage",
      head: [
        t("usage.export.key"), t("usage.export.prefix"), t("usage.export.status"), t("usage.export.deployment"), t("usage.export.model"), t("usage.export.source"),
        t("usage.table.requests"), t("usage.table.input"), t("usage.table.cached"), t("usage.table.hitRate"), t("usage.table.uncached"), t("usage.table.output"), t("usage.table.noUsage"),
        t("usage.export.first"), t("usage.export.last"),
      ],
      rows: [
        ...data.rows.map((r) => {
          const info = { id: r.api_key_id, name: r.api_key_name, prefix: r.api_key_prefix, revoked: r.api_key_revoked, deleted: r.api_key_deleted, playground: !r.api_key_id && r.source === "playground", batch: !r.api_key_id && r.source === "batch", evaluation: !r.api_key_id && r.source === "evaluation", userName: r.user_name };
          return [
            keyTitle(t, info).title, r.api_key_prefix, r.api_key_revoked ? t("names.revoked") : "", r.deployment_name || t("names.deploymentId", { id: r.deployment_id }), r.served_name, t(`usage.source.${r.source}`),
            ...cs(r), fullTime(r.first_ms, tz), fullTime(r.last_ms, tz),
          ];
        }),
        [t("table.total"), "", "", "", "", "", ...cs(data.totals), "", ""],
      ],
    };
  }, [data, t, tz]);
  useExportModel(exportModel);

  const cards = (): StatItem[] => {
    const c = data!.totals;
    const hit = hitRate(c);
    const unreported = uncachedUnreported(c);
    return [
      { key: "requests", title: t("usage.cards.requests"), value: fmtInt(c.requests), sub: t("usage.cards.withUsage", { count: c.requests - c.no_usage }) },
      { key: "input", title: t("usage.cards.input"), value: fmtCompact(c.prompt_tokens, language), exact: fmtInt(c.prompt_tokens), sub: t("usage.cards.inputSub") },
      {
        key: "cached", title: t("usage.cards.cached"), value: fmtCompact(c.cached_tokens, language), exact: fmtInt(c.cached_tokens), dot: SERIES_COLORS.hit,
        sub: hit == null ? t("usage.cards.hitUnknown") : t("usage.cards.hitRate", { rate: fmtPct(hit) }),
      },
      {
        key: "uncached", title: t("usage.cards.uncached"), value: fmtCompact(uncachedTokens(c), language), exact: fmtInt(uncachedTokens(c)), dot: SERIES_COLORS.miss,
        tip: t("usage.cards.uncachedTip"),
        sub: unreported > 0 ? t("usage.cards.uncachedUnreported", { tokens: fmtCompact(unreported, language) }) : t("usage.cards.uncachedShare", { rate: fmtPct(ratio(uncachedTokens(c), c.prompt_tokens)) }),
      },
      {
        key: "output", title: t("usage.cards.output"), value: fmtCompact(c.completion_tokens, language), exact: fmtInt(c.completion_tokens), dot: SERIES_COLORS.answer,
        sub: c.reasoning_reported > 0
          ? t("usage.cards.thinking", { tokens: fmtCompact(c.reasoning_tokens, language), count: c.reasoning_reported })
          : t("usage.cards.thinkingUnknown"),
      },
      {
        key: "noUsage", title: t("usage.cards.noUsage"), value: fmtInt(c.no_usage), tip: t("usage.cards.noUsageTip"),
        sub: t("usage.cards.noUsageShare", { rate: fmtPct(ratio(c.no_usage, c.requests)) }),
      },
    ];
  };

  const chart = (d: UsageAnalytics) => {
    const labels = d.range.buckets.map((b) => bucketLabel(b, tz, d.range.granularity, spansDays(d.range.buckets, tz)));
    const series = mode === "in"
      ? [
          { name: t("usage.chart.cached"), color: SERIES_COLORS.hit, data: d.series.map((s) => s.cached_tokens) },
          { name: t("usage.chart.uncached"), color: SERIES_COLORS.miss, data: d.series.map((s) => uncachedTokens(s)) },
        ]
      : [
          { name: t("usage.chart.answer"), color: SERIES_COLORS.answer, data: d.series.map((s) => Math.max(0, s.completion_tokens - s.reasoning_tokens)) },
          { name: t("usage.chart.thinking"), color: SERIES_COLORS.thinking, data: d.series.map((s) => s.reasoning_tokens) },
        ];
    return stackedBarOption({
      labels, series, valueFormat: fmtInt, axisFormat: (v) => fmtCompact(v, language), totalLabel: t("table.total"),
      shadeLeading: leadingBackfilled(d.range), shadeLabel: t("backfill.label"),
    });
  };

  const keySelect = [
    { value: "", label: t("usage.filters.allKeys") },
    ...(keyless ? [{ value: "none", label: t("usage.filters.noKey") }] : []),
    ...options.map((o) => ({ value: String(o.id), label: `${keyTitle(t, o.info).title}${o.info.revoked ? ` (${t("names.revoked")})` : ""}`.trim(), prefix: o.info.prefix })),
  ];

  return (
    <div>
      <Space wrap size={16} style={{ marginBottom: 16 }}>
        <Space size={8}>
          <Typography.Text type="secondary">{t("usage.filters.key")}</Typography.Text>
          <Select
            showSearch
            optionFilterProp="label"
            style={{ minWidth: 200 }}
            aria-label={t("usage.filters.key")}
            value={key}
            options={keySelect}
            onChange={(v) => setSearch({ key: v || undefined, keyPrefix: undefined })}
          />
        </Space>
        <Space size={8}>
          <Typography.Text type="secondary">{t("usage.filters.source")}</Typography.Text>
          <Select
            style={{ minWidth: 140 }}
            aria-label={t("usage.filters.source")}
            value={source ?? ""}
            options={[
              { value: "", label: t("usage.filters.allSources") },
              { value: "api", label: t("usage.source.api") },
              { value: "playground", label: t("usage.source.playground") },
              { value: "batch", label: t("usage.source.batch") },
              { value: "evaluation", label: t("usage.source.evaluation") },
            ]}
            onChange={(v) => setSearch({ source: v || undefined })}
          />
        </Space>
      </Space>
      {prefixMissing && <Alert type="info" showIcon style={{ marginBottom: 16 }} message={t("usage.keyPrefixMissing", { prefix: keyPrefix })} />}
      <QueryState query={query} isEmpty={(d) => d.totals.requests === 0 && d.rows.length === 0}>
        {(d) => (
          <>
            <StatCards items={cards()} />
            <Card style={{ marginBottom: 16 }}>
              <Space wrap align="center" style={{ width: "100%", marginBottom: 8 }}>
                <Typography.Title level={5} style={{ margin: 0 }}>
                  {t(mode === "in" ? "usage.chart.inputTitle" : "usage.chart.outputTitle", { granularity: t(`granularityName.${d.range.granularity}`) })}
                </Typography.Title>
                <Segmented
                  value={mode}
                  onChange={(v) => setMode(v as "in" | "out")}
                  options={[{ value: "in", label: t("usage.chart.modeIn") }, { value: "out", label: t("usage.chart.modeOut") }]}
                />
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t(mode === "in" ? "usage.chart.metaIn" : "usage.chart.metaOut")}</Typography.Text>
              </Space>
              <EChart option={chart(d)} height={340} label={t("usage.chart.aria")} />
            </Card>
            {/* The grouping shapes only the details, so it sits on them, where its effect is seen. */}
            <Card
              styles={{ body: { padding: 0 } }}
              title={<Space align="baseline"><span>{t("usage.table.title")}</span><Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{t("usage.table.hint")}</Typography.Text></Space>}
              extra={
                <Space size={8}>
                  <Typography.Text type="secondary">{t("usage.filters.group")}</Typography.Text>
                  <Segmented
                    value={group}
                    onChange={(v) => setSearch({ group: v === "key" ? undefined : String(v) })}
                    options={GROUPS.map((g) => ({ value: g, label: t(`usage.group.${g}`) }))}
                  />
                </Space>
              }
            >
              <UsageTable tree={tree} totals={d.totals} group={group} granularity={d.range.granularity} tz={tz} />
            </Card>
            <BackfillNote range={d.range} kind="usage" />
            <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>{t("zoneNote", { zone: d.range.time_zone || tz })}</Typography.Paragraph>
          </>
        )}
      </QueryState>
    </div>
  );
}
