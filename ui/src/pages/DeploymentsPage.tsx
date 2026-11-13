import { Skeleton } from "../components/ui/skeleton";
import { Tabs, TabsList, TabItem, TabPanel } from "../components/ui/tabs";
import { playgroundRoute } from "./playground/route";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
  Dropdown,
  Input,
  Modal,
  Space,
  Tooltip,
  Typography,
  message,
} from "antd";
import { ApiOutlined, CodeOutlined, CopyOutlined, DeleteOutlined, EditOutlined, ExperimentOutlined, FileAddOutlined, MessageOutlined, MoreOutlined, PlusOutlined } from "@ant-design/icons";
import { api, useEventPayload, useEvents, usePolling, type ChangeEvent, type Deployment, type DeploymentListItem, type DeploymentRuntimeNode, type Instance, type RoutingPolicyOption } from "../api";
import { ListPreparationAction, PreparationLabel } from "./DeploymentPreparation";
import DetailDrawer from "../components/detail/DetailDrawer";
import DeploymentDetailOverview from "./DeploymentDetailOverview";
import { MetricWindowProvider } from "../components/MetricWindow";
import DeploymentMetricsPanel from "../components/DeploymentMetricsPanel";
import { effectiveDeploymentState, partlyAsleep, stateLabel } from "./state";
import DeploymentInstances from "./DeploymentInstances";
import { isModelFileReady } from "../storageTypes";
import DeploymentCatalogTemplateModal from "./DeploymentCatalogTemplateModal";
import DeploymentDetailActions, { type DeploymentActions } from "./DeploymentLifecycleActions";
import { useAssistantSubjects } from "../assistant/AssistantContext";
import { deploymentSubjects } from "../assistant/pageSubjects";
import ResourceTable, { storedPageSize, type ResourceColumn, type ServerSort } from "../components/resourceTable/ResourceTable";
import { formatTime } from "../i18n/datetime";
import { usePagedList } from "../pagedList";
import PageHeader from "../components/PageHeader";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { createRefreshCoalescer } from "./evaluations/evaluationRefresh";
import { localizedText } from "../i18n/localizedText";
import { apiErrorText } from "../apiError";
import { useCopy } from "../components/CopyButton";
import { HealthCheckHelp, HealthTag } from "../components/DeploymentHealth";

export { actualNodesDiffer, DeploymentRuntimeNodeLinks } from "./DeploymentDetailOverview";

const DETAIL_REFRESH_MS = 15_000;
const DEPLOYMENT_PAGE_SIZE = 20;
const DEPLOYMENT_TABLE_ID = "deployments";
const DEPLOYMENT_LIST_STATES = ["running", "sleeping", "asleep", "offline", "starting", "pulling", "pending", "failed", "stopped", "deleting"];

export function deploymentTerminalInstances(deployment: Deployment): Instance[] {
  return (deployment.instances || []).filter((instance) => ["starting", "optimizing", "running"].includes(instance.state) && Boolean(instance.node_uuid && instance.container_id));
}

export function containerTerminalURL(instance: Instance): string {
  return `/nodes/${encodeURIComponent(instance.node_uuid)}/terminal?target=container&ref=${encodeURIComponent(instance.container_id || "")}`;
}

// The list carries ready/desired totals for ordinary and PD deployments.
// Missing readiness stays unknown rather than looking like a ready count.
function replicaCount(d: DeploymentListItem): string {
  const desired = d.replicas ?? 0;
  return `${d.ready_replicas ?? "—"}/${desired}`;
}

// asleepActionHint is the tooltip on an action the deployment cannot take yet.
// A sleep order explains itself: the runtime is fine, and requests are refused
// until someone wakes the deployment, which is not what "not running yet" says.
function asleepActionHint(d: DeploymentListItem, t: (key: string) => string): string {
  switch (effectiveDeploymentState(d)) {
    case "asleep":
    case "sleeping":
      return t("page.asleepUnavailable");
    case "running":
      return "";
    default:
      return t("page.runtimeUnavailable");
  }
}

