import { Tag } from "antd";
import type { TFunction } from "i18next";
import type { ImageCatalogEntry, ImageNode } from "../../api";
import { hasLiveAgent } from "../../peerSync";

/**
 * transferRunning reports whether any node row of these versions is still
 * moving. Only versions read from the series detail carry node rows; the
 * catalog-wide list never does, so it never speeds up the poll.
 */
export function transferRunning(versions: ImageCatalogEntry[] | null | undefined) {
  return (versions ?? []).some((version) =>
    (version.nodes ?? []).some(
      (node) => node.state === "pulling" || node.state === "syncing",
    ),
  );
}

export function bytes(value?: number) {
  if (value === undefined || value === null) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let n = value;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i += 1;
  }
  return `${n.toFixed(i > 1 ? 1 : 0)} ${units[i]}`;
}

export function stateTag(t: TFunction, state: string) {
  const colors: Record<string, string> = {
    present: "green",
    pulling: "blue",
    syncing: "blue",
    failed: "red",
    absent: "default",
  };
  return (
    <Tag color={colors[state] || "default"}>
      {t(`state.${state}`, { defaultValue: state })}
    </Tag>
  );
}

export function registryName(ref: string) {
  const host = ref.split("/")[0].toLowerCase();
  if (!host.includes(".") && !host.includes(":") && host !== "localhost")
    return "Docker Hub";
  if (host === "docker.io" || host === "index.docker.io") return "Docker Hub";
  if (host === "quay.io") return "Quay";
  if (host.includes("myhuaweicloud.com")) return "Huawei Cloud";
  if (host.includes("modelscope-registry")) return "ModelScope Registry";
  return host;
}

export type ImageSourceOption = {
  value: string;
  label: string;
  /** Full address shown on hover when the label is only a host. */
  title?: string;
  /** Set on archive-URL options; the address the node is asked to download. */
  archiveUrl?: string;
  /**
   * Set on the one option that is not an address but an action: opening the
   * dialog that asks for an archive URL. Choosing it never starts a transfer
   * on its own, because there is nothing to fetch until an address is given.
   */
  opensDialog?: boolean;
};

/**
 * archiveURLDialogValue is the value of that action option. It is deliberately
 * not a URL, so no code that treats an option value as an address can mistake
 * it for one.
 */
export const archiveURLDialogValue = "__archive_url__";

/**
 * isArchiveURL mirrors the server's single rule for telling an image archive
 * address apart from a registry reference, so the UI can never offer a URL as
 * something to pull.
 */
export function isArchiveURL(entry: string) {
  const value = entry.trim().toLowerCase();
  return value.startsWith("http://") || value.startsWith("https://");
}

/** archiveURLHost is the short label an archive option is shown under. */
export function archiveURLHost(entry: string) {
  try {
    return new URL(entry.trim()).host || entry.trim();
  } catch {
    return entry.trim();
  }
}

/**
 * autoSourceValue is the default way to bring an image to a node: a plain
 * request, and the platform decides where it comes from (pull settings, source
 * rules, probing). The client never names a registry.
 */
export const autoSourceValue = "auto";

/**
 * imageSourceOptions lists what an operator can pick for a node that lacks the
 * image: the automatic pull, the archive addresses the image declares, and the
 * action that asks for another archive URL. Registry references declared on
 * the image are never offered: which registry serves a pull is not a choice
 * made here.
 */
export function imageSourceOptions(registrySources?: string[], t?: TFunction) {
  const options: ImageSourceOption[] = [
    { value: autoSourceValue, label: t?.("nodeTable.sourceAuto") || "Automatic" },
  ];
  const seen = new Set<string>();
  for (const source of registrySources || []) {
    const entry = source.trim();
    // An archive URL is a file to download, never a registry to pull from. It
    // gets one option of its own, labelled with the host and carrying the full
    // address as a tooltip.
    if (!entry || !isArchiveURL(entry) || seen.has(entry)) continue;
    seen.add(entry);
    options.push({
      value: entry,
      label: archiveURLHost(entry),
      title: entry,
      archiveUrl: entry,
    });
  }
  options.push({
    value: archiveURLDialogValue,
    label: t?.("nodeTable.sourceFromURL") || "From URL…",
    opensDialog: true,
  });
  return options;
}

export function rollingTag(t: TFunction, row: ImageCatalogEntry) {
  return row.rolling_tag ? <Tag color="gold">{t("tag.rolling")}</Tag> : null;
}

export function upstreamGoneTag(t: TFunction, row: ImageCatalogEntry) {
  return row.upstream_gone ? <Tag color="red">{t("tag.upstreamGone")}</Tag> : null;
}

/**
 * Nodes that can serve a peer copy of this image to `target`: any
 * other connected node that has it. Only the agent's connection matters: a
 * node whose vendor tool has stopped answering is still connected, and
 * serving a copy needs no GPU. Whether a direct or relayed path is available
 * is not guessed from an address; the probe proves it before
 * sync unlocks.
 */
export function peerSources(version: ImageCatalogEntry, target: ImageNode) {
  return (version.nodes ?? []).filter(
    (node) =>
      node.node_uuid !== target.node_uuid &&
      node.state === "present" &&
      hasLiveAgent(node.node_state),
  );
}

/** shortDigest is the 12-character image ID a node reports, without its "sha256:" prefix. */
export function shortDigest(id?: string) {
  return (id ?? "").trim().replace(/^sha256:/i, "").slice(0, 12);
}

/**
 * digestMismatchNodes counts the nodes that recorded an image ID for this
 * version when those IDs disagree, and is 0 when they agree or fewer than two
 * nodes recorded one. A rolling tag pulled at different times is how two
 * nodes end up running different images under one reference.
 */
export function digestMismatchNodes(version: ImageCatalogEntry) {
  const digests = (version.nodes ?? [])
    .map((node) => (node.image_digest_id ?? "").trim())
    .filter(Boolean);
  return digests.length >= 2 && new Set(digests).size > 1 ? digests.length : 0;
}
