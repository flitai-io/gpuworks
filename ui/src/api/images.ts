export interface ImageNode {
  node_uuid: string;
  /** The node's display name and state, so the image page needs no node list. */
  node_name: string;
  node_state: string;
  state: string;
  size_bytes?: number;
  transferred_bytes?: number;
  rate_bps?: number;
  transfer_id?: string;
  transfer_error_code?: string;
  source_node_uuid?: string;
  source_node_name?: string;
  source?: "peer" | "offline" | "quay" | "huaweicloud" | "direct" | string;
  rate_limit_kib_s?: number;
  last_rate_limit_kib_s?: number;
  message?: string;
  /** The image ID the node's runtime reported after the last pull ("sha256:…"); omitted when unknown. */
  image_digest_id?: string;
  /** When this row last changed (RFC 3339), so a rolling tag shows how old the node's copy is. */
  updated_at: string;
}

export interface ImageCatalogEntry {
  id: number;
  series_id?: number;
  purpose?: "inference" | string;
  name: string;
  engine: string;
  engine_version?: string;
  framework: string;
  framework_version?: string;
  driver_requirement?: string;
  arch?: string;
  image_ref: string;
  source: string;
  deprecated: boolean;
  enabled: boolean;
  notes?: string;
  size_bytes?: number;
  local_runtime_size_bytes?: number;
  local_size_known_nodes?: number;
  /** Deployments, live instances and models that name this image; a deprecated system image stays while this is above zero. */
  reference_count: number;
  /** Nodes holding this version and nodes in the fleet: every response carries them. */
  present_nodes: number;
  total_nodes: number;
  /**
   * One row per node. Only the series detail (GET /images/{series_id}) and the
   * responses of per-version writes carry it; the catalog-wide version list
   * (GET /images/versions) never does.
   */
  nodes?: ImageNode[];
  deprecated_since?: string;
  registry_sources?: string[];
  rolling_tag?: boolean;
  upstream_gone?: boolean;
  effective_enabled?: boolean;
}

/**
 * One engine an image catalog entry may declare (GET /images/engines), with
 * its product name. The server's order is the order the image form offers.
 */
export interface ImageEngineOption {
  name: string;
  label: string;
}

/**
 * An image's OCI config as one node's runtime reports it. It is read on
 * request and shown as a runtime fact; the catalog does not store it.
 */
export interface ImageInspection {
  image_ref: string;
  image_id?: string;
  entrypoint?: string[];
  cmd?: string[];
  env?: Record<string, string>;
  volumes?: string[];
}

export interface ArchiveImageMetadata {
  engine?: string;
  engine_version?: string;
  framework?: string;
  framework_version?: string;
  arch?: string;
}

export interface RuntimeImage {
  ref: string;
  identity: string;
  size: string;
}

export interface RuntimeImageList {
  runtime: string;
  namespace: string;
  target_namespace: string;
  namespaces?: string[];
  items: RuntimeImage[];
}

export interface NodeImageSelection extends Pick<RuntimeImage, "ref" | "identity"> {
  node_uuid: string;
  runtime: string;
  namespace: string;
  target_namespace: string;
}

export interface ArchiveContents {
  repo_tags: string[];
  metadata_by_tag?: Record<string, ArchiveImageMetadata>;
}

/** What the server could learn about a remote image archive without downloading it. */
export interface ImageArchivePreview extends ArchiveContents {
  url: string;
  size_bytes: number;
  previewable: boolean;
}

export interface ImageNodeSyncResult {
  state: string;
  transfer_id?: string;
  message?: string;
}

/** Options for archive URLs, node files, peer copies and forced pulls. */
export interface ImageFetchOptions {
  archive_url?: string;
  archive_path?: string;
  relay?: boolean;
  source_node?: string;
  rate_limit_kib_s?: number;
  /** Pull again even when the node already holds the image (a rolling tag moved upstream). */
  force?: boolean;
}

/**
 * imageFetchBody builds the sync request. No source means "pull through the
 * pull settings": the body is empty (or only {force:true}). A manual copy from
 * a node names source_node; archives name source "url" or "local". The
 * client never names a registry.
 */
export function imageFetchBody(source?: string, fetch?: ImageFetchOptions) {
  if (fetch?.source_node) return { source_node: fetch.source_node, rate_limit_kib_s: fetch.rate_limit_kib_s ?? 0 };
  const force = fetch?.force ? { force: true } : {};
  if (source === "local") return { source, archive_path: fetch?.archive_path || "", ...force };
  if (source !== "url") return { ...force };
  return {
    source,
    archive_url: fetch?.archive_url || "",
    relay: Boolean(fetch?.relay),
    ...force,
  };
}

export interface ImageSeries {
  id: number;
  name: string;
  engine?: string;
  purpose: "inference" | string;
  framework?: string;
  common_args?: Array<{ flag: string; value?: string; description?: string }>;
  common_env?: Array<{ key: string; value?: string; description?: string }>;
  source: string;
  notes?: string;
  enabled: boolean;
  version_count: number;
  present_nodes: number;
  total_nodes: number;
  versions?: ImageCatalogEntry[];
}

/** One built-in (or administrator-configured) community repository card. */
export interface CommunityRepo {
  access_host?: string;
  repo: string;
  registry: "docker-hub" | "quay" | "oci" | string;
  engine: string;
  framework: string;
  /** Further repository paths shown on the same card; each lists its own tags. */
  siblings?: string[];
}

export interface CommunityTag {
  tag: string;
  engine?: string;
  engine_version?: string;
  framework?: string;
  framework_version?: string;
  arch?: string;
  rolling_tag?: boolean;
  upstream_gone?: boolean;
  /** When the registry last saw the tag move (RFC 3339); absent for plain OCI registries. */
  last_updated?: string;
}

/** Every tag the upstream registry lists for one repository, unfiltered. */
export interface CommunityRepoBrowse {
  repo: string;
  registry: string;
  engine: string;
  framework: string;
  siblings?: string[];
  tags: CommunityTag[];
}

/** A manifest item the import left alone because a community or custom row already holds its name. */
export interface ImageImportConflict {
  kind: string;
  name: string;
  reason: string;
}

/** What "check for updates" and a manifest upload changed in the system catalog. */
export interface ImageCatalogSyncResult {
  catalog_version: string;
  created: number;
  updated: number;
  deleted: number;
  deprecated: number;
  skipped: ImageImportConflict[];
  series: ImageSeries[];
}
