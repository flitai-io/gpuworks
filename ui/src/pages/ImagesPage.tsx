import { useEffect, useMemo, useRef, useState } from "react";
import { Form, message } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  useEvents,
  usePolling,
  type ImageArchivePreview,
  type ImageCatalogEntry,
  type List,
  type ImageCatalogSyncResult,
  type ImageInspection,
  type ImageNode,
  type ImageSeries,
} from "../api";
import { apiErrorText } from "../apiError";
import ImageImportDrawer from "./images/ImageImportDrawer";
import ImageDetailDrawer from "./images/ImageDetailDrawer";
import { buildVersionColumns } from "./images/ImageVersionColumns";
import {
  archiveURLHost,
  autoSourceValue,
  peerSources,
  transferRunning,
} from "./images/ImagePageHelpers";
import ImagesPageOverview, { type ImageSourceTab } from "./images/ImagesPageOverview";
import { createRefreshCoalescer } from "./evaluations/evaluationRefresh";

type FetchMode = "peer" | string;
function hasExplicitTag(ref: string) {
  const name = ref.slice(ref.lastIndexOf("/") + 1);
  return name.includes(":") && !ref.toLowerCase().includes("@sha256:");
}
export default function ImagesPage() {
  const { t } = useTranslation("images");
  const { t: tCommon } = useTranslation("common");
  const [importForm] = Form.useForm();
  const query = usePolling<List<ImageSeries>>(api.images, 10000);
  const { data, loading, error, refetch, updatedAt } = query;
  const imageRefreshRef = useRef(query.refresh);
  imageRefreshRef.current = query.refresh;
  const imageRefreshCoalescer = useMemo(
    () => createRefreshCoalescer(() => imageRefreshRef.current(), 1000),
    [],
  );
  useEffect(() => () => imageRefreshCoalescer.dispose(), [imageRefreshCoalescer]);
  useEvents("image", imageRefreshCoalescer.trigger);
  // The catalog-wide version list carries per-version counts only
  // (present_nodes / total_nodes), never node rows: those are read from the
  // series detail while its drawer is open.
  const versions = usePolling<List<ImageCatalogEntry>>(api.imageVersions, 10000);
  const [sourceTab, setSourceTab] = useState<
    ImageSourceTab
  >("official");
  const series = (data?.items ?? []).filter((row) => row.source === sourceTab);
  const allVersions = (versions.data?.items ?? []).filter(
    (row) => row.source === sourceTab,
  );
  // The open series. It starts as the list row (no versions) and is replaced
  // by GET /images/{series_id}, whose versions carry the node rows every node
  // action and the digest check read.
  const [detail, setDetail] = useState<ImageSeries | null>(null);
  const detailRef = useRef<ImageSeries | null>(null);
  detailRef.current = detail;
  const [selectedVersionID, setSelectedVersionID] = useState<number | null>(null);
  const [sourceByNode, setSourceByNode] = useState<Record<string, FetchMode>>(
    {},
  );
  const [importOpen, setImportOpen] = useState(false);
  const [importing, setImporting] = useState(false);
  const [sourceError, setSourceError] = useState<string | null>(null);
  const [inspecting, setInspecting] = useState<string | null>(null);
  // The last on-demand inspection, keyed to the version and node it read. It
  // is a runtime fact shown as read, never written back to the catalog.
  const [inspection, setInspection] = useState<{
    versionID: number;
    nodeUUID: string;
    result: ImageInspection;
  } | null>(null);
  const [catalogVersion, setCatalogVersion] = useState("");
  const [syncingCatalog, setSyncingCatalog] = useState(false);
  const [uploadingManifest, setUploadingManifest] = useState(false);
  const totalVersions =
    allVersions.length ||
    series.reduce((sum, item) => sum + item.version_count, 0);
  const distributed = allVersions.reduce(
    (sum, item) => sum + (item.present_nodes || 0),
    0,
  );
  const sizesAvailable = versions.data !== undefined && versions.data !== null && !versions.error;
  const catalogUnknown = allVersions.filter((item) => item.size_bytes == null).length;
  const catalogBytes = sizesAvailable && (allVersions.length === 0 || catalogUnknown < allVersions.length)
    ? allVersions.reduce((sum, item) => sum + (item.size_bytes ?? 0), 0)
    : undefined;
  const localKnown = allVersions.reduce((sum, item) => sum + (item.local_size_known_nodes ?? 0), 0);
  const localUnknown = distributed - localKnown;
  const localBytes = sizesAvailable && (distributed === 0 || localKnown > 0)
    ? allVersions.reduce((sum, item) => sum + (item.local_runtime_size_bytes ?? 0), 0)
    : undefined;
  // "Check for updates" and a manifest upload change the system catalog the
  // same way, so both report the same four counts.
  const applyCatalog = async (
    run: () => Promise<ImageCatalogSyncResult>,
    setBusy: (busy: boolean) => void,
  ) => {
    setBusy(true);
    try {
      const result = await run();
      setCatalogVersion(result.catalog_version || "");
      message.success(
        (["created", "updated", "deleted", "deprecated"] as const)
          .map((key) => t(`page.catalog.result.${key}`, { n: result[key] ?? 0 }))
          .join(" · "),
      );
      refetch();
      versions.refetch();
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("page.catalog.updateFailed"));
    } finally {
      setBusy(false);
    }
  };
  // refreshDetail reads the series detail again. An answer for a series the
  // drawer no longer shows is dropped; a background poll stays quiet on errors.
  const refreshDetail = async (id: number, quiet = false) => {
    if (!id) return;
    try {
      const next = await api.image(id);
      if (detailRef.current?.id === id) setDetail(next);
    } catch (e) {
      if (!quiet)
        message.error(e instanceof Error ? e.message : t("action.detailFailed"));
    }
  };
  const refreshDetailRef = useRef(refreshDetail);
  refreshDetailRef.current = refreshDetail;
  // A running transfer has to be visible while it runs, so the open detail is
  // polled faster while something is moving and at the calm interval when
  // nothing is.
  const detailID = detail?.id ?? 0;
  const detailMoving = transferRunning(detail?.versions);
  useEffect(() => {
    if (!detailID) return;
    const timer = setInterval(() => {
      if (!document.hidden) void refreshDetailRef.current(detailID, true);
    }, detailMoving ? 2000 : 10000);
    return () => clearInterval(timer);
  }, [detailID, detailMoving]);
  // nodeRow is a node's row of one version as the series detail reports it.
  const nodeRow = (version: ImageCatalogEntry, nodeUUID: string) =>
    detailRef.current?.versions
      ?.find((item) => item.id === version.id)
      ?.nodes?.find((node) => node.node_uuid === nodeUUID);
  const availablePeer = (version: ImageCatalogEntry, target: ImageNode) =>
    peerSources(version, target).length > 0;
  // A node pulls through the pull settings unless the operator chose another
  // way for it (a peer copy or an archive URL), each of which has its own dialog.
  const modeFor = (version: ImageCatalogEntry, target: ImageNode): FetchMode =>
    sourceByNode[`${version.id}:${target.node_uuid}`] || autoSourceValue;
  const setMode = (
    version: ImageCatalogEntry,
    nodeUUID: string,
    mode: FetchMode,
  ) =>
    setSourceByNode((current) => ({
      ...current,
      [`${version.id}:${nodeUUID}`]: mode,
    }));
  const pullVersion = async (
    version: ImageCatalogEntry,
    nodeUUID: string,
    retry = false,
    force = false,
  ) => {
    setSourceError(null);
    try {
      // None of these requests names a source: the platform decides through
      // the pull settings. Archive addresses and peer copies never come
      // through here, because both need choices made and checked before
      // anything moves, so the node table opens a dialog for each.
      if (force) {
        await api.syncImage(version.id, nodeUUID, undefined, { force: true });
        message.success(t("action.repullSubmitted"));
      } else {
        if (retry) await api.retryImage(version.id, nodeUUID);
        else await api.syncImage(version.id, nodeUUID);
        message.success(t("action.pullSubmitted"));
      }
    } catch (e) {
      const msg = apiErrorText(tCommon, e) || t("action.pullFailed");
      setSourceError(msg);
      message.error(msg);
    } finally {
      await refreshDetail(version.series_id || detail?.id || 0);
      versions.refetch();
    }
  };
  // syncFromPeer starts a peer copy from the node the operator picked in the
  // peer dialog. It rejects so the dialog can keep the reason on screen.
  const syncFromPeer = async (
    version: ImageCatalogEntry,
    nodeUUID: string,
    sourceNode: string,
    rateLimitKiB: number,
  ) => {
    const target = nodeRow(version, nodeUUID);
    const fetch = { source_node: sourceNode, rate_limit_kib_s: rateLimitKiB };
    try {
      if (target?.state === "failed") await api.retryImage(version.id, nodeUUID, undefined, fetch);
      else await api.syncImage(version.id, nodeUUID, undefined, fetch);
      message.success(t("action.submitted", { source: t("action.source.peer") }));
    } finally {
      await refreshDetail(version.series_id || detail?.id || 0);
      versions.refetch();
    }
  };
  // cancelTransfer stops a transfer that is still running on a node. The server
  // also fails the row, so the row comes back retryable even when the node had
  // already lost the operation.
  const cancelTransfer = async (version: ImageCatalogEntry, nodeUUID: string) => {
    try {
      const row = await api.cancelImage(version.id, nodeUUID);
      message.success(t("action.cancelled", { state: row.state }));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.cancelFailed"));
    } finally {
      await refreshDetail(version.series_id || detail?.id || 0);
      versions.refetch();
    }
  };
  // removeFromNode deletes the image from one node. The node's runtime has the
  // last word — a container still using it makes the deletion fail, and the
  // reason comes back to the operator.
  const removeFromNode = async (version: ImageCatalogEntry, nodeUUID: string) => {
    try {
      await api.removeImage(version.id, nodeUUID);
      message.success(t("action.removed"));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.removeFailed"));
    } finally {
      await refreshDetail(version.series_id || detail?.id || 0);
      versions.refetch();
    }
  };
  // pullFromURL is the dialog's submit: the address is recorded on the image —
  // which is what makes it selectable on every other node and acceptable to the
  // sync endpoint — and only then does the node start fetching. A rejection
  // propagates so the dialog can show why instead of closing on a failure.
  const pullFromURL = async (
    version: ImageCatalogEntry,
    nodeUUID: string,
    url: string,
    relay: boolean,
  ) => {
    const existing = version.registry_sources ?? [];
    if (!existing.includes(url)) {
      await api.updateImageVersion(version.id, {
        registry_sources: [...existing, url],
      });
    }
    const target = nodeRow(version, nodeUUID);
    const fetchOptions = { archive_url: url, relay };
    if (target?.state === "failed") {
      await api.retryImage(version.id, nodeUUID, "url", fetchOptions);
    } else {
      await api.syncImage(version.id, nodeUUID, "url", fetchOptions);
    }
    message.success(t("action.submitted", { source: archiveURLHost(url) }));
    await refreshDetail(version.series_id || detail?.id || 0);
    versions.refetch();
  };
  // recordArchiveURL appends a pasted archive address to a catalog version's
  // declared sources, which is what makes it selectable on every node and
  // acceptable to the sync endpoint.
  const recordArchiveURL = async (
    version: ImageCatalogEntry,
    preview: ImageArchivePreview,
  ) => {
    const existing = version.registry_sources ?? [];
    try {
      if (!existing.includes(preview.url)) {
        await api.updateImageVersion(version.id, {
          registry_sources: [...existing, preview.url],
        });
      }
      message.success(t("import.url.recorded", { ref: version.image_ref }));
      setImportOpen(false);
      const series = (data?.items ?? []).find((row) => row.id === version.series_id);
      if (series) {
        detailRef.current = series;
        setDetail(series);
        await refreshDetail(series.id);
      }
      setSelectedVersionID(version.id);
      versions.refetch();
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("import.url.failed"));
    }
  };
  const importCustom = async () => {
    let value;
    try {
      value = await importForm.validateFields();
    } catch {
      return; // the form shows what is missing
    }
    setImporting(true);
    try {
      const importSource = sourceTab === "custom" ? "custom" : "community";
      // Purpose is left to the server's default (inference), which is what
      // the deploy page lists; series defaults are not edited here.
      await api.importImages([
        {
          name: value.series_name,
          engine: value.engine,
          framework: value.framework || "",
          source: importSource,
          versions: [
            {
              engine_version: value.engine_version || "",
              framework_version: value.framework_version || "",
              arch: value.arch || "multi",
              image_ref: String(value.image_ref || "").trim(),
              source: importSource,
              rolling_tag: Boolean(value.rolling_tag),
              registry_sources: value.registry_sources || [],
            },
          ],
        },
      ]);
      message.success(t("action.imported", {
        source: t(`action.sourceType.${importSource}`),
      }));
      importForm.resetFields();
      setImportOpen(false);
      refetch();
      versions.refetch();
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.importFailed"));
    } finally {
      setImporting(false);
    }
  };
  const toggleSeries = async (row: ImageSeries, enabled: boolean) => {
    try {
      const updated = await api.setImageSeriesEnabled(row.id, enabled);
      if (detail?.id === row.id) setDetail(updated);
      refetch();
      message.success(t(enabled ? "action.seriesEnabled" : "action.seriesDisabled"));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.updateFailed"));
    }
  };
  const deleteSeries = async (row: ImageSeries) => {
    try {
      await api.deleteImageSeries(row.id);
      if (detail?.id === row.id) setDetail(null);
      refetch();
      versions.refetch();
      message.success(t("action.seriesDeleted"));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.deleteSeriesFailed"));
    }
  };
  const toggleVersion = async (row: ImageCatalogEntry, enabled: boolean) => {
    try {
      await api.setImageVersionEnabled(row.id, enabled);
      if (detail?.id) await refreshDetail(detail.id);
      versions.refetch();
      message.success(t(enabled ? "action.versionEnabled" : "action.versionDisabled"));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.updateFailed"));
    }
  };
  const deleteVersion = async (row: ImageCatalogEntry) => {
    try {
      await api.deleteImage(row.id);
      if (selectedVersionID === row.id) setSelectedVersionID(null);
      if (detail?.id) await refreshDetail(detail.id);
      refetch();
      versions.refetch();
      message.success(t("action.versionDeleted"));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.deleteVersionFailed"));
    }
  };
  const inspectVersion = async (
    version: ImageCatalogEntry,
    nodeUUID: string,
  ) => {
    const key = `${version.id}:${nodeUUID}`;
    setInspecting(key);
    try {
      const result = await api.inspectImage(version.id, nodeUUID);
      setInspection({ versionID: version.id, nodeUUID, result });
      message.success(t("action.inspectUpdated"));
      await refreshDetail(version.series_id || detail?.id || 0);
    } catch (e) {
      message.error(e instanceof Error ? e.message : t("action.inspectFailed"));
    } finally {
      setInspecting(null);
    }
  };
  // Until the detail answers, the drawer lists the series' versions from the
  // catalog list, which has counts but no node rows.
  const detailVersions =
    detail?.versions ??
    allVersions.filter((item) => item.series_id === detail?.id);
  const selectedVersion =
    detailVersions.find((item) => item.id === selectedVersionID) ?? null;
  const selectVersion = (row: ImageCatalogEntry) => setSelectedVersionID(row.id);
  const versionColumns = buildVersionColumns({
    seriesEnabled: detail?.enabled ?? true,
    onSelect: selectVersion,
    onToggle: (row, enabled) => void toggleVersion(row, enabled),
  }, t);
  return (
    <div className="resource-page">
      <ImagesPageOverview
        sourceTab={sourceTab}
        onSourceTabChange={(tab) => {
          setSourceTab(tab);
          setDetail(null);
          setSelectedVersionID(null);
        }}
        series={series}
        loading={loading}
        error={error || versions.error}
        updatedAt={updatedAt}
        catalogVersion={catalogVersion}
        syncingCatalog={syncingCatalog}
        onSyncCatalog={() => void applyCatalog(api.syncImages, setSyncingCatalog)}
        uploadingManifest={uploadingManifest}
        onUploadManifest={(file) =>
          void applyCatalog(() => api.importImageCatalogFile(file), setUploadingManifest)
        }
        onOpenImport={() => setImportOpen(true)}
        totalVersions={totalVersions}
        distributed={distributed}
        catalogBytes={catalogBytes}
        localBytes={localBytes}
        catalogUnknown={sizesAvailable ? catalogUnknown : 0}
        localUnknown={sizesAvailable ? localUnknown : 0}
        sourceError={sourceError}
        onDismissSourceError={() => setSourceError(null)}
        onRefresh={() => {
          refetch();
          versions.refetch();
        }}
        onOpenSeries={(row) => {
          detailRef.current = row;
          setDetail(row);
          void refreshDetail(row.id);
        }}
        onToggleSeries={(row, enabled) => void toggleSeries(row, enabled)}
        onDeleteSeries={(row) => void deleteSeries(row)}
      />
      <ImageDetailDrawer
        detail={detail}
        onClose={() => {
          detailRef.current = null;
          setDetail(null);
          setSelectedVersionID(null);
        }}
        detailVersions={detailVersions}
        nodesLoading={Boolean(detail && !detail.versions)}
        versionColumns={versionColumns}
        selectedVersion={selectedVersion}
        setSelectedVersion={selectVersion}
        inspection={
          inspection && selectedVersion && inspection.versionID === selectedVersion.id
            ? inspection
            : null
        }
        nodeName={(nodeUUID) =>
          selectedVersion?.nodes?.find((row) => row.node_uuid === nodeUUID)?.node_name || nodeUUID
        }
        inspecting={inspecting}
        availablePeer={availablePeer}
        modeFor={modeFor}
        setMode={setMode}
        pullVersion={(version, nodeUUID, retry) =>
          void pullVersion(version, nodeUUID, retry)
        }
        probeImage={(version, nodeUUID, sourceNode) => api.probeImage(version.id, nodeUUID, sourceNode)}
        syncFromPeer={syncFromPeer}
        pullFromURL={pullFromURL}
        cancelTransfer={(version, nodeUUID) => void cancelTransfer(version, nodeUUID)}
        removeFromNode={(version, nodeUUID) => void removeFromNode(version, nodeUUID)}
        inspectVersion={(version, nodeUUID) =>
          void inspectVersion(version, nodeUUID)
        }
        repullVersion={(version, nodeUUID) =>
          void pullVersion(version, nodeUUID, false, true)
        }
        deleteSeries={(row) => void deleteSeries(row)}
        deleteVersion={(row) => void deleteVersion(row)}
      />
      <ImageImportDrawer
        open={importOpen && sourceTab !== "official"}
        onClose={() => setImportOpen(false)}
        form={importForm}
        importing={importing}
        importCustom={() => void importCustom()}
        hasExplicitTag={hasExplicitTag}
        sourceTab={sourceTab === "custom" ? "custom" : "community"}
        versions={allVersions}
        onArchiveURLMatched={(version, preview) =>
          void recordArchiveURL(version, preview)
        }
      />
    </div>
  );
}
