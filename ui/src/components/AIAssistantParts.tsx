import { Alert, Space, Table, Tag, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { AIFinding, AIModelFit, AIStreamEvent } from "../api";
import { formatBytes } from "../format";

const { Text } = Typography;

// The deterministic half of the assistant: what the server knows without an
// LLM, shown before the answer and on its own when no AI service is set up.

/** FindingCards renders the known failures an instance's logs match. */
export function FindingCards({ findings }: { findings: AIFinding[] }) {
  const { t } = useTranslation("ai");
  if (findings.length === 0) {
    return <Text type="secondary" style={{ fontSize: 12 }}>{t("finding.none")}</Text>;
  }
  return (
    <Space direction="vertical" size={8} style={{ width: "100%" }}>
      {findings.map((f) => (
        <Alert
          key={f.code}
          type="warning"
          showIcon
          message={
            <Space size={6} wrap>
              <span>{t(`finding.${f.code}.title`, { defaultValue: f.code })}</span>
              <Tag color="gold">{t("finding.ruleTag")}</Tag>
            </Space>
          }
          description={
            <Space direction="vertical" size={6} style={{ width: "100%" }}>
              <span>{t(`finding.${f.code}.fix`, { defaultValue: "" })}</span>
              <pre aria-label={t("finding.evidence")} style={{ margin: 0, fontSize: 11, whiteSpace: "pre-wrap", wordBreak: "break-all", background: "rgba(0,0,0,0.04)", padding: "6px 8px", borderRadius: 4 }}>
                {f.evidence.map((e) => e.line).join("\n")}
              </pre>
            </Space>
          }
        />
      ))}
      <Text type="secondary" style={{ fontSize: 12 }}>{t("finding.verifyNote")}</Text>
    </Space>
  );
}

/** FitTable renders the estimator's fit of a model on each card type. */
export function FitTable({ fit }: { fit: AIModelFit }) {
  const { t } = useTranslation("ai");
  if (fit.rows.length === 0) {
    return <Text type="secondary" style={{ fontSize: 12 }}>{t("fit.noNodes")}</Text>;
  }
  const gib = (v: number) => formatBytes(v, { minUnit: "GiB", maxUnit: "GiB" });
  return (
    <Space direction="vertical" size={6} style={{ width: "100%" }}>
      <Space size={6}>
        <Text strong style={{ fontSize: 13 }}>{t("fit.title")}</Text>
        <Tag color="blue">{t("fit.estimatorTag")}</Tag>
      </Space>
      <Table
        size="small"
        pagination={false}
        rowKey={(r) => `${r.vendor}/${r.name}`}
        dataSource={fit.rows}
        columns={[
          {
            title: t("fit.card"), key: "card", render: (_, r) => (
              <Space direction="vertical" size={0}>
                <span>{r.name} <Text type="secondary">×{r.cards} · {t("fit.nodes", { count: r.nodes })}</Text></span>
                {r.unusable > 0 && <Text type="danger" style={{ fontSize: 12 }}>{t("fit.unusable", { count: r.unusable })}</Text>}
              </Space>
            ),
          },
          {
            title: t("fit.runs"), key: "fits", render: (_, r) => (
              <Space direction="vertical" size={0}>
                {r.fits ? <Text type="success">{t("fit.yes", { tp: r.tp })}</Text> : <Text type="danger">{t("fit.no")}</Text>}
                {!r.dtype_supported && <Tooltip title={r.dtype_note}><Text type="warning" style={{ fontSize: 12 }}>{t("fit.dtypeUnsupported", { dtype: fit.dtype || "" })}</Text></Tooltip>}
              </Space>
            ),
          },
          { title: t("fit.link"), key: "link", render: (_, r) => (r.fast_link_cards > 1 ? t("fit.fastLink", { count: r.fast_link_cards }) : r.fast_link_cards === 1 ? t("fit.pcieOnly") : "—") },
          { title: t("fit.perGPU"), key: "need", render: (_, r) => `${gib(r.per_gpu_bytes)} / ${gib(r.usable_bytes)}` },
          { title: t("fit.headroom"), key: "headroom", render: (_, r) => (r.fits ? gib(r.headroom_bytes) : "—") },
        ]}
      />
      <Text type="secondary" style={{ fontSize: 12 }}>
        {t("fit.note", { util: Math.round(fit.gpu_memory_utilization * 100) })}
      </Text>
    </Space>
  );
}

/** One tool call of a streamed answer, as its step events last said. */
export interface LiveStep {
  id: number;
  tool: string;
  args: string;
  status: "running" | "done" | "error";
  ms: number;
  error?: string;
}

/** StepList shows the tool calls the AI made, live: running, done or failed. */
/**
 * stepDetail is what one tool call read, for a person: the endpoint and jq
 * filter of an API read, the words of a search, else the raw arguments.
 */
export function stepDetail(tool: string, args: string): string {
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(args || "{}");
  } catch {
    return args;
  }
  if (tool === "call_api" && typeof parsed.path === "string") {
    const query = parsed.query && typeof parsed.query === "object" ? new URLSearchParams(Object.entries(parsed.query as Record<string, unknown>).map(([k, v]) => [k, String(v)])).toString() : "";
    return `GET ${parsed.path}${query ? `?${query}` : ""}${typeof parsed.jq === "string" && parsed.jq ? ` · jq: ${parsed.jq}` : ""}`;
  }
  if ((tool === "search_api" || tool === "search_metrics") && typeof parsed.query === "string") return parsed.query;
  if ((tool === "get_gpu_topology" || tool === "get_node_connection_diagnostics") && typeof parsed.node_uuid === "string") return parsed.node_uuid;
  if (tool === "compare_with_healthy_replica" && typeof parsed.instance_uuid === "string") return parsed.instance_uuid;
  if (tool === "cluster_overview") return "";
  return args;
}

