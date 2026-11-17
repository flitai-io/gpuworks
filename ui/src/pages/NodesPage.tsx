import { useEffect, useMemo, useState } from "react";
import { PlusOutlined } from "@ant-design/icons";
import {
  Button,
  Card,
  Checkbox,
  Input,
  Modal,
  Select,
  Space,
  Typography,
  message,
} from "antd";
import {
  api,
  useEvents,
  usePolling,
  type EngineProfile,
  type List,
  type Node,
  type TopoHighlight,
} from "../api";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { nodeSourcesOf } from "../nodeSources";
import { useTranslation } from "react-i18next";
import ResourceTable, { storedPageSize, type ServerSort } from "../components/resourceTable/ResourceTable";
import { NODE_TABLE_ID, nodeColumns } from "./nodes/nodeColumns";
import NodeDetailDrawer, { type NodeDetailTab } from "./nodes/NodeDetailDrawer";
import { usePagedList } from "../pagedList";
import PageHeader from "../components/PageHeader";
import AddNodeDrawer from "../components/AddNodeDrawer";
import { nodeDisplayName } from "../nodeDisplay";
import NodeLabelsModal from "../components/NodeLabelsModal";
import { NodeSchedulingModal } from "../components/NodeSchedulingModal";
import { labelPair } from "../nodeLabels";
import { useIsAdmin } from "../components/useIsAdmin";
import AgentFleetNotice from "../components/agentUpgrades/AgentFleetNotice";
import AgentUpgradeManager from "../components/agentUpgrades/AgentUpgradeManager";
import {
  useAgentUpgrades,
  type UpgradeEvent,
} from "../agentUpgrade";

import { OptionSelect } from "../ui-kit/OptionSelect";

const NODE_PAGE_SIZE = 20;
const fleetGroups = ["all", "upgrade", "downgrade", "different_build", "check"] as const;

