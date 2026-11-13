import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { KubeStoppedInstance, ProductWarning } from "../../api";
import { Notice } from "../../ui-kit/Notice";

/** The instances a confirmed node selection change asked to stop; nothing when none. The server decides which, this only lists them. */
export function KubeStoppedInstances({ instances }: { instances?: KubeStoppedInstance[] }) {
  const { t } = useTranslation("kubernetes");
  if (!instances || instances.length === 0) return null;
  return (
    <Notice tone="info" title={t("stopped.title", { count: instances.length })}>
      <span>{t("stopped.help")}</span>
      <ul className="mt-1 flex flex-col gap-0.5 pl-5" style={{ listStyle: "disc" }}>
        {instances.map((i) => (
          <li key={i.instance_uuid}>{t("stopped.item", { deployment: i.deployment || "—", instance: i.instance_uuid })}</li>
        ))}
      </ul>
    </Notice>
  );
}

interface PortsOutside { instance_uuid?: string; deployment?: string; ports?: number[] }

/** The text of one warning an edit carries: its localized text with the server's params, the code itself for one this console has no text for. */
export function kubeWarningText(t: TFunction<"deploy">, warning: ProductWarning): string {
  const params: Record<string, unknown> = { ...warning.params };
  if (warning.code === "KUBE_ENGINE_PORTS_OUTSIDE") {
    const list = Array.isArray(params.instances) ? (params.instances as PortsOutside[]) : [];
    params.instances = list.map((i) => `${i.deployment || i.instance_uuid || "?"} (${(i.ports ?? []).join(", ")})`).join("; ");
  }
  return t(`warning.${warning.code}`, { ...params, defaultValue: warning.code });
}

/** What an edit was allowed to do but the administrator should know about; the texts are the backend warnings' (deploy.json, warning.*). */
export function KubeWarnings({ warnings }: { warnings?: ProductWarning[] }) {
  const { t } = useTranslation("deploy");
  if (!warnings || warnings.length === 0) return null;
  return (
    <>
      {warnings.map((w, i) => <Notice key={`${w.code}-${i}`} tone="warning" title={kubeWarningText(t, w)} />)}
    </>
  );
}
