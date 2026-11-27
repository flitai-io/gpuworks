import { getJSON, localizedResponseError, sendJSON, type List } from "../http";
import type { KubeCluster, KubeClusterEdit, KubeClusterOperation, KubeClusterPools, KubeClusterRequest, KubeClusterUpdate, KubeDefaults, KubePrecheck, KubePrecheckRequest, KubeRollout } from "../kube";
import { getText } from "./transport";

/** An error response as an Error in the reader's language: a product error's text, or the proxy-page explanation. */
async function kubeClusterOperation(uuid: string, op: "apply" | "remove", dryRun: boolean): Promise<KubeClusterOperation> {
  const res = await fetch(`/api/v1/kube/clusters/${encodeURIComponent(uuid)}/${op}`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dry_run: dryRun }),
  });
  if (res.status === 424) {
    // The cluster or the cloud refused: the body still says what happened to each object.
    const body = (await res.clone().json().catch(() => null)) as KubeClusterOperation | null;
    if (body && Array.isArray(body.results)) return body;
  }
  if (!res.ok) throw await localizedResponseError(res, `${op}: ${res.status}`);
  return (await res.json()) as KubeClusterOperation;
}

export const kubeApi = {
  kubeClusters: () => getJSON<List<KubeCluster>>("/api/v1/kube/clusters"),
  kubeDefaults: () => getJSON<KubeDefaults>("/api/v1/kube/defaults"),
  /** Replaces the node pools a cloud entry holds; the cluster follows when the manifest is applied again. */
  setKubeClusterPools: (uuid: string, poolIds: string[], confirmDropInstances?: boolean) =>
    sendJSON<KubeClusterPools>("PUT", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}/pools`,
      confirmDropInstances ? { provider_pool_ids: poolIds, confirm_drop_instances: true } : { provider_pool_ids: poolIds }),
  /** Renders the pre-check for a cluster form's fields; nothing is stored. */
  kubePrecheck: (body: KubePrecheckRequest) => sendJSON<KubePrecheck>("POST", "/api/v1/kube/precheck", body),
  createKubeCluster: (body: KubeClusterRequest) => sendJSON<KubeCluster>("POST", "/api/v1/kube/clusters", body),
  updateKubeCluster: (uuid: string, body: KubeClusterUpdate) => sendJSON<KubeClusterEdit>("PUT", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}`, body),
  /** Reads how a cloud entry's agents stand in its cluster, node by node; only reads. */
  kubeRollout: (uuid: string) => getJSON<KubeRollout>(`/api/v1/kube/clusters/${encodeURIComponent(uuid)}/rollout`),
  kubeManifest: (uuid: string) => getText(`/api/v1/kube/clusters/${encodeURIComponent(uuid)}/manifest`),
  revokeKubeCluster: (uuid: string) => sendJSON<KubeCluster>("POST", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}/revoke`),
  deleteKubeCluster: (uuid: string) => sendJSON<void>("DELETE", `/api/v1/kube/clusters/${encodeURIComponent(uuid)}`),
  /** Applies (or, with remove, deletes) an entry's manifest on its cluster; a failure still carries the results it got to. */
  kubeClusterOperation: (uuid: string, op: "apply" | "remove", dryRun: boolean) => kubeClusterOperation(uuid, op, dryRun),
};
