import { useEffect, useRef, useState } from "react";
import { Alert, Button, Card, Dropdown, Input, Modal, Pagination, Segmented, Select, Space, Tooltip, Typography, message } from "antd";
import { FilterChips } from "../ui-kit/FilterChips";
import {
  AppstoreOutlined,
  BarsOutlined,
  CloudDownloadOutlined,
  CopyOutlined,
  DeleteOutlined,
  FolderAddOutlined,
  MoreOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
} from "@ant-design/icons";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling, type Model, type ModelCore, type ModelListItem } from "../api";
import { usePagedList } from "../pagedList";
import AIAssistant from "../components/AIAssistant";
import ResourceTable, { storedPageSize, type ResourceColumn, type ServerSort } from "../components/resourceTable/ResourceTable";
import { PAGE_SIZE_OPTIONS, saveStoredPageSize } from "../components/resourceTable/columnPrefs";
import CellTags from "../components/resourceTable/CellTags";
import EmptyState from "../components/EmptyState";
import PageHeader from "../components/PageHeader";
import {
  DownloadModal,
  Modalities,
  ModelDetailModal,
  ModelLogo,
	ModelSourceTag,
  ModelTile,
  modelDate,
  modelPublishedAt,
  RegisterPathModal,
} from "./ModelsPageParts";
import { ModelCopyModal } from "./models/ModelCopyModal";
import { ModelPresenceSummary } from "./models/ModelPresenceSummary";
import { RegisterModal } from "./models/RegisterModelModal";
import { allOperationsLabelKey, operationLabelKey, operationTypes, operationTypesOf } from "../modelInference";
import type { OperationType } from "../inferenceApi";

const modelOperationOptions: Array<{ value: "all" | OperationType; labelKey: string }> = [
  { value: "all", labelKey: allOperationsLabelKey },
  ...operationTypes.map((value) => ({ value, labelKey: operationLabelKey[value] })),
];

const MODEL_TABLE_ID = "models";

