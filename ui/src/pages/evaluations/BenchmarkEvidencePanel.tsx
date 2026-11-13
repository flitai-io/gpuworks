import { Alert, Button, Card, Collapse, Descriptions, Space, Table, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun, BenchmarkTokenDistribution } from "../../api";

const number = (value: number) => value.toLocaleString(undefined, { maximumFractionDigits: 2 });
const ratio = (numerator: number, denominator: number, unknown: string) => denominator > 0 ? `${number(numerator / denominator * 100)}%` : unknown;
const mean = (value: BenchmarkTokenDistribution, unknown: string) => value.count > 0 ? number(value.total / value.count) : unknown;

export default function BenchmarkEvidencePanel({ run, point }: { run: BenchmarkRun; point?: BenchmarkPoint }) {
  const { t } = useTranslation("evaluations");
  const evidence = point?.long_tail?.evidence;
  const unknown = t("evidence.unknown");
  if (!evidence || evidence.version !== 1) return <Card title={t("evidence.title")}><Alert showIcon type="info" message={t("evidence.missing")} /></Card>;
  const { plan, outcomes, all, timing, execution } = evidence;
  const groups = [
    { key: "all", name: t("evidence.all"), value: all },
    { key: "first", name: t("evidence.first"), value: evidence.first_turn },
    { key: "followup", name: t("evidence.followup"), value: evidence.followup_turns },
  ];
  const duration = timing.started_at && timing.finished_at ? (Date.parse(timing.finished_at) - Date.parse(timing.started_at)) / 1000 : undefined;
  const outputPolicy = plan.output_policy === "natural" ? t("evidence.natural") : plan.output_policy === "ignore_eos_requested" ? t("evidence.ignoreEOSRequested") : unknown;
  const counts = [
    ["planned", plan.planned_requests], ["attempted", all.attempted], ["success", all.success], ["failed", all.failed],
    ["canceled", all.canceled], ["skipped", outcomes.skipped], ["beforeStart", outcomes.canceled_before_start], ["retry", outcomes.retry_count],
  ] as const;
  const tokenRows = [
    { key: "input", name: t("evidence.input"), effective: all.tokens.input, server: all.tokens.server_input, local: all.tokens.local_input },
    { key: "output", name: t("evidence.output"), effective: all.tokens.output, server: all.tokens.server_output, local: all.tokens.local_output },
  ];
  const metricColumns = (field: "ttft" | "tpot" | "last_output_latency" | "completion", title: string) => ({
    title, key: field, render: (_: unknown, row: typeof groups[number]) => {
      const summary = row.value[field];
      return summary.count > 0 ? `${number(summary.p50_ms)} / ${number(summary.p99_ms)} ms · n=${summary.count}` : unknown;
    },
  });
  // TTFT counts reasoning; first visible answer text is a separate time, and a
  // reasoning model can finish a request without any.
  const firstContentColumn = {
    title: t("evidence.firstContent"), key: "first_content", render: (_: unknown, row: typeof groups[number]) => {
      const summary = row.value.first_content;
      const timing = summary && summary.count > 0 ? `${number(summary.p50_ms)} / ${number(summary.p99_ms)} ms · n=${summary.count}` : unknown;
      return row.value.no_content ? `${timing} · ${t("evidence.noContent", { count: row.value.no_content })}` : timing;
    },
  };
  const reasonRows = Object.entries(all.finish_reasons || {}).map(([reason, count]) => ({ reason, count }));
  const finishUnknown = Math.max(0, all.attempted - reasonRows.reduce((total, row) => total + row.count, 0));
  return <Card title={t("evidence.title")} extra={point?.detail_path ? <Button data-report-exclude href={`/api/v1/benchmarks/${run.id}/points/${point.seq}/detail`}>{t("evidence.download")}</Button> : undefined}>
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Descriptions size="small" bordered column={{ xs: 1, sm: 2, xl: 4 }} items={[
        ...counts.map(([key, value]) => ({ key, label: t(`evidence.${key}`), children: number(value) })),
        { key: "requestRate", label: t("evidence.requestSuccess"), children: ratio(all.success, all.attempted, unknown) },
        { key: "sessions", label: t("evidence.completedSessions"), children: `${outcomes.completed_sessions} / ${plan.planned_sessions}` },
        { key: "sessionRate", label: t("evidence.sessionCompletion"), children: ratio(outcomes.completed_sessions, plan.planned_sessions, unknown) },
        { key: "duration", label: t("evidence.duration"), children: duration != null && duration >= 0 ? `${number(duration)} s` : unknown },
        { key: "sessionSpeed", label: t("evidence.sessionsPerMinute"), children: duration && duration > 0 ? number(outcomes.completed_sessions * 60 / duration) : unknown },
        { key: "sessionTime", label: t("evidence.sessionLatency"), children: timing.completed_session_latency.count > 0 ? `${number(timing.completed_session_latency.p50_ms)} / ${number(timing.completed_session_latency.p99_ms)} ms · n=${timing.completed_session_latency.count}` : unknown },
      ]} />
      <Typography.Text type="secondary">{t("evidence.percentiles")}</Typography.Text>
      <Table size="small" rowKey="key" pagination={false} scroll={{ x: 900 }} dataSource={groups} columns={[
        { title: t("evidence.group"), dataIndex: "name" },
        { title: t("evidence.attempted"), render: (_, row) => row.value.attempted },
        metricColumns("ttft", "TTFT"), firstContentColumn, metricColumns("tpot", "TPOT"),
        metricColumns("last_output_latency", t("evidence.lastOutput")), metricColumns("completion", t("evidence.completion")),
      ]} />
      <Typography.Title level={5}>{t("evidence.tokens")}</Typography.Title>
      <Descriptions size="small" column={2} items={[
        { key: "budget", label: t("evidence.outputBudget"), children: plan.output_budget || unknown },
        { key: "policy", label: t("evidence.outputPolicy"), children: outputPolicy },
        { key: "context", label: t("evidence.contextPeak"), children: all.tokens.context.count > 0 ? number(all.tokens.context.max) : unknown },
        { key: "failedOutput", label: t("evidence.failedOutput"), children: all.tokens.failed_output.count > 0 ? number(all.tokens.failed_output.total) : unknown },
      ]} />
      <Table size="small" rowKey="key" pagination={false} scroll={{ x: 620 }} dataSource={tokenRows} columns={[
        { title: t("evidence.tokenType"), dataIndex: "name" },
        { title: t("evidence.observedMean"), render: (_, row) => mean(row.effective, unknown) },
        { title: t("evidence.serverMean"), render: (_, row) => mean(row.server, unknown) },
        { title: t("evidence.localMean"), render: (_, row) => mean(row.local, unknown) },
        { title: t("evidence.peak"), render: (_, row) => row.effective.count > 0 ? number(row.effective.max) : unknown },
        { title: "P50 / P99", render: (_, row) => row.effective.count > 0 ? `${number(row.effective.p50)} / ${number(row.effective.p99)}` : unknown },
      ]} />
      <Typography.Text type="secondary">{t("evidence.localEstimate")}</Typography.Text>
      <Typography.Title level={5}>{t("evidence.cache")}</Typography.Title>
      <Table size="small" rowKey="key" pagination={false} dataSource={groups} columns={[
        { title: t("evidence.group"), dataIndex: "name" },
        { title: t("evidence.cacheReported"), render: (_, row) => `${row.value.cache.reported} / ${row.value.attempted}` },
        { title: t("evidence.cacheUnknown"), render: (_, row) => row.value.cache.unknown },
        { title: t("evidence.coverage"), render: (_, row) => ratio(row.value.cache.reported, row.value.attempted, unknown) },
        { title: t("evidence.cacheWeighted"), render: (_, row) => ratio(row.value.cache.cached_tokens, row.value.cache.input_tokens, unknown) },
      ]} />
      <Alert type="info" showIcon message={t("evidence.cacheState", { policy: execution.cache_policy === "unknown" ? unknown : execution.cache_policy, state: execution.cache_state === "unknown" ? unknown : execution.cache_state })} description={t("evidence.cacheIndependent")} />
      <Descriptions size="small" column={2} items={[
        { key: "finish", label: t("evidence.finishReason"), children: `${reasonRows.map(({ reason, count }) => `${reason}: ${count}`).join(" · ")}${reasonRows.length ? " · " : ""}${unknown}: ${finishUnknown}` },
        { key: "errors", label: t("evidence.errorStages"), children: Object.entries(all.error_stages || {}).map(([stage, count]) => `${stage}: ${count}`).join(" · ") || unknown },
        { key: "seed", label: t("evidence.seed"), children: String(execution.seed) },
        { key: "warmup", label: t("evidence.warmup"), children: `${execution.warmup_succeeded} / ${execution.warmup_requests}` },
      ]} />
      <Collapse items={[{ key: "sessions", label: t("evidence.sessions"), children: <>
        {evidence.sessions_truncated && <Alert type="warning" message={t("evidence.sessionTruncated")} />}
        <Table size="small" rowKey="session_id" pagination={{ pageSize: 10 }} scroll={{ x: 620 }} dataSource={evidence.sessions || []} columns={[
          { title: t("evidence.session"), dataIndex: "session_id" },
          { title: t("evidence.sessionState"), render: (_, row) => t(`evidence.state.${row.state}`, { defaultValue: row.state }) },
          { title: t("evidence.rounds"), render: (_, row) => `${row.success} / ${row.planned_turns}` },
          { title: t("evidence.skipped"), dataIndex: "skipped" },
          { title: t("evidence.sent"), render: (_, row) => row.started_at || unknown },
          { title: t("evidence.finished"), render: (_, row) => row.finished_at || unknown },
        ]} />
        <Typography.Text type="secondary">{t("evidence.timelineExport")}</Typography.Text>
      </> }]} />
    </Space>
  </Card>;
}
