import { useEffect, useState } from "react";
import type { TFunction } from "i18next";
import { api, type KubeDefaults, type KubeVendorNodes } from "./api";
import { vendorName } from "./vendorName";

/**
 * The cluster form's defaults and limits, which the server owns
 * (GET /api/v1/kube/defaults). null until they arrive; error says why they
 * did not, and the form then sends only what was typed.
 */
export function useKubeDefaults(enabled: boolean): { defaults: KubeDefaults | null; error: Error | null } {
  const [state, setState] = useState<{ defaults: KubeDefaults | null; error: Error | null }>({ defaults: null, error: null });
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    api.kubeDefaults()
      .then((defaults) => { if (alive) setState({ defaults, error: null }); })
      .catch((error: Error) => { if (alive) setState((previous) => ({ defaults: previous.defaults, error })); });
    return () => { alive = false; };
  }, [enabled]);
  return state;
}

/** The kubectl commands that show why a cluster's agents did not join. */
export function kubeTroubleshootCommands(namespace: string): string {
  const ns = /^[a-z0-9-]+$/.test(namespace) ? namespace : `'${namespace}'`;
  return [
    `kubectl -n ${ns} get daemonset,pods -l app.kubernetes.io/name=gpuworks-agent -o wide`,
    `kubectl -n ${ns} describe daemonset gpuworks-agent`,
    `kubectl -n ${ns} logs -l app.kubernetes.io/name=gpuworks-agent --tail=50`,
  ].join("\n");
}

/** How long a borrowed card may sit idle before it goes back, as the list shows it. */
export function cardReturnText(t: TFunction<"kubernetes">, idleReturnSeconds: number): string {
  if (idleReturnSeconds < 0) return t("returnPolicy.keep");
  const duration =
    idleReturnSeconds > 0 && idleReturnSeconds % 3600 === 0 ? t("duration.hours", { count: idleReturnSeconds / 3600 })
    : idleReturnSeconds > 0 && idleReturnSeconds % 60 === 0 ? t("duration.minutes", { count: idleReturnSeconds / 60 })
    : t("duration.seconds", { count: idleReturnSeconds });
  return t("returnPolicy.afterIdle", { duration });
}

/**
 * The vendors of a cluster's joined nodes, most first, as the list shows them:
 * "NVIDIA ×3 · Ascend ×1". A vendor the server leaves empty is nodes that
 * reported no cards; no entry at all (no node yet) is a dash.
 */
export function vendorsText(t: TFunction<"kubernetes">, tCommon: (key: string) => string, vendors: readonly KubeVendorNodes[] | undefined): string {
  if (!vendors || vendors.length === 0) return "—";
  return vendors
    .map((v) => t("vendors.entry", { vendor: v.vendor ? vendorName(v.vendor, tCommon) : t("vendors.none"), count: v.nodes }))
    .join(t("vendors.separator"));
}
