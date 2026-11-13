// Shared node presentation rules. Operators name nodes when adding them; the
// hostname is only a fallback for nodes nobody named.

interface NamedNode {
  uuid: string;
  name?: string;
  hostname?: string;
}

/** The name a person gave the node, else its hostname, else a short UUID. */
export function nodeDisplayName(node?: NamedNode | null): string {
  if (!node) return "";
  return node.name?.trim() || node.hostname?.trim() || node.uuid.slice(0, 8);
}

/** A node as a status row or a file location names it (node_name, node_uuid): its name, else the full UUID. */
export function nodeRowName(row?: { node_name?: string; node_uuid?: string } | null): string {
  return row?.node_name || row?.node_uuid || "";
}

/**
 * The address a multi-node rank advertises: its compute network's bootstrap
 * IP, else its direct address. Empty means the node cannot join a group.
 */
export function nodeRankAddress(node?: { compute_address?: string; management_address?: string } | null): string {
  return node?.compute_address || node?.management_address || "";
}

// The "udocker" driver ID is kept on the wire and in storage for existing
// nodes and API callers; its implementation is runveil (native image handling
// plus a direct PRoot launcher), which is what operators see.
export function runtimeLabel(driver: string): string {
  return driver === "udocker" ? "runveil" : driver;
}
