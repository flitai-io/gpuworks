import { useEffect, useRef, useState } from "react";
import { Button } from "../components/ui/button";
import { Tooltip } from "../components/ui/tooltip";
import { PageLayout, PageContent, PageBody } from "../components/ui/page-layout";
import { fromAntd } from "../ui-kit/antdIcon";
import { message } from "../ui-kit/feedback";
import { PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";
import { useEvents } from "../api";
import { BATCH_POLL_MS, batchApi, isBatchActive, type Batch, type BatchStatus } from "../batchApi";
import { useIsAdmin } from "../components/useIsAdmin";
import PageHeader from "../components/PageHeader";
import ResourceTable, { storedPageSize, type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { formatTime } from "../i18n/datetime";
import { usePagedList } from "../pagedList";
import BatchCreateModal from "./batches/BatchCreateModal";
import BatchAPIUsage from "./batches/BatchAPIUsage";
import BatchDetailDrawer from "./batches/BatchDetailDrawer";
import { batchWarningText } from "./batches/batchErrors";
import { BatchProgress, BatchStatusTag } from "./batches/batchDisplay";
import { useBatchPolling } from "./batches/useBatchPolling";

const TABLE_ID = "batches";

// BatchesPage lists the Batch API jobs, newest first: how far each has got,
// when it was made and when it expires. A row opens the detail drawer. The
// list refreshes itself while any batch on the page is still moving, and
// stops once they have all ended.
// pollMs is the refresh cadence while a batch is moving; only tests change it.
export default function BatchesPage({ pollMs = BATCH_POLL_MS }: { pollMs?: number }) {
  const { t } = useTranslation("batches");
  const isAdmin = useIsAdmin();
  const [searchParams, setSearchParams] = useSearchParams();
  const [status, setStatus] = useState<string[]>([]);
  const [createOpen, setCreateOpen] = useState(false);
  const detailID = searchParams.get("detail");
  // The statuses to filter by are the server's; until they are read the filter offers none.
  const [statuses, setStatuses] = useState<BatchStatus[]>([]);
  useEffect(() => {
    let live = true;
    batchApi.dictionary().then((dictionary) => { if (live) setStatuses(dictionary.statuses); }).catch(() => { /* the list still works unfiltered */ });
    return () => { live = false; };
  }, []);

  const list = usePagedList(batchApi.list, {
    filters: { state: status },
    pageSize: storedPageSize(TABLE_ID, 20),
  });
  const rows = list.data?.items ?? [];
  const refresh = () => { void list.refresh(); };
  useBatchPolling(rows.some((row) => isBatchActive(row.status)), refresh, pollMs);
  // Status changes arrive on the event stream; polling above is the baseline.
  useEvents("batch", refresh);
  // A returning tab should not wait for the next tick.
  const latestRefresh = useRef(refresh);
  latestRefresh.current = refresh;
  useEffect(() => {
    const onVisible = () => { if (!document.hidden) latestRefresh.current(); };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, []);

  const openDetail = (id: string) => {
    const next = new URLSearchParams(searchParams);
    next.set("detail", id);
    setSearchParams(next);
  };
  const closeDetail = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("detail");
    setSearchParams(next, { replace: true });
  };
  const created = (batch: Batch) => {
    setCreateOpen(false);
    message.success(t("create.created", { id: batch.id }));
    // A finding that did not refuse the batch, e.g. its model sleeps by an operator's order.
    for (const warning of batch.warnings ?? []) {
      message.warning(batchWarningText(t, warning));
    }
    void list.refetch();
    openDetail(batch.id);
  };

  // Together the widths fit the 1440 panel; a narrower screen scrolls the table.
  const columns: ResourceColumn<Batch>[] = [
    {
      key: "id", title: t("columns.id"), width: 210, role: "name",
      render: (_: unknown, batch) => (
        <Button variant="ghost" style={{ padding: 0 }} onClick={() => openDetail(batch.id)} aria-label={t("action.open", { id: batch.id })}>{batch.id}</Button>
      ),
    },
    {
      key: "status", title: t("columns.status"), width: 110,
      filters: statuses.map((value) => ({ value, text: t(`status.${value}`) })), filteredValue: status.length ? status : null,
      render: (_: unknown, batch) => <BatchStatusTag status={batch.status} />,
    },
    { key: "endpoint", title: t("columns.endpoint"), width: 160, render: (_: unknown, batch) => <code>{batch.endpoint}</code> },
    {
      key: "model", title: t("columns.model"), width: 160,
      render: (_: unknown, batch) => (batch.model ? <code>{batch.model}</code> : <span className="text-fg-muted">{t("columns.modelPerLine")}</span>),
    },
    { key: "progress", title: t("columns.progress"), width: 190, render: (_: unknown, batch) => <BatchProgress batch={batch} /> },
    { key: "created_at", title: t("columns.createdAt"), width: 150, render: (_: unknown, batch) => formatTime(batch.created_at) },
    { key: "expires_at", title: t("columns.expiresAt"), width: 150, render: (_: unknown, batch) => formatTime(batch.expires_at) },
  ];

  const filtering = status.length > 0;
  const createAction = <Button variant="primary" leadingIcon={fromAntd(PlusOutlined)} disabled={!isAdmin} onClick={() => setCreateOpen(true)}>{t("action.create")}</Button>;
  // Only a non-admin gets a reason; an admin hovering the button sees no empty bubble.
  const createButton = isAdmin ? createAction : (
    <Tooltip content={t("action.createAdminOnly")}><span>{createAction}</span></Tooltip>
  );

  return (
    <PageLayout gutter="none" className="h-auto gap-0">
      <PageHeader title={t("page.title")} description={t("page.description")} extra={createButton} />
      <PageContent><PageBody>
      <BatchAPIUsage />
      {/* The list sits in a bordered panel like the other list pages. */}
      <div className="rounded-xl border border-border-subtle bg-surface-floating p-4">
      <ResourceTable<Batch>
        tableId={TABLE_ID}
        rowKey="id"
        columns={columns}
        data={rows}
        loading={list.loading}
        error={list.error}
        onRefresh={() => void list.refetch()}
        updatedAt={list.updatedAt}
        pagination={list.pagination}
        onFiltersChange={(next) => setStatus(next.status ?? [])}
        emptyDescription={t(filtering ? "empty.filtered" : "empty.all")}
        emptyAction={filtering || !isAdmin ? undefined : <Button variant="primary" onClick={() => setCreateOpen(true)}>{t("action.create")}</Button>}
        onRow={(batch) => ({ className: "batch-row", onClick: () => openDetail(batch.id) })}
      />
      </div>
      </PageBody></PageContent>
      <BatchDetailDrawer id={detailID} canManage={isAdmin} pollMs={pollMs} onClose={closeDetail} onChanged={refresh} />
      <BatchCreateModal open={createOpen} onClose={() => setCreateOpen(false)} onCreated={created} />
    </PageLayout>
  );
}
