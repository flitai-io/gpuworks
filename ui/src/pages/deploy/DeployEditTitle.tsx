import { Badge, Space } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { effectiveDeploymentState, stateLabel } from "../state";
import type { OpenedDeployment } from "./useDeployEditMode";

// The title of the page opened from a deployment's draft: what is being done
// to which deployment, and (for an edit) the state it is in.
export function DeployEditTitle({ opened, compact = false }: { opened: OpenedDeployment; compact?: boolean }) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const name = String(opened.draft.deployment.name ?? opened.deployment?.name ?? opened.id);
  const state = opened.deployment
    ? stateLabel(effectiveDeploymentState(opened.deployment), tCommon)
    : undefined;
  const title = t(opened.mode === "edit" ? "edit.title" : "edit.cloneTitle", { name });
  if (compact) return title;
  return (
    <Space size={12} wrap>
      <span>{title}</span>
      {opened.mode === "edit" && state && <Badge status={state.status} text={state.text} />}
    </Space>
  );
}

/** Back to the deployment's own detail, without saving. */
export function DeployEditBack({ opened }: { opened: OpenedDeployment }) {
  const { t } = useTranslation("deploy");
  return (
    <Link to={`/deployments?deployment=${opened.id}`}>
      {t(opened.mode === "edit" ? "edit.back" : "edit.backSource")}
    </Link>
  );
}
