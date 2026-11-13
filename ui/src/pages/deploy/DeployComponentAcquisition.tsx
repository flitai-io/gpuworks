import { useTranslation } from "react-i18next";
import type { AcquisitionComponent, AcquisitionResult } from "../../api";
import { Alert, AlertDescription, AlertTitle, type AlertStatus } from "../../components/ui/alert";
import { formatBytes } from "../../format";
import { askSaveTo } from "./deployWeightChoices";

// How each template component other than the primary reaches the target
// nodes, as the server forecasts it: the create acts on the same plan, so a
// component written into a shared storage without the person choosing one is
// said here before they deploy.
export default function DeployComponentAcquisition({ acquisition }: { acquisition?: AcquisitionResult }) {
  const { t } = useTranslation("deploy");
  const components = acquisition?.components || [];
  if (!acquisition || !components.length) return null;
  const saveToAsked = askSaveTo(acquisition);
  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">{t("workbench.componentAcquisition.title")}</div>
      <ul className="deploy-workbench-body deploy-component-acquisition" aria-label={t("workbench.componentAcquisition.title")}>
        {components.map((component) => (
          <li key={component.name}>
            <Alert status={status(component)}>
              <AlertTitle>
                {t("workbench.componentAcquisition.heading", {
                  name: component.name,
                  model: component.model,
                  size: component.size_bytes ? formatBytes(component.size_bytes) : t("workbench.componentAcquisition.sizeUnknown"),
                })}
              </AlertTitle>
              <AlertDescription>
                {planLines(component, saveToAsked).map(([key, values]) => (
                  <span key={key} className="deploy-component-acquisition-line">{t(key, values)}</span>
                ))}
              </AlertDescription>
            </Alert>
          </li>
        ))}
      </ul>
    </div>
  );
}

// A write into a shared storage nobody chose is the one to notice.
function status(component: AcquisitionComponent): AlertStatus {
  if (component.plan === "shared_download" && component.follows_primary) return "info";
  return component.plan === "ready" ? "default" : "neutral";
}

type Line = [string, Record<string, string | number>];

// saveToAsked says the form shows the Save to question, so the avoid hint can
// point at it.
function planLines(component: AcquisitionComponent, saveToAsked: boolean): Line[] {
  const prefix = "workbench.componentAcquisition.";
  const size = component.size_bytes ? formatBytes(component.size_bytes) : "";
  const missing = component.nodes - component.ready_nodes;
  const lines: Line[] = [];
  switch (component.plan) {
    case "ready":
      lines.push([`${prefix}ready`, { count: component.nodes }]);
      break;
    case "shared_download":
      lines.push([
        `${prefix}${component.follows_primary ? "sharedFollowsPrimary" : "sharedChosen"}`,
        { storage: component.storage || "", count: component.nodes },
      ]);
      if (size) lines.push([`${prefix}sharedUsage`, { storage: component.storage || "", size }]);
      if (component.follows_primary) lines.push([`${prefix}${saveToAsked ? "sharedAvoidOrSaveTo" : "sharedAvoid"}`, {}]);
      break;
    case "node_download":
      lines.push([
        `${prefix}nodeDownload`,
        { count: missing, total: component.size_bytes ? formatBytes(component.size_bytes * missing) : "" },
      ]);
      break;
  }
  if (component.plan !== "ready" && component.ready_nodes > 0) {
    lines.push([`${prefix}someReady`, { count: component.ready_nodes }]);
  }
  return lines;
}
