import { Alert, Button, Descriptions, Skeleton, Space, Tag, Typography, message } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CardCheckDetail, type CardCheckRun, type CardRef } from "../../api";
import { apiErrorText } from "../../apiError";
import CopyButton from "../CopyButton";
import { HelpTip } from "../../ui-kit/HelpTip";
import { checkName, localTime, reasonText, stateName } from "./text";

const stateColor: Record<string, string> = { pass: "green", warn: "orange", fail: "red" };

function Run({ title, run }: { title: string; run: CardCheckRun }) {
  const { t } = useTranslation("nodes");
  const reason = reasonText(t, run.state, run.reason);
  return (
    <Descriptions size="small" column={1} bordered title={title}>
      <Descriptions.Item label={t("cardHealth.detail.at")}>
        <Space size={8} wrap>
          <Tag color={stateColor[run.state]}>{stateName(t, run.state)}</Tag>
          {reason && <Typography.Text>{reason}</Typography.Text>}
          <Typography.Text type="secondary">{localTime(run.at)}</Typography.Text>
        </Space>
      </Descriptions.Item>
      {!!run.duration_ms && <Descriptions.Item label={t("cardHealth.detail.duration")}>{`${run.duration_ms} ms`}</Descriptions.Item>}
      <Descriptions.Item label={t("cardHealth.detail.evidence")}>
        {run.evidence.length ? (
          run.evidence.map((f) => (
            <div key={f.name}>
              <Typography.Text type="secondary">{f.name}</Typography.Text> <Typography.Text>{f.value}</Typography.Text>
            </div>
          ))
        ) : (
          <Typography.Text type="secondary">{t("cardHealth.detail.noEvidence")}</Typography.Text>
        )}
      </Descriptions.Item>
      {run.probe && (
        <Descriptions.Item label={t("cardHealth.detail.probe")}>
          <Space align="start">
            <Typography.Text code style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
              {run.probe.length > 160 ? `${run.probe.slice(0, 160)}…` : run.probe}
            </Typography.Text>
            <CopyButton text={run.probe} label={t("cardHealth.detail.probe")} />
          </Space>
        </Descriptions.Item>
      )}
    </Descriptions>
  );
}

// CheckDetail shows what one check last did on one card, asked of the agent
// when it opens (the agent answers from memory and runs nothing). The
// runtime check can be run again from here when the server offers it.
export default function CheckDetail({
  nodeUUID,
  card,
  cardLabel,
  check,
  canRecheck,
  onRechecked,
}: {
  nodeUUID: string;
  card: CardRef;
  cardLabel: string;
  check: string;
  canRecheck?: boolean;
  onRechecked?: () => void;
}) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [detail, setDetail] = useState<CardCheckDetail>();
  const [error, setError] = useState<string>();
  const [rechecking, setRechecking] = useState(false);
  useEffect(() => {
    let live = true;
    setDetail(undefined);
    setError(undefined);
    api.cardCheckDetail(nodeUUID, card, check).then(
      (d) => live && setDetail(d),
      (err) => live && setError(apiErrorText(tCommon, err)),
    );
    return () => {
      live = false;
    };
  }, [nodeUUID, card.pcie_bdf, card.uuid, check, tCommon]);

  const recheck = async () => {
    setRechecking(true);
    try {
      const d = await api.recheckCard(nodeUUID, card);
      setDetail(d);
      const last = d.last;
      if (last?.state === "pass") message.success(t("cardHealth.recheckDone"));
      else if (last?.state === "skip") message.warning(t("cardHealth.recheckSkipped", { reason: reasonText(t, "skip", last.reason) }));
      else message.error(t("cardHealth.recheckFailed", { reason: last?.evidence[0]?.value ?? reasonText(t, "fail", last?.reason) }));
      onRechecked?.();
    } catch (err) {
      message.error(apiErrorText(tCommon, err));
    } finally {
      setRechecking(false);
    }
  };

  return (
    <div className="card-check-detail">
      <Space style={{ width: "100%", justifyContent: "space-between", marginBottom: 8 }} wrap>
        <span className="inline-flex items-center gap-1">
          <Typography.Text strong>{t("cardHealth.detail.title", { card: cardLabel, check: checkName(t, check) })}</Typography.Text>
          {detail?.agent_started_at && <HelpTip content={t("cardHealth.detail.agentSinceHelp", { time: localTime(detail.agent_started_at) })} />}
        </span>
        {check === "runtime" && canRecheck && (
          <Button size="small" loading={rechecking} onClick={recheck}>
            {t("cardHealth.recheck")}
          </Button>
        )}
      </Space>
      {error && <Alert type="warning" showIcon message={error} />}
      {!error && !detail && <Skeleton active paragraph={{ rows: 3 }} />}
      {detail && (
        <Space direction="vertical" style={{ width: "100%" }} size={12}>
          {detail.last ? <Run title={t("cardHealth.detail.last")} run={detail.last} /> : <Typography.Text type="secondary">{t("cardHealth.detail.noRuns")}</Typography.Text>}
          {detail.last_ran && <Run title={t("cardHealth.detail.lastRan")} run={detail.last_ran} />}
          {detail.last_problem && <Run title={t("cardHealth.detail.lastProblem")} run={detail.last_problem} />}
        </Space>
      )}
    </div>
  );
}
