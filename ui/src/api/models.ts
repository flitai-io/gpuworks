import type { ModelStoragePresence } from "../storageTypes";
import type { ProductMessage } from "../apiError";
import type { InferenceAPIOperation } from "../inferenceApi";
import type { ListPage } from "./http";
import type { WeightSource } from "./inferenceTemplates";

export interface ModelNodePresence {
  model_id?: number;
  model_name?: string;
  preparation?: { phase: string; reason?: string; total_known: boolean; idle_seconds: number; eta_seconds?: number; retryable?: boolean };

  node_uuid: string;
  // Present so a client never has to render a raw uuid; the uuid remains the
  // identifier.
  node_name?: string;
  source_node_name?: string;
  state: string;
  path: string;
  revision?: string;
  progress: number;
  size_bytes: number;
  downloaded: number;
  transferred_bytes: number;
  rate_bps: number;
  rate_limit_kib_s?: number;
  last_rate_limit_kib_s?: number;
  acquisition: "auto" | "peer" | "download" | "registered" | string;
  transfer_id?: string;
  transfer_error_code?: string;
  source_node_uuid?: string;
  message?: string;
  /** The storage the files are in; cache marks an nfs_cache storage's copy on this node. */
  storage_id?: number;
  storage_name?: string;
  storage_type?: string;
  cache?: boolean;
  last_used_at?: string;
  /** These files may be deleted from the node (not in a read-only local storage); absent means not. */
  deletable?: boolean;
}

export interface Model {
  id: number;
  source: string;
  source_id: string;
  /** Set when PATCH /models/{id} may change source_id (a local or URL model). */
  source_id_editable?: boolean;
  /** The HTTP directory a source="url" model is served from; absent for every other source. */
  source_url?: string;
  revision: string;
  artifact_subfolder?: string;
  name: string;
  arch: string;
  quant_label: string;
  weight_gib: number;
  operations?: InferenceAPIOperation[];
  input_modalities?: Array<"text" | "image" | "audio" | "video" | string>;
  readme?: string;
  logo_path?: string;
  origin_created_at?: string;
  origin_updated_at?: string;
  context_length?: number;
  precision?: string;
  created_at: string;
  nodes?: ModelNodePresence[];
  /** The model's single copy in each shared storage that holds it. */
  storages?: ModelStoragePresence[];
  /** On a registration's answer only: why the management node read no metadata. */
  metadata_notice?: ProductMessage;
}

export interface LocalModelPreview {
  node_uuid: string;
  path: string;
  source_id: string;
  name: string;
  arch?: string;
  precision?: string;
  context_length?: number;
  weight_bytes: number;
  operations?: InferenceAPIOperation[];
  input_modalities?: string[];
  modalities?: string[];
  config_raw?: string;
  readme?: string;
  readme_truncated?: boolean;
}

export interface URLModelPreview {
  source_url: string;
  source_id: string;
  name: string;
  arch?: string;
  precision?: string;
  context_length?: number;
  weight_bytes: number;
  files_count: number;
  input_modalities?: string[];
}

/** Where a model's files are, summed: GET /models/{id} lists each location. */
export interface ModelPresenceSummary {
  ready_nodes: number;
  cached_nodes: number;
  ready_storages: number;
  in_flight: number;
  /** Mean progress of the transfers in flight, 0–100. */
  in_flight_percent: number;
  failed: number;
}

/** A model's own fields, without the per-location presence and README a list row leaves out. */
export type ModelCore = Omit<Model, "nodes" | "storages" | "readme">;

/** A model as a list row: no README and no per-location presence, which GET /models/{id} carries. */
export type ModelListItem = ModelCore & { presence: ModelPresenceSummary };

/** GET /inference-templates/weight-models: the registered models serving a spec's weight sources. */
export interface TemplateWeightModels {
  /** Every model serving at least one source, newest first, each once. */
  models: ModelListItem[];
  /** The spec's sources in order; reuse_model_id is the model a choice of that source reuses. */
  sources: Array<WeightSource & { reuse_model_id?: number }>;
}

/** GET /inference-templates/weight-source-probe: the server's answer on registering and deploying from one declared source. */
export interface WeightSourceProbe {
  /** The source served the repository, or its files are already in place. */
  proceed: boolean;
  /** A registered model of this source has its files in place; the site was not asked. */
  present?: boolean;
  /** The address that served the repository. */
  host?: string;
  /** Why proceed is false. */
  reason?: ProductMessage;
}

/** One page of GET /models?page=…, with the summary of every model. */
export interface ModelPage extends ListPage<ModelListItem> {
  summary: { registered: number; ready_files: number; sources: number; weight_gib: number };
}