export default function DeploymentsPage() {
  const { t, i18n } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The server pages, searches and filters: at thousands of deployments the
  // page used to fetch and render every one of them every few seconds.
  const [search, setSearch] = useState("");
  const [states, setStates] = useState<string[]>([]);
  const [sort, setSort] = useState<ServerSort | null>(null);
  const { data, refetch, refresh, loading, error, updatedAt, pagination } = usePagedList(api.deploymentsPage, {
    filters: { q: search, state: states, ...(sort ? { sort: `${sort.order === "desc" ? "-" : ""}${sort.key}` } : {}) },
    pageSize: storedPageSize(DEPLOYMENT_TABLE_ID, DEPLOYMENT_PAGE_SIZE),
    intervalMs: 8000,
  });
  const filtered = search.trim() !== "" || states.length > 0;
  const routingPolicies = usePolling(api.routingPolicies, 0);
  const requested = Number(searchParams.get("deployment"));
  const detailId = Number.isInteger(requested) && requested > 0 ? requested : null;
  const routingOptions = useMemo(
    () => (routingPolicies.data?.items ?? []).map((policy) => ({ value: policy.value, label: localizedText(policy.label, i18n.language) })),
    [routingPolicies.data, i18n.language],
  );
  // Image pulls report progress frequently. SSE updates refresh the existing
  // rows at most once per second so an event stream cannot keep the initial
  // request in a perpetual trailing-refresh loop.
  const refreshRef = useRef(refresh);
  refreshRef.current = refresh;
  const eventRefresh = useMemo(() => createRefreshCoalescer(() => refreshRef.current(), 1000), []);
  useEffect(() => () => eventRefresh.dispose(), [eventRefresh]);
  // No column of the list shows byte counters, and with a fleet downloading
  // there is always a progress notice in flight: reloading every deployment for
  // each would never stop. The open drawer below still follows progress, for the
  // one deployment it shows.
  const refreshUnlessProgress = (event: ChangeEvent) => { if (event.reason !== "progress") eventRefresh.trigger(); };
  useEvents("deployment", eventRefresh.trigger);
  useEventPayload("instance", refreshUnlessProgress);
  useEventPayload("modelfile", refreshUnlessProgress);
  useEventPayload("node", (event) => { if (event.reason?.startsWith("state:")) eventRefresh.trigger(); });
  const openDetail = (id: number) => setSearchParams({ deployment: String(id) });
  const closeDetail = () => setSearchParams({});
  const clone = async (dep: DeploymentListItem) => { try { await api.cloneDeployment(dep.id); message.success(t("page.cloneCreated")); refetch(); } catch (e) { message.error(t("page.cloneFailed", { detail: apiErrorText(tCommon, e) })); } };
  const benchmark = (dep: DeploymentListItem) => navigate(`/evaluations?create=saturation&deployment_id=${dep.id}`);
  const openTerminal = async (deployment: DeploymentListItem) => {
    const tab = window.open("about:blank", "_blank");
    if (tab) tab.opener = null;
    try {
      const detail = await api.deployment(deployment.id);
      const instance = deploymentTerminalInstances(detail)[0];
      if (!instance) throw new Error(t("instanceTerminal.unavailable"));
      const target = containerTerminalURL(instance);
      if (tab) tab.location.replace(target);
      else window.location.assign(target);
    } catch (error) {
      tab?.close();
      message.error(t("instanceTerminal.openFailed", { detail: (error as Error).message }));
    }
  };
  const [deleting, setDeleting] = useState<DeploymentListItem | null>(null);
  const [templating, setTemplating] = useState<number | null>(null);
  const copyCurl = async (dep: DeploymentListItem) => { await copy(deploymentCurl(window.location.origin, dep), { success: t("page.curlCopied") }); };

  function actionsFor(d: DeploymentListItem): DeploymentActions {
    const state = effectiveDeploymentState(d);
    const running = state === "running";
    const unavailable = asleepActionHint(d, t);
    const playground = t("page.openPlayground");
    const curl = t("page.copyCurl");
    return {
      primary: [
        { key: "playground", label: playground, icon: <MessageOutlined />,
          hint: running ? [playground, (d.operations ?? []).map(op => op.type).join(", ")].filter(Boolean).join(" · ") : unavailable,
          disabled: !running || !d.operations?.length, onClick: () => {
            const op = d.operations?.[0];
            if (op) navigate(playgroundRoute(op, d.served_name || d.model_name));
          } },
        { key: "curl", label: curl, icon: <ApiOutlined />, hint: running ? curl : unavailable,
          disabled: !running, onClick: () => void copyCurl(d) },
      ],
      terminal: { key: "terminal", label: t("instanceTerminal.open"), icon: <CodeOutlined />,
        disabled: !["starting", "optimizing", "running", "sleeping", "asleep"].includes(state), onClick: () => void openTerminal(d) },
      menu: [
        { key: "edit", label: t("page.edit"), icon: <EditOutlined />, disabled: d.desired_state === "deleting", onClick: () => navigate(`/deploy?edit=${d.id}`) },
        { key: "clone", label: t("page.clone"), icon: <CopyOutlined />, onClick: () => void clone(d) },
        { key: "cloneModify", label: t("page.cloneModify"), icon: <CopyOutlined />, onClick: () => navigate(`/deploy?from=${d.id}`) },
        { key: "benchmark", label: t("page.benchmark"), icon: <ExperimentOutlined />, disabled: !running, onClick: () => void benchmark(d) },
        { key: "catalogTemplate", label: t("catalogTemplate.open"), icon: <FileAddOutlined />, disabled: !running, onClick: () => setTemplating(d.id) },
      ],
      remove: { key: "delete", label: t("delete.button"), icon: <DeleteOutlined />, danger: true, onClick: () => setDeleting(d) },
    };
  }

  const columns: ResourceColumn<DeploymentListItem>[] = [
    {
      key: "name",
      role: "name",
      title: t("page.column.name"),
      width: 220,
      serverSort: true,
      render: (_: unknown, d: DeploymentListItem) => <Button type="link" onClick={() => openDetail(d.id)}>{d.name}</Button>,
    },
    {
      key: "served_name",
      title: t("page.column.servedName"),
      width: 220,
      // ResourceTable ellipsizes every column but actions, and that clips the
      // whole cell: with an inline copy button inside it, a long external
      // model name took the button off the edge with it, so the name could not
      // be copied at all (GWKS-145). The cell therefore clips nothing itself —
      // the name truncates on its own and the copy button is its sibling, so
      // the button keeps its space at any name length. Typography's ellipsis
      // does not engage alongside copyable, hence the two elements.
      ellipsis: false,
      render: (_: unknown, d: DeploymentListItem) => d.served_name
        ? (
          <span className="served-name-cell">
            <Typography.Text className="served-name-text" ellipsis={{ tooltip: d.served_name }}>{d.served_name}</Typography.Text>
            <Typography.Text className="served-name-copy" copyable={{ text: d.served_name }} />
          </span>
        )
        : "—",
    },
    { key: "model", title: t("page.column.model"), width: 220, serverSort: true, render: (_: unknown, d: DeploymentListItem) => d.model_name || "—" },
    {
      key: "runtime",
      title: t("page.column.runtime"),
      width: 220,
      render: (_: unknown, d: DeploymentListItem) => <><span>{d.engine || "—"}</span><Typography.Text type="secondary" className="resource-table-sub">{d.image || t("page.autoImage")}</Typography.Text></>,
    },
    {
      key: "node",
      title: t("page.column.node"),
      width: 160,
      // Where the replicas run now - not where the deployment was asked to run,
      // which has its own column beside it. A spread deployment folds into
      // "198.51.100.31 and 1 more" with every node in the tooltip and the drawer,
      // so no replica's node is hidden behind another's (GWKS-120).
      render: (_: unknown, d: DeploymentListItem) => <DeploymentRuntimeNodes dep={d} />,
    },
    {
      key: "targetNode",
      title: t("page.column.targetNode"),
      width: 160,
      defaultHidden: true,
      // The node the deployment was asked to run on. It stays available beside
      // the actual one rather than carrying both meanings in one field.
      render: (_: unknown, d: DeploymentListItem) => <DeploymentTargetNodes dep={d} />,
    },
    {
      key: "state",
      title: t("page.column.state"),
      width: 120,
      serverSort: true,
      filters: DEPLOYMENT_LIST_STATES.map((state) => ({ value: state, text: stateLabel(state, tCommon).text })),
      filteredValue: states,
      render: (_: unknown, deployment: DeploymentListItem) => {
        const state = effectiveDeploymentState(deployment);
        const b = stateLabel(state, tCommon);
        // A partly slept deployment still serves, so it keeps the running
        // text; the count beside it says how much of it is awake, which a
        // plain green badge would hide.
        const partial = partlyAsleep(deployment);
        // A failed model preparation is the reason the row is not running, so
        // the retry is offered here, where that state is read - not only in
        // the drawer behind it (GWKS-143).
        return <>
          <Badge status={partial ? "processing" : b.status} text={b.text} />
          {partial && <Typography.Text type="secondary" className="resource-table-sub">{t("sleep.partial", { asleep: deployment.asleep_replicas, total: deployment.replicas ?? 0 })}</Typography.Text>}
          {deployment.preparation && <Typography.Text type="secondary" className="resource-table-sub"><PreparationLabel item={deployment.preparation} /></Typography.Text>}
          {deployment.preparation && <ListPreparationAction item={deployment.preparation} onChanged={refetch} />}
        </>;
      },
    },
    { key: "health", title: <Space size={0}>{t("page.column.health")}<HealthCheckHelp /></Space>, width: 140, render: (_: unknown, d: DeploymentListItem) => <HealthTag health={d.health} /> },
    // A list row carries the replica counts; the detail lists the instances.
    { key: "replicas", title: t("page.column.instances"), width: 160, align: "right", serverSort: true, render: (_: unknown, d: DeploymentListItem) => replicaCount(d) },
    { key: "created_at", title: t("page.column.createdAt"), width: 180, serverSort: true, render: (_: unknown, d: DeploymentListItem) => formatTime(d.created_at) },
    {
      key: "action",
      role: "actions",
      width: 120,
      render: (_: unknown, d: DeploymentListItem) => {
        const { primary, terminal, menu, remove } = actionsFor(d);
        return <Space size={4}>
          {primary.map(action => <Tooltip key={action.key} title={action.hint}>
            <Button type="text" size="small" aria-label={action.label} icon={action.icon} disabled={action.disabled} onClick={action.onClick} />
          </Tooltip>)}
          <Dropdown trigger={["click"]} menu={{ items: [{ ...terminal }, ...menu.map(item => ({ ...item })), { type: "divider" }, { ...remove }] }}>
            <Tooltip title={t("page.more")}><Button type="text" size="small" icon={<MoreOutlined />} aria-label={t("page.more")} /></Tooltip>
          </Dropdown>
        </Space>;
      },
    },
  ];

  return (
    <>
      {copyHolder}
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={
          <Space>
            <Typography.Text type="secondary">{t("page.count", { count: data?.total ?? 0 })}</Typography.Text>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => navigate("/deploy")}>{t("page.create")}</Button>
          </Space>
        }
      />
      <Card>
        <ResourceTable<DeploymentListItem>
          tableId={DEPLOYMENT_TABLE_ID}
          rowKey="id"
          columns={columns}
          data={data?.items ?? []}
          loading={loading}
          error={error}
          onRefresh={refetch}
          updatedAt={updatedAt}
          pagination={pagination}
          sort={sort}
          onSortChange={setSort}
          onFiltersChange={(filters) => setStates(filters.state ?? [])}
          toolbar={
            <Input.Search
              allowClear
              style={{ width: 280 }}
              placeholder={t("page.search")}
              aria-label={t("page.search")}
              onSearch={(value) => setSearch(value.trim())}
            />
          }
          emptyDescription={t(filtered ? "page.emptyFiltered" : "page.empty")}
          emptyKind="deployments"
          emptyAction={filtered ? undefined : <Button type="primary" onClick={() => navigate("/deploy")}>{t("page.create")}</Button>}
        />
      </Card>
      <DeleteDeploymentModal dep={deleting} onClose={() => setDeleting(null)} onDeleted={() => {
        if (deleting?.id === detailId) closeDetail();
        refetch();
      }} />
      {templating != null && <DeploymentCatalogTemplateModal deploymentID={templating} onClose={() => setTemplating(null)} />}
      <DeploymentDetail
        id={detailId}
        focusInstance={searchParams.get("instance")}
        routingPolicies={routingPolicies.data?.items ?? []}
        routingOptions={routingOptions}
        onClose={closeDetail}
        actionsFor={actionsFor}
      />
    </>
  );
}

