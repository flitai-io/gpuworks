import type { CloudProvider, KindResource, KubeCluster } from "../../api";

/**
 * A row of the external platforms list: an account of a cloud vendor, or a
 * Kubernetes cluster connected on its own (with kubectl, not from an
 * account). A cluster connected from an account is not a row: it is listed
 * under its account.
 */
export type PlatformRow =
  | { key: string; type: "provider"; provider: CloudProvider }
  | { key: string; type: "cluster"; cluster: KubeCluster };

/** The platform type of a cluster connected on its own; accounts carry theirs from the server. */
export const KUBERNETES_CATEGORY = "kubernetes";
/** The "external platform" a cluster connected on its own is filed under. */
export const KUBERNETES_KIND = "kubernetes";
const KUBERNETES_RESOURCE: KindResource = { type: "kubernetes" };

export const providerRowKey = (id: number) => `p${id}`;
export const clusterRowKey = (uuid: string) => `k${uuid}`;

/** Accounts in the server's order, then the clusters connected on their own. */
export function platformRows(providers: CloudProvider[], clusters: KubeCluster[]): PlatformRow[] {
  return [
    ...providers.map((provider): PlatformRow => ({ key: providerRowKey(provider.id), type: "provider", provider })),
    ...clusters.filter((cluster) => !cluster.source).map((cluster): PlatformRow => ({ key: clusterRowKey(cluster.uuid), type: "cluster", cluster })),
  ];
}

export const rowName = (row: PlatformRow) => row.type === "provider" ? row.provider.name : row.cluster.name;
export const rowKind = (row: PlatformRow) => row.type === "provider" ? row.provider.kind : KUBERNETES_KIND;
export const rowCategory = (row: PlatformRow): string => row.type === "provider" ? row.provider.category : KUBERNETES_CATEGORY;
export const rowNodeCount = (row: PlatformRow) => row.type === "provider" ? row.provider.node_count ?? 0 : row.cluster.node_count;
export const rowUpdatedAt = (row: PlatformRow) => row.type === "provider" ? row.provider.updated_at : row.cluster.created_at;

/** What the row connects. */
export function rowResources(row: PlatformRow): KindResource[] {
  return row.type === "provider" ? row.provider.resources ?? [] : [KUBERNETES_RESOURCE];
}

/**
 * The row's state as the status filter names it, in the words its badge
 * uses: an account's own state (valid, invalid), or where a cluster's entry
 * stands (active, pending until its first node joins, revoked).
 */
export function rowState(row: PlatformRow): string {
  if (row.type === "provider") return row.provider.state;
  if (row.cluster.revoked) return "revoked";
  return row.cluster.cluster_uid ? "active" : "pending";
}