export default function NodesPage() {
  const { t } = useTranslation("nodes");
  const isAdmin = useIsAdmin();
  const [selectedNodeUUIDs, setSelectedNodeUUIDs] = useState<string[]>([]);
  const location = useLocation();
  const navigate = useNavigate();
  const params = new URLSearchParams(location.search);
  const requestedGroup = params.get("agent_upgrade");
  const fleetGroup = requestedGroup === "align" ? "align" : fleetGroups.find(group => group === requestedGroup) ?? "all";
  const fleetSearch = (group: string) => {
    const next = new URLSearchParams(location.search);
    if (group === "all") next.delete("agent_upgrade");
    else next.set("agent_upgrade", group);
    return next.toString();
  };
  const [upgradeRunID, setUpgradeRunID] = useState<number>(() => Number(params.get("agent_run")) || 0);
  const incoming = location.state as {
    nodeUUID?: string;
    tab?: NodeDetailTab;
    highlight?: TopoHighlight;
    openAddDrawer?: boolean;
  } | null;
  // The server pages, searches and filters by label: at a thousand nodes the
  // page used to fetch and render all of them every few seconds.
  const [search, setSearch] = useState("");
  const [labelFilter, setLabelFilter] = useState<string[]>([]);
  const [states, setStates] = useState<string[]>([]);
  const [sort, setSort] = useState<ServerSort | null>(null);
  // The source filter lives in the address, so an external platform's node
  // count can open this list on the nodes that joined through it (?source=).
  const [searchParams, setSearchParams] = useSearchParams();
  const sources = nodeSourcesOf(searchParams);
  const setSources = (next: string[]) => {
    const params = new URLSearchParams(searchParams);
    if (next.length) params.set("source", next.join(","));
    else params.delete("source");
    setSearchParams(params, { replace: true });
  };
  const { data, refetch, refresh, loading, error, updatedAt, pagination } = usePagedList(api.nodesPage, {
    filters: { q: search, label: labelFilter, state: states, extra: { agent_upgrade: [fleetGroup], ...(sources.length ? { source: sources } : {}) }, ...(sort ? { sort: `${sort.order === "desc" ? "-" : ""}${sort.key}` } : {}) },
    pageSize: storedPageSize(NODE_TABLE_ID, NODE_PAGE_SIZE),
    intervalMs: 8000,
  });
  const filtering = fleetGroup !== "all" || search !== "" || labelFilter.length > 0 || states.length > 0 || sources.length > 0;
  const serverHealth = usePolling(api.health, 30000);
  const upgradeErrors = usePolling(api.agentUpgradeErrors, 8000);
  // Engine profiles are static server configuration; one load feeds the detail
  // drawer's kernel check, which is why they are fetched here and not per node.
  const engines = usePolling<List<EngineProfile>>(api.engines, 0);
  // Node events refresh in the background: with a fleet reporting status
  // every minute a foreground reload would keep the table spinning.
  useEvents("node", refresh);
  useEvents("agent_upgrade", refresh);
  const [detail, setDetail] = useState<Node | null>(null);
  const [scheduling, setScheduling] = useState<Node | null>(null);
  const [confirmingDelete, setConfirmingDelete] = useState<Node | null>(null);
  const [forceDelete, setForceDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [editingLabels, setEditingLabels] = useState<Node | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const onUpgradeEvent = (event: UpgradeEvent) => {
    const name = event.name;
    switch (event.kind) {
      case "serverTracked":
        setUpgradeRunID(event.runID);
        break;
      case "serverSettled":
        if (event.node) setDetail((current) => current?.uuid === event.uuid ? event.node! : current);
        refresh();
        break;
      case "dispatched":
        message.success(t("upgrade.dispatched"));
        break;
      case "requestFailed":
        message.error(t("upgrade.requestFailed", { detail: event.detail }));
        break;
      case "completed":
        message.success(t("upgrade.completed", { name }));
        // Reload the open drawer so it shows the new version.
        setDetail((current) =>
          current?.uuid === event.uuid ? event.node : current,
        );
        break;
      case "failed":
        message.error(
          t("upgrade.failedDetail", { name, detail: event.detail }),
        );
        break;
      case "timeout":
        message.error(t("upgrade.timeout", { name }));
        break;
    }
  };
  const upgrades = useAgentUpgrades({
    nodes: data?.items,
    serverVersion: serverHealth.data?.version,
    upgradeErrors: upgradeErrors.data,
    onEvent: onUpgradeEvent,
  });
  // A deep link names a node that need not be on the first page.
  useEffect(() => {
    if (!incoming?.nodeUUID) return;
    let alive = true;
    api
      .node(incoming.nodeUUID)
      .then((node) => alive && setDetail(node))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [incoming?.nodeUUID]);
  useEffect(() => {
    if (incoming?.openAddDrawer) setAddOpen(true);
  }, [incoming?.openAddDrawer]);

  const nodes = data?.items ?? [];
  // The server offers every label in the fleet, not only the page's.
  const labelOptions = useMemo(
    () =>
      (data?.labels ?? []).map(({ key, values }) => ({
        label: key,
        options: values.map((value) => {
          const pair = labelPair(key, value);
          return { value: pair, label: pair };
        }),
      })),
    [data?.labels],
  );

  const deleteNode = async (node: Node) => {
    setDeleting(true);
    try {
      const result = await api.deleteNode(node.uuid, forceDelete);
      const agentUninstalled = result?.agent_uninstalled ?? !forceDelete;
      message.success(t(agentUninstalled ? "message.deleted" : "message.forceDeleted"));
      setConfirmingDelete(null);
      setSelectedNodeUUIDs((current) => current.filter((uuid) => uuid !== node.uuid));
      if (detail?.uuid === node.uuid) setDetail(null);
      refetch();
    } catch (e) {
      message.error(
        t("message.deleteFailed", { detail: (e as Error).message }),
      );
    } finally {
      setDeleting(false);
    }
  };

  // The delete affordance lives in the row's "more" dropdown; this controlled
  // modal keeps the two-step confirmation the old Popconfirm provided. The
  // same shape DeploymentsPage uses for its delete flow.
  const requestDelete = (node: Node) => {
    setForceDelete(false);
    setConfirmingDelete(node);
  };

  const columns = nodeColumns({
    t,
    states,
    showCheckReason: fleetGroup === "check",
    sources,
    sourceOptions: data?.sources ?? [],
    upgradeErrors: upgradeErrors.data,
    upgradePhase: upgrades.phaseOf,
    onOpen: setDetail,
    onEditLabels: setEditingLabels,
    onScheduling: setScheduling,
    onDelete: requestDelete,
  });

  return (
    <>
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={
          <Space>
            <Typography.Text type="secondary">
              {t("page.count", {
                filtered: data?.total ?? 0,
                total: data?.all ?? 0,
              })}
            </Typography.Text>
            <Button
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => setAddOpen(true)}
            >
              {t("action.add")}
            </Button>
          </Space>
        }
      />
      {!error && <div className="empty:hidden mb-4"><AgentFleetNotice summary={data?.agent_fleet} failed={!!data && !data.agent_fleet} retry={refetch} fleetSearch={fleetSearch} /></div>}
      <Card>
        <ResourceTable<Node>
          tableId={NODE_TABLE_ID}
          rowKey="uuid"
          columns={columns}
          data={nodes}
          loading={loading}
          error={error}
          onRefresh={refetch}
          updatedAt={updatedAt}
          pagination={pagination}
          rowSelection={isAdmin ? {
            selectedRowKeys: selectedNodeUUIDs,
            preserveSelectedRowKeys: true,
            onChange: (keys) => setSelectedNodeUUIDs(keys.map(String)),
          } : undefined}
          toolbarExtra={<>
            {isAdmin && selectedNodeUUIDs.length > 0 && <Space>
              <Typography.Text>{t("batchUpgrade.selectedCount", { count: selectedNodeUUIDs.length })}</Typography.Text>
              <Button type="link" onClick={() => setSelectedNodeUUIDs([])}>{t("batchUpgrade.clearSelection")}</Button>
            </Space>}
            <AgentUpgradeManager isAdmin={isAdmin} requestedRunID={upgradeRunID} selectedNodeUUIDs={selectedNodeUUIDs} onStarted={() => setSelectedNodeUUIDs([])} />
          </>}
          sort={sort}
          onSortChange={setSort}
          onFiltersChange={(filters) => {
            setStates(filters.state ?? []);
            // The address is rewritten only when the choice changed: every filter change reports them all.
            const next = filters.source ?? [];
            if (next.join(",") !== sources.join(",")) setSources(next);
          }}
          toolbar={
            <>
              <Input.Search
                allowClear
                style={{ width: 260 }}
                placeholder={t("page.search")}
                aria-label={t("page.search")}
                onSearch={(value) => setSearch(value.trim())}
              />
              <OptionSelect
                prefix={<span className="text-body">{t("fleet.filter")}</span>}
                aria-label={t("fleet.filter")}
                value={fleetGroup}
                options={(fleetGroup === "align" ? [...fleetGroups, "align"] : fleetGroups).map(value => ({ value, label: t(`fleet.status.${value}`) }))}
                onChange={group => navigate({ search: fleetSearch(group) })}
              />
              <Select
                mode="multiple"
                allowClear
                style={{ minWidth: 240 }}
                value={labelFilter}
                onChange={(next: string[]) => setLabelFilter(next)}
                aria-label={t("labelFilter.aria")}
                placeholder={t("labelFilter.placeholder")}
                notFoundContent={t("labelFilter.none")}
                options={labelOptions}
                prefix={<Typography.Text type="secondary">{t("labelFilter.label")}</Typography.Text>}
              />
            </>
          }
          emptyDescription={t(filtering ? "empty.filtered" : "empty.all")}
          emptyKind="nodes"
          emptyAction={
            <Button type="primary" onClick={() => setAddOpen(true)}>
              {t("action.add")}
            </Button>
          }
        />
      </Card>
      <NodeDetailDrawer
        node={detail}
        engines={engines.data?.items}
        // The deep link's tab and highlight belong to the node it names, not to
        // one opened from the list afterwards.
        initialTab={detail && detail.uuid === incoming?.nodeUUID ? incoming.tab : undefined}
        highlight={detail && detail.uuid === incoming?.nodeUUID ? incoming.highlight : undefined}
        upgradePhase={upgrades.phaseOf(detail?.uuid)}
        onUpgrade={(node) => void upgrades.start(node)}
        onClose={() => setDetail(null)}
      />
      <Modal
        open={!!confirmingDelete}
        title={t("delete.confirm")}
        okText={t("action.delete")}
        cancelText={t("action.cancel")}
        okButtonProps={{ danger: true, "aria-label": t("action.delete"), "aria-busy": deleting }}
        confirmLoading={deleting}
        cancelButtonProps={{ disabled: deleting }}
        closable={!deleting}
        maskClosable={!deleting}
        keyboard={!deleting}
        onOk={() => confirmingDelete && !deleting && deleteNode(confirmingDelete)}
        onCancel={() => !deleting && setConfirmingDelete(null)}
      >
        <p>{confirmingDelete && nodeDisplayName(confirmingDelete)}</p>
        <p>{t("delete.description")}</p>
        <p>{t("delete.uninstallDescription")}</p>
        {confirmingDelete?.has_ssh_credential === false && <p role="alert">{t("delete.noSSHDescription")}</p>}
        <Checkbox checked={forceDelete} disabled={deleting} onChange={(event) => setForceDelete(event.target.checked)}>
          {t("delete.force")}
        </Checkbox>
        {forceDelete && <p role="alert">{t("delete.forceWarning")}</p>}
      </Modal>
      {scheduling && (
        <NodeSchedulingModal node={scheduling} onClose={() => setScheduling(null)} onChanged={() => refetch()} />
      )}
      <NodeLabelsModal
        node={editingLabels}
        onClose={() => setEditingLabels(null)}
        onSaved={(node) => {
          if (detail?.uuid === node.uuid) setDetail(node);
          setEditingLabels(null);
          refetch();
        }}
      />
      <AddNodeDrawer
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onInstalled={refetch}
      />
    </>
  );
}
