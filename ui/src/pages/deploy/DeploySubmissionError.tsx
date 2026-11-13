import { Alert } from "antd";
import { useTranslation } from "react-i18next";
import { APIError, apiErrorText } from "../../apiError";
import DeployDiagnostics from "./DeployDiagnostics";

export default function DeploySubmissionError({ error }: { error: Error | null }) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  if (!error) return null;
  return <Alert type="error" showIcon message={t("diagnostics.createFailed")}
    description={<><p>{apiErrorText(tCommon, error)}</p>{error instanceof APIError && <DeployDiagnostics report={error.diagnostics} requestId={error.requestId} />}</>} />;
}
