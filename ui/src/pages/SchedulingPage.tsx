import { useDeferredValue, useMemo, useRef, useState } from "react";
import { Alert, Button, Card, Spin, message } from "antd";
import { Tabs, TabsList, TabItem, TabPanel } from "../components/ui/tabs";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../api";
import { schedulingApi, type BoardWaitingWake } from "../schedulingApi";
import { apiErrorText } from "../apiError";
import EmptyState from "../components/EmptyState";
import PageHeader from "../components/PageHeader";
import { PendingCard, UpcomingCard, WaitingWakesCard } from "./scheduling/BoardTables";
import Heatmap from "./scheduling/HeatmapGrid";
import ActivityCard from "./scheduling/ActivityCard";
import HistoryCard from "./scheduling/HistoryCard";
import NodeDrawer from "./scheduling/NodeDrawer";
import { useNodeParam } from "./scheduling/NodeLink";
import OverviewFilters, { DEFAULT_VIEW, type ViewState } from "./scheduling/OverviewFilters";
import SharingStats from "./scheduling/SharingStats";
import SummaryStrip from "./scheduling/SummaryStrip";
import { arrange, buildModel, type NodeView } from "./scheduling/overviewModel";

const POLL_MS = 10_000;

// SchedulingPage is the scheduling center: the whole fleet's GPUs as a heatmap
// with the fleet's summary above it, what waits to be placed or woken, what the
// schedules do next, the stats of the deployments that share cards and, as the
// scheduling history, what the platform and the operators did to the
// deployments. A node opens in a drawer for its cards in detail, from the
// heatmap or any node named on the page. Below the heatmap, the scheduling
// activity over a range and the cards that changed hands most.
export default function SchedulingPage() {
  const { t } = useTranslation("scheduling");
  const { t: tCommon } = useTranslation("common");
  const overview = usePolling(() => schedulingApi.overview(), POLL_MS);
  const queues = usePolling(() => schedulingApi.board({ queues: true }), POLL_MS);
  const [view, setView] = useState<ViewState>(DEFAULT_VIEW);
  const tabsRef = useRef<HTMLDivElement>(null);
  const [tabsMinHeight, setTabsMinHeight] = useState(0);
  const preserveTabScroll = () => {
    const tabs = tabsRef.current;
    if (!tabs) return;
    const visibleHeight = document.documentElement.clientHeight - tabs.getBoundingClientRect().top;
    setTabsMinHeight(window.scrollY > 0 ? Math.max(0, Math.ceil(visibleHeight)) : 0);
  };
  // The open node lives in the URL (?node=<uuid>), so a link anywhere on the
  // page opens it, the browser goes back from it, and it can be shared.
  const { node: openUUID, openNode, closeNode } = useNodeParam();
  const [cancelling, setCancelling] = useState<number | null>(null);

  // A poll that finds the fleet unchanged hands the grid the same node views.
  const seen = useRef<Map<string, { signature: string; view: NodeView }>>(undefined);
  const built = useMemo(() => {
    if (!overview.data) return null;
    const result = buildModel(overview.data, seen.current);
    seen.current = result.seen;
    return result.model;
  }, [overview.data]);

  // Typing a deployment name must not hold up the keystroke while thousands of cells restyle.
  const search = useDeferredValue(view.filters.search);
  const arrangement = useMemo(
    () => (built ? arrange(built, { ...view.filters, search }, view.groupBy, view.sortBy) : null),
    [built, view.filters, search, view.groupBy, view.sortBy],
  );
  const openName = useMemo(
    () => (openUUID ? (built?.nodes.find((candidate) => candidate.uuid === openUUID)?.name ?? openUUID) : ""),
    [built, openUUID],
  );

  const cancelWake = async (row: BoardWaitingWake) => {
    setCancelling(row.deployment_id);
    try {
      await api.endWakeWaiting(row.deployment_id);
      message.success(t("waiting.cancelled", { name: row.deployment }));
      await Promise.all([queues.refetch(), overview.refetch()]);
    } catch (e) {
      message.error(t("waiting.cancelFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setCancelling(null);
    }
  };

  const header = <PageHeader title={t("page.title")} description={t("page.description")} />;
  const data = overview.data;
  if (!data && overview.error) {
    return (
      <>
        {header}
        <Alert
          type="error"
          showIcon
          message={t("page.loadFailed")}
          description={overview.error.message}
          action={<Button onClick={() => void overview.refetch()}>{t("page.retry")}</Button>}
        />
      </>
    );
  }
  if (!data || !built || !arrangement) {
    return (
      <>
        {header}
        <div role="status" className="scheduling-loading"><Spin /> <span>{t("page.loading")}</span></div>
      </>
    );
  }

  const board = queues.data;
  const searching = view.filters.search.trim() !== "";
  return (
    <>
      {header}
      {overview.error && (
        <Alert
          style={{ marginBottom: 16 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: overview.error.message })}
          action={<Button size="small" onClick={() => void overview.refetch()}>{t("page.retry")}</Button>}
        />
      )}
      <QueueCards board={board} error={queues.error} heldRequests={data.summary.contention.held_requests} onRetry={() => void queues.refetch()} cancelling={cancelling} onCancel={(row) => void cancelWake(row)} />
      <Card className="scheduling-card" title={t("heatmap.title")}>
        <SummaryStrip summary={data.summary} searching={searching} />
        {data.summary.cards === 0 ? (
          <EmptyState kind="nodes" compact description={t("nodes.empty")} />
        ) : (
          <>
            <OverviewFilters model={built} view={view} onChange={setView} />
            <p className="hm-status" role="status">
              {t("heatmap.shown", { nodes: arrangement.nodes, cards: arrangement.cards })}
              {searching && ` · ${arrangement.matched.size === 0 ? t("heatmap.noDeployment") : t("heatmap.highlighted", { cards: arrangement.highlighted })}`}
            </p>
            {arrangement.cards === 0 ? (
              <EmptyState kind="nodes" compact description={t("heatmap.emptyFiltered")} />
            ) : (
              <Heatmap arrangement={arrangement} model={built} groupBy={view.groupBy} onOpen={openNode} />
            )}
          </>
        )}
      </Card>
      <Tabs ref={tabsRef} defaultValue="overview" variant="underline" onValueChange={preserveTabScroll} style={{ minHeight: tabsMinHeight }}>
        <TabsList>
          <TabItem value="overview" label={t("page.activityAndSharing")} />
          <TabItem value="history" label={t("history.title")} />
        </TabsList>
        <TabPanel value="overview" className="pt-4"><ActivityCard/><SharingStats/></TabPanel>
        <TabPanel value="history" className="pt-4"><HistoryCard/></TabPanel>
      </Tabs>
      <NodeDrawer uuid={openUUID} name={openName} onClose={closeNode} />
    </>
  );
}

function QueueCards({ board, error, heldRequests, onRetry, cancelling, onCancel }: {
  board: Awaited<ReturnType<typeof schedulingApi.board>> | null;
  error: Error | null;
  heldRequests: number;
  onRetry: () => void;
  cancelling: number | null;
  onCancel: (row: BoardWaitingWake) => void;
}) {
  const { t } = useTranslation("scheduling");
  if (!board) {
    return error ? (
      <Alert
        style={{ marginBottom: 16 }}
        type="error"
        showIcon
        message={t("queues.loadFailed")}
        description={error.message}
        action={<Button onClick={onRetry}>{t("page.retry")}</Button>}
      />
    ) : (
      <div role="status" className="scheduling-loading"><Spin /> <span>{t("queues.loading")}</span></div>
    );
  }
  return (
    <>
      {error && (
        <Alert
          style={{ marginBottom: 16 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: error.message })}
          action={<Button size="small" onClick={onRetry}>{t("page.retry")}</Button>}
        />
      )}
      <div className="scheduling-queue-groups">
        <Card size="small" styles={{ body: { padding: 0 } }} title={t("queues.currentWaiting")}>
          <div className="scheduling-wait-columns">
            <PendingCard rows={board.pending} />
            <WaitingWakesCard rows={board.waiting_wakes} heldRequests={heldRequests} busyID={cancelling} onCancel={onCancel} />
          </div>
        </Card>
        <Card size="small" styles={{ body: { padding: 0 } }} title={t("summary.upcoming")}>
          <UpcomingCard rows={board.upcoming} />
        </Card>
      </div>
    </>
  );
}
