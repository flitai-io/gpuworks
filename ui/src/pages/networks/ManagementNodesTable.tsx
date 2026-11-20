import { useMemo, useState } from "react";
import { Button } from "../../components/ui/button";
import { useNetworkTargetSearch } from "./useNetworkTargetSearch";
import { Switch } from "../../components/ui/switch";
import { Combobox, ComboboxInput, ComboboxContent, ComboboxList, ComboboxItem } from "../../components/ui/combobox";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import { Tooltip } from "../../components/ui/tooltip";
import { Notice } from "../../ui-kit/Notice";
import { message } from "../../ui-kit/feedback";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { useIsAdmin } from "../../components/useIsAdmin";
import { apiErrorText } from "../../apiError";
import type { NetworkNode, NetworkRole } from "../../networkTypes";
import { Endpoint, WarningMark } from "./networkDisplay";

const ROLES: NetworkRole[] = ["serving", "storage_sync", "compute"];
type BulkChoice = "keep" | "allow" | "deny";

/** "key=value" for every label of a node, the way the label filter lists them. */
function labelPairs(labels: Record<string, string> | undefined): string[] {
  return Object.entries(labels ?? {}).map(([k, v]) => `${k}=${v}`);
}

/**
 * Every node on the management network with a switch per role it may carry.
 * Administrators flip one node's role in place, or filter by name, address or
 * label, select nodes and set several roles on all of them at once. Only the
 * roles changed are sent; each node keeps its other settings.
 */
