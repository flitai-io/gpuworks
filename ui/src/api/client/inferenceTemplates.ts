import { getJSON, listQueryString, sendJSON, type List, type ListQuery } from "../http";
import type { TemplateWeightModels, WeightSourceProbe } from "../models";
import type { DetectedTemplateChip, InferenceTemplate, InferenceTemplateChip, InferenceTemplateDetail, InferenceTemplateImportResult, InferenceTemplateIndex, InferenceTemplateOrganizations, InferenceTemplatePage, InferenceTemplateSummary, OfficialInferenceCatalogResult, WeightSource } from "../inferenceTemplates";
import { getText, sendFile } from "./transport";

export const inferenceTemplatesApi = {
  templateWeightModels: (templateID: number, specSource: string, specName: string) =>
    getJSON<TemplateWeightModels>(
      `/api/v1/inference-templates/weight-models?${new URLSearchParams({ template_id: String(templateID), spec_source: specSource, spec_name: specName })}`,
    ),
  probeWeightSource: (templateID: number, specSource: string, specName: string, source: WeightSource, signal?: AbortSignal) =>
    getJSON<WeightSourceProbe>(
      `/api/v1/inference-templates/weight-source-probe?${new URLSearchParams({
        template_id: String(templateID), spec_source: specSource, spec_name: specName, provider: source.provider, repo_id: source.repo_id,
        ...(source.revision ? { revision: source.revision } : {}), ...(source.subfolder ? { subfolder: source.subfolder } : {}),
      })}`,
      signal,
    ),
  inferenceTemplates: (query: ListQuery) =>
    getJSON<InferenceTemplatePage>(`/api/v1/inference-templates?${listQueryString(query)}`),
  inferenceTemplateIndex: (scope?: { vendor: string; node_uuid?: string }) =>
    getJSON<InferenceTemplateIndex>(`/api/v1/inference-templates/index${scope ? `?${new URLSearchParams({ compatibility: "detected", vendor: scope.vendor, ...(scope.node_uuid ? { node_uuid: scope.node_uuid } : {}) })}` : ""}`),
  inferenceTemplateOrganizations: (query = "") =>
    getJSON<InferenceTemplateOrganizations>(
      `/api/v1/inference-templates/organizations${query}`,
    ),
  inferenceTemplate: (modelID: string) =>
    getJSON<InferenceTemplateDetail>(
      `/api/v1/inference-templates/model/${modelID.split("/").map(encodeURIComponent).join("/")}`,
    ),
  // The flat YAML the template editor shows. The server writes it from the
  // types the import endpoint reads, so saving it unchanged is lossless;
  // specSource keeps only the specs of that source.
  inferenceTemplateYAML: (modelID: string, specSource?: string) =>
    getText(
      `/api/v1/inference-templates/yaml?${new URLSearchParams({ model_id: modelID, ...(specSource ? { spec_source: specSource } : {}) })}`,
    ),
  inferenceTemplateChips: () =>
    getJSON<List<InferenceTemplateChip>>("/api/v1/inference-templates/chips"),
  inferenceTemplateSchema: () =>
    getJSON<unknown>("/api/v1/inference-templates/schema"),
  detectedInferenceTemplateChips: () =>
    getJSON<List<DetectedTemplateChip>>("/api/v1/inference-templates/my-chips"),
  inferenceTemplateSummary: (query = "") =>
    getJSON<InferenceTemplateSummary>(
      `/api/v1/inference-templates/summary${query}`,
    ),
  cloneInferenceTemplate: (
    sourceModelID: string,
    modelID?: string,
    displayTitle?: string,
  ) =>
    sendJSON<InferenceTemplate>("POST", "/api/v1/inference-templates/clone", {
      source_model_id: sourceModelID,
      model_id: modelID,
      display_title: displayTitle,
    }),
  // The official catalog comes from the distribution channel; the request has no body.
  syncInferenceTemplates: () =>
    sendJSON<OfficialInferenceCatalogResult>("POST", "/api/v1/inference-templates/sync"),
  // An official catalog package uploaded for a server that cannot reach the channel.
  uploadOfficialInferenceCatalog: (file: File) =>
    sendFile<OfficialInferenceCatalogResult>("/api/v1/inference-templates/official", file),
  importInferenceTemplates: (file: File, overwrite = false) =>
    sendFile<InferenceTemplateImportResult>(
      `/api/v1/inference-templates/import${overwrite ? "?overwrite=true" : ""}`,
      file,
    ),
  deleteInferenceTemplate: (modelID: string) =>
    sendJSON<{ deleted: boolean; model_deleted: boolean }>("DELETE", `/api/v1/inference-templates?model_id=${encodeURIComponent(modelID)}`),
  deleteInferenceTemplateSpec: (modelID: string, spec: string) =>
    sendJSON<{ deleted: boolean; model_deleted: boolean }>(
      "DELETE",
      `/api/v1/inference-templates/spec?model_id=${encodeURIComponent(modelID)}&spec=${encodeURIComponent(spec)}`,
    ),
};
