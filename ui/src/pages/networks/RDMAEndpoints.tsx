import { Button as ActionButton } from "../../components/ui/button";
import { useState } from "react";
import { Button, Checkbox, Popconfirm, Select, Space, Tag, Tooltip, Typography, message } from "antd";
import { CloseOutlined, ExclamationCircleFilled, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkDetail, NetworkNode, NetworkRail, NetworkType, NodeNetworks, RailGap } from "../../networkTypes";
import { acceleratorFabric, isAcceleratorRDMA, railAddressTitle } from "./networkDisplay";

type T = (key: string, options?: Record<string, unknown>) => string;

/** Why a port cannot carry RDMA traffic, in the viewer's language. */
export function railGapText(t: T, gap?: RailGap | string, type?: NetworkType) {
  const fabric = acceleratorFabric(type);
  if (gap === "no_ipv4" && fabric) return t("rail.cardMissingIP", { fabric });
  return gap ? t(`rail.gap.${gap}`, { defaultValue: gap }) : "";
}

/** "auto" or "manual", with a way back to auto when manual. */
function SourceTag({ source, onReset, busy }: { source?: string; onReset?: () => void; busy?: boolean }) {
  const { t } = useTranslation("networks");
  if (source !== "manual") return <Tag>{t("rail.auto")}</Tag>;
  return (
    <Space size={0} wrap>
      <Tag color="blue">{t("rail.manual")}</Tag>
      {onReset && <Button type="link" size="small" loading={busy} onClick={onReset}>{t("rail.reset")}</Button>}
    </Space>
  );
}

/**
 * Removes an HCA, from every node that uses it (the default: rails pair up
 * across nodes, so they stay alike) or from one node only.
 */