/**
 * selfCorrectable reports a tool error the model is handed the means to fix
 * on its own next step - a jq filter or arguments it wrote wrong, an endpoint
 * it guessed - rather than a failure of the platform.
 */
export function selfCorrectable(error: string): boolean {
  return /^(jq: |arguments are not valid JSON|no GET endpoint |path must start with |no node is named )/.test(error);
}

export function StepList({ steps }: { steps: LiveStep[] }) {
  const { t } = useTranslation("ai");
  if (steps.length === 0) return null;
  return (
    <ul aria-label={t("trace.steps")} style={{ listStyle: "none", margin: 0, padding: 0, fontSize: 12 }}>
      {steps.map((s) => (
        <li key={s.id} style={{ padding: "2px 0" }}>
          {/* One line per step: the tool tag and the time never wrap or
              shrink; only what it read is cut. A note or error goes on its
              own line below, so it cannot squeeze the row. */}
          <div style={{ display: "flex", gap: 6, alignItems: "center", minWidth: 0 }}>
            <span aria-hidden style={{ width: 14, flex: "none" }}>{s.status === "running" ? "…" : s.status === "done" ? "✓" : s.error && selfCorrectable(s.error) ? "↻" : "✗"}</span>
            <Text code style={{ flex: "none", whiteSpace: "nowrap" }}>{t(`tool.${s.tool}`, { defaultValue: s.tool })}</Text>
            <Text type="secondary" ellipsis={{ tooltip: s.args }} style={{ flex: 1, minWidth: 0 }}>{stepDetail(s.tool, s.args)}</Text>
            <Text type="secondary" style={{ flex: "none", whiteSpace: "nowrap" }}>{s.status === "running" ? t("trace.running") : `${s.ms} ms`}</Text>
          </div>
          {s.error && (
            <div style={{ paddingLeft: 20 }}>
              {selfCorrectable(s.error)
                ? <Text type="secondary" title={s.error} style={{ fontSize: 12 }}>{t("trace.selfCorrect")}</Text>
                : <Text type="danger" style={{ fontSize: 12, wordBreak: "break-word" }}>{s.error}</Text>}
            </div>
          )}
        </li>
      ))}
    </ul>
  );
}

/** AnswerSummary says what a finished answer cost. */
export function AnswerSummary({ done, tools }: { done: Pick<AIStreamEvent & { type: "done" }, "calls" | "usage" | "first_text_ms" | "total_ms">; tools: number }) {
  const { t } = useTranslation("ai");
  const seconds = (ms: number) => (ms / 1000).toFixed(1);
  return (
    <Text type="secondary" style={{ fontSize: 12 }}>
      {t("trace.summary", {
        first: seconds(done.first_text_ms), total: seconds(done.total_ms), tools, calls: done.calls,
        tokens: done.usage.prompt_tokens + done.usage.completion_tokens,
      })}
      {tools === 0 && ` · ${t("trace.noTools")}`}
    </Text>
  );
}

// tailChars bounds what ReasoningTail renders: a long chain of thought grows
// to thousands of tokens, and only its last lines are worth a glance.
const tailChars = 600;

/**
 * ReasoningTail shows the newest lines of the model's reasoning while it
 * thinks and no answer text has come yet: a reasoning model can think for
 * twenty seconds, and a spinner alone reads as a hang.
 */
export function ReasoningTail({ text, label }: { text: string; label: string }) {
  const tail = text.length > tailChars ? "…" + text.slice(-tailChars) : text;
  return (
    <div
      role="log"
      aria-label={label}
      style={{
        maxHeight: "4.8em", overflow: "hidden", display: "flex", flexDirection: "column", justifyContent: "flex-end",
        fontSize: 12, lineHeight: "1.6em", color: "rgba(0,0,0,0.45)", whiteSpace: "pre-wrap", wordBreak: "break-word",
        borderLeft: "2px solid rgba(0,0,0,0.12)", paddingLeft: 8,
        maskImage: "linear-gradient(to bottom, transparent, black 1.6em)", WebkitMaskImage: "linear-gradient(to bottom, transparent, black 1.6em)",
      }}
    >
      <div>{tail}</div>
    </div>
  );
}
