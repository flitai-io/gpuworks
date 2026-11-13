import { Alert, Form, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { EvaluateResponse } from "../../api";
import VRAMBreakdown from "../../components/VRAMBreakdown";
import { warningKey, warningText } from "./deployWarnings";
import { diskRegistrationModelID, scheduleText } from "./deploySchedule";
import ModelDiskRegistrationLink from "./ModelDiskRegistrationLink";
import EvictionSuggestionAlert from "./EvictionSuggestion";
import { APIError } from "../../apiError";
import DeployDiagnostics from "./DeployDiagnostics";

export default function DeployStepSummary({
  result,
  error,
  failure,
  loading,
  onReevaluate,
}: {
  result: EvaluateResponse | null;
  error: string | null;
  failure?: Error | null;
  loading: boolean;
  onReevaluate?: () => void;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  // A hand-written command owns the launch configuration, so the estimator has
  // nothing to read: say so instead of showing numbers from a configuration
  // the instance will not start with (ADR 0020 decision 6).
  const manualCommand =
    Form.useWatch("command_mode", { form, preserve: true }) === "manual";
  const replicas = Number(
    Form.useWatch("replicas", { form, preserve: true }) || 1,
  );
  const warnings = result?.warnings ?? [];
  // A failed evaluation says why, in the scheduler's own words. The summary used
  // to announce a "startup memory check" for every failure, which was wrong for
  // each of the ones that were not about memory — a tensor parallel that cannot
  // divide the model's heads, an image the node cannot run, a template whose
  // model does not resolve (that last one arrives as its own warning).
  const planFailure =
    result?.plan && !result.plan.ok
      ? scheduleText(t, result.plan.message) || t("eval.noPlan")
      : "";
  const registrationModelId = diskRegistrationModelID(result?.plan?.message);
  const conclusion = loading ? (
    <Typography.Text type="warning">{t("summary.loading")}</Typography.Text>
  ) : error ? (
    <Alert type="error" showIcon message={error} />
  ) : !result ? (
    <Typography.Text type="secondary">
      {t("summary.selectModelNode")}
    </Typography.Text>
  ) : result.skipped ? (
    <Alert
      type="info"
      showIcon
      message={t("summary.evaluationSkipped")}
      description={t("summary.weightUnknown")}
    />
  ) : !result.ok ? (
    planFailure ? (
      <Alert type="error" showIcon message={planFailure} action={registrationModelId ? <ModelDiskRegistrationLink modelId={registrationModelId} /> : undefined} />
    ) : null
  ) : replicas > 1 ? (
    <Alert
      type="info"
      showIcon
      message={t("catalog.replicaEstimate", { count: replicas })}
    />
  ) : warnings.length > 0 ? (
    <Typography.Text type="warning">{t("summary.risk")}</Typography.Text>
  ) : (
    <Typography.Text type="success">{t("summary.passed")}</Typography.Text>
  );
  return (
    <div
      className={`deploy-step-summary${result && !result.ok ? " is-risky" : ""}`}
    >
      <Space direction="vertical" size={5} style={{ width: "100%" }}>
        <Typography.Text strong>{t("summary.title")}</Typography.Text>
        {conclusion}
        {!loading && <DeployDiagnostics report={failure instanceof APIError ? failure.diagnostics : result?.diagnostics} requestId={failure instanceof APIError ? failure.requestId : result?.request_id} />}
        {warnings.map((warning) => (
          <Alert
            key={warningKey(warning)}
            type="warning"
            showIcon
            message={warningText(t, warning)}
          />
        ))}
        {result?.eviction?.map((suggestion) => (
          <EvictionSuggestionAlert
            key={suggestion.instance_uuid}
            suggestion={suggestion}
            onSlept={onReevaluate}
          />
        ))}
        {replicas > 1 && result?.plan_scope === "per_replica" && (
          <Typography.Text type="secondary">
            {t("catalog.perReplicaCapacity")}
          </Typography.Text>
        )}
        {manualCommand ? (
          <Alert
            type="info"
            showIcon
            message={t("launchConfig.manualEstimate")}
            description={t("launchConfig.manualEstimateDetail")}
          />
        ) : (
          result?.breakdown &&
          !result.skipped && <VRAMBreakdown breakdown={result.breakdown} />
        )}
      </Space>
    </div>
  );
}
