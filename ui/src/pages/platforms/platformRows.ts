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
/** The vendor of an account; a cluster connected on its own is no vendor's. */
export const rowKind = (row: PlatformRow) => row.type === "provider" ? row.provider.kind : "";
/** The platform type, as the server files the account or the cluster entry. */
export const rowCategory = (row: PlatformRow): string => row.type === "provider" ? row.provider.category : row.cluster.category;
export const rowNodeCount = (row: PlatformRow) => row.type === "provider" ? row.provider.node_count ?? 0 : row.cluster.node_count;
export const rowUpdatedAt = (row: PlatformRow) => row.type === "provider" ? row.provider.updated_at : row.cluster.created_at;

/** What the row connects, as the server says. */
export function rowResources(row: PlatformRow): KindResource[] {
  return row.type === "provider" ? row.provider.resources ?? [] : row.cluster.resources ?? [];
}

/**
 * The row's state as the status filter names it: an account's own state
 * (valid, invalid), or where the server says a cluster's entry stands.
 */
export const rowState = (row: PlatformRow): string => row.type === "provider" ? row.provider.state : row.cluster.state;
