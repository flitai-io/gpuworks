import { getJSON, localizedResponseError, sendJSON, type List } from "../http";
import { imageFetchBody, type ArchiveContents, type ArchiveImageMetadata, type NodeImageSelection, type RuntimeImageList, type CommunityRepo, type CommunityRepoBrowse, type ImageArchivePreview, type ImageCatalogEntry, type ImageCatalogSyncResult, type ImageEngineOption, type ImageFetchOptions, type ImageInspection, type ImageNodeSyncResult, type ImageSeries } from "../images";

export const imagesApi = {
  images: (source?: string) =>
    getJSON<List<ImageSeries>>(
      `/api/v1/images${source ? `?source=${encodeURIComponent(source)}` : ""}`,
    ),
  imageVersions: (source?: string) =>
    getJSON<List<ImageCatalogEntry>>(
      `/api/v1/images/versions${source ? `?source=${encodeURIComponent(source)}` : ""}`,
    ),
  imageEngines: () => getJSON<List<ImageEngineOption>>("/api/v1/images/engines"),
  communityRepos: () =>
    getJSON<List<CommunityRepo>>("/api/v1/images/community/repos"),
  browseCommunity: (repo: string) =>
    getJSON<CommunityRepoBrowse>(
      `/api/v1/images/community/browse?repo=${encodeURIComponent(repo)}`,
    ),
  image: (id: number) => getJSON<ImageSeries>(`/api/v1/images/${id}`),
  deleteImage: (id: number) => sendJSON<void>("DELETE", `/api/v1/images/${id}`),
  deleteImageSeries: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/image-series/${id}`),
  setImageSeriesEnabled: (id: number, enabled: boolean) =>
    sendJSON<ImageSeries>("PATCH", `/api/v1/images/${id}`, { enabled }),
  setImageVersionEnabled: (id: number, enabled: boolean) =>
    sendJSON<ImageCatalogEntry>("PATCH", `/api/v1/images/versions/${id}`, {
      enabled,
    }),
  updateImageVersion: (id: number, body: Record<string, unknown>) =>
    sendJSON<ImageCatalogEntry>("PATCH", `/api/v1/images/versions/${id}`, body),
  importImages: (series: unknown[], runtime_image?: NodeImageSelection) =>
    sendJSON<List<ImageSeries>>("POST", "/api/v1/images/import", { series, runtime_image }),
  // A manifest upload is the manifest JSON itself; the server answers with
  // the same counts as a sync from the distribution channel.
  importImageCatalogFile: async (file: File) => {
    const res = await fetch("/api/v1/images/import", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: await file.text(),
    });
    if (!res.ok) {
      throw await localizedResponseError(
        res,
        `image catalog import: ${res.status}`,
      );
    }
    return (await res.json()) as ImageCatalogSyncResult;
  },
  // The catalog comes from the distribution channel; the request has no body.
  syncImages: () =>
    sendJSON<ImageCatalogSyncResult>("POST", "/api/v1/images/sync"),
  runtimeImages: (node: string, namespace: string, signal?: AbortSignal) =>
    getJSON<RuntimeImageList>(`/api/v1/images/nodes/${encodeURIComponent(node)}/runtime-images?namespace=${encodeURIComponent(namespace)}`, signal),
  inspectRuntimeImage: (selection: NodeImageSelection, signal?: AbortSignal) =>
    sendJSON<{ metadata: ArchiveImageMetadata }>("POST", `/api/v1/images/nodes/${encodeURIComponent(selection.node_uuid)}/runtime-images/inspect`, selection, signal),
  previewImageArchivePath: (nodeUUID: string, path: string, signal?: AbortSignal) =>
    sendJSON<ArchiveContents>("POST", `/api/v1/images/nodes/${encodeURIComponent(nodeUUID)}/preview-archive-path`, { archive_path: path }, signal),
  previewImageArchiveURL: (url: string, signal?: AbortSignal) =>
    sendJSON<ImageArchivePreview>(
      "POST",
      "/api/v1/images/preview-archive-url",
      { url },
      signal,
    ),
  syncImage: (
    id: number,
    nodeUuid: string,
    source?: string,
    fetch?: ImageFetchOptions,
  ) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/sync`,
      imageFetchBody(source, fetch),
    ),
  probeImage: (id: number, nodeUuid: string, sourceNode: string) =>
    sendJSON<{ state: string; transport?: "direct" | "relay" }>("POST", `/api/v1/images/${id}/nodes/${nodeUuid}/sync/probe`, { source_node: sourceNode }),
  retryImage: (
    id: number,
    nodeUuid: string,
    source?: string,
    fetch?: ImageFetchOptions,
  ) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/sync/retry`,
      imageFetchBody(source, fetch),
    ),
  cancelImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageNodeSyncResult>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/cancel`,
    ),
  removeImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageNodeSyncResult>(
      "DELETE",
      `/api/v1/images/${id}/nodes/${nodeUuid}`,
    ),
  inspectImage: (id: number, nodeUuid: string) =>
    sendJSON<ImageInspection>(
      "POST",
      `/api/v1/images/${id}/nodes/${nodeUuid}/inspect`,
      {},
    ),
};