export default function ManagementNodesTable({ nodes, onChanged }: { nodes: NetworkNode[]; onChanged: () => void }) {
  const { t } = useTranslation("networks");
  const { t: tCommon } = useTranslation("common");
  // Until the identity is known, or if the lookup fails, the table is read-only.
  const isAdmin = useIsAdmin();
  const search = useNetworkTargetSearch(0, "nodes", nodes, t("management.search"));
  const [labelFilter, setLabelFilter] = useState<string[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [bulk, setBulk] = useState<Record<NetworkRole, BulkChoice>>({ serving: "keep", storage_sync: "keep", compute: "keep" });
  const [busy, setBusy] = useState<string | null>(null);

  const labelOptions = useMemo(() => {
    const all = new Set<string>();
    // Each row carries its node's labels; no node list is fetched for them.
    for (const n of nodes) for (const pair of labelPairs(n.labels)) all.add(pair);
    return [...all].sort().map((pair) => ({ value: pair, label: pair }));
  }, [nodes]);

  const visible = useMemo(() => {
    if (!labelFilter.length) return search.visible;
    return search.visible.filter((n) => {
      const pairs = labelPairs(n.labels);
      return labelFilter.every((pair) => pairs.includes(pair));
    });
  }, [search.visible, labelFilter]);

  const apply = async (uuids: string[], allow: NetworkRole[], deny: NetworkRole[], key: string) => {
    setBusy(key);
    try {
      const { updated } = await api.updateManagementRoles(uuids, allow, deny);
      message.success(t("management.bulk.done", { count: updated }));
      onChanged();
      return true;
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
      return false;
    } finally {
      setBusy(null);
    }
  };

  // The header says what the switch is about; hovering it says what turning it off does.
  const roleColumn = (role: NetworkRole): ResourceColumn<NetworkNode> => ({
    width: 130, label: t(`role.${role}`),
    // Dotted underline, no icon: an icon would widen the column in the 720px drawer.
    title: <Tooltip content={t(`management.roleTip.${role}`)}><span className="header-help" tabIndex={0}>{t(`role.${role}`)}</span></Tooltip>,
    key: role,
    render: (_: unknown, n: NetworkNode) => {
      const allowed = !(n.roles_off ?? []).includes(role);
      const key = `${n.node_uuid}/${role}`;
      return (
        <div className="isolate flex flex-col items-start gap-0.5" onClick={(event) => event.stopPropagation()}>
          <Switch
            labelVisibility="sr-only"
            label={`${n.node_name} ${t(`management.roleSwitch.${role}`)}`}
            checked={allowed}
            disabled={!isAdmin || busy !== null}
            aria-busy={busy === key}
            onCheckedChange={(on) => void apply([n.node_uuid], on ? [role] : [], on ? [] : [role], key)}
          />
          {allowed && !n.roles.includes(role) && <span className="max-w-full truncate text-label text-fg-muted" title={t("management.dedicated")}>{t("management.dedicatedShort")}</span>}
        </div>
      );
    },
  });

  const columns: ResourceColumn<NetworkNode>[] = [
    {
      title: t("nodeTable.node"),
      dataIndex: "node_name", key: "node", width: 180, role: "name",
      render: (_, n) => <div className="min-w-0"><span><Link to="/nodes" state={{ nodeUUID: n.node_uuid }} className="detail-name-cap" title={n.node_name} onClick={(event) => event.stopPropagation()}>{n.node_name}</Link><WarningMark warnings={n.warnings} /></span><div className="truncate text-label text-fg-muted" title={[n.interface, n.ip].filter(Boolean).join(" · ")}><Endpoint iface={n.interface} ip={n.ip} /></div></div>,
    },
    ...ROLES.map(roleColumn),
  ];
  // A node's labels sit one step down: the row keeps the node, its address
  // and the role switches, which is what fits the 720px detail drawer.
  const labelsOf = (n: NetworkNode) => (
    <dl className="network-node-details text-body" data-selectable={isAdmin}>
      <div className="min-w-0">
        <dt className="text-fg-muted">{t("management.labels")}</dt>
        <dd>{labelPairs(n.labels).join(", ") || "—"}</dd>
      </div>
    </dl>
  );

  const allow = ROLES.filter((r) => bulk[r] === "allow");
  const deny = ROLES.filter((r) => bulk[r] === "deny");
  const choiceOptions = (["keep", "allow", "deny"] as const).map((c) => ({ value: c, label: t(`management.bulk.${c}`) }));

  return (
    <div className="network-management-nodes flex min-w-0 flex-col gap-3">
      <Notice title={t("management.switchIntro")}><span>{t("management.switchEffect")}</span></Notice>
      <div className="flex flex-wrap items-center gap-2">
        <div className="min-w-0 w-60">
          <Combobox multiple items={labelOptions.map(option => option.value)} value={labelFilter} onValueChange={setLabelFilter}>
            <ComboboxInput showClear clearAriaLabel={tCommon("resourceTable.clearFilters")} triggerAriaLabel={t("management.labelFilter")} placeholder={t("management.labelFilter")} aria-label={t("management.labelFilter")} />
            <ComboboxContent><ComboboxList>{(pair: string) => <ComboboxItem key={pair} value={pair}>{pair}</ComboboxItem>}</ComboboxList></ComboboxContent>
          </Combobox>
        </div>
        <span className="text-label text-fg-muted">{t("management.shown", { shown: visible.length, total: nodes.length })}</span>
      </div>
      {labelFilter.length > 0 && <div className="flex flex-wrap gap-1">{labelFilter.map(pair => <Button key={pair} size="sm" variant="secondary" onClick={() => setLabelFilter(values => values.filter(value => value !== pair))}>{pair} ×</Button>)}</div>}
      {isAdmin && (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-body font-semibold">{t("management.bulk.label")}</span>
          <Button size="sm" variant="secondary" onClick={() => setSelected(visible.map((n) => n.node_uuid))} disabled={visible.length === 0 || busy !== null}>
            {t("management.bulk.selectShown", { count: visible.length })}
          </Button>
          <span className="text-body">{t("management.bulk.selected", { count: selected.length })}</span>
          {ROLES.map((role) => (
            <div key={role} className="flex items-center gap-1">
              <span className="text-body text-fg-muted">{t(`role.${role}`)}</span>
              <OptionSelect size="sm" aria-label={`${t("management.bulk.title")} ${t(`role.${role}`)}`} value={bulk[role]} options={choiceOptions}
                onChange={(v) => setBulk((b) => ({ ...b, [role]: v as BulkChoice }))} disabled={busy !== null} className="w-28" />
            </div>
          ))}
          <ConfirmPopover title={t("management.bulk.confirm", { count: selected.length })}
            onConfirm={async () => {
              if (await apply(selected, allow, deny, "bulk")) {
                setSelected([]);
                setBulk({ serving: "keep", storage_sync: "keep", compute: "keep" });
              }
            }} confirmText={t("management.bulk.apply")} cancelText={t("actions.cancel")}
            trigger={<Button size="sm" loading={busy === "bulk"} disabled={busy !== null || selected.length === 0 || (allow.length === 0 && deny.length === 0)}>{t("management.bulk.apply")}</Button>} />
        </div>
      )}
      <ResourceTable<NetworkNode> tableId="network-management-nodes" rowKey="node_uuid"
        columns={columns} data={visible} toolbar={search.searchBox} pageResetKey={JSON.stringify([search.query, labelFilter])}
        loading={search.loading} error={search.error} onRefresh={search.onRefresh}
        rowSelection={isAdmin ? { selectedRowKeys: selected, onChange: (keys) => setSelected(keys.map(String)), preserveSelectedRowKeys: true, getCheckboxProps: () => ({ disabled: busy !== null }) } : undefined}
        emptyDescription={nodes.length === 0 ? t("detail.noNodes") : t("management.noMatch")}
        expandable={{ expandedRowRender: labelsOf, expandRowByClick: true }} />
    </div>
  );
}