function DeleteDeploymentModal({
  dep,
  onClose,
  onDeleted,
}: {
  dep: Pick<Deployment, "id" | "name"> | null;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [force, setForce] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const close = () => {
    if (deleting) return;
    setForce(false);
    onClose();
  };

  const remove = async () => {
    if (!dep) return;
    setDeleting(true);
    try {
      await api.deleteDeployment(dep.id, force);
      message.success(force ? t("delete.forced") : t("delete.started"));
      setForce(false);
      onClose();
      onDeleted();
    } catch (e) {
      message.error(t("delete.failed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <Modal
      title={t("delete.title", { name: dep?.name ?? "" })}
      open={dep != null}
      onCancel={close}
      onOk={remove}
      okText={force ? t("delete.forceButton") : t("delete.button")}
      cancelText={t("delete.cancel")}
      okButtonProps={{ danger: true, loading: deleting }}
      cancelButtonProps={{ disabled: deleting }}
      maskClosable={!deleting}
      closable={!deleting}
    >
      <Typography.Paragraph>
        {t("delete.description")}
      </Typography.Paragraph>
      <Checkbox checked={force} onChange={(event) => setForce(event.target.checked)}>
        {t("delete.force")}
      </Checkbox>
      {force && (
        <Alert
          style={{ marginTop: 16 }}
          type="warning"
          showIcon
          message={t("delete.forceWarning")}
          description={t("delete.forceDescription")}
        />
      )}
    </Modal>
  );
}

function DeploymentDetail({
  id,
  routingPolicies,
  routingOptions,
  focusInstance,
  onClose,
  actionsFor,
}: {
  id: number | null;
  focusInstance?: string | null;
  routingPolicies: RoutingPolicyOption[];
  routingOptions: Array<{ value: RoutingPolicyOption["value"]; label: string }>;
  onClose: () => void;
  actionsFor: (dep: DeploymentListItem) => DeploymentActions;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [loaded, setLoaded] = useState<Deployment | null>(null);
  const firstTab = !focusInstance ? "overview" : "instances";
  const [detailTab, setDetailTab] = useState(firstTab);
  const [visitedInstances, setVisitedInstances] = useState<number | null>(null);
  useEffect(() => {
    if (detailTab === "instances") setVisitedInstances(id);
  }, [detailTab, id]);
  const [drilldown, setDrilldown] = useState<{ uuid: string; visit: number; target: "metrics" | "startup" } | null>(null);
  useEffect(() => {
    setDetailTab(firstTab);
    setDrilldown(null);
  }, [id, focusInstance, firstTab]);
  const inspectInstance = (uuid: string) => {
    setDrilldown((last) => ({ uuid, visit: (last?.visit ?? 0) + 1, target: "metrics" }));
    setDetailTab("instances");
  };
  const [loadError, setLoadError] = useState<Error | null>(null);
  const { t: tAI } = useTranslation("ai");
  const dep = loaded?.id === id ? loaded : null;
  const seenLaunches = useRef(new Set<number>());
  useEffect(() => {
    if (!dep || seenLaunches.current.has(dep.id)) return;
    const instances = dep.instances ?? [];
    if (focusInstance || drilldown || instances.some(inst => ["running", "failed", "stopped", "removing"].includes(inst.state))) {
      seenLaunches.current.add(dep.id);
      return;
    }
    const first = [...instances].reverse().find(inst => inst.replica && inst.dist_role !== "follower");
    if (!first || !["pulling", "starting", "optimizing"].includes(first.state)) return;
    if (dep.model_progress?.some(row => row.node_uuid === first.node_uuid && !isModelFileReady(row.state))) return;
    seenLaunches.current.add(dep.id);
    setDrilldown(last => ({ uuid: first.uuid, visit: (last?.visit ?? 0) + 1, target: "startup" }));
    setDetailTab("instances");
  }, [dep, drilldown, focusInstance]);
  useAssistantSubjects(dep ? deploymentSubjects(dep, tAI) : null);
  const requestVersion = useRef(0);
  const currentIDRef = useRef(id);
  currentIDRef.current = id;

  const load = useCallback(async () => {
    if (id == null) return;
    const version = ++requestVersion.current;
    try {
      const next = await api.deployment(id);
      if (currentIDRef.current === id && version === requestVersion.current) {
        setLoaded(next);
        setLoadError(null);
      }
    } catch (error) {
      if (currentIDRef.current === id && version === requestVersion.current) setLoadError(error instanceof Error ? error : new Error(String(error)));
    }
  }, [id]);
  const refreshRef = useRef<ReturnType<typeof createRefreshCoalescer> | null>(null);
  const refresh = useCallback(() => refreshRef.current?.trigger(), []);
  useEffect(() => {
    setLoaded(null);
    setLoadError(null);
    if (id == null) return;
    const coalescer = createRefreshCoalescer(load, 1000);
    refreshRef.current = coalescer;
    coalescer.trigger();
    const refreshVisible = () => { if (!document.hidden) coalescer.trigger(); };
    const interval = setInterval(refreshVisible, DETAIL_REFRESH_MS);
    document.addEventListener("visibilitychange", refreshVisible);
    return () => {
      clearInterval(interval);
      document.removeEventListener("visibilitychange", refreshVisible);
      refreshRef.current = null;
      coalescer.dispose();
    };
  }, [id, load]);
  useEvents("instance", refresh);
  useEvents("deployment", refresh);
  useEvents("modelfile", refresh);
  useEventPayload("node", (event) => { if (event.reason?.startsWith("state:")) refresh(); });
  const instanceCount = dep?.instances?.length ?? 0;
  const instanceDetailsLabel = instanceCount ? t("detail.instances", { count: instanceCount }) : t("detail.instanceDetails");

  return (
    <DetailDrawer
      width="min(832px, calc(100vw - 32px))"
      title={dep?.name || t("detail.title")}
      className="deployment-detail-drawer"
      deferEscapeToModal
      open={id != null}
      onClose={onClose}
      extra={dep && (
        <DeploymentDetailActions key={dep.id} dep={dep} actions={actionsFor(dep)} onChanged={load} />
      )}
    >
      {loadError && (
        <Alert type="error" showIcon message={tCommon("dataTable.loadFailed")}
          description={apiErrorText(tCommon, loadError)}
          action={<Button size="small" onClick={refresh}>{tCommon("actions.retry")}</Button>} />
      )}
      {!dep && !loadError && <Skeleton className="h-20 w-full" />}
      {dep && (
        <div className="deployment-detail-content">
          <MetricWindowProvider key={dep.id}>
            <Tabs className="deployment-inspection-tabs deployment-detail-tabs" value={detailTab} onValueChange={setDetailTab} variant="underline">
              <TabsList className="deployment-top-tabs m-0 max-w-full" aria-label={t("detail.title")}>
                <TabItem value="overview" label={t("detail.title")} />
                <TabItem value="instances" label={instanceDetailsLabel} />
              </TabsList>
              <TabPanel value="overview" className="pt-3">
                <DeploymentDetailOverview dep={dep} onChanged={load} routingPolicies={routingPolicies} routingOptions={routingOptions} />
                <section className="deployment-metrics-section" aria-label={t("detail.metrics")}>
                  <h4 className="deployment-section-title">{t("detail.metrics")}</h4>
                  <DeploymentMetricsPanel instances={dep.instances ?? []} deploymentId={dep.id} onSelectInstance={inspectInstance} />
                </section>
              </TabPanel>
              <TabPanel value="instances" keepMounted={visitedInstances === dep.id}>
                <DeploymentInstances key={`${dep.id}:${focusInstance ?? ""}:${drilldown?.visit ?? 0}`}
                  dep={dep} focus={drilldown?.uuid ?? focusInstance} focusStartup={drilldown?.target === "startup"} initialView={drilldown?.target === "metrics" ? "metrics" : undefined} onChanged={load} />
              </TabPanel>
            </Tabs>
          </MetricWindowProvider>
        </div>
      )}
    </DetailDrawer>
  );
}

// runtimeNodesLabel names the nodes a deployment's replicas run on now. One
// node prints its name. Several fold to the first and how many nodes there are
// in all - 「198.51.100.31 等 2 台」, "198.51.100.31 and 1 more" - so the cell stays
// one line and still says the deployment is not on one machine. The full set is
// in the cell's tooltip and in the drawer, so a spread deployment can always be
// expanded; it is never printed as a single node.
//
// nodes comes from runtime_nodes, which the server derives from the instance
// rows. Nothing here falls back to the placement target: naming a machine the
// replicas are not on is exactly the bug (GWKS-120).
export function runtimeNodesLabel(
  nodes: DeploymentRuntimeNode[] | undefined,
  folded: (others: number, total: number) => string,
): string {
  if (!nodes?.length) return "—";
  const name = (node: DeploymentRuntimeNode) => node.node_name || node.node_uuid.slice(0, 8);
  const [first, ...rest] = nodes;
  return rest.length ? `${name(first)} ${folded(rest.length, nodes.length)}` : name(first);
}

// The target nodes a deployment was asked to run on: node_names rank by rank
// for a multi-node one, else its single named node. This is the placement
// target, and the drawer labels it as such rather than sharing 「计算节点」 with
// where the replicas run.
export function deploymentTargetLabel(d: Pick<Deployment, "node_name" | "node_names">, leaderUUID?: string): string {
  if ((d.node_names?.length ?? 0) > 1) return d.node_names!.join(", ");
  if ((d.node_names?.length ?? 0) === 1) return d.node_names![0];
  return d.node_name || (leaderUUID ? leaderUUID.slice(0, 8) : "—");
}

// DeploymentRuntimeNodes is the 「计算节点」 cell: the distinct nodes the
// instances are on now, folded to one line, with every node and its instance
// count in the tooltip and the whole list in the drawer.
//
// One node is printed as a link to its page. Several are printed as the folded
// text "198.51.100.31 等 2 台" ("198.51.100.31 and 1 more") inside the tooltip that
// spells out the rest, because a row of links per node would wrap the cell and
// bury the state beside it; the drawer lists one link per node.
function DeploymentRuntimeNodes({ dep }: { dep: Pick<Deployment, "runtime_nodes"> }) {
  const { t } = useTranslation("deployments");
  const nodes = dep.runtime_nodes ?? [];
  if (!nodes.length) {
    // No instance exists, so no node hosts a replica - true for a deployment
    // that was never placed and for one that has stopped. Saying so is truer
    // than printing the node that was asked for.
    return <Typography.Text type="secondary">{t("page.runtimeNodes.none")}</Typography.Text>;
  }
  const name = (node: DeploymentRuntimeNode) => node.node_name || node.node_uuid.slice(0, 8);
  if (nodes.length === 1) {
    return <Link to="/nodes" state={{ nodeUUID: nodes[0].node_uuid }}>{name(nodes[0])}</Link>;
  }
  const folded = runtimeNodesLabel(nodes, (others, total) => t("page.runtimeNodes.others", { count: others, total }));
  // The whole set in one string, so hover and a screen reader both learn every
  // node rather than only the first the cell has room for.
  const all = nodes.map((node) => `${name(node)} (${t("page.runtimeNodes.replicas", { count: node.instances })})`).join(", ");
  return (
    <Tooltip title={all}>
      <span aria-label={all}>{folded}</span>
    </Tooltip>
  );
}

// DeploymentTargetNodes is the optional 「目标节点」 cell: where the deployment
// was asked to run, which is node_name/node_names and can differ from the node
// its replicas actually run on.
function DeploymentTargetNodes({ dep }: { dep: Pick<Deployment, "node_uuid" | "node_name" | "node_names"> }) {
  return <>{deploymentTargetLabel(dep, dep.node_uuid)}</>;
}

// The copied request names the gateway's key header: every /v1 route needs
// an API key, and a curl without one reads as if none were required.
export function deploymentCurl(origin: string, dep: Pick<DeploymentListItem, "served_name" | "model_name">): string {
  return `curl '${origin}/v1/chat/completions' -H 'Content-Type: application/json' -H "Authorization: Bearer <key>" -d '{"model":"${dep.served_name || dep.model_name}","messages":[{"role":"user","content":"hello"}]}'`;
}