export function RemoveHCAButton({ network, hca, nodeUUID, nodeName, onChanged, label }: {
  network: NetworkDetail;
  hca: string;
  nodeUUID: string;
  nodeName: string;
  onChanged: (detail: NetworkDetail) => void;
  label?: string;
}) {
  const { t } = useTranslation("networks");
  const [onlyThis, setOnlyThis] = useState(false);
  const remove = async () => {
    try {
      const detail = await api.editNetworkHCAs(network.id, { op: "remove", hca, node_uuid: onlyThis ? nodeUUID : undefined });
      message.success(t("rail.removed"));
      onChanged(detail);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };
  return (
    <Popconfirm
      title={t(isAcceleratorRDMA(network.type) ? "rail.removeCardTitle" : "rail.removeTitle", { hca })}
      description={<Checkbox checked={onlyThis} onChange={(e) => setOnlyThis(e.target.checked)}>{t("rail.onlyNode", { node: nodeName })}</Checkbox>}
      onConfirm={remove}
      onOpenChange={(open) => { if (open) setOnlyThis(false); }}
      okText={t("rail.confirm")}
      cancelText={t("actions.cancel")}
    >
      {label
        ? <Button size="small" danger>{label}</Button>
        : <Button type="text" size="small" danger icon={<CloseOutlined />} aria-label={t("rail.remove", { hca })} />}
    </Popconfirm>
  );
}

/** Fetch again on each open or retry so newly reported hardware is available. */
function useInventory(nodeUUID: string) {
  const [inventory, setInventory] = useState<NodeNetworks | null>(null);
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  const load = async () => {
    if (loading) return;
    setLoading(true);
    setFailed(false);
    setInventory(null);
    try {
      setInventory(await api.nodeNetworks(nodeUUID));
    } catch {
      setFailed(true);
    } finally {
      setLoading(false);
    }
  };
  return { inventory, loading, failed, load };
}

function InventoryPicker({ state, options, placeholder, emptyMessage, onClose, onSelect }: {
  state: ReturnType<typeof useInventory>;
  options: { label: string; value: string }[];
  placeholder: string;
  emptyMessage: string;
  onClose: () => void;
  onSelect: (value: string) => void;
}) {
  const { t } = useTranslation("networks");
  const { inventory, loading, failed, load } = state;
  if (!loading && (failed || !inventory?.inventory_reported || options.length === 0)) {
    const message = failed ? t("rail.inventoryReadFailed") : !inventory?.inventory_reported ? t("rail.inventoryPending") : emptyMessage;
    return <span className="inline-flex flex-wrap items-center gap-2">
      <span role={failed ? "alert" : "status"}>{message}</span>
      <Button size="small" onClick={load}>{t("detail.retry")}</Button>
      <Button size="small" type="text" onClick={onClose}>{t("actions.cancel")}</Button>
    </span>;
  }
  return <Select autoFocus size="small" className="w-64 max-w-full" open loading={loading}
    aria-label={placeholder} placeholder={placeholder} options={options} notFoundContent={t("rail.inventoryLoading")}
    onOpenChange={open => { if (!open) onClose(); }} onSelect={onSelect} />;
}

/** An RDMA network's rails on one node: each with its address, removable, and a way to add one. */
export function RailsCell({ network, node, onChanged }: { network: NetworkDetail; node: NetworkNode; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const state = useInventory(node.node_uuid);
  const { inventory, load } = state;
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(false);
  const used = new Set(node.hcas.map((h) => h.name));
  const edit = async (body: Parameters<typeof api.editNetworkHCAs>[1]) => {
    setBusy(true);
    try {
      onChanged(await api.editNetworkHCAs(network.id, body));
      setAdding(false);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };
  // An accelerator network's rails are the cards; a host RDMA network's, the host's RDMA ports.
  const accelerator = isAcceleratorRDMA(network.type);
  const hccs = network.type === "accelerator_hccs";
  const ports: { name: string; ip: string; gap?: string }[] = accelerator
    ? (inventory?.accelerator_ports ?? []).map((p) => ({ name: p.name, ip: hccs ? (p.hccs_ip ?? "") : p.ip, gap: hccs ? p.hccs_gap : p.gap }))
    : (inventory?.rdma_ports ?? []).map((p) => ({ name: `${p.device}:${p.port}`, ip: p.ip, gap: p.gap }));
  const options = ports
    .filter((p) => !used.has(p.name))
    .map((p) => ({
      value: p.name,
      label: [p.name, p.ip || "—", p.gap ? railGapText(t, p.gap, network.type) : ""].filter(Boolean).join(" · "),
    }));
  return (
    <div className="grid min-w-0 gap-2">
      <div className="flex min-w-0 flex-wrap items-center gap-1">
        {adding ? (
          <InventoryPicker state={state} options={options} placeholder={t(accelerator ? "rail.pickCard" : "rail.pick")}
            emptyMessage={t(ports.length ? "rail.allPortsUsed" : "rail.noReportedPorts")}
            onClose={() => setAdding(false)} onSelect={hca => edit({ op: "add", hca, node_uuid: node.node_uuid })} />
        ) : (
          <Button size="small" type="dashed" icon={<PlusOutlined />} disabled={busy} onClick={() => { load(); setAdding(true); }}>{t(accelerator ? "rail.addCard" : "rail.add")}</Button>
        )}
        <SourceTag source={node.hca_source} busy={busy} onReset={() => edit({ op: "reset", node_uuid: node.node_uuid })} />
      </div>
      {node.hcas.length === 0 ? <Typography.Text type="secondary">—</Typography.Text> : (
        <ul aria-label={railAddressTitle(t, network.type)} className="m-0 grid max-h-60 list-none gap-1 overflow-y-auto p-0">
          {node.hcas.map((rail) => (
            <li key={rail.name}>
              <RailLine rail={rail} type={network.type}>
                <RemoveHCAButton network={network} hca={rail.name} nodeUUID={node.node_uuid} nodeName={node.node_name} onChanged={onChanged} />
              </RailLine>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** One rail: its name, its address ("—" for InfiniBand without IPoIB) and why it is unusable. */
export function RailLine({ rail, type, children }: { rail: NetworkRail; type?: NetworkType; children?: React.ReactNode }) {
  const { t } = useTranslation("networks");
  const gap = railGapText(t, rail.gap, type);
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1.5 [overflow-wrap:anywhere]">
      <Typography.Text code>{rail.name}</Typography.Text>
      <Typography.Text type="secondary">{rail.ip || "—"}</Typography.Text>
      {gap && (
        <Tooltip title={gap}>
          <ExclamationCircleFilled role="img" aria-label={gap} tabIndex={0} style={{ color: "var(--warning)" }} />
        </Tooltip>
      )}
      {children}
    </div>
  );
}

/**
 * An RDMA network's bootstrap address on one node, and a way to pin it to one
 * of the node's interfaces or let it be inferred again. Pinning a node reached
 * through a label creates a node attachment for it.
 */
export function BootstrapCell({ network, node, onChanged }: { network: NetworkDetail; node: NetworkNode; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const state = useInventory(node.node_uuid);
  const { inventory, load } = state;
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const attachment = network.attachments.find((a) => a.target === "node" && a.node_uuid === node.node_uuid);
  const save = async (iface: string) => {
    setBusy(true);
    try {
      const detail = attachment
        ? await api.updateNetworkAttachment(network.id, attachment.id, { interface: iface, ip: "", hcas: attachment.hcas })
        : await api.attachNetwork(network.id, { node_uuid: node.node_uuid, interface: iface });
      setEditing(false);
      onChanged(detail);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };
  const options = (inventory?.interfaces ?? [])
    .filter((i) => i.addrs.length > 0)
    .map((i) => ({ value: i.name, label: [i.name, ...i.addrs].join(" · ") }));
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 text-body">
      {node.ip
        ? <code className="min-w-0 font-mono text-body">{[node.interface, node.ip].filter(Boolean).join(" · ")}</code>
        : <span className="text-fg-danger">{t("rail.noBootstrap")}</span>}
      {editing ? (
        <InventoryPicker state={state} options={options} placeholder={t("rail.pickInterface")} emptyMessage={t("rail.noAddressedInterface")}
          onClose={() => setEditing(false)} onSelect={save} />
      ) : (
        <ActionButton size="sm" variant="link" className="text-body" disabled={busy} onClick={() => { load(); setEditing(true); }}>{t("rail.changeBootstrap")}</ActionButton>
      )}
      <SourceTag source={node.bootstrap_source} busy={busy} onReset={attachment ? () => save("") : undefined} />
    </div>
  );
}
