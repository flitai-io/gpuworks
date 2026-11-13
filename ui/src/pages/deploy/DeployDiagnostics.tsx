import { Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { DiagnosticCheck, DiagnosticReport } from "../../diagnosticChecks";
import { diagnosticMessageText } from "./deployDiagnosticText";

const statuses = new Set(["passed", "failed", "pending", "blocked", "unknown", "not_applicable"]);
const observers = new Set(["control_plane", "agent_report", "management_node"]);

function objectLabel(params: Record<string, unknown>, names: string[]): string {
  for (const name of names) {
    const value = params[name];
    if (typeof value === "string" && value.trim()) return value;
    if (typeof value === "number" && Number.isFinite(value)) return String(value);
  }
  return "";
}

function resourceID(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

/** This renders server decisions; it never reruns selection or starts a probe. */
export default function DeployDiagnostics({ report, requestId }: { report?: DiagnosticReport; requestId?: string }) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  if (!report?.checks.length && !requestId) return null;
  const text = (check: DiagnosticCheck) => diagnosticMessageText(t, tCommon, check.message);
  return (
    <Space direction="vertical" style={{ width: "100%" }}>
      {requestId && <Typography.Text type="secondary" copyable={{ text: requestId }}>{t("diagnostics.request", { id: requestId })}</Typography.Text>}
      {!!report?.checks.length && (
        <details open={report.checks.some((check) => check.status === "failed" || check.status === "blocked")}>
          <summary style={{ cursor: "pointer" }}>{t("diagnostics.title", { count: report.checks.length })}</summary>
          <ul style={{ paddingInlineStart: 20, marginBottom: 0 }}>
            {report.checks.map((check) => {
              const params = check.message.params ?? {};
              const status = statuses.has(check.status) ? check.status : "unknown";
              const observer = observers.has(String(params.observer)) ? String(params.observer) : "unknown";
              const objects = [
                ["node", objectLabel(params, ["node", "node_name", "node_uuid", "node_id"])],
                ["storage", objectLabel(params, ["storage", "storage_name", "storage_id"])],
                ["model", objectLabel(params, ["model", "model_name", "model_id"])],
              ].filter(([, name]) => name);
              const dependencies = (check.blocked_by ?? []).map((id) => report.checks.find((item) => item.id === id)).filter((item): item is DiagnosticCheck => Boolean(item));
              return (
                <li key={check.id} style={{ marginBlock: 8 }}>
                  <Tag color={status === "failed" ? "error" : status === "passed" ? "success" : undefined}>{t(`diagnostics.status.${status}`)}</Tag>
                  <span>{text(check)}</span>
                  {objects.length > 0 && <div><Typography.Text type="secondary">{objects.map(([kind, name]) => t(`diagnostics.object.${kind}`, { name })).join(" · ")}</Typography.Text></div>}
                  <div><Typography.Text type="secondary">{t(`diagnostics.observer.${observer}`)}</Typography.Text></div>
                  <Space size="small">
                    {resourceID(params.storage_id) && <Typography.Link href={`/storage?storage=${resourceID(params.storage_id)}`} target="_blank" rel="noopener noreferrer">{t("diagnostics.storageAction")}</Typography.Link>}
                    {resourceID(params.model_id) && <Typography.Link href={`/models?detail=${resourceID(params.model_id)}`} target="_blank" rel="noopener noreferrer">{t("diagnostics.modelAction")}</Typography.Link>}
                  </Space>
                  {check.blocked_by?.length ? <div><Typography.Text type="secondary">{t("diagnostics.blockedBy", { checks: dependencies.length ? dependencies.map(text).join("; ") : t("diagnostics.missingDependency") })}</Typography.Text></div> : null}
                </li>
              );
            })}
          </ul>
          {report.truncated && <Typography.Text type="warning">{t("diagnostics.truncated")}</Typography.Text>}
        </details>
      )}
    </Space>
  );
}
