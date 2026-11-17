import { Badge, type BadgeColor } from "../../components/ui/badge";
import { Tooltip } from "../../components/ui/tooltip";
import { ExclamationCircleFilled } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import type { Network, NetworkRail, NetworkRole, NetworkTestRun, NetworkType, NetworkWarning } from "../../networkTypes";

const roleColors: Record<string, BadgeColor> = { storage_sync: "blue", compute: "purple", serving: "cyan" };

export function RoleTags({ roles }: { roles: NetworkRole[] }) {
  const { t } = useTranslation("networks");
  return (
    <span className="inline-flex flex-wrap gap-1">
      {roles.map((role) => <Badge size="sm" key={role} color={roleColors[role]}>{t(`role.${role}`, { defaultValue: role })}</Badge>)}
    </span>
  );
}

export function TypeTag({ type }: { type: NetworkType }) {
  const { t } = useTranslation("networks");
  const color = type === "host_ethernet" ? "gray" : type === "host_rdma" ? "amber" : "orange";
  return <Badge size="sm" color={color}>{t(`type.${type}`, { defaultValue: type })}</Badge>;
}

/** The host's RDMA ports carry the traffic. */
export function isHostRDMA(type: NetworkType) {
  return type === "host_rdma";
}

/** The accelerator cards' own ports carry the traffic. */
export function isAcceleratorRDMA(type: NetworkType) {
  return type === "accelerator_rdma" || type === "accelerator_hccs";
}

/** The network carries traffic on rails (RDMA ports or cards), apart from its bootstrap address. */
export function usesRails(type: NetworkType) {
  return isHostRDMA(type) || isAcceleratorRDMA(type);
}

/** The nodes a test run covered, as its summary names them: a label run names its key=value. */
export function testScopeText(t: (key: string, options?: Record<string, unknown>) => string, run: Pick<NetworkTestRun, "scope" | "label_key" | "label_value">) {
  const detail = run.scope === "label" && run.label_key ? `${run.label_key}=${run.label_value ?? ""}` : "";
  return t(`test.scopeName.${run.scope}`, { detail });
}

export function SelectorSummary({ network }: { network: Pick<Network, "cidr" | "interface" | "type"> }) {
  const { t } = useTranslation("networks");
  // A network that uses rails infers them and its bootstrap; only a bootstrap CIDR narrows it.
  if (usesRails(network.type)) {
    if (!network.cidr) return <span className="text-fg-muted">{t("selector.inferred")}</span>;
    return <code className="font-mono whitespace-normal">{`${t("selector.bootstrapCidr")} ${network.cidr}`}</code>;
  }
  const parts: string[] = [];
  if (network.cidr) parts.push(`${t("selector.cidr")} ${network.cidr}`);
  if (network.interface) parts.push(`${t("selector.interface")} ${network.interface}`);
  if (parts.length === 0) return <span className="text-fg-muted">{t("selector.none")}</span>;
  return <code className="font-mono whitespace-normal">{parts.join(" · ")}</code>;
}

const knownWarnings = new Set(["conflict", "override_missing", "link_layer_mismatch", "rdma_no_hca", "bootstrap_missing", "hca_unusable", "hca_mismatch"]);

export function warningText(t: (key: string, options?: Record<string, unknown>) => string, warning: NetworkWarning) {
  const params: Record<string, unknown> = { ...(warning.params ?? {}) };
  if (warning.code === "conflict" && typeof params.role === "string") {
    params.role = t(`role.${params.role}`, { defaultValue: params.role });
  }
  if (!knownWarnings.has(warning.code)) {
    return t("warning.unknown", { code: warning.code });
  }
  return t(`warning.${warning.code}`, params);
}

/** Orange exclamation with the reasons in a tooltip; renders nothing without warnings. */
export function WarningMark({ warnings, summary }: { warnings?: NetworkWarning[]; summary?: string }) {
  const { t } = useTranslation("networks");
  const text = summary ?? (warnings ?? []).map((w) => warningText(t, w)).join("\n");
  if (!text) return null;
  return (
    <Tooltip content={<span style={{ whiteSpace: "pre-line" }}>{text}</span>}>
      <ExclamationCircleFilled
        role="img"
        aria-label={text}
        tabIndex={0}
        className="ml-1.5 cursor-help text-fg-warning"
      />
    </Tooltip>
  );
}

/** Network traffic reads in bits per second, like link speeds. */
export function formatBitRate(bytesPerSec: number): string {
  const bits = bytesPerSec * 8;
  for (const [scale, unit] of [[1e12, "Tb/s"], [1e9, "Gb/s"], [1e6, "Mb/s"], [1e3, "Kb/s"]] as const) {
    if (bits >= scale) return `${(bits / scale).toFixed(bits / scale >= 100 ? 0 : 1)} ${unit}`;
  }
  return `${bits.toFixed(0)} b/s`;
}

/** "rx / tx", or a dash when the agent reported no rate recently. */
export function LinkRate({ rx, tx }: { rx?: number; tx?: number }) {
  if (rx == null && tx == null) return <span className="text-fg-muted">—</span>;
  return <span style={{ whiteSpace: "nowrap" }}>{rx == null ? "—" : formatBitRate(rx)} / {tx == null ? "—" : formatBitRate(tx)}</span>;
}

/** The management network's name is shown in the viewer's language. */
export function networkName(t: (key: string) => string, network: { name: string; builtin?: boolean }) {
  return network.builtin ? t("management.name") : network.name;
}

/** Where a node got a network from: a node or label attachment, or the management network. */
export function sourceText(t: (key: string) => string, n: { source: string; label_key?: string; label_value?: string }) {
  if (n.source === "label") return `${t("source.label")} ${n.label_key}=${n.label_value ?? ""}`;
  return n.source === "management" ? t("source.management") : t("source.node");
}

export function Endpoint({ iface, ip }: { iface: string; ip: string }) {
  if (!iface && !ip) return <span className="text-fg-muted">—</span>;
  return <code className="font-mono">{[iface, ip].filter(Boolean).join(" · ")}</code>;
}

/** A compact read-only list of rails ("mlx5_0:1, … mlx5_7:1 (8)") with each address in the tooltip. */
export function HCAList({ hcas, linkLayer }: { hcas: NetworkRail[]; linkLayer?: string }) {
  const { t } = useTranslation("networks");
  if (hcas.length === 0) return <span className="text-fg-muted">—</span>;
  const names = hcas.map((h) => h.name);
  const layer = linkLayer ? <Badge size="sm" className="ml-1.5">{t(`linkLayer.${linkLayer}`, { defaultValue: linkLayer })}</Badge> : null;
  const shown = names.length > 3 ? `${names.slice(0, 2).join(", ")} … ${names[names.length - 1]} (${names.length})` : names.join(", ");
  const detail = hcas.map((h) => `${h.name} ${h.ip || "—"}`).join("\n");
  return <span><Tooltip content={<span style={{ whiteSpace: "pre-line" }}>{detail}</span>}><code className="font-mono">{shown}</code></Tooltip>{layer}</span>;
}
