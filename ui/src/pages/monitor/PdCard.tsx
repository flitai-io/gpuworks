import type { ReactNode } from "react";
import { Card, Space, Tag, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { MonitorDeploymentPd } from "../../api";
import Dash from "./Dash";
import InfoTitle from "./InfoTitle";

function pct(value: number | null): ReactNode {
  return value == null ? <Dash /> : `${value.toFixed(1)}%`;
}
function ms(value: number | null): ReactNode {
  return value == null ? <Dash /> : `${Math.round(value)} ms`;
}
function num(value: number | null, digits = 0): ReactNode {
  return value == null ? <Dash /> : value.toFixed(digits);
}

// count is a counter over the window; a non-zero one is a problem worth
// seeing.
function count(value: number | null): ReactNode {
  if (value == null) return <Dash />;
  return value > 0 ? <Typography.Text type="danger">{Math.round(value)}</Typography.Text> : "0";
}

function Stat({ label, value }: { label: string; value: ReactNode }) {
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {label}
      </Typography.Text>
      <Typography.Text strong>{value}</Typography.Text>
    </Space>
  );
}

// PdCard is the service monitor page's PD disaggregation summary (ADR 0028,
// ADR 0047): effectiveness, fallback rate with its top reasons, the prefill
// stage's time and own load, and the engine's KV transfer (or why there is
// none) - only rendered when the deployment is a PD deployment
// (data.pd present). Every field is independently nullable (not collected
// yet); each renders its own dash rather than a fabricated 0, same contract
// as ReplicaTable and the rest of this page.
export default function PdCard({ pd }: { pd: MonitorDeploymentPd }) {
  const { t } = useTranslation("monitoring");
  const kv = pd.kv_transfer;
  const topReasons = Object.entries(pd.fallbacks ?? {})
    .sort((a, b) => b[1] - a[1])
    .slice(0, 3);
  return (
    <Card
      size="small"
      style={{ marginBottom: 16 }}
      title={<InfoTitle title={t("deployment.pd.title")} help={t("deployment.pdHelp")} />}
      data-testid="pd-card"
    >
      {/* One row that wraps: the KV transfer stats sit in it too (display:
          contents keeps them flex items of the row). */}
      <div style={{ display: "flex", flexWrap: "wrap", columnGap: 16, rowGap: 8 }}>
        <Stat label={t("deployment.pd.effectiveness")} value={pct(pd.effectiveness_pct)} />
        <Stat label={t("deployment.pd.fallbackRate")} value={pct(pd.fallback_rate_pct)} />
        <Stat label={t("deployment.pd.prefillStage")} value={ms(pd.prefill_stage_avg_ms)} />
        <Stat label={t("deployment.pd.prefillStageP95")} value={ms(pd.prefill_stage_p95_ms)} />
        <Stat label={t("deployment.pd.prefillQueue")} value={num(pd.prefill_waiting_avg, 1)} />
        <Stat label={t("deployment.pd.avgPromptTokens")} value={num(pd.avg_prompt_tokens)} />
        <div style={{ display: "contents" }} data-testid="pd-kv-transfer">
          {kv.exposed ? (
            <>
              <Stat label={t("deployment.pd.kvAvg")} value={ms(kv.avg_ms)} />
              <Stat label={t("deployment.pd.kvP95")} value={ms(kv.p95_ms)} />
              <Stat label={t("deployment.pd.kvSize")} value={kv.avg_mb == null ? <Dash /> : `${kv.avg_mb.toFixed(1)} MB`} />
              <Stat label={t("deployment.pd.kvRate")} value={kv.mb_per_s == null ? <Dash /> : `${kv.mb_per_s.toFixed(1)} MB/s`} />
              <Stat label={t("deployment.pd.kvFailed")} value={count(kv.failed)} />
              <Stat label={t("deployment.pd.kvExpired")} value={count(kv.expired)} />
            </>
          ) : (
            <Stat
              label={t("deployment.pd.kvTransfer")}
              value={
                <Tooltip title={t("deployment.pd.kvNotExposed")}>
                  <Typography.Text type="secondary" style={{ fontWeight: "normal" }} tabIndex={0}>
                    {t("deployment.pd.kvNotExposedShort")}
                  </Typography.Text>
                </Tooltip>
              }
            />
          )}
        </div>
      </div>
      {topReasons.length > 0 && (
        <div style={{ marginTop: 8 }}>
          <Typography.Text type="secondary">{t("deployment.pd.topFallbackReasons")}: </Typography.Text>
          <Space size={4} wrap>
            {topReasons.map(([reason, count]) => (
              <Tag key={reason}>
                {reason} · {count}
              </Tag>
            ))}
          </Space>
        </div>
      )}
    </Card>
  );
}