export default function ModelsPage() {
  const { t } = useTranslation("models");
  const { t: tCommon } = useTranslation("common");
  const [searchParams, setSearchParams] = useSearchParams();
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [source, setSource] = useState("all");
  const [modelOperation, setModelOperation] = useState<"all" | OperationType>("all");
  const [sort, setSort] = useState<ServerSort | null>(null);
  // The search follows typing, settled for a moment before the server is asked.
  useEffect(() => {
    const timer = setTimeout(() => setSearch(query.trim()), 300);
    return () => clearTimeout(timer);
  }, [query]);
  // The server pages, searches, filters and sorts; a row carries the presence
  // summary, and GET /models/{id} the per-location detail.
  const { data, refetch, refresh, loading, error, pagination } = usePagedList(api.modelsPage, {
    filters: {
      q: search,
      extra: { source: source === "all" ? [] : [source], operation: modelOperation === "all" ? [] : [modelOperation] },
      ...(sort ? { sort: `${sort.order === "desc" ? "-" : ""}${sort.key}` } : {}),
    },
    pageSize: storedPageSize(MODEL_TABLE_ID, 20),
    intervalMs: 30000,
  });
  const detailID = Number(searchParams.get("detail"));
  const openID = Number.isInteger(detailID) && detailID > 0 ? detailID : 0;
  const detail = usePolling<Model | null>(() => (openID ? api.model(openID) : Promise.resolve(null)), openID ? 15000 : 0, [openID]);
  const modelfileRefreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const scheduleModelfileRefresh = () => {
    // Downloads can emit progress events several times per second. Coalesce
    // them into a background refresh so the page remains usable and does not
    // repeatedly enter the full-page loading state.
    if (modelfileRefreshTimer.current != null) return;
    modelfileRefreshTimer.current = setTimeout(() => {
      modelfileRefreshTimer.current = null;
      void refresh();
      if (openID) void detail.refresh();
    }, 500);
  };
  useEvents("modelfile", scheduleModelfileRefresh);
  useEffect(() => () => {
    if (modelfileRefreshTimer.current != null) clearTimeout(modelfileRefreshTimer.current);
  }, []);
  useEvents("node", () => {
    void refresh();
  });

  const [registerOpen, setRegisterOpen] = useState(false);
  const [downloadFor, setDownloadFor] = useState<Model | null>(null);
  const [pathFor, setPathFor] = useState<Model | null>(null);
  const [deleting, setDeleting] = useState<ModelCore | null>(null);
  const [focusFiles, setFocusFiles] = useState(false);
  const [copyFor, setCopyFor] = useState<Model | null>(null);
  const [view, setView] = useState<"grid" | "list">(() => localStorage.getItem("gpuworks-model-view") === "list" ? "list" : "grid");
	const removeModel = async (model: ModelCore) => {
		try {
			await api.deleteModel(model.id);
			message.success(t("message.deleted"));
			// Deleting the page's last row empties it; step back one page instead
			// of refetching an empty page above a non-empty list.
			if (rows.length === 1 && pagination.current > 1) {
				pagination.onChange(pagination.current - 1, pagination.pageSize);
			} else {
				refetch();
			}
		} catch (e) {
			message.error(t("message.deleteFailed", { detail: (e as Error).message }));
		}
	};
  // A list row has no per-location presence; the download, copy and
  // register-path dialogs choose among locations, so they open on the full model.
  const withModel = (row: ModelCore, open: (model: Model) => void) => {
    api.model(row.id).then(open).catch((e: Error) => message.error(e.message));
  };

  const rows = data?.items ?? [];
  const summary = data?.summary;
  const filtering = search !== "" || source !== "all" || modelOperation !== "all";
  const openDetail = (model: ModelCore) => {
    const next = new URLSearchParams(searchParams);
    next.set("detail", String(model.id));
    setSearchParams(next);
  };
  const closeDetail = () => {
    setFocusFiles(false);
    const next = new URLSearchParams(searchParams);
    next.delete("detail");
    setSearchParams(next, { replace: true });
  };

  const changeView = (value: string | number) => {
    const next = value === "list" ? "list" : "grid";
    setView(next);
    localStorage.setItem("gpuworks-model-view", next);
  };

  const openFiles = (model: ModelCore) => {
    setFocusFiles(true);
    openDetail(model);
  };
  const columns: ResourceColumn<ModelListItem>[] = [
    {
      key: "name",
      role: "name",
      title: t("column.name"),
      width: 240,
      serverSort: true,
      render: (_: unknown, model: ModelListItem) => <span className="model-name-cell"><ModelLogo model={model} size="small" /><Button type="link" className="model-name-link" onClick={() => openDetail(model)}>{model.name || model.source_id || "—"}</Button></span>,
    },
    { key: "source", title: t("column.source"), width: 130, serverSort: true, render: (_: unknown, model: ModelListItem) => <ModelSourceTag source={model.source} /> },
    {
      key: "presence",
      title: t("column.nodePresence"),
      width: 260,
      serverSort: true,
      render: (_: unknown, m: ModelListItem) => <ModelPresenceSummary presence={m.presence} onOpen={() => openFiles(m)} />,
    },
    { key: "weight", title: t("column.weight"), width: 110, align: "right", serverSort: true, render: (_: unknown, m: ModelListItem) => (m.weight_gib ? `${m.weight_gib.toFixed(1)} GiB` : "—") },
    { key: "source_id", title: t("column.sourceID"), width: 240, serverSort: true, render: (_: unknown, model: ModelListItem) => model.source_id },
    {
      key: "published_at",
      title: t("column.publishedAt"),
      width: 140,
      serverSort: true,
      render: (_: unknown, model: ModelListItem) => <Tooltip title={<>{t("detail.originCreatedAt")}: {modelDate(modelPublishedAt(model))}<br />{t("detail.originUpdatedAt")}: {modelDate(model.origin_updated_at)}</>}><span>{modelDate(modelPublishedAt(model))}</span></Tooltip>,
    },
    { key: "created_at", title: t("column.addedAt"), width: 140, serverSort: true, render: (_: unknown, model: ModelListItem) => modelDate(model.created_at) },
    {
      key: "operations",
      title: t("column.operations"),
      width: 200,
      render: (_: unknown, m: ModelListItem) => <CellTags tags={operationTypesOf(m.operations).map((type) => ({ key: type, text: t(operationLabelKey[type]), color: "blue" }))} />,
    },
    {
      key: "input_modalities",
      title: t("column.inputModalities"),
      width: 160,
      defaultHidden: true,
      render: (_: unknown, m: ModelListItem) => <Modalities values={m.input_modalities} />,
    },
    {
      key: "action",
      role: "actions",
      title: t("column.actions"),
      width: 150,
      render: (_: unknown, m: ModelListItem) => (
        <Space size={2} className="model-row-actions">
          {m.source !== "local" && (
            <Tooltip title={t("action.downloadFromSource")}>
              <Button type="text" size="small" icon={<CloudDownloadOutlined />} onClick={() => withModel(m, setDownloadFor)} aria-label={t("action.downloadToNode")} />
            </Tooltip>
          )}
          <Tooltip title={t("action.copyTo")}>
            <Button type="text" size="small" icon={<CopyOutlined />} onClick={() => withModel(m, setCopyFor)} aria-label={t("action.copyTo")} />
          </Tooltip>
          <AIAssistant
            task="model.fit"
            subject={{ kind: "model", id: m.id, label: m.name || m.source_id }}
            buttonText=""
            buttonType="link"
            buttonSize="small"
          />
          <Dropdown
            trigger={["click"]}
            menu={{
              items: [
                { key: "path", icon: <FolderAddOutlined />, label: t("action.registerExistingPath") },
                { type: "divider" },
                { key: "delete", icon: <DeleteOutlined />, danger: true, label: t("action.deleteModel") },
              ],
              onClick: ({ key }) => {
                if (key === "path") withModel(m, setPathFor);
                if (key === "delete") setDeleting(m);
              },
            }}
          >
            <Tooltip title={t("action.more")}>
              <Button type="text" size="small" icon={<MoreOutlined />} aria-label={t("action.more")} />
            </Tooltip>
          </Dropdown>
        </Space>
      ),
    },
  ];
  // Search and source sit above the rows, where every list keeps them; the
  // card view has the same bar without the column settings.
  const filterBar = (
    <>
      <div className="model-resource-filters" role="search" aria-label={t("filter.aria")}>
        <Input
          id="model-search"
          name="model-search"
          allowClear
          prefix={<SearchOutlined />}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          placeholder={t("filter.searchPlaceholder")}
          aria-label={t("filter.searchAria")}
        />
        <Select
          value={source}
          onChange={setSource}
          aria-label={t("filter.sourceAria")}
          options={[
            { value: "all", label: t("filter.allSources") },
            { value: "huggingface", label: "HuggingFace" },
            { value: "modelscope", label: "ModelScope" },
            { value: "url", label: t("source.url") },
            { value: "local", label: t("source.local") },
          ]}
        />
      </div>
      {filtering && data && (
        <Typography.Text type="secondary" className="model-filter-result">{t("summary.filteredCount", { count: data.total })}</Typography.Text>
      )}
    </>
  );

  return (
    <div>
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={
          <Segmented
            aria-label={t("view.aria")}
            value={view}
            onChange={changeView}
            options={[
              { value: "grid", label: <Tooltip title={t("view.grid")}><AppstoreOutlined aria-label={t("view.grid")} /></Tooltip> },
              { value: "list", label: <Tooltip title={t("view.list")}><BarsOutlined aria-label={t("view.list")} /></Tooltip> },
            ]}
          />
        }
      />
      <FilterChips className="model-type-filter" label={t("filter.operationAria")} value={modelOperation} onChange={setModelOperation}
        items={modelOperationOptions.map((option) => ({ value: option.value, label: t(option.labelKey) }))} />
      <Card className="model-resource-card">
        <div className="model-content-toolbar">
          <div className="model-toolbar-actions">
            <Tooltip title={t("action.refresh")}>
              <Button icon={<ReloadOutlined />} loading={loading} onClick={() => { refetch(); if (openID) void detail.refresh(); }} aria-label={t("action.refresh")} />
            </Tooltip>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => setRegisterOpen(true)}>{t("action.register")}</Button>
          </div>
          <div className="model-summary-strip model-summary-inline" aria-label={t("summary.aria")}>
            <div className="model-summary-item"><span>{t("summary.registered")}</span><strong>{summary?.registered ?? "—"}</strong></div>
            <div className="model-summary-item"><span>{t("files.count")}</span><strong>{summary?.ready_files ?? "—"}</strong></div>
            <div className="model-summary-item"><span>{t("summary.sources")}</span><strong>{summary?.sources ?? "—"}</strong></div>
            <div className="model-summary-item"><span>{t("summary.totalWeight")}</span><strong>{summary?.weight_gib ? `${summary.weight_gib.toFixed(1)} GiB` : "—"}</strong></div>
          </div>
        </div>
        {error ? (
          <Alert type="error" showIcon message={t("error.loadFailed")} description={error.message} action={<Button onClick={refetch}>{t("action.retry")}</Button>} />
        ) : view === "grid" ? (
          <div className="model-grid-region" aria-live="polite">
            <div className="resource-table-toolbar"><div className="resource-table-toolbar-left">{filterBar}</div></div>
            {rows.length > 0 ? (
              <>
                <div className="model-tile-grid">
                  {rows.map((model) => (
                    <ModelTile
                      key={model.id}
                      model={model}
                      onOpen={() => openDetail(model)}
                      onDownload={() => withModel(model, setDownloadFor)}
                      onCopy={() => withModel(model, setCopyFor)}
                      onRegisterPath={() => withModel(model, setPathFor)}
                      onDelete={() => removeModel(model)}
                    />
                  ))}
                </div>
                <Pagination
                  className="model-grid-pagination"
                  {...pagination}
                  pageSizeOptions={PAGE_SIZE_OPTIONS}
                  onChange={(page, size) => {
                    // The views share one page size, remembered as the table's.
                    if (size !== pagination.pageSize) saveStoredPageSize(MODEL_TABLE_ID, size);
                    pagination.onChange(page, size);
                  }}
                  showTotal={(total) => tCommon("resourceTable.total", { count: total })}
                />
              </>
            ) : loading && !data ? null : (
              <EmptyState
                kind="models"
                description={t(filtering ? "empty.filtered" : "empty.all")}
                action={filtering ? undefined : <Button type="primary" onClick={() => setRegisterOpen(true)}>{t("action.register")}</Button>}
              />
            )}
          </div>
        ) : (
          <ResourceTable<ModelListItem>
            tableId={MODEL_TABLE_ID}
            rowKey="id"
            columns={columns}
            data={rows}
            loading={loading}
            error={error}
            pagination={pagination}
            sort={sort}
            onSortChange={setSort}
            toolbar={filterBar}
            emptyDescription={t(filtering ? "empty.filtered" : "empty.all")}
            emptyKind="models"
            emptyAction={filtering ? undefined : <Button type="primary" onClick={() => setRegisterOpen(true)}>{t("action.register")}</Button>}
          />
        )}
        <ModelDetailModal
          model={openID ? detail.data : null}
          onClose={closeDetail}
          focusFiles={focusFiles}
          onDownload={(model) => { closeDetail(); setDownloadFor(model); }}
          onCopy={(model) => { closeDetail(); setCopyFor(model); }}
          onRegisterPath={(model) => { closeDetail(); setPathFor(model); }}
          onChanged={() => {
            refetch();
            void detail.refresh();
          }}
        />
        <RegisterModal
			open={registerOpen}
			onClose={() => setRegisterOpen(false)}
			onDone={() => {
			  setRegisterOpen(false);
			  refetch();
			}}
		  />
	      <DownloadModal
        model={downloadFor}
        onClose={() => setDownloadFor(null)}
        onDone={() => {
          setDownloadFor(null);
          refetch();
        }}
	      />
			<ModelCopyModal
				model={copyFor}
				onClose={() => setCopyFor(null)}
				onDone={() => {
					setCopyFor(null);
					refetch();
				}}
			/>
        <Modal
          open={!!deleting}
          title={t("delete.confirm", { name: deleting?.name || deleting?.source_id || "" })}
          okText={t("action.delete")}
          cancelText={t("action.cancel")}
          okButtonProps={{ danger: true }}
          onOk={() => deleting && removeModel(deleting).finally(() => setDeleting(null))}
          onCancel={() => setDeleting(null)}
        >
          {t("delete.listDescription")}
        </Modal>
        <RegisterPathModal
        model={pathFor}
        onClose={() => setPathFor(null)}
        onDone={() => {
          setPathFor(null);
          refetch();
        }}
      />
      </Card>
    </div>
  );
}
