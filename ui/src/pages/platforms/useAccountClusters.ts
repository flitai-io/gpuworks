import { useCallback, useEffect, useRef, useState } from "react";
import { api, type ProviderCluster } from "../../api";
import type { KubeClusterPrefill } from "../kubernetes/kubeClusterForm";

/**
 * A cloud account's managed Kubernetes clusters, read live from the cloud,
 * and connecting one: the connect form opens on the cluster with its GPU node
 * pools to choose from, all of them chosen. Nothing is read while `enabled` is
 * false (a kind without managed Kubernetes).
 */
export function useAccountClusters({ providerId, providerName, enabled, onChanged }: {
  providerId: number;
  providerName: string;
  enabled: boolean;
  /** A cluster entry was made, changed or deleted. */
  onChanged?: () => void;
}) {
  // null until the cloud is first read. A later read keeps the clusters listed
  // meanwhile, so a drawer or dialog open on one of them stays open.
  const [clusters, setClusters] = useState<ProviderCluster[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [connecting, setConnecting] = useState<KubeClusterPrefill | null>(null);
  // Only the latest read lands: an earlier one answering late is dropped.
  const readRef = useRef(0);

  const load = useCallback(() => {
    if (!enabled) return;
    const read = ++readRef.current;
    api.providerClusters(providerId)
      .then((list) => { if (read !== readRef.current) return; setClusters(list.items ?? []); setLoadError(null); })
      .catch((e: Error) => { if (read !== readRef.current) return; setClusters((current) => current ?? []); setLoadError(e.message); });
  }, [providerId, enabled]);
  useEffect(() => {
    load();
    // A read still out when the account changes or the list goes away is not applied.
    return () => { readRef.current++; };
  }, [load]);

  const reload = () => { load(); onChanged?.(); };

  return {
    clusters,
    loadError,
    connecting,
    load,
    reload,
    /** Opens the connect form on the cluster. */
    connect: (c: ProviderCluster) => setConnecting({
      name: c.name,
      pools: c.gpu_pools,
      pool_ids: c.gpu_pools.map((p) => p.id),
      provider_id: providerId,
      provider_cluster_id: c.external_id,
      agent_image_source: c.agent_image_source,
      source_label: `${providerName} · ${c.name}`,
    }),
    closeConnect: () => setConnecting(null),
  };
}

export type AccountClusters = ReturnType<typeof useAccountClusters>;
