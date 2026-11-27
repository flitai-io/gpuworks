import type { ModelLocationRef } from "../../storageTypes";
import type { InferenceAPIOperation } from "../../inferenceApi";
import { getJSON, listQueryString, sendJSON, type List, type ListQuery } from "../http";
import type { LocalModelPreview, Model, ModelPage, ModelPresenceSummary, URLModelPreview } from "../models";

export const modelsApi = {
  models: () => getJSON<List<Model>>("/api/v1/models"),
  modelsPage: (query: ListQuery) => getJSON<ModelPage>(`/api/v1/models?${listQueryString(query)}`),
  modelPresence: (ids: number[]) => getJSON<Record<number, ModelPresenceSummary>>(`/api/v1/models/presence?ids=${ids.join(",")}`),
  model: (id: number) => getJSON<Model>(`/api/v1/models/${id}`),
  deleteModel: (id: number) => sendJSON<void>("DELETE", `/api/v1/models/${id}`),
  createModel: (body: {
    source: string;
    source_id: string;
    revision: string;
    name: string;
    readme?: string;
    config_raw?: string;
    weight_bytes?: number;
    logo_data?: string;
    browser_metadata?: boolean;
    /** Registers a repository its site says it does not have (422 MODEL_REPO_NOT_FOUND / MODEL_REVISION_NOT_FOUND without it). */
    force?: boolean;
  }) => sendJSON<Model>("POST", "/api/v1/models", body),
  quickRegisterModel: (body: {
    source: string;
    source_id: string;
    revision?: string;
    name?: string;
    weight_bytes?: number;
    quantization?: string;
    architecture?: string;
    context_length?: number;
    artifact_subfolder?: string;
  }) => sendJSON<Model>("POST", "/api/v1/models/quick-register", body),
  refreshModelMetadata: (
    id: number,
    metadata?: {
      readme?: string;
      config_raw?: string;
      weight_bytes?: number;
      logo_data?: string;
      browser_metadata: true;
    },
  ) =>
    sendJSON<Model>("POST", `/api/v1/models/${id}/metadata/refresh`, metadata),
  updateModelSourceID: (id: number, sourceID: string) =>
    sendJSON<Model>("PATCH", `/api/v1/models/${id}`, { source_id: sourceID }),
  updateModelInferenceConfig: (
    id: number,
    operations: Pick<InferenceAPIOperation, "type">[],
    inputModalities: string[],
  ) =>
    sendJSON<Model>("PATCH", `/api/v1/models/${id}/inference-config`, {
      operations,
      input_modalities: inputModalities,
    }),
  previewLocalModel: (nodeUuid: string, path: string) =>
    sendJSON<LocalModelPreview>("POST", "/api/v1/models/local/preview", {
      node_uuid: nodeUuid,
      path,
    }),
  registerLocalModel: (body: {
    node_uuid: string;
    path: string;
    source_id?: string;
    name?: string;
    storage_id?: number;
  }) => sendJSON<Model>("POST", "/api/v1/models/local", body),
  previewURLModel: (url: string) =>
    sendJSON<URLModelPreview>("POST", "/api/v1/models/preview-url", { url }),
  registerURLModel: (body: {
    source_url: string;
    source_id?: string;
    name?: string;
  }) => sendJSON<Model>("POST", "/api/v1/models/register-url", body),
  retryModelCache: (id: number, node: string, storageID: number) => sendJSON<{ status: string }>("POST", `/api/v1/models/${id}/nodes/${encodeURIComponent(node)}/cache/retry`, { storage_id: storageID }),
  downloadModel: (id: number, nodeUuid: string) =>
    sendJSON<void>("POST", `/api/v1/models/${id}/download`, {
      node_uuid: nodeUuid,
    }),
  /** Download into a storage: a local one on each chosen node, a shared one once (no node). */
  /** status is "present" when the storage (or every chosen node) already has the model. */
  downloadModelToStorage: (id: number, body: { storage_id: number; node_uuids?: string[] }) =>
    sendJSON<{ status?: string; writer?: string } | undefined>("POST", `/api/v1/models/${id}/download`, body),
  cancelModelDownload: (id: number, body: { storage_id?: number; node_uuid?: string }) =>
    sendJSON<{ state: string; message: string }>("POST", `/api/v1/models/${id}/download/cancel`, body),
  copyModel: (id: number, body: { source: ModelLocationRef; target: ModelLocationRef; rate_limit_kib_s?: number }) =>
    sendJSON<{ state?: string; transfer_id?: string; message?: string }>("POST", `/api/v1/models/${id}/copy`, body),
  /** Delete a model's copy in a shared storage; force overrides other gpuworks using it. */
  deleteModelInStorage: (id: number, storageId: number, force = false) =>
    sendJSON<void>("DELETE", `/api/v1/models/${id}/storages/${storageId}${force ? "?force=1" : ""}`),
  /** Delete a node's local model files or cache; storageId picks one when the node has several. */
  deleteModelOnNode: (id: number, nodeUuid: string, storageId?: number) =>
    sendJSON<void>("DELETE", `/api/v1/models/${id}/nodes/${nodeUuid}${storageId ? `?storage_id=${storageId}` : ""}`),
  syncModel: (id: number, nodeUuid: string, sourceNode?: string, rateLimitKiB?: number) =>
    sendJSON<{ state: string; transfer_id?: string; message?: string }>(
      "POST",
      `/api/v1/models/${id}/nodes/${nodeUuid}/sync`,
      sourceNode ? { source_node: sourceNode, rate_limit_kib_s: rateLimitKiB ?? 0 } : {},
    ),
  cancelModelSync: (id: number, nodeUuid: string) =>
    sendJSON<{ state: string; transfer_id?: string; message?: string }>(
      "POST",
      `/api/v1/models/${id}/nodes/${nodeUuid}/sync/cancel`,
    ),
  registerModelPath: (id: number, nodeUuid: string, path: string, storageId?: number) =>
    sendJSON<void>("POST", `/api/v1/models/${id}/register-path`, {
      node_uuid: nodeUuid,
      path,
      ...(storageId ? { storage_id: storageId } : {}),
    }),
};
