import type { ModelNodePresence } from "../../api";
import { formatBytes } from "../../format";

/** Tag colour and models-namespace label of a model file's state. */
export const presenceVisual: Record<string, { labelKey: string; color: string }> = {
  canceled: { labelKey: "presence.canceled", color: "default" },
  pending: { labelKey: "presence.pending", color: "default" },
  downloading: { labelKey: "presence.downloading", color: "blue" },
  syncing: { labelKey: "presence.syncing", color: "processing" },
  present: { labelKey: "presence.present", color: "green" },
  registered: { labelKey: "presence.registered", color: "green" },
  deleting: { labelKey: "presence.deleting", color: "orange" },
  failed: { labelKey: "presence.failed", color: "red" },
  insufficient_disk_space: { labelKey: "presence.insufficientDisk", color: "red" },
  download_required: { labelKey: "presence.downloadRequired", color: "orange" },
  missing: { labelKey: "presence.missing", color: "red" },
};

export const acquisitionLabel: Record<string, string> = {
  peer: "acquisition.peer",
  download: "acquisition.download",
  registered: "acquisition.registered",
  auto: "acquisition.auto",
  imported: "acquisition.imported",
  copy: "acquisition.copy",
};

/**
 * Where a transfer's bytes come from: the node a copy is made from, or the
 * host a download is made from. The agent says the host in its progress
 * message ("downloading from <host>"), so a mirror shows as the mirror; after
 * it leaves an address that failed, the message adds why
 * ("downloading from <host> (switched: <reason>)").
 */
export function transferOrigin(
  row: { acquisition?: string; message?: string; source_node_name?: string; source_node_uuid?: string },
): { node: string } | { host: string; switched?: string } | null {
  if (row.acquisition === "peer" && (row.source_node_name || row.source_node_uuid))
    return { node: row.source_node_name || (row.source_node_uuid ?? "").slice(0, 8) };
  const match = /^downloading from (\S+)(?: \(switched: (.*)\))?$/.exec((row.message ?? "").trim());
  if (!match) return null;
  return match[2] ? { host: match[1], switched: match[2] } : { host: match[1] };
}

export function rateLabel(bytes: number): string {
  if (!bytes) return "";
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GiB/s`;
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MiB/s`;
  return `${(bytes / 1024).toFixed(1)} KiB/s`;
}

export function modelPresenceBytes(presence: Pick<ModelNodePresence, "downloaded" | "size_bytes">): string {
  if (presence.size_bytes > 0) return `${formatBytes(presence.downloaded)} / ${formatBytes(presence.size_bytes)}`;
  if (presence.downloaded > 0) return formatBytes(presence.downloaded);
  return "—";
}
