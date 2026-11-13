// The node list's source filter, and where an external platform and its
// nodes link to each other: a node count lands on the nodes it counted, and a
// node's source on the platform's row.

/** The filter value for the nodes added by hand. */
export const NODE_SOURCE_MANUAL = "manual";
/** The filter value for one platform account: the nodes on its instances and of its clusters. */
export const providerSource = (providerID: number) => `provider:${providerID}`;
/** The filter value for the nodes that joined through one cluster entry. */
export const clusterSource = (clusterUUID: string) => `cluster:${clusterUUID}`;

const nodesPath = (source: string) => `/nodes?source=${encodeURIComponent(source)}`;

/** The node list filtered to one platform account's nodes. */
export const platformNodesPath = (providerID: number) => nodesPath(providerSource(providerID));

/** The node list filtered to the nodes that joined through one cluster entry. */
export const kubeClusterNodesPath = (clusterUUID: string) => nodesPath(clusterSource(clusterUUID));

/** The external platforms list with one row opened: an account (with one of its clusters brought into view) or a cluster connected on its own. */
export function platformPath(row: { provider_id?: number; cluster_uuid?: string }): string {
  if (row.provider_id) return `/providers?open=p${row.provider_id}${row.cluster_uuid ? `&cluster=${encodeURIComponent(row.cluster_uuid)}` : ""}`;
  return row.cluster_uuid ? `/providers?open=k${row.cluster_uuid}` : "/providers";
}

const SOURCE_VALUE = /^(manual|provider:[1-9]\d*|cluster:\S+)$/;

/** The sources a node list address asks for (?source=a,b). A value of no known form is dropped rather than sent: the server would refuse the whole list. */
export function nodeSourcesOf(params: URLSearchParams): string[] {
  return [...new Set((params.get("source") ?? "").split(",").map((value) => value.trim()).filter((value) => SOURCE_VALUE.test(value)))];
}
