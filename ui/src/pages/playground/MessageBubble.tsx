import { safeErrorText } from "./errors";
import { Alert, Collapse, Space, Spin, Tag, Tooltip, Typography } from "antd";
import { memo } from "react";
import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";
import MarkdownContent from "../../components/MarkdownContent";
import { ABORTED_ERROR } from "./conversations/streams";
import { STREAM_INTERRUPTED_ERROR } from "./api";
import { PreviewAction } from "./HtmlPreview";
import { looksLikeHTMLDocument, PREVIEW_LANGUAGES } from "./previewSandbox";
import type { ChatMessage } from "./types";
import { assistantText } from "./assistantText";

// Memoized: while a reply streams only its own bubble re-renders.
const MessageBubble = memo(function MessageBubble({ message: item }: { message: ChatMessage }) {
  const { t } = useTranslation("playground");
  const metrics = item.metrics;
  const { content, reasoning } = item.role === "assistant"
    ? assistantText(item.content, item.reasoning, item.streaming)
    : item;
  const errorText = item.error === ABORTED_ERROR
    ? t("pane.generationStopped")
    : item.error === STREAM_INTERRUPTED_ERROR ? t("pane.streamInterrupted") : item.error ? safeErrorText(item.error) : undefined;
  // Token-based values estimated from chunk counts carry a ≈.
  const approx = (value: string) => metrics?.approximate && value !== "—" ? `≈${value}` : value;
  const cachedTokens = item.usage?.prompt_tokens_details?.cached_tokens;
  const promptTokens = item.usage?.prompt_tokens;
  const cacheHit = typeof cachedTokens === "number"
    ? promptTokens ? `${cachedTokens.toLocaleString()} (${Math.round((cachedTokens / promptTokens) * 100)}%)` : cachedTokens.toLocaleString()
    : undefined;
  return (
    <article className={`playground-message is-${item.role}`}>
      {item.role === "assistant" && (
        <div className="playground-message-meta">
          <Typography.Text strong>{item.model || t("pane.model")}</Typography.Text>
          <Space size={2}>
            {looksLikeHTMLDocument(content) && <PreviewAction previewKey={`${item.id}:document`} language="html" code={content} complete={!item.streaming} />}
            {item.streaming ? <Spin size="small" /> : content && <CopyButton size="small" label={t("pane.copyReply")} text={content} />}
          </Space>
        </div>
      )}
      {!!item.attachments?.length && <Space size={[4, 4]} wrap>{item.attachments.map((attachment) => <Tag key={attachment.id}>{attachment.type === "image" ? t("pane.image") : t("pane.audio")} · {attachment.name}</Tag>)}</Space>}
      {reasoning && <Collapse ghost size="small" defaultActiveKey={["reasoning"]} items={[{ key: "reasoning", label: t("pane.reasoning"), children: <MarkdownContent content={reasoning} /> }]} />}
      {content ? (
        <MarkdownContent
          content={content}
          codeBlockActions={(block) => PREVIEW_LANGUAGES.has(block.language) && (
            <PreviewAction previewKey={`${item.id}:${block.startOffset}`} language={block.language} code={block.code} complete={block.closed || !item.streaming} />
          )}
        />
      ) : item.streaming ? <span className="playground-stream-cursor" /> : null}
      {item.interrupted && <Alert type="info" showIcon message={t("pane.interrupted")} />}
      {item.error && <Alert type={item.error === ABORTED_ERROR ? "info" : "error"} showIcon message={errorText} />}
      {metrics && (
        <div className="playground-metrics">
          <Metric label="TTFT" help={t("metrics.ttft")} value={formatMS(metrics.ttftMS)} />
          <Metric label="TPOT" help={t("metrics.tpot")} value={approx(formatMS(metrics.tpotMS))} />
          <Metric label="TPS" help={t("metrics.tps")} value={approx(formatNumber(metrics.tps))} />
          <Metric label="Prefill" help={t("metrics.prefill")} value={formatNumber(metrics.prefillTPS, " tok/s")} />
          <Metric label="Token" help={t(metrics.approximate ? "metrics.tokensApproximate" : "metrics.tokens")} value={approx((metrics.outputTokens ?? item.usage?.completion_tokens)?.toLocaleString() || "—")} />
          {cacheHit !== undefined && <Metric label="Cache hit" value={cacheHit} />}
          {item.priorityClass && <Metric label={t("metrics.priorityLabel")} help={t("metrics.priority")} value={item.priorityClass} />}
        </div>
      )}
    </article>
  );
});

export default MessageBubble;

function Metric({ label, value, help }: { label: string; value: string; help?: string }) {
  const body = <span><small>{label}</small><strong>{value}</strong></span>;
  return help ? <Tooltip title={help}>{body}</Tooltip> : body;
}

function formatMS(value?: number): string {
  if (value === undefined || !Number.isFinite(value)) return "—";
  return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${value.toFixed(0)}ms`;
}

function formatNumber(value?: number, suffix = ""): string {
  return value === undefined || !Number.isFinite(value) ? "—" : `${value.toFixed(1)}${suffix}`;
}
